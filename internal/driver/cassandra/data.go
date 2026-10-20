package cassandra

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	gocql "github.com/apache/cassandra-gocql-driver/v2"

	"rowsmith/internal/driver"
)

var (
	_ driver.BrowseQuerier = (*conn)(nil)
	_ driver.BulkImporter  = (*conn)(nil)
)

// rowReader scans rows of any shape: a pointer to a pointer per value, so
// NULL reads as nil, and one value per element of a tuple, as gocql wants.
type rowReader struct {
	cols  []gocql.ColumnInfo
	dest  []any
	width []int // values per column (a tuple's elements)
}

func newRowReader(cols []gocql.ColumnInfo) *rowReader {
	r := &rowReader{cols: cols, width: make([]int, len(cols))}
	for i, c := range cols {
		if tt, ok := c.TypeInfo.(gocql.TupleTypeInfo); ok {
			for _, e := range tt.Elems {
				r.dest = append(r.dest, reflect.New(reflect.PointerTo(reflect.TypeOf(e.Zero()))).Interface())
			}
			r.width[i] = len(tt.Elems)
			continue
		}
		r.dest = append(r.dest, reflect.New(reflect.PointerTo(reflect.TypeOf(c.TypeInfo.Zero()))).Interface())
		r.width[i] = 1
	}
	return r
}

func (r *rowReader) columns() []driver.ResultColumn {
	out := make([]driver.ResultColumn, len(r.cols))
	for i, c := range r.cols {
		t := typeName(c.TypeInfo)
		out[i] = driver.ResultColumn{Name: c.Name, Type: t, Kind: kindOfType(t), Table: c.Table}
	}
	return out
}

func value(p any) any {
	v := reflect.ValueOf(p).Elem()
	if v.IsNil() {
		return nil
	}
	return v.Elem().Interface()
}

// next reads a row into cells; false at the end or on an error.
func (r *rowReader) next(iter *gocql.Iter) ([]any, bool) {
	if !iter.Scan(r.dest...) {
		return nil, false
	}
	row := make([]any, len(r.cols))
	k := 0
	for i, c := range r.cols {
		if tt, ok := c.TypeInfo.(gocql.TupleTypeInfo); ok {
			elems := make([]any, r.width[i])
			allNil := true
			for j := range elems {
				v := value(r.dest[k+j])
				allNil = allNil && v == nil
				elems[j] = encodeTyped(tt.Elems[j], v)
			}
			if !allNil {
				row[i] = elems
			}
		} else {
			row[i] = encodeTyped(c.TypeInfo, value(r.dest[k]))
		}
		k += r.width[i]
	}
	return row, true
}

// ---- browsing -------------------------------------------------------------------

func colIndex(t *driver.Table) map[string]*driver.Column {
	m := map[string]*driver.Column{}
	for i := range t.Columns {
		m[t.Columns[i].Name] = &t.Columns[i]
	}
	return m
}

func partitionKey(t *driver.Table) []string {
	var out []string
	for _, p := range strings.Split(t.Options["partition_key"], ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// where writes a browse request's filters in CQL. Cassandra compares a
// column with a value of its own type, which fromJson(?) gives; it filters
// on the primary key cheaply and on other columns only when asked to
// (ALLOW FILTERING), by reading every partition.
func where(t *driver.Table, req driver.BrowseRequest) (conds []string, args []any, eqPartition bool, err error) {
	if strings.TrimSpace(req.Search) != "" {
		return nil, nil, false, errors.New("Cassandra can't search every column; filter on a column instead")
	}
	cols := colIndex(t)
	eq := map[string]bool{}
	add := func(c *driver.Column, op string, v any) error {
		expr, arg, err := inputExpr(c, v)
		if err != nil {
			return err
		}
		conds = append(conds, quote(c.Name)+" "+op+" "+expr)
		if arg != nil {
			args = append(args, arg)
		}
		return nil
	}
	for _, f := range req.Filters {
		c := cols[f.Column]
		if c == nil {
			return nil, nil, false, fmt.Errorf("column %s does not exist", f.Column)
		}
		switch f.Op {
		case "=", "<", "<=", ">", ">=":
			if err := add(c, f.Op, f.Value); err != nil {
				return nil, nil, false, err
			}
			if f.Op == "=" {
				eq[c.Name] = true
			}
		case "between":
			if len(f.Values) != 2 {
				return nil, nil, false, fmt.Errorf("%s: between needs two values", f.Column)
			}
			if err := add(c, ">=", f.Values[0]); err != nil {
				return nil, nil, false, err
			}
			if err := add(c, "<=", f.Values[1]); err != nil {
				return nil, nil, false, err
			}
		case "in":
			var exprs []string
			for _, v := range f.Values {
				expr, arg, err := inputExpr(c, v)
				if err != nil {
					return nil, nil, false, err
				}
				exprs = append(exprs, expr)
				if arg != nil {
					args = append(args, arg)
				}
			}
			if len(exprs) == 0 {
				return nil, nil, false, fmt.Errorf("%s: choose at least one value", f.Column)
			}
			conds = append(conds, quote(c.Name)+" IN ("+strings.Join(exprs, ", ")+")")
			eq[c.Name] = true
		case "contains":
			if c.Kind != driver.KindArray && c.Kind != driver.KindObject {
				return nil, nil, false, fmt.Errorf("%s: Cassandra can't search inside text; filter with = or a range instead", f.Column)
			}
			// CONTAINS takes an element of the collection.
			el := *c
			el.Kind = elementKind(c.Type)
			if err := add(&el, "CONTAINS", f.Value); err != nil {
				return nil, nil, false, err
			}
		default:
			return nil, nil, false, fmt.Errorf("%s: Cassandra filters with =, <, <=, >, >=, between, in and (for collections) contains", f.Column)
		}
	}
	if w := strings.TrimSpace(req.Where); w != "" {
		conds = append(conds, w)
	}
	pk := partitionKey(t)
	eqPartition = len(pk) > 0
	for _, p := range pk {
		eqPartition = eqPartition && eq[p]
	}
	return conds, args, eqPartition, nil
}

// elementKind is the kind of a collection's elements (a map's values).
func elementKind(typ string) driver.ValueKind {
	t := strings.TrimSpace(typ)
	for strings.HasPrefix(t, "frozen<") && strings.HasSuffix(t, ">") {
		t = t[7 : len(t)-1]
	}
	i, j := strings.IndexByte(t, '<'), strings.LastIndexByte(t, '>')
	if i < 0 || j < i {
		return driver.KindString
	}
	inner := t[i+1 : j]
	if strings.HasPrefix(t, "map<") {
		parts := splitTop(inner)
		if len(parts) == 2 {
			inner = parts[1]
		}
	} else if strings.HasPrefix(t, "vector<") {
		inner = splitTop(inner)[0]
	}
	return kindOfType(inner)
}

func splitTop(s string) []string {
	var out []string
	depth, start := 0, 0
	for i, r := range s {
		switch r {
		case '<', '(':
			depth++
		case '>', ')':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, strings.TrimSpace(s[start:i]))
				start = i + 1
			}
		}
	}
	return append(out, strings.TrimSpace(s[start:]))
}

// orderBy allows the orders Cassandra keeps: inside one partition, by the
// first clustering columns in their declared order, all in their declared
// direction or all reversed.
func orderBy(t *driver.Table, sorts []driver.Sort, eqPartition bool) (string, error) {
	if len(sorts) == 0 {
		return "", nil
	}
	type clusteringCol struct {
		name string
		desc bool
	}
	var declared []clusteringCol
	var names []string
	for _, c := range strings.Split(t.Options["clustering_order"], ",") {
		name, dir, _ := strings.Cut(strings.TrimSpace(c), " ")
		if name != "" {
			declared = append(declared, clusteringCol{name, strings.EqualFold(dir, "DESC")})
			names = append(names, name)
		}
	}
	inOrder := fmt.Errorf("sort by the clustering columns (%s), in that order", strings.Join(names, ", "))
	if !eqPartition || len(declared) == 0 {
		return "", errors.New("Cassandra keeps rows in partition order: sort by a clustering column after filtering the whole partition key with =")
	}
	if len(sorts) > len(declared) {
		return "", inOrder
	}
	reversed := sorts[0].Desc != declared[0].desc
	parts := make([]string, len(sorts))
	for i, s := range sorts {
		if s.Column != declared[i].name {
			return "", inOrder
		}
		if (s.Desc != declared[i].desc) != reversed {
			return "", errors.New("Cassandra sorts by the clustering columns all in their declared directions or all reversed")
		}
		parts[i] = quote(s.Column) + map[bool]string{true: " DESC", false: " ASC"}[s.Desc]
	}
	return " ORDER BY " + strings.Join(parts, ", "), nil
}

func selectList(t *driver.Table, names []string) (string, error) {
	if len(names) == 0 {
		return "*", nil
	}
	cols := colIndex(t)
	out := make([]string, len(names))
	for i, n := range names {
		if cols[n] == nil {
			return "", fmt.Errorf("column %s does not exist", n)
		}
		out[i] = quote(n)
	}
	return strings.Join(out, ", "), nil
}

func (c *conn) browseStatement(t *driver.Table, req driver.BrowseRequest, limit int64) (string, []any, error) {
	sel, err := selectList(t, req.Columns)
	if err != nil {
		return "", nil, err
	}
	conds, args, eqPartition, err := where(t, req)
	if err != nil {
		return "", nil, err
	}
	order, err := orderBy(t, req.Sort, eqPartition)
	if err != nil {
		return "", nil, err
	}
	q := "SELECT " + sel + " FROM " + qualify(t.Ref.Database, t.Ref.Name)
	if len(conds) > 0 {
		q += " WHERE " + strings.Join(conds, " AND ")
	}
	q += order
	if limit > 0 {
		q += " LIMIT " + strconv.FormatInt(limit, 10)
	}
	if len(conds) > 0 {
		q += " ALLOW FILTERING"
	}
	return q, args, nil
}

func (c *conn) Browse(ctx context.Context, req driver.BrowseRequest) (*driver.Result, error) {
	start := time.Now()
	t, err := c.describe(ctx, req.Ref, false)
	if err != nil {
		return nil, err
	}
	limit := req.Limit
	if limit <= 0 {
		limit = 100
	}
	// There is no OFFSET: read past the earlier rows, one more to tell
	// whether others follow.
	q, args, err := c.browseStatement(t, req, req.Offset+int64(limit)+1)
	if err != nil {
		return nil, err
	}
	iter := c.query(ctx, q, args...).PageSize(min(limit+1, 5000)).Iter()
	rd := newRowReader(iter.Columns())
	res := &driver.Result{Columns: rd.columns(), Rows: [][]any{}, SQL: inline(q, args)}
	var skipped int64
	size := 0
	for {
		row, ok := rd.next(iter)
		if !ok {
			break
		}
		if skipped < req.Offset {
			skipped++
			continue
		}
		if len(res.Rows) == limit {
			res.Truncated = true
			break
		}
		driver.PreviewRow(row)
		res.Rows = append(res.Rows, row)
		if size += driver.RowSize(row); size > driver.BrowseBudget {
			res.Truncated, res.Clipped = true, true
			break
		}
	}
	if err := iter.Close(); err != nil {
		return nil, mapError(err)
	}
	res.Duration = time.Since(start)
	res.DurationMS = float64(res.Duration.Microseconds()) / 1000
	return res, nil
}

// Count counts with count(*), which reads every row in range; a whole table
// gets a time limit so a large one fails clearly rather than hanging.
func (c *conn) Count(ctx context.Context, req driver.BrowseRequest) (driver.Count, error) {
	t, err := c.describe(ctx, req.Ref, false)
	if err != nil {
		return driver.Count{}, err
	}
	conds, args, _, err := where(t, req)
	if err != nil {
		return driver.Count{}, err
	}
	q := "SELECT count(*) FROM " + qualify(t.Ref.Database, t.Ref.Name)
	if len(conds) > 0 {
		q += " WHERE " + strings.Join(conds, " AND ") + " ALLOW FILTERING"
	}
	cctx := ctx
	if len(conds) == 0 {
		var cancel context.CancelFunc
		cctx, cancel = context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
	}
	var n int64
	if err := c.query(cctx, q, args...).Scan(&n); err != nil {
		if len(conds) == 0 && cctx.Err() != nil && ctx.Err() == nil {
			return driver.Count{}, errors.New("the table is too large to count in 10 seconds; filter by its partition key to count part of it")
		}
		return driver.Count{}, mapError(err)
	}
	return driver.Count{Rows: n, Exact: true}, nil
}

// ---- edits ------------------------------------------------------------------------

func keyCondition(t *driver.Table, key map[string]any) (string, []any, error) {
	cols := colIndex(t)
	var conds []string
	var args []any
	for _, name := range t.PrimaryKey {
		v, ok := key[name]
		if !ok || v == nil {
			return "", nil, fmt.Errorf("missing primary key column %s", name)
		}
		expr, arg, err := inputExpr(cols[name], v)
		if err != nil {
			return "", nil, err
		}
		conds = append(conds, quote(name)+" = "+expr)
		if arg != nil {
			args = append(args, arg)
		}
	}
	return strings.Join(conds, " AND "), args, nil
}

// ApplyEdits writes each change as a lightweight transaction on its primary
// key (INSERT … IF NOT EXISTS, UPDATE and DELETE … IF EXISTS), so an insert
// never overwrites a row and a change never brings back a deleted one.
// Every changed row is checked first; Cassandra has no transaction across
// rows, so changes before a failing one stay saved.
func (c *conn) ApplyEdits(ctx context.Context, ref driver.ObjectRef, edits []driver.RowEdit) (*driver.EditResult, error) {
	if c.ro {
		return nil, driver.ErrReadOnly
	}
	t, err := c.describe(ctx, ref, false)
	if err != nil {
		return nil, err
	}
	if !t.Editable {
		switch {
		case t.Kind == "materialized_view":
			return nil, errors.New("a materialized view is written by the server from its base table; edit the base table")
		case t.Options["counter"] == "true":
			return nil, errors.New("counter columns change only by increments; use UPDATE … SET c = c + 1 in the console")
		}
		return nil, driver.ErrReadOnly
	}
	cols := colIndex(t)
	target := qualify(t.Ref.Database, t.Ref.Name)
	pk := map[string]bool{}
	for _, p := range t.PrimaryKey {
		pk[p] = true
	}
	for i, ed := range edits {
		if ed.Op != "update" && ed.Op != "delete" {
			continue
		}
		cond, args, err := keyCondition(t, ed.Key)
		if err != nil {
			return nil, fmt.Errorf("change %d: %w", i+1, err)
		}
		var n int64
		if err := c.query(ctx, "SELECT count(*) FROM "+target+" WHERE "+cond, args...).Scan(&n); err != nil {
			return nil, fmt.Errorf("change %d: %w", i+1, mapError(err))
		}
		if n == 0 {
			return nil, fmt.Errorf("change %d: the row no longer exists or was modified by someone else", i+1)
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
	for i, ed := range edits {
		var q string
		var args []any
		switch ed.Op {
		case "insert":
			var names, exprs []string
			for _, col := range t.Columns {
				v, ok := ed.Values[col.Name]
				if !ok {
					if pk[col.Name] {
						return nil, fail(i, fmt.Errorf("the primary key column %s needs a value", col.Name))
					}
					continue
				}
				expr, arg, err := inputExpr(cols[col.Name], v)
				if err != nil {
					return nil, fail(i, err)
				}
				names = append(names, quote(col.Name))
				exprs = append(exprs, expr)
				if arg != nil {
					args = append(args, arg)
				}
			}
			q = "INSERT INTO " + target + " (" + strings.Join(names, ", ") + ") VALUES (" + strings.Join(exprs, ", ") + ") IF NOT EXISTS"
		case "update":
			var sets []string
			for _, col := range t.Columns {
				v, ok := ed.Values[col.Name]
				if !ok {
					continue
				}
				if pk[col.Name] {
					return nil, fail(i, fmt.Errorf("%s is part of the primary key, which can't change; insert a new row and delete this one", col.Name))
				}
				expr, arg, err := inputExpr(cols[col.Name], v)
				if err != nil {
					return nil, fail(i, err)
				}
				sets = append(sets, quote(col.Name)+" = "+expr)
				if arg != nil {
					args = append(args, arg)
				}
			}
			if len(sets) == 0 {
				continue
			}
			cond, kargs, err := keyCondition(t, ed.Key)
			if err != nil {
				return nil, fail(i, err)
			}
			args = append(args, kargs...)
			q = "UPDATE " + target + " SET " + strings.Join(sets, ", ") + " WHERE " + cond + " IF EXISTS"
		case "delete":
			cond, kargs, err := keyCondition(t, ed.Key)
			if err != nil {
				return nil, fail(i, err)
			}
			args = kargs
			q = "DELETE FROM " + target + " WHERE " + cond + " IF EXISTS"
		default:
			return nil, fail(i, fmt.Errorf("unknown edit operation %q", ed.Op))
		}
		applied, err := c.query(ctx, q, args...).MapScanCAS(map[string]any{})
		if err != nil {
			return nil, fail(i, mapError(err))
		}
		if !applied {
			if ed.Op == "insert" {
				return nil, fail(i, errors.New("a row with this primary key already exists"))
			}
			return nil, fail(i, errors.New("the row no longer exists or was modified by someone else"))
		}
		res.Statements = append(res.Statements, inline(q, args))
		res.Applied++
	}
	return res, nil
}

// ---- exports and imports ------------------------------------------------------------

// BrowseQuery is the whole (filtered) table as a console statement, which
// the console reads page by page, for exports and migrations.
func (c *conn) BrowseQuery(ctx context.Context, t *driver.Table, req driver.BrowseRequest) (string, []any, error) {
	req.Sort = nil
	return c.browseStatement(t, req, 0)
}

// BeginImport writes rows with concurrent INSERTs. Cassandra inserts are
// upserts: a row whose key exists is replaced. Nothing can be rolled back.
func (c *conn) BeginImport(ctx context.Context, t *driver.Table, cols []string, empty bool) (driver.RowImporter, error) {
	if c.ro {
		return nil, driver.ErrReadOnly
	}
	if t.Kind != "table" || t.Options["counter"] == "true" {
		return nil, fmt.Errorf("%s can't be imported into: only tables without counters take rows", t.Ref.Name)
	}
	byName := colIndex(t)
	im := &importer{c: c, target: qualify(t.Ref.Database, t.Ref.Name)}
	for _, n := range cols {
		col := byName[n]
		if col == nil {
			return nil, fmt.Errorf("column %s does not exist", n)
		}
		im.cols = append(im.cols, col)
	}
	for _, p := range t.PrimaryKey {
		found := false
		for _, n := range cols {
			found = found || n == p
		}
		if !found {
			return nil, fmt.Errorf("map the primary key column %s: every row needs it", p)
		}
	}
	if empty {
		if err := c.query(ctx, "TRUNCATE "+im.target).Exec(); err != nil {
			return nil, mapError(err)
		}
	}
	return im, nil
}

type importer struct {
	c      *conn
	target string
	cols   []*driver.Column
}

func (im *importer) Insert(ctx context.Context, rows [][]any) error {
	var wg sync.WaitGroup
	var mu sync.Mutex
	var first error
	sem := make(chan struct{}, 16)
	for n, r := range rows {
		var names, exprs []string
		var args []any
		for j, col := range im.cols {
			if j >= len(r) || r[j] == nil {
				continue // leave out NULLs, which would write tombstones
			}
			expr, arg, err := inputExpr(col, r[j])
			if err != nil {
				return fmt.Errorf("row %d: %w", n+1, err)
			}
			names = append(names, quote(col.Name))
			exprs = append(exprs, expr)
			if arg != nil {
				args = append(args, arg)
			}
		}
		q := "INSERT INTO " + im.target + " (" + strings.Join(names, ", ") + ") VALUES (" + strings.Join(exprs, ", ") + ")"
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer func() { <-sem; wg.Done() }()
			if err := im.c.query(ctx, q, args...).Idempotent(true).Exec(); err != nil {
				mu.Lock()
				if first == nil {
					first = mapError(err)
				}
				mu.Unlock()
			}
		}()
		mu.Lock()
		stop := first != nil
		mu.Unlock()
		if stop || ctx.Err() != nil {
			break
		}
	}
	wg.Wait()
	if first != nil {
		return first
	}
	return ctx.Err()
}

func (im *importer) Commit() error   { return nil }
func (im *importer) Rollback() error { return nil }
