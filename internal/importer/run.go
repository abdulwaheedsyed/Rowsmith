package importer

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"rowsmith/internal/driver"
)

// Mapping sends a source column to a table column.
type Mapping struct {
	Source string `json:"source"` // source column name
	Column string `json:"column"` // table column name
}

// Job loads a source into a table.
type Job struct {
	Source   Source
	Table    *driver.Table
	Mapping  []Mapping
	Empty    bool // delete existing rows first
	Options  Options
	Batch    int              // rows per Insert call (default 500)
	Progress func(rows int64) // called at most every Interval
	Interval time.Duration
}

// RowError locates a failure in the file.
type RowError struct {
	From, To int // 1-based record numbers
	Column   string
	Err      error
}

func (e *RowError) Error() string {
	where := fmt.Sprintf("record %d", e.From)
	if e.To > e.From {
		where = fmt.Sprintf("records %d–%d", e.From, e.To)
	}
	if e.Column != "" {
		where += ", column " + e.Column
	}
	return where + ": " + e.Err.Error()
}

func (e *RowError) Unwrap() error { return e.Err }

// Run loads every record in one transaction. Nothing is kept on error.
func Run(ctx context.Context, bi driver.BulkImporter, job Job) (int64, error) {
	cols := job.Source.Columns()
	srcIdx := map[string]int{}
	for i, c := range cols {
		srcIdx[c] = i
	}
	byName := map[string]*driver.Column{}
	for i := range job.Table.Columns {
		byName[job.Table.Columns[i].Name] = &job.Table.Columns[i]
	}
	type target struct {
		src int
		col *driver.Column
	}
	var targets []target
	var names []string
	seen := map[string]bool{}
	for _, m := range job.Mapping {
		if m.Column == "" {
			continue
		}
		i, ok := srcIdx[m.Source]
		if !ok {
			return 0, fmt.Errorf("the file has no column %q", m.Source)
		}
		c, ok := byName[m.Column]
		if !ok {
			if !job.Options.Documents {
				return 0, fmt.Errorf("the table has no column %q", m.Column)
			}
			c = &driver.Column{Name: m.Column} // documents take any field
		}
		if seen[m.Column] {
			return 0, fmt.Errorf("column %s is mapped twice", m.Column)
		}
		seen[m.Column] = true
		targets = append(targets, target{i, c})
		names = append(names, c.Name)
	}
	if len(targets) == 0 {
		return 0, errors.New("map at least one column of the file to the table")
	}
	ri, err := bi.BeginImport(ctx, job.Table, names, job.Empty)
	if err != nil {
		return 0, err
	}
	committed := false
	defer func() {
		if !committed {
			ri.Rollback()
		}
	}()
	batchSize := job.Batch
	if batchSize <= 0 {
		batchSize = 500
	}
	var total int64
	batch := make([][]any, 0, batchSize)
	firstRec := 0
	last := time.Now()
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := ri.Insert(ctx, batch); err != nil {
			return &RowError{From: firstRec, To: job.Source.Record(), Err: err}
		}
		total += int64(len(batch))
		batch = batch[:0]
		if job.Progress != nil && time.Since(last) >= job.Interval {
			last = time.Now()
			job.Progress(total)
		}
		return nil
	}
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		rec, err := job.Source.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return total, err
		}
		row := make([]any, len(targets))
		blank := true
		for j, t := range targets {
			var v any
			if t.src < len(rec) {
				v = rec[t.src]
			}
			pv, err := Prepare(v, t.col, job.Options)
			if err != nil {
				return total, &RowError{From: job.Source.Record(), Column: t.col.Name, Err: err}
			}
			row[j] = pv
			if v != nil && v != "" {
				blank = false
			}
		}
		if blank {
			continue // empty line or row
		}
		if len(batch) == 0 {
			firstRec = job.Source.Record()
		}
		batch = append(batch, row)
		if len(batch) >= batchSize {
			if err := flush(); err != nil {
				return total, err
			}
		}
	}
	if err := flush(); err != nil {
		return total, err
	}
	if err := ri.Commit(); err != nil {
		return total, err
	}
	committed = true
	if job.Progress != nil {
		job.Progress(total)
	}
	return total, nil
}

func textual(c *driver.Column) bool {
	switch c.Kind {
	case driver.KindString, driver.KindText, driver.KindEnum:
		return true
	}
	return false
}

// Prepare turns a file value into what the column's grid editor would send.
func Prepare(v any, col *driver.Column, opts Options) (any, error) {
	if opts.Documents {
		// The document store converts values itself, using the field's type.
		if s, ok := v.(string); ok && (s == "" && opts.EmptyAsNull || opts.NullText != "" && s == opts.NullText) {
			return nil, nil
		}
		return v, nil
	}
	switch x := v.(type) {
	case nil:
		return nil, nil
	case string:
		if opts.NullText != "" && x == opts.NullText {
			return nil, nil
		}
		if x == "" {
			if textual(col) && !opts.EmptyAsNull {
				return "", nil
			}
			return nil, nil
		}
		switch col.Kind {
		case driver.KindBinary:
			for _, p := range []string{"0x", "0X", `\x`} {
				if strings.HasPrefix(x, p) {
					if b, err := hex.DecodeString(x[len(p):]); err == nil {
						return map[string]any{"$bin": base64.StdEncoding.EncodeToString(b)}, nil
					}
				}
			}
			return map[string]any{"$bin": base64.StdEncoding.EncodeToString([]byte(x))}, nil
		case driver.KindBool:
			switch strings.ToLower(strings.TrimSpace(x)) {
			case "true", "t", "yes", "y", "1", "on":
				return true, nil
			case "false", "f", "no", "n", "0", "off":
				return false, nil
			}
			return nil, fmt.Errorf("%q is not a yes/no value", x)
		case driver.KindInt, driver.KindFloat, driver.KindDecimal:
			return strings.TrimSpace(x), nil
		}
		return x, nil
	case json.Number:
		if col.Kind == driver.KindBool {
			return x.String() != "0", nil
		}
		return x.String(), nil
	case bool, float64, int64:
		return x, nil
	case map[string]any:
		for _, k := range []string{"$oid", "$numberDecimal", "$numberLong", "$numberInt", "$numberDouble", "$uuid"} {
			if s, ok := x[k].(string); ok {
				return Prepare(s, col, opts)
			}
		}
		if d, ok := x["$date"]; ok {
			if s, ok := d.(string); ok {
				return s, nil
			}
		}
		if b, ok := x["$binary"].(map[string]any); ok {
			if s, ok := b["base64"].(string); ok {
				return map[string]any{"$bin": s}, nil
			}
		}
		if _, ok := x["$bin"]; ok {
			return x, nil
		}
		if col.Kind == driver.KindGeometry {
			if _, ok := x["type"]; ok {
				b, _ := json.Marshal(x)
				return string(b), nil // GeoJSON
			}
		}
		b, err := json.Marshal(x)
		return string(b), err
	case []any:
		b, err := json.Marshal(x)
		return string(b), err
	}
	return v, nil
}
