package snowflake

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"rowsmith/internal/driver"
)

// columnDef writes a column for CREATE TABLE or ADD COLUMN.
func columnDef(c driver.Column) (string, error) {
	if strings.TrimSpace(c.Type) == "" {
		return "", fmt.Errorf("column %s has no type", c.Name)
	}
	if c.Generated != "" {
		return "", fmt.Errorf("column %s: Snowflake tables have no computed columns; use a view", c.Name)
	}
	var b strings.Builder
	b.WriteString(quote(c.Name) + " " + c.Type)
	if c.Collation != "" {
		b.WriteString(" COLLATE " + literal(c.Collation))
	}
	switch {
	case c.AutoIncrement:
		b.WriteString(" AUTOINCREMENT")
	case c.Default != nil && strings.TrimSpace(*c.Default) != "":
		b.WriteString(" DEFAULT " + *c.Default)
	}
	if !c.Nullable {
		b.WriteString(" NOT NULL")
	}
	if c.Comment != "" {
		b.WriteString(" COMMENT " + literal(c.Comment))
	}
	return b.String(), nil
}

func uniqueName(table string, cols []string) string {
	return "uq_" + strings.ToLower(table) + "_" + strings.ToLower(strings.Join(cols, "_"))
}

func fkDef(fk driver.ForeignKey, db, schema string) string {
	ref := fk.RefTable
	if ref.Database == "" {
		ref.Database = db
	}
	if ref.Schema == "" {
		ref.Schema = schema
	}
	cols := make([]string, len(fk.Columns))
	for i, c := range fk.Columns {
		cols[i] = quote(c)
	}
	refCols := make([]string, len(fk.RefColumns))
	for i, c := range fk.RefColumns {
		refCols[i] = quote(c)
	}
	s := "FOREIGN KEY (" + strings.Join(cols, ", ") + ") REFERENCES " + qualify(driver.ObjectRef{Database: ref.Database, Schema: ref.Schema, Name: ref.Name}) + " (" + strings.Join(refCols, ", ") + ")"
	if fk.Name != "" {
		s = "CONSTRAINT " + quote(fk.Name) + " " + s
	}
	return s
}

func quoteList(cols []string) string {
	q := make([]string, len(cols))
	for i, c := range cols {
		q[i] = quote(c)
	}
	return strings.Join(q, ", ")
}

func (c *conn) CreateTableSQL(def driver.TableDef) ([]string, error) {
	if def.Ref.Database == "" || def.Ref.Schema == "" {
		return nil, errors.New("choose a database and schema")
	}
	if len(def.Columns) == 0 {
		return nil, errors.New("a table needs at least one column")
	}
	var parts []string
	for _, col := range def.Columns {
		s, err := columnDef(col.Column)
		if err != nil {
			return nil, err
		}
		parts = append(parts, s)
	}
	if len(def.PrimaryKey) > 0 {
		parts = append(parts, "PRIMARY KEY ("+quoteList(def.PrimaryKey)+")")
	}
	for _, ix := range def.Indexes {
		if ix.Primary {
			continue
		}
		if !ix.Unique {
			return nil, fmt.Errorf("index %s: Snowflake tables have no indexes, only unique constraints and a clustering key", ix.Name)
		}
		name := ix.Name
		if name == "" {
			name = uniqueName(def.Ref.Name, ix.Columns)
		}
		parts = append(parts, "CONSTRAINT "+quote(name)+" UNIQUE ("+quoteList(ix.Columns)+")")
	}
	for _, fk := range def.ForeignKeys {
		parts = append(parts, fkDef(fk, def.Ref.Database, def.Ref.Schema))
	}
	kw := "CREATE TABLE "
	if def.Options["transient"] == "true" {
		kw = "CREATE TRANSIENT TABLE "
	}
	q := kw + qualify(def.Ref) + " (\n  " + strings.Join(parts, ",\n  ") + "\n)"
	if cb := strings.TrimSpace(def.Options["cluster_by"]); cb != "" {
		q += "\nCLUSTER BY (" + cb + ")"
	}
	if def.Comment != "" {
		q += "\nCOMMENT = " + literal(def.Comment)
	}
	return []string{q}, nil
}

// AlterTableSQL writes one ALTER TABLE per change, as Snowflake takes them.
// Snowflake changes a column's type only to a wider one of the same kind,
// and cannot change a default or a collation in place.
func (c *conn) AlterTableSQL(from *driver.Table, to driver.TableDef) ([]string, error) {
	if from == nil {
		return nil, errors.New("current table definition is missing")
	}
	from, to = driver.FollowRenames(from, to)
	target := qualify(from.Ref)
	alter := func(clause string) string { return "ALTER TABLE " + target + " " + clause }
	var out []string

	// A column in the clustering key cannot be renamed: the key is dropped
	// first and set again, with the new names, at the end.
	oldCB, newCB := from.Options["cluster_by"], strings.TrimSpace(to.Options["cluster_by"])
	reCluster := false
	for _, col := range to.Columns {
		if col.OriginalName != "" && col.OriginalName != col.Name {
			re := regexp.MustCompile(`(?i)(^|[^\w$"])"?` + regexp.QuoteMeta(col.OriginalName) + `"?([^\w$"]|$)`)
			if re.MatchString(oldCB) {
				reCluster = true
			}
			newCB = re.ReplaceAllString(newCB, "${1}"+col.Name+"${2}")
		}
	}
	if reCluster && oldCB != "" {
		out = append(out, alter("DROP CLUSTERING KEY"))
		oldCB = ""
	}

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
	// Keys go first: a column cannot be dropped while a constraint uses it.
	keyChanged := !slices.Equal(from.PrimaryKey, to.PrimaryKey)
	if keyChanged && len(from.PrimaryKey) > 0 {
		out = append(out, alter("DROP PRIMARY KEY"))
	}
	fromUnique := map[string]driver.Index{}
	for _, ix := range from.Indexes {
		if ix.Unique && !ix.Primary {
			fromUnique[ix.Name] = ix
		}
	}
	toUnique := map[string]driver.Index{}
	for _, ix := range to.Indexes {
		if ix.Primary {
			continue
		}
		if !ix.Unique {
			return nil, fmt.Errorf("index %s: Snowflake tables have no indexes, only unique constraints and a clustering key", ix.Name)
		}
		if ix.Name == "" {
			ix.Name = uniqueName(from.Ref.Name, ix.Columns)
		}
		toUnique[ix.Name] = ix
	}
	for name, ix := range fromUnique {
		if t, ok := toUnique[name]; !ok || !slices.Equal(t.Columns, ix.Columns) {
			out = append(out, alter("DROP CONSTRAINT "+quote(name)))
		}
	}
	fromFK := map[string]driver.ForeignKey{}
	for _, fk := range from.ForeignKeys {
		fromFK[fk.Name] = fk
	}
	toFK := map[string]driver.ForeignKey{}
	for _, fk := range to.ForeignKeys {
		toFK[fk.Name] = fk
	}
	for name, fk := range fromFK {
		if t, ok := toFK[name]; !ok || !sameFK(fk, t) {
			out = append(out, alter("DROP CONSTRAINT "+quote(name)))
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
		col2 := alter("ALTER COLUMN " + quote(name) + " ")
		if !strings.EqualFold(col.Type, old.Type) {
			out = append(out, col2+"SET DATA TYPE "+col.Type)
		}
		if col.Nullable != old.Nullable {
			out = append(out, col2+map[bool]string{true: "DROP NOT NULL", false: "SET NOT NULL"}[col.Nullable])
		}
		if col.Comment != old.Comment {
			if col.Comment == "" {
				out = append(out, col2+"UNSET COMMENT")
			} else {
				out = append(out, col2+"COMMENT "+literal(col.Comment))
			}
		}
		oldDef, newDef := "", ""
		if old.Default != nil {
			oldDef = *old.Default
		}
		if col.Default != nil {
			newDef = *col.Default
		}
		if oldDef != newDef && !col.AutoIncrement {
			if newDef != "" {
				return nil, fmt.Errorf("column %s: Snowflake cannot change a default; add a new column with it and copy the values", col.Name)
			}
			out = append(out, col2+"DROP DEFAULT")
		}
		if col.Collation != old.Collation {
			return nil, fmt.Errorf("column %s: Snowflake cannot change a collation in place", col.Name)
		}
		if col.AutoIncrement != old.AutoIncrement {
			return nil, fmt.Errorf("column %s: Snowflake cannot add or remove AUTOINCREMENT on an existing column", col.Name)
		}
	}

	if keyChanged && len(to.PrimaryKey) > 0 {
		out = append(out, alter("ADD PRIMARY KEY ("+quoteList(to.PrimaryKey)+")"))
	}
	for name, ix := range toUnique {
		if f, ok := fromUnique[name]; !ok || !slices.Equal(f.Columns, ix.Columns) {
			out = append(out, alter("ADD CONSTRAINT "+quote(name)+" UNIQUE ("+quoteList(ix.Columns)+")"))
		}
	}
	for name, fk := range toFK {
		if f, ok := fromFK[name]; !ok || !sameFK(f, fk) {
			out = append(out, alter("ADD "+fkDef(fk, from.Ref.Database, from.Ref.Schema)))
		}
	}
	if to.Comment != from.Comment {
		if to.Comment == "" {
			out = append(out, alter("UNSET COMMENT"))
		} else {
			out = append(out, alter("SET COMMENT = "+literal(to.Comment)))
		}
	}
	if cb, old := newCB, oldCB; cb != old {
		if cb == "" {
			out = append(out, alter("DROP CLUSTERING KEY"))
		} else {
			out = append(out, alter("CLUSTER BY ("+cb+")"))
		}
	}
	if (to.Options["transient"] == "true") != (from.Options["transient"] == "true") {
		return nil, errors.New("a table cannot become transient or permanent in place; create a new table and copy the rows")
	}
	if to.Ref.Name != "" && to.Ref.Name != from.Ref.Name {
		out = append(out, alter("RENAME TO "+qualify(driver.ObjectRef{Database: from.Ref.Database, Schema: from.Ref.Schema, Name: to.Ref.Name})))
	}
	return out, nil
}

func sameFK(a, b driver.ForeignKey) bool {
	return slices.Equal(a.Columns, b.Columns) && slices.Equal(a.RefColumns, b.RefColumns) && a.RefTable.Name == b.RefTable.Name
}

var dropKeyword = map[string]string{
	"table": "TABLE", "view": "VIEW", "materialized_view": "MATERIALIZED VIEW", "dynamic_table": "DYNAMIC TABLE",
	"external_table": "EXTERNAL TABLE", "sequence": "SEQUENCE", "function": "FUNCTION", "procedure": "PROCEDURE",
}

func (c *conn) DropObjectSQL(ref driver.ObjectRef, cascade bool) ([]string, error) {
	kw, ok := dropKeyword[ref.Kind]
	if !ok {
		return nil, fmt.Errorf("cannot drop a %s", ref.Kind)
	}
	if ref.Kind == "function" || ref.Kind == "procedure" {
		sigs, err := c.signatures(ref)
		if err != nil {
			return nil, err
		}
		var out []string
		for _, s := range sigs {
			out = append(out, "DROP "+kw+" "+s)
		}
		return out, nil
	}
	q := "DROP " + kw + " " + qualify(ref)
	if cascade && ref.Kind == "table" {
		q += " CASCADE"
	}
	return []string{q}, nil
}

// signatures lists every overload of a routine as "DB"."S"."NAME"(TYPE, …).
func (c *conn) signatures(ref driver.ObjectRef) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	rs, err := c.show(ctx, "SHOW USER "+strings.ToUpper(ref.Kind)+"S LIKE "+literal(likeEscape(ref.Name))+" IN SCHEMA "+quote(ref.Database)+"."+quote(ref.Schema))
	if err != nil {
		return nil, err
	}
	var out []string
	for _, r := range rs {
		if r.str("name") != ref.Name {
			continue
		}
		sig := r.str("arguments")
		if i := strings.LastIndex(sig, ") RETURN"); i > 0 {
			sig = sig[:i+1]
		}
		if i := strings.Index(sig, "("); i >= 0 {
			out = append(out, qualify(ref)+sig[i:])
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s %s not found", ref.Kind, ref.Name)
	}
	return out, nil
}

func (c *conn) TruncateSQL(ref driver.ObjectRef) ([]string, error) {
	return []string{"TRUNCATE TABLE " + qualify(ref)}, nil
}

func (c *conn) RenameObjectSQL(ref driver.ObjectRef, newName string) ([]string, error) {
	kw, ok := dropKeyword[ref.Kind]
	if !ok || ref.Kind == "function" || ref.Kind == "procedure" {
		return nil, fmt.Errorf("renaming a %s is not supported", ref.Kind)
	}
	to := qualify(driver.ObjectRef{Database: ref.Database, Schema: ref.Schema, Name: newName})
	return []string{"ALTER " + kw + " " + qualify(ref) + " RENAME TO " + to}, nil
}

func (c *conn) CreateDatabaseSQL(name string, opts map[string]string) ([]string, error) {
	q := "CREATE DATABASE " + quote(name)
	if cm := opts["comment"]; cm != "" {
		q += " COMMENT = " + literal(cm)
	}
	return []string{q}, nil
}

func (c *conn) DropDatabaseSQL(name string, _ bool) ([]string, error) {
	return []string{"DROP DATABASE " + quote(name)}, nil
}

func (c *conn) CreateSchemaSQL(database, name string) ([]string, error) {
	return []string{"CREATE SCHEMA " + quote(database) + "." + quote(name)}, nil
}

func (c *conn) DropSchemaSQL(database, name string, cascade bool) ([]string, error) {
	q := "DROP SCHEMA " + quote(database) + "." + quote(name)
	if !cascade {
		q += " RESTRICT"
	}
	return []string{q}, nil
}
