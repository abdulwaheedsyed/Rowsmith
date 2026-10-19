package snowflake

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"rowsmith/internal/driver"
	"rowsmith/internal/driver/sqlbase"
	"rowsmith/internal/sqlsplit"
)

// sfType is a column's data_type as SHOW COLUMNS reports it, such as
// {"type":"FIXED","precision":38,"scale":0,"nullable":false}.
type sfType struct {
	Type      string `json:"type"`
	Precision *int64 `json:"precision"`
	Scale     *int64 `json:"scale"`
	Length    *int64 `json:"length"`
	Nullable  bool   `json:"nullable"`
	Fixed     bool   `json:"fixed"`
	// VECTOR(type, dimension)
	ElementType string `json:"vectorElementType"`
	Dimension   int64  `json:"vectorDimension"`
}

const (
	maxText   = 16777216 // VARCHAR without a length
	maxBinary = 8388608  // BINARY without a length
)

// column turns SHOW COLUMNS' description into a column with its declared
// type, as GET_DDL writes it.
func column(name string, t sfType) driver.Column {
	c := driver.Column{Name: name, Nullable: t.Nullable, Precision: t.Precision, Scale: t.Scale, Length: t.Length}
	switch t.Type {
	case "FIXED":
		p, s := int64(38), int64(0)
		if t.Precision != nil {
			p = *t.Precision
		}
		if t.Scale != nil {
			s = *t.Scale
		}
		c.Type, c.BaseType = fmt.Sprintf("NUMBER(%d,%d)", p, s), "number"
		c.Kind = driver.KindDecimal
		if s == 0 {
			c.Kind = driver.KindInt
		}
	case "REAL":
		c.Type, c.BaseType, c.Kind = "FLOAT", "float", driver.KindFloat
	case "TEXT":
		c.BaseType, c.Kind = "varchar", driver.KindString
		if t.Length == nil || *t.Length >= maxText {
			c.Type, c.Kind = "VARCHAR", driver.KindText
		} else {
			c.Type = fmt.Sprintf("VARCHAR(%d)", *t.Length)
			if *t.Length > 255 {
				c.Kind = driver.KindText
			}
		}
	case "BOOLEAN":
		c.Type, c.BaseType, c.Kind = "BOOLEAN", "boolean", driver.KindBool
	case "BINARY":
		c.Type, c.BaseType, c.Kind = "BINARY", "binary", driver.KindBinary
		if t.Length != nil && *t.Length < maxBinary {
			c.Type = fmt.Sprintf("BINARY(%d)", *t.Length)
		}
	case "DATE":
		c.Type, c.BaseType, c.Kind = "DATE", "date", driver.KindDate
	case "TIME", "TIMESTAMP_NTZ", "TIMESTAMP_TZ", "TIMESTAMP_LTZ":
		c.BaseType = strings.ToLower(t.Type)
		c.Type = t.Type
		if t.Scale != nil && *t.Scale != 9 {
			c.Type = fmt.Sprintf("%s(%d)", t.Type, *t.Scale)
		}
		c.Kind = map[string]driver.ValueKind{"TIME": driver.KindTime, "TIMESTAMP_NTZ": driver.KindDateTime}[t.Type]
		if c.Kind == "" {
			c.Kind = driver.KindTimestamp
		}
	case "VARIANT", "OBJECT", "ARRAY", "MAP":
		c.Type, c.BaseType, c.Kind = t.Type, strings.ToLower(t.Type), driver.KindJSON
	case "GEOGRAPHY":
		c.Type, c.BaseType, c.Kind, c.SRID = "GEOGRAPHY", "geography", driver.KindGeometry, 4326
	case "GEOMETRY":
		c.Type, c.BaseType, c.Kind = "GEOMETRY", "geometry", driver.KindGeometry
	case "VECTOR":
		c.Type, c.BaseType, c.Kind = fmt.Sprintf("VECTOR(%s, %d)", t.ElementType, t.Dimension), "vector", driver.KindArray
	default:
		c.Type, c.BaseType, c.Kind = t.Type, strings.ToLower(t.Type), driver.KindOther
	}
	return c
}

// object finds one object in a schema with SHOW OBJECTS, matching the name
// exactly (LIKE is case-insensitive and treats _ and % as wildcards).
func (c *conn) object(ctx context.Context, ref driver.ObjectRef) (showRow, error) {
	rows, err := c.show(ctx, "SHOW OBJECTS LIKE "+literal(likeEscape(ref.Name))+" IN SCHEMA "+quote(ref.Database)+"."+quote(ref.Schema))
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		if r.str("name") == ref.Name {
			return r, nil
		}
	}
	return nil, fmt.Errorf("%s.%s.%s not found", ref.Database, ref.Schema, ref.Name)
}

// likeEscape escapes LIKE wildcards; SHOW … LIKE takes \ as its escape.
func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `_`, `\_`, `%`, `\%`).Replace(s)
}

func (c *conn) Describe(ctx context.Context, ref driver.ObjectRef) (*driver.Table, error) {
	if ref.Database == "" || ref.Schema == "" {
		return nil, fmt.Errorf("%s: choose a database and schema", ref.Name)
	}
	o, err := c.object(ctx, ref)
	if err != nil {
		return nil, err
	}
	t := &driver.Table{Ref: ref, Options: map[string]string{}, Indexes: []driver.Index{}, ForeignKeys: []driver.ForeignKey{},
		Referenced: []driver.ForeignKey{}, Checks: []driver.Check{}, Triggers: []driver.Trigger{}, Comment: o.str("comment")}
	kw := "TABLE"
	switch kind := o.str("kind"); {
	case kind == "VIEW" && o.str("is_materialized") == "true", kind == "MATERIALIZED_VIEW":
		t.Kind, kw = "materialized_view", "VIEW"
	case kind == "VIEW":
		t.Kind, kw = "view", "VIEW"
	case o.str("is_dynamic") == "Y":
		t.Kind, kw = "dynamic_table", "DYNAMIC TABLE"
	case o.str("is_external") == "Y":
		t.Kind, kw = "external_table", "EXTERNAL TABLE"
	default:
		t.Kind = "table"
		if kind == "TRANSIENT" {
			t.Options["transient"] = "true"
		}
	}
	t.Ref.Kind = t.Kind
	if t.Kind != "view" {
		t.RowEstimate, t.Size = o.int("rows"), o.int("bytes")
	}
	if cb := o.str("cluster_by"); cb != "" {
		t.Options["cluster_by"] = strings.TrimSuffix(strings.TrimPrefix(cb, "LINEAR("), ")")
	}
	if rt := o.str("retention_time"); rt != "" {
		t.Options["retention_days"] = rt
	}
	if owner := o.str("owner"); owner != "" {
		t.Options["owner"] = owner
	}

	in := " IN " + map[bool]string{true: "VIEW", false: "TABLE"}[kw == "VIEW"] + " " + qualify(ref)
	cols, err := c.show(ctx, "SHOW COLUMNS"+in)
	if err != nil {
		return nil, err
	}
	for _, r := range cols {
		var st sfType
		_ = json.Unmarshal([]byte(r.str("data_type")), &st)
		col := column(r.str("column_name"), st)
		col.Nullable = r.str("null?") == "true"
		col.Comment = r.str("comment")
		if d := r.str("default"); d != "" {
			col.Default = &d
		}
		if ai := r.str("autoincrement"); ai != "" {
			col.AutoIncrement = true
		}
		if r.str("kind") == "VIRTUAL" {
			col.Generated = r.str("expression")
		}
		t.Columns = append(t.Columns, col)
	}

	if t.Kind == "table" || t.Kind == "dynamic_table" {
		tbl := " IN TABLE " + qualify(ref)
		if rs, err := c.show(ctx, "SHOW PRIMARY KEYS"+tbl); err == nil {
			t.PrimaryKey = keyColumns(rs)
			for i := range t.Columns {
				for _, k := range t.PrimaryKey {
					if t.Columns[i].Name == k {
						t.Columns[i].PrimaryKey = true
					}
				}
			}
			if len(rs) > 0 {
				t.Indexes = append(t.Indexes, driver.Index{Name: rs[0].str("constraint_name"), Columns: t.PrimaryKey, Unique: true, Primary: true})
			}
		}
		if rs, err := c.show(ctx, "SHOW UNIQUE KEYS"+tbl); err == nil {
			byName := map[string][]showRow{}
			var names []string
			for _, r := range rs {
				n := r.str("constraint_name")
				if _, ok := byName[n]; !ok {
					names = append(names, n)
				}
				byName[n] = append(byName[n], r)
			}
			for _, n := range names {
				t.Indexes = append(t.Indexes, driver.Index{Name: n, Columns: keyColumns(byName[n]), Unique: true})
			}
		}
		if rs, err := c.show(ctx, "SHOW IMPORTED KEYS"+tbl); err == nil {
			t.ForeignKeys = foreignKeys(rs, false)
		}
		if rs, err := c.show(ctx, "SHOW EXPORTED KEYS"+tbl); err == nil {
			t.Referenced = foreignKeys(rs, true)
		}
		sqlbase.ChooseRowKey(t, false)
	}
	var ddl string
	if err := c.db.QueryRowContext(ctx, "SELECT GET_DDL(?, ?)", kw, qualify(ref)).Scan(&ddl); err == nil {
		t.DDL = strings.TrimSpace(ddl)
		if t.Kind == "view" || t.Kind == "materialized_view" {
			t.Definition = t.DDL
		}
	}
	return t, nil
}

// keyColumns orders a key's columns by key_sequence.
func keyColumns(rs []showRow) []string {
	sort.SliceStable(rs, func(i, j int) bool { return *seq(rs[i]) < *seq(rs[j]) })
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.str("column_name")
	}
	return out
}

func seq(r showRow) *int64 {
	if n := r.int("key_sequence"); n != nil {
		return n
	}
	zero := int64(0)
	return &zero
}

// foreignKeys groups SHOW IMPORTED/EXPORTED KEYS rows by constraint.
func foreignKeys(rs []showRow, incoming bool) []driver.ForeignKey {
	sort.SliceStable(rs, func(i, j int) bool { return *seq(rs[i]) < *seq(rs[j]) })
	var out []driver.ForeignKey
	idx := map[string]int{}
	for _, r := range rs {
		name := r.str("fk_name")
		i, ok := idx[name]
		if !ok {
			fk := driver.ForeignKey{Name: name, OnUpdate: r.str("update_rule"), OnDelete: r.str("delete_rule"),
				RefTable: driver.ObjectRef{Database: r.str("pk_database_name"), Schema: r.str("pk_schema_name"), Name: r.str("pk_table_name"), Kind: "table"}}
			if incoming {
				fk.Table = &driver.ObjectRef{Database: r.str("fk_database_name"), Schema: r.str("fk_schema_name"), Name: r.str("fk_table_name"), Kind: "table"}
			}
			i = len(out)
			idx[name] = i
			out = append(out, fk)
		}
		out[i].Columns = append(out[i].Columns, r.str("fk_column_name"))
		out[i].RefColumns = append(out[i].RefColumns, r.str("pk_column_name"))
	}
	if out == nil {
		out = []driver.ForeignKey{}
	}
	return out
}

// Definition implements driver.Definer: GET_DDL for views, sequences,
// functions and procedures (every overload of a routine's name).
func (c *conn) Definition(ctx context.Context, ref driver.ObjectRef) (string, error) {
	switch ref.Kind {
	case "function", "procedure":
		rs, err := c.show(ctx, "SHOW USER "+strings.ToUpper(ref.Kind)+"S LIKE "+literal(likeEscape(ref.Name))+" IN SCHEMA "+quote(ref.Database)+"."+quote(ref.Schema))
		if err != nil {
			return "", err
		}
		var parts []string
		for _, r := range rs {
			if r.str("name") != ref.Name {
				continue
			}
			sig := r.str("arguments")
			if i := strings.LastIndex(sig, ") RETURN"); i > 0 {
				sig = sig[:i+1]
			}
			// ORDER_COUNT(NUMBER) → "DB"."SCHEMA"."ORDER_COUNT"(NUMBER)
			if i := strings.Index(sig, "("); i >= 0 {
				sig = qualify(ref) + sig[i:]
			}
			var ddl string
			if err := c.db.QueryRowContext(ctx, "SELECT GET_DDL(?, ?)", strings.ToUpper(ref.Kind), sig).Scan(&ddl); err != nil {
				return "", mapError(err, sqlsplit.Statement{})
			}
			parts = append(parts, strings.TrimSpace(ddl))
		}
		if len(parts) == 0 {
			return "", fmt.Errorf("%s %s not found", ref.Kind, ref.Name)
		}
		return strings.Join(parts, "\n\n"), nil
	}
	kw := map[string]string{"sequence": "SEQUENCE", "view": "VIEW", "materialized_view": "VIEW"}[ref.Kind]
	if kw == "" {
		kw = "TABLE"
	}
	var ddl string
	if err := c.db.QueryRowContext(ctx, "SELECT GET_DDL(?, ?)", kw, qualify(ref)).Scan(&ddl); err != nil {
		return "", mapError(err, sqlsplit.Statement{})
	}
	return strings.TrimSpace(ddl), nil
}

// CatalogColumns implements driver.Catalog with one SHOW COLUMNS for the
// schema, plus its keys.
func (c *conn) CatalogColumns(ctx context.Context, s driver.Scope) ([]driver.CatalogTable, error) {
	if s.Database == "" || s.Schema == "" {
		return nil, nil
	}
	in := " IN SCHEMA " + quote(s.Database) + "." + quote(s.Schema)
	cols, err := c.show(ctx, "SHOW COLUMNS"+in)
	if err != nil {
		return nil, err
	}
	pks := map[string]map[string]bool{}
	if rs, err := c.show(ctx, "SHOW PRIMARY KEYS"+in); err == nil {
		for _, r := range rs {
			if pks[r.str("table_name")] == nil {
				pks[r.str("table_name")] = map[string]bool{}
			}
			pks[r.str("table_name")][r.str("column_name")] = true
		}
	}
	fks := map[string][]driver.ForeignKey{}
	if rs, err := c.show(ctx, "SHOW IMPORTED KEYS"+in); err == nil {
		for _, fk := range foreignKeys(rs, true) {
			fks[fk.Table.Name] = append(fks[fk.Table.Name], fk)
		}
	}
	idx := map[string]int{}
	var out []driver.CatalogTable
	for _, r := range cols {
		tn := r.str("table_name")
		i, ok := idx[tn]
		if !ok {
			i = len(out)
			idx[tn] = i
			out = append(out, driver.CatalogTable{Schema: s.Schema, Name: tn, Kind: "table", FKs: fks[tn]})
		}
		var st sfType
		_ = json.Unmarshal([]byte(r.str("data_type")), &st)
		col := column(r.str("column_name"), st)
		out[i].Columns = append(out[i].Columns, driver.CatalogColumn{Name: col.Name, Type: col.Type, PK: pks[tn][col.Name]})
	}
	return out, nil
}
