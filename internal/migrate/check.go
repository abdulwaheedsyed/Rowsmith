package migrate

import (
	"fmt"
	"regexp"
	"strings"

	"rowsmith/internal/driver"
)

var (
	quotedRe = regexp.MustCompile(`'(?:[^']|'')*'`)
	typeRe   = regexp.MustCompile(`^[A-Za-z"][A-Za-z0-9_ (),.=\[\]"-]*$`)
)

// Validate checks a plan that came back from the browser before it runs:
// the engines must be the ones connected, and names and types must be
// plain identifiers and type names.
func Validate(p *Plan, src, dst Endpoint) error {
	if p.SourceEngine != src.engine() || p.TargetEngine != dst.engine() {
		return fmt.Errorf("the plan was made for other connections; plan it again")
	}
	p.SameEngine = family(src.engine()) == family(dst.engine())
	p.Options.normalize()
	n := 0
	for _, t := range p.Tables {
		if !t.Include {
			continue
		}
		n++
		t.Source.Database, t.Source.Schema = src.Scope.Database, src.Scope.Schema
		if err := checkName(t.Target); err != nil {
			return fmt.Errorf("table %s: %w", t.Source.Name, err)
		}
		cols := 0
		seen := map[string]bool{}
		for _, c := range t.Columns {
			if !c.Include {
				continue
			}
			cols++
			if err := checkName(c.Target); err != nil {
				return fmt.Errorf("%s.%s: %w", t.Source.Name, c.Source, err)
			}
			if seen[strings.ToLower(c.Target)] {
				return fmt.Errorf("%s has two columns named %s", t.Target, c.Target)
			}
			seen[strings.ToLower(c.Target)] = true
			if dst.engine() != MongoDB {
				if stripped := quotedRe.ReplaceAllString(c.Type, "v"); len(c.Type) > 400 || !typeRe.MatchString(stripped) {
					return fmt.Errorf("%s.%s: %q is not a type name", t.Source.Name, c.Source, c.Type)
				}
			}
			if c.Default != nil && (len(*c.Default) > 400 || strings.ContainsAny(*c.Default, ";")) {
				return fmt.Errorf("%s.%s: the default is not a single expression", t.Source.Name, c.Source)
			}
			if !p.SameEngine {
				c.Generated = "" // only same-engine plans recreate computed columns
			}
		}
		if cols == 0 {
			return fmt.Errorf("%s has no columns to create", t.Source.Name)
		}
	}
	if n == 0 {
		return ErrNothing
	}
	return nil
}

func checkName(s string) error {
	switch {
	case strings.TrimSpace(s) == "":
		return fmt.Errorf("a name is empty")
	case len(s) > 128:
		return fmt.Errorf("%q is longer than 128 characters", s)
	case strings.ContainsAny(s, "\x00\r\n"):
		return fmt.Errorf("%q contains control characters", s)
	}
	return nil
}

// Preview returns the statements that would create one table of the plan.
func Preview(p *Plan, source string, dst Endpoint) ([]string, error) {
	r := &run{p: p, dst: dst, byName: map[string]*TablePlan{}, failed: map[string]bool{}}
	var tp *TablePlan
	for _, t := range p.Tables {
		if t.Include {
			r.byName[t.Source.Name] = t
		}
		if t.Source.Name == source {
			tp = t
		}
	}
	if tp == nil {
		return nil, fmt.Errorf("the plan has no table %s", source)
	}
	gen, ok := dst.Conn.(driver.DDLGenerator)
	if !ok {
		return nil, fmt.Errorf("Rowsmith cannot create tables in %s", dst.Info.Name)
	}
	def := r.createDef(tp)
	out, err := gen.CreateTableSQL(def)
	if err != nil {
		return nil, err
	}
	if dst.engine() == SQLite || dst.engine() == MongoDB {
		return out, nil
	}
	// The keys added after the rows, as changes to the new table.
	from := &driver.Table{Ref: def.Ref, PrimaryKey: def.PrimaryKey, Checks: def.Checks, Comment: def.Comment, Options: def.Options, Kind: "table"}
	for _, c := range def.Columns {
		from.Columns = append(from.Columns, c.Column)
	}
	to := defFrom(from)
	to.Indexes = append(to.Indexes, r.indexes(tp)...)
	if p.Options.ForeignKeys {
		to.ForeignKeys = append(to.ForeignKeys, r.foreignKeys(tp)...)
	}
	if len(to.Indexes) > 0 || len(to.ForeignKeys) > 0 {
		more, err := gen.AlterTableSQL(from, to)
		if err != nil {
			return out, err
		}
		out = append(out, more...)
	}
	return out, nil
}
