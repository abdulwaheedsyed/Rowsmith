package clickhouse

import (
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"net"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/paulmach/orb"
	orbwkt "github.com/paulmach/orb/encoding/wkt"

	"rowsmith/internal/driver"
	"rowsmith/internal/driver/clickhouse/chtype"
	"rowsmith/internal/driver/geo"
)

// Parsed types are cached: every cell is encoded against its column's type.
var parsed sync.Map

func typeOf(s string) chtype.Type {
	if t, ok := parsed.Load(s); ok {
		return t.(chtype.Type)
	}
	t := chtype.Parse(s)
	if m, ok := orderedMap(t); ok {
		t = m
		t.Ordered = true
	}
	parsed.Store(s, t)
	return t
}

// Maps are read as a list of key-value pairs, which keeps their order (a
// Go map wouldn't): Array(Tuple(_rs_key K, _rs_value V)) stands for Map(K, V).
const mapKey, mapValue = "_rs_key", "_rs_value"

// orderedSelect selects a Map column as its ordered pairs.
func orderedSelect(c driver.Column) (string, bool) {
	t := chtype.Parse(c.Type)
	if t.Name != "Map" || len(t.Args) != 2 {
		return "", false
	}
	return "CAST(" + quote(c.Name) + " AS Array(Tuple(" + mapKey + " " + t.Args[0].Raw + ", " + mapValue + " " + t.Args[1].Raw + ")))", true
}

// orderedMap recognizes the pairs and gives back the Map type.
func orderedMap(t chtype.Type) (chtype.Type, bool) {
	if t.Name != "Array" || len(t.Args) != 1 {
		return t, false
	}
	tup := t.Args[0]
	if tup.Name != "Tuple" || len(tup.Args) != 2 || tup.Args[0].Field != mapKey || tup.Args[1].Field != mapValue {
		return t, false
	}
	k, v := tup.Args[0], tup.Args[1]
	k.Field, v.Field = "", ""
	return chtype.Type{Name: "Map", Args: []chtype.Type{k, v}, Raw: "Map(" + k.Raw + ", " + v.Raw + ")"}, true
}

// kindOf maps a ClickHouse type to the value kind the grid renders.
func kindOf(t chtype.Type) driver.ValueKind {
	t, _ = t.Unwrap()
	if _, _, ok := t.IntBits(); ok {
		return driver.KindInt
	}
	switch {
	case t.IsDecimal():
		return driver.KindDecimal
	case t.GeoShape() != "":
		return driver.KindGeometry
	case strings.HasPrefix(t.Name, "Interval"):
		return driver.KindInterval
	}
	switch t.Name {
	case "Float32", "Float64", "BFloat16":
		return driver.KindFloat
	case "Bool":
		return driver.KindBool
	case "String", "FixedString", "IPv4", "IPv6":
		return driver.KindString
	case "UUID":
		return driver.KindUUID
	case "Date", "Date32":
		return driver.KindDate
	case "DateTime", "DateTime64":
		return driver.KindDateTime
	case "Time", "Time64":
		return driver.KindTime
	case "Enum8", "Enum16":
		return driver.KindEnum
	case "Array", "Nested":
		return driver.KindArray
	case "Map", "Tuple":
		return driver.KindObject
	case "JSON", "Object":
		return driver.KindJSON
	case "SimpleAggregateFunction":
		if len(t.Args) == 1 {
			return kindOf(t.Args[0])
		}
	case "AggregateFunction":
		return driver.KindBinary
	}
	return driver.KindOther
}

// encode turns a scanned value into its cell. Values inside arrays, maps
// and tuples are plain JSON values.
func encode(v any, t chtype.Type) any { return encodeIn(v, t, true) }

func encodeIn(v any, t chtype.Type, top bool) any {
	if v == nil {
		return nil
	}
	t, _ = t.Unwrap()
	if t.Name == "SimpleAggregateFunction" && len(t.Args) == 1 {
		t = t.Args[0]
	}
	switch x := v.(type) {
	case string:
		return encodeString(x, t, top)
	case bool:
		return x
	case int8:
		return int64(x)
	case int16:
		return int64(x)
	case int32:
		return int64(x)
	case int64:
		return driver.EncodeInt(x)
	case int:
		return driver.EncodeInt(int64(x))
	case uint8:
		return int64(x)
	case uint16:
		return int64(x)
	case uint32:
		return int64(x)
	case uint64:
		return driver.EncodeUint(x)
	case float64:
		return driver.EncodeFloat(x)
	case float32:
		f, _ := strconv.ParseFloat(strconv.FormatFloat(float64(x), 'g', -1, 32), 64)
		return driver.EncodeFloat(f)
	case big.Int:
		return bigInt(&x)
	case *big.Int:
		if x == nil {
			return nil
		}
		return bigInt(x)
	case time.Time:
		return encodeTime(x, t)
	case time.Duration:
		return clockTime(x)
	case net.IP:
		if x == nil {
			return nil
		}
		return x.String()
	case orb.Geometry:
		if cell, ok := geo.FromWKT(orbwkt.MarshalString(x)); ok {
			if top {
				return cell
			}
			if m, ok := cell.(map[string]any); ok {
				return m["$geo"]
			}
		}
		return orbwkt.MarshalString(x)
	case json.Marshaler:
		// JSON columns, and other values that know their JSON form.
		b, err := x.MarshalJSON()
		if err != nil {
			return driver.EncodeText(fmt.Sprint(v))
		}
		return driver.EncodeText(string(b))
	case fmt.Stringer:
		// UUIDs, decimals and the like render as their text.
		if !isContainer(reflect.ValueOf(v)) {
			return encodeString(x.String(), t, top)
		}
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Pointer, reflect.Interface:
		if rv.IsNil() {
			return nil
		}
		return encodeIn(rv.Elem().Interface(), t, top)
	case reflect.Slice, reflect.Array:
		if t.Ordered {
			return encodePairs(rv, t)
		}
		elem := chtype.Type{Name: "String"}
		if len(t.Args) > 0 {
			elem = t.Args[0]
		}
		out := make([]any, rv.Len())
		for i := range out {
			et := elem
			if t.Name == "Tuple" && i < len(t.Args) {
				et = t.Args[i]
			}
			out[i] = encodeIn(rv.Index(i).Interface(), et, false)
		}
		return out
	case reflect.Map:
		return encodeMap(rv, t)
	}
	return driver.EncodeText(fmt.Sprint(v))
}

// encodePairs renders a map read as its ordered pairs.
func encodePairs(rv reflect.Value, t chtype.Type) any {
	doc := driver.Doc{Values: map[string]any{}}
	for i := 0; i < rv.Len(); i++ {
		pair, ok := rv.Index(i).Interface().(map[string]any)
		if !ok {
			continue
		}
		k := fmt.Sprint(encodeIn(pair[mapKey], t.Args[0], false))
		if _, dup := doc.Values[k]; !dup {
			doc.Keys = append(doc.Keys, k)
		}
		doc.Values[k] = encodeIn(pair[mapValue], t.Args[1], false)
	}
	return doc
}

func isContainer(rv reflect.Value) bool {
	switch rv.Kind() {
	case reflect.Slice, reflect.Array, reflect.Map:
		return rv.Type().Elem().Kind() != reflect.Uint8 || rv.Kind() == reflect.Map
	}
	return false
}

// encodeMap renders a Map in key order, and a named Tuple in field order.
func encodeMap(rv reflect.Value, t chtype.Type) any {
	doc := driver.Doc{Values: map[string]any{}}
	if t.Name == "Tuple" {
		for _, f := range t.Args {
			if f.Field == "" {
				continue
			}
			doc.Keys = append(doc.Keys, f.Field)
			if mv := rv.MapIndex(reflect.ValueOf(f.Field)); mv.IsValid() {
				doc.Values[f.Field] = encodeIn(mv.Interface(), f, false)
			} else {
				doc.Values[f.Field] = nil
			}
		}
		if len(doc.Keys) > 0 {
			return doc
		}
	}
	valType := chtype.Type{Name: "String"}
	if t.Name == "Map" && len(t.Args) == 2 {
		valType = t.Args[1]
	}
	iter := rv.MapRange()
	for iter.Next() {
		k := fmt.Sprint(iter.Key().Interface())
		doc.Keys = append(doc.Keys, k)
		doc.Values[k] = encodeIn(iter.Value().Interface(), valType, false)
	}
	sort.Strings(doc.Keys)
	return doc
}

// encodeString keeps text as text and bytes (FixedString hashes, binary
// String values) as binary.
func encodeString(s string, t chtype.Type, top bool) any {
	switch t.Name {
	case "String", "FixedString":
		if !utf8.ValidString(s) || (t.Name == "FixedString" && !printable(s)) {
			if top {
				return driver.EncodeBinary([]byte(s))
			}
			return strings.ToValidUTF8(s, "�")
		}
	}
	if top {
		return driver.EncodeText(s)
	}
	return s
}

func printable(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x09 || (c > 0x0d && c < 0x20) || c == 0x7f {
			return false
		}
	}
	return true
}

var maxSafe = big.NewInt(1<<53 - 1)

func bigInt(x *big.Int) any {
	if x.CmpAbs(maxSafe) <= 0 {
		return x.Int64()
	}
	return x.String()
}

// encodeTime renders dates as dates and times in the column's time zone,
// which is the zone the value was read in.
func encodeTime(x time.Time, t chtype.Type) any {
	switch t.Name {
	case "Date", "Date32":
		return x.Format("2006-01-02")
	}
	return driver.EncodeTime(x, driver.KindDateTime)
}

// clockTime renders a Time value, which can be negative or past 24 hours.
func clockTime(d time.Duration) string {
	sign := ""
	if d < 0 {
		sign, d = "-", -d
	}
	h := int64(d / time.Hour)
	m := int64(d % time.Hour / time.Minute)
	s := d % time.Minute
	secs := strconv.FormatFloat(s.Seconds(), 'f', -1, 64)
	if s < 10*time.Second {
		secs = "0" + secs
	}
	return fmt.Sprintf("%s%02d:%02d:%s", sign, h, m, secs)
}

// floatText writes a float the way ClickHouse reads it back.
func floatText(f float64) string {
	switch {
	case math.IsNaN(f):
		return "nan"
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}
