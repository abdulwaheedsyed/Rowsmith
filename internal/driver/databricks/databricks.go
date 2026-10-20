// Package databricks implements the Databricks SQL driver on databricks-sql-go
// (its pure-Go Thrift backend): Unity Catalog's catalogs, schemas, tables,
// views, materialized views, streaming tables, volumes and functions; Delta
// tables with their partitioning, clustering, properties and history; edits
// checked before they run; Spark plans; and the warehouse's state.
package databricks

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	dbsql "github.com/databricks/databricks-sql-go"
	dbauth "github.com/databricks/databricks-sql-go/auth"
	"github.com/databricks/databricks-sql-go/auth/oauth/m2m"
	"github.com/databricks/databricks-sql-go/auth/pat"
	dberrors "github.com/databricks/databricks-sql-go/errors"

	"rowsmith/internal/driver"
	"rowsmith/internal/driver/sqlbase"
	"rowsmith/internal/sqlsplit"
)

type dbxDriver struct{}

func init() { driver.Register(dbxDriver{}) }

var kinds = []driver.KindInfo{
	{Kind: "table", Label: "Tables", Icon: "table", Browse: true},
	{Kind: "view", Label: "Views", Icon: "view", Browse: true},
	{Kind: "materialized_view", Label: "Materialized views", Icon: "matview", Browse: true},
	{Kind: "streaming_table", Label: "Streaming tables", Icon: "stream", Browse: true},
	{Kind: "volume", Label: "Volumes", Icon: "package"},
	{Kind: "function", Label: "Functions", Icon: "function"},
}

var types = []string{
	"STRING", "BIGINT", "INT", "SMALLINT", "TINYINT", "DOUBLE", "FLOAT", "DECIMAL(18,2)", "BOOLEAN", "DATE", "TIMESTAMP",
	"TIMESTAMP_NTZ", "BINARY", "VARIANT", "ARRAY<STRING>", "MAP<STRING,STRING>", "STRUCT<name: STRING, value: DOUBLE>",
}

var design = driver.TableDesign{
	Columns: true, ColumnComments: true, TableComment: true, Checks: true, PrimaryKey: true,
	Options: []driver.Field{
		{Key: "cluster_by", Label: "Clustering columns", Type: driver.FieldText, Span: 3, Placeholder: "e.g. customer_id, placed_at",
			Help: "Liquid clustering; the columns can change later. Leave empty to partition instead."},
		{Key: "partitioned_by", Label: "Partition columns", Type: driver.FieldText, Span: 3, Placeholder: "e.g. day",
			Help: "Fixed when the table is created."},
		{Key: "location", Label: "Location", Type: driver.FieldText, Placeholder: "s3://… or abfss://… (optional; makes the table external)"},
	},
	Note: "Delta tables: primary keys are informational (not enforced), while NOT NULL and CHECK constraints are. Renaming or dropping columns turns on column mapping first, which only newer readers understand.",
}

func (dbxDriver) Info() driver.Info {
	fields := []driver.Field{
		{Key: "host", Label: "Workspace host", Type: driver.FieldText, Required: true, Placeholder: "dbc-a1b2c3d4-e5f6.cloud.databricks.com", Span: 4},
		{Key: "port", Label: "Port", Type: driver.FieldNumber, Default: 443, Span: 2},
		{Key: "httpPath", Label: "HTTP path", Type: driver.FieldText, Required: true, Placeholder: "/sql/1.0/warehouses/abc123def456",
			Help: "From the SQL warehouse's Connection details."},
		{Key: "auth", Label: "Sign in with", Type: driver.FieldSelect, Default: "token", Section: "auth", Options: []driver.Option{
			{Value: "token", Label: "Personal access token"}, {Value: "oauth", Label: "Service principal (OAuth)"}}},
		{Key: "token", Label: "Access token", Type: driver.FieldPassword, Secret: true, Section: "auth", ShowIf: map[string][]string{"auth": {"token"}}},
		{Key: "clientId", Label: "Client ID", Type: driver.FieldText, Section: "auth", Span: 3, ShowIf: map[string][]string{"auth": {"oauth"}}},
		{Key: "clientSecret", Label: "Client secret", Type: driver.FieldPassword, Secret: true, Section: "auth", Span: 3, ShowIf: map[string][]string{"auth": {"oauth"}}},
		{Key: "catalog", Label: "Catalog", Type: driver.FieldText, Span: 3, Placeholder: "optional — all catalogs are listed"},
		{Key: "schema", Label: "Schema", Type: driver.FieldText, Span: 3, Placeholder: "optional"},
		{Key: "queryTimeout", Label: "Query timeout (seconds)", Type: driver.FieldNumber, Default: 300, Section: "advanced", Span: 3,
			Help: "A stopped warehouse takes a few seconds (serverless) to minutes to start."},
	}
	return driver.Info{
		ID: "databricks", Name: "Databricks", Order: 79,
		Description: "Databricks SQL warehouses (serverless, pro, classic) with Unity Catalog, on AWS, Azure and Google Cloud",
		Dialect:     "databricks", DefaultPort: 443, Fields: fields, SSH: true, QuoteChar: "`",
		Levels: &driver.Levels{Database: "catalog"},
		Caps: driver.Caps{Databases: true, Schemas: true, SQL: true, EditRows: true, DDL: true, CreateDatabase: true, DropDatabase: true,
			Explain: true, Processes: true, Variables: true},
		Kinds: kinds, Types: types, URLSchemes: []string{"databricks"}, Design: &design,
	}
}

type conn struct {
	eng       *sqlbase.Engine
	db        *sql.DB
	ro        bool
	host      string
	warehouse string // the SQL warehouse's id, from the HTTP path
	auth      dbauth.Authenticator
	hc        *http.Client // for the REST API (warehouse state, query history)
	user      string
	version   string
	catalog   string // the connection's starting catalog and schema
	schema    string
}

var warehouseRe = regexp.MustCompile(`/warehouses/([0-9a-f]+)`)

func (dbxDriver) Open(ctx context.Context, p driver.OpenParams) (driver.Conn, error) {
	host := strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(p.String("host")), "https://"), "/")
	if host == "" {
		return nil, errors.New("enter the workspace host")
	}
	path := strings.TrimSpace(p.String("httpPath"))
	if path == "" {
		return nil, errors.New("enter the SQL warehouse's HTTP path")
	}
	port := p.Int("port", 443)
	var authr dbauth.Authenticator
	switch p.String("auth") {
	case "oauth":
		id, secret := strings.TrimSpace(p.String("clientId")), p.Secret("clientSecret")
		if id == "" || secret == "" {
			return nil, errors.New("enter the service principal's client ID and secret")
		}
		authr = m2m.NewAuthenticator(id, secret, host)
	default:
		tok := strings.TrimSpace(p.Secret("token"))
		if tok == "" {
			return nil, errors.New("enter an access token")
		}
		authr = &pat.PATAuth{AccessToken: tok}
	}
	transport := &http.Transport{Proxy: http.ProxyFromEnvironment, ForceAttemptHTTP2: true, MaxIdleConnsPerHost: 8,
		IdleConnTimeout: 90 * time.Second, TLSHandshakeTimeout: 15 * time.Second}
	if p.Dial != nil {
		dial := p.Dial
		transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) { return dial(ctx, network, addr) }
	}
	c := &conn{ro: p.ReadOnly, host: host, auth: authr, catalog: strings.TrimSpace(p.String("catalog")), schema: strings.TrimSpace(p.String("schema")),
		hc: &http.Client{Transport: transport, Timeout: time.Minute}}
	if m := warehouseRe.FindStringSubmatch(path); m != nil {
		c.warehouse = m[1]
	}
	opts := []dbsql.ConnOption{dbsql.WithServerHostname(host), dbsql.WithPort(port), dbsql.WithHTTPPath(path), dbsql.WithAuthenticator(authr),
		dbsql.WithTransport(transport), dbsql.WithUserAgentEntry("rowsmith"), dbsql.WithMaxRows(10000),
		dbsql.WithTimeout(time.Duration(p.Int("queryTimeout", 300)) * time.Second)}
	if c.catalog != "" || c.schema != "" {
		opts = append(opts, dbsql.WithInitialNamespace(c.catalog, c.schema))
	}
	connector, err := dbsql.NewConnector(opts...)
	if err != nil {
		return nil, err
	}
	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(2)
	db.SetConnMaxIdleTime(10 * time.Minute)
	c.db = db
	c.eng = &sqlbase.Engine{DB: db, D: dialect{}}
	var ver string
	if err := db.QueryRowContext(ctx, "SELECT current_user(), current_version().dbsql_version").Scan(&c.user, &ver); err != nil {
		db.Close()
		return nil, mapError(err, sqlsplit.Statement{})
	}
	c.version = ver
	return c, nil
}

func (c *conn) Close() error                   { return c.db.Close() }
func (c *conn) Ping(ctx context.Context) error { return c.db.PingContext(ctx) }

// api calls the workspace's REST API with the connection's credentials.
func (c *conn) api(ctx context.Context, method, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, "https://"+c.host+path, nil)
	if err != nil {
		return err
	}
	if err := c.auth.Authenticate(req); err != nil {
		return err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode >= 300 {
		var e struct {
			Message string `json:"message"`
		}
		json.Unmarshal(body, &e)
		if e.Message == "" {
			e.Message = strings.TrimSpace(string(body))
		}
		return fmt.Errorf("Databricks API: %s (HTTP %d)", e.Message, resp.StatusCode)
	}
	return json.Unmarshal(body, out)
}

type warehouseInfo struct {
	Name           string `json:"name"`
	State          string `json:"state"`
	ClusterSize    string `json:"cluster_size"`
	MinClusters    int    `json:"min_num_clusters"`
	MaxClusters    int    `json:"max_num_clusters"`
	AutoStopMins   int    `json:"auto_stop_mins"`
	Serverless     bool   `json:"enable_serverless_compute"`
	WarehouseType  string `json:"warehouse_type"`
	NumClusters    int    `json:"num_clusters"`
	ActiveSessions int    `json:"num_active_sessions"`
	Channel        struct {
		Name string `json:"name"`
	} `json:"channel"`
}

func (c *conn) warehouseState(ctx context.Context) (*warehouseInfo, error) {
	if c.warehouse == "" {
		return nil, errors.New("the HTTP path names no SQL warehouse")
	}
	var w warehouseInfo
	if err := c.api(ctx, "GET", "/api/2.0/sql/warehouses/"+c.warehouse, &w); err != nil {
		return nil, err
	}
	return &w, nil
}

func (c *conn) Server(ctx context.Context) (*driver.ServerInfo, error) {
	info := &driver.ServerInfo{Product: "Databricks SQL", Version: c.version, User: c.user, Database: c.catalog, Extras: map[string]string{}}
	if w, err := c.warehouseState(ctx); err == nil {
		kind := strings.ToLower(w.WarehouseType)
		if w.Serverless {
			kind = "serverless"
		}
		info.Extras["Warehouse"] = fmt.Sprintf("%s (%s, %s)", w.Name, kind, w.ClusterSize)
	}
	if c.schema != "" {
		info.Extras["Schema"] = c.schema
	}
	if c.ro {
		info.Extras["Session"] = "read-only"
	}
	return info, nil
}

// Databases lists the catalogs the user can see.
func (c *conn) Databases(ctx context.Context) ([]driver.Database, error) {
	rows, err := c.db.QueryContext(ctx, "SELECT catalog_name, comment FROM system.information_schema.catalogs ORDER BY catalog_name")
	if err != nil {
		return nil, mapError(err, sqlsplit.Statement{})
	}
	defer rows.Close()
	var out []driver.Database
	for rows.Next() {
		var name string
		var comment sql.NullString
		if err := rows.Scan(&name, &comment); err != nil {
			return nil, err
		}
		out = append(out, driver.Database{Name: name, Comment: comment.String, System: name == "system" || strings.HasPrefix(name, "__")})
	}
	return out, mapError(rows.Err(), sqlsplit.Statement{})
}

func (c *conn) Schemas(ctx context.Context, catalog string) ([]driver.Schema, error) {
	if catalog == "" {
		return nil, errors.New("choose a catalog")
	}
	rows, err := c.db.QueryContext(ctx, "SELECT schema_name, schema_owner FROM "+quote(catalog)+".information_schema.schemata ORDER BY schema_name")
	if err != nil {
		return nil, mapError(err, sqlsplit.Statement{})
	}
	defer rows.Close()
	var out []driver.Schema
	for rows.Next() {
		var s driver.Schema
		var owner sql.NullString
		if err := rows.Scan(&s.Name, &owner); err != nil {
			return nil, err
		}
		s.Owner, s.System = owner.String, s.Name == "information_schema"
		out = append(out, s)
	}
	return out, mapError(rows.Err(), sqlsplit.Statement{})
}

// tableKind maps information_schema's table_type.
func tableKind(t string) string {
	switch strings.ToUpper(t) {
	case "VIEW", "METRIC_VIEW":
		return "view"
	case "MATERIALIZED_VIEW":
		return "materialized_view"
	case "STREAMING_TABLE":
		return "streaming_table"
	}
	return "table" // MANAGED, EXTERNAL, FOREIGN
}

func (c *conn) Objects(ctx context.Context, s driver.Scope) ([]driver.Object, error) {
	if s.Database == "" || s.Schema == "" {
		return nil, errors.New("choose a catalog and schema")
	}
	cat := quote(s.Database)
	rows, err := c.db.QueryContext(ctx, "SELECT table_name, table_type, data_source_format, comment FROM "+cat+
		".information_schema.tables WHERE table_schema = ? ORDER BY table_name", s.Schema)
	if err != nil {
		return nil, mapError(err, sqlsplit.Statement{})
	}
	var out []driver.Object
	for rows.Next() {
		var o driver.Object
		var typ string
		var format, comment sql.NullString
		if err := rows.Scan(&o.Name, &typ, &format, &comment); err != nil {
			rows.Close()
			return nil, err
		}
		o.Kind, o.Engine, o.Comment = tableKind(typ), strings.ToLower(format.String), comment.String
		if strings.EqualFold(typ, "EXTERNAL") || strings.EqualFold(typ, "FOREIGN") {
			o.Extra = strings.ToLower(typ)
		}
		out = append(out, o)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, mapError(err, sqlsplit.Statement{})
	}
	if vr, err := c.db.QueryContext(ctx, "SELECT volume_name, volume_type, comment FROM "+cat+".information_schema.volumes WHERE volume_schema = ?", s.Schema); err == nil {
		for vr.Next() {
			var name, typ string
			var comment sql.NullString
			if vr.Scan(&name, &typ, &comment) == nil {
				out = append(out, driver.Object{Name: name, Kind: "volume", Extra: strings.ToLower(typ), Comment: comment.String})
			}
		}
		vr.Close()
	}
	if fr, err := c.db.QueryContext(ctx, "SELECT routine_name, data_type, comment FROM "+cat+".information_schema.routines WHERE routine_schema = ?", s.Schema); err == nil {
		for fr.Next() {
			var name string
			var ret, comment sql.NullString
			if fr.Scan(&name, &ret, &comment) == nil {
				out = append(out, driver.Object{Name: name, Kind: "function", Extra: strings.ToLower(ret.String), Comment: comment.String})
			}
		}
		fr.Close()
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (c *conn) Browse(ctx context.Context, req driver.BrowseRequest) (*driver.Result, error) {
	t, err := c.describe(ctx, req.Ref, false)
	if err != nil {
		return nil, err
	}
	res, err := c.eng.Browse(ctx, t, req)
	return res, mapError(err, sqlsplit.Statement{})
}

func (c *conn) Count(ctx context.Context, req driver.BrowseRequest) (driver.Count, error) {
	t, err := c.describe(ctx, req.Ref, false)
	if err != nil {
		return driver.Count{}, err
	}
	// Delta answers count(*) from file statistics, without reading data.
	n, err := c.eng.Count(ctx, t, req)
	return n, mapError(err, sqlsplit.Statement{})
}

func (c *conn) ApplyEdits(ctx context.Context, ref driver.ObjectRef, edits []driver.RowEdit) (*driver.EditResult, error) {
	if c.ro {
		return nil, driver.ErrReadOnly
	}
	t, err := c.describe(ctx, ref, false)
	if err != nil {
		return nil, err
	}
	if !t.Editable {
		return nil, fmt.Errorf("%s can't be edited row by row: only Delta tables take changes", strings.ReplaceAll(t.Kind, "_", " "))
	}
	res, err := c.eng.ApplyEdits(ctx, t, edits)
	return res, mapError(err, sqlsplit.Statement{})
}

// ---- names and errors -------------------------------------------------------------

func quote(name string) string { return "`" + strings.ReplaceAll(name, "`", "``") + "`" }

func qualify(ref driver.ObjectRef) string {
	var parts []string
	for _, p := range []string{ref.Database, ref.Schema} {
		if p != "" {
			parts = append(parts, quote(p))
		}
	}
	return strings.Join(append(parts, quote(ref.Name)), ".")
}

func literal(s string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, "'", `\'`).Replace(s) + "'"
}

var (
	errorClassRe = regexp.MustCompile(`\[([A-Z0-9_.]+)\]\s*`)
	errorPosRe   = regexp.MustCompile(`(?:line (\d+),? pos (\d+))`)
	// The SQLSTATE and position the message repeats, shown separately.
	errorTailRe = regexp.MustCompile(`[.;]?\s*SQLSTATE: \w+|;?\s*line \d+,? pos \d+`)
	// The driver wraps the server's message in its own prefixes.
	errorPrefixRe = regexp.MustCompile(`^(databricks: )?(execution error: )?(failed to execute query: )?(unexpected operation state ERROR_STATE: )?`)
)

// mapError turns a Databricks failure into a QueryError with the error
// class as its code and the line it points at.
func mapError(err error, st sqlsplit.Statement) error {
	if err == nil {
		return nil
	}
	var ee dberrors.DBExecutionError
	if !errors.As(err, &ee) {
		return err
	}
	msg := errorPrefixRe.ReplaceAllString(ee.Error(), "")
	if i := strings.Index(msg, "\n=="); i > 0 {
		msg = msg[:i] // the SQL excerpt the server appends
	}
	qe := &driver.QueryError{Message: strings.TrimSpace(msg), Code: ee.SqlState()}
	if m := errorClassRe.FindStringSubmatch(msg); m != nil {
		qe.Code = m[1]
		qe.Message = strings.Replace(qe.Message, m[0], "", 1)
	}
	qe.Message = strings.TrimSpace(errorTailRe.ReplaceAllString(qe.Message, ""))
	if !strings.HasSuffix(qe.Message, ".") && !strings.HasSuffix(qe.Message, "]") {
		qe.Message += "."
	}
	if m := errorPosRe.FindStringSubmatch(ee.Error()); m != nil {
		l, _ := strconv.Atoi(m[1])
		pos, _ := strconv.Atoi(m[2])
		qe.Line = max(st.Line, 1) + l - 1
		qe.Position = pos + 1
	}
	return qe
}
