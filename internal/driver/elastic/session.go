package elastic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"rowsmith/internal/driver"
)

// session is a console: requests carry no state between runs.
type session struct {
	c  *conn
	mu sync.Mutex
}

func (c *conn) NewSession(context.Context, driver.Scope) (driver.Session, error) {
	return &session{c: c}, nil
}

func (s *session) InTransaction() bool { return false }
func (s *session) Close() error        { return nil }

func (s *session) Execute(ctx context.Context, script string, opts driver.ExecOptions, sink driver.Sink) error {
	if !s.mu.TryLock() {
		return errors.New("this console is still running a previous request")
	}
	defer s.mu.Unlock()
	reqs, err := parseScript(script)
	if err != nil {
		if e := sink.BeginStatement(driver.StatementInfo{SQL: strings.TrimSpace(script), Line: 1, Kind: driver.StmtUnknown}); e != nil {
			return e
		}
		qe := &driver.QueryError{Message: err.Error()}
		var se *SyntaxError
		if errors.As(err, &se) {
			qe = &driver.QueryError{Message: se.Msg, Line: se.Line}
		}
		return sink.EndStatement(qe)
	}
	for i, r := range reqs {
		k := r.kind()
		if err := sink.BeginStatement(driver.StatementInfo{Index: i, SQL: r.text, Line: r.line, Kind: k}); err != nil {
			return err
		}
		var runErr error
		if (opts.ReadOnly || s.c.ro) && !k.Safe() {
			runErr = &driver.QueryError{Message: "Blocked: this connection is read-only for you, and the request may modify data (" + string(k) + ")."}
		} else {
			runErr = s.c.run(ctx, r, opts, sink)
		}
		if runErr != nil {
			var qe *driver.QueryError
			if !errors.As(runErr, &qe) {
				qe = &driver.QueryError{Message: runErr.Error()}
				if m := mapError(runErr); m != runErr {
					qe = m.(*driver.QueryError)
				}
			}
			qe.Line = r.line
			runErr = qe
		}
		if ctx.Err() != nil && runErr != nil {
			runErr = &driver.QueryError{Message: "Cancelled"}
		}
		if err := sink.EndStatement(runErr); err != nil {
			return err
		}
		if runErr != nil && (opts.StopOnError || ctx.Err() != nil) {
			break
		}
	}
	return nil
}

func (c *conn) run(ctx context.Context, r request, opts driver.ExecOptions, sink driver.Sink) error {
	start := time.Now()
	ep := r.endpoint()
	path := "/" + strings.TrimPrefix(r.path, "/")
	// _cat answers in columns of text unless asked for JSON.
	if strings.HasPrefix(ep, "_cat/") && !strings.Contains(path, "format=") {
		path += map[bool]string{true: "&", false: "?"}[strings.Contains(path, "?")] + "format=json"
	}
	// A search's hits are typed by the index mapping, when it names one.
	var fields map[string]field
	if isSearch(ep) {
		if target, _, _ := strings.Cut(ep, "/"); target != "_search" && !strings.ContainsAny(target, "*,") {
			if l, err := c.mappings(ctx, target); err == nil {
				fields = l.fields
			}
		}
	}
	if isSearch(ep) && r.method != "HEAD" && wholeIndex(opts, r.body) {
		return c.scrollAll(ctx, path, r.body, fields, opts, sink, start)
	}
	var body any
	if r.body != nil {
		body = r.body
		if r.ndjson() {
			body = ndjson(r.body)
		}
	}
	status, data, err := c.raw(ctx, r.method, path, body)
	if err != nil {
		return err
	}
	if r.method == "HEAD" {
		return emit(sink, resultSet{cols: []driver.ResultColumn{col("status", driver.KindInt)}, rows: [][]any{{int64(status)}}}, opts, start)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return emit(sink, resultSet{cols: []driver.ResultColumn{col("response", driver.KindText)}, rows: [][]any{{driver.EncodeText(string(data))}}}, opts, start)
	}
	for _, rs := range render(ep, v, fields) {
		if err := emit(sink, rs, opts, start); err != nil {
			return err
		}
	}
	return nil
}

func isSearch(ep string) bool { return ep == "_search" || strings.HasSuffix(ep, "/_search") }

// wholeIndex says whether every hit is wanted: exports and migrations read
// with no row limit, of a search that names no size, from or aggregations
// of its own (those run as written).
func wholeIndex(opts driver.ExecOptions, body []byte) bool {
	if opts.MaxRows != 0 {
		return false
	}
	var q map[string]json.RawMessage
	if len(body) > 0 && json.Unmarshal(body, &q) != nil {
		return false
	}
	for _, k := range []string{"size", "from", "aggs", "aggregations", "search_after", "pit"} {
		if _, ok := q[k]; ok {
			return false
		}
	}
	return true
}

// scrollAll reads every hit of a search, a thousand at a time, as exports
// and migrations need: from and size stop at 10,000 hits.
func (c *conn) scrollAll(ctx context.Context, path string, body []byte, fields map[string]field, opts driver.ExecOptions, sink driver.Sink, start time.Time) error {
	q := map[string]any{}
	if body != nil {
		if err := json.Unmarshal(body, &q); err != nil {
			return err
		}
	}
	if _, ok := q["size"]; !ok {
		q["size"] = 1000
	}
	if _, ok := q["sort"]; !ok {
		q["sort"] = []any{"_doc"}
	}
	path += map[bool]string{true: "&", false: "?"}[strings.Contains(path, "?")] + "scroll=2m"
	var page searchReply
	if err := c.do(ctx, "POST", path, q, &page); err != nil {
		return err
	}
	var cols []driver.ResultColumn
	idx := map[string]int{}
	// Named fields (as exports and migrations ask for) fix the columns;
	// otherwise they come from the documents.
	if names, ok := q["_source"].([]any); ok {
		cols = append(cols, col("_id", driver.KindString))
		idx["_id"] = 0
		for _, n := range names {
			if s, ok := n.(string); ok {
				idx[s] = len(cols)
				k := driver.KindOther
				if f, ok := fields[s]; ok {
					k = kindOf(f.typ)
				}
				cols = append(cols, col(s, k))
			}
		}
	}
	fixed := len(cols) > 0
	sum := driver.ResultSummary{}
	scrollID := page.ScrollID
	defer func() {
		if scrollID != "" {
			c.raw(context.WithoutCancel(ctx), "DELETE", "/_search/scroll", map[string]any{"scroll_id": scrollID})
		}
	}()
	full := driver.WantsFullValues(sink)
	for {
		rows := hitRows(page.Hits.Hits, &cols, idx, fixed, fields)
		if sum.RowCount == 0 {
			// Columns come from the first page; later fields join at the end.
			if err := sink.Columns(cols); err != nil {
				return err
			}
		}
		for _, r := range rows {
			for len(r) < len(cols) {
				r = append(r, nil)
			}
			if !full {
				driver.PreviewRow(r)
			}
		}
		if len(rows) > 0 {
			if err := sink.Rows(rows); err != nil {
				return err
			}
		}
		sum.RowCount += int64(len(rows))
		if len(page.Hits.Hits) == 0 || page.ScrollID == "" {
			break
		}
		next := searchReply{}
		if err := c.do(ctx, "POST", "/_search/scroll", map[string]any{"scroll": "2m", "scroll_id": scrollID}, &next); err != nil {
			return err
		}
		page = next
		if page.ScrollID != "" {
			scrollID = page.ScrollID
		}
	}
	sum.DurationMS = float64(time.Since(start).Microseconds()) / 1000
	return sink.EndResult(sum)
}

// ---- replies as tables -------------------------------------------------------------

type resultSet struct {
	cols   []driver.ResultColumn
	rows   [][]any
	notice string
}

func col(name string, kind driver.ValueKind) driver.ResultColumn {
	return driver.ResultColumn{Name: name, Kind: kind, Type: string(kind)}
}

type searchReply struct {
	Took     int64  `json:"took"`
	ScrollID string `json:"_scroll_id"`
	Shards   struct {
		Failed int `json:"failed"`
	} `json:"_shards"`
	Hits struct {
		Total json.RawMessage `json:"total"`
		Hits  []hit           `json:"hits"`
	} `json:"hits"`
	Aggregations map[string]any `json:"aggregations"`
}

type hit struct {
	Index  string         `json:"_index"`
	ID     string         `json:"_id"`
	Score  *float64       `json:"_score"`
	Source map[string]any `json:"_source"`
	Fields map[string]any `json:"fields"`
}

// hitRows lays hits out as rows: _id, then each _source field in the order
// first seen (new fields add columns).
func hitRows(hits []hit, cols *[]driver.ResultColumn, idx map[string]int, fixed bool, fields map[string]field) [][]any {
	if len(*cols) == 0 {
		*cols = append(*cols, col("_id", driver.KindString))
		idx["_id"] = 0
	}
	var rows [][]any
	for _, h := range hits {
		names := make([]string, 0, len(h.Source))
		for k := range h.Source {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			if _, ok := idx[k]; !ok && !fixed {
				idx[k] = len(*cols)
				kind := kindOfValue(h.Source[k])
				if f, ok := fields[k]; ok {
					kind = kindOf(f.typ)
				}
				*cols = append(*cols, col(k, kind))
			}
		}
		row := make([]any, len(*cols))
		row[0] = h.ID
		for k, v := range h.Source {
			if i, ok := idx[k]; ok {
				row[i] = encode(v, fields[k])
			}
		}
		rows = append(rows, row)
	}
	return rows
}

func kindOfValue(v any) driver.ValueKind {
	switch x := v.(type) {
	case json.Number:
		if _, err := x.Int64(); err == nil {
			return driver.KindInt
		}
		return driver.KindFloat
	case bool:
		return driver.KindBool
	case map[string]any:
		return driver.KindObject
	case []any:
		return driver.KindArray
	}
	return driver.KindString
}

func totalHits(raw json.RawMessage) string {
	var t struct {
		Value    int64  `json:"value"`
		Relation string `json:"relation"`
	}
	if json.Unmarshal(raw, &t) == nil && t.Relation != "" {
		if t.Relation == "gte" {
			return "more than " + strconv.FormatInt(t.Value, 10)
		}
		return strconv.FormatInt(t.Value, 10)
	}
	var n int64
	if json.Unmarshal(raw, &n) == nil {
		return strconv.FormatInt(n, 10)
	}
	return ""
}

// render turns a reply into result sets: search hits and aggregations, the
// rows of _cat, SQL, PPL and ES|QL, a count, or the reply's fields.
func render(ep string, v any, fields map[string]field) []resultSet {
	m, isObj := v.(map[string]any)
	if arr, ok := v.([]any); ok {
		return []resultSet{objects(arr)}
	}
	if !isObj {
		return []resultSet{{cols: []driver.ResultColumn{col("response", driver.KindText)}, rows: [][]any{{encode(v, field{})}}}}
	}
	if _, ok := m["hits"].(map[string]any); ok && (isSearch(ep) || m["took"] != nil) {
		b, _ := json.Marshal(m)
		var sr searchReply
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.UseNumber()
		dec.Decode(&sr)
		var cols []driver.ResultColumn
		rs := resultSet{rows: hitRows(sr.Hits.Hits, &cols, map[string]int{}, false, fields)}
		rs.cols = cols
		for i := range rs.rows {
			for len(rs.rows[i]) < len(cols) {
				rs.rows[i] = append(rs.rows[i], nil)
			}
		}
		rs.notice = fmt.Sprintf("%d of %s hits in %d ms", len(sr.Hits.Hits), totalHits(sr.Hits.Total), sr.Took)
		if sr.Shards.Failed > 0 {
			rs.notice += fmt.Sprintf("; %d shards failed", sr.Shards.Failed)
		}
		out := []resultSet{rs}
		if len(sr.Hits.Hits) == 0 && len(sr.Aggregations) > 0 {
			out = nil // size 0: only the aggregations were asked for
		}
		names := make([]string, 0, len(sr.Aggregations))
		for k := range sr.Aggregations {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, name := range names {
			a := aggregation(name, sr.Aggregations[name])
			if len(out) == 0 {
				a.notice = rs.notice + "; aggregation " + name
			}
			out = append(out, a)
		}
		return out
	}
	// SQL (Elasticsearch: columns and rows; OpenSearch: schema and datarows) and ES|QL (columns and values).
	if cols, ok := m["columns"].([]any); ok {
		rows, _ := m["rows"].([]any)
		if rows == nil {
			rows, _ = m["values"].([]any)
		}
		return []resultSet{table(cols, rows)}
	}
	if cols, ok := m["schema"].([]any); ok {
		rows, _ := m["datarows"].([]any)
		return []resultSet{table(cols, rows)}
	}
	if n, ok := m["count"].(json.Number); ok && len(m) <= 2 {
		return []resultSet{{cols: []driver.ResultColumn{col("count", driver.KindInt)}, rows: [][]any{{encode(n, field{})}}}}
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	rs := resultSet{cols: []driver.ResultColumn{col("key", driver.KindString), col("value", driver.KindObject)}}
	for _, k := range keys {
		rs.rows = append(rs.rows, []any{k, encode(m[k], field{})})
	}
	return []resultSet{rs}
}

// objects lays out an array of objects (as _cat returns) as a table.
func objects(arr []any) resultSet {
	var rs resultSet
	idx := map[string]int{}
	for _, e := range arr {
		o, ok := e.(map[string]any)
		if !ok {
			rs.cols = []driver.ResultColumn{col("value", driver.KindObject)}
			rs.rows = nil
			for _, e := range arr {
				rs.rows = append(rs.rows, []any{encode(e, field{})})
			}
			return rs
		}
		keys := make([]string, 0, len(o))
		for k := range o {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if _, ok := idx[k]; !ok {
				idx[k] = len(rs.cols)
				rs.cols = append(rs.cols, col(k, driver.KindString))
			}
		}
		row := make([]any, len(rs.cols))
		for k, v := range o {
			row[idx[k]] = encode(v, field{})
		}
		rs.rows = append(rs.rows, row)
	}
	for i := range rs.rows {
		for len(rs.rows[i]) < len(rs.cols) {
			rs.rows[i] = append(rs.rows[i], nil)
		}
	}
	return rs
}

func table(cols, rows []any) resultSet {
	var rs resultSet
	for _, c := range cols {
		cm, _ := c.(map[string]any)
		name, _ := cm["name"].(string)
		typ, _ := cm["type"].(string)
		k := kindOf(typ)
		switch typ {
		case "integer", "long", "short", "byte":
			k = driver.KindInt
		case "datetime", "timestamp":
			k = driver.KindTimestamp
		}
		rs.cols = append(rs.cols, driver.ResultColumn{Name: name, Type: typ, Kind: k})
	}
	for _, r := range rows {
		vals, _ := r.([]any)
		row := make([]any, len(vals))
		for i, v := range vals {
			row[i] = encode(v, field{})
		}
		rs.rows = append(rs.rows, row)
	}
	return rs
}

// aggregation lays out one aggregation: buckets as rows (key, doc_count and
// the metrics under each), or a metric's values as one row.
func aggregation(name string, v any) resultSet {
	m, _ := v.(map[string]any)
	rs := resultSet{notice: "Aggregation " + name}
	var buckets []any
	switch b := m["buckets"].(type) {
	case []any:
		buckets = b
	case map[string]any: // keyed buckets
		keys := make([]string, 0, len(b))
		for k := range b {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if bm, ok := b[k].(map[string]any); ok {
				bm["key"] = k
				buckets = append(buckets, bm)
			}
		}
	}
	if buckets == nil {
		keys := make([]string, 0, len(m))
		for k := range m {
			if k != "meta" {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		row := []any{}
		for _, k := range keys {
			rs.cols = append(rs.cols, col(k, kindOfValue(m[k])))
			row = append(row, encode(m[k], field{}))
		}
		rs.rows = [][]any{row}
		return rs
	}
	idx := map[string]int{}
	add := func(k string, kind driver.ValueKind) int {
		if i, ok := idx[k]; ok {
			return i
		}
		idx[k] = len(rs.cols)
		rs.cols = append(rs.cols, col(k, kind))
		return idx[k]
	}
	add("key", driver.KindString)
	add("doc_count", driver.KindInt)
	for _, b := range buckets {
		bm, _ := b.(map[string]any)
		row := map[int]any{}
		row[0] = encode(bm["key"], field{})
		if s, ok := bm["key_as_string"]; ok {
			row[0] = s
		}
		row[1] = encode(bm["doc_count"], field{})
		keys := make([]string, 0, len(bm))
		for k := range bm {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if k == "key" || k == "key_as_string" || k == "doc_count" {
				continue
			}
			sub, ok := bm[k].(map[string]any)
			if !ok {
				continue
			}
			// A metric under the bucket: its value, or its whole result.
			if val, ok := sub["value"]; ok && len(sub) <= 2 {
				row[add(k, kindOfValue(val))] = encode(val, field{})
			} else {
				row[add(k, driver.KindObject)] = encode(sub, field{})
			}
		}
		out := make([]any, len(rs.cols))
		for i, v := range row {
			out[i] = v
		}
		rs.rows = append(rs.rows, out)
	}
	for i := range rs.rows {
		for len(rs.rows[i]) < len(rs.cols) {
			rs.rows[i] = append(rs.rows[i], nil)
		}
	}
	return rs
}

func emit(sink driver.Sink, rs resultSet, opts driver.ExecOptions, start time.Time) error {
	if rs.notice != "" {
		if err := sink.Notice("info", rs.notice); err != nil {
			return err
		}
	}
	if rs.cols == nil {
		rs.cols = []driver.ResultColumn{col("response", driver.KindText)}
	}
	return driver.EmitRows(sink, rs.cols, rs.rows, opts.MaxRows, start)
}

// ---- profiles, tasks and settings ------------------------------------------------------

// Explain runs a search with profiling on and shows each shard's query
// tree with the time of every part.
func (c *conn) Explain(ctx context.Context, _ driver.Scope, stmt string, _ bool) (*driver.Plan, error) {
	reqs, err := parseScript(stmt)
	if err != nil {
		return nil, err
	}
	if len(reqs) != 1 || !isSearch(reqs[0].endpoint()) {
		return nil, errors.New("explain profiles one search request, such as GET /orders/_search with its query")
	}
	q := map[string]any{}
	if reqs[0].body != nil {
		if err := json.Unmarshal(reqs[0].body, &q); err != nil {
			return nil, err
		}
	}
	q["profile"] = true
	var reply struct {
		Took    float64                         `json:"took"`
		Hits    struct{ Total json.RawMessage } `json:"hits"`
		Profile struct {
			Shards []struct {
				ID       string `json:"id"`
				Searches []struct {
					Query     []profNode `json:"query"`
					Collector []profNode `json:"collector"`
				} `json:"searches"`
				Aggregations []profNode `json:"aggregations"`
			} `json:"shards"`
		} `json:"profile"`
	}
	if err := c.do(ctx, "POST", "/"+strings.TrimPrefix(reqs[0].path, "/"), q, &reply); err != nil {
		return nil, mapError(err)
	}
	root := &driver.PlanNode{Operation: "Search", Detail: reqs[0].path}
	total := 0.0
	for _, sh := range reply.Profile.Shards {
		n := &driver.PlanNode{Operation: "Shard", Detail: sh.ID}
		ms := 0.0
		for _, s := range sh.Searches {
			for _, q := range s.Query {
				qn := q.node()
				ms += *qn.TimeMS
				n.Children = append(n.Children, qn)
			}
			for _, cl := range s.Collector {
				n.Children = append(n.Children, cl.node())
			}
		}
		for _, a := range sh.Aggregations {
			an := a.node()
			ms += *an.TimeMS
			n.Children = append(n.Children, an)
		}
		n.TimeMS = &ms
		total += ms
		root.Children = append(root.Children, n)
	}
	root.TimeMS = &total
	raw, _ := json.MarshalIndent(reply.Profile, "", "  ")
	return &driver.Plan{Root: root, Raw: string(raw), Format: "json",
		Totals: map[string]any{"Took (ms)": reply.Took, "Hits": totalHits(reply.Hits.Total), "Shards": len(reply.Profile.Shards)}}, nil
}

type profNode struct {
	Type        string     `json:"type"`
	Name        string     `json:"name"`
	Description string     `json:"description"`
	Reason      string     `json:"reason"`
	TimeNanos   float64    `json:"time_in_nanos"`
	Children    []profNode `json:"children"`
}

func (p profNode) node() *driver.PlanNode {
	op := p.Type
	if op == "" {
		op = p.Name
	}
	ms := p.TimeNanos / 1e6
	n := &driver.PlanNode{Operation: op, Detail: p.Description, TimeMS: &ms}
	if p.Reason != "" {
		n.Detail = strings.TrimSpace(n.Detail + " " + p.Reason)
	}
	for _, ch := range p.Children {
		n.Children = append(n.Children, ch.node())
	}
	return n
}

// Processes lists running tasks (searches, bulk writes, reindexing…).
func (c *conn) Processes(ctx context.Context) (*driver.Result, error) {
	var reply struct {
		Tasks []struct {
			Node        string `json:"node"`
			ID          int64  `json:"id"`
			Action      string `json:"action"`
			Description string `json:"description"`
			StartMillis int64  `json:"start_time_in_millis"`
			RunNanos    int64  `json:"running_time_in_nanos"`
			Cancellable bool   `json:"cancellable"`
			Parent      string `json:"parent_task_id"`
		} `json:"tasks"`
	}
	if err := c.do(ctx, "GET", "/_tasks?detailed=true&group_by=none", nil, &reply); err != nil {
		return nil, mapError(err)
	}
	res := &driver.Result{Columns: []driver.ResultColumn{col("id", driver.KindString), col("action", driver.KindString),
		col("seconds", driver.KindFloat), col("cancellable", driver.KindBool), col("description", driver.KindText), col("node", driver.KindString)},
		Rows: [][]any{}}
	for _, t := range reply.Tasks {
		if strings.HasPrefix(t.Action, "cluster:monitor/tasks/lists") || t.Parent != "" {
			continue
		}
		res.Rows = append(res.Rows, []any{fmt.Sprintf("%s:%d", t.Node, t.ID), t.Action, float64(t.RunNanos/1e6) / 1000, t.Cancellable, t.Description, t.Node})
	}
	return res, nil
}

func (c *conn) KillProcess(ctx context.Context, id string) error {
	if !strings.Contains(id, ":") {
		return fmt.Errorf("invalid task id %q", id)
	}
	var reply struct {
		NodeFailures []struct {
			Reason string `json:"reason"`
		} `json:"node_failures"`
		TaskFailures []struct {
			Reason struct {
				Reason string `json:"reason"`
			} `json:"reason"`
		} `json:"task_failures"`
	}
	if err := c.do(ctx, "POST", "/_tasks/"+esc(id)+"/_cancel", nil, &reply); err != nil {
		return mapError(err)
	}
	if len(reply.TaskFailures) > 0 {
		return errors.New(reply.TaskFailures[0].Reason.Reason)
	}
	if len(reply.NodeFailures) > 0 {
		return errors.New(reply.NodeFailures[0].Reason)
	}
	return nil
}

// Variables shows cluster settings ("variables", with defaults) or the
// nodes with their load ("status").
func (c *conn) Variables(ctx context.Context, kind string) (*driver.Result, error) {
	if kind == "status" {
		var nodes []any
		if err := c.do(ctx, "GET", "/_cat/nodes?format=json&h=name,ip,node.role,master,heap.percent,ram.percent,cpu,load_1m,disk.used_percent,version", nil, &nodes); err != nil {
			return nil, mapError(err)
		}
		rs := objects(nodes)
		return &driver.Result{Columns: rs.cols, Rows: rs.rows}, nil
	}
	var reply map[string]map[string]any
	if err := c.do(ctx, "GET", "/_cluster/settings?flat_settings=true&include_defaults=true", nil, &reply); err != nil {
		return nil, mapError(err)
	}
	res := &driver.Result{Columns: []driver.ResultColumn{col("name", driver.KindString), col("value", driver.KindText), col("set_in", driver.KindString)}, Rows: [][]any{}}
	seen := map[string]bool{}
	for _, scope := range []string{"transient", "persistent", "defaults"} {
		names := make([]string, 0, len(reply[scope]))
		for k := range reply[scope] {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			if seen[k] {
				continue
			}
			seen[k] = true
			v := reply[scope][k]
			text, ok := v.(string)
			if !ok {
				b, _ := json.Marshal(v)
				text = string(b)
			}
			res.Rows = append(res.Rows, []any{k, text, scope})
		}
	}
	sort.SliceStable(res.Rows, func(i, j int) bool { return res.Rows[i][0].(string) < res.Rows[j][0].(string) })
	return res, nil
}
