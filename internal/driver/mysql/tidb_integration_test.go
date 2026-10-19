package mysql

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"rowsmith/internal/driver"
)

// TiDB runs against the dev stack's seeded shop database (dev/seed/tidb.sql)
// when ROWSMITH_TEST_TIDB_HOST is set, signing in as root with
// ROWSMITH_TEST_MYSQL_PASSWORD. It creates and drops its own it_ tables.

func openTiDB(t *testing.T) *conn {
	t.Helper()
	host := os.Getenv("ROWSMITH_TEST_TIDB_HOST")
	if host == "" {
		t.Skip("ROWSMITH_TEST_TIDB_HOST not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	d, _ := driver.Get("tidb")
	cn, err := d.Open(ctx, driver.OpenParams{AppName: "rowsmith-test",
		Params:  map[string]any{"host": host, "user": "root", "database": "shop", "tls": "disable"},
		Secrets: map[string]string{"password": os.Getenv("ROWSMITH_TEST_MYSQL_PASSWORD")}})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { cn.Close() })
	return cn.(*conn)
}

type tidbSink struct {
	stmts []driver.StatementInfo
	errs  []error
	rows  [][][]any
}

func (s *tidbSink) BeginStatement(i driver.StatementInfo) error {
	s.stmts = append(s.stmts, i)
	return nil
}
func (s *tidbSink) Columns([]driver.ResultColumn) error { s.rows = append(s.rows, nil); return nil }
func (s *tidbSink) Rows(r [][]any) error {
	s.rows[len(s.rows)-1] = append(s.rows[len(s.rows)-1], r...)
	return nil
}
func (s *tidbSink) EndResult(driver.ResultSummary) error { return nil }
func (s *tidbSink) Notice(string, string) error          { return nil }
func (s *tidbSink) EndStatement(err error) error         { s.errs = append(s.errs, err); return nil }

func tidbRun(t *testing.T, sess driver.Session, stmts ...string) *tidbSink {
	t.Helper()
	sink := &tidbSink{}
	if err := sess.Execute(context.Background(), strings.Join(stmts, ";\n"), driver.ExecOptions{StopOnError: true, MaxRows: 1000}, sink); err != nil {
		t.Errorf("execute: %v", err)
	}
	for i, err := range sink.errs {
		if err != nil {
			t.Errorf("%s: %v", sink.stmts[i].SQL, err)
		}
	}
	return sink
}

func TestIntegrationTiDB(t *testing.T) {
	c := openTiDB(t)
	ctx := context.Background()
	srv, err := c.Server(ctx)
	if err != nil || srv.Product != "TiDB" || strings.Contains(srv.Version, "TiDB") || srv.Extras["MySQL compatibility"] == "" {
		t.Errorf("server = %+v, %v", srv, err)
	}
	dbs, err := c.Databases(ctx)
	if err != nil || !slices.ContainsFunc(dbs, func(d driver.Database) bool { return d.Name == "shop" && !d.System }) ||
		!slices.ContainsFunc(dbs, func(d driver.Database) bool { return d.Name == "METRICS_SCHEMA" && d.System }) {
		t.Errorf("databases = %+v, %v", dbs, err)
	}
	objs, err := c.Objects(ctx, driver.Scope{Database: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]string{}
	for _, o := range objs {
		kinds[o.Name] = o.Kind
	}
	for name, kind := range map[string]string{"customers": "table", "orders": "table", "audit_events": "table", "customer_revenue": "view"} {
		if kinds[name] != kind {
			t.Errorf("object %s: %q, want %q (all %v)", name, kinds[name], kind, kinds)
		}
	}

	tb, err := c.Describe(ctx, driver.ObjectRef{Database: "shop", Name: "products"})
	if err != nil {
		t.Fatal(err)
	}
	cols := map[string]driver.Column{}
	for _, col := range tb.Columns {
		cols[col.Name] = col
	}
	if !cols["id"].AutoIncrement || !cols["id"].PrimaryKey || cols["margin"].Generated == "" || cols["thumbnail"].Kind != driver.KindBinary ||
		len(tb.ForeignKeys) != 1 || len(tb.Indexes) < 2 || tb.DDL == "" {
		t.Errorf("products = %+v", tb)
	}
	tb, err = c.Describe(ctx, driver.ObjectRef{Database: "shop", Name: "customers"})
	if err != nil {
		t.Fatal(err)
	}
	cols = map[string]driver.Column{}
	for _, col := range tb.Columns {
		cols[col.Name] = col
	}
	if cols["tier"].Kind != driver.KindEnum || len(cols["tier"].Enum) != 3 || cols["preferences"].Kind != driver.KindJSON || cols["updated_at"].OnUpdate == "" || tb.Comment == "" {
		t.Errorf("customers = %+v", tb.Columns)
	}
	if tb, err := c.Describe(ctx, driver.ObjectRef{Database: "shop", Name: "order_items"}); err != nil || len(tb.Checks) == 0 || len(tb.PrimaryKey) != 2 {
		t.Errorf("order_items = %+v, %v", tb, err)
	}
	if tb, err := c.Describe(ctx, driver.ObjectRef{Database: "shop", Name: "audit_events"}); err != nil || len(tb.Columns) != 4 {
		t.Errorf("audit_events = %+v, %v", tb, err)
	}
	if def, err := c.Definition(ctx, driver.ObjectRef{Database: "shop", Name: "customer_revenue", Kind: "view"}); err != nil || def == "" {
		t.Errorf("view definition: %q, %v", def, err)
	}
	if cat, err := c.CatalogColumns(ctx, driver.Scope{Database: "shop"}); err != nil || len(cat) < 7 {
		t.Errorf("catalog: %d, %v", len(cat), err)
	}

	ref := driver.ObjectRef{Database: "shop", Name: "orders"}
	res, err := c.Browse(ctx, driver.BrowseRequest{Ref: ref, Limit: 5, Filters: []driver.Filter{{Column: "status", Op: "=", Value: "paid"}},
		Sort: []driver.Sort{{Column: "placed_at", Desc: true}}})
	if err != nil || len(res.Rows) != 5 {
		t.Fatalf("browse: %+v, %v", res, err)
	}
	if n, err := c.Count(ctx, driver.BrowseRequest{Ref: ref, Search: "door"}); err != nil || n.Rows == 0 {
		t.Errorf("count: %+v, %v", n, err)
	}

	sess, err := c.NewSession(ctx, driver.Scope{Database: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	tidbRun(t, sess, "DROP TABLE IF EXISTS it_people")
	def := driver.TableDef{Ref: driver.ObjectRef{Database: "shop", Name: "it_people"}, PrimaryKey: []string{"id"}, Comment: "people for tests",
		Columns: []driver.ColumnDef{
			{Column: driver.Column{Name: "id", Type: "bigint", AutoIncrement: true}},
			{Column: driver.Column{Name: "name", Type: "varchar(80)", Comment: "full name"}},
			{Column: driver.Column{Name: "score", Type: "decimal(8,2)", Nullable: true}},
		},
		Indexes: []driver.Index{{Name: "it_people_name", Columns: []string{"name"}, Unique: true}}}
	stmts, err := c.CreateTableSQL(def)
	if err != nil {
		t.Fatal(err)
	}
	tidbRun(t, sess, stmts...)
	pref := def.Ref
	if _, err := c.ApplyEdits(ctx, pref, []driver.RowEdit{{Op: "insert", Values: map[string]any{"name": "Ada", "score": "9.5"}}, {Op: "insert", Values: map[string]any{"name": "Grace"}}}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	res, err = c.Browse(ctx, driver.BrowseRequest{Ref: pref, Limit: 10, Sort: []driver.Sort{{Column: "name"}}})
	if err != nil || len(res.Rows) != 2 {
		t.Fatalf("browse it_people: %+v, %v", res, err)
	}
	if _, err := c.ApplyEdits(ctx, pref, []driver.RowEdit{{Op: "update", Key: map[string]any{"id": res.Rows[0][0]}, Values: map[string]any{"score": float64(10)}},
		{Op: "delete", Key: map[string]any{"id": res.Rows[1][0]}}}); err != nil {
		t.Errorf("update and delete: %v", err)
	}
	from, err := c.Describe(ctx, pref)
	if err != nil {
		t.Fatal(err)
	}
	to := driver.TableDef{Ref: from.Ref, PrimaryKey: from.PrimaryKey, Comment: from.Comment, Options: from.Options, ForeignKeys: from.ForeignKeys, Checks: from.Checks}
	for _, col := range from.Columns {
		to.Columns = append(to.Columns, driver.ColumnDef{Column: col, OriginalName: col.Name})
	}
	for _, ix := range from.Indexes {
		if !ix.Primary {
			to.Indexes = append(to.Indexes, ix)
		}
	}
	if again, err := c.AlterTableSQL(from, to); err != nil || len(again) != 0 {
		t.Errorf("unchanged table altered: %q, %v", again, err)
	}
	to.Columns = append(to.Columns, driver.ColumnDef{Column: driver.Column{Name: "email", Type: "varchar(190)", Nullable: true}, After: "name"})
	to.Columns[2].Name = "points"
	stmts, err = c.AlterTableSQL(from, to)
	if err != nil {
		t.Fatal(err)
	}
	tidbRun(t, sess, stmts...)
	if after, err := c.Describe(ctx, pref); err != nil || len(after.Columns) != 4 || after.Columns[2].Name != "email" && after.Columns[3].Name != "email" {
		t.Errorf("after alter: %+v, %v", after, err)
	}
	s := tidbRun(t, sess, "BEGIN", "INSERT INTO it_people (name) VALUES ('Tx')", "SELECT COUNT(*) FROM it_people")
	if !sess.InTransaction() || len(s.rows) == 0 {
		t.Errorf("transaction: %v %v", sess.InTransaction(), s.rows)
	}
	tidbRun(t, sess, "ROLLBACK")
	imp, err := c.BeginImport(ctx, from, []string{"name"}, false)
	if err != nil {
		t.Errorf("import: %v", err)
	} else if err := imp.Insert(ctx, [][]any{{"Imp"}}); err != nil {
		t.Errorf("import insert: %v", err)
		imp.Rollback()
	} else if err := imp.Commit(); err != nil {
		t.Errorf("import commit: %v", err)
	}
	for _, gen := range []func() ([]string, error){
		func() ([]string, error) { return c.TruncateSQL(pref) },
		func() ([]string, error) {
			return c.RenameObjectSQL(driver.ObjectRef{Database: "shop", Name: "it_people", Kind: "table"}, "it_people2")
		},
		func() ([]string, error) {
			return c.DropObjectSQL(driver.ObjectRef{Database: "shop", Name: "it_people2", Kind: "table"}, false)
		},
		func() ([]string, error) { return c.CreateDatabaseSQL("it_db", nil) },
		func() ([]string, error) { return c.DropDatabaseSQL("it_db", false) },
	} {
		st, err := gen()
		if err != nil {
			t.Errorf("ddl: %v", err)
			continue
		}
		tidbRun(t, sess, st...)
	}

	if plan, err := c.Explain(ctx, driver.Scope{Database: "shop"}, "SELECT * FROM orders WHERE customer_id = 7 ORDER BY placed_at", false); err != nil || plan.Root == nil {
		t.Errorf("explain: %+v, %v", plan, err)
	} else {
		t.Logf("plan root %q, %d children", plan.Root.Operation, len(plan.Root.Children))
	}
	if plan, err := c.Explain(ctx, driver.Scope{Database: "shop"}, "SELECT COUNT(*) FROM orders", true); err != nil || plan.Root == nil {
		t.Errorf("explain analyze: %+v, %v", plan, err)
	}
	for name, f := range map[string]func() (*driver.Result, error){
		"processes": func() (*driver.Result, error) { return c.Processes(ctx) },
		"variables": func() (*driver.Result, error) { return c.Variables(ctx, "variables") },
		"status":    func() (*driver.Result, error) { return c.Variables(ctx, "status") },
		"users":     func() (*driver.Result, error) { return c.Users(ctx) },
	} {
		if r, err := f(); err != nil || len(r.Rows) == 0 {
			t.Errorf("%s: %v, %v", name, r, err)
		}
	}
}
