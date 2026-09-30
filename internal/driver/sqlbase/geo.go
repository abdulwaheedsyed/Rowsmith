package sqlbase

import (
	"encoding/json"
	"strings"
)

// GeoCell wraps a GeoJSON text value (as produced by ST_AsGeoJSON and
// friends in browse queries) into the {"$geo": ...} cell encoding.
func GeoCell(v any, srid int) any {
	var s string
	switch x := v.(type) {
	case nil:
		return nil
	case string:
		s = x
	case map[string]any:
		if t, ok := x["$text"].(string); ok {
			s = t
		} else {
			return v
		}
	default:
		return v
	}
	s = strings.TrimSpace(s)
	// Browse queries mark geometries too large to send in full (see the
	// dialects' SelectExpr): S = simplified, E = extent only, X = omitted.
	display := ""
	switch {
	case strings.HasPrefix(s, "S{"):
		display, s = "simplified", s[1:]
	case strings.HasPrefix(s, "E{"):
		display, s = "extent", s[1:]
	case strings.HasPrefix(s, "X"):
		return map[string]any{"$text": "Geometry too large to show here (" + strings.TrimSpace(s[1:]) + ")", "omitted": true}
	}
	if !strings.HasPrefix(s, "{") || !json.Valid([]byte(s)) {
		return v
	}
	out := map[string]any{"$geo": json.RawMessage(s)}
	if srid != 0 {
		out["srid"] = srid
	}
	if display != "" {
		out["display"] = display
	}
	return out
}
