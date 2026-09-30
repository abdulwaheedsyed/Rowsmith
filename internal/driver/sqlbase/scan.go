package sqlbase

import (
	"context"
	"database/sql"
	"time"

	"rowsmith/internal/driver"
)

func asTime(v any) (time.Time, bool) {
	switch t := v.(type) {
	case time.Time:
		return t, true
	case *time.Time:
		if t != nil {
			return *t, true
		}
	}
	return time.Time{}, false
}

// Columns converts database/sql column metadata into result columns.
func Columns(d Dialect, cts []*sql.ColumnType) ([]driver.ResultColumn, []driver.ValueKind) {
	cols := make([]driver.ResultColumn, len(cts))
	kinds := make([]driver.ValueKind, len(cts))
	for i, ct := range cts {
		k := d.Kind(ct)
		kinds[i] = k
		cols[i] = driver.ResultColumn{Name: ct.Name(), Type: ct.DatabaseTypeName(), Kind: k}
		if n, ok := ct.Nullable(); ok {
			nn := n
			cols[i].Nullable = &nn
		}
	}
	return cols, kinds
}

// ScanRow reads the current row and encodes every cell.
func ScanRow(d Dialect, rows *sql.Rows, cts []*sql.ColumnType, kinds []driver.ValueKind) ([]any, error) {
	vals := make([]any, len(cts))
	ptrs := make([]any, len(cts))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		return nil, err
	}
	for i, v := range vals {
		vals[i] = d.Encode(v, cts[i], kinds[i])
	}
	return vals, nil
}

// Collect materializes a query result, reading at most max rows (0 = no limit).
func Collect(ctx context.Context, d Dialect, c Conn, max int, query string, args ...any) (*driver.Result, error) {
	start := time.Now()
	rows, err := c.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	res, err := CollectRows(d, rows, max)
	if err != nil {
		return nil, err
	}
	res.Duration = time.Since(start)
	res.DurationMS = float64(res.Duration.Microseconds()) / 1000
	return res, nil
}

func CollectRows(d Dialect, rows *sql.Rows, max int) (*driver.Result, error) {
	cts, err := rows.ColumnTypes()
	if err != nil {
		return nil, err
	}
	cols, kinds := Columns(d, cts)
	res := &driver.Result{Columns: cols, Rows: [][]any{}}
	for rows.Next() {
		if max > 0 && len(res.Rows) >= max {
			res.Truncated = true
			break
		}
		r, err := ScanRow(d, rows, cts, kinds)
		if err != nil {
			return nil, err
		}
		driver.PreviewRow(r) // collected results are for display
		res.Rows = append(res.Rows, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return res, nil
}

// Strings runs a query returning one text column.
func Strings(ctx context.Context, c Conn, query string, args ...any) ([]string, error) {
	rows, err := c.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s sql.NullString
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s.String)
	}
	return out, rows.Err()
}

// NullInt returns a pointer for optional numeric metadata.
func NullInt(n sql.NullInt64) *int64 {
	if !n.Valid {
		return nil
	}
	v := n.Int64
	return &v
}

func NullStr(n sql.NullString) *string {
	if !n.Valid {
		return nil
	}
	v := n.String
	return &v
}
