package mysql

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"rowsmith/internal/driver"
)

// DDL generation (driver.DDLGenerator). Statements are previewed by the UI
// and executed through the console, so nothing here touches the server.

func lit(s string) string {
	return "'" + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), "'", "''") + "'"
}

func (c *conn) columnDef(col driver.Column) (string, error) {
	if strings.TrimSpace(col.Name) == "" {
		return "", errors.New("every column needs a name")
	}
	if strings.TrimSpace(col.Type) == "" {
		return "", fmt.Errorf("column %s needs a type", col.Name)
	}
	var b strings.Builder
	b.WriteString(quote(col.Name) + " " + col.Type)
	if col.Collation != "" && isTextual(col) {
		b.WriteString(" COLLATE " + col.Collation)
	}
	if col.Generated != "" {
		b.WriteString(" GENERATED ALWAYS AS (" + col.Generated + ")")
		if col.GeneratedStored {
			b.WriteString(" STORED")
		} else {
			b.WriteString(" VIRTUAL")
		}
	}
	if col.Nullable {
		b.WriteString(" NULL")
	} else {
		b.WriteString(" NOT NULL")
	}
	if col.Default != nil && col.Generated == "" && !col.AutoIncrement {
		b.WriteString(" DEFAULT " + *col.Default)
	}
	if col.OnUpdate != "" {
		b.WriteString(" ON UPDATE " + col.OnUpdate)
	}
	if col.AutoIncrement {
		b.WriteString(" AUTO_INCREMENT")
	}
	if col.Comment != "" {
		b.WriteString(" COMMENT " + lit(col.Comment))
	}
	return b.String(), nil
}

func isTextual(col driver.Column) bool {
	switch col.Kind {
	case driver.KindString, driver.KindText, driver.KindEnum:
		return true
	}
	t := strings.ToLower(col.Type)
	return strings.Contains(t, "char") || strings.Contains(t, "text") || strings.HasPrefix(t, "enum") || strings.HasPrefix(t, "set")
}

func indexDef(ix driver.Index) string {
	cols := make([]string, len(ix.Columns))
	for i, c := range ix.Columns {
		if strings.HasPrefix(c, "(") {
			cols[i] = c // functional key part
		} else {
			cols[i] = quote(c)
		}
		if i < len(ix.Lengths) && ix.Lengths[i] > 0 {
			cols[i] += fmt.Sprintf("(%d)", ix.Lengths[i])
		}
		if i < len(ix.Desc) && ix.Desc[i] {
			cols[i] += " DESC"
		}
	}
	kind := "INDEX"
	switch {
	case ix.Unique:
		kind = "UNIQUE INDEX"
	case strings.EqualFold(ix.Type, "fulltext"):
		kind = "FULLTEXT INDEX"
	case strings.EqualFold(ix.Type, "spatial"):
		kind = "SPATIAL INDEX"
	}
	s := kind + " " + quote(ix.Name) + " (" + strings.Join(cols, ", ") + ")"
	if ix.Comment != "" {
		s += " COMMENT " + lit(ix.Comment)
	}
	return s
}

func (c *conn) fkDef(db string, fk driver.ForeignKey) string {
	refDB := fk.RefTable.Database
	if refDB == "" {
		refDB = db
	}
	s := "CONSTRAINT " + quote(fk.Name) + " FOREIGN KEY (" + quoteAll(fk.Columns) + ") REFERENCES " +
		qualify(refDB, fk.RefTable.Name) + " (" + quoteAll(fk.RefColumns) + ")"
	if fk.OnDelete != "" && !strings.EqualFold(fk.OnDelete, "NO ACTION") && !strings.EqualFold(fk.OnDelete, "RESTRICT") {
		s += " ON DELETE " + strings.ToUpper(fk.OnDelete)
	}
	if fk.OnUpdate != "" && !strings.EqualFold(fk.OnUpdate, "NO ACTION") && !strings.EqualFold(fk.OnUpdate, "RESTRICT") {
		s += " ON UPDATE " + strings.ToUpper(fk.OnUpdate)
	}
	return s
}

func quoteAll(cols []string) string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = quote(c)
	}
	return strings.Join(out, ", ")
}

func tableOptions(opts map[string]string, comment string) string {
	var parts []string
	if e := opts["engine"]; e != "" {
		parts = append(parts, "ENGINE="+e)
	}
	if co := opts["collation"]; co != "" {
		parts = append(parts, "COLLATE="+co)
	}
	if ai := opts["auto_increment"]; ai != "" {
		parts = append(parts, "AUTO_INCREMENT="+ai)
	}
	if comment != "" {
		parts = append(parts, "COMMENT="+lit(comment))
	}
	return strings.Join(parts, " ")
}

func (c *conn) CreateTableSQL(def driver.TableDef) ([]string, error) {
	if def.Ref.Name == "" {
		return nil, errors.New("give the table a name")
	}
	if len(def.Columns) == 0 {
		return nil, errors.New("a table needs at least one column")
	}
	var lines []string
	for _, col := range def.Columns {
		d, err := c.columnDef(col.Column)
		if err != nil {
			return nil, err
		}
		lines = append(lines, d)
	}
	if len(def.PrimaryKey) > 0 {
		lines = append(lines, "PRIMARY KEY ("+quoteAll(def.PrimaryKey)+")")
	}
	for _, ix := range def.Indexes {
		if !ix.Primary {
			lines = append(lines, indexDef(ix))
		}
	}
	for _, fk := range def.ForeignKeys {
		lines = append(lines, c.fkDef(def.Ref.Database, fk))
	}
	for _, ch := range def.Checks {
		lines = append(lines, "CONSTRAINT "+quote(ch.Name)+" CHECK ("+ch.Expression+")")
	}
	s := "CREATE TABLE " + qualify(def.Ref.Database, def.Ref.Name) + " (\n  " + strings.Join(lines, ",\n  ") + "\n)"
	if o := tableOptions(def.Options, def.Comment); o != "" {
		s += " " + o
	}
	return []string{s}, nil
}

// AlterTableSQL diffs the current table against the desired definition.
// Columns are matched by OriginalName; unmatched current columns are dropped.
func (c *conn) AlterTableSQL(from *driver.Table, to driver.TableDef) ([]string, error) {
	if from == nil {
		return nil, errors.New("current table definition is missing")
	}
	from, to = driver.FollowRenames(from, to)
	target := qualify(from.Ref.Database, from.Ref.Name)
	var pre, clauses []string

	kept := map[string]bool{}
	for _, col := range to.Columns {
		if col.OriginalName != "" {
			kept[col.OriginalName] = true
		}
	}
	current := map[string]driver.Column{}
	for _, col := range from.Columns {
		current[col.Name] = col
	}

	// Foreign keys are dropped up front: an index cannot be dropped while a key uses it.
	fromFK := map[string]driver.ForeignKey{}
	for _, fk := range from.ForeignKeys {
		fromFK[fk.Name] = fk
	}
	toFK := map[string]driver.ForeignKey{}
	for _, fk := range to.ForeignKeys {
		toFK[fk.Name] = fk
	}
	for _, fk := range from.ForeignKeys {
		if n, ok := toFK[fk.Name]; !ok || !sameFK(fk, n) {
			pre = append(pre, "DROP FOREIGN KEY "+quote(fk.Name))
		}
	}

	for _, col := range from.Columns {
		if !kept[col.Name] {
			clauses = append(clauses, "DROP COLUMN "+quote(col.Name))
		}
	}
	// A kept column has moved when its place among the kept columns changed;
	// newly added columns alone do not move the others.
	var oldOrder, newOrder []string
	for _, col := range from.Columns {
		if kept[col.Name] {
			oldOrder = append(oldOrder, col.Name)
		}
	}
	for _, col := range to.Columns {
		if col.OriginalName != "" {
			newOrder = append(newOrder, col.OriginalName)
		}
	}
	moved := map[string]bool{}
	for j := range newOrder {
		if j >= len(oldOrder) || oldOrder[j] != newOrder[j] {
			moved[newOrder[j]] = true
		}
	}
	prev := ""
	for _, col := range to.Columns {
		d, err := c.columnDef(col.Column)
		if err != nil {
			return nil, err
		}
		pos := " FIRST"
		if prev != "" {
			pos = " AFTER " + quote(prev)
		}
		if col.OriginalName == "" {
			clauses = append(clauses, "ADD COLUMN "+d+pos)
		} else {
			old, ok := current[col.OriginalName]
			if !ok {
				return nil, fmt.Errorf("column %s no longer exists; reload the structure", col.OriginalName)
			}
			oldDef, _ := c.columnDef(old)
			if oldDef != d || moved[col.OriginalName] {
				clause := "CHANGE COLUMN " + quote(col.OriginalName) + " " + d
				if moved[col.OriginalName] {
					clause += pos
				}
				clauses = append(clauses, clause)
			}
		}
		prev = col.Name
	}

	if !slices.Equal(from.PrimaryKey, to.PrimaryKey) {
		if len(from.PrimaryKey) > 0 {
			clauses = append(clauses, "DROP PRIMARY KEY")
		}
		if len(to.PrimaryKey) > 0 {
			clauses = append(clauses, "ADD PRIMARY KEY ("+quoteAll(to.PrimaryKey)+")")
		}
	}

	fromIx := map[string]driver.Index{}
	for _, ix := range from.Indexes {
		if !ix.Primary {
			fromIx[ix.Name] = ix
		}
	}
	toIx := map[string]driver.Index{}
	for _, ix := range to.Indexes {
		if !ix.Primary {
			toIx[ix.Name] = ix
		}
	}
	for _, ix := range from.Indexes {
		if ix.Primary {
			continue
		}
		if n, ok := toIx[ix.Name]; !ok || indexDef(n) != indexDef(ix) {
			clauses = append(clauses, "DROP INDEX "+quote(ix.Name))
		}
	}
	for _, ix := range to.Indexes {
		if ix.Primary {
			continue
		}
		if old, ok := fromIx[ix.Name]; !ok || indexDef(old) != indexDef(ix) {
			clauses = append(clauses, "ADD "+indexDef(ix))
		}
	}
	for _, fk := range to.ForeignKeys {
		if old, ok := fromFK[fk.Name]; !ok || !sameFK(old, fk) {
			clauses = append(clauses, "ADD "+c.fkDef(from.Ref.Database, fk))
		}
	}

	fromCk := map[string]string{}
	for _, ch := range from.Checks {
		fromCk[ch.Name] = ch.Expression
	}
	toCk := map[string]string{}
	for _, ch := range to.Checks {
		toCk[ch.Name] = ch.Expression
	}
	dropCheck := "DROP CHECK "
	if c.flavor == "mariadb" {
		dropCheck = "DROP CONSTRAINT "
	}
	for _, ch := range from.Checks {
		if e, ok := toCk[ch.Name]; !ok || e != ch.Expression {
			clauses = append(clauses, dropCheck+quote(ch.Name))
		}
	}
	for _, ch := range to.Checks {
		if e, ok := fromCk[ch.Name]; !ok || e != ch.Expression {
			clauses = append(clauses, "ADD CONSTRAINT "+quote(ch.Name)+" CHECK ("+ch.Expression+")")
		}
	}

	var opts []string
	if to.Comment != from.Comment {
		opts = append(opts, "COMMENT = "+lit(to.Comment))
	}
	for _, k := range []string{"engine", "collation", "auto_increment"} {
		if v := to.Options[k]; v != "" && v != from.Options[k] {
			opts = append(opts, map[string]string{"engine": "ENGINE", "collation": "COLLATE", "auto_increment": "AUTO_INCREMENT"}[k]+" = "+v)
		}
	}
	clauses = append(clauses, opts...)

	var out []string
	if len(pre) > 0 {
		out = append(out, "ALTER TABLE "+target+"\n  "+strings.Join(pre, ",\n  "))
	}
	if len(clauses) > 0 {
		out = append(out, "ALTER TABLE "+target+"\n  "+strings.Join(clauses, ",\n  "))
	}
	if to.Ref.Name != "" && to.Ref.Name != from.Ref.Name {
		out = append(out, "RENAME TABLE "+target+" TO "+qualify(from.Ref.Database, to.Ref.Name))
	}
	return out, nil
}

func sameFK(a, b driver.ForeignKey) bool {
	return slices.Equal(a.Columns, b.Columns) && slices.Equal(a.RefColumns, b.RefColumns) && a.RefTable.Name == b.RefTable.Name &&
		strings.EqualFold(norm(a.OnDelete), norm(b.OnDelete)) && strings.EqualFold(norm(a.OnUpdate), norm(b.OnUpdate))
}

func norm(rule string) string {
	if rule == "" || strings.EqualFold(rule, "RESTRICT") {
		return "NO ACTION"
	}
	return rule
}

var dropKeyword = map[string]string{
	"table": "TABLE", "view": "VIEW", "procedure": "PROCEDURE", "function": "FUNCTION", "trigger": "TRIGGER", "event": "EVENT",
}

func (c *conn) DropObjectSQL(ref driver.ObjectRef, _ bool) ([]string, error) {
	kw, ok := dropKeyword[ref.Kind]
	if !ok {
		return nil, fmt.Errorf("cannot drop objects of kind %q", ref.Kind)
	}
	return []string{"DROP " + kw + " " + qualify(ref.Database, ref.Name)}, nil
}

func (c *conn) TruncateSQL(ref driver.ObjectRef) ([]string, error) {
	return []string{"TRUNCATE TABLE " + qualify(ref.Database, ref.Name)}, nil
}

func (c *conn) RenameObjectSQL(ref driver.ObjectRef, newName string) ([]string, error) {
	if strings.TrimSpace(newName) == "" {
		return nil, errors.New("enter a new name")
	}
	if ref.Kind != "table" && ref.Kind != "view" && ref.Kind != "" {
		return nil, fmt.Errorf("renaming a %s is not supported; recreate it instead", ref.Kind)
	}
	return []string{"RENAME TABLE " + qualify(ref.Database, ref.Name) + " TO " + qualify(ref.Database, newName)}, nil
}

func (c *conn) CreateDatabaseSQL(name string, opts map[string]string) ([]string, error) {
	if strings.TrimSpace(name) == "" {
		return nil, errors.New("enter a database name")
	}
	s := "CREATE DATABASE " + quote(name)
	if cs := opts["charset"]; cs != "" {
		s += " CHARACTER SET " + cs
	}
	if co := opts["collation"]; co != "" {
		s += " COLLATE " + co
	}
	return []string{s}, nil
}

func (c *conn) DropDatabaseSQL(name string) ([]string, error) {
	if strings.TrimSpace(name) == "" {
		return nil, errors.New("choose a database")
	}
	return []string{"DROP DATABASE " + quote(name)}, nil
}
