// Package cassandra implements the Apache Cassandra and ScyllaDB driver on
// the Apache gocql driver: keyspaces with their tables, materialized views,
// user types and functions, a grid that reads by token range and filters
// with the server's rules, edits as lightweight transactions on the primary
// key, a CQL console with cqlsh's session commands, and traced queries as
// plans.
package cassandra

import (
	"context"
	"errors"
	"fmt"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	gocql "github.com/apache/cassandra-gocql-driver/v2"

	"rowsmith/internal/driver"
	"rowsmith/internal/sqlsplit"
)

type cqlDriver struct {
	id, name string
	order    int
}

func init() {
	driver.Register(cqlDriver{id: "cassandra", name: "Cassandra", order: 67})
	driver.Register(cqlDriver{id: "scylladb", name: "ScyllaDB", order: 68})
}

var kinds = []driver.KindInfo{
	{Kind: "table", Label: "Tables", Icon: "table", Browse: true},
	{Kind: "materialized_view", Label: "Materialized views", Icon: "matview", Browse: true},
	{Kind: "type", Label: "User types", Icon: "type"},
	{Kind: "function", Label: "Functions", Icon: "function"},
	{Kind: "aggregate", Label: "Aggregates", Icon: "function"},
}

var types = []string{
	"text", "ascii", "int", "bigint", "smallint", "tinyint", "varint", "float", "double", "decimal", "boolean",
	"uuid", "timeuuid", "timestamp", "date", "time", "duration", "blob", "inet", "counter",
	"list<text>", "set<text>", "map<text, text>", "frozen<list<int>>", "tuple<int, text>", "vector<float, 3>",
}

var consistencies = []driver.Option{
	{Value: "LOCAL_QUORUM", Label: "LOCAL_QUORUM"}, {Value: "LOCAL_ONE", Label: "LOCAL_ONE"}, {Value: "ONE", Label: "ONE"},
	{Value: "QUORUM", Label: "QUORUM"}, {Value: "EACH_QUORUM", Label: "EACH_QUORUM"}, {Value: "ALL", Label: "ALL"},
}

func (d cqlDriver) Info() driver.Info {
	fields := []driver.Field{
		{Key: "host", Label: "Hosts", Type: driver.FieldText, Required: true, Placeholder: "node1.example.com, node2.example.com", Span: 4,
			Help: "One or more nodes to reach the cluster through; the others are found from them."},
		{Key: "port", Label: "Port", Type: driver.FieldNumber, Default: 9042, Span: 2},
		{Key: "user", Label: "User", Type: driver.FieldText, Section: "auth", Span: 3},
		{Key: "password", Label: "Password", Type: driver.FieldPassword, Secret: true, Section: "auth", Span: 3},
		{Key: "database", Label: "Keyspace", Type: driver.FieldText, Placeholder: "optional — all keyspaces are listed", Span: 3},
		{Key: "datacenter", Label: "Local data center", Type: driver.FieldText, Span: 3, Placeholder: "found from the first node",
			Help: "Queries go to nodes in this data center first."},
	}
	fields = append(fields, driver.TLSFields("disable")...)
	fields = append(fields,
		driver.Field{Key: "consistency", Label: "Consistency", Type: driver.FieldSelect, Default: "LOCAL_QUORUM", Options: consistencies,
			Section: "advanced", Span: 3, Help: "For reads and writes from the grid and the console; CONSISTENCY in the console changes it there."},
		driver.Field{Key: "contactOnly", Label: "Use only the listed hosts", Type: driver.FieldBool, Section: "advanced", Span: 3,
			Help: "For SSH tunnels and networks where the addresses nodes announce can't be reached."},
		driver.Field{Key: "connectTimeout", Label: "Connect timeout (seconds)", Type: driver.FieldNumber, Default: 15, Section: "advanced", Span: 3},
		driver.Field{Key: "queryTimeout", Label: "Query timeout (seconds)", Type: driver.FieldNumber, Default: 60, Section: "advanced", Span: 3},
	)
	desc := "Apache Cassandra 4.0 and later, and DataStax Enterprise"
	if d.id == "scylladb" {
		desc = "ScyllaDB 6 and later, and ScyllaDB Cloud"
	}
	indexTypes := []string{"sai", "secondary"}
	if d.id == "scylladb" {
		indexTypes = []string{"secondary"}
	}
	return driver.Info{
		ID: d.id, Name: d.name, Order: d.order, Description: desc,
		Dialect: "cql", DefaultPort: 9042, SSH: true, QuoteChar: `"`,
		Fields: fields,
		Caps: driver.Caps{Databases: true, SQL: true, EditRows: true, DDL: true, CreateDatabase: true, DropDatabase: true,
			Explain: true, Processes: true, Variables: true},
		Kinds: kinds, Types: types,
		Design: &driver.TableDesign{
			Columns: true, TableComment: true, PrimaryKey: true, Indexes: true, IndexTypes: indexTypes,
			Options: designOptions,
			Note:    "Rows are placed by the partition key and ordered by the clustering columns, so the primary key can't change once the table exists.",
		},
	}
}

var designOptions = []driver.Field{
	{Key: "partition_key", Label: "Partition key", Type: driver.FieldText, Placeholder: "the first primary key column",
		Help: "The primary key columns that choose the partition; the rest order rows inside it."},
	{Key: "clustering_order", Label: "Clustering order", Type: driver.FieldText, Placeholder: "placed_at DESC"},
	{Key: "default_time_to_live", Label: "Default time to live (seconds)", Type: driver.FieldNumber, Placeholder: "0 (rows don't expire)"},
	{Key: "compaction", Label: "Compaction", Type: driver.FieldSelect, Options: []driver.Option{
		{Value: "", Label: "Server default"},
		{Value: "SizeTieredCompactionStrategy", Label: "Size-tiered"},
		{Value: "LeveledCompactionStrategy", Label: "Leveled"},
		{Value: "TimeWindowCompactionStrategy", Label: "Time-window"},
		{Value: "UnifiedCompactionStrategy", Label: "Unified (Cassandra 5)"},
		{Value: "IncrementalCompactionStrategy", Label: "Incremental (ScyllaDB)"},
	}},
	{Key: "gc_grace_seconds", Label: "Tombstone grace (seconds)", Type: driver.FieldNumber, Placeholder: "864000"},
}

type conn struct {
	cfg     *gocql.ClusterConfig
	s       *gocql.Session
	ro      bool
	defKS   string
	cons    gocql.Consistency
	product string // Cassandra or ScyllaDB
	version string
	dc      string
	user    string
	timeout time.Duration

	mu       sync.Mutex
	sessions map[string]*gocql.Session // console sessions by keyspace
}

func (d cqlDriver) Open(ctx context.Context, p driver.OpenParams) (driver.Conn, error) {
	var hosts []string
	for _, h := range strings.Split(p.String("host"), ",") {
		if h = strings.TrimSpace(h); h != "" {
			hosts = append(hosts, h)
		}
	}
	if len(hosts) == 0 {
		return nil, errors.New("enter at least one host")
	}
	port := p.Int("port", 9042)
	cfg := gocql.NewCluster(hosts...)
	cfg.Port = port
	cfg.ConnectTimeout = time.Duration(p.Int("connectTimeout", 15)) * time.Second
	cfg.Timeout = time.Duration(p.Int("queryTimeout", 60)) * time.Second
	cfg.NumConns = 1
	cfg.ReconnectionPolicy = &gocql.ConstantReconnectionPolicy{MaxRetries: 3, Interval: time.Second}
	cons, err := parseConsistency(p.String("consistency"))
	if err != nil {
		return nil, err
	}
	cfg.Consistency = cons
	cfg.SerialConsistency = gocql.LocalSerial
	if user := strings.TrimSpace(p.String("user")); user != "" {
		cfg.Authenticator = gocql.PasswordAuthenticator{Username: user, Password: p.Secret("password")}
	}
	if dc := strings.TrimSpace(p.String("datacenter")); dc != "" {
		cfg.PoolConfig.HostSelectionPolicy = gocql.TokenAwareHostPolicy(gocql.DCAwareRoundRobinPolicy(dc))
	}
	if p.Bool("contactOnly") {
		// Only the listed nodes, not the addresses the cluster announces.
		cfg.DisableInitialHostLookup = true
	}
	if p.Dial != nil {
		cfg.Dialer = dialer(p.Dial)
	}
	tc, err := driver.TLSConfig(p, hosts[0])
	if err != nil {
		return nil, err
	}
	if tc != nil {
		cfg.SslOpts = &gocql.SslOptions{Config: tc, EnableHostVerification: !tc.InsecureSkipVerify}
	}
	c := &conn{cfg: cfg, ro: p.ReadOnly, defKS: strings.TrimSpace(p.String("database")), cons: cons, user: strings.TrimSpace(p.String("user")),
		timeout: cfg.Timeout, sessions: map[string]*gocql.Session{}}
	type opened struct {
		s   *gocql.Session
		err error
	}
	done := make(chan opened, 1)
	go func() {
		s, err := cfg.CreateSession()
		done <- opened{s, err}
	}()
	select {
	case o := <-done:
		if o.err != nil {
			return nil, mapError(o.err)
		}
		c.s = o.s
	case <-ctx.Done():
		go func() {
			if o := <-done; o.s != nil {
				o.s.Close()
			}
		}()
		return nil, ctx.Err()
	}
	var release string
	if err := c.s.Query("SELECT release_version, data_center FROM system.local").WithContext(ctx).Scan(&release, &c.dc); err != nil {
		c.Close()
		return nil, mapError(err)
	}
	c.product, c.version = "Cassandra", release
	var scylla string
	if c.s.Query("SELECT version FROM system.versions WHERE key = 'local'").WithContext(ctx).Scan(&scylla) == nil && scylla != "" {
		c.product, c.version = "ScyllaDB", strings.SplitN(scylla, "-", 2)[0]
	}
	if c.defKS != "" {
		var n int
		if err := c.s.Query("SELECT count(*) FROM system_schema.keyspaces WHERE keyspace_name = ?", c.defKS).WithContext(ctx).Scan(&n); err == nil && n == 0 {
			c.Close()
			return nil, fmt.Errorf("keyspace %q does not exist", c.defKS)
		}
	}
	return c, nil
}

type dialer driver.DialFunc

func (d dialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	return d(ctx, network, addr)
}

func parseConsistency(s string) (gocql.Consistency, error) {
	if s == "" {
		return gocql.LocalQuorum, nil
	}
	c, err := gocql.ParseConsistencyWrapper(strings.ToUpper(strings.TrimSpace(s)))
	if err != nil {
		return 0, fmt.Errorf("unknown consistency level %q", s)
	}
	return c, nil
}

func (c *conn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, s := range c.sessions {
		s.Close()
	}
	c.sessions = map[string]*gocql.Session{}
	if c.s != nil {
		c.s.Close()
	}
	return nil
}

func (c *conn) Ping(ctx context.Context) error {
	var now time.Time
	return mapError(c.s.Query("SELECT now() FROM system.local").WithContext(ctx).Scan(&now))
}

// session returns a session whose unqualified names resolve in a keyspace
// (gocql keeps one keyspace per session; USE on a pooled session would move
// only one of its connections).
func (c *conn) session(ks string) (*gocql.Session, error) {
	if ks == "" {
		return c.s, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if s, ok := c.sessions[ks]; ok {
		return s, nil
	}
	cfg := *c.cfg
	cfg.Keyspace = ks
	s, err := cfg.CreateSession()
	if err != nil {
		return nil, mapError(err)
	}
	c.sessions[ks] = s
	return s, nil
}

func (c *conn) query(ctx context.Context, stmt string, args ...any) *gocql.Query {
	return c.s.Query(stmt, args...).WithContext(ctx).Consistency(c.cons)
}

func (c *conn) Server(ctx context.Context) (*driver.ServerInfo, error) {
	info := &driver.ServerInfo{Product: c.product, Version: c.version, User: c.user, Database: c.defKS, Extras: map[string]string{}}
	var cluster string
	if c.query(ctx, "SELECT cluster_name FROM system.local").Scan(&cluster) == nil {
		info.Extras["Cluster"] = cluster
	}
	info.Extras["Data center"] = c.dc
	nodes := 1
	iter := c.query(ctx, "SELECT peer FROM system.peers").Iter()
	var peer net.IP
	for iter.Scan(&peer) {
		nodes++
	}
	if iter.Close() == nil {
		info.Extras["Nodes"] = strconv.Itoa(nodes)
	}
	info.Extras["Consistency"] = c.cons.String()
	if c.ro {
		info.Extras["Session"] = "read-only"
	}
	return info, nil
}

func systemKeyspace(name string) bool {
	return strings.HasPrefix(name, "system") || strings.HasPrefix(name, "dse_") || name == "solr_admin" || name == "OpsCenter"
}

func (c *conn) Databases(ctx context.Context) ([]driver.Database, error) {
	counts := map[string]int64{}
	iter := c.query(ctx, "SELECT keyspace_name FROM system_schema.tables").Iter()
	var ks string
	for iter.Scan(&ks) {
		counts[ks]++
	}
	if err := iter.Close(); err != nil {
		return nil, mapError(err)
	}
	var out []driver.Database
	iter = c.query(ctx, "SELECT keyspace_name, replication FROM system_schema.keyspaces").Iter()
	var repl map[string]string
	for iter.Scan(&ks, &repl) {
		n := counts[ks]
		out = append(out, driver.Database{Name: ks, Tables: &n, System: systemKeyspace(ks), Comment: replicationText(repl)})
	}
	if err := iter.Close(); err != nil {
		return nil, mapError(err)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].System != out[j].System {
			return !out[i].System
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// replicationText reads a keyspace's replication map, such as
// NetworkTopologyStrategy dc1=3.
func replicationText(r map[string]string) string {
	class := r["class"]
	class = class[strings.LastIndexByte(class, '.')+1:]
	var parts []string
	for k, v := range r {
		if k != "class" {
			parts = append(parts, k+"="+v)
		}
	}
	sort.Strings(parts)
	return strings.TrimSpace(class + " " + strings.Join(parts, ", "))
}

func (c *conn) Schemas(context.Context, string) ([]driver.Schema, error) { return nil, nil }

func (c *conn) Objects(ctx context.Context, s driver.Scope) ([]driver.Object, error) {
	ks := c.ksOf(s.Database)
	if ks == "" {
		return nil, errors.New("choose a keyspace")
	}
	var tn string
	// No row counts: the server's estimates count partitions, roughly,
	// and only a full scan counts rows.
	var out []driver.Object
	var comment string
	iter := c.query(ctx, "SELECT table_name, comment FROM system_schema.tables WHERE keyspace_name = ?", ks).Iter()
	for iter.Scan(&tn, &comment) {
		out = append(out, driver.Object{Name: tn, Kind: "table", Comment: comment})
	}
	if err := iter.Close(); err != nil {
		return nil, mapError(err)
	}
	var base string
	hidden := c.indexViews(ctx, ks)
	iter = c.query(ctx, "SELECT view_name, base_table_name FROM system_schema.views WHERE keyspace_name = ?", ks).Iter()
	for iter.Scan(&tn, &base) {
		if !hidden[tn] {
			out = append(out, driver.Object{Name: tn, Kind: "materialized_view", Extra: "of " + base})
		}
	}
	iter.Close()
	iter = c.query(ctx, "SELECT type_name FROM system_schema.types WHERE keyspace_name = ?", ks).Iter()
	for iter.Scan(&tn) {
		out = append(out, driver.Object{Name: tn, Kind: "type"})
	}
	iter.Close()
	var args []string
	iter = c.query(ctx, "SELECT function_name, argument_types FROM system_schema.functions WHERE keyspace_name = ?", ks).Iter()
	for iter.Scan(&tn, &args) {
		out = append(out, driver.Object{Name: tn, Kind: "function", Extra: "(" + strings.Join(args, ", ") + ")"})
	}
	iter.Close()
	iter = c.query(ctx, "SELECT aggregate_name, argument_types FROM system_schema.aggregates WHERE keyspace_name = ?", ks).Iter()
	for iter.Scan(&tn, &args) {
		out = append(out, driver.Object{Name: tn, Kind: "aggregate", Extra: "(" + strings.Join(args, ", ") + ")"})
	}
	iter.Close()
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// indexViews names the views ScyllaDB keeps behind secondary indexes
// (<index>_index), which belong to the index rather than the keyspace.
func (c *conn) indexViews(ctx context.Context, ks string) map[string]bool {
	out := map[string]bool{}
	if c.product != "ScyllaDB" {
		return out
	}
	iter := c.query(ctx, "SELECT index_name FROM system_schema.indexes WHERE keyspace_name = ?", ks).Iter()
	var name string
	for iter.Scan(&name) {
		out[name+"_index"] = true
	}
	iter.Close()
	return out
}

func (c *conn) ksOf(name string) string {
	if name != "" {
		return name
	}
	return c.defKS
}

// ---- names and errors ---------------------------------------------------------------

var plainIdent = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// reserved holds the CQL keywords that can't be column or table names
// unquoted.
var reserved = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`add aggregate all allow alter and apply asc authorize batch begin by columnfamily create default delete desc
		describe drop entries execute from full grant if in index infinity insert into is keyspace limit materialized modify nan norecursive
		not null of on or order primary rename replace revoke schema select set table to token truncate unlogged update use using view where with`) {
		reserved[w] = true
	}
}

// quote writes a name as CQL reads it: unquoted names are folded to lower
// case, so anything else is double-quoted.
func quote(name string) string {
	if plainIdent.MatchString(name) && !reserved[name] {
		return name
	}
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

func qualify(ks, name string) string {
	if ks == "" {
		return quote(name)
	}
	return quote(ks) + "." + quote(name)
}

func literal(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

var syntaxPos = regexp.MustCompile(`^line (\d+):(\d+) `)

func mapError(err error) error {
	if err == nil {
		return nil
	}
	var re gocql.RequestError
	if errors.As(err, &re) {
		msg := re.Message()
		qe := &driver.QueryError{Message: msg, Code: errorName(re.Code())}
		if m := syntaxPos.FindStringSubmatch(msg); m != nil {
			qe.Line, _ = strconv.Atoi(m[1])
			col, _ := strconv.Atoi(m[2])
			qe.Position = col + 1
		}
		if re.Code() == gocql.ErrCodeUnavailable || re.Code() == gocql.ErrCodeReadTimeout || re.Code() == gocql.ErrCodeWriteTimeout {
			qe.Hint = "Not enough replicas answered at this consistency level; try a lower one with CONSISTENCY, or check the nodes."
		}
		return qe
	}
	if errors.Is(err, gocql.ErrNoConnections) || errors.Is(err, gocql.ErrNoConnectionsStarted) {
		return fmt.Errorf("no node of the cluster could be reached: %w", err)
	}
	return err
}

func errorName(code int) string {
	switch code {
	case gocql.ErrCodeSyntax:
		return "SYNTAX_ERROR"
	case gocql.ErrCodeInvalid:
		return "INVALID"
	case gocql.ErrCodeUnauthorized:
		return "UNAUTHORIZED"
	case gocql.ErrCodeCredentials:
		return "BAD_CREDENTIALS"
	case gocql.ErrCodeUnavailable:
		return "UNAVAILABLE"
	case gocql.ErrCodeReadTimeout:
		return "READ_TIMEOUT"
	case gocql.ErrCodeWriteTimeout:
		return "WRITE_TIMEOUT"
	case gocql.ErrCodeAlreadyExists:
		return "ALREADY_EXISTS"
	case gocql.ErrCodeConfig:
		return "CONFIG_ERROR"
	case gocql.ErrCodeOverloaded:
		return "OVERLOADED"
	}
	return fmt.Sprintf("0x%04X", code)
}

func (c *conn) Classify(stmt string) driver.StatementKind {
	k, _ := sqlsplit.Classify(stmt, sqlsplit.CQL)
	return k
}
