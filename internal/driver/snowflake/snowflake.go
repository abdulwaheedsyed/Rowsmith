// Package snowflake implements the Snowflake driver over gosnowflake:
// sign-in with a key pair, programmatic access token, OAuth token or
// password; the catalog read with SHOW commands, which run in Snowflake's
// cloud services without waking a warehouse; and browsing, editing and the
// console through sqlbase.
package snowflake

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"database/sql"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	sf "github.com/snowflakedb/gosnowflake/v2"
	"github.com/youmark/pkcs8"

	"rowsmith/internal/driver"
	"rowsmith/internal/driver/sqlbase"
	"rowsmith/internal/sqlsplit"
)

type sfDriver struct{}

func init() {
	// The driver logs every failed query to stderr; Rowsmith reports errors itself.
	_ = sf.GetLogger().SetLogLevel("OFF")
	driver.Register(sfDriver{})
}

var kinds = []driver.KindInfo{
	{Kind: "table", Label: "Tables", Icon: "table", Browse: true},
	{Kind: "view", Label: "Views", Icon: "view", Browse: true},
	{Kind: "materialized_view", Label: "Materialized views", Icon: "view", Browse: true},
	{Kind: "dynamic_table", Label: "Dynamic tables", Icon: "table", Browse: true},
	{Kind: "external_table", Label: "External tables", Icon: "table", Browse: true},
	{Kind: "sequence", Label: "Sequences", Icon: "sequence"},
	{Kind: "function", Label: "Functions", Icon: "function"},
	{Kind: "procedure", Label: "Procedures", Icon: "procedure"},
}

var types = []string{
	"NUMBER(38,0)", "NUMBER(12,2)", "INTEGER", "FLOAT", "BOOLEAN", "VARCHAR(255)", "VARCHAR", "BINARY",
	"DATE", "TIME", "TIMESTAMP_NTZ", "TIMESTAMP_TZ", "TIMESTAMP_LTZ", "VARIANT", "OBJECT", "ARRAY", "GEOGRAPHY", "GEOMETRY",
}

var design = driver.TableDesign{
	Columns: true, AutoIncrement: true, ColumnComments: true, TableComment: true, Collation: true,
	PrimaryKey: true, ForeignKeys: true,
	FKActions: []string{"NO ACTION"},
	Options: []driver.Field{
		{Key: "cluster_by", Label: "Clustering key", Type: driver.FieldText, Span: 3, Placeholder: "e.g. placed_at, customer_id"},
		{Key: "transient", Label: "Transient table (no Fail-safe)", Type: driver.FieldBool, Span: 3},
	},
	Note: "Snowflake records primary, unique and foreign keys but does not enforce them (except NOT NULL). Tables have no indexes; a clustering key orders large tables.",
}

func (sfDriver) Info() driver.Info {
	authShow := func(modes ...string) map[string][]string { return map[string][]string{"auth": modes} }
	fields := []driver.Field{
		{Key: "account", Label: "Account identifier", Type: driver.FieldText, Required: true, Placeholder: "orgname-accountname",
			Help: "As in https://<identifier>.snowflakecomputing.com. In Snowsight: your account menu, View account details."},
		{Key: "user", Label: "User", Type: driver.FieldText, Required: true, Section: "auth", Span: 3},
		{Key: "auth", Label: "Sign in with", Type: driver.FieldSelect, Default: "keypair", Section: "auth", Span: 3, Options: []driver.Option{
			{Value: "keypair", Label: "Key pair"}, {Value: "pat", Label: "Programmatic access token"},
			{Value: "oauth", Label: "OAuth access token"}, {Value: "password", Label: "Password"},
		}},
		{Key: "privateKey", Label: "Private key (PEM)", Type: driver.FieldFile, Secret: true, Section: "auth", ShowIf: authShow("keypair"),
			Help: "An unencrypted or encrypted PKCS#8 key whose public key is set on the user (ALTER USER … SET RSA_PUBLIC_KEY)."},
		{Key: "passphrase", Label: "Key passphrase", Type: driver.FieldPassword, Secret: true, Section: "auth", ShowIf: authShow("keypair"),
			Help: "Only for an encrypted key."},
		{Key: "token", Label: "Token", Type: driver.FieldPassword, Secret: true, Section: "auth", ShowIf: authShow("pat", "oauth")},
		{Key: "password", Label: "Password", Type: driver.FieldPassword, Secret: true, Section: "auth", ShowIf: authShow("password"),
			Help: "Snowflake is phasing out password-only sign-in for people; prefer a key pair or token for Rowsmith."},
		{Key: "warehouse", Label: "Warehouse", Type: driver.FieldText, Span: 2, Placeholder: "the user's default",
			Help: "Runs queries. Browsing the catalog does not wake it."},
		{Key: "role", Label: "Role", Type: driver.FieldText, Span: 2, Placeholder: "the user's default"},
		{Key: "database", Label: "Database", Type: driver.FieldText, Span: 2, Placeholder: "optional"},
		{Key: "schema", Label: "Schema", Type: driver.FieldText, Section: "advanced", Span: 3, Placeholder: "optional"},
		{Key: "host", Label: "Host override", Type: driver.FieldText, Section: "advanced", Span: 3,
			Placeholder: "<account>.snowflakecomputing.com", Help: "For PrivateLink or a proxy in front of Snowflake."},
		{Key: "connectTimeout", Label: "Sign-in timeout (seconds)", Type: driver.FieldNumber, Default: 60, Section: "advanced", Span: 3},
	}
	return driver.Info{
		ID: "snowflake", Name: "Snowflake", Order: 75,
		Description: "Snowflake, on AWS, Azure and Google Cloud",
		Dialect:     "snowflake", Fields: fields, SSH: true,
		Caps: driver.Caps{Databases: true, Schemas: true, SQL: true, Transactions: true, EditRows: true, DDL: true,
			CreateDatabase: true, DropDatabase: true, ForeignKeys: true, Explain: true, Processes: true, Variables: true, Users: true,
			Geometry: true},
		Kinds: kinds, Types: types, URLSchemes: []string{"snowflake"}, QuoteChar: `"`,
		Design: &design,
	}
}

type conn struct {
	eng      *sqlbase.Engine
	db       *sql.DB
	ro       bool
	version  string
	user     string
	role     string
	wh       string
	account  string
	region   string
	database string // the connection's starting database and schema
	schema   string
}

func (sfDriver) Open(ctx context.Context, p driver.OpenParams) (driver.Conn, error) {
	cfg, err := config(p)
	if err != nil {
		return nil, err
	}
	db := sql.OpenDB(sf.NewConnector(sf.SnowflakeDriver{}, *cfg))
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(2)
	db.SetConnMaxIdleTime(10 * time.Minute)
	db.SetConnMaxLifetime(3 * time.Hour) // a session lasts four hours without keep-alive
	c := &conn{db: db, ro: p.ReadOnly}
	c.eng = &sqlbase.Engine{DB: db, D: dialect{}}
	pctx, cancel := context.WithTimeout(ctx, cfg.LoginTimeout+10*time.Second)
	defer cancel()
	var wh, db0, sc0, region sql.NullString
	if err := db.QueryRowContext(pctx, `SELECT CURRENT_VERSION(), CURRENT_USER(), CURRENT_ROLE(), CURRENT_WAREHOUSE(),
		CURRENT_DATABASE(), CURRENT_SCHEMA(), CURRENT_ACCOUNT_NAME(), CURRENT_REGION()`).
		Scan(&c.version, &c.user, &c.role, &wh, &db0, &sc0, &c.account, &region); err != nil {
		db.Close()
		return nil, mapError(err, sqlsplit.Statement{})
	}
	c.wh, c.database, c.schema, c.region = wh.String, db0.String, sc0.String, region.String
	return c, nil
}

// config turns the connection form into gosnowflake's configuration.
func config(p driver.OpenParams) (*sf.Config, error) {
	account := strings.TrimSpace(p.String("account"))
	account = strings.TrimPrefix(strings.TrimPrefix(account, "https://"), "http://")
	account = strings.TrimSuffix(strings.TrimSuffix(account, "/"), ".snowflakecomputing.com")
	if account == "" {
		return nil, errors.New("enter the account identifier, such as myorg-myaccount")
	}
	user := strings.TrimSpace(p.String("user"))
	if user == "" {
		return nil, errors.New("enter the user")
	}
	cfg := &sf.Config{
		Account: account, User: user,
		Warehouse: strings.TrimSpace(p.String("warehouse")), Role: strings.TrimSpace(p.String("role")),
		Database: strings.TrimSpace(p.String("database")), Schema: strings.TrimSpace(p.String("schema")),
		Application:  clientName(p.AppName),
		LoginTimeout: time.Duration(p.Int("connectTimeout", 60)) * time.Second,
		Params:       map[string]*string{},
	}
	if h := strings.TrimSpace(p.String("host")); h != "" {
		cfg.Host, cfg.Port, cfg.Protocol = h, 443, "https"
	}
	switch p.String("auth") {
	case "", "keypair":
		key, err := privateKey(p.Secret("privateKey"), p.Secret("passphrase"))
		if err != nil {
			return nil, err
		}
		cfg.Authenticator, cfg.PrivateKey = sf.AuthTypeJwt, key
	case "pat":
		cfg.Authenticator, cfg.Token = sf.AuthTypePat, strings.TrimSpace(p.Secret("token"))
		if cfg.Token == "" {
			return nil, errors.New("enter the programmatic access token")
		}
	case "oauth":
		cfg.Authenticator, cfg.Token = sf.AuthTypeOAuth, strings.TrimSpace(p.Secret("token"))
		if cfg.Token == "" {
			return nil, errors.New("enter the OAuth access token")
		}
	case "password":
		cfg.Authenticator, cfg.Password = sf.AuthTypeSnowflake, p.Secret("password")
	default:
		return nil, fmt.Errorf("unknown sign-in method %q", p.String("auth"))
	}
	if p.Dial != nil {
		dial := p.Dial
		cfg.Transporter = &http.Transport{
			DialContext:         func(ctx context.Context, network, addr string) (net.Conn, error) { return dial(ctx, "tcp", addr) },
			TLSHandshakeTimeout: 30 * time.Second,
			IdleConnTimeout:     90 * time.Second,
			MaxIdleConnsPerHost: 4,
		}
	}
	return cfg, nil
}

func clientName(app string) string {
	if app = strings.TrimSpace(app); app == "" {
		return "Rowsmith"
	}
	return regexp.MustCompile(`[^A-Za-z0-9._-]+`).ReplaceAllString(app, "_")
}

// privateKey reads a PEM private key: PKCS#8 (encrypted when a passphrase is
// given) or PKCS#1.
func privateKey(pemText, passphrase string) (*rsa.PrivateKey, error) {
	pemText = strings.TrimSpace(pemText)
	if pemText == "" {
		return nil, errors.New("choose the private key file (PEM)")
	}
	blk, _ := pem.Decode([]byte(pemText))
	if blk == nil {
		return nil, errors.New("the private key is not PEM")
	}
	var parsed any
	var err error
	switch blk.Type {
	case "ENCRYPTED PRIVATE KEY":
		if passphrase == "" {
			return nil, errors.New("the private key is encrypted: enter its passphrase")
		}
		parsed, err = pkcs8.ParsePKCS8PrivateKey(blk.Bytes, []byte(passphrase))
	case "RSA PRIVATE KEY":
		parsed, err = x509.ParsePKCS1PrivateKey(blk.Bytes)
	default:
		parsed, err = x509.ParsePKCS8PrivateKey(blk.Bytes)
	}
	if err != nil {
		return nil, fmt.Errorf("private key: %w", err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("Snowflake key-pair sign-in needs an RSA key")
	}
	return key, nil
}

func (c *conn) Close() error                   { return c.db.Close() }
func (c *conn) Ping(ctx context.Context) error { return c.db.PingContext(ctx) }

func (c *conn) Server(ctx context.Context) (*driver.ServerInfo, error) {
	info := &driver.ServerInfo{Product: "Snowflake", Version: c.version, User: c.user, Database: c.database,
		Extras: map[string]string{"Account": c.account, "Role": c.role}}
	if c.region != "" {
		info.Extras["Region"] = c.region
	}
	if c.wh != "" {
		info.Extras["Warehouse"] = c.wh
		if rows, err := c.show(ctx, "SHOW WAREHOUSES LIKE "+literal(c.wh)); err == nil && len(rows) > 0 {
			info.Extras["Warehouse"] = c.wh + " (" + rows[0].str("size") + ", " + strings.ToLower(rows[0].str("state")) + ")"
		}
	} else {
		info.Extras["Warehouse"] = "none: set one to run queries"
	}
	if c.ro {
		info.Extras["Session"] = "read-only"
	}
	return info, nil
}

func (c *conn) Databases(ctx context.Context) ([]driver.Database, error) {
	rows, err := c.show(ctx, "SHOW DATABASES")
	if err != nil {
		return nil, err
	}
	var out []driver.Database
	for _, r := range rows {
		d := driver.Database{Name: r.str("name"), Owner: r.str("owner"), Comment: r.str("comment")}
		kind := r.str("kind")
		d.System = d.Name == "SNOWFLAKE" || kind == "APPLICATION" || d.Name == "SNOWFLAKE_SAMPLE_DATA"
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (c *conn) Schemas(ctx context.Context, database string) ([]driver.Schema, error) {
	if database == "" {
		return nil, errors.New("choose a database")
	}
	rows, err := c.show(ctx, "SHOW SCHEMAS IN DATABASE "+quote(database))
	if err != nil {
		return nil, err
	}
	var out []driver.Schema
	for _, r := range rows {
		s := driver.Schema{Name: r.str("name"), Owner: r.str("owner")}
		s.System = s.Name == "INFORMATION_SCHEMA"
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Name == "PUBLIC" || (out[j].Name != "PUBLIC" && out[i].Name < out[j].Name)
	})
	return out, nil
}

func (c *conn) Objects(ctx context.Context, s driver.Scope) ([]driver.Object, error) {
	if s.Database == "" || s.Schema == "" {
		return nil, errors.New("choose a database and schema")
	}
	in := " IN SCHEMA " + quote(s.Database) + "." + quote(s.Schema)
	rows, err := c.show(ctx, "SHOW OBJECTS"+in)
	if err != nil {
		return nil, err
	}
	var out []driver.Object
	for _, r := range rows {
		o := driver.Object{Name: r.str("name"), Comment: r.str("comment")}
		switch kind := r.str("kind"); {
		case kind == "VIEW":
			o.Kind = "view"
			if r.str("is_materialized") == "true" {
				o.Kind = "materialized_view"
			}
		case kind == "MATERIALIZED_VIEW":
			o.Kind = "materialized_view"
		case kind == "TABLE":
			o.Kind = "table"
			if r.str("is_dynamic") == "Y" {
				o.Kind = "dynamic_table"
			} else if r.str("is_external") == "Y" {
				o.Kind = "external_table"
			}
		case kind == "TRANSIENT" || kind == "TEMPORARY":
			o.Kind, o.Extra = "table", strings.ToLower(kind)
		default:
			o.Kind = strings.ToLower(kind)
		}
		if o.Kind == "table" || o.Kind == "dynamic_table" || o.Kind == "materialized_view" {
			o.Rows, o.Size = r.int("rows"), r.int("bytes")
		}
		if cb := r.str("cluster_by"); cb != "" {
			o.Extra = strings.TrimSpace(strings.Join([]string{o.Extra, "cluster by " + strings.TrimSuffix(strings.TrimPrefix(cb, "LINEAR("), ")")}, " "))
		}
		out = append(out, o)
	}
	if rs, err := c.show(ctx, "SHOW SEQUENCES"+in); err == nil {
		for _, r := range rs {
			out = append(out, driver.Object{Name: r.str("name"), Kind: "sequence", Comment: r.str("comment"), Extra: "next " + r.str("next_value")})
		}
	}
	for _, kind := range []string{"function", "procedure"} {
		rs, err := c.show(ctx, "SHOW USER "+strings.ToUpper(kind)+"S"+in)
		if err != nil {
			continue
		}
		for _, r := range rs {
			// arguments reads ORDER_COUNT(NUMBER) RETURN NUMBER.
			sig := r.str("arguments")
			args := ""
			if i, j := strings.Index(sig, "("), strings.LastIndex(sig, ") RETURN"); i >= 0 && j > i {
				args = sig[i+1 : j]
			}
			out = append(out, driver.Object{Name: r.str("name"), Kind: kind, Extra: args, Comment: r.str("description")})
		}
	}
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
	if len(req.Filters) == 0 && req.Where == "" && req.Search == "" && t.RowEstimate != nil && t.Kind == "table" {
		// SHOW OBJECTS counts table rows exactly, without a warehouse.
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
	res, err := c.eng.ApplyEdits(ctx, t, edits)
	return res, mapError(err, sqlsplit.Statement{})
}

// ---- SHOW results -----------------------------------------------------------------

// showRow is one row of a SHOW command, by lower-case column name.
type showRow map[string]any

func (r showRow) str(k string) string {
	switch v := r[k].(type) {
	case nil:
		return ""
	case string:
		return v
	case []byte:
		return string(v)
	case time.Time:
		return v.Format(time.RFC3339)
	}
	return fmt.Sprint(r[k])
}

func (r showRow) int(k string) *int64 {
	s := r.str(k)
	if s == "" {
		return nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return nil
	}
	return &n
}

// show runs a SHOW (or DESCRIBE) command. These run in Snowflake's cloud
// services layer and never wake a warehouse.
func (c *conn) show(ctx context.Context, q string, args ...any) ([]showRow, error) {
	rows, err := c.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, mapError(err, sqlsplit.Statement{})
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out []showRow
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		r := showRow{}
		for i, n := range cols {
			r[strings.ToLower(n)] = vals[i]
		}
		out = append(out, r)
	}
	return out, mapError(rows.Err(), sqlsplit.Statement{})
}

// ---- names and errors -------------------------------------------------------------

func quote(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }

func qualify(ref driver.ObjectRef) string {
	parts := []string{}
	if ref.Database != "" {
		parts = append(parts, quote(ref.Database))
	}
	if ref.Schema != "" {
		parts = append(parts, quote(ref.Schema))
	}
	return strings.Join(append(parts, quote(ref.Name)), ".")
}

func literal(s string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s) + "'"
}

var positionRe = regexp.MustCompile(`line (\d+) at position (\d+)`)

// mapError turns a Snowflake error into a QueryError pointing at the line.
func mapError(err error, st sqlsplit.Statement) error {
	var se *sf.SnowflakeError
	if err == nil || !errors.As(err, &se) {
		return err
	}
	msg := se.Message
	if len(se.MessageArgs) > 0 {
		msg = fmt.Sprintf(se.Message, se.MessageArgs...)
	}
	msg = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(msg), "SQL compilation error:"))
	qe := &driver.QueryError{Message: strings.Join(strings.Fields(msg), " "), Code: strconv.Itoa(se.Number)}
	if se.SQLState != "" {
		qe.Detail = "SQLSTATE " + se.SQLState
	}
	if se.QueryID != "" {
		qe.Hint = "Query ID " + se.QueryID
	}
	if m := positionRe.FindStringSubmatch(msg); m != nil {
		n, _ := strconv.Atoi(m[1])
		qe.Line = max(st.Line, 1) + n - 1
	}
	return qe
}
