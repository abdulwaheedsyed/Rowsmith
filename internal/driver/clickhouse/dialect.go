package clickhouse

import (
	"database/sql"
	"strconv"

	"rowsmith/internal/driver"
	"rowsmith/internal/driver/clickhouse/chtype"
	"rowsmith/internal/driver/sqlbase"
	"rowsmith/internal/sqlsplit"
)

type dialect struct{ sqlbase.Base }

func (dialect) Split() sqlsplit.Dialect       { return sqlsplit.ClickHouse }
func (dialect) QuoteIdent(name string) string { return quote(name) }
func (dialect) Placeholder(int) string        { return "?" }

func (dialect) Qualify(r driver.ObjectRef) string { return qualify(r.Database, r.Name) }

func (dialect) Paginate(q string, limit int, offset int64, _ bool) string {
	q += " LIMIT " + strconv.Itoa(limit)
	if offset > 0 {
		q += " OFFSET " + strconv.FormatInt(offset, 10)
	}
	return q
}

func (dialect) Kind(ct *sql.ColumnType) driver.ValueKind {
	return kindOf(typeOf(ct.DatabaseTypeName()))
}

func (dialect) Encode(v any, ct *sql.ColumnType, _ driver.ValueKind) any {
	return encode(v, typeOf(ct.DatabaseTypeName()))
}

// SelectExpr reads maps as their ordered pairs, so their keys keep their order.
func (dialect) SelectExpr(c driver.Column) string {
	if expr, ok := orderedSelect(c); ok {
		return expr
	}
	return quote(c.Name)
}

// TypeName reports maps read as pairs under their own type.
func (dialect) TypeName(ct *sql.ColumnType) string { return typeOf(ct.DatabaseTypeName()).Raw }

// TextExpr searches text and numbers; containers are searched in their text form.
func (dialect) TextExpr(c driver.Column) string { return "toString(" + quote(c.Name) + ")" }

func (dialect) Like(expr, ph string, negate bool) string {
	if negate {
		return expr + " NOT ILIKE " + ph
	}
	return expr + " ILIKE " + ph
}

func (dialect) Regexp(expr, ph string) string { return "match(" + expr + ", " + ph + ")" }

// InputExpr writes an edited value as a typed literal: Rowsmith formats it
// rather than the client library, so arrays, maps, tuples, geo values and
// binary strings come out right for the column's type.
func (dialect) InputExpr(col *driver.Column, v any, ph string) (string, any, error) {
	typ := "String"
	if col != nil {
		typ = col.Type
		if col.Nullable {
			typ = "Nullable(" + typ + ")"
		}
	}
	lit, err := chtype.Literal(typeOf(typ), v, true)
	if err != nil && col != nil {
		return "", nil, &driver.QueryError{Message: "Column " + col.Name + ": " + err.Error()}
	}
	return lit, nil, err
}
