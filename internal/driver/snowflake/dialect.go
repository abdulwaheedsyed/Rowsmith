package snowflake

import (
	"bytes"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"rowsmith/internal/driver"
	"rowsmith/internal/driver/sqlbase"
	"rowsmith/internal/sqlsplit"
)

type dialect struct{ sqlbase.Base }

func (dialect) Split() sqlsplit.Dialect             { return sqlsplit.Snowflake }
func (dialect) QuoteIdent(name string) string       { return quote(name) }
func (dialect) Qualify(ref driver.ObjectRef) string { return qualify(ref) }
func (dialect) Placeholder(int) string              { return "?" }

// InsertsFromSelect implements sqlbase.SelectInserter: Snowflake refuses
// PARSE_JSON and TO_GEOGRAPHY in INSERT … VALUES.
func (dialect) InsertsFromSelect() bool { return true }

// Kind maps a result column's type, as gosnowflake names it.
func (dialect) Kind(ct *sql.ColumnType) driver.ValueKind {
	switch strings.ToUpper(ct.DatabaseTypeName()) {
	case "FIXED", "NUMBER", "DECIMAL":
		if _, scale, ok := ct.DecimalSize(); ok && scale > 0 {
			return driver.KindDecimal
		}
		return driver.KindInt
	case "REAL", "FLOAT", "DOUBLE":
		return driver.KindFloat
	case "TEXT", "VARCHAR", "STRING":
		if n, ok := ct.Length(); ok && n > 0 && n <= 255 {
			return driver.KindString
		}
		return driver.KindText
	case "BOOLEAN":
		return driver.KindBool
	case "BINARY":
		return driver.KindBinary
	case "DATE":
		return driver.KindDate
	case "TIME":
		return driver.KindTime
	case "TIMESTAMP_NTZ":
		return driver.KindDateTime
	case "TIMESTAMP_TZ", "TIMESTAMP_LTZ":
		return driver.KindTimestamp
	case "VARIANT", "OBJECT", "ARRAY", "MAP":
		return driver.KindJSON
	case "VECTOR":
		return driver.KindArray
	}
	return driver.KindOther
}

// Encode turns a scanned value into a cell. Numbers arrive as exact text;
// semi-structured values as indented JSON, sent compacted.
func (dialect) Encode(v any, ct *sql.ColumnType, kind driver.ValueKind) any {
	switch x := v.(type) {
	case nil:
		return nil
	case time.Time:
		switch kind {
		case driver.KindDate:
			return x.Format("2006-01-02")
		case driver.KindTime:
			return strings.TrimSuffix(strings.TrimRight(x.Format("15:04:05.000000000"), "0"), ".")
		case driver.KindDateTime:
			return driver.EncodeTime(x, driver.KindDateTime)
		}
		return driver.EncodeTime(x, driver.KindTimestamp)
	case string:
		switch kind {
		case driver.KindInt:
			if n, err := strconv.ParseInt(x, 10, 64); err == nil {
				return driver.EncodeInt(n)
			}
			return x
		case driver.KindDecimal:
			return x
		case driver.KindFloat:
			if f, err := strconv.ParseFloat(x, 64); err == nil {
				return driver.EncodeFloat(f)
			}
			return x
		case driver.KindJSON, driver.KindArray, driver.KindGeometry:
			var b bytes.Buffer
			if json.Compact(&b, []byte(x)) == nil {
				return driver.EncodeText(b.String())
			}
		}
		return driver.EncodeText(x)
	case []byte:
		return driver.EncodeBinary(x)
	case float64:
		if kind == driver.KindDecimal {
			return strconv.FormatFloat(x, 'f', -1, 64)
		}
		return driver.EncodeFloat(x)
	}
	return sqlbase.GenericEncode(v, kind)
}

// SelectExpr sends shapes as GeoJSON text.
func (dialect) SelectExpr(c driver.Column) string {
	if c.Kind == driver.KindGeometry {
		return "TO_VARCHAR(ST_ASGEOJSON(" + quote(c.Name) + "))"
	}
	return quote(c.Name)
}

func (dialect) TextExpr(c driver.Column) string {
	switch c.Kind {
	case driver.KindString, driver.KindText:
		return quote(c.Name)
	}
	return "TO_VARCHAR(" + quote(c.Name) + ")"
}

func (dialect) Like(expr, ph string, negate bool) string {
	if negate {
		return expr + " NOT ILIKE " + ph
	}
	return expr + " ILIKE " + ph
}

func (dialect) Regexp(expr, ph string) string { return "REGEXP_LIKE(" + expr + ", " + ph + ")" }

func (dialect) Paginate(q string, limit int, offset int64, ordered bool) string {
	return sqlbase.Base{}.Paginate(q, limit, offset, ordered)
}

func (dialect) LimitOne() (string, string) { return "", "" }

// InputExpr writes an edited value: JSON through PARSE_JSON, shapes through
// TO_GEOGRAPHY or TO_GEOMETRY (from GeoJSON or WKT), binary as bytes.
func (d dialect) InputExpr(col *driver.Column, v any, ph string) (string, any, error) {
	if col != nil {
		if m, ok := v.(map[string]any); ok && (m["$null"] == true || m["$default"] == true || m["$expr"] != nil) {
			return sqlbase.DefaultInputExpr(d, col, v, ph)
		}
		switch col.Kind {
		case driver.KindJSON, driver.KindArray:
			if v == nil {
				return "NULL", nil, nil
			}
			text, err := jsonText(v)
			if err != nil {
				return "", nil, fmt.Errorf("column %s: %w", col.Name, err)
			}
			return "PARSE_JSON(" + ph + ")", text, nil
		case driver.KindGeometry:
			if v == nil {
				return "NULL", nil, nil
			}
			fn := "TO_GEOGRAPHY("
			if col.BaseType == "geometry" {
				fn = "TO_GEOMETRY("
			}
			switch x := v.(type) {
			case string:
				return fn + ph + ")", x, nil
			case map[string]any:
				if g, ok := x["$geo"]; ok {
					b, _ := json.Marshal(g)
					return fn + ph + ")", string(b), nil
				}
			}
			return "", nil, fmt.Errorf("column %s: a shape is GeoJSON or WKT text", col.Name)
		case driver.KindBinary:
			if m, ok := v.(map[string]any); ok {
				if b64, ok := m["$bin"].(string); ok {
					raw, err := base64.StdEncoding.DecodeString(b64)
					if err != nil {
						return "", nil, fmt.Errorf("column %s: invalid binary value", col.Name)
					}
					return "TO_BINARY(" + ph + ", 'HEX')", fmt.Sprintf("%X", raw), nil
				}
			}
		case driver.KindBool:
			if s, ok := v.(string); ok {
				return "TO_BOOLEAN(" + ph + ")", s, nil
			}
		}
	}
	return sqlbase.DefaultInputExpr(d, col, v, ph)
}

// jsonText reads a semi-structured cell: JSON text as typed, or a value to encode.
func jsonText(v any) (string, error) {
	switch x := v.(type) {
	case string:
		if json.Valid([]byte(x)) {
			return x, nil
		}
		b, _ := json.Marshal(x) // plain text becomes a JSON string
		return string(b), nil
	case json.RawMessage:
		return string(x), nil
	}
	b, err := json.Marshal(v)
	return string(b), err
}
