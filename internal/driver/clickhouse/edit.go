package clickhouse

import (
	"context"
	"errors"
	"fmt"
	"strings"

	ch "github.com/ClickHouse/clickhouse-go/v2"

	"rowsmith/internal/driver"
	"rowsmith/internal/sqlsplit"
)

// ApplyEdits changes rows of MergeTree tables. ClickHouse has no
// transactions to roll back, so every change is checked first: each row
// to update or delete must still match exactly one row. Updates run as
// mutations (ALTER TABLE … UPDATE), deletes as lightweight deletes; both
// wait until they're done.
func (c *conn) ApplyEdits(ctx context.Context, ref driver.ObjectRef, edits []driver.RowEdit) (*driver.EditResult, error) {
	if c.ro {
		return nil, driver.ErrReadOnly
	}
	t, err := c.Describe(ctx, ref)
	if err != nil {
		return nil, err
	}
	if !t.Editable {
		return nil, fmt.Errorf("rows of %s can't be changed: only MergeTree tables support it", t.Ref.Name)
	}
	byName := map[string]*driver.Column{}
	for i := range t.Columns {
		byName[t.Columns[i].Name] = &t.Columns[i]
	}
	target := qualify(t.Ref.Database, t.Ref.Name)
	d := dialect{}

	type step struct{ sql, where string }
	var steps []step
	for i, ed := range edits {
		switch ed.Op {
		case "insert":
			var cols, vals []string
			for _, col := range t.Columns {
				v, ok := ed.Values[col.Name]
				if !ok || isMarker(v, "$default") || col.Generated != "" {
					continue
				}
				expr, _, err := d.InputExpr(byName[col.Name], v, "")
				if err != nil {
					return nil, fmt.Errorf("change %d: %w", i+1, err)
				}
				cols = append(cols, quote(col.Name))
				vals = append(vals, expr)
			}
			if len(cols) == 0 {
				return nil, fmt.Errorf("change %d: enter at least one value", i+1)
			}
			steps = append(steps, step{sql: "INSERT INTO " + target + " (" + strings.Join(cols, ", ") + ") VALUES (" + strings.Join(vals, ", ") + ")"})
		case "update", "delete":
			where, err := keyCondition(t, ed.Key)
			if err != nil {
				return nil, fmt.Errorf("change %d: %w", i+1, err)
			}
			if ed.Op == "delete" {
				steps = append(steps, step{sql: "DELETE FROM " + target + " WHERE " + where, where: where})
				continue
			}
			var sets []string
			for _, col := range t.Columns {
				v, ok := ed.Values[col.Name]
				if !ok {
					continue
				}
				if col.Generated != "" {
					return nil, fmt.Errorf("change %d: %s is computed from other columns", i+1, col.Name)
				}
				expr, _, err := d.InputExpr(byName[col.Name], v, "")
				if err != nil {
					return nil, fmt.Errorf("change %d: %w", i+1, err)
				}
				sets = append(sets, quote(col.Name)+" = "+expr)
			}
			if len(sets) == 0 {
				continue
			}
			steps = append(steps, step{sql: "ALTER TABLE " + target + " UPDATE " + strings.Join(sets, ", ") + " WHERE " + where, where: where})
		default:
			return nil, fmt.Errorf("unknown edit operation %q", ed.Op)
		}
	}

	for i, s := range steps {
		if s.where == "" {
			continue
		}
		var n uint64
		if err := c.db.QueryRowContext(ctx, "SELECT count() FROM "+target+" WHERE "+s.where).Scan(&n); err != nil {
			return nil, fmt.Errorf("change %d: %w", i+1, mapError(err, sqlsplit.Statement{}))
		}
		switch {
		case n == 0:
			return nil, fmt.Errorf("change %d: the row no longer exists or was changed by someone else; nothing was saved", i+1)
		case n > 1:
			return nil, fmt.Errorf("change %d: %d rows have exactly these values, so it can't tell them apart; nothing was saved", i+1, n)
		}
	}

	// Mutations finish before the call returns, on every replica.
	wctx := ch.Context(ctx, ch.WithSettings(ch.Settings{"mutations_sync": 2, "lightweight_deletes_sync": 2}))
	res := &driver.EditResult{}
	for i, s := range steps {
		if _, err := c.db.ExecContext(wctx, s.sql); err != nil {
			err = mapError(err, sqlsplit.Statement{})
			if i == 0 {
				return nil, fmt.Errorf("change 1: %w", err)
			}
			return nil, fmt.Errorf("change %d: %w (the %d before it were saved)", i+1, err, i)
		}
		res.Statements = append(res.Statements, s.sql)
		res.Applied++
	}
	return res, nil
}

func isMarker(v any, key string) bool {
	m, ok := v.(map[string]any)
	if !ok {
		return false
	}
	b, _ := m[key].(bool)
	return b
}

// keyCondition matches a row on all its comparable values.
func keyCondition(t *driver.Table, key map[string]any) (string, error) {
	if len(key) == 0 {
		return "", errors.New("missing row key")
	}
	var conds []string
	d := dialect{}
	for i := range t.Columns {
		col := &t.Columns[i]
		v, ok := key[col.Name]
		if !ok || !comparable(col) || preview(v) {
			continue
		}
		if v == nil || isMarker(v, "$null") {
			conds = append(conds, "isNull("+quote(col.Name)+")")
			continue
		}
		expr, _, err := d.InputExpr(col, v, "")
		if err != nil {
			return "", err
		}
		conds = append(conds, quote(col.Name)+" = "+expr)
	}
	if len(conds) == 0 {
		return "", errors.New("this row has no values that can identify it")
	}
	return strings.Join(conds, " AND "), nil
}

// comparable: columns whose shown value identifies the stored one.
func comparable(c *driver.Column) bool {
	if c.Generated != "" && !c.GeneratedStored {
		return false // ALIAS columns aren't stored
	}
	switch c.Kind {
	case driver.KindArray, driver.KindObject, driver.KindJSON, driver.KindGeometry, driver.KindBinary, driver.KindOther, driver.KindFloat:
		return false
	}
	return true
}

// preview reports a cell shown in part: it can't be matched.
func preview(v any) bool {
	m, ok := v.(map[string]any)
	if !ok {
		return false
	}
	_, text := m["$text"]
	_, bin := m["$bin"]
	size, _ := m["size"].(float64)
	return text || (bin && size > 64<<10)
}
