package elastic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"rowsmith/internal/driver"
)

var (
	_ driver.BrowseQuerier = (*conn)(nil)
	_ driver.BulkImporter  = (*conn)(nil)
)

// maxWindow is how deep from and size page by default (index.max_result_window).
const maxWindow = 10000

func colIndex(t *driver.Table) map[string]*driver.Column {
	m := map[string]*driver.Column{}
	for i := range t.Columns {
		m[t.Columns[i].Name] = &t.Columns[i]
	}
	return m
}

// exact is the field to match exactly and sort by: a text field's keyword
// sub-field when it has one.
func exact(l *layout, name string) (string, bool) {
	if name == "_id" {
		return "_id", true
	}
	f, ok := l.fields[name]
	if !ok {
		return name, false
	}
	if kindOf(f.typ) == driver.KindText {
		if f.keyword != "" {
			return f.keyword, true
		}
		return name, false
	}
	return name, true
}

// query writes a browse request's filters and search as a bool query.
func query(t *driver.Table, l *layout, req driver.BrowseRequest) (map[string]any, error) {
	cols := colIndex(t)
	var must, not []any
	value := func(c *driver.Column, v any) (any, error) {
		if c.Name == "_id" {
			return fmt.Sprint(v), nil
		}
		return inputValue(c, v)
	}
	for _, f := range req.Filters {
		c := cols[f.Column]
		if c == nil {
			return nil, fmt.Errorf("field %s is not in the mapping", f.Column)
		}
		name, ok := exact(l, c.Name)
		text := c.Kind == driver.KindText && !ok
		switch f.Op {
		case "=", "!=":
			v, err := value(c, f.Value)
			if err != nil {
				return nil, err
			}
			var q any
			switch {
			case c.Name == "_id":
				q = map[string]any{"ids": map[string]any{"values": []any{v}}}
			case text:
				q = map[string]any{"match_phrase": map[string]any{name: v}}
			default:
				q = map[string]any{"term": map[string]any{name: v}}
			}
			if f.Op == "=" {
				must = append(must, q)
			} else {
				not = append(not, q)
			}
		case "<", "<=", ">", ">=", "between":
			r := map[string]any{}
			op := map[string]string{"<": "lt", "<=": "lte", ">": "gt", ">=": "gte"}[f.Op]
			vals := []any{f.Value}
			ops := []string{op}
			if f.Op == "between" {
				if len(f.Values) != 2 {
					return nil, fmt.Errorf("%s: between needs two values", f.Column)
				}
				vals, ops = f.Values, []string{"gte", "lte"}
			}
			for i, v := range vals {
				x, err := value(c, v)
				if err != nil {
					return nil, err
				}
				r[ops[i]] = x
			}
			must = append(must, map[string]any{"range": map[string]any{name: r}})
		case "in", "notin":
			var vals []any
			for _, v := range f.Values {
				x, err := value(c, v)
				if err != nil {
					return nil, err
				}
				vals = append(vals, x)
			}
			q := map[string]any{"terms": map[string]any{name: vals}}
			if c.Name == "_id" {
				q = map[string]any{"ids": map[string]any{"values": vals}}
			}
			if f.Op == "in" {
				must = append(must, q)
			} else {
				not = append(not, q)
			}
		case "contains", "startswith", "endswith", "like", "notlike":
			s := fmt.Sprint(f.Value)
			var q any
			if text {
				switch f.Op {
				case "startswith":
					q = map[string]any{"match_phrase_prefix": map[string]any{name: s}}
				case "endswith", "like", "notlike":
					return nil, fmt.Errorf("%s: a text field matches words; use contains or starts with, or add a keyword sub-field", f.Column)
				default:
					q = map[string]any{"match_phrase": map[string]any{name: s}}
				}
			} else {
				pat := strings.NewReplacer(`\`, `\\`, "*", `\*`, "?", `\?`).Replace(s)
				switch f.Op {
				case "contains":
					pat = "*" + pat + "*"
				case "startswith":
					pat += "*"
				case "endswith":
					pat = "*" + pat
				default: // SQL LIKE: % and _
					pat = strings.NewReplacer("%", "*", "_", "?").Replace(pat)
				}
				q = map[string]any{"wildcard": map[string]any{name: map[string]any{"value": pat, "case_insensitive": true}}}
			}
			if f.Op == "notlike" {
				not = append(not, q)
			} else {
				must = append(must, q)
			}
		case "regexp":
			must = append(must, map[string]any{"regexp": map[string]any{name: fmt.Sprint(f.Value)}})
		case "null":
			not = append(not, map[string]any{"exists": map[string]any{"field": c.Name}})
		case "notnull":
			must = append(must, map[string]any{"exists": map[string]any{"field": c.Name}})
		default:
			return nil, fmt.Errorf("%s: unsupported filter %q", f.Column, f.Op)
		}
	}
	if s := strings.TrimSpace(req.Search); s != "" {
		must = append(must, map[string]any{"simple_query_string": map[string]any{"query": s, "default_operator": "and", "lenient": true}})
	}
	if w := strings.TrimSpace(req.Where); w != "" {
		// A query as JSON, or Lucene query syntax.
		if strings.HasPrefix(w, "{") {
			var q any
			if err := json.Unmarshal([]byte(w), &q); err != nil {
				return nil, fmt.Errorf("the filter is not valid JSON: %v", err)
			}
			must = append(must, q)
		} else {
			must = append(must, map[string]any{"query_string": map[string]any{"query": w, "lenient": true}})
		}
	}
	if len(must) == 0 && len(not) == 0 {
		return map[string]any{"match_all": map[string]any{}}, nil
	}
	b := map[string]any{}
	if len(must) > 0 {
		b["filter"] = must
	}
	if len(not) > 0 {
		b["must_not"] = not
	}
	return map[string]any{"bool": b}, nil
}

func sorts(l *layout, ss []driver.Sort) ([]any, error) {
	var out []any
	for _, s := range ss {
		if s.Column == "_id" {
			return nil, errors.New("Elasticsearch doesn't sort by _id; sort by a field instead")
		}
		name, ok := exact(l, s.Column)
		if !ok {
			return nil, fmt.Errorf("%s is a text field without a keyword sub-field, so it can't be sorted", s.Column)
		}
		out = append(out, map[string]any{name: map[string]any{"order": map[bool]string{true: "desc", false: "asc"}[s.Desc], "unmapped_type": "keyword"}})
	}
	return out, nil
}

func (c *conn) searchBody(t *driver.Table, l *layout, req driver.BrowseRequest) (map[string]any, error) {
	q, err := query(t, l, req)
	if err != nil {
		return nil, err
	}
	body := map[string]any{"query": q}
	if len(req.Sort) > 0 {
		s, err := sorts(l, req.Sort)
		if err != nil {
			return nil, err
		}
		body["sort"] = s
	}
	if len(req.Columns) > 0 {
		var src []any
		for _, n := range req.Columns {
			if n != "_id" {
				src = append(src, n)
			}
		}
		body["_source"] = src
	}
	return body, nil
}

func (c *conn) Browse(ctx context.Context, req driver.BrowseRequest) (*driver.Result, error) {
	start := time.Now()
	t, l, err := c.describe(ctx, req.Ref)
	if err != nil {
		return nil, err
	}
	limit := req.Limit
	if limit <= 0 {
		limit = 100
	}
	if req.Offset+int64(limit) > maxWindow {
		return nil, fmt.Errorf("Elasticsearch pages through the first %d documents; filter or sort to reach the others", maxWindow)
	}
	body, err := c.searchBody(t, l, req)
	if err != nil {
		return nil, err
	}
	body["from"], body["size"], body["track_total_hits"] = req.Offset, limit, true
	var reply searchReply
	path := "/" + esc(t.Ref.Name) + "/_search"
	if err := c.do(ctx, "POST", path, body, &reply); err != nil {
		return nil, mapError(err)
	}
	cols := t.Columns
	if len(req.Columns) > 0 {
		cols = nil
		byName := colIndex(t)
		for _, n := range req.Columns {
			if c := byName[n]; c != nil {
				cols = append(cols, *c)
			}
		}
	}
	res := &driver.Result{Rows: [][]any{}}
	for _, col := range cols {
		res.Columns = append(res.Columns, driver.ResultColumn{Name: col.Name, Type: col.Type, Kind: col.Kind})
	}
	size := 0
	for _, h := range reply.Hits.Hits {
		row := make([]any, len(cols))
		for i, col := range cols {
			if col.Name == "_id" {
				row[i] = h.ID
				continue
			}
			row[i] = encode(h.Source[col.Name], l.fields[col.Name])
		}
		driver.PreviewRow(row)
		res.Rows = append(res.Rows, row)
		if size += driver.RowSize(row); size > driver.BrowseBudget {
			res.Truncated, res.Clipped = true, true
			break
		}
	}
	if total := totalHits(reply.Hits.Total); total != "" {
		var n int64
		fmt.Sscan(total, &n)
		res.Truncated = res.Truncated || req.Offset+int64(len(res.Rows)) < n
	}
	pretty, _ := json.MarshalIndent(body, "", "  ")
	res.SQL = "GET " + path + "\n" + string(pretty)
	res.Duration = time.Since(start)
	res.DurationMS = float64(res.Duration.Microseconds()) / 1000
	return res, nil
}

func (c *conn) Count(ctx context.Context, req driver.BrowseRequest) (driver.Count, error) {
	t, l, err := c.describe(ctx, req.Ref)
	if err != nil {
		return driver.Count{}, err
	}
	q, err := query(t, l, req)
	if err != nil {
		return driver.Count{}, err
	}
	var reply struct {
		Count int64 `json:"count"`
	}
	if err := c.do(ctx, "POST", "/"+esc(t.Ref.Name)+"/_count", map[string]any{"query": q}, &reply); err != nil {
		return driver.Count{}, mapError(err)
	}
	return driver.Count{Rows: reply.Count, Exact: true}, nil
}

// BrowseQuery is the whole (filtered) index as a console search, which the
// console reads with a scroll, for exports and migrations.
func (c *conn) BrowseQuery(ctx context.Context, t *driver.Table, req driver.BrowseRequest) (string, []any, error) {
	l, err := c.mappings(ctx, t.Ref.Name)
	if err != nil {
		return "", nil, err
	}
	if len(req.Columns) == 0 {
		for _, col := range t.Columns {
			req.Columns = append(req.Columns, col.Name)
		}
	}
	body, err := c.searchBody(t, l, req)
	if err != nil {
		return "", nil, err
	}
	b, _ := json.MarshalIndent(body, "", "  ")
	return "GET /" + t.Ref.Name + "/_search\n" + string(b), nil, nil
}

// ---- edits ---------------------------------------------------------------------------

type version struct {
	SeqNo       int64 `json:"_seq_no"`
	PrimaryTerm int64 `json:"_primary_term"`
	Found       bool  `json:"found"`
	Index       string
}

// docVersion reads a document's sequence number and primary term, which
// the change sends back so a document changed meanwhile is not overwritten.
func (c *conn) docVersion(ctx context.Context, target, id string) (*version, error) {
	var v struct {
		version
		Index string `json:"_index"`
	}
	status, data, err := c.raw(ctx, "GET", "/"+esc(target)+"/_doc/"+esc(id)+"?_source=false", nil)
	if status == 404 {
		return nil, errors.New("the document no longer exists or was modified by someone else")
	}
	if err != nil {
		return nil, mapError(err)
	}
	json.Unmarshal(data, &v)
	if !v.Found {
		return nil, errors.New("the document no longer exists or was modified by someone else")
	}
	v.version.Index = v.Index
	return &v.version, nil
}

// ApplyEdits writes each change as its own request, refreshing so the grid
// sees it. Updates and deletes name the version they were checked against:
// a document changed in between fails instead of being overwritten.
func (c *conn) ApplyEdits(ctx context.Context, ref driver.ObjectRef, edits []driver.RowEdit) (*driver.EditResult, error) {
	if c.ro {
		return nil, driver.ErrReadOnly
	}
	t, l, err := c.describe(ctx, ref)
	if err != nil {
		return nil, err
	}
	if !t.Editable {
		return nil, errors.New("a data stream takes new documents only; edit them in its backing index")
	}
	cols := colIndex(t)
	versions := make([]*version, len(edits))
	for i, ed := range edits {
		if ed.Op == "update" || ed.Op == "delete" {
			id, _ := ed.Key["_id"].(string)
			if id == "" {
				return nil, fmt.Errorf("change %d: missing _id", i+1)
			}
			if versions[i], err = c.docVersion(ctx, t.Ref.Name, id); err != nil {
				return nil, fmt.Errorf("change %d: %w", i+1, err)
			}
		}
	}
	res := &driver.EditResult{}
	fail := func(i int, err error) error {
		err = fmt.Errorf("change %d: %w", i+1, err)
		if res.Applied > 0 {
			err = fmt.Errorf("%w (the %d changes before it were saved)", err, res.Applied)
		}
		return err
	}
	docOf := func(values map[string]any) (map[string]any, string, error) {
		d := map[string]any{}
		id := ""
		for k, v := range values {
			if k == "_id" {
				id = fmt.Sprint(v)
				continue
			}
			c := cols[k]
			if c == nil {
				c = &driver.Column{Name: k, Kind: driver.KindOther}
			}
			x, err := inputValue(c, v)
			if err != nil {
				return nil, "", err
			}
			d[k] = x
		}
		if id == "" && l.pk != "" && d[l.pk] != nil {
			id = fmt.Sprint(d[l.pk])
		}
		return d, id, nil
	}
	for i, ed := range edits {
		var method, path string
		var body any
		switch ed.Op {
		case "insert":
			d, id, err := docOf(ed.Values)
			if err != nil {
				return nil, fail(i, err)
			}
			method, path, body = "POST", "/"+esc(t.Ref.Name)+"/_doc?refresh=wait_for", d
			if id != "" {
				// _create fails when the id is taken, rather than replacing that document.
				method, path = "PUT", "/"+esc(t.Ref.Name)+"/_create/"+esc(id)+"?refresh=wait_for"
			}
		case "update":
			d, _, err := docOf(ed.Values)
			if err != nil {
				return nil, fail(i, err)
			}
			if len(d) == 0 {
				continue
			}
			v := versions[i]
			method, body = "POST", map[string]any{"doc": d}
			path = fmt.Sprintf("/%s/_update/%s?if_seq_no=%d&if_primary_term=%d&refresh=wait_for", esc(v.Index), esc(ed.Key["_id"].(string)), v.SeqNo, v.PrimaryTerm)
		case "delete":
			v := versions[i]
			method = "DELETE"
			path = fmt.Sprintf("/%s/_doc/%s?if_seq_no=%d&if_primary_term=%d&refresh=wait_for", esc(v.Index), esc(ed.Key["_id"].(string)), v.SeqNo, v.PrimaryTerm)
		default:
			return nil, fail(i, fmt.Errorf("unknown edit operation %q", ed.Op))
		}
		var reply struct {
			ID string `json:"_id"`
		}
		status, data, err := c.raw(ctx, method, path, body)
		if status == 409 {
			if ed.Op == "insert" {
				return nil, fail(i, errors.New("a document with this _id already exists"))
			}
			return nil, fail(i, errors.New("the document was modified by someone else; reload it and try again"))
		}
		if err != nil {
			return nil, fail(i, mapError(err))
		}
		json.Unmarshal(data, &reply)
		text := method + " " + path
		if body != nil {
			b, _ := json.Marshal(body)
			text += "\n" + string(b)
		}
		res.Statements = append(res.Statements, text)
		res.Applied++
		if ed.Op == "insert" {
			res.Inserted = append(res.Inserted, []any{reply.ID})
		}
	}
	return res, nil
}

// ---- imports ---------------------------------------------------------------------------

// BeginImport loads documents with _bulk. An _id column, or the index's
// primary key field, gives each document its id, so a repeated import
// replaces documents rather than adding copies. Nothing can be rolled back.
func (c *conn) BeginImport(ctx context.Context, t *driver.Table, cols []string, empty bool) (driver.RowImporter, error) {
	if c.ro {
		return nil, driver.ErrReadOnly
	}
	l, err := c.mappings(ctx, t.Ref.Name)
	if err != nil {
		return nil, err
	}
	byName := colIndex(t)
	im := &importer{c: c, index: t.Ref.Name, idCol: -1, op: "index"}
	if t.Kind == "data_stream" {
		im.op = "create" // data streams take create only
	}
	for i, n := range cols {
		col := byName[n]
		if col == nil {
			col = &driver.Column{Name: n, Kind: driver.KindOther}
		}
		im.cols = append(im.cols, col)
		if n == "_id" || (n == l.pk && im.idCol < 0) {
			im.idCol = i
		}
	}
	if empty {
		if err := c.do(ctx, "POST", "/"+esc(t.Ref.Name)+"/_delete_by_query?refresh=true&conflicts=proceed", map[string]any{"query": map[string]any{"match_all": map[string]any{}}}, nil); err != nil {
			return nil, mapError(err)
		}
	}
	return im, nil
}

type importer struct {
	c     *conn
	index string
	cols  []*driver.Column
	idCol int
	op    string
}

func (im *importer) Insert(ctx context.Context, rows [][]any) error {
	for start := 0; start < len(rows); start += 1000 {
		var b bytes.Buffer
		for n, r := range rows[start:min(start+1000, len(rows))] {
			action := map[string]any{"_index": im.index}
			if im.idCol >= 0 && im.idCol < len(r) && r[im.idCol] != nil {
				action["_id"] = fmt.Sprint(r[im.idCol])
			}
			d := map[string]any{}
			for j, col := range im.cols {
				if col.Name == "_id" || j >= len(r) || r[j] == nil {
					continue
				}
				v, err := inputValue(col, r[j])
				if err != nil {
					return fmt.Errorf("row %d: %w", start+n+1, err)
				}
				d[col.Name] = v
			}
			ab, _ := json.Marshal(map[string]any{im.op: action})
			db, err := json.Marshal(d)
			if err != nil {
				return fmt.Errorf("row %d: %w", start+n+1, err)
			}
			b.Write(ab)
			b.WriteByte('\n')
			b.Write(db)
			b.WriteByte('\n')
		}
		var reply struct {
			Errors bool `json:"errors"`
			Items  []map[string]struct {
				Status int `json:"status"`
				Error  *struct {
					Type   string `json:"type"`
					Reason string `json:"reason"`
				} `json:"error"`
			} `json:"items"`
		}
		if err := im.c.do(ctx, "POST", "/_bulk", ndjson(b.String()), &reply); err != nil {
			return mapError(err)
		}
		if reply.Errors {
			for n, it := range reply.Items {
				for _, r := range it {
					if r.Error != nil {
						return fmt.Errorf("row %d: %s", start+n+1, r.Error.Reason)
					}
				}
			}
		}
	}
	return nil
}

func (im *importer) Commit() error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	return mapError(im.c.do(ctx, "POST", "/"+esc(im.index)+"/_refresh", nil, nil))
}

func (im *importer) Rollback() error { return nil }
