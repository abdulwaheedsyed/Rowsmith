package elastic

import (
	"encoding/json"
	"strings"
	"testing"

	"rowsmith/internal/driver"
)

func TestParseScript(t *testing.T) {
	script := `# a comment
GET /orders/_search
{
  "query": { "match": { "note": """two
lines""" } }
}

POST _bulk
{"index": {"_index": "orders"}}
{"a": 1}

DELETE /orders`
	reqs, err := parseScript(script)
	if err != nil || len(reqs) != 3 {
		t.Fatalf("requests = %+v, %v", reqs, err)
	}
	if reqs[0].method != "GET" || reqs[0].line != 2 || !strings.Contains(string(reqs[0].body), `"two\nlines"`) {
		t.Errorf("first = %+v (%s)", reqs[0], reqs[0].body)
	}
	if string(reqs[1].body) != "{\"index\":{\"_index\":\"orders\"}}\n{\"a\":1}\n" {
		t.Errorf("bulk body = %q", reqs[1].body)
	}
	if reqs[2].kind() != driver.StmtDDL || reqs[2].danger(reqs[2].kind()).Level != "destructive" {
		t.Errorf("delete = %v %v", reqs[2].kind(), reqs[2].danger(reqs[2].kind()))
	}
	if _, err := parseScript("GET /x/_search\n{ broken"); err == nil || err.(*SyntaxError).Line != 2 {
		t.Errorf("bad body: %v", err)
	}
	if _, err := parseScript("hello"); err == nil {
		t.Error("parsed a line that is not a request")
	}
	for path, want := range map[string]driver.StatementKind{
		"POST /orders/_search": driver.StmtRead, "POST /_sql?format=json": driver.StmtRead, "POST /orders/_explain/1": driver.StmtRead,
		"POST /orders/_doc": driver.StmtWrite, "PUT /orders/_mapping": driver.StmtDDL, "POST /_security/user/x": driver.StmtDCL,
		"POST /orders/_delete_by_query": driver.StmtWrite,
	} {
		r, _ := parseScript(path)
		if k := r[0].kind(); k != want {
			t.Errorf("%s = %s, want %s", path, k, want)
		}
	}
}

func decode(s string) any {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var v any
	dec.Decode(&v)
	return v
}

func TestRender(t *testing.T) {
	sets := render("orders/_search", decode(`{"took": 3, "hits": {"total": {"value": 12, "relation": "eq"}, "hits": [
		{"_id": "1", "_source": {"b": 1, "a": {"x": true}}}, {"_id": "2", "_source": {"c": "z"}}]},
		"aggregations": {"by": {"buckets": [{"key": "tea", "doc_count": 4, "avg": {"value": 2.5}}]}, "max": {"value": 9}}}`), nil)
	if len(sets) != 3 {
		t.Fatalf("sets = %d", len(sets))
	}
	if n := []string{sets[0].cols[0].Name, sets[0].cols[1].Name, sets[0].cols[2].Name, sets[0].cols[3].Name}; strings.Join(n, ",") != "_id,a,b,c" ||
		sets[0].notice != "2 of 12 hits in 3 ms" || len(sets[0].rows[0]) != 4 {
		t.Errorf("hits = %+v", sets[0])
	}
	if sets[1].cols[2].Name != "avg" || sets[1].rows[0][2] != 2.5 || sets[2].rows[0][0] != int64(9) {
		t.Errorf("aggregations = %+v %+v", sets[1], sets[2])
	}
	sql := render("_sql", decode(`{"columns": [{"name": "n", "type": "long"}], "rows": [[1], [2]]}`), nil)
	if len(sql[0].rows) != 2 || sql[0].cols[0].Kind != driver.KindInt {
		t.Errorf("sql = %+v", sql)
	}
	ack := render("orders", decode(`{"acknowledged": true, "index": "orders"}`), nil)
	if len(ack[0].rows) != 2 || ack[0].rows[0][0] != "acknowledged" {
		t.Errorf("ack = %+v", ack)
	}
}

func TestValues(t *testing.T) {
	geo, _ := json.Marshal(encode(decode(`{"lat": 24.7, "lon": 46.6}`), field{typ: "geo_point"}))
	if string(geo) != `{"$geo":{"coordinates":[46.6,24.7],"type":"Point"},"srid":4326}` {
		t.Errorf("geo = %s", geo)
	}
	pt := &driver.Column{Name: "w", Type: "geo_point", Kind: driver.KindGeometry}
	if v, err := inputValue(pt, map[string]any{"$geo": map[string]any{"type": "Point", "coordinates": []any{46.6, 24.7}}}); err != nil {
		t.Error(err)
	} else if b, _ := json.Marshal(v); string(b) != `{"lat":24.7,"lon":46.6}` {
		t.Errorf("geo input = %s", b)
	}
	if v, _ := inputValue(pt, map[string]any{"$geo": json.RawMessage(`{"type":"Point","coordinates":[1,2]}`)}); v == nil {
		t.Error("raw GeoJSON was not read")
	} else if b, _ := json.Marshal(v); string(b) != `{"lat":2,"lon":1}` {
		t.Errorf("raw geo input = %s", b)
	}
	at := &driver.Column{Name: "at", Kind: driver.KindTimestamp}
	if v, _ := inputValue(at, "2025-01-02 03:04:05+00:00"); v != "2025-01-02T03:04:05+00:00" {
		t.Errorf("date = %v", v)
	}
	n := &driver.Column{Name: "n", Kind: driver.KindInt}
	if _, err := inputValue(n, "abc"); err == nil {
		t.Error("abc read as a number")
	}
	big := encode(json.Number("18446744073709551615"), field{typ: "unsigned_long"})
	if big != "18446744073709551615" {
		t.Errorf("unsigned_long = %v", big)
	}
}

func TestDDL(t *testing.T) {
	c := &conn{}
	q, err := c.CreateTableSQL(driver.TableDef{Ref: driver.ObjectRef{Name: "notes"}, PrimaryKey: []string{"code"}, Options: map[string]string{"number_of_shards": "2"},
		Columns: []driver.ColumnDef{{Column: driver.Column{Name: "code", Type: "keyword"}}, {Column: driver.Column{Name: "body", Type: "text"}},
			{Column: driver.Column{Name: "price", Type: `{"type": "scaled_float", "scaling_factor": 100}`}}}})
	if err != nil || !strings.HasPrefix(q[0], "PUT /notes\n") || !strings.Contains(q[0], `"primary_key": "code"`) || !strings.Contains(q[0], `"scaling_factor": 100`) ||
		!strings.Contains(q[0], `"keyword": {`) {
		t.Errorf("create = %q, %v", q, err)
	}
	if _, err := c.CreateTableSQL(driver.TableDef{Ref: driver.ObjectRef{Name: "Bad Name"}}); err == nil {
		t.Error("accepted an invalid index name")
	}
}
