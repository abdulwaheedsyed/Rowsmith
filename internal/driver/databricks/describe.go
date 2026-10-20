package databricks

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"rowsmith/internal/driver"
	"rowsmith/internal/driver/sqlbase"
	"rowsmith/internal/sqlsplit"
)

var paramsRe = regexp.MustCompile(`\((\d+)\s*,\s*(\d+)\)`)

func column(name, typ string, nullable bool) driver.Column {
	c := driver.Column{Name: name, Type: typ, Kind: kindOf(typ), Nullable: nullable}
	c.BaseType = strings.ToLower(typ)
	if i := strings.IndexAny(c.BaseType, "(<"); i > 0 {
		c.BaseType = c.BaseType[:i]
	}
	if c.BaseType == "decimal" {
		if m := paramsRe.FindStringSubmatch(typ); m != nil {
			p, _ := strconv.ParseInt(m[1], 10, 64)
			s, _ := strconv.ParseInt(m[2], 10, 64)
			c.Precision, c.Scale = &p, &s
		}
	}
	return c
}

func (c *conn) Describe(ctx context.Context, ref driver.ObjectRef) (*driver.Table, error) {
	return c.describe(ctx, ref, true)
}

// describe reads a table or view; without full it leaves out what browsing
// and editing don't use (Delta details, history, the CREATE statement).
func (c *conn) describe(ctx context.Context, ref driver.ObjectRef, full bool) (*driver.Table, error) {
	if ref.Database == "" || ref.Schema == "" {
		return nil, fmt.Errorf("%s: choose a catalog and schema", ref.Name)
	}
	t := &driver.Table{Ref: ref, Options: map[string]string{}, Indexes: []driver.Index{}, ForeignKeys: []driver.ForeignKey{},
		Referenced: []driver.ForeignKey{}, Checks: []driver.Check{}, Triggers: []driver.Trigger{}, PrimaryKey: []string{}}
	if ref.Kind == "function" || ref.Kind == "volume" {
		def, err := c.Definition(ctx, ref)
		if err != nil {
			return nil, err
		}
		t.Kind, t.Definition, t.DDL = ref.Kind, def, def
		return t, nil
	}
	cat := quote(ref.Database)
	var typ string
	var format, comment sql.NullString
	err := c.db.QueryRowContext(ctx, "SELECT table_type, data_source_format, comment FROM "+cat+".information_schema.tables WHERE table_schema = ? AND table_name = ?",
		ref.Schema, ref.Name).Scan(&typ, &format, &comment)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("%s.%s.%s not found", ref.Database, ref.Schema, ref.Name)
	}
	if err != nil {
		return nil, mapError(err, sqlsplit.Statement{})
	}
	t.Kind, t.Comment = tableKind(typ), comment.String
	t.Ref.Kind = t.Kind
	t.Options["type"] = strings.ToLower(typ)
	if format.Valid {
		t.Options["format"] = strings.ToLower(format.String)
	}

	rows, err := c.db.QueryContext(ctx, "SELECT column_name, full_data_type, is_nullable, comment, column_default FROM "+cat+
		".information_schema.columns WHERE table_schema = ? AND table_name = ? ORDER BY ordinal_position", ref.Schema, ref.Name)
	if err != nil {
		return nil, mapError(err, sqlsplit.Statement{})
	}
	for rows.Next() {
		var name, ftype, nullable string
		var cm, def sql.NullString
		if err := rows.Scan(&name, &ftype, &nullable, &cm, &def); err != nil {
			rows.Close()
			return nil, err
		}
		col := column(name, ftype, nullable != "NO")
		col.Comment = cm.String
		if def.Valid {
			d := def.String
			col.Default = &d
		}
		t.Columns = append(t.Columns, col)
	}
	rows.Close()

	// Primary keys are informational in Unity Catalog: a row key all the
	// same, as edits still check that each matches one row.
	pk, err := c.db.QueryContext(ctx, `SELECT k.column_name, k.constraint_name FROM `+cat+`.information_schema.table_constraints c
		JOIN `+cat+`.information_schema.key_column_usage k ON k.constraint_catalog = c.constraint_catalog
		AND k.constraint_schema = c.constraint_schema AND k.constraint_name = c.constraint_name
		WHERE c.table_schema = ? AND c.table_name = ? AND c.constraint_type = 'PRIMARY KEY' ORDER BY k.ordinal_position`, ref.Schema, ref.Name)
	if err == nil {
		var pkName string
		for pk.Next() {
			var col string
			if pk.Scan(&col, &pkName) == nil {
				t.PrimaryKey = append(t.PrimaryKey, col)
			}
		}
		pk.Close()
		if len(t.PrimaryKey) > 0 {
			t.Options["primary_key"] = pkName + " (informational)"
			t.Indexes = append(t.Indexes, driver.Index{Name: pkName, Columns: t.PrimaryKey, Primary: true, Unique: true})
		}
	}
	delta := t.Kind == "table" && t.Options["format"] == "delta"
	if delta {
		sqlbase.ChooseRowKey(t, false)
		t.Editable = !c.ro
	}
	if !full {
		return t, nil
	}

	if delta || t.Kind == "materialized_view" || t.Kind == "streaming_table" {
		c.deltaDetail(ctx, t)
	}
	if t.Kind == "view" {
		var def sql.NullString
		if c.db.QueryRowContext(ctx, "SELECT view_definition FROM "+cat+".information_schema.views WHERE table_schema = ? AND table_name = ?",
			ref.Schema, ref.Name).Scan(&def) == nil {
			t.Definition = def.String
		}
	}
	var ddl string
	if c.db.QueryRowContext(ctx, "SHOW CREATE TABLE "+qualify(ref)).Scan(&ddl) == nil {
		t.DDL = ddl
	}
	return t, nil
}

// deltaDetail reads DESCRIBE DETAIL (format, location, partitioning,
// clustering, files, size, properties with CHECK constraints) and the last
// version from the table's history.
func (c *conn) deltaDetail(ctx context.Context, t *driver.Table) {
	rows, err := c.db.QueryContext(ctx, "DESCRIBE DETAIL "+qualify(t.Ref))
	if err != nil {
		return
	}
	cols, _ := rows.Columns()
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	detail := map[string]any{}
	if rows.Next() && rows.Scan(ptrs...) == nil {
		for i, n := range cols {
			detail[n] = vals[i]
		}
	}
	rows.Close()
	list := func(v any) string {
		s, _ := v.(string)
		if parsed, ok := orderedJSON(s); ok {
			if arr, ok := parsed.([]any); ok {
				parts := make([]string, len(arr))
				for i, p := range arr {
					parts[i] = fmt.Sprint(p)
				}
				return strings.Join(parts, ", ")
			}
		}
		return ""
	}
	if s, ok := detail["location"].(string); ok && s != "" {
		t.Options["location"] = s
	}
	if v := list(detail["partitionColumns"]); v != "" {
		t.Options["partitioned_by"] = v
	}
	if v := list(detail["clusteringColumns"]); v != "" {
		t.Options["cluster_by"] = v
	}
	if n, ok := detail["numFiles"].(int64); ok {
		t.Options["files"] = strconv.FormatInt(n, 10)
	}
	if n, ok := detail["sizeInBytes"].(int64); ok {
		t.Size = &n
	}
	if s, ok := detail["properties"].(string); ok {
		if parsed, ok := orderedJSON(s); ok {
			if d, ok := parsed.(driver.Doc); ok {
				for _, k := range d.Keys {
					v := fmt.Sprint(d.Values[k])
					switch {
					case strings.HasPrefix(k, "delta.constraints."):
						t.Checks = append(t.Checks, driver.Check{Name: strings.TrimPrefix(k, "delta.constraints."), Expression: v})
					case k == "delta.columnMapping.mode":
						t.Options["column_mapping"] = v
					case k == "delta.enableChangeDataFeed" && v == "true":
						t.Options["change_data_feed"] = "on"
					}
				}
			}
		}
	}
	sort.Slice(t.Checks, func(i, j int) bool { return t.Checks[i].Name < t.Checks[j].Name })
	var version int64
	var ts sql.NullTime
	if c.db.QueryRowContext(ctx, "SELECT version, timestamp FROM (DESCRIBE HISTORY "+qualify(t.Ref)+" LIMIT 1)").Scan(&version, &ts) == nil {
		t.Options["version"] = strconv.FormatInt(version, 10)
		if ts.Valid {
			t.Options["last_change"] = ts.Time.UTC().Format("2006-01-02 15:04:05 UTC")
		}
	}
}

// Definition implements driver.Definer for views, functions and volumes.
func (c *conn) Definition(ctx context.Context, ref driver.ObjectRef) (string, error) {
	var q string
	switch ref.Kind {
	case "function":
		q = "DESCRIBE FUNCTION EXTENDED " + qualify(ref)
	case "volume":
		q = "DESCRIBE VOLUME " + qualify(ref)
	default:
		var ddl string
		if err := c.db.QueryRowContext(ctx, "SHOW CREATE TABLE "+qualify(ref)).Scan(&ddl); err != nil {
			return "", mapError(err, sqlsplit.Statement{})
		}
		return ddl, nil
	}
	rows, err := c.db.QueryContext(ctx, q)
	if err != nil {
		return "", mapError(err, sqlsplit.Statement{})
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	var lines []string
	for rows.Next() {
		vals := make([]sql.NullString, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if rows.Scan(ptrs...) != nil {
			continue
		}
		var parts []string
		for i, v := range vals {
			if v.Valid && v.String != "" {
				if len(cols) > 1 {
					parts = append(parts, cols[i]+": "+v.String)
				} else {
					parts = append(parts, v.String)
				}
			}
		}
		lines = append(lines, strings.Join(parts, "  "))
	}
	return strings.Join(lines, "\n"), mapError(rows.Err(), sqlsplit.Statement{})
}

// CatalogColumns implements driver.Catalog for one schema.
func (c *conn) CatalogColumns(ctx context.Context, s driver.Scope) ([]driver.CatalogTable, error) {
	if s.Database == "" || s.Schema == "" {
		return nil, nil
	}
	rows, err := c.db.QueryContext(ctx, "SELECT table_name, column_name, full_data_type FROM "+quote(s.Database)+
		".information_schema.columns WHERE table_schema = ? ORDER BY table_name, ordinal_position", s.Schema)
	if err != nil {
		return nil, mapError(err, sqlsplit.Statement{})
	}
	defer rows.Close()
	var out []driver.CatalogTable
	idx := map[string]int{}
	for rows.Next() {
		var tn, cn, typ string
		if err := rows.Scan(&tn, &cn, &typ); err != nil {
			return nil, err
		}
		i, ok := idx[tn]
		if !ok {
			i = len(out)
			idx[tn] = i
			out = append(out, driver.CatalogTable{Schema: s.Schema, Name: tn, Kind: "table"})
		}
		out[i].Columns = append(out[i].Columns, driver.CatalogColumn{Name: cn, Type: typ})
	}
	return out, mapError(rows.Err(), sqlsplit.Statement{})
}
