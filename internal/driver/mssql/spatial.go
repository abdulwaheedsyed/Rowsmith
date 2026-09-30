package mssql

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"

	"github.com/twpayne/go-geom"

	"rowsmith/internal/driver"
	"rowsmith/internal/driver/geo"
)

// Serialization flags of SQL Server's native spatial format ([MS-SSCLRT]).
const (
	spatialZ             = 0x01
	spatialM             = 0x02
	spatialSinglePoint   = 0x08
	spatialSingleSegment = 0x10
)

// spatialCell renders a raw geometry/geography value for the map view,
// falling back to binary for shapes it cannot decode.
func spatialCell(b []byte, geography bool) any {
	if g, srid, err := decodeSpatial(b, geography); err == nil {
		if cell := geo.Cell(g, srid); cell != nil {
			return cell
		}
	}
	return driver.EncodeBinary(b)
}

// decodeSpatial parses a geometry or geography value as SQL Server sends it
// (the CLR serialization, not WKB). Geography stores latitude first; the
// result always has x = longitude. Curves and FullGlobe are not supported.
func decodeSpatial(b []byte, geography bool) (geom.T, int, error) {
	r := &spatialReader{b: b}
	srid := int(int32(r.u32()))
	if v := r.byte(); v != 1 && v != 2 {
		return nil, 0, fmt.Errorf("unknown spatial serialization version %d", v)
	}
	flags := r.byte()
	layout := geom.XY
	switch {
	case flags&spatialZ != 0 && flags&spatialM != 0:
		layout = geom.XYZM
	case flags&spatialZ != 0:
		layout = geom.XYZ
	case flags&spatialM != 0:
		layout = geom.XYM
	}
	stride := layout.Stride()
	var n int
	switch {
	case flags&spatialSinglePoint != 0:
		n = 1
	case flags&spatialSingleSegment != 0:
		n = 2
	default:
		n = r.count(16)
	}
	d := &spatialDecoder{layout: layout, coords: make([]float64, n*stride), npoints: n}
	for i := 0; i < n; i++ {
		x, y := r.f64(), r.f64()
		if geography {
			x, y = y, x
		}
		d.coords[i*stride], d.coords[i*stride+1] = x, y
	}
	if flags&spatialZ != 0 {
		for i := 0; i < n; i++ {
			d.coords[i*stride+2] = r.f64()
		}
	}
	if flags&spatialM != 0 {
		for i := 0; i < n; i++ {
			d.coords[i*stride+stride-1] = r.f64()
		}
	}
	if flags&(spatialSinglePoint|spatialSingleSegment) != 0 {
		if r.err != nil {
			return nil, 0, r.err
		}
		if n == 1 {
			return geom.NewPointFlat(layout, d.coords), srid, nil
		}
		return geom.NewLineStringFlat(layout, d.coords), srid, nil
	}
	nf := r.count(5)
	for i := 0; i < nf; i++ {
		r.byte() // figure attribute: ring or stroke, implied by the shape type
		d.figures = append(d.figures, int(int32(r.u32())))
	}
	ns := r.count(9)
	for i := 0; i < ns; i++ {
		d.shapes = append(d.shapes, shape{parent: int(int32(r.u32())), figure: int(int32(r.u32())), kind: r.byte()})
	}
	if r.err != nil {
		return nil, 0, r.err
	}
	if len(d.shapes) == 0 {
		return nil, 0, errors.New("spatial value has no shapes")
	}
	g, err := d.shape(0)
	return g, srid, err
}

type shape struct {
	parent, figure int
	kind           byte // OGC type: 1 point ... 7 collection
}

type spatialDecoder struct {
	layout  geom.Layout
	coords  []float64
	npoints int
	figures []int // offset of each figure's first point
	shapes  []shape
}

// figureRange returns the figures [from, to) of shape s.
func (d *spatialDecoder) figureRange(s int) (int, int) {
	from := d.shapes[s].figure
	if from < 0 {
		return 0, 0
	}
	for _, next := range d.shapes[s+1:] {
		if next.figure >= 0 {
			return from, next.figure
		}
	}
	return from, len(d.figures)
}

// points returns the flat coordinates of figure f.
func (d *spatialDecoder) points(f int) ([]float64, error) {
	from, to := d.figures[f], d.npoints
	if f+1 < len(d.figures) {
		to = d.figures[f+1]
	}
	if from < 0 || from > to || to > d.npoints {
		return nil, errors.New("invalid spatial figure")
	}
	s := d.layout.Stride()
	return d.coords[from*s : to*s], nil
}

func (d *spatialDecoder) shape(s int) (geom.T, error) {
	from, to := d.figureRange(s)
	if from > to || to > len(d.figures) {
		return nil, errors.New("invalid spatial shape")
	}
	switch d.shapes[s].kind {
	case 1:
		if from == to {
			return geom.NewPointEmpty(d.layout), nil
		}
		pts, err := d.points(from)
		if err != nil || len(pts) != d.layout.Stride() {
			return nil, errors.New("invalid spatial point")
		}
		return geom.NewPointFlat(d.layout, pts), nil
	case 2:
		if from == to {
			return geom.NewLineString(d.layout), nil
		}
		pts, err := d.points(from)
		if err != nil {
			return nil, err
		}
		return geom.NewLineStringFlat(d.layout, pts), nil
	case 3:
		var flat []float64
		var ends []int
		for f := from; f < to; f++ {
			pts, err := d.points(f)
			if err != nil {
				return nil, err
			}
			flat = append(flat, pts...)
			ends = append(ends, len(flat))
		}
		return geom.NewPolygonFlat(d.layout, flat, ends), nil
	case 4, 5, 6, 7:
		var parts []geom.T
		for c := s + 1; c < len(d.shapes); c++ {
			if d.shapes[c].parent != s {
				continue
			}
			g, err := d.shape(c)
			if err != nil {
				return nil, err
			}
			parts = append(parts, g)
		}
		return collect(d.layout, d.shapes[s].kind, parts)
	}
	return nil, fmt.Errorf("spatial type %d is not supported", d.shapes[s].kind)
}

func collect(layout geom.Layout, kind byte, parts []geom.T) (geom.T, error) {
	var err error
	switch kind {
	case 4:
		mp := geom.NewMultiPoint(layout)
		for _, p := range parts {
			if pt, ok := p.(*geom.Point); ok && err == nil {
				err = mp.Push(pt)
			}
		}
		return mp, err
	case 5:
		ml := geom.NewMultiLineString(layout)
		for _, p := range parts {
			if ls, ok := p.(*geom.LineString); ok && err == nil {
				err = ml.Push(ls)
			}
		}
		return ml, err
	case 6:
		mp := geom.NewMultiPolygon(layout)
		for _, p := range parts {
			if pg, ok := p.(*geom.Polygon); ok && err == nil {
				err = mp.Push(pg)
			}
		}
		return mp, err
	}
	gc := geom.NewGeometryCollection()
	return gc, gc.Push(parts...)
}

// spatialReader reads little-endian values, remembering the first overrun.
type spatialReader struct {
	b   []byte
	err error
}

func (r *spatialReader) take(n int) []byte {
	if r.err != nil || len(r.b) < n {
		r.err = errors.New("truncated spatial value")
		return make([]byte, n)
	}
	out := r.b[:n]
	r.b = r.b[n:]
	return out
}

func (r *spatialReader) byte() byte  { return r.take(1)[0] }
func (r *spatialReader) u32() uint32 { return binary.LittleEndian.Uint32(r.take(4)) }
func (r *spatialReader) f64() float64 {
	return math.Float64frombits(binary.LittleEndian.Uint64(r.take(8)))
}

// count reads an element count and rejects counts the remaining bytes
// cannot hold, so corrupt input cannot force a huge allocation.
func (r *spatialReader) count(size int) int {
	n := int(r.u32())
	if r.err == nil && n > len(r.b)/size {
		r.err = errors.New("truncated spatial value")
		return 0
	}
	return n
}
