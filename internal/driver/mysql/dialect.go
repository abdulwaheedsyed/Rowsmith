package mysql

import (
	"database/sql"
	"encoding/json"
	"strconv"
	"strings"

	"rowsmith/internal/driver"
	"rowsmith/internal/driver/geo"
	"rowsmith/internal/driver/sqlbase"
	"rowsmith/internal/sqlsplit"
)

type dialect struct {
	sqlbase.Base
	c *conn
}

func (dialect) Split() sqlsplit.Dialect       { return sqlsplit.MySQL }
func (dialect) QuoteIdent(n string) string    { return quote(n) }
func (dialect) Placeholder(int) string        { return "?" }
func (dialect) LimitOne() (string, string)    { return "", " LIMIT 1" }
func (dialect) Regexp(expr, ph string) string { return expr + " REGEXP " + ph }

func (dialect) Qualify(r driver.ObjectRef) string { return qualify(r.Database, r.Name) }

func (dialect) Kind(ct *sql.ColumnType) driver.ValueKind {
	t := strings.TrimPrefix(ct.DatabaseTypeName(), "UNSIGNED ")
	switch t {
	case "BIT":
		if n, ok := ct.Length(); ok && n == 1 {
			return driver.KindBool
		}
		return driver.KindInt
	case "YEAR":
		return driver.KindInt
	case "TIMESTAMP":
		return driver.KindDateTime
	case "ENUM":
		return driver.KindEnum
	case "SET":
		return driver.KindString
	case "VECTOR":
		return driver.KindBinary
	}
	return driver.KindFromTypeName(t)
}

func (dialect) Encode(v any, ct *sql.ColumnType, kind driver.ValueKind) any {
	b, ok := v.([]byte)
	if !ok {
		return sqlbase.GenericEncode(v, kind)
	}
	switch kind {
	case driver.KindGeometry:
		if cell, ok := geo.FromMySQL(b); ok {
			return cell
		}
		return driver.EncodeBinary(b)
	case driver.KindBool, driver.KindInt:
		if ct.DatabaseTypeName() == "BIT" {
			var n uint64
			for _, x := range b {
				n = n<<8 | uint64(x)
			}
			if kind == driver.KindBool {
				return n != 0
			}
			return driver.EncodeUint(n)
		}
		if strings.HasPrefix(ct.DatabaseTypeName(), "UNSIGNED") {
			if n, err := strconv.ParseUint(string(b), 10, 64); err == nil {
				return driver.EncodeUint(n)
			}
		}
	case driver.KindDecimal:
		return string(b)
	}
	return sqlbase.GenericEncode(b, kind)
}

func (dialect) SelectExpr(c driver.Column) string {
	if c.Kind == driver.KindGeometry {
		// Oversized geometries are reported by type and size instead of
		// being sent whole (see sqlbase.GeoCell).
		g := quote(c.Name)
		return "CASE WHEN LENGTH(" + g + ") <= 262144 THEN ST_AsGeoJSON(" + g + ") ELSE CONCAT('X', ST_GeometryType(" + g + "), ', ', ROUND(LENGTH(" + g + ") / 1048576, 1), ' MB') END"
	}
	return quote(c.Name)
}

// ExactSelectExpr sends geometry whole, for exports.
func (d dialect) ExactSelectExpr(c driver.Column) string {
	if c.Kind == driver.KindGeometry {
		return "ST_AsGeoJSON(" + quote(c.Name) + ")"
	}
	return d.SelectExpr(c)
}

func (dialect) TextExpr(c driver.Column) string {
	switch c.Kind {
	case driver.KindString, driver.KindText, driver.KindEnum:
		return quote(c.Name)
	}
	return "CAST(" + quote(c.Name) + " AS CHAR)"
}

func (dialect) Like(expr, ph string, negate bool) string {
	if negate {
		return expr + " NOT LIKE " + ph
	}
	return expr + " LIKE " + ph
}

func (d dialect) InputExpr(col *driver.Column, v any, ph string) (string, any, error) {
	if col != nil && col.Kind == driver.KindGeometry {
		srid := strconv.Itoa(col.SRID)
		switch x := v.(type) {
		case string:
			if strings.HasPrefix(strings.TrimSpace(x), "{") {
				return "ST_GeomFromGeoJSON(" + ph + ", 1, " + srid + ")", x, nil
			}
			return "ST_GeomFromText(" + ph + ", " + srid + ")", x, nil
		case map[string]any:
			if g, ok := x["$geo"]; ok {
				b, _ := json.Marshal(g)
				return "ST_GeomFromGeoJSON(" + ph + ", 1, " + srid + ")", string(b), nil
			}
		}
	}
	return sqlbase.DefaultInputExpr(d, col, v, ph)
}

func (dialect) Paginate(q string, limit int, offset int64, ordered bool) string {
	return sqlbase.Base{}.Paginate(q, limit, offset, ordered)
}
