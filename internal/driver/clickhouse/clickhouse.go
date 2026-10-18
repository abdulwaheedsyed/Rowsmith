// Package clickhouse implements the ClickHouse driver on top of
// clickhouse-go, over the native protocol or HTTP.
package clickhouse

import (
	"context"
	"database/sql"
	"errors"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"

	ch "github.com/ClickHouse/clickhouse-go/v2"

	"rowsmith/internal/driver"
	"rowsmith/internal/driver/sqlbase"
	"rowsmith/internal/sqlsplit"
)

type chDriver struct{}

func init() { driver.Register(chDriver{}) }

var kinds = []driver.KindInfo{
	{Kind: "table", Label: "Tables", Icon: "table", Browse: true},
	{Kind: "view", Label: "Views", Icon: "view", Browse: true},
	{Kind: "materialized_view", Label: "Materialized views", Icon: "matview", Browse: true},
	{Kind: "dictionary", Label: "Dictionaries", Icon: "dictionary", Browse: true},
}

var types = []string{
	"UInt8", "UInt16", "UInt32", "UInt64", "Int8", "Int16", "Int32", "Int64", "Int128", "UInt256",
	"Float32", "Float64", "Decimal(18, 2)", "Bool",
	"String", "LowCardinality(String)", "FixedString(16)", "UUID", "Enum8('a' = 1, 'b' = 2)",
	"Date", "Date32", "DateTime", "DateTime64(3)", "DateTime('UTC')",
	"Array(String)", "Array(UInt64)", "Map(String, String)", "Tuple(name String, value Float64)", "JSON",
	"IPv4", "IPv6", "Point", "Polygon",
}

// tlsModes leaves out "prefer": the client can't fall back to plain text.
var tlsModes = []driver.Option{
	{Value: "disable", Label: "Disabled"},
	{Value: "require", Label: "Required (encrypt, do not verify certificate)"},
	{Value: "verify-ca", Label: "Verify CA (trusted certificate, any hostname)"},
	{Value: "verify-full", Label: "Verify full (trusted certificate and hostname)"},
}

func (chDriver) Info() driver.Info {
	fields := []driver.Field{
		{Key: "host", Label: "Host", Type: driver.FieldText, Required: true, Placeholder: "clickhouse.example.com", Span: 4},
		{Key: "port", Label: "Port", Type: driver.FieldNumber, Default: 9000, Span: 2},
		{Key: "protocol", Label: "Protocol", Type: driver.FieldSelect, Default: "native", Options: []driver.Option{
			{Value: "native", Label: "Native (TCP)"}, {Value: "http", Label: "HTTP"}},
			Help: "Native is faster; use HTTP when only the HTTP port is reachable, for example through a proxy."},
		{Key: "user", Label: "User", Type: driver.FieldText, Default: "default", Section: "auth", Span: 3},
		{Key: "password", Label: "Password", Type: driver.FieldPassword, Secret: true, Section: "auth", Span: 3},
		{Key: "database", Label: "Database", Type: driver.FieldText, Placeholder: "optional — all visible databases are listed"},
	}
	for _, f := range driver.TLSFields("disable") {
		if f.Key == "tls" {
			f.Options = tlsModes
			f.Help = "ClickHouse Cloud needs TLS: port 9440 for native, 8443 for HTTP."
		}
		if f.ShowIf != nil {
			f.ShowIf = map[string][]string{"tls": {"require", "verify-ca", "verify-full"}}
			if f.Key == "tlsServerName" {
				f.ShowIf = map[string][]string{"tls": {"verify-full"}}
			}
		}
		fields = append(fields, f)
	}
	fields = append(fields,
		driver.Field{Key: "connectTimeout", Label: "Connect timeout (seconds)", Type: driver.FieldNumber, Default: 15, Section: "advanced", Span: 3},
		driver.Field{Key: "compression", Label: "Compression", Type: driver.FieldSelect, Default: "lz4", Section: "advanced", Span: 3,
			Options: []driver.Option{{Value: "lz4", Label: "LZ4"}, {Value: "zstd", Label: "ZSTD"}, {Value: "none", Label: "None"}}},
		driver.Field{Key: "httpPath", Label: "HTTP path", Type: driver.FieldText, Section: "advanced", Placeholder: "/",
			ShowIf: map[string][]string{"protocol": {"http"}}, Help: "For a ClickHouse behind a proxy at a sub-path."},
	)
	secure := []string{"require", "verify-ca", "verify-full"}
	return driver.Info{
		ID: "clickhouse", Name: "ClickHouse", Order: 70,
		Description: "ClickHouse 24.8 and later, and ClickHouse Cloud",
		Dialect:     "clickhouse", DefaultPort: 9000, Fields: fields, SSH: true,
		Ports: []driver.PortRule{
			{When: map[string][]string{"protocol": {"http"}, "tls": secure}, Port: 8443},
			{When: map[string][]string{"protocol": {"http"}}, Port: 8123},
			{When: map[string][]string{"tls": secure}, Port: 9440},
		},
		Caps: driver.Caps{Databases: true, SQL: true, EditRows: true, DDL: true, CreateDatabase: true, DropDatabase: true,
			Explain: true, Processes: true, Variables: true, Users: true, Geometry: true, Dump: true},
		Kinds: kinds, Types: types, URLSchemes: []string{"clickhouse"}, QuoteChar: "`",
		Design: &design,
	}
}

type conn struct {
	eng     *sqlbase.Engine
	db      *sql.DB
	http    bool
	version string
	major   int
	minor   int
	ro      bool
	user    string
	// defaultDB is the database the connection starts in.
	defaultDB string
}

func (chDriver) Open(ctx context.Context, p driver.OpenParams) (driver.Conn, error) {
	host, port := driver.HostPort(p, 9000)
	if host == "" {
		return nil, errors.New("host is required")
	}
	user := p.String("user")
	if user == "" {
		user = "default"
	}
	opts := &ch.Options{
		Addr:        []string{net.JoinHostPort(host, strconv.Itoa(port))},
		Auth:        ch.Auth{Username: user, Password: p.Secret("password"), Database: p.String("database")},
		DialTimeout: time.Duration(p.Int("connectTimeout", 15)) * time.Second,
		ClientInfo:  ch.ClientInfo{Products: []struct{ Name, Version string }{{Name: strings.ReplaceAll(p.AppName, "/", " "), Version: ""}}},
		Settings:    ch.Settings{},
	}
	if p.String("protocol") == "http" {
		opts.Protocol = ch.HTTP
		opts.HttpUrlPath = strings.TrimSpace(p.String("httpPath"))
		// Over HTTP the database goes with every request and would undo a
		// console's USE; Rowsmith qualifies names instead.
		opts.Auth.Database = ""
	}
	switch p.String("compression") {
	case "", "lz4":
		opts.Compression = &ch.Compression{Method: ch.CompressionLZ4}
	case "zstd":
		opts.Compression = &ch.Compression{Method: ch.CompressionZSTD}
	}
	if opts.Protocol == ch.HTTP && opts.Compression != nil && opts.Compression.Method == ch.CompressionLZ4 {
		opts.Compression = &ch.Compression{Method: ch.CompressionGZIP} // HTTP compresses with gzip, zstd or brotli
	}
	if p.String("tls") != "" && p.String("tls") != "disable" {
		tc, err := driver.TLSConfig(p, host)
		if err != nil {
			return nil, err
		}
		opts.TLS = tc
	}
	if p.Dial != nil {
		dial := p.Dial
		opts.DialContext = func(ctx context.Context, addr string) (net.Conn, error) { return dial(ctx, "tcp", addr) }
	}
	if p.ReadOnly {
		// readonly = 2: no writes or DDL, but settings can still change.
		opts.Settings["readonly"] = 2
	}
	c, err := open(ctx, opts, p)
	if err != nil && p.ReadOnly && isCode(err, 164) {
		// The account is read-only already (readonly = 1), and can't set it.
		delete(opts.Settings, "readonly")
		c, err = open(ctx, opts, p)
	}
	return c, err
}

func open(ctx context.Context, opts *ch.Options, p driver.OpenParams) (*conn, error) {
	db := ch.OpenDB(opts)
	db.SetMaxOpenConns(16)
	db.SetMaxIdleConns(4)
	db.SetConnMaxIdleTime(5 * time.Minute)
	db.SetConnMaxLifetime(time.Hour)
	c := &conn{db: db, http: opts.Protocol == ch.HTTP, ro: p.ReadOnly, defaultDB: strings.TrimSpace(p.String("database"))}
	c.eng = &sqlbase.Engine{DB: db, D: dialect{}}
	pctx, cancel := context.WithTimeout(ctx, opts.DialTimeout+5*time.Second)
	defer cancel()
	if err := db.QueryRowContext(pctx, "SELECT version(), currentUser()").Scan(&c.version, &c.user); err != nil {
		db.Close()
		return nil, mapError(err, sqlsplit.Statement{})
	}
	c.major, c.minor = parseVersion(c.version)
	return c, nil
}

var versionRe = regexp.MustCompile(`^(\d+)\.(\d+)`)

func parseVersion(v string) (int, int) {
	m := versionRe.FindStringSubmatch(v)
	if m == nil {
		return 0, 0
	}
	a, _ := strconv.Atoi(m[1])
	b, _ := strconv.Atoi(m[2])
	return a, b
}

// atLeast reports whether the server is version major.minor or later.
func (c *conn) atLeast(major, minor int) bool {
	return c.major > major || (c.major == major && c.minor >= minor)
}

func (c *conn) Close() error                   { return c.db.Close() }
func (c *conn) Ping(ctx context.Context) error { return c.db.PingContext(ctx) }

func (c *conn) Server(ctx context.Context) (*driver.ServerInfo, error) {
	var db, tz, uptime string
	if err := c.db.QueryRowContext(ctx, "SELECT currentDatabase(), timezone(), formatReadableTimeDelta(uptime())").Scan(&db, &tz, &uptime); err != nil {
		return nil, mapError(err, sqlsplit.Statement{})
	}
	if c.defaultDB != "" {
		db = c.defaultDB
	}
	info := &driver.ServerInfo{Product: "ClickHouse", Version: c.version, User: c.user, Database: db,
		Extras: map[string]string{"Time zone": tz, "Up": uptime}}
	if c.http {
		info.Extras["Protocol"] = "HTTP"
	}
	var cloud uint8
	if c.db.QueryRowContext(ctx, "SELECT count() FROM system.settings WHERE name = 'cloud_mode' AND value = '1'").Scan(&cloud) == nil && cloud > 0 {
		info.Extras["Edition"] = "Cloud"
	}
	if c.ro {
		info.Extras["Session"] = "read-only"
	}
	return info, nil
}

var systemDBs = map[string]bool{"system": true, "information_schema": true, "INFORMATION_SCHEMA": true}

func (c *conn) Databases(ctx context.Context) ([]driver.Database, error) {
	rows, err := c.db.QueryContext(ctx, `SELECT d.name, d.engine, d.comment, p.size, t.n FROM system.databases d
		LEFT JOIN (SELECT database, sum(bytes_on_disk) AS size FROM system.parts WHERE active GROUP BY database) p ON p.database = d.name
		LEFT JOIN (SELECT database, count() AS n FROM system.tables GROUP BY database) t ON t.database = d.name
		ORDER BY d.name`)
	if err != nil {
		return nil, mapError(err, sqlsplit.Statement{})
	}
	defer rows.Close()
	var out []driver.Database
	for rows.Next() {
		var name, engine, comment string
		var size, tables uint64
		if err := rows.Scan(&name, &engine, &comment, &size, &tables); err != nil {
			return nil, err
		}
		d := driver.Database{Name: name, Comment: comment, System: systemDBs[name]}
		n, sz := int64(tables), int64(size)
		d.Tables, d.Size = &n, &sz
		out = append(out, d)
	}
	return out, rows.Err()
}

func (c *conn) Schemas(context.Context, string) ([]driver.Schema, error) { return nil, nil }

// objectKind maps a table engine to a navigator kind.
func objectKind(engine string) string {
	switch engine {
	case "View":
		return "view"
	case "MaterializedView":
		return "materialized_view"
	case "Dictionary":
		return "dictionary"
	case "LiveView", "WindowView":
		return "view"
	}
	return "table"
}

func (c *conn) Objects(ctx context.Context, s driver.Scope) ([]driver.Object, error) {
	if s.Database == "" {
		return nil, errors.New("choose a database")
	}
	// A materialized view writing into a table of its own names it after TO.
	rows, err := c.db.QueryContext(ctx, `SELECT name, engine, total_rows, total_bytes, comment, toString(metadata_modification_time),
		if(engine = 'MaterializedView', extract(create_table_query, '\\sTO\\s+([^\\s(]+)'), '')
		FROM system.tables WHERE database = ? AND NOT is_temporary ORDER BY name`, s.Database)
	if err != nil {
		return nil, mapError(err, sqlsplit.Statement{})
	}
	defer rows.Close()
	var out []driver.Object
	for rows.Next() {
		var name, engine, comment, updated, target string
		var nrows, size *uint64
		if err := rows.Scan(&name, &engine, &nrows, &size, &comment, &updated, &target); err != nil {
			return nil, err
		}
		o := driver.Object{Name: name, Kind: objectKind(engine), Engine: engine, Comment: comment, Updated: updated}
		if nrows != nil {
			n := int64(*nrows)
			o.Rows = &n
		}
		if size != nil && o.Kind == "table" {
			n := int64(*size)
			o.Size = &n
		}
		if target != "" {
			o.Extra = "to " + strings.Trim(target, "`")
		} else if o.Kind == "table" && engine != "MergeTree" {
			o.Extra = engine
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

func (c *conn) Browse(ctx context.Context, req driver.BrowseRequest) (*driver.Result, error) {
	t, err := c.Describe(ctx, req.Ref)
	if err != nil {
		return nil, err
	}
	res, err := c.eng.Browse(ctx, t, req)
	return res, mapError(err, sqlsplit.Statement{})
}

func (c *conn) Count(ctx context.Context, req driver.BrowseRequest) (driver.Count, error) {
	t, err := c.Describe(ctx, req.Ref)
	if err != nil {
		return driver.Count{}, err
	}
	n, err := c.eng.Count(ctx, t, req)
	return n, mapError(err, sqlsplit.Statement{})
}

var lineColRe = regexp.MustCompile(`\(line (\d+), col (\d+)\)`)
var positionRe = regexp.MustCompile(`failed at position (\d+)`)

// mapError turns a server exception into a QueryError, with the position of
// a syntax error.
func mapError(err error, st sqlsplit.Statement) error {
	var ex *ch.Exception
	if err == nil || !errors.As(err, &ex) {
		return err
	}
	msg := strings.TrimSpace(ex.Message)
	// Messages repeat the code name and end with the server version.
	if i := strings.Index(msg, ". ("); i > 0 && strings.HasSuffix(msg, ")") {
		msg = msg[:i+1]
	}
	qe := &driver.QueryError{Message: msg, Code: strconv.Itoa(int(ex.Code))}
	if ex.CodeName != "" {
		qe.Code = ex.CodeName
	}
	if m := lineColRe.FindStringSubmatch(ex.Message); m != nil {
		n, _ := strconv.Atoi(m[1])
		qe.Line = st.Line + n - 1
	}
	if m := positionRe.FindStringSubmatch(ex.Message); m != nil {
		qe.Position, _ = strconv.Atoi(m[1])
		// The position is a byte offset into the statement: find its line.
		if qe.Line == 0 && st.Line > 0 && qe.Position <= len(st.SQL) {
			qe.Line = st.Line + strings.Count(st.SQL[:qe.Position-1], "\n")
		}
	}
	return qe
}

// isCode reports a server error with that code.
func isCode(err error, code int32) bool {
	var ex *ch.Exception
	return errors.As(err, &ex) && ex.Code == code
}

func quote(name string) string {
	return "`" + strings.NewReplacer(`\`, `\\`, "`", "\\`").Replace(name) + "`"
}

func qualify(db, name string) string {
	if db == "" {
		return quote(name)
	}
	return quote(db) + "." + quote(name)
}

// literal writes a string literal.
func literal(s string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s) + "'"
}
