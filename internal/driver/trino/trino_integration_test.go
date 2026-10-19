package trino

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

// Integration tests run against the dev stack's Trino (dev/compose.yaml)
// when ROWSMITH_TEST_TRINO_HOST is set: Iceberg tables in iceberg.shop and
// plain Parquet and CSV files in hive.raw (dev/seed/trino.sql). They create
// and drop their own it_ tables.
//
//	ROWSMITH_TEST_TRINO_HOST, _PORT (8080)

func openTrino(t *testing.T) *conn {
	t.Helper()
	host := os.Getenv("ROWSMITH_TEST_TRINO_HOST")
	if host == "" {
		t.Skip("ROWSMITH_TEST_TRINO_HOST not set")
	}
	port := os.Getenv("ROWSMITH_TEST_TRINO_PORT")
	if port == "" {
		port = "8080"
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	d, _ := driver.Get("trino")
	cn, err := d.Open(ctx, driver.OpenParams{AppName: "rowsmith-test",
		Params: map[string]any{"host": host, "port": port, "user": "rowsmith", "catalog": "iceberg", "schema": "shop", "tls": "disable"}})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { cn.Close() })
	return cn.(*conn)
}

type sink struct {
	stmts []driver.StatementInfo
	errs  []error
	cols  [][]driver.ResultColumn
	rows  [][][]any
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
func (s *sink) Notice(string, string) error          { return nil }
func (s *sink) EndStatement(err error) error         { s.errs = append(s.errs, err); return nil }

func run(t *testing.T, sess driver.Session, script string) *sink {
	t.Helper()
	out := &sink{}
	if err := sess.Execute(context.Background(), script, driver.ExecOptions{StopOnError: true, MaxRows: 1000}, out); err != nil {
		t.Fatalf("execute: %v", err)
	}
	for i, err := range out.errs {
		if err != nil {
			t.Fatalf("%s: %v", out.stmts[i].SQL, err)
		}
	}
	return out
}

func TestIntegrationTrino(t *testing.T) {
	c := openTrino(t)
	ctx := context.Background()
	srv, err := c.Server(ctx)
	if err != nil || srv.Product != "Trino" || srv.Version == "" {
		t.Errorf("server = %+v, %v", srv, err)
	}
	dbs, err := c.Databases(ctx)
	names := map[string]string{}
	for _, d := range dbs {
		names[d.Name] = d.Comment
	}
	if err != nil || names["iceberg"] != "iceberg" || names["hive"] != "hive" || names["tpch"] != "tpch" {
		t.Errorf("catalogs = %v, %v", names, err)
	}
	schemas, err := c.Schemas(ctx, "iceberg")
	if err != nil || !slices.ContainsFunc(schemas, func(s driver.Schema) bool { return s.Name == "shop" }) {
		t.Errorf("schemas = %+v, %v", schemas, err)
	}
	objs, err := c.Objects(ctx, driver.Scope{Database: "iceberg", Schema: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]string{}
	for _, o := range objs {
		kinds[o.Name] = o.Kind
	}
	if kinds["orders"] != "table" || kinds["revenue_by_segment"] != "view" || kinds["customers"] != "table" {
		t.Errorf("objects = %v", kinds)
	}

	ice := func(name string) driver.ObjectRef {
		return driver.ObjectRef{Database: "iceberg", Schema: "shop", Name: name}
	}
	tb, err := c.Describe(ctx, ice("orders"))
	if err != nil {
		t.Fatal(err)
	}
	if tb.Options["format"] != "PARQUET" || tb.Options["partitioning"] != "month(placed_at)" || tb.Options["snapshots"] != "4" ||
		tb.RowEstimate == nil || tb.Size == nil || !tb.Editable || tb.RowKeyKind != "all" || tb.DDL == "" {
		t.Errorf("orders = %+v", tb)
	}
	cols := map[string]driver.Column{}
	for _, col := range tb.Columns {
		cols[col.Name] = col
	}
	if cols["placed_at"].Kind != driver.KindTimestamp || cols["handled_by"].Kind != driver.KindArray || cols["total"].Kind != driver.KindFloat {
		t.Errorf("orders columns = %+v", tb.Columns)
	}
	if tb, err := c.Describe(ctx, driver.ObjectRef{Database: "hive", Schema: "raw", Name: "events"}); err != nil || tb.Options["format"] != "PARQUET" ||
		tb.Options["location"] != "s3://lake/raw/events/" || tb.Editable {
		t.Errorf("hive events = %+v, %v", tb, err)
	}
	if def, err := c.Definition(ctx, driver.ObjectRef{Database: "iceberg", Schema: "shop", Name: "revenue_by_segment", Kind: "view"}); err != nil || def == "" {
		t.Errorf("view definition: %q, %v", def, err)
	}
	if cat, err := c.CatalogColumns(ctx, driver.Scope{Database: "iceberg", Schema: "shop"}); err != nil || len(cat) < 4 {
		t.Errorf("catalog: %d, %v", len(cat), err)
	}

	// Browsing: typed filters, search, nested values, exact counts from metadata.
	res, err := c.Browse(ctx, driver.BrowseRequest{Ref: ice("orders"), Limit: 5,
		Filters: []driver.Filter{{Column: "status", Op: "=", Value: "F"}, {Column: "total", Op: ">", Value: "100000"}, {Column: "id", Op: ">", Value: float64(10)}},
		Sort:    []driver.Sort{{Column: "placed_at", Desc: true}}, Offset: 2})
	if err != nil || len(res.Rows) != 5 {
		t.Fatalf("orders: %+v, %v", res, err)
	}
	t.Logf("order: %#v", res.Rows[0])
	if n, err := c.Count(ctx, driver.BrowseRequest{Ref: ice("orders")}); err != nil || n.Rows == 0 {
		t.Errorf("count: %+v, %v", n, err)
	}
	if n, err := c.Count(ctx, driver.BrowseRequest{Ref: ice("customers"), Search: "furniture"}); err != nil || n.Rows == 0 {
		t.Errorf("search count: %+v, %v", n, err)
	}
	res, err = c.Browse(ctx, driver.BrowseRequest{Ref: ice("customers"), Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := json.Marshal(res.Rows[0][5]); !strings.HasPrefix(string(b), `{"phone":`) {
		t.Errorf("row value = %s", b)
	}
	if res, err := c.Browse(ctx, driver.BrowseRequest{Ref: driver.ObjectRef{Database: "hive", Schema: "raw", Name: "nations"}, Limit: 30}); err != nil || len(res.Rows) != 25 {
		t.Errorf("csv nations: %v rows, %v", len(res.Rows), err)
	}

	// Edits on an Iceberg table, matched on whole rows.
	sess, err := c.NewSession(ctx, driver.Scope{Database: "iceberg", Schema: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	run(t, sess, "DROP TABLE IF EXISTS it_people")
	def := driver.TableDef{Ref: ice("it_people"), Comment: "people for tests", Options: map[string]string{"format": "PARQUET", "partitioning": "bucket(id, 2)"},
		Columns: []driver.ColumnDef{
			{Column: driver.Column{Name: "id", Type: "bigint"}},
			{Column: driver.Column{Name: "name", Type: "varchar", Comment: "full name", Nullable: true}},
			{Column: driver.Column{Name: "score", Type: "decimal(8,2)", Nullable: true}},
			{Column: driver.Column{Name: "seen", Type: "timestamp(6) with time zone", Nullable: true}},
			{Column: driver.Column{Name: "tags", Type: "array(varchar)", Nullable: true}},
		}}
	stmts, err := c.CreateTableSQL(def)
	if err != nil {
		t.Fatal(err)
	}
	run(t, sess, strings.Join(stmts, ";\n"))
	if _, err := c.ApplyEdits(ctx, ice("it_people"), []driver.RowEdit{
		{Op: "insert", Values: map[string]any{"id": float64(1), "name": "Ada", "score": "9.50", "seen": "2025-01-02 03:04:05+00:00", "tags": []any{"a", "b"}}},
		{Op: "insert", Values: map[string]any{"id": float64(2), "name": "Grace"}},
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	res, err = c.Browse(ctx, driver.BrowseRequest{Ref: ice("it_people"), Limit: 10, Sort: []driver.Sort{{Column: "id"}}})
	if err != nil || len(res.Rows) != 2 {
		t.Fatalf("it_people: %+v, %v", res, err)
	}
	key := map[string]any{}
	for i, col := range res.Columns {
		key[col.Name] = res.Rows[0][i]
	}
	if _, err := c.ApplyEdits(ctx, ice("it_people"), []driver.RowEdit{{Op: "update", Key: key, Values: map[string]any{"score": float64(10)}}}); err != nil {
		t.Errorf("update by whole row: %v", err)
	}
	if _, err := c.ApplyEdits(ctx, ice("it_people"), []driver.RowEdit{{Op: "delete", Key: map[string]any{"id": float64(2), "name": "Grace", "score": nil, "seen": nil, "tags": nil}}}); err != nil {
		t.Errorf("delete: %v", err)
	}
	if _, err := c.ApplyEdits(ctx, driver.ObjectRef{Database: "hive", Schema: "raw", Name: "nations"}, []driver.RowEdit{{Op: "delete", Key: map[string]any{"id": "1"}}}); err == nil {
		t.Errorf("a Hive table was edited")
	}

	// Structure changes, imports, exports.
	from, err := c.Describe(ctx, ice("it_people"))
	if err != nil {
		t.Fatal(err)
	}
	to := driver.TableDef{Ref: from.Ref, Comment: from.Comment, Options: maps.Clone(from.Options)}
	for _, col := range from.Columns {
		to.Columns = append(to.Columns, driver.ColumnDef{Column: col, OriginalName: col.Name})
	}
	if again, err := c.AlterTableSQL(from, to); err != nil || len(again) != 0 {
		t.Errorf("unchanged table altered: %q, %v", again, err)
	}
	to.Columns[1].Name = "full_name"
	to.Columns = append(to.Columns, driver.ColumnDef{Column: driver.Column{Name: "email", Type: "varchar", Nullable: true}})
	to.Comment = "renamed"
	to.Options["partitioning"] = "bucket(id, 4)"
	stmts, err = c.AlterTableSQL(from, to)
	if err != nil {
		t.Fatal(err)
	}
	run(t, sess, strings.Join(stmts, ";\n"))
	after, err := c.Describe(ctx, ice("it_people"))
	if err != nil || len(after.Columns) != 6 || after.Columns[1].Name != "full_name" || after.Comment != "renamed" || after.Options["partitioning"] != "bucket(id, 4)" {
		t.Errorf("after alter: %+v, %v", after, err)
	}
	imp, err := c.BeginImport(ctx, after, []string{"id", "full_name", "tags"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := imp.Insert(ctx, [][]any{{float64(3), "Imp", `["x"]`}, {"4", nil, nil}}); err != nil {
		imp.Rollback()
		t.Fatalf("import: %v", err)
	}
	imp.Commit()
	if q, _, err := c.BrowseQuery(ctx, after, driver.BrowseRequest{}); err != nil || q == "" {
		t.Errorf("browse query: %q, %v", q, err)
	}

	// The console: USE carries, errors point at the line, time travel works.
	s := run(t, sess, `SELECT min(snapshot_id) FROM "orders$snapshots"`)
	first := s.rows[0][0][0]
	s = run(t, sess, fmt.Sprintf("SELECT count(*) FROM orders;\nSELECT count(*) FROM orders FOR VERSION AS OF %v", first))
	if len(s.rows) != 2 || s.rows[0][0][0] == s.rows[1][0][0] {
		t.Errorf("time travel counts = %v", s.rows)
	}
	bad := &sink{}
	sess.Execute(ctx, "SELECT 1;\nSELECT nope\n  FROM orders", driver.ExecOptions{}, bad)
	var qe *driver.QueryError
	if len(bad.errs) != 2 || bad.errs[1] == nil || !asQE(bad.errs[1], &qe) || qe.Line != 2 {
		t.Errorf("error = %v (%+v)", bad.errs, qe)
	}
	for _, gen := range []func() ([]string, error){
		func() ([]string, error) { return c.TruncateSQL(ice("it_people")) },
		func() ([]string, error) {
			return c.RenameObjectSQL(driver.ObjectRef{Database: "iceberg", Schema: "shop", Name: "it_people", Kind: "table"}, "it_people2")
		},
		func() ([]string, error) {
			return c.DropObjectSQL(driver.ObjectRef{Database: "iceberg", Schema: "shop", Name: "it_people2", Kind: "table"}, false)
		},
		func() ([]string, error) { return c.CreateSchemaSQL("iceberg", "it_schema") },
		func() ([]string, error) { return c.DropSchemaSQL("iceberg", "it_schema", false) },
	} {
		st, err := gen()
		if err != nil {
			t.Errorf("ddl: %v", err)
			continue
		}
		run(t, sess, strings.Join(st, ";\n"))
	}

	// Plans and administration.
	plan, err := c.Explain(ctx, driver.Scope{Database: "iceberg", Schema: "shop"}, "SELECT * FROM orders WHERE customer_id = 7", false)
	if err != nil || plan.Root == nil {
		t.Errorf("explain: %+v, %v", plan, err)
	} else {
		b, _ := json.Marshal(plan.Root)
		t.Logf("plan: %.400s", b)
	}
	plan, err = c.Explain(ctx, driver.Scope{Database: "iceberg", Schema: "shop"}, "SELECT count(*) FROM orders", true)
	if err != nil || plan.Root == nil || len(plan.Root.Children) == 0 {
		t.Errorf("explain analyze: %+v, %v", plan, err)
	} else {
		b, _ := json.Marshal(plan.Root)
		t.Logf("analyze: %.500s", b)
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
}

func asQE(err error, qe **driver.QueryError) bool {
	q, ok := err.(*driver.QueryError)
	if ok {
		*qe = q
	}
	return ok
}
