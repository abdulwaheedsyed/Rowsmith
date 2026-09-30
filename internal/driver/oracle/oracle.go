// Package oracle implements the Oracle Database driver on top of go-ora, a
// pure Go implementation of the Oracle Net protocol (no Instant Client).
//
// Oracle has no database level below the server: a schema is a user, so the
// hierarchy is schemas only and the default schema is the session user.
// There is no session-wide read-only switch either; read-only connections
// rely on the statement classifier (enforced by console sessions) and on
// ApplyEdits refusing to run.
package oracle

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	go_ora "github.com/sijms/go-ora/v2"
	"github.com/sijms/go-ora/v2/network"

	"rowsmith/internal/driver"
	"rowsmith/internal/driver/geo"
	"rowsmith/internal/driver/sqlbase"
)

type oracleDriver struct{}

func init() { driver.Register(oracleDriver{}) }

var kinds = []driver.KindInfo{
	{Kind: "table", Label: "Tables", Icon: "table", Browse: true},
	{Kind: "view", Label: "Views", Icon: "view", Browse: true},
	{Kind: "materialized_view", Label: "Materialized views", Icon: "view", Browse: true},
	{Kind: "sequence", Label: "Sequences", Icon: "sequence"},
	{Kind: "procedure", Label: "Procedures", Icon: "procedure"},
	{Kind: "function", Label: "Functions", Icon: "function"},
	{Kind: "package", Label: "Packages", Icon: "package"},
	{Kind: "trigger", Label: "Triggers", Icon: "trigger"},
	{Kind: "type", Label: "Types", Icon: "type"},
	{Kind: "synonym", Label: "Synonyms", Icon: "synonym"},
}

var types = []string{
	"NUMBER(10)", "NUMBER(19)", "NUMBER(12,2)", "NUMBER", "FLOAT", "BINARY_FLOAT", "BINARY_DOUBLE",
	"VARCHAR2(100 CHAR)", "VARCHAR2(4000 BYTE)", "NVARCHAR2(100)", "CHAR(1)", "CLOB", "NCLOB", "BLOB", "RAW(16)",
	"DATE", "TIMESTAMP", "TIMESTAMP WITH TIME ZONE", "TIMESTAMP WITH LOCAL TIME ZONE",
	"INTERVAL DAY TO SECOND", "INTERVAL YEAR TO MONTH", "BOOLEAN", "JSON", "XMLTYPE", "SDO_GEOMETRY",
}

func (oracleDriver) Info() driver.Info {
	net := driver.NetworkFields(1521, "", false) // host, port, user, password
	fields := []driver.Field{net[0], net[1],
		{Key: "service", Label: "Service name", Type: driver.FieldText, Required: true, Placeholder: "FREEPDB1", Span: 4,
			Help: "The service of the database (or pluggable database) to open, or its SID."},
		{Key: "serviceType", Label: "Connect by", Type: driver.FieldSelect, Default: "service", Span: 2,
			Options: []driver.Option{{Value: "service", Label: "Service name"}, {Value: "sid", Label: "SID"}}},
		net[2], net[3],
	}
	tls := driver.TLSFields("disable")
	tls[0].Options = nil
	for _, o := range driver.TLSModes {
		if o.Value != "prefer" {
			tls[0].Options = append(tls[0].Options, o)
		}
	}
	tls[0].Help = "Connects over TCPS, Oracle's TLS listener (often port 2484). There is no \"preferred\" mode: " +
		"TCPS and plain TCP are separate listeners. Wallets are not supported; use PEM certificates."
	fields = append(fields, tls...)
	fields = append(fields,
		driver.Field{Key: "role", Label: "Role", Type: driver.FieldSelect, Default: "normal", Section: "advanced", Span: 3,
			Options: []driver.Option{{Value: "normal", Label: "Normal"}, {Value: "sysdba", Label: "SYSDBA"}, {Value: "sysoper", Label: "SYSOPER"}}},
		driver.Field{Key: "connectTimeout", Label: "Connect timeout (seconds)", Type: driver.FieldNumber, Default: 15, Section: "advanced", Span: 3},
	)
	return driver.Info{
		ID: "oracle", Name: "Oracle", Order: 40,
		Description: "Oracle Database 12c and later: 19c, 21c, 23ai, Free, and Autonomous Database over TLS",
		Dialect:     "plsql", DefaultPort: 1521, Fields: fields, SSH: true,
		Caps: driver.Caps{Schemas: true, SQL: true, Transactions: true, EditRows: true, DDL: true, ForeignKeys: true,
			Explain: true, Processes: true, Variables: true, Users: true, Geometry: true},
		Kinds: kinds, Types: types, URLSchemes: []string{"oracle"}, QuoteChar: `"`,
		Design: &design,
	}
}

type conn struct {
	eng     *sqlbase.Engine
	db      *sql.DB
	user    string // session user, also the default schema
	version string
	release string // product line, e.g. "Oracle Database 19c Enterprise Edition"
	ro      bool
}

// sessionSetup gives every pooled connection ISO date formats and a "."
// decimal separator, so text conversions never depend on the client locale.
const sessionSetup = `ALTER SESSION SET NLS_DATE_FORMAT = 'YYYY-MM-DD HH24:MI:SS'
	NLS_TIMESTAMP_FORMAT = 'YYYY-MM-DD HH24:MI:SS.FF' NLS_TIMESTAMP_TZ_FORMAT = 'YYYY-MM-DD HH24:MI:SS.FF TZH:TZM'
	NLS_NUMERIC_CHARACTERS = '.,'`

func (oracleDriver) Open(ctx context.Context, p driver.OpenParams) (driver.Conn, error) {
	host, port := driver.HostPort(p, 1521)
	if host == "" {
		return nil, errors.New("host is required")
	}
	service := strings.TrimSpace(p.String("service"))
	if service == "" {
		return nil, errors.New("service name is required")
	}
	user := strings.TrimSpace(p.String("user"))
	if user == "" {
		return nil, errors.New("user is required")
	}
	timeout := time.Duration(p.Int("connectTimeout", 15)) * time.Second
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	opts := map[string]string{"CONNECT TIMEOUT": strconv.Itoa(int(timeout / time.Second)), "PREFETCH_ROWS": "250"}
	if p.AppName != "" {
		opts["PROGRAM"] = p.AppName
	}
	if p.String("serviceType") == "sid" {
		opts["SID"], service = service, ""
	}
	switch strings.ToLower(p.String("role")) {
	case "", "normal":
	case "sysdba":
		opts["DBA PRIVILEGE"] = "SYSDBA"
	case "sysoper":
		opts["DBA PRIVILEGE"] = "SYSOPER"
	default:
		return nil, fmt.Errorf("unknown role %q", p.String("role"))
	}
	tc, err := tlsConfig(p, host)
	if err != nil {
		return nil, err
	}
	if tc != nil {
		opts["SSL"] = "true"
	}
	inner := &connector{dsn: go_ora.BuildUrl(host, port, service, user, p.Secret("password"), opts), dial: p.Dial, tls: tc, timeout: timeout}
	db := sql.OpenDB(&sqlbase.InitConnector{Inner: inner, Init: []string{sessionSetup}})
	db.SetMaxOpenConns(12)
	db.SetMaxIdleConns(3)
	db.SetConnMaxIdleTime(5 * time.Minute)
	db.SetConnMaxLifetime(time.Hour)

	c := &conn{db: db, ro: p.ReadOnly}
	c.eng = &sqlbase.Engine{DB: db, D: dialect{c: c}, RowIDExpr: "ROWID"}
	pctx, cancel := context.WithTimeout(ctx, timeout+10*time.Second)
	defer cancel()
	if err := db.QueryRowContext(pctx, "SELECT USER FROM DUAL").Scan(&c.user); err != nil {
		db.Close()
		return nil, cleanErr(err)
	}
	c.version, c.release = serverVersion(pctx, db)
	if major, _ := strconv.Atoi(strings.SplitN(c.version, ".", 2)[0]); major > 0 && major < 12 {
		db.Close()
		return nil, fmt.Errorf("Oracle Database %s is not supported; version 12c or later is required", c.version)
	}
	return c, nil
}

// serverVersion reads the full version (VERSION_FULL exists from 18c on).
func serverVersion(ctx context.Context, db *sql.DB) (version, release string) {
	for _, col := range []string{"VERSION_FULL", "VERSION"} {
		var v, p sql.NullString
		err := db.QueryRowContext(ctx, "SELECT "+col+", PRODUCT FROM PRODUCT_COMPONENT_VERSION WHERE PRODUCT LIKE 'Oracle%'").Scan(&v, &p)
		if err == nil {
			return v.String, strings.TrimSpace(p.String)
		}
	}
	return "", ""
}

func (c *conn) Close() error                   { return c.db.Close() }
func (c *conn) Ping(ctx context.Context) error { return cleanErr(c.db.PingContext(ctx)) }

func (c *conn) Server(ctx context.Context) (*driver.ServerInfo, error) {
	var user, container, service sql.NullString
	if err := c.db.QueryRowContext(ctx, `SELECT USER, SYS_CONTEXT('USERENV', 'CON_NAME'), SYS_CONTEXT('USERENV', 'SERVICE_NAME') FROM DUAL`).
		Scan(&user, &container, &service); err != nil {
		return nil, cleanErr(err)
	}
	info := &driver.ServerInfo{Product: "Oracle Database", Version: c.version, User: user.String, Extras: map[string]string{}}
	for k, v := range map[string]string{"Release": c.release, "Container": container.String, "Service": service.String} {
		if v != "" {
			info.Extras[k] = v
		}
	}
	if c.ro {
		info.Extras["Session"] = "read-only"
	}
	return info, nil
}

func (c *conn) Databases(ctx context.Context) ([]driver.Database, error) { return nil, nil }

// schema resolves an empty scope to the session user's schema.
func (c *conn) schema(s string) string {
	if s == "" {
		return c.user
	}
	return s
}

func (c *conn) Schemas(ctx context.Context, database string) ([]driver.Schema, error) {
	rows, err := c.db.QueryContext(ctx, `SELECT USERNAME, ORACLE_MAINTAINED FROM ALL_USERS
		ORDER BY CASE WHEN USERNAME = USER THEN 0 ELSE 1 END, ORACLE_MAINTAINED, USERNAME`)
	if err != nil {
		return nil, cleanErr(err)
	}
	defer rows.Close()
	var out []driver.Schema
	for rows.Next() {
		var name, maintained sql.NullString
		if err := rows.Scan(&name, &maintained); err != nil {
			return nil, err
		}
		out = append(out, driver.Schema{Name: name.String, System: maintained.String == "Y"})
	}
	return out, rows.Err()
}

var codeKinds = map[string]string{
	"SEQUENCE": "sequence", "PROCEDURE": "procedure", "FUNCTION": "function", "PACKAGE": "package",
	"TRIGGER": "trigger", "TYPE": "type", "SYNONYM": "synonym",
}

func (c *conn) Objects(ctx context.Context, s driver.Scope) ([]driver.Object, error) {
	schema := c.schema(s.Schema)
	sizes := c.segmentSizes(ctx, schema, "")
	size := func(name string) *int64 {
		if n, ok := sizes[name]; ok {
			return &n
		}
		return nil
	}
	var out []driver.Object
	// Nested tables, overflow segments, the recycle bin and materialized view
	// containers are implementation details, not user tables.
	rows, err := c.db.QueryContext(ctx, `SELECT t.TABLE_NAME, t.NUM_ROWS, t.TEMPORARY, t.PARTITIONED, t.IOT_TYPE, c.COMMENTS
		FROM ALL_TABLES t LEFT JOIN ALL_TAB_COMMENTS c ON c.OWNER = t.OWNER AND c.TABLE_NAME = t.TABLE_NAME
		WHERE t.OWNER = :1 AND t.NESTED = 'NO' AND t.SECONDARY = 'N' AND t.DROPPED = 'NO' AND (t.IOT_TYPE IS NULL OR t.IOT_TYPE = 'IOT')
		AND NOT EXISTS (SELECT 1 FROM ALL_MVIEWS m WHERE m.OWNER = t.OWNER AND m.MVIEW_NAME = t.TABLE_NAME)
		ORDER BY t.TABLE_NAME`, schema)
	if err != nil {
		return nil, cleanErr(err)
	}
	for rows.Next() {
		var name, temp, part, iot, comment sql.NullString
		var n sql.NullInt64
		if err := rows.Scan(&name, &n, &temp, &part, &iot, &comment); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, driver.Object{Name: name.String, Kind: "table", Rows: sqlbase.NullInt(n), Size: size(name.String),
			Comment: comment.String, Extra: tableTraits(temp.String, part.String, iot.String)})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, cleanErr(err)
	}

	// Views, materialized views and code objects are best-effort extras.
	if r, err := c.db.QueryContext(ctx, `SELECT v.VIEW_NAME, c.COMMENTS FROM ALL_VIEWS v
		LEFT JOIN ALL_TAB_COMMENTS c ON c.OWNER = v.OWNER AND c.TABLE_NAME = v.VIEW_NAME
		WHERE v.OWNER = :1 ORDER BY v.VIEW_NAME`, schema); err == nil {
		for r.Next() {
			var name, comment sql.NullString
			if r.Scan(&name, &comment) == nil {
				out = append(out, driver.Object{Name: name.String, Kind: "view", Comment: comment.String})
			}
		}
		r.Close()
	}
	if r, err := c.db.QueryContext(ctx, `SELECT m.MVIEW_NAME, t.NUM_ROWS, c.COMMENTS, m.STALENESS FROM ALL_MVIEWS m
		LEFT JOIN ALL_TABLES t ON t.OWNER = m.OWNER AND t.TABLE_NAME = m.CONTAINER_NAME
		LEFT JOIN ALL_MVIEW_COMMENTS c ON c.OWNER = m.OWNER AND c.MVIEW_NAME = m.MVIEW_NAME
		WHERE m.OWNER = :1 ORDER BY m.MVIEW_NAME`, schema); err == nil {
		for r.Next() {
			var name, comment, stale sql.NullString
			var n sql.NullInt64
			if r.Scan(&name, &n, &comment, &stale) == nil {
				out = append(out, driver.Object{Name: name.String, Kind: "materialized_view", Rows: sqlbase.NullInt(n),
					Size: size(name.String), Comment: comment.String, Extra: strings.ToLower(stale.String)})
			}
		}
		r.Close()
	}
	extras := c.objectExtras(ctx, schema)
	if r, err := c.db.QueryContext(ctx, `SELECT OBJECT_NAME, OBJECT_TYPE, STATUS FROM ALL_OBJECTS
		WHERE OWNER = :1 AND OBJECT_TYPE IN ('SEQUENCE', 'PROCEDURE', 'FUNCTION', 'PACKAGE', 'TRIGGER', 'TYPE', 'SYNONYM')
		AND GENERATED = 'N' AND OBJECT_NAME NOT LIKE 'BIN$%' ORDER BY OBJECT_NAME`, schema); err == nil {
		for r.Next() {
			var name, typ, status sql.NullString
			if r.Scan(&name, &typ, &status) != nil {
				continue
			}
			o := driver.Object{Name: name.String, Kind: codeKinds[typ.String], Extra: extras[typ.String+"."+name.String]}
			if status.String == "INVALID" {
				o.Extra = strings.TrimPrefix(o.Extra+", invalid", ", ")
			}
			out = append(out, o)
		}
		r.Close()
	}
	return out, nil
}

func tableTraits(temporary, partitioned, iot string) string {
	var t []string
	if temporary == "Y" {
		t = append(t, "global temporary")
	}
	if partitioned == "YES" {
		t = append(t, "partitioned")
	}
	if iot == "IOT" {
		t = append(t, "index-organized")
	}
	return strings.Join(t, ", ")
}

// objectExtras describes triggers and synonyms, keyed by "TYPE.NAME".
func (c *conn) objectExtras(ctx context.Context, schema string) map[string]string {
	out := map[string]string{}
	if r, err := c.db.QueryContext(ctx, `SELECT TRIGGER_NAME, TRIGGER_TYPE, TRIGGERING_EVENT, TABLE_NAME FROM ALL_TRIGGERS WHERE OWNER = :1`, schema); err == nil {
		for r.Next() {
			var name, typ, event, table sql.NullString
			if r.Scan(&name, &typ, &event, &table) == nil {
				x := strings.TrimSpace(typ.String + " " + event.String)
				if table.String != "" {
					x += " ON " + table.String
				}
				out["TRIGGER."+name.String] = x
			}
		}
		r.Close()
	}
	if r, err := c.db.QueryContext(ctx, `SELECT SYNONYM_NAME, TABLE_OWNER, TABLE_NAME, DB_LINK FROM ALL_SYNONYMS WHERE OWNER = :1`, schema); err == nil {
		for r.Next() {
			var name, owner, table, link sql.NullString
			if r.Scan(&name, &owner, &table, &link) == nil {
				out["SYNONYM."+name.String] = "for " + synonymTarget(owner.String, table.String, link.String)
			}
		}
		r.Close()
	}
	return out
}

func synonymTarget(owner, name, link string) string {
	t := ident(name)
	if owner != "" {
		t = ident(owner) + "." + t
	}
	if link != "" {
		t += "@" + link
	}
	return t
}

// segmentSizes sums table, index and LOB segments per table (of one table
// when table is set). Other schemas' segments are only visible with DBA
// privileges, so sizes are simply omitted without them.
func (c *conn) segmentSizes(ctx context.Context, schema, table string) map[string]int64 {
	const name = "COALESCE(l.TABLE_NAME, i.TABLE_NAME, s.SEGMENT_NAME)"
	q := `SELECT ` + name + `, SUM(s.BYTES) FROM USER_SEGMENTS s
		LEFT JOIN USER_INDEXES i ON i.INDEX_NAME = s.SEGMENT_NAME
		LEFT JOIN USER_LOBS l ON l.SEGMENT_NAME = s.SEGMENT_NAME OR l.INDEX_NAME = s.SEGMENT_NAME`
	var where []string
	var args []any
	if schema != c.user {
		q = `SELECT ` + name + `, SUM(s.BYTES) FROM DBA_SEGMENTS s
			LEFT JOIN ALL_INDEXES i ON i.OWNER = s.OWNER AND i.INDEX_NAME = s.SEGMENT_NAME
			LEFT JOIN ALL_LOBS l ON l.OWNER = s.OWNER AND (l.SEGMENT_NAME = s.SEGMENT_NAME OR l.INDEX_NAME = s.SEGMENT_NAME)`
		args = append(args, schema)
		where = append(where, "s.OWNER = :1")
	}
	if table != "" {
		args = append(args, table)
		where = append(where, name+" = :"+strconv.Itoa(len(args)))
	}
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	out := map[string]int64{}
	rows, err := c.db.QueryContext(ctx, q+" GROUP BY "+name, args...)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var name sql.NullString
		var n sql.NullInt64
		if rows.Scan(&name, &n) == nil && n.Valid {
			out[name.String] = n.Int64
		}
	}
	return out
}

func (c *conn) Browse(ctx context.Context, req driver.BrowseRequest) (*driver.Result, error) {
	t, err := c.describe(ctx, req.Ref, false)
	if err != nil {
		return nil, err
	}
	res, err := c.eng.Browse(ctx, t, req)
	if err != nil {
		return nil, cleanErr(err)
	}
	restoreColumns(t, res)
	return res, nil
}

// restoreColumns labels browse results with the declared column types and
// converts values the wire format cannot express: BOOLEAN arrives as a
// number, SDO_GEOMETRY as WKT text.
func restoreColumns(t *driver.Table, res *driver.Result) {
	byName := map[string]*driver.Column{}
	for i := range t.Columns {
		byName[t.Columns[i].Name] = &t.Columns[i]
	}
	for i := range res.Columns {
		res.Columns[i].Type = sqlTypeName(res.Columns[i].Type)
		col, ok := byName[res.Columns[i].Name]
		if !ok {
			continue
		}
		res.Columns[i].Type = col.Type
		switch col.Kind {
		case driver.KindBool, driver.KindGeometry, driver.KindJSON:
			res.Columns[i].Kind = col.Kind
		default:
			continue
		}
		for _, row := range res.Rows {
			switch col.Kind {
			case driver.KindBool:
				if s, ok := row[i].(string); ok {
					row[i] = s != "0"
				}
			case driver.KindGeometry:
				if s, ok := row[i].(string); ok {
					if cell, ok := geo.FromWKT(ewkt(s, col.SRID)); ok {
						row[i] = cell
					}
				}
			}
		}
	}
}

// ewkt prefixes well-known text with an SRID when one is known.
func ewkt(wkt string, srid int) string {
	if srid == 0 {
		return wkt
	}
	return "SRID=" + strconv.Itoa(srid) + ";" + wkt
}

func (c *conn) Count(ctx context.Context, req driver.BrowseRequest) (driver.Count, error) {
	t, err := c.describe(ctx, req.Ref, false)
	if err != nil {
		return driver.Count{}, err
	}
	if len(req.Filters) == 0 && req.Where == "" && req.Search == "" && t.RowEstimate != nil && *t.RowEstimate > 2_000_000 {
		// Optimizer statistics are good enough for huge tables.
		return driver.Count{Rows: *t.RowEstimate, Exact: false}, nil
	}
	n, err := c.eng.Count(ctx, t, req)
	return n, cleanErr(err)
}

func (c *conn) ApplyEdits(ctx context.Context, ref driver.ObjectRef, edits []driver.RowEdit) (*driver.EditResult, error) {
	if c.ro {
		return nil, driver.ErrReadOnly
	}
	t, err := c.describe(ctx, ref, false)
	if err != nil {
		return nil, err
	}
	// Oracle has no INSERT ... DEFAULT VALUES; an empty insert names one
	// insertable column explicitly instead.
	for i, ed := range edits {
		if ed.Op == "insert" && len(ed.Values) == 0 {
			for _, col := range t.Columns {
				if col.Generated == "" {
					edits[i].Values = map[string]any{col.Name: map[string]any{"$default": true}}
					break
				}
			}
		}
	}
	res, err := c.eng.ApplyEdits(ctx, t, edits)
	return res, cleanErr(err)
}

// ---- helpers -------------------------------------------------------------------

func quote(n string) string { return `"` + strings.ReplaceAll(n, `"`, `""`) + `"` }

func qualify(schema, name string) string { return quote(schema) + "." + quote(name) }

func literal(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

var plainIdent = regexp.MustCompile(`^[A-Z][A-Z0-9_$#]*$`)

// ident renders a name the way Oracle tools print it: bare when it needs no
// quotes, quoted otherwise.
func ident(n string) string {
	if plainIdent.MatchString(n) {
		return n
	}
	return quote(n)
}

// cleanErr turns go-ora errors into query errors without the driver's
// "error occur at position" suffix, keeping any context prefix.
func cleanErr(err error) error {
	var oe *network.OracleError
	if err == nil || !errors.As(err, &oe) {
		return err
	}
	full := oe.Error()
	qe := &driver.QueryError{Message: strings.TrimSpace(oe.ErrMsg), Code: oraCode(oe.ErrCode)}
	if msg := err.Error(); msg != full && strings.HasSuffix(msg, full) {
		qe.Message = strings.TrimSuffix(msg, full) + qe.Message
	}
	return qe
}

func oraCode(n int) string { return fmt.Sprintf("ORA-%05d", n) }

// privErr explains missing dictionary privileges instead of ORA-00942.
func privErr(err error, what, views string) error {
	var oe *network.OracleError
	if errors.As(err, &oe) && (oe.ErrCode == 942 || oe.ErrCode == 1031) {
		return fmt.Errorf("%s needs SELECT access to %s (for example through the SELECT_CATALOG_ROLE role)", what, views)
	}
	return cleanErr(err)
}

// collect runs an administrative query and labels its columns with SQL type names.
func (c *conn) collect(ctx context.Context, q string, args ...any) (*driver.Result, error) {
	res, err := sqlbase.Collect(ctx, dialect{c: c}, c.db, 0, q, args...)
	if err != nil {
		return nil, err
	}
	for i := range res.Columns {
		res.Columns[i].Type = sqlTypeName(res.Columns[i].Type)
	}
	return res, nil
}
