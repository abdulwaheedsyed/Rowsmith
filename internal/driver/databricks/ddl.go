package databricks

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"rowsmith/internal/driver"
)

var (
	_ driver.DDLGenerator  = (*conn)(nil)
	_ driver.BrowseQuerier = (*conn)(nil)
	_ driver.BulkImporter  = (*conn)(nil)
)

func colList(list string) []string {
	var out []string
	for _, p := range strings.Split(list, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func quoteList(names []string) string {
	q := make([]string, len(names))
	for i, n := range names {
		q[i] = quote(n)
	}
	return strings.Join(q, ", ")
}

func columnDef(c driver.Column, notNull bool) (string, error) {
	if strings.TrimSpace(c.Type) == "" {
		return "", fmt.Errorf("column %s has no type", c.Name)
	}
	s := quote(c.Name) + " " + c.Type
	if !c.Nullable || notNull {
		s += " NOT NULL"
	}
	if c.Default != nil && strings.TrimSpace(*c.Default) != "" {
		s += " DEFAULT " + *c.Default
	}
	if c.Comment != "" {
		s += " COMMENT " + literal(c.Comment)
	}
	return s, nil
}

const allowDefaults = "'delta.feature.allowColumnDefaults' = 'supported'"

func (c *conn) CreateTableSQL(def driver.TableDef) ([]string, error) {
	if def.Ref.Database == "" || def.Ref.Schema == "" {
		return nil, errors.New("choose a catalog and schema")
	}
	if len(def.Columns) == 0 {
		return nil, errors.New("a table needs at least one column")
	}
	if len(def.Indexes) > 0 || len(def.ForeignKeys) > 0 {
		return nil, errors.New("Delta tables have no indexes, and Rowsmith doesn't create foreign keys on them")
	}
	part, cluster := colList(def.Options["partitioned_by"]), colList(def.Options["cluster_by"])
	if len(part) > 0 && len(cluster) > 0 {
		return nil, errors.New("a Delta table is either partitioned or clustered, not both")
	}
	target := qualify(def.Ref)
	var cols []string
	defaults := false
	for _, col := range def.Columns {
		s, err := columnDef(col.Column, slices.Contains(def.PrimaryKey, col.Name))
		if err != nil {
			return nil, err
		}
		defaults = defaults || col.Default != nil && strings.TrimSpace(*col.Default) != ""
		cols = append(cols, s)
	}
	if len(def.PrimaryKey) > 0 {
		cols = append(cols, "CONSTRAINT "+quote(def.Ref.Name+"_pk")+" PRIMARY KEY ("+quoteList(def.PrimaryKey)+")")
	}
	q := "CREATE TABLE " + target + " (\n  " + strings.Join(cols, ",\n  ") + "\n) USING DELTA"
	if len(part) > 0 {
		q += "\nPARTITIONED BY (" + quoteList(part) + ")"
	}
	if len(cluster) > 0 {
		q += "\nCLUSTER BY (" + quoteList(cluster) + ")"
	}
	if loc := strings.TrimSpace(def.Options["location"]); loc != "" {
		q += "\nLOCATION " + literal(loc)
	}
	if def.Comment != "" {
		q += "\nCOMMENT " + literal(def.Comment)
	}
	if defaults {
		q += "\nTBLPROPERTIES (" + allowDefaults + ")"
	}
	out := []string{q}
	for _, ch := range def.Checks {
		out = append(out, "ALTER TABLE "+target+" ADD CONSTRAINT "+quote(ch.Name)+" CHECK ("+ch.Expression+")")
	}
	return out, nil
}

// AlterTableSQL writes one statement per change. Renaming or dropping a
// column needs column mapping, which is turned on first.
func (c *conn) AlterTableSQL(from *driver.Table, to driver.TableDef) ([]string, error) {
	if from == nil {
		return nil, errors.New("current table definition is missing")
	}
	if from.Kind != "table" {
		return nil, fmt.Errorf("a %s is changed by creating it again", strings.ReplaceAll(from.Kind, "_", " "))
	}
	target := qualify(from.Ref)
	alter := func(clause string) string { return "ALTER TABLE " + target + " " + clause }
	var out, prep []string
	current := map[string]driver.Column{}
	for _, col := range from.Columns {
		current[col.Name] = col
	}
	kept := map[string]bool{}
	for _, col := range to.Columns {
		if col.OriginalName != "" {
			kept[col.OriginalName] = true
		}
	}
	needMapping := false
	var dropped []string
	for _, col := range from.Columns {
		if !kept[col.Name] {
			dropped = append(dropped, col.Name)
		}
	}
	if len(dropped) > 0 {
		out = append(out, alter("DROP COLUMNS ("+quoteList(dropped)+")"))
		needMapping = true
	}
	defaults := false
	for _, col := range to.Columns {
		if col.OriginalName == "" {
			if !col.Nullable {
				return nil, fmt.Errorf("column %s: a new column starts empty, so add it nullable and set NOT NULL once it's filled", col.Name)
			}
			s, err := columnDef(col.Column, false)
			if err != nil {
				return nil, err
			}
			defaults = defaults || col.Default != nil && *col.Default != ""
			out = append(out, alter("ADD COLUMN "+s))
			continue
		}
		old, ok := current[col.OriginalName]
		if !ok {
			return nil, fmt.Errorf("column %s no longer exists", col.OriginalName)
		}
		name := old.Name
		if col.Name != old.Name {
			out = append(out, alter("RENAME COLUMN "+quote(old.Name)+" TO "+quote(col.Name)))
			needMapping = true
			name = col.Name
		}
		if !strings.EqualFold(strings.ReplaceAll(col.Type, " ", ""), strings.ReplaceAll(old.Type, " ", "")) {
			out = append(out, alter("ALTER COLUMN "+quote(name)+" TYPE "+col.Type))
		}
		if col.Nullable != old.Nullable {
			out = append(out, alter("ALTER COLUMN "+quote(name)+map[bool]string{true: " DROP NOT NULL", false: " SET NOT NULL"}[col.Nullable]))
		}
		oldDef, newDef := "", ""
		if old.Default != nil {
			oldDef = *old.Default
		}
		if col.Default != nil {
			newDef = *col.Default
		}
		if strings.TrimSpace(oldDef) != strings.TrimSpace(newDef) {
			if newDef == "" {
				out = append(out, alter("ALTER COLUMN "+quote(name)+" DROP DEFAULT"))
			} else {
				out = append(out, alter("ALTER COLUMN "+quote(name)+" SET DEFAULT "+newDef))
				defaults = true
			}
		}
		if col.Comment != old.Comment {
			out = append(out, alter("ALTER COLUMN "+quote(name)+" COMMENT "+literal(col.Comment)))
		}
	}
	if to.Comment != from.Comment {
		out = append(out, "COMMENT ON TABLE "+target+" IS "+literal(to.Comment))
	}
	if !slices.Equal(to.PrimaryKey, from.PrimaryKey) {
		if len(from.PrimaryKey) > 0 {
			out = append(out, alter("DROP PRIMARY KEY IF EXISTS"))
		}
		if len(to.PrimaryKey) > 0 {
			out = append(out, alter("ADD CONSTRAINT "+quote(from.Ref.Name+"_pk")+" PRIMARY KEY ("+quoteList(to.PrimaryKey)+")"))
		}
	}
	oldChecks := map[string]string{}
	for _, ch := range from.Checks {
		oldChecks[ch.Name] = ch.Expression
	}
	newChecks := map[string]bool{}
	for _, ch := range to.Checks {
		newChecks[ch.Name] = true
		if e, ok := oldChecks[ch.Name]; !ok || e != ch.Expression {
			if ok {
				out = append(out, alter("DROP CONSTRAINT "+quote(ch.Name)))
			}
			out = append(out, alter("ADD CONSTRAINT "+quote(ch.Name)+" CHECK ("+ch.Expression+")"))
		}
	}
	for _, ch := range from.Checks {
		if !newChecks[ch.Name] {
			out = append(out, alter("DROP CONSTRAINT "+quote(ch.Name)))
		}
	}
	if v, ok := to.Options["partitioned_by"]; ok && strings.Join(colList(v), ",") != strings.Join(colList(from.Options["partitioned_by"]), ",") {
		return nil, errors.New("partition columns are fixed when the table is created; cluster the table instead (CLUSTER BY), or create a new one")
	}
	if v, ok := to.Options["cluster_by"]; ok && strings.Join(colList(v), ",") != strings.Join(colList(from.Options["cluster_by"]), ",") {
		if cols := colList(v); len(cols) > 0 {
			out = append(out, alter("CLUSTER BY ("+quoteList(cols)+")"))
		} else {
			out = append(out, alter("CLUSTER BY NONE"))
		}
	}
	if to.Ref.Name != "" && to.Ref.Name != from.Ref.Name {
		out = append(out, alter("RENAME TO "+qualify(driver.ObjectRef{Database: from.Ref.Database, Schema: from.Ref.Schema, Name: to.Ref.Name})))
	}
	if needMapping && from.Options["column_mapping"] != "name" {
		prep = append(prep, alter("SET TBLPROPERTIES ('delta.columnMapping.mode' = 'name')"))
	}
	if defaults {
		prep = append(prep, alter("SET TBLPROPERTIES ("+allowDefaults+")"))
	}
	if len(out) == 0 {
		return nil, nil
	}
	return append(prep, out...), nil
}

var dropKeyword = map[string]string{"table": "TABLE", "streaming_table": "TABLE", "view": "VIEW", "materialized_view": "MATERIALIZED VIEW",
	"function": "FUNCTION", "volume": "VOLUME"}

func (c *conn) DropObjectSQL(ref driver.ObjectRef, _ bool) ([]string, error) {
	kind := ref.Kind
	if kind == "" {
		kind = "table"
	}
	kw, ok := dropKeyword[kind]
	if !ok {
		return nil, fmt.Errorf("cannot drop a %s", ref.Kind)
	}
	return []string{"DROP " + kw + " " + qualify(ref)}, nil
}

func (c *conn) TruncateSQL(ref driver.ObjectRef) ([]string, error) {
	return []string{"TRUNCATE TABLE " + qualify(ref)}, nil
}

func (c *conn) RenameObjectSQL(ref driver.ObjectRef, newName string) ([]string, error) {
	kw := map[string]string{"": "TABLE", "table": "TABLE", "view": "VIEW", "volume": "VOLUME"}[ref.Kind]
	if kw == "" {
		return nil, fmt.Errorf("a %s can't be renamed", strings.ReplaceAll(ref.Kind, "_", " "))
	}
	return []string{"ALTER " + kw + " " + qualify(ref) + " RENAME TO " + qualify(driver.ObjectRef{Database: ref.Database, Schema: ref.Schema, Name: newName})}, nil
}

func (c *conn) CreateDatabaseSQL(name string, _ map[string]string) ([]string, error) {
	return []string{"CREATE CATALOG " + quote(name)}, nil
}

func (c *conn) DropDatabaseSQL(name string, force bool) ([]string, error) {
	q := "DROP CATALOG " + quote(name)
	if force {
		q += " CASCADE"
	}
	return []string{q}, nil
}

func (c *conn) CreateSchemaSQL(catalog, name string) ([]string, error) {
	return []string{"CREATE SCHEMA " + quote(catalog) + "." + quote(name)}, nil
}

func (c *conn) DropSchemaSQL(catalog, name string, cascade bool) ([]string, error) {
	q := "DROP SCHEMA " + quote(catalog) + "." + quote(name)
	if cascade {
		q += " CASCADE"
	}
	return []string{q}, nil
}

// BrowseQuery implements driver.BrowseQuerier for exports and migrations.
func (c *conn) BrowseQuery(_ context.Context, t *driver.Table, req driver.BrowseRequest) (string, []any, error) {
	return c.eng.ExportQuery(t, req)
}

// BeginImport implements driver.BulkImporter with multi-row INSERTs, each
// committed as it runs.
func (c *conn) BeginImport(ctx context.Context, t *driver.Table, cols []string, empty bool) (driver.RowImporter, error) {
	if c.ro {
		return nil, driver.ErrReadOnly
	}
	return c.eng.BeginImport(ctx, t, cols, empty)
}
