package mssql

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	ms "github.com/microsoft/go-mssqldb"
	"github.com/twpayne/go-geom"
	"github.com/twpayne/go-geom/encoding/geojson"
	"github.com/twpayne/go-geom/encoding/wkt"

	"rowsmith/internal/driver"
	"rowsmith/internal/driver/sqlbase"
	"rowsmith/internal/sqlsplit"
)

type dialect struct{ sqlbase.Base }

func (dialect) Split() sqlsplit.Dialect           { return sqlsplit.MSSQL }
func (dialect) QuoteIdent(n string) string        { return quote(n) }
func (dialect) Placeholder(n int) string          { return "@p" + strconv.Itoa(n) }
func (dialect) LimitOne() (string, string)        { return "TOP (1) ", "" }
func (dialect) Qualify(r driver.ObjectRef) string { return qualify(r.Database, r.Schema, r.Name) }

// Paginate uses OFFSET/FETCH, which requires an ORDER BY.
func (dialect) Paginate(q string, limit int, offset int64, ordered bool) string {
	if !ordered {
		q += " ORDER BY (SELECT NULL)"
	}
	return q + " OFFSET " + strconv.FormatInt(offset, 10) + " ROWS FETCH NEXT " + strconv.Itoa(limit) + " ROWS ONLY"
}

func (dialect) Kind(ct *sql.ColumnType) driver.ValueKind {
	t := strings.ToLower(ct.DatabaseTypeName())
	if t == "hierarchyid" {
		return driver.KindBinary // arrives in its binary form outside browse queries
	}
	return kindOf(t, false)
}

// kindOf maps a system type name to a value kind; max marks (n)varchar(max).
func kindOf(base string, max bool) driver.ValueKind {
	switch base {
	case "timestamp":
		return driver.KindBinary // rowversion, not a date
	case "varchar", "nvarchar":
		if max {
			return driver.KindText
		}
	case "hierarchyid":
		return driver.KindString // browsed through ToString()
	}
	return driver.KindFromTypeName(base)
}

func (dialect) Encode(v any, ct *sql.ColumnType, kind driver.ValueKind) any {
	return encode(v, ct.DatabaseTypeName(), kind)
}

// encode converts a scanned value; typeName is the upper-case type go-mssqldb reports.
func encode(v any, typeName string, kind driver.ValueKind) any {
	switch x := v.(type) {
	case []byte:
		switch kind {
		case driver.KindUUID:
			return uuidCell(x)
		case driver.KindGeometry:
			return spatialCell(x, typeName == "GEOGRAPHY")
		}
	case time.Time:
		if typeName == "DATETIME" || typeName == "SMALLDATETIME" {
			// These types count 1/300 s; the server only parses three
			// fractional digits back, so round to milliseconds.
			x = x.Round(time.Millisecond)
		}
		return driver.EncodeTime(x, kind) // GenericEncode would take it for a fmt.Stringer
	}
	return sqlbase.GenericEncode(v, kind)
}

// uuidCell renders a uniqueidentifier, whose first three groups travel
// little-endian on the wire.
func uuidCell(b []byte) any {
	var u ms.UniqueIdentifier
	if err := u.Scan(b); err != nil {
		return driver.EncodeBinary(b)
	}
	return driver.EncodeUUIDBytes(u[:])
}

func (dialect) SelectExpr(c driver.Column) string {
	q := quote(c.Name)
	switch {
	case c.Kind == driver.KindGeometry:
		return "'SRID=' + CAST(" + q + ".STSrid AS varchar(12)) + ';' + " + q + ".STAsText()"
	case c.BaseType == "hierarchyid":
		return q + ".ToString()"
	}
	return q
}

func (dialect) TextExpr(c driver.Column) string {
	switch c.BaseType {
	case "char", "varchar", "nchar", "nvarchar", "sysname":
		return quote(c.Name)
	case "datetime", "smalldatetime":
		return "CONVERT(NVARCHAR(MAX), " + quote(c.Name) + ", 121)" // style 0 would be "Jan  2 2024  3:04PM"
	}
	return "CAST(" + quote(c.Name) + " AS NVARCHAR(MAX))"
}

// Like matches with the column's collation. Rowsmith escapes wildcards with
// a backslash, which T-SQL only honors with ESCAPE; '[' opens a character
// class unless escaped too.
func (dialect) Like(expr, ph string, negate bool) string {
	op := " LIKE "
	if negate {
		op = " NOT LIKE "
	}
	return expr + op + "REPLACE(" + ph + `, '[', '\[') ESCAPE '\'`
}

func (d dialect) InputExpr(col *driver.Column, v any, ph string) (string, any, error) {
	if col != nil {
		switch {
		case col.Kind == driver.KindGeometry:
			expr, arg, ok, err := geoInput(col, v, ph)
			if err != nil || ok {
				return expr, arg, err
			}
		case col.BaseType == "datetime" || col.BaseType == "smalldatetime":
			// Strings convert to datetime according to the session language
			// (2024-01-02 is 1 February for British logins); bind the value typed.
			if s, ok := v.(string); ok {
				if t, ok := parseDateTime(s); ok {
					return ph, ms.DateTime1(t), nil
				}
			}
		}
	}
	return sqlbase.DefaultInputExpr(d, col, v, ph)
}

var dateTimeLayouts = []string{"2006-01-02 15:04:05", "2006-01-02T15:04:05", "2006-01-02 15:04", "2006-01-02T15:04", "2006-01-02"}

func parseDateTime(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	for _, l := range dateTimeLayouts {
		if t, err := time.Parse(l, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// geoInput builds a geometry::STGeomFromText call from WKT, EWKT or a
// {"$geo": GeoJSON} cell; SQL Server cannot parse GeoJSON itself. ok is false
// for values it does not handle (NULL and the other edit markers).
func geoInput(col *driver.Column, v any, ph string) (expr string, arg any, ok bool, err error) {
	srid := col.SRID
	var text string
	switch x := v.(type) {
	case string:
		text = strings.TrimSpace(x)
	case map[string]any:
		g, found := x["$geo"]
		if !found {
			return "", nil, false, nil
		}
		b, _ := json.Marshal(g)
		text = string(b)
		if n, isNum := x["srid"].(float64); isNum {
			srid = int(n)
		}
	default:
		return "", nil, false, nil
	}
	if strings.HasPrefix(text, "{") {
		if text, err = geoJSONToWKT(text); err != nil {
			return "", nil, false, fmt.Errorf("column %s: %w", col.Name, err)
		}
	}
	if strings.HasPrefix(strings.ToUpper(text), "SRID=") {
		if i := strings.IndexByte(text, ';'); i > 0 {
			n, err := strconv.Atoi(text[5:i])
			if err != nil {
				return "", nil, false, fmt.Errorf("column %s: invalid SRID", col.Name)
			}
			srid, text = n, text[i+1:]
		}
	}
	typ := "geometry"
	if col.BaseType == "geography" {
		typ = "geography"
	}
	return typ + "::STGeomFromText(" + ph + ", " + strconv.Itoa(srid) + ")", text, true, nil
}

func geoJSONToWKT(s string) (string, error) {
	var g geom.T
	if err := geojson.Unmarshal([]byte(s), &g); err != nil {
		return "", fmt.Errorf("invalid GeoJSON: %w", err)
	}
	return wkt.Marshal(g)
}
