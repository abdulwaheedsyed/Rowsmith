// Package mysql implements the MySQL and MariaDB drivers.
package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"

	my "github.com/go-sql-driver/mysql"

	"rowsmith/internal/driver"
	"rowsmith/internal/driver/sqlbase"
	"rowsmith/internal/sqlsplit"
)

type mysqlDriver struct {
	id, name string
	order    int
}

func init() {
	driver.Register(&mysqlDriver{id: "mysql", name: "MySQL", order: 10})
	driver.Register(&mysqlDriver{id: "mariadb", name: "MariaDB", order: 11})
}

var kinds = []driver.KindInfo{
	{Kind: "table", Label: "Tables", Icon: "table", Browse: true},
	{Kind: "view", Label: "Views", Icon: "view", Browse: true},
	{Kind: "procedure", Label: "Procedures", Icon: "procedure"},
	{Kind: "function", Label: "Functions", Icon: "function"},
	{Kind: "trigger", Label: "Triggers", Icon: "trigger"},
	{Kind: "event", Label: "Events", Icon: "event"},
}

var types = []string{
	"int", "bigint", "tinyint", "smallint", "mediumint", "decimal(10,2)", "float", "double", "bit(1)",
	"varchar(255)", "char(36)", "text", "mediumtext", "longtext", "json", "enum('a','b')", "set('a','b')",
	"date", "datetime", "timestamp", "time", "year", "binary(16)", "varbinary(255)", "blob", "longblob",
	"geometry", "point", "linestring", "polygon", "multipolygon",
}

func (d *mysqlDriver) Info() driver.Info {
	fields := driver.NetworkFields(3306, "Database", false)
	fields = append(fields, driver.TLSFields("prefer")...)
	fields = append(fields,
		driver.Field{Key: "connectTimeout", Label: "Connect timeout (seconds)", Type: driver.FieldNumber, Default: 15, Section: "advanced", Span: 3},
		driver.Field{Key: "timezone", Label: "Session time zone", Type: driver.FieldText, Placeholder: "server default, e.g. +00:00", Section: "advanced", Span: 3},
		driver.Field{Key: "allowCleartext", Label: "Allow cleartext password plugin (only over TLS/SSH)", Type: driver.FieldBool, Section: "advanced"},
	)
	return driver.Info{
		ID: d.id, Name: d.name, Order: d.order,
		Description: map[string]string{"mysql": "MySQL 5.7, 8.x and 9.x, Percona, Aurora MySQL", "mariadb": "MariaDB 10.x and 11.x"}[d.id],
		Dialect:     "mysql", DefaultPort: 3306, Fields: fields, SSH: true,
		Caps: driver.Caps{Databases: true, SQL: true, Transactions: true, EditRows: true, DDL: true, CreateDatabase: true,
			ForeignKeys: true, Explain: true, Processes: true, Variables: true, Users: true, Geometry: true, Dump: true},
		Kinds: kinds, Types: types, URLSchemes: []string{"mysql", "mariadb"}, QuoteChar: "`",
	}
}

type conn struct {
	eng     *sqlbase.Engine
	db      *sql.DB
	flavor  string // "mysql" or "mariadb"
	version string
	major   int
	minor   int
	ro      bool
}

func (d *mysqlDriver) Open(ctx context.Context, p driver.OpenParams) (driver.Conn, error) {
	host, port := driver.HostPort(p, 3306)
	if host == "" {
		return nil, errors.New("host is required")
	}
	cfg := my.NewConfig()
	cfg.Net = "tcp"
	cfg.Addr = fmt.Sprintf("%s:%d", host, port)
	cfg.User = p.String("user")
	cfg.Passwd = p.Secret("password")
	cfg.DBName = p.String("database")
	cfg.InterpolateParams = true // text protocol everywhere → consistent value encoding
	cfg.ParseTime = false        // keep DATETIME exactly as stored (incl. zero dates)
	cfg.MultiStatements = false
	cfg.ClientFoundRows = true // UPDATE reports matched rows, so unchanged saves are not "missing"
	cfg.Timeout = time.Duration(p.Int("connectTimeout", 15)) * time.Second
	cfg.AllowCleartextPasswords = p.Bool("allowCleartext")
	cfg.Params = map[string]string{}
	cfg.ConnectionAttributes = "program_name:" + p.AppName
	if tz := strings.TrimSpace(p.String("timezone")); tz != "" {
		cfg.Params["time_zone"] = "'" + strings.ReplaceAll(tz, "'", "") + "'"
	}
	if p.Dial != nil {
		dial := p.Dial
		cfg.DialFunc = func(ctx context.Context, network, addr string) (net.Conn, error) { return dial(ctx, "tcp", addr) }
	}
	switch mode := p.String("tls"); mode {
	case "", "disable":
	case "prefer":
		cfg.TLSConfig = "preferred"
	default:
		tc, err := driver.TLSConfig(p, host)
		if err != nil {
			return nil, err
		}
		cfg.TLS = tc
	}
	inner, err := my.NewConnector(cfg)
	if err != nil {
		return nil, err
	}
	var init []string
	if p.ReadOnly {
		init = append(init, "SET SESSION TRANSACTION READ ONLY")
	}
	db := sql.OpenDB(&sqlbase.InitConnector{Inner: inner, Init: init})
	db.SetMaxOpenConns(16)
	db.SetMaxIdleConns(4)
	db.SetConnMaxIdleTime(5 * time.Minute)
	db.SetConnMaxLifetime(time.Hour)

	c := &conn{db: db, ro: p.ReadOnly}
	c.eng = &sqlbase.Engine{DB: db, D: dialect{c: c}}
	pctx, cancel := context.WithTimeout(ctx, cfg.Timeout+5*time.Second)
	defer cancel()
	var ver string
	if err := db.QueryRowContext(pctx, "SELECT VERSION()").Scan(&ver); err != nil {
		db.Close()
		return nil, err
	}
	c.version = ver
	c.flavor = "mysql"
	if strings.Contains(strings.ToLower(ver), "mariadb") {
		c.flavor = "mariadb"
	}
	c.major, c.minor = parseVersion(ver)
	return c, nil
}

func parseVersion(v string) (int, int) {
	m := regexp.MustCompile(`^(\d+)\.(\d+)`).FindStringSubmatch(v)
	if m == nil {
		return 0, 0
	}
	a, _ := strconv.Atoi(m[1])
	b, _ := strconv.Atoi(m[2])
	return a, b
}

func (c *conn) Close() error                   { return c.db.Close() }
func (c *conn) Ping(ctx context.Context) error { return c.db.PingContext(ctx) }

func (c *conn) Server(ctx context.Context) (*driver.ServerInfo, error) {
	var user, db sql.NullString
	if err := c.db.QueryRowContext(ctx, "SELECT CURRENT_USER(), DATABASE()").Scan(&user, &db); err != nil {
		return nil, err
	}
	product := "MySQL"
	if c.flavor == "mariadb" {
		product = "MariaDB"
	}
	info := &driver.ServerInfo{Product: product, Version: c.version, User: user.String, Database: db.String, Extras: map[string]string{}}
	var comment sql.NullString
	if err := c.db.QueryRowContext(ctx, "SELECT @@version_comment").Scan(&comment); err == nil && comment.String != "" {
		info.Extras["Distribution"] = comment.String
	}
	if c.ro {
		info.Extras["Session"] = "read-only"
	}
	return info, nil
}

var systemDBs = map[string]bool{"mysql": true, "information_schema": true, "performance_schema": true, "sys": true}

func (c *conn) Databases(ctx context.Context) ([]driver.Database, error) {
	rows, err := c.db.QueryContext(ctx, `SELECT s.SCHEMA_NAME, s.DEFAULT_COLLATION_NAME,
		(SELECT SUM(t.DATA_LENGTH + t.INDEX_LENGTH) FROM information_schema.TABLES t WHERE t.TABLE_SCHEMA = s.SCHEMA_NAME),
		(SELECT COUNT(*) FROM information_schema.TABLES t WHERE t.TABLE_SCHEMA = s.SCHEMA_NAME)
		FROM information_schema.SCHEMATA s ORDER BY s.SCHEMA_NAME`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []driver.Database
	for rows.Next() {
		var name, coll sql.NullString
		var size, tables sql.NullInt64
		if err := rows.Scan(&name, &coll, &size, &tables); err != nil {
			return nil, err
		}
		out = append(out, driver.Database{Name: name.String, Collation: coll.String, Size: sqlbase.NullInt(size),
			Tables: sqlbase.NullInt(tables), System: systemDBs[strings.ToLower(name.String)]})
	}
	return out, rows.Err()
}

func (c *conn) Schemas(ctx context.Context, database string) ([]driver.Schema, error) { return nil, nil }

func (c *conn) Objects(ctx context.Context, s driver.Scope) ([]driver.Object, error) {
	if s.Database == "" {
		return nil, errors.New("choose a database")
	}
	var out []driver.Object
	rows, err := c.db.QueryContext(ctx, `SELECT TABLE_NAME, TABLE_TYPE, ENGINE, TABLE_ROWS, DATA_LENGTH + INDEX_LENGTH,
		TABLE_COMMENT, TABLE_COLLATION, COALESCE(UPDATE_TIME, CREATE_TIME)
		FROM information_schema.TABLES WHERE TABLE_SCHEMA = ? ORDER BY TABLE_NAME`, s.Database)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var name, typ, engine, comment, coll, updated sql.NullString
		var nrows, size sql.NullInt64
		if err := rows.Scan(&name, &typ, &engine, &nrows, &size, &comment, &coll, &updated); err != nil {
			rows.Close()
			return nil, err
		}
		// InnoDB statistics lag behind bulk loads; a zero estimate with data on
		// disk means "unknown", not "empty".
		if nrows.Valid && nrows.Int64 == 0 && size.Valid && size.Int64 > 16384 {
			nrows.Valid = false
		}
		kind := "table"
		if strings.Contains(typ.String, "VIEW") {
			kind = "view"
			comment.String = strings.TrimSuffix(comment.String, "VIEW")
		}
		out = append(out, driver.Object{Name: name.String, Kind: kind, Rows: sqlbase.NullInt(nrows), Size: sqlbase.NullInt(size),
			Engine: engine.String, Comment: comment.String, Collation: coll.String, Updated: updated.String})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Routines, triggers and events are best-effort: they need extra privileges.
	if r, err := c.db.QueryContext(ctx, `SELECT ROUTINE_NAME, ROUTINE_TYPE, COALESCE(ROUTINE_COMMENT, '') FROM information_schema.ROUTINES
		WHERE ROUTINE_SCHEMA = ? ORDER BY ROUTINE_NAME`, s.Database); err == nil {
		for r.Next() {
			var n, t, cm string
			if r.Scan(&n, &t, &cm) == nil {
				out = append(out, driver.Object{Name: n, Kind: strings.ToLower(t), Comment: cm})
			}
		}
		r.Close()
	}
	if r, err := c.db.QueryContext(ctx, `SELECT TRIGGER_NAME, CONCAT(ACTION_TIMING, ' ', EVENT_MANIPULATION, ' ON ', EVENT_OBJECT_TABLE)
		FROM information_schema.TRIGGERS WHERE TRIGGER_SCHEMA = ? ORDER BY TRIGGER_NAME`, s.Database); err == nil {
		for r.Next() {
			var n, x string
			if r.Scan(&n, &x) == nil {
				out = append(out, driver.Object{Name: n, Kind: "trigger", Extra: x})
			}
		}
		r.Close()
	}
	if r, err := c.db.QueryContext(ctx, `SELECT EVENT_NAME, STATUS FROM information_schema.EVENTS WHERE EVENT_SCHEMA = ? ORDER BY EVENT_NAME`, s.Database); err == nil {
		for r.Next() {
			var n, st string
			if r.Scan(&n, &st) == nil {
				out = append(out, driver.Object{Name: n, Kind: "event", Extra: st})
			}
		}
		r.Close()
	}
	return out, nil
}

func (c *conn) Browse(ctx context.Context, req driver.BrowseRequest) (*driver.Result, error) {
	t, err := c.Describe(ctx, req.Ref)
	if err != nil {
		return nil, err
	}
	return c.eng.Browse(ctx, t, req)
}

func (c *conn) Count(ctx context.Context, req driver.BrowseRequest) (driver.Count, error) {
	t, err := c.Describe(ctx, req.Ref)
	if err != nil {
		return driver.Count{}, err
	}
	if len(req.Filters) == 0 && req.Where == "" && req.Search == "" && t.RowEstimate != nil && *t.RowEstimate > 1_000_000 {
		// COUNT(*) on huge InnoDB tables scans everything; return the estimate.
		return driver.Count{Rows: *t.RowEstimate, Exact: false}, nil
	}
	return c.eng.Count(ctx, t, req)
}

func (c *conn) ApplyEdits(ctx context.Context, ref driver.ObjectRef, edits []driver.RowEdit) (*driver.EditResult, error) {
	if c.ro {
		return nil, driver.ErrReadOnly
	}
	t, err := c.Describe(ctx, ref)
	if err != nil {
		return nil, err
	}
	return c.eng.ApplyEdits(ctx, t, edits)
}

func (c *conn) NewSession(ctx context.Context, s driver.Scope) (driver.Session, error) {
	sc, err := c.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	if s.Database != "" {
		if _, err := sc.ExecContext(ctx, "USE "+quote(s.Database)); err != nil {
			sc.Close()
			return nil, err
		}
	}
	d := dialect{c: c}
	hooks := sqlbase.SessionHooks{
		MapError: mapError,
		Close: func(sc *sql.Conn) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, _ = sc.ExecContext(ctx, "ROLLBACK")
		},
	}
	if c.flavor == "mariadb" {
		hooks.InTx = func(ctx context.Context, sc *sql.Conn) (bool, error) {
			var n int
			err := sc.QueryRowContext(ctx, "SELECT @@in_transaction").Scan(&n)
			return n == 1, err
		}
	}
	return sqlbase.NewSession(sc, d, hooks), nil
}

var lineRe = regexp.MustCompile(`at line (\d+)`)

func mapError(err error, st sqlsplit.Statement) error {
	var me *my.MySQLError
	if errors.As(err, &me) {
		qe := &driver.QueryError{Message: me.Message, Code: strconv.Itoa(int(me.Number))}
		if m := lineRe.FindStringSubmatch(me.Message); m != nil {
			n, _ := strconv.Atoi(m[1])
			qe.Line = st.Line + n - 1
		}
		return qe
	}
	return err
}

func quote(name string) string { return "`" + strings.ReplaceAll(name, "`", "``") + "`" }

func qualify(db, name string) string {
	if db == "" {
		return quote(name)
	}
	return quote(db) + "." + quote(name)
}
