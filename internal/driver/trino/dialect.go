package trino

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/trinodb/trino-go-client/trino"

	"rowsmith/internal/driver"
	"rowsmith/internal/driver/sqlbase"
	"rowsmith/internal/sqlsplit"
)

type dialect struct{ sqlbase.Base }

func (dialect) Split() sqlsplit.Dialect             { return sqlsplit.Trino }
func (dialect) QuoteIdent(name string) string       { return quote(name) }
func (dialect) Qualify(ref driver.ObjectRef) string { return qualify(ref) }
func (dialect) Placeholder(int) string              { return "?" }
func (dialect) LimitOne() (string, string)          { return "", "" }

// Autocommit: Iceberg and Delta Lake take writes only outside transactions.
func (dialect) Autocommit() bool { return true }

// Paginate puts OFFSET before LIMIT, as Trino's grammar has them.
func (dialect) Paginate(q string, limit int, offset int64, _ bool) string {
	if offset > 0 {
		q += " OFFSET " + strconv.FormatInt(offset, 10)
	}
	if limit > 0 {
		q += " LIMIT " + strconv.Itoa(limit)
	}
	return q
}

func (dialect) Kind(ct *sql.ColumnType) driver.ValueKind { return kindOf(ct.DatabaseTypeName()) }

// Encode turns a scanned value into a cell. The client decodes arrays,
// maps and rows into Go values: rows as trino.Row with their field names,
// VARIANT values as trino.Variant, which reads as JSON.
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
	case []any:
		typ := ""
		if ct != nil {
			typ = ct.DatabaseTypeName()
		}
		return encodeNested(x, typ)
	case map[string]any:
		return encodeNested(x, "")
	case trino.Row, trino.Variant:
		typ := ""
		if ct != nil {
			typ = ct.DatabaseTypeName()
		}
		return encodeNested(x, typ)
	}
	return sqlbase.GenericEncode(v, kind)
}

// encodeNested encodes arrays, maps (keys sorted) and rows (as objects
// keyed by the row type's field names) for the grid.
func encodeNested(v any, typ string) any {
	switch x := v.(type) {
	case []any:
		elem := elementType(typ)
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = encodeNested(e, elem)
		}
		return out
	case trino.Row:
		if !x.Valid {
			return nil
		}
		fieldTypes := rowFieldTypes(typ)
		d := driver.Doc{Values: map[string]any{}}
		for i := range x.Len() {
			name, ft := x.Name(i), ""
			if len(fieldTypes) == x.Len() {
				ft = fieldTypes[i]
			}
			if _, dup := d.Values[name]; !dup {
				d.Keys = append(d.Keys, name)
			}
			d.Values[name] = encodeNested(x.Value(i), ft)
		}
		return d
	case trino.Variant:
		if !x.Valid {
			return nil
		}
		b, err := x.MarshalJSON()
		if err != nil {
			return x.String()
		}
		dec := json.NewDecoder(strings.NewReader(string(b)))
		dec.UseNumber() // numbers stay exact
		var out any
		if dec.Decode(&out) != nil {
			return string(b)
		}
		return encodeNested(out, "")
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		d := driver.Doc{Keys: keys, Values: map[string]any{}}
		for _, k := range keys {
			d.Values[k] = encodeNested(x[k], "")
		}
		return d
	case time.Time:
		return x.Format("2006-01-02 15:04:05.999999999")
	case []byte:
		return driver.EncodeBinary(x)
	case float64:
		return driver.EncodeFloat(x)
	case int64:
		return driver.EncodeInt(x)
	}
	return v
}

func rowFieldTypes(typ string) []string {
	parts := rowParts(typ)
	out := make([]string, len(parts))
	for i, p := range parts {
		_, t, _ := strings.Cut(strings.TrimSpace(p), " ")
		out[i] = strings.TrimSpace(t)
	}
	return out
}

// rowParts splits the fields of ROW(…) at top-level commas.
func rowParts(typ string) []string {
	t := strings.TrimSpace(typ)
	if !strings.HasPrefix(strings.ToUpper(t), "ROW(") || !strings.HasSuffix(t, ")") {
		return nil
	}
	return splitTop(t[4 : len(t)-1])
}

// elementType reads T from ARRAY(T).
func elementType(typ string) string {
	t := strings.TrimSpace(typ)
	if strings.HasPrefix(strings.ToUpper(t), "ARRAY(") && strings.HasSuffix(t, ")") {
		return t[6 : len(t)-1]
	}
	return ""
}

func splitTop(s string) []string {
	var out []string
	depth, start := 0, 0
	for i, r := range s {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	return append(out, s[start:])
}

func (dialect) TextExpr(c driver.Column) string {
	switch c.Kind {
	case driver.KindString, driver.KindText:
		return quote(c.Name)
	case driver.KindArray, driver.KindObject, driver.KindJSON:
		return "json_format(CAST(" + quote(c.Name) + " AS json))"
	}
	return "CAST(" + quote(c.Name) + " AS varchar)"
}

// Like compares case-insensitively; sqlbase escapes % and _ with \.
func (dialect) Like(expr, ph string, negate bool) string {
	op := " LIKE "
	if negate {
		op = " NOT LIKE "
	}
	return "lower(" + expr + ")" + op + "lower(" + ph + ") ESCAPE '\\'"
}

func (dialect) Regexp(expr, ph string) string { return "regexp_like(" + expr + ", " + ph + ")" }

// TypedArg casts a filter value to the column's type: Trino compares only
// values of one type.
func (dialect) TypedArg(c *driver.Column, ph string) string {
	switch c.Kind {
	case driver.KindString, driver.KindText, driver.KindArray, driver.KindObject, driver.KindJSON, driver.KindGeometry, driver.KindOther:
		return ph
	}
	return "CAST(" + ph + " AS " + c.Type + ")"
}

var zoneRe = regexp.MustCompile(`(\d)([+-]\d\d:?\d\d|Z)$`)

// InputExpr writes an edited value as the column's type: Trino does not
// coerce a text or double parameter into a bigint, decimal or timestamp.
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
		return "json_parse(" + ph + ")", text, nil
	case driver.KindArray, driver.KindObject:
		text, err := jsonText(v)
		if err != nil {
			return "", nil, fmt.Errorf("column %s: %w", col.Name, err)
		}
		return "CAST(json_parse(" + ph + ") AS " + col.Type + ")", text, nil
	case driver.KindBinary:
		if m, ok := v.(map[string]any); ok {
			if b64, ok := m["$bin"].(string); ok {
				if _, err := base64.StdEncoding.DecodeString(b64); err != nil {
					return "", nil, fmt.Errorf("column %s: invalid binary value", col.Name)
				}
				return "from_base64(" + ph + ")", b64, nil
			}
		}
		return "to_utf8(" + ph + ")", fmt.Sprint(v), nil
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
		s = zoneRe.ReplaceAllString(strings.Replace(s, "T", " ", 1), "$1 $2") // Trino wants a space before the zone
	}
	return "CAST(" + ph + " AS " + col.Type + ")", s, nil
}

// jsonText reads a JSON cell: JSON text as typed, or a value to encode.
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

func (dialect) SelectExpr(c driver.Column) string { return quote(c.Name) }
