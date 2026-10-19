package redis

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	goredis "github.com/redis/go-redis/v9"

	"rowsmith/internal/driver"
)

// elementCap bounds the elements read into memory to filter or sort a key
// whose server-side order does not serve the request.
const elementCap = 200_000

// browseRows reads the rows a request asks for. Rows hold raw values:
// strings (a key's bytes), int64 list indexes, float64 scores and Docs.
func (c *conn) browseRows(ctx context.Context, req driver.BrowseRequest) (t string, cols []driver.Column, rows [][]any, total int64, exact bool, err error) {
	cl, err := c.clientFor(req.Ref.Database)
	if err != nil {
		return
	}
	key := req.Ref.Name
	if t, err = keyType(ctx, cl, key); err != nil {
		return
	}
	cols = columnsFor(t)
	if len(cols) == 0 {
		err = fmt.Errorf("Rowsmith cannot show %s keys yet; read this one from the console", t)
		return
	}
	filtered := req.Search != "" || len(req.Filters) > 0 || strings.TrimSpace(req.Where) != ""
	if strings.TrimSpace(req.Where) != "" {
		err = errors.New("Redis keys have no WHERE conditions; use the filters or quick search")
		return
	}
	limit := int64(req.Limit)
	if limit <= 0 {
		limit = 100
	}
	sortCol, desc := "", false
	if len(req.Sort) > 0 {
		sortCol, desc = req.Sort[0].Column, req.Sort[0].Desc
	}
	ex := func(e error) error { return mapError(e) }

	// Pages the server can serve in order, without reading the whole key.
	if !filtered {
		switch t {
		case kindList:
			if sortCol == "" || sortCol == "index" {
				n, e := cl.LLen(ctx, key).Result()
				if e != nil {
					err = ex(e)
					return
				}
				rows, err = listPage(ctx, cl, key, n, req.Offset, limit, desc)
				return t, cols, rows, n, true, err
			}
		case kindZSet:
			if sortCol == "" || sortCol == "score" {
				n, e := cl.ZCard(ctx, key).Result()
				if e != nil {
					err = ex(e)
					return
				}
				zr := cl.ZRangeWithScores
				if desc {
					zr = cl.ZRevRangeWithScores
				}
				zs, e := zr(ctx, key, req.Offset, req.Offset+limit-1).Result()
				if e != nil {
					err = ex(e)
					return
				}
				for _, z := range zs {
					rows = append(rows, []any{fmt.Sprint(z.Member), z.Score})
				}
				return t, cols, rows, n, true, nil
			}
		case kindStream:
			if sortCol == "" || sortCol == "id" {
				n, e := cl.XLen(ctx, key).Result()
				if e != nil {
					err = ex(e)
					return
				}
				rows, err = streamPage(ctx, cl, key, req.Offset, limit, desc)
				return t, cols, rows, n, true, err
			}
		}
	}

	all, complete, err := readAll(ctx, cl, t, key)
	if err != nil {
		return
	}
	match, err := matcher(cols, req)
	if err != nil {
		return
	}
	kept := all[:0]
	for _, r := range all {
		if match(r) {
			kept = append(kept, r)
		}
	}
	if sortCol == "" {
		sortCol = cols[0].Name
	}
	sortRows(kept, cols, sortCol, desc)
	total = int64(len(kept))
	lo := min(req.Offset, total)
	hi := min(lo+limit, total)
	return t, cols, kept[lo:hi], total, complete, nil
}

func listPage(ctx context.Context, cl goredis.UniversalClient, key string, n, offset, limit int64, desc bool) ([][]any, error) {
	var rows [][]any
	if offset >= n {
		return rows, nil
	}
	start, stop := offset, min(offset+limit, n)-1
	if desc {
		start, stop = n-min(offset+limit, n), n-1-offset
	}
	vals, err := cl.LRange(ctx, key, start, stop).Result()
	if err != nil {
		return nil, mapError(err)
	}
	for i, v := range vals {
		rows = append(rows, []any{start + int64(i), v})
	}
	if desc {
		for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
			rows[i], rows[j] = rows[j], rows[i]
		}
	}
	return rows, nil
}

// streamPage skips offset entries in chunks, then reads one page.
func streamPage(ctx context.Context, cl goredis.UniversalClient, key string, offset, limit int64, desc bool) ([][]any, error) {
	from, to := "-", "+"
	read := func(n int64) ([][]any, error) {
		if desc {
			return xrange(ctx, cl, "XREVRANGE", key, to, from, n)
		}
		return xrange(ctx, cl, "XRANGE", key, from, to, n)
	}
	// Continue after the last entry read: an exclusive range bound.
	advance := func(id string) {
		if desc {
			to = "(" + id
		} else {
			from = "(" + id
		}
	}
	for offset > 0 {
		rows, err := read(min(offset, 10_000))
		if err != nil || len(rows) == 0 {
			return nil, err
		}
		advance(rows[len(rows)-1][0].(string))
		offset -= int64(len(rows))
	}
	return read(limit)
}

type doer interface {
	Do(ctx context.Context, args ...any) *goredis.Cmd
}

// xrange runs XRANGE or XREVRANGE and returns [id, fields] rows, with each
// entry's fields in the order they were added.
func xrange(ctx context.Context, cl doer, cmd, key, start, end string, n int64) ([][]any, error) {
	reply, err := cl.Do(ctx, cmd, key, start, end, "COUNT", n).Slice()
	if err != nil {
		return nil, mapError(err)
	}
	rows := make([][]any, 0, len(reply))
	for _, e := range reply {
		if row, ok := streamEntry(e); ok {
			rows = append(rows, row)
		}
	}
	return rows, nil
}

// streamEntry reads one [id, [field, value, …]] reply.
func streamEntry(e any) ([]any, bool) {
	pair, ok := e.([]any)
	if !ok || len(pair) != 2 {
		return nil, false
	}
	id, ok := pair[0].(string)
	if !ok {
		return nil, false
	}
	d := driver.Doc{Values: map[string]any{}}
	kv, _ := pair[1].([]any)
	for i := 0; i+1 < len(kv); i += 2 {
		k := fmt.Sprint(kv[i])
		if _, dup := d.Values[k]; !dup {
			d.Keys = append(d.Keys, k)
		}
		d.Values[k] = fmt.Sprint(kv[i+1])
	}
	return []any{id, d}, true
}

// readAll reads every element of a key, up to elementCap; complete is false
// when the cap cut it short.
func readAll(ctx context.Context, cl goredis.UniversalClient, t, key string) (rows [][]any, complete bool, err error) {
	complete = true
	switch t {
	case kindString:
		v, err := cl.Get(ctx, key).Result()
		if err != nil {
			return nil, false, mapError(err)
		}
		return [][]any{{v}}, true, nil
	case kindJSON:
		v, err := cl.Do(ctx, "JSON.GET", key).Text()
		if err != nil {
			return nil, false, mapError(err)
		}
		return [][]any{{json.RawMessage(v)}}, true, nil
	case kindHash:
		var cursor uint64
		for {
			kv, next, err := cl.HScan(ctx, key, cursor, "*", 1000).Result()
			if err != nil {
				return nil, false, mapError(err)
			}
			for i := 0; i+1 < len(kv); i += 2 {
				rows = append(rows, []any{kv[i], kv[i+1]})
			}
			cursor = next
			if cursor == 0 {
				break
			}
			if len(rows) >= elementCap {
				complete = false
				break
			}
		}
		return dedupe(rows), complete, nil
	case kindSet:
		var cursor uint64
		for {
			ms, next, err := cl.SScan(ctx, key, cursor, "*", 1000).Result()
			if err != nil {
				return nil, false, mapError(err)
			}
			for _, m := range ms {
				rows = append(rows, []any{m})
			}
			cursor = next
			if cursor == 0 {
				break
			}
			if len(rows) >= elementCap {
				complete = false
				break
			}
		}
		return dedupe(rows), complete, nil
	case kindList:
		n, err := cl.LLen(ctx, key).Result()
		if err != nil {
			return nil, false, mapError(err)
		}
		if n > elementCap {
			n, complete = elementCap, false
		}
		for start := int64(0); start < n; start += 10_000 {
			vals, err := cl.LRange(ctx, key, start, min(start+10_000, n)-1).Result()
			if err != nil {
				return nil, false, mapError(err)
			}
			for i, v := range vals {
				rows = append(rows, []any{start + int64(i), v})
			}
		}
		return rows, complete, nil
	case kindZSet:
		n, err := cl.ZCard(ctx, key).Result()
		if err != nil {
			return nil, false, mapError(err)
		}
		if n > elementCap {
			n, complete = elementCap, false
		}
		for start := int64(0); start < n; start += 10_000 {
			zs, err := cl.ZRangeWithScores(ctx, key, start, min(start+10_000, n)-1).Result()
			if err != nil {
				return nil, false, mapError(err)
			}
			for _, z := range zs {
				rows = append(rows, []any{fmt.Sprint(z.Member), z.Score})
			}
		}
		return rows, complete, nil
	case kindStream:
		from := "-"
		for {
			page, err := xrange(ctx, cl, "XRANGE", key, from, "+", 10_000)
			if err != nil {
				return nil, false, err
			}
			rows = append(rows, page...)
			if len(page) < 10_000 {
				break
			}
			if len(rows) >= elementCap {
				complete = false
				break
			}
			from = "(" + page[len(page)-1][0].(string)
		}
		return rows, complete, nil
	}
	return nil, false, fmt.Errorf("Rowsmith cannot read %s keys yet", t)
}

// dedupe drops repeats that HSCAN and SSCAN may return.
func dedupe(rows [][]any) [][]any {
	seen := make(map[string]bool, len(rows))
	out := rows[:0]
	for _, r := range rows {
		k := r[0].(string)
		if !seen[k] {
			seen[k] = true
			out = append(out, r)
		}
	}
	return out
}

// text is a raw value as the filters compare it.
func text(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case json.RawMessage:
		return string(x)
	case driver.Doc:
		b, _ := json.Marshal(x)
		return string(b)
	}
	return fmt.Sprint(v)
}

func number(v any) (float64, bool) {
	switch x := v.(type) {
	case int64:
		return float64(x), true
	case float64:
		return x, true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		return f, err == nil
	}
	return 0, false
}

func compare(a, b any) int {
	if x, ok := number(a); ok {
		if y, ok := number(b); ok {
			switch {
			case x < y:
				return -1
			case x > y:
				return 1
			}
			return 0
		}
	}
	return strings.Compare(text(a), text(b))
}

func sortRows(rows [][]any, cols []driver.Column, by string, desc bool) {
	i := colIndex(cols, by)
	if i < 0 {
		return
	}
	// Text columns sort as numbers when every value is one, as counters are.
	numeric := cols[i].Kind == driver.KindInt || cols[i].Kind == driver.KindFloat
	if !numeric && cols[i].Kind == driver.KindText && len(rows) > 0 {
		numeric = true
		for _, r := range rows {
			if _, ok := number(r[i]); !ok {
				numeric = false
				break
			}
		}
	}
	sort.SliceStable(rows, func(a, b int) bool {
		var c int
		if numeric {
			c = compare(rows[a][i], rows[b][i])
		} else {
			c = strings.Compare(text(rows[a][i]), text(rows[b][i]))
		}
		if desc {
			return c > 0
		}
		return c < 0
	})
}

func colIndex(cols []driver.Column, name string) int {
	for i, c := range cols {
		if c.Name == name {
			return i
		}
	}
	return -1
}

// matcher builds the test for quick search and column filters.
func matcher(cols []driver.Column, req driver.BrowseRequest) (func([]any) bool, error) {
	type test func(v any) bool
	var tests []func([]any) bool
	if s := strings.ToLower(strings.TrimSpace(req.Search)); s != "" {
		tests = append(tests, func(r []any) bool {
			for _, v := range r {
				if strings.Contains(strings.ToLower(text(v)), s) {
					return true
				}
			}
			return false
		})
	}
	for _, f := range req.Filters {
		i := colIndex(cols, f.Column)
		if i < 0 {
			return nil, fmt.Errorf("unknown column %q", f.Column)
		}
		want := fmt.Sprint(f.Value)
		if f.Value == nil {
			want = ""
		}
		var t test
		switch f.Op {
		case "=":
			t = func(v any) bool { return compare(v, want) == 0 }
		case "!=":
			t = func(v any) bool { return compare(v, want) != 0 }
		case "<":
			t = func(v any) bool { return compare(v, want) < 0 }
		case "<=":
			t = func(v any) bool { return compare(v, want) <= 0 }
		case ">":
			t = func(v any) bool { return compare(v, want) > 0 }
		case ">=":
			t = func(v any) bool { return compare(v, want) >= 0 }
		case "contains":
			t = func(v any) bool { return strings.Contains(strings.ToLower(text(v)), strings.ToLower(want)) }
		case "startswith":
			t = func(v any) bool { return strings.HasPrefix(strings.ToLower(text(v)), strings.ToLower(want)) }
		case "endswith":
			t = func(v any) bool { return strings.HasSuffix(strings.ToLower(text(v)), strings.ToLower(want)) }
		case "between":
			if len(f.Values) != 2 {
				return nil, errors.New("between needs two values")
			}
			lo, hi := fmt.Sprint(f.Values[0]), fmt.Sprint(f.Values[1])
			t = func(v any) bool { return compare(v, lo) >= 0 && compare(v, hi) <= 0 }
		case "in", "notin":
			set := map[string]bool{}
			for _, x := range f.Values {
				set[fmt.Sprint(x)] = true
			}
			neg := f.Op == "notin"
			t = func(v any) bool { return set[text(v)] != neg }
		case "like", "notlike":
			re, err := likeRegexp(want)
			if err != nil {
				return nil, err
			}
			neg := f.Op == "notlike"
			t = func(v any) bool { return re.MatchString(text(v)) != neg }
		case "regexp":
			re, err := regexp.Compile(want)
			if err != nil {
				return nil, fmt.Errorf("regular expression: %w", err)
			}
			t = func(v any) bool { return re.MatchString(text(v)) }
		case "empty":
			t = func(v any) bool { return text(v) == "" }
		case "null":
			t = func(any) bool { return false } // Redis values are never NULL
		case "notnull":
			t = func(any) bool { return true }
		default:
			return nil, fmt.Errorf("filter %q is not available for Redis", f.Op)
		}
		tests = append(tests, func(r []any) bool { return t(r[i]) })
	}
	return func(r []any) bool {
		for _, t := range tests {
			if !t(r) {
				return false
			}
		}
		return true
	}, nil
}

// likeRegexp turns a SQL LIKE pattern (% and _) into a case-insensitive regexp.
func likeRegexp(p string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString("(?is)^")
	for _, r := range p {
		switch r {
		case '%':
			b.WriteString(".*")
		case '_':
			b.WriteString(".")
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}

// encodeValue renders a raw value as a grid cell.
func encodeValue(v any) any {
	switch x := v.(type) {
	case string:
		if utf8.ValidString(x) {
			return driver.EncodeText(x)
		}
		return driver.EncodeBinary([]byte(x))
	case int64:
		return driver.EncodeInt(x)
	case float64:
		return driver.EncodeFloat(x)
	case driver.Doc:
		out := driver.Doc{Keys: x.Keys, Values: make(map[string]any, len(x.Values))}
		for k, e := range x.Values {
			out.Values[k] = encodeValue(e)
		}
		return out
	}
	return v
}

func resultColumns(cols []driver.Column) []driver.ResultColumn {
	out := make([]driver.ResultColumn, len(cols))
	for i, c := range cols {
		out[i] = driver.ResultColumn{Name: c.Name, Type: c.Type, Kind: c.Kind}
	}
	return out
}

func (c *conn) Browse(ctx context.Context, req driver.BrowseRequest) (*driver.Result, error) {
	start := time.Now()
	_, cols, rows, total, _, err := c.browseRows(ctx, req)
	if err != nil {
		return nil, err
	}
	res := &driver.Result{Columns: resultColumns(cols), Rows: make([][]any, 0, len(rows))}
	size := 0
	for _, r := range rows {
		out := make([]any, len(r))
		for i, v := range r {
			out[i] = encodeValue(v)
		}
		driver.PreviewRow(out)
		size += driver.RowSize(out)
		if size > driver.BrowseBudget && len(res.Rows) > 0 {
			res.Clipped, res.Truncated = true, true
			break
		}
		res.Rows = append(res.Rows, out)
	}
	if req.Offset+int64(len(res.Rows)) < total {
		res.Truncated = true
	}
	res.Duration = time.Since(start)
	res.DurationMS = float64(res.Duration.Microseconds()) / 1000
	return res, nil
}

func (c *conn) Count(ctx context.Context, req driver.BrowseRequest) (driver.Count, error) {
	req.Offset, req.Limit = 0, 1
	_, _, _, total, exact, err := c.browseRows(ctx, req)
	if err != nil {
		return driver.Count{}, err
	}
	return driver.Count{Rows: total, Exact: exact}, nil
}

// BrowseQuery implements driver.BrowseQuerier: the console command that
// reads a whole key, for exports. Its result has the grid's columns.
func (c *conn) BrowseQuery(_ context.Context, t *driver.Table, req driver.BrowseRequest) (string, []any, error) {
	if req.Search != "" || len(req.Filters) > 0 {
		return "", nil, errors.New("Redis keys export whole; clear the filters and search first")
	}
	k := quoteArg(t.Ref.Name)
	switch t.Kind {
	case kindString:
		return "GET " + k, nil, nil
	case kindHash:
		return "HGETALL " + k, nil, nil
	case kindList:
		return "LRANGE " + k + " 0 -1", nil, nil
	case kindSet:
		return "SMEMBERS " + k, nil, nil
	case kindZSet:
		return "ZRANGE " + k + " 0 -1 WITHSCORES", nil, nil
	case kindStream:
		return "XRANGE " + k + " - +", nil, nil
	case kindJSON:
		return "JSON.GET " + k, nil, nil
	}
	return "", nil, fmt.Errorf("Rowsmith cannot export %s keys yet", t.Kind)
}

// ---- edits -------------------------------------------------------------------

var errNoNull = errors.New("Redis values cannot be NULL; leave the value empty instead")

// cellValue turns an edited cell (grid encoding) into the bytes to store.
func cellValue(v any) (string, error) {
	switch x := v.(type) {
	case nil:
		return "", errNoNull
	case string:
		return x, nil
	case bool:
		return strconv.FormatBool(x), nil
	case float64:
		if x == math.Trunc(x) && math.Abs(x) < 1e15 {
			return strconv.FormatInt(int64(x), 10), nil
		}
		return strconv.FormatFloat(x, 'f', -1, 64), nil
	case json.Number:
		return x.String(), nil
	case map[string]any:
		if x["$null"] == true {
			return "", errNoNull
		}
		if x["$default"] == true {
			return "", nil
		}
		if b, ok := x["$bin"].(string); ok {
			if _, partial := x["size"]; partial {
				if n, _ := x["size"].(float64); int(n) > base64.StdEncoding.DecodedLen(len(b)) {
					return "", errors.New("only a preview of this binary value was loaded; it cannot be saved back")
				}
			}
			raw, err := base64.StdEncoding.DecodeString(b)
			if err != nil {
				return "", fmt.Errorf("binary value: %w", err)
			}
			return string(raw), nil
		}
		if _, ok := x["$text"]; ok {
			return "", errors.New("only a preview of this long value was loaded; it cannot be saved back")
		}
		if _, ok := x["$expr"]; ok {
			return "", errors.New("Redis has no SQL expressions; enter the value itself")
		}
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func cellFloat(v any) (float64, error) {
	s, err := cellValue(v)
	if err != nil {
		return 0, err
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || math.IsNaN(f) {
		return 0, fmt.Errorf("a score must be a number, not %q", s)
	}
	return f, nil
}

func cellIndex(v any) (int64, error) {
	s, err := cellValue(v)
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("a list index must be a whole number, not %q", s)
	}
	return n, nil
}

// cellFields reads a stream entry's fields: an object, or JSON text of one.
func cellFields(v any) ([]string, error) {
	if s, ok := v.(string); ok {
		var m any
		if err := json.Unmarshal([]byte(s), &m); err != nil {
			return nil, errors.New(`stream fields are an object, such as {"status": "paid"}`)
		}
		v = m
	}
	m, ok := v.(map[string]any)
	if !ok || len(m) == 0 {
		return nil, errors.New(`stream fields are an object with at least one field, such as {"status": "paid"}`)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []string
	for _, k := range keys {
		s, err := cellValue(m[k])
		if err != nil {
			return nil, fmt.Errorf("field %s: %w", k, err)
		}
		out = append(out, k, s)
	}
	return out, nil
}

// cellJSON reads a JSON document cell: JSON text as typed, or a value to encode.
func cellJSON(v any) (string, error) {
	if s, ok := v.(string); ok {
		if json.Valid([]byte(s)) {
			return s, nil
		}
	}
	if v == nil {
		return "null", nil
	}
	b, err := json.Marshal(v)
	return string(b), err
}

// pending is one command of a save, kept as arguments and as console text.
type pending struct{ args []any }

func (p pending) String() string {
	parts := make([]string, len(p.args))
	for i, a := range p.args {
		parts[i] = quoteArg(fmt.Sprint(a))
	}
	return strings.Join(parts, " ")
}

func (c *conn) ApplyEdits(ctx context.Context, ref driver.ObjectRef, edits []driver.RowEdit) (*driver.EditResult, error) {
	if c.ro {
		return nil, driver.ErrReadOnly
	}
	cl, err := c.clientFor(ref.Database)
	if err != nil {
		return nil, err
	}
	key := ref.Name
	var cmds []pending
	err = cl.Watch(ctx, func(tx *goredis.Tx) error {
		t, err := keyType(ctx, cl, key)
		if err != nil {
			return err
		}
		cmds, err = planEdits(ctx, tx, t, key, edits)
		if err != nil {
			return err
		}
		if len(cmds) == 0 {
			return nil
		}
		_, err = tx.TxPipelined(ctx, func(p goredis.Pipeliner) error {
			for _, cmd := range cmds {
				p.Do(ctx, cmd.args...)
			}
			return nil
		})
		return err
	}, key)
	if errors.Is(err, goredis.TxFailedErr) {
		return nil, errors.New("the key changed while saving, so nothing was saved; reload it and try again")
	}
	if err != nil {
		return nil, mapError(err)
	}
	res := &driver.EditResult{Applied: len(edits), Statements: make([]string, len(cmds))}
	for i, cmd := range cmds {
		res.Statements[i] = cmd.String()
	}
	return res, nil
}

// planEdits checks every edit against the key as it is now and returns the
// commands that apply them. Each change must find exactly what it expects
// (a field, member, index or entry), or nothing is saved.
func planEdits(ctx context.Context, tx *goredis.Tx, t, key string, edits []driver.RowEdit) ([]pending, error) {
	var out []pending
	add := func(args ...any) { out = append(out, pending{args}) }
	switch t {
	case kindString, kindJSON:
		for _, e := range edits {
			if e.Op != "update" {
				return nil, fmt.Errorf("a %s key holds one value: edit it, or delete the key from the navigator", typeLabel(t))
			}
			v, ok := e.Values["value"]
			if !ok {
				continue
			}
			if t == kindJSON {
				s, err := cellJSON(v)
				if err != nil {
					return nil, err
				}
				add("JSON.SET", key, "$", s)
				continue
			}
			s, err := cellValue(v)
			if err != nil {
				return nil, err
			}
			add("SET", key, s, "KEEPTTL")
		}

	case kindHash:
		for _, e := range edits {
			switch e.Op {
			case "insert":
				if e.Values["field"] == nil {
					return nil, errors.New("enter a field name for the new row")
				}
				f, err := cellValue(e.Values["field"])
				if err != nil {
					return nil, fmt.Errorf("field: %w", err)
				}
				v, err := cellValue(e.Values["value"])
				if err != nil && e.Values["value"] != nil {
					return nil, err
				}
				if ok, err := tx.HExists(ctx, key, f).Result(); err != nil {
					return nil, err
				} else if ok {
					return nil, fmt.Errorf("field %q already exists", f)
				}
				add("HSET", key, f, v)
			case "update", "delete":
				f, err := cellValue(e.Key["field"])
				if err != nil {
					return nil, err
				}
				if ok, err := tx.HExists(ctx, key, f).Result(); err != nil {
					return nil, err
				} else if !ok {
					return nil, fmt.Errorf("field %q no longer exists", f)
				}
				if e.Op == "delete" {
					add("HDEL", key, f)
					continue
				}
				v, hasV := e.Values["value"]
				if nf, renamed := e.Values["field"]; renamed {
					n, err := cellValue(nf)
					if err != nil {
						return nil, err
					}
					if n != f {
						if ok, err := tx.HExists(ctx, key, n).Result(); err != nil {
							return nil, err
						} else if ok {
							return nil, fmt.Errorf("field %q already exists", n)
						}
						if !hasV {
							old, err := tx.HGet(ctx, key, f).Result()
							if err != nil {
								return nil, err
							}
							v, hasV = old, true
						}
						add("HDEL", key, f)
						f = n
					}
				}
				if hasV {
					s, err := cellValue(v)
					if err != nil {
						return nil, err
					}
					add("HSET", key, f, s)
				}
			}
		}

	case kindList:
		n, err := tx.LLen(ctx, key).Result()
		if err != nil {
			return nil, err
		}
		var pushes []any
		deleted := 0
		marker := fmt.Sprintf("\x00rowsmith-deleted-%d", time.Now().UnixNano())
		for _, e := range edits {
			if e.Op == "insert" {
				v, err := cellValue(e.Values["value"])
				if err != nil && e.Values["value"] != nil {
					return nil, err
				}
				pushes = append(pushes, v)
				continue
			}
			i, err := cellIndex(e.Key["index"])
			if err != nil {
				return nil, err
			}
			if i < 0 || i >= n {
				return nil, fmt.Errorf("item %d no longer exists (the list has %d items)", i, n)
			}
			if e.Op == "delete" {
				add("LSET", key, i, marker)
				deleted++
				continue
			}
			if _, moved := e.Values["index"]; moved {
				return nil, errors.New("list positions cannot be edited; delete the item and add it again")
			}
			if v, ok := e.Values["value"]; ok {
				s, err := cellValue(v)
				if err != nil {
					return nil, err
				}
				add("LSET", key, i, s)
			}
		}
		if deleted > 0 {
			add("LREM", key, deleted, marker)
		}
		if len(pushes) > 0 {
			add(append([]any{"RPUSH", key}, pushes...)...)
		}

	case kindSet:
		for _, e := range edits {
			switch e.Op {
			case "insert":
				if e.Values["member"] == nil {
					return nil, errors.New("enter a member for the new row")
				}
				m, err := cellValue(e.Values["member"])
				if err != nil {
					return nil, err
				}
				if ok, err := tx.SIsMember(ctx, key, m).Result(); err != nil {
					return nil, err
				} else if ok {
					return nil, fmt.Errorf("%q is already a member", m)
				}
				add("SADD", key, m)
			case "update", "delete":
				m, err := cellValue(e.Key["member"])
				if err != nil {
					return nil, err
				}
				if ok, err := tx.SIsMember(ctx, key, m).Result(); err != nil {
					return nil, err
				} else if !ok {
					return nil, fmt.Errorf("%q is no longer a member", m)
				}
				if e.Op == "delete" {
					add("SREM", key, m)
					continue
				}
				nv, ok := e.Values["member"]
				if !ok {
					continue
				}
				n, err := cellValue(nv)
				if err != nil {
					return nil, err
				}
				if n == m {
					continue
				}
				if ok, err := tx.SIsMember(ctx, key, n).Result(); err != nil {
					return nil, err
				} else if ok {
					return nil, fmt.Errorf("%q is already a member", n)
				}
				add("SREM", key, m)
				add("SADD", key, n)
			}
		}

	case kindZSet:
		for _, e := range edits {
			switch e.Op {
			case "insert":
				if e.Values["member"] == nil {
					return nil, errors.New("enter a member for the new row")
				}
				m, err := cellValue(e.Values["member"])
				if err != nil {
					return nil, err
				}
				score := 0.0
				if v, ok := e.Values["score"]; ok && v != nil {
					if score, err = cellFloat(v); err != nil {
						return nil, err
					}
				}
				if err := tx.ZScore(ctx, key, m).Err(); err == nil {
					return nil, fmt.Errorf("%q is already a member", m)
				} else if !errors.Is(err, goredis.Nil) {
					return nil, err
				}
				add("ZADD", key, "NX", formatScore(score), m)
			case "update", "delete":
				m, err := cellValue(e.Key["member"])
				if err != nil {
					return nil, err
				}
				old, err := tx.ZScore(ctx, key, m).Result()
				if errors.Is(err, goredis.Nil) {
					return nil, fmt.Errorf("%q is no longer a member", m)
				} else if err != nil {
					return nil, err
				}
				if e.Op == "delete" {
					add("ZREM", key, m)
					continue
				}
				score := old
				if v, ok := e.Values["score"]; ok {
					if score, err = cellFloat(v); err != nil {
						return nil, err
					}
				}
				n := m
				if nv, ok := e.Values["member"]; ok {
					if n, err = cellValue(nv); err != nil {
						return nil, err
					}
				}
				if n != m {
					if err := tx.ZScore(ctx, key, n).Err(); err == nil {
						return nil, fmt.Errorf("%q is already a member", n)
					} else if !errors.Is(err, goredis.Nil) {
						return nil, err
					}
					add("ZREM", key, m)
					add("ZADD", key, formatScore(score), n)
				} else if score != old {
					add("ZADD", key, "XX", formatScore(score), m)
				}
			}
		}

	case kindStream:
		for _, e := range edits {
			switch e.Op {
			case "insert":
				id := "*"
				if v, ok := e.Values["id"]; ok && v != nil {
					s, err := cellValue(v)
					if err != nil {
						return nil, err
					}
					if s = strings.TrimSpace(s); s != "" {
						id = s
					}
				}
				fields, err := cellFields(e.Values["fields"])
				if err != nil {
					return nil, err
				}
				args := []any{"XADD", key, id}
				for _, f := range fields {
					args = append(args, f)
				}
				add(args...)
			case "delete":
				id, err := cellValue(e.Key["id"])
				if err != nil {
					return nil, err
				}
				msgs, err := tx.XRangeN(ctx, key, id, id, 1).Result()
				if err != nil {
					return nil, err
				}
				if len(msgs) == 0 {
					return nil, fmt.Errorf("entry %s no longer exists", id)
				}
				add("XDEL", key, id)
			default:
				return nil, errors.New("stream entries cannot be changed once added; add a new entry instead")
			}
		}

	default:
		return nil, fmt.Errorf("Rowsmith cannot edit %s keys yet; use the console", t)
	}
	return out, nil
}

func formatScore(f float64) string {
	switch {
	case math.IsInf(f, 1):
		return "+inf"
	case math.IsInf(f, -1):
		return "-inf"
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}
