package sqlite

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"

	"rowsmith/internal/driver"
	"rowsmith/internal/driver/sqlbase"
)

func (c *conn) Describe(ctx context.Context, ref driver.ObjectRef) (*driver.Table, error) {
	if ref.Name == "" {
		return nil, errors.New("table name is required")
	}
	var kind, name, ddl string
	err := c.db.QueryRowContext(ctx, `SELECT type, name, COALESCE(sql, '') FROM sqlite_schema
		WHERE type IN ('table', 'view') AND name = ? COLLATE NOCASE`, ref.Name).Scan(&kind, &name, &ddl)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("table %s not found", ref.Name)
	}
	if err != nil {
		return nil, err
	}
	// SQLite names are case-insensitive; continue with the stored spelling.
	t := &driver.Table{Ref: driver.ObjectRef{Name: name, Kind: kind}, Kind: kind, Options: map[string]string{}}
	def := parseTable(ddl)
	if kind == "table" {
		setFlag(t.Options, "without_rowid", def.withoutRowid)
		setFlag(t.Options, "strict", def.strict)
		if def.module != "" {
			t.Options["virtual"] = def.module
		}
	}
	if err := c.describeColumns(ctx, t, def); err != nil {
		return nil, err
	}
	if kind == "table" {
		if err := c.describeIndexes(ctx, t); err != nil {
			return nil, err
		}
		if t.ForeignKeys, err = c.fks(ctx, "m.name = ?", name); err != nil {
			return nil, err
		}
		if t.Referenced, err = c.fks(ctx, `f."table" = ? COLLATE NOCASE`, name); err != nil {
			return nil, err
		}
		t.Checks = def.checks
		sqlbase.ChooseRowKey(t, !def.withoutRowid)
	}
	if t.Triggers, err = c.triggers(ctx, name); err != nil {
		return nil, err
	}
	if kind == "view" {
		t.Definition = ddl
	}
	if t.DDL, err = c.schemaSQL(ctx, name); err != nil {
		return nil, err
	}
	return t, nil
}

func setFlag(m map[string]string, k string, on bool) {
	if on {
		m[k] = "true"
	}
}

func (c *conn) describeColumns(ctx context.Context, t *driver.Table, def tableDef) error {
	rows, err := c.db.QueryContext(ctx, `SELECT name, type, "notnull", dflt_value, pk, hidden
		FROM pragma_table_xinfo(?, 'main') ORDER BY cid`, t.Ref.Name)
	if err != nil {
		return err
	}
	defer rows.Close()
	pkPos := map[string]int{}
	for rows.Next() {
		var col driver.Column
		var notNull bool
		var dflt sql.NullString
		var pk, hidden int
		if err := rows.Scan(&col.Name, &col.Type, &notNull, &dflt, &pk, &hidden); err != nil {
			return err
		}
		if hidden == 1 {
			continue // hidden columns of virtual tables are not part of the row
		}
		col.BaseType = strings.ToLower(strings.TrimSpace(strings.SplitN(col.Type, "(", 2)[0]))
		col.Kind = kindOf(col.Type)
		col.Nullable = !notNull
		col.Default = sqlbase.NullStr(dflt)
		col.PrimaryKey = pk > 0
		col.Collation = def.collations[col.Name]
		if hidden == 2 || hidden == 3 { // generated VIRTUAL / STORED
			col.Generated = cmp.Or(def.generated[col.Name], "generated")
			col.GeneratedStored = hidden == 3
		}
		if pk > 0 {
			pkPos[col.Name] = pk
			t.PrimaryKey = append(t.PrimaryKey, col.Name)
		}
		t.Columns = append(t.Columns, col)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	sort.SliceStable(t.PrimaryKey, func(i, j int) bool { return pkPos[t.PrimaryKey[i]] < pkPos[t.PrimaryKey[j]] })
	// A single INTEGER PRIMARY KEY aliases the rowid, which SQLite assigns.
	if len(t.PrimaryKey) == 1 && t.Options["without_rowid"] == "" && t.Options["virtual"] == "" {
		for i := range t.Columns {
			if t.Columns[i].Name == t.PrimaryKey[0] && strings.EqualFold(t.Columns[i].Type, "INTEGER") {
				t.Columns[i].AutoIncrement = true
			}
		}
	}
	return nil
}

func (c *conn) describeIndexes(ctx context.Context, t *driver.Table) error {
	rows, err := c.db.QueryContext(ctx, `SELECT il.name, il."unique", il.origin = 'pk', COALESCE(m.sql, '')
		FROM pragma_index_list(?, 'main') il LEFT JOIN sqlite_schema m ON m.type = 'index' AND m.name = il.name
		ORDER BY il.origin = 'pk' DESC, il.name`, t.Ref.Name)
	if err != nil {
		return err
	}
	for rows.Next() {
		var ix driver.Index
		if err := rows.Scan(&ix.Name, &ix.Unique, &ix.Primary, &ix.Definition); err != nil {
			rows.Close()
			return err
		}
		t.Indexes = append(t.Indexes, ix)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range t.Indexes {
		ix := &t.Indexes[i]
		keys, where := parseIndex(ix.Definition)
		ix.Where = where
		cols, err := c.db.QueryContext(ctx, `SELECT cid, COALESCE(name, ''), "desc" FROM pragma_index_xinfo(?, 'main')
			WHERE key = 1 ORDER BY seqno`, ix.Name)
		if err != nil {
			return err
		}
		for n := 0; cols.Next(); n++ {
			var cid int
			var name string
			var desc bool
			if err := cols.Scan(&cid, &name, &desc); err != nil {
				cols.Close()
				return err
			}
			if cid == -2 { // expression key
				name = "(expression)"
				if n < len(keys) {
					name = keys[n]
				}
			}
			ix.Columns = append(ix.Columns, name)
			ix.Desc = append(ix.Desc, desc)
		}
		cols.Close()
		if err := cols.Err(); err != nil {
			return err
		}
	}
	return nil
}

// fkSelect lists foreign keys of every table; callers append a condition on
// m (the child table) or f (the key).
const fkSelect = `SELECT m.name, f.id, COALESCE((SELECT p.name FROM sqlite_schema p WHERE p.type = 'table' AND p.name = f."table" COLLATE NOCASE), f."table"),
	f."from", f."to", f.on_update, f.on_delete
	FROM sqlite_schema m JOIN pragma_foreign_key_list(m.name, 'main') f
	WHERE m.type = 'table' AND `

// fks returns foreign keys matching cond. SQLite does not report constraint
// names, so each key is named after its table and position.
func (c *conn) fks(ctx context.Context, cond string, args ...any) ([]driver.ForeignKey, error) {
	rows, err := c.db.QueryContext(ctx, fkSelect+cond+" ORDER BY m.name, f.id, f.seq", args...)
	if err != nil {
		return nil, err
	}
	var out []driver.ForeignKey
	for rows.Next() {
		var table, parent, from, onUpdate, onDelete string
		var to sql.NullString
		var id int
		if err := rows.Scan(&table, &id, &parent, &from, &to, &onUpdate, &onDelete); err != nil {
			rows.Close()
			return nil, err
		}
		name := fmt.Sprintf("fk_%s_%d", table, id)
		if n := len(out); n == 0 || out[n-1].Name != name || out[n-1].Table.Name != table {
			out = append(out, driver.ForeignKey{Name: name, RefTable: driver.ObjectRef{Name: parent, Kind: "table"},
				OnUpdate: onUpdate, OnDelete: onDelete, Table: &driver.ObjectRef{Name: table, Kind: "table"}})
		}
		fk := &out[len(out)-1]
		fk.Columns = append(fk.Columns, from)
		if to.Valid {
			fk.RefColumns = append(fk.RefColumns, to.String)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		if len(out[i].RefColumns) == 0 {
			// REFERENCES parent without a column list targets the parent's primary key.
			if out[i].RefColumns, err = sqlbase.Strings(ctx, c.db, `SELECT name FROM pragma_table_info(?, 'main') WHERE pk > 0 ORDER BY pk`,
				out[i].RefTable.Name); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

func (c *conn) triggers(ctx context.Context, table string) ([]driver.Trigger, error) {
	rows, err := c.db.QueryContext(ctx, `SELECT name, sql FROM sqlite_schema WHERE type = 'trigger' AND tbl_name = ? COLLATE NOCASE ORDER BY name`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []driver.Trigger
	for rows.Next() {
		var name, ddl string
		if err := rows.Scan(&name, &ddl); err != nil {
			return nil, err
		}
		tr := parseTrigger(ddl)
		tr.Name = name
		out = append(out, tr)
	}
	return out, rows.Err()
}

// schemaSQL returns the statements defining a table or view followed by its
// indexes and triggers.
func (c *conn) schemaSQL(ctx context.Context, name string) (string, error) {
	stmts, err := sqlbase.Strings(ctx, c.db, `SELECT sql FROM sqlite_schema WHERE tbl_name = ? COLLATE NOCASE AND sql IS NOT NULL
		ORDER BY CASE type WHEN 'index' THEN 1 WHEN 'trigger' THEN 2 ELSE 0 END, name`, name)
	if err != nil || len(stmts) == 0 {
		return "", err
	}
	return strings.Join(stmts, ";\n\n") + ";", nil
}

// Definition implements driver.Definer.
func (c *conn) Definition(ctx context.Context, ref driver.ObjectRef) (string, error) {
	q, args := `SELECT type, name, COALESCE(sql, '') FROM sqlite_schema WHERE name = ? COLLATE NOCASE`, []any{ref.Name}
	if ref.Kind != "" {
		q, args = q+" AND type = ?", append(args, ref.Kind)
	}
	var kind, name, ddl string
	err := c.db.QueryRowContext(ctx, q, args...).Scan(&kind, &name, &ddl)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("%s not found", ref.Name)
	}
	if err != nil {
		return "", err
	}
	switch {
	case kind == "table" || kind == "view":
		return c.schemaSQL(ctx, name)
	case ddl == "":
		return "", fmt.Errorf("%s was created implicitly by a constraint and has no definition", name)
	}
	return ddl + ";", nil
}

// CatalogColumns implements driver.Catalog. Columns are read per object so
// that one broken view (e.g. over a dropped table) does not hide the rest.
func (c *conn) CatalogColumns(ctx context.Context, _ driver.Scope) ([]driver.CatalogTable, error) {
	rows, err := c.db.QueryContext(ctx, `SELECT name, type FROM sqlite_schema
		WHERE type IN ('table', 'view') AND name NOT LIKE 'sqlite\_%' ESCAPE '\' ORDER BY name`)
	if err != nil {
		return nil, err
	}
	var out []driver.CatalogTable
	for rows.Next() {
		var ct driver.CatalogTable
		if err := rows.Scan(&ct.Name, &ct.Kind); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, ct)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	idx := map[string]int{}
	for i := range out {
		idx[out[i].Name] = i
		cols, err := c.db.QueryContext(ctx, `SELECT name, type, pk FROM pragma_table_xinfo(?, 'main') WHERE hidden <> 1 ORDER BY cid`, out[i].Name)
		if err != nil {
			continue
		}
		for cols.Next() {
			var cc driver.CatalogColumn
			var pk int
			if cols.Scan(&cc.Name, &cc.Type, &pk) == nil {
				cc.PK = pk > 0
				out[i].Columns = append(out[i].Columns, cc)
			}
		}
		cols.Close()
	}
	fks, err := c.fks(ctx, "1")
	if err != nil {
		return nil, err
	}
	for _, fk := range fks {
		if i, ok := idx[fk.Table.Name]; ok {
			out[i].FKs = append(out[i].FKs, fk)
		}
	}
	return out, nil
}
