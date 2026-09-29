// Package sqlbase implements the parts of a driver that are common to every
// engine reachable through database/sql: browsing with filters and paging,
// safe row edits, catalog helpers and streaming script execution. Engines
// supply a Dialect for quoting, pagination and type conversion.
package sqlbase

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"

	"rowsmith/internal/driver"
	"rowsmith/internal/sqlsplit"
)

type Dialect interface {
	Split() sqlsplit.Dialect
	QuoteIdent(name string) string
	// Qualify renders a fully qualified object name for use in DML.
	Qualify(ref driver.ObjectRef) string
	Placeholder(n int) string // 1-based
	// Paginate appends a row window to a SELECT that already has ORDER BY (if any).
	Paginate(query string, limit int, offset int64, ordered bool) string
	// Kind maps a result column's engine type name to a value kind.
	Kind(ct *sql.ColumnType) driver.ValueKind
	// Encode converts a scanned value into its JSON cell representation.
	Encode(v any, ct *sql.ColumnType, kind driver.ValueKind) any
	// SelectExpr renders a column in browse queries (e.g. geometry as GeoJSON).
	SelectExpr(col driver.Column) string
	// TextExpr renders a column cast to text for quick search.
	TextExpr(col driver.Column) string
	// Like returns a case-insensitive LIKE comparison of expr against placeholder ph.
	Like(expr, ph string, negate bool) string
	// Regexp returns a regular-expression match, or "" when unsupported.
	Regexp(expr, ph string) string
	// InputExpr turns an edited cell value into a SQL expression and bind argument.
	// arg is ignored when expr contains no placeholder.
	InputExpr(col *driver.Column, v any, ph string) (expr string, arg any, err error)
	// LimitOne returns SQL fragments that restrict UPDATE/DELETE to one row
	// when rows are matched on all columns (no key). Either may be empty.
	LimitOne() (prefix, suffix string)
}

// Base provides default implementations; engines embed it and override.
type Base struct{}

func (Base) Split() sqlsplit.Dialect { return sqlsplit.Generic }

func (Base) QuoteIdent(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }

func (Base) Placeholder(n int) string { return "?" }

func (Base) Paginate(q string, limit int, offset int64, _ bool) string {
	q += " LIMIT " + strconv.Itoa(limit)
	if offset > 0 {
		q += " OFFSET " + strconv.FormatInt(offset, 10)
	}
	return q
}

func (Base) Kind(ct *sql.ColumnType) driver.ValueKind { return driver.KindFromTypeName(ct.DatabaseTypeName()) }

func (Base) TextExpr(col driver.Column) string { return "CAST(" + `"` + col.Name + `"` + " AS VARCHAR(4000))" }

func (Base) Like(expr, ph string, negate bool) string {
	op := " LIKE "
	if negate {
		op = " NOT LIKE "
	}
	return "LOWER(" + expr + ")" + op + "LOWER(" + ph + ")"
}

func (Base) Regexp(expr, ph string) string { return "" }

func (Base) LimitOne() (string, string) { return "", "" }

// GenericEncode handles the value types database/sql drivers commonly return.
func GenericEncode(v any, kind driver.ValueKind) any {
	switch x := v.(type) {
	case nil:
		return nil
	case []byte:
		switch kind {
		case driver.KindBinary, driver.KindOther:
			if kind == driver.KindOther && isPrintable(x) {
				return driver.EncodeText(string(x))
			}
			return driver.EncodeBinary(x)
		case driver.KindInt:
			if n, err := strconv.ParseInt(string(x), 10, 64); err == nil {
				return driver.EncodeInt(n)
			}
			return string(x)
		case driver.KindFloat:
			if f, err := strconv.ParseFloat(string(x), 64); err == nil {
				return driver.EncodeFloat(f)
			}
			return string(x)
		case driver.KindBool:
			s := string(x)
			return s == "1" || strings.EqualFold(s, "true") || s == "t"
		case driver.KindUUID:
			if len(x) == 16 {
				return driver.EncodeUUIDBytes(x)
			}
			return string(x)
		default:
			return driver.EncodeText(string(x))
		}
	case string:
		return driver.EncodeText(x)
	case int64:
		if kind == driver.KindBool {
			return x != 0
		}
		return driver.EncodeInt(x)
	case int32:
		return int64(x)
	case int:
		return driver.EncodeInt(int64(x))
	case uint64:
		return driver.EncodeUint(x)
	case float64:
		return driver.EncodeFloat(x)
	case float32:
		return driver.EncodeFloat(float64(x))
	case bool:
		return x
	case fmt.Stringer:
		return driver.EncodeText(x.String())
	}
	if t, ok := asTime(v); ok {
		return driver.EncodeTime(t, kind)
	}
	return driver.EncodeText(fmt.Sprint(v))
}

func isPrintable(b []byte) bool {
	for _, c := range b {
		if c < 0x09 || (c > 0x0d && c < 0x20) || c == 0x7f {
			return false
		}
	}
	return true
}

// Conn is satisfied by *sql.DB, *sql.Conn and *sql.Tx.
type Conn interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}
