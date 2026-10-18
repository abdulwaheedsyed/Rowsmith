package clickhouse

import (
	"context"
	"crypto/rand"
	"database/sql"
	sqldriver "database/sql/driver"
	"encoding/hex"
	"fmt"
	"strings"

	ch "github.com/ClickHouse/clickhouse-go/v2"

	"rowsmith/internal/driver"
	"rowsmith/internal/driver/clickhouse/chtype"
	"rowsmith/internal/sqlsplit"
)

// SelectColumn reads a column for a dump the way browsing does: maps as
// their ordered pairs.
func (c *conn) SelectColumn(col driver.Column) string { return dialect{}.SelectExpr(col) }

// BrowseQuery selects every row a browse request matches, for exports.
func (c *conn) BrowseQuery(_ context.Context, t *driver.Table, req driver.BrowseRequest) (string, []any, error) {
	return c.eng.ExportQuery(t, req)
}

// BeginImport loads rows into a staging table first, then copies them into
// the target in one INSERT … SELECT on Commit: if a row fails, the target
// is untouched. ClickHouse has no transactions to do this otherwise. Without
// the right to create tables, rows go straight into the target.
func (c *conn) BeginImport(ctx context.Context, t *driver.Table, cols []string, empty bool) (driver.RowImporter, error) {
	if c.ro {
		return nil, driver.ErrReadOnly
	}
	byName := map[string]driver.Column{}
	for _, col := range t.Columns {
		byName[col.Name] = col
	}
	imp := &importer{conn: c, target: qualify(t.Ref.Database, t.Ref.Name), empty: empty}
	for _, name := range cols {
		col, ok := byName[name]
		if !ok {
			return nil, fmt.Errorf("unknown column %q", name)
		}
		imp.cols = append(imp.cols, quote(name))
		imp.types = append(imp.types, typeOf(fullType(col)))
	}
	sc, err := c.db.Conn(ctx)
	if err != nil {
		return nil, mapError(err, sqlsplit.Statement{})
	}
	imp.sc = sc
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	imp.staging = qualify(t.Ref.Database, "_rowsmith_import_"+hex.EncodeToString(b))
	defs := make([]string, len(cols))
	for i, name := range cols {
		defs[i] = quote(name) + " " + imp.types[i].Raw
	}
	// On disk, unsorted: a large migration doesn't sit in the server's memory.
	if _, err := sc.ExecContext(ctx, "CREATE TABLE "+imp.staging+" ("+strings.Join(defs, ", ")+") ENGINE = MergeTree ORDER BY tuple()"); err != nil {
		// Without CREATE TABLE rows go straight in, chunk by chunk.
		imp.staging = ""
	}
	return imp, nil
}

type importer struct {
	conn    *conn
	sc      *sql.Conn
	target  string
	staging string
	cols    []string
	types   []chtype.Type
	empty   bool
	rows    int64
	// Rows are sent in large inserts: every insert makes a part on disk,
	// and ClickHouse is happiest with few, big ones.
	buf     strings.Builder
	pending int
}

const (
	flushBytes = 16 << 20
	flushRows  = 200_000
)

func (im *importer) into() string {
	if im.staging != "" {
		return im.staging
	}
	return im.target
}

func (im *importer) Insert(ctx context.Context, rows [][]any) error {
	if len(rows) == 0 {
		return nil
	}
	if im.staging == "" && im.empty {
		if _, err := im.sc.ExecContext(ctx, "TRUNCATE TABLE "+im.target); err != nil {
			return mapError(err, sqlsplit.Statement{})
		}
		im.empty = false
	}
	b := &im.buf
	for i, row := range rows {
		if im.pending+i > 0 {
			b.WriteString(", ")
		}
		b.WriteByte('(')
		for j, v := range row {
			if j > 0 {
				b.WriteString(", ")
			}
			if j >= len(im.types) {
				return fmt.Errorf("row %d has more values than columns", im.rows+int64(im.pending+i)+1)
			}
			lit, err := chtype.Literal(im.types[j], v, false)
			if err != nil {
				return fmt.Errorf("row %d, column %s: %w", im.rows+int64(im.pending+i)+1, strings.Trim(im.cols[j], "`"), err)
			}
			b.WriteString(lit)
		}
		b.WriteByte(')')
	}
	im.pending += len(rows)
	if b.Len() >= flushBytes || im.pending >= flushRows {
		return im.flush(ctx)
	}
	return nil
}

// flush sends the rows gathered so far.
func (im *importer) flush(ctx context.Context) error {
	if im.pending == 0 {
		return nil
	}
	q := "INSERT INTO " + im.into() + " (" + strings.Join(im.cols, ", ") + ") VALUES " + im.buf.String()
	n := im.pending
	im.buf.Reset()
	im.pending = 0
	// Times with an offset (2025-01-02 03:04:05+03:00) are read as the instant they name.
	ictx := ch.Context(ctx, ch.WithSettings(ch.Settings{"date_time_input_format": "best_effort"}))
	if _, err := im.sc.ExecContext(ictx, q); err != nil {
		return mapError(err, sqlsplit.Statement{})
	}
	im.rows += int64(n)
	return nil
}

func (im *importer) Commit() error {
	defer im.close()
	ctx := context.Background()
	if err := im.flush(ctx); err != nil {
		return err
	}
	if im.staging == "" {
		return nil
	}
	if im.empty {
		if _, err := im.sc.ExecContext(ctx, "TRUNCATE TABLE "+im.target); err != nil {
			return mapError(err, sqlsplit.Statement{})
		}
	}
	cols := strings.Join(im.cols, ", ")
	_, err := im.sc.ExecContext(ctx, "INSERT INTO "+im.target+" ("+cols+") SELECT "+cols+" FROM "+im.staging)
	return mapError(err, sqlsplit.Statement{})
}

func (im *importer) Rollback() error {
	im.close()
	return nil
}

func (im *importer) close() {
	if im.sc == nil {
		return
	}
	if im.staging != "" {
		_, _ = im.sc.ExecContext(context.Background(), "DROP TABLE IF EXISTS "+im.staging)
	}
	_ = im.sc.Raw(func(any) error { return sqldriver.ErrBadConn })
	im.sc.Close()
	im.sc = nil
}
