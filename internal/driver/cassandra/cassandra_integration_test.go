package cassandra

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"rowsmith/internal/driver"
)

// Integration tests run against the dev stack's servers (dev/compose.yaml,
// seeded from dev/seed/cassandra.cql) when their hosts are set. They create
// and drop their own it_ tables in the shop keyspace.
//
//	ROWSMITH_TEST_CASSANDRA_HOST, _PASSWORD   user rowsmith
//	ROWSMITH_TEST_SCYLLA_HOST, _PASSWORD      user rowsmith

type target struct{ id, prefix string }

var targets = []target{{"cassandra", "ROWSMITH_TEST_CASSANDRA_"}, {"scylladb", "ROWSMITH_TEST_SCYLLA_"}}

func open(t *testing.T, tg target) *conn {
	t.Helper()
	host := os.Getenv(tg.prefix + "HOST")
	if host == "" {
		t.Skip(tg.prefix + "HOST not set")
	}
	d, _ := driver.Get(tg.id)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cn, err := d.Open(ctx, driver.OpenParams{AppName: "rowsmith test",
		Params:  map[string]any{"host": host, "user": "rowsmith", "database": "shop", "tls": "disable"},
		Secrets: map[string]string{"password": os.Getenv(tg.prefix + "PASSWORD")}})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { cn.Close() })
	return cn.(*conn)
}

type sink struct {
	stmts   []driver.StatementInfo
	errs    []error
	cols    [][]driver.ResultColumn
	rows    [][][]any
	notices []string
}

func (s *sink) BeginStatement(i driver.StatementInfo) error { s.stmts = append(s.stmts, i); return nil }
func (s *sink) Columns(c []driver.ResultColumn) error {
	s.cols = append(s.cols, c)
	s.rows = append(s.rows, nil)
	return nil
}
func (s *sink) Rows(r [][]any) error {
	s.rows[len(s.rows)-1] = append(s.rows[len(s.rows)-1], r...)
	return nil
}
func (s *sink) EndResult(driver.ResultSummary) error { return nil }
func (s *sink) Notice(_, text string) error          { s.notices = append(s.notices, text); return nil }
func (s *sink) EndStatement(err error) error         { s.errs = append(s.errs, err); return nil }

func run(t *testing.T, sess driver.Session, script string, params ...any) *sink {
	t.Helper()
	out := &sink{}
	if err := sess.Execute(context.Background(), script, driver.ExecOptions{StopOnError: true, MaxRows: 1000, Params: params}, out); err != nil {
		t.Fatalf("execute: %v", err)
	}
	for i, err := range out.errs {
		if err != nil {
			t.Fatalf("%s: %v", out.stmts[i].SQL, err)
		}
	}
	return out
}

func TestIntegration(t *testing.T) {
	for _, tg := range targets {
		t.Run(tg.id, func(t *testing.T) { testEngine(t, open(t, tg)) })
	}
}

func testEngine(t *testing.T, c *conn) {
	ctx := context.Background()
	srv, err := c.Server(ctx)
	if err != nil || srv.Version == "" || srv.Extras["Nodes"] != "1" {
		t.Errorf("server = %+v, %v", srv, err)
	}
	t.Logf("%s %s, dc %s", srv.Product, srv.Version, c.dc)
	dbs, err := c.Databases(ctx)
	if err != nil || !slices.ContainsFunc(dbs, func(d driver.Database) bool { return d.Name == "shop" && !d.System && *d.Tables >= 4 }) {
		t.Fatalf("keyspaces = %+v, %v", dbs, err)
	}
	objs, err := c.Objects(ctx, driver.Scope{Database: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]string{}
	for _, o := range objs {
		kinds[o.Name] = o.Kind
	}
	if kinds["customers"] != "table" || kinds["orders_by_status"] != "materialized_view" || kinds["address"] != "type" || len(objs) != 6 {
		t.Errorf("objects = %v", kinds)
	}

	ks := func(name string) driver.ObjectRef { return driver.ObjectRef{Database: "shop", Name: name} }
	orders, err := c.Describe(ctx, ks("orders_by_customer"))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(orders.PrimaryKey, []string{"customer_id", "placed_at", "order_id"}) || orders.Options["partition_key"] != "customer_id" ||
		orders.Options["clustering_order"] != "placed_at DESC, order_id ASC" || orders.Options["static_columns"] != "channel" || !orders.Editable ||
		!strings.Contains(orders.DDL, "PRIMARY KEY ((customer_id), placed_at, order_id)") {
		t.Errorf("orders = %+v", orders)
	}
	sensors, err := c.Describe(ctx, ks("sensor_readings"))
	if err != nil || sensors.Options["partition_key"] != "sensor_id, day" || sensors.Options["compaction"] != "TimeWindowCompactionStrategy" {
		t.Errorf("sensors = %+v, %v", sensors.Options, err)
	}
	cust, err := c.Describe(ctx, ks("customers"))
	if err != nil || len(cust.Indexes) != 3 {
		t.Errorf("customers indexes = %+v, %v", cust.Indexes, err)
	}
	if pv, err := c.Describe(ctx, ks("page_views")); err != nil || pv.Editable {
		t.Errorf("counter table editable: %v", err)
	}
	if mv, err := c.Describe(ctx, ks("orders_by_status")); err != nil || mv.Kind != "materialized_view" || mv.Editable || !strings.Contains(mv.DDL, "CREATE MATERIALIZED VIEW") {
		t.Errorf("view = %+v, %v", mv, err)
	}
	if def, err := c.Definition(ctx, driver.ObjectRef{Database: "shop", Name: "address", Kind: "type"}); err != nil || !strings.Contains(def, "street text") {
		t.Errorf("type = %q, %v", def, err)
	}

	// Browsing: typed filters, a partition with its clustering order, nested values.
	res, err := c.Browse(ctx, driver.BrowseRequest{Ref: ks("sensor_readings"), Limit: 5, Offset: 2,
		Filters: []driver.Filter{{Column: "sensor_id", Op: "=", Value: "boiler-1"}, {Column: "day", Op: "=", Value: "2025-06-02"},
			{Column: "value", Op: ">", Value: "10"}},
		Sort: []driver.Sort{{Column: "ts", Desc: true}}})
	if err != nil || len(res.Rows) != 5 || !res.Truncated {
		t.Fatalf("sensor readings: %+v, %v", res, err)
	}
	if !strings.Contains(res.SQL, "ORDER BY ts DESC") || !strings.Contains(res.SQL, "fromJson('\"boiler-1\"')") {
		t.Errorf("sql = %s", res.SQL)
	}
	if _, err := c.Browse(ctx, driver.BrowseRequest{Ref: ks("customers"), Limit: 5, Sort: []driver.Sort{{Column: "name"}}}); err == nil {
		t.Error("sorted a whole table")
	}
	res, err = c.Browse(ctx, driver.BrowseRequest{Ref: ks("customers"), Limit: 3,
		Filters: []driver.Filter{{Column: "tier", Op: "=", Value: "pro"}, {Column: "tags", Op: "contains", Value: "vip"}}})
	if err != nil || len(res.Rows) == 0 {
		t.Fatalf("customers: %+v, %v", res, err)
	}
	row := map[string]any{}
	for i, col := range res.Columns {
		row[col.Name] = res.Rows[0][i]
	}
	addr, _ := json.Marshal(row["address"])
	if !strings.HasPrefix(string(addr), `{"street":`) || row["tier"] != "pro" || !slices.Contains(row["tags"].([]any), any("vip")) {
		t.Errorf("customer = %v (address %s)", row, addr)
	}
	if n, err := c.Count(ctx, driver.BrowseRequest{Ref: ks("orders_by_customer")}); err != nil || n.Rows != 400 || !n.Exact {
		t.Errorf("count = %+v, %v", n, err)
	}
	if n, err := c.Count(ctx, driver.BrowseRequest{Ref: ks("customers"), Filters: []driver.Filter{{Column: "tier", Op: "in", Values: []any{"pro", "team"}}}}); err != nil || n.Rows != 30 {
		t.Errorf("filtered count = %+v, %v", n, err)
	}

	// A scratch table through the structure editor, then edits as lightweight transactions.
	sess, err := c.NewSession(ctx, driver.Scope{Database: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	run(t, sess, "DROP TABLE IF EXISTS it_events")
	def := driver.TableDef{Ref: ks("it_events"), Comment: "events for tests", PrimaryKey: []string{"device", "at"},
		Options: map[string]string{"clustering_order": "at DESC", "default_time_to_live": "86400"},
		Columns: []driver.ColumnDef{
			{Column: driver.Column{Name: "device", Type: "text"}},
			{Column: driver.Column{Name: "at", Type: "timestamp"}},
			{Column: driver.Column{Name: "level", Type: "int"}},
			{Column: driver.Column{Name: "tags", Type: "set<text>"}},
			{Column: driver.Column{Name: "attrs", Type: "map<text, int>"}},
			{Column: driver.Column{Name: "amount", Type: "decimal"}},
			{Column: driver.Column{Name: "pair", Type: "tuple<int, text>"}},
		},
		Indexes: []driver.Index{{Name: "it_events_level", Columns: []string{"level"}, Type: "secondary"}}}
	stmts, err := c.CreateTableSQL(def)
	if err != nil {
		t.Fatal(err)
	}
	run(t, sess, strings.Join(stmts, ";\n"))
	ref := ks("it_events")
	if _, err := c.ApplyEdits(ctx, ref, []driver.RowEdit{
		{Op: "insert", Values: map[string]any{"device": "d1", "at": "2025-03-04 05:06:07+00:00", "level": float64(3), "tags": []any{"a", "b"},
			"attrs": map[string]any{"x": float64(1)}, "amount": "12.50", "pair": []any{float64(7), "seven"}}},
		{Op: "insert", Values: map[string]any{"device": "d1", "at": "2025-03-04 06:00:00+00:00", "level": "4"}},
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := c.ApplyEdits(ctx, ref, []driver.RowEdit{{Op: "insert", Values: map[string]any{"device": "d1", "at": "2025-03-04 06:00:00+00:00"}}}); err == nil ||
		!strings.Contains(err.Error(), "already exists") {
		t.Errorf("duplicate insert: %v", err)
	}
	res, err = c.Browse(ctx, driver.BrowseRequest{Ref: ref, Limit: 10, Filters: []driver.Filter{{Column: "device", Op: "=", Value: "d1"}}})
	if err != nil || len(res.Rows) != 2 {
		t.Fatalf("it_events: %+v, %v", res, err)
	}
	first := map[string]any{}
	for i, col := range res.Columns {
		first[col.Name] = res.Rows[1][i] // clustering order is newest first
	}
	if first["amount"] != "12.50" || first["at"] != "2025-03-04 05:06:07+00:00" {
		t.Errorf("row = %v", first)
	}
	if p, _ := json.Marshal(first["pair"]); string(p) != `[7,"seven"]` {
		t.Errorf("tuple = %s", p)
	}
	key := map[string]any{"device": first["device"], "at": first["at"]}
	if _, err := c.ApplyEdits(ctx, ref, []driver.RowEdit{
		{Op: "update", Key: key, Values: map[string]any{"level": float64(9), "tags": `["c"]`, "amount": map[string]any{"$null": true}}},
		{Op: "delete", Key: map[string]any{"device": "d1", "at": "2025-03-04 06:00:00+00:00"}},
	}); err != nil {
		t.Fatalf("edit: %v", err)
	}
	s := run(t, sess, "SELECT level, tags, amount FROM it_events WHERE device = ?", "d1")
	if len(s.rows[0]) != 1 || s.rows[0][0][0] != int64(9) || s.rows[0][0][2] != nil {
		t.Errorf("after edit = %v", s.rows)
	}
	if _, err := c.ApplyEdits(ctx, ref, []driver.RowEdit{{Op: "delete", Key: map[string]any{"device": "d1", "at": "2025-03-04 06:00:00+00:00"}}}); err == nil {
		t.Error("deleted a missing row")
	}
	if _, err := c.ApplyEdits(ctx, ref, []driver.RowEdit{{Op: "update", Key: key, Values: map[string]any{"device": "d2"}}}); err == nil {
		t.Error("changed a primary key column")
	}

	// Structure changes; the key stays.
	from, err := c.Describe(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	to := driver.TableDef{Ref: from.Ref, Comment: from.Comment, PrimaryKey: from.PrimaryKey, Options: map[string]string{}, Indexes: from.Indexes}
	for k, v := range from.Options {
		to.Options[k] = v
	}
	for _, col := range from.Columns {
		to.Columns = append(to.Columns, driver.ColumnDef{Column: col, OriginalName: col.Name})
	}
	if again, err := c.AlterTableSQL(from, to); err != nil || len(again) != 0 {
		t.Errorf("unchanged table altered: %q, %v", again, err)
	}
	to.Columns = append(to.Columns, driver.ColumnDef{Column: driver.Column{Name: "note", Type: "text"}})
	to.Comment = "changed"
	to.Options["gc_grace_seconds"] = "3600"
	to.Indexes = []driver.Index{from.Indexes[0]}
	stmts, err = c.AlterTableSQL(from, to)
	if err != nil {
		t.Fatal(err)
	}
	run(t, sess, strings.Join(stmts, ";\n"))
	after, err := c.Describe(ctx, ref)
	if err != nil || after.Comment != "changed" || after.Options["gc_grace_seconds"] != "3600" || len(after.Indexes) != 1 || len(after.Columns) != 8 {
		t.Errorf("after alter (%q): %+v, %v", stmts, after, err)
	}
	to.PrimaryKey = []string{"device"}
	if _, err := c.AlterTableSQL(after, to); err == nil {
		t.Error("changed the primary key")
	}

	// Imports, then the console: USE, CONSISTENCY, batches, errors, tracing.
	imp, err := c.BeginImport(ctx, after, []string{"device", "at", "level", "attrs"}, true)
	if err != nil {
		t.Fatal(err)
	}
	var rows [][]any
	for i := range 50 {
		rows = append(rows, []any{"d9", time.Date(2025, 1, 1, 0, i, 0, 0, time.UTC).Format("2006-01-02 15:04:05+00:00"), float64(i), `{"k": 1}`})
	}
	if err := imp.Insert(ctx, rows); err != nil {
		t.Fatalf("import: %v", err)
	}
	imp.Commit()
	if n, err := c.Count(ctx, driver.BrowseRequest{Ref: ref}); err != nil || n.Rows != 50 {
		t.Errorf("after import = %+v, %v", n, err)
	}
	q, args, err := c.BrowseQuery(ctx, after, driver.BrowseRequest{Ref: ref, Filters: []driver.Filter{{Column: "device", Op: "=", Value: "d9"}}})
	if err != nil {
		t.Fatal(err)
	}
	s = run(t, sess, q, args...)
	if len(s.rows[0]) != 50 {
		t.Errorf("export query read %d rows", len(s.rows[0]))
	}
	s = run(t, sess, `USE system;
CONSISTENCY ONE;
SELECT count(*) FROM shop.customers;
USE shop;
BEGIN BATCH
  INSERT INTO it_events (device, at, level) VALUES ('d5', '2025-01-01 00:00:00+0000', 1);
  INSERT INTO it_events (device, at, level) VALUES ('d5', '2025-01-02 00:00:00+0000', 2);
APPLY BATCH;
SELECT count(*) FROM it_events WHERE device = 'd5';
CONSISTENCY`)
	if len(s.rows) != 2 || s.rows[1][0][0] != int64(2) || !strings.Contains(strings.Join(s.notices, " "), "Current consistency level is ONE") {
		t.Errorf("console = %v, %q", s.rows, s.notices)
	}
	bad := &sink{}
	sess.Execute(ctx, "SELECT 1 FROM it_events;\nSELECT nope\n  FROM it_events", driver.ExecOptions{}, bad)
	if len(bad.errs) != 2 || bad.errs[1] == nil {
		t.Errorf("errors = %v", bad.errs)
	} else {
		t.Logf("error: %v (line %d)", bad.errs[1], bad.errs[1].(*driver.QueryError).Line)
	}
	s = run(t, sess, "TRACING ON;\nSELECT * FROM customers LIMIT 1;\nTRACING OFF")
	if len(s.rows) != 2 || len(s.rows[1]) == 0 {
		t.Errorf("traced = %d result sets", len(s.rows))
	}

	// Plans as traces, processes, settings, DDL statements.
	if _, err := c.Explain(ctx, driver.Scope{Database: "shop"}, "INSERT INTO it_events (device, at) VALUES ('x', '2025-01-01')", true); err == nil {
		t.Error("traced a write")
	}
	plan, err := c.Explain(ctx, driver.Scope{Database: "shop"}, "SELECT * FROM sensor_readings WHERE sensor_id = 'boiler-2' AND day = '2025-06-01'", false)
	if err != nil || plan.Root == nil || len(plan.Root.Children) == 0 {
		t.Errorf("explain: %+v, %v", plan, err)
	} else {
		b, _ := json.Marshal(plan.Root)
		t.Logf("plan: %.300s", b)
	}
	for name, f := range map[string]func() (*driver.Result, error){
		"processes": func() (*driver.Result, error) { return c.Processes(ctx) },
		"variables": func() (*driver.Result, error) { return c.Variables(ctx, "variables") },
		"status":    func() (*driver.Result, error) { return c.Variables(ctx, "status") },
	} {
		if r, err := f(); err != nil || len(r.Rows) == 0 {
			t.Errorf("%s: %v, %v", name, r, err)
		}
	}
	for _, gen := range []func() ([]string, error){
		func() ([]string, error) { return c.TruncateSQL(ref) },
		func() ([]string, error) {
			return c.DropObjectSQL(driver.ObjectRef{Database: "shop", Name: "it_events", Kind: "table"}, false)
		},
		func() ([]string, error) { return c.CreateDatabaseSQL("it_keyspace", nil) },
		func() ([]string, error) { return c.DropDatabaseSQL("it_keyspace", false) },
	} {
		st, err := gen()
		if err != nil {
			t.Fatalf("ddl: %v", err)
		}
		run(t, sess, strings.Join(st, ";\n"))
	}
}
