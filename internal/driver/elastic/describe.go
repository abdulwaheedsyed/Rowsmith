package elastic

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"rowsmith/internal/driver"
)

// field is a top-level field of a mapping.
type field struct {
	typ     string
	keyword string          // a keyword sub-field (name.keyword) of a text field, for exact matches and sorting
	mapping json.RawMessage // the field's whole mapping
}

// layout is what the grid needs beyond driver.Table: each field's mapping.
type layout struct {
	fields map[string]field
	pk     string // _meta.primary_key, the field copied into _id
}

func kindOf(typ string) driver.ValueKind {
	switch typ {
	case "keyword", "constant_keyword", "wildcard", "ip", "version":
		return driver.KindString
	case "text", "match_only_text", "search_as_you_type", "semantic_text":
		return driver.KindText
	case "long", "integer", "short", "byte":
		return driver.KindInt
	case "unsigned_long":
		return driver.KindDecimal
	case "double", "float", "half_float", "scaled_float":
		return driver.KindFloat
	case "boolean":
		return driver.KindBool
	case "date", "date_nanos":
		return driver.KindTimestamp
	case "binary":
		return driver.KindBinary
	case "geo_point", "geo_shape", "point", "shape":
		return driver.KindGeometry
	case "object", "flattened", "join":
		return driver.KindObject
	case "nested", "dense_vector", "sparse_vector", "knn_vector":
		return driver.KindArray
	}
	return driver.KindOther
}

// mappings reads the mappings of an index, data stream or alias; several
// backing indices merge, the newest field mapping winning.
func (c *conn) mappings(ctx context.Context, name string) (*layout, error) {
	var resp map[string]struct {
		Mappings struct {
			Properties map[string]json.RawMessage `json:"properties"`
			Meta       struct {
				PrimaryKey string `json:"primary_key"`
			} `json:"_meta"`
		} `json:"mappings"`
	}
	if err := c.do(ctx, "GET", "/"+esc(name)+"/_mapping", nil, &resp); err != nil {
		return nil, mapError(err)
	}
	names := make([]string, 0, len(resp))
	for n := range resp {
		names = append(names, n)
	}
	sort.Strings(names)
	l := &layout{fields: map[string]field{}}
	for _, n := range names {
		m := resp[n].Mappings
		if m.Meta.PrimaryKey != "" {
			l.pk = m.Meta.PrimaryKey
		}
		for fname, raw := range m.Properties {
			var p struct {
				Type       string                     `json:"type"`
				Properties map[string]json.RawMessage `json:"properties"`
				Fields     map[string]struct {
					Type string `json:"type"`
				} `json:"fields"`
			}
			json.Unmarshal(raw, &p)
			f := field{typ: p.Type, mapping: raw}
			if f.typ == "" && p.Properties != nil {
				f.typ = "object"
			}
			for sub, sf := range p.Fields {
				if sf.Type == "keyword" && (f.keyword == "" || sub == "keyword") {
					f.keyword = fname + "." + sub
				}
			}
			l.fields[fname] = f
		}
	}
	return l, nil
}

func (l *layout) columns() []driver.Column {
	cols := []driver.Column{{Name: "_id", Type: "keyword", BaseType: "_id", Kind: driver.KindString, PrimaryKey: true}}
	names := make([]string, 0, len(l.fields))
	for n := range l.fields {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		f := l.fields[n]
		col := driver.Column{Name: n, Type: f.typ, BaseType: f.typ, Kind: kindOf(f.typ), Nullable: true}
		if f.keyword != "" {
			col.Comment = "exact matches and sorting use " + f.keyword
		}
		if f.typ == "geo_point" || f.typ == "geo_shape" {
			col.SRID, col.GeometryType = 4326, map[string]string{"geo_point": "point"}[f.typ]
		}
		cols = append(cols, col)
	}
	return cols
}

func (c *conn) describe(ctx context.Context, ref driver.ObjectRef) (*driver.Table, *layout, error) {
	l, err := c.mappings(ctx, ref.Name)
	if err != nil {
		return nil, nil, err
	}
	kind := ref.Kind
	if kind == "" {
		kind = c.kindOf(ctx, ref.Name)
	}
	t := &driver.Table{Ref: driver.ObjectRef{Name: ref.Name, Kind: kind}, Kind: kind, Columns: l.columns(), Indexes: []driver.Index{},
		ForeignKeys: []driver.ForeignKey{}, Referenced: []driver.ForeignKey{}, Checks: []driver.Check{}, Triggers: []driver.Trigger{},
		PrimaryKey: []string{"_id"}, RowKey: []string{"_id"}, RowKeyKind: "_id", Options: map[string]string{}}
	if l.pk != "" {
		t.Options["primary_key"] = l.pk + " (copied into _id)"
	}
	// Data streams take new documents only; edits go to a backing index.
	t.Editable = !c.ro && kind != "data_stream"

	var stats struct {
		All struct {
			Primaries struct {
				Docs struct {
					Count int64 `json:"count"`
				} `json:"docs"`
			} `json:"primaries"`
			Total struct {
				Store struct {
					Size int64 `json:"size_in_bytes"`
				} `json:"store"`
			} `json:"total"`
		} `json:"_all"`
	}
	if c.do(ctx, "GET", "/"+esc(ref.Name)+"/_stats/docs,store", nil, &stats) == nil {
		n, s := stats.All.Primaries.Docs.Count, stats.All.Total.Store.Size
		t.RowEstimate, t.Size = &n, &s
	}
	var settings map[string]struct {
		Settings struct {
			Index map[string]any `json:"index"`
		} `json:"settings"`
	}
	if c.do(ctx, "GET", "/"+esc(ref.Name)+"/_settings", nil, &settings) == nil {
		names := make([]string, 0, len(settings))
		for n := range settings {
			names = append(names, n)
		}
		sort.Strings(names)
		if len(names) > 0 {
			ix := settings[names[len(names)-1]].Settings.Index
			for _, k := range []string{"number_of_shards", "number_of_replicas", "refresh_interval"} {
				if v, ok := ix[k].(string); ok {
					t.Options[k] = v
				}
			}
			if kind != "table" {
				t.Options["indices"] = strings.Join(names, ", ")
			}
		}
	}
	t.DDL = createRequest(ref.Name, t.Options, l)
	return t, l, nil
}

func (c *conn) Describe(ctx context.Context, ref driver.ObjectRef) (*driver.Table, error) {
	t, _, err := c.describe(ctx, ref)
	return t, err
}

// kindOf tells an index from a data stream or an alias.
func (c *conn) kindOf(ctx context.Context, name string) string {
	var ds struct {
		DataStreams []json.RawMessage `json:"data_streams"`
	}
	if c.do(ctx, "GET", "/_data_stream/"+esc(name), nil, &ds) == nil && len(ds.DataStreams) > 0 {
		return "data_stream"
	}
	if _, _, err := c.raw(ctx, "HEAD", "/_alias/"+esc(name), nil); err == nil {
		return "alias"
	}
	return "table"
}

// createRequest writes an index as the console request that creates it.
func createRequest(name string, opts map[string]string, l *layout) string {
	var b bytes.Buffer
	b.WriteString(`{"settings":{"index":{`)
	first := true
	for _, k := range []string{"number_of_shards", "number_of_replicas", "refresh_interval"} {
		if v := opts[k]; v != "" {
			if !first {
				b.WriteByte(',')
			}
			first = false
			kb, _ := json.Marshal(k)
			vb, _ := json.Marshal(v)
			b.Write(kb)
			b.WriteByte(':')
			b.Write(vb)
		}
	}
	b.WriteString(`}},"mappings":{`)
	if l.pk != "" {
		pb, _ := json.Marshal(l.pk)
		b.WriteString(`"_meta":{"primary_key":`)
		b.Write(pb)
		b.WriteString(`},`)
	}
	b.WriteString(`"properties":{`)
	names := make([]string, 0, len(l.fields))
	for n := range l.fields {
		names = append(names, n)
	}
	sort.Strings(names)
	for i, n := range names {
		if i > 0 {
			b.WriteByte(',')
		}
		nb, _ := json.Marshal(n)
		b.Write(nb)
		b.WriteByte(':')
		b.Write(l.fields[n].mapping)
	}
	b.WriteString(`}}}`)
	var pretty bytes.Buffer
	json.Indent(&pretty, b.Bytes(), "", "  ")
	return "PUT /" + name + "\n" + pretty.String()
}

// ---- values ------------------------------------------------------------------------

// encode turns a JSON value from _source into a cell.
func encode(v any, f field) any {
	switch x := v.(type) {
	case nil:
		return nil
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return driver.EncodeInt(i)
		}
		if kindOf(f.typ) == driver.KindDecimal {
			return x.String()
		}
		fl, err := x.Float64()
		if err != nil {
			return x.String()
		}
		return driver.EncodeFloat(fl)
	case string:
		if f.typ == "geo_point" {
			if g := geoPoint(x); g != nil {
				return g
			}
		}
		return driver.EncodeText(x)
	case map[string]any:
		if g := geoValue(x, f.typ); g != nil {
			return g
		}
		return doc(x)
	case []any:
		if f.typ == "geo_point" && len(x) == 2 {
			if g := geoValue(x, f.typ); g != nil {
				return g
			}
		}
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = encode(e, field{})
		}
		return out
	}
	return v
}

func doc(m map[string]any) driver.Doc {
	d := driver.Doc{Values: map[string]any{}}
	for k, v := range m {
		d.Keys = append(d.Keys, k)
		d.Values[k] = encode(v, field{})
	}
	sort.Strings(d.Keys)
	return d
}

var latLon = regexp.MustCompile(`^\s*(-?[\d.]+)\s*,\s*(-?[\d.]+)\s*$`)

func geoCell(gj map[string]any) map[string]any {
	return map[string]any{"$geo": gj, "srid": 4326}
}

// geoPoint reads "lat,lon".
func geoPoint(s string) any {
	m := latLon.FindStringSubmatch(s)
	if m == nil {
		return nil
	}
	lat, _ := strconv.ParseFloat(m[1], 64)
	lon, _ := strconv.ParseFloat(m[2], 64)
	return geoCell(map[string]any{"type": "Point", "coordinates": []any{lon, lat}})
}

// geoValue reads a geo_point ({lat, lon}, [lon, lat] or GeoJSON) or a
// geo_shape (GeoJSON) for the map.
func geoValue(v any, typ string) any {
	num := func(a any) (float64, bool) {
		switch n := a.(type) {
		case json.Number:
			f, err := n.Float64()
			return f, err == nil
		case float64:
			return n, true
		}
		return 0, false
	}
	switch x := v.(type) {
	case []any:
		lon, ok1 := num(x[0])
		lat, ok2 := num(x[1])
		if ok1 && ok2 {
			return geoCell(map[string]any{"type": "Point", "coordinates": []any{lon, lat}})
		}
	case map[string]any:
		if typ != "geo_point" && typ != "geo_shape" {
			return nil
		}
		if lat, ok := num(x["lat"]); ok {
			if lon, ok := num(x["lon"]); ok {
				return geoCell(map[string]any{"type": "Point", "coordinates": []any{lon, lat}})
			}
		}
		if _, ok := x["type"].(string); ok && x["coordinates"] != nil || x["geometries"] != nil {
			b, _ := json.Marshal(x)
			var gj map[string]any
			json.Unmarshal(b, &gj)
			return geoCell(gj)
		}
	}
	return nil
}

var spacedTime = regexp.MustCompile(`^(\d{4}-\d\d-\d\d) (\d\d:\d\d)`)

// inputValue turns an edited cell into the JSON value the field takes.
func inputValue(col *driver.Column, v any) (any, error) {
	if m, ok := v.(map[string]any); ok {
		switch {
		case m["$null"] == true:
			return nil, nil
		case m["$default"] == true || m["$expr"] != nil:
			return nil, fmt.Errorf("field %s: documents take values, not expressions", col.Name)
		}
	}
	if v == nil {
		return nil, nil
	}
	switch col.Kind {
	case driver.KindInt, driver.KindFloat, driver.KindDecimal:
		switch x := v.(type) {
		case float64:
			if col.Kind != driver.KindFloat && x == math.Trunc(x) && math.Abs(x) < 1e15 {
				return json.Number(strconv.FormatInt(int64(x), 10)), nil
			}
			return x, nil
		case string:
			s := strings.TrimSpace(x)
			if _, err := strconv.ParseFloat(s, 64); err != nil {
				return nil, fmt.Errorf("field %s: %q is not a number", col.Name, x)
			}
			return json.Number(s), nil
		}
	case driver.KindBool:
		if s, ok := v.(string); ok {
			b, err := strconv.ParseBool(strings.TrimSpace(s))
			if err != nil {
				return nil, fmt.Errorf("field %s: %q is not true or false", col.Name, s)
			}
			return b, nil
		}
	case driver.KindTimestamp:
		if s, ok := v.(string); ok {
			// The default date format wants a T between date and time.
			return spacedTime.ReplaceAllString(strings.TrimSpace(s), "${1}T$2"), nil
		}
	case driver.KindBinary:
		if m, ok := v.(map[string]any); ok {
			if b64, ok := m["$bin"].(string); ok {
				if _, err := base64.StdEncoding.DecodeString(b64); err != nil {
					return nil, fmt.Errorf("field %s: invalid binary value", col.Name)
				}
				return b64, nil
			}
		}
	case driver.KindGeometry:
		if m, ok := v.(map[string]any); ok {
			gj, _ := m["$geo"].(map[string]any)
			if raw, ok := m["$geo"].(json.RawMessage); ok { // from a migration
				json.Unmarshal(raw, &gj)
			}
			if gj != nil {
				if col.Type == "geo_point" && gj["type"] == "Point" {
					if c, ok := gj["coordinates"].([]any); ok && len(c) == 2 {
						return map[string]any{"lat": c[1], "lon": c[0]}, nil
					}
				}
				return gj, nil
			}
		}
		if s, ok := v.(string); ok && json.Valid([]byte(s)) {
			var out any
			json.Unmarshal([]byte(s), &out)
			return out, nil
		}
	case driver.KindObject, driver.KindArray, driver.KindOther:
		if s, ok := v.(string); ok {
			t := strings.TrimSpace(s)
			if (strings.HasPrefix(t, "{") || strings.HasPrefix(t, "[")) && json.Valid([]byte(t)) {
				dec := json.NewDecoder(strings.NewReader(t))
				dec.UseNumber()
				var out any
				dec.Decode(&out)
				return out, nil
			}
		}
	}
	return v, nil
}
