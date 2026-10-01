package migrate

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"math"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/twpayne/go-geom"
	"github.com/twpayne/go-geom/encoding/geojson"
	"github.com/twpayne/go-geom/encoding/wkt"

	"rowsmith/internal/driver"
)

// conv turns a source cell into a value the target's importer accepts
// (the grid-edit cell shape).
type conv func(any) (any, error)

var errShapeTooLarge = errors.New("a shape is too large for the source to send exactly")

// unwrap resolves the cell forms every engine shares: complete large
// values, and MongoDB's extended JSON scalars.
func unwrap(v any) any {
	switch x := v.(type) {
	case driver.LongText:
		return x.Text
	case driver.LargeBinary:
		return map[string]any{"$bin": base64.StdEncoding.EncodeToString(x.Data)}
	case map[string]any:
		if len(x) == 1 || (len(x) == 2 && x["size"] != nil) {
			for _, k := range []string{"$oid", "$numberDecimal", "$numberLong", "$numberDouble", "$uuid", "$text"} {
				if s, ok := x[k].(string); ok {
					return s
				}
			}
			if s, ok := x["$date"].(string); ok {
				return s
			}
		}
	}
	return v
}

func toBool(v any) (bool, bool) {
	switch x := v.(type) {
	case bool:
		return x, true
	case int64:
		return x != 0, true
	case int:
		return x != 0, true
	case float64:
		return x != 0, true
	case json.Number:
		f, err := x.Float64()
		return f != 0, err == nil
	case string:
		switch strings.ToLower(strings.TrimSpace(x)) {
		case "1", "t", "true", "y", "yes", "on":
			return true, true
		case "0", "f", "false", "n", "no", "off":
			return false, true
		}
	}
	return false, false
}

func jsonText(v any) (string, error) {
	switch x := v.(type) {
	case string:
		return x, nil
	}
	b, err := json.Marshal(v)
	return string(b), err
}

var offsetRe = regexp.MustCompile(`(Z|[+-]\d{2}(:?\d{2})?)$`)

var timeLayouts = []string{
	"2006-01-02 15:04:05.999999999Z07:00", "2006-01-02T15:04:05.999999999Z07:00", "2006-01-02 15:04:05.999999999Z07",
	"2006-01-02 15:04:05.999999999", "2006-01-02T15:04:05.999999999", "2006-01-02 15:04", "2006-01-02",
}

// parseTime reads the datetime forms engines send; ok reports an offset.
func parseTime(s string) (time.Time, bool, error) {
	s = strings.TrimSpace(s)
	for _, l := range timeLayouts {
		if t, err := time.Parse(l, s); err == nil {
			return t, strings.Contains(l, "Z07"), nil
		}
	}
	return time.Time{}, false, fmt.Errorf("%q is not a date and time", s)
}

func wallClock(t time.Time) string {
	s := t.Format("2006-01-02 15:04:05.999999")
	return s
}

// money strips currency symbols and grouping from PostgreSQL money text.
var moneyRe = regexp.MustCompile(`[^0-9.\-]`)

func newConverter(srcEngine, dstEngine string, sk, dk canon) conv {
	toMongo := dstEngine == MongoDB
	return func(v any) (any, error) {
		v = unwrap(v)
		if v == nil {
			return nil, nil
		}
		if m, ok := v.(map[string]any); ok && m["$geo"] != nil {
			if m["display"] != nil || m["omitted"] != nil {
				return nil, errShapeTooLarge
			}
			raw, _ := json.Marshal(m["$geo"])
			if dk.T == "geometry" || toMongo {
				return map[string]any{"$geo": json.RawMessage(raw)}, nil
			}
			return string(raw), nil
		}
		if toMongo {
			switch v.(type) {
			case driver.Doc, []any:
				return v, nil // documents and arrays go in as they are, fields in order
			}
		}
		if s, ok := v.(string); ok && sk.T == "geometry" {
			// SQL Server and Oracle send shapes as (E)WKT; GeoJSON carries
			// the same shape to every engine without axis-order surprises.
			if raw, ok := wktToGeoJSON(s); ok {
				if dk.T == "geometry" || toMongo {
					return map[string]any{"$geo": json.RawMessage(raw)}, nil
				}
				return string(raw), nil
			}
		}
		if m, ok := v.(map[string]any); ok && m["$text"] != nil && m["omitted"] != nil {
			return nil, errors.New("a value is too large for the source to send")
		}
		switch dk.T {
		case "bool":
			if b, ok := toBool(v); ok {
				return b, nil
			}
			return nil, fmt.Errorf("%v is not true or false", v)
		case "int", "decimal", "float":
			if b, ok := v.(bool); ok {
				if b {
					return int64(1), nil
				}
				return int64(0), nil
			}
			if s, ok := v.(string); ok && sk.Note == "money is copied as a decimal" {
				return moneyRe.ReplaceAllString(s, ""), nil
			}
			if dk.T == "decimal" {
				// One type per column: SQL Server types a multi-row VALUES
				// list by its "highest" member, so a bigint 0 in one row makes
				// 18446744073709551615 in another overflow.
				switch x := v.(type) {
				case int64:
					return strconv.FormatInt(x, 10), nil
				case float64:
					return strconv.FormatFloat(x, 'f', -1, 64), nil
				case json.Number:
					return string(x), nil
				}
			}
			return v, nil
		case "array":
			return v, nil // a PostgreSQL array column takes its own literal
		case "json", "object":
			if s, ok := v.(string); ok && sk.T == "array" && strings.HasPrefix(s, "{") {
				if arr, err := pgArray(s); err == nil {
					b, _ := json.Marshal(arr)
					return string(b), nil
				}
			}
			if sk.T != "json" && sk.T != "object" && sk.T != "array" {
				if _, ok := v.(string); ok {
					b, _ := json.Marshal(v) // a plain value becomes a JSON string
					return string(b), nil
				}
			}
			return jsonText(v)
		case "timestamptz", "datetime", "date":
			s, ok := v.(string)
			if !ok {
				return v, nil
			}
			t, hasOffset, err := parseTime(s)
			if err != nil {
				return s, nil // let the target judge it
			}
			switch {
			case toMongo:
				return t.UTC().Format("2006-01-02T15:04:05.999999Z07:00"), nil
			case dk.T == "date":
				return t.Format("2006-01-02"), nil
			case dk.T == "timestamptz":
				if hasOffset {
					return t.Format("2006-01-02 15:04:05.999999-07:00"), nil
				}
				return wallClock(t) + "+00:00", nil
			default:
				if hasOffset {
					return wallClock(t.UTC()), nil
				}
				return wallClock(t), nil
			}
		case "time":
			if s, ok := v.(string); ok {
				if i := strings.IndexAny(s, "+-Z"); i > 0 && offsetRe.MatchString(s) {
					return s[:i], nil
				}
				if t, _, err := parseTime(s); err == nil && strings.Contains(s, "-") {
					return t.Format("15:04:05.999999"), nil // a datetime into a time-of-day column
				}
			}
			return v, nil
		case "binary", "varbinary", "blob":
			return v, nil
		}
		// Text-like targets take strings.
		switch x := v.(type) {
		case string:
			if sk.T == "char" {
				return strings.TrimRight(x, " "), nil
			}
			return x, nil
		case bool:
			return strconv.FormatBool(x), nil
		case int64:
			return strconv.FormatInt(x, 10), nil
		case float64:
			return strconv.FormatFloat(x, 'g', -1, 64), nil
		case map[string]any:
			if b, ok := x["$bin"].(string); ok {
				raw, _ := base64.StdEncoding.DecodeString(b)
				return "0x" + strings.ToUpper(hex.EncodeToString(raw)), nil
			}
		}
		return jsonText(v)
	}
}

var sridPrefix = regexp.MustCompile(`(?i)^\s*SRID=\d+;`)

func wktToGeoJSON(s string) ([]byte, bool) {
	g, err := wkt.Unmarshal(sridPrefix.ReplaceAllString(s, ""))
	if err != nil {
		return nil, false
	}
	gj, err := geojson.Encode(g)
	if err != nil {
		return nil, false
	}
	raw, err := json.Marshal(gj)
	return raw, err == nil
}

// pgArray parses a PostgreSQL array literal such as {1,"a b",NULL,{2,3}}.
func pgArray(s string) (any, error) {
	p := &arrParser{s: s}
	v, err := p.array()
	if err != nil {
		return nil, err
	}
	if p.i != len(p.s) {
		return nil, errors.New("trailing text after the array")
	}
	return v, nil
}

type arrParser struct {
	s string
	i int
}

func (p *arrParser) array() ([]any, error) {
	if p.i >= len(p.s) || p.s[p.i] != '{' {
		return nil, errors.New("expected {")
	}
	p.i++
	out := []any{}
	if p.i < len(p.s) && p.s[p.i] == '}' {
		p.i++
		return out, nil
	}
	for {
		if p.i >= len(p.s) {
			return nil, errors.New("unterminated array")
		}
		switch p.s[p.i] {
		case '{':
			a, err := p.array()
			if err != nil {
				return nil, err
			}
			out = append(out, a)
		case '"':
			p.i++
			var b strings.Builder
			for p.i < len(p.s) && p.s[p.i] != '"' {
				if p.s[p.i] == '\\' && p.i+1 < len(p.s) {
					p.i++
				}
				b.WriteByte(p.s[p.i])
				p.i++
			}
			p.i++
			out = append(out, b.String())
		default:
			j := p.i
			for p.i < len(p.s) && p.s[p.i] != ',' && p.s[p.i] != '}' {
				p.i++
			}
			tok := strings.TrimSpace(p.s[j:p.i])
			switch {
			case strings.EqualFold(tok, "null"):
				out = append(out, nil)
			case tok == "t" || tok == "f":
				out = append(out, tok == "t")
			case numberRe.MatchString(tok):
				out = append(out, json.Number(tok))
			default:
				out = append(out, tok)
			}
		}
		if p.i >= len(p.s) {
			return nil, errors.New("unterminated array")
		}
		if p.s[p.i] == ',' {
			p.i++
			continue
		}
		if p.s[p.i] == '}' {
			p.i++
			return out, nil
		}
		return nil, errors.New("unexpected character in array")
	}
}

// ---- verification ------------------------------------------------------------

// normalize renders a value so that the same data reads the same on any
// engine: numbers without formatting, times in one layout, JSON with sorted
// keys. k is the source column's type, used for both sides.
func normalize(v any, k canon) string {
	v = unwrap(v)
	if v == nil {
		return "\x00"
	}
	if m, ok := v.(map[string]any); ok && m["$geo"] != nil || k.T == "geometry" {
		return geoKey(v)
	}
	switch k.T {
	case "bool":
		if b, ok := toBool(v); ok {
			return map[bool]string{true: "1", false: "0"}[b]
		}
	case "int", "decimal":
		if b, ok := v.(bool); ok {
			return map[bool]string{true: "1", false: "0"}[b]
		}
		return canonNumber(v)
	case "float":
		return canonFloat(v, k.Bits)
	case "date", "datetime", "timestamptz":
		s, ok := v.(string)
		if !ok {
			break
		}
		t, hasOffset, err := parseTime(s)
		if err != nil {
			return s
		}
		if hasOffset || k.T == "timestamptz" {
			t = t.UTC()
		}
		out := t.Format("2006-01-02 15:04:05.999999")
		return strings.TrimSuffix(out, " 00:00:00")
	case "time":
		if s, ok := v.(string); ok {
			if i := strings.IndexAny(s, "+Z"); i > 0 {
				s = s[:i]
			}
			if t, err := time.Parse("15:04:05.999999999", strings.TrimSpace(s)); err == nil {
				return t.Format("15:04:05.999999")
			}
			return s
		}
	case "interval":
		if s, ok := v.(string); ok {
			if n, ok := intervalValue(s); ok {
				return n
			}
			return s
		}
	case "json", "object", "array":
		return canonJSON(v, k.T == "array")
	case "uuid":
		if s, ok := v.(string); ok {
			return strings.ToLower(strings.Trim(s, "{}"))
		}
	case "binary", "varbinary", "blob":
		if m, ok := v.(map[string]any); ok {
			if b, ok := m["$bin"].(string); ok {
				raw, _ := base64.StdEncoding.DecodeString(b)
				if len(raw) == 0 {
					return "\x00" // Oracle stores empty binaries as NULL
				}
				return hex.EncodeToString(raw)
			}
		}
		if s, ok := v.(string); ok && strings.HasPrefix(strings.ToLower(s), "0x") {
			if len(s) == 2 {
				return "\x00"
			}
			return strings.ToLower(s[2:])
		}
	}
	switch x := v.(type) {
	case string:
		if k.T == "char" {
			x = strings.TrimRight(x, " ")
		}
		if x == "" {
			return "\x00" // Oracle stores empty strings as NULL
		}
		return x
	case bool:
		return map[bool]string{true: "1", false: "0"}[x]
	case int64, float64, json.Number:
		return canonNumber(x)
	}
	return canonJSON(v, false)
}

var (
	pgIntervalRe  = regexp.MustCompile(`(?i)([-+]?\d+(?:\.\d+)?)\s*(years?|mons?|months?|days?|hours?|mins?|minutes?|secs?|seconds?)`)
	clockRe       = regexp.MustCompile(`([-+])?(\d+):(\d{2})(?::(\d{2}(?:\.\d+)?))?`)
	oracleDSRe    = regexp.MustCompile(`^([-+])?(\d+) (\d+):(\d{2}):(\d{2}(?:\.\d+)?)$`)
	oracleYMRe    = regexp.MustCompile(`^([-+])?(\d+)-(\d+)$`)
	isoIntervalRe = regexp.MustCompile(`^(-)?P(?:(\d+)Y)?(?:(\d+)M)?(?:(\d+)W)?(?:(\d+)D)?(?:T(?:(\d+)H)?(?:(\d+)M)?(?:(\d+(?:\.\d+)?)S)?)?$`)
)

// intervalValue reads PostgreSQL ("1 day 02:03:04"), Oracle
// ("+01 02:03:04.000000", "+01-02") and ISO 8601 ("P1DT2H3M4S") intervals as
// months and seconds, which compare equal however they were written.
func intervalValue(s string) (string, bool) {
	s = strings.TrimSpace(s)
	var months, secs float64
	num := func(x string) float64 { f, _ := strconv.ParseFloat(x, 64); return f }
	sign := func(x string) float64 {
		if x == "-" {
			return -1
		}
		return 1
	}
	switch {
	case oracleDSRe.MatchString(s):
		m := oracleDSRe.FindStringSubmatch(s)
		secs = sign(m[1]) * (num(m[2])*86400 + num(m[3])*3600 + num(m[4])*60 + num(m[5]))
	case oracleYMRe.MatchString(s):
		m := oracleYMRe.FindStringSubmatch(s)
		months = sign(m[1]) * (num(m[2])*12 + num(m[3]))
	case isoIntervalRe.MatchString(s) && s != "P":
		m := isoIntervalRe.FindStringSubmatch(s)
		months = num(m[2])*12 + num(m[3])
		secs = (num(m[4])*7+num(m[5]))*86400 + num(m[6])*3600 + num(m[7])*60 + num(m[8])
		if m[1] == "-" {
			months, secs = -months, -secs
		}
	default:
		rest := s
		for _, m := range pgIntervalRe.FindAllStringSubmatch(s, -1) {
			n := num(m[1])
			switch u := strings.ToLower(m[2]); {
			case strings.HasPrefix(u, "year"):
				months += n * 12
			case strings.HasPrefix(u, "mon"):
				months += n
			case strings.HasPrefix(u, "day"):
				secs += n * 86400
			case strings.HasPrefix(u, "hour"):
				secs += n * 3600
			case strings.HasPrefix(u, "min"):
				secs += n * 60
			default:
				secs += n
			}
			rest = strings.Replace(rest, m[0], "", 1)
		}
		if m := clockRe.FindStringSubmatch(rest); m != nil {
			secs += sign(m[1]) * (num(m[2])*3600 + num(m[3])*60 + num(m[4]))
		} else if strings.TrimSpace(rest) != "" && rest == s {
			return "", false
		}
	}
	return strconv.FormatFloat(months, 'f', -1, 64) + "m" + strconv.FormatFloat(secs, 'f', 6, 64) + "s", true
}

// geoKey renders a shape the same way whichever engine sent it and in
// which format: coordinates rounded to 7 decimals, polygon rings in one
// orientation (outer counterclockwise), so a copy compares by its points.
func geoKey(v any) string {
	var g geom.T
	switch x := v.(type) {
	case map[string]any:
		raw, _ := json.Marshal(x["$geo"])
		if err := geojson.Unmarshal(raw, &g); err != nil {
			return string(raw)
		}
	case string:
		s := strings.TrimSpace(x)
		var err error
		if strings.HasPrefix(s, "{") {
			err = geojson.Unmarshal([]byte(s), &g)
		} else {
			g, err = wkt.Unmarshal(sridPrefix.ReplaceAllString(s, ""))
		}
		if err != nil {
			return s
		}
	default:
		return fmt.Sprint(v)
	}
	var b strings.Builder
	writeGeo(&b, g)
	return b.String()
}

func writeCoords(b *strings.Builder, flat []float64, stride int) {
	for i := 0; i+stride <= len(flat); i += stride {
		if i > 0 {
			b.WriteByte(',')
		}
		for j := 0; j < stride; j++ {
			if j > 0 {
				b.WriteByte(' ')
			}
			b.WriteString(strconv.FormatFloat(math.Round(flat[i+j]*1e7)/1e7, 'f', -1, 64))
		}
	}
}

// ring orients a closed ring's coordinates: ccw for outer rings.
func ring(flat []float64, stride int, ccw bool) []float64 {
	area := 0.0
	for i := 0; i+2*stride <= len(flat); i += stride {
		area += flat[i]*flat[i+stride+1] - flat[i+stride]*flat[i+1]
	}
	if (area > 0) == ccw || area == 0 {
		return flat
	}
	out := make([]float64, len(flat))
	n := len(flat) / stride
	for i := 0; i < n; i++ {
		copy(out[i*stride:(i+1)*stride], flat[(n-1-i)*stride:(n-i)*stride])
	}
	return out
}

func writeGeo(b *strings.Builder, g geom.T) {
	stride := g.Stride()
	switch x := g.(type) {
	case *geom.Point:
		b.WriteString("P(")
		writeCoords(b, x.FlatCoords(), stride)
	case *geom.LineString:
		b.WriteString("L(")
		writeCoords(b, x.FlatCoords(), stride)
	case *geom.Polygon:
		b.WriteString("Y(")
		for i := 0; i < x.NumLinearRings(); i++ {
			if i > 0 {
				b.WriteByte('|')
			}
			writeCoords(b, ring(x.LinearRing(i).FlatCoords(), stride, i == 0), stride)
		}
	case *geom.MultiPoint:
		b.WriteString("MP(")
		writeCoords(b, x.FlatCoords(), stride)
	case *geom.MultiLineString:
		b.WriteString("ML(")
		for i := 0; i < x.NumLineStrings(); i++ {
			if i > 0 {
				b.WriteByte('|')
			}
			writeCoords(b, x.LineString(i).FlatCoords(), stride)
		}
	case *geom.MultiPolygon:
		b.WriteString("MY(")
		for i := 0; i < x.NumPolygons(); i++ {
			if i > 0 {
				b.WriteByte(';')
			}
			writeGeo(b, x.Polygon(i))
		}
	case *geom.GeometryCollection:
		b.WriteString("GC(")
		for i, e := range x.Geoms() {
			if i > 0 {
				b.WriteByte(';')
			}
			writeGeo(b, e)
		}
	default:
		b.WriteString(fmt.Sprintf("%T(", g))
	}
	b.WriteByte(')')
}

func canonNumber(v any) string {
	var s string
	switch x := v.(type) {
	case string:
		s = moneyRe.ReplaceAllString(strings.TrimSpace(x), "")
	case float64:
		if math.IsInf(x, 0) || math.IsNaN(x) {
			return fmt.Sprint(x)
		}
		s = strconv.FormatFloat(x, 'f', -1, 64)
	default:
		s = fmt.Sprint(v)
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return s
	}
	return r.RatString()
}

func canonFloat(v any, bits int) string {
	var f float64
	switch x := v.(type) {
	case float64:
		f = x
	case int64:
		f = float64(x)
	case string:
		var err error
		if f, err = strconv.ParseFloat(strings.TrimSpace(x), 64); err != nil {
			return x
		}
	case json.Number:
		f, _ = x.Float64()
	default:
		return fmt.Sprint(v)
	}
	if bits == 32 {
		return strconv.FormatFloat(float64(float32(f)), 'g', 7, 32)
	}
	return strconv.FormatFloat(f, 'g', 15, 64)
}

func canonJSON(v any, pgArr bool) string {
	var parsed any
	switch x := v.(type) {
	case string:
		if pgArr && strings.HasPrefix(x, "{") {
			if a, err := pgArray(x); err == nil {
				parsed = a
				break
			}
		}
		d := json.NewDecoder(strings.NewReader(x))
		d.UseNumber()
		if err := d.Decode(&parsed); err != nil {
			return x
		}
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprint(v)
		}
		d := json.NewDecoder(bytes.NewReader(b))
		d.UseNumber()
		if err := d.Decode(&parsed); err != nil {
			return string(b)
		}
	}
	b, _ := json.Marshal(numbersCanon(parsed))
	return string(b)
}

// numbersCanon rewrites JSON numbers so 1.50 and 1.5 compare equal.
func numbersCanon(v any) any {
	switch x := v.(type) {
	case json.Number:
		return canonNumber(string(x))
	case map[string]any:
		for k, e := range x {
			x[k] = numbersCanon(e)
		}
	case []any:
		for i, e := range x {
			x[i] = numbersCanon(e)
		}
	}
	return v
}

// digest sums a hash of every value per column, so it does not depend on
// the order rows arrive in.
type digest struct {
	Rows int64
	Sums []uint64
}

func newDigest(n int) *digest { return &digest{Sums: make([]uint64, n)} }

func (d *digest) add(values []string) {
	d.Rows++
	for i, s := range values {
		h := fnv.New64a()
		h.Write([]byte(s))
		d.Sums[i] += h.Sum64()
	}
}
