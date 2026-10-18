package clickhouse

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"rowsmith/internal/driver"
	"rowsmith/internal/driver/clickhouse/chtype"
	"rowsmith/internal/sqlsplit"
)

func (c *conn) Describe(ctx context.Context, ref driver.ObjectRef) (*driver.Table, error) {
	if ref.Database == "" {
		ref.Database = c.defaultDB
	}
	if ref.Database == "" {
		if err := c.db.QueryRowContext(ctx, "SELECT currentDatabase()").Scan(&ref.Database); err != nil {
			return nil, mapError(err, sqlsplit.Statement{})
		}
	}
	var engine, engineFull, sortingKey, primaryKey, partitionKey, samplingKey, comment string
	var nrows, size *uint64
	err := c.db.QueryRowContext(ctx, `SELECT engine, engine_full, sorting_key, primary_key, partition_key, sampling_key, comment, total_rows, total_bytes
		FROM system.tables WHERE database = ? AND name = ?`, ref.Database, ref.Name).
		Scan(&engine, &engineFull, &sortingKey, &primaryKey, &partitionKey, &samplingKey, &comment, &nrows, &size)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("table %s.%s not found", ref.Database, ref.Name)
	}
	if err != nil {
		return nil, mapError(err, sqlsplit.Statement{})
	}
	kind := objectKind(engine)
	t := &driver.Table{Ref: driver.ObjectRef{Database: ref.Database, Name: ref.Name, Kind: kind}, Kind: kind, Comment: comment, Options: map[string]string{}}
	if nrows != nil {
		n := int64(*nrows)
		t.RowEstimate = &n
	}
	if size != nil {
		n := int64(*size)
		t.Size = &n
	}

	rows, err := c.db.QueryContext(ctx, `SELECT name, type, default_kind, default_expression, comment, is_in_primary_key, compression_codec
		FROM system.columns WHERE database = ? AND table = ? ORDER BY position`, ref.Database, ref.Name)
	if err != nil {
		return nil, mapError(err, sqlsplit.Statement{})
	}
	for rows.Next() {
		var name, typ, defKind, defExpr, comment, codec string
		var inPK uint8
		if err := rows.Scan(&name, &typ, &defKind, &defExpr, &comment, &inPK, &codec); err != nil {
			rows.Close()
			return nil, err
		}
		col := columnOf(name, typ, defKind, defExpr, comment)
		col.PrimaryKey, col.Codec = inPK == 1, codec
		t.Columns = append(t.Columns, col)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// The primary key is a prefix of the sort order; it orders and indexes
	// rows but doesn't make them unique.
	for _, e := range splitKey(primaryKey) {
		if hasColumn(t, e) {
			t.PrimaryKey = append(t.PrimaryKey, e)
		}
	}
	if sortingKey != "" {
		t.Indexes = append(t.Indexes, driver.Index{Name: "sorting key", Columns: splitKey(sortingKey), Primary: true, Type: "order by",
			Comment: "Rows are stored in this order; the primary key is its first " + plural(len(splitKey(primaryKey)), "column") + "."})
	}
	if ix, err := c.db.QueryContext(ctx, `SELECT name, type_full, expr, granularity FROM system.data_skipping_indices
		WHERE database = ? AND table = ? ORDER BY name`, ref.Database, ref.Name); err == nil {
		for ix.Next() {
			var name, typ, expr string
			var gran uint64
			if ix.Scan(&name, &typ, &expr, &gran) == nil {
				t.Indexes = append(t.Indexes, driver.Index{Name: name, Columns: []string{expr}, Type: typ,
					Definition: fmt.Sprintf("GRANULARITY %d", gran)})
			}
		}
		ix.Close()
	}

	if kind == "table" {
		parts := engineClauses(engineFull)
		t.Options["engine"] = parts["ENGINE"]
		for k, v := range map[string]string{"order_by": sortingKey, "partition_by": partitionKey, "sample_by": samplingKey,
			"ttl": parts["TTL"], "settings": parts["SETTINGS"]} {
			if v != "" {
				t.Options[k] = v
			}
		}
		if primaryKey != "" && primaryKey != sortingKey {
			t.Options["primary_key"] = primaryKey
		}
		if partitionKey != "" {
			c.partitions(ctx, t)
		}
	}
	var ddl string
	if err := c.db.QueryRowContext(ctx, "SHOW CREATE TABLE "+qualify(ref.Database, ref.Name)).Scan(&ddl); err == nil {
		t.DDL = ddl
		if kind != "table" {
			t.Definition = ddl
		}
	}
	// Rows of MergeTree tables are changed with mutations, matched on every
	// value; other engines (logs, external tables, views) can't change rows.
	if kind == "table" && strings.Contains(engine, "MergeTree") {
		t.RowKeyKind, t.Editable = "all", true
	}
	return t, nil
}

// partitions notes how many partitions a table has and their range.
func (c *conn) partitions(ctx context.Context, t *driver.Table) {
	var n uint64
	var first, last string
	if c.db.QueryRowContext(ctx, `SELECT uniqExact(partition), min(partition), max(partition) FROM system.parts
		WHERE database = ? AND table = ? AND active`, t.Ref.Database, t.Ref.Name).Scan(&n, &first, &last) == nil && n > 0 {
		t.Options["partitions"] = fmt.Sprintf("%d (%s … %s)", n, first, last)
	}
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

func hasColumn(t *driver.Table, name string) bool {
	for _, c := range t.Columns {
		if c.Name == name {
			return true
		}
	}
	return false
}

// splitKey splits a key expression list such as "event_type, toDate(ts)".
func splitKey(k string) []string {
	k = strings.TrimSpace(k)
	if k == "" {
		return nil
	}
	var out []string
	depth, start := 0, 0
	for i := 0; i < len(k); i++ {
		switch k[i] {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, strings.Trim(strings.TrimSpace(k[start:i]), "`"))
				start = i + 1
			}
		}
	}
	return append(out, strings.Trim(strings.TrimSpace(k[start:]), "`"))
}

// columnOf builds a column from system.columns.
func columnOf(name, typ, defKind, defExpr, comment string) driver.Column {
	t := typeOf(typ)
	inner, nullable := t.Unwrap()
	col := driver.Column{Name: name, Type: chtype.StripNullable(typ), BaseType: strings.ToLower(inner.Name), Kind: kindOf(t),
		Nullable: nullable, Comment: comment}
	switch defKind {
	case "DEFAULT":
		col.Default = &defExpr
	case "MATERIALIZED":
		col.Generated, col.GeneratedStored = defExpr, true
	case "ALIAS":
		col.Generated = defExpr
	case "EPHEMERAL":
		if defExpr != "" {
			col.Default = &defExpr
		}
		col.Comment = strings.TrimSpace("ephemeral: not stored. " + comment)
	}
	if col.Kind == driver.KindEnum {
		col.Enum = inner.EnumValues()
	}
	if inner.IsDecimal() {
		p, s := inner.DecimalPS()
		pp, ss := int64(p), int64(s)
		col.Precision, col.Scale = &pp, &ss
	}
	if inner.Name == "FixedString" && len(inner.Params) == 1 {
		var n int64
		fmt.Sscan(inner.Params[0], &n)
		col.Length = &n
	}
	if g := inner.GeoShape(); g != "" {
		col.GeometryType = g
	}
	if _, unsigned, ok := inner.IntBits(); ok {
		col.Unsigned = unsigned
	}
	return col
}

var clauseRe = regexp.MustCompile(`\s(PARTITION BY|PRIMARY KEY|ORDER BY|SAMPLE BY|TTL|SETTINGS|COMMENT)\s`)

// engineClauses splits engine_full ("MergeTree PARTITION BY … ORDER BY …
// TTL … SETTINGS …") into its clauses; ENGINE is the engine with its arguments.
func engineClauses(full string) map[string]string {
	out := map[string]string{}
	full = " " + strings.TrimSpace(full) + " "
	locs := clauseRe.FindAllStringSubmatchIndex(full, -1)
	// Keywords inside parentheses or quotes belong to an expression.
	var top [][]int
	for _, l := range locs {
		if depthAt(full, l[0]) == 0 {
			top = append(top, l)
		}
	}
	end := len(full)
	if len(top) > 0 {
		end = top[0][0]
	}
	out["ENGINE"] = strings.TrimSpace(full[:end])
	for i, l := range top {
		stop := len(full)
		if i+1 < len(top) {
			stop = top[i+1][0]
		}
		out[full[l[2]:l[3]]] = strings.TrimSpace(full[l[1]:stop])
	}
	return out
}

// depthAt is the bracket depth at position i, outside quotes.
func depthAt(s string, i int) int {
	depth := 0
	var quote byte
	for j := 0; j < i && j < len(s); j++ {
		c := s[j]
		switch {
		case quote != 0:
			if c == '\\' {
				j++
			} else if c == quote {
				quote = 0
			}
		case c == '\'' || c == '`' || c == '"':
			quote = c
		case c == '(':
			depth++
		case c == ')':
			depth--
		}
	}
	if quote != 0 {
		return 1
	}
	return depth
}

func (c *conn) Definition(ctx context.Context, ref driver.ObjectRef) (string, error) {
	var ddl string
	stmt := "SHOW CREATE TABLE "
	if ref.Kind == "dictionary" {
		stmt = "SHOW CREATE DICTIONARY "
	}
	if err := c.db.QueryRowContext(ctx, stmt+qualify(ref.Database, ref.Name)).Scan(&ddl); err != nil {
		return "", mapError(err, sqlsplit.Statement{})
	}
	return ddl, nil
}

func (c *conn) CatalogColumns(ctx context.Context, s driver.Scope) ([]driver.CatalogTable, error) {
	if s.Database == "" {
		return nil, nil
	}
	rows, err := c.db.QueryContext(ctx, `SELECT t.name, t.engine, c.name, c.type, c.is_in_primary_key
		FROM system.tables t JOIN system.columns c ON c.database = t.database AND c.table = t.name
		WHERE t.database = ? ORDER BY t.name, c.position`, s.Database)
	if err != nil {
		return nil, mapError(err, sqlsplit.Statement{})
	}
	defer rows.Close()
	var out []driver.CatalogTable
	for rows.Next() {
		var table, engine, col, typ string
		var pk uint8
		if err := rows.Scan(&table, &engine, &col, &typ, &pk); err != nil {
			return nil, err
		}
		if len(out) == 0 || out[len(out)-1].Name != table {
			out = append(out, driver.CatalogTable{Name: table, Kind: objectKind(engine)})
		}
		last := &out[len(out)-1]
		last.Columns = append(last.Columns, driver.CatalogColumn{Name: col, Type: typ, PK: pk == 1})
	}
	return out, rows.Err()
}
