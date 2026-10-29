package trino

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"rowsmith/internal/driver"
)

func names(n *driver.PlanNode, depth int, out *[]string) {
	*out = append(*out, strings.Repeat(" ", depth)+n.Operation)
	for _, ch := range n.Children {
		names(ch, depth+1, out)
	}
}

func TestParsePlan(t *testing.T) {
	raw, err := os.ReadFile("testdata/plan.json")
	if err != nil {
		t.Fatal(err)
	}
	p := parsePlan(string(raw))
	if p.Root == nil || p.Root.Operation != "Fragment 0" {
		t.Fatalf("root = %+v", p.Root)
	}
	var tree []string
	names(p.Root, 0, &tree)
	text := strings.Join(tree, "\n")
	// Every fragment hangs under the RemoteSource that reads it.
	for _, f := range []string{"Fragment 1", "Fragment 2"} {
		if !strings.Contains(text, f) {
			t.Errorf("%s missing:\n%s", f, text)
		}
	}
	var scans int
	var walk func(n *driver.PlanNode)
	walk = func(n *driver.PlanNode) {
		if strings.HasPrefix(n.Object, "iceberg:shop.") {
			scans++
		}
		for _, ch := range n.Children {
			walk(ch)
		}
	}
	walk(p.Root)
	if scans != 2 {
		t.Errorf("table scans = %d\n%s", scans, text)
	}
}

func TestParseAnalyze(t *testing.T) {
	raw, err := os.ReadFile("testdata/analyze.txt")
	if err != nil {
		t.Fatal(err)
	}
	p := parseAnalyze(string(raw))
	var tree []string
	names(p.Root, 0, &tree)
	want := []string{"Fragment 1", " Aggregate", "  LocalExchange", "   RemoteSource", "    Fragment 2", "     Aggregate", "      TableScan"}
	if !reflect.DeepEqual(tree, want) {
		t.Fatalf("tree:\n%s", strings.Join(tree, "\n"))
	}
	scan := p.Root.Children[0].Children[0].Children[0].Children[0].Children[0].Children[0]
	if !strings.HasPrefix(scan.Object, "iceberg:shop.orders") || scan.ActualRows == nil || *scan.ActualRows != 14995 || scan.TimeMS == nil {
		t.Errorf("scan = %+v", scan)
	}
	if p.Totals["Trino version"] != "483" {
		t.Errorf("totals = %v", p.Totals)
	}
}

func TestRowFieldTypes(t *testing.T) {
	typ := `row(phone varchar, "Address" row(city varchar, zip integer), tags array(varchar))`
	if got := rowFieldTypes(typ); len(got) != 3 || got[1] != "row(city varchar, zip integer)" || got[2] != "array(varchar)" {
		t.Errorf("types = %q", got)
	}
	if got := rowFieldTypes("array(varchar)"); len(got) != 0 {
		t.Errorf("array read as a row: %q", got)
	}
}

func TestDialect(t *testing.T) {
	d := dialect{}
	if q := d.Paginate("SELECT 1", 50, 100, false); q != "SELECT 1 OFFSET 100 LIMIT 50" {
		t.Errorf("paginate = %q", q)
	}
	for _, tc := range []struct {
		col  driver.Column
		v    any
		expr string
		arg  any
	}{
		{column("id", "bigint"), float64(42), "CAST(? AS bigint)", "42"},
		{column("total", "decimal(12,2)"), "9.50", "CAST(? AS decimal(12,2))", "9.50"},
		{column("seen", "timestamp(6) with time zone"), "2025-01-02T03:04:05+00:00", "CAST(? AS timestamp(6) with time zone)", "2025-01-02 03:04:05 +00:00"},
		{column("tags", "array(varchar)"), []any{"a"}, "CAST(json_parse(?) AS array(varchar))", `["a"]`},
		{column("doc", "json"), "not json", "json_parse(?)", `"not json"`},
		{column("raw", "varbinary"), map[string]any{"$bin": "AAE="}, "from_base64(?)", "AAE="},
		{column("name", "varchar(20)"), "Ada", "?", "Ada"},
	} {
		expr, arg, err := d.InputExpr(&tc.col, tc.v, "?")
		if err != nil || expr != tc.expr || arg != tc.arg {
			t.Errorf("%s: %q %v %v", tc.col.Name, expr, arg, err)
		}
	}
	if c := column("total", "decimal(12,2)"); *c.Precision != 12 || *c.Scale != 2 || c.Kind != driver.KindDecimal {
		t.Errorf("decimal = %+v", c)
	}
	if got := d.TypedArg(&driver.Column{Kind: driver.KindInt, Type: "bigint"}, "?"); got != "CAST(? AS bigint)" {
		t.Errorf("typed arg = %q", got)
	}
}

func TestDDL(t *testing.T) {
	c := &conn{connectors: map[string]string{"hive": "hive"}}
	q, err := c.CreateTableSQL(driver.TableDef{Ref: driver.ObjectRef{Database: "hive", Schema: "raw", Name: "t"},
		Options: map[string]string{"format": "csv", "location": "s3://lake/raw/t/", "partitioned_by": "day"},
		Columns: []driver.ColumnDef{{Column: driver.Column{Name: "a", Type: "varchar", Nullable: true}}, {Column: driver.Column{Name: "day", Type: "varchar", Nullable: true}}}})
	want := "CREATE TABLE \"hive\".\"raw\".\"t\" (\n  \"a\" varchar,\n  \"day\" varchar\n)\nWITH (\n  format = 'CSV',\n  partitioned_by = ARRAY['day'],\n  external_location = 's3://lake/raw/t/'\n)"
	if err != nil || len(q) != 1 || q[0] != want {
		t.Errorf("create = %q, %v", q, err)
	}
	if got := arrayList("month(placed_at), bucket(id, 4)"); got != "ARRAY['month(placed_at)', 'bucket(id, 4)']" {
		t.Errorf("array = %q", got)
	}
}
