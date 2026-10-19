package trino

import (
	"errors"
	"fmt"
	"strings"

	"rowsmith/internal/driver"
)

func columnDef(c driver.Column) (string, error) {
	if strings.TrimSpace(c.Type) == "" {
		return "", fmt.Errorf("column %s has no type", c.Name)
	}
	s := quote(c.Name) + " " + c.Type
	if !c.Nullable {
		s += " NOT NULL"
	}
	if c.Comment != "" {
		s += " COMMENT " + literal(c.Comment)
	}
	return s, nil
}

// arrayList writes ARRAY['a', 'b'] from "a, b" (top-level commas only, so
// bucket(id, 4) stays whole).
func arrayList(list string) string {
	var items []string
	for _, p := range splitTop(list) {
		if p = strings.TrimSpace(p); p != "" {
			items = append(items, literal(p))
		}
	}
	return "ARRAY[" + strings.Join(items, ", ") + "]"
}

// withClause writes the table properties the structure editor offers.
func withClause(opts map[string]string) string {
	var props []string
	if f := strings.TrimSpace(opts["format"]); f != "" {
		props = append(props, "format = "+literal(strings.ToUpper(f)))
	}
	if p := strings.TrimSpace(opts["partitioning"]); p != "" {
		props = append(props, "partitioning = "+arrayList(p))
	}
	if p := strings.TrimSpace(opts["partitioned_by"]); p != "" {
		props = append(props, "partitioned_by = "+arrayList(p))
	}
	if p := strings.TrimSpace(opts["sorted_by"]); p != "" {
		props = append(props, "sorted_by = "+arrayList(p))
	}
	if l := strings.TrimSpace(opts["location"]); l != "" {
		key := "location"
		if opts["connector"] == "hive" {
			key = "external_location"
		}
		props = append(props, key+" = "+literal(l))
	}
	if len(props) == 0 {
		return ""
	}
	return "\nWITH (\n  " + strings.Join(props, ",\n  ") + "\n)"
}

func (c *conn) CreateTableSQL(def driver.TableDef) ([]string, error) {
	if def.Ref.Database == "" || def.Ref.Schema == "" {
		return nil, errors.New("choose a catalog and schema")
	}
	if len(def.Columns) == 0 {
		return nil, errors.New("a table needs at least one column")
	}
	if len(def.PrimaryKey) > 0 || len(def.ForeignKeys) > 0 || len(def.Indexes) > 0 {
		return nil, errors.New("Trino tables have no keys or indexes")
	}
	var cols []string
	for _, col := range def.Columns {
		s, err := columnDef(col.Column)
		if err != nil {
			return nil, err
		}
		cols = append(cols, s)
	}
	q := "CREATE TABLE " + qualify(def.Ref) + " (\n  " + strings.Join(cols, ",\n  ") + "\n)"
	if def.Comment != "" {
		q += "\nCOMMENT " + literal(def.Comment)
	}
	opts := map[string]string{}
	for k, v := range def.Options {
		opts[k] = v
	}
	if opts["connector"] == "" {
		opts["connector"] = c.connectorName(def.Ref.Database)
	}
	return []string{q + withClause(opts)}, nil
}

func (c *conn) connectorName(catalog string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connectors[catalog]
}

// AlterTableSQL writes one statement per change. Which changes a table
// takes depends on its connector (Iceberg: nearly all; Hive: adding and
// renaming columns); Trino reports the ones it cannot make.
func (c *conn) AlterTableSQL(from *driver.Table, to driver.TableDef) ([]string, error) {
	if from == nil {
		return nil, errors.New("current table definition is missing")
	}
	target := qualify(from.Ref)
	alter := func(clause string) string { return "ALTER TABLE " + target + " " + clause }
	var out []string
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
	for _, col := range from.Columns {
		if !kept[col.Name] {
			out = append(out, alter("DROP COLUMN "+quote(col.Name)))
		}
	}
	for _, col := range to.Columns {
		if col.OriginalName == "" {
			s, err := columnDef(col.Column)
			if err != nil {
				return nil, err
			}
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
			name = col.Name
		}
		if !strings.EqualFold(strings.TrimSpace(col.Type), strings.TrimSpace(old.Type)) {
			out = append(out, alter("ALTER COLUMN "+quote(name)+" SET DATA TYPE "+col.Type))
		}
		if col.Nullable != old.Nullable {
			if !col.Nullable {
				return nil, fmt.Errorf("column %s: Trino cannot add NOT NULL to an existing column", col.Name)
			}
			out = append(out, alter("ALTER COLUMN "+quote(name)+" DROP NOT NULL"))
		}
		if col.Comment != old.Comment {
			cm := "NULL"
			if col.Comment != "" {
				cm = literal(col.Comment)
			}
			out = append(out, "COMMENT ON COLUMN "+target+"."+quote(name)+" IS "+cm)
		}
	}
	if to.Comment != from.Comment {
		cm := "NULL"
		if to.Comment != "" {
			cm = literal(to.Comment)
		}
		out = append(out, "COMMENT ON TABLE "+target+" IS "+cm)
	}
	var props []string
	for _, k := range []string{"partitioning", "sorted_by"} {
		if v := strings.TrimSpace(to.Options[k]); v != strings.TrimSpace(from.Options[k]) {
			props = append(props, k+" = "+arrayList(v))
		}
	}
	if f := strings.TrimSpace(to.Options["format"]); f != "" && !strings.EqualFold(f, from.Options["format"]) {
		props = append(props, "format = "+literal(strings.ToUpper(f)))
	}
	if len(props) > 0 {
		out = append(out, alter("SET PROPERTIES "+strings.Join(props, ", ")))
	}
	if to.Ref.Name != "" && to.Ref.Name != from.Ref.Name {
		out = append(out, alter("RENAME TO "+qualify(driver.ObjectRef{Database: from.Ref.Database, Schema: from.Ref.Schema, Name: to.Ref.Name})))
	}
	return out, nil
}

var dropKeyword = map[string]string{"table": "TABLE", "view": "VIEW", "materialized_view": "MATERIALIZED VIEW"}

func (c *conn) DropObjectSQL(ref driver.ObjectRef, _ bool) ([]string, error) {
	kw, ok := dropKeyword[ref.Kind]
	if !ok {
		return nil, fmt.Errorf("cannot drop a %s", ref.Kind)
	}
	return []string{"DROP " + kw + " " + qualify(ref)}, nil
}

func (c *conn) TruncateSQL(ref driver.ObjectRef) ([]string, error) {
	return []string{"TRUNCATE TABLE " + qualify(ref)}, nil
}

func (c *conn) RenameObjectSQL(ref driver.ObjectRef, newName string) ([]string, error) {
	kw, ok := dropKeyword[ref.Kind]
	if !ok {
		return nil, fmt.Errorf("renaming a %s is not supported", ref.Kind)
	}
	return []string{"ALTER " + kw + " " + qualify(ref) + " RENAME TO " + qualify(driver.ObjectRef{Database: ref.Database, Schema: ref.Schema, Name: newName})}, nil
}

func (c *conn) CreateDatabaseSQL(string, map[string]string) ([]string, error) {
	return nil, errors.New("Trino catalogs are configured on the cluster, not created with SQL")
}

func (c *conn) DropDatabaseSQL(string, bool) ([]string, error) {
	return nil, errors.New("Trino catalogs are configured on the cluster, not dropped with SQL")
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
