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
	if !strings.HasPrefix(s, "{") || !json.Valid([]byte(s)) {
		return v
	}
	out := map[string]any{"$geo": json.RawMessage(s)}
	if srid != 0 {
		out["srid"] = srid
	}
	return out
}
