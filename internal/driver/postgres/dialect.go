package postgres

import (
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"

	"rowsmith/internal/driver"
	"rowsmith/internal/driver/geo"
	"rowsmith/internal/driver/sqlbase"
	"rowsmith/internal/sqlsplit"
)

type dialect struct{ sqlbase.Base }

func (dialect) Split() sqlsplit.Dialect    { return sqlsplit.Postgres }
func (dialect) QuoteIdent(n string) string { return quote(n) }
func (dialect) Placeholder(n int) string   { return "$" + strconv.Itoa(n) }
func (dialect) Qualify(r driver.ObjectRef) string {
	s := r.Schema
	if s == "" {
		s = "public"
	}
	return qualify(s, r.Name)
}

func (dialect) Paginate(q string, limit int, offset int64, ordered bool) string {
	return sqlbase.Base{}.Paginate(q, limit, offset, ordered)
}

func (dialect) Kind(ct *sql.ColumnType) driver.ValueKind {
	t := strings.ToLower(ct.DatabaseTypeName())
	switch t {
	case "bpchar", "name", "text", "varchar", "citext":
		if t == "text" || t == "citext" {
			return driver.KindText
		}
		return driver.KindString
	case "timestamptz":
		return driver.KindTimestamp
	case "timestamp":
		return driver.KindDateTime
	case "tid", "xid", "cid", "oid", "regclass", "regproc", "regtype", "pg_lsn", "inet", "cidr", "macaddr", "hstore", "ltree":
		return driver.KindString
	case "vector", "halfvec", "sparsevec", "tsvector", "tsquery", "xml":
		return driver.KindText
	case "raster":
		return driver.KindBinary
	}
	if _, err := strconv.Atoi(t); err == nil {
		return driver.KindOther // type unknown to the client; value arrives as text
	}
	return driver.KindFromTypeName(t)
}

func (dialect) Encode(v any, ct *sql.ColumnType, kind driver.ValueKind) any {
	switch kind {
	case driver.KindGeometry:
		if s, ok := v.(string); ok {
			if cell, ok := geo.FromEWKBHex(s); ok {
				return cell
			}
		}
	case driver.KindJSON:
		if b, ok := v.([]byte); ok {
			return driver.EncodeText(string(b))
		}
	case driver.KindBinary:
		if s, ok := v.(string); ok && strings.HasPrefix(s, `\x`) {
			if b, err := hex.DecodeString(s[2:]); err == nil {
				return driver.EncodeBinary(b)
			}
		}
	}
	return sqlbase.GenericEncode(v, kind)
}

func (dialect) SelectExpr(c driver.Column) string {
	if c.Kind == driver.KindGeometry {
		// Large geometries (a detailed boundary or network can be hundreds
		// of MB as GeoJSON) are simplified, or reduced to their extent, in
		// the database so a browse page stays small; GeoCell marks them.
		g := quote(c.Name)
		if strings.Contains(strings.ToLower(c.Type), "geography") {
			g += "::geometry"
		}
		extent := "'E' || ST_AsGeoJSON(ST_Envelope(" + g + "))"
		tol := "GREATEST(ST_XMax(" + g + ") - ST_XMin(" + g + "), ST_YMax(" + g + ") - ST_YMin(" + g + ")) / 1000.0"
		return "CASE WHEN ST_MemSize(" + g + ") <= 65536 THEN ST_AsGeoJSON(" + g + ")" +
			" WHEN ST_NumGeometries(" + g + ") > 2000 OR ST_NPoints(" + g + ") > 2000000 THEN " + extent +
			" ELSE (SELECT CASE WHEN ST_NPoints(s) <= 10000 THEN 'S' || ST_AsGeoJSON(s, 6) ELSE " + extent + " END" +
			" FROM (SELECT ST_Simplify(" + g + ", " + tol + ", true) AS s) rowsmith_simplified) END"
	}
	return quote(c.Name)
}

func (dialect) TextExpr(c driver.Column) string { return quote(c.Name) + "::text" }

func (dialect) Like(expr, ph string, negate bool) string {
	if negate {
		return expr + " NOT ILIKE " + ph
	}
	return expr + " ILIKE " + ph
}

func (dialect) Regexp(expr, ph string) string { return expr + " ~* " + ph }

func (d dialect) InputExpr(col *driver.Column, v any, ph string) (string, any, error) {
	if col != nil && col.Kind == driver.KindGeometry {
		cast := "::geometry"
		if strings.HasPrefix(strings.ToLower(col.Type), "geography") {
			cast = "::geography"
		}
		srid := strconv.Itoa(col.SRID)
		switch x := v.(type) {
		case string:
			s := strings.TrimSpace(x)
			if strings.HasPrefix(s, "{") {
				return "ST_SetSRID(ST_GeomFromGeoJSON(" + ph + "), " + srid + ")" + cast, s, nil
			}
			if strings.HasPrefix(strings.ToUpper(s), "SRID=") || col.SRID == 0 {
				return "ST_GeomFromEWKT(" + ph + ")" + cast, s, nil
			}
			return "ST_GeomFromText(" + ph + ", " + srid + ")" + cast, s, nil
		case map[string]any:
			if g, ok := x["$geo"]; ok {
				b, _ := json.Marshal(g)
				return "ST_SetSRID(ST_GeomFromGeoJSON(" + ph + "), " + srid + ")" + cast, string(b), nil
			}
		}
	}
	return sqlbase.DefaultInputExpr(d, col, v, ph)
}
