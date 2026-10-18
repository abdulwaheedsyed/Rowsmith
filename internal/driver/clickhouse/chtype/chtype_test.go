package chtype

import (
	"slices"
	"testing"

	"rowsmith/internal/driver"
)

func TestParse(t *testing.T) {
	ty := Parse("LowCardinality(Nullable(String))")
	if in, null := ty.Unwrap(); in.Name != "String" || !null {
		t.Fatalf("unwrap: %+v %v", in, null)
	}
	tup := Parse("Tuple(name String, `the n` UInt8, Array(Nullable(Int32)))")
	if len(tup.Args) != 3 || tup.Args[0].Field != "name" || tup.Args[1].Field != "the n" || tup.Args[2].Name != "Array" || tup.Args[2].Field != "" {
		t.Fatalf("tuple: %+v", tup.Args)
	}
	if m := Parse("Map(String, Array(UInt64))"); len(m.Args) != 2 || m.Args[1].Args[0].Name != "UInt64" {
		t.Fatalf("map: %+v", m)
	}
	if e := Parse("Enum8('a' = 1, 'it''s' = 2, 'b\\'c' = 3)").EnumValues(); !slices.Equal(e, []string{"a", "it's", "b'c"}) {
		t.Fatalf("enum: %q", e)
	}
	if p, s := Parse("Decimal(38, 10)").DecimalPS(); p != 38 || s != 10 {
		t.Fatalf("decimal: %d %d", p, s)
	}
	if p, s := Parse("Decimal64(4)").DecimalPS(); p != 18 || s != 4 {
		t.Fatalf("decimal64: %d %d", p, s)
	}
	if dt := Parse("DateTime64(3, 'Asia/Riyadh')"); dt.Name != "DateTime64" || len(dt.Params) != 2 {
		t.Fatalf("datetime64: %+v", dt)
	}
	if s := StripNullable("Nullable(Array(String))"); s != "Array(String)" {
		t.Fatalf("strip: %s", s)
	}
	if s := StripNullable("LowCardinality(Nullable(String))"); s != "LowCardinality(Nullable(String))" {
		t.Fatalf("strip keeps LowCardinality: %s", s)
	}
}

func TestLiteral(t *testing.T) {
	for _, c := range []struct {
		typ  string
		v    any
		expr bool
		want string
	}{
		{"UInt64", float64(42), false, "42"},
		{"UInt64", "18446744073709551615", false, "18446744073709551615"},
		{"Nullable(String)", nil, true, "NULL"},
		{"String", "it's a \\ test", false, `'it\'s a \\ test'`},
		{"Decimal(10, 2)", "12.50", false, "12.50"},
		{"Decimal(10, 2)", "12.50", true, "CAST('12.50' AS Decimal(10, 2))"},
		{"Bool", "true", false, "true"},
		{"Float64", "nan", false, "nan"},
		{"DateTime", "2025-01-02T03:04:05Z", true, "CAST('2025-01-02 03:04:05' AS DateTime)"},
		{"Array(Nullable(Int32))", []any{float64(1), nil}, false, "[1, NULL]"},
		{"Array(String)", `["a","b"]`, false, "['a', 'b']"},
		{"Map(String, UInt8)", driver.Doc{Keys: []string{"b", "a"}, Values: map[string]any{"a": float64(1), "b": float64(2)}}, false, "{'b': 2, 'a': 1}"},
		{"Map(String, UInt8)", map[string]any{"a": float64(1)}, true, "map('a', 1)"},
		{"Tuple(name String, n UInt8)", map[string]any{"n": float64(3), "name": "x"}, false, "('x', 3)"},
		{"Tuple(String)", []any{"x"}, false, "tuple('x')"},
		{"JSON", map[string]any{"a": float64(1)}, false, `'{"a":1}'`},
		{"Point", `{"type":"Point","coordinates":[46.7,24.7]}`, false, "(46.7, 24.7)"},
		{"Polygon", "POLYGON((0 0, 1 0, 1 1, 0 0))", false, "[[(0, 0), (1, 0), (1, 1), (0, 0)]]"},
		{"FixedString(2)", map[string]any{"$bin": "AAE="}, false, "unhex('0001')"},
		{"String", map[string]any{"$expr": "concat('a', 'b')"}, true, "(concat('a', 'b'))"},
	} {
		got, err := Literal(Parse(c.typ), c.v, c.expr)
		if err != nil || got != c.want {
			t.Errorf("Literal(%s, %#v, %v) = %q, %v; want %q", c.typ, c.v, c.expr, got, err, c.want)
		}
	}
	for _, bad := range []struct {
		typ string
		v   any
	}{{"UInt8", "x"}, {"Int32", float64(1.5)}, {"Decimal(5, 2)", "lots"}, {"Bool", "maybe"}, {"Array(String)", "nope"}, {"Point", "LINESTRING(0 0, 1 1)"}} {
		if got, err := Literal(Parse(bad.typ), bad.v, false); err == nil {
			t.Errorf("Literal(%s, %#v) = %q, want an error", bad.typ, bad.v, got)
		}
	}
}
