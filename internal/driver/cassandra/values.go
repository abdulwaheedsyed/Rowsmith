package cassandra

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"net"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	gocql "github.com/apache/cassandra-gocql-driver/v2"
	"gopkg.in/inf.v0"

	"rowsmith/internal/driver"
)

// kindOfType maps a CQL type as system_schema writes it (text,
// frozen<map<text, int>>, vector<float, 3>, a user type's name…) to a value
// kind.
func kindOfType(typ string) driver.ValueKind {
	t := strings.ToLower(strings.TrimSpace(typ))
	for strings.HasPrefix(t, "frozen<") && strings.HasSuffix(t, ">") {
		t = strings.TrimSpace(t[7 : len(t)-1])
	}
	base := t
	if i := strings.IndexAny(t, "<("); i > 0 {
		base = strings.TrimSpace(t[:i])
	}
	switch base {
	case "text", "varchar", "ascii", "inet":
		return driver.KindString
	case "int", "bigint", "smallint", "tinyint", "counter":
		return driver.KindInt
	case "varint", "decimal":
		return driver.KindDecimal
	case "float", "double":
		return driver.KindFloat
	case "boolean":
		return driver.KindBool
	case "uuid", "timeuuid":
		return driver.KindUUID
	case "timestamp":
		return driver.KindTimestamp
	case "date":
		return driver.KindDate
	case "time":
		return driver.KindTime
	case "duration":
		return driver.KindInterval
	case "blob":
		return driver.KindBinary
	case "list", "set", "vector", "tuple":
		return driver.KindArray
	case "map":
		return driver.KindObject
	}
	if strings.Contains(base, "'") || strings.Contains(base, ".") {
		return driver.KindOther // a custom type class
	}
	return driver.KindObject // a user type
}

// typeName writes a result column's type in CQL's own spelling.
func typeName(ti gocql.TypeInfo) string {
	switch t := ti.(type) {
	case gocql.CollectionType:
		switch t.Type() {
		case gocql.TypeMap:
			return "map<" + typeName(t.Key) + ", " + typeName(t.Elem) + ">"
		case gocql.TypeSet:
			return "set<" + typeName(t.Elem) + ">"
		default:
			return "list<" + typeName(t.Elem) + ">"
		}
	case gocql.TupleTypeInfo:
		parts := make([]string, len(t.Elems))
		for i, e := range t.Elems {
			parts[i] = typeName(e)
		}
		return "tuple<" + strings.Join(parts, ", ") + ">"
	case gocql.UDTTypeInfo:
		return t.Name
	case gocql.VectorType:
		return "vector<" + typeName(t.SubType) + ", " + strconv.Itoa(t.Dimensions) + ">"
	}
	if ti == nil {
		return ""
	}
	if n, ok := simpleTypes[ti.Type()]; ok {
		return n
	}
	return "custom"
}

var simpleTypes = map[gocql.Type]string{
	gocql.TypeAscii: "ascii", gocql.TypeBigInt: "bigint", gocql.TypeBlob: "blob", gocql.TypeBoolean: "boolean",
	gocql.TypeCounter: "counter", gocql.TypeDecimal: "decimal", gocql.TypeDouble: "double", gocql.TypeFloat: "float",
	gocql.TypeInt: "int", gocql.TypeText: "text", gocql.TypeTimestamp: "timestamp", gocql.TypeUUID: "uuid",
	gocql.TypeVarchar: "text", gocql.TypeVarint: "varint", gocql.TypeTimeUUID: "timeuuid", gocql.TypeInet: "inet",
	gocql.TypeDate: "date", gocql.TypeTime: "time", gocql.TypeSmallInt: "smallint", gocql.TypeTinyInt: "tinyint",
	gocql.TypeDuration: "duration",
}

// encode turns a value gocql scanned into a cell.
func encode(v any) any {
	switch x := v.(type) {
	case nil:
		return nil
	case string:
		return driver.EncodeText(x)
	case int:
		return driver.EncodeInt(int64(x))
	case int8:
		return int64(x)
	case int16:
		return int64(x)
	case int32:
		return int64(x)
	case int64:
		return driver.EncodeInt(x)
	case float32:
		// Shortest text that reads back as the same float32.
		f, _ := strconv.ParseFloat(strconv.FormatFloat(float64(x), 'g', -1, 32), 64)
		return driver.EncodeFloat(f)
	case float64:
		return driver.EncodeFloat(x)
	case bool:
		return x
	case *inf.Dec:
		if x == nil {
			return nil
		}
		return x.String()
	case *big.Int:
		if x == nil {
			return nil
		}
		return x.String()
	case gocql.UUID:
		return x.String()
	case time.Time:
		if x.IsZero() {
			return nil
		}
		return driver.EncodeTime(x.UTC(), driver.KindTimestamp)
	case time.Duration: // time of day
		return timeOfDay(x)
	case gocql.Duration:
		return durationText(x)
	case []byte:
		return driver.EncodeBinary(x)
	case net.IP:
		if x == nil {
			return nil
		}
		return x.String()
	case map[string]any: // a user type, in field order where known
		return docOf(x)
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Slice, reflect.Array:
		if rv.Kind() == reflect.Slice && rv.IsNil() {
			return nil
		}
		out := make([]any, rv.Len())
		for i := range out {
			out[i] = encode(rv.Index(i).Interface())
		}
		return out
	case reflect.Map:
		if rv.IsNil() {
			return nil
		}
		d := driver.Doc{Values: map[string]any{}}
		for _, k := range rv.MapKeys() {
			key := fmt.Sprint(encode(k.Interface()))
			d.Keys = append(d.Keys, key)
			d.Values[key] = encode(rv.MapIndex(k).Interface())
		}
		sort.Strings(d.Keys)
		return d
	case reflect.Pointer:
		if rv.IsNil() {
			return nil
		}
		return encode(rv.Elem().Interface())
	}
	return fmt.Sprint(v)
}

func docOf(m map[string]any) driver.Doc {
	d := driver.Doc{Values: map[string]any{}}
	for k, v := range m {
		d.Keys = append(d.Keys, k)
		d.Values[k] = encode(v)
	}
	sort.Strings(d.Keys)
	return d
}

// udtDoc orders a user type's fields as the type declares them.
func udtDoc(ti gocql.UDTTypeInfo, m map[string]any) driver.Doc {
	d := driver.Doc{Values: map[string]any{}}
	for _, f := range ti.Elements {
		d.Keys = append(d.Keys, f.Name)
		d.Values[f.Name] = encodeTyped(f.Type, m[f.Name])
	}
	return d
}

// encodeTyped encodes with the column's type at hand, which keeps user
// types' field order and dates without a time.
func encodeTyped(ti gocql.TypeInfo, v any) any {
	if v == nil {
		return nil
	}
	switch t := ti.(type) {
	case gocql.UDTTypeInfo:
		if m, ok := v.(map[string]any); ok {
			return udtDoc(t, m)
		}
	case gocql.CollectionType:
		rv := reflect.ValueOf(v)
		if t.Type() != gocql.TypeMap && rv.Kind() == reflect.Slice {
			if rv.IsNil() {
				return nil
			}
			out := make([]any, rv.Len())
			for i := range out {
				out[i] = encodeTyped(t.Elem, rv.Index(i).Interface())
			}
			return out
		}
	}
	if ti != nil && ti.Type() == gocql.TypeDate {
		if tm, ok := v.(time.Time); ok {
			return tm.UTC().Format("2006-01-02")
		}
	}
	return encode(v)
}

func timeOfDay(d time.Duration) string {
	h := d / time.Hour
	m := (d % time.Hour) / time.Minute
	s := (d % time.Minute) / time.Second
	ns := d % time.Second
	out := fmt.Sprintf("%02d:%02d:%02d", h, m, s)
	if ns > 0 {
		out += strings.TrimRight(fmt.Sprintf(".%09d", ns), "0")
	}
	return out
}

// durationText writes a duration as CQL reads it back: 1mo2d3h4m5s6ms.
func durationText(d gocql.Duration) string {
	if d.Months == 0 && d.Days == 0 && d.Nanoseconds == 0 {
		return "0s"
	}
	var b strings.Builder
	neg := d.Months < 0 || d.Days < 0 || d.Nanoseconds < 0
	if neg {
		b.WriteByte('-')
	}
	abs := func(n int64) int64 {
		if n < 0 {
			return -n
		}
		return n
	}
	months, days, ns := abs(int64(d.Months)), abs(int64(d.Days)), abs(d.Nanoseconds)
	if y := months / 12; y > 0 {
		fmt.Fprintf(&b, "%dy", y)
	}
	if mo := months % 12; mo > 0 {
		fmt.Fprintf(&b, "%dmo", mo)
	}
	if days > 0 {
		fmt.Fprintf(&b, "%dd", days)
	}
	for _, u := range []struct {
		n    int64
		unit string
	}{{int64(time.Hour), "h"}, {int64(time.Minute), "m"}, {int64(time.Second), "s"}, {int64(time.Millisecond), "ms"}, {int64(time.Microsecond), "us"}, {1, "ns"}} {
		if q := ns / u.n; q > 0 {
			fmt.Fprintf(&b, "%d%s", q, u.unit)
			ns %= u.n
		}
	}
	return b.String()
}

// ---- values written back -------------------------------------------------------

var (
	zoneColon = regexp.MustCompile(`([+-]\d\d):(\d\d)$`)
	hasZone   = regexp.MustCompile(`[+-]\d\d\d\d$`)
)

// inputExpr writes an edited or filtered value for a column as fromJson(?)
// with the value's JSON text, which the server reads in the column's own
// type (sets, maps, user types and tuples included). The markers $null and
// $expr give NULL or a CQL expression such as now() or uuid().
func inputExpr(col *driver.Column, v any) (expr string, arg any, err error) {
	if m, ok := v.(map[string]any); ok {
		switch {
		case m["$null"] == true:
			return "null", nil, nil
		case m["$default"] == true:
			return "", nil, fmt.Errorf("column %s: Cassandra columns have no defaults", col.Name)
		case m["$expr"] != nil:
			e, _ := m["$expr"].(string)
			if strings.TrimSpace(e) == "" {
				return "", nil, fmt.Errorf("column %s: empty expression", col.Name)
			}
			return e, nil, nil
		}
	}
	if v == nil {
		return "null", nil, nil
	}
	text, err := jsonValue(col, v)
	if err != nil {
		return "", nil, fmt.Errorf("column %s: %w", col.Name, err)
	}
	return "fromJson(?)", text, nil
}

// jsonValue is a cell as the JSON fromJson() expects for the column's type.
func jsonValue(col *driver.Column, v any) (string, error) {
	str := func(s string) string { b, _ := json.Marshal(s); return string(b) }
	switch col.Kind {
	case driver.KindInt, driver.KindFloat, driver.KindDecimal:
		num := ""
		switch x := v.(type) {
		case float64:
			if col.Kind != driver.KindFloat && x == math.Trunc(x) && math.Abs(x) < 1e15 {
				num = strconv.FormatInt(int64(x), 10)
			} else {
				num = strconv.FormatFloat(x, 'g', -1, 64)
			}
		case string:
			num = strings.TrimSpace(x)
			if _, err := strconv.ParseFloat(num, 64); err != nil {
				if _, ok := new(big.Int).SetString(num, 10); !ok {
					return "", fmt.Errorf("%q is not a number", x)
				}
			}
		}
		if num != "" {
			if col.BaseType == "decimal" {
				return str(num), nil // as text, which keeps the scale: 12.50 stays 12.50
			}
			return num, nil
		}
	case driver.KindBool:
		switch x := v.(type) {
		case bool:
			return strconv.FormatBool(x), nil
		case string:
			b, err := strconv.ParseBool(strings.TrimSpace(x))
			if err != nil {
				return "", fmt.Errorf("%q is not true or false", x)
			}
			return strconv.FormatBool(b), nil
		}
	case driver.KindBinary:
		switch x := v.(type) {
		case map[string]any:
			if b64, ok := x["$bin"].(string); ok {
				b, err := base64.StdEncoding.DecodeString(b64)
				if err != nil {
					return "", fmt.Errorf("invalid binary value")
				}
				return str("0x" + hex.EncodeToString(b)), nil
			}
		case string:
			if strings.HasPrefix(x, "0x") {
				return str(x), nil
			}
			return str("0x" + hex.EncodeToString([]byte(x))), nil
		}
	case driver.KindTimestamp:
		if s, ok := v.(string); ok {
			// The server reads +0000, not +00:00; a time without a zone
			// would be read in the server's zone, so it is taken as UTC.
			s = zoneColon.ReplaceAllString(strings.Replace(strings.TrimSpace(s), "T", " ", 1), "$1$2")
			if strings.HasSuffix(s, "Z") {
				s = strings.TrimSuffix(s, "Z") + "+0000"
			} else if strings.Contains(s, ":") && !hasZone.MatchString(s) {
				s += "+0000"
			}
			return str(s), nil
		}
	case driver.KindArray, driver.KindObject, driver.KindJSON, driver.KindOther:
		if s, ok := v.(string); ok {
			if json.Valid([]byte(s)) {
				return s, nil
			}
			return str(s), nil
		}
		b, err := json.Marshal(v)
		return string(b), err
	}
	switch x := v.(type) {
	case string:
		return str(x), nil
	case float64, bool:
		b, _ := json.Marshal(x)
		if col.Kind == driver.KindString || col.Kind == driver.KindUUID || col.Kind == driver.KindDate || col.Kind == driver.KindTime {
			return str(string(b)), nil
		}
		return string(b), nil
	}
	b, err := json.Marshal(v)
	return string(b), err
}

// inline writes a statement with its bound values in place, for showing
// what ran.
func inline(stmt string, args []any) string {
	if len(args) == 0 {
		return stmt
	}
	var b strings.Builder
	i := 0
	for _, r := range stmt {
		if r == '?' && i < len(args) {
			switch a := args[i].(type) {
			case string:
				b.WriteString(literal(a))
			default:
				fmt.Fprint(&b, a)
			}
			i++
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
