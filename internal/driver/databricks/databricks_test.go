package databricks

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"rowsmith/internal/driver"
)

func TestParsePlan(t *testing.T) {
	raw, err := os.ReadFile("testdata/plan.txt")
	if err != nil {
		t.Fatal(err)
	}
	p := parsePlan(string(raw))
	if p.Root == nil || p.Root.Operation != "AdaptiveSparkPlan" || p.Totals["Photon"] != "The query is fully supported by Photon." {
		t.Fatalf("plan = %+v", p)
	}
	// The join has both inputs as children, each ending in a scan.
	var join *driver.PlanNode
	var scans []string
	var walk func(n *driver.PlanNode)
	walk = func(n *driver.PlanNode) {
		if n.Operation == "PhotonShuffledHashJoin" {
			join = n
		}
		if n.Object != "" {
			scans = append(scans, n.Object)
		}
		for _, ch := range n.Children {
			walk(ch)
		}
	}
	walk(p.Root)
	if join == nil || len(join.Children) != 2 || strings.Join(scans, ",") != "samples.tpch.orders,samples.tpch.customer" {
		t.Errorf("join = %+v, scans = %v", join, scans)
	}
}

func TestValues(t *testing.T) {
	v, ok := orderedJSON(`{"b":1,"a":[1.5,"x",null,{"z":true}]}`)
	b, _ := json.Marshal(v)
	if !ok || string(b) != `{"b":1,"a":[1.5,"x",null,{"z":true}]}` {
		t.Errorf("ordered = %s", b)
	}
	for typ, want := range map[string]driver.ValueKind{
		"decimal(18,2)": driver.KindDecimal, "BIGINT": driver.KindInt, "array<string>": driver.KindArray, "struct<a: int>": driver.KindObject,
		"map<string,int>": driver.KindObject, "timestamp_ntz": driver.KindDateTime, "variant": driver.KindJSON, "string": driver.KindString,
	} {
		if k := kindOf(typ); k != want {
			t.Errorf("%s = %s", typ, k)
		}
	}
	d := dialect{}
	for _, c := range []struct {
		col  driver.Column
		v    any
		expr string
		arg  any
	}{
		{column("id", "bigint", false), float64(7), "CAST(? AS bigint)", "7"},
		{column("amount", "decimal(10,2)", true), "12.50", "CAST(? AS decimal(10,2))", "12.50"},
		{column("at", "timestamp", true), "2025-01-02 03:04:05+0000", "CAST(? AS timestamp)", "2025-01-02 03:04:05+00:00"},
		{column("tags", "array<string>", true), []any{"a"}, "from_json(?, 'array<string>')", `["a"]`},
		{column("doc", "variant", true), `{"k":1}`, "parse_json(?)", `{"k":1}`},
		{column("raw", "binary", true), map[string]any{"$bin": "yv4="}, "unbase64(?)", "yv4="},
		{column("name", "string", true), "it's", "?", "it's"},
	} {
		expr, arg, err := d.InputExpr(&c.col, c.v, "?")
		if err != nil || expr != c.expr || arg != c.arg {
			t.Errorf("%s: %q %v %v", c.col.Name, expr, arg, err)
		}
	}
	if q := d.Paginate("SELECT 1", 50, 100, false); q != "SELECT 1 LIMIT 50 OFFSET 100" {
		t.Errorf("paginate = %q", q)
	}
}

func TestDDL(t *testing.T) {
	c := &conn{}
	def := "current_timestamp()"
	q, err := c.CreateTableSQL(driver.TableDef{Ref: driver.ObjectRef{Database: "main", Schema: "s", Name: "t"}, PrimaryKey: []string{"id"}, Comment: "it's",
		Options: map[string]string{"partitioned_by": "day"}, Checks: []driver.Check{{Name: "pos", Expression: "id > 0"}},
		Columns: []driver.ColumnDef{{Column: driver.Column{Name: "id", Type: "BIGINT", Nullable: true}}, {Column: driver.Column{Name: "day", Type: "DATE", Nullable: true}},
			{Column: driver.Column{Name: "at", Type: "TIMESTAMP", Nullable: true, Default: &def}}}})
	want := "CREATE TABLE `main`.`s`.`t` (\n  `id` BIGINT NOT NULL,\n  `day` DATE,\n  `at` TIMESTAMP DEFAULT current_timestamp(),\n  CONSTRAINT `t_pk` PRIMARY KEY (`id`)\n) USING DELTA" +
		"\nPARTITIONED BY (`day`)\nCOMMENT 'it\\'s'\nTBLPROPERTIES ('delta.feature.allowColumnDefaults' = 'supported')"
	if err != nil || len(q) != 2 || q[0] != want || q[1] != "ALTER TABLE `main`.`s`.`t` ADD CONSTRAINT `pos` CHECK (id > 0)" {
		t.Errorf("create = %q, %v", q, err)
	}
	if _, err := c.CreateTableSQL(driver.TableDef{Ref: driver.ObjectRef{Database: "m", Schema: "s", Name: "t"}, Options: map[string]string{"partitioned_by": "a", "cluster_by": "b"},
		Columns: []driver.ColumnDef{{Column: driver.Column{Name: "a", Type: "INT"}}}}); err == nil {
		t.Error("partitioned and clustered")
	}
	from := &driver.Table{Ref: driver.ObjectRef{Database: "m", Schema: "s", Name: "t"}, Kind: "table", Options: map[string]string{},
		Columns: []driver.Column{{Name: "a", Type: "INT", Nullable: true}, {Name: "b", Type: "STRING", Nullable: true}}}
	to := driver.TableDef{Ref: from.Ref, Options: map[string]string{}, Columns: []driver.ColumnDef{
		{Column: driver.Column{Name: "a2", Type: "INT", Nullable: true}, OriginalName: "a"}}}
	q, err = c.AlterTableSQL(from, to)
	if err != nil || len(q) != 3 || !strings.Contains(q[0], "delta.columnMapping.mode") || !strings.Contains(q[1], "DROP COLUMNS (`b`)") || !strings.Contains(q[2], "RENAME COLUMN `a` TO `a2`") {
		t.Errorf("alter = %q, %v", q, err)
	}
}
