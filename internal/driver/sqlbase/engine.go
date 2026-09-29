package sqlbase

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"rowsmith/internal/driver"
)

// HiddenRowKey is the alias of the synthetic row identifier (ctid, ROWID,
// rowid) added to browse results when a table has no primary key.
const HiddenRowKey = "__rowsmith_rowid"

// Engine bundles a pool with its dialect and implements browse and edit.
type Engine struct {
	DB *sql.DB
	D  Dialect
	// RowIDExpr, when set, yields a physical row identifier for tables
	// without keys, e.g. "ctid" or "ROWID".
	RowIDExpr string
}

// DefaultSelectExpr is the plain quoted column.
func DefaultSelectExpr(d Dialect, col driver.Column) string { return d.QuoteIdent(col.Name) }

// DefaultInputExpr binds the value as-is, handling the edit markers and base64 binary.
func DefaultInputExpr(d Dialect, col *driver.Column, v any, ph string) (string, any, error) {
	switch x := v.(type) {
	case nil:
		return "NULL", nil, nil
	case map[string]any:
		if b, _ := x["$null"].(bool); b {
			return "NULL", nil, nil
		}
		if b, _ := x["$default"].(bool); b {
			return "DEFAULT", nil, nil
		}
		if e, ok := x["$expr"].(string); ok && strings.TrimSpace(e) != "" {
			return e, nil, nil
		}
		if b64, ok := x["$bin"].(string); ok {
			raw, err := base64.StdEncoding.DecodeString(b64)
			if err != nil {
				return "", nil, fmt.Errorf("column %s: invalid binary value", col.Name)
			}
			return ph, raw, nil
		}
		if t, ok := x["$text"].(string); ok {
			return ph, t, nil
		}
		return "", nil, fmt.Errorf("column %s: unsupported value", col.Name)
	case bool:
		if col != nil && (col.Kind == driver.KindInt || col.BaseType == "bit" || col.BaseType == "tinyint") {
			if x {
				return ph, 1, nil
			}
			return ph, 0, nil
		}
		return ph, x, nil
	case float64:
		if x == float64(int64(x)) && col != nil && col.Kind == driver.KindInt {
			return ph, int64(x), nil
		}
		return ph, x, nil
	}
	return ph, v, nil
}

type queryBuilder struct {
	d    Dialect
	args []any
}

func (b *queryBuilder) arg(v any) string {
	b.args = append(b.args, v)
	return b.d.Placeholder(len(b.args))
}

// BuildSelect renders the SELECT used by the data browser.
func (e *Engine) BuildSelect(t *driver.Table, req driver.BrowseRequest, count bool) (string, []any, error) {
	d := e.D
	b := &queryBuilder{d: d}
	byName := map[string]*driver.Column{}
	for i := range t.Columns {
		byName[t.Columns[i].Name] = &t.Columns[i]
	}

	var sel []string
	if !count {
		cols := t.Columns
		if len(req.Columns) > 0 {
			cols = nil
			for _, n := range req.Columns {
				if c, ok := byName[n]; ok {
					cols = append(cols, *c)
				}
			}
		}
		for _, c := range cols {
			expr := d.SelectExpr(c)
			if expr != d.QuoteIdent(c.Name) {
				expr += " AS " + d.QuoteIdent(c.Name)
			}
			sel = append(sel, expr)
		}
		if t.RowKeyKind == "rowid" && e.RowIDExpr != "" {
			sel = append(sel, e.RowIDExpr+" AS "+d.QuoteIdent(HiddenRowKey))
		}
		if len(sel) == 0 {
			sel = []string{"*"}
		}
	}

	var where []string
	for _, f := range req.Filters {
		c, ok := byName[f.Column]
		if !ok {
			return "", nil, fmt.Errorf("unknown column %q", f.Column)
		}
		cond, err := e.filter(b, c, f)
		if err != nil {
			return "", nil, err
		}
		where = append(where, cond)
	}
	if s := strings.TrimSpace(req.Search); s != "" {
		var ors []string
		pat := "%" + escapeLike(s) + "%"
		for _, c := range t.Columns {
			if searchable(c.Kind) {
				ors = append(ors, d.Like(d.TextExpr(c), b.arg(pat), false))
			}
		}
		if len(ors) > 0 {
			where = append(where, "("+strings.Join(ors, " OR ")+")")
		}
	}
	if w := strings.TrimSpace(req.Where); w != "" {
		where = append(where, "("+w+")")
	}

	var q strings.Builder
	q.WriteString("SELECT ")
	if count {
		q.WriteString("COUNT(*)")
	} else {
		q.WriteString(strings.Join(sel, ", "))
	}
	q.WriteString(" FROM ")
	q.WriteString(d.Qualify(t.Ref))
	if len(where) > 0 {
		q.WriteString(" WHERE ")
		q.WriteString(strings.Join(where, " AND "))
	}
	if count {
		return q.String(), b.args, nil
	}

	var order []string
	for _, s := range req.Sort {
		if _, ok := byName[s.Column]; !ok {
			return "", nil, fmt.Errorf("unknown sort column %q", s.Column)
		}
		o := d.QuoteIdent(s.Column)
		if s.Desc {
			o += " DESC"
		}
		order = append(order, o)
	}
	if len(order) == 0 && t.Kind != "view" {
		// A stable order keeps pages consistent.
		for _, k := range t.PrimaryKey {
			order = append(order, d.QuoteIdent(k))
		}
	}
	if len(order) > 0 {
		q.WriteString(" ORDER BY ")
		q.WriteString(strings.Join(order, ", "))
	}
	limit := req.Limit
	if limit <= 0 || limit > 100000 {
		limit = 200
	}
	// Fetch one extra row to learn whether another page exists.
	return d.Paginate(q.String(), limit+1, req.Offset, len(order) > 0), b.args, nil
}

func searchable(k driver.ValueKind) bool {
	switch k {
	case driver.KindString, driver.KindText, driver.KindEnum, driver.KindJSON, driver.KindUUID, driver.KindInt, driver.KindDecimal:
		return true
	}
	return false
}

func (e *Engine) filter(b *queryBuilder, c *driver.Column, f driver.Filter) (string, error) {
	d := e.D
	col := d.QuoteIdent(c.Name)
	val := func(v any) any {
		if s, ok := v.(string); ok && c.Kind == driver.KindBool {
			return s == "1" || strings.EqualFold(s, "true")
		}
		return v
	}
	switch f.Op {
	case "=", "!=", "<", "<=", ">", ">=":
		op := f.Op
		if op == "!=" {
			op = "<>"
		}
		return col + " " + op + " " + b.arg(val(f.Value)), nil
	case "like", "notlike":
		return d.Like(d.TextExpr(*c), b.arg(fmt.Sprint(f.Value)), f.Op == "notlike"), nil
	case "contains":
		return d.Like(d.TextExpr(*c), b.arg("%"+escapeLike(fmt.Sprint(f.Value))+"%"), false), nil
	case "startswith":
		return d.Like(d.TextExpr(*c), b.arg(escapeLike(fmt.Sprint(f.Value))+"%"), false), nil
	case "endswith":
		return d.Like(d.TextExpr(*c), b.arg("%"+escapeLike(fmt.Sprint(f.Value))), false), nil
	case "in", "notin":
		if len(f.Values) == 0 {
			if f.Op == "in" {
				return "1 = 0", nil
			}
			return "1 = 1", nil
		}
		ph := make([]string, len(f.Values))
		for i, v := range f.Values {
			ph[i] = b.arg(val(v))
		}
		op := " IN ("
		if f.Op == "notin" {
			op = " NOT IN ("
		}
		return col + op + strings.Join(ph, ", ") + ")", nil
	case "null":
		return col + " IS NULL", nil
	case "notnull":
		return col + " IS NOT NULL", nil
	case "empty":
		return "(" + col + " IS NULL OR " + d.TextExpr(*c) + " = '')", nil
	case "regexp":
		r := d.Regexp(d.TextExpr(*c), b.arg(fmt.Sprint(f.Value)))
		if r == "" {
			return "", errors.New("regular expressions are not supported by this database")
		}
		return r, nil
	case "between":
		if len(f.Values) != 2 {
			return "", errors.New("between needs two values")
		}
		return col + " BETWEEN " + b.arg(val(f.Values[0])) + " AND " + b.arg(val(f.Values[1])), nil
	}
	return "", fmt.Errorf("unknown filter operator %q", f.Op)
}

func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// Browse runs the data browser query for a described table.
func (e *Engine) Browse(ctx context.Context, t *driver.Table, req driver.BrowseRequest) (*driver.Result, error) {
	q, args, err := e.BuildSelect(t, req, false)
	if err != nil {
		return nil, err
	}
	limit := req.Limit
	if limit <= 0 || limit > 100000 {
		limit = 200
	}
	res, err := Collect(ctx, e.D, e.DB, limit, q, args...)
	if err != nil {
		return nil, err
	}
	res.SQL = q
	// Restore declared kinds (e.g. geometry rendered as GeoJSON text).
	byName := map[string]driver.Column{}
	for _, c := range t.Columns {
		byName[c.Name] = c
	}
	for i, rc := range res.Columns {
		if c, ok := byName[rc.Name]; ok && c.Kind != "" {
			if c.Kind == driver.KindGeometry {
				res.Columns[i].Kind = driver.KindGeometry
				res.Columns[i].Type = c.Type
				for _, row := range res.Rows {
					row[i] = GeoCell(row[i], c.SRID)
				}
			}
		}
	}
	return res, nil
}

func (e *Engine) Count(ctx context.Context, t *driver.Table, req driver.BrowseRequest) (driver.Count, error) {
	q, args, err := e.BuildSelect(t, req, true)
	if err != nil {
		return driver.Count{}, err
	}
	var n int64
	if err := e.DB.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
		return driver.Count{}, err
	}
	return driver.Count{Rows: n, Exact: true}, nil
}

// ApplyEdits performs grid edits inside one transaction. Every UPDATE and
// DELETE must match exactly one row or the whole batch is rolled back.
func (e *Engine) ApplyEdits(ctx context.Context, t *driver.Table, edits []driver.RowEdit) (*driver.EditResult, error) {
	if len(t.RowKey) == 0 && t.RowKeyKind != "all" {
		for _, ed := range edits {
			if ed.Op != "insert" {
				return nil, driver.ErrNoRowKey
			}
		}
	}
	byName := map[string]*driver.Column{}
	for i := range t.Columns {
		byName[t.Columns[i].Name] = &t.Columns[i]
	}
	tx, err := e.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	res := &driver.EditResult{}
	target := e.D.Qualify(t.Ref)
	for i, ed := range edits {
		b := &queryBuilder{d: e.D}
		var q string
		switch ed.Op {
		case "insert":
			var cols, vals []string
			for _, c := range t.Columns {
				v, ok := ed.Values[c.Name]
				if !ok {
					continue
				}
				expr, arg, err := e.D.InputExpr(byName[c.Name], v, placeholderToken)
				if err != nil {
					return nil, err
				}
				cols = append(cols, e.D.QuoteIdent(c.Name))
				vals = append(vals, b.bind(expr, arg))
			}
			if len(cols) == 0 {
				q = "INSERT INTO " + target + " DEFAULT VALUES"
			} else {
				q = "INSERT INTO " + target + " (" + strings.Join(cols, ", ") + ") VALUES (" + strings.Join(vals, ", ") + ")"
			}
		case "update", "delete":
			pre, suf := "", ""
			if t.RowKeyKind == "all" {
				pre, suf = e.D.LimitOne()
			}
			if ed.Op == "delete" {
				cond, err := e.keyCondition(b, t, byName, ed.Key)
				if err != nil {
					return nil, err
				}
				q = "DELETE " + pre + "FROM " + target + " WHERE " + cond + suf
				break
			}
			// Positional placeholders: SET arguments must be bound before WHERE arguments.
			var sets []string
			for _, c := range t.Columns {
				v, ok := ed.Values[c.Name]
				if !ok {
					continue
				}
				expr, arg, err := e.D.InputExpr(byName[c.Name], v, placeholderToken)
				if err != nil {
					return nil, err
				}
				sets = append(sets, e.D.QuoteIdent(c.Name)+" = "+b.bind(expr, arg))
			}
			if len(sets) == 0 {
				continue
			}
			cond, err := e.keyCondition(b, t, byName, ed.Key)
			if err != nil {
				return nil, err
			}
			q = "UPDATE " + pre + target + " SET " + strings.Join(sets, ", ") + " WHERE " + cond + suf
		default:
			return nil, fmt.Errorf("unknown edit operation %q", ed.Op)
		}
		r, err := tx.ExecContext(ctx, q, b.args...)
		if err != nil {
			return nil, fmt.Errorf("change %d: %w", i+1, err)
		}
		if ed.Op != "insert" {
			if n, err := r.RowsAffected(); err == nil && n != 1 {
				if n == 0 {
					return nil, fmt.Errorf("change %d: the row no longer exists or was modified by someone else", i+1)
				}
				return nil, fmt.Errorf("change %d would affect %d rows; nothing was saved", i+1, n)
			}
		}
		res.Statements = append(res.Statements, q)
		res.Applied++
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return res, nil
}

const placeholderToken = "\x00ph\x00"

// bind substitutes the dialect placeholder for the token InputExpr returned.
func (b *queryBuilder) bind(expr string, arg any) string {
	if !strings.Contains(expr, placeholderToken) {
		return expr
	}
	return strings.Replace(expr, placeholderToken, b.arg(arg), 1)
}

func (e *Engine) keyCondition(b *queryBuilder, t *driver.Table, byName map[string]*driver.Column, key map[string]any) (string, error) {
	if len(key) == 0 {
		return "", errors.New("missing row key")
	}
	var conds []string
	if t.RowKeyKind == "rowid" {
		v, ok := key[HiddenRowKey]
		if !ok {
			return "", errors.New("missing row identifier")
		}
		return e.RowIDExpr + " = " + b.arg(v), nil
	}
	cols := t.RowKey
	if t.RowKeyKind == "all" {
		cols = nil
		for _, c := range t.Columns {
			if _, ok := key[c.Name]; ok && comparable(c.Kind) {
				cols = append(cols, c.Name)
			}
		}
	}
	for _, name := range cols {
		v, ok := key[name]
		if !ok {
			return "", fmt.Errorf("missing key column %q", name)
		}
		c := byName[name]
		qc := e.D.QuoteIdent(name)
		if v == nil {
			conds = append(conds, qc+" IS NULL")
			continue
		}
		expr, arg, err := e.D.InputExpr(c, v, placeholderToken)
		if err != nil {
			return "", err
		}
		conds = append(conds, qc+" = "+b.bind(expr, arg))
	}
	if len(conds) == 0 {
		return "", errors.New("no usable key columns")
	}
	return strings.Join(conds, " AND "), nil
}

func comparable(k driver.ValueKind) bool {
	switch k {
	case driver.KindText, driver.KindBinary, driver.KindJSON, driver.KindGeometry, driver.KindArray, driver.KindObject:
		return false
	}
	return true
}

// ChooseRowKey picks how rows of t are identified for editing.
func ChooseRowKey(t *driver.Table, rowID bool) {
	if len(t.PrimaryKey) > 0 {
		t.RowKey, t.RowKeyKind, t.Editable = t.PrimaryKey, "primary", true
		return
	}
	for _, ix := range t.Indexes {
		if !ix.Unique || len(ix.Columns) == 0 || ix.Where != "" {
			continue
		}
		ok := true
		for _, c := range ix.Columns {
			col := findCol(t, c)
			if col == nil || col.Nullable {
				ok = false
				break
			}
		}
		if ok {
			t.RowKey, t.RowKeyKind, t.Editable = ix.Columns, "unique", true
			return
		}
	}
	if t.Kind == "table" || t.Kind == "partitioned_table" {
		if rowID {
			t.RowKey, t.RowKeyKind, t.Editable = []string{HiddenRowKey}, "rowid", true
			return
		}
		t.RowKeyKind, t.Editable = "all", true
	}
}

func findCol(t *driver.Table, name string) *driver.Column {
	for i := range t.Columns {
		if t.Columns[i].Name == name {
			return &t.Columns[i]
		}
	}
	return nil
}

// Timed runs fn and reports its duration in milliseconds.
func Timed(fn func() error) (float64, error) {
	start := time.Now()
	err := fn()
	return float64(time.Since(start).Microseconds()) / 1000, err
}
