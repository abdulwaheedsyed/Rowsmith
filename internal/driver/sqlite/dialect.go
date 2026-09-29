package sqlite

import (
	"database/sql"
	"strings"
	"time"

	"rowsmith/internal/driver"
	"rowsmith/internal/driver/sqlbase"
	"rowsmith/internal/sqlsplit"
)

type dialect struct{ sqlbase.Base }

func (dialect) Split() sqlsplit.Dialect           { return sqlsplit.SQLite }
func (dialect) QuoteIdent(n string) string        { return quote(n) }
func (dialect) Qualify(r driver.ObjectRef) string { return quote(r.Name) }

func (dialect) Kind(ct *sql.ColumnType) driver.ValueKind { return kindOf(ct.DatabaseTypeName()) }

// kindOf maps a declared column type with SQLite's affinity rules
// (https://www.sqlite.org/datatype3.html#determination_of_column_affinity).
// Names that fall into NUMERIC affinity are refined when they denote dates,
// booleans, JSON or UUIDs.
func kindOf(declared string) driver.ValueKind {
	t := strings.ToUpper(strings.TrimSpace(declared))
	switch {
	case strings.Contains(t, "INT"):
		return driver.KindInt
	case strings.Contains(t, "CHAR"):
		return driver.KindString
	case strings.Contains(t, "CLOB"), strings.Contains(t, "TEXT"):
		return driver.KindText
	case strings.Contains(t, "BLOB"):
		return driver.KindBinary
	case t == "":
		return driver.KindOther
	case strings.Contains(t, "REAL"), strings.Contains(t, "FLOA"), strings.Contains(t, "DOUB"):
		return driver.KindFloat
	}
	switch k := driver.KindFromTypeName(t); k {
	case driver.KindDate, driver.KindTime, driver.KindDateTime, driver.KindTimestamp, driver.KindBool, driver.KindJSON, driver.KindUUID:
		return k
	}
	return driver.KindDecimal
}

// Encode follows the value's storage class rather than the declared type:
// any SQLite column can hold integers, reals, text and blobs side by side.
func (dialect) Encode(v any, _ *sql.ColumnType, kind driver.ValueKind) any {
	switch x := v.(type) {
	case int64:
		if kind == driver.KindBool && (x == 0 || x == 1) {
			return x == 1
		}
		return driver.EncodeInt(x)
	case float64:
		return driver.EncodeFloat(x)
	case string:
		return driver.EncodeText(x)
	case []byte:
		return driver.EncodeBinary(x)
	case time.Time:
		// modernc parses the text of DATE, DATETIME and TIMESTAMP columns in
		// console results; use a format that drops nothing the value carries.
		if _, off := x.Zone(); off != 0 {
			return driver.EncodeTime(x, driver.KindTimestamp)
		}
		if kind == driver.KindDate && x.Equal(x.Truncate(24*time.Hour)) {
			return driver.EncodeTime(x, driver.KindDate)
		}
		return driver.EncodeTime(x, driver.KindDateTime)
	}
	return sqlbase.GenericEncode(v, kind)
}

// SelectExpr keeps browse values exactly as stored: modernc turns the text of
// columns declared DATE, DATETIME or TIMESTAMP into time.Time, and the no-op
// unary plus hides the declared type from it.
func (dialect) SelectExpr(c driver.Column) string {
	switch strings.ToUpper(c.Type) {
	case "DATE", "DATETIME", "TIMESTAMP":
		return "+" + quote(c.Name)
	}
	return quote(c.Name)
}

func (dialect) TextExpr(c driver.Column) string { return "CAST(" + quote(c.Name) + " AS TEXT)" }

// Like relies on SQLite's LIKE being case-insensitive for ASCII; the escape
// clause matches the backslash escaping sqlbase applies to search patterns.
func (dialect) Like(expr, ph string, negate bool) string {
	op := " LIKE "
	if negate {
		op = " NOT LIKE "
	}
	return expr + op + ph + ` ESCAPE '\'`
}

func (d dialect) InputExpr(col *driver.Column, v any, ph string) (string, any, error) {
	if m, ok := v.(map[string]any); ok {
		if b, _ := m["$default"].(bool); b {
			// SQLite has no DEFAULT keyword in VALUES or SET: use the declared default.
			if col != nil && col.Default != nil {
				return "(" + *col.Default + ")", nil, nil
			}
			return "NULL", nil, nil
		}
		if e, ok := m["$expr"].(string); ok {
			if err := checkFragment(e); err != nil {
				return "", nil, err
			}
		}
	}
	return sqlbase.DefaultInputExpr(d, col, v, ph)
}
