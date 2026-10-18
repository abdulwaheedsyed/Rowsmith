// Package chtype reads ClickHouse type names, such as
// LowCardinality(Nullable(String)) or Tuple(name String, n UInt8), and
// writes values as ClickHouse literals.
package chtype

import (
	"strings"
)

// Type is a parsed type name.
type Type struct {
	Name string // as written: String, Array, Nullable, DateTime64...
	// Args are the nested types of composite types (Array, Nullable,
	// LowCardinality, Map, Tuple, Nested, Variant), and the value type of
	// SimpleAggregateFunction.
	Args []Type
	// Params are the other parameters, as written: Decimal precision and
	// scale, FixedString length, DateTime time zone, Enum values.
	Params []string
	Field  string // the element's name in a named Tuple or Nested
	Raw    string // the whole type as written
	// Ordered marks a Map read as its list of pairs, which keeps its order.
	Ordered bool
}

// Parse reads a type name. Unknown names parse too; they keep their
// parameters as written.
func Parse(s string) Type {
	s = strings.TrimSpace(s)
	t := Type{Raw: s}
	i := strings.IndexByte(s, '(')
	if i < 0 || !strings.HasSuffix(s, ")") {
		t.Name = s
		return t
	}
	t.Name = strings.TrimSpace(s[:i])
	parts := splitTop(s[i+1 : len(s)-1])
	switch t.Name {
	case "Array", "Nullable", "LowCardinality", "Map", "Variant":
		for _, p := range parts {
			t.Args = append(t.Args, Parse(p))
		}
	case "Tuple", "Nested":
		for _, p := range parts {
			field, rest := fieldOf(p)
			el := Parse(rest)
			el.Field = field
			t.Args = append(t.Args, el)
		}
	case "SimpleAggregateFunction":
		if len(parts) > 0 {
			t.Params = parts[:1]
		}
		for _, p := range parts[min(1, len(parts)):] {
			t.Args = append(t.Args, Parse(p))
		}
	default:
		t.Params = parts
	}
	return t
}

// splitTop splits at commas outside brackets and quotes.
func splitTop(s string) []string {
	var out []string
	depth, start := 0, 0
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == '\\' {
				i++
			} else if c == quote {
				quote = 0
			}
		case c == '\'' || c == '`' || c == '"':
			quote = c
		case c == '(' || c == '[':
			depth++
		case c == ')' || c == ']':
			depth--
		case c == ',' && depth == 0:
			out = append(out, strings.TrimSpace(s[start:i]))
			start = i + 1
		}
	}
	if rest := strings.TrimSpace(s[start:]); rest != "" || len(out) > 0 {
		out = append(out, rest)
	}
	return out
}

// fieldOf splits "name Type" (a named tuple element) from a bare type.
func fieldOf(p string) (field, typ string) {
	p = strings.TrimSpace(p)
	if strings.HasPrefix(p, "`") {
		if j := strings.IndexByte(p[1:], '`'); j >= 0 {
			return p[1 : j+1], strings.TrimSpace(p[j+2:])
		}
	}
	i := strings.IndexAny(p, " \t")
	if i < 0 || strings.ContainsAny(p[:i], "(),'") {
		return "", p
	}
	return p[:i], strings.TrimSpace(p[i+1:])
}

// Unwrap removes LowCardinality and Nullable, reporting whether NULL is allowed.
func (t Type) Unwrap() (Type, bool) {
	nullable := false
	for (t.Name == "Nullable" || t.Name == "LowCardinality") && len(t.Args) == 1 {
		if t.Name == "Nullable" {
			nullable = true
		}
		t = t.Args[0]
	}
	return t, nullable
}

// Nullable reports whether the type accepts NULL.
func (t Type) Nullable() bool {
	_, n := t.Unwrap()
	return n
}

// StripNullable removes an outer Nullable(...), the way a column's type is
// shown beside its "nullable" flag.
func StripNullable(s string) string {
	t := Parse(s)
	if t.Name == "Nullable" && len(t.Args) == 1 {
		return t.Args[0].Raw
	}
	return s
}

// EnumValues lists an Enum8/Enum16's names in order.
func (t Type) EnumValues() []string {
	var out []string
	for _, p := range t.Params {
		p = strings.TrimSpace(p)
		if !strings.HasPrefix(p, "'") {
			continue
		}
		name, ok := unquote(p)
		if ok {
			out = append(out, name)
		}
	}
	return out
}

// unquote reads the leading single-quoted literal of s.
func unquote(s string) (string, bool) {
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		c := s[i]
		switch c {
		case '\\':
			if i+1 < len(s) {
				i++
				b.WriteByte(unescape(s[i]))
			}
		case '\'':
			if i+1 < len(s) && s[i+1] == '\'' {
				b.WriteByte('\'')
				i++
				continue
			}
			return b.String(), true
		default:
			b.WriteByte(c)
		}
	}
	return "", false
}

func unescape(c byte) byte {
	switch c {
	case 'n':
		return '\n'
	case 't':
		return '\t'
	case 'r':
		return '\r'
	case '0':
		return 0
	}
	return c
}

// Integer families and their widths.
var ints = map[string]int{
	"Int8": 8, "Int16": 16, "Int32": 32, "Int64": 64, "Int128": 128, "Int256": 256,
	"UInt8": 8, "UInt16": 16, "UInt32": 32, "UInt64": 64, "UInt128": 128, "UInt256": 256,
}

// IntBits reports an integer type's width and signedness.
func (t Type) IntBits() (bits int, unsigned, ok bool) {
	bits, ok = ints[t.Name]
	return bits, strings.HasPrefix(t.Name, "U"), ok
}

// IsDecimal reports Decimal(P, S) and Decimal32/64/128/256(S).
func (t Type) IsDecimal() bool { return strings.HasPrefix(t.Name, "Decimal") }

// DecimalPS returns a decimal's precision and scale.
func (t Type) DecimalPS() (int, int) {
	atoi := func(s string) int {
		n := 0
		for _, c := range strings.TrimSpace(s) {
			if c < '0' || c > '9' {
				break
			}
			n = n*10 + int(c-'0')
		}
		return n
	}
	switch t.Name {
	case "Decimal":
		if len(t.Params) == 2 {
			return atoi(t.Params[0]), atoi(t.Params[1])
		}
		if len(t.Params) == 1 {
			return atoi(t.Params[0]), 0
		}
		return 10, 0
	case "Decimal32", "Decimal64", "Decimal128", "Decimal256":
		p := map[string]int{"Decimal32": 9, "Decimal64": 18, "Decimal128": 38, "Decimal256": 76}[t.Name]
		if len(t.Params) == 1 {
			return p, atoi(t.Params[0])
		}
		return p, 0
	}
	return 0, 0
}

// Geo types and the shapes they hold.
var geo = map[string]string{
	"Point": "point", "Ring": "polygon", "LineString": "linestring", "MultiLineString": "multilinestring",
	"Polygon": "polygon", "MultiPolygon": "multipolygon",
}

// GeoShape names a geo type's shape, or "" for other types.
func (t Type) GeoShape() string { return geo[t.Name] }
