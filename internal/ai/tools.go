package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"rowsmith/internal/driver"
	"rowsmith/internal/export"
	"rowsmith/internal/sqlsplit"
)

// Tools give the assistant read-only access to the connection it is helping
// with. Schema tools are always available; row tools only when the policy
// allows the assistant to see data.

const (
	maxToolText = 24 << 10 // characters of one tool result sent to the model
	maxCellText = 160      // characters of one value in row results
	maxListed   = 400      // objects in list_tables
)

type toolEnv struct {
	conn     driver.Conn
	info     driver.Info
	scope    driver.Scope
	data     bool // sample_rows and run_query allowed
	product  string
	calls    []string
	roSess   driver.Session
	openSess func(ctx context.Context) (driver.Session, error)
}

func strProp(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

// toolSpec is a provider-neutral tool definition; every property is
// required and no others are allowed, which strict modes expect.
type toolSpec struct {
	Name        string
	Description string
	Properties  map[string]any
}

func (t toolSpec) schema() map[string]any {
	req := make([]string, 0, len(t.Properties))
	for k := range t.Properties {
		req = append(req, k)
	}
	sort.Strings(req)
	return map[string]any{"type": "object", "properties": t.Properties, "required": req, "additionalProperties": false}
}

func (e *toolEnv) definitions() []toolSpec {
	tools := []toolSpec{
		{"list_tables", "List the tables, views and other objects in the current database/schema, with row estimates and comments. Use a pattern to narrow long lists.",
			map[string]any{"pattern": strProp("Case-insensitive substring to match object names; empty for all")}},
		{"describe_table", "Show a table's or view's columns (type, nullability, default, keys), indexes, foreign keys and comment.",
			map[string]any{"table": strProp("Table or view name, exactly as listed"), "schema": strProp("Schema, or empty for the current one")}},
	}
	if e.info.Caps.Explain {
		tools = append(tools, toolSpec{"check_query", "Check that a statement is valid against the real schema by asking the database for its execution plan, without running it. Returns the plan summary or the database's error.",
			map[string]any{"sql": strProp("One complete statement")}})
	}
	if e.data {
		tools = append(tools,
			toolSpec{"sample_rows", "Read a few rows of a table to learn what its values look like (formats, codes, typical content).",
				map[string]any{"table": strProp("Table or view name"), "schema": strProp("Schema, or empty for the current one"), "limit": map[string]any{"type": "integer", "description": "Rows to read, 1-10"}}},
			toolSpec{"run_query", "Run one read-only statement and see up to 50 rows of its result, to check an answer or look something up. Statements that could change data are refused.",
				map[string]any{"sql": strProp("One read-only statement")}},
		)
	}
	return tools
}

// run executes one tool call and returns text for the model.
func (e *toolEnv) run(ctx context.Context, name string, raw json.RawMessage) (string, error) {
	e.calls = append(e.calls, name)
	var in struct {
		Pattern string `json:"pattern"`
		Table   string `json:"table"`
		Schema  string `json:"schema"`
		SQL     string `json:"sql"`
		Limit   int    `json:"limit"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return "", fmt.Errorf("invalid input: %v", err)
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	switch name {
	case "list_tables":
		return e.listTables(ctx, in.Pattern)
	case "describe_table":
		return e.describe(ctx, in.Schema, in.Table)
	case "check_query":
		return e.checkQuery(ctx, in.SQL)
	case "sample_rows":
		if !e.data {
			return "", errors.New("reading rows is not allowed on this connection")
		}
		return e.sample(ctx, in.Schema, in.Table, in.Limit)
	case "run_query":
		if !e.data {
			return "", errors.New("running queries is not allowed on this connection")
		}
		return e.runQuery(ctx, in.SQL)
	}
	return "", fmt.Errorf("unknown tool %q", name)
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := s[:n]
	for len(cut) > 0 && !utf8Start(cut[len(cut)-1]) {
		cut = cut[:len(cut)-1]
	}
	return cut + fmt.Sprintf("… [%d more characters omitted]", len(s)-len(cut))
}

func utf8Start(b byte) bool { return b < 0x80 || b >= 0xC0 }

func (e *toolEnv) ref(schema, name string) driver.ObjectRef {
	r := driver.ObjectRef{Database: e.scope.Database, Schema: e.scope.Schema, Name: name}
	if schema != "" {
		r.Schema = schema
	}
	return r
}

func (e *toolEnv) listTables(ctx context.Context, pattern string) (string, error) {
	objs, err := e.conn.Objects(ctx, e.scope)
	if err != nil {
		return "", err
	}
	p := strings.ToLower(strings.TrimSpace(pattern))
	var b strings.Builder
	n := 0
	for _, o := range objs {
		if o.Extension != "" || (p != "" && !strings.Contains(strings.ToLower(o.Name), p)) {
			continue
		}
		if n == maxListed {
			b.WriteString("… more objects; use a pattern to narrow the list\n")
			break
		}
		n++
		fmt.Fprintf(&b, "%s (%s)", o.Name, o.Kind)
		if o.Rows != nil {
			fmt.Fprintf(&b, ", ~%d rows", *o.Rows)
		}
		if o.Comment != "" {
			b.WriteString(" — " + clip(o.Comment, 120))
		}
		b.WriteByte('\n')
	}
	if n == 0 {
		return "No objects match.", nil
	}
	return b.String(), nil
}

func (e *toolEnv) describe(ctx context.Context, schema, name string) (string, error) {
	t, err := e.conn.Describe(ctx, e.ref(schema, name))
	if err != nil {
		return "", err
	}
	return clip(describeText(t), maxToolText), nil
}

// describeText is a compact description of a table for the model.
func describeText(t *driver.Table) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s", t.Kind, t.Ref.Name)
	if t.RowEstimate != nil {
		fmt.Fprintf(&b, " (~%d rows)", *t.RowEstimate)
	}
	if t.Comment != "" {
		b.WriteString(" — " + clip(t.Comment, 300))
	}
	b.WriteString("\nColumns:\n")
	pk := map[string]bool{}
	for _, k := range t.PrimaryKey {
		pk[k] = true
	}
	for _, c := range t.Columns {
		fmt.Fprintf(&b, "  %s %s", c.Name, c.Type)
		if pk[c.Name] {
			b.WriteString(" PRIMARY KEY")
		}
		if !c.Nullable {
			b.WriteString(" NOT NULL")
		}
		if c.AutoIncrement {
			b.WriteString(" auto-increment")
		}
		if c.Default != nil && *c.Default != "" {
			b.WriteString(" DEFAULT " + clip(*c.Default, 80))
		}
		if c.Generated != "" {
			b.WriteString(" GENERATED AS (" + clip(c.Generated, 120) + ")")
		}
		if len(c.Enum) > 0 {
			b.WriteString(" values: " + clip(strings.Join(c.Enum, ", "), 200))
		}
		if c.Comment != "" {
			b.WriteString(" — " + clip(c.Comment, 160))
		}
		b.WriteByte('\n')
	}
	for _, ix := range t.Indexes {
		if ix.Primary {
			continue
		}
		kind := "INDEX"
		if ix.Unique {
			kind = "UNIQUE INDEX"
		}
		fmt.Fprintf(&b, "%s %s (%s)", kind, ix.Name, strings.Join(ix.Columns, ", "))
		if ix.Type != "" {
			b.WriteString(" " + ix.Type)
		}
		if ix.Where != "" {
			b.WriteString(" WHERE " + clip(ix.Where, 120))
		}
		b.WriteByte('\n')
	}
	for _, fk := range t.ForeignKeys {
		fmt.Fprintf(&b, "FOREIGN KEY (%s) → %s(%s)\n", strings.Join(fk.Columns, ", "), fk.RefTable.Name, strings.Join(fk.RefColumns, ", "))
	}
	for _, fk := range t.Referenced {
		if fk.Table != nil {
			fmt.Fprintf(&b, "Referenced by %s(%s)\n", fk.Table.Name, strings.Join(fk.Columns, ", "))
		}
	}
	for _, ch := range t.Checks {
		fmt.Fprintf(&b, "CHECK %s: %s\n", ch.Name, clip(ch.Expression, 160))
	}
	if t.Definition != "" {
		b.WriteString("Definition:\n" + clip(t.Definition, 4000) + "\n")
	}
	return b.String()
}

func (e *toolEnv) checkQuery(ctx context.Context, sql string) (string, error) {
	ex, ok := e.conn.(driver.Explainer)
	if !ok {
		return "", errors.New("this engine cannot check statements")
	}
	sql = strings.TrimSpace(strings.TrimRight(strings.TrimSpace(sql), ";"))
	if sql == "" {
		return "", errors.New("empty statement")
	}
	plan, err := ex.Explain(ctx, e.scope, sql, false) // never ANALYZE: nothing runs
	if err != nil {
		var qe *driver.QueryError
		if errors.As(err, &qe) {
			return "", errors.New(qe.Message)
		}
		return "", err
	}
	var b strings.Builder
	b.WriteString("The statement is valid. Plan:\n")
	var walk func(n *driver.PlanNode, depth int)
	lines := 0
	walk = func(n *driver.PlanNode, depth int) {
		if n == nil || lines > 40 {
			return
		}
		lines++
		fmt.Fprintf(&b, "%s%s", strings.Repeat("  ", depth), n.Operation)
		if n.Object != "" {
			b.WriteString(" on " + n.Object)
		}
		if n.Detail != "" {
			b.WriteString(" (" + clip(n.Detail, 160) + ")")
		}
		b.WriteByte('\n')
		for _, c := range n.Children {
			walk(c, depth+1)
		}
	}
	walk(plan.Root, 0)
	return clip(b.String(), maxToolText), nil
}

func (e *toolEnv) sample(ctx context.Context, schema, name string, limit int) (string, error) {
	if limit < 1 || limit > 10 {
		limit = 5
	}
	res, err := e.conn.Browse(ctx, driver.BrowseRequest{Ref: e.ref(schema, name), Limit: limit})
	if err != nil {
		return "", err
	}
	return rowsText(res.Columns, res.Rows, len(res.Rows) >= limit && res.Truncated), nil
}

func (e *toolEnv) runQuery(ctx context.Context, sql string) (string, error) {
	sql = strings.TrimSpace(sql)
	if e.info.Caps.SQL {
		stmts := sqlsplit.Split(sql, sqlsplit.Dialect(e.info.Dialect))
		if len(stmts) != 1 {
			return "", errors.New("run exactly one statement")
		}
		if k := stmts[0].Kind; k != driver.StmtRead {
			return "", fmt.Errorf("only read-only statements can run here (this one is %s)", k)
		}
		sql = stmts[0].SQL
	}
	if e.roSess == nil {
		s, err := e.openSess(ctx)
		if err != nil {
			return "", err
		}
		e.roSess = s
	}
	sink := &collectSink{max: 50}
	err := e.roSess.Execute(ctx, sql, driver.ExecOptions{MaxRows: 50, ReadOnly: true, StopOnError: true}, sink)
	if err == nil {
		err = sink.err
	}
	if err != nil {
		var qe *driver.QueryError
		if errors.As(err, &qe) {
			return "", errors.New(qe.Message)
		}
		return "", err
	}
	if sink.cols == nil {
		return "The statement ran and returned no rows.", nil
	}
	return rowsText(sink.cols, sink.rows, sink.truncated), nil
}

func (e *toolEnv) close() {
	if e.roSess != nil {
		e.roSess.Close()
	}
}

// rowsText renders rows as a compact table for the model.
func rowsText(cols []driver.ResultColumn, rows [][]any, more bool) string {
	var b strings.Builder
	names := make([]string, len(cols))
	for i, c := range cols {
		names[i] = c.Name
	}
	b.WriteString(strings.Join(names, " | ") + "\n")
	for _, r := range rows {
		vals := make([]string, len(cols))
		for i := range cols {
			var v any
			if i < len(r) {
				v = r[i]
			}
			s, null := export.Text(v)
			if null {
				s = "NULL"
			}
			vals[i] = clip(strings.ReplaceAll(s, "\n", " "), maxCellText)
		}
		b.WriteString(strings.Join(vals, " | ") + "\n")
	}
	fmt.Fprintf(&b, "(%d rows%s)", len(rows), map[bool]string{true: ", more exist", false: ""}[more])
	return clip(b.String(), maxToolText)
}

// collectSink keeps the first result set of a statement.
type collectSink struct {
	max       int
	cols      []driver.ResultColumn
	rows      [][]any
	truncated bool
	err       error
	done      bool
}

func (s *collectSink) BeginStatement(driver.StatementInfo) error { return nil }
func (s *collectSink) Columns(c []driver.ResultColumn) error {
	if s.cols == nil {
		s.cols = c
	} else {
		s.done = true
	}
	return nil
}
func (s *collectSink) Rows(r [][]any) error {
	if s.done {
		return nil
	}
	for _, row := range r {
		if len(s.rows) >= s.max {
			s.truncated = true
			return nil
		}
		s.rows = append(s.rows, row)
	}
	return nil
}
func (s *collectSink) EndResult(sum driver.ResultSummary) error {
	if !s.done {
		s.truncated = s.truncated || sum.Truncated
		if s.cols != nil {
			s.done = true
		}
	}
	return nil
}
func (s *collectSink) Notice(string, string) error { return nil }
func (s *collectSink) EndStatement(err error) error {
	if err != nil && s.err == nil {
		s.err = err
	}
	return nil
}

// schemaOverview lists every table with its columns when that fits, so most
// questions need no tool call; larger schemas get names only.
func schemaOverview(ctx context.Context, conn driver.Conn, scope driver.Scope) string {
	ctx, cancel := context.WithTimeout(ctx, driver.CatalogTimeout)
	defer cancel()
	if cat, ok := conn.(driver.Catalog); ok {
		if tables, err := cat.CatalogColumns(ctx, scope); err == nil && len(tables) > 0 {
			sort.Slice(tables, func(i, j int) bool { return tables[i].Name < tables[j].Name })
			var b strings.Builder
			for _, t := range tables {
				fks := map[string]string{}
				for _, fk := range t.FKs {
					for i, c := range fk.Columns {
						if i < len(fk.RefColumns) {
							fks[c] = fk.RefTable.Name + "." + fk.RefColumns[i]
						}
					}
				}
				fmt.Fprintf(&b, "%s", t.Name)
				if t.Kind != "" && t.Kind != "table" {
					b.WriteString(" [" + t.Kind + "]")
				}
				b.WriteString(": ")
				for i, c := range t.Columns {
					if i > 0 {
						b.WriteString(", ")
					}
					b.WriteString(c.Name + " " + c.Type)
					if c.PK {
						b.WriteString(" PK")
					}
					if r, ok := fks[c.Name]; ok {
						b.WriteString(" → " + r)
					}
				}
				b.WriteByte('\n')
			}
			if b.Len() <= 60_000 {
				return b.String()
			}
			var names strings.Builder
			names.WriteString("(The schema is large: only table names are listed. Use describe_table for columns.)\n")
			for _, t := range tables {
				fmt.Fprintf(&names, "%s (%d columns)\n", t.Name, len(t.Columns))
			}
			return clip(names.String(), 60_000)
		}
	}
	objs, err := conn.Objects(ctx, scope)
	if err != nil {
		return "(The object list could not be read: " + err.Error() + ")"
	}
	var b strings.Builder
	b.WriteString("(Columns are not listed here; use describe_table.)\n")
	for i, o := range objs {
		if o.Extension != "" {
			continue
		}
		if i >= 2000 {
			b.WriteString("… more objects; use list_tables with a pattern\n")
			break
		}
		fmt.Fprintf(&b, "%s (%s)\n", o.Name, o.Kind)
	}
	return b.String()
}
