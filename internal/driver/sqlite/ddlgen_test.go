package sqlite

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"rowsmith/internal/driver"
)

func strp(s string) *string { return &s }

// library is the fixture most generator tests change.
const library = `
CREATE TABLE authors (
	id INTEGER PRIMARY KEY,
	name TEXT NOT NULL COLLATE NOCASE,
	email TEXT UNIQUE
);
CREATE TABLE books (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	author_id INTEGER REFERENCES authors (id) ON DELETE CASCADE DEFERRABLE INITIALLY DEFERRED,
	title TEXT NOT NULL,
	pages INT CHECK (pages > 0),
	price NUMERIC DEFAULT 0,
	isbn TEXT,
	slug TEXT GENERATED ALWAYS AS (lower(title)) STORED,
	CONSTRAINT price_sane CHECK (price < 1000)
);
CREATE INDEX books_title ON books (title COLLATE NOCASE DESC);
CREATE UNIQUE INDEX books_isbn ON books (isbn) WHERE isbn IS NOT NULL;
CREATE TABLE reviews (id INTEGER PRIMARY KEY, book_id INTEGER NOT NULL REFERENCES books (id), stars INT);
CREATE TABLE audit (msg TEXT);
CREATE TRIGGER books_audit AFTER UPDATE OF title ON books
BEGIN
	INSERT INTO audit (msg) VALUES ('retitled ' || OLD.title || ' as ' || NEW.title);
END;
CREATE VIEW book_titles AS SELECT b.id, b.title, a.name FROM books b JOIN authors a ON a.id = b.author_id;
INSERT INTO authors (name, email) VALUES ('Ann', 'ann@example.com'), ('Ben', NULL);
INSERT INTO books (author_id, title, pages, price, isbn) VALUES
	(1, 'Alpha', 100, 10, '111'), (1, 'Beta', 200, 20.5, NULL), (2, 'Gamma', 300, 30, '333'), (2, 'Delta', 400, 40, NULL);
DELETE FROM books WHERE id = 4;
INSERT INTO reviews (book_id, stars) VALUES (1, 5), (3, 3);
INSERT INTO audit VALUES ('boot');
`

// oddities covers what Describe reports beyond plain columns.
const oddities = `
CREATE TABLE "odd ""names""" ("key col" TEXT PRIMARY KEY, [value] ANY, ` + "`when`" + ` INT DEFAULT (-1), CHECK ("key col" <> '')) STRICT;
CREATE INDEX odd_when ON "odd ""names""" ("when") WHERE "when" > 0;
CREATE TABLE wr (a TEXT, b INT NOT NULL, c TEXT COLLATE RTRIM, PRIMARY KEY (a, b), UNIQUE (c)) WITHOUT ROWID;
CREATE TABLE gen (a INT, b INT AS (a * 2), c TEXT AS (a || 'x') STORED NOT NULL);
CREATE TABLE defaults (a DEFAULT CURRENT_TIMESTAMP, b DEFAULT (datetime('now')), c DEFAULT 'it''s', d DEFAULT x'00ff', e DEFAULT -2.5e3, f DEFAULT NULL, g, h DEFAULT (-1));
CREATE TABLE tree (id INTEGER PRIMARY KEY, parent INTEGER REFERENCES tree ON DELETE SET NULL, other INTEGER, FOREIGN KEY (other) REFERENCES tree (id) ON UPDATE CASCADE);
CREATE TABLE plain (a, b);
INSERT INTO "odd ""names""" VALUES ('k', 1, 5), ('l', 'text', -3);
INSERT INTO wr VALUES ('x', 1, 'c1 '), ('y', 2, NULL);
INSERT INTO gen (a) VALUES (1), (2);
INSERT INTO defaults (g) VALUES (1);
INSERT INTO tree (id, parent, other) VALUES (1, NULL, NULL), (2, 1, 1), (3, 2, 1);
INSERT INTO plain VALUES (1, 'one'), (2, 'two'), (3, 'three');
DELETE FROM plain WHERE a = 2;
`

type ddlDB struct {
	t    *testing.T
	c    *conn
	sess driver.Session
}

func newDDLDB(t *testing.T, setup ...string) *ddlDB {
	t.Helper()
	t.Setenv("ROWSMITH_SQLITE_DIR", t.TempDir())
	c := open(t, map[string]any{"file": "ddl.db", "create": true}, false)
	sess, err := c.NewSession(context.Background(), driver.Scope{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sess.Close() })
	d := &ddlDB{t: t, c: c, sess: sess}
	for _, s := range setup {
		d.mustRun(s)
	}
	return d
}

// run executes a script in the console session, stopping at the first error
// as the API does by default.
func (d *ddlDB) run(script string) *sink {
	d.t.Helper()
	out := &sink{}
	if err := d.sess.Execute(context.Background(), script, driver.ExecOptions{StopOnError: true}, out); err != nil {
		d.t.Fatal(err)
	}
	return out
}

func (d *ddlDB) mustRun(script string) *sink {
	d.t.Helper()
	out := d.run(script)
	for i, err := range out.errs {
		if err != nil {
			d.t.Fatalf("statement %d (%s): %v", i, out.stmts[i].SQL, err)
		}
	}
	return out
}

// failure runs a script that must fail and returns the error.
func (d *ddlDB) failure(stmts []string) error {
	d.t.Helper()
	out := d.run(strings.Join(stmts, ";\n"))
	for _, err := range out.errs {
		if err != nil {
			return err
		}
	}
	d.t.Fatalf("script succeeded:\n%s", strings.Join(stmts, ";\n"))
	return nil
}

// apply runs generated statements as the server hands them to the console:
// joined into one script.
func (d *ddlDB) apply(stmts []string) {
	d.t.Helper()
	out := d.mustRun(strings.Join(stmts, ";\n"))
	if len(out.stmts) != len(stmts) {
		d.t.Fatalf("script split into %d statements, want %d", len(out.stmts), len(stmts))
	}
	for i, st := range out.stmts {
		if st.SQL != stmts[i] {
			d.t.Fatalf("statement %d split as %q, want %q", i, st.SQL, stmts[i])
		}
	}
	if len(out.rows) != 0 {
		d.t.Fatalf("foreign_key_check reported %v", out.rows)
	}
	if d.sess.InTransaction() {
		d.t.Fatal("script left a transaction open")
	}
	if fk := d.rows("PRAGMA foreign_keys"); fk[0][0] != int64(1) {
		d.t.Fatalf("foreign_keys = %v after the script", fk[0][0])
	}
}

func (d *ddlDB) rows(query string) [][]any {
	d.t.Helper()
	return d.mustRun(query).rows
}

func (d *ddlDB) describe(name string) *driver.Table {
	d.t.Helper()
	tb, err := d.c.Describe(context.Background(), driver.ObjectRef{Name: name, Kind: "table"})
	if err != nil {
		d.t.Fatal(err)
	}
	return tb
}

// editorDef turns a described table into the definition the structure editor
// starts from, through JSON like the browser.
func editorDef(t *testing.T, tb *driver.Table) driver.TableDef {
	t.Helper()
	b, err := json.Marshal(tb)
	if err != nil {
		t.Fatal(err)
	}
	var seen driver.Table
	if err := json.Unmarshal(b, &seen); err != nil {
		t.Fatal(err)
	}
	def := driver.TableDef{Ref: seen.Ref, Indexes: seen.Indexes, ForeignKeys: seen.ForeignKeys, Checks: seen.Checks,
		PrimaryKey: seen.PrimaryKey, Comment: seen.Comment, Options: seen.Options}
	for _, col := range seen.Columns {
		def.Columns = append(def.Columns, driver.ColumnDef{Column: col, OriginalName: col.Name})
	}
	if b, err = json.Marshal(def); err != nil {
		t.Fatal(err)
	}
	var sent driver.TableDef
	if err := json.Unmarshal(b, &sent); err != nil {
		t.Fatal(err)
	}
	return sent
}

// alter generates statements the way the API does: the editor's definition
// is diffed against a fresh Describe.
func (d *ddlDB) alter(name string, edit func(*driver.TableDef)) ([]string, error) {
	d.t.Helper()
	def := editorDef(d.t, d.describe(name))
	edit(&def)
	return d.c.AlterTableSQL(d.describe(name), def)
}

func (d *ddlDB) mustAlter(name string, edit func(*driver.TableDef)) []string {
	d.t.Helper()
	stmts, err := d.alter(name, edit)
	if err != nil {
		d.t.Fatal(err)
	}
	if len(stmts) == 0 {
		d.t.Fatal("no statements generated")
	}
	return stmts
}

// unchanged checks that an untouched definition generates nothing.
func (d *ddlDB) unchanged(name string) {
	d.t.Helper()
	if stmts, err := d.alter(name, func(*driver.TableDef) {}); err != nil || len(stmts) != 0 {
		d.t.Fatalf("unchanged %s: %v\n%s", name, err, strings.Join(stmts, ";\n"))
	}
}

func column(t *testing.T, def *driver.TableDef, name string) *driver.ColumnDef {
	t.Helper()
	for i := range def.Columns {
		if def.Columns[i].Name == name {
			return &def.Columns[i]
		}
	}
	t.Fatalf("no column %s", name)
	return nil
}

func dropColumn(def *driver.TableDef, name string) {
	def.Columns = slices.DeleteFunc(def.Columns, func(c driver.ColumnDef) bool { return c.Name == name })
}

func dropIndex(def *driver.TableDef, match func(driver.Index) bool) {
	def.Indexes = slices.DeleteFunc(def.Indexes, match)
}

func columnNames(tb *driver.Table) []string {
	var out []string
	for _, c := range tb.Columns {
		out = append(out, c.Name)
	}
	return out
}

func rebuilds(stmts []string) bool {
	return len(stmts) > 2 && stmts[0] == "PRAGMA foreign_keys = OFF" && stmts[len(stmts)-1] == "PRAGMA foreign_keys = ON"
}

func TestDesign(t *testing.T) {
	info := sqliteDriver{}.Info()
	ds := info.Design
	if ds == nil || !ds.Columns || !ds.ReorderColumns || !ds.AutoIncrement || !ds.Collation || !ds.Generated || !ds.GeneratedVirtual ||
		!ds.Checks || !ds.PrimaryKey || !ds.ForeignKeys || !ds.Indexes || !ds.PartialIndexes || ds.ColumnComments || ds.TableComment || ds.Note == "" {
		t.Fatalf("design = %+v", ds)
	}
	if !reflect.DeepEqual(ds.FKActions, []string{"NO ACTION", "RESTRICT", "CASCADE", "SET NULL", "SET DEFAULT"}) {
		t.Errorf("fk actions = %v", ds.FKActions)
	}
	// Option keys are the ones Describe fills in.
	d := newDDLDB(t, oddities)
	for _, f := range ds.Options {
		if f.Type != driver.FieldBool {
			t.Errorf("option %s is %s", f.Key, f.Type)
		}
	}
	if keys := []string{ds.Options[0].Key, ds.Options[1].Key}; !reflect.DeepEqual(keys, []string{"without_rowid", "strict"}) {
		t.Errorf("option keys = %v", keys)
	}
	if o := d.describe("wr").Options; o["without_rowid"] != "true" {
		t.Errorf("wr options = %v", o)
	}
	if o := d.describe(`odd "names"`).Options; o["strict"] != "true" {
		t.Errorf("odd options = %v", o)
	}
}

func TestDescribeForDesign(t *testing.T) {
	d := newDDLDB(t, library, oddities)
	books := d.describe("books")
	if s := columnNamed(t, books, "slug"); s.Generated != "lower(title)" || !s.GeneratedStored {
		t.Errorf("slug = %+v", s)
	}
	if b := columnNamed(t, d.describe("gen"), "b"); b.Generated != "a * 2" || b.GeneratedStored {
		t.Errorf("gen.b = %+v", b)
	}
	if n := columnNamed(t, d.describe("authors"), "name"); n.Collation != "NOCASE" {
		t.Errorf("authors.name = %+v", n)
	}
	if c := columnNamed(t, d.describe("wr"), "c"); c.Collation != "RTRIM" {
		t.Errorf("wr.c = %+v", c)
	}
	def := parseTable(tableStatement(books.DDL))
	if def.autoincrement != "id" || !reflect.DeepEqual(def.deferred, []bool{true}) || !def.unnamed["check_1"] || def.unnamed["price_sane"] || def.onConflict {
		t.Errorf("books extras = %+v", def)
	}
	if def := parseTable("CREATE TABLE t (a REFERENCES p NOT DEFERRABLE, b UNIQUE ON CONFLICT IGNORE, FOREIGN KEY (a) REFERENCES p DEFERRABLE INITIALLY DEFERRED)"); !reflect.DeepEqual(def.deferred, []bool{false, true}) || !def.onConflict {
		t.Errorf("extras = %+v", def)
	}
}

func TestCreateTableSQL(t *testing.T) {
	d := newDDLDB(t, "CREATE TABLE customers (id INTEGER PRIMARY KEY, name TEXT)")
	def := driver.TableDef{
		Ref: driver.ObjectRef{Name: "orders"},
		Columns: []driver.ColumnDef{
			{Column: driver.Column{Name: "id", Type: "INTEGER", AutoIncrement: true}},
			{Column: driver.Column{Name: "customer_id", Type: "INTEGER"}},
			{Column: driver.Column{Name: "qty", Type: "INT", Default: strp("1")}},
			{Column: driver.Column{Name: "price", Type: "REAL", Nullable: true, Default: strp("0.0")}},
			{Column: driver.Column{Name: "total", Type: "REAL", Nullable: true, Generated: "qty * price", GeneratedStored: true}},
			{Column: driver.Column{Name: "code", Type: "TEXT", Collation: "NOCASE", Default: strp("lower(hex(randomblob(4)))")}},
			{Column: driver.Column{Name: "placed", Type: "DATETIME", Default: strp("CURRENT_TIMESTAMP")}},
			{Column: driver.Column{Name: "label", Type: "TEXT", Nullable: true, Generated: "upper(code)"}},
		},
		PrimaryKey: []string{"id"},
		ForeignKeys: []driver.ForeignKey{
			{Columns: []string{"customer_id"}, RefTable: driver.ObjectRef{Name: "customers"}, RefColumns: []string{"id"}, OnDelete: "CASCADE"},
		},
		Checks: []driver.Check{{Name: "qty_positive", Expression: "qty > 0"}},
		Indexes: []driver.Index{
			{Name: "orders_open", Columns: []string{"customer_id", "placed"}, Desc: []bool{false, true}, Where: "total IS NULL"},
			{Name: "orders_code", Columns: []string{"lower(code)"}, Unique: true},
		},
	}
	stmts, err := d.c.CreateTableSQL(def)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"BEGIN",
		`CREATE TABLE "orders" (
  "id" INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL,
  "customer_id" INTEGER NOT NULL,
  "qty" INT NOT NULL DEFAULT 1,
  "price" REAL DEFAULT 0.0,
  "total" REAL GENERATED ALWAYS AS (qty * price) STORED,
  "code" TEXT NOT NULL DEFAULT (lower(hex(randomblob(4)))) COLLATE NOCASE,
  "placed" DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  "label" TEXT GENERATED ALWAYS AS (upper(code)) VIRTUAL,
  FOREIGN KEY ("customer_id") REFERENCES "customers" ("id") ON DELETE CASCADE,
  CONSTRAINT "qty_positive" CHECK (qty > 0)
)`,
		`CREATE INDEX "orders_open" ON "orders" ("customer_id", "placed" DESC) WHERE total IS NULL`,
		`CREATE UNIQUE INDEX "orders_code" ON "orders" (lower(code))`,
		"COMMIT",
	}
	if !reflect.DeepEqual(stmts, want) {
		t.Fatalf("got\n%s\nwant\n%s", strings.Join(stmts, ";\n"), strings.Join(want, ";\n"))
	}
	d.apply(stmts)
	d.mustRun("INSERT INTO customers (name) VALUES ('Ann'); INSERT INTO orders (customer_id, price) VALUES (1, 2.5)")

	or := d.describe("orders")
	if id := columnNamed(t, or, "id"); !id.AutoIncrement || !reflect.DeepEqual(or.PrimaryKey, []string{"id"}) {
		t.Errorf("id = %+v, key %v", id, or.PrimaryKey)
	}
	if tot := columnNamed(t, or, "total"); tot.Generated != "qty * price" || !tot.GeneratedStored {
		t.Errorf("total = %+v", tot)
	}
	if code := columnNamed(t, or, "code"); code.Collation != "NOCASE" || deref(code.Default) != "lower(hex(randomblob(4)))" {
		t.Errorf("code = %+v", code)
	}
	if fk := or.ForeignKeys; len(fk) != 1 || fk[0].OnDelete != "CASCADE" || fk[0].RefTable.Name != "customers" {
		t.Errorf("fks = %+v", fk)
	}
	if got := d.rows("SELECT id, qty, total FROM orders"); !reflect.DeepEqual(got, [][]any{{int64(1), int64(1), 2.5}}) {
		t.Errorf("rows = %v", got)
	}
	d.unchanged("orders")

	// A single statement needs no transaction.
	stmts, err = d.c.CreateTableSQL(driver.TableDef{Ref: driver.ObjectRef{Name: "kv"},
		Columns:    []driver.ColumnDef{{Column: driver.Column{Name: "k", Type: "TEXT"}}, {Column: driver.Column{Name: "v", Nullable: true}}},
		PrimaryKey: []string{"k"}, Options: map[string]string{"without_rowid": "true", "strict": "false"}})
	if err != nil || len(stmts) != 1 || stmts[0] != "CREATE TABLE \"kv\" (\n  \"k\" TEXT NOT NULL,\n  \"v\",\n  PRIMARY KEY (\"k\")\n) WITHOUT ROWID" {
		t.Fatalf("kv = %q, %v", stmts, err)
	}
	d.apply(stmts)
	d.unchanged("kv")

	for _, bad := range []struct {
		def  driver.TableDef
		want string
	}{
		{driver.TableDef{Columns: []driver.ColumnDef{{Column: driver.Column{Name: "a"}}}}, "give the table a name"},
		{driver.TableDef{Ref: driver.ObjectRef{Name: "t"}}, "a table needs at least one column"},
		{driver.TableDef{Ref: driver.ObjectRef{Name: "t"}, Columns: []driver.ColumnDef{{Column: driver.Column{Name: " "}}}}, "every column needs a name"},
		{driver.TableDef{Ref: driver.ObjectRef{Name: "t"}, Columns: []driver.ColumnDef{{Column: driver.Column{Name: "a"}}, {Column: driver.Column{Name: "A"}}}}, "two columns are named A"},
		{driver.TableDef{Ref: driver.ObjectRef{Name: "t"}, Columns: []driver.ColumnDef{{Column: driver.Column{Name: "id", Type: "INT", AutoIncrement: true}}}, PrimaryKey: []string{"id"}},
			"auto-increment needs id to be the only primary key column, with the type INTEGER"},
		{driver.TableDef{Ref: driver.ObjectRef{Name: "t"}, Columns: []driver.ColumnDef{{Column: driver.Column{Name: "id", Type: "INTEGER", AutoIncrement: true}}}},
			"auto-increment needs id to be the only primary key column, with the type INTEGER"},
		{driver.TableDef{Ref: driver.ObjectRef{Name: "t"}, Columns: []driver.ColumnDef{{Column: driver.Column{Name: "a"}}}, Options: map[string]string{"without_rowid": "true"}},
			"a WITHOUT ROWID table needs a primary key"},
		{driver.TableDef{Ref: driver.ObjectRef{Name: "t"}, Columns: []driver.ColumnDef{{Column: driver.Column{Name: "a"}}}, PrimaryKey: []string{"b"}},
			"the primary key uses unknown column b"},
		{driver.TableDef{Ref: driver.ObjectRef{Name: "t"}, Columns: []driver.ColumnDef{{Column: driver.Column{Name: "a"}}}, Indexes: []driver.Index{{Name: "ix", Columns: []string{"b"}}}},
			"index ix uses unknown column b"},
		{driver.TableDef{Ref: driver.ObjectRef{Name: "t"}, Columns: []driver.ColumnDef{{Column: driver.Column{Name: "a"}}}, Indexes: []driver.Index{{Name: "sqlite_x", Columns: []string{"a"}}}},
			"index names starting with sqlite_ are reserved; rename sqlite_x"},
		{driver.TableDef{Ref: driver.ObjectRef{Name: "t"}, Columns: []driver.ColumnDef{{Column: driver.Column{Name: "a"}}},
			ForeignKeys: []driver.ForeignKey{{Columns: []string{"a"}, RefTable: driver.ObjectRef{Name: "p"}, RefColumns: []string{"x", "y"}}}},
			"the foreign key on a needs as many referenced columns as columns"},
	} {
		if _, err := d.c.CreateTableSQL(bad.def); err == nil || err.Error() != bad.want {
			t.Errorf("CreateTableSQL(%+v) = %v, want %q", bad.def, err, bad.want)
		}
	}
}

func TestAlterUnchanged(t *testing.T) {
	d := newDDLDB(t, schema, library, oddities)
	objs, err := d.c.Objects(context.Background(), driver.Scope{})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, o := range objs {
		if o.Kind == "table" {
			d.unchanged(o.Name)
			n++
		}
	}
	if n != 15 {
		t.Errorf("checked %d tables", n)
	}
}

// TestRebuildKeepsEverything rebuilds tables without changing them: Describe
// must report the same structure afterwards, and the rows must survive.
func TestRebuildKeepsEverything(t *testing.T) {
	d := newDDLDB(t, schema, library, oddities)
	for _, name := range []string{"customers", "orders", "order_items", "logs", "settings", "authors", "books", "reviews", "audit",
		`odd "names"`, "wr", "gen", "defaults", "tree", "plain"} {
		t.Run(name, func(t *testing.T) {
			d.t = t
			before := d.describe(name)
			query := "SELECT rowid, * FROM " + quote(name) + " ORDER BY 1"
			if before.Options["without_rowid"] != "" {
				query = "SELECT * FROM " + quote(name) + " ORDER BY 1"
			}
			data := d.rows(query)
			p, err := newPlan(before, editorDef(t, before))
			if err != nil {
				t.Fatal(err)
			}
			stmts, err := p.rebuild(nil)
			if err != nil {
				t.Fatal(err)
			}
			d.apply(stmts)
			after := d.describe(name)
			if !reflect.DeepEqual(after.Columns, before.Columns) || !reflect.DeepEqual(after.PrimaryKey, before.PrimaryKey) ||
				!reflect.DeepEqual(after.Checks, before.Checks) || !reflect.DeepEqual(after.ForeignKeys, before.ForeignKeys) ||
				!reflect.DeepEqual(after.Referenced, before.Referenced) || !reflect.DeepEqual(after.Options, before.Options) ||
				!reflect.DeepEqual(after.Triggers, before.Triggers) || after.RowKeyKind != before.RowKeyKind {
				t.Errorf("before %+v\nafter  %+v\n%s", before, after, after.DDL)
			}
			ixs := func(tb *driver.Table) []driver.Index {
				out := slices.Clone(tb.Indexes)
				for i := range out {
					if isAuto(out[i].Name) {
						out[i].Name = ""
					}
				}
				return out
			}
			if !reflect.DeepEqual(ixs(after), ixs(before)) {
				t.Errorf("indexes before %+v\nafter %+v", before.Indexes, after.Indexes)
			}
			if again := d.rows(query); !reflect.DeepEqual(again, data) {
				t.Errorf("rows before %v\nafter %v", data, again)
			}
			d.unchanged(name)
		})
	}
}

func TestAlterInPlace(t *testing.T) {
	t.Run("add columns", func(t *testing.T) {
		d := newDDLDB(t, library)
		stmts := d.mustAlter("books", func(def *driver.TableDef) {
			def.Columns = append(def.Columns,
				driver.ColumnDef{Column: driver.Column{Name: "subtitle", Type: "TEXT", Nullable: true}},
				driver.ColumnDef{Column: driver.Column{Name: "format", Type: "TEXT", Default: strp("'paper'"), Collation: "NOCASE"}},
				driver.ColumnDef{Column: driver.Column{Name: "weight", Type: "REAL", Nullable: true, Generated: "pages * 2.5"}})
		})
		want := []string{"BEGIN",
			`ALTER TABLE "books" ADD COLUMN "subtitle" TEXT`,
			`ALTER TABLE "books" ADD COLUMN "format" TEXT NOT NULL DEFAULT 'paper' COLLATE NOCASE`,
			`ALTER TABLE "books" ADD COLUMN "weight" REAL GENERATED ALWAYS AS (pages * 2.5) VIRTUAL`,
			"COMMIT"}
		if !reflect.DeepEqual(stmts, want) {
			t.Fatalf("got %q", stmts)
		}
		d.apply(stmts)
		if got := d.rows("SELECT format, weight FROM books WHERE id = 1"); !reflect.DeepEqual(got, [][]any{{"paper", 250.0}}) {
			t.Errorf("rows = %v", got)
		}
		d.unchanged("books")

		// ADD COLUMN cannot add these; the table is rebuilt instead.
		for _, col := range []driver.Column{
			{Name: "added", Type: "DATETIME", Default: strp("CURRENT_TIMESTAMP")},
			{Name: "code", Type: "TEXT", Default: strp("hex(randomblob(2))")},
			{Name: "required", Type: "TEXT"},
			{Name: "stored", Type: "INT", Nullable: true, Generated: "pages + 1", GeneratedStored: true},
		} {
			stmts := d.mustAlter("books", func(def *driver.TableDef) { def.Columns = append(def.Columns, driver.ColumnDef{Column: col}) })
			if !rebuilds(stmts) {
				t.Errorf("%s: %q", col.Name, stmts)
			}
		}
		// A new column placed before existing ones also needs a rebuild.
		stmts = d.mustAlter("audit", func(def *driver.TableDef) {
			def.Columns = append([]driver.ColumnDef{{Column: driver.Column{Name: "at", Type: "TEXT", Nullable: true}}}, def.Columns...)
		})
		if !rebuilds(stmts) {
			t.Errorf("new first column: %q", stmts)
		}
		d.apply(stmts)
		if got := columnNames(d.describe("audit")); !reflect.DeepEqual(got, []string{"at", "msg"}) {
			t.Errorf("audit columns = %v", got)
		}
	})

	t.Run("rename column", func(t *testing.T) {
		for _, updateRefs := range []bool{false, true} {
			d := newDDLDB(t, library)
			stmts := d.mustAlter("books", func(def *driver.TableDef) {
				column(t, def, "title").Name = "heading"
				if updateRefs { // an editor may carry the rename into keys
					for i := range def.Indexes {
						for k, c := range def.Indexes[i].Columns {
							if c == "title" {
								def.Indexes[i].Columns[k] = "heading"
							}
						}
					}
				}
			})
			if want := []string{`ALTER TABLE "books" RENAME COLUMN "title" TO "heading"`}; !reflect.DeepEqual(stmts, want) {
				t.Fatalf("got %q", stmts)
			}
			d.apply(stmts)
			d.mustRun("UPDATE books SET heading = 'Alef' WHERE id = 1")
			if got := d.rows("SELECT msg FROM audit ORDER BY rowid DESC LIMIT 1"); got[0][0] != "retitled Alpha as Alef" {
				t.Errorf("trigger after rename: %v", got)
			}
			if got := d.rows("SELECT * FROM book_titles WHERE id = 1"); !reflect.DeepEqual(got, [][]any{{int64(1), "Alef", "Ann"}}) {
				t.Errorf("view after rename: %v", got)
			}
			if got := d.rows("SELECT slug FROM books WHERE id = 1"); got[0][0] != "alef" {
				t.Errorf("generated column after rename: %v", got)
			}
			d.unchanged("books")
		}
	})

	t.Run("new items beside a rename", func(t *testing.T) {
		d := newDDLDB(t, library)
		stmts := d.mustAlter("books", func(def *driver.TableDef) {
			column(t, def, "title").Name = "heading"
			def.Checks = append(def.Checks, driver.Check{Name: "has_title", Expression: "length(title) > 0"})
			def.Indexes = append(def.Indexes, driver.Index{Name: "books_heading", Columns: []string{"title", "upper(heading)"}})
		})
		want := []string{"BEGIN",
			`ALTER TABLE "books" RENAME COLUMN "title" TO "heading"`,
			`ALTER TABLE "books" ADD CONSTRAINT "has_title" CHECK (length("heading") > 0)`,
			`CREATE INDEX "books_heading" ON "books" ("heading", upper(heading))`,
			"COMMIT"}
		if !reflect.DeepEqual(stmts, want) {
			t.Fatalf("got %q", stmts)
		}
		d.apply(stmts)
		d.unchanged("books")
	})

	t.Run("swap names", func(t *testing.T) {
		d := newDDLDB(t, oddities)
		stmts := d.mustAlter("plain", func(def *driver.TableDef) {
			def.Columns[0].Name, def.Columns[1].Name = "b", "a"
		})
		want := []string{"BEGIN",
			`ALTER TABLE "plain" RENAME COLUMN "a" TO "_rowsmith_tmp_1"`,
			`ALTER TABLE "plain" RENAME COLUMN "b" TO "a"`,
			`ALTER TABLE "plain" RENAME COLUMN "_rowsmith_tmp_1" TO "b"`,
			"COMMIT"}
		if !reflect.DeepEqual(stmts, want) {
			t.Fatalf("got %q", stmts)
		}
		d.apply(stmts)
		if got := d.rows("SELECT a, b FROM plain ORDER BY b"); !reflect.DeepEqual(got, [][]any{{"one", int64(1)}, {"three", int64(3)}}) {
			t.Errorf("rows = %v", got)
		}
	})

	t.Run("nullability", func(t *testing.T) {
		d := newDDLDB(t, library)
		stmts := d.mustAlter("books", func(def *driver.TableDef) {
			column(t, def, "pages").Nullable = false
			column(t, def, "title").Nullable = true
		})
		want := []string{"BEGIN", `ALTER TABLE "books" ALTER COLUMN "title" DROP NOT NULL`, `ALTER TABLE "books" ALTER COLUMN "pages" SET NOT NULL`, "COMMIT"}
		if !reflect.DeepEqual(stmts, want) {
			t.Fatalf("got %q", stmts)
		}
		d.apply(stmts)
		d.unchanged("books")
		// Existing NULLs make SET NOT NULL fail, and nothing is applied.
		stmts = d.mustAlter("books", func(def *driver.TableDef) {
			column(t, def, "isbn").Nullable = false
			column(t, def, "title").Nullable = false
		})
		if err := d.failure(stmts); !strings.Contains(err.Error(), "constraint failed") {
			t.Errorf("error = %v", err)
		}
		d.mustRun("ROLLBACK")
		if !columnNamed(t, d.describe("books"), "title").Nullable {
			t.Error("a failed script changed the table")
		}
	})

	t.Run("checks", func(t *testing.T) {
		d := newDDLDB(t, library)
		stmts := d.mustAlter("books", func(def *driver.TableDef) {
			def.Checks = slices.DeleteFunc(def.Checks, func(c driver.Check) bool { return c.Name == "price_sane" })
			def.Checks = append(def.Checks, driver.Check{Name: "title_set", Expression: "length(title) > 0"}, driver.Check{Expression: "pages < 10000"})
		})
		want := []string{"BEGIN",
			`ALTER TABLE "books" DROP CONSTRAINT "price_sane"`,
			`ALTER TABLE "books" ADD CONSTRAINT "title_set" CHECK (length(title) > 0)`,
			`ALTER TABLE "books" ADD CHECK (pages < 10000)`,
			"COMMIT"}
		if !reflect.DeepEqual(stmts, want) {
			t.Fatalf("got %q", stmts)
		}
		d.apply(stmts)
		got := d.describe("books").Checks
		if len(got) != 3 || got[0].Expression != "pages > 0" || got[1].Name != "title_set" || got[2].Expression != "pages < 10000" {
			t.Errorf("checks = %+v", got)
		}
		if out := d.run("INSERT INTO books (title, pages) VALUES ('', 1)"); out.errs[0] == nil {
			t.Error("new check not enforced")
		}
		d.unchanged("books")
		// Changing a named check replaces it; an unnamed one needs a rebuild.
		stmts = d.mustAlter("books", func(def *driver.TableDef) { def.Checks[1].Expression = "length(title) > 1" })
		if want := []string{"BEGIN", `ALTER TABLE "books" DROP CONSTRAINT "title_set"`, `ALTER TABLE "books" ADD CONSTRAINT "title_set" CHECK (length(title) > 1)`, "COMMIT"}; !reflect.DeepEqual(stmts, want) {
			t.Errorf("changed check: %q", stmts)
		}
		stmts = d.mustAlter("books", func(def *driver.TableDef) { def.Checks[0].Expression = "pages >= 1" })
		if !rebuilds(stmts) || !strings.Contains(stmts[2], "\n  CHECK (pages >= 1),") {
			t.Errorf("changed unnamed check: %q", stmts)
		}
		d.apply(stmts)
		if got := d.describe("books").Checks; got[0] != (driver.Check{Name: "check_1", Expression: "pages >= 1"}) {
			t.Errorf("checks = %+v", got)
		}
	})

	t.Run("indexes", func(t *testing.T) {
		d := newDDLDB(t, library)
		stmts := d.mustAlter("books", func(def *driver.TableDef) {
			dropIndex(def, func(ix driver.Index) bool { return ix.Name == "books_title" })
			def.Indexes = append(def.Indexes, driver.Index{Name: "books_cheap", Columns: []string{"price", "lower(title)"}, Where: "price < 15"})
			for i := range def.Indexes {
				if def.Indexes[i].Name == "books_isbn" {
					def.Indexes[i].Where = ""
				}
			}
		})
		want := []string{"BEGIN",
			`DROP INDEX "books_isbn"`,
			`DROP INDEX "books_title"`,
			`CREATE UNIQUE INDEX "books_isbn" ON "books" ("isbn")`,
			`CREATE INDEX "books_cheap" ON "books" ("price", lower(title)) WHERE price < 15`,
			"COMMIT"}
		if !reflect.DeepEqual(stmts, want) {
			t.Fatalf("got %q", stmts)
		}
		d.apply(stmts)
		d.unchanged("books")
	})

	t.Run("drop columns", func(t *testing.T) {
		d := newDDLDB(t, library)
		// Dropping a column's index along with it keeps the change in place.
		stmts := d.mustAlter("books", func(def *driver.TableDef) {
			dropColumn(def, "isbn")
			dropIndex(def, func(ix driver.Index) bool { return ix.Name == "books_isbn" })
		})
		if want := []string{"BEGIN", `DROP INDEX "books_isbn"`, `ALTER TABLE "books" DROP COLUMN "isbn"`, "COMMIT"}; !reflect.DeepEqual(stmts, want) {
			t.Fatalf("got %q", stmts)
		}
		d.apply(stmts)
		d.unchanged("books")
		// SQLite refuses to drop a column a view uses.
		stmts = d.mustAlter("authors", func(def *driver.TableDef) { dropColumn(def, "name") })
		if err := d.failure(stmts); !strings.Contains(err.Error(), "book_titles") {
			t.Errorf("error = %v", err)
		}
	})

	t.Run("rename table", func(t *testing.T) {
		d := newDDLDB(t, library)
		stmts := d.mustAlter("books", func(def *driver.TableDef) { def.Ref.Name = "volumes" })
		if want := []string{`ALTER TABLE "books" RENAME TO "volumes"`}; !reflect.DeepEqual(stmts, want) {
			t.Fatalf("got %q", stmts)
		}
		d.apply(stmts)
		if fk := d.describe("reviews").ForeignKeys; fk[0].RefTable.Name != "volumes" {
			t.Errorf("reviews still references %s", fk[0].RefTable.Name)
		}
		if got := d.rows("SELECT count(*) FROM book_titles"); got[0][0] != int64(3) {
			t.Errorf("view = %v", got)
		}
		stmts = d.mustAlter("volumes", func(def *driver.TableDef) { def.Ref.Name = "Volumes" })
		if want := []string{"BEGIN", `ALTER TABLE "volumes" RENAME TO "_rowsmith_tmp_Volumes"`, `ALTER TABLE "_rowsmith_tmp_Volumes" RENAME TO "Volumes"`, "COMMIT"}; !reflect.DeepEqual(stmts, want) {
			t.Fatalf("got %q", stmts)
		}
		d.apply(stmts)
		if tb := d.describe("volumes"); tb.Ref.Name != "Volumes" {
			t.Errorf("name = %s", tb.Ref.Name)
		}
		d.unchanged("Volumes")
	})
}

func TestAlterRebuild(t *testing.T) {
	t.Run("change type", func(t *testing.T) {
		d := newDDLDB(t, library)
		stmts := d.mustAlter("books", func(def *driver.TableDef) { column(t, def, "price").Type = "REAL" })
		want := []string{
			"PRAGMA foreign_keys = OFF",
			"BEGIN",
			`CREATE TABLE "_rowsmith_new_books" (
  "id" INTEGER PRIMARY KEY AUTOINCREMENT,
  "author_id" INTEGER,
  "title" TEXT NOT NULL,
  "pages" INT,
  "price" REAL DEFAULT 0,
  "isbn" TEXT,
  "slug" TEXT GENERATED ALWAYS AS (lower(title)) STORED,
  FOREIGN KEY ("author_id") REFERENCES "authors" ("id") ON DELETE CASCADE DEFERRABLE INITIALLY DEFERRED,
  CHECK (pages > 0),
  CONSTRAINT "price_sane" CHECK (price < 1000)
)`,
			`INSERT INTO "_rowsmith_new_books" ("id", "author_id", "title", "pages", "price", "isbn")
  SELECT "id", "author_id", "title", "pages", "price", "isbn" FROM "books"`,
			`DELETE FROM sqlite_sequence WHERE name = '_rowsmith_new_books'`,
			`INSERT INTO sqlite_sequence (name, seq) SELECT '_rowsmith_new_books', seq FROM sqlite_sequence WHERE name = 'books'`,
			`DROP TABLE "books"`,
			"PRAGMA legacy_alter_table = ON",
			`ALTER TABLE "_rowsmith_new_books" RENAME TO "books"`,
			"PRAGMA legacy_alter_table = OFF",
			"CREATE UNIQUE INDEX books_isbn ON books (isbn) WHERE isbn IS NOT NULL",
			"CREATE INDEX books_title ON books (title COLLATE NOCASE DESC)",
			"CREATE TRIGGER books_audit AFTER UPDATE OF title ON books\nBEGIN\n\tINSERT INTO audit (msg) VALUES ('retitled ' || OLD.title || ' as ' || NEW.title);\nEND",
			`PRAGMA foreign_key_check("books")`,
			`PRAGMA foreign_key_check("reviews")`,
			"COMMIT",
			"PRAGMA foreign_keys = ON",
		}
		if !reflect.DeepEqual(stmts, want) {
			t.Fatalf("got\n%s", strings.Join(stmts, ";\n"))
		}
		d.apply(stmts)

		books := d.describe("books")
		if p := columnNamed(t, books, "price"); p.Type != "REAL" {
			t.Errorf("price = %+v", p)
		}
		if got := d.rows("SELECT id, author_id, title, pages, price, isbn, slug FROM books ORDER BY id"); !reflect.DeepEqual(got, [][]any{
			{int64(1), int64(1), "Alpha", int64(100), 10.0, "111", "alpha"},
			{int64(2), int64(1), "Beta", int64(200), 20.5, nil, "beta"},
			{int64(3), int64(2), "Gamma", int64(300), 30.0, "333", "gamma"},
		}) {
			t.Errorf("rows = %v", got)
		}
		// The AUTOINCREMENT high-water mark survives: id 4 stays retired.
		d.mustRun("INSERT INTO books (author_id, title, pages) VALUES (1, 'Epsilon', 10)")
		if got := d.rows("SELECT max(id) FROM books"); got[0][0] != int64(5) {
			t.Errorf("next id = %v", got)
		}
		d.mustRun("UPDATE books SET title = 'Alef' WHERE id = 1")
		if got := d.rows("SELECT msg FROM audit ORDER BY rowid DESC LIMIT 1"); got[0][0] != "retitled Alpha as Alef" {
			t.Errorf("trigger = %v", got)
		}
		if got := d.rows("SELECT count(*) FROM book_titles"); got[0][0] != int64(4) {
			t.Errorf("view = %v", got)
		}
		// ON DELETE CASCADE still applies, and the deferred key is still deferred.
		d.mustRun("DELETE FROM reviews; DELETE FROM authors WHERE id = 2")
		if got := d.rows("SELECT count(*) FROM books WHERE author_id = 2"); got[0][0] != int64(0) {
			t.Errorf("cascade = %v", got)
		}
		d.mustRun("BEGIN; INSERT INTO books (author_id, title, pages) VALUES (9, 'Orphan', 1); INSERT INTO authors (id, name) VALUES (9, 'Nine'); COMMIT")
		d.unchanged("books")
	})

	t.Run("drop unique column", func(t *testing.T) {
		d := newDDLDB(t, library)
		stmts := d.mustAlter("authors", func(def *driver.TableDef) {
			dropColumn(def, "email")
			dropIndex(def, func(ix driver.Index) bool { return ix.Unique })
		})
		want := []string{
			"PRAGMA foreign_keys = OFF",
			"BEGIN",
			"CREATE TABLE \"_rowsmith_new_authors\" (\n  \"id\" INTEGER,\n  \"name\" TEXT NOT NULL COLLATE NOCASE,\n  PRIMARY KEY (\"id\")\n)",
			"INSERT INTO \"_rowsmith_new_authors\" (\"id\", \"name\")\n  SELECT \"id\", \"name\" FROM \"authors\"",
			`DROP TABLE "authors"`,
			"PRAGMA legacy_alter_table = ON",
			`ALTER TABLE "_rowsmith_new_authors" RENAME TO "authors"`,
			"PRAGMA legacy_alter_table = OFF",
			`ALTER TABLE "authors" RENAME COLUMN "id" TO "id"`,
			`PRAGMA foreign_key_check("authors")`,
			`PRAGMA foreign_key_check("books")`,
			"COMMIT",
			"PRAGMA foreign_keys = ON",
		}
		if !reflect.DeepEqual(stmts, want) {
			t.Fatalf("got\n%s", strings.Join(stmts, ";\n"))
		}
		d.apply(stmts)
		au := d.describe("authors")
		if !reflect.DeepEqual(columnNames(au), []string{"id", "name"}) || !columnNamed(t, au, "id").AutoIncrement || len(au.Indexes) != 0 {
			t.Errorf("authors = %+v", au)
		}
		if got := d.rows("SELECT id, name FROM authors WHERE name = 'ann'"); !reflect.DeepEqual(got, [][]any{{int64(1), "Ann"}}) {
			t.Errorf("rows (NOCASE kept) = %v", got)
		}
		d.unchanged("authors")
	})

	t.Run("drop column with unnamed check", func(t *testing.T) {
		d := newDDLDB(t, library)
		if _, err := d.alter("books", func(def *driver.TableDef) { dropColumn(def, "pages") }); err == nil || err.Error() != "check check_1 uses column pages, which is being dropped" {
			t.Fatalf("kept check: %v", err)
		}
		stmts := d.mustAlter("books", func(def *driver.TableDef) {
			dropColumn(def, "pages")
			def.Checks = def.Checks[1:]
		})
		if !rebuilds(stmts) {
			t.Fatalf("got %q", stmts)
		}
		d.apply(stmts)
		books := d.describe("books")
		if slices.Contains(columnNames(books), "pages") || len(books.Checks) != 1 || books.Checks[0].Name != "price_sane" {
			t.Errorf("books = %+v", books)
		}
		d.unchanged("books")
	})

	t.Run("reorder", func(t *testing.T) {
		d := newDDLDB(t, library)
		stmts := d.mustAlter("books", func(def *driver.TableDef) {
			title := def.Columns[2]
			def.Columns = slices.Insert(slices.Delete(def.Columns, 2, 3), 1, title)
		})
		if !rebuilds(stmts) {
			t.Fatalf("got %q", stmts)
		}
		d.apply(stmts)
		if got := columnNames(d.describe("books")); !reflect.DeepEqual(got, []string{"id", "title", "author_id", "pages", "price", "isbn", "slug"}) {
			t.Errorf("columns = %v", got)
		}
		if got := d.rows("SELECT * FROM books WHERE id = 2"); !reflect.DeepEqual(got, [][]any{{int64(2), "Beta", int64(1), int64(200), 20.5, nil, "beta"}}) {
			t.Errorf("row = %v", got)
		}
		d.unchanged("books")
	})

	t.Run("rename during rebuild", func(t *testing.T) {
		d := newDDLDB(t, library)
		stmts := d.mustAlter("books", func(def *driver.TableDef) {
			column(t, def, "title").Name = "heading"
			column(t, def, "price").Type = "REAL"
			def.Ref.Name = "volumes"
			def.Indexes = append(def.Indexes, driver.Index{Name: "volumes_heading", Columns: []string{"heading", "lower(heading)"}})
			def.Checks = append(def.Checks, driver.Check{Name: "heading_set", Expression: "heading <> ''"})
		})
		if !rebuilds(stmts) {
			t.Fatalf("got %q", stmts)
		}
		tail := stmts[len(stmts)-7:]
		if want := []string{`ALTER TABLE "books" RENAME COLUMN "title" TO "heading"`, `ALTER TABLE "books" RENAME TO "volumes"`,
			`PRAGMA foreign_key_check("volumes")`, `PRAGMA foreign_key_check("reviews")`, "COMMIT", "PRAGMA foreign_keys = ON"}; !reflect.DeepEqual(tail[1:], want) {
			t.Errorf("tail = %q", tail)
		}
		if !strings.Contains(stmts[2], `CONSTRAINT "heading_set" CHECK ("title" <> '')`) || !slices.Contains(stmts, `CREATE INDEX "volumes_heading" ON "books" ("title", lower("title"))`) {
			t.Errorf("new items not written with current names:\n%s", strings.Join(stmts, ";\n"))
		}
		d.apply(stmts)
		vol := d.describe("volumes")
		if s := columnNamed(t, vol, "slug"); s.Generated != `lower("heading")` && s.Generated != "lower(heading)" {
			t.Errorf("slug = %+v", s)
		}
		d.mustRun("UPDATE volumes SET heading = 'Alef' WHERE id = 1")
		if got := d.rows("SELECT msg FROM audit ORDER BY rowid DESC LIMIT 1"); got[0][0] != "retitled Alpha as Alef" {
			t.Errorf("trigger = %v", got)
		}
		if got := d.rows("SELECT * FROM book_titles WHERE id = 1"); !reflect.DeepEqual(got, [][]any{{int64(1), "Alef", "Ann"}}) {
			t.Errorf("view = %v", got)
		}
		if fk := d.describe("reviews").ForeignKeys; fk[0].RefTable.Name != "volumes" {
			t.Errorf("reviews fk = %+v", fk)
		}
		d.unchanged("volumes")
	})

	t.Run("foreign keys", func(t *testing.T) {
		d := newDDLDB(t, library)
		stmts := d.mustAlter("reviews", func(def *driver.TableDef) { def.ForeignKeys = nil })
		if !rebuilds(stmts) || strings.Contains(stmts[2], "FOREIGN KEY") {
			t.Fatalf("got %q", stmts)
		}
		d.apply(stmts)
		if fk := d.describe("reviews").ForeignKeys; len(fk) != 0 {
			t.Errorf("fks = %+v", fk)
		}
		stmts = d.mustAlter("reviews", func(def *driver.TableDef) {
			def.ForeignKeys = []driver.ForeignKey{{Columns: []string{"book_id"}, RefTable: driver.ObjectRef{Name: "books"}, RefColumns: []string{"id"}, OnDelete: "CASCADE"}}
		})
		if !rebuilds(stmts) || !strings.Contains(stmts[2], `FOREIGN KEY ("book_id") REFERENCES "books" ("id") ON DELETE CASCADE`) {
			t.Fatalf("got %q", stmts)
		}
		d.apply(stmts)
		d.unchanged("reviews")
		d.mustRun("DELETE FROM books WHERE id = 1")
		if got := d.rows("SELECT count(*) FROM reviews"); got[0][0] != int64(1) {
			t.Errorf("cascade = %v", got)
		}
		// Rows that break a key are reported by foreign_key_check.
		d.mustRun("PRAGMA foreign_keys = OFF; INSERT INTO reviews (book_id, stars) VALUES (99, 1); PRAGMA foreign_keys = ON")
		stmts = d.mustAlter("reviews", func(def *driver.TableDef) { def.ForeignKeys[0].OnDelete = "NO ACTION" })
		out := d.mustRun(strings.Join(stmts, ";\n"))
		if len(out.rows) != 1 || out.rows[0][0] != "reviews" || out.rows[0][2] != "books" {
			t.Errorf("foreign_key_check = %v", out.rows)
		}
		d.unchanged("reviews")
	})

	t.Run("primary key", func(t *testing.T) {
		d := newDDLDB(t, library)
		if _, err := d.alter("reviews", func(def *driver.TableDef) { def.PrimaryKey = []string{"book_id", "id"} }); err == nil ||
			err.Error() != "auto-increment needs id to be the only primary key column, with the type INTEGER" {
			t.Fatalf("err = %v", err)
		}
		if _, err := d.alter("reviews", func(def *driver.TableDef) { column(t, def, "id").AutoIncrement = false }); err == nil ||
			!strings.Contains(err.Error(), "give it another type such as INT") {
			t.Fatalf("err = %v", err)
		}
		stmts := d.mustAlter("reviews", func(def *driver.TableDef) {
			def.PrimaryKey = []string{"book_id", "id"}
			column(t, def, "id").AutoIncrement = false
		})
		// The old rowid alias no longer carries the rowids, so they are copied.
		if !rebuilds(stmts) || !strings.Contains(stmts[3], `(rowid, "id", "book_id", "stars")`) {
			t.Fatalf("got %q", stmts)
		}
		d.apply(stmts)
		rv := d.describe("reviews")
		if !reflect.DeepEqual(rv.PrimaryKey, []string{"book_id", "id"}) || columnNamed(t, rv, "id").AutoIncrement {
			t.Errorf("reviews = %+v", rv)
		}
		if got := d.rows("SELECT rowid, id, book_id FROM reviews ORDER BY id"); !reflect.DeepEqual(got, [][]any{{int64(1), int64(1), int64(1)}, {int64(2), int64(2), int64(3)}}) {
			t.Errorf("rows = %v", got)
		}
		d.unchanged("reviews")
	})

	t.Run("options", func(t *testing.T) {
		d := newDDLDB(t, oddities)
		stmts := d.mustAlter("wr", func(def *driver.TableDef) { def.Options["without_rowid"] = "false"; def.Options["strict"] = "true" })
		if !rebuilds(stmts) || !strings.HasSuffix(stmts[2], ") STRICT") {
			t.Fatalf("got %q", stmts)
		}
		d.apply(stmts)
		if o := d.describe("wr").Options; o["strict"] != "true" || o["without_rowid"] != "" {
			t.Errorf("options = %v", o)
		}
		d.unchanged("wr")
		// Rows keep their rowids when a rowid table is rebuilt.
		stmts = d.mustAlter("plain", func(def *driver.TableDef) { column(t, def, "b").Type = "TEXT" })
		d.apply(stmts)
		if got := d.rows("SELECT rowid, a FROM plain ORDER BY rowid"); !reflect.DeepEqual(got, [][]any{{int64(1), int64(1)}, {int64(3), int64(3)}}) {
			t.Errorf("rowids = %v", got)
		}
	})

	t.Run("self reference", func(t *testing.T) {
		d := newDDLDB(t, oddities)
		stmts := d.mustAlter("tree", func(def *driver.TableDef) {
			column(t, def, "id").Name = "node"
			column(t, def, "other").Type = "INT"
			def.Ref.Name = "nodes"
		})
		d.apply(stmts)
		tr := d.describe("nodes")
		for _, fk := range tr.ForeignKeys {
			if fk.RefTable.Name != "nodes" || !reflect.DeepEqual(fk.RefColumns, []string{"node"}) {
				t.Errorf("fk = %+v", fk)
			}
		}
		d.mustRun("DELETE FROM nodes WHERE node = 2")
		if got := d.rows("SELECT parent FROM nodes WHERE node = 3"); got[0][0] != nil {
			t.Errorf("SET NULL = %v", got)
		}
		d.unchanged("nodes")
	})

	t.Run("refusals", func(t *testing.T) {
		d := newDDLDB(t, library, `CREATE TABLE tags (id INTEGER PRIMARY KEY, name TEXT, code TEXT UNIQUE);
			CREATE TRIGGER tags_log AFTER INSERT ON tags BEGIN INSERT INTO audit (msg) VALUES (NEW.name); END;
			CREATE TABLE confl (a TEXT NOT NULL ON CONFLICT REPLACE DEFAULT 'x', b INT)`)
		for _, tc := range []struct {
			table string
			edit  func(*driver.TableDef)
			want  string
		}{
			{"tags", func(def *driver.TableDef) {
				dropColumn(def, "name")
				dropIndex(def, func(driver.Index) bool { return true })
			}, "trigger tags_log uses column name, which is being dropped; change or drop the trigger first"},
			{"authors", func(def *driver.TableDef) { dropColumn(def, "id"); def.PrimaryKey = nil },
				"column id is referenced by a foreign key of books; drop that key first"},
			{"authors", func(def *driver.TableDef) { def.PrimaryKey = nil; column(t, def, "id").AutoIncrement = false },
				"a foreign key of books references id, which must remain a primary key or unique index"},
			{"confl", func(def *driver.TableDef) { column(t, def, "b").Type = "TEXT" },
				"this change rebuilds confl, which would lose its ON CONFLICT clauses; make it in the SQL editor instead"},
			{"books", func(def *driver.TableDef) { def.Columns[1].OriginalName = "missing" }, "column missing no longer exists; reload the structure"},
			{"books", func(def *driver.TableDef) { def.Columns[1].OriginalName = "id" }, "column id appears twice in the new definition"},
			{"books", func(def *driver.TableDef) {
				dropColumn(def, "title")
				dropIndex(def, func(ix driver.Index) bool { return ix.Name == "books_title" })
			}, "generated column slug uses column title, which is being dropped"},
			{"books", func(def *driver.TableDef) { dropColumn(def, "isbn") }, "index books_isbn uses column isbn, which is being dropped"},
		} {
			if _, err := d.alter(tc.table, tc.edit); err == nil || err.Error() != tc.want {
				t.Errorf("%s: err = %v, want %q", tc.table, err, tc.want)
			}
		}
		for _, edit := range []func(*driver.Index){
			func(ix *driver.Index) { ix.Unique = false },
			func(ix *driver.Index) { ix.Where = "email IS NOT NULL" },
			func(ix *driver.Index) { ix.Columns = []string{"lower(email)"} },
		} {
			_, err := d.alter("authors", func(def *driver.TableDef) { edit(&def.Indexes[0]) })
			if err == nil || err.Error() != "index sqlite_autoindex_authors_1 belongs to a UNIQUE constraint, which covers plain columns of every row; remove it and add a new index instead" {
				t.Errorf("constraint index edit: %v", err)
			}
		}
		// Its columns can change, through a rebuild.
		stmts := d.mustAlter("authors", func(def *driver.TableDef) { def.Indexes[0].Columns = []string{"name", "email"} })
		if !rebuilds(stmts) || !strings.Contains(stmts[2], `UNIQUE ("name", "email")`) {
			t.Errorf("unique constraint change: %q", stmts)
		}
		d.apply(stmts)
		d.unchanged("authors")
		if _, err := d.c.AlterTableSQL(d.describe("book_titles"), driver.TableDef{}); err == nil {
			t.Error("altered a view")
		}
		if _, err := d.c.AlterTableSQL(nil, driver.TableDef{}); err == nil {
			t.Error("altered without a current definition")
		}
	})

	t.Run("broken view stops the rebuild", func(t *testing.T) {
		d := newDDLDB(t, library)
		stmts := d.mustAlter("authors", func(def *driver.TableDef) {
			dropColumn(def, "name")
			dropIndex(def, func(ix driver.Index) bool { return ix.Unique })
		})
		if err := d.failure(stmts); !strings.Contains(err.Error(), "error in view book_titles") {
			t.Errorf("error = %v", err)
		}
		d.mustRun("ROLLBACK; PRAGMA legacy_alter_table = OFF; PRAGMA foreign_keys = ON")
		if got := columnNames(d.describe("authors")); !reflect.DeepEqual(got, []string{"id", "name", "email"}) {
			t.Errorf("columns after rollback = %v", got)
		}
		if got := d.rows("SELECT count(*) FROM authors"); got[0][0] != int64(2) {
			t.Errorf("rows after rollback = %v", got)
		}
	})
}

func TestNamesChangeHands(t *testing.T) {
	d := newDDLDB(t, `CREATE TABLE p (a INT, b TEXT, c TEXT); INSERT INTO p VALUES (1, 'x', 'y');
		CREATE TABLE rid ("rowid" TEXT, x INT); INSERT INTO rid VALUES ('r1', 1), ('r2', 2), ('r3', 3); DELETE FROM rid WHERE x = 2`)
	// In place: rename a to z and add a new a; drop c and give its name to b.
	stmts := d.mustAlter("p", func(def *driver.TableDef) {
		column(t, def, "a").Name = "z"
		column(t, def, "b").Name = "c"
		def.Columns = slices.DeleteFunc(def.Columns, func(c driver.ColumnDef) bool { return c.OriginalName == "c" })
		def.Columns = append(def.Columns, driver.ColumnDef{Column: driver.Column{Name: "a", Type: "INT", Nullable: true, Default: strp("7")}})
	})
	want := []string{"BEGIN", `ALTER TABLE "p" DROP COLUMN "c"`, `ALTER TABLE "p" RENAME COLUMN "a" TO "z"`, `ALTER TABLE "p" RENAME COLUMN "b" TO "c"`,
		`ALTER TABLE "p" ADD COLUMN "a" INT DEFAULT 7`, "COMMIT"}
	if !reflect.DeepEqual(stmts, want) {
		t.Fatalf("got %q", stmts)
	}
	d.apply(stmts)
	if got := d.rows("SELECT z, c, a FROM p"); !reflect.DeepEqual(got, [][]any{{int64(1), "x", int64(7)}}) {
		t.Errorf("rows = %v", got)
	}
	// The same during a rebuild: the new column waits under a temporary name.
	stmts = d.mustAlter("p", func(def *driver.TableDef) {
		column(t, def, "z").Type = "INTEGER"
		column(t, def, "a").Name = "old_a"
		column(t, def, "c").Name = "a"
		def.Columns = append([]driver.ColumnDef{{Column: driver.Column{Name: "c", Type: "TEXT", Nullable: true, Default: strp("'new'")}}}, def.Columns...)
	})
	if !rebuilds(stmts) || !strings.Contains(stmts[2], `"_rowsmith_tmp_1" TEXT DEFAULT 'new'`) {
		t.Fatalf("got\n%s", strings.Join(stmts, ";\n"))
	}
	d.apply(stmts)
	if got := columnNames(d.describe("p")); !reflect.DeepEqual(got, []string{"c", "z", "a", "old_a"}) {
		t.Errorf("columns = %v", got)
	}
	if got := d.rows("SELECT c, z, a, old_a FROM p"); !reflect.DeepEqual(got, [][]any{{"new", int64(1), "x", int64(7)}}) {
		t.Errorf("rows = %v", got)
	}
	d.unchanged("p")

	// A column named rowid hides the rowid under that name; _rowid_ still reaches it.
	stmts = d.mustAlter("rid", func(def *driver.TableDef) { column(t, def, "x").Type = "TEXT" })
	if !strings.Contains(stmts[3], `(_rowid_, "rowid", "x")`) {
		t.Fatalf("got %q", stmts[3])
	}
	d.apply(stmts)
	if got := d.rows(`SELECT _rowid_, "rowid", x FROM rid ORDER BY 1`); !reflect.DeepEqual(got, [][]any{{int64(1), "r1", "1"}, {int64(3), "r3", "3"}}) {
		t.Errorf("rows = %v", got)
	}
}

func TestVirtualTable(t *testing.T) {
	d := newDDLDB(t)
	if out := d.run("CREATE VIRTUAL TABLE docs USING fts5(title, body)"); out.errs[0] != nil {
		t.Skip("fts5 unavailable:", out.errs[0])
	}
	d.unchanged("docs")
	stmts := d.mustAlter("docs", func(def *driver.TableDef) { def.Ref.Name = "notes" })
	if want := []string{`ALTER TABLE "docs" RENAME TO "notes"`}; !reflect.DeepEqual(stmts, want) {
		t.Fatalf("got %q", stmts)
	}
	d.apply(stmts)
	if _, err := d.alter("notes", func(def *driver.TableDef) { def.Columns = def.Columns[:1] }); err == nil || err.Error() != "virtual tables cannot be restructured, only renamed" {
		t.Errorf("err = %v", err)
	}
}

func TestObjectSQL(t *testing.T) {
	d := newDDLDB(t, library, `CREATE TRIGGER book_titles_add INSTEAD OF INSERT ON book_titles
		BEGIN INSERT INTO books (author_id, title, pages) VALUES (1, NEW.title, 1); END;
		CREATE VIEW v2 AS SELECT * FROM book_titles`)
	gen := func(stmts []string, err error) []string {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return stmts
	}

	if got := gen(d.c.TruncateSQL(driver.ObjectRef{Name: "audit", Kind: "table"})); !reflect.DeepEqual(got, []string{`DELETE FROM "audit"`}) {
		t.Errorf("truncate = %q", got)
	}
	if _, err := d.c.TruncateSQL(driver.ObjectRef{Name: "book_titles", Kind: "view"}); err == nil {
		t.Error("emptied a view")
	}
	if got := gen(d.c.RenameObjectSQL(driver.ObjectRef{Name: "audit", Kind: "table"}, "log")); !reflect.DeepEqual(got, []string{`ALTER TABLE "audit" RENAME TO "log"`}) {
		t.Errorf("rename table = %q", got)
	}
	d.apply(gen(d.c.RenameObjectSQL(driver.ObjectRef{Name: "audit", Kind: "table"}, "log")))

	// Views, indexes and triggers are recreated under the new name.
	if _, err := d.c.RenameObjectSQL(driver.ObjectRef{Name: "book_titles", Kind: "view"}, "titles"); err == nil ||
		err.Error() != "view v2 uses view book_titles and would break; rename both in the SQL editor" {
		t.Errorf("rename used view = %v", err)
	}
	d.mustRun("DROP VIEW v2")
	stmts := gen(d.c.RenameObjectSQL(driver.ObjectRef{Name: "book_titles", Kind: "view"}, "titles"))
	want := []string{"BEGIN", `DROP VIEW "book_titles"`,
		`CREATE VIEW "titles" AS SELECT b.id, b.title, a.name FROM books b JOIN authors a ON a.id = b.author_id`,
		"CREATE TRIGGER book_titles_add INSTEAD OF INSERT ON \"titles\"\n\t\tBEGIN INSERT INTO books (author_id, title, pages) VALUES (1, NEW.title, 1); END",
		"COMMIT"}
	if !reflect.DeepEqual(stmts, want) {
		t.Fatalf("rename view = %q", stmts)
	}
	d.apply(stmts)
	d.mustRun("INSERT INTO titles (title) VALUES ('Via view')")
	if got := d.rows("SELECT count(*) FROM titles"); got[0][0] != int64(4) {
		t.Errorf("renamed view = %v", got)
	}
	d.apply(gen(d.c.RenameObjectSQL(driver.ObjectRef{Name: "books_title", Kind: "index"}, "books_by_title")))
	d.apply(gen(d.c.RenameObjectSQL(driver.ObjectRef{Name: "books_audit", Kind: "trigger"}, "books_retitled")))
	objs, err := d.c.Objects(context.Background(), driver.Scope{})
	if err != nil {
		t.Fatal(err)
	}
	defs := map[string]string{}
	for _, o := range objs {
		defs[o.Name] = o.Kind
	}
	for name, kind := range map[string]string{"titles": "view", "books_by_title": "index", "books_retitled": "trigger", "book_titles_add": "trigger", "log": "table"} {
		if defs[name] != kind {
			t.Errorf("%s: %q, objects %v", name, defs[name], defs)
		}
	}
	if _, err := d.c.RenameObjectSQL(driver.ObjectRef{Name: "sqlite_autoindex_authors_1", Kind: "index"}, "x"); err == nil {
		t.Error("renamed a constraint index")
	}
	if _, err := d.c.RenameObjectSQL(driver.ObjectRef{Name: "books_by_title", Kind: "index"}, "books"); err == nil || err.Error() != "there is already a table named books" {
		t.Errorf("rename onto a table = %v", err)
	}
	if _, err := d.c.RenameObjectSQL(driver.ObjectRef{Name: "log", Kind: "table"}, " "); err == nil {
		t.Error("renamed to a blank name")
	}

	for ref, want := range map[driver.ObjectRef]string{
		{Name: "titles", Kind: "view"}:            `DROP VIEW "titles"`,
		{Name: "books_by_title", Kind: "index"}:   `DROP INDEX "books_by_title"`,
		{Name: "books_retitled", Kind: "trigger"}: `DROP TRIGGER "books_retitled"`,
		{Name: "log", Kind: "table"}:              `DROP TABLE "log"`,
	} {
		stmts := gen(d.c.DropObjectSQL(ref, true))
		if !reflect.DeepEqual(stmts, []string{want}) {
			t.Errorf("drop %v = %q", ref, stmts)
		}
		d.apply(stmts)
	}
	if _, err := d.c.DropObjectSQL(driver.ObjectRef{Name: "x", Kind: "procedure"}, false); err == nil {
		t.Error("dropped a procedure")
	}
	if _, err := d.c.CreateDatabaseSQL("x", nil); err == nil || !strings.HasPrefix(err.Error(), "SQLite databases are files") {
		t.Errorf("create database = %v", err)
	}
	if _, err := d.c.DropDatabaseSQL("x"); err == nil || !strings.HasPrefix(err.Error(), "SQLite databases are files") {
		t.Errorf("drop database = %v", err)
	}
}

func TestRenameIdents(t *testing.T) {
	names := map[string]string{"price": "cost", "a b": "c"}
	got := renameIdents(`price > 0 AND "Price" < max(price, 1) AND t.price = 'price' AND price(1) AND x COLLATE price AND CAST(y AS price) AND [a b]`, names)
	want := `"cost" > 0 AND "cost" < max("cost", 1) AND t."cost" = 'price' AND price(1) AND x COLLATE price AND CAST(y AS price) AND "c"`
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
	if !mentions(`NEW."a b" + 1`, "A B") || mentions(`'price'`, "price") {
		t.Error("mentions")
	}
}
