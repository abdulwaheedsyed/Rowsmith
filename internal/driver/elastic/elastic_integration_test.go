package elastic

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"rowsmith/internal/driver"
)

// Integration tests run against the dev stack's servers (dev/compose.yaml,
// seeded by dev/seed/elastic-seed.sh) when their hosts are set. They create
// and drop their own it- indices.
//
//	ROWSMITH_TEST_ELASTIC_HOST, _PASSWORD      user elastic, plain HTTP
//	ROWSMITH_TEST_OPENSEARCH_HOST, _PASSWORD   user admin, HTTPS (self-signed)

type target struct {
	id, prefix, user, tls string
}

var targets = []target{{"elasticsearch", "ROWSMITH_TEST_ELASTIC_", "elastic", "disable"}, {"opensearch", "ROWSMITH_TEST_OPENSEARCH_", "admin", "require"}}

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
		Params:  map[string]any{"host": host, "user": tg.user, "tls": tg.tls},
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

func run(t *testing.T, sess driver.Session, script string, maxRows int) *sink {
	t.Helper()
	out := &sink{}
	if err := sess.Execute(context.Background(), script, driver.ExecOptions{StopOnError: true, MaxRows: maxRows}, out); err != nil {
		t.Fatalf("execute: %v", err)
	}
	for i, err := range out.errs {
		if err != nil {
			t.Fatalf("%s: %v", out.stmts[i].SQL, err)
		}
	}
	return out
}

func cell(res *driver.Result, row int, name string) any {
	for i, c := range res.Columns {
		if c.Name == name {
			return res.Rows[row][i]
		}
	}
	return "<missing>"
}

func TestIntegration(t *testing.T) {
	for _, tg := range targets {
		t.Run(tg.id, func(t *testing.T) { testEngine(t, open(t, tg)) })
	}
}

func testEngine(t *testing.T, c *conn) {
	ctx := context.Background()
	srv, err := c.Server(ctx)
	if err != nil || srv.Version == "" || srv.User == "" || srv.Extras["Health"] == "" {
		t.Errorf("server = %+v, %v", srv, err)
	}
	t.Logf("%s %s as %s", srv.Product, srv.Version, srv.User)
	objs, err := c.Objects(ctx, driver.Scope{})
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]string{}
	for _, o := range objs {
		kinds[o.Name] = o.Kind
	}
	if kinds["products"] != "table" || kinds["events-app"] != "data_stream" || kinds["catalog"] != "alias" || kinds[".security-7"] != "" {
		t.Errorf("objects = %v", kinds)
	}

	ref := driver.ObjectRef{Name: "products", Kind: "table"}
	tb, err := c.Describe(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	cols := map[string]driver.Column{}
	for _, col := range tb.Columns {
		cols[col.Name] = col
	}
	if tb.Columns[0].Name != "_id" || cols["name"].Kind != driver.KindText || cols["price"].Kind != driver.KindFloat || cols["warehouse"].Kind != driver.KindGeometry ||
		*tb.RowEstimate != 240 || tb.Options["primary_key"] != "sku (copied into _id)" || !strings.HasPrefix(tb.DDL, "PUT /products\n{") || !tb.Editable {
		t.Errorf("products = %+v", tb)
	}
	if ds, err := c.Describe(ctx, driver.ObjectRef{Name: "events-app"}); err != nil || ds.Kind != "data_stream" || ds.Editable || *ds.RowEstimate != 150 {
		t.Errorf("data stream = %+v, %v", ds, err)
	}

	// Typed filters, a text field's keyword sub-field for sorting, nested and geo values.
	res, err := c.Browse(ctx, driver.BrowseRequest{Ref: ref, Limit: 5, Offset: 1,
		Filters: []driver.Filter{{Column: "category", Op: "=", Value: "coffee"}, {Column: "price", Op: ">=", Value: "20"},
			{Column: "available", Op: "=", Value: "true"}, {Column: "name", Op: "contains", Value: "blend"}},
		Sort: []driver.Sort{{Column: "name"}, {Column: "price", Desc: true}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) == 0 || !strings.Contains(res.SQL, `"name.keyword"`) {
		t.Fatalf("browse = %+v", res)
	}
	if g, _ := json.Marshal(cell(res, 0, "warehouse")); !strings.Contains(string(g), `"$geo":{"coordinates":[`) {
		t.Errorf("geo = %s", g)
	}
	if a, _ := json.Marshal(cell(res, 0, "attributes")); !strings.Contains(string(a), `"origin"`) {
		t.Errorf("object = %s", a)
	}
	for _, f := range []driver.Filter{{Column: "sku", Op: "in", Values: []any{"SKU-0001", "SKU-0002"}}, {Column: "_id", Op: "=", Value: "SKU-0003"},
		{Column: "tags", Op: "notnull"}, {Column: "added", Op: "between", Values: []any{"2024-01-01 00:00:00", "2024-06-30 23:59:59"}},
		{Column: "sku", Op: "startswith", Value: "sku-01"}} {
		n, err := c.Count(ctx, driver.BrowseRequest{Ref: ref, Filters: []driver.Filter{f}})
		if err != nil || n.Rows == 0 || !n.Exact {
			t.Errorf("count %s %s = %+v, %v", f.Column, f.Op, n, err)
		}
	}
	if n, err := c.Count(ctx, driver.BrowseRequest{Ref: driver.ObjectRef{Name: "catalog", Kind: "alias"}, Search: "fruity"}); err != nil || n.Rows == 0 {
		t.Errorf("search through the alias = %+v, %v", n, err)
	}
	if _, err := c.Browse(ctx, driver.BrowseRequest{Ref: ref, Limit: 10, Sort: []driver.Sort{{Column: "description"}}}); err == nil {
		t.Error("sorted by a text field")
	}

	// A scratch index through the structure editor; edits checked by version.
	sess, _ := c.NewSession(ctx, driver.Scope{})
	defer sess.Close()
	idx := "it-notes"
	c.raw(ctx, "DELETE", "/"+idx, nil)
	stmts, err := c.CreateTableSQL(driver.TableDef{Ref: driver.ObjectRef{Name: idx}, PrimaryKey: []string{"code"},
		Options: map[string]string{"number_of_shards": "1", "number_of_replicas": "0"},
		Columns: []driver.ColumnDef{{Column: driver.Column{Name: "code", Type: "keyword"}}, {Column: driver.Column{Name: "body", Type: "text"}},
			{Column: driver.Column{Name: "n", Type: "long"}}, {Column: driver.Column{Name: "at", Type: "date"}}}})
	if err != nil {
		t.Fatal(err)
	}
	run(t, sess, strings.Join(stmts, "\n\n"), 100)
	nref := driver.ObjectRef{Name: idx, Kind: "table"}
	ins, err := c.ApplyEdits(ctx, nref, []driver.RowEdit{
		{Op: "insert", Values: map[string]any{"code": "a1", "body": "first note", "n": float64(1), "at": "2025-01-02 03:04:05+00:00"}},
		{Op: "insert", Values: map[string]any{"code": "b2", "body": "second", "n": "2"}},
	})
	if err != nil || len(ins.Inserted) != 2 || ins.Inserted[0][0] != "a1" {
		t.Fatalf("insert = %+v, %v", ins, err)
	}
	if _, err := c.ApplyEdits(ctx, nref, []driver.RowEdit{{Op: "insert", Values: map[string]any{"code": "a1"}}}); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("duplicate insert: %v", err)
	}
	if _, err := c.ApplyEdits(ctx, nref, []driver.RowEdit{
		{Op: "update", Key: map[string]any{"_id": "a1"}, Values: map[string]any{"n": float64(10), "body": map[string]any{"$null": true}}},
		{Op: "delete", Key: map[string]any{"_id": "b2"}},
	}); err != nil {
		t.Fatalf("edit: %v", err)
	}
	res, err = c.Browse(ctx, driver.BrowseRequest{Ref: nref, Limit: 10})
	if err != nil || len(res.Rows) != 1 || cell(res, 0, "n") != int64(10) || cell(res, 0, "body") != nil || cell(res, 0, "at") != "2025-01-02T03:04:05+00:00" {
		t.Errorf("after edits = %+v, %v", res, err)
	}
	if _, err := c.ApplyEdits(ctx, nref, []driver.RowEdit{{Op: "delete", Key: map[string]any{"_id": "b2"}}}); err == nil {
		t.Error("deleted a missing document")
	}
	if _, err := c.ApplyEdits(ctx, driver.ObjectRef{Name: "events-app"}, []driver.RowEdit{{Op: "delete", Key: map[string]any{"_id": "x"}}}); err == nil {
		t.Error("edited a data stream")
	}

	// Mappings grow; they don't change.
	from, _ := c.Describe(ctx, nref)
	to := driver.TableDef{Ref: from.Ref, PrimaryKey: []string{"code"}, Options: map[string]string{"number_of_replicas": "0", "refresh_interval": "5s"}}
	for _, col := range from.Columns {
		to.Columns = append(to.Columns, driver.ColumnDef{Column: col, OriginalName: col.Name})
	}
	to.Columns = append(to.Columns, driver.ColumnDef{Column: driver.Column{Name: "tags", Type: "keyword"}})
	stmts, err = c.AlterTableSQL(from, to)
	if err != nil || len(stmts) != 2 {
		t.Fatalf("alter = %q, %v", stmts, err)
	}
	run(t, sess, strings.Join(stmts, "\n"), 100)
	if after, err := c.Describe(ctx, nref); err != nil || len(after.Columns) != 6 || after.Options["refresh_interval"] != "5s" {
		t.Errorf("after alter = %+v, %v", after, err)
	}
	to.Columns[2].Type = "keyword"
	if _, err := c.AlterTableSQL(from, to); err == nil {
		t.Error("changed a field's type")
	}

	// Bulk import, then a whole-index read the way exports do it (a scroll past 10,000 is the same path).
	imp, err := c.BeginImport(ctx, from, []string{"code", "body", "n"}, true)
	if err != nil {
		t.Fatal(err)
	}
	var rows [][]any
	for i := range 2500 {
		rows = append(rows, []any{fmt.Sprintf("k%04d", i), "imported", float64(i)})
	}
	if err := imp.Insert(ctx, rows); err != nil {
		t.Fatalf("import: %v", err)
	}
	if err := imp.Commit(); err != nil {
		t.Fatal(err)
	}
	q, _, err := c.BrowseQuery(ctx, from, driver.BrowseRequest{Ref: nref, Columns: []string{"_id", "code", "n"}})
	if err != nil {
		t.Fatal(err)
	}
	s := run(t, sess, q, 0)
	if len(s.rows[0]) != 2500 || len(s.cols[0]) != 3 {
		t.Errorf("export read %d rows, %d columns", len(s.rows[0]), len(s.cols[0]))
	}

	// The console: Dev Tools requests, aggregations, _cat, SQL, errors and read-only.
	s = run(t, sess, `# products by category
GET /products/_search
{
  "size": 2,
  "query": { "range": { "price": { "gte": 10 } } },
  "aggs": { "by_category": { "terms": { "field": "category" }, "aggs": { "avg_price": { "avg": { "field": "price" } } } } }
}

GET _cat/indices/products

POST /products/_count
{ "query": { "term": { "category": "tea" } } }`, 100)
	if len(s.rows) != 4 || len(s.rows[0]) != 2 || len(s.rows[1]) != 5 || s.cols[1][2].Name != "avg_price" || len(s.rows[2]) != 1 {
		t.Errorf("console = %d result sets: %v", len(s.rows), s.cols)
	}
	sqlReq := "POST /_sql?format=json\n{\"query\": \"SELECT category, count(*) AS n FROM products GROUP BY category\"}"
	if c.product == "OpenSearch" {
		sqlReq = "POST /_plugins/_sql\n{\"query\": \"SELECT category, count(*) AS n FROM products GROUP BY category\"}"
	}
	s = run(t, sess, sqlReq, 100)
	if len(s.rows[0]) != 5 {
		t.Errorf("sql = %v", s.rows)
	}
	bad := &sink{}
	sess.Execute(ctx, "GET /products/_search\n{ \"query\": { \"nope\": {} } }", driver.ExecOptions{}, bad)
	if len(bad.errs) != 1 || bad.errs[0] == nil || bad.errs[0].(*driver.QueryError).Line != 1 {
		t.Errorf("bad query: %v", bad.errs)
	} else {
		t.Logf("error: %v", bad.errs[0])
	}
	bad = &sink{}
	sess.Execute(ctx, "GET /products/_search\n{ \"query\": ", driver.ExecOptions{}, bad)
	if len(bad.errs) != 1 || bad.errs[0] == nil || bad.errs[0].(*driver.QueryError).Line != 2 {
		t.Errorf("bad JSON: %v", bad.errs)
	}
	ro := &sink{}
	sess.Execute(ctx, "DELETE /"+idx, driver.ExecOptions{ReadOnly: true}, ro)
	if ro.errs[0] == nil {
		t.Error("deleted an index read-only")
	}

	// Profiles, tasks, settings, then cleanup through the DDL statements.
	plan, err := c.Explain(ctx, driver.Scope{}, "GET /products/_search\n{\"query\": {\"bool\": {\"filter\": [{\"term\": {\"category\": \"coffee\"}}, {\"range\": {\"price\": {\"lt\": 50}}}]}}}", true)
	if err != nil || len(plan.Root.Children) == 0 || len(plan.Root.Children[0].Children) == 0 {
		t.Errorf("explain = %+v, %v", plan, err)
	} else {
		b, _ := json.Marshal(plan.Root)
		t.Logf("plan: %.300s", b)
	}
	if p, err := c.Processes(ctx); err != nil {
		t.Errorf("processes: %v", err)
	} else if slices.ContainsFunc(p.Rows, func(r []any) bool { return strings.HasPrefix(r[1].(string), "cluster:monitor/tasks/lists") }) {
		t.Error("the task listing lists itself")
	}
	for _, kind := range []string{"variables", "status"} {
		if r, err := c.Variables(ctx, kind); err != nil || len(r.Rows) == 0 {
			t.Errorf("%s: %v", kind, err)
		}
	}
	for _, gen := range []func() ([]string, error){
		func() ([]string, error) { return c.TruncateSQL(nref) },
		func() ([]string, error) { return c.DropObjectSQL(nref, false) },
	} {
		st, err := gen()
		if err != nil {
			t.Fatal(err)
		}
		run(t, sess, strings.Join(st, "\n"), 100)
	}
}
