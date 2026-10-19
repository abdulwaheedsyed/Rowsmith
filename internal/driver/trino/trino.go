// Package trino implements the Trino driver (Trino and Starburst): every
// catalog the cluster has is a database in Rowsmith, so one connection
// reaches Iceberg, Delta Lake and Hive tables (Parquet, ORC, CSV files in
// object storage) as well as the other databases Trino federates. Iceberg
// tables also show their snapshots, files and table properties.
package trino

import (
	"context"
	"crypto/tls"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/trinodb/trino-go-client/trino"

	"rowsmith/internal/driver"
	"rowsmith/internal/driver/sqlbase"
	"rowsmith/internal/sqlsplit"
)

type trinoDriver struct{}

func init() { driver.Register(trinoDriver{}) }

var kinds = []driver.KindInfo{
	{Kind: "table", Label: "Tables", Icon: "table", Browse: true},
	{Kind: "view", Label: "Views", Icon: "view", Browse: true},
	{Kind: "materialized_view", Label: "Materialized views", Icon: "view", Browse: true},
}

var types = []string{
	"bigint", "integer", "smallint", "tinyint", "double", "real", "decimal(12,2)", "boolean", "varchar", "varchar(255)",
	"date", "time(3)", "timestamp(6)", "timestamp(6) with time zone", "varbinary", "uuid", "json", "array(varchar)",
	"map(varchar, varchar)", "row(name varchar, value double)",
}

var design = driver.TableDesign{
	Columns: true, ColumnComments: true, TableComment: true,
	Options: []driver.Field{
		{Key: "format", Label: "File format", Type: driver.FieldSelect, Span: 2, Options: []driver.Option{
			{Value: "", Label: "catalog default"}, {Value: "PARQUET", Label: "Parquet"}, {Value: "ORC", Label: "ORC"}, {Value: "AVRO", Label: "Avro (Iceberg)"},
			{Value: "CSV", Label: "CSV (Hive)"}, {Value: "JSON", Label: "JSON (Hive)"}, {Value: "TEXTFILE", Label: "Text (Hive)"}}},
		{Key: "partitioning", Label: "Partitioning (Iceberg)", Type: driver.FieldText, Span: 4, Placeholder: "e.g. month(placed_at), bucket(id, 16)"},
		{Key: "partitioned_by", Label: "Partition columns (Hive, Delta)", Type: driver.FieldText, Span: 3, Placeholder: "e.g. day — the last columns"},
		{Key: "sorted_by", Label: "Sort order (Iceberg)", Type: driver.FieldText, Span: 3, Placeholder: "e.g. customer_id"},
		{Key: "location", Label: "Location", Type: driver.FieldText, Placeholder: "s3://bucket/path/ (optional; external for Hive)"},
	},
	Note: "Trino tables have no keys or indexes. What a table can change depends on its catalog's connector: Iceberg and Delta Lake take most changes, Hive fewer.",
}

func (trinoDriver) Info() driver.Info {
	fields := []driver.Field{
		{Key: "host", Label: "Host", Type: driver.FieldText, Required: true, Placeholder: "trino.example.com", Span: 4},
		{Key: "port", Label: "Port", Type: driver.FieldNumber, Default: 8080, Span: 2},
		{Key: "user", Label: "User", Type: driver.FieldText, Required: true, Section: "auth", Span: 3},
		{Key: "password", Label: "Password", Type: driver.FieldPassword, Secret: true, Section: "auth", Span: 3,
			Help: "Trino accepts passwords only over TLS."},
		{Key: "accessToken", Label: "Access token (JWT)", Type: driver.FieldPassword, Secret: true, Section: "auth",
			Help: "Instead of a password, for clusters that take OAuth 2.0 or JWT tokens."},
		{Key: "catalog", Label: "Catalog", Type: driver.FieldText, Span: 3, Placeholder: "optional — all catalogs are listed"},
		{Key: "schema", Label: "Schema", Type: driver.FieldText, Span: 3, Placeholder: "optional"},
	}
	fields = append(fields, driver.TLSFields("disable")...)
	fields = append(fields,
		driver.Field{Key: "connectTimeout", Label: "Connect timeout (seconds)", Type: driver.FieldNumber, Default: 15, Section: "advanced", Span: 3},
		driver.Field{Key: "clientTags", Label: "Client tags", Type: driver.FieldText, Section: "advanced", Span: 3, Placeholder: "e.g. etl, adhoc",
			Help: "Comma-separated; resource groups can route on them."},
	)
	secure := []string{"prefer", "require", "verify-ca", "verify-full"}
	return driver.Info{
		ID: "trino", Name: "Trino", Order: 80,
		Description: "Trino 400+ and Starburst: Iceberg, Delta Lake, Hive (Parquet, ORC, CSV) and every catalog the cluster federates",
		Dialect:     "trino", DefaultPort: 8080, Fields: fields, SSH: true,
		Ports: []driver.PortRule{{When: map[string][]string{"tls": secure}, Port: 443}},
		Caps: driver.Caps{Databases: true, Schemas: true, SQL: true, EditRows: true, DDL: true, Explain: true,
			Processes: true, Variables: true},
		Kinds: kinds, Types: types, URLSchemes: []string{"trino", "starburst"}, QuoteChar: `"`,
		Design: &design,
	}
}

type conn struct {
	eng     *sqlbase.Engine
	db      *sql.DB
	ro      bool
	user    string
	version string
	catalog string // the connection's starting catalog and schema
	schema  string

	mu         sync.Mutex
	connectors map[string]string // catalog → connector name (iceberg, delta_lake, hive…)
}

func (trinoDriver) Open(ctx context.Context, p driver.OpenParams) (driver.Conn, error) {
	host, port := driver.HostPort(p, 8080)
	if host == "" {
		return nil, errors.New("host is required")
	}
	user := strings.TrimSpace(p.String("user"))
	if user == "" {
		return nil, errors.New("enter the user")
	}
	timeout := time.Duration(p.Int("connectTimeout", 15)) * time.Second
	tc, err := driver.TLSConfig(p, host)
	if err != nil {
		return nil, err
	}
	scheme := "http"
	if tc != nil {
		scheme = "https"
	}
	u := &url.URL{Scheme: scheme, Host: net.JoinHostPort(host, strconv.Itoa(port))}
	if pw := p.Secret("password"); pw != "" {
		if tc == nil {
			return nil, errors.New("Trino accepts passwords only over TLS: set TLS / SSL to Required or Verify")
		}
		u.User = url.UserPassword(user, pw)
	} else {
		u.User = url.User(user)
	}
	transport := &http.Transport{TLSClientConfig: tc, DialContext: (&net.Dialer{Timeout: timeout}).DialContext,
		IdleConnTimeout: 90 * time.Second, MaxIdleConnsPerHost: 8, ResponseHeaderTimeout: 0}
	if tc == nil {
		transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	if p.Dial != nil {
		dial := p.Dial
		transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) { return dial(ctx, "tcp", addr) }
	}
	cfg := &trino.Config{
		ServerURI: u.String(), Source: "rowsmith", ClientInfo: p.AppName,
		Catalog: strings.TrimSpace(p.String("catalog")), Schema: strings.TrimSpace(p.String("schema")),
		AccessToken: strings.TrimSpace(p.Secret("accessToken")),
		// EXECUTE IMMEDIATE sends statements in the request body; explicit
		// PREPARE would carry them in headers, which large INSERTs outgrow.
		DisableExplicitPrepare: true,
		HTTPClient:             &http.Client{Transport: transport},
	}
	if tags := strings.TrimSpace(p.String("clientTags")); tags != "" {
		for _, t := range strings.Split(tags, ",") {
			if t = strings.TrimSpace(t); t != "" {
				cfg.ClientTags = append(cfg.ClientTags, t)
			}
		}
	}
	connector, err := trino.NewConnector(cfg)
	if err != nil {
		return nil, err
	}
	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(16)
	db.SetMaxIdleConns(4)
	db.SetConnMaxIdleTime(5 * time.Minute)
	c := &conn{db: db, ro: p.ReadOnly, catalog: cfg.Catalog, schema: cfg.Schema, connectors: map[string]string{}}
	c.eng = &sqlbase.Engine{DB: db, D: dialect{}}
	pctx, cancel := context.WithTimeout(ctx, timeout+10*time.Second)
	defer cancel()
	if err := db.QueryRowContext(pctx, "SELECT current_user").Scan(&c.user); err != nil {
		db.Close()
		return nil, mapError(err, sqlsplit.Statement{})
	}
	var ver sql.NullString
	if db.QueryRowContext(pctx, "SELECT node_version FROM system.runtime.nodes WHERE coordinator LIMIT 1").Scan(&ver) == nil {
		c.version = ver.String
	}
	return c, nil
}

func (c *conn) Close() error                   { return c.db.Close() }
func (c *conn) Ping(ctx context.Context) error { return c.db.PingContext(ctx) }

func (c *conn) Server(ctx context.Context) (*driver.ServerInfo, error) {
	info := &driver.ServerInfo{Product: "Trino", Version: c.version, User: c.user, Database: c.catalog, Extras: map[string]string{}}
	var nodes int
	if c.db.QueryRowContext(ctx, "SELECT count(*) FROM system.runtime.nodes WHERE state = 'active'").Scan(&nodes) == nil {
		info.Extras["Nodes"] = strconv.Itoa(nodes)
	}
	if c.schema != "" {
		info.Extras["Schema"] = c.schema
	}
	if c.ro {
		info.Extras["Session"] = "read-only"
	}
	return info, nil
}

// ConnectorName names a catalog's connector for migrations, which write the
// types its table format takes.
func (c *conn) ConnectorName(ctx context.Context, catalog string) string {
	return c.connector(ctx, catalog)
}

// connector names a catalog's connector (iceberg, delta_lake, hive, …).
func (c *conn) connector(ctx context.Context, catalog string) string {
	c.mu.Lock()
	name, ok := c.connectors[catalog]
	c.mu.Unlock()
	if ok {
		return name
	}
	if _, err := c.Databases(ctx); err != nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connectors[catalog]
}

// Databases lists the catalogs, with their connectors.
func (c *conn) Databases(ctx context.Context) ([]driver.Database, error) {
	rows, err := c.db.QueryContext(ctx, "SELECT catalog_name, connector_name FROM system.metadata.catalogs ORDER BY catalog_name")
	if err != nil {
		return nil, mapError(err, sqlsplit.Statement{})
	}
	defer rows.Close()
	var out []driver.Database
	found := map[string]string{}
	for rows.Next() {
		var name, conn string
		if err := rows.Scan(&name, &conn); err != nil {
			return nil, err
		}
		found[name] = conn
		out = append(out, driver.Database{Name: name, Comment: conn, System: name == "system" || conn == "jmx"})
	}
	c.mu.Lock()
	c.connectors = found
	c.mu.Unlock()
	return out, mapError(rows.Err(), sqlsplit.Statement{})
}

func (c *conn) Schemas(ctx context.Context, catalog string) ([]driver.Schema, error) {
	if catalog == "" {
		return nil, errors.New("choose a catalog")
	}
	rows, err := c.db.QueryContext(ctx, "SELECT schema_name FROM "+quote(catalog)+".information_schema.schemata ORDER BY schema_name")
	if err != nil {
		return nil, mapError(err, sqlsplit.Statement{})
	}
	defer rows.Close()
	var out []driver.Schema
	for rows.Next() {
		var s driver.Schema
		if err := rows.Scan(&s.Name); err != nil {
			return nil, err
		}
		s.System = s.Name == "information_schema"
		out = append(out, s)
	}
	return out, mapError(rows.Err(), sqlsplit.Statement{})
}

func (c *conn) Objects(ctx context.Context, s driver.Scope) ([]driver.Object, error) {
	if s.Database == "" || s.Schema == "" {
		return nil, errors.New("choose a catalog and schema")
	}
	rows, err := c.db.QueryContext(ctx, "SELECT table_name, table_type FROM "+quote(s.Database)+".information_schema.tables WHERE table_schema = ? ORDER BY table_name", s.Schema)
	if err != nil {
		return nil, mapError(err, sqlsplit.Statement{})
	}
	var out []driver.Object
	idx := map[string]int{}
	for rows.Next() {
		var o driver.Object
		var t string
		if err := rows.Scan(&o.Name, &t); err != nil {
			rows.Close()
			return nil, err
		}
		o.Kind = "table"
		if t == "VIEW" {
			o.Kind = "view"
		}
		idx[o.Name] = len(out)
		out = append(out, o)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, mapError(err, sqlsplit.Statement{})
	}
	// Materialized views read as tables in information_schema.
	if mv, err := c.db.QueryContext(ctx, "SELECT name FROM system.metadata.materialized_views WHERE catalog_name = ? AND schema_name = ?", s.Database, s.Schema); err == nil {
		for mv.Next() {
			var n string
			if mv.Scan(&n) == nil {
				if i, ok := idx[n]; ok {
					out[i].Kind = "materialized_view"
				}
			}
		}
		mv.Close()
	}
	if cm, err := c.db.QueryContext(ctx, "SELECT table_name, comment FROM system.metadata.table_comments WHERE catalog_name = ? AND schema_name = ? AND comment IS NOT NULL", s.Database, s.Schema); err == nil {
		for cm.Next() {
			var n, text string
			if cm.Scan(&n, &text) == nil {
				if i, ok := idx[n]; ok {
					out[i].Comment = text
				}
			}
		}
		cm.Close()
	}
	conn := c.connector(ctx, s.Database)
	for i := range out {
		if out[i].Kind == "table" {
			out[i].Engine = conn
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
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
	if len(req.Filters) == 0 && req.Where == "" && req.Search == "" && t.RowEstimate != nil && t.Options["delete_files"] == "0" {
		// Iceberg metadata counts rows exactly when no row-level deletes are pending.
		return driver.Count{Rows: *t.RowEstimate, Exact: true}, nil
	}
	n, err := c.eng.Count(ctx, t, req)
	return n, mapError(err, sqlsplit.Statement{})
}

func (c *conn) ApplyEdits(ctx context.Context, ref driver.ObjectRef, edits []driver.RowEdit) (*driver.EditResult, error) {
	if c.ro {
		return nil, driver.ErrReadOnly
	}
	t, err := c.Describe(ctx, ref)
	if err != nil {
		return nil, err
	}
	if !t.Editable {
		return nil, fmt.Errorf("%s tables cannot be edited row by row; Iceberg and Delta Lake tables can", strings.ReplaceAll(t.Options["connector"], "_", " "))
	}
	res, err := c.eng.ApplyEdits(ctx, t, edits)
	return res, mapError(err, sqlsplit.Statement{})
}

// ---- names and errors -------------------------------------------------------------

func quote(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }

func qualify(ref driver.ObjectRef) string {
	var parts []string
	for _, p := range []string{ref.Database, ref.Schema} {
		if p != "" {
			parts = append(parts, quote(p))
		}
	}
	return strings.Join(append(parts, quote(ref.Name)), ".")
}

func literal(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

var linePrefixRe = regexp.MustCompile(`^line \d+:\d+: `)

// mapError turns a Trino failure into a QueryError pointing at the line.
func mapError(err error, st sqlsplit.Statement) error {
	if err == nil {
		return nil
	}
	var et *trino.ErrTrino
	if !errors.As(err, &et) {
		var ev trino.ErrTrino
		if !errors.As(err, &ev) {
			return err
		}
		et = &ev
	}
	qe := &driver.QueryError{Message: linePrefixRe.ReplaceAllString(et.Message, ""), Code: et.ErrorName}
	if l := et.ErrorLocation.LineNumber; l > 0 {
		qe.Line = max(st.Line, 1) + l - 1
		qe.Position = et.ErrorLocation.ColumnNumber
	}
	return qe
}
