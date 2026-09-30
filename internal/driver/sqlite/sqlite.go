// Package sqlite implements the SQLite driver on top of modernc.org/sqlite.
//
// SQLite files live on the Rowsmith server itself, so the driver only opens
// files inside one directory: ROWSMITH_SQLITE_DIR, defaulting to "sqlite"
// inside ROWSMITH_DATA_DIR (itself defaulting to /data). Names are resolved
// relative to it, symbolic links included, and Rowsmith's own rowsmith.db is
// refused. Every connection also runs with SQLITE_LIMIT_ATTACHED set to zero,
// so SQL cannot ATTACH or VACUUM INTO files elsewhere. The special name
// ":memory:" opens a private scratch database that lives as long as the pool.
package sqlite

import (
	"context"
	"crypto/rand"
	"database/sql"
	sqldriver "database/sql/driver"
	"errors"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	msqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"rowsmith/internal/driver"
	"rowsmith/internal/driver/sqlbase"
	"rowsmith/internal/sqlsplit"
)

type sqliteDriver struct{}

func init() { driver.Register(sqliteDriver{}) }

// memoryName selects an in-memory database instead of a file.
const memoryName = ":memory:"

var kinds = []driver.KindInfo{
	{Kind: "table", Label: "Tables", Icon: "table", Browse: true},
	{Kind: "view", Label: "Views", Icon: "view", Browse: true},
	{Kind: "index", Label: "Indexes", Icon: "index"},
	{Kind: "trigger", Label: "Triggers", Icon: "trigger"},
}

var types = []string{
	"integer", "real", "text", "blob", "numeric", "boolean", "date", "datetime", "varchar(255)", "decimal(10,2)", "json",
}

// design describes the structure editor; ddlgen.go generates the SQL.
var design = driver.TableDesign{
	Columns: true, ReorderColumns: true, AutoIncrement: true, Collation: true, Generated: true, GeneratedVirtual: true,
	Checks: true, PrimaryKey: true, ForeignKeys: true, Indexes: true, PartialIndexes: true,
	FKActions: []string{"NO ACTION", "RESTRICT", "CASCADE", "SET NULL", "SET DEFAULT"},
	Options: []driver.Field{
		{Key: "without_rowid", Label: "WITHOUT ROWID", Type: driver.FieldBool, Span: 3,
			Help: "Store rows in primary key order, without the hidden rowid. Needs a primary key."},
		{Key: "strict", Label: "STRICT", Type: driver.FieldBool, Span: 3,
			Help: "Reject values that do not match the column type. Types must be INTEGER, INT, REAL, TEXT, BLOB or ANY."},
	},
	Note: "SQLite changes a table in place only to rename it, add, drop or rename columns, switch NOT NULL and add or drop named checks. " +
		"Any other change rebuilds the table: the rows are copied into a new table with the new structure, and its indexes and triggers are recreated.",
}

func (sqliteDriver) Info() driver.Info {
	return driver.Info{
		ID: "sqlite", Name: "SQLite", Order: 50,
		Description: "SQLite 3 database files stored on the Rowsmith server",
		Dialect:     "sqlite",
		Fields: []driver.Field{
			{Key: "file", Label: "Database file", Type: driver.FieldText, Required: true, Placeholder: "reports/sales.db",
				Help: "Path inside the server's SQLite directory (ROWSMITH_SQLITE_DIR), or :memory: for a private scratch database."},
			{Key: "create", Label: "Create the file if it does not exist", Type: driver.FieldBool, Span: 3},
			{Key: "readOnly", Label: "Open read-only", Type: driver.FieldBool, Span: 3},
		},
		Caps:  driver.Caps{SQL: true, Transactions: true, EditRows: true, DDL: true, ForeignKeys: true, Explain: true, Variables: true},
		Kinds: kinds, Types: types, URLSchemes: []string{"sqlite", "file"}, QuoteChar: `"`,
		Design: &design,
	}
}

type conn struct {
	eng  *sqlbase.Engine
	db   *sql.DB
	name string // as entered, for display
	ro   bool
	// anchor keeps an in-memory database alive while pooled connections come and go.
	anchor sqldriver.Conn
}

func (sqliteDriver) Open(ctx context.Context, p driver.OpenParams) (driver.Conn, error) {
	name := strings.TrimSpace(p.String("file"))
	ro := p.ReadOnly || p.Bool("readOnly")
	q := url.Values{"_pragma": {"busy_timeout(5000)", "foreign_keys(1)"}, "_txlock": {"immediate"}}
	if ro {
		q.Add("_pragma", "query_only(1)")
	}
	var path string
	if name == memoryName {
		// The memdb VFS shares one database between the pool's connections;
		// a random name keeps it private to this pool.
		path = "/rowsmith-" + rand.Text()
		q.Set("vfs", "memdb")
	} else {
		var err error
		if path, err = resolve(root(), name, p.Bool("create") && !ro); err != nil {
			return nil, err
		}
		switch {
		case ro:
			q.Set("mode", "ro")
		case p.Bool("create"):
			q.Set("mode", "rwc")
		default:
			q.Set("mode", "rw")
		}
	}
	inner, err := msqlite.NewConnector((&url.URL{Scheme: "file", Path: path, RawQuery: q.Encode()}).String())
	if err != nil {
		return nil, err
	}
	cn := connector{inner}
	c := &conn{name: name, ro: ro}
	if name == memoryName {
		if c.anchor, err = cn.Connect(ctx); err != nil {
			return nil, err
		}
	}
	c.db = sql.OpenDB(cn)
	c.db.SetMaxOpenConns(8)
	c.db.SetMaxIdleConns(2)
	c.db.SetConnMaxIdleTime(5 * time.Minute)
	c.eng = &sqlbase.Engine{DB: c.db, D: dialect{}, RowIDExpr: "rowid"}

	// Reading the schema fails early on files that are not databases.
	var n int
	if err := c.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_schema").Scan(&n); err != nil {
		c.Close()
		return nil, mapError(err, sqlsplit.Statement{})
	}
	return c, nil
}

// connector opens connections with SQLITE_LIMIT_ATTACHED set to zero, which
// makes ATTACH and VACUUM INTO fail: both would open arbitrary server paths.
type connector struct{ inner sqldriver.Connector }

func (c connector) Connect(ctx context.Context) (sqldriver.Conn, error) {
	dc, err := c.inner.Connect(ctx)
	if err != nil {
		return nil, err
	}
	if err := forbidAttach(ctx, c.inner, dc); err != nil {
		dc.Close()
		return nil, err
	}
	return dc, nil
}

func (c connector) Driver() sqldriver.Driver { return c.inner.Driver() }

// forbidAttach applies the limit to a new connection. modernc only exposes
// sqlite3_limit through a *sql.Conn, so dc is lent to a throwaway pool; that
// pool is closed while dc is still checked out, and database/sql never closes
// a checked-out connection, so dc stays open for the caller.
func forbidAttach(ctx context.Context, inner sqldriver.Connector, dc sqldriver.Conn) error {
	tmp := sql.OpenDB(lent{inner, dc})
	defer tmp.Close()
	sc, err := tmp.Conn(ctx)
	if err != nil {
		return err
	}
	_, err = msqlite.Limit(sc, sqlite3.SQLITE_LIMIT_ATTACHED, 0)
	return err
}

// lent is a connector that hands out one existing connection.
type lent struct {
	sqldriver.Connector
	dc sqldriver.Conn
}

func (l lent) Connect(context.Context) (sqldriver.Conn, error) { return l.dc, nil }

func (c *conn) Close() error {
	err := c.db.Close()
	if c.anchor != nil {
		c.anchor.Close()
	}
	return err
}

func (c *conn) Ping(ctx context.Context) error { return c.db.PingContext(ctx) }

func (c *conn) Server(ctx context.Context) (*driver.ServerInfo, error) {
	var version, journal string
	if err := c.db.QueryRowContext(ctx, "SELECT sqlite_version(), (SELECT journal_mode FROM pragma_journal_mode)").Scan(&version, &journal); err != nil {
		return nil, err
	}
	info := &driver.ServerInfo{Product: "SQLite", Version: version, Database: c.name, Extras: map[string]string{"Journal mode": journal}}
	if c.ro {
		info.Extras["Session"] = "read-only"
	}
	return info, nil
}

func (c *conn) Databases(ctx context.Context) ([]driver.Database, error) { return nil, nil }

func (c *conn) Schemas(ctx context.Context, database string) ([]driver.Schema, error) {
	return nil, nil
}

func (c *conn) Objects(ctx context.Context, _ driver.Scope) ([]driver.Object, error) {
	rows, err := c.db.QueryContext(ctx, `SELECT type, name, tbl_name, COALESCE(sql, '') FROM sqlite_schema
		WHERE type IN ('table', 'view', 'index', 'trigger') AND name NOT LIKE 'sqlite\_%' ESCAPE '\' ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []driver.Object
	for rows.Next() {
		var o driver.Object
		var table, ddl string
		if err := rows.Scan(&o.Kind, &o.Name, &table, &ddl); err != nil {
			return nil, err
		}
		switch o.Kind {
		case "index":
			o.Extra = "ON " + table
		case "trigger":
			tr := parseTrigger(ddl)
			o.Extra = tr.Timing + " " + tr.Event + " ON " + table
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

func (c *conn) Browse(ctx context.Context, req driver.BrowseRequest) (*driver.Result, error) {
	if err := checkFragment(req.Where); err != nil {
		return nil, err
	}
	t, err := c.Describe(ctx, req.Ref)
	if err != nil {
		return nil, err
	}
	res, err := c.eng.Browse(ctx, t, req)
	if err != nil {
		return nil, err
	}
	// Columns selected through SelectExpr lose their declared type.
	byName := map[string]driver.Column{}
	for _, col := range t.Columns {
		byName[col.Name] = col
	}
	for i, rc := range res.Columns {
		if col, ok := byName[rc.Name]; ok {
			res.Columns[i].Type, res.Columns[i].Kind = col.Type, col.Kind
		}
	}
	return res, nil
}

func (c *conn) Count(ctx context.Context, req driver.BrowseRequest) (driver.Count, error) {
	if err := checkFragment(req.Where); err != nil {
		return driver.Count{}, err
	}
	t, err := c.Describe(ctx, req.Ref)
	if err != nil {
		return driver.Count{}, err
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

// vacuumRe matches a plain VACUUM, which rebuilds the file through a
// temporary attached database and so needs one ATTACH slot.
var vacuumRe = regexp.MustCompile(`(?i)^VACUUM(\s+(main|temp))?$`)

func (c *conn) NewSession(ctx context.Context, _ driver.Scope) (driver.Session, error) {
	pinned, err := c.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	hooks := sqlbase.SessionHooks{
		Prepare: func(_ context.Context, sc *sql.Conn, st sqlsplit.Statement, _ driver.ExecOptions) (string, error) {
			if err := guard(st.SQL); err != nil {
				return "", err
			}
			if vacuumRe.MatchString(strings.TrimSpace(st.SQL)) {
				_, err := msqlite.Limit(sc, sqlite3.SQLITE_LIMIT_ATTACHED, 1)
				return "", err
			}
			return "", nil
		},
		// Re-arm the ATTACH ban after every statement, since VACUUM lifts it.
		After: func(_ context.Context, sc *sql.Conn, _ driver.Sink) {
			_, _ = msqlite.Limit(sc, sqlite3.SQLITE_LIMIT_ATTACHED, 0)
		},
		MapError: mapError,
	}
	return sqlbase.NewSession(pinned, dialect{}, hooks), nil
}

// guard refuses PRAGMA temp_store_directory: it redirects the temporary
// files of every SQLite database in the process, Rowsmith's own included,
// and takes effect as soon as the statement is compiled.
func guard(s string) error {
	if strings.Contains(strings.ToLower(s), "temp_store_directory") {
		return errors.New("PRAGMA temp_store_directory is not allowed: it affects every database on the Rowsmith server")
	}
	return nil
}

// checkFragment vets SQL that Rowsmith embeds in its own statements (browse
// conditions, edit expressions, EXPLAIN input). modernc runs every statement
// of a multi-statement string, so a fragment must not smuggle in another one.
func checkFragment(s string) error {
	if len(sqlsplit.Split(s, sqlsplit.SQLite)) > 1 {
		return errors.New("only a single SQL expression or statement is allowed here")
	}
	return guard(s)
}

func mapError(err error, _ sqlsplit.Statement) error {
	var se *msqlite.Error
	if !errors.As(err, &se) {
		return err
	}
	// modernc formats errors as "<error class>: <message> (<code>)".
	msg := strings.TrimSuffix(se.Error(), " (SQLITE_BUSY)")
	msg = strings.TrimSuffix(msg, " ("+strconv.Itoa(se.Code())+")")
	if _, rest, ok := strings.Cut(msg, ": "); ok && rest != "" {
		msg = rest
	}
	if strings.HasPrefix(msg, "too many attached databases") {
		msg = "ATTACH and VACUUM INTO are disabled: Rowsmith only opens SQLite files inside its SQLite directory"
	}
	return &driver.QueryError{Message: msg, Code: strconv.Itoa(se.Code())}
}

func quote(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }
