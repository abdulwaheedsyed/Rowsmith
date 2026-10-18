package clickhouse

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"rowsmith/internal/driver"
)

var design = driver.TableDesign{
	Columns: true, ReorderColumns: true, ColumnComments: true, TableComment: true,
	Generated: true, GeneratedVirtual: true, GeneratedStored: true, PrimaryKey: true, Indexes: true,
	IndexTypes: []string{"minmax", "bloom_filter", "set(100)", "ngrambf_v1(3, 256, 2, 0)", "tokenbf_v1(256, 2, 0)"},
	Options: []driver.Field{
		{Key: "engine", Label: "Engine", Type: driver.FieldText, Span: 3, Placeholder: "MergeTree",
			Help: "MergeTree, or ReplacingMergeTree(version), SummingMergeTree, AggregatingMergeTree, Memory, Log…"},
		{Key: "order_by", Label: "ORDER BY", Type: driver.FieldText, Span: 3, Placeholder: "the primary key's columns, or tuple()",
			Help: "How rows are sorted on disk; queries filtering on its first columns read the least."},
		{Key: "partition_by", Label: "PARTITION BY", Type: driver.FieldText, Span: 3, Placeholder: "e.g. toYYYYMM(created_at)"},
		{Key: "primary_key", Label: "PRIMARY KEY", Type: driver.FieldText, Span: 3, Placeholder: "same as ORDER BY",
			Help: "Only when it's shorter than ORDER BY: a prefix of it."},
		{Key: "ttl", Label: "TTL", Type: driver.FieldText, Span: 3, Placeholder: "e.g. created_at + INTERVAL 90 DAY"},
		{Key: "settings", Label: "SETTINGS", Type: driver.FieldText, Span: 3, Placeholder: "e.g. index_granularity = 8192"},
	},
	Note: "The primary key orders rows on disk; it doesn't make them unique. Sorting and partition key columns can't be renamed or dropped, and changing a column's type rewrites the table's data.",
}

// fullType is a column's type with Nullable(…) when it allows NULL.
func fullType(c driver.Column) string {
	t := strings.TrimSpace(c.Type)
	if c.Nullable && !strings.Contains(t, "Nullable(") {
		return "Nullable(" + t + ")"
	}
	return t
}

// columnDef renders a column for CREATE TABLE, ADD COLUMN and MODIFY COLUMN.
func columnDef(c driver.Column) (string, error) {
	if strings.TrimSpace(c.Name) == "" {
		return "", errors.New("every column needs a name")
	}
	if strings.TrimSpace(c.Type) == "" {
		return "", fmt.Errorf("column %s needs a type", c.Name)
	}
	s := quote(c.Name) + " " + fullType(c)
	switch {
	case c.Generated != "" && c.GeneratedStored:
		s += " MATERIALIZED " + c.Generated
	case c.Generated != "":
		s += " ALIAS " + c.Generated
	case c.Default != nil && strings.TrimSpace(*c.Default) != "":
		s += " DEFAULT " + *c.Default
	}
	if c.Comment != "" {
		s += " COMMENT " + literal(c.Comment)
	}
	if c.Codec != "" {
		s += " " + c.Codec
	}
	return s, nil
}

var mergeTree = regexp.MustCompile(`(?i)MergeTree\b`)

func engineOf(opts map[string]string) string {
	e := strings.TrimSpace(opts["engine"])
	if e == "" {
		return "MergeTree"
	}
	return e
}

// keyList wraps several expressions in parentheses: ORDER BY (a, b).
func keyList(s string) string {
	s = strings.TrimSpace(s)
	if len(splitKey(s)) > 1 && !(strings.HasPrefix(s, "(") && strings.HasSuffix(s, ")") && len(splitKey(s[1:len(s)-1])) > 1) {
		return "(" + s + ")"
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

func indexDef(ix driver.Index) (string, error) {
	if ix.Name == "" || len(ix.Columns) == 0 {
		return "", errors.New("an index needs a name and an expression")
	}
	typ := strings.TrimSpace(ix.Type)
	if typ == "" || typ == "btree" {
		typ = "minmax"
	}
	expr := strings.Join(ix.Columns, ", ")
	if len(ix.Columns) > 1 {
		expr = "(" + expr + ")"
	}
	s := "INDEX " + quote(ix.Name) + " " + expr + " TYPE " + typ
	if g := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(ix.Definition), "GRANULARITY")); g != "" {
		s += " GRANULARITY " + g
	}
	return s, nil
}

// skipIndexes leaves out the sorting key, which is shown as an index.
func skipIndexes(ixs []driver.Index) []driver.Index {
	var out []driver.Index
	for _, ix := range ixs {
		if !ix.Primary {
			out = append(out, ix)
		}
	}
	return out
}

func (c *conn) CreateTableSQL(def driver.TableDef) ([]string, error) {
	if strings.TrimSpace(def.Ref.Name) == "" {
		return nil, errors.New("the table needs a name")
	}
	if len(def.Columns) == 0 {
		return nil, errors.New("add at least one column")
	}
	var lines []string
	for _, col := range def.Columns {
		d, err := columnDef(col.Column)
		if err != nil {
			return nil, err
		}
		lines = append(lines, "    "+d)
	}
	for _, ix := range skipIndexes(def.Indexes) {
		d, err := indexDef(ix)
		if err != nil {
			return nil, err
		}
		lines = append(lines, "    "+d)
	}
	opts := def.Options
	if opts == nil {
		opts = map[string]string{}
	}
	engine := engineOf(opts)
	s := "CREATE TABLE " + qualify(def.Ref.Database, def.Ref.Name) + "\n(\n" + strings.Join(lines, ",\n") + "\n)\nENGINE = " + engine
	if mergeTree.MatchString(engine) {
		order := strings.TrimSpace(opts["order_by"])
		if order == "" && len(def.PrimaryKey) > 0 {
			order = quoteList(def.PrimaryKey)
		}
		if order == "" {
			order = "tuple()"
		}
		if p := strings.TrimSpace(opts["partition_by"]); p != "" {
			s += "\nPARTITION BY " + p
		}
		if pk := strings.TrimSpace(opts["primary_key"]); pk != "" {
			s += "\nPRIMARY KEY " + keyList(pk)
		} else if len(def.PrimaryKey) > 0 && strings.TrimSpace(opts["order_by"]) != "" && !samePrefix(splitKey(order), def.PrimaryKey) {
			s += "\nPRIMARY KEY " + keyList(quoteList(def.PrimaryKey))
		}
		s += "\nORDER BY " + keyList(order)
		if v := strings.TrimSpace(opts["sample_by"]); v != "" {
			s += "\nSAMPLE BY " + v
		}
		if v := strings.TrimSpace(opts["ttl"]); v != "" {
			s += "\nTTL " + v
		}
		if v := strings.TrimSpace(opts["settings"]); v != "" {
			s += "\nSETTINGS " + v
		}
	}
	if def.Comment != "" {
		s += "\nCOMMENT " + literal(def.Comment)
	}
	return []string{s}, nil
}

// samePrefix reports whether the primary key is where the sort order starts.
func samePrefix(order, pk []string) bool {
	if len(pk) > len(order) {
		return false
	}
	for i, k := range pk {
		if strings.Trim(order[i], "`") != k {
			return false
		}
	}
	return true
}

func (c *conn) AlterTableSQL(from *driver.Table, to driver.TableDef) ([]string, error) {
	if from == nil {
		return nil, errors.New("current table definition is missing")
	}
	from, to = driver.FollowRenames(from, to)
	target := "ALTER TABLE " + qualify(from.Ref.Database, from.Ref.Name) + " "
	var out []string

	if e := strings.TrimSpace(to.Options["engine"]); e != "" && e != from.Options["engine"] {
		return nil, errors.New("the engine of an existing table can't change: create a new table and copy the rows into it")
	}
	if strings.TrimSpace(to.Options["partition_by"]) != strings.TrimSpace(from.Options["partition_by"]) {
		return nil, errors.New("PARTITION BY can't change on an existing table: create a new table and copy the rows into it")
	}

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
	fromIx := map[string]driver.Index{}
	for _, ix := range skipIndexes(from.Indexes) {
		fromIx[ix.Name] = ix
	}
	toIx := map[string]driver.Index{}
	for _, ix := range skipIndexes(to.Indexes) {
		toIx[ix.Name] = ix
	}
	// Indexes go before the columns they use.
	for name, ix := range fromIx {
		if n, ok := toIx[name]; !ok || !sameIndex(ix, n) {
			out = append(out, target+"DROP INDEX "+quote(name))
		}
	}
	for _, col := range from.Columns {
		if !kept[col.Name] {
			out = append(out, target+"DROP COLUMN "+quote(col.Name))
		}
	}

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
		position := " FIRST"
		if prev != "" {
			position = " AFTER " + quote(prev)
		}
		prev = col.Name
		if col.OriginalName == "" {
			d, err := columnDef(col.Column)
			if err != nil {
				return nil, err
			}
			out = append(out, target+"ADD COLUMN "+d+position)
			continue
		}
		old := current[col.OriginalName]
		name := col.OriginalName
		if col.Name != col.OriginalName {
			out = append(out, target+"RENAME COLUMN "+quote(col.OriginalName)+" TO "+quote(col.Name))
			name = col.Name
		}
		typeChanged := fullType(old) != fullType(col.Column) || old.Codec != col.Codec
		exprChanged := old.Generated != col.Generated || old.GeneratedStored != col.GeneratedStored || strPtr(old.Default) != strPtr(col.Default)
		switch {
		case typeChanged || exprChanged || moved[col.OriginalName]:
			c2 := col.Column
			c2.Name = name
			c2.Comment = "" // comments change on their own
			d, err := columnDef(c2)
			if err != nil {
				return nil, err
			}
			if strPtr(old.Default) != "" && strPtr(col.Default) == "" && col.Generated == "" {
				out = append(out, target+"MODIFY COLUMN "+quote(name)+" REMOVE DEFAULT")
			}
			pos := ""
			if moved[col.OriginalName] {
				pos = position
			}
			out = append(out, target+"MODIFY COLUMN "+d+pos)
		}
		if old.Comment != col.Comment {
			out = append(out, target+"COMMENT COLUMN "+quote(name)+" "+literal(col.Comment))
		}
	}

	for name, ix := range toIx {
		if o, ok := fromIx[name]; ok && sameIndex(o, ix) {
			continue
		}
		d, err := indexDef(ix)
		if err != nil {
			return nil, err
		}
		// New indexes cover only new data until they're built for the old.
		out = append(out, target+"ADD "+d, target+"MATERIALIZE INDEX "+quote(name))
	}
	if o, n := strings.TrimSpace(from.Options["order_by"]), strings.TrimSpace(to.Options["order_by"]); n != "" && o != n {
		out = append(out, target+"MODIFY ORDER BY "+keyList(n))
	}
	if o, n := strings.TrimSpace(from.Options["ttl"]), strings.TrimSpace(to.Options["ttl"]); o != n {
		if n == "" {
			out = append(out, target+"REMOVE TTL")
		} else {
			out = append(out, target+"MODIFY TTL "+n)
		}
	}
	if o, n := strings.TrimSpace(from.Options["settings"]), strings.TrimSpace(to.Options["settings"]); n != "" && o != n {
		out = append(out, target+"MODIFY SETTING "+n)
	}
	if from.Comment != to.Comment {
		out = append(out, target+"MODIFY COMMENT "+literal(to.Comment))
	}
	if to.Ref.Name != "" && to.Ref.Name != from.Ref.Name {
		out = append(out, "RENAME TABLE "+qualify(from.Ref.Database, from.Ref.Name)+" TO "+qualify(from.Ref.Database, to.Ref.Name))
	}
	return out, nil
}

func strPtr(p *string) string {
	if p == nil {
		return ""
	}
	return strings.TrimSpace(*p)
}

func sameIndex(a, b driver.Index) bool {
	return strings.Join(a.Columns, ",") == strings.Join(b.Columns, ",") && strings.TrimSpace(a.Type) == strings.TrimSpace(b.Type) &&
		strings.TrimSpace(a.Definition) == strings.TrimSpace(b.Definition)
}

func (c *conn) DropObjectSQL(ref driver.ObjectRef, _ bool) ([]string, error) {
	what := "TABLE"
	switch ref.Kind {
	case "view", "materialized_view":
		what = "VIEW"
	case "dictionary":
		what = "DICTIONARY"
	}
	return []string{"DROP " + what + " " + qualify(ref.Database, ref.Name)}, nil
}

func (c *conn) TruncateSQL(ref driver.ObjectRef) ([]string, error) {
	return []string{"TRUNCATE TABLE " + qualify(ref.Database, ref.Name)}, nil
}

func (c *conn) RenameObjectSQL(ref driver.ObjectRef, newName string) ([]string, error) {
	if strings.TrimSpace(newName) == "" {
		return nil, errors.New("enter a new name")
	}
	what := "TABLE"
	if ref.Kind == "dictionary" {
		what = "DICTIONARY"
	}
	return []string{"RENAME " + what + " " + qualify(ref.Database, ref.Name) + " TO " + qualify(ref.Database, newName)}, nil
}

func (c *conn) CreateDatabaseSQL(name string, opts map[string]string) ([]string, error) {
	if strings.TrimSpace(name) == "" {
		return nil, errors.New("enter a name")
	}
	s := "CREATE DATABASE " + quote(name)
	if e := strings.TrimSpace(opts["engine"]); e != "" {
		s += " ENGINE = " + e
	}
	if cm := strings.TrimSpace(opts["comment"]); cm != "" {
		s += " COMMENT " + literal(cm)
	}
	return []string{s}, nil
}

func (c *conn) DropDatabaseSQL(name string, _ bool) ([]string, error) {
	return []string{"DROP DATABASE " + quote(name)}, nil
}
