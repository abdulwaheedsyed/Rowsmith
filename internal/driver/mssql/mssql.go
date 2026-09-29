// Package mssql implements the Microsoft SQL Server driver (SQL Server 2012+,
// Azure SQL Database and Azure SQL Managed Instance).
package mssql

import (
	"context"
	"database/sql"
	sqldriver "database/sql/driver"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	ms "github.com/microsoft/go-mssqldb"
	"github.com/microsoft/go-mssqldb/msdsn"

	"rowsmith/internal/driver"
	"rowsmith/internal/driver/geo"
	"rowsmith/internal/driver/sqlbase"
)

type mssqlDriver struct{}

func init() { driver.Register(mssqlDriver{}) }

var kinds = []driver.KindInfo{
	{Kind: "table", Label: "Tables", Icon: "table", Browse: true},
	{Kind: "view", Label: "Views", Icon: "view", Browse: true},
	{Kind: "procedure", Label: "Procedures", Icon: "procedure"},
	{Kind: "function", Label: "Functions", Icon: "function"},
	{Kind: "trigger", Label: "Triggers", Icon: "trigger"},
	{Kind: "sequence", Label: "Sequences", Icon: "sequence"},
	{Kind: "synonym", Label: "Synonyms", Icon: "synonym"},
	{Kind: "type", Label: "Types", Icon: "type"},
}

var types = []string{
	"int", "bigint", "smallint", "tinyint", "bit", "decimal(18,2)", "numeric(18,2)", "money", "float", "real",
	"nvarchar(255)", "nvarchar(max)", "varchar(255)", "varchar(max)", "nchar(10)", "char(10)", "uniqueidentifier",
	"date", "datetime2(7)", "datetime", "datetimeoffset(7)", "time(7)", "varbinary(max)", "binary(16)", "xml",
	"geometry", "geography", "hierarchyid", "sql_variant", "rowversion",
}

var encryptModes = []driver.Option{
	{Value: "disable", Label: "Disabled (no TLS)"},
	{Value: "true", Label: "Required (TLS after pre-login)"},
	{Value: "strict", Label: "Strict (TDS 8.0, TLS before any traffic)"},
}

func (mssqlDriver) Info() driver.Info {
	fields := driver.NetworkFields(1433, "Database", false)
	fields = append(fields,
		driver.Field{Key: "encrypt", Label: "Encryption", Type: driver.FieldSelect, Options: encryptModes, Default: "true", Section: "tls"},
		driver.Field{Key: "trustServerCertificate", Label: "Trust the server certificate without validation", Type: driver.FieldBool, Section: "tls",
			ShowIf: map[string][]string{"encrypt": {"true"}}, Help: "Needed for self-signed certificates, as used by development servers and containers."},
		driver.Field{Key: "connectTimeout", Label: "Connect timeout (seconds)", Type: driver.FieldNumber, Default: 15, Section: "advanced", Span: 3},
		driver.Field{Key: "readOnlyIntent", Label: "Application intent: read-only", Type: driver.FieldBool, Section: "advanced",
			Help: "Routes to a readable secondary of an Always On availability group (set Database for routing to work)."},
	)
	return driver.Info{
		ID: "mssql", Name: "SQL Server", Order: 30,
		Description: "Microsoft SQL Server 2012+, Azure SQL Database and Managed Instance",
		Dialect:     "mssql", DefaultPort: 1433, Fields: fields, SSH: true,
		Caps: driver.Caps{Databases: true, Schemas: true, SQL: true, Transactions: true, EditRows: true, ForeignKeys: true,
			Explain: true, Processes: true, Variables: true, Users: true, Geometry: true},
		Kinds: kinds, Types: types, URLSchemes: []string{"sqlserver", "mssql"}, QuoteChar: "[",
	}
}

// conn is one pool per server: every catalog query names its database
// explicitly ([db].sys.tables), and consoles switch with USE.
type conn struct {
	eng       *sqlbase.Engine
	db        *sql.DB
	defaultDB string // database new connections start in
	version   string
	ro        bool
}

func (mssqlDriver) Open(ctx context.Context, p driver.OpenParams) (driver.Conn, error) {
	host, port := driver.HostPort(p, 1433)
	if host == "" {
		return nil, errors.New("host is required")
	}
	cfg, err := config(p, host, port)
	if err != nil {
		return nil, err
	}
	inner := ms.NewConnectorConfig(cfg)
	inner.Dialer = directDialer{d: net.Dialer{KeepAlive: 30 * time.Second}}
	if p.Dial != nil {
		inner.Dialer = tunnelDialer{dial: p.Dial, host: host}
	}
	db := sql.OpenDB(connector{inner: inner})
	db.SetMaxOpenConns(16)
	db.SetMaxIdleConns(4)
	db.SetConnMaxIdleTime(5 * time.Minute)
	db.SetConnMaxLifetime(time.Hour)

	c := &conn{db: db, ro: p.ReadOnly}
	c.eng = &sqlbase.Engine{DB: db, D: dialect{}}
	pctx, cancel := context.WithTimeout(ctx, cfg.DialTimeout+5*time.Second)
	defer cancel()
	if err := db.QueryRowContext(pctx, "SELECT DB_NAME(), CAST(SERVERPROPERTY('ProductVersion') AS nvarchar(128))").Scan(&c.defaultDB, &c.version); err != nil {
		db.Close()
		return nil, mapError(err)
	}
	return c, nil
}

// config translates the connection form into go-mssqldb settings.
//
// Read-only connections declare ApplicationIntent=ReadOnly, which only routes
// to readable secondaries; SQL Server itself does not make the session
// read-only. Consoles block writing statements via the classifier, but real
// enforcement needs credentials without write permissions.
func config(p driver.OpenParams, host string, port int) (msdsn.Config, error) {
	timeout := p.Int("connectTimeout", 15)
	if timeout <= 0 {
		timeout = 15
	}
	encrypt := p.String("encrypt")
	if encrypt == "" {
		encrypt = "true"
	}
	switch encrypt {
	case "disable", "true", "strict":
	default:
		return msdsn.Config{}, fmt.Errorf("unknown encryption mode %q", encrypt)
	}
	q := url.Values{}
	q.Set("database", p.String("database"))
	q.Set("encrypt", encrypt)
	q.Set("TrustServerCertificate", strconv.FormatBool(p.Bool("trustServerCertificate")))
	// Not "connection timeout": go-mssqldb applies it to every read for the
	// connection's lifetime, which would abort long-running queries.
	q.Set("dial timeout", strconv.Itoa(timeout))
	if p.AppName != "" {
		q.Set("app name", p.AppName)
	}
	u := url.URL{Scheme: "sqlserver", Host: net.JoinHostPort(host, strconv.Itoa(port)),
		User: url.UserPassword(p.String("user"), p.Secret("password")), RawQuery: q.Encode()}
	cfg, err := msdsn.Parse(u.String())
	if err != nil {
		return msdsn.Config{}, err
	}
	// Set directly: the DSN form insists on a database, but intent without
	// one is harmless (the server simply does not route).
	cfg.ReadOnlyIntent = p.ReadOnly || p.Bool("readOnlyIntent")
	return cfg, nil
}

func (c *conn) Close() error                   { return c.db.Close() }
func (c *conn) Ping(ctx context.Context) error { return c.db.PingContext(ctx) }

func (c *conn) Server(ctx context.Context) (*driver.ServerInfo, error) {
	var ver, edition, level, update, user, db, name, coll sql.NullString
	var engine sql.NullInt64
	err := c.db.QueryRowContext(ctx, `SELECT @@VERSION, CAST(SERVERPROPERTY('Edition') AS nvarchar(128)),
		CAST(SERVERPROPERTY('ProductLevel') AS nvarchar(128)), CAST(SERVERPROPERTY('ProductUpdateLevel') AS nvarchar(128)),
		CAST(SERVERPROPERTY('EngineEdition') AS int), SUSER_SNAME(), DB_NAME(), @@SERVERNAME,
		CAST(SERVERPROPERTY('Collation') AS nvarchar(128))`).Scan(&ver, &edition, &level, &update, &engine, &user, &db, &name, &coll)
	if err != nil {
		return nil, mapError(err)
	}
	info := &driver.ServerInfo{Product: productName(engine.Int64), Version: c.version, User: user.String, Database: db.String,
		Extras: map[string]string{}}
	if line, _, _ := strings.Cut(ver.String, "\n"); line != "" {
		info.Extras["Build"] = strings.TrimSpace(line)
	}
	setExtra(info.Extras, "Edition", edition.String)
	setExtra(info.Extras, "Level", strings.Trim(level.String+"-"+update.String, "-"))
	setExtra(info.Extras, "Server name", name.String)
	setExtra(info.Extras, "Collation", coll.String)
	if c.ro {
		info.Extras["Session"] = "read-only intent"
	}
	return info, nil
}

func productName(engineEdition int64) string {
	switch engineEdition {
	case 5:
		return "Azure SQL Database"
	case 8:
		return "Azure SQL Managed Instance"
	case 9:
		return "Azure SQL Edge"
	}
	return "SQL Server"
}

func setExtra(m map[string]string, k, v string) {
	if v != "" {
		m[k] = v
	}
}

func (c *conn) Databases(ctx context.Context) ([]driver.Database, error) {
	const q = `SELECT d.name, d.collation_name, SUSER_SNAME(d.owner_sid), d.database_id, d.is_distributor, d.state_desc,
		HAS_DBACCESS(d.name), %s FROM sys.databases d ORDER BY d.name`
	// File sizes need VIEW ANY DEFINITION; sys.master_files is missing in Azure SQL Database.
	rows, err := c.db.QueryContext(ctx, fmt.Sprintf(q, "(SELECT SUM(CAST(f.size AS bigint)) * 8192 FROM sys.master_files f WHERE f.database_id = d.database_id)"))
	if err != nil {
		rows, err = c.db.QueryContext(ctx, fmt.Sprintf(q, "CAST(NULL AS bigint)"))
	}
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	var out []driver.Database
	for rows.Next() {
		var d driver.Database
		var coll, owner, state sql.NullString
		var id int
		var distributor bool
		var access, size sql.NullInt64
		if err := rows.Scan(&d.Name, &coll, &owner, &id, &distributor, &state, &access, &size); err != nil {
			return nil, err
		}
		d.Collation, d.Owner, d.Size = coll.String, owner.String, sqlbase.NullInt(size)
		d.System = id <= 4 || distributor
		switch {
		case state.String != "" && state.String != "ONLINE":
			d.Comment = strings.ToLower(state.String)
		case access.Valid && access.Int64 == 0:
			d.Comment = "no access"
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (c *conn) Schemas(ctx context.Context, database string) ([]driver.Schema, error) {
	db := c.dbName(database)
	rows, err := c.db.QueryContext(ctx, `SELECT s.name, COALESCE(p.name, ''), s.schema_id FROM `+sysView(db, "schemas")+` s
		LEFT JOIN `+sysView(db, "database_principals")+` p ON p.principal_id = s.principal_id
		ORDER BY CASE WHEN s.name = 'dbo' THEN 0 ELSE 1 END, s.name`)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	var out []driver.Schema
	for rows.Next() {
		var s driver.Schema
		var id int
		if err := rows.Scan(&s.Name, &s.Owner, &id); err != nil {
			return nil, err
		}
		// Fixed database roles own schemas numbered from 16384.
		s.System = s.Name == "sys" || s.Name == "INFORMATION_SCHEMA" || s.Name == "guest" || id >= 16384
		out = append(out, s)
	}
	return out, rows.Err()
}

var functionTypes = map[string]string{"FN": "scalar", "IF": "inline table-valued", "TF": "table-valued",
	"FS": "CLR scalar", "FT": "CLR table-valued", "AF": "CLR aggregate"}

func objectKind(t string) string {
	switch t {
	case "U":
		return "table"
	case "V":
		return "view"
	case "P", "PC", "X":
		return "procedure"
	case "FN", "IF", "TF", "FS", "FT", "AF":
		return "function"
	case "TR", "TA":
		return "trigger"
	case "SO":
		return "sequence"
	case "SN":
		return "synonym"
	}
	return ""
}

func (c *conn) Objects(ctx context.Context, s driver.Scope) ([]driver.Object, error) {
	db, schema := c.dbName(s.Database), schemaName(s.Schema)
	rows, err := c.db.QueryContext(ctx, `SELECT o.name, RTRIM(o.type), o.modify_date,
		CASE WHEN o.type = 'U' THEN (SELECT SUM(p.rows) FROM `+sysView(db, "partitions")+` p WHERE p.object_id = o.object_id AND p.index_id IN (0, 1)) END,
		CASE WHEN o.type = 'U' THEN (SELECT SUM(a.total_pages) FROM `+sysView(db, "partitions")+` p
			JOIN `+sysView(db, "allocation_units")+` a ON a.container_id = p.partition_id WHERE p.object_id = o.object_id) * 8192 END,
		COALESCE(CAST(ep.value AS nvarchar(4000)), ''), COALESCE(sn.base_object_name, ''), COALESCE(po.name, ''),
		COALESCE(tr.is_instead_of_trigger, 0),
		CASE WHEN EXISTS (SELECT 1 FROM `+sysView(db, "trigger_events")+` e WHERE e.object_id = o.object_id AND e.type = 1) THEN 1 ELSE 0 END,
		CASE WHEN EXISTS (SELECT 1 FROM `+sysView(db, "trigger_events")+` e WHERE e.object_id = o.object_id AND e.type = 2) THEN 1 ELSE 0 END,
		CASE WHEN EXISTS (SELECT 1 FROM `+sysView(db, "trigger_events")+` e WHERE e.object_id = o.object_id AND e.type = 3) THEN 1 ELSE 0 END
		FROM `+sysView(db, "objects")+` o
		JOIN `+sysView(db, "schemas")+` s ON s.schema_id = o.schema_id
		LEFT JOIN `+sysView(db, "extended_properties")+` ep ON ep.class = 1 AND ep.major_id = o.object_id AND ep.minor_id = 0 AND ep.name = N'MS_Description'
		LEFT JOIN `+sysView(db, "synonyms")+` sn ON sn.object_id = o.object_id
		LEFT JOIN `+sysView(db, "triggers")+` tr ON tr.object_id = o.object_id
		LEFT JOIN `+sysView(db, "objects")+` po ON po.object_id = o.parent_object_id AND o.type IN ('TR', 'TA')
		WHERE s.name = @p1 AND o.is_ms_shipped = 0
		AND o.type IN ('U', 'V', 'P', 'PC', 'X', 'FN', 'IF', 'TF', 'FS', 'FT', 'AF', 'TR', 'TA', 'SO', 'SN')
		ORDER BY o.name`, schema)
	if err != nil {
		return nil, mapError(err)
	}
	var out []driver.Object
	for rows.Next() {
		var o driver.Object
		var typ, base, parent string
		var modified time.Time
		var nrows, size sql.NullInt64
		var instead, ins, upd, del bool
		if err := rows.Scan(&o.Name, &typ, &modified, &nrows, &size, &o.Comment, &base, &parent, &instead, &ins, &upd, &del); err != nil {
			rows.Close()
			return nil, err
		}
		o.Kind = objectKind(typ)
		o.Rows, o.Size = sqlbase.NullInt(nrows), sqlbase.NullInt(size)
		o.Updated = modified.Format("2006-01-02 15:04:05")
		switch o.Kind {
		case "function":
			o.Extra = functionTypes[typ]
		case "procedure":
			o.Extra = map[string]string{"PC": "CLR", "X": "extended"}[typ]
		case "synonym":
			o.Extra = base
		case "trigger":
			timing, events := triggerTiming(instead, ins, upd, del)
			o.Extra = timing + " " + events + " ON " + parent
		}
		out = append(out, o)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// User-defined types live in sys.types, not sys.objects.
	if r, err := c.db.QueryContext(ctx, `SELECT t.name, t.is_table_type, t.is_assembly_type, COALESCE(bt.name, ''), t.max_length, t.precision, t.scale
		FROM `+sysView(db, "types")+` t JOIN `+sysView(db, "schemas")+` s ON s.schema_id = t.schema_id
		LEFT JOIN `+sysView(db, "types")+` bt ON bt.user_type_id = t.system_type_id
		WHERE t.is_user_defined = 1 AND s.name = @p1 ORDER BY t.name`, schema); err == nil {
		for r.Next() {
			var o driver.Object
			var table, clr bool
			var base string
			var maxLen int
			var prec, scale int
			if r.Scan(&o.Name, &table, &clr, &base, &maxLen, &prec, &scale) != nil {
				continue
			}
			o.Kind = "type"
			switch {
			case table:
				o.Extra = "table type"
			case clr:
				o.Extra = "CLR type"
			default:
				o.Extra = typeString(base, maxLen, prec, scale)
			}
			out = append(out, o)
		}
		r.Close()
	}
	return out, nil
}

// triggerTiming renders the timing and events of a DML trigger.
func triggerTiming(instead, ins, upd, del bool) (timing, events string) {
	timing = "AFTER"
	if instead {
		timing = "INSTEAD OF"
	}
	var ev []string
	for _, e := range []struct {
		on   bool
		name string
	}{{ins, "INSERT"}, {upd, "UPDATE"}, {del, "DELETE"}} {
		if e.on {
			ev = append(ev, e.name)
		}
	}
	return timing, strings.Join(ev, ", ")
}

func (c *conn) Browse(ctx context.Context, req driver.BrowseRequest) (*driver.Result, error) {
	t, err := c.Describe(ctx, req.Ref)
	if err != nil {
		return nil, err
	}
	res, err := c.eng.Browse(ctx, t, req)
	if err != nil {
		return nil, mapError(err)
	}
	// Geometry is selected as EWKT text (SQL Server has no GeoJSON output);
	// the engine has restored the column kind, so convert the cells here.
	for i, rc := range res.Columns {
		if rc.Kind != driver.KindGeometry {
			continue
		}
		for _, row := range res.Rows {
			if s, ok := row[i].(string); ok {
				if cell, ok := geo.FromWKT(s); ok {
					row[i] = cell
				}
			}
		}
	}
	return res, nil
}

func (c *conn) Count(ctx context.Context, req driver.BrowseRequest) (driver.Count, error) {
	t, err := c.Describe(ctx, req.Ref)
	if err != nil {
		return driver.Count{}, err
	}
	if len(req.Filters) == 0 && req.Where == "" && req.Search == "" && t.RowEstimate != nil && *t.RowEstimate > 2_000_000 {
		// Partition row counts are close; COUNT(*) is slow on huge tables and
		// overflows int beyond 2^31 rows.
		return driver.Count{Rows: *t.RowEstimate, Exact: false}, nil
	}
	n, err := c.eng.Count(ctx, t, req)
	return n, mapError(err)
}

func (c *conn) ApplyEdits(ctx context.Context, ref driver.ObjectRef, edits []driver.RowEdit) (*driver.EditResult, error) {
	if c.ro {
		return nil, driver.ErrReadOnly
	}
	t, err := c.Describe(ctx, ref)
	if err != nil {
		return nil, err
	}
	res, err := c.eng.ApplyEdits(ctx, t, edits)
	return res, mapError(err)
}

// NewSession pins a connection and switches it to the scope's database.
// SQL Server has no session-level default schema: unqualified names resolve
// through the user's default schema (usually dbo), so the scope schema is unused.
func (c *conn) NewSession(ctx context.Context, s driver.Scope) (driver.Session, error) {
	sc, err := c.db.Conn(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	if s.Database != "" {
		if _, err := sc.ExecContext(ctx, "USE "+quote(s.Database)); err != nil {
			discard(sc)
			return nil, mapError(err)
		}
	}
	var live *liveness
	_ = sc.Raw(func(dc any) error {
		if d, ok := dc.(*driverConn); ok {
			live = d.live
		}
		return nil
	})
	return &session{conn: sc, live: live}, nil
}

// discard closes a dedicated connection without returning it to the pool, so
// session state (SET options, open transactions, USE) never leaks.
func discard(sc *sql.Conn) {
	_ = sc.Raw(func(any) error { return sqldriver.ErrBadConn })
	_ = sc.Close()
}

// mapError turns SQL Server errors into *driver.QueryError, keeping any
// context the caller added in front ("change 2: ..."). The server often
// follows the real cause with generic errors ("See previous errors"), so
// the first one leads.
func mapError(err error) error {
	var me ms.Error
	if err == nil || !errors.As(err, &me) {
		return err
	}
	prefix := strings.TrimSuffix(err.Error(), me.Error())
	if prefix == err.Error() {
		prefix = ""
	}
	all := me.All
	if len(all) == 0 {
		all = []ms.Error{me}
	}
	msgs := make([]string, len(all))
	for i, e := range all {
		msgs[i] = e.Message
	}
	return &driver.QueryError{Message: prefix + strings.Join(msgs, "\n"), Code: strconv.Itoa(int(all[0].Number))}
}

func (c *conn) dbName(database string) string {
	if database == "" {
		return c.defaultDB
	}
	return database
}

func schemaName(s string) string {
	if s == "" {
		return "dbo"
	}
	return s
}

func quote(name string) string { return "[" + strings.ReplaceAll(name, "]", "]]") + "]" }

func qualify(db, schema, name string) string {
	q := quote(schemaName(schema)) + "." + quote(name)
	if db != "" {
		q = quote(db) + "." + q
	}
	return q
}

// sysView names a catalog view of another database, e.g. [shop].sys.tables.
func sysView(db, view string) string { return quote(db) + ".sys." + view }

func literal(s string) string { return "N'" + strings.ReplaceAll(s, "'", "''") + "'" }
