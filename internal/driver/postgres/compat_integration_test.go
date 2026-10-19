package postgres

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"rowsmith/internal/driver"
)

// Integration tests for the PostgreSQL-compatible servers, against the dev
// stack's seeded shop database (dev/seed/pgwire.sql). They create and drop
// their own tables, prefixed it_.
//
//	ROWSMITH_TEST_COCKROACH_HOST, _PORT (26257), _USER (rowsmith), _PASSWORD   over TLS
//	ROWSMITH_TEST_YUGABYTE_HOST, _PORT (5433), _USER (yugabyte), _PASSWORD

type compatTarget struct {
	drv          pgDriver
	prefix, user string
	tls          string
	product      string
}

var compatTargets = []compatTarget{
	{cockroachDriver, "ROWSMITH_TEST_COCKROACH_", "rowsmith", "require", "CockroachDB"},
	{yugabyteDriver, "ROWSMITH_TEST_YUGABYTE_", "yugabyte", "disable", "YugabyteDB"},
}

func eachCompat(t *testing.T, fn func(t *testing.T, c *conn, tg compatTarget)) {
	for _, tg := range compatTargets {
		t.Run(tg.drv.id, func(t *testing.T) {
			host := os.Getenv(tg.prefix + "HOST")
			if host == "" {
				t.Skip(tg.prefix + "HOST not set")
			}
			user := os.Getenv(tg.prefix + "USER")
			if user == "" {
				user = tg.user
			}
			port := os.Getenv(tg.prefix + "PORT")
			if port == "" {
				port = fmt.Sprint(tg.drv.port)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			cn, err := tg.drv.Open(ctx, driver.OpenParams{AppName: "rowsmith-test",
				Params:  map[string]any{"host": host, "port": port, "user": user, "database": "shop", "tls": tg.tls},
				Secrets: map[string]string{"password": os.Getenv(tg.prefix + "PASSWORD")}})
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			t.Cleanup(func() { cn.Close() })
			fn(t, cn.(*conn), tg)
		})
	}
}

type compatSink struct {
	stmts []driver.StatementInfo
	errs  []error
	cols  [][]driver.ResultColumn
	rows  [][][]any
	notes []string
}

func (s *compatSink) BeginStatement(i driver.StatementInfo) error {
	s.stmts = append(s.stmts, i)
	return nil
}
func (s *compatSink) Columns(c []driver.ResultColumn) error {
	s.cols = append(s.cols, c)
	s.rows = append(s.rows, nil)
	return nil
}
func (s *compatSink) Rows(r [][]any) error {
	s.rows[len(s.rows)-1] = append(s.rows[len(s.rows)-1], r...)
	return nil
}
func (s *compatSink) EndResult(driver.ResultSummary) error { return nil }
func (s *compatSink) Notice(_, text string) error          { s.notes = append(s.notes, text); return nil }
func (s *compatSink) EndStatement(err error) error         { s.errs = append(s.errs, err); return nil }

// run executes a script in a session and reports each failed statement.
func run(t *testing.T, sess driver.Session, script string) *compatSink {
	t.Helper()
	sink := &compatSink{}
	if err := sess.Execute(context.Background(), script, driver.ExecOptions{StopOnError: true, MaxRows: 1000}, sink); err != nil {
		t.Errorf("execute: %v", err)
	}
	for i, err := range sink.errs {
		if err != nil {
			t.Errorf("%s: %v", sink.stmts[i].SQL, err)
		}
	}
	return sink
}

func TestIntegrationCompatCatalog(t *testing.T) {
	eachCompat(t, func(t *testing.T, c *conn, tg compatTarget) {
		ctx := context.Background()
		srv, err := c.Server(ctx)
		if err != nil || srv.Product != tg.product || srv.Version == "" || strings.Contains(srv.Version, " ") {
			t.Errorf("server = %+v, %v", srv, err)
		}
		dbs, err := c.Databases(ctx)
		if err != nil || !slices.ContainsFunc(dbs, func(d driver.Database) bool { return d.Name == "shop" }) {
			t.Errorf("databases = %+v, %v", dbs, err)
		}
		schemas, err := c.Schemas(ctx, "shop")
		names := []string{}
		for _, s := range schemas {
			names = append(names, s.Name)
		}
		if err != nil || !slices.Contains(names, "public") || !slices.Contains(names, "reporting") {
			t.Errorf("schemas = %v, %v", names, err)
		}
		objs, err := c.Objects(ctx, driver.Scope{Database: "shop", Schema: "public"})
		if err != nil {
			t.Fatalf("objects: %v", err)
		}
		kinds := map[string]string{}
		for _, o := range objs {
			kinds[o.Name] = o.Kind
		}
		for name, kind := range map[string]string{"customers": "table", "orders": "table", "audit_events": "table", "customer_revenue": "view",
			"daily_sales": "materialized_view", "invoice_numbers": "sequence", "order_count": "function", "tier": "type"} {
			if kinds[name] != kind {
				t.Errorf("object %s: kind %q, want %q (all: %v)", name, kinds[name], kind, kinds)
			}
		}

		tb, err := c.Describe(ctx, driver.ObjectRef{Database: "shop", Schema: "public", Name: "products"})
		if err != nil {
			t.Fatalf("describe products: %v", err)
		}
		cols := map[string]driver.Column{}
		for _, col := range tb.Columns {
			cols[col.Name] = col
		}
		if len(tb.Columns) != 11 || !cols["id"].PrimaryKey || cols["margin"].Generated == "" || cols["price"].Kind != driver.KindDecimal ||
			cols["thumbnail"].Kind != driver.KindBinary || len(tb.ForeignKeys) != 1 || len(tb.Checks) == 0 || !tb.Editable {
			t.Errorf("products = %d columns %+v; fks %+v; checks %+v", len(tb.Columns), tb.Columns, tb.ForeignKeys, tb.Checks)
		}
		var partial bool
		for _, ix := range tb.Indexes {
			if ix.Name == "idx_products_low_stock" && ix.Where != "" {
				partial = true
			}
		}
		if !partial {
			t.Errorf("products indexes = %+v", tb.Indexes)
		}
		tb, err = c.Describe(ctx, driver.ObjectRef{Database: "shop", Schema: "public", Name: "customers"})
		if err != nil {
			t.Fatalf("describe customers: %v", err)
		}
		cols = map[string]driver.Column{}
		for _, col := range tb.Columns {
			cols[col.Name] = col
		}
		if !cols["id"].AutoIncrement || cols["tier"].Kind != driver.KindEnum || len(cols["tier"].Enum) != 3 || cols["tier"].Comment == "" ||
			cols["preferences"].Kind != driver.KindJSON || cols["tags"].Kind != driver.KindArray || tb.Comment == "" {
			t.Errorf("customers columns = %+v comment %q", tb.Columns, tb.Comment)
		}
		if tb.DDL == "" {
			t.Errorf("customers has no DDL")
		}
		tb, err = c.Describe(ctx, driver.ObjectRef{Database: "shop", Schema: "public", Name: "orders"})
		if err != nil || len(tb.ForeignKeys) != 1 || tb.ForeignKeys[0].OnDelete != "CASCADE" {
			t.Errorf("orders fks = %+v, %v", tb, err)
		}
		tb, err = c.Describe(ctx, driver.ObjectRef{Database: "shop", Schema: "public", Name: "categories"})
		if err != nil || len(tb.Referenced) < 2 {
			t.Errorf("categories referenced by = %+v, %v", tb, err)
		}
		tb, err = c.Describe(ctx, driver.ObjectRef{Database: "shop", Schema: "public", Name: "audit_events"})
		if err != nil || !tb.Editable || len(tb.RowKey) == 0 {
			t.Errorf("audit_events (no primary key) = %+v, %v", tb, err)
		}
		for _, ref := range []driver.ObjectRef{{Name: "customer_revenue", Kind: "view"}, {Name: "daily_sales", Kind: "materialized_view"}, {Name: "order_count", Kind: "function"}} {
			ref.Database, ref.Schema = "shop", "public"
			if def, err := c.Definition(ctx, ref); err != nil || def == "" {
				t.Errorf("definition of %s: %q, %v", ref.Name, def, err)
			}
		}
		if _, err := c.Describe(ctx, driver.ObjectRef{Database: "shop", Schema: "public", Name: "customer_revenue"}); err != nil {
			t.Errorf("describe view: %v", err)
		}
		cat, err := c.CatalogColumns(ctx, driver.Scope{Database: "shop", Schema: "public"})
		if err != nil || len(cat) < 6 {
			t.Errorf("catalog = %d tables, %v", len(cat), err)
		}
	})
}

func TestIntegrationCompatData(t *testing.T) {
	eachCompat(t, func(t *testing.T, c *conn, tg compatTarget) {
		ctx := context.Background()
		ref := driver.ObjectRef{Database: "shop", Schema: "public", Name: "orders"}
		res, err := c.Browse(ctx, driver.BrowseRequest{Ref: ref, Limit: 5,
			Filters: []driver.Filter{{Column: "status", Op: "=", Value: "paid"}, {Column: "total", Op: ">", Value: "100"}},
			Sort:    []driver.Sort{{Column: "placed_at", Desc: true}}})
		if err != nil || len(res.Rows) != 5 {
			t.Fatalf("browse orders: %+v, %v", res, err)
		}
		n, err := c.Count(ctx, driver.BrowseRequest{Ref: ref, Filters: []driver.Filter{{Column: "status", Op: "=", Value: "paid"}}})
		if err != nil || n.Rows != 1200 {
			t.Errorf("count paid = %+v, %v", n, err)
		}
		if _, err := c.Browse(ctx, driver.BrowseRequest{Ref: driver.ObjectRef{Database: "shop", Schema: "public", Name: "customers"}, Search: "Okafor", Limit: 3}); err != nil {
			t.Errorf("search: %v", err)
		}
		res, err = c.Browse(ctx, driver.BrowseRequest{Ref: driver.ObjectRef{Database: "shop", Schema: "public", Name: "customers"}, Limit: 2})
		if err != nil {
			t.Fatalf("browse customers: %v", err)
		}
		t.Logf("customer row: %v", res.Rows[0])

		sess, err := c.NewSession(ctx, driver.Scope{Database: "shop", Schema: "public"})
		if err != nil {
			t.Fatal(err)
		}
		defer sess.Close()
		run(t, sess, "DROP TABLE IF EXISTS it_people")
		def := driver.TableDef{Ref: driver.ObjectRef{Database: "shop", Schema: "public", Name: "it_people"}, PrimaryKey: []string{"id"},
			Comment: "people for tests",
			Columns: []driver.ColumnDef{
				{Column: driver.Column{Name: "id", Type: "bigint", AutoIncrement: true}},
				{Column: driver.Column{Name: "name", Type: "varchar(80)", Comment: "full name"}},
				{Column: driver.Column{Name: "score", Type: "numeric(8,2)", Nullable: true}},
				{Column: driver.Column{Name: "meta", Type: "jsonb", Nullable: true}},
			},
			Indexes: []driver.Index{{Name: "it_people_name", Columns: []string{"name"}, Unique: true}}}
		stmts, err := c.CreateTableSQL(def)
		if err != nil {
			t.Fatal(err)
		}
		run(t, sess, strings.Join(stmts, ";\n"))
		pref := def.Ref
		er, err := c.ApplyEdits(ctx, pref, []driver.RowEdit{
			{Op: "insert", Values: map[string]any{"name": "Ada", "score": "9.5", "meta": `{"a": 1}`}},
			{Op: "insert", Values: map[string]any{"name": "Grace"}},
		})
		if err != nil {
			t.Fatalf("insert: %v", err)
		}
		t.Logf("inserted %+v", er)
		res, err = c.Browse(ctx, driver.BrowseRequest{Ref: pref, Limit: 10, Sort: []driver.Sort{{Column: "name"}}})
		if err != nil || len(res.Rows) != 2 {
			t.Fatalf("browse it_people: %+v, %v", res, err)
		}
		id := res.Rows[0][0]
		if _, err := c.ApplyEdits(ctx, pref, []driver.RowEdit{{Op: "update", Key: map[string]any{"id": id}, Values: map[string]any{"score": float64(10)}}}); err != nil {
			t.Errorf("update: %v", err)
		}
		if _, err := c.ApplyEdits(ctx, pref, []driver.RowEdit{{Op: "delete", Key: map[string]any{"id": res.Rows[1][0]}}}); err != nil {
			t.Errorf("delete: %v", err)
		}

		// Structure changes round-trip.
		from, err := c.Describe(ctx, pref)
		if err != nil {
			t.Fatal(err)
		}
		if from.Comment != "people for tests" {
			t.Errorf("table comment = %q", from.Comment)
		}
		to := tableDef(from)
		if again, err := c.AlterTableSQL(from, to); err != nil || len(again) != 0 {
			t.Errorf("unchanged table altered: %q, %v", again, err)
		}
		to.Columns = append(to.Columns, driver.ColumnDef{Column: driver.Column{Name: "email", Type: "text", Nullable: true}})
		for i := range to.Columns {
			if to.Columns[i].Name == "score" {
				to.Columns[i].Name = "points"
			}
		}
		to.Indexes = append(to.Indexes, driver.Index{Name: "it_people_email", Columns: []string{"email"}})
		stmts, err = c.AlterTableSQL(from, to)
		if err != nil {
			t.Fatal(err)
		}
		run(t, sess, strings.Join(stmts, ";\n"))
		after, err := c.Describe(ctx, pref)
		if err != nil || len(after.Columns) != 5 || !slices.ContainsFunc(after.Columns, func(c driver.Column) bool { return c.Name == "points" }) {
			t.Errorf("after alter: %+v, %v", after, err)
		}
		if again, err := c.AlterTableSQL(after, tableDef(after)); err != nil || len(again) != 0 {
			t.Errorf("altered table round-trip: %q, %v", again, err)
		}

		// The console: transactions, results, notices and errors.
		s := run(t, sess, "BEGIN;\nINSERT INTO it_people (name) VALUES ('Tx');\nSELECT count(*) FROM it_people")
		if !sess.InTransaction() {
			t.Errorf("BEGIN did not open a transaction")
		}
		if len(s.rows) == 0 || len(s.rows[len(s.rows)-1]) != 1 {
			t.Errorf("count rows = %v", s.rows)
		}
		run(t, sess, "ROLLBACK")
		if sess.InTransaction() {
			t.Errorf("still in a transaction after ROLLBACK")
		}
		bad := &compatSink{}
		sess.Execute(ctx, "SELECT 1;\nSELECT nope FROM it_people", driver.ExecOptions{}, bad)
		if len(bad.errs) != 2 || bad.errs[1] == nil {
			t.Errorf("error statement = %v", bad.errs)
		} else {
			t.Logf("error: %v", bad.errs[1])
		}

		// Exports and imports.
		q, args, err := c.BrowseQuery(ctx, after, driver.BrowseRequest{})
		if err != nil || q == "" {
			t.Errorf("browse query: %q %v %v", q, args, err)
		}
		imp, err := c.BeginImport(ctx, after, []string{"name", "points"}, false)
		if err != nil {
			t.Errorf("begin import: %v", err)
		} else {
			if err := imp.Insert(ctx, [][]any{{"Imp One", "1.5"}, {"Imp Two", nil}}); err != nil {
				t.Errorf("import insert: %v", err)
				imp.Rollback()
			} else if err := imp.Commit(); err != nil {
				t.Errorf("import commit: %v", err)
			}
		}
		if n, err := c.Count(ctx, driver.BrowseRequest{Ref: pref}); err != nil || n.Rows != 3 {
			t.Errorf("rows after import = %+v, %v", n, err)
		}

		// Rename, empty and drop.
		for _, gen := range []func() ([]string, error){
			func() ([]string, error) { return c.TruncateSQL(pref) },
			func() ([]string, error) { return c.RenameObjectSQL(after.Ref, "it_people2") },
			func() ([]string, error) {
				return c.DropObjectSQL(driver.ObjectRef{Database: "shop", Schema: "public", Name: "it_people2", Kind: "table"}, false)
			},
		} {
			st, err := gen()
			if err != nil {
				t.Errorf("ddl: %v", err)
				continue
			}
			run(t, sess, strings.Join(st, ";\n"))
		}
	})
}

func TestIntegrationCompatAdmin(t *testing.T) {
	eachCompat(t, func(t *testing.T, c *conn, tg compatTarget) {
		ctx := context.Background()
		plan, err := c.Explain(ctx, driver.Scope{Database: "shop", Schema: "public"}, "SELECT * FROM orders WHERE customer_id = 7 ORDER BY placed_at", false)
		if err != nil || plan.Root == nil {
			t.Errorf("explain: %+v, %v", plan, err)
		} else {
			t.Logf("plan: %s (%d children) format %s", plan.Root.Operation, len(plan.Root.Children), plan.Format)
		}
		if plan, err := c.Explain(ctx, driver.Scope{Database: "shop", Schema: "public"}, "SELECT count(*) FROM orders", true); err != nil || plan.Root == nil {
			t.Errorf("explain analyze: %+v, %v", plan, err)
		}
		for _, f := range []func() (*driver.Result, error){
			func() (*driver.Result, error) { return c.Processes(ctx) },
			func() (*driver.Result, error) { return c.Variables(ctx, "variables") },
			func() (*driver.Result, error) { return c.Variables(ctx, "status") },
			func() (*driver.Result, error) { return c.Users(ctx) },
		} {
			if r, err := f(); err != nil || len(r.Rows) == 0 {
				t.Errorf("admin result: %v rows, %v", r, err)
			}
		}
		// Databases, schemas, roles and grants, generated and run as the UI does.
		sess, err := c.NewSession(ctx, driver.Scope{Database: "shop"})
		if err != nil {
			t.Fatal(err)
		}
		defer sess.Close()
		gen := func(st []string, err error) {
			t.Helper()
			if err != nil {
				t.Errorf("generate: %v", err)
				return
			}
			run(t, sess, strings.Join(st, ";\n"))
		}
		gen(c.CreateDatabaseSQL("it_db", nil))
		gen(c.CreateSchemaSQL("shop", "it_schema"))
		gen(c.DropSchemaSQL("shop", "it_schema", true))
		gen(c.DropDatabaseSQL("it_db", false))
		if dbs, _ := c.Databases(ctx); slices.ContainsFunc(dbs, func(d driver.Database) bool { return d.Name == "it_db" }) {
			t.Errorf("it_db was not dropped")
		}
		run(t, sess, "DROP ROLE IF EXISTS it_reader")
		gen(c.AccountSQL(driver.AccountOp{Op: "create", Name: "it_reader", Kind: "user", Password: "Quiet-River-Stone-81"}))
		gen(c.GrantSQL([]driver.GrantChange{{Grant: driver.Grant{Principal: "it_reader", Level: driver.LevelTable, Database: "shop", Schema: "public", Object: "orders", Privilege: "SELECT"}}}))
		if gs, err := c.ObjectAccess(ctx, driver.ObjectRef{Database: "shop", Schema: "public", Name: "orders"}); err != nil ||
			!slices.ContainsFunc(gs, func(g driver.Grant) bool { return g.Principal == "it_reader" && g.Privilege == "SELECT" }) {
			t.Errorf("grant not visible: %+v, %v", gs, err)
		}
		gen(c.GrantSQL([]driver.GrantChange{{Grant: driver.Grant{Principal: "it_reader", Level: driver.LevelTable, Database: "shop", Schema: "public", Object: "orders", Privilege: "SELECT"}, Revoke: true}}))
		gen(c.AccountSQL(driver.AccountOp{Op: "drop", Name: "it_reader"}))

		ps, err := c.Principals(ctx)
		if err != nil || len(ps) == 0 {
			t.Errorf("principals: %v, %v", ps, err)
		}
		if _, err := c.Access(ctx, tg.user, driver.Scope{Database: "shop", Schema: "public"}); err != nil {
			t.Errorf("access: %v", err)
		}
		if _, err := c.ObjectAccess(ctx, driver.ObjectRef{Database: "shop", Schema: "public", Name: "orders"}); err != nil {
			t.Errorf("object access: %v", err)
		}
	})
}

// tableDef turns a described table back into the definition the structure
// editor would submit unchanged.
func tableDef(t *driver.Table) driver.TableDef {
	d := driver.TableDef{Ref: t.Ref, PrimaryKey: t.PrimaryKey, Comment: t.Comment, Options: t.Options, Indexes: t.Indexes, ForeignKeys: t.ForeignKeys, Checks: t.Checks}
	for _, c := range t.Columns {
		d.Columns = append(d.Columns, driver.ColumnDef{Column: c, OriginalName: c.Name})
	}
	var ix []driver.Index
	for _, i := range t.Indexes {
		if !i.Primary {
			ix = append(ix, i)
		}
	}
	d.Indexes = ix
	return d
}
