package mysql

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"rowsmith/internal/driver"
	"rowsmith/internal/driver/sqlbase"
)

// ---- EXPLAIN -------------------------------------------------------------------

func (c *conn) Explain(ctx context.Context, s driver.Scope, stmt string, analyze bool) (*driver.Plan, error) {
	sc, err := c.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer sc.Close()
	if s.Database != "" {
		if _, err := sc.ExecContext(ctx, "USE "+quote(s.Database)); err != nil {
			return nil, err
		}
	}
	treeOK := c.flavor == "mysql" && (c.major > 8 || c.major == 8)
	var q, format string
	switch {
	case analyze && c.flavor == "mariadb":
		q, format = "ANALYZE FORMAT=JSON "+stmt, "json"
	case analyze && treeOK:
		q, format = "EXPLAIN ANALYZE "+stmt, "text"
	case analyze:
		return nil, errors.New("EXPLAIN ANALYZE needs MySQL 8.0.18+ or MariaDB")
	case treeOK:
		q, format = "EXPLAIN FORMAT=TREE "+stmt, "text"
	default:
		q, format = "EXPLAIN FORMAT=JSON "+stmt, "json"
	}
	var raw string
	if err := sc.QueryRowContext(ctx, q).Scan(&raw); err != nil {
		if format == "text" && !analyze {
			// FORMAT=TREE is unavailable before 8.0.16; fall back to JSON.
			q, format = "EXPLAIN FORMAT=JSON "+stmt, "json"
			if err2 := sc.QueryRowContext(ctx, q).Scan(&raw); err2 != nil {
				return nil, err2
			}
		} else {
			return nil, err
		}
	}
	plan := &driver.Plan{Raw: raw, Format: format}
	if format == "text" {
		plan.Root = parseTree(raw)
	} else {
		var v any
		if err := json.Unmarshal([]byte(raw), &v); err == nil {
			nodes := walkJSON("query", v)
			if len(nodes) == 1 {
				plan.Root = nodes[0]
			} else {
				plan.Root = &driver.PlanNode{Operation: "Query", Children: nodes}
			}
		}
	}
	return plan, nil
}

var (
	costRe   = regexp.MustCompile(`\(cost=([\d.e+]+)(?:\.\.([\d.e+]+))? rows=([\d.e+]+)\)`)
	actualRe = regexp.MustCompile(`\(actual time=([\d.e+]+)\.\.([\d.e+]+) rows=([\d.e+]+) loops=([\d.e+]+)\)`)
)

// parseTree parses MySQL's "-> Operation (cost=.. rows=..) (actual ...)" tree.
func parseTree(s string) *driver.PlanNode {
	root := &driver.PlanNode{Operation: "Query"}
	type frame struct {
		indent int
		node   *driver.PlanNode
	}
	stack := []frame{{indent: -1, node: root}}
	for _, line := range strings.Split(s, "\n") {
		i := strings.Index(line, "-> ")
		if i < 0 {
			continue
		}
		text := line[i+3:]
		n := &driver.PlanNode{}
		if m := actualRe.FindStringSubmatch(text); m != nil {
			n.TimeMS = pf(m[2])
			n.ActualRows = pf(m[3])
			n.Loops = pf(m[4])
			text = strings.Replace(text, m[0], "", 1)
		}
		if m := costRe.FindStringSubmatch(text); m != nil {
			if m[2] != "" {
				n.Cost = pf(m[2])
			} else {
				n.Cost = pf(m[1])
			}
			n.Rows = pf(m[3])
			text = strings.Replace(text, m[0], "", 1)
		}
		text = strings.TrimSpace(text)
		n.Operation, n.Object, n.Detail = splitOp(text)
		for len(stack) > 1 && stack[len(stack)-1].indent >= i {
			stack = stack[:len(stack)-1]
		}
		parent := stack[len(stack)-1].node
		parent.Children = append(parent.Children, n)
		stack = append(stack, frame{indent: i, node: n})
	}
	if len(root.Children) == 1 {
		return root.Children[0]
	}
	return root
}

var onRe = regexp.MustCompile(`^(.*?) on (\S+)(.*)$`)

func splitOp(text string) (op, obj, detail string) {
	if i := strings.Index(text, ": "); i > 0 && !strings.Contains(text[:i], " on ") {
		return text[:i], "", text[i+2:]
	}
	if m := onRe.FindStringSubmatch(text); m != nil {
		return m[1], m[2], strings.TrimSpace(m[3])
	}
	return text, "", ""
}

func pf(s string) *float64 {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil
	}
	return &f
}

var accessLabels = map[string]string{
	"ALL": "Full table scan", "index": "Full index scan", "range": "Index range scan", "ref": "Index lookup",
	"eq_ref": "Unique index lookup", "const": "Constant row", "system": "System row", "ref_or_null": "Index lookup (or null)",
	"index_merge": "Index merge", "fulltext": "Full-text search", "unique_subquery": "Unique subquery", "index_subquery": "Index subquery",
}

// walkJSON turns EXPLAIN FORMAT=JSON (MySQL 5.7 / MariaDB) into plan nodes.
func walkJSON(key string, v any) []*driver.PlanNode {
	switch x := v.(type) {
	case []any:
		var out []*driver.PlanNode
		for _, e := range x {
			out = append(out, walkJSON(key, e)...)
		}
		return out
	case map[string]any:
		if key == "table" {
			n := &driver.PlanNode{Props: map[string]string{}}
			at, _ := x["access_type"].(string)
			n.Operation = accessLabels[at]
			if n.Operation == "" {
				n.Operation = "Table access"
			}
			n.Object, _ = x["table_name"].(string)
			if k, ok := x["key"].(string); ok {
				n.Detail = "using " + k
			}
			if cond, ok := x["attached_condition"].(string); ok {
				n.Props["condition"] = cond
			}
			n.Rows = num(x["rows_examined_per_scan"])
			if n.Rows == nil {
				n.Rows = num(x["rows"])
			}
			if r, ok := x["r_rows"]; ok {
				n.ActualRows = num(r)
			}
			if ci, ok := x["cost_info"].(map[string]any); ok {
				n.Cost = num(ci["prefix_cost"])
			}
			for _, k := range sortedKeys(x) {
				switch k {
				case "materialized_from_subquery", "attached_subqueries", "nested_loop", "query_block":
					n.Children = append(n.Children, walkJSON(k, x[k])...)
				}
			}
			return []*driver.PlanNode{n}
		}
		var children []*driver.PlanNode
		for _, k := range sortedKeys(x) {
			switch x[k].(type) {
			case map[string]any, []any:
				children = append(children, walkJSON(k, x[k])...)
			}
		}
		label := humanize(key)
		switch key {
		case "query", "nested_loop", "query_specifications":
			return children
		}
		n := &driver.PlanNode{Operation: label, Children: children}
		if ci, ok := x["cost_info"].(map[string]any); ok {
			n.Cost = num(ci["query_cost"])
		}
		for _, flag := range []string{"using_filesort", "using_temporary_table"} {
			if b, _ := x[flag].(bool); b {
				n.Detail = strings.TrimSpace(n.Detail + " " + strings.ReplaceAll(flag, "_", " "))
			}
		}
		return []*driver.PlanNode{n}
	}
	return nil
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func humanize(k string) string {
	k = strings.ReplaceAll(k, "_", " ")
	if k == "" {
		return k
	}
	return strings.ToUpper(k[:1]) + k[1:]
}

func num(v any) *float64 {
	switch x := v.(type) {
	case float64:
		return &x
	case string:
		return pf(x)
	}
	return nil
}

// ---- server administration -----------------------------------------------------

func (c *conn) Processes(ctx context.Context) (*driver.Result, error) {
	return sqlbase.Collect(ctx, dialect{c: c}, c.db, 0, "SHOW FULL PROCESSLIST")
}

func (c *conn) KillProcess(ctx context.Context, id string) error {
	n, err := strconv.ParseUint(id, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid process id %q", id)
	}
	_, err = c.db.ExecContext(ctx, "KILL "+strconv.FormatUint(n, 10))
	return err
}

func (c *conn) Variables(ctx context.Context, kind string) (*driver.Result, error) {
	q := "SHOW VARIABLES"
	if kind == "status" {
		q = "SHOW GLOBAL STATUS"
	}
	return sqlbase.Collect(ctx, dialect{c: c}, c.db, 0, q)
}

func (c *conn) Users(ctx context.Context) (*driver.Result, error) {
	return sqlbase.Collect(ctx, dialect{c: c}, c.db, 0, "SELECT User, Host FROM mysql.user ORDER BY User, Host")
}

func (c *conn) UserGrants(ctx context.Context, user string) ([]string, error) {
	u, h, ok := strings.Cut(user, "@")
	if !ok {
		h = "%"
	}
	q := "SHOW GRANTS FOR '" + strings.ReplaceAll(u, "'", "''") + "'@'" + strings.ReplaceAll(h, "'", "''") + "'"
	return sqlbase.Strings(ctx, c.db, q)
}
