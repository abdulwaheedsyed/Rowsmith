package cassandra

import (
	"encoding/json"
	"math/big"
	"strings"
	"testing"
	"time"

	gocql "github.com/apache/cassandra-gocql-driver/v2"
	"gopkg.in/inf.v0"

	"rowsmith/internal/driver"
)

func TestKinds(t *testing.T) {
	for typ, want := range map[string]driver.ValueKind{
		"text": driver.KindString, "bigint": driver.KindInt, "varint": driver.KindDecimal, "decimal": driver.KindDecimal,
		"timeuuid": driver.KindUUID, "timestamp": driver.KindTimestamp, "date": driver.KindDate, "duration": driver.KindInterval,
		"frozen<list<int>>": driver.KindArray, "map<text, int>": driver.KindObject, "tuple<int, text>": driver.KindArray,
		"vector<float, 3>": driver.KindArray, "address": driver.KindObject, "frozen<address>": driver.KindObject,
		"'org.apache.cassandra.db.marshal.DynamicCompositeType'": driver.KindOther,
	} {
		if got := kindOfType(typ); got != want {
			t.Errorf("%s = %s, want %s", typ, got, want)
		}
	}
	if k := elementKind("frozen<map<text, frozen<set<int>>>>"); k != driver.KindArray {
		t.Errorf("map value kind = %s", k)
	}
}

func TestQuote(t *testing.T) {
	for in, want := range map[string]string{"orders": "orders", "Orders": `"Orders"`, "select": `"select"`, "a b": `"a b"`, `x"y`: `"x""y"`} {
		if got := quote(in); got != want {
			t.Errorf("quote(%q) = %s", in, got)
		}
	}
}

func TestEncode(t *testing.T) {
	d := inf.NewDec(1250, 2)
	cases := []struct {
		in   any
		want string
	}{
		{d, `"12.50"`},
		{new(big.Int).Lsh(big.NewInt(1), 70), `"1180591620717411303424"`},
		{int64(1) << 60, `"1152921504606846976"`},
		{float32(0.1), `0.1`},
		{time.Date(2025, 3, 4, 5, 6, 7, 8000000, time.UTC), `"2025-03-04 05:06:07.008+00:00"`},
		{13*time.Hour + 14*time.Minute + 15*time.Second + 120*time.Millisecond, `"13:14:15.12"`},
		{gocql.Duration{Months: 14, Days: 3, Nanoseconds: int64(90 * time.Minute)}, `"1y2mo3d1h30m"`},
		{map[string]int{"b": 2, "a": 1}, `{"a":1,"b":2}`},
		{[]string{"x", "y"}, `["x","y"]`},
	}
	for _, c := range cases {
		b, _ := json.Marshal(encode(c.in))
		if string(b) != c.want {
			t.Errorf("encode(%#v) = %s, want %s", c.in, b, c.want)
		}
	}
	udt := gocql.UDTTypeInfo{Name: "address", Elements: []gocql.UDTField{{Name: "street"}, {Name: "city"}}}
	if b, _ := json.Marshal(encodeTyped(udt, map[string]any{"city": "Oslo", "street": "Main"})); string(b) != `{"street":"Main","city":"Oslo"}` {
		t.Errorf("udt = %s", b)
	}
}

func TestInputExpr(t *testing.T) {
	col := func(typ string) *driver.Column {
		c := column(schemaColumn{name: "c", typ: typ, kind: "regular"})
		return &c
	}
	for _, c := range []struct {
		typ  string
		v    any
		expr string
		arg  any
	}{
		{"int", float64(42), "fromJson(?)", "42"},
		{"decimal", "12.50", "fromJson(?)", `"12.50"`},
		{"text", "it's", "fromJson(?)", `"it's"`},
		{"timestamp", "2025-01-02T03:04:05+00:00", "fromJson(?)", `"2025-01-02 03:04:05+0000"`},
		{"timestamp", "2025-01-02 03:04:05.123", "fromJson(?)", `"2025-01-02 03:04:05.123+0000"`},
		{"timestamp", "2025-01-02T03:04:05Z", "fromJson(?)", `"2025-01-02 03:04:05+0000"`},
		{"blob", map[string]any{"$bin": "yv4="}, "fromJson(?)", `"0xcafe"`},
		{"set<text>", `["a","b"]`, "fromJson(?)", `["a","b"]`},
		{"list<text>", []any{"a"}, "fromJson(?)", `["a"]`},
		{"boolean", "true", "fromJson(?)", "true"},
		{"uuid", "1600a35a-0999-40d8-b6f6-75cc81e74ef5", "fromJson(?)", `"1600a35a-0999-40d8-b6f6-75cc81e74ef5"`},
		{"timeuuid", map[string]any{"$expr": "now()"}, "now()", nil},
		{"text", map[string]any{"$null": true}, "null", nil},
	} {
		expr, arg, err := inputExpr(col(c.typ), c.v)
		if err != nil || expr != c.expr || arg != c.arg {
			t.Errorf("%s %v: %s %v %v", c.typ, c.v, expr, arg, err)
		}
	}
	if _, _, err := inputExpr(col("int"), "abc"); err == nil {
		t.Error("abc read as an int")
	}
}

func TestDDL(t *testing.T) {
	c := &conn{defKS: "shop"}
	q, err := c.CreateTableSQL(driver.TableDef{Ref: driver.ObjectRef{Name: "readings"}, PrimaryKey: []string{"sensor", "day", "ts"}, Comment: "it's",
		Options: map[string]string{"partition_key": "sensor, day", "clustering_order": "ts desc", "compaction": "TimeWindowCompactionStrategy"},
		Columns: []driver.ColumnDef{{Column: driver.Column{Name: "sensor", Type: "text"}}, {Column: driver.Column{Name: "day", Type: "date"}},
			{Column: driver.Column{Name: "ts", Type: "timestamp"}}, {Column: driver.Column{Name: "Value", Type: "double"}}},
		Indexes: []driver.Index{{Name: "by_value", Columns: []string{"Value"}, Type: "sai"}}})
	want := "CREATE TABLE shop.readings (\n  sensor text,\n  day date,\n  ts timestamp,\n  \"Value\" double,\n  PRIMARY KEY ((sensor, day), ts)\n)" +
		" WITH CLUSTERING ORDER BY (ts DESC)\n  AND comment = 'it''s'\n  AND compaction = {'class': 'TimeWindowCompactionStrategy'}"
	if err != nil || len(q) != 2 || q[0] != want || q[1] != `CREATE INDEX by_value ON shop.readings ("Value") USING 'sai'` {
		t.Errorf("create = %q, %v", q, err)
	}
	if _, err := c.CreateTableSQL(driver.TableDef{Ref: driver.ObjectRef{Name: "x"}, Columns: []driver.ColumnDef{{Column: driver.Column{Name: "a", Type: "int"}}}}); err == nil {
		t.Error("created a table without a key")
	}
	if _, err := c.CreateTableSQL(driver.TableDef{Ref: driver.ObjectRef{Name: "x"}, PrimaryKey: []string{"a", "b"}, Options: map[string]string{"partition_key": "b"},
		Columns: []driver.ColumnDef{{Column: driver.Column{Name: "a", Type: "int"}}, {Column: driver.Column{Name: "b", Type: "int"}}}}); err == nil {
		t.Error("partition key after a clustering column")
	}
	if got := primaryKeyText([]string{"id"}, nil); got != "PRIMARY KEY (id)" {
		t.Errorf("pk = %s", got)
	}
	if got := inline("SELECT * FROM t WHERE a = fromJson(?) AND b = ?", []any{`"x'y"`, 3}); !strings.Contains(got, `fromJson('"x''y"')`) || !strings.HasSuffix(got, "b = 3") {
		t.Errorf("inline = %s", got)
	}
}

func TestOrderBy(t *testing.T) {
	tb := &driver.Table{Options: map[string]string{"clustering_order": "placed_at DESC, order_id ASC"}}
	for _, c := range []struct {
		sorts []driver.Sort
		eq    bool
		want  string
	}{
		{[]driver.Sort{{Column: "placed_at", Desc: true}}, true, " ORDER BY placed_at DESC"},
		{[]driver.Sort{{Column: "placed_at"}, {Column: "order_id", Desc: true}}, true, " ORDER BY placed_at ASC, order_id DESC"}, // all reversed
		{[]driver.Sort{{Column: "order_id"}}, true, ""},                                                                          // not a prefix
		{[]driver.Sort{{Column: "placed_at", Desc: true}, {Column: "order_id", Desc: true}}, true, ""},                           // mixed
		{[]driver.Sort{{Column: "placed_at", Desc: true}}, false, ""},                                                            // whole table
	} {
		got, err := orderBy(tb, c.sorts, c.eq)
		if (c.want == "") != (err != nil) || got != c.want {
			t.Errorf("%+v (partition %v) = %q, %v", c.sorts, c.eq, got, err)
		}
	}
}
