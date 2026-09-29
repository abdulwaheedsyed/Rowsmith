// Package postgres implements the PostgreSQL driver (including PostGIS,
// TimescaleDB, Aurora/AlloyDB/Cloud SQL and other wire-compatible servers).
package postgres

import (
	"context"
	"database/sql"
	"errors"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/stdlib"

	"rowsmith/internal/driver"
	"rowsmith/internal/driver/sqlbase"
	"rowsmith/internal/sqlsplit"
)

type pgDriver struct{}

func init() { driver.Register(pgDriver{}) }

var kinds = []driver.KindInfo{
	{Kind: "table", Label: "Tables", Icon: "table", Browse: true},
	{Kind: "partitioned_table", Label: "Partitioned tables", Icon: "table", Browse: true},
	{Kind: "view", Label: "Views", Icon: "view", Browse: true},
	{Kind: "materialized_view", Label: "Materialized views", Icon: "view", Browse: true},
	{Kind: "foreign_table", Label: "Foreign tables", Icon: "table", Browse: true},
	{Kind: "sequence", Label: "Sequences", Icon: "sequence"},
	{Kind: "function", Label: "Functions", Icon: "function"},
	{Kind: "procedure", Label: "Procedures", Icon: "procedure"},
	{Kind: "type", Label: "Types", Icon: "type"},
	{Kind: "extension", Label: "Extensions", Icon: "extension"},
}

var types = []string{
	"integer", "bigint", "smallint", "serial", "bigserial", "numeric(12,2)", "real", "double precision", "boolean",
	"text", "varchar(255)", "char(10)", "uuid", "jsonb", "json", "date", "timestamp", "timestamptz", "time", "interval",
	"bytea", "inet", "cidr", "text[]", "integer[]", "tsvector", "geometry(Point,4326)", "geometry(Polygon,4326)",
	"geometry(LineString,4326)", "geometry(MultiPolygon,4326)", "geography(Point,4326)",
}

func (pgDriver) Info() driver.Info {
	fields := driver.NetworkFields(5432, "Database", false)
	fields = append(fields, driver.TLSFields("prefer")...)
	fields = append(fields,
		driver.Field{Key: "connectTimeout", Label: "Connect timeout (seconds)", Type: driver.FieldNumber, Default: 15, Section: "advanced", Span: 3},
		driver.Field{Key: "searchPath", Label: "Search path", Type: driver.FieldText, Placeholder: "e.g. app, public", Section: "advanced", Span: 3},
		driver.Field{Key: "onlyDatabase", Label: "Only show the configured database", Type: driver.FieldBool, Section: "advanced"},
	)
	return driver.Info{
		ID: "postgres", Name: "PostgreSQL", Order: 20,
		Description: "PostgreSQL 10+ with PostGIS, TimescaleDB, Supabase, Neon, Aurora, AlloyDB, Cloud SQL",
		Dialect:     "postgresql", DefaultPort: 5432, Fields: fields, SSH: true,
		Caps: driver.Caps{Databases: true, Schemas: true, SQL: true, Transactions: true, EditRows: true, DDL: true,
			CreateDatabase: true, ForeignKeys: true, Explain: true, Processes: true, Variables: true, Users: true,
			Geometry: true, Dump: true},
		Kinds: kinds, Types: types, URLSchemes: []string{"postgres", "postgresql"}, QuoteChar: `"`,
	}
}

// conn keeps one pool per database, since a PostgreSQL connection is bound
// to a single database.
type conn struct {
	base      *pgx.ConnConfig
	defaultDB string
	only      bool
	ro        bool

	mu    sync.Mutex
	pools map[string]*pool

	notices noticeBuffer
}

type pool struct {
	db      *sql.DB
	eng     *sqlbase.Engine
	postgis string // version, "" when absent
	server  int    // server_version_num
}

func (pgDriver) Open(ctx context.Context, p driver.OpenParams) (driver.Conn, error) {
	host, port := driver.HostPort(p, 5432)
	if host == "" {
		return nil, errors.New("host is required")
	}
	cfg, err := pgx.ParseConfig("")
	if err != nil {
		return nil, err
	}
	cfg.Host, cfg.Port = host, uint16(port)
	cfg.User = p.String("user")
	cfg.Password = p.Secret("password")
	cfg.ConnectTimeout = time.Duration(p.Int("connectTimeout", 15)) * time.Second
	cfg.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	cfg.RuntimeParams = map[string]string{"application_name": p.AppName}
	if sp := strings.TrimSpace(p.String("searchPath")); sp != "" {
		cfg.RuntimeParams["search_path"] = sp
	}
	if p.ReadOnly {
		cfg.RuntimeParams["default_transaction_read_only"] = "on"
	}
	cfg.Fallbacks = nil
	if p.Dial != nil {
		dial := p.Dial
		cfg.DialFunc = func(ctx context.Context, network, addr string) (net.Conn, error) { return dial(ctx, "tcp", addr) }
		// Resolve names on the far side of the tunnel, not here.
		cfg.LookupFunc = func(ctx context.Context, host string) ([]string, error) { return []string{host}, nil }
	}
	mode := p.String("tls")
	switch mode {
	case "", "disable":
		cfg.TLSConfig = nil
	default:
		tc, err := driver.TLSConfig(p, host)
		if err != nil {
			return nil, err
		}
		cfg.TLSConfig = tc
		if mode == "prefer" {
			cfg.Fallbacks = []*pgconn.FallbackConfig{{Host: host, Port: uint16(port)}}
		}
	}
	c := &conn{base: cfg, defaultDB: p.String("database"), only: p.Bool("onlyDatabase"), ro: p.ReadOnly, pools: map[string]*pool{}}
	c.notices.init()
	cfg.OnNotice = c.notices.handler
	if c.defaultDB == "" {
		c.defaultDB = "postgres"
	}
	if _, err := c.pool(ctx, c.defaultDB); err != nil {
		return nil, err
	}
	return c, nil
}

// extraTypes are registered as text so they surface with readable names.
var extraTypes = []string{"geometry", "geography", "box2d", "box3d", "raster", "citext", "hstore", "ltree", "vector", "halfvec", "sparsevec"}

func (c *conn) pool(ctx context.Context, database string) (*pool, error) {
	if database == "" {
		database = c.defaultDB
	}
	c.mu.Lock()
	if p, ok := c.pools[database]; ok {
		c.mu.Unlock()
		return p, nil
	}
	c.mu.Unlock()

	cfg := c.base.Copy()
	cfg.Database = database
	db := stdlib.OpenDB(*cfg, stdlib.OptionAfterConnect(func(ctx context.Context, pc *pgx.Conn) error {
		rows, err := pc.Query(ctx, "SELECT oid, typname FROM pg_type WHERE typname = ANY($1) AND typtype = 'b'", extraTypes)
		if err != nil {
			return nil
		}
		type ot struct {
			oid  uint32
			name string
		}
		var found []ot
		for rows.Next() {
			var x ot
			if rows.Scan(&x.oid, &x.name) == nil {
				found = append(found, x)
			}
		}
		rows.Close()
		for _, x := range found {
			pc.TypeMap().RegisterType(&pgtype.Type{Name: x.name, OID: x.oid, Codec: pgtype.TextCodec{}})
		}
		return nil
	}))
	db.SetMaxOpenConns(12)
	db.SetMaxIdleConns(3)
	db.SetConnMaxIdleTime(5 * time.Minute)
	db.SetConnMaxLifetime(time.Hour)

	pctx, cancel := context.WithTimeout(ctx, c.base.ConnectTimeout+5*time.Second)
	defer cancel()
	p := &pool{db: db}
	if err := db.QueryRowContext(pctx, "SELECT current_setting('server_version_num')::int").Scan(&p.server); err != nil {
		db.Close()
		return nil, mapConnError(err)
	}
	var gis sql.NullString
	_ = db.QueryRowContext(pctx, "SELECT extversion FROM pg_extension WHERE extname = 'postgis'").Scan(&gis)
	p.postgis = gis.String
	p.eng = &sqlbase.Engine{DB: db, D: dialect{}, RowIDExpr: "ctid"}

	c.mu.Lock()
	defer c.mu.Unlock()
	if existing, ok := c.pools[database]; ok {
		db.Close()
		return existing, nil
	}
	// Bound the number of per-database pools held open at once.
	if len(c.pools) >= 8 {
		for name, old := range c.pools {
			if name != c.defaultDB {
				old.db.Close()
				delete(c.pools, name)
				break
			}
		}
	}
	c.pools[database] = p
	return p, nil
}

func mapConnError(err error) error {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return &driver.QueryError{Message: pe.Message, Code: pe.Code, Detail: pe.Detail, Hint: pe.Hint}
	}
	return err
}

func (c *conn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, p := range c.pools {
		p.db.Close()
	}
	c.pools = map[string]*pool{}
	return nil
}

func (c *conn) Ping(ctx context.Context) error {
	p, err := c.pool(ctx, "")
	if err != nil {
		return err
	}
	return p.db.PingContext(ctx)
}

func (c *conn) Server(ctx context.Context) (*driver.ServerInfo, error) {
	p, err := c.pool(ctx, "")
	if err != nil {
		return nil, err
	}
	var ver, user, db string
	if err := p.db.QueryRowContext(ctx, "SELECT version(), current_user, current_database()").Scan(&ver, &user, &db); err != nil {
		return nil, err
	}
	info := &driver.ServerInfo{Product: "PostgreSQL", User: user, Database: db, Extras: map[string]string{}}
	info.Version = ver
	if f := strings.Fields(ver); len(f) >= 2 {
		info.Version = f[1]
		info.Extras["Build"] = ver
	}
	if p.postgis != "" {
		info.Extras["PostGIS"] = p.postgis
	}
	var ts sql.NullString
	if p.db.QueryRowContext(ctx, "SELECT extversion FROM pg_extension WHERE extname = 'timescaledb'").Scan(&ts) == nil && ts.Valid {
		info.Extras["TimescaleDB"] = ts.String
	}
	if c.ro {
		info.Extras["Session"] = "read-only"
	}
	return info, nil
}

func (c *conn) Databases(ctx context.Context) ([]driver.Database, error) {
	p, err := c.pool(ctx, "")
	if err != nil {
		return nil, err
	}
	q := `SELECT d.datname,
		CASE WHEN has_database_privilege(d.datname, 'CONNECT') THEN pg_database_size(d.oid) END,
		d.datcollate, pg_get_userbyid(d.datdba), d.datistemplate, COALESCE(shobj_description(d.oid, 'pg_database'), '')
		FROM pg_database d WHERE d.datallowconn`
	if c.only {
		q += ` AND d.datname = current_database()`
	}
	q += ` ORDER BY d.datname`
	rows, err := p.db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []driver.Database
	for rows.Next() {
		var d driver.Database
		var size sql.NullInt64
		var coll, owner sql.NullString
		var tmpl bool
		if err := rows.Scan(&d.Name, &size, &coll, &owner, &tmpl, &d.Comment); err != nil {
			return nil, err
		}
		d.Size, d.Collation, d.Owner, d.System = sqlbase.NullInt(size), coll.String, owner.String, tmpl
		out = append(out, d)
	}
	return out, rows.Err()
}

func (c *conn) Schemas(ctx context.Context, database string) ([]driver.Schema, error) {
	p, err := c.pool(ctx, database)
	if err != nil {
		return nil, err
	}
	rows, err := p.db.QueryContext(ctx, `SELECT nspname, pg_get_userbyid(nspowner) FROM pg_namespace
		WHERE nspname !~ '^pg_toast' AND nspname !~ '^pg_temp_' ORDER BY nspname = 'public' DESC, nspname`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []driver.Schema
	for rows.Next() {
		var s driver.Schema
		if err := rows.Scan(&s.Name, &s.Owner); err != nil {
			return nil, err
		}
		s.System = s.Name == "pg_catalog" || s.Name == "information_schema"
		out = append(out, s)
	}
	return out, rows.Err()
}

func (c *conn) Objects(ctx context.Context, s driver.Scope) ([]driver.Object, error) {
	p, err := c.pool(ctx, s.Database)
	if err != nil {
		return nil, err
	}
	schema := s.Schema
	if schema == "" {
		schema = "public"
	}
	var out []driver.Object
	rows, err := p.db.QueryContext(ctx, `SELECT c.relname, c.relkind::text,
		CASE WHEN c.reltuples >= 0 THEN c.reltuples::bigint END,
		CASE WHEN c.relkind IN ('r','m','p','t') THEN pg_total_relation_size(c.oid) END,
		COALESCE(obj_description(c.oid, 'pg_class'), ''),
		CASE WHEN c.relispartition THEN 'partition of ' || (SELECT pc.relname FROM pg_inherits i JOIN pg_class pc ON pc.oid = i.inhparent WHERE i.inhrelid = c.oid LIMIT 1) ELSE '' END
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relkind IN ('r','p','v','m','f','S')
		ORDER BY c.relname`, schema)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var o driver.Object
		var kind string
		var n, size sql.NullInt64
		if err := rows.Scan(&o.Name, &kind, &n, &size, &o.Comment, &o.Extra); err != nil {
			rows.Close()
			return nil, err
		}
		o.Kind = relkind(kind)
		o.Rows, o.Size = sqlbase.NullInt(n), sqlbase.NullInt(size)
		out = append(out, o)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	prokind := "p.prokind"
	if p.server < 110000 {
		prokind = "CASE WHEN p.proisagg THEN 'a' ELSE 'f' END"
	}
	if r, err := p.db.QueryContext(ctx, `SELECT p.proname, `+prokind+`::text, pg_get_function_identity_arguments(p.oid),
		COALESCE(obj_description(p.oid, 'pg_proc'), '')
		FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
		WHERE n.nspname = $1 AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.objid = p.oid AND d.deptype = 'e')
		ORDER BY p.proname`, schema); err == nil {
		for r.Next() {
			var o driver.Object
			var k string
			if r.Scan(&o.Name, &k, &o.Extra, &o.Comment) == nil {
				switch k {
				case "p":
					o.Kind = "procedure"
				case "f", "w":
					o.Kind = "function"
				default:
					continue
				}
				out = append(out, o)
			}
		}
		r.Close()
	}
	if r, err := p.db.QueryContext(ctx, `SELECT t.typname, CASE t.typtype WHEN 'e' THEN 'enum' WHEN 'c' THEN 'composite' WHEN 'd' THEN 'domain' WHEN 'r' THEN 'range' ELSE t.typtype::text END
		FROM pg_type t JOIN pg_namespace n ON n.oid = t.typnamespace
		WHERE n.nspname = $1 AND t.typtype IN ('e','c','d','r')
		AND (t.typtype <> 'c' OR (SELECT c.relkind FROM pg_class c WHERE c.oid = t.typrelid) = 'c')
		AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.objid = t.oid AND d.deptype = 'e')
		ORDER BY t.typname`, schema); err == nil {
		for r.Next() {
			var o driver.Object
			if r.Scan(&o.Name, &o.Extra) == nil {
				o.Kind = "type"
				out = append(out, o)
			}
		}
		r.Close()
	}
	if r, err := p.db.QueryContext(ctx, `SELECT e.extname, e.extversion FROM pg_extension e JOIN pg_namespace n ON n.oid = e.extnamespace
		WHERE n.nspname = $1 ORDER BY e.extname`, schema); err == nil {
		for r.Next() {
			var o driver.Object
			if r.Scan(&o.Name, &o.Extra) == nil {
				o.Kind = "extension"
				out = append(out, o)
			}
		}
		r.Close()
	}
	return out, nil
}

func relkind(k string) string {
	switch k {
	case "r":
		return "table"
	case "p":
		return "partitioned_table"
	case "v":
		return "view"
	case "m":
		return "materialized_view"
	case "f":
		return "foreign_table"
	case "S":
		return "sequence"
	}
	return k
}

func (c *conn) Browse(ctx context.Context, req driver.BrowseRequest) (*driver.Result, error) {
	p, err := c.pool(ctx, req.Ref.Database)
	if err != nil {
		return nil, err
	}
	t, err := c.Describe(ctx, req.Ref)
	if err != nil {
		return nil, err
	}
	return p.eng.Browse(ctx, t, req)
}

func (c *conn) Count(ctx context.Context, req driver.BrowseRequest) (driver.Count, error) {
	p, err := c.pool(ctx, req.Ref.Database)
	if err != nil {
		return driver.Count{}, err
	}
	t, err := c.Describe(ctx, req.Ref)
	if err != nil {
		return driver.Count{}, err
	}
	if len(req.Filters) == 0 && req.Where == "" && req.Search == "" && t.RowEstimate != nil && *t.RowEstimate > 2_000_000 {
		return driver.Count{Rows: *t.RowEstimate, Exact: false}, nil
	}
	return p.eng.Count(ctx, t, req)
}

func (c *conn) ApplyEdits(ctx context.Context, ref driver.ObjectRef, edits []driver.RowEdit) (*driver.EditResult, error) {
	if c.ro {
		return nil, driver.ErrReadOnly
	}
	p, err := c.pool(ctx, ref.Database)
	if err != nil {
		return nil, err
	}
	t, err := c.Describe(ctx, ref)
	if err != nil {
		return nil, err
	}
	return p.eng.ApplyEdits(ctx, t, edits)
}

func (c *conn) NewSession(ctx context.Context, s driver.Scope) (driver.Session, error) {
	p, err := c.pool(ctx, s.Database)
	if err != nil {
		return nil, err
	}
	sc, err := p.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	if s.Schema != "" {
		if _, err := sc.ExecContext(ctx, "SET search_path TO "+quote(s.Schema)+", public"); err != nil {
			sc.Close()
			return nil, err
		}
	}
	hooks := sqlbase.SessionHooks{
		InTx: func(ctx context.Context, sc *sql.Conn) (bool, error) {
			in := false
			err := sc.Raw(func(dc any) error {
				st := dc.(*stdlib.Conn).Conn().PgConn().TxStatus()
				in = st == 'T' || st == 'E'
				return nil
			})
			return in, err
		},
		After: func(ctx context.Context, sc *sql.Conn, sink driver.Sink) {
			_ = sc.Raw(func(dc any) error {
				for _, n := range c.notices.drain(dc.(*stdlib.Conn).Conn().PgConn()) {
					_ = sink.Notice(strings.ToLower(n.Severity), n.Message)
				}
				return nil
			})
		},
		MapError: func(err error, st sqlsplit.Statement) error {
			var pe *pgconn.PgError
			if errors.As(err, &pe) {
				qe := &driver.QueryError{Message: pe.Message, Code: pe.Code, Detail: pe.Detail, Hint: pe.Hint, Position: int(pe.Position)}
				if pe.Position > 0 {
					qe.Line = st.Line + strings.Count(prefixRunes(st.SQL, int(pe.Position)), "\n")
				}
				return qe
			}
			return err
		},
	}
	return sqlbase.NewSession(sc, dialect{}, hooks), nil
}

func prefixRunes(s string, n int) string {
	i := 0
	for pos := range s {
		if i == n {
			return s[:pos]
		}
		i++
	}
	return s
}

// noticeBuffer collects server NOTICE messages per physical connection so
// the console can show RAISE NOTICE output next to results.
type noticeBuffer struct {
	mu sync.Mutex
	m  map[*pgconn.PgConn][]*pgconn.Notice
}

func (b *noticeBuffer) init() { b.m = map[*pgconn.PgConn][]*pgconn.Notice{} }

func (b *noticeBuffer) handler(pc *pgconn.PgConn, n *pgconn.Notice) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.m[pc]) < 1000 {
		b.m[pc] = append(b.m[pc], n)
	}
}

func (b *noticeBuffer) drain(pc *pgconn.PgConn) []*pgconn.Notice {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := b.m[pc]
	delete(b.m, pc)
	return out
}

func quote(n string) string { return `"` + strings.ReplaceAll(n, `"`, `""`) + `"` }

func qualify(schema, name string) string {
	if schema == "" {
		return quote(name)
	}
	return quote(schema) + "." + quote(name)
}

func literal(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
