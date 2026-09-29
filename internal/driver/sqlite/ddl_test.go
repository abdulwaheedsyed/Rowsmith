package sqlite

import (
	"reflect"
	"testing"

	"rowsmith/internal/driver"
)

func TestParseTable(t *testing.T) {
	def := parseTable(`CREATE TABLE "order items" (
		id INTEGER PRIMARY KEY, -- note: AS (not generated)
		qty INT NOT NULL CONSTRAINT "qty positive" CHECK (qty > 0),
		price DECIMAL(10, 2) DEFAULT (0.0) CHECK(price >= 0),
		total REAL GENERATED ALWAYS AS (qty * price) STORED,
		[label] TEXT AS (upper('a,b' || "id")),
		note TEXT DEFAULT 'CHECK (1)',
		CONSTRAINT sane CHECK (total < 1e9),
		CHECK (id > 0)
	) WITHOUT ROWID, STRICT`)
	wantChecks := []driver.Check{
		{Name: "qty positive", Expression: "qty > 0"},
		{Name: "check_2", Expression: "price >= 0"},
		{Name: "sane", Expression: "total < 1e9"},
		{Name: "check_4", Expression: "id > 0"},
	}
	if !reflect.DeepEqual(def.checks, wantChecks) {
		t.Errorf("checks = %#v", def.checks)
	}
	if want := map[string]string{"total": "qty * price", "label": `upper('a,b' || "id")`}; !reflect.DeepEqual(def.generated, want) {
		t.Errorf("generated = %#v", def.generated)
	}
	if !def.withoutRowid || !def.strict || def.module != "" {
		t.Errorf("options = %+v", def)
	}
	if v := parseTable("CREATE VIRTUAL TABLE docs USING fts5(title, body)"); v.module != "fts5" || len(v.checks) != 0 {
		t.Errorf("virtual table = %+v", v)
	}
}

func TestParseIndex(t *testing.T) {
	keys, where := parseIndex(`CREATE UNIQUE INDEX "ix(1)" ON t (lower(name) COLLATE NOCASE DESC, "a,b", substr(x, 1, 2)) WHERE deleted IS NULL`)
	if want := []string{"lower(name) COLLATE NOCASE", `"a,b"`, "substr(x, 1, 2)"}; !reflect.DeepEqual(keys, want) {
		t.Errorf("keys = %q", keys)
	}
	if where != "deleted IS NULL" {
		t.Errorf("where = %q", where)
	}
}

func TestParseTrigger(t *testing.T) {
	tests := map[string][2]string{
		"CREATE TRIGGER a AFTER UPDATE OF total ON orders BEGIN SELECT 1; END":                {"AFTER", "UPDATE"},
		"CREATE TEMP TRIGGER IF NOT EXISTS main.b INSTEAD OF DELETE ON v BEGIN SELECT 1; END": {"INSTEAD OF", "DELETE"},
		`CREATE TRIGGER "after insert" INSERT ON t BEGIN SELECT 1; END`:                       {"BEFORE", "INSERT"},
		"create trigger c before delete on t for each row when old.x > 0 begin select 1; end": {"BEFORE", "DELETE"},
	}
	for ddl, want := range tests {
		if tr := parseTrigger(ddl); tr.Timing != want[0] || tr.Event != want[1] || tr.Statement != ddl {
			t.Errorf("parseTrigger(%q) = %s %s", ddl, tr.Timing, tr.Event)
		}
	}
}

func TestBuildPlan(t *testing.T) {
	plan := buildPlan([]planRow{
		{5, 0, "SEARCH a USING INDEX ix_v (v=?)"},
		{9, 0, "LIST SUBQUERY 1"},
		{11, 9, "SEARCH t USING INTEGER PRIMARY KEY (rowid>?)"},
		{25, 0, "SEARCH b USING INTEGER PRIMARY KEY (rowid=?)"},
		{62, 0, "USE TEMP B-TREE FOR ORDER BY"},
	})
	want := "QUERY PLAN\n" +
		"|--SEARCH a USING INDEX ix_v (v=?)\n" +
		"|--LIST SUBQUERY 1\n" +
		"|  `--SEARCH t USING INTEGER PRIMARY KEY (rowid>?)\n" +
		"|--SEARCH b USING INTEGER PRIMARY KEY (rowid=?)\n" +
		"`--USE TEMP B-TREE FOR ORDER BY"
	if plan.Raw != want || plan.Format != "text" {
		t.Errorf("raw plan:\n%s", plan.Raw)
	}
	root := plan.Root
	if root.Operation != "Query plan" || len(root.Children) != 4 {
		t.Fatalf("root = %+v", root)
	}
	if n := root.Children[0]; n.Operation != "Search" || n.Object != "a" || n.Detail != "USING INDEX ix_v (v=?)" {
		t.Errorf("first step = %+v", n)
	}
	if sub := root.Children[1]; sub.Operation != "LIST SUBQUERY 1" || len(sub.Children) != 1 || sub.Children[0].Object != "t" {
		t.Errorf("subquery = %+v", sub)
	}

	single := buildPlan([]planRow{{2, 0, "SCAN CONSTANT ROW"}}).Root
	if single.Operation != "SCAN CONSTANT ROW" || single.Object != "" || len(single.Children) != 0 {
		t.Errorf("single step = %+v", single)
	}
	if scan := buildPlan([]planRow{{2, 0, "SCAN logs"}}).Root; scan.Operation != "Scan" || scan.Object != "logs" || scan.Detail != "" {
		t.Errorf("scan = %+v", scan)
	}
}
