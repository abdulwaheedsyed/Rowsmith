package oracle

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/twpayne/go-geom"
	"github.com/twpayne/go-geom/encoding/geojson"
	"github.com/twpayne/go-geom/encoding/wkt"

	"rowsmith/internal/driver"
	"rowsmith/internal/driver/sqlbase"
	"rowsmith/internal/sqlsplit"
)

type dialect struct {
	sqlbase.Base
	c *conn
}

func (dialect) Split() sqlsplit.Dialect    { return sqlsplit.Oracle }
func (dialect) QuoteIdent(n string) string { return quote(n) }
func (dialect) Placeholder(n int) string   { return ":" + strconv.Itoa(n) }

func (d dialect) Qualify(r driver.ObjectRef) string { return qualify(d.c.schema(r.Schema), r.Name) }

// Paginate uses the 12c row-limiting clause.
func (dialect) Paginate(q string, limit int, offset int64, _ bool) string {
	return q + " OFFSET " + strconv.FormatInt(offset, 10) + " ROWS FETCH NEXT " + strconv.Itoa(limit) + " ROWS ONLY"
}

// wireTypes maps go-ora's wire type names to SQL type names. Inline LOBs
// arrive as LongVarChar (CLOB, NCLOB) and LongRaw (BLOB); types go-ora has
// no name for are reported by number.
var wireTypes = map[string]string{
	"NCHAR": "VARCHAR2", "CHAR": "CHAR", "NUMBER": "NUMBER", "DATE": "DATE", "LONG": "LONG", "RAW": "RAW",
	"LongVarChar": "CLOB", "LongRaw": "BLOB", "OCIClobLocator": "CLOB", "OCIBlobLocator": "BLOB", "OCIFileLocator": "BFILE",
	"TimeStampDTY": "TIMESTAMP", "TIMESTAMP": "TIMESTAMP", "TimeStampTZ_DTY": "TIMESTAMP WITH TIME ZONE", "TimeStampTZ": "TIMESTAMP WITH TIME ZONE",
	"TimeStampLTZ_DTY": "TIMESTAMP WITH LOCAL TIME ZONE", "TimeStampeLTZ": "TIMESTAMP WITH LOCAL TIME ZONE",
	"IntervalYM_DTY": "INTERVAL YEAR TO MONTH", "IntervalYM": "INTERVAL YEAR TO MONTH",
	"IntervalDS_DTY": "INTERVAL DAY TO SECOND", "IntervalDS": "INTERVAL DAY TO SECOND",
	"IBFloat": "BINARY_FLOAT", "IBDouble": "BINARY_DOUBLE", "ROWID": "ROWID", "UROWID": "UROWID",
	"XMLType": "XMLTYPE", "REFCURSOR": "REF CURSOR", "TNSType(119)": "JSON", "TNSType(127)": "VECTOR", "TNSType(252)": "BOOLEAN",
}

func sqlTypeName(wire string) string {
	if t, ok := wireTypes[wire]; ok {
		return t
	}
	return wire
}

func (dialect) Kind(ct *sql.ColumnType) driver.ValueKind {
	switch t := sqlTypeName(ct.DatabaseTypeName()); t {
	case "NUMBER":
		p, s, ok := ct.DecimalSize()
		return numberKind(ok, p, ok, s)
	case "DATE":
		return driver.KindDateTime
	case "JSON":
		return driver.KindJSON
	case "BOOLEAN":
		return driver.KindBool
	case "XMLTYPE", "VECTOR":
		return driver.KindText
	case "REF CURSOR":
		return driver.KindOther
	default:
		return driver.KindFromTypeName(t)
	}
}

// numberKind treats NUMBER(p,0) with p <= 18 as an integer; anything wider,
// scaled or unconstrained stays an exact decimal string.
func numberKind(hasPrec bool, prec int64, hasScale bool, scale int64) driver.ValueKind {
	if hasPrec && hasScale && scale == 0 && prec > 0 && prec <= 18 {
		return driver.KindInt
	}
	return driver.KindDecimal
}

func (dialect) Encode(v any, ct *sql.ColumnType, kind driver.ValueKind) any {
	switch x := v.(type) {
	case string: // go-ora returns NUMBER as exact decimal text
		switch kind {
		case driver.KindInt:
			if n, err := strconv.ParseInt(x, 10, 64); err == nil {
				return driver.EncodeInt(n)
			}
			return x
		case driver.KindDecimal:
			return x
		}
	case time.Time:
		// Handled here: sqlbase.GenericEncode would render it as a fmt.Stringer.
		return driver.EncodeTime(x, kind)
	case float32:
		// Keep the shortest decimal form rather than float32's binary expansion.
		f, _ := strconv.ParseFloat(strconv.FormatFloat(float64(x), 'g', -1, 32), 64)
		return driver.EncodeFloat(f)
	case []byte:
		if kind == driver.KindJSON {
			return driver.EncodeText(string(x))
		}
	}
	return sqlbase.GenericEncode(v, kind)
}

// SelectExpr converts values go-ora cannot decode into text. Other object
// types (user-defined types, collections, REFs) are shown by type name, and
// SDO_GEOMETRY degrades the same way when Oracle Spatial is not available.
func (dialect) SelectExpr(c driver.Column) string {
	q := quote(c.Name)
	switch {
	case c.Kind == driver.KindGeometry:
		return "SDO_UTIL.TO_WKTGEOMETRY(" + q + ")"
	case c.BaseType == "json":
		return "JSON_SERIALIZE(" + q + " RETURNING CLOB)"
	case c.BaseType == "vector":
		return "VECTOR_SERIALIZE(" + q + " RETURNING CLOB)"
	case c.Kind == driver.KindOther:
		return "CASE WHEN " + q + " IS NOT NULL THEN " + literal("("+c.Type+")") + " END"
	}
	return q
}

func (dialect) TextExpr(c driver.Column) string {
	q := quote(c.Name)
	switch {
	case c.BaseType == "long":
		return "NULL" // LONG cannot be passed to functions; quick search skips it
	case c.BaseType == "xmltype":
		return "XMLSERIALIZE(CONTENT " + q + " AS CLOB)"
	case c.BaseType == "json":
		return "JSON_SERIALIZE(" + q + " RETURNING CLOB)"
	case c.BaseType == "date":
		return "TO_CHAR(" + q + ", 'YYYY-MM-DD HH24:MI:SS')"
	case c.Kind == driver.KindDateTime:
		return "TO_CHAR(" + q + ", 'YYYY-MM-DD HH24:MI:SS.FF')"
	case c.Kind == driver.KindTimestamp:
		return "TO_CHAR(" + q + ", 'YYYY-MM-DD HH24:MI:SS.FF TZH:TZM')"
	case c.Kind == driver.KindString, c.Kind == driver.KindText:
		return q
	}
	return "TO_CHAR(" + q + ")"
}

// Like matches case-insensitively. Oracle has no default LIKE escape, so the
// backslash used by the shared search code is declared explicitly.
func (dialect) Like(expr, ph string, negate bool) string {
	op := " LIKE "
	if negate {
		op = " NOT LIKE "
	}
	return "UPPER(" + expr + ")" + op + "UPPER(" + ph + ") ESCAPE '\\'"
}

func (dialect) Regexp(expr, ph string) string { return "REGEXP_LIKE(" + expr + ", " + ph + ", 'i')" }

func (d dialect) InputExpr(col *driver.Column, v any, ph string) (string, any, error) {
	if x, ok := v.(bool); ok {
		// NUMBER(1) flags and 23ai BOOLEAN both accept 1 and 0.
		if x {
			return ph, 1, nil
		}
		return ph, 0, nil
	}
	if col != nil {
		switch {
		case col.Kind == driver.KindDateTime || col.Kind == driver.KindTimestamp:
			if s, ok := v.(string); ok {
				return timeInput(col, s, ph)
			}
		case col.Kind == driver.KindGeometry:
			if expr, arg, ok, err := geometryInput(col, v, ph); ok || err != nil {
				return expr, arg, err
			}
		case col.BaseType == "xmltype":
			if s, ok := v.(string); ok {
				return "XMLTYPE(" + ph + ")", s, nil
			}
		}
	}
	return sqlbase.DefaultInputExpr(d, col, v, ph)
}

var timeLayouts = []struct {
	layout string
	zone   bool
}{
	{"2006-01-02 15:04:05Z07:00", true}, {"2006-01-02T15:04:05Z07:00", true},
	{"2006-01-02 15:04:05 Z07:00", true}, {"2006-01-02 15:04:05-0700", true}, {"2006-01-02 15:04:05 -0700", true},
	{"2006-01-02 15:04:05", false}, {"2006-01-02T15:04:05", false},
	{"2006-01-02 15:04", false}, {"2006-01-02T15:04", false}, {"2006-01-02", false},
}

// parseTime accepts the grid's rendering (see driver.EncodeTime) and common
// ISO 8601 variants, with optional fractional seconds and offset.
func parseTime(s string) (time.Time, bool, bool) {
	s = strings.TrimSpace(s)
	for _, l := range timeLayouts {
		if t, err := time.Parse(l.layout, s); err == nil {
			return t, l.zone, true
		}
	}
	return time.Time{}, false, false
}

// timeInput binds an edited date or timestamp as text with an explicit ISO
// format, so the session's NLS settings never matter.
func timeInput(col *driver.Column, s, ph string) (string, any, error) {
	t, zone, ok := parseTime(s)
	if !ok {
		return "", nil, fmt.Errorf("column %s: %q is not a valid date/time (use YYYY-MM-DD HH:MM:SS)", col.Name, s)
	}
	switch {
	case col.BaseType == "date":
		return "TO_DATE(" + ph + ", 'YYYY-MM-DD HH24:MI:SS')", t.Format("2006-01-02 15:04:05"), nil
	case col.Kind == driver.KindTimestamp && zone:
		return "TO_TIMESTAMP_TZ(" + ph + ", 'YYYY-MM-DD HH24:MI:SS.FF9 TZH:TZM')", t.Format("2006-01-02 15:04:05.000000000 -07:00"), nil
	default:
		return "TO_TIMESTAMP(" + ph + ", 'YYYY-MM-DD HH24:MI:SS.FF9')", t.Format("2006-01-02 15:04:05.000000000"), nil
	}
}

// geometryInput accepts WKT, EWKT or GeoJSON (text or a {"$geo": ...} cell)
// and builds an SDO_GEOMETRY from well-known text.
func geometryInput(col *driver.Column, v any, ph string) (string, any, bool, error) {
	var text string
	switch x := v.(type) {
	case string:
		text = strings.TrimSpace(x)
	case map[string]any:
		g, ok := x["$geo"]
		if !ok {
			return "", nil, false, nil
		}
		b, err := json.Marshal(g)
		if err != nil {
			return "", nil, false, err
		}
		text = string(b)
	default:
		return "", nil, false, nil
	}
	srid := col.SRID
	switch {
	case strings.HasPrefix(text, "{"):
		var g geom.T
		if err := geojson.Unmarshal([]byte(text), &g); err != nil {
			return "", nil, false, fmt.Errorf("column %s: invalid GeoJSON: %w", col.Name, err)
		}
		s, err := wkt.Marshal(g)
		if err != nil {
			return "", nil, false, fmt.Errorf("column %s: %w", col.Name, err)
		}
		text = s
	case strings.HasPrefix(strings.ToUpper(text), "SRID="):
		prefix, rest, _ := strings.Cut(text[5:], ";")
		n, err := strconv.Atoi(prefix)
		if err != nil {
			return "", nil, false, fmt.Errorf("column %s: invalid SRID in %q", col.Name, text)
		}
		srid, text = n, rest
	}
	expr := "SDO_GEOMETRY(TO_CLOB(" + ph + "))"
	if srid != 0 {
		expr = "SDO_GEOMETRY(TO_CLOB(" + ph + "), " + strconv.Itoa(srid) + ")"
	}
	return expr, text, true, nil
}
