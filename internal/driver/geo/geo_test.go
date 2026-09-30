package geo

import (
	"testing"

	"github.com/twpayne/go-geom"
)

func TestHugeGeometryBecomesExtent(t *testing.T) {
	coords := make([]geom.Coord, 0, MaxPoints+10)
	for i := 0; i < MaxPoints+10; i++ {
		coords = append(coords, geom.Coord{float64(i%1000) / 10, float64(i/1000) / 10})
	}
	ls := geom.NewLineString(geom.XY).MustSetCoords(coords)
	cell, ok := Cell(ls, 4326).(map[string]any)
	if !ok || cell["display"] != "extent" || cell["points"] != MaxPoints+10 || cell["srid"] != 4326 {
		t.Fatalf("got %#v", cell)
	}
	small := geom.NewPoint(geom.XY).MustSetCoords(geom.Coord{1, 2})
	if c := Cell(small, 0).(map[string]any); c["display"] != nil || c["wkt"] == nil {
		t.Fatalf("small geometry changed: %#v", c)
	}
	coll := geom.NewGeometryCollection()
	coll.MustPush(small, ls)
	if Points(coll) != MaxPoints+11 {
		t.Fatal("collection points")
	}
}
