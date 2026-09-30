package mssql

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"rowsmith/internal/driver"
)

// DDL generation (driver.DDLGenerator, driver.SchemaDDL). Statements are
// previewed by the UI and run as separate batches (GO between them) in a
// console session opened in the table's database, so names are qualified
// with the schema only. Nothing here touches the server.
//
// SQL Server refuses to alter, rename or drop a column while other objects
// depend on it (indexes, keys, checks, defaults, computed columns), so the
// diff drops those dependents first and re-creates them afterwards.

func tableName(r driver.ObjectRef) string { return qualify("", r.Schema, r.Name) }

// computed reports whether a column is a computed column. Describe fills
// Generated with a note for rowversion columns, which are not computed.
func computed(col driver.Column) bool {
	return strings.TrimSpace(col.Generated) != "" && normType(col.Type) != "rowversion"
}

// defaultOf returns the column default as raw SQL, or "" for none.
func defaultOf(col driver.Column) string {
	if col.Default == nil || col.AutoIncrement || computed(col) || normType(col.Type) == "rowversion" {
		return ""
	}
	return strings.TrimSpace(*col.Default)
}

// normType canonicalizes a declared type for comparison, so that equivalent
// spellings (NVARCHAR(50), datetime2 vs datetime2(7), timestamp) match.
func normType(t string) string {
	s := strings.ToLower(strings.Join(strings.Fields(t), ""))
	s = strings.NewReplacer("[", "", "]", "").Replace(s)
	name, args, _ := strings.Cut(s, "(")
	args = strings.TrimSuffix(args, ")")
	switch name {
	case "timestamp":
		return "rowversion"
	case "integer":
		name = "int"
	case "dec":
		name = "decimal"
	case "doubleprecision":
		return "float"
	case "character":
		name = "char"
	case "charactervarying", "charvarying":
		name = "varchar"
	case "nationalcharacter", "nationalchar":
		name = "nchar"
	case "nationalcharactervarying", "nationalcharvarying":
		name = "nvarchar"
	}
	switch name {
	case "decimal", "numeric":
		switch {
		case args == "":
			args = "18,0"
		case !strings.Contains(args, ","):
			args += ",0"
		}
	case "datetime2", "time", "datetimeoffset":
		if args == "" {
			args = "7"
		}
	case "char", "varchar", "nchar", "nvarchar", "binary", "varbinary":
		if args == "" {
			args = "1"
		}
	case "float":
		if n, err := strconv.Atoi(args); err == nil && n >= 1 && n <= 24 {
			return "real"
		}
		return "float"
	}
	if args == "" {
		return name
	}
	return name + "(" + args + ")"
}

// textTypes are the types a COLLATE clause applies to; SQL Server rejects
// it on alias types, which inherit their base type's collation.
var textTypes = map[string]bool{
	"char": true, "varchar": true, "nchar": true, "nvarchar": true, "text": true, "ntext": true, "sysname": true,
}

func takesCollation(col driver.Column) bool {
	base, _, _ := strings.Cut(normType(col.Type), "(")
	return textTypes[base]
}

func validCollation(name string) bool {
	for _, r := range name {
		if !(r == '_' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') {
			return false
		}
	}
	return name != ""
}

// typeSQL renders the declared type with its collation.
func typeSQL(col driver.Column) (string, error) {
	s := strings.TrimSpace(col.Type)
	if col.Collation != "" && takesCollation(col) {
		if !validCollation(col.Collation) {
			return "", fmt.Errorf("column %s: %q is not a valid collation name", col.Name, col.Collation)
		}
		s += " COLLATE " + col.Collation
	}
	return s, nil
}

func nullSQL(nullable bool) string {
	if nullable {
		return " NULL"
	}
	return " NOT NULL"
}

// columnSQL renders a column definition for CREATE TABLE and ADD. Columns of
// the primary key are always NOT NULL.
func columnSQL(col driver.Column, inPK bool) (string, error) {
	if strings.TrimSpace(col.Name) == "" {
		return "", errors.New("every column needs a name")
	}
	if computed(col) {
		s := quote(col.Name) + " AS (" + strings.TrimSpace(col.Generated) + ")"
		if col.GeneratedStored {
			s += " PERSISTED"
			if !col.Nullable || inPK {
				s += " NOT NULL"
			}
		}
		return s, nil
	}
	if strings.TrimSpace(col.Type) == "" {
		return "", fmt.Errorf("column %s needs a type", col.Name)
	}
	t, err := typeSQL(col)
	if err != nil {
		return "", err
	}
	s := quote(col.Name) + " " + t
	if col.AutoIncrement {
		s += " IDENTITY(1,1)"
	}
	s += nullSQL(col.Nullable && !inPK)
	if d := defaultOf(col); d != "" {
		s += " DEFAULT (" + d + ")"
	}
	return s, nil
}

// canonExpr normalizes an expression for comparison: SQL Server stores
// defaults, checks and filters wrapped in parentheses ("((0))").
func canonExpr(s string) string {
	s = unwrapParens(s)
	if !strings.ContainsRune(s, '\'') {
		s = strings.ToLower(s)
	}
	return s
}

func sameExpr(a, b string) bool { return canonExpr(a) == canonExpr(b) }

// scanRefs walks an expression outside string literals and calls fn for
// every bracketed identifier, with its byte range and unescaped name.
func scanRefs(expr string, fn func(start, end int, name string)) {
	for i := 0; i < len(expr); i++ {
		switch expr[i] {
		case '\'':
			for i++; i < len(expr); i++ {
				if expr[i] == '\'' {
					if i+1 < len(expr) && expr[i+1] == '\'' {
						i++
						continue
					}
					break
				}
			}
		case '[':
			var b strings.Builder
			j := i + 1
			for ; j < len(expr); j++ {
				if expr[j] == ']' {
					if j+1 < len(expr) && expr[j+1] == ']' {
						b.WriteByte(']')
						j++
						continue
					}
					break
				}
				b.WriteByte(expr[j])
			}
			if j >= len(expr) {
				return // unterminated
			}
			fn(i, j+1, b.String())
			i = j
		}
	}
}

// refs returns the bracketed identifiers an expression uses. Definitions
// read back from SQL Server always bracket column names.
func refs(expr string) []string {
	var out []string
	scanRefs(expr, func(_, _ int, name string) { out = append(out, name) })
	return out
}

// mapRefs rewrites the bracketed identifiers of an expression.
func mapRefs(expr string, fn func(string) string) string {
	var b strings.Builder
	last := 0
	scanRefs(expr, func(start, end int, name string) {
		b.WriteString(expr[last:start])
		b.WriteString(quote(fn(name)))
		last = end
	})
	b.WriteString(expr[last:])
	return b.String()
}

// keyCols renders index key columns with their sort order.
func keyCols(cols []string, desc []bool) string {
	parts := make([]string, len(cols))
	for i, c := range cols {
		parts[i] = quote(c)
		if i < len(desc) && desc[i] {
			parts[i] += " DESC"
		}
	}
	return strings.Join(parts, ", ")
}

// normIndexType maps an index type to the lower-case form Describe reports.
func normIndexType(t string) string {
	t = strings.ToLower(strings.Join(strings.Fields(t), " "))
	if t == "" {
		return "nonclustered"
	}
	return t
}

func isClustered(t string) bool {
	t = normIndexType(t)
	return t == "clustered" || t == "clustered columnstore"
}

// isConstraint reports whether a described index backs a UNIQUE constraint
// (Describe scripts those as ALTER TABLE ... ADD CONSTRAINT).
func isConstraint(ix driver.Index) bool { return strings.HasPrefix(ix.Definition, "ALTER TABLE ") }

// includedColumns reads the INCLUDE list from a described index definition.
func includedColumns(ix driver.Index) []string {
	_, rest, ok := strings.Cut(ix.Definition, ") INCLUDE (")
	if !ok {
		return nil
	}
	var out []string
	depth := 0
	for i := 0; i < len(rest); i++ {
		switch rest[i] {
		case '[':
			name := strings.Builder{}
			j := i + 1
			for ; j < len(rest); j++ {
				if rest[j] == ']' {
					if j+1 < len(rest) && rest[j+1] == ']' {
						name.WriteByte(']')
						j++
						continue
					}
					break
				}
				name.WriteByte(rest[j])
			}
			out = append(out, name.String())
			i = j
		case '(':
			depth++
		case ')':
			if depth == 0 {
				return out
			}
			depth--
		}
	}
	return out
}

// indexSQL renders the statement that creates an index. asConstraint
// re-creates a UNIQUE constraint as one.
func indexSQL(table string, ix driver.Index, included []string, asConstraint bool) (string, error) {
	if strings.TrimSpace(ix.Name) == "" {
		return "", errors.New("every index needs a name")
	}
	typ := normIndexType(ix.Type)
	if len(ix.Columns) == 0 && typ != "clustered columnstore" {
		return "", fmt.Errorf("index %s needs at least one column", ix.Name)
	}
	where := strings.TrimSpace(ix.Where)
	switch typ {
	case "clustered", "nonclustered":
		if asConstraint && ix.Unique && where == "" && len(included) == 0 {
			return "ALTER TABLE " + table + " ADD CONSTRAINT " + quote(ix.Name) + " UNIQUE " + strings.ToUpper(typ) +
				" (" + keyCols(ix.Columns, ix.Desc) + ")", nil
		}
		s := "CREATE "
		if ix.Unique {
			s += "UNIQUE "
		}
		if strings.TrimSpace(ix.Type) != "" {
			s += strings.ToUpper(typ) + " "
		}
		s += "INDEX " + quote(ix.Name) + " ON " + table + " (" + keyCols(ix.Columns, ix.Desc) + ")"
		if len(included) > 0 {
			s += " INCLUDE (" + nameList(included) + ")"
		}
		if where != "" {
			s += " WHERE " + where
		}
		return s, nil
	case "nonclustered columnstore", "clustered columnstore":
		if ix.Unique {
			return "", fmt.Errorf("index %s: columnstore indexes cannot be unique", ix.Name)
		}
		s := "CREATE " + strings.ToUpper(typ) + " INDEX " + quote(ix.Name) + " ON " + table
		if typ == "nonclustered columnstore" {
			s += " (" + nameList(ix.Columns) + ")"
		}
		if where != "" {
			s += " WHERE " + where
		}
		return s, nil
	}
	return "", fmt.Errorf("index %s: %s indexes cannot be created here; use the console", ix.Name, typ)
}

// fkRule normalizes an ON DELETE / ON UPDATE rule.
func fkRule(r string) string {
	r = strings.ToUpper(strings.Join(strings.Fields(r), " "))
	if r == "" || r == "RESTRICT" {
		return "NO ACTION"
	}
	return r
}

func fkSQL(fk driver.ForeignKey, refTable string) (string, error) {
	if strings.TrimSpace(fk.Name) == "" {
		return "", errors.New("every foreign key needs a name")
	}
	if len(fk.Columns) == 0 || len(fk.Columns) != len(fk.RefColumns) {
		return "", fmt.Errorf("foreign key %s needs matching columns on both sides", fk.Name)
	}
	s := "CONSTRAINT " + quote(fk.Name) + " FOREIGN KEY (" + nameList(fk.Columns) + ") REFERENCES " + refTable + " (" + nameList(fk.RefColumns) + ")"
	for _, r := range []struct{ on, rule string }{{"DELETE", fk.OnDelete}, {"UPDATE", fk.OnUpdate}} {
		switch rule := fkRule(r.rule); rule {
		case "NO ACTION":
		case "CASCADE", "SET NULL", "SET DEFAULT":
			s += " ON " + r.on + " " + rule
		default:
			return "", fmt.Errorf("foreign key %s: SQL Server does not support ON %s %s", fk.Name, r.on, rule)
		}
	}
	return s, nil
}

func checkSQL(ch driver.Check, expr string) (string, error) {
	if strings.TrimSpace(ch.Name) == "" {
		return "", errors.New("every check constraint needs a name")
	}
	if strings.TrimSpace(expr) == "" {
		return "", fmt.Errorf("check constraint %s needs an expression", ch.Name)
	}
	return "CONSTRAINT " + quote(ch.Name) + " CHECK (" + strings.TrimSpace(expr) + ")", nil
}

// commentSQL adds, updates or drops the MS_Description extended property of
// a table (column "") or column.
func commentSQL(proc string, t driver.ObjectRef, column, text string) string {
	s := "EXEC sys." + proc + " @name = N'MS_Description', "
	if proc != "sp_dropextendedproperty" {
		s += "@value = " + literal(text) + ", "
	}
	s += "@level0type = N'SCHEMA', @level0name = " + literal(schemaName(t.Schema)) + ", @level1type = N'TABLE', @level1name = " + literal(t.Name)
	if column != "" {
		s += ", @level2type = N'COLUMN', @level2name = " + literal(column)
	}
	return s
}

func commentChange(t driver.ObjectRef, column, old, text string) []string {
	switch {
	case old == text:
		return nil
	case old == "":
		return []string{commentSQL("sp_addextendedproperty", t, column, text)}
	case text == "":
		return []string{commentSQL("sp_dropextendedproperty", t, column, "")}
	}
	return []string{commentSQL("sp_updateextendedproperty", t, column, text)}
}

// dropDefaultSQL drops a column's default constraint, whose name Describe
// does not report, as one self-contained batch.
func dropDefaultSQL(table, column string) string {
	return "DECLARE @sql nvarchar(max) = (SELECT " + literal("ALTER TABLE "+table+" DROP CONSTRAINT ") + " + QUOTENAME(dc.name)\n" +
		"  FROM sys.default_constraints dc JOIN sys.columns c ON c.object_id = dc.parent_object_id AND c.column_id = dc.parent_column_id\n" +
		"  WHERE dc.parent_object_id = OBJECT_ID(" + literal(table) + ") AND c.name = " + literal(column) + ");\n" +
		"IF @sql IS NOT NULL EXEC sp_executesql @sql"
}

func renameSQL(object, newName, kind string) string {
	s := "EXEC sp_rename " + literal(object) + ", " + literal(newName)
	if kind != "" {
		s += ", " + literal(kind)
	}
	return s
}

func (c *conn) CreateTableSQL(def driver.TableDef) ([]string, error) {
	if strings.TrimSpace(def.Ref.Name) == "" {
		return nil, errors.New("give the table a name")
	}
	if len(def.Columns) == 0 {
		return nil, errors.New("a table needs at least one column")
	}
	table := tableName(def.Ref)
	names := map[string]bool{}
	seen := map[string]bool{}
	pk := map[string]bool{}
	for _, n := range def.PrimaryKey {
		pk[n] = true
	}
	var lines []string
	for _, col := range def.Columns {
		d, err := columnSQL(col.Column, pk[col.Name])
		if err != nil {
			return nil, err
		}
		if seen[strings.ToLower(col.Name)] {
			return nil, fmt.Errorf("column %s is listed twice", col.Name)
		}
		seen[strings.ToLower(col.Name)] = true
		names[col.Name] = true
		lines = append(lines, d)
	}
	known := func(what string, cols []string) error {
		for _, n := range cols {
			if !names[n] {
				return fmt.Errorf("%s uses column %s, which is not in the table", what, n)
			}
		}
		return nil
	}
	if err := known("the primary key", def.PrimaryKey); err != nil {
		return nil, err
	}
	clustered := false
	for _, ix := range def.Indexes {
		if !ix.Primary && isClustered(ix.Type) {
			clustered = true
		}
	}
	if len(def.PrimaryKey) > 0 {
		name, kind := "PK_"+def.Ref.Name, ""
		for _, ix := range def.Indexes {
			if ix.Primary {
				if ix.Name != "" {
					name = ix.Name
				}
				if ix.Type != "" {
					kind = " " + strings.ToUpper(normIndexType(ix.Type))
				}
			}
		}
		if clustered {
			kind = " NONCLUSTERED"
		}
		lines = append(lines, "CONSTRAINT "+quote(name)+" PRIMARY KEY"+kind+" ("+nameList(def.PrimaryKey)+")")
	}
	for _, ch := range def.Checks {
		s, err := checkSQL(ch, ch.Expression)
		if err != nil {
			return nil, err
		}
		lines = append(lines, s)
	}
	for _, fk := range def.ForeignKeys {
		if err := known("foreign key "+fk.Name, fk.Columns); err != nil {
			return nil, err
		}
		ref := fk.RefTable
		if ref.Schema == "" {
			ref.Schema = def.Ref.Schema
		}
		s, err := fkSQL(fk, tableName(ref))
		if err != nil {
			return nil, err
		}
		lines = append(lines, s)
	}
	out := []string{"CREATE TABLE " + table + " (\n  " + strings.Join(lines, ",\n  ") + "\n)"}
	for _, ix := range def.Indexes {
		if ix.Primary {
			continue
		}
		if err := known("index "+ix.Name, ix.Columns); err != nil {
			return nil, err
		}
		s, err := indexSQL(table, ix, nil, false)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	if def.Comment != "" {
		out = append(out, commentSQL("sp_addextendedproperty", def.Ref, "", def.Comment))
	}
	for _, col := range def.Columns {
		if col.Comment != "" {
			out = append(out, commentSQL("sp_addextendedproperty", def.Ref, col.Name, col.Comment))
		}
	}
	return out, nil
}

// alterPlan holds the column mapping between the current table and the
// edited definition.
type alterPlan struct {
	from    *driver.Table
	to      driver.TableDef
	table   string                   // qualified name before the change
	cur     map[string]driver.Column // current columns by name
	renamed map[string]string        // current name → new name
	dropped map[string]bool          // current columns that go away
	toNames map[string]bool          // column names of the new definition
}

// toCol maps a column name used by the new definition to the column's name
// after the change: names of the new definition stand, while old names of
// renamed columns (an editor may keep them in indexes) follow the rename.
func (p *alterPlan) toCol(name string) string {
	if p.toNames[name] {
		return name
	}
	if n, ok := p.renamed[name]; ok {
		return n
	}
	return name
}

// fromCol maps a current column name to its name after the change.
func (p *alterPlan) fromCol(name string) string {
	if n, ok := p.renamed[name]; ok {
		return n
	}
	return name
}

func mapNames(names []string, fn func(string) string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = fn(n)
	}
	return out
}

// missing returns the names that are not columns of the new definition.
func (p *alterPlan) missing(names []string) []string {
	var out []string
	for _, n := range names {
		if !p.toNames[n] {
			out = append(out, n)
		}
	}
	return out
}

// goneWith decides what happens to an existing object of the new definition
// that uses columns missing from it: when they are all being dropped, the
// object goes with them (skip = true); otherwise that is an error.
func (p *alterPlan) goneWith(what string, missing []string, existed bool) (skip bool, err error) {
	if len(missing) == 0 {
		return false, nil
	}
	for _, n := range missing {
		if !existed || !p.dropped[n] {
			return false, fmt.Errorf("%s uses column %s, which is not in the table", what, n)
		}
	}
	return true, nil
}

func (p *alterPlan) isSelf(ref driver.ObjectRef) bool {
	s := ref.Schema
	if s == "" {
		s = p.from.Ref.Schema
	}
	return strings.EqualFold(schemaName(s), schemaName(p.from.Ref.Schema)) &&
		(strings.EqualFold(ref.Name, p.from.Ref.Name) || p.to.Ref.Name != "" && strings.EqualFold(ref.Name, p.to.Ref.Name))
}

func (p *alterPlan) ixKey(ix driver.Index, cols func(string) string) string {
	desc := make([]bool, len(ix.Columns))
	copy(desc, ix.Desc)
	return fmt.Sprint(ix.Unique, "|", normIndexType(ix.Type), "|", mapNames(ix.Columns, cols), desc, "|",
		canonExpr(mapRefs(ix.Where, cols)))
}

func (p *alterPlan) fkKey(fk driver.ForeignKey, cols func(string) string) string {
	ref := fk.RefTable
	refCols := fk.RefColumns
	if p.isSelf(ref) {
		ref = p.from.Ref
		refCols = mapNames(refCols, cols)
	} else if ref.Schema == "" {
		ref.Schema = p.from.Ref.Schema
	}
	return fmt.Sprint(mapNames(fk.Columns, cols), "|", strings.ToLower(schemaName(ref.Schema)), ".", strings.ToLower(ref.Name),
		refCols, "|", fkRule(fk.OnDelete), "|", fkRule(fk.OnUpdate))
}

func anyIn(names []string, sets ...map[string]bool) bool {
	for _, n := range names {
		for _, s := range sets {
			if s[n] {
				return true
			}
		}
	}
	return false
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x, y := slices.Clone(a), slices.Clone(b)
	slices.Sort(x)
	slices.Sort(y)
	return slices.Equal(x, y)
}

type colChange struct {
	def      driver.ColumnDef
	old      driver.Column
	recreate bool // computed column: dropped and added again
	alter    bool // ALTER COLUMN
	dropDef  bool
	addDef   bool
}

// AlterTableSQL diffs the current table against the edited definition.
// Columns are matched by OriginalName; unmatched current columns are dropped.
// Indexes, keys and checks are matched by name and re-created when changed
// or when a column they depend on is altered.
func (c *conn) AlterTableSQL(from *driver.Table, to driver.TableDef) ([]string, error) {
	if from == nil {
		return nil, errors.New("current table definition is missing")
	}
	if from.Kind != "" && from.Kind != "table" {
		return nil, fmt.Errorf("%s is a %s; only tables can be changed here", from.Ref.Name, from.Kind)
	}
	if len(to.Columns) == 0 {
		return nil, errors.New("a table needs at least one column")
	}
	p := &alterPlan{from: from, to: to, table: tableName(from.Ref), cur: map[string]driver.Column{},
		renamed: map[string]string{}, dropped: map[string]bool{}, toNames: map[string]bool{}}
	for _, col := range from.Columns {
		p.cur[col.Name] = col
	}
	kept := map[string]bool{}
	seen := map[string]bool{}
	for _, col := range to.Columns {
		if strings.TrimSpace(col.Name) == "" {
			return nil, errors.New("every column needs a name")
		}
		if seen[strings.ToLower(col.Name)] {
			return nil, fmt.Errorf("column %s is listed twice", col.Name)
		}
		seen[strings.ToLower(col.Name)] = true
		p.toNames[col.Name] = true
		if col.OriginalName == "" {
			continue
		}
		if _, ok := p.cur[col.OriginalName]; !ok {
			return nil, fmt.Errorf("column %s no longer exists; reload the structure", col.OriginalName)
		}
		if kept[col.OriginalName] {
			return nil, fmt.Errorf("column %s is listed twice", col.OriginalName)
		}
		kept[col.OriginalName] = true
		if col.Name != col.OriginalName {
			p.renamed[col.OriginalName] = col.Name
		}
	}
	for _, col := range from.Columns {
		if !kept[col.Name] {
			p.dropped[col.Name] = true
		}
	}
	finalName := from.Ref.Name
	if to.Ref.Name != "" {
		finalName = to.Ref.Name
	}

	toPK := mapNames(to.PrimaryKey, p.toCol)
	inPK := map[string]bool{}
	for _, n := range toPK {
		inPK[n] = true
	}

	// Columns. Sets are keyed by current names.
	var changes []*colChange
	altered, recreated := map[string]bool{}, map[string]bool{}
	for _, col := range to.Columns {
		if col.OriginalName == "" {
			if _, err := columnSQL(col.Column, inPK[col.Name]); err != nil {
				return nil, err
			}
			continue
		}
		old := p.cur[col.OriginalName]
		ch := &colChange{def: col, old: old}
		nullable := col.Nullable && !inPK[col.Name]
		oc, nc := computed(old), computed(col.Column)
		if oc || nc {
			ch.recreate = oc != nc || !sameExpr(mapRefs(old.Generated, p.fromCol), mapRefs(col.Generated, p.toCol)) ||
				old.GeneratedStored != col.GeneratedStored || col.GeneratedStored && old.Nullable != nullable
		} else {
			if col.AutoIncrement != old.AutoIncrement {
				if col.AutoIncrement {
					return nil, fmt.Errorf("SQL Server cannot add IDENTITY to the existing column %s; add a new column instead", col.OriginalName)
				}
				return nil, fmt.Errorf("SQL Server cannot remove IDENTITY from the existing column %s; add a new column instead", col.OriginalName)
			}
			typeChanged := normType(old.Type) != normType(col.Type) ||
				takesCollation(col.Column) && !strings.EqualFold(old.Collation, col.Collation)
			ch.alter = typeChanged || nullable != old.Nullable
			defChanged := !sameExpr(defaultOf(old), defaultOf(col.Column))
			ch.dropDef = defaultOf(old) != "" && (defChanged || typeChanged)
			ch.addDef = defaultOf(col.Column) != "" && (defChanged || ch.dropDef)
		}
		if _, err := columnSQL(col.Column, inPK[col.Name]); err != nil {
			return nil, err
		}
		if ch.alter {
			altered[old.Name] = true
		}
		changes = append(changes, ch)
	}
	// Renames that SQL Server refuses while an object depends on the column.
	renamedCols := map[string]bool{}
	for old := range p.renamed {
		renamedCols[old] = true
	}
	for _, ch := range changes {
		if !ch.recreate && computed(ch.old) && anyIn(refs(ch.old.Generated), altered, renamedCols) {
			ch.recreate = true // a computed column blocks changes to the columns it reads
		}
		if ch.recreate {
			recreated[ch.old.Name] = true
		}
	}
	for _, ch := range changes {
		if !computed(ch.old) {
			continue
		}
		expr := ch.old.Generated
		if ch.recreate && computed(ch.def.Column) {
			expr = mapRefs(ch.def.Generated, p.toCol)
		} else if ch.recreate {
			continue
		}
		for _, r := range refs(expr) {
			if p.dropped[r] && !p.toNames[r] {
				return nil, fmt.Errorf("computed column %s uses column %s, which is being dropped", ch.def.Name, r)
			}
		}
	}
	// hard: columns whose dependents must all be dropped and re-created.
	hard := map[string]bool{}
	for _, s := range []map[string]bool{altered, recreated, p.dropped} {
		for k := range s {
			hard[k] = true
		}
	}

	type key struct {
		what string
		cols []string // current names
	}
	var droppedKeys []key

	// Primary key.
	var fromPK *driver.Index
	for i := range from.Indexes {
		if from.Indexes[i].Primary {
			fromPK = &from.Indexes[i]
		}
	}
	pkChanged := !slices.Equal(mapNames(from.PrimaryKey, p.fromCol), toPK)
	pkDrop := len(from.PrimaryKey) > 0 && (pkChanged || anyIn(from.PrimaryKey, hard))
	pkAdd := len(toPK) > 0 && (pkChanged || pkDrop)
	if pkAdd {
		skip, err := p.goneWith("the primary key", p.missing(toPK), len(from.PrimaryKey) > 0)
		if err != nil {
			return nil, err
		}
		pkAdd = !skip
	}
	if pkDrop {
		droppedKeys = append(droppedKeys, key{"the primary key", from.PrimaryKey})
	}

	// Indexes.
	toIx := map[string]driver.Index{}
	for _, ix := range to.Indexes {
		if ix.Primary {
			continue
		}
		if strings.TrimSpace(ix.Name) == "" {
			return nil, errors.New("every index needs a name")
		}
		if _, dup := toIx[ix.Name]; dup {
			return nil, fmt.Errorf("index %s is listed twice", ix.Name)
		}
		toIx[ix.Name] = ix
	}
	fromIx := map[string]driver.Index{}
	var dropIx []driver.Index
	for _, ix := range from.Indexes {
		if ix.Primary {
			continue
		}
		fromIx[ix.Name] = ix
		n, ok := toIx[ix.Name]
		if !ok || p.ixKey(ix, p.fromCol) != p.ixKey(n, p.toCol) || anyIn(ix.Columns, hard) || anyIn(includedColumns(ix), hard) ||
			anyIn(refs(ix.Where), hard, renamedCols) {
			dropIx = append(dropIx, ix)
			if ix.Unique {
				droppedKeys = append(droppedKeys, key{"index " + ix.Name, ix.Columns})
			}
		}
	}
	var createIx []string
	toHasClustered := false
	for _, ix := range to.Indexes {
		if !ix.Primary && isClustered(ix.Type) {
			toHasClustered = true
		}
	}
	for _, ix := range to.Indexes {
		if ix.Primary {
			continue
		}
		old, existed := fromIx[ix.Name]
		if existed && !slices.ContainsFunc(dropIx, func(d driver.Index) bool { return d.Name == ix.Name }) {
			continue
		}
		ix.Columns = mapNames(ix.Columns, p.toCol)
		ix.Where = mapRefs(ix.Where, p.toCol)
		skip, err := p.goneWith("index "+ix.Name, append(p.missing(ix.Columns), refsDropped(refs(ix.Where), p)...), existed)
		if err != nil {
			return nil, err
		}
		if skip {
			continue
		}
		var included []string
		if existed {
			for _, n := range includedColumns(old) {
				if p.dropped[n] {
					continue
				}
				if n = p.fromCol(n); !slices.Contains(ix.Columns, n) {
					included = append(included, n)
				}
			}
		}
		s, err := indexSQL(p.table, ix, included, existed && isConstraint(old))
		if err != nil {
			return nil, err
		}
		createIx = append(createIx, s)
	}

	// Check constraints.
	toCk := map[string]driver.Check{}
	for _, ch := range to.Checks {
		if strings.TrimSpace(ch.Name) == "" {
			return nil, errors.New("every check constraint needs a name")
		}
		if _, dup := toCk[ch.Name]; dup {
			return nil, fmt.Errorf("check constraint %s is listed twice", ch.Name)
		}
		toCk[ch.Name] = ch
	}
	fromCk := map[string]bool{}
	dropCk := map[string]bool{}
	for _, ch := range from.Checks {
		fromCk[ch.Name] = true
		n, ok := toCk[ch.Name]
		if !ok || !sameExpr(mapRefs(ch.Expression, p.fromCol), mapRefs(n.Expression, p.toCol)) || anyIn(refs(ch.Expression), hard, renamedCols) {
			dropCk[ch.Name] = true
		}
	}
	var addCk []string
	for _, ch := range to.Checks {
		if fromCk[ch.Name] && !dropCk[ch.Name] {
			continue
		}
		expr := mapRefs(ch.Expression, p.toCol)
		skip, err := p.goneWith("check constraint "+ch.Name, refsDropped(refs(expr), p), fromCk[ch.Name])
		if err != nil {
			return nil, err
		}
		if skip {
			continue
		}
		s, err := checkSQL(ch, expr)
		if err != nil {
			return nil, err
		}
		addCk = append(addCk, "ALTER TABLE "+p.table+" ADD "+s)
	}

	// Foreign keys, after the keys they may reference are known.
	for _, k := range droppedKeys {
		for _, fk := range from.Referenced {
			if fk.Table != nil && !p.isSelf(*fk.Table) && sameSet(fk.RefColumns, k.cols) {
				return nil, fmt.Errorf("foreign key %s on %s.%s references %s; drop that foreign key first",
					fk.Name, schemaName(fk.Table.Schema), fk.Table.Name, k.what)
			}
		}
	}
	toFK := map[string]driver.ForeignKey{}
	for _, fk := range to.ForeignKeys {
		if strings.TrimSpace(fk.Name) == "" {
			return nil, errors.New("every foreign key needs a name")
		}
		if _, dup := toFK[fk.Name]; dup {
			return nil, fmt.Errorf("foreign key %s is listed twice", fk.Name)
		}
		toFK[fk.Name] = fk
	}
	fromFK := map[string]bool{}
	dropFK := map[string]bool{}
	var dropFKs []string
	for _, fk := range from.ForeignKeys {
		fromFK[fk.Name] = true
		n, ok := toFK[fk.Name]
		self := p.isSelf(fk.RefTable)
		refsDroppedKey := self && slices.ContainsFunc(droppedKeys, func(k key) bool { return sameSet(fk.RefColumns, k.cols) })
		if !ok || p.fkKey(fk, p.fromCol) != p.fkKey(n, p.toCol) || anyIn(fk.Columns, hard) || self && anyIn(fk.RefColumns, hard) || refsDroppedKey {
			dropFK[fk.Name] = true
			dropFKs = append(dropFKs, "ALTER TABLE "+p.table+" DROP CONSTRAINT "+quote(fk.Name))
		}
	}
	var addFK []string
	for _, fk := range to.ForeignKeys {
		if fromFK[fk.Name] && !dropFK[fk.Name] {
			continue
		}
		fk.Columns = mapNames(fk.Columns, p.toCol)
		used := slices.Clone(fk.Columns)
		ref := fk.RefTable
		if p.isSelf(ref) {
			ref = from.Ref // keys are added before the table is renamed
			fk.RefColumns = mapNames(fk.RefColumns, p.toCol)
			used = append(used, fk.RefColumns...)
		} else if ref.Schema == "" {
			ref.Schema = from.Ref.Schema
		}
		skip, err := p.goneWith("foreign key "+fk.Name, p.missing(used), fromFK[fk.Name])
		if err != nil {
			return nil, err
		}
		if skip {
			continue
		}
		s, err := fkSQL(fk, tableName(ref))
		if err != nil {
			return nil, err
		}
		addFK = append(addFK, "ALTER TABLE "+p.table+" ADD "+s)
	}

	// Emit: drop dependents, change columns, re-create dependents.
	alter := func(clause string) string { return "ALTER TABLE " + p.table + " " + clause }
	out := dropFKs
	for _, ch := range from.Checks {
		if dropCk[ch.Name] {
			out = append(out, alter("DROP CONSTRAINT "+quote(ch.Name)))
		}
	}
	for _, ix := range dropIx {
		if isConstraint(ix) {
			out = append(out, alter("DROP CONSTRAINT "+quote(ix.Name)))
		} else {
			out = append(out, "DROP INDEX "+quote(ix.Name)+" ON "+p.table)
		}
	}
	if pkDrop {
		if fromPK == nil || fromPK.Name == "" {
			return nil, errors.New("the primary key constraint name is unknown; reload the structure")
		}
		out = append(out, alter("DROP CONSTRAINT "+quote(fromPK.Name)))
	}
	dropColumn := func(col driver.Column) {
		if defaultOf(col) != "" {
			out = append(out, dropDefaultSQL(p.table, col.Name))
		}
		out = append(out, alter("DROP COLUMN "+quote(col.Name)))
	}
	for _, ch := range changes {
		if ch.recreate {
			dropColumn(ch.old)
		}
	}
	for _, col := range from.Columns {
		if p.dropped[col.Name] {
			dropColumn(col)
		}
	}
	out = append(out, p.renames(changes)...)
	for _, ch := range changes {
		if ch.recreate {
			continue
		}
		name := ch.def.Name
		if ch.dropDef {
			out = append(out, dropDefaultSQL(p.table, name))
		}
		if ch.alter {
			t, _ := typeSQL(ch.def.Column)
			out = append(out, alter("ALTER COLUMN "+quote(name)+" "+t+nullSQL(ch.def.Nullable && !inPK[name])))
		}
		if ch.addDef {
			out = append(out, alter("ADD DEFAULT ("+defaultOf(ch.def.Column)+") FOR "+quote(name)))
		}
	}
	// New columns, then computed ones, which may read them.
	recreatedDef := map[string]bool{}
	for _, ch := range changes {
		if ch.recreate {
			recreatedDef[ch.def.Name] = true
		}
	}
	for _, pass := range []bool{false, true} {
		for _, col := range to.Columns {
			if (col.OriginalName == "" || recreatedDef[col.Name]) && computed(col.Column) == pass {
				c := col.Column
				c.Generated = mapRefs(c.Generated, p.toCol)
				d, _ := columnSQL(c, inPK[col.Name])
				out = append(out, alter("ADD "+d))
			}
		}
	}
	if pkAdd {
		name, kind := "PK_"+finalName, "CLUSTERED"
		var desc []bool
		if fromPK != nil {
			name, kind = fromPK.Name, strings.ToUpper(normIndexType(fromPK.Type))
			if !pkChanged {
				desc = fromPK.Desc
			}
		}
		if toHasClustered {
			kind = "NONCLUSTERED"
		}
		out = append(out, alter("ADD CONSTRAINT "+quote(name)+" PRIMARY KEY "+kind+" ("+keyCols(toPK, desc)+")"))
	}
	out = append(out, createIx...)
	out = append(out, addCk...)
	out = append(out, addFK...)
	out = append(out, commentChange(from.Ref, "", from.Comment, to.Comment)...)
	for _, col := range to.Columns {
		switch {
		case col.OriginalName == "" || recreatedDef[col.Name]:
			out = append(out, commentChange(from.Ref, col.Name, "", col.Comment)...)
		default:
			out = append(out, commentChange(from.Ref, col.Name, p.cur[col.OriginalName].Comment, col.Comment)...)
		}
	}
	if to.Ref.Name != "" && to.Ref.Name != from.Ref.Name {
		out = append(out, renameSQL(p.table, to.Ref.Name, ""))
	}
	return out, nil
}

// refsDropped returns the referenced names that are columns being dropped.
func refsDropped(names []string, p *alterPlan) []string {
	var out []string
	for _, n := range names {
		if p.dropped[n] && !p.toNames[n] {
			out = append(out, n)
		}
	}
	return out
}

// renames renames kept columns. When names are swapped, every column first
// moves to a temporary name.
func (p *alterPlan) renames(changes []*colChange) []string {
	type rn struct{ old, new string }
	var list []rn
	for _, ch := range changes {
		if !ch.recreate && ch.def.Name != ch.old.Name {
			list = append(list, rn{ch.old.Name, ch.def.Name})
		}
	}
	swap := false
	for _, a := range list {
		for _, b := range list {
			if a.old != b.old && strings.EqualFold(a.new, b.old) {
				swap = true
			}
		}
	}
	col := func(name string) string { return p.table + "." + quote(name) }
	var out []string
	if swap {
		for i, r := range list {
			tmp := "rowsmith_tmp_" + strconv.Itoa(i+1)
			out = append(out, renameSQL(col(r.old), tmp, "COLUMN"))
			list[i].old = tmp
		}
	}
	for _, r := range list {
		out = append(out, renameSQL(col(r.old), r.new, "COLUMN"))
	}
	return out
}

var dropKeyword = map[string]string{
	"table": "TABLE", "view": "VIEW", "procedure": "PROCEDURE", "function": "FUNCTION", "trigger": "TRIGGER",
	"sequence": "SEQUENCE", "synonym": "SYNONYM", "type": "TYPE",
}

func (c *conn) DropObjectSQL(ref driver.ObjectRef, cascade bool) ([]string, error) {
	kind := ref.Kind
	if kind == "" {
		kind = "table"
	}
	kw, ok := dropKeyword[kind]
	if !ok {
		return nil, fmt.Errorf("cannot drop objects of kind %q", ref.Kind)
	}
	if strings.TrimSpace(ref.Name) == "" {
		return nil, errors.New("choose an object to drop")
	}
	if cascade {
		return nil, errors.New("SQL Server has no CASCADE option; drop the dependent objects first")
	}
	return []string{"DROP " + kw + " " + tableName(ref)}, nil
}

func (c *conn) TruncateSQL(ref driver.ObjectRef) ([]string, error) {
	if ref.Kind != "" && ref.Kind != "table" {
		return nil, fmt.Errorf("only tables can be emptied, not a %s", ref.Kind)
	}
	return []string{"TRUNCATE TABLE " + tableName(ref)}, nil
}

func (c *conn) RenameObjectSQL(ref driver.ObjectRef, newName string) ([]string, error) {
	if strings.TrimSpace(newName) == "" {
		return nil, errors.New("enter a new name")
	}
	switch ref.Kind {
	case "", "table", "sequence", "synonym":
		return []string{renameSQL(tableName(ref), newName, "")}, nil
	case "type":
		return []string{renameSQL(tableName(ref), newName, "USERDATATYPE")}, nil
	case "view", "procedure", "function", "trigger":
		return nil, fmt.Errorf("renaming a %s would leave its stored definition under the old name; recreate it under the new name instead", ref.Kind)
	}
	return nil, fmt.Errorf("renaming a %s is not supported", ref.Kind)
}

func (c *conn) CreateDatabaseSQL(name string, opts map[string]string) ([]string, error) {
	if strings.TrimSpace(name) == "" {
		return nil, errors.New("enter a database name")
	}
	s := "CREATE DATABASE " + quote(name)
	if co := strings.TrimSpace(opts["collation"]); co != "" {
		if !validCollation(co) {
			return nil, fmt.Errorf("%q is not a valid collation name", co)
		}
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

// CreateSchemaSQL runs in the session's database: CREATE SCHEMA cannot name
// another one, and must be alone in its batch.
func (c *conn) CreateSchemaSQL(_ string, name string) ([]string, error) {
	if strings.TrimSpace(name) == "" {
		return nil, errors.New("enter a schema name")
	}
	return []string{"CREATE SCHEMA " + quote(name)}, nil
}

func (c *conn) DropSchemaSQL(_ string, name string, cascade bool) ([]string, error) {
	if strings.TrimSpace(name) == "" {
		return nil, errors.New("choose a schema")
	}
	if cascade {
		return nil, errors.New("SQL Server cannot drop a schema together with its objects; drop or move them first")
	}
	return []string{"DROP SCHEMA " + quote(name)}, nil
}
