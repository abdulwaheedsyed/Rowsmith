package sqlite

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"rowsmith/internal/driver"
)

// DDL generation (driver.DDLGenerator). Nothing here changes the database:
// the UI previews the statements and runs them through the console.
//
// ALTER TABLE in SQLite renames tables and columns, adds and drops columns,
// sets and drops NOT NULL and adds and drops named CHECK constraints; such
// changes are made in place. Anything else rebuilds the table as described in
// https://www.sqlite.org/lang_altertable.html#otheralter: the rows are copied
// into a new table with the new structure, which then takes the old one's
// name and gets its indexes and triggers back.
//
// A rebuild creates the new table under the current column names, so that
// the saved index and trigger definitions still apply to it, and renames
// columns only at the end, with ALTER TABLE, which makes SQLite update every
// index, trigger, view and foreign key that mentions them.

var _ driver.DDLGenerator = (*conn)(nil)

// Temporary names: a rebuild creates the new table as rebuildPrefix + name,
// and columns that must step aside while others are renamed get tempPrefix + n.
const (
	rebuildPrefix = "_rowsmith_new_"
	tempPrefix    = "_rowsmith_tmp_"
)

func lit(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func quoteAll(names []string) string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = quote(n)
	}
	return strings.Join(out, ", ")
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return strings.TrimSpace(*s)
}

// on reads a boolean table option.
func on(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "1", "yes", "on":
		return true
	}
	return false
}

func lower(s string) string { return strings.ToLower(s) }

// isInteger reports the one declared type that makes a single-column primary
// key an alias of the rowid.
func isInteger(typ string) bool { return strings.EqualFold(strings.TrimSpace(typ), "INTEGER") }

var (
	numberLit = regexp.MustCompile(`^[+-]?(?:\d+(?:\.\d*)?|\.\d+)(?:[eE][+-]?\d+)?$|^[+-]?0[xX][0-9a-fA-F]+$`)
	stringLit = regexp.MustCompile(`^'(?:[^']|'')*'$`)
	blobLit   = regexp.MustCompile(`^[xX]'[0-9a-fA-F]*'$`)
	bareName  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// literal reports whether a default can be written without parentheses.
// constant is false for CURRENT_TIME and friends, which ADD COLUMN refuses.
func literal(s string) (ok, constant bool) {
	switch strings.ToUpper(s) {
	case "CURRENT_TIME", "CURRENT_DATE", "CURRENT_TIMESTAMP":
		return true, false
	case "NULL", "TRUE", "FALSE":
		return true, true
	}
	ok = numberLit.MatchString(s) || stringLit.MatchString(s) || blobLit.MatchString(s)
	return ok, ok
}

// defaultSQL writes a default as Describe reports it: PRAGMA table_info
// drops the parentheses around expressions, so they are put back.
func defaultSQL(d string) string {
	if ok, _ := literal(d); ok {
		return d
	}
	return "(" + d + ")"
}

func collationSQL(name string) string {
	if bareName.MatchString(name) {
		return name
	}
	return quote(name)
}

// columnSQL renders a column definition under the given name. autoKey makes
// it the table's INTEGER PRIMARY KEY AUTOINCREMENT.
func columnSQL(name string, col driver.Column, autoKey bool) string {
	s := quote(name)
	if t := strings.TrimSpace(col.Type); t != "" {
		s += " " + t
	}
	if autoKey {
		s += " PRIMARY KEY AUTOINCREMENT"
	}
	if !col.Nullable {
		s += " NOT NULL"
	}
	if g := strings.TrimSpace(col.Generated); g != "" {
		s += " GENERATED ALWAYS AS (" + g + ")"
		if col.GeneratedStored {
			s += " STORED"
		} else {
			s += " VIRTUAL"
		}
	} else if d := deref(col.Default); d != "" {
		s += " DEFAULT " + defaultSQL(d)
	}
	if col.Collation != "" {
		s += " COLLATE " + collationSQL(col.Collation)
	}
	return s
}

// columnChanged reports differences that ALTER TABLE cannot apply in place.
// Names and nullability are compared separately.
func columnChanged(old, next driver.Column) bool {
	return !strings.EqualFold(strings.TrimSpace(old.Type), strings.TrimSpace(next.Type)) ||
		deref(old.Default) != deref(next.Default) ||
		!strings.EqualFold(old.Collation, next.Collation) ||
		strings.TrimSpace(old.Generated) != strings.TrimSpace(next.Generated) ||
		next.Generated != "" && old.GeneratedStored != next.GeneratedStored
}

// isIdent reports tokens that can name a column: words and quoted names
// (single quotes make string literals).
func isIdent(t token) bool {
	if t.quoted {
		return t.text[0] != '\''
	}
	c := t.text[0]
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80
}

// mentions reports whether SQL text uses name as an identifier anywhere.
func mentions(sql, name string) bool {
	for _, t := range lex(sql) {
		if isIdent(t) && strings.EqualFold(unquote(t.text), name) {
			return true
		}
	}
	return false
}

// renameIdents rewrites column names in an expression of the table's own
// (a check, generated column or index key) through names, keyed by lower
// case. Function names, qualifiers, collations and CAST types are left alone.
func renameIdents(expr string, names map[string]string) string {
	if len(names) == 0 {
		return expr
	}
	toks := lex(expr)
	var b strings.Builder
	last := 0
	for i, t := range toks {
		if !isIdent(t) {
			continue
		}
		to, ok := names[lower(unquote(t.text))]
		if !ok || i+1 < len(toks) && (toks[i+1].text == "(" || toks[i+1].text == ".") || i > 0 && (toks[i-1].is("COLLATE") || toks[i-1].is("AS")) {
			continue
		}
		b.WriteString(expr[last:t.start])
		b.WriteString(quote(to))
		last = t.end
	}
	b.WriteString(expr[last:])
	return b.String()
}

// tableStatement returns the CREATE TABLE statement that starts Table.DDL.
func tableStatement(ddl string) string {
	for _, t := range lex(ddl) {
		if t.text == ";" {
			return ddl[:t.start]
		}
	}
	return ddl
}

// findName returns the position of name among n names, preferring an exact
// match to a case-insensitive one (SQLite ignores ASCII case in names).
func findName(n int, at func(int) string, name string) int {
	for i := 0; i < n; i++ {
		if at(i) == name {
			return i
		}
	}
	for i := 0; i < n; i++ {
		if strings.EqualFold(at(i), name) {
			return i
		}
	}
	return -1
}

func freeName(prefix string, taken map[string]bool) string {
	for n := 1; ; n++ {
		if s := fmt.Sprintf("%s%d", prefix, n); !taken[lower(s)] {
			taken[lower(s)] = true
			return s
		}
	}
}

// inTransaction wraps several statements so they apply together or not at all.
func inTransaction(stmts []string) []string {
	if len(stmts) < 2 {
		return stmts
	}
	return append(append([]string{"BEGIN"}, stmts...), "COMMIT")
}

// ---- resolving a definition ---------------------------------------------------

// part is one key of an index or constraint: a column of the new definition,
// or an expression.
type part struct {
	col  int // position in TableDef.Columns, or -1 for an expression
	expr string
	desc bool
}

type indexPlan struct {
	ix    driver.Index
	parts []part
	auto  bool          // backs a UNIQUE constraint (sqlite_autoindex_*), so it lives in CREATE TABLE
	same  *driver.Index // the current index when nothing changed
}

type fkPlan struct {
	fk       driver.ForeignKey
	cols     []int
	self     bool  // references this table
	refCols  []int // for a self-reference: columns of the new definition
	deferred bool  // DEFERRABLE INITIALLY DEFERRED, carried over from the current key
}

type checkPlan struct {
	name    string // "" writes an unnamed constraint
	label   string // for messages
	expr    string
	changed bool // new, or its expression differs from the current check
}

// plan resolves a TableDef against the current table (empty for a new one).
// References inside the definition (keys, index columns) may use new or
// current column names: lists copied unchanged from Describe keep current
// names, while the structure editor may have updated others after a rename.
type plan struct {
	from *driver.Table
	to   driver.TableDef
	orig tableDef // what the current CREATE TABLE says beyond Describe

	src []int // new column -> current column, -1 when added
	dst []int // current column -> new column, -1 when dropped

	pk      []int // primary key columns
	fromPK  []int // current primary key as new columns (-1 when dropped)
	autoKey int   // column written as INTEGER PRIMARY KEY AUTOINCREMENT, or -1

	indexes   []indexPlan
	dropIx    []driver.Index // current indexes removed or changed
	fks       []fkPlan
	fkChanged bool
	checks    []checkPlan
	dropCk    []driver.Check // current checks removed or changed

	withoutRowid, strict bool
}

func newPlan(from *driver.Table, to driver.TableDef) (*plan, error) {
	if from == nil {
		from = &driver.Table{}
	}
	p := &plan{from: from, to: to, autoKey: -1}
	if from.DDL != "" {
		p.orig = parseTable(tableStatement(from.DDL))
	}
	if len(to.Columns) == 0 {
		return nil, errors.New("a table needs at least one column")
	}
	p.src = make([]int, len(to.Columns))
	p.dst = make([]int, len(from.Columns))
	for j := range p.dst {
		p.dst[j] = -1
	}
	names := map[string]bool{}
	for i, col := range to.Columns {
		p.src[i] = -1
		if strings.TrimSpace(col.Name) == "" {
			return nil, errors.New("every column needs a name")
		}
		if names[lower(col.Name)] {
			return nil, fmt.Errorf("two columns are named %s", col.Name)
		}
		names[lower(col.Name)] = true
		if col.OriginalName == "" {
			continue
		}
		j := findName(len(from.Columns), func(j int) string { return from.Columns[j].Name }, col.OriginalName)
		if j < 0 {
			return nil, fmt.Errorf("column %s no longer exists; reload the structure", col.OriginalName)
		}
		if p.dst[j] >= 0 {
			return nil, fmt.Errorf("column %s appears twice in the new definition", from.Columns[j].Name)
		}
		p.src[i], p.dst[j] = j, i
	}
	p.withoutRowid, p.strict = on(to.Options["without_rowid"]), on(to.Options["strict"])
	for _, step := range []func() error{p.resolveKey, p.resolveIndexes, p.resolveFKs, p.resolveChecks} {
		if err := step(); err != nil {
			return nil, err
		}
	}
	return p, nil
}

// current resolves a current column name: ok is false when there is no such
// column, and col is -1 when the column is dropped.
func (p *plan) current(name string) (col int, ok bool) {
	j := findName(len(p.from.Columns), func(j int) string { return p.from.Columns[j].Name }, name)
	if j < 0 {
		return -1, false
	}
	return p.dst[j], true
}

// column resolves a name of the new definition, falling back to the current
// name of a renamed column; -1 when it names no column.
func (p *plan) column(name string) int {
	if i := findName(len(p.to.Columns), func(i int) string { return p.to.Columns[i].Name }, name); i >= 0 {
		return i
	}
	return findName(len(p.to.Columns), func(i int) string {
		if p.src[i] < 0 {
			return ""
		}
		return p.from.Columns[p.src[i]].Name
	}, name)
}

// columns resolves the column list of a key or index: with current names when
// the list is the current one unchanged, else with new names first.
func (p *plan) columns(what string, names []string, same bool) ([]int, error) {
	out := make([]int, len(names))
	for k, n := range names {
		col, ok := -1, false
		if same {
			col, ok = p.current(n)
		} else if col = p.column(n); col >= 0 {
			ok = true
		}
		switch {
		case !ok:
			return nil, fmt.Errorf("%s uses unknown column %s", what, n)
		case col < 0:
			return nil, fmt.Errorf("%s uses column %s, which is being dropped", what, n)
		}
		out[k] = col
	}
	return out, nil
}

func (p *plan) resolveKey() error {
	for _, n := range p.from.PrimaryKey {
		col, _ := p.current(n)
		p.fromPK = append(p.fromPK, col)
	}
	var err error
	if p.pk, err = p.columns("the primary key", p.to.PrimaryKey, slices.Equal(p.to.PrimaryKey, p.from.PrimaryKey)); err != nil {
		return err
	}
	for k, col := range p.pk {
		if slices.Index(p.pk, col) != k {
			return fmt.Errorf("column %s appears twice in the primary key", p.to.Columns[col].Name)
		}
	}
	if p.withoutRowid && len(p.pk) == 0 {
		return errors.New("a WITHOUT ROWID table needs a primary key")
	}
	for i, col := range p.to.Columns {
		alias := len(p.pk) == 1 && p.pk[0] == i && isInteger(col.Type) && !p.withoutRowid
		was := p.src[i] >= 0 && p.from.Columns[p.src[i]].AutoIncrement
		switch {
		case col.AutoIncrement && p.withoutRowid:
			return errors.New("WITHOUT ROWID tables cannot auto-increment")
		case col.AutoIncrement && !alias:
			return fmt.Errorf("auto-increment needs %s to be the only primary key column, with the type INTEGER", col.Name)
		case col.AutoIncrement && (!was || strings.EqualFold(p.orig.autoincrement, p.from.Columns[p.src[i]].Name)):
			// Newly switched on, or declared AUTOINCREMENT already. A rowid
			// alias without the keyword (also reported as auto-increment)
			// stays as it is.
			p.autoKey = i
		case !col.AutoIncrement && was && alias:
			return fmt.Errorf("SQLite numbers a single INTEGER PRIMARY KEY column automatically; to stop that for %s, give it another type such as INT", col.Name)
		}
	}
	return nil
}

func isAuto(name string) bool { return strings.HasPrefix(lower(name), "sqlite_autoindex_") }

// indexParts resolves index keys: entries naming a column become column
// parts, anything else is an expression.
func (p *plan) indexParts(ix driver.Index, same bool) ([]part, error) {
	parts := make([]part, len(ix.Columns))
	for k, n := range ix.Columns {
		parts[k] = part{col: -1, expr: strings.TrimSpace(n), desc: k < len(ix.Desc) && ix.Desc[k]}
		if same {
			if col, ok := p.current(n); ok {
				if col < 0 {
					return nil, fmt.Errorf("index %s uses column %s, which is being dropped", ix.Name, n)
				}
				parts[k].col, parts[k].expr = col, ""
			}
			continue
		}
		if col := p.column(n); col >= 0 {
			parts[k].col, parts[k].expr = col, ""
		} else if toks := lex(n); len(toks) == 1 && isIdent(toks[0]) {
			return nil, fmt.Errorf("index %s uses unknown column %s", ix.Name, n)
		}
	}
	return parts, nil
}

func (p *plan) resolveIndexes() error {
	current := map[string]*driver.Index{}
	for i := range p.from.Indexes {
		if ix := &p.from.Indexes[i]; !ix.Primary {
			current[lower(ix.Name)] = ix
		}
	}
	seen := map[string]bool{}
	for _, ix := range p.to.Indexes {
		if ix.Primary {
			continue
		}
		key := lower(ix.Name)
		switch {
		case strings.TrimSpace(ix.Name) == "":
			return errors.New("every index needs a name")
		case seen[key]:
			return fmt.Errorf("two indexes are named %s", ix.Name)
		case len(ix.Columns) == 0:
			return fmt.Errorf("index %s needs at least one column", ix.Name)
		}
		seen[key] = true
		old := current[key]
		if old == nil && strings.HasPrefix(key, "sqlite_") {
			return fmt.Errorf("index names starting with sqlite_ are reserved; rename %s", ix.Name)
		}
		ip := indexPlan{ix: ix, auto: old != nil && isAuto(old.Name)}
		var err error
		if ip.parts, err = p.indexParts(ix, old != nil && slices.Equal(ix.Columns, old.Columns)); err != nil {
			return err
		}
		if old != nil {
			oldParts, err := p.indexParts(*old, true)
			if err == nil && slices.Equal(ip.parts, oldParts) && ix.Unique == old.Unique && strings.TrimSpace(ix.Where) == strings.TrimSpace(old.Where) {
				ip.same = old
			} else {
				p.dropIx = append(p.dropIx, *old)
			}
		}
		if ip.auto && ip.same == nil {
			// It is written back as a UNIQUE table constraint.
			if !ix.Unique || strings.TrimSpace(ix.Where) != "" || slices.ContainsFunc(ip.parts, func(pt part) bool { return pt.col < 0 }) {
				return fmt.Errorf("index %s belongs to a UNIQUE constraint, which covers plain columns of every row; remove it and add a new index instead", ix.Name)
			}
		}
		p.indexes = append(p.indexes, ip)
	}
	for _, ix := range p.from.Indexes {
		if !ix.Primary && !seen[lower(ix.Name)] {
			p.dropIx = append(p.dropIx, ix)
		}
	}
	return nil
}

func normRule(rule string) string {
	if r := strings.ToUpper(strings.Join(strings.Fields(rule), " ")); r != "" {
		return r
	}
	return "NO ACTION"
}

func (p *plan) isSelf(table string) bool {
	return strings.EqualFold(table, p.from.Ref.Name) && p.from.Ref.Name != "" || strings.EqualFold(table, p.to.Ref.Name)
}

func (p *plan) resolveFKs() error {
	// Describe names keys fk_<table>_<id>, and SQLite numbers them from the
	// last one declared.
	deferred := map[string]bool{}
	if n := len(p.orig.deferred); n == len(p.from.ForeignKeys) {
		for i, d := range p.orig.deferred {
			deferred[fmt.Sprintf("fk_%s_%d", p.from.Ref.Name, n-1-i)] = d
		}
	}
	current := map[string]*driver.ForeignKey{}
	for i := range p.from.ForeignKeys {
		current[p.from.ForeignKeys[i].Name] = &p.from.ForeignKeys[i]
	}
	seen := map[string]bool{}
	for _, fk := range p.to.ForeignKeys {
		what := "foreign key " + fk.Name
		if fk.Name == "" {
			what = "the foreign key on " + strings.Join(fk.Columns, ", ")
		}
		switch {
		case len(fk.Columns) == 0:
			return fmt.Errorf("%s needs at least one column", what)
		case strings.TrimSpace(fk.RefTable.Name) == "":
			return fmt.Errorf("%s needs a referenced table", what)
		case len(fk.RefColumns) > 0 && len(fk.RefColumns) != len(fk.Columns):
			return fmt.Errorf("%s needs as many referenced columns as columns", what)
		}
		var old *driver.ForeignKey
		if fk.Name != "" && !seen[fk.Name] {
			old = current[fk.Name]
		}
		seen[fk.Name] = true
		fp := fkPlan{fk: fk, self: p.isSelf(fk.RefTable.Name), deferred: old != nil && deferred[old.Name]}
		var err error
		if fp.cols, err = p.columns(what, fk.Columns, old != nil && slices.Equal(fk.Columns, old.Columns)); err != nil {
			return err
		}
		if fp.self && len(fk.RefColumns) > 0 {
			if fp.refCols, err = p.columns(what, fk.RefColumns, old != nil && slices.Equal(fk.RefColumns, old.RefColumns)); err != nil {
				return err
			}
		}
		if old == nil || !p.sameFK(*old, fp) {
			p.fkChanged = true
		}
		p.fks = append(p.fks, fp)
	}
	for _, fk := range p.from.ForeignKeys {
		if !seen[fk.Name] {
			p.fkChanged = true
		}
	}
	return nil
}

func (p *plan) sameFK(old driver.ForeignKey, next fkPlan) bool {
	cols, err := p.columns("", old.Columns, true)
	if err != nil || !slices.Equal(cols, next.cols) || p.isSelf(old.RefTable.Name) != next.self ||
		!next.self && !strings.EqualFold(old.RefTable.Name, next.fk.RefTable.Name) ||
		normRule(old.OnDelete) != normRule(next.fk.OnDelete) || normRule(old.OnUpdate) != normRule(next.fk.OnUpdate) ||
		len(old.RefColumns) == 0 != (len(next.fk.RefColumns) == 0) {
		return false
	}
	if next.self && len(old.RefColumns) > 0 {
		ref, err := p.columns("", old.RefColumns, true)
		return err == nil && slices.Equal(ref, next.refCols)
	}
	return slices.EqualFunc(old.RefColumns, next.fk.RefColumns, strings.EqualFold)
}

func (p *plan) resolveChecks() error {
	current := map[string]driver.Check{}
	for _, ck := range p.from.Checks {
		current[ck.Name] = ck
	}
	seen := map[string]bool{}
	for _, ck := range p.to.Checks {
		expr := strings.TrimSpace(ck.Expression)
		cp := checkPlan{name: ck.Name, label: ck.Name, expr: expr}
		if ck.Name == "" {
			cp.label = "(" + expr + ")"
		}
		if expr == "" {
			return fmt.Errorf("check %s needs an expression", cp.label)
		}
		old, ok := current[ck.Name]
		if ck.Name != "" {
			if seen[ck.Name] {
				return fmt.Errorf("two checks are named %s", ck.Name)
			}
			seen[ck.Name] = true
		} else {
			ok = false
		}
		cp.changed = !ok || strings.TrimSpace(old.Expression) != expr
		if ok && p.orig.unnamed[ck.Name] {
			cp.name = "" // keep the constraint unnamed, as it was
		}
		if ok && cp.changed {
			p.dropCk = append(p.dropCk, old)
		}
		p.checks = append(p.checks, cp)
	}
	for _, ck := range p.from.Checks {
		if !seen[ck.Name] {
			p.dropCk = append(p.dropCk, ck)
		}
	}
	return nil
}

// ---- checks against losing things -------------------------------------------

// dropped lists current columns that the new definition drops, leaving out
// names a new column takes over.
func (p *plan) dropped() []string {
	var out []string
	for j, col := range p.from.Columns {
		if p.dst[j] < 0 && findName(len(p.to.Columns), func(i int) string { return p.to.Columns[i].Name }, col.Name) < 0 {
			out = append(out, col.Name)
		}
	}
	return out
}

// checkDropped refuses definitions that keep an expression over a dropped column.
func (p *plan) checkDropped() error {
	for _, d := range p.dropped() {
		for _, ck := range p.checks {
			if !ck.changed && mentions(ck.expr, d) {
				return fmt.Errorf("check %s uses column %s, which is being dropped", ck.label, d)
			}
		}
		for i, col := range p.to.Columns {
			if j := p.src[i]; j >= 0 && col.Generated != "" && strings.TrimSpace(col.Generated) == strings.TrimSpace(p.from.Columns[j].Generated) && mentions(col.Generated, d) {
				return fmt.Errorf("generated column %s uses column %s, which is being dropped", col.Name, d)
			}
		}
		for _, ip := range p.indexes {
			if ip.same == nil {
				continue
			}
			uses := mentions(ip.same.Where, d)
			for _, pt := range ip.parts {
				uses = uses || pt.col < 0 && mentions(pt.expr, d)
			}
			if uses {
				return fmt.Errorf("index %s uses column %s, which is being dropped", ip.ix.Name, d)
			}
		}
	}
	return nil
}

// checkReferences keeps other tables' foreign keys working: the columns they
// reference must stay, as a primary key or unique index.
func (p *plan) checkReferences() error {
	for _, fk := range p.from.Referenced {
		if fk.Table == nil || strings.EqualFold(fk.Table.Name, p.from.Ref.Name) {
			continue
		}
		var cur, next []int
		for _, n := range fk.RefColumns {
			j := findName(len(p.from.Columns), func(j int) string { return p.from.Columns[j].Name }, n)
			if j < 0 {
				break
			}
			if p.dst[j] < 0 {
				return fmt.Errorf("column %s is referenced by a foreign key of %s; drop that key first", p.from.Columns[j].Name, fk.Table.Name)
			}
			cur, next = append(cur, j), append(next, p.dst[j])
		}
		if len(cur) == len(fk.RefColumns) && p.keyedBefore(cur) && !p.keyedAfter(next) {
			return fmt.Errorf("a foreign key of %s references %s, which must remain a primary key or unique index", fk.Table.Name, strings.Join(fk.RefColumns, ", "))
		}
	}
	return nil
}

func sameSet(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for _, x := range a {
		if !slices.Contains(b, x) {
			return false
		}
	}
	return true
}

// keyedBefore reports whether current columns are the primary key or have a unique index.
func (p *plan) keyedBefore(cols []int) bool {
	index := func(names []string) []int {
		var out []int
		for _, n := range names {
			if j := findName(len(p.from.Columns), func(j int) string { return p.from.Columns[j].Name }, n); j >= 0 {
				out = append(out, j)
			}
		}
		return out
	}
	if sameSet(cols, index(p.from.PrimaryKey)) {
		return true
	}
	for _, ix := range p.from.Indexes {
		if ix.Unique && ix.Where == "" && sameSet(cols, index(ix.Columns)) {
			return true
		}
	}
	return false
}

// keyedAfter is keyedBefore for new columns in the new definition.
func (p *plan) keyedAfter(cols []int) bool {
	if sameSet(cols, p.pk) {
		return true
	}
	for _, ip := range p.indexes {
		var ixCols []int
		for _, pt := range ip.parts {
			ixCols = append(ixCols, pt.col)
		}
		if ip.ix.Unique && strings.TrimSpace(ip.ix.Where) == "" && sameSet(cols, ixCols) {
			return true
		}
	}
	return false
}

// ---- in place or rebuild -------------------------------------------------------

// addable reports whether ALTER TABLE ADD COLUMN can add a column.
func (p *plan) addable(i int) bool {
	col := p.to.Columns[i]
	if slices.Contains(p.pk, i) || col.AutoIncrement {
		return false
	}
	if col.Generated != "" {
		return !col.GeneratedStored
	}
	d := deref(col.Default)
	if d == "" {
		return col.Nullable
	}
	ok, constant := literal(d)
	return ok && constant && (col.Nullable || !strings.EqualFold(d, "NULL"))
}

// droppable reports whether ALTER TABLE DROP COLUMN can drop a current column
// once the indexes and checks being removed are gone. SQLite itself refuses
// the drop when a view or trigger uses the column.
func (p *plan) droppable(j int) bool {
	name := p.from.Columns[j].Name
	is := func(n string) bool { return strings.EqualFold(n, name) }
	if slices.ContainsFunc(p.from.PrimaryKey, is) {
		return false
	}
	for _, fk := range p.from.ForeignKeys {
		if slices.ContainsFunc(fk.Columns, is) {
			return false
		}
	}
	for _, ix := range p.from.Indexes {
		if !isAuto(ix.Name) && slices.ContainsFunc(p.dropIx, func(d driver.Index) bool { return d.Name == ix.Name }) {
			continue
		}
		if slices.ContainsFunc(ix.Columns, func(c string) bool { return is(c) || mentions(c, name) }) || mentions(ix.Where, name) {
			return false
		}
	}
	for _, ck := range p.from.Checks {
		if !p.orig.unnamed[ck.Name] && slices.ContainsFunc(p.dropCk, func(d driver.Check) bool { return d.Name == ck.Name }) {
			continue
		}
		if mentions(ck.Expression, name) {
			return false
		}
	}
	for k, col := range p.from.Columns {
		if k != j && col.Generated != "" && mentions(col.Generated, name) {
			return false
		}
	}
	return true
}

func (p *plan) needsRebuild() bool {
	if p.withoutRowid != on(p.from.Options["without_rowid"]) || p.strict != on(p.from.Options["strict"]) ||
		!slices.Equal(p.pk, p.fromPK) || p.fkChanged {
		return true
	}
	for _, ck := range p.dropCk {
		if p.orig.unnamed[ck.Name] {
			return true // only named checks can be dropped in place
		}
	}
	for _, ix := range p.dropIx {
		if isAuto(ix.Name) {
			return true // UNIQUE constraints are part of the table
		}
	}
	// ADD COLUMN appends: kept columns must stay in order, with new ones last.
	var order []int
	for _, i := range p.dst {
		if i >= 0 {
			order = append(order, i)
		}
	}
	for i, j := range p.src {
		if j < 0 {
			order = append(order, i)
		}
	}
	for k, i := range order {
		if k != i {
			return true
		}
	}
	for i, col := range p.to.Columns {
		j := p.src[i]
		if j < 0 {
			if !p.addable(i) {
				return true
			}
			continue
		}
		old := p.from.Columns[j]
		if columnChanged(old, col.Column) || (p.autoKey == i) != strings.EqualFold(p.orig.autoincrement, old.Name) {
			return true
		}
	}
	for j := range p.from.Columns {
		if p.dst[j] < 0 && !p.droppable(j) {
			return true
		}
	}
	return false
}

// renameColumns orders column renames so that none collides with a name
// still in use, moving a column aside under a temporary name to break a
// cycle such as a swap. inUse lists the column names before the renames.
func renameColumns(table string, moves [][2]string, inUse []string, taken map[string]bool) []string {
	used := map[string]bool{}
	for _, n := range inUse {
		used[lower(n)] = true
	}
	var pending [][2]string
	for _, m := range moves {
		if m[0] != m[1] {
			pending = append(pending, m)
		}
	}
	var out []string
	rename := func(from, to string) {
		out = append(out, "ALTER TABLE "+table+" RENAME COLUMN "+quote(from)+" TO "+quote(to))
		delete(used, lower(from))
		used[lower(to)] = true
	}
	for len(pending) > 0 {
		moved := false
		for k := 0; k < len(pending); k++ {
			if m := pending[k]; !used[lower(m[1])] || strings.EqualFold(m[0], m[1]) {
				rename(m[0], m[1])
				pending = slices.Delete(pending, k, k+1)
				k--
				moved = true
			}
		}
		if !moved {
			tmp := freeName(tempPrefix, taken)
			rename(pending[0][0], tmp)
			pending[0][0] = tmp
		}
	}
	return out
}

// renameTable renames a table; a change of case alone goes through a
// temporary name, since SQLite sees the new name as already taken.
func renameTable(from, to string) []string {
	if !strings.EqualFold(from, to) {
		return []string{"ALTER TABLE " + quote(from) + " RENAME TO " + quote(to)}
	}
	tmp := tempPrefix + to
	return []string{"ALTER TABLE " + quote(from) + " RENAME TO " + quote(tmp), "ALTER TABLE " + quote(tmp) + " RENAME TO " + quote(to)}
}

// createSQL writes CREATE TABLE for the definition under the given column
// names. rw rewrites column names in new or changed expressions; self is the
// table name that references to this table use.
func (p *plan) createSQL(table string, names []string, rw map[string]string, self string) string {
	cols := func(idx []int) string {
		out := make([]string, len(idx))
		for k, i := range idx {
			out[k] = quote(names[i])
		}
		return strings.Join(out, ", ")
	}
	var lines []string
	for i, col := range p.to.Columns {
		c := col.Column
		if c.Generated != "" && (p.src[i] < 0 || strings.TrimSpace(c.Generated) != strings.TrimSpace(p.from.Columns[p.src[i]].Generated)) {
			c.Generated = renameIdents(c.Generated, rw)
		}
		lines = append(lines, columnSQL(names[i], c, p.autoKey == i))
	}
	if p.autoKey < 0 && len(p.pk) > 0 {
		lines = append(lines, "PRIMARY KEY ("+cols(p.pk)+")")
	}
	for _, ip := range p.indexes {
		if ip.auto {
			keys := make([]string, len(ip.parts))
			for k, pt := range ip.parts {
				if keys[k] = quote(names[pt.col]); pt.desc {
					keys[k] += " DESC"
				}
			}
			lines = append(lines, "UNIQUE ("+strings.Join(keys, ", ")+")")
		}
	}
	// SQLite numbers foreign keys from the last one declared, and Describe
	// names them by that number: writing them in reverse keeps the names.
	for k := len(p.fks) - 1; k >= 0; k-- {
		fp := p.fks[k]
		s := "FOREIGN KEY (" + cols(fp.cols) + ") REFERENCES "
		switch {
		case fp.self:
			s += quote(self)
		default:
			s += quote(fp.fk.RefTable.Name)
		}
		switch {
		case fp.self && len(fp.refCols) > 0:
			s += " (" + cols(fp.refCols) + ")"
		case len(fp.fk.RefColumns) > 0:
			s += " (" + quoteAll(fp.fk.RefColumns) + ")"
		}
		if r := normRule(fp.fk.OnDelete); r != "NO ACTION" {
			s += " ON DELETE " + r
		}
		if r := normRule(fp.fk.OnUpdate); r != "NO ACTION" {
			s += " ON UPDATE " + r
		}
		if fp.deferred {
			s += " DEFERRABLE INITIALLY DEFERRED"
		}
		lines = append(lines, s)
	}
	for _, ck := range p.checks {
		expr := ck.expr
		if ck.changed {
			expr = renameIdents(expr, rw)
		}
		s := "CHECK (" + expr + ")"
		if ck.name != "" {
			s = "CONSTRAINT " + quote(ck.name) + " " + s
		}
		lines = append(lines, s)
	}
	s := "CREATE TABLE " + table + " (\n  " + strings.Join(lines, ",\n  ") + "\n)"
	var opts []string
	if p.withoutRowid {
		opts = append(opts, "WITHOUT ROWID")
	}
	if p.strict {
		opts = append(opts, "STRICT")
	}
	if len(opts) > 0 {
		s += " " + strings.Join(opts, ", ")
	}
	return s
}

// indexSQL writes CREATE INDEX on table with the given column names.
func indexSQL(table string, ip indexPlan, names []string, rw map[string]string) string {
	s := "CREATE "
	if ip.ix.Unique {
		s += "UNIQUE "
	}
	keys := make([]string, len(ip.parts))
	for k, pt := range ip.parts {
		if pt.col >= 0 {
			keys[k] = quote(names[pt.col])
		} else {
			keys[k] = renameIdents(pt.expr, rw)
		}
		if pt.desc {
			keys[k] += " DESC"
		}
	}
	s += "INDEX " + quote(ip.ix.Name) + " ON " + table + " (" + strings.Join(keys, ", ") + ")"
	if w := strings.TrimSpace(ip.ix.Where); w != "" {
		s += " WHERE " + renameIdents(w, rw)
	}
	return s
}

func (p *plan) finalNames() []string {
	out := make([]string, len(p.to.Columns))
	for i, col := range p.to.Columns {
		out[i] = col.Name
	}
	return out
}

// takenNames holds every current and new column name, lower-cased.
func (p *plan) takenNames() map[string]bool {
	taken := map[string]bool{}
	for _, col := range p.from.Columns {
		taken[lower(col.Name)] = true
	}
	for _, col := range p.to.Columns {
		taken[lower(col.Name)] = true
	}
	return taken
}

// inPlace changes the table with ALTER TABLE alone.
func (p *plan) inPlace() []string {
	table := quote(p.from.Ref.Name)
	alter := func(clause string) string { return "ALTER TABLE " + table + " " + clause }
	var out []string
	for _, ix := range p.dropIx {
		out = append(out, "DROP INDEX "+quote(ix.Name))
	}
	for _, ck := range p.dropCk {
		out = append(out, alter("DROP CONSTRAINT "+quote(ck.Name)))
	}
	var inUse []string
	var moves [][2]string
	for j, col := range p.from.Columns {
		if i := p.dst[j]; i < 0 {
			out = append(out, alter("DROP COLUMN "+quote(col.Name)))
		} else {
			inUse = append(inUse, col.Name)
			moves = append(moves, [2]string{col.Name, p.to.Columns[i].Name})
		}
	}
	out = append(out, renameColumns(table, moves, inUse, p.takenNames())...)

	// New expressions use new names; current names of renamed columns are
	// accepted too, unless another column has taken them.
	rw := map[string]string{}
	for i, col := range p.to.Columns {
		if j := p.src[i]; j >= 0 && p.column(p.from.Columns[j].Name) == i && p.from.Columns[j].Name != col.Name {
			rw[lower(p.from.Columns[j].Name)] = col.Name
		}
	}
	for i, col := range p.to.Columns {
		if j := p.src[i]; j >= 0 && col.Nullable != p.from.Columns[j].Nullable {
			if col.Nullable {
				out = append(out, alter("ALTER COLUMN "+quote(col.Name)+" DROP NOT NULL"))
			} else {
				out = append(out, alter("ALTER COLUMN "+quote(col.Name)+" SET NOT NULL"))
			}
		}
	}
	for i, col := range p.to.Columns {
		if p.src[i] < 0 {
			c := col.Column
			c.Generated = renameIdents(c.Generated, rw)
			out = append(out, alter("ADD COLUMN "+columnSQL(col.Name, c, false)))
		}
	}
	for _, ck := range p.checks {
		if !ck.changed {
			continue
		}
		clause := "ADD CHECK (" + renameIdents(ck.expr, rw) + ")"
		if ck.name != "" {
			clause = "ADD CONSTRAINT " + quote(ck.name) + " CHECK (" + renameIdents(ck.expr, rw) + ")"
		}
		out = append(out, alter(clause))
	}
	names := p.finalNames()
	for _, ip := range p.indexes {
		if ip.same == nil {
			out = append(out, indexSQL(table, ip, names, rw))
		}
	}
	return out
}

// rebuild copies the table into a new one with the new structure. The new
// table keeps the current column names, so that the saved definitions of
// indexes and triggers apply to it unchanged; renames come last. rename holds
// the statements renaming the table itself, if any.
func (p *plan) rebuild(rename []string) ([]string, error) {
	if p.from.Options["virtual"] != "" {
		return nil, errors.New("virtual tables cannot be restructured, only renamed")
	}
	if p.orig.onConflict {
		return nil, fmt.Errorf("this change rebuilds %s, which would lose its ON CONFLICT clauses; make it in the SQL editor instead", p.from.Ref.Name)
	}
	drops := p.dropped()
	for _, tr := range p.from.Triggers {
		for _, d := range drops {
			if mentions(tr.Statement, d) {
				return nil, fmt.Errorf("trigger %s uses column %s, which is being dropped; change or drop the trigger first", tr.Name, d)
			}
		}
	}

	name := p.from.Ref.Name
	table, tmp := quote(name), quote(rebuildPrefix+name)
	taken := p.takenNames()
	// Kept columns keep their current names for now; a new column whose name
	// a kept column still holds waits under a temporary one.
	names := make([]string, len(p.to.Columns))
	current := map[string]bool{}
	for i, j := range p.src {
		if j >= 0 {
			names[i] = p.from.Columns[j].Name
			current[lower(names[i])] = true
		}
	}
	for i, j := range p.src {
		if j < 0 {
			if names[i] = p.to.Columns[i].Name; current[lower(names[i])] {
				names[i] = freeName(tempPrefix, taken)
			}
		}
	}
	// New and changed expressions are written with new names.
	rw := map[string]string{}
	for i, col := range p.to.Columns {
		if names[i] != col.Name {
			rw[lower(col.Name)] = names[i]
		}
	}

	out := []string{"PRAGMA foreign_keys = OFF", "BEGIN", p.createSQL(tmp, names, rw, name)}
	var into, from []string
	for i, col := range p.to.Columns {
		if j := p.src[i]; j >= 0 && col.Generated == "" {
			into, from = append(into, quote(names[i])), append(from, quote(p.from.Columns[j].Name))
		}
	}
	// Keep rowids, which other tables or FTS indexes may rely on, unless a
	// copied INTEGER PRIMARY KEY column carries them already.
	aliasCopied := len(p.pk) == 1 && isInteger(p.to.Columns[p.pk[0]].Type) && p.src[p.pk[0]] >= 0
	if !p.withoutRowid && !on(p.from.Options["without_rowid"]) && !aliasCopied {
		for _, r := range []string{"rowid", "_rowid_", "oid"} {
			if !taken[r] {
				into, from = append([]string{r}, into...), append([]string{r}, from...)
				break
			}
		}
	}
	if len(into) > 0 {
		out = append(out, "INSERT INTO "+tmp+" ("+strings.Join(into, ", ")+")\n  SELECT "+strings.Join(from, ", ")+" FROM "+table)
	}
	if p.orig.autoincrement != "" && p.autoKey >= 0 {
		// Carry the AUTOINCREMENT high-water mark over, so deleted ids stay unused.
		out = append(out, "DELETE FROM sqlite_sequence WHERE name = "+lit(rebuildPrefix+name),
			"INSERT INTO sqlite_sequence (name, seq) SELECT "+lit(rebuildPrefix+name)+", seq FROM sqlite_sequence WHERE name = "+lit(name))
	}
	// Views that use the table would make the rename fail while it is gone;
	// legacy_alter_table skips that check.
	out = append(out, "DROP TABLE "+table, "PRAGMA legacy_alter_table = ON", "ALTER TABLE "+tmp+" RENAME TO "+table, "PRAGMA legacy_alter_table = OFF")
	for _, ip := range p.indexes {
		switch {
		case ip.auto:
		case ip.same != nil && ip.same.Definition != "":
			out = append(out, ip.same.Definition)
		case ip.same != nil:
			out = append(out, indexSQL(table, ip, names, nil))
		default:
			out = append(out, indexSQL(table, ip, names, rw))
		}
	}
	for _, tr := range p.from.Triggers {
		if tr.Statement != "" {
			out = append(out, tr.Statement)
		}
	}
	var moves [][2]string
	for i, col := range p.to.Columns {
		moves = append(moves, [2]string{names[i], col.Name})
	}
	renames := renameColumns(table, moves, names, taken)
	out = append(out, renames...)
	if len(drops) > 0 && len(renames) == 0 && len(rename) == 0 {
		// Renaming makes SQLite check every view, so a view still using a
		// dropped column fails the script; renaming a column to itself does
		// just that check.
		out = append(out, "ALTER TABLE "+table+" RENAME COLUMN "+quote(names[0])+" TO "+quote(names[0]))
	}
	out = append(out, rename...)
	final := name
	if p.to.Ref.Name != "" {
		final = p.to.Ref.Name
	}
	out = append(out, "PRAGMA foreign_key_check("+quote(final)+")")
	checked := map[string]bool{lower(name): true, lower(final): true}
	for _, fk := range p.from.Referenced {
		if fk.Table != nil && !checked[lower(fk.Table.Name)] {
			checked[lower(fk.Table.Name)] = true
			out = append(out, "PRAGMA foreign_key_check("+quote(fk.Table.Name)+")")
		}
	}
	return append(out, "COMMIT", "PRAGMA foreign_keys = ON"), nil
}

// ---- driver.DDLGenerator -------------------------------------------------------

func (c *conn) CreateTableSQL(def driver.TableDef) ([]string, error) {
	name := def.Ref.Name
	switch {
	case strings.TrimSpace(name) == "":
		return nil, errors.New("give the table a name")
	case strings.HasPrefix(lower(name), "sqlite_"):
		return nil, errors.New("table names starting with sqlite_ are reserved")
	case def.Options["virtual"] != "":
		return nil, errors.New("virtual tables cannot be designed here; create them with CREATE VIRTUAL TABLE in the SQL editor")
	}
	def.Columns = slices.Clone(def.Columns)
	for i := range def.Columns {
		def.Columns[i].OriginalName = ""
	}
	p, err := newPlan(nil, def)
	if err != nil {
		return nil, err
	}
	names := p.finalNames()
	stmts := []string{p.createSQL(quote(name), names, nil, name)}
	for _, ip := range p.indexes {
		stmts = append(stmts, indexSQL(quote(name), ip, names, nil))
	}
	return inTransaction(stmts), nil
}

// AlterTableSQL diffs the current table against the desired definition.
// Columns are matched by OriginalName; unmatched current columns are dropped.
func (c *conn) AlterTableSQL(from *driver.Table, to driver.TableDef) ([]string, error) {
	if from == nil {
		return nil, errors.New("current table definition is missing")
	}
	if from.Kind == "view" {
		return nil, errors.New("views cannot be changed here; edit the view definition in the SQL editor")
	}
	p, err := newPlan(from, to)
	if err != nil {
		return nil, err
	}
	if err := p.checkDropped(); err != nil {
		return nil, err
	}
	if err := p.checkReferences(); err != nil {
		return nil, err
	}
	var rename []string
	if to.Ref.Name != "" && to.Ref.Name != from.Ref.Name {
		if strings.HasPrefix(lower(to.Ref.Name), "sqlite_") {
			return nil, errors.New("table names starting with sqlite_ are reserved")
		}
		rename = renameTable(from.Ref.Name, to.Ref.Name)
	}
	if p.needsRebuild() {
		return p.rebuild(rename)
	}
	stmts := p.inPlace()
	if len(stmts) > 0 && from.Options["virtual"] != "" {
		return nil, errors.New("virtual tables cannot be restructured, only renamed")
	}
	return inTransaction(append(stmts, rename...)), nil
}

var dropKeyword = map[string]string{"table": "TABLE", "view": "VIEW", "index": "INDEX", "trigger": "TRIGGER"}

func (c *conn) DropObjectSQL(ref driver.ObjectRef, _ bool) ([]string, error) {
	kw, ok := dropKeyword[ref.Kind]
	if !ok {
		return nil, fmt.Errorf("cannot drop objects of kind %q", ref.Kind)
	}
	return []string{"DROP " + kw + " " + quote(ref.Name)}, nil
}

func (c *conn) TruncateSQL(ref driver.ObjectRef) ([]string, error) {
	if ref.Kind != "" && ref.Kind != "table" {
		return nil, errors.New("only tables can be emptied")
	}
	return []string{"DELETE FROM " + quote(ref.Name)}, nil
}

func (c *conn) RenameObjectSQL(ref driver.ObjectRef, newName string) ([]string, error) {
	switch {
	case strings.TrimSpace(newName) == "":
		return nil, errors.New("enter a new name")
	case strings.HasPrefix(lower(newName), "sqlite_"):
		return nil, errors.New("names starting with sqlite_ are reserved")
	case newName == ref.Name:
		return nil, nil
	}
	switch ref.Kind {
	case "", "table":
		return inTransaction(renameTable(ref.Name, newName)), nil
	case "view", "index", "trigger":
		return c.renameBySource(ref, newName)
	}
	return nil, fmt.Errorf("renaming a %s is not supported", ref.Kind)
}

func (c *conn) CreateDatabaseSQL(string, map[string]string) ([]string, error) {
	return nil, errors.New("SQLite databases are files: to create one, add a connection with a new file name and let Rowsmith create the file")
}

func (c *conn) DropDatabaseSQL(string) ([]string, error) {
	return nil, errors.New("SQLite databases are files and cannot be dropped with SQL: remove the connection and delete the file on the server")
}

// ---- renaming views, indexes and triggers ------------------------------------

type schemaEntry struct{ kind, name, table, sql string }

// renameBySource renames a view, index or trigger, which SQLite cannot do in
// place: the object is dropped and created again from its stored SQL under
// the new name. Dropping a view drops its INSTEAD OF triggers, so they are
// recreated on the renamed view; a view other objects use is refused, since
// they would keep the old name.
func (c *conn) renameBySource(ref driver.ObjectRef, newName string) ([]string, error) {
	if c.db == nil {
		return nil, driver.ErrNotSupported
	}
	ctx, cancel := context.WithTimeout(context.Background(), driver.CatalogTimeout)
	defer cancel()
	rows, err := c.db.QueryContext(ctx, `SELECT type, name, tbl_name, COALESCE(sql, '') FROM sqlite_schema`)
	if err != nil {
		return nil, err
	}
	var all []schemaEntry
	for rows.Next() {
		var e schemaEntry
		if err := rows.Scan(&e.kind, &e.name, &e.table, &e.sql); err != nil {
			rows.Close()
			return nil, err
		}
		all = append(all, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var target *schemaEntry
	for i, e := range all {
		if e.kind == ref.Kind && strings.EqualFold(e.name, ref.Name) {
			target = &all[i]
		}
	}
	switch {
	case target == nil:
		return nil, fmt.Errorf("%s %s not found", ref.Kind, ref.Name)
	case target.sql == "":
		return nil, fmt.Errorf("%s was created by a UNIQUE or PRIMARY KEY constraint and cannot be renamed", target.name)
	}
	for _, e := range all {
		if strings.EqualFold(e.name, newName) && !strings.EqualFold(e.name, target.name) {
			return nil, fmt.Errorf("there is already a %s named %s", e.kind, e.name)
		}
	}
	created, ok := renameCreate(target.sql, newName)
	if !ok {
		return nil, fmt.Errorf("could not read the definition of %s", target.name)
	}
	stmts := []string{"DROP " + dropKeyword[target.kind] + " " + quote(target.name), created}
	if target.kind == "view" {
		for _, e := range all {
			attached := e.kind == "trigger" && strings.EqualFold(e.table, target.name)
			switch {
			case e == *target:
			case attached:
				tr, ok := retarget(e.sql, newName)
				if !ok {
					return nil, fmt.Errorf("could not read the definition of trigger %s", e.name)
				}
				stmts = append(stmts, tr)
			case (e.kind == "view" || e.kind == "trigger") && mentions(e.sql, target.name):
				return nil, fmt.Errorf("%s %s uses view %s and would break; rename both in the SQL editor", e.kind, e.name, target.name)
			}
		}
	}
	return inTransaction(stmts), nil
}

// objectName finds the name token of CREATE VIEW / INDEX / TRIGGER text:
// the span to replace, schema qualifier included, and the index of the
// token after it.
func objectName(toks []token) (start, end, next int, ok bool) {
	i := 0
	for i < len(toks) && !(toks[i].is("VIEW") || toks[i].is("INDEX") || toks[i].is("TRIGGER")) {
		i++
	}
	i++
	if i+2 < len(toks) && toks[i].is("IF") {
		i += 3 // IF NOT EXISTS
	}
	if i >= len(toks) {
		return 0, 0, 0, false
	}
	start, end, next = toks[i].start, toks[i].end, i+1
	if i+2 < len(toks) && toks[i+1].text == "." {
		end, next = toks[i+2].end, i+3
	}
	return start, end, next, true
}

// renameCreate rewrites the object name in CREATE VIEW / INDEX / TRIGGER text.
func renameCreate(sql, newName string) (string, bool) {
	start, end, _, ok := objectName(lex(sql))
	if !ok {
		return "", false
	}
	return sql[:start] + quote(newName) + sql[end:], true
}

// retarget points CREATE TRIGGER text at another table or view.
func retarget(sql, table string) (string, bool) {
	toks := lex(sql)
	_, _, i, ok := objectName(toks)
	if !ok {
		return "", false
	}
	for ; i+1 < len(toks); i++ {
		if toks[i].is("ON") {
			start, end := toks[i+1].start, toks[i+1].end
			if i+3 < len(toks) && toks[i+2].text == "." {
				end = toks[i+3].end
			}
			return sql[:start] + quote(table) + sql[end:], true
		}
	}
	return "", false
}
