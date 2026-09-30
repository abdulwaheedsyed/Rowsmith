// Package mongodb implements the MongoDB driver: document browsing with
// inferred schemas, safe single-document edits, and a mongosh-style console.
package mongodb

import (
	"context"
	"errors"
	"fmt"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readpref"

	"rowsmith/internal/driver"
)

type mongoDriver struct{}

func init() { driver.Register(mongoDriver{}) }

var kinds = []driver.KindInfo{
	{Kind: "collection", Label: "Collections", Icon: "table", Browse: true},
	{Kind: "timeseries", Label: "Time series", Icon: "table", Browse: true},
	{Kind: "view", Label: "Views", Icon: "view", Browse: true},
}

func (mongoDriver) Info() driver.Info {
	connect := map[string][]string{"mode": {"fields"}}
	return driver.Info{
		ID: "mongodb", Name: "MongoDB", Order: 60,
		Description: "MongoDB 4.4+, Atlas, Amazon DocumentDB-compatible servers",
		Dialect:     "mongodb", DefaultPort: 27017, SSH: true, QuoteChar: "",
		URLSchemes: []string{"mongodb", "mongodb+srv"},
		Fields: []driver.Field{
			{Key: "mode", Label: "Connect with", Type: driver.FieldSelect, Default: "fields",
				Options: []driver.Option{{Value: "fields", Label: "Host and credentials"}, {Value: "uri", Label: "Connection string"}}},
			{Key: "uri", Label: "Connection string", Type: driver.FieldPassword, Secret: true, Placeholder: "mongodb+srv://user:pass@cluster.example.net/app",
				ShowIf: map[string][]string{"mode": {"uri"}}, Help: "Stored encrypted, like a password."},
			{Key: "host", Label: "Host", Type: driver.FieldText, Placeholder: "mongo.example.com", Span: 4, ShowIf: connect},
			{Key: "port", Label: "Port", Type: driver.FieldNumber, Default: 27017, Span: 2, ShowIf: connect},
			{Key: "user", Label: "User", Type: driver.FieldText, Section: "auth", Span: 3, ShowIf: connect},
			{Key: "password", Label: "Password", Type: driver.FieldPassword, Secret: true, Section: "auth", Span: 3, ShowIf: connect},
			{Key: "authSource", Label: "Authentication database", Type: driver.FieldText, Placeholder: "admin", Section: "auth", Span: 3, ShowIf: connect},
			{Key: "database", Label: "Default database", Type: driver.FieldText, Placeholder: "optional", Span: 3},
			{Key: "replicaSet", Label: "Replica set", Type: driver.FieldText, Section: "advanced", Span: 3, ShowIf: connect},
			{Key: "tls", Label: "TLS / SSL", Type: driver.FieldSelect, Options: driver.TLSModes, Default: "disable", Section: "tls", ShowIf: connect},
			{Key: "tlsCA", Label: "CA certificate (PEM)", Type: driver.FieldFile, Section: "tls", ShowIf: map[string][]string{"tls": {"require", "verify-ca", "verify-full"}}},
			{Key: "connectTimeout", Label: "Connect timeout (seconds)", Type: driver.FieldNumber, Default: 15, Section: "advanced", Span: 3},
		},
		Caps: driver.Caps{Databases: true, Documents: true, EditRows: true, DDL: true, Explain: true, Processes: true, Variables: true,
			Users: true, Geometry: true, CreateDatabase: false},
		Kinds:  kinds,
		Design: &design,
		Types:  []string{"string", "int", "long", "double", "decimal", "bool", "date", "objectId", "object", "array", "binData"},
	}
}

type conn struct {
	client    *mongo.Client
	defaultDB string
	ro        bool
	version   string
}

type dialer struct{ fn driver.DialFunc }

func (d dialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	return d.fn(ctx, network, addr)
}

func (mongoDriver) Open(ctx context.Context, p driver.OpenParams) (driver.Conn, error) {
	opts := options.Client().SetAppName(p.AppName).
		SetConnectTimeout(time.Duration(p.Int("connectTimeout", 15)) * time.Second).
		SetServerSelectionTimeout(time.Duration(p.Int("connectTimeout", 15)+5) * time.Second)
	defaultDB := p.String("database")
	if p.String("mode") == "uri" {
		uri := strings.TrimSpace(p.Secret("uri"))
		if uri == "" {
			return nil, errors.New("enter the connection string")
		}
		opts.ApplyURI(uri)
		if defaultDB == "" {
			if m := regexp.MustCompile(`^mongodb(?:\+srv)?://[^/]+/([^?]+)`).FindStringSubmatch(uri); m != nil {
				defaultDB = m[1]
			}
		}
	} else {
		host, port := driver.HostPort(p, 27017)
		if host == "" {
			return nil, errors.New("host is required")
		}
		opts.SetHosts([]string{net.JoinHostPort(host, strconv.Itoa(port))})
		if u := p.String("user"); u != "" {
			src := p.String("authSource")
			if src == "" {
				src = "admin"
			}
			opts.SetAuth(options.Credential{Username: u, Password: p.Secret("password"), AuthSource: src})
		}
		if rs := p.String("replicaSet"); rs != "" {
			opts.SetReplicaSet(rs)
		}
		if tc, err := driver.TLSConfig(p, host); err != nil {
			return nil, err
		} else if tc != nil {
			opts.SetTLSConfig(tc)
		}
	}
	if p.Dial != nil {
		// Through a tunnel only the configured host is reachable, so do not
		// follow replica set members to hostnames we cannot resolve.
		opts.SetDialer(dialer{p.Dial}).SetDirect(true)
	}
	if p.ReadOnly {
		opts.SetReadPreference(readpref.PrimaryPreferred())
	}
	client, err := mongo.Connect(opts)
	if err != nil {
		return nil, err
	}
	pctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var info bson.M
	if err := client.Database("admin").RunCommand(pctx, bson.D{{Key: "buildInfo", Value: 1}}).Decode(&info); err != nil {
		client.Disconnect(context.Background())
		return nil, mapError(err)
	}
	c := &conn{client: client, defaultDB: defaultDB, ro: p.ReadOnly}
	c.version, _ = info["version"].(string)
	return c, nil
}

func (c *conn) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return c.client.Disconnect(ctx)
}

func (c *conn) Ping(ctx context.Context) error {
	return c.client.Ping(ctx, readpref.PrimaryPreferred())
}

func (c *conn) db(name string) *mongo.Database {
	if name == "" {
		name = c.defaultDB
	}
	if name == "" {
		name = "test"
	}
	return c.client.Database(name)
}

func (c *conn) Server(ctx context.Context) (*driver.ServerInfo, error) {
	info := &driver.ServerInfo{Product: "MongoDB", Version: c.version, Database: c.defaultDB, Extras: map[string]string{}}
	var st bson.M
	if err := c.client.Database("admin").RunCommand(ctx, bson.D{{Key: "connectionStatus", Value: 1}}).Decode(&st); err == nil {
		if ai, ok := st["authInfo"].(bson.D); ok {
			for _, e := range ai {
				if e.Key == "authenticatedUsers" {
					if users, ok := e.Value.(bson.A); ok && len(users) > 0 {
						if u, ok := users[0].(bson.D); ok {
							for _, f := range u {
								if f.Key == "user" {
									info.User = fmt.Sprint(f.Value)
								}
							}
						}
					}
				}
			}
		}
	}
	var hello bson.M
	if err := c.client.Database("admin").RunCommand(ctx, bson.D{{Key: "hello", Value: 1}}).Decode(&hello); err == nil {
		if rs, ok := hello["setName"].(string); ok {
			info.Extras["Replica set"] = rs
		}
		if msg, ok := hello["msg"].(string); ok && msg == "isdbgrid" {
			info.Extras["Topology"] = "sharded (mongos)"
		}
	}
	if c.ro {
		info.Extras["Session"] = "read-only"
	}
	return info, nil
}

var systemDBs = map[string]bool{"admin": true, "local": true, "config": true}

func (c *conn) Databases(ctx context.Context) ([]driver.Database, error) {
	res, err := c.client.ListDatabases(ctx, bson.D{})
	if err != nil {
		// Users without listDatabases privilege can still use their default database.
		if c.defaultDB != "" {
			return []driver.Database{{Name: c.defaultDB}}, nil
		}
		return nil, mapError(err)
	}
	out := make([]driver.Database, 0, len(res.Databases))
	for _, d := range res.Databases {
		size := d.SizeOnDisk
		out = append(out, driver.Database{Name: d.Name, Size: &size, System: systemDBs[d.Name]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (c *conn) Schemas(context.Context, string) ([]driver.Schema, error) { return nil, nil }

func (c *conn) Objects(ctx context.Context, s driver.Scope) ([]driver.Object, error) {
	db := c.db(s.Database)
	cur, err := db.ListCollections(ctx, bson.D{}, options.ListCollections().SetAuthorizedCollections(true))
	if err != nil {
		return nil, mapError(err)
	}
	var specs []bson.M
	if err := cur.All(ctx, &specs); err != nil {
		return nil, mapError(err)
	}
	out := make([]driver.Object, 0, len(specs))
	budget, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for _, sp := range specs {
		name, _ := sp["name"].(string)
		typ, _ := sp["type"].(string)
		if strings.HasPrefix(name, "system.") {
			continue
		}
		kind := "collection"
		switch typ {
		case "view":
			kind = "view"
		case "timeseries":
			kind = "timeseries"
		}
		o := driver.Object{Name: name, Kind: kind}
		if kind == "collection" && budget.Err() == nil {
			if n, err := db.Collection(name).EstimatedDocumentCount(budget); err == nil {
				o.Rows = &n
			}
		}
		if opts, ok := sp["options"].(bson.D); ok {
			for _, e := range opts {
				if e.Key == "viewOn" {
					o.Extra = "on " + fmt.Sprint(e.Value)
				}
				if e.Key == "capped" && e.Value == true {
					o.Extra = "capped"
				}
			}
		}
		out = append(out, o)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// fieldStat summarizes one top-level field across sampled documents.
type fieldStat struct {
	name  string
	seen  int
	types map[string]int
	first int
}

// inferFields samples documents and returns top-level fields ordered by
// first appearance (with _id first), their dominant type and frequency.
func inferFields(docs []bson.D) []fieldStat {
	stats := map[string]*fieldStat{}
	order := 0
	for _, d := range docs {
		for _, e := range d {
			st, ok := stats[e.Key]
			if !ok {
				st = &fieldStat{name: e.Key, types: map[string]int{}, first: order}
				order++
				stats[e.Key] = st
			}
			st.seen++
			st.types[typeName(e.Value)]++
		}
	}
	out := make([]fieldStat, 0, len(stats))
	for _, s := range stats {
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].name == "_id" || out[j].name == "_id" {
			return out[i].name == "_id"
		}
		return out[i].first < out[j].first
	})
	return out
}

func (f fieldStat) dominant() string {
	best, n := "null", -1
	var nonNull []string
	for t, c := range f.types {
		if t != "null" {
			nonNull = append(nonNull, t)
		}
		if t != "null" && c > n || (t == "null" && n < 0) {
			best, n = t, c
		}
	}
	if len(nonNull) > 1 {
		sort.Strings(nonNull)
		return "mixed(" + strings.Join(nonNull, ",") + ")"
	}
	return best
}

func (c *conn) sample(ctx context.Context, db, coll string, n int) ([]bson.D, error) {
	cur, err := c.db(db).Collection(coll).Aggregate(ctx, mongo.Pipeline{{{Key: "$sample", Value: bson.D{{Key: "size", Value: n}}}}})
	if err != nil {
		// $sample is not allowed on some views; fall back to the first documents.
		cur, err = c.db(db).Collection(coll).Find(ctx, bson.D{}, options.Find().SetLimit(int64(n)))
		if err != nil {
			return nil, mapError(err)
		}
	}
	var docs []bson.D
	if err := cur.All(ctx, &docs); err != nil {
		return nil, mapError(err)
	}
	return docs, nil
}

func (c *conn) Describe(ctx context.Context, ref driver.ObjectRef) (*driver.Table, error) {
	if ref.Name == "" {
		return nil, errors.New("choose a collection")
	}
	db := c.db(ref.Database)
	t := &driver.Table{Ref: ref, Kind: "collection", Options: map[string]string{}}
	cur, err := db.ListCollections(ctx, bson.D{{Key: "name", Value: ref.Name}})
	if err != nil {
		return nil, mapError(err)
	}
	var specs []bson.M
	_ = cur.All(ctx, &specs)
	if len(specs) == 0 {
		return nil, fmt.Errorf("collection %s not found", ref.Name)
	}
	spec := specs[0]
	switch spec["type"] {
	case "view":
		t.Kind = "view"
	case "timeseries":
		t.Kind = "timeseries"
	}
	t.Ref.Kind = t.Kind
	var optsDoc bson.D
	if o, ok := spec["options"].(bson.D); ok {
		optsDoc = o
		for _, e := range o {
			switch e.Key {
			case "validator":
				t.Options["validator"] = shellLiteralIndent(e.Value)
			case "size", "max", "expireAfterSeconds":
				t.Options[e.Key] = numText(e.Value)
			case "capped", "viewOn", "validationLevel", "validationAction":
				t.Options[e.Key] = fmt.Sprint(e.Value)
			case "timeseries":
				b, _ := bson.MarshalExtJSON(e.Value, false, false)
				t.Options["timeseries"] = string(b)
			case "pipeline":
				b, _ := bson.MarshalExtJSONIndent(bson.D{{Key: "pipeline", Value: e.Value}}, false, false, "", "  ")
				t.Definition = string(b)
			}
		}
	}

	docs, err := c.sample(ctx, ref.Database, ref.Name, 200)
	if err != nil {
		return nil, err
	}
	total := len(docs)
	for _, f := range inferFields(docs) {
		typ := f.dominant()
		col := driver.Column{Name: f.name, Type: typ, BaseType: typ, Kind: kindOf(typ), Nullable: f.seen < total || f.types["null"] > 0}
		if f.name == "_id" {
			col.PrimaryKey = true
			col.Nullable = false
		}
		if total > 0 && f.seen < total {
			col.Comment = fmt.Sprintf("in %d%% of sampled documents", f.seen*100/total)
		}
		t.Columns = append(t.Columns, col)
	}
	if len(t.Columns) == 0 {
		t.Columns = []driver.Column{{Name: "_id", Type: "objectId", BaseType: "objectId", Kind: driver.KindOther, PrimaryKey: true}}
	}
	if n, err := db.Collection(ref.Name).EstimatedDocumentCount(ctx); err == nil {
		t.RowEstimate = &n
	}

	if t.Kind != "view" {
		icur, err := db.Collection(ref.Name).Indexes().List(ctx)
		if err == nil {
			var ixs []bson.D
			_ = icur.All(ctx, &ixs)
			for _, ix := range ixs {
				t.Indexes = append(t.Indexes, describeIndex(ref.Name, ix))
			}
		}
		t.PrimaryKey = []string{"_id"}
		t.RowKey, t.RowKeyKind, t.Editable = []string{"_id"}, "_id", !c.ro
	}
	t.DDL = recreateScript(ref.Name, t.Kind, optsDoc, t.Indexes)
	return t, nil
}

// recreateScript renders shell commands that recreate the collection.
func recreateScript(name, kind string, opts bson.D, idx []driver.Index) string {
	q := quoteStr(name)
	if kind == "view" {
		var on string
		var pipeline any = bson.A{}
		for _, e := range opts {
			if e.Key == "viewOn" {
				on = fmt.Sprint(e.Value)
			}
			if e.Key == "pipeline" {
				pipeline = e.Value
			}
		}
		return "db.createView(" + q + ", " + quoteStr(on) + ", " + shellLiteral(pipeline) + ")"
	}
	var b strings.Builder
	b.WriteString("db.createCollection(" + q)
	if len(opts) > 0 {
		b.WriteString(", " + shellLiteral(opts))
	}
	b.WriteString(")")
	for _, ix := range idx {
		if !ix.Primary && ix.Definition != "" {
			b.WriteString("\n" + ix.Definition)
		}
	}
	return b.String()
}

// ---- browse ---------------------------------------------------------------------

func (c *conn) buildFilter(ctx context.Context, req driver.BrowseRequest, t *driver.Table) (bson.D, error) {
	hints := map[string]string{}
	for _, col := range t.Columns {
		hints[col.Name] = col.Type
	}
	var and bson.A
	for _, f := range req.Filters {
		cond, err := filterDoc(f, hints[f.Column])
		if err != nil {
			return nil, err
		}
		and = append(and, cond)
	}
	if s := strings.TrimSpace(req.Search); s != "" {
		var or bson.A
		re := bson.Regex{Pattern: regexp.QuoteMeta(s), Options: "i"}
		for _, col := range t.Columns {
			if col.Type == "string" {
				or = append(or, bson.D{{Key: col.Name, Value: re}})
			}
		}
		if len(or) > 0 {
			and = append(and, bson.D{{Key: "$or", Value: or}})
		}
	}
	if w := strings.TrimSpace(req.Where); w != "" {
		v, err := parseLiteral(w)
		if err != nil {
			return nil, fmt.Errorf("filter: %w", err)
		}
		d, ok := v.(bson.D)
		if !ok {
			return nil, errors.New("the filter must be a document, e.g. { status: \"paid\" }")
		}
		and = append(and, d)
	}
	_ = ctx
	switch len(and) {
	case 0:
		return bson.D{}, nil
	case 1:
		return and[0].(bson.D), nil
	}
	return bson.D{{Key: "$and", Value: and}}, nil
}

func filterDoc(f driver.Filter, hint string) (bson.D, error) {
	val := func(v any) (any, error) { return decodeCell(v, hint) }
	one := func(op string) (bson.D, error) {
		v, err := val(f.Value)
		if err != nil {
			return nil, err
		}
		if op == "" {
			return bson.D{{Key: f.Column, Value: v}}, nil
		}
		return bson.D{{Key: f.Column, Value: bson.D{{Key: op, Value: v}}}}, nil
	}
	str := fmt.Sprint(f.Value)
	switch f.Op {
	case "=":
		return one("")
	case "!=":
		return one("$ne")
	case "<":
		return one("$lt")
	case "<=":
		return one("$lte")
	case ">":
		return one("$gt")
	case ">=":
		return one("$gte")
	case "contains":
		return bson.D{{Key: f.Column, Value: bson.Regex{Pattern: regexp.QuoteMeta(str), Options: "i"}}}, nil
	case "startswith":
		return bson.D{{Key: f.Column, Value: bson.Regex{Pattern: "^" + regexp.QuoteMeta(str), Options: "i"}}}, nil
	case "endswith":
		return bson.D{{Key: f.Column, Value: bson.Regex{Pattern: regexp.QuoteMeta(str) + "$", Options: "i"}}}, nil
	case "like", "notlike":
		pat := "^" + strings.NewReplacer("%", ".*", "_", ".").Replace(regexp.QuoteMeta(str)) + "$"
		re := bson.Regex{Pattern: pat, Options: "i"}
		if f.Op == "notlike" {
			return bson.D{{Key: f.Column, Value: bson.D{{Key: "$not", Value: re}}}}, nil
		}
		return bson.D{{Key: f.Column, Value: re}}, nil
	case "regexp":
		return bson.D{{Key: f.Column, Value: bson.Regex{Pattern: str, Options: "i"}}}, nil
	case "in", "notin":
		arr := bson.A{}
		for _, x := range f.Values {
			v, err := val(x)
			if err != nil {
				return nil, err
			}
			arr = append(arr, v)
		}
		op := "$in"
		if f.Op == "notin" {
			op = "$nin"
		}
		return bson.D{{Key: f.Column, Value: bson.D{{Key: op, Value: arr}}}}, nil
	case "null":
		return bson.D{{Key: f.Column, Value: nil}}, nil
	case "notnull":
		return bson.D{{Key: f.Column, Value: bson.D{{Key: "$ne", Value: nil}}}}, nil
	case "empty":
		return bson.D{{Key: f.Column, Value: bson.D{{Key: "$in", Value: bson.A{nil, ""}}}}}, nil
	case "between":
		if len(f.Values) != 2 {
			return nil, errors.New("between needs two values")
		}
		lo, err := val(f.Values[0])
		if err != nil {
			return nil, err
		}
		hi, err := val(f.Values[1])
		if err != nil {
			return nil, err
		}
		return bson.D{{Key: f.Column, Value: bson.D{{Key: "$gte", Value: lo}, {Key: "$lte", Value: hi}}}}, nil
	}
	return nil, fmt.Errorf("unknown filter operator %q", f.Op)
}

func (c *conn) Browse(ctx context.Context, req driver.BrowseRequest) (*driver.Result, error) {
	t, err := c.Describe(ctx, req.Ref)
	if err != nil {
		return nil, err
	}
	filter, err := c.buildFilter(ctx, req, t)
	if err != nil {
		return nil, err
	}
	limit := req.Limit
	if limit <= 0 || limit > 5000 {
		limit = 200
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	fo := options.Find().SetLimit(int64(limit + 1)).SetSkip(req.Offset)
	if len(req.Sort) > 0 {
		s := bson.D{}
		for _, x := range req.Sort {
			dir := 1
			if x.Desc {
				dir = -1
			}
			s = append(s, bson.E{Key: x.Column, Value: dir})
		}
		fo.SetSort(s)
	}
	cols := make([]string, 0, len(t.Columns))
	if len(req.Columns) > 0 {
		proj := bson.D{}
		for _, n := range req.Columns {
			proj = append(proj, bson.E{Key: n, Value: 1})
			cols = append(cols, n)
		}
		fo.SetProjection(proj)
	} else {
		for _, col := range t.Columns {
			cols = append(cols, col.Name)
		}
	}
	start := time.Now()
	cur, err := c.db(req.Ref.Database).Collection(req.Ref.Name).Find(ctx, filter, fo)
	if err != nil {
		return nil, mapError(err)
	}
	var docs []bson.D
	if err := cur.All(ctx, &docs); err != nil {
		return nil, mapError(err)
	}
	res := &driver.Result{Rows: [][]any{}}
	if len(docs) > limit {
		res.Truncated = true
		docs = docs[:limit]
	}
	res.Columns, res.Rows = docsToRows(docs, cols, t)
	res.Duration = time.Since(start)
	res.DurationMS = float64(res.Duration.Microseconds()) / 1000
	fj, _ := bson.MarshalExtJSON(filter, false, false)
	res.SQL = fmt.Sprintf("db.getCollection(%q).find(%s)", req.Ref.Name, fj)
	return res, nil
}

// docsToRows lays documents out as rows over the given columns; fields not
// in cols are collected into a trailing "…" object column.
func docsToRows(docs []bson.D, cols []string, t *driver.Table) ([]driver.ResultColumn, [][]any) {
	byName := map[string]driver.Column{}
	if t != nil {
		for _, c := range t.Columns {
			byName[c.Name] = c
		}
	}
	idx := map[string]int{}
	for i, n := range cols {
		idx[n] = i
	}
	extra := false
	rows := make([][]any, 0, len(docs))
	for _, d := range docs {
		row := make([]any, len(cols)+1)
		var rest bson.D
		for _, e := range d {
			if i, ok := idx[e.Key]; ok {
				row[i] = encode(e.Value)
			} else {
				rest = append(rest, e)
			}
		}
		if len(rest) > 0 {
			extra = true
			row[len(cols)] = encode(rest)
		}
		rows = append(rows, row)
	}
	rc := make([]driver.ResultColumn, 0, len(cols)+1)
	for _, n := range cols {
		c, ok := byName[n]
		typ, kind := "mixed", driver.KindOther
		if ok {
			typ, kind = c.Type, c.Kind
		}
		rc = append(rc, driver.ResultColumn{Name: n, Type: typ, Kind: kind})
	}
	if extra {
		rc = append(rc, driver.ResultColumn{Name: "…", Type: "other fields", Kind: driver.KindObject})
	} else {
		for i := range rows {
			rows[i] = rows[i][:len(cols)]
		}
	}
	return rc, rows
}

func (c *conn) Count(ctx context.Context, req driver.BrowseRequest) (driver.Count, error) {
	coll := c.db(req.Ref.Database).Collection(req.Ref.Name)
	if len(req.Filters) == 0 && req.Search == "" && strings.TrimSpace(req.Where) == "" {
		n, err := coll.EstimatedDocumentCount(ctx)
		if err == nil {
			return driver.Count{Rows: n, Exact: false}, nil
		}
	}
	t, err := c.Describe(ctx, req.Ref)
	if err != nil {
		return driver.Count{}, err
	}
	filter, err := c.buildFilter(ctx, req, t)
	if err != nil {
		return driver.Count{}, err
	}
	cctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	n, err := coll.CountDocuments(cctx, filter)
	if err != nil {
		return driver.Count{}, mapError(err)
	}
	return driver.Count{Rows: n, Exact: true}, nil
}

// ApplyEdits writes grid changes; each update or delete targets one _id.
func (c *conn) ApplyEdits(ctx context.Context, ref driver.ObjectRef, edits []driver.RowEdit) (*driver.EditResult, error) {
	if c.ro {
		return nil, driver.ErrReadOnly
	}
	t, err := c.Describe(ctx, ref)
	if err != nil {
		return nil, err
	}
	hints := map[string]string{}
	for _, col := range t.Columns {
		hints[col.Name] = col.Type
	}
	coll := c.db(ref.Database).Collection(ref.Name)
	res := &driver.EditResult{}
	for i, ed := range edits {
		switch ed.Op {
		case "insert":
			doc := bson.D{}
			for _, col := range t.Columns {
				v, ok := ed.Values[col.Name]
				if !ok {
					continue
				}
				bv, err := decodeCell(v, hints[col.Name])
				if err != nil {
					return res, fmt.Errorf("change %d, field %s: %w", i+1, col.Name, err)
				}
				doc = append(doc, bson.E{Key: col.Name, Value: bv})
			}
			r, err := coll.InsertOne(ctx, doc)
			if err != nil {
				return res, fmt.Errorf("change %d: %w", i+1, mapError(err))
			}
			res.Inserted = append(res.Inserted, []any{encode(r.InsertedID)})
			res.Statements = append(res.Statements, fmt.Sprintf("db.getCollection(%q).insertOne(…)", ref.Name))
		case "update", "delete":
			id, ok := ed.Key["_id"]
			if !ok {
				return res, fmt.Errorf("change %d: missing _id", i+1)
			}
			bid, err := decodeCell(id, hints["_id"])
			if err != nil {
				return res, err
			}
			filter := bson.D{{Key: "_id", Value: bid}}
			if ed.Op == "delete" {
				r, err := coll.DeleteOne(ctx, filter)
				if err != nil {
					return res, fmt.Errorf("change %d: %w", i+1, mapError(err))
				}
				if r.DeletedCount != 1 {
					return res, fmt.Errorf("change %d: the document no longer exists", i+1)
				}
				res.Statements = append(res.Statements, fmt.Sprintf("db.getCollection(%q).deleteOne({_id: …})", ref.Name))
				res.Applied++
				continue
			}
			set, unset := bson.D{}, bson.D{}
			for k, v := range ed.Values {
				if m, ok := v.(map[string]any); ok && m["$unset"] == true {
					unset = append(unset, bson.E{Key: k, Value: ""})
					continue
				}
				bv, err := decodeCell(v, hints[k])
				if err != nil {
					return res, fmt.Errorf("change %d, field %s: %w", i+1, k, err)
				}
				set = append(set, bson.E{Key: k, Value: bv})
			}
			update := bson.D{}
			if len(set) > 0 {
				update = append(update, bson.E{Key: "$set", Value: set})
			}
			if len(unset) > 0 {
				update = append(update, bson.E{Key: "$unset", Value: unset})
			}
			if len(update) == 0 {
				continue
			}
			r, err := coll.UpdateOne(ctx, filter, update)
			if err != nil {
				return res, fmt.Errorf("change %d: %w", i+1, mapError(err))
			}
			if r.MatchedCount != 1 {
				return res, fmt.Errorf("change %d: the document no longer exists", i+1)
			}
			res.Statements = append(res.Statements, fmt.Sprintf("db.getCollection(%q).updateOne({_id: …}, {$set: …})", ref.Name))
		default:
			return res, fmt.Errorf("unknown edit operation %q", ed.Op)
		}
		res.Applied++
	}
	return res, nil
}

var lineRe = regexp.MustCompile(`line (\d+)`)

func mapError(err error) error {
	if err == nil {
		return nil
	}
	var ce mongo.CommandError
	if errors.As(err, &ce) {
		return &driver.QueryError{Message: ce.Message, Code: ce.Name}
	}
	var we mongo.WriteException
	if errors.As(err, &we) && len(we.WriteErrors) > 0 {
		return &driver.QueryError{Message: we.WriteErrors[0].Message, Code: strconv.Itoa(we.WriteErrors[0].Code)}
	}
	return err
}
