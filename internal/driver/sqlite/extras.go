package sqlite

import (
	"context"
	"errors"
	"regexp"
	"strings"

	"rowsmith/internal/driver"
	"rowsmith/internal/driver/sqlbase"
	"rowsmith/internal/sqlsplit"
)

// Explain runs EXPLAIN QUERY PLAN. SQLite has no EXPLAIN ANALYZE.
func (c *conn) Explain(ctx context.Context, _ driver.Scope, stmt string, analyze bool) (*driver.Plan, error) {
	if analyze {
		return nil, driver.ErrNotSupported
	}
	stmt = strings.TrimSpace(stmt)
	if stmt == "" {
		return nil, errors.New("nothing to explain")
	}
	if err := checkFragment(stmt); err != nil {
		return nil, err
	}
	rows, err := c.db.QueryContext(ctx, "EXPLAIN QUERY PLAN "+stmt)
	if err != nil {
		return nil, mapError(err, sqlsplit.Statement{})
	}
	defer rows.Close()
	var steps []planRow
	for rows.Next() {
		var r planRow
		var notUsed int64
		if err := rows.Scan(&r.id, &r.parent, &notUsed, &r.detail); err != nil {
			return nil, err
		}
		steps = append(steps, r)
	}
	if err := rows.Err(); err != nil {
		return nil, mapError(err, sqlsplit.Statement{})
	}
	return buildPlan(steps), nil
}

// planRow is one row of EXPLAIN QUERY PLAN output.
type planRow struct {
	id, parent int64
	detail     string
}

type planStep struct {
	detail   string
	children []*planStep
}

// buildPlan assembles EXPLAIN QUERY PLAN rows, which reference their parent
// by id (0 for top-level steps), into a tree, and renders it the way the
// sqlite3 shell does.
func buildPlan(rows []planRow) *driver.Plan {
	top := &planStep{}
	byID := map[int64]*planStep{0: top}
	for _, r := range rows {
		s := &planStep{detail: r.detail}
		parent, ok := byID[r.parent]
		if !ok {
			parent = top
		}
		parent.children = append(parent.children, s)
		byID[r.id] = s
	}
	var raw strings.Builder
	raw.WriteString("QUERY PLAN\n")
	renderPlan(&raw, top.children, "")
	root := &driver.PlanNode{Operation: "Query plan"}
	for _, s := range top.children {
		root.Children = append(root.Children, planNode(s))
	}
	if len(root.Children) == 1 {
		root = root.Children[0]
	}
	return &driver.Plan{Root: root, Raw: strings.TrimRight(raw.String(), "\n"), Format: "text"}
}

func renderPlan(b *strings.Builder, steps []*planStep, prefix string) {
	for i, s := range steps {
		branch, indent := "|--", "|  "
		if i == len(steps)-1 {
			branch, indent = "`--", "   "
		}
		b.WriteString(prefix + branch + s.detail + "\n")
		renderPlan(b, s.children, prefix+indent)
	}
}

// accessRe matches table accesses such as "SEARCH o USING INDEX ix (a=?)".
var accessRe = regexp.MustCompile(`^(SCAN|SEARCH) (\S+)(?: (.+))?$`)

func planNode(s *planStep) *driver.PlanNode {
	n := &driver.PlanNode{Operation: s.detail}
	if m := accessRe.FindStringSubmatch(s.detail); m != nil && s.detail != "SCAN CONSTANT ROW" {
		n.Operation = map[string]string{"SCAN": "Scan", "SEARCH": "Search"}[m[1]]
		n.Object, n.Detail = m[2], m[3]
	}
	for _, c := range s.children {
		n.Children = append(n.Children, planNode(c))
	}
	return n
}

// variables are the PRAGMA values shown on the variables panel.
var variables = []string{"page_size", "page_count", "freelist_count", "journal_mode", "auto_vacuum", "encoding",
	"foreign_keys", "user_version", "application_id"}

// Variables implements driver.VariablesReader: "variables" lists database
// settings, "status" the options SQLite was compiled with.
func (c *conn) Variables(ctx context.Context, kind string) (*driver.Result, error) {
	if kind == "status" {
		return sqlbase.Collect(ctx, dialect{}, c.db, 0, "SELECT compile_options AS option FROM pragma_compile_options")
	}
	parts := make([]string, 0, len(variables)+1)
	for _, v := range variables {
		parts = append(parts, "SELECT '"+v+"' AS name, "+v+" AS value FROM pragma_"+v)
	}
	parts = append(parts, "SELECT 'sqlite_version', sqlite_version()")
	return sqlbase.Collect(ctx, dialect{}, c.db, 0, strings.Join(parts, " UNION ALL "))
}
