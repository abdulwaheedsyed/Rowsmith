package sqlite

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"rowsmith/internal/driver"
	"rowsmith/internal/driver/sqlbase"
)

// sink records what a session streams.
type sink struct {
	stmts []driver.StatementInfo
	cols  [][]driver.ResultColumn
	rows  [][]any
	sums  []driver.ResultSummary
	errs  []error
}

func (s *sink) BeginStatement(i driver.StatementInfo) error { s.stmts = append(s.stmts, i); return nil }
func (s *sink) Columns(c []driver.ResultColumn) error       { s.cols = append(s.cols, c); return nil }
func (s *sink) Rows(r [][]any) error                        { s.rows = append(s.rows, r...); return nil }
func (s *sink) EndResult(r driver.ResultSummary) error      { s.sums = append(s.sums, r); return nil }
func (s *sink) Notice(string, string) error                 { return nil }
func (s *sink) EndStatement(err error) error                { s.errs = append(s.errs, err); return nil }

const schema = `
CREATE TABLE customers (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	name TEXT NOT NULL,
	email VARCHAR(120) UNIQUE,
	balance NUMERIC DEFAULT 0,
	joined DATE,
	updated DATETIME,
	active BOOLEAN DEFAULT 1,
	avatar BLOB,
	CHECK (balance >= 0)
);
CREATE TABLE orders (
	id INTEGER PRIMARY KEY,
	customer_id INTEGER NOT NULL REFERENCES customers (id) ON DELETE CASCADE,
	total REAL,
	qty INT CONSTRAINT qty_positive CHECK (qty > 0),
	total_with_tax REAL GENERATED ALWAYS AS (total * 1.2) VIRTUAL,
	note TEXT
);
CREATE TABLE order_items (
	order_id INTEGER, line INTEGER, sku TEXT,
	PRIMARY KEY (order_id, line),
	FOREIGN KEY (order_id) REFERENCES Orders ON DELETE CASCADE
);
CREATE TABLE logs (message TEXT, level TEXT);
CREATE TABLE settings (key TEXT PRIMARY KEY, value) WITHOUT ROWID;
CREATE INDEX orders_by_customer ON orders (customer_id, total DESC);
CREATE INDEX customers_lower_name ON customers (lower(name)) WHERE active = 1;
CREATE VIEW big_orders AS SELECT o.id, c.name, o.total FROM orders o JOIN customers c ON c.id = o.customer_id WHERE o.total > 100;
CREATE TRIGGER orders_audit AFTER UPDATE OF total ON orders
BEGIN
	INSERT INTO logs (message, level)
	VALUES ('order ' || NEW.id || ' changed', CASE WHEN NEW.total > OLD.total THEN 'up' ELSE 'down' END);
END;
INSERT INTO customers (name, email, balance, joined, updated, active, avatar) VALUES
	('Ada', 'ada@example.com', 12.5, '2024-01-15', '2024-01-15T10:30:00+02:00', 1, x'00ff'),
	('Bob', NULL, 0, NULL, NULL, 0, NULL),
	('Cy', 'cy@example.com', 100, '2023-12-01', '2023-12-01 08:00:00', 1, NULL);
INSERT INTO orders (customer_id, total, qty, note) VALUES (1, 150.0, 2, 'first'), (1, 20, 1, NULL), (2, 99.5, 3, 'mixed');
INSERT INTO order_items VALUES (1, 1, 'A'), (1, 2, 'B'), (3, 1, 'C');
INSERT INTO logs VALUES ('boot', 'info'), ('disk low', 'warn');
INSERT INTO settings VALUES ('theme', 'dark'), ('retries', 3);
`

func open(t *testing.T, params map[string]any, readOnly bool) *conn {
	t.Helper()
	c, err := sqliteDriver{}.Open(context.Background(), driver.OpenParams{Params: params, ReadOnly: readOnly})
	if err != nil {
		t.Fatalf("open %v: %v", params, err)
	}
	t.Cleanup(func() { c.Close() })
	return c.(*conn)
}

func run(t *testing.T, s driver.Session, script string) *sink {
	t.Helper()
	out := &sink{}
	if err := s.Execute(context.Background(), script, driver.ExecOptions{}, out); err != nil {
		t.Fatalf("execute %q: %v", script, err)
	}
	return out
}

// queryError returns the single statement error of a run.
func queryError(t *testing.T, out *sink) *driver.QueryError {
	t.Helper()
	if len(out.errs) != 1 || out.errs[0] == nil {
		t.Fatalf("want one failed statement, got %v", out.errs)
	}
	var qe *driver.QueryError
	if !errors.As(out.errs[0], &qe) {
		return &driver.QueryError{Message: out.errs[0].Error()}
	}
	return qe
}

func columnNamed(t *testing.T, tb *driver.Table, name string) driver.Column {
	t.Helper()
	for _, c := range tb.Columns {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("%s has no column %s", tb.Ref.Name, name)
	return driver.Column{}
}

func TestDriver(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	t.Setenv("ROWSMITH_SQLITE_DIR", dir)

	if _, err := (sqliteDriver{}).Open(ctx, driver.OpenParams{Params: map[string]any{"file": "shop.db"}}); err == nil {
		t.Fatal("opened a missing file without create")
	}
	if _, err := (sqliteDriver{}).Open(ctx, driver.OpenParams{Params: map[string]any{"file": "../shop.db", "create": true}}); err == nil {
		t.Fatal("opened a file outside the SQLite directory")
	}
	c := open(t, map[string]any{"file": "shop.db", "create": true}, false)
	if _, err := os.Stat(filepath.Join(dir, "shop.db")); err != nil {
		t.Fatal(err)
	}

	sess, err := c.NewSession(ctx, driver.Scope{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	out := run(t, sess, schema)
	if len(out.stmts) != 14 {
		t.Fatalf("schema split into %d statements", len(out.stmts))
	}
	for i, err := range out.errs {
		if err != nil {
			t.Fatalf("statement %d (%s): %v", i, out.stmts[i].SQL, err)
		}
	}
	if !strings.HasSuffix(out.stmts[8].SQL, "END") || out.stmts[8].Line != 31 {
		t.Errorf("trigger statement = line %d %q", out.stmts[8].Line, out.stmts[8].SQL)
	}

	t.Run("server", func(t *testing.T) {
		info, err := c.Server(ctx)
		if err != nil || info.Product != "SQLite" || info.Version == "" || info.Database != "shop.db" || info.Extras["Journal mode"] != "delete" {
			t.Fatalf("server = %+v, %v", info, err)
		}
	})

	t.Run("objects", func(t *testing.T) {
		objs, err := c.Objects(ctx, driver.Scope{})
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]driver.Object{}
		for _, o := range objs {
			got[o.Name] = o
			if strings.HasPrefix(o.Name, "sqlite_") || o.Rows != nil {
				t.Errorf("unexpected object %+v", o)
			}
		}
		want := map[string]string{"customers": "table", "orders": "table", "order_items": "table", "logs": "table", "settings": "table",
			"big_orders": "view", "orders_by_customer": "index", "customers_lower_name": "index", "orders_audit": "trigger"}
		if len(got) != len(want) {
			t.Errorf("objects = %v", objs)
		}
		for name, kind := range want {
			if got[name].Kind != kind {
				t.Errorf("%s: kind %q, want %q", name, got[name].Kind, kind)
			}
		}
		if x := got["orders_audit"].Extra; x != "AFTER UPDATE ON orders" {
			t.Errorf("trigger extra = %q", x)
		}
	})

	t.Run("describe", func(t *testing.T) {
		cu, err := c.Describe(ctx, driver.ObjectRef{Name: "CUSTOMERS"})
		if err != nil {
			t.Fatal(err)
		}
		if cu.Ref.Name != "customers" || cu.RowKeyKind != "primary" || !reflect.DeepEqual(cu.PrimaryKey, []string{"id"}) || !cu.Editable {
			t.Errorf("customers key: %+v %v %v", cu.Ref, cu.RowKeyKind, cu.PrimaryKey)
		}
		if id := columnNamed(t, cu, "id"); !id.AutoIncrement || !id.PrimaryKey || id.Kind != driver.KindInt {
			t.Errorf("id = %+v", id)
		}
		kinds := map[string]driver.ValueKind{"name": driver.KindText, "email": driver.KindString, "balance": driver.KindDecimal,
			"joined": driver.KindDate, "updated": driver.KindDateTime, "active": driver.KindBool, "avatar": driver.KindBinary}
		for n, k := range kinds {
			if got := columnNamed(t, cu, n); got.Kind != k {
				t.Errorf("%s kind = %s, want %s", n, got.Kind, k)
			}
		}
		if n := columnNamed(t, cu, "name"); n.Nullable {
			t.Error("name should be NOT NULL")
		}
		if b := columnNamed(t, cu, "balance"); b.Default == nil || *b.Default != "0" || b.BaseType != "numeric" {
			t.Errorf("balance = %+v", b)
		}
		if len(cu.Checks) != 1 || cu.Checks[0].Expression != "balance >= 0" {
			t.Errorf("checks = %+v", cu.Checks)
		}
		var lower, unique *driver.Index
		for i, ix := range cu.Indexes {
			switch {
			case ix.Name == "customers_lower_name":
				lower = &cu.Indexes[i]
			case ix.Unique:
				unique = &cu.Indexes[i]
			}
		}
		if lower == nil || !reflect.DeepEqual(lower.Columns, []string{"lower(name)"}) || lower.Where != "active = 1" || lower.Unique {
			t.Errorf("expression index = %+v", lower)
		}
		if unique == nil || !reflect.DeepEqual(unique.Columns, []string{"email"}) || unique.Primary {
			t.Errorf("unique index = %+v", unique)
		}
		if len(cu.Referenced) != 1 || cu.Referenced[0].Table.Name != "orders" || !reflect.DeepEqual(cu.Referenced[0].Columns, []string{"customer_id"}) {
			t.Errorf("referenced = %+v", cu.Referenced)
		}
		if !strings.HasPrefix(cu.DDL, "CREATE TABLE customers") || !strings.Contains(cu.DDL, ";\n\nCREATE INDEX customers_lower_name") {
			t.Errorf("ddl = %s", cu.DDL)
		}

		or, err := c.Describe(ctx, driver.ObjectRef{Name: "orders"})
		if err != nil {
			t.Fatal(err)
		}
		fk := or.ForeignKeys
		if len(fk) != 1 || fk[0].RefTable.Name != "customers" || !reflect.DeepEqual(fk[0].RefColumns, []string{"id"}) || fk[0].OnDelete != "CASCADE" {
			t.Errorf("orders fks = %+v", fk)
		}
		if g := columnNamed(t, or, "total_with_tax"); g.Generated != "total * 1.2" {
			t.Errorf("generated = %+v", g)
		}
		if len(or.Checks) != 1 || or.Checks[0].Name != "qty_positive" {
			t.Errorf("orders checks = %+v", or.Checks)
		}
		if len(or.Triggers) != 1 || or.Triggers[0].Name != "orders_audit" || or.Triggers[0].Timing != "AFTER" || or.Triggers[0].Event != "UPDATE" {
			t.Errorf("triggers = %+v", or.Triggers)
		}
		if ix := or.Indexes; len(ix) != 1 || !reflect.DeepEqual(ix[0].Columns, []string{"customer_id", "total"}) || !reflect.DeepEqual(ix[0].Desc, []bool{false, true}) {
			t.Errorf("orders indexes = %+v", ix)
		}

		oi, err := c.Describe(ctx, driver.ObjectRef{Name: "order_items"})
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(oi.PrimaryKey, []string{"order_id", "line"}) || oi.RowKeyKind != "primary" {
			t.Errorf("composite key = %v %s", oi.PrimaryKey, oi.RowKeyKind)
		}
		if f := oi.ForeignKeys; len(f) != 1 || f[0].RefTable.Name != "orders" || !reflect.DeepEqual(f[0].RefColumns, []string{"id"}) {
			t.Errorf("implicit reference = %+v", f)
		}
		if id := columnNamed(t, oi, "order_id"); id.AutoIncrement {
			t.Error("a composite key column is not a rowid alias")
		}

		lg, err := c.Describe(ctx, driver.ObjectRef{Name: "logs"})
		if err != nil {
			t.Fatal(err)
		}
		if lg.RowKeyKind != "rowid" || !reflect.DeepEqual(lg.RowKey, []string{sqlbase.HiddenRowKey}) {
			t.Errorf("logs key = %s %v", lg.RowKeyKind, lg.RowKey)
		}

		st, err := c.Describe(ctx, driver.ObjectRef{Name: "settings"})
		if err != nil {
			t.Fatal(err)
		}
		if st.Options["without_rowid"] != "true" || st.RowKeyKind != "primary" || columnNamed(t, st, "value").Kind != driver.KindOther {
			t.Errorf("settings = %+v", st)
		}

		v, err := c.Describe(ctx, driver.ObjectRef{Name: "big_orders"})
		if err != nil {
			t.Fatal(err)
		}
		if v.Kind != "view" || v.Editable || len(v.Columns) != 3 || !strings.HasPrefix(v.Definition, "CREATE VIEW big_orders") {
			t.Errorf("view = %+v", v)
		}
		if _, err := c.Describe(ctx, driver.ObjectRef{Name: "nope"}); err == nil {
			t.Error("described a missing table")
		}
	})

	t.Run("definition", func(t *testing.T) {
		tests := map[driver.ObjectRef]string{
			{Name: "orders_audit", Kind: "trigger"}:     "CREATE TRIGGER orders_audit AFTER UPDATE OF total ON orders\nBEGIN",
			{Name: "orders_by_customer", Kind: "index"}: "CREATE INDEX orders_by_customer ON orders (customer_id, total DESC);",
			{Name: "orders", Kind: "table"}:             "CREATE TABLE orders (",
			{Name: "big_orders"}:                        "CREATE VIEW big_orders AS",
		}
		for ref, prefix := range tests {
			def, err := c.Definition(ctx, ref)
			if err != nil || !strings.HasPrefix(def, prefix) || !strings.HasSuffix(def, ";") {
				t.Errorf("definition %+v = %q, %v", ref, def, err)
			}
		}
		if def, _ := c.Definition(ctx, driver.ObjectRef{Name: "orders", Kind: "table"}); !strings.Contains(def, "CREATE INDEX orders_by_customer") || !strings.HasSuffix(def, "END;") {
			t.Errorf("table definition misses its index or trigger: %s", def)
		}
	})

	t.Run("catalog", func(t *testing.T) {
		// A view over a dropped table must not hide the rest of the catalog.
		run(t, sess, "CREATE TABLE gone (a); CREATE VIEW broken AS SELECT a FROM gone; DROP TABLE gone")
		defer run(t, sess, "DROP VIEW broken")
		cat, err := c.CatalogColumns(ctx, driver.Scope{})
		if err != nil {
			t.Fatal(err)
		}
		byName := map[string]driver.CatalogTable{}
		for _, ct := range cat {
			byName[ct.Name] = ct
		}
		if len(cat) != 7 || byName["big_orders"].Kind != "view" || len(byName["customers"].Columns) != 8 || !byName["customers"].Columns[0].PK {
			t.Errorf("catalog = %+v", cat)
		}
		if b, ok := byName["broken"]; !ok || len(b.Columns) != 0 {
			t.Errorf("broken view = %+v", b)
		}
		if f := byName["order_items"].FKs; len(f) != 1 || f[0].RefTable.Name != "orders" {
			t.Errorf("catalog fks = %+v", f)
		}
	})

	browse := func(t *testing.T, req driver.BrowseRequest) *driver.Result {
		t.Helper()
		res, err := c.Browse(ctx, req)
		if err != nil {
			t.Fatalf("browse %+v: %v", req, err)
		}
		return res
	}
	ref := func(name string) driver.ObjectRef { return driver.ObjectRef{Name: name, Kind: "table"} }

	t.Run("browse", func(t *testing.T) {
		res := browse(t, driver.BrowseRequest{Ref: ref("customers"), Columns: []string{"id", "name", "joined", "updated", "active", "avatar"}})
		if len(res.Rows) != 3 || res.Truncated {
			t.Fatalf("rows = %v", res.Rows)
		}
		ada := res.Rows[0]
		want := []any{int64(1), "Ada", "2024-01-15", "2024-01-15T10:30:00+02:00", true, map[string]any{"$bin": "AP8=", "size": 2}}
		if !reflect.DeepEqual(ada, want) {
			t.Errorf("ada = %#v", ada)
		}
		if col := res.Columns[3]; col.Type != "DATETIME" || col.Kind != driver.KindDateTime {
			t.Errorf("updated column = %+v", col)
		}

		page := browse(t, driver.BrowseRequest{Ref: ref("customers"), Columns: []string{"name"}, Sort: []driver.Sort{{Column: "name", Desc: true}}, Limit: 2})
		if len(page.Rows) != 2 || !page.Truncated || page.Rows[0][0] != "Cy" {
			t.Errorf("page 1 = %v truncated=%v", page.Rows, page.Truncated)
		}
		page = browse(t, driver.BrowseRequest{Ref: ref("customers"), Columns: []string{"name"}, Sort: []driver.Sort{{Column: "name", Desc: true}}, Limit: 2, Offset: 2})
		if len(page.Rows) != 1 || page.Truncated || page.Rows[0][0] != "Ada" {
			t.Errorf("page 2 = %v", page.Rows)
		}

		filtered := browse(t, driver.BrowseRequest{Ref: ref("customers"), Columns: []string{"name"}, Filters: []driver.Filter{
			{Column: "active", Op: "=", Value: "true"}, {Column: "email", Op: "endswith", Value: "@EXAMPLE.com"}, {Column: "balance", Op: ">", Value: 50.0},
		}})
		if len(filtered.Rows) != 1 || filtered.Rows[0][0] != "Cy" {
			t.Errorf("filtered = %v (%s)", filtered.Rows, filtered.SQL)
		}
		if res := browse(t, driver.BrowseRequest{Ref: ref("customers"), Search: "ADA"}); len(res.Rows) != 1 {
			t.Errorf("search = %v", res.Rows)
		}
		if res := browse(t, driver.BrowseRequest{Ref: ref("customers"), Search: "100%"}); len(res.Rows) != 0 {
			t.Errorf("search wildcard not escaped: %v", res.Rows)
		}
		if res := browse(t, driver.BrowseRequest{Ref: ref("orders"), Where: "note IS NULL OR note LIKE 'mix%'"}); len(res.Rows) != 2 {
			t.Errorf("where = %v", res.Rows)
		}
		if res := browse(t, driver.BrowseRequest{Ref: ref("big_orders")}); len(res.Rows) != 1 || res.Rows[0][1] != "Ada" {
			t.Errorf("view rows = %v", res.Rows)
		}
		if _, err := c.Browse(ctx, driver.BrowseRequest{Ref: ref("customers"), Filters: []driver.Filter{{Column: "name", Op: "regexp", Value: "^A"}}}); err == nil {
			t.Error("regexp filter should be unsupported")
		}
		for _, where := range []string{"1); DELETE FROM logs; SELECT (1", "1); PRAGMA temp_store_directory = '/tmp'; SELECT (1"} {
			if _, err := c.Browse(ctx, driver.BrowseRequest{Ref: ref("logs"), Where: where}); err == nil {
				t.Errorf("where %q was accepted", where)
			}
		}

		n, err := c.Count(ctx, driver.BrowseRequest{Ref: ref("customers"), Filters: []driver.Filter{{Column: "email", Op: "notnull"}}})
		if err != nil || n.Rows != 2 || !n.Exact {
			t.Errorf("count = %+v, %v", n, err)
		}
	})

	t.Run("edits", func(t *testing.T) {
		res, err := c.ApplyEdits(ctx, ref("customers"), []driver.RowEdit{
			{Op: "insert", Values: map[string]any{"name": "Dee", "balance": 5.0, "active": map[string]any{"$default": true}}},
			{Op: "update", Key: map[string]any{"id": 2.0}, Values: map[string]any{"email": "bob@example.com", "active": true}},
		})
		if err != nil || res.Applied != 2 {
			t.Fatalf("customers edits = %+v, %v", res, err)
		}
		dee := browse(t, driver.BrowseRequest{Ref: ref("customers"), Columns: []string{"active"}, Filters: []driver.Filter{{Column: "name", Op: "=", Value: "Dee"}}})
		if len(dee.Rows) != 1 || dee.Rows[0][0] != true {
			t.Errorf("inserted row = %v", dee.Rows)
		}

		if _, err := c.ApplyEdits(ctx, ref("orders"), []driver.RowEdit{{Op: "update", Key: map[string]any{"id": 1.0}, Values: map[string]any{"total": 175.0}}}); err != nil {
			t.Fatal(err)
		}
		logs := browse(t, driver.BrowseRequest{Ref: ref("logs")})
		if len(logs.Rows) != 3 || logs.Rows[2][0] != "order 1 changed" || logs.Rows[2][1] != "up" {
			t.Fatalf("trigger output = %v", logs.Rows)
		}

		// Rows of a table without a key are addressed by rowid.
		hidden := len(logs.Columns) - 1
		if logs.Columns[hidden].Name != sqlbase.HiddenRowKey {
			t.Fatalf("columns = %+v", logs.Columns)
		}
		_, err = c.ApplyEdits(ctx, ref("logs"), []driver.RowEdit{
			{Op: "update", Key: map[string]any{sqlbase.HiddenRowKey: logs.Rows[1][hidden]}, Values: map[string]any{"level": "error"}},
			{Op: "delete", Key: map[string]any{sqlbase.HiddenRowKey: logs.Rows[0][hidden]}},
		})
		if err != nil {
			t.Fatal(err)
		}
		logs = browse(t, driver.BrowseRequest{Ref: ref("logs"), Columns: []string{"message", "level"}})
		if len(logs.Rows) != 2 || !reflect.DeepEqual(logs.Rows[0][:2], []any{"disk low", "error"}) {
			t.Errorf("logs after rowid edits = %v", logs.Rows)
		}

		if _, err := c.ApplyEdits(ctx, ref("settings"), []driver.RowEdit{
			{Op: "update", Key: map[string]any{"key": "retries"}, Values: map[string]any{"value": 5.0}},
			{Op: "insert", Values: map[string]any{"key": "lang", "value": "en"}},
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := c.ApplyEdits(ctx, ref("settings"), []driver.RowEdit{{Op: "delete", Key: map[string]any{"key": "missing"}}}); err == nil {
			t.Error("deleting a missing row succeeded")
		}
		if _, err := c.ApplyEdits(ctx, ref("settings"), []driver.RowEdit{
			{Op: "update", Key: map[string]any{"key": "lang"}, Values: map[string]any{"value": map[string]any{"$expr": "'de'; DELETE FROM settings"}}},
		}); err == nil {
			t.Error("an expression carrying a second statement was applied")
		}

		// ON DELETE CASCADE needs foreign_keys(1): customer 1 takes orders 1-2 and their items along.
		if _, err := c.ApplyEdits(ctx, ref("customers"), []driver.RowEdit{{Op: "delete", Key: map[string]any{"id": 1.0}}}); err != nil {
			t.Fatal(err)
		}
		for table, want := range map[string]int64{"orders": 1, "order_items": 1} {
			if n, err := c.Count(ctx, driver.BrowseRequest{Ref: ref(table)}); err != nil || n.Rows != want {
				t.Errorf("%s after cascade = %+v, %v", table, n, err)
			}
		}
		if _, err := c.ApplyEdits(ctx, ref("orders"), []driver.RowEdit{{Op: "insert", Values: map[string]any{"customer_id": 99.0, "qty": 1.0}}}); err == nil {
			t.Error("foreign key violation was accepted")
		}
	})

	t.Run("session", func(t *testing.T) {
		qe := queryError(t, run(t, sess, "SELECT * FROM missing"))
		if qe.Message != "no such table: missing" || qe.Code != "1" {
			t.Errorf("error = %+v", qe)
		}
		if qe := queryError(t, run(t, sess, "INSERT INTO settings VALUES ('theme', 'x')")); !strings.HasPrefix(qe.Message, "UNIQUE constraint failed") || qe.Code != "1555" {
			t.Errorf("constraint error = %+v", qe)
		}

		run(t, sess, "BEGIN")
		if !sess.InTransaction() {
			t.Fatal("BEGIN did not open a transaction")
		}
		out := run(t, sess, "UPDATE settings SET value = 'light' WHERE key = 'theme'; SELECT value FROM settings WHERE key = 'theme'")
		if len(out.rows) != 1 || out.rows[0][0] != "light" || *out.sums[0].RowsAffected != 1 {
			t.Fatalf("in transaction = %v", out.rows)
		}
		outside := browse(t, driver.BrowseRequest{Ref: ref("settings"), Filters: []driver.Filter{{Column: "key", Op: "=", Value: "theme"}}})
		if outside.Rows[0][1] != "dark" {
			t.Errorf("uncommitted change visible outside the session: %v", outside.Rows)
		}
		run(t, sess, "ROLLBACK")
		if sess.InTransaction() {
			t.Error("ROLLBACK did not end the transaction")
		}
		if out := run(t, sess, "SELECT value FROM settings WHERE key = 'theme'"); out.rows[0][0] != "dark" {
			t.Errorf("rollback kept %v", out.rows)
		}

		out = run(t, sess, "SELECT id, joined, updated FROM customers WHERE id = 3")
		if want := []any{int64(3), "2023-12-01", "2023-12-01 08:00:00"}; !reflect.DeepEqual(out.rows[0], want) {
			t.Errorf("console dates = %#v", out.rows[0])
		}

	})

	t.Run("attach", func(t *testing.T) {
		other := filepath.Join(dir, "other.db")
		// The limit comes from the connector: pooled connections and the
		// first statement of a console have not been through any hook yet.
		if _, err := c.db.ExecContext(ctx, "ATTACH ? AS other", other); err == nil || !strings.Contains(err.Error(), "too many attached databases") {
			t.Errorf("ATTACH on a pooled connection: %v", err)
		}
		for _, stmt := range []string{"ATTACH '" + other + "' AS other", "VACUUM INTO '" + other + "'"} {
			fresh, err := c.NewSession(ctx, driver.Scope{})
			if err != nil {
				t.Fatal(err)
			}
			if qe := queryError(t, run(t, fresh, stmt)); !strings.Contains(qe.Message, "disabled") {
				t.Errorf("%s: %+v", stmt, qe)
			}
			fresh.Close()
		}
		if out := run(t, sess, "VACUUM"); out.errs[0] != nil {
			t.Errorf("VACUUM: %v", out.errs[0])
		}
		if qe := queryError(t, run(t, sess, "ATTACH '"+other+"' AS other")); !strings.Contains(qe.Message, "disabled") {
			t.Errorf("ATTACH after VACUUM: %+v", qe)
		}
		if _, err := os.Stat(other); err == nil {
			t.Error("a file outside the database was created")
		}
		if qe := queryError(t, run(t, sess, "PRAGMA temp_store_directory = '"+dir+"'")); !strings.Contains(qe.Message, "not allowed") {
			t.Errorf("temp_store_directory: %+v", qe)
		}
	})

	t.Run("explain", func(t *testing.T) {
		plan, err := c.Explain(ctx, driver.Scope{}, "SELECT * FROM orders WHERE customer_id = 2", false)
		if err != nil {
			t.Fatal(err)
		}
		if plan.Root.Operation != "Search" || plan.Root.Object != "orders" || !strings.Contains(plan.Raw, "USING INDEX orders_by_customer") {
			t.Errorf("plan = %+v\n%s", plan.Root, plan.Raw)
		}
		if _, err := c.Explain(ctx, driver.Scope{}, "SELECT 1", true); !errors.Is(err, driver.ErrNotSupported) {
			t.Errorf("analyze = %v", err)
		}
		if _, err := c.Explain(ctx, driver.Scope{}, "SELECT 1; DELETE FROM logs", false); err == nil {
			t.Error("explained a multi-statement script")
		}
	})

	t.Run("variables", func(t *testing.T) {
		vars, err := c.Variables(ctx, "variables")
		if err != nil {
			t.Fatal(err)
		}
		got := map[any]any{}
		for _, r := range vars.Rows {
			got[r[0]] = r[1]
		}
		if len(got) != 10 || got["foreign_keys"] != int64(1) || got["encoding"] != "UTF-8" || got["sqlite_version"] == "" {
			t.Errorf("variables = %v", got)
		}
		status, err := c.Variables(ctx, "status")
		if err != nil || len(status.Rows) == 0 || status.Columns[0].Name != "option" {
			t.Errorf("status = %+v, %v", status, err)
		}
	})

	t.Run("read-only", func(t *testing.T) {
		for _, ro := range []struct {
			params   map[string]any
			readOnly bool
		}{{map[string]any{"file": "shop.db", "readOnly": true}, false}, {map[string]any{"file": "shop.db"}, true}} {
			rc := open(t, ro.params, ro.readOnly)
			if _, err := rc.ApplyEdits(ctx, ref("logs"), []driver.RowEdit{{Op: "delete", Key: map[string]any{sqlbase.HiddenRowKey: 1}}}); !errors.Is(err, driver.ErrReadOnly) {
				t.Errorf("edit on read-only = %v", err)
			}
			rs, err := rc.NewSession(ctx, driver.Scope{})
			if err != nil {
				t.Fatal(err)
			}
			for _, stmt := range []string{"DELETE FROM logs", "PRAGMA query_only = 0; DELETE FROM logs", "CREATE TABLE x (a)"} {
				out := run(t, rs, stmt)
				if failed := out.errs[len(out.errs)-1]; failed == nil || !strings.Contains(failed.Error(), "readonly") {
					t.Errorf("%s on read-only: %v", stmt, out.errs)
				}
			}
			if out := run(t, rs, "SELECT COUNT(*) FROM logs"); out.errs[0] != nil || out.rows[0][0] != int64(2) {
				t.Errorf("read on read-only = %v %v", out.rows, out.errs)
			}
			rs.Close()
			if info, _ := rc.Server(ctx); info.Extras["Session"] != "read-only" {
				t.Errorf("server = %+v", info)
			}
		}
		if _, err := (sqliteDriver{}).Open(ctx, driver.OpenParams{Params: map[string]any{"file": "new.db", "create": true, "readOnly": true}}); err == nil {
			t.Error("read-only open created a file")
		}
	})
}

func TestMemory(t *testing.T) {
	ctx := context.Background()
	t.Setenv("ROWSMITH_SQLITE_DIR", t.TempDir())
	c := open(t, map[string]any{"file": ":memory:"}, false)
	sess, err := c.NewSession(ctx, driver.Scope{})
	if err != nil {
		t.Fatal(err)
	}
	if out := run(t, sess, "CREATE TABLE notes (body TEXT); INSERT INTO notes VALUES ('kept')"); out.errs[0] != nil || out.errs[1] != nil {
		t.Fatal(out.errs)
	}
	// Closing a console discards its connection; the database must survive it.
	sess.Close()
	res, err := c.Browse(ctx, driver.BrowseRequest{Ref: driver.ObjectRef{Name: "notes"}})
	if err != nil || len(res.Rows) != 1 || res.Rows[0][0] != "kept" {
		t.Fatalf("after session close = %v, %v", res, err)
	}
	if info, err := c.Server(ctx); err != nil || info.Database != ":memory:" {
		t.Errorf("server = %+v, %v", info, err)
	}

	other := open(t, map[string]any{"file": ":memory:"}, false)
	if objs, err := other.Objects(ctx, driver.Scope{}); err != nil || len(objs) != 0 {
		t.Errorf("a second pool shares the scratch database: %v, %v", objs, err)
	}
}
