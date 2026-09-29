package bigquery

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/bigquery"
	"google.golang.org/api/iterator"

	"rowsmith/internal/driver"
)

// freeReadSQL is shown instead of a query when rows come from tabledata.list.
const freeReadSQL = "-- Read from table storage with tabledata.list: no query job, nothing billed."

// unfiltered reports whether a browse request needs no query at all.
func unfiltered(req driver.BrowseRequest) bool {
	return len(req.Filters) == 0 && strings.TrimSpace(req.Search) == "" && strings.TrimSpace(req.Where) == ""
}

// storedRows reports whether a table type can be paged with tabledata.list.
func storedRows(t bigquery.TableType) bool {
	return t == bigquery.RegularTable || t == bigquery.Snapshot
}

func browseLimit(n int) int {
	if n <= 0 || n > 100000 {
		return 200
	}
	return n
}

// Browse pages through a table. An unfiltered, unsorted browse of a stored
// table is free: rows are read with tabledata.list instead of a query. Any
// filter, search or sort needs a query job, which the cost guard applies to.
func (c *conn) Browse(ctx context.Context, req driver.BrowseRequest) (*driver.Result, error) {
	tbl, md, err := c.metadata(ctx, req.Ref)
	if err != nil {
		return nil, err
	}
	limit := browseLimit(req.Limit)
	if unfiltered(req) && len(req.Sort) == 0 && storedRows(md.Type) {
		return readTable(ctx, tbl, md.Schema, req, limit)
	}
	t := describeMetadata(req.Ref, tbl, md)
	sql, params, err := buildSelect(tablePath(tbl.ProjectID, tbl.DatasetID, tbl.TableID), t, req, false)
	if err != nil {
		return nil, err
	}
	q := c.newQuery(sql, md.Location)
	q.Parameters = params
	res, err := c.collect(ctx, q, limit)
	if err != nil {
		return nil, err
	}
	res.SQL = sql
	return res, nil
}

// readTable reads one page of stored rows with tabledata.list.
func readTable(ctx context.Context, tbl *bigquery.Table, schema bigquery.Schema, req driver.BrowseRequest, limit int) (*driver.Result, error) {
	start := time.Now()
	it := tbl.Read(ctx)
	it.Schema = schema // known already: saves the iterator a metadata request
	it.StartIndex = uint64(max(req.Offset, 0))
	it.PageInfo().MaxSize = pageSize(limit)

	// tabledata.list returns whole rows; project requested columns locally.
	pick := make([]int, 0, len(schema))
	if len(req.Columns) > 0 {
		pos := map[string]int{}
		for i, fs := range schema {
			pos[fs.Name] = i
		}
		for _, n := range req.Columns {
			if i, ok := pos[n]; ok {
				pick = append(pick, i)
			}
		}
	}
	if len(pick) == 0 {
		for i := range schema {
			pick = append(pick, i)
		}
	}
	cols := make(bigquery.Schema, len(pick))
	for j, i := range pick {
		cols[j] = schema[i]
	}

	res := &driver.Result{Columns: resultColumns(cols), Rows: [][]any{}, SQL: freeReadSQL}
	for {
		var row []bigquery.Value
		err := it.Next(&row)
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, mapError(err)
		}
		if len(res.Rows) >= limit {
			res.Truncated = true
			break
		}
		cells := make([]any, len(pick))
		for j, i := range pick {
			if i < len(row) {
				cells[j] = encodeValue(row[i], schema[i])
			}
		}
		res.Rows = append(res.Rows, cells)
	}
	res.Duration = time.Since(start)
	res.DurationMS = float64(res.Duration.Microseconds()) / 1000
	return res, nil
}

// Count uses table metadata when nothing is filtered (free and exact for
// stored tables, apart from rows still in the streaming buffer) and a
// COUNT(*) query under the cost guard otherwise.
func (c *conn) Count(ctx context.Context, req driver.BrowseRequest) (driver.Count, error) {
	tbl, md, err := c.metadata(ctx, req.Ref)
	if err != nil {
		return driver.Count{}, err
	}
	if unfiltered(req) {
		switch {
		case storedRows(md.Type):
			n, exact := int64(md.NumRows), true
			if sb := md.StreamingBuffer; sb != nil && sb.EstimatedRows > 0 {
				n, exact = n+int64(sb.EstimatedRows), false
			}
			return driver.Count{Rows: n, Exact: exact}, nil
		case md.Type == bigquery.MaterializedView:
			return driver.Count{Rows: int64(md.NumRows), Exact: false}, nil
		}
	}
	t := describeMetadata(req.Ref, tbl, md)
	sql, params, err := buildSelect(tablePath(tbl.ProjectID, tbl.DatasetID, tbl.TableID), t, req, true)
	if err != nil {
		return driver.Count{}, err
	}
	q := c.newQuery(sql, md.Location)
	q.Parameters = params
	job, _, err := c.runJob(ctx, q)
	if err != nil {
		return driver.Count{}, mapError(err)
	}
	it, err := job.Read(ctx)
	if err != nil {
		return driver.Count{}, mapError(err)
	}
	var row []bigquery.Value
	if err := it.Next(&row); err != nil {
		return driver.Count{}, mapError(err)
	}
	n, ok := row[0].(int64)
	if !ok {
		return driver.Count{}, fmt.Errorf("unexpected COUNT(*) result %v", row[0])
	}
	return driver.Count{Rows: n, Exact: true}, nil
}

// ApplyEdits is not supported: BigQuery rows have no identity to target
// safely. Changes are made with DML in the SQL console.
func (c *conn) ApplyEdits(ctx context.Context, ref driver.ObjectRef, edits []driver.RowEdit) (*driver.EditResult, error) {
	return nil, driver.ErrNotSupported
}

// ---- query building ------------------------------------------------------------

// builder collects named query parameters (@p1, @p2, ...).
type builder struct {
	params []bigquery.QueryParameter
}

func (b *builder) arg(v any) string {
	name := "p" + strconv.Itoa(len(b.params)+1)
	b.params = append(b.params, bigquery.QueryParameter{Name: name, Value: v})
	return "@" + name
}

// buildSelect renders the GoogleSQL query for a filtered, searched or sorted
// browse (or its COUNT(*)). Values always travel as named parameters.
func buildSelect(path string, t *driver.Table, req driver.BrowseRequest, count bool) (string, []bigquery.QueryParameter, error) {
	b := &builder{}
	byName := map[string]*driver.Column{}
	for i := range t.Columns {
		byName[t.Columns[i].Name] = &t.Columns[i]
	}

	sel := "COUNT(*)"
	if !count {
		var cols []string
		for _, n := range req.Columns {
			if _, ok := byName[n]; ok {
				cols = append(cols, quote(n))
			}
		}
		if len(cols) == 0 {
			for _, c := range t.Columns {
				cols = append(cols, quote(c.Name))
			}
		}
		if len(cols) == 0 {
			cols = []string{"*"}
		}
		sel = strings.Join(cols, ", ")
	}

	var where []string
	for _, f := range req.Filters {
		c, ok := byName[f.Column]
		if !ok {
			return "", nil, fmt.Errorf("unknown column %q", f.Column)
		}
		cond, err := b.filter(c, f)
		if err != nil {
			return "", nil, err
		}
		where = append(where, cond)
	}
	if s := strings.TrimSpace(req.Search); s != "" {
		var ors []string
		pat := "%" + escapeLike(s) + "%"
		for i := range t.Columns {
			if c := &t.Columns[i]; searchable(c) {
				ors = append(ors, like(textExpr(c), b.arg(pat), false))
			}
		}
		if len(ors) > 0 {
			where = append(where, "("+strings.Join(ors, " OR ")+")")
		}
	}
	if w := strings.TrimSpace(req.Where); w != "" {
		where = append(where, "("+w+")")
	}

	q := "SELECT " + sel + " FROM " + path
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	if count {
		return q, b.params, nil
	}

	var order []string
	for _, s := range req.Sort {
		c, ok := byName[s.Column]
		if !ok {
			return "", nil, fmt.Errorf("unknown sort column %q", s.Column)
		}
		if !orderable(c) {
			return "", nil, fmt.Errorf("cannot sort by %s: %s values are not orderable", c.Name, c.Type)
		}
		o := quote(c.Name)
		if s.Desc {
			o += " DESC"
		}
		order = append(order, o)
	}
	if len(order) == 0 {
		// A declared (unenforced) primary key keeps pages stable.
		for _, k := range t.PrimaryKey {
			order = append(order, quote(k))
		}
	}
	if len(order) > 0 {
		q += " ORDER BY " + strings.Join(order, ", ")
	}
	// Fetch one extra row to learn whether another page exists.
	q += " LIMIT " + strconv.Itoa(browseLimit(req.Limit)+1)
	if req.Offset > 0 {
		q += " OFFSET " + strconv.FormatInt(req.Offset, 10)
	}
	return q, b.params, nil
}

func (b *builder) filter(c *driver.Column, f driver.Filter) (string, error) {
	col := quote(c.Name)
	switch f.Op {
	case "=", "!=", "<", "<=", ">", ">=":
		v, err := b.value(c, f.Value)
		if err != nil {
			return "", err
		}
		return col + " " + f.Op + " " + v, nil
	case "like", "notlike":
		return like(textExpr(c), b.arg(text(f.Value)), f.Op == "notlike"), nil
	case "contains":
		return like(textExpr(c), b.arg("%"+escapeLike(text(f.Value))+"%"), false), nil
	case "startswith":
		return like(textExpr(c), b.arg(escapeLike(text(f.Value))+"%"), false), nil
	case "endswith":
		return like(textExpr(c), b.arg("%"+escapeLike(text(f.Value))), false), nil
	case "in", "notin":
		if len(f.Values) == 0 {
			if f.Op == "in" {
				return "FALSE", nil
			}
			return "TRUE", nil
		}
		vals := make([]string, len(f.Values))
		for i, v := range f.Values {
			s, err := b.value(c, v)
			if err != nil {
				return "", err
			}
			vals[i] = s
		}
		op := " IN ("
		if f.Op == "notin" {
			op = " NOT IN ("
		}
		return col + op + strings.Join(vals, ", ") + ")", nil
	case "null":
		return col + " IS NULL", nil
	case "notnull":
		return col + " IS NOT NULL", nil
	case "empty":
		return "(" + col + " IS NULL OR " + textExpr(c) + " = '')", nil
	case "regexp":
		return "REGEXP_CONTAINS(" + textExpr(c) + ", " + b.arg("(?i)"+text(f.Value)) + ")", nil
	case "between":
		if len(f.Values) != 2 {
			return "", errors.New("between needs two values")
		}
		lo, err := b.value(c, f.Values[0])
		if err != nil {
			return "", err
		}
		hi, err := b.value(c, f.Values[1])
		if err != nil {
			return "", err
		}
		return col + " BETWEEN " + lo + " AND " + hi, nil
	}
	return "", fmt.Errorf("unknown filter operator %q", f.Op)
}

// value binds a filter value for comparison with column c. Native JSON
// values bind with their own type; everything else is sent as text and cast
// by BigQuery, which reports malformed input precisely.
func (b *builder) value(c *driver.Column, v any) (string, error) {
	if !orderable(c) {
		return "", fmt.Errorf("column %s (%s) cannot be compared with a value", c.Name, c.Type)
	}
	if v == nil {
		return "NULL", nil
	}
	switch c.BaseType {
	case "string":
		return b.arg(text(v)), nil
	case "int64":
		if f, ok := v.(float64); ok && f == math.Trunc(f) && math.Abs(f) <= 1<<53 {
			return b.arg(int64(f)), nil
		}
	case "float64":
		if f, ok := v.(float64); ok {
			return b.arg(f), nil
		}
	case "bool":
		switch x := v.(type) {
		case bool:
			return b.arg(x), nil
		case string:
			switch strings.ToLower(strings.TrimSpace(x)) {
			case "1", "true", "t", "yes":
				return b.arg(true), nil
			case "0", "false", "f", "no":
				return b.arg(false), nil
			}
		}
	}
	return "CAST(" + b.arg(text(v)) + " AS " + castType(c) + ")", nil
}

// castType is the column type without length or precision parameters.
func castType(c *driver.Column) string {
	if i := strings.IndexByte(c.Type, '('); i > 0 {
		return c.Type[:i]
	}
	return c.Type
}

// orderable reports whether values of c support comparison and ORDER BY.
func orderable(c *driver.Column) bool {
	switch c.Kind {
	case driver.KindObject, driver.KindArray, driver.KindGeometry, driver.KindJSON:
		return false
	}
	return true
}

func searchable(c *driver.Column) bool {
	return c.BaseType == "string" || c.Kind == driver.KindInt || c.Kind == driver.KindDecimal || c.Kind == driver.KindJSON
}

// textExpr renders a column as STRING for pattern matching.
func textExpr(c *driver.Column) string {
	col := quote(c.Name)
	switch {
	case c.BaseType == "string":
		return col
	case c.Kind == driver.KindGeometry:
		return "ST_ASTEXT(" + col + ")"
	case c.Kind == driver.KindJSON, c.Kind == driver.KindObject, c.Kind == driver.KindArray, c.Kind == driver.KindBinary:
		return "TO_JSON_STRING(" + col + ")"
	}
	return "CAST(" + col + " AS STRING)"
}

func like(expr, ph string, negate bool) string {
	op := " LIKE "
	if negate {
		op = " NOT LIKE "
	}
	return "LOWER(" + expr + ")" + op + "LOWER(" + ph + ")"
}

func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

func text(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(x)
	case nil:
		return ""
	}
	return fmt.Sprint(v)
}
