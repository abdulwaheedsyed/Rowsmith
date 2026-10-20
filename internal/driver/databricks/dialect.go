package databricks

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"rowsmith/internal/driver"
	"rowsmith/internal/driver/sqlbase"
	"rowsmith/internal/sqlsplit"
)

type dialect struct{ sqlbase.Base }

func (dialect) Split() sqlsplit.Dialect             { return sqlsplit.Databricks }
func (dialect) QuoteIdent(name string) string       { return quote(name) }
func (dialect) Qualify(ref driver.ObjectRef) string { return qualify(ref) }
func (dialect) Placeholder(int) string              { return "?" }
func (dialect) LimitOne() (string, string)          { return "", "" }

// Autocommit: each statement on a SQL warehouse commits on its own, so edits
// check every row they match before writing.
func (dialect) Autocommit() bool { return true }

func (dialect) Paginate(q string, limit int, offset int64, _ bool) string {
	if limit > 0 {
		q += " LIMIT " + strconv.Itoa(limit)
	}
	if offset > 0 {
		q += " OFFSET " + strconv.FormatInt(offset, 10)
	}
	return q
}

// kindOf maps a Databricks type (as information_schema or a result column
// names it: BIGINT, DECIMAL(10,2), ARRAY<STRING>, STRUCT<…>…) to a kind.
func kindOf(typ string) driver.ValueKind {
	t := strings.ToUpper(strings.TrimSpace(typ))
	base := t
	if i := strings.IndexAny(t, "(<"); i > 0 {
		base = t[:i]
	}
	switch base {
	case "TINYINT", "BYTE", "SMALLINT", "SHORT", "INT", "INTEGER", "BIGINT", "LONG":
		return driver.KindInt
	case "FLOAT", "REAL", "DOUBLE":
		return driver.KindFloat
	case "DECIMAL", "DEC", "NUMERIC":
		return driver.KindDecimal
	case "BOOLEAN":
		return driver.KindBool
	case "STRING", "VARCHAR", "CHAR":
		return driver.KindString
	case "BINARY":
		return driver.KindBinary
	case "DATE":
		return driver.KindDate
	case "TIMESTAMP", "TIMESTAMP_LTZ":
		return driver.KindTimestamp
	case "TIMESTAMP_NTZ":
		return driver.KindDateTime
	case "ARRAY":
		return driver.KindArray
	case "MAP", "STRUCT":
		return driver.KindObject
	case "VARIANT", "OBJECT":
		return driver.KindJSON
	case "INTERVAL":
		return driver.KindInterval
	case "GEOMETRY", "GEOGRAPHY":
		return driver.KindText // read as WKT
	}
	return driver.KindOther
}

func (dialect) Kind(ct *sql.ColumnType) driver.ValueKind { return kindOf(ct.DatabaseTypeName()) }

// Encode turns a scanned value into a cell. Arrays, maps, structs and
// variants arrive as JSON text.
func (dialect) Encode(v any, ct *sql.ColumnType, kind driver.ValueKind) any {
	switch x := v.(type) {
	case nil:
		return nil
	case time.Time:
		switch kind {
		case driver.KindDate:
			return x.Format("2006-01-02")
		case driver.KindDateTime:
			return driver.EncodeTime(x, driver.KindDateTime)
		}
		return driver.EncodeTime(x.UTC(), driver.KindTimestamp)
	case float32:
		f, _ := strconv.ParseFloat(strconv.FormatFloat(float64(x), 'g', -1, 32), 64)
		return driver.EncodeFloat(f)
	case int8:
		return int64(x)
	case int16:
		return int64(x)
	case int32:
		return int64(x)
	case string:
		switch kind {
		case driver.KindArray, driver.KindObject, driver.KindJSON:
			if v, ok := orderedJSON(x); ok {
				return v
			}
		case driver.KindDecimal:
			return x // exact
		}
	}
	return sqlbase.GenericEncode(v, kind)
}

// orderedJSON decodes JSON keeping object keys in order, as Doc cells.
func orderedJSON(s string) (any, bool) {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	v, err := decodeValue(dec)
	if err != nil || dec.More() {
		return nil, false
	}
	return v, true
}

func decodeValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		if t == '[' {
			out := []any{}
			for dec.More() {
				v, err := decodeValue(dec)
				if err != nil {
					return nil, err
				}
				out = append(out, v)
			}
			_, err := dec.Token()
			return out, err
		}
		d := driver.Doc{Values: map[string]any{}}
		for dec.More() {
			k, err := dec.Token()
			if err != nil {
				return nil, err
			}
			key, _ := k.(string)
			v, err := decodeValue(dec)
			if err != nil {
				return nil, err
			}
			d.Keys = append(d.Keys, key)
			d.Values[key] = v
		}
		_, err := dec.Token()
		return d, err
	case json.Number:
		if i, err := t.Int64(); err == nil {
			return driver.EncodeInt(i), nil
		}
		f, err := t.Float64()
		if err != nil {
			return t.String(), nil
		}
		return driver.EncodeFloat(f), nil
	case string:
		return driver.EncodeText(t), nil
	}
	return tok, nil // bool or nil
}

func (dialect) SelectExpr(c driver.Column) string { return quote(c.Name) }

func (dialect) TextExpr(c driver.Column) string {
	switch c.Kind {
	case driver.KindString:
		return quote(c.Name)
	case driver.KindArray, driver.KindObject:
		return "to_json(" + quote(c.Name) + ")"
	}
	return "CAST(" + quote(c.Name) + " AS STRING)"
}

// Like compares case-insensitively; sqlbase escapes % and _ with \, Spark's
// default escape.
func (dialect) Like(expr, ph string, negate bool) string {
	op := " LIKE "
	if negate {
		op = " NOT LIKE "
	}
	return "lower(" + expr + ")" + op + "lower(" + ph + ")"
}

func (dialect) Regexp(expr, ph string) string { return expr + " RLIKE " + ph }

// TypedArg casts a filter value to the column's type, as ANSI mode wants.
func (dialect) TypedArg(c *driver.Column, ph string) string {
	switch c.Kind {
	case driver.KindString, driver.KindText, driver.KindArray, driver.KindObject, driver.KindJSON, driver.KindOther:
		return ph
	}
	return "CAST(" + ph + " AS " + c.Type + ")"
}

var zoneRe = regexp.MustCompile(`([+-]\d\d):?(\d\d)$`)

// InputExpr writes an edited value in the column's type: nested types from
// JSON text, binary from base64, everything else cast from text.
func (d dialect) InputExpr(col *driver.Column, v any, ph string) (string, any, error) {
	if col == nil {
		return sqlbase.DefaultInputExpr(d, col, v, ph)
	}
	if m, ok := v.(map[string]any); ok && (m["$null"] == true || m["$default"] == true || m["$expr"] != nil) {
		return sqlbase.DefaultInputExpr(d, col, v, ph)
	}
	if v == nil {
		return "NULL", nil, nil
	}
	switch col.Kind {
	case driver.KindString, driver.KindText:
		if s, ok := v.(string); ok {
			return ph, s, nil
		}
		return ph, fmt.Sprint(v), nil
	case driver.KindJSON:
		text, err := jsonText(v)
		if err != nil {
			return "", nil, fmt.Errorf("column %s: %w", col.Name, err)
		}
		return "parse_json(" + ph + ")", text, nil
	case driver.KindArray, driver.KindObject:
		text, err := jsonText(v)
		if err != nil {
			return "", nil, fmt.Errorf("column %s: %w", col.Name, err)
		}
		return "from_json(" + ph + ", " + literal(col.Type) + ")", text, nil
	case driver.KindBinary:
		if m, ok := v.(map[string]any); ok {
			if b64, ok := m["$bin"].(string); ok {
				if _, err := base64.StdEncoding.DecodeString(b64); err != nil {
					return "", nil, fmt.Errorf("column %s: invalid binary value", col.Name)
				}
				return "unbase64(" + ph + ")", b64, nil
			}
		}
		return "CAST(" + ph + " AS BINARY)", fmt.Sprint(v), nil
	}
	s := ""
	switch x := v.(type) {
	case string:
		s = x
	case float64:
		if x == math.Trunc(x) && math.Abs(x) < 1e15 {
			s = strconv.FormatInt(int64(x), 10)
		} else {
			s = strconv.FormatFloat(x, 'g', -1, 64)
		}
	case bool:
		s = strconv.FormatBool(x)
	default:
		s = fmt.Sprint(v)
	}
	if col.Kind == driver.KindTimestamp {
		s = zoneRe.ReplaceAllString(s, "$1:$2")
	}
	return "CAST(" + ph + " AS " + col.Type + ")", s, nil
}

func jsonText(v any) (string, error) {
	if s, ok := v.(string); ok {
		if json.Valid([]byte(s)) {
			return s, nil
		}
		b, _ := json.Marshal(s)
		return string(b), nil
	}
	b, err := json.Marshal(v)
	return string(b), err
}
