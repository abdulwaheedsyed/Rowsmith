package dynamodb

import (
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"rowsmith/internal/driver"
)

func TestSplitScript(t *testing.T) {
	script := `-- reads
SELECT * FROM "Orders"
WHERE customer_id = 'C;1';
SELECT * FROM Customers

Query
{"TableName": "Orders",
 "KeyConditionExpression": "customer_id = :c",
 "ExpressionAttributeValues": {":c": {"S": "C0001"}}}
DeleteTable {"TableName": "Old"}
ListTables
DELETE FROM "Orders" WHERE customer_id = 'C1' AND order_id = 'x'
`
	stmts, err := ddbDriver{}.SplitScript(script)
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		start string
		line  int
		kind  driver.StatementKind
		level string
	}{
		{"SELECT * FROM \"Orders\"\nWHERE customer_id = 'C;1'", 2, driver.StmtRead, ""},
		{"SELECT * FROM Customers", 4, driver.StmtRead, ""},
		{"Query\n{\"TableName\"", 6, driver.StmtRead, ""},
		{"DeleteTable {", 10, driver.StmtDDL, "destructive"},
		{"ListTables", 11, driver.StmtRead, ""},
		{"DELETE FROM", 12, driver.StmtWrite, "caution"},
	}
	if len(stmts) != len(want) {
		t.Fatalf("%d statements: %+v", len(stmts), stmts)
	}
	for i, w := range want {
		s := stmts[i]
		if !strings.HasPrefix(s.SQL, w.start) || s.Line != w.line || s.Kind != w.kind || s.Danger.Level != w.level {
			t.Errorf("%d: %+v, want %+v", i, s, w)
		}
		if script[s.Start:s.End] != s.SQL {
			t.Errorf("%d: offsets %d-%d give %q", i, s.Start, s.End, script[s.Start:s.End])
		}
		if strings.HasSuffix(s.SQL, ";") {
			t.Errorf("%d: keeps its semicolon", i)
		}
	}
	calls, _ := parseScript(script)
	if string(calls[2].body) != `{"TableName":"Orders","KeyConditionExpression":"customer_id = :c","ExpressionAttributeValues":{":c":{"S":"C0001"}}}` {
		t.Errorf("body %s", calls[2].body)
	}
	if string(calls[4].body) != "{}" {
		t.Errorf("empty body %s", calls[4].body)
	}

	for _, bad := range []string{"Scan\n{\"TableName\": ", "FROM x", "Query\n[1]"} {
		if _, err := parseScript(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

func TestPlan(t *testing.T) {
	tbl := &driver.Table{Columns: []driver.Column{{Name: "pk", Type: "S"}, {Name: "sk", Type: "N"}, {Name: "tags", Type: "SS"}, {Name: "note", Type: "S"}}}
	l := &layout{hash: "pk", rng: "sk", types: map[string]string{"pk": "S", "sk": "N", "tags": "SS", "note": "S"},
		indexes: []index{{name: "ByNote", hash: "note", all: true, global: true}}}

	r, err := plan(tbl, l, driver.BrowseRequest{Filters: []driver.Filter{{Column: "pk", Op: "=", Value: "a"}, {Column: "sk", Op: "between", Values: []any{"1", "5"}},
		{Column: "tags", Op: "contains", Value: "x"}}, Sort: []driver.Sort{{Column: "sk", Desc: true}}})
	if err != nil {
		t.Fatal(err)
	}
	if !r.query || r.index != "" || r.keyCond != "#a0 = :v0 AND #a1 BETWEEN :v1 AND :v2" || r.filter != "contains(#a2, :v3)" || r.forward == nil || *r.forward {
		t.Fatalf("query plan: %+v", r)
	}
	if n, ok := r.e.values[":v1"].(*types.AttributeValueMemberN); !ok || n.Value != "1" {
		t.Fatalf("sort key value typed %T", r.e.values[":v1"])
	}
	if s, ok := r.e.values[":v3"].(*types.AttributeValueMemberS); !ok || s.Value != "x" {
		t.Fatalf("set member typed %T", r.e.values[":v3"])
	}

	r, _ = plan(tbl, l, driver.BrowseRequest{Filters: []driver.Filter{{Column: "note", Op: "=", Value: "hi"}}})
	if !r.query || r.index != "ByNote" {
		t.Fatalf("index plan: %+v", r)
	}
	r, _ = plan(tbl, l, driver.BrowseRequest{Filters: []driver.Filter{{Column: "note", Op: "null"}}, Search: "q"})
	if r.query || !strings.Contains(r.filter, "attribute_not_exists(#a0)") || !strings.Contains(r.filter, "contains(") {
		t.Fatalf("scan plan: %+v", r)
	}
	for _, req := range []driver.BrowseRequest{
		{Sort: []driver.Sort{{Column: "sk"}}},
		{Filters: []driver.Filter{{Column: "note", Op: "like", Value: "%a"}}},
		{Where: "x = 1"},
	} {
		if _, err := plan(tbl, l, req); err == nil {
			t.Errorf("%+v planned", req)
		}
	}
}

func TestValues(t *testing.T) {
	cases := []struct {
		typ string
		in  any
		out string
	}{
		{"N", "12.50", "N"}, {"N", 3.0, "N"}, {"S", 5.0, "S"}, {"BOOL", "true", "BOOL"},
		{"M", `{"a": {"b": [1, "x"]}}`, "M"}, {"L", `[1, 2]`, "L"}, {"SS", []any{"a", "b"}, "SS"}, {"NS", `[1, 2.5]`, "NS"},
		{"", 4.5, "N"}, {"", "text", "S"}, {"", map[string]any{"$bin": "AAE="}, "B"}, {"", map[string]any{"$null": true}, "NULL"},
	}
	for _, c := range cases {
		a, err := attribute(c.typ, c.in)
		if err != nil || typeOf(a) != c.out {
			t.Errorf("%s %v: %v %v", c.typ, c.in, a, err)
		}
	}
	for _, c := range []struct {
		typ string
		in  any
	}{{"N", "abc"}, {"SS", []any{}}, {"S", map[string]any{"$expr": "NOW()"}}} {
		if _, err := attribute(c.typ, c.in); err == nil {
			t.Errorf("%s %v accepted", c.typ, c.in)
		}
	}
	// DynamoDB JSON both ways.
	item, err := itemFromJSON(map[string]any{"a": map[string]any{"M": map[string]any{"n": map[string]any{"N": "1.50"}, "s": map[string]any{"SS": []any{"x"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	back := itemJSON(item)["a"].(map[string]any)["M"].(map[string]any)
	if back["n"].(map[string]any)["N"] != "1.50" {
		t.Fatalf("round trip: %v", back)
	}
	if cell(&types.AttributeValueMemberN{Value: "0.1000000000000000055"}) != "0.1000000000000000055" {
		t.Fatal("numbers lose precision")
	}
}

func TestCreateTable(t *testing.T) {
	c := &conn{}
	def := driver.TableDef{Ref: driver.ObjectRef{Name: "T"}, PrimaryKey: []string{"pk", "sk"},
		Columns: []driver.ColumnDef{{Column: driver.Column{Name: "pk", Type: "S"}}, {Column: driver.Column{Name: "sk", Type: "N"}},
			{Column: driver.Column{Name: "at", Type: "S"}}, {Column: driver.Column{Name: "doc", Type: "M"}}, {Column: driver.Column{Name: "bad,name", Type: "S"}}},
		Indexes: []driver.Index{{Name: "ByAt", Type: "global", Columns: []string{"at"}, Definition: "projects INCLUDE doc"},
			{Name: "BySk", Type: "local", Columns: []string{"pk", "at"}}},
		Options: map[string]string{"ttl_attribute": "exp", "deletion_protection": "on"}}
	stmts, err := c.CreateTableSQL(def)
	if err != nil {
		t.Fatal(err)
	}
	if len(stmts) != 2 || !strings.HasPrefix(stmts[1], "UpdateTimeToLive") {
		t.Fatalf("%v", stmts)
	}
	for _, want := range []string{`"BillingMode": "PAY_PER_REQUEST"`, `"NonKeyAttributes": [`, `"LocalSecondaryIndexes"`, `"DeletionProtectionEnabled": true`,
		`"Value": "at=S doc=M"`, `"AttributeName": "at",`} {
		if !strings.Contains(stmts[0], want) {
			t.Errorf("missing %s in\n%s", want, stmts[0])
		}
	}
	calls, err := parseScript(strings.Join(stmts, "\n"))
	if err != nil || len(calls) != 2 {
		t.Fatalf("the DDL doesn't parse back: %v", err)
	}
	def.PrimaryKey = []string{"doc"}
	if _, err := c.CreateTableSQL(def); err == nil {
		t.Error("a map key accepted")
	}
	def.PrimaryKey = []string{"pk"}
	if _, err := c.CreateTableSQL(def); err == nil {
		t.Error("a local index without the table's sort key accepted")
	}
}
