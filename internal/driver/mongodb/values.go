package mongodb

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"rowsmith/internal/driver"
)

// Cells use relaxed Extended JSON so BSON types survive a round trip through
// the browser: {"$oid": ...}, {"$date": ...}, {"$numberDecimal": ...},
// {"$numberLong": ...} for integers beyond ±2^53. Binary becomes the shared
// {"$bin": ...} cell and GeoJSON sub-documents become {"$geo": ...} so the
// map view can draw them.

const maxSafe = 1<<53 - 1

func encode(v any) any {
	switch x := v.(type) {
	case nil:
		return nil
	case bson.ObjectID:
		return map[string]any{"$oid": x.Hex()}
	case bson.DateTime:
		return map[string]any{"$date": x.Time().UTC().Format("2006-01-02T15:04:05.000Z07:00")}
	case time.Time:
		return map[string]any{"$date": x.UTC().Format("2006-01-02T15:04:05.000Z07:00")}
	case bson.Decimal128:
		return map[string]any{"$numberDecimal": x.String()}
	case int32:
		return int64(x)
	case int64:
		if x > maxSafe || x < -maxSafe {
			return map[string]any{"$numberLong": strconv.FormatInt(x, 10)}
		}
		return x
	case int:
		return encode(int64(x))
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return map[string]any{"$numberDouble": strconv.FormatFloat(x, 'g', -1, 64)}
		}
		return x
	case string:
		return driver.EncodeText(x)
	case bool:
		return x
	case bson.Binary:
		if x.Subtype == 4 && len(x.Data) == 16 {
			return map[string]any{"$uuid": driver.EncodeUUIDBytes(x.Data)}
		}
		return driver.EncodeBinary(x.Data)
	case bson.Regex:
		return map[string]any{"$regex": x.Pattern, "$options": x.Options}
	case bson.Timestamp:
		return map[string]any{"$timestamp": map[string]any{"t": x.T, "i": x.I}}
	case bson.D:
		m := make(map[string]any, len(x))
		keys := make([]string, 0, len(x))
		for _, e := range x {
			m[e.Key] = encode(e.Value)
			keys = append(keys, e.Key)
		}
		if isGeoJSON(x) {
			if raw, err := json.Marshal(orderedMap(x)); err == nil {
				return map[string]any{"$geo": json.RawMessage(raw)}
			}
		}
		return orderedJSON{keys: keys, m: m}
	case bson.M:
		m := make(map[string]any, len(x))
		for k, e := range x {
			m[k] = encode(e)
		}
		return m
	case bson.A:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = encode(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = encode(e)
		}
		return out
	case bson.MinKey:
		return map[string]any{"$minKey": 1}
	case bson.MaxKey:
		return map[string]any{"$maxKey": 1}
	case bson.Null, bson.Undefined:
		return nil
	case bson.JavaScript:
		return map[string]any{"$code": string(x)}
	}
	return fmt.Sprint(v)
}

// orderedJSON keeps sub-document key order when marshalled.
type orderedJSON struct {
	keys []string
	m    map[string]any
}

func (o orderedJSON) MarshalJSON() ([]byte, error) {
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range o.keys {
		if i > 0 {
			b.WriteByte(',')
		}
		kb, _ := json.Marshal(k)
		b.Write(kb)
		b.WriteByte(':')
		vb, err := json.Marshal(o.m[k])
		if err != nil {
			return nil, err
		}
		b.Write(vb)
	}
	b.WriteByte('}')
	return []byte(b.String()), nil
}

func orderedMap(d bson.D) orderedJSON {
	m := map[string]any{}
	keys := make([]string, 0, len(d))
	for _, e := range d {
		keys = append(keys, e.Key)
		switch v := e.Value.(type) {
		case bson.D:
			m[e.Key] = orderedMap(v)
		case bson.A:
			m[e.Key] = plainArray(v)
		default:
			m[e.Key] = v
		}
	}
	return orderedJSON{keys: keys, m: m}
}

func plainArray(a bson.A) []any {
	out := make([]any, len(a))
	for i, v := range a {
		switch x := v.(type) {
		case bson.A:
			out[i] = plainArray(x)
		case bson.D:
			out[i] = orderedMap(x)
		default:
			out[i] = x
		}
	}
	return out
}

var geoTypes = map[string]bool{"Point": true, "LineString": true, "Polygon": true, "MultiPoint": true, "MultiLineString": true, "MultiPolygon": true, "GeometryCollection": true}

func isGeoJSON(d bson.D) bool {
	var typ string
	hasCoords := false
	for _, e := range d {
		switch e.Key {
		case "type":
			typ, _ = e.Value.(string)
		case "coordinates", "geometries":
			hasCoords = true
		}
	}
	return geoTypes[typ] && hasCoords && len(d) <= 3
}

// typeName returns the BSON type name used in inferred schemas.
func typeName(v any) string {
	switch v.(type) {
	case nil, bson.Null:
		return "null"
	case bson.ObjectID:
		return "objectId"
	case string:
		return "string"
	case int32:
		return "int"
	case int64:
		return "long"
	case float64:
		return "double"
	case bson.Decimal128:
		return "decimal"
	case bool:
		return "bool"
	case bson.DateTime, time.Time:
		return "date"
	case bson.D, bson.M:
		if d, ok := v.(bson.D); ok && isGeoJSON(d) {
			return "geojson"
		}
		return "object"
	case bson.A, []any:
		return "array"
	case bson.Binary:
		return "binData"
	case bson.Regex:
		return "regex"
	case bson.Timestamp:
		return "timestamp"
	}
	return fmt.Sprintf("%T", v)
}

func kindOf(t string) driver.ValueKind {
	switch t {
	case "objectId":
		return driver.KindOther
	case "string":
		return driver.KindString
	case "int", "long":
		return driver.KindInt
	case "double":
		return driver.KindFloat
	case "decimal":
		return driver.KindDecimal
	case "bool":
		return driver.KindBool
	case "date", "timestamp":
		return driver.KindTimestamp
	case "object":
		return driver.KindObject
	case "array":
		return driver.KindArray
	case "binData":
		return driver.KindBinary
	case "geojson":
		return driver.KindGeometry
	}
	return driver.KindOther
}

// decodeCell converts a value sent by the UI back into BSON. hint is the
// column's dominant BSON type, used to keep numbers numbers and ids ids.
func decodeCell(v any, hint string) (any, error) {
	switch x := v.(type) {
	case nil:
		return nil, nil
	case bool:
		return x, nil
	case json.Number:
		return numberFor(string(x), hint)
	case float64:
		return numberFor(strconv.FormatFloat(x, 'g', -1, 64), hint)
	case map[string]any:
		if g, ok := x["$geo"]; ok {
			b, _ := json.Marshal(g)
			return parseLiteral(string(b))
		}
		if b, ok := x["$bin"].(string); ok {
			raw, err := base64.StdEncoding.DecodeString(b)
			if err != nil {
				return nil, err
			}
			return bson.Binary{Subtype: 0, Data: raw}, nil
		}
		if u, ok := x["$uuid"].(string); ok {
			raw, err := hex.DecodeString(strings.ReplaceAll(u, "-", ""))
			if err != nil || len(raw) != 16 {
				return nil, fmt.Errorf("invalid UUID %q", u)
			}
			return bson.Binary{Subtype: 4, Data: raw}, nil
		}
		// Extended JSON wrappers ({"$oid": …}) only decode inside a document.
		b, _ := json.Marshal(map[string]any{"v": x})
		var doc bson.D
		if err := bson.UnmarshalExtJSON(b, false, &doc); err == nil && len(doc) == 1 {
			return normalize(doc[0].Value), nil
		}
		raw, _ := json.Marshal(x)
		return parseLiteral(string(raw))
	case []any:
		b, _ := json.Marshal(x)
		return parseLiteral(string(b))
	case string:
		return stringFor(x, hint)
	}
	return v, nil
}

func numberFor(s, hint string) (any, error) {
	switch hint {
	case "int":
		if n, err := strconv.ParseInt(s, 10, 32); err == nil {
			return int32(n), nil
		}
	case "long":
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			return n, nil
		}
	case "decimal":
		return bson.ParseDecimal128(s)
	case "string":
		return s, nil
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		if n >= math.MinInt32 && n <= math.MaxInt32 {
			return int32(n), nil
		}
		return n, nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil, fmt.Errorf("%q is not a number", s)
	}
	return f, nil
}

// stringFor interprets text typed into a cell editor.
func stringFor(s, hint string) (any, error) {
	t := strings.TrimSpace(s)
	switch hint {
	case "string":
		return s, nil
	case "int", "long", "double", "decimal":
		if t != "" {
			return numberFor(t, hint)
		}
	case "bool":
		switch strings.ToLower(t) {
		case "true", "1", "yes":
			return true, nil
		case "false", "0", "no":
			return false, nil
		}
	case "objectId":
		if len(t) == 24 {
			if id, err := bson.ObjectIDFromHex(t); err == nil {
				return id, nil
			}
		}
	case "date":
		if tm, ok := parseTime(t); ok {
			return bson.NewDateTimeFromTime(tm), nil
		}
	}
	// Shell-style literals: {…}, […], ObjectId("…"), ISODate("…"), numbers, true/false/null.
	if looksLiteral(t) {
		if v, err := parseLiteral(t); err == nil {
			return v, nil
		}
	}
	return s, nil
}

func looksLiteral(t string) bool {
	if t == "" {
		return false
	}
	switch t[0] {
	case '{', '[':
		return true
	}
	for _, p := range []string{"ObjectId(", "ISODate(", "new Date(", "NumberLong(", "NumberInt(", "NumberDecimal(", "UUID(", "Timestamp("} {
		if strings.HasPrefix(t, p) {
			return true
		}
	}
	return t == "true" || t == "false" || t == "null"
}

func parseTime(s string) (time.Time, bool) {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.000Z07:00", "2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func normalize(v any) any {
	switch x := v.(type) {
	case bson.M:
		d := bson.D{}
		for k, e := range x {
			d = append(d, bson.E{Key: k, Value: normalize(e)})
		}
		return d
	case bson.A:
		for i := range x {
			x[i] = normalize(x[i])
		}
		return x
	}
	return v
}
