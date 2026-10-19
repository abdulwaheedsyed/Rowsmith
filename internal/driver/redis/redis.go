// Package redis implements the Redis and Valkey driver: keys listed by
// pattern and grouped by type, a grid for each value type (strings, hashes,
// lists, sets, sorted sets, streams and JSON documents) with edits checked
// before they run, expiry, and a redis-cli style console.
package redis

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"rowsmith/internal/driver"
)

type redisDriver struct {
	id, name string
	order    int
}

func init() {
	driver.Register(redisDriver{id: "redis", name: "Redis", order: 65})
	driver.Register(redisDriver{id: "valkey", name: "Valkey", order: 66})
}

// Key types as TYPE reports them, which are also the object kinds.
const (
	kindString = "string"
	kindHash   = "hash"
	kindList   = "list"
	kindSet    = "set"
	kindZSet   = "zset"
	kindStream = "stream"
	kindJSON   = "ReJSON-RL"
)

var kinds = []driver.KindInfo{
	{Kind: kindString, Label: "Strings", Icon: "string", Browse: true},
	{Kind: kindHash, Label: "Hashes", Icon: "hash", Browse: true},
	{Kind: kindList, Label: "Lists", Icon: "list", Browse: true},
	{Kind: kindSet, Label: "Sets", Icon: "set", Browse: true},
	{Kind: kindZSet, Label: "Sorted sets", Icon: "zset", Browse: true},
	{Kind: kindStream, Label: "Streams", Icon: "stream", Browse: true},
	{Kind: kindJSON, Label: "JSON documents", Icon: "json", Browse: true},
}

func browsable(kind string) bool {
	for _, k := range kinds {
		if k.Kind == kind {
			return true
		}
	}
	return false
}

func (d redisDriver) Info() driver.Info {
	fields := []driver.Field{
		{Key: "host", Label: "Host", Type: driver.FieldText, Required: true, Placeholder: "cache.example.com", Span: 4},
		{Key: "port", Label: "Port", Type: driver.FieldNumber, Default: 6379, Span: 2},
		{Key: "user", Label: "User", Type: driver.FieldText, Section: "auth", Span: 3, Placeholder: "default",
			Help: "An ACL user. Leave empty to sign in with the password alone."},
		{Key: "password", Label: "Password", Type: driver.FieldPassword, Secret: true, Section: "auth", Span: 3},
		{Key: "database", Label: "Database number", Type: driver.FieldNumber, Default: 0, Span: 3,
			Help: "The database to open first; the others stay a click away."},
	}
	fields = append(fields, driver.TLSFields("disable")...)
	fields = append(fields, driver.Field{Key: "connectTimeout", Label: "Connect timeout (seconds)", Type: driver.FieldNumber, Default: 15, Section: "advanced", Span: 3})
	desc := "Redis 6 and later, Redis Stack, Amazon ElastiCache and other Redis-compatible servers"
	schemes := []string{"redis", "rediss"}
	if d.id == "valkey" {
		desc = "Valkey 7.2 and later, Amazon ElastiCache and MemoryDB for Valkey"
		schemes = []string{"valkey", "valkeys", "redis", "rediss"}
	}
	return driver.Info{
		ID: d.id, Name: d.name, Order: d.order, Description: desc,
		Dialect: "redis", DefaultPort: 6379, SSH: true, QuoteChar: `"`,
		URLSchemes: schemes,
		Fields:     fields,
		Caps:       driver.Caps{Databases: true, Keys: true, EditRows: true, DDL: true, Processes: true, Variables: true, Transactions: true},
		Kinds:      kinds,
		Types:      []string{kindString, kindHash, kindList, kindSet, kindZSet, kindStream},
	}
}

type conn struct {
	opt     goredis.Options
	ro      bool
	defDB   int
	mu      sync.Mutex
	clients map[int]*goredis.Client
	// From INFO server at connect.
	product string // Redis or Valkey
	version string
}

func (d redisDriver) Open(ctx context.Context, p driver.OpenParams) (driver.Conn, error) {
	host, port := driver.HostPort(p, 6379)
	if host == "" {
		return nil, errors.New("host is required")
	}
	timeout := time.Duration(p.Int("connectTimeout", 15)) * time.Second
	opt := goredis.Options{
		Addr:     net.JoinHostPort(host, strconv.Itoa(port)),
		Username: strings.TrimSpace(p.String("user")),
		Password: p.Secret("password"),
		// RESP2 keeps replies in the server's order, as redis-cli shows them,
		// and works with every server version.
		Protocol:        2,
		DialTimeout:     timeout,
		ReadTimeout:     30 * time.Second,
		WriteTimeout:    30 * time.Second,
		PoolSize:        4,
		DisableIdentity: true,
		ClientName:      clientName(p.AppName),
	}
	if p.Dial != nil {
		dial := p.Dial
		opt.Dialer = func(ctx context.Context, network, addr string) (net.Conn, error) { return dial(ctx, network, addr) }
	}
	tc, err := driver.TLSConfig(p, host)
	if err != nil {
		return nil, err
	}
	opt.TLSConfig = tc
	c := &conn{opt: opt, ro: p.ReadOnly, defDB: p.Int("database", 0), clients: map[int]*goredis.Client{}}
	if c.defDB < 0 {
		c.defDB = 0
	}
	pctx, cancel := context.WithTimeout(ctx, timeout+5*time.Second)
	defer cancel()
	info, err := c.client(c.defDB).Info(pctx, "server").Result()
	if err != nil {
		c.Close()
		return nil, mapError(err)
	}
	f := infoFields(info)
	c.product, c.version = "Redis", f["redis_version"]
	if v := f["valkey_version"]; v != "" || f["server_name"] == "valkey" {
		c.product = "Valkey"
		if v != "" {
			c.version = v
		}
	}
	return c, nil
}

// clientName is what CLIENT LIST shows for Rowsmith's connections; names
// cannot contain spaces.
func clientName(app string) string {
	app = strings.Join(strings.Fields(app), "-")
	if app == "" {
		return "rowsmith"
	}
	return app
}

// client returns the pooled client for a database number.
func (c *conn) client(db int) *goredis.Client {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cl, ok := c.clients[db]; ok {
		return cl
	}
	opt := c.opt
	opt.DB = db
	cl := goredis.NewClient(&opt)
	c.clients[db] = cl
	return cl
}

// dbOf reads a database name (db0, db1…) from a scope; empty means the
// connection's default.
func (c *conn) dbOf(name string) (int, error) {
	if name == "" {
		return c.defDB, nil
	}
	n, err := strconv.Atoi(strings.TrimPrefix(name, "db"))
	if err != nil || n < 0 {
		return 0, fmt.Errorf("unknown database %q", name)
	}
	return n, nil
}

func dbName(n int) string { return "db" + strconv.Itoa(n) }

func (c *conn) clientFor(name string) (*goredis.Client, error) {
	n, err := c.dbOf(name)
	if err != nil {
		return nil, err
	}
	return c.client(n), nil
}

func (c *conn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, cl := range c.clients {
		cl.Close()
	}
	c.clients = map[int]*goredis.Client{}
	return nil
}

func (c *conn) Ping(ctx context.Context) error {
	return mapError(c.client(c.defDB).Ping(ctx).Err())
}

func (c *conn) Server(ctx context.Context) (*driver.ServerInfo, error) {
	cl := c.client(c.defDB)
	info := &driver.ServerInfo{Product: c.product, Version: c.version, Database: dbName(c.defDB), Extras: map[string]string{}}
	if u, err := cl.Do(ctx, "ACL", "WHOAMI").Text(); err == nil {
		info.User = u
	}
	if s, err := cl.Info(ctx, "server", "replication", "memory").Result(); err == nil {
		f := infoFields(s)
		if m := f["redis_mode"]; m == "cluster" {
			info.Extras["Mode"] = "cluster node: only its own keys are listed"
		} else if m == "sentinel" {
			info.Extras["Mode"] = "sentinel"
		}
		if r := f["role"]; r != "" {
			info.Extras["Role"] = r
		}
		if m := f["used_memory_human"]; m != "" {
			info.Extras["Memory used"] = m
		}
	}
	if c.ro {
		info.Extras["Session"] = "read-only"
	}
	return info, nil
}

// infoFields parses INFO output into its key:value pairs.
func infoFields(s string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' {
			continue
		}
		if k, v, ok := strings.Cut(line, ":"); ok {
			out[k] = v
		}
	}
	return out
}

// Databases lists the numbered databases with their key counts. Servers
// that hide CONFIG (managed services) show the ones that hold keys, plus
// the default 16.
func (c *conn) Databases(ctx context.Context) ([]driver.Database, error) {
	cl := c.client(c.defDB)
	count := 0
	if v, err := cl.ConfigGet(ctx, "databases").Result(); err == nil {
		count, _ = strconv.Atoi(v["databases"])
	}
	keys := map[int]int64{}
	s, err := cl.Info(ctx, "keyspace").Result()
	if err != nil {
		return nil, mapError(err)
	}
	for k, v := range infoFields(s) {
		n, err := strconv.Atoi(strings.TrimPrefix(k, "db"))
		if err != nil || !strings.HasPrefix(k, "db") {
			continue
		}
		for _, part := range strings.Split(v, ",") {
			if kv, nv, ok := strings.Cut(part, "="); ok && kv == "keys" {
				keys[n], _ = strconv.ParseInt(nv, 10, 64)
			}
		}
	}
	if count == 0 {
		count = 16
		for n := range keys {
			if n+1 > count {
				count = n + 1
			}
		}
		if c.defDB+1 > count {
			count = c.defDB + 1
		}
	}
	out := make([]driver.Database, 0, count)
	for n := 0; n < count; n++ {
		k := keys[n]
		out = append(out, driver.Database{Name: dbName(n), Tables: &k})
	}
	return out, nil
}

func (c *conn) Schemas(context.Context, string) ([]driver.Schema, error) { return nil, nil }

// Navigator listing: at most this many keys, found within this time.
const (
	objectLimit = 2000
	scanBudget  = 10 * time.Second
)

func (c *conn) Objects(ctx context.Context, s driver.Scope) ([]driver.Object, error) {
	return c.ObjectsMatching(ctx, s, "*", objectLimit)
}

// ObjectsMatching implements driver.KeyLister: the keys matching a glob
// pattern, with their types and lengths, sorted by name.
func (c *conn) ObjectsMatching(ctx context.Context, s driver.Scope, pattern string, limit int) ([]driver.Object, error) {
	cl, err := c.clientFor(s.Database)
	if err != nil {
		return nil, err
	}
	if pattern == "" {
		pattern = "*"
	}
	if limit <= 0 || limit > objectLimit {
		limit = objectLimit
	}
	keys, err := scanKeys(ctx, cl, pattern, limit)
	if err != nil {
		return nil, mapError(err)
	}
	types, err := keyTypes(ctx, cl, keys)
	if err != nil {
		return nil, mapError(err)
	}
	out := make([]driver.Object, 0, len(keys))
	var lenCmds []*goredis.IntCmd
	var lenIdx []int
	pipe := cl.Pipeline()
	for i, k := range keys {
		t := types[i]
		if t == "none" {
			continue // expired since the scan
		}
		out = append(out, driver.Object{Name: k, Kind: t})
		var cmd *goredis.IntCmd
		switch t {
		case kindHash:
			cmd = pipe.HLen(ctx, k)
		case kindList:
			cmd = pipe.LLen(ctx, k)
		case kindSet:
			cmd = pipe.SCard(ctx, k)
		case kindZSet:
			cmd = pipe.ZCard(ctx, k)
		case kindStream:
			cmd = pipe.XLen(ctx, k)
		}
		if cmd != nil {
			lenCmds = append(lenCmds, cmd)
			lenIdx = append(lenIdx, len(out)-1)
		}
	}
	if len(lenCmds) > 0 {
		pipe.Exec(ctx)
		for i, cmd := range lenCmds {
			if n, err := cmd.Result(); err == nil {
				out[lenIdx[i]].Rows = &n
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// scanKeys walks SCAN until it has limit keys, the keyspace ends or the
// time budget runs out.
func scanKeys(ctx context.Context, cl *goredis.Client, pattern string, limit int) ([]string, error) {
	deadline := time.Now().Add(scanBudget)
	seen := map[string]bool{}
	var keys []string
	var cursor uint64
	for {
		batch, next, err := cl.Scan(ctx, cursor, pattern, 1000).Result()
		if err != nil {
			return nil, err
		}
		for _, k := range batch {
			// SCAN may return a key more than once.
			if !seen[k] {
				seen[k] = true
				keys = append(keys, k)
			}
		}
		cursor = next
		if cursor == 0 || len(keys) >= limit || time.Now().After(deadline) {
			break
		}
	}
	sort.Strings(keys)
	if len(keys) > limit {
		keys = keys[:limit]
	}
	return keys, nil
}

func keyTypes(ctx context.Context, cl *goredis.Client, keys []string) ([]string, error) {
	out := make([]string, len(keys))
	for start := 0; start < len(keys); start += 1000 {
		end := min(start+1000, len(keys))
		pipe := cl.Pipeline()
		cmds := make([]*goredis.StatusCmd, end-start)
		for i := start; i < end; i++ {
			cmds[i-start] = pipe.Type(ctx, keys[i])
		}
		if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, goredis.Nil) {
			return nil, err
		}
		for i, cmd := range cmds {
			out[start+i] = cmd.Val()
		}
	}
	return out, nil
}

// keyType reads one key's type, failing when the key is gone.
func keyType(ctx context.Context, cl *goredis.Client, key string) (string, error) {
	t, err := cl.Type(ctx, key).Result()
	if err != nil {
		return "", mapError(err)
	}
	if t == "none" {
		return "", fmt.Errorf("key %q does not exist (it may have expired or been deleted)", key)
	}
	return t, nil
}

func (c *conn) Describe(ctx context.Context, ref driver.ObjectRef) (*driver.Table, error) {
	cl, err := c.clientFor(ref.Database)
	if err != nil {
		return nil, err
	}
	t, err := keyType(ctx, cl, ref.Name)
	if err != nil {
		return nil, err
	}
	tab := &driver.Table{Ref: driver.ObjectRef{Database: ref.Database, Name: ref.Name, Kind: t}, Kind: t,
		Columns: columnsFor(t), Indexes: []driver.Index{}, ForeignKeys: []driver.ForeignKey{}, Referenced: []driver.ForeignKey{},
		Checks: []driver.Check{}, Triggers: []driver.Trigger{}, Options: map[string]string{"Type": typeLabel(t)}}
	for _, col := range tab.Columns {
		if col.PrimaryKey {
			tab.PrimaryKey = append(tab.PrimaryKey, col.Name)
		}
	}
	tab.RowKey = tab.PrimaryKey
	tab.RowKeyKind = "primary"
	tab.Editable = !c.ro && browsable(t)

	pipe := cl.Pipeline()
	ttl := pipe.PTTL(ctx, ref.Name)
	enc := pipe.ObjectEncoding(ctx, ref.Name)
	mem := pipe.MemoryUsage(ctx, ref.Name)
	var n *goredis.IntCmd
	switch t {
	case kindString:
		n = pipe.StrLen(ctx, ref.Name)
	case kindHash:
		n = pipe.HLen(ctx, ref.Name)
	case kindList:
		n = pipe.LLen(ctx, ref.Name)
	case kindSet:
		n = pipe.SCard(ctx, ref.Name)
	case kindZSet:
		n = pipe.ZCard(ctx, ref.Name)
	case kindStream:
		n = pipe.XLen(ctx, ref.Name)
	}
	pipe.Exec(ctx)
	if d, err := ttl.Result(); err == nil {
		tab.Options["Expires"] = ttlText(d)
		if d > 0 {
			tab.Options["TTL (ms)"] = strconv.FormatInt(d.Milliseconds(), 10)
		}
	}
	if e, err := enc.Result(); err == nil {
		tab.Options["Encoding"] = e
	}
	if m, err := mem.Result(); err == nil {
		tab.Size = &m
	}
	if n != nil {
		if v, err := n.Result(); err == nil {
			if t == kindString {
				tab.Options["Length"] = strconv.FormatInt(v, 10) + " bytes"
			} else {
				tab.RowEstimate = &v
			}
		}
	}
	return tab, nil
}

// ttlText describes PTTL's answer (negative: no expiry) in its two
// largest units: "in 29d 23h", "in 4m 10s".
func ttlText(d time.Duration) string {
	if d < 0 {
		return "never"
	}
	s := int64(d.Round(time.Second) / time.Second)
	switch {
	case s >= 86400:
		return fmt.Sprintf("in %dd %dh", s/86400, s%86400/3600)
	case s >= 3600:
		return fmt.Sprintf("in %dh %dm", s/3600, s%3600/60)
	case s >= 60:
		return fmt.Sprintf("in %dm %ds", s/60, s%60)
	}
	return fmt.Sprintf("in %ds", s)
}

func typeLabel(t string) string {
	switch t {
	case kindZSet:
		return "sorted set"
	case kindJSON:
		return "JSON"
	}
	return t
}

func col(name string, kind driver.ValueKind, typ string, pk bool) driver.Column {
	return driver.Column{Name: name, Type: typ, BaseType: typ, Kind: kind, PrimaryKey: pk}
}

// columnsFor gives the grid's columns for a key type.
func columnsFor(t string) []driver.Column {
	switch t {
	case kindString:
		return []driver.Column{col("value", driver.KindText, "string", false)}
	case kindHash:
		return []driver.Column{col("field", driver.KindText, "string", true), col("value", driver.KindText, "string", false)}
	case kindList:
		return []driver.Column{col("index", driver.KindInt, "integer", true), col("value", driver.KindText, "string", false)}
	case kindSet:
		return []driver.Column{col("member", driver.KindText, "string", true)}
	case kindZSet:
		return []driver.Column{col("member", driver.KindText, "string", true), col("score", driver.KindFloat, "double", false)}
	case kindStream:
		return []driver.Column{col("id", driver.KindString, "stream id", true), col("fields", driver.KindObject, "fields", false)}
	case kindJSON:
		return []driver.Column{col("value", driver.KindJSON, "json", false)}
	}
	return []driver.Column{}
}

func mapError(err error) error {
	if err == nil {
		return nil
	}
	var re goredis.Error
	if errors.As(err, &re) {
		msg := re.Error()
		code, _, _ := strings.Cut(msg, " ")
		if code == strings.ToUpper(code) && len(code) > 2 {
			return &driver.QueryError{Message: msg, Code: code}
		}
		return &driver.QueryError{Message: msg}
	}
	return err
}
