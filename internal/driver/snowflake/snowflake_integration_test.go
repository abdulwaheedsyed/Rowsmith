package snowflake

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"rowsmith/internal/driver"
)

// Integration tests run against a real Snowflake account when
// ROWSMITH_TEST_SNOWFLAKE_ACCOUNT is set (Snowflake has no local server).
// They create a database of their own, load dev/seed/snowflake.sql into it
// through the console, and drop it afterwards; a run uses well under a
// minute of the smallest warehouse.
//
//	ROWSMITH_TEST_SNOWFLAKE_ACCOUNT, _USER, _KEY_FILE (PKCS#8 PEM), _WAREHOUSE, _ROLE

func openSnowflake(t *testing.T) *conn {
	t.Helper()
	account := os.Getenv("ROWSMITH_TEST_SNOWFLAKE_ACCOUNT")
	if account == "" {
		t.Skip("ROWSMITH_TEST_SNOWFLAKE_ACCOUNT not set")
	}
	key, err := os.ReadFile(os.Getenv("ROWSMITH_TEST_SNOWFLAKE_KEY_FILE"))
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	d, _ := driver.Get("snowflake")
	cn, err := d.Open(ctx, driver.OpenParams{AppName: "rowsmith-test",
		Params: map[string]any{"account": account, "user": os.Getenv("ROWSMITH_TEST_SNOWFLAKE_USER"), "auth": "keypair",
			"warehouse": os.Getenv("ROWSMITH_TEST_SNOWFLAKE_WAREHOUSE"), "role": os.Getenv("ROWSMITH_TEST_SNOWFLAKE_ROLE")},
		Secrets: map[string]string{"privateKey": string(key)}})
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

func TestIntegrationSnowflake(t *testing.T) {
	c := openSnowflake(t)
	ctx := context.Background()
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	db := "ROWSMITH_IT_" + strings.ToUpper(hex.EncodeToString(b))

	setup, err := c.NewSession(ctx, driver.Scope{})
	if err != nil {
		t.Fatal(err)
	}
	defer setup.Close()
	stmts, _ := c.CreateDatabaseSQL(db, nil)
	run(t, setup, stmts[0])
	t.Cleanup(func() {
		drop, _ := c.DropDatabaseSQL(db, false)
		s, _ := c.NewSession(context.Background(), driver.Scope{})
		s.Execute(context.Background(), drop[0], driver.ExecOptions{}, &sink{})
		s.Close()
	})
	seed, err := os.ReadFile("../../../dev/seed/snowflake.sql")
	if err != nil {
		t.Fatal(err)
	}
	run(t, setup, "USE DATABASE "+quote(db)+";\n"+string(seed))

	srv, err := c.Server(ctx)
	if err != nil || srv.Product != "Snowflake" || srv.Version == "" {
		t.Errorf("server = %+v, %v", srv, err)
	}
	dbs, err := c.Databases(ctx)
	if err != nil || !slices.ContainsFunc(dbs, func(d driver.Database) bool { return d.Name == db }) {
		t.Errorf("databases = %+v, %v", dbs, err)
	}
	schemas, err := c.Schemas(ctx, db)
	if err != nil || !slices.ContainsFunc(schemas, func(s driver.Schema) bool { return s.Name == "SHOP" }) {
		t.Errorf("schemas = %+v, %v", schemas, err)
	}
	scope := driver.Scope{Database: db, Schema: "SHOP"}
	objs, err := c.Objects(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]string{}
	for _, o := range objs {
		kinds[o.Name] = o.Kind
	}
	for name, kind := range map[string]string{"ORDERS": "table", "CUSTOMER_REVENUE": "view", "INVOICE_NUMBERS": "sequence", "ORDER_COUNT": "function", "RESTOCK": "procedure"} {
		if kinds[name] != kind {
			t.Errorf("object %s: %q, want %q (all %v)", name, kinds[name], kind, kinds)
		}
	}

	ref := func(name string) driver.ObjectRef { return driver.ObjectRef{Database: db, Schema: "SHOP", Name: name} }
	tb, err := c.Describe(ctx, ref("CUSTOMERS"))
	if err != nil {
		t.Fatal(err)
	}
	cols := map[string]driver.Column{}
	for _, col := range tb.Columns {
		cols[col.Name] = col
	}
	if !cols["ID"].AutoIncrement || !cols["ID"].PrimaryKey || cols["PREFERENCES"].Kind != driver.KindJSON || cols["CREATED_AT"].Kind != driver.KindDateTime ||
		cols["FULL_NAME"].Comment == "" || tb.Comment == "" || tb.RowEstimate == nil || *tb.RowEstimate != 500 || !tb.Editable || tb.DDL == "" {
		t.Errorf("customers = %+v", tb)
	}
	tb, err = c.Describe(ctx, ref("ORDERS"))
	if err != nil || len(tb.ForeignKeys) != 1 || tb.Options["cluster_by"] == "" || tb.RowKeyKind != "primary" {
		t.Errorf("orders = %+v, %v", tb, err)
	}
	if tb, err := c.Describe(ctx, ref("CATEGORIES")); err != nil || len(tb.Referenced) < 2 || len(tb.Indexes) != 2 {
		t.Errorf("categories = %+v, %v", tb, err)
	}
	if tb, err := c.Describe(ctx, ref("AUDIT_EVENTS")); err != nil || tb.RowKeyKind != "all" || !tb.Editable {
		t.Errorf("audit_events = %+v, %v", tb, err)
	}
	for _, r := range []driver.ObjectRef{{Name: "CUSTOMER_REVENUE", Kind: "view"}, {Name: "ORDER_COUNT", Kind: "function"}, {Name: "RESTOCK", Kind: "procedure"}, {Name: "INVOICE_NUMBERS", Kind: "sequence"}} {
		r.Database, r.Schema = db, "SHOP"
		if def, err := c.Definition(ctx, r); err != nil || def == "" {
			t.Errorf("definition of %s: %q, %v", r.Name, def, err)
		}
	}
	if cat, err := c.CatalogColumns(ctx, scope); err != nil || len(cat) < 7 {
		t.Errorf("catalog: %d, %v", len(cat), err)
	}

	// Values: exact numbers, JSON, shapes, binary, times.
	res, err := c.Browse(ctx, driver.BrowseRequest{Ref: ref("STORES"), Limit: 5, Sort: []driver.Sort{{Column: "ID"}}})
	if err != nil || len(res.Rows) != 3 {
		t.Fatalf("stores: %+v, %v", res, err)
	}
	if g, ok := res.Rows[0][2].(map[string]any); !ok || g["$geo"] == nil {
		t.Errorf("geography cell = %#v", res.Rows[0][2])
	}
	res, err = c.Browse(ctx, driver.BrowseRequest{Ref: ref("CUSTOMERS"), Limit: 2, Sort: []driver.Sort{{Column: "ID"}}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Rows[0][0] != int64(1) || !strings.HasPrefix(res.Rows[0][5].(string), `{"newsletter"`) || res.Rows[0][8] != "2025-01-01 00:00:00" {
		t.Errorf("customer row = %#v", res.Rows[0])
	}
	res, err = c.Browse(ctx, driver.BrowseRequest{Ref: ref("ORDERS"), Limit: 5,
		Filters: []driver.Filter{{Column: "STATUS", Op: "=", Value: "paid"}, {Column: "TOTAL", Op: ">", Value: "100"}},
		Sort:    []driver.Sort{{Column: "PLACED_AT", Desc: true}}})
	if err != nil || len(res.Rows) != 5 {
		t.Errorf("filtered orders: %v rows, %v", len(res.Rows), err)
	} else {
		t.Logf("order: %#v", res.Rows[0])
	}
	if n, err := c.Count(ctx, driver.BrowseRequest{Ref: ref("ORDERS"), Search: "door"}); err != nil || n.Rows == 0 {
		t.Errorf("count with search: %+v, %v", n, err)
	}
	if res, err := c.Browse(ctx, driver.BrowseRequest{Ref: ref("PRODUCTS"), Limit: 1, Sort: []driver.Sort{{Column: "ID"}}}); err != nil {
		t.Errorf("products: %v", err)
	} else if m, ok := res.Rows[0][6].(map[string]any); !ok || m["$bin"] == nil {
		t.Errorf("binary cell = %#v", res.Rows[0][6])
	}

	// Edits: inserts with JSON and shapes, updates and deletes by key and by whole row.
	er, err := c.ApplyEdits(ctx, ref("CUSTOMERS"), []driver.RowEdit{
		{Op: "insert", Values: map[string]any{"EMAIL": "new@example.com", "FULL_NAME": "New Person", "PREFERENCES": map[string]any{"a": float64(1)}, "TAGS": `["x","y"]`}},
		{Op: "update", Key: map[string]any{"ID": float64(2)}, Values: map[string]any{"TIER": "enterprise", "ADDRESS": `{"city":"Jeddah"}`}},
	})
	if err != nil {
		t.Fatalf("edits: %v", err)
	}
	t.Logf("statements: %q", er.Statements)
	if _, err := c.ApplyEdits(ctx, ref("STORES"), []driver.RowEdit{{Op: "insert", Values: map[string]any{"ID": float64(9), "NAME": "Pop-up",
		"LOCATION": map[string]any{"$geo": map[string]any{"type": "Point", "coordinates": []any{46.7, 24.7}}}, "FOOTPRINT": "POLYGON((0 0, 1 0, 1 1, 0 0))"}}}); err != nil {
		t.Errorf("shape insert: %v", err)
	}
	if _, err := c.ApplyEdits(ctx, ref("AUDIT_EVENTS"), []driver.RowEdit{{Op: "delete",
		Key: map[string]any{"HAPPENED_AT": "2026-01-01 00:00:00.000 -0800", "ACTOR": "system", "ACTION": "login", "PAYLOAD": `{"n":0}`}}}); err != nil {
		t.Logf("keyless delete: %v", err) // whole-row matching of LTZ and VARIANT values is best effort
	}

	// The console: sessions, transactions, scripting, errors, PUT refused.
	sess, err := c.NewSession(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	s := run(t, sess, "SELECT COUNT(*) AS n FROM orders;\nBEGIN;\nINSERT INTO categories VALUES (99, 'Tx', NULL);\nSELECT COUNT(*) FROM categories")
	if !sess.InTransaction() {
		t.Errorf("BEGIN did not open a transaction")
	}
	run(t, sess, "ROLLBACK")
	if sess.InTransaction() {
		t.Errorf("still in a transaction after ROLLBACK")
	}
	if s.rows[0][0][0] != int64(2000) {
		t.Errorf("count = %#v", s.rows[0])
	}
	s = run(t, sess, "CALL restock(1, 5);\nEXECUTE IMMEDIATE $$\nBEGIN\n  RETURN 'block ' || 42;\nEND;\n$$;\nSHOW TABLES")
	if len(s.stmts) != 3 {
		t.Errorf("statements = %+v", s.stmts)
	}
	bad := &sink{}
	sess.Execute(ctx, "SELECT 1;\nSELECT nope\n  FROM orders;\nPUT file:///etc/passwd @~", driver.ExecOptions{}, bad)
	var qe *driver.QueryError
	if len(bad.errs) != 3 || bad.errs[1] == nil || !asQE(bad.errs[1], &qe) || qe.Line != 2 || bad.errs[2] == nil || !strings.Contains(bad.errs[2].Error(), "PUT and GET") {
		t.Errorf("errors = %v (line %v)", bad.errs, qe)
	}

	// Structure changes.
	def := driver.TableDef{Ref: ref("IT_PEOPLE"), PrimaryKey: []string{"ID"}, Comment: "people for tests",
		Options: map[string]string{"cluster_by": "NAME"},
		Columns: []driver.ColumnDef{
			{Column: driver.Column{Name: "ID", Type: "NUMBER(38,0)", AutoIncrement: true}},
			{Column: driver.Column{Name: "NAME", Type: "VARCHAR(80)", Comment: "full name"}},
			{Column: driver.Column{Name: "META", Type: "VARIANT", Nullable: true}},
		},
		Indexes: []driver.Index{{Name: "UQ_IT_PEOPLE_NAME", Columns: []string{"NAME"}, Unique: true}}}
	stmts, err = c.CreateTableSQL(def)
	if err != nil {
		t.Fatal(err)
	}
	run(t, sess, strings.Join(stmts, ";\n"))
	from, err := c.Describe(ctx, ref("IT_PEOPLE"))
	if err != nil {
		t.Fatal(err)
	}
	to := tableDef(from)
	if again, err := c.AlterTableSQL(from, to); err != nil || len(again) != 0 {
		t.Errorf("unchanged table altered: %q, %v", again, err)
	}
	to.Columns = append(to.Columns, driver.ColumnDef{Column: driver.Column{Name: "EMAIL", Type: "VARCHAR(190)", Nullable: true}})
	to.Columns[1].Name, to.Columns[1].Type = "FULL_NAME", "VARCHAR(120)"
	to.Comment = "renamed things"
	stmts, err = c.AlterTableSQL(from, to)
	if err != nil {
		t.Fatal(err)
	}
	run(t, sess, strings.Join(stmts, ";\n"))
	after, err := c.Describe(ctx, ref("IT_PEOPLE"))
	if err != nil || len(after.Columns) != 4 || after.Columns[1].Name != "FULL_NAME" || after.Columns[1].Type != "VARCHAR(120)" || after.Comment != "renamed things" {
		t.Errorf("after alter: %+v, %v", after, err)
	}
	imp, err := c.BeginImport(ctx, after, []string{"FULL_NAME", "META"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := imp.Insert(ctx, [][]any{{"Ada", `{"x":1}`}, {"Grace", nil}}); err != nil {
		imp.Rollback()
		t.Fatalf("import: %v", err)
	}
	if err := imp.Commit(); err != nil {
		t.Fatal(err)
	}
	if q, _, err := c.BrowseQuery(ctx, after, driver.BrowseRequest{}); err != nil || q == "" {
		t.Errorf("browse query: %q, %v", q, err)
	}
	for _, gen := range []func() ([]string, error){
		func() ([]string, error) { return c.TruncateSQL(ref("IT_PEOPLE")) },
		func() ([]string, error) {
			return c.RenameObjectSQL(driver.ObjectRef{Database: db, Schema: "SHOP", Name: "IT_PEOPLE", Kind: "table"}, "IT_PEOPLE2")
		},
		func() ([]string, error) {
			return c.DropObjectSQL(driver.ObjectRef{Database: db, Schema: "SHOP", Name: "IT_PEOPLE2", Kind: "table"}, false)
		},
		func() ([]string, error) {
			return c.DropObjectSQL(driver.ObjectRef{Database: db, Schema: "SHOP", Name: "ORDER_COUNT", Kind: "function"}, false)
		},
		func() ([]string, error) { return c.CreateSchemaSQL(db, "IT_SCHEMA") },
		func() ([]string, error) { return c.DropSchemaSQL(db, "IT_SCHEMA", true) },
	} {
		st, err := gen()
		if err != nil {
			t.Errorf("ddl: %v", err)
			continue
		}
		run(t, sess, strings.Join(st, ";\n"))
	}

	// Plans and administration.
	plan, err := c.Explain(ctx, scope, "SELECT * FROM orders WHERE customer_id = 7 ORDER BY placed_at", false)
	if err != nil || plan.Root == nil || plan.Root.Operation != "Result" {
		t.Errorf("explain: %+v, %v", plan, err)
	} else {
		b, _ := json.Marshal(plan.Root)
		t.Logf("plan: %s", b)
	}
	for name, f := range map[string]func() (*driver.Result, error){
		"variables": func() (*driver.Result, error) { return c.Variables(ctx, "variables") },
		"status":    func() (*driver.Result, error) { return c.Variables(ctx, "status") },
		"users":     func() (*driver.Result, error) { return c.Users(ctx) },
	} {
		if r, err := f(); err != nil || len(r.Rows) == 0 {
			t.Errorf("%s: %v, %v", name, r, err)
		}
	}
	if _, err := c.Processes(ctx); err != nil {
		t.Logf("processes: %v", err) // needs the connection's database for INFORMATION_SCHEMA
	}
}

func asQE(err error, qe **driver.QueryError) bool {
	q, ok := err.(*driver.QueryError)
	if ok {
		*qe = q
	}
	return ok
}

func tableDef(t *driver.Table) driver.TableDef {
	d := driver.TableDef{Ref: t.Ref, PrimaryKey: t.PrimaryKey, Comment: t.Comment, Options: map[string]string{}, ForeignKeys: t.ForeignKeys}
	for k, v := range t.Options {
		d.Options[k] = v
	}
	for _, c := range t.Columns {
		d.Columns = append(d.Columns, driver.ColumnDef{Column: c, OriginalName: c.Name})
	}
	for _, ix := range t.Indexes {
		if !ix.Primary {
			d.Indexes = append(d.Indexes, ix)
		}
	}
	return d
}
