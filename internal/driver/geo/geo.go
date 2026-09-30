// Package geo converts spatial wire formats (WKB, EWKB, MySQL's SRID-prefixed
// WKB) into the {"$geo": GeoJSON} cell encoding used by the map view.
package geo

import (
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/twpayne/go-geom"
	"github.com/twpayne/go-geom/encoding/ewkb"
	"github.com/twpayne/go-geom/encoding/geojson"
	"github.com/twpayne/go-geom/encoding/wkb"
	"github.com/twpayne/go-geom/encoding/wkt"
)

// MaxPoints is the most vertices sent as a full geometry; larger ones are
// sent as their extent, marked "display": "extent", with the point count.
const MaxPoints = 200_000

// Points counts the vertices of a geometry.
func Points(g geom.T) int {
	if gc, ok := g.(*geom.GeometryCollection); ok {
		n := 0
		for _, c := range gc.Geoms() {
			n += Points(c)
		}
		return n
	}
	if st := g.Stride(); st > 0 {
		return len(g.FlatCoords()) / st
	}
	return 0
}

// Cell builds the cell encoding from a geometry.
func Cell(g geom.T, srid int) any {
	if n := Points(g); n > MaxPoints {
		b := g.Bounds()
		env := geom.NewPolygon(geom.XY).MustSetCoords([][]geom.Coord{{
			{b.Min(0), b.Min(1)}, {b.Max(0), b.Min(1)}, {b.Max(0), b.Max(1)}, {b.Min(0), b.Max(1)}, {b.Min(0), b.Min(1)},
		}})
		out, _ := Cell(env, srid).(map[string]any)
		if out != nil {
			delete(out, "wkt")
			out["display"] = "extent"
			out["points"] = n
		}
		return out
	}
	gj, err := geojson.Encode(g)
	if err != nil {
		return nil
	}
	raw, err := json.Marshal(gj)
	if err != nil {
		return nil
	}
	out := map[string]any{"$geo": json.RawMessage(raw)}
	if srid != 0 {
		out["srid"] = srid
	}
	if s, err := wkt.Marshal(g); err == nil && len(s) <= 4096 {
		out["wkt"] = s
	}
	return out
}

// FromMySQL decodes MySQL's internal format: 4-byte little-endian SRID + WKB.
func FromMySQL(b []byte) (any, bool) {
	if len(b) < 9 {
		return nil, false
	}
	srid := int(binary.LittleEndian.Uint32(b[:4]))
	g, err := wkb.Unmarshal(b[4:])
	if err != nil {
		return nil, false
	}
	return Cell(g, srid), true
}

// FromEWKB decodes PostGIS extended WKB (binary).
func FromEWKB(b []byte) (any, bool) {
	g, err := ewkb.Unmarshal(b)
	if err != nil {
		if g2, err2 := wkb.Unmarshal(b); err2 == nil {
			return Cell(g2, 0), true
		}
		return nil, false
	}
	return Cell(g, g.SRID()), true
}

// FromEWKBHex decodes the hex text PostGIS sends for geometry in text format.
func FromEWKBHex(s string) (any, bool) {
	s = strings.TrimSpace(s)
	if len(s) < 18 || len(s)%2 != 0 {
		return nil, false
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return nil, false
	}
	return FromEWKB(b)
}

// FromWKT parses well-known text (optionally EWKT "SRID=4326;POINT(...)").
func FromWKT(s string) (any, bool) {
	srid := 0
	if strings.HasPrefix(strings.ToUpper(s), "SRID=") {
		if i := strings.IndexByte(s, ';'); i > 0 {
			for _, c := range s[5:i] {
				if c < '0' || c > '9' {
					return nil, false
				}
				srid = srid*10 + int(c-'0')
			}
			s = s[i+1:]
		}
	}
	g, err := wkt.Unmarshal(s)
	if err != nil {
		return nil, false
	}
	return Cell(g, srid), true
}
