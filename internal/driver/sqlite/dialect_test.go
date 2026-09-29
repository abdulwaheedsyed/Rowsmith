package sqlite

import (
	"reflect"
	"testing"
	"time"

	"rowsmith/internal/driver"
)

func TestKindOf(t *testing.T) {
	tests := map[string]driver.ValueKind{
		"INTEGER": driver.KindInt, "int": driver.KindInt, "BIGINT": driver.KindInt, "UNSIGNED BIG INT": driver.KindInt,
		"TINYINT(1)": driver.KindInt, "POINT": driver.KindInt, // contains "INT": INTEGER affinity
		"VARCHAR(255)": driver.KindString, "NCHAR(55)": driver.KindString, "CHARACTER(20)": driver.KindString,
		"TEXT": driver.KindText, "clob": driver.KindText,
		"BLOB": driver.KindBinary, "": driver.KindOther,
		"REAL": driver.KindFloat, "DOUBLE PRECISION": driver.KindFloat, "FLOAT": driver.KindFloat,
		"NUMERIC": driver.KindDecimal, "DECIMAL(10,5)": driver.KindDecimal, "STRING": driver.KindDecimal, // NUMERIC affinity
		"DATE": driver.KindDate, "datetime": driver.KindDateTime, "TIMESTAMP": driver.KindDateTime, "TIME": driver.KindTime,
		"BOOLEAN": driver.KindBool, "JSON": driver.KindJSON, "UUID": driver.KindUUID,
	}
	for typ, want := range tests {
		if got := kindOf(typ); got != want {
			t.Errorf("kindOf(%q) = %s, want %s", typ, got, want)
		}
	}
}

func TestEncode(t *testing.T) {
	d := dialect{}
	tests := []struct {
		v    any
		kind driver.ValueKind
		want any
	}{
		{nil, driver.KindInt, nil},
		{int64(7), driver.KindText, int64(7)}, // storage class wins over the declared type
		{int64(1) << 60, driver.KindInt, "1152921504606846976"},
		{int64(1), driver.KindBool, true},
		{int64(2), driver.KindBool, int64(2)},
		{12.5, driver.KindDecimal, 12.5},
		{"abc", driver.KindInt, "abc"},
		{[]byte("hi"), driver.KindText, map[string]any{"$bin": "aGk=", "size": 2}},
		{time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC), driver.KindDate, "2024-01-15"},
		{time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC), driver.KindDate, "2024-01-15 10:30:00"},
		{time.Date(2024, 1, 15, 10, 30, 0, 0, time.FixedZone("", 7200)), driver.KindDateTime, "2024-01-15 10:30:00+02:00"},
	}
	for _, tt := range tests {
		if got := d.Encode(tt.v, nil, tt.kind); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("Encode(%#v, %s) = %#v, want %#v", tt.v, tt.kind, got, tt.want)
		}
	}
}

func TestInputExpr(t *testing.T) {
	d := dialect{}
	def := "datetime('now')"
	col := &driver.Column{Name: "at", Kind: driver.KindDateTime, Default: &def}
	if expr, _, err := d.InputExpr(col, map[string]any{"$default": true}, "?"); err != nil || expr != "(datetime('now'))" {
		t.Errorf("default with declared value: %q, %v", expr, err)
	}
	if expr, _, err := d.InputExpr(&driver.Column{Name: "x"}, map[string]any{"$default": true}, "?"); err != nil || expr != "NULL" {
		t.Errorf("default without declared value: %q, %v", expr, err)
	}
	if expr, _, err := d.InputExpr(col, map[string]any{"$expr": "datetime('now', '+1 day')"}, "?"); err != nil || expr != "datetime('now', '+1 day')" {
		t.Errorf("expression: %q, %v", expr, err)
	}
	if _, _, err := d.InputExpr(col, map[string]any{"$expr": "1; DROP TABLE t"}, "?"); err == nil {
		t.Error("an expression carrying a second statement was accepted")
	}
}

func TestCheckFragment(t *testing.T) {
	ok := []string{"", "a = 1", "name = 'x;y'", `"we;ird" > 2`, "[a;b] IS NULL", "a = 1 -- trailing; comment"}
	for _, s := range ok {
		if err := checkFragment(s); err != nil {
			t.Errorf("checkFragment(%q) = %v", s, err)
		}
	}
	bad := []string{"1); DELETE FROM t; SELECT (1", "1) ; PRAGMA query_only = 0", "x IN (SELECT * FROM pragma_temp_store_directory)",
		"1); PRAGMA TEMP_STORE_DIRECTORY = '/tmp'; SELECT (1"}
	for _, s := range bad {
		if err := checkFragment(s); err == nil {
			t.Errorf("checkFragment(%q) accepted", s)
		}
	}
}
