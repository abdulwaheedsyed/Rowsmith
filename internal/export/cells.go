// Package export writes query results and table data to files (CSV, TSV,
// JSON, NDJSON, Excel and SQL INSERT statements) and produces SQL dumps of
// whole schemas. It consumes the encoded cells every driver streams to a
// driver.Sink, asking for complete values instead of display previews.
package export

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"

	"rowsmith/internal/driver"
)

// binBytes returns the bytes of a binary cell: {"$bin": base64} or a
// driver.LargeBinary.
func binBytes(v any) ([]byte, bool) {
	switch x := v.(type) {
	case driver.LargeBinary:
		return x.Data, true
	case map[string]any:
		if b64, ok := x["$bin"].(string); ok {
			b, err := base64.StdEncoding.DecodeString(b64)
			return b, err == nil
		}
	}
	return nil, false
}

// HexBinary renders binary data the way imports read it back: 0x + hex.
func HexBinary(b []byte) string { return "0x" + strings.ToUpper(hex.EncodeToString(b)) }

// Text renders a cell as plain text for CSV and spreadsheets. null reports
// SQL NULL (and absent document fields).
func Text(v any) (s string, null bool) {
	switch x := v.(type) {
	case nil:
		return "", true
	case string:
		return x, false
	case driver.LongText:
		return x.Text, false
	case driver.LargeBinary:
		return HexBinary(x.Data), false
	case bool:
		return strconv.FormatBool(x), false
	case int64:
		return strconv.FormatInt(x, 10), false
	case int:
		return strconv.Itoa(x), false
	case int32:
		return strconv.FormatInt(int64(x), 10), false
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64), false
	case float32:
		return strconv.FormatFloat(float64(x), 'g', -1, 32), false
	case json.Number:
		return x.String(), false
	case map[string]any:
		if b, ok := binBytes(x); ok {
			return HexBinary(b), false
		}
		if t, ok := x["$text"].(string); ok {
			return t, false
		}
		if g, ok := x["$geo"]; ok {
			if wkt, ok := x["wkt"].(string); ok && wkt != "" {
				return wkt, false
			}
			return compactJSON(g), false
		}
		for _, k := range []string{"$oid", "$numberDecimal", "$numberLong", "$numberInt", "$numberDouble", "$uuid", "$symbol"} {
			if s, ok := x[k].(string); ok {
				return s, false
			}
		}
		if d, ok := x["$date"]; ok {
			if s, ok := d.(string); ok {
				return s, false
			}
			return compactJSON(d), false
		}
		return compactJSON(plainJSON(x)), false
	case []any:
		return compactJSON(plainJSON(x)), false
	}
	return compactJSON(v), false
}

func compactJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// plainJSON converts a cell to an ordinary JSON value for relational data:
// binary becomes a 0x-hex string, geometry its GeoJSON, long text a string.
func plainJSON(v any) any {
	switch x := v.(type) {
	case driver.LongText:
		return x.Text
	case driver.LargeBinary:
		return HexBinary(x.Data)
	case map[string]any:
		if b, ok := binBytes(x); ok {
			return HexBinary(b)
		}
		if t, ok := x["$text"].(string); ok {
			return t
		}
		if g, ok := x["$geo"]; ok {
			return g
		}
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = plainJSON(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = plainJSON(e)
		}
		return out
	}
	return v
}

// docJSON converts a document-store cell to relaxed Extended JSON, keeping
// $oid, $date, $numberDecimal and friends so the file re-imports with the
// same BSON types.
func docJSON(v any) any {
	switch x := v.(type) {
	case driver.LongText:
		return x.Text
	case driver.LargeBinary:
		return map[string]any{"$binary": map[string]any{"base64": base64.StdEncoding.EncodeToString(x.Data), "subType": "00"}}
	case map[string]any:
		if b64, ok := x["$bin"].(string); ok {
			sub := "00"
			if s, ok := x["subType"].(string); ok && s != "" {
				sub = s
			}
			return map[string]any{"$binary": map[string]any{"base64": b64, "subType": sub}}
		}
		if t, ok := x["$text"].(string); ok {
			return t
		}
		if g, ok := x["$geo"]; ok {
			return g
		}
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = docJSON(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = docJSON(e)
		}
		return out
	}
	return v
}
