package chtype

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/paulmach/orb"
	orbwkt "github.com/paulmach/orb/encoding/wkt"
	orbjson "github.com/paulmach/orb/geojson"

	"rowsmith/internal/driver"
)

var (
	intRe     = regexp.MustCompile(`^[-+]?\d+$`)
	numericRe = regexp.MustCompile(`^[-+]?(\d+\.?\d*|\.\d+)([eE][-+]?\d+)?$`)
)

// Quote writes a string literal.
func Quote(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `'`, `\'`, "\x00", `\0`)
	return "'" + r.Replace(s) + "'"
}

// Literal writes v as a value of type t. Values are cells as the grid and
// exports carry them: nil, bools, numbers, strings, []any, maps and
// driver.Doc, {"$bin": …}, {"$geo": …}. With expr set the literal can stand
// in an expression (UPDATE … SET, WHERE); without it, it's written for
// INSERT … VALUES, which reads text for each column's type.
func Literal(t Type, v any, expr bool) (string, error) {
	inner, _ := t.Unwrap()
	if inner.Name == "SimpleAggregateFunction" && len(inner.Args) == 1 {
		inner = inner.Args[0]
	}
	switch x := v.(type) {
	case nil:
		return "NULL", nil
	case map[string]any:
		if b, _ := x["$null"].(bool); b {
			return "NULL", nil
		}
		if e, ok := x["$expr"].(string); ok && strings.TrimSpace(e) != "" {
			return "(" + e + ")", nil
		}
		if s, ok := x["$bin"].(string); ok {
			b, err := base64.StdEncoding.DecodeString(s)
			if err != nil {
				return "", fmt.Errorf("invalid binary value")
			}
			return binLiteral(b, t, expr), nil
		}
		if s, ok := x["$text"].(string); ok {
			return Literal(t, s, expr)
		}
		if g, ok := x["$geo"]; ok {
			return geoLiteral(inner, g)
		}
	case driver.LongText:
		return Literal(t, x.Text, expr)
	case driver.LargeBinary:
		return binLiteral(x.Data, t, expr), nil
	case []byte:
		return binLiteral(x, t, expr), nil
	}
	if _, _, ok := inner.IntBits(); ok {
		return wholeNumber(v)
	}
	switch {
	case inner.IsDecimal():
		s, ok := number(v)
		if !ok {
			return "", fmt.Errorf("%v isn't a number", v)
		}
		if expr {
			return "CAST(" + Quote(s) + " AS " + t.Raw + ")", nil
		}
		return s, nil
	case inner.GeoShape() != "":
		return geoLiteral(inner, v)
	}
	switch inner.Name {
	case "Float32", "Float64", "BFloat16":
		switch x := v.(type) {
		case float64:
			return floatText(x), nil
		case string:
			l := strings.ToLower(strings.TrimSpace(x))
			if numericRe.MatchString(l) || l == "nan" || l == "inf" || l == "-inf" || l == "+inf" {
				return l, nil
			}
		}
		if s, ok := number(v); ok {
			return s, nil
		}
		return "", fmt.Errorf("%v isn't a number", v)
	case "Bool":
		switch x := v.(type) {
		case bool:
			return strconv.FormatBool(x), nil
		case float64:
			return strconv.FormatBool(x != 0), nil
		case string:
			switch strings.ToLower(strings.TrimSpace(x)) {
			case "true", "1", "t", "yes":
				return "true", nil
			case "false", "0", "f", "no":
				return "false", nil
			}
		}
		return "", fmt.Errorf("%v isn't true or false", v)
	case "String", "FixedString":
		return Quote(text(v)), nil
	case "Array", "Nested":
		items, err := listOf(v)
		if err != nil {
			return "", err
		}
		elem := Type{Name: "String", Raw: "String"}
		if inner.Name == "Array" && len(inner.Args) == 1 {
			elem = inner.Args[0]
		} else if inner.Name == "Nested" {
			elem = Type{Name: "Tuple", Args: inner.Args, Raw: "Tuple(" + joinRaw(inner.Args) + ")"}
		}
		parts := make([]string, len(items))
		for i, it := range items {
			if parts[i], err = Literal(elem, it, expr); err != nil {
				return "", err
			}
		}
		return "[" + strings.Join(parts, ", ") + "]", nil
	case "Map":
		keys, vals, err := entriesOf(v)
		if err != nil {
			return "", err
		}
		kt, vt := Type{Name: "String", Raw: "String"}, Type{Name: "String", Raw: "String"}
		if len(inner.Args) == 2 {
			kt, vt = inner.Args[0], inner.Args[1]
		}
		parts := make([]string, 0, len(keys))
		for i, k := range keys {
			ks, err := Literal(kt, k, expr)
			if err != nil {
				return "", err
			}
			vs, err := Literal(vt, vals[i], expr)
			if err != nil {
				return "", err
			}
			if expr {
				parts = append(parts, ks, vs)
			} else {
				parts = append(parts, ks+": "+vs)
			}
		}
		if expr {
			return "map(" + strings.Join(parts, ", ") + ")", nil
		}
		return "{" + strings.Join(parts, ", ") + "}", nil
	case "Tuple":
		vals, err := tupleOf(v, inner)
		if err != nil {
			return "", err
		}
		parts := make([]string, len(vals))
		for i, it := range vals {
			et := Type{Name: "String", Raw: "String"}
			if i < len(inner.Args) {
				et = inner.Args[i]
			}
			if parts[i], err = Literal(et, it, expr); err != nil {
				return "", err
			}
		}
		if len(parts) == 1 {
			return "tuple(" + parts[0] + ")", nil
		}
		return "(" + strings.Join(parts, ", ") + ")", nil
	case "JSON", "Object":
		s, ok := v.(string)
		if !ok {
			b, err := json.Marshal(plain(v))
			if err != nil {
				return "", err
			}
			s = string(b)
		}
		if expr {
			return "CAST(" + Quote(s) + " AS " + t.Raw + ")", nil
		}
		return Quote(s), nil
	}
	// Dates, times, UUIDs, IPs and enums are read from their text.
	s := text(v)
	if inner.Name == "DateTime" || inner.Name == "DateTime64" || inner.Name == "Date" || inner.Name == "Date32" {
		s = strings.Replace(strings.TrimSuffix(s, "Z"), "T", " ", 1)
	}
	return cast(Quote(s), t, expr), nil
}

func joinRaw(ts []Type) string {
	parts := make([]string, len(ts))
	for i, t := range ts {
		parts[i] = t.Raw
		if t.Field != "" {
			parts[i] = t.Field + " " + t.Raw
		}
	}
	return strings.Join(parts, ", ")
}

func cast(lit string, t Type, expr bool) string {
	if !expr {
		return lit
	}
	return "CAST(" + lit + " AS " + t.Raw + ")"
}

func binLiteral(b []byte, t Type, expr bool) string {
	return cast("unhex('"+strings.ToUpper(hex.EncodeToString(b))+"')", t, expr)
}

func text(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return floatText(x)
	case bool:
		return strconv.FormatBool(x)
	case json.Number:
		return x.String()
	}
	return fmt.Sprint(v)
}

func number(v any) (string, bool) {
	switch x := v.(type) {
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return "", false
		}
		return strconv.FormatFloat(x, 'f', -1, 64), true
	case int64:
		return strconv.FormatInt(x, 10), true
	case int:
		return strconv.Itoa(x), true
	case json.Number:
		return x.String(), numericRe.MatchString(x.String())
	case string:
		s := strings.TrimSpace(x)
		return s, numericRe.MatchString(s)
	}
	return "", false
}

func wholeNumber(v any) (string, error) {
	switch x := v.(type) {
	case bool:
		if x {
			return "1", nil
		}
		return "0", nil
	case float64:
		if x != math.Trunc(x) || math.IsInf(x, 0) {
			return "", fmt.Errorf("%v isn't a whole number", x)
		}
		return strconv.FormatFloat(x, 'f', 0, 64), nil
	}
	s, _ := number(v)
	if !intRe.MatchString(s) {
		return "", fmt.Errorf("%v isn't a whole number", v)
	}
	return strings.TrimPrefix(s, "+"), nil
}

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

// listOf accepts an array, or its JSON text.
func listOf(v any) ([]any, error) {
	switch x := v.(type) {
	case []any:
		return x, nil
	case string:
		var out []any
		d := json.NewDecoder(strings.NewReader(x))
		d.UseNumber()
		if err := d.Decode(&out); err != nil {
			return nil, fmt.Errorf("an array is written like [1, 2, 3]")
		}
		return out, nil
	}
	return nil, fmt.Errorf("an array is written like [1, 2, 3]")
}

// entriesOf accepts a map, a driver.Doc or an object's JSON text, in key order.
func entriesOf(v any) ([]any, []any, error) {
	switch x := v.(type) {
	case driver.Doc:
		keys := make([]any, len(x.Keys))
		vals := make([]any, len(x.Keys))
		for i, k := range x.Keys {
			keys[i], vals[i] = k, x.Values[k]
		}
		return keys, vals, nil
	case map[string]any:
		names := make([]string, 0, len(x))
		for k := range x {
			names = append(names, k)
		}
		sort.Strings(names)
		keys := make([]any, len(names))
		vals := make([]any, len(names))
		for i, k := range names {
			keys[i], vals[i] = k, x[k]
		}
		return keys, vals, nil
	case string:
		var m map[string]any
		d := json.NewDecoder(strings.NewReader(x))
		d.UseNumber()
		if err := d.Decode(&m); err != nil {
			return nil, nil, fmt.Errorf(`a map is written like {"key": "value"}`)
		}
		return entriesOf(m)
	}
	return nil, nil, fmt.Errorf(`a map is written like {"key": "value"}`)
}

// tupleOf accepts a tuple as an array, or as an object keyed by its fields.
func tupleOf(v any, t Type) ([]any, error) {
	switch x := v.(type) {
	case []any:
		return x, nil
	case driver.Doc:
		return tupleOf(x.Values, t)
	case map[string]any:
		out := make([]any, len(t.Args))
		for i, f := range t.Args {
			out[i] = x[f.Field]
		}
		return out, nil
	case string:
		var parsed any
		d := json.NewDecoder(strings.NewReader(x))
		d.UseNumber()
		if err := d.Decode(&parsed); err == nil {
			if _, ok := parsed.(string); !ok {
				return tupleOf(parsed, t)
			}
		}
	}
	return nil, fmt.Errorf("a tuple is written like [\"a\", 1] or {\"field\": value}")
}

// plain turns docs back into maps for JSON encoding.
func plain(v any) any {
	switch x := v.(type) {
	case driver.Doc:
		m := make(map[string]any, len(x.Keys))
		for k, val := range x.Values {
			m[k] = plain(val)
		}
		return m
	case []any:
		out := make([]any, len(x))
		for i, it := range x {
			out[i] = plain(it)
		}
		return out
	}
	return v
}

// geoLiteral writes a geo value from GeoJSON (object or text) or WKT.
func geoLiteral(t Type, v any) (string, error) {
	var g orb.Geometry
	switch x := v.(type) {
	case json.RawMessage:
		return geoLiteral(t, string(x))
	case map[string]any, driver.Doc:
		b, err := json.Marshal(plain(x))
		if err != nil {
			return "", err
		}
		return geoLiteral(t, string(b))
	case string:
		s := strings.TrimSpace(x)
		if strings.HasPrefix(s, "{") {
			gm, err := orbjson.UnmarshalGeometry([]byte(s))
			if err != nil {
				return "", fmt.Errorf("invalid GeoJSON: %v", err)
			}
			g = gm.Geometry()
		} else {
			if i := strings.IndexByte(s, ';'); i > 0 && strings.HasPrefix(strings.ToUpper(s), "SRID=") {
				s = s[i+1:]
			}
			var err error
			if g, err = orbwkt.Unmarshal(s); err != nil {
				return "", fmt.Errorf("a %s is written as WKT, like POINT(1 2), or GeoJSON", t.Name)
			}
		}
	default:
		return "", fmt.Errorf("a %s is written as WKT, like POINT(1 2), or GeoJSON", t.Name)
	}
	return orbLiteral(t.Name, g)
}

func pt(p orb.Point) string { return "(" + floatText(p[0]) + ", " + floatText(p[1]) + ")" }

func pts(ps []orb.Point) string {
	parts := make([]string, len(ps))
	for i, p := range ps {
		parts[i] = pt(p)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

func rings(rs []orb.Ring) string {
	parts := make([]string, len(rs))
	for i, r := range rs {
		parts[i] = pts(r)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

func orbLiteral(name string, g orb.Geometry) (string, error) {
	mismatch := fmt.Errorf("a %s column can't hold a %s", name, g.GeoJSONType())
	switch name {
	case "Point":
		if p, ok := g.(orb.Point); ok {
			return pt(p), nil
		}
	case "Ring", "LineString":
		switch x := g.(type) {
		case orb.LineString:
			return pts(x), nil
		case orb.Ring:
			return pts(x), nil
		case orb.Polygon:
			if len(x) == 1 && name == "Ring" {
				return pts(x[0]), nil
			}
		}
	case "MultiLineString":
		if ml, ok := g.(orb.MultiLineString); ok {
			parts := make([]string, len(ml))
			for i, l := range ml {
				parts[i] = pts(l)
			}
			return "[" + strings.Join(parts, ", ") + "]", nil
		}
		if l, ok := g.(orb.LineString); ok {
			return "[" + pts(l) + "]", nil
		}
	case "Polygon":
		if p, ok := g.(orb.Polygon); ok {
			return rings(p), nil
		}
	case "MultiPolygon":
		switch x := g.(type) {
		case orb.MultiPolygon:
			parts := make([]string, len(x))
			for i, p := range x {
				parts[i] = rings(p)
			}
			return "[" + strings.Join(parts, ", ") + "]", nil
		case orb.Polygon:
			return "[" + rings(x) + "]", nil
		}
	}
	return "", mismatch
}
