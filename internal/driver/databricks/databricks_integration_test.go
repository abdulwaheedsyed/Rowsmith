package databricks

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"rowsmith/internal/driver"
)

// The integration test runs against a Databricks workspace of your own,
// on the host (it needs only the internet):
//
//	ROWSMITH_TEST_DATABRICKS_HOST       workspace host
//	ROWSMITH_TEST_DATABRICKS_HTTP_PATH  a SQL warehouse's HTTP path
//	ROWSMITH_TEST_DATABRICKS_TOKEN_FILE a file holding a personal access token
//
// It reads samples.tpch and works in a schema of its own in the workspace
// catalog, which it drops at the end. A run takes a minute or two of the
// warehouse.
func openDatabricks(t *testing.T) *conn {
	t.Helper()
	host := os.Getenv("ROWSMITH_TEST_DATABRICKS_HOST")
	if host == "" {
		t.Skip("ROWSMITH_TEST_DATABRICKS_HOST not set")
	}
	tok, err := os.ReadFile(os.Getenv("ROWSMITH_TEST_DATABRICKS_TOKEN_FILE"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	d, _ := driver.Get("databricks")
	cn, err := d.Open(ctx, driver.OpenParams{AppName: "rowsmith test",
		Params:  map[string]any{"host": host, "httpPath": os.Getenv("ROWSMITH_TEST_DATABRICKS_HTTP_PATH"), "auth": "token", "catalog": "samples", "schema": "tpch"},
		Secrets: map[string]string{"token": strings.TrimSpace(string(tok))}})
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

func cell(res *driver.Result, row int, name string) any {
	for i, c := range res.Columns {
		if c.Name == name {
			return res.Rows[row][i]
		}
	}
	return "<missing>"
}

func TestIntegrationDatabricks(t *testing.T) {
	c := openDatabricks(t)
	ctx := context.Background()
	srv, err := c.Server(ctx)
	if err != nil || srv.Version == "" || srv.User == "" || !strings.Contains(srv.Extras["Warehouse"], "(") {
		t.Errorf("server = %+v, %v", srv, err)
	}
	t.Logf("%s %s as %s on %s", srv.Product, srv.Version, srv.User, srv.Extras["Warehouse"])
	dbs, err := c.Databases(ctx)
	if err != nil || !slices.ContainsFunc(dbs, func(d driver.Database) bool { return d.Name == "samples" }) {
		t.Fatalf("catalogs = %+v, %v", dbs, err)
	}
	objs, err := c.Objects(ctx, driver.Scope{Database: "samples", Schema: "tpch"})
	if err != nil || !slices.ContainsFunc(objs, func(o driver.Object) bool { return o.Name == "orders" && o.Kind == "table" && o.Engine == "delta" }) {
		t.Fatalf("objects = %+v, %v", objs, err)
	}

	// Reading the samples: typed filters, sorting, exact decimals, plans.
	ref := driver.ObjectRef{Database: "samples", Schema: "tpch", Name: "orders"}
	orders, err := c.Describe(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	cols := map[string]driver.Column{}
	for _, col := range orders.Columns {
		cols[col.Name] = col
	}
	if cols["o_totalprice"].Kind != driver.KindDecimal || *cols["o_totalprice"].Precision != 18 || cols["o_orderdate"].Kind != driver.KindDate ||
		orders.DDL == "" || orders.Options["format"] != "delta" || orders.Size == nil {
		t.Errorf("orders = %+v", orders)
	}
	res, err := c.Browse(ctx, driver.BrowseRequest{Ref: ref, Limit: 5, Offset: 3,
		Filters: []driver.Filter{{Column: "o_orderstatus", Op: "=", Value: "F"}, {Column: "o_totalprice", Op: ">", Value: "100000.50"},
			{Column: "o_orderdate", Op: "between", Values: []any{"1993-01-01", "1993-12-31"}}, {Column: "o_clerk", Op: "like", Value: "%0001%"}},
		Sort: []driver.Sort{{Column: "o_totalprice", Desc: true}}})
	if err != nil || len(res.Rows) != 5 {
		t.Fatalf("browse = %+v, %v", res, err)
	}
	if p, ok := cell(res, 0, "o_totalprice").(string); !ok || !strings.Contains(p, ".") {
		t.Errorf("decimal = %#v", cell(res, 0, "o_totalprice"))
	}
	if n, err := c.Count(ctx, driver.BrowseRequest{Ref: ref, Filters: []driver.Filter{{Column: "o_orderpriority", Op: "in", Values: []any{"1-URGENT", "2-HIGH"}}}}); err != nil || n.Rows == 0 {
		t.Errorf("count = %+v, %v", n, err)
	}
	plan, err := c.Explain(ctx, driver.Scope{Database: "samples", Schema: "tpch"}, "SELECT o_orderstatus, count(*) FROM orders o JOIN customer c ON c.c_custkey = o.o_custkey GROUP BY 1", false)
	if err != nil || plan.Root == nil || len(plan.Root.Children) == 0 {
		t.Errorf("explain = %+v, %v", plan, err)
	} else {
		b, _ := json.Marshal(plan.Root)
		t.Logf("plan: %.400s", b)
	}

	// A schema of our own: a Delta table through the structure editor, then edits.
	schema := fmt.Sprintf("rowsmith_it_%d", time.Now().Unix())
	sess, err := c.NewSession(ctx, driver.Scope{Database: "workspace", Schema: "default"})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	st, _ := c.CreateSchemaSQL("workspace", schema)
	run(t, sess, strings.Join(st, ";\n"))
	t.Cleanup(func() {
		st, _ := c.DropSchemaSQL("workspace", schema, true)
		sess.Execute(context.Background(), strings.Join(st, ";\n"), driver.ExecOptions{}, &sink{})
	})
	tref := driver.ObjectRef{Database: "workspace", Schema: schema, Name: "events"}
	stmts, err := c.CreateTableSQL(driver.TableDef{Ref: tref, Comment: "events for tests", PrimaryKey: []string{"id"},
		Options: map[string]string{"cluster_by": "device"},
		Checks:  []driver.Check{{Name: "level_ok", Expression: "level BETWEEN 0 AND 9"}},
		Columns: []driver.ColumnDef{
			{Column: driver.Column{Name: "id", Type: "BIGINT"}},
			{Column: driver.Column{Name: "device", Type: "STRING", Nullable: true}},
			{Column: driver.Column{Name: "level", Type: "INT", Nullable: true}},
			{Column: driver.Column{Name: "amount", Type: "DECIMAL(10,2)", Nullable: true}},
			{Column: driver.Column{Name: "at", Type: "TIMESTAMP", Nullable: true}},
			{Column: driver.Column{Name: "tags", Type: "ARRAY<STRING>", Nullable: true}},
			{Column: driver.Column{Name: "attrs", Type: "STRUCT<color: STRING, size: INT>", Nullable: true}},
		}})
	if err != nil {
		t.Fatal(err)
	}
	run(t, sess, strings.Join(stmts, ";\n"))
	if _, err := c.ApplyEdits(ctx, tref, []driver.RowEdit{
		{Op: "insert", Values: map[string]any{"id": float64(1), "device": "d1", "level": float64(3), "amount": "12.50", "at": "2025-03-04 05:06:07+00:00",
			"tags": []any{"a", "b"}, "attrs": map[string]any{"color": "red", "size": float64(2)}}},
		{Op: "insert", Values: map[string]any{"id": "2", "device": "d2", "level": "4"}},
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := c.ApplyEdits(ctx, tref, []driver.RowEdit{{Op: "insert", Values: map[string]any{"id": float64(3), "level": float64(42)}}}); err == nil {
		t.Error("the CHECK constraint let 42 in")
	}
	res, err = c.Browse(ctx, driver.BrowseRequest{Ref: tref, Limit: 10, Sort: []driver.Sort{{Column: "id"}}})
	if err != nil || len(res.Rows) != 2 {
		t.Fatalf("events = %+v, %v", res, err)
	}
	if a, _ := json.Marshal(cell(res, 0, "attrs")); string(a) != `{"color":"red","size":2}` || cell(res, 0, "amount") != "12.50" || cell(res, 0, "at") != "2025-03-04 05:06:07+00:00" {
		t.Errorf("row = %v (attrs %s)", res.Rows[0], a)
	}
	if _, err := c.ApplyEdits(ctx, tref, []driver.RowEdit{
		{Op: "update", Key: map[string]any{"id": int64(1)}, Values: map[string]any{"level": float64(9), "tags": `["c"]`, "amount": map[string]any{"$null": true}}},
		{Op: "delete", Key: map[string]any{"id": int64(2)}},
	}); err != nil {
		t.Fatalf("edit: %v", err)
	}
	s := run(t, sess, "SELECT level, tags, amount FROM "+qualify(tref))
	if len(s.rows[0]) != 1 || s.rows[0][0][0] != int64(9) || s.rows[0][0][2] != nil {
		t.Errorf("after edit = %v", s.rows)
	}

	// Structure changes: add, rename (column mapping), comment, checks, clustering.
	from, err := c.Describe(ctx, tref)
	if err != nil {
		t.Fatal(err)
	}
	if from.Options["cluster_by"] != "device" || len(from.Checks) != 1 || from.Options["version"] == "" || !from.Editable || len(from.PrimaryKey) != 1 {
		t.Errorf("events = %+v", from)
	}
	to := driver.TableDef{Ref: from.Ref, Comment: from.Comment, PrimaryKey: from.PrimaryKey, Options: maps.Clone(from.Options), Checks: from.Checks}
	for _, col := range from.Columns {
		to.Columns = append(to.Columns, driver.ColumnDef{Column: col, OriginalName: col.Name})
	}
	if again, err := c.AlterTableSQL(from, to); err != nil || len(again) != 0 {
		t.Errorf("unchanged table altered: %q, %v", again, err)
	}
	to.Columns[1].Name = "device_id"
	to.Columns = append(to.Columns, driver.ColumnDef{Column: driver.Column{Name: "note", Type: "STRING", Nullable: true, Comment: "free text"}})
	to.Comment = "changed"
	to.Checks = append(to.Checks, driver.Check{Name: "id_positive", Expression: "id > 0"})
	to.Options["cluster_by"] = "device_id, level"
	stmts, err = c.AlterTableSQL(from, to)
	if err != nil {
		t.Fatal(err)
	}
	run(t, sess, strings.Join(stmts, ";\n"))
	after, err := c.Describe(ctx, tref)
	if err != nil || after.Comment != "changed" || after.Columns[1].Name != "device_id" || len(after.Checks) != 2 || after.Options["cluster_by"] != "device_id, level" ||
		after.Options["column_mapping"] != "name" {
		t.Errorf("after alter (%q) = %+v, %v", stmts, after, err)
	}

	// Imports and a whole-table read as exports do it.
	imp, err := c.BeginImport(ctx, after, []string{"id", "device_id", "level", "tags"}, true)
	if err != nil {
		t.Fatal(err)
	}
	var rows [][]any
	for i := range 300 {
		rows = append(rows, []any{float64(i + 10), fmt.Sprintf("d%d", i%7), float64(i % 10), `["x"]`})
	}
	if err := imp.Insert(ctx, rows); err != nil {
		t.Fatalf("import: %v", err)
	}
	imp.Commit()
	q, args, err := c.BrowseQuery(ctx, after, driver.BrowseRequest{Ref: tref, Filters: []driver.Filter{{Column: "level", Op: ">=", Value: "5"}}})
	if err != nil {
		t.Fatal(err)
	}
	s = run(t, sess, q, args...)
	if len(s.rows[0]) != 150 {
		t.Errorf("export read %d rows", len(s.rows[0]))
	}

	// The console: USE carries over, errors point at the line.
	s = run(t, sess, "USE SCHEMA "+quote(schema)+";\nSELECT count(*) FROM events")
	if len(s.rows) != 1 || s.rows[0][0][0] != int64(300) {
		t.Errorf("console = %v", s.rows)
	}
	bad := &sink{}
	sess.Execute(ctx, "SELECT 1;\nSELECT nope\n  FROM events", driver.ExecOptions{}, bad)
	if len(bad.errs) != 2 || bad.errs[1] == nil {
		t.Errorf("errors = %v", bad.errs)
	} else if qe, ok := bad.errs[1].(*driver.QueryError); !ok || qe.Line != 2 || qe.Code == "" {
		t.Errorf("error = %#v", bad.errs[1])
	} else {
		t.Logf("error: [%s] %s (line %d, position %d)", qe.Code, qe.Message, qe.Line, qe.Position)
	}

	for name, f := range map[string]func() (*driver.Result, error){
		"processes": func() (*driver.Result, error) { return c.Processes(ctx) },
		"variables": func() (*driver.Result, error) { return c.Variables(ctx, "variables") },
		"status":    func() (*driver.Result, error) { return c.Variables(ctx, "status") },
	} {
		if r, err := f(); err != nil || (name != "processes" && len(r.Rows) == 0) {
			t.Errorf("%s: %v, %v", name, r, err)
		}
	}
	for _, gen := range []func() ([]string, error){
		func() ([]string, error) { return c.TruncateSQL(tref) },
		func() ([]string, error) { return c.RenameObjectSQL(tref, "events2") },
		func() ([]string, error) {
			return c.DropObjectSQL(driver.ObjectRef{Database: "workspace", Schema: schema, Name: "events2", Kind: "table"}, false)
		},
	} {
		st, err := gen()
		if err != nil {
			t.Fatal(err)
		}
		run(t, sess, strings.Join(st, ";\n"))
	}
}
