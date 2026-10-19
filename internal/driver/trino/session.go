package trino

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"rowsmith/internal/driver"
	"rowsmith/internal/driver/sqlbase"
	"rowsmith/internal/sqlsplit"
)

// NewSession pins one client connection for the console: USE, SET SESSION
// and START TRANSACTION carry from one run to the next (the client keeps
// them as request headers).
func (c *conn) NewSession(ctx context.Context, s driver.Scope) (driver.Session, error) {
	sc, err := c.db.Conn(ctx)
	if err != nil {
		return nil, mapError(err, sqlsplit.Statement{})
	}
	if s.Database != "" && s.Schema != "" {
		if _, err := sc.ExecContext(ctx, "USE "+quote(s.Database)+"."+quote(s.Schema)); err != nil {
			sc.Close()
			return nil, mapError(err, sqlsplit.Statement{})
		}
	}
	return sqlbase.NewSession(sc, dialect{}, sqlbase.SessionHooks{MapError: mapError}), nil
}

// ---- plans ------------------------------------------------------------------------

// Explain runs EXPLAIN (FORMAT JSON), or EXPLAIN ANALYZE, which runs the
// statement and reports what each step read and how long it took.
func (c *conn) Explain(ctx context.Context, s driver.Scope, stmt string, analyze bool) (*driver.Plan, error) {
	sc, err := c.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer sc.Close()
	if s.Database != "" && s.Schema != "" {
		if _, err := sc.ExecContext(ctx, "USE "+quote(s.Database)+"."+quote(s.Schema)); err != nil {
			return nil, mapError(err, sqlsplit.Statement{})
		}
	}
	if analyze {
		if k, _ := sqlsplit.Classify(stmt, sqlsplit.Trino); k != driver.StmtRead {
			return nil, fmt.Errorf("EXPLAIN ANALYZE runs the statement, so Rowsmith analyzes only queries that read")
		}
		var raw string
		if err := sc.QueryRowContext(ctx, "EXPLAIN ANALYZE "+stmt).Scan(&raw); err != nil {
			return nil, mapError(err, sqlsplit.Statement{SQL: stmt, Line: 1})
		}
		return parseAnalyze(raw), nil
	}
	var raw string
	if err := sc.QueryRowContext(ctx, "EXPLAIN (FORMAT JSON) "+stmt).Scan(&raw); err != nil {
		return nil, mapError(err, sqlsplit.Statement{SQL: stmt, Line: 1})
	}
	return parsePlan(raw), nil
}

type planNode struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	Descriptor map[string]string `json:"descriptor"`
	Details    []string          `json:"details"`
	Estimates  []struct {
		Rows *number `json:"outputRowCount"`
		Size *number `json:"outputSizeInBytes"`
		CPU  *number `json:"cpuCost"`
	} `json:"estimates"`
	Children []planNode `json:"children"`
}

func (n planNode) node() *driver.PlanNode {
	out := &driver.PlanNode{Operation: n.Name, Props: map[string]string{}}
	for k, v := range n.Descriptor {
		if k == "table" {
			// iceberg:shop.orders$data@2419997646423162496
			out.Object = v
			continue
		}
		out.Props[k] = v
	}
	if len(n.Details) > 0 {
		out.Detail = strings.Join(n.Details, "; ")
	}
	if len(n.Estimates) > 0 {
		e := n.Estimates[len(n.Estimates)-1]
		out.Rows, out.Cost = finite(e.Rows), finite(e.CPU)
		if s := finite(e.Size); s != nil {
			out.Props["estimated bytes"] = strconv.FormatFloat(*s, 'f', 0, 64)
		}
	}
	for _, ch := range n.Children {
		out.Children = append(out.Children, ch.node())
	}
	return out
}

// number reads an estimate: a number, or "NaN" when Trino cannot tell.
type number float64

func (n *number) UnmarshalJSON(b []byte) error {
	f, err := strconv.ParseFloat(strings.Trim(string(b), `"`), 64)
	*n = number(f)
	if err != nil {
		*n = number(math.NaN())
	}
	return nil
}

func finite(n *number) *float64 {
	if n == nil {
		return nil
	}
	f := float64(*n)
	if math.IsNaN(f) || math.IsInf(f, 0) || math.Abs(f) > 1e300 {
		return nil
	}
	return &f
}

// parsePlan reads EXPLAIN (FORMAT JSON): one plan per fragment, keyed "0", "1"…
func parsePlan(raw string) *driver.Plan {
	plan := &driver.Plan{Raw: raw, Format: "json"}
	var frags map[string]planNode
	if json.Unmarshal([]byte(raw), &frags) != nil || len(frags) == 0 {
		return plan
	}
	if len(frags) == 1 {
		for _, f := range frags {
			plan.Root = f.node()
		}
		return plan
	}
	nodes := map[string]*driver.PlanNode{}
	for id, f := range frags {
		nodes[id] = &driver.PlanNode{Operation: "Fragment " + id, Children: []*driver.PlanNode{f.node()}}
	}
	plan.Root = stitch(nodes)
	return plan
}

var sourceFragmentsRe = regexp.MustCompile(`sourceFragmentIds\s*=?\s*\[([\d, ]+)\]`)

// stitch hangs each fragment under the RemoteSource or RemoteMerge that
// reads it, so a distributed plan reads as one tree from the first fragment.
func stitch(frags map[string]*driver.PlanNode) *driver.PlanNode {
	used := map[string]bool{}
	var walk func(n *driver.PlanNode)
	walk = func(n *driver.PlanNode) {
		ids := n.Props["sourceFragmentIds"]
		if m := sourceFragmentsRe.FindStringSubmatch(n.Detail); m != nil {
			ids = m[1]
		}
		for _, id := range strings.Split(strings.Trim(ids, "[]"), ",") {
			if f, ok := frags[strings.TrimSpace(id)]; ok && !used[strings.TrimSpace(id)] {
				used[strings.TrimSpace(id)] = true
				n.Children = append(n.Children, f)
			}
		}
		for _, ch := range n.Children {
			walk(ch)
		}
	}
	ids := make([]int, 0, len(frags))
	for id := range frags {
		if n, err := strconv.Atoi(id); err == nil {
			ids = append(ids, n)
		}
	}
	slices.Sort(ids)
	root := &driver.PlanNode{Operation: "Query"}
	for _, n := range ids {
		id := strconv.Itoa(n)
		if used[id] {
			continue
		}
		used[id] = true
		walk(frags[id])
		root.Children = append(root.Children, frags[id])
	}
	if len(root.Children) == 1 {
		return root.Children[0]
	}
	return root
}

var (
	analyzeOpRe  = regexp.MustCompile(`^(\s*)(?:[└├]─\s*)?([A-Z][A-Za-z]+)(\[.*\])?\s*$`)
	analyzeCPURe = regexp.MustCompile(`CPU: ([\d.]+)(ns|us|ms|s|m)\b`)
	analyzeOutRe = regexp.MustCompile(`Output: ([\d,]+) rows?`)
	fragmentRe   = regexp.MustCompile(`^Fragment (\d+) \[(\w+)\]`)
)

// parseAnalyze reads EXPLAIN ANALYZE's text: fragments, and in each the
// operators drawn with └─, with their output rows and CPU time.
func parseAnalyze(raw string) *driver.Plan {
	plan := &driver.Plan{Raw: raw, Format: "text", Totals: map[string]any{}}
	frags := map[string]*driver.PlanNode{}
	type open struct {
		col  int
		node *driver.PlanNode
	}
	var frag *driver.PlanNode
	var stack []open
	for _, line := range strings.Split(raw, "\n") {
		if m := fragmentRe.FindStringSubmatch(line); m != nil {
			frag = &driver.PlanNode{Operation: "Fragment " + m[1], Detail: m[2], Props: map[string]string{}}
			frags[m[1]] = frag
			stack = nil
			continue
		}
		if frag == nil {
			for _, part := range strings.Split(line, ", ") {
				if k, v, ok := strings.Cut(part, ": "); ok {
					plan.Totals[strings.TrimSpace(k)] = strings.TrimSpace(v)
				}
			}
			continue
		}
		if m := analyzeOpRe.FindStringSubmatchIndex(line); m != nil {
			// Depth is where the operator's name starts, after any └─ branch.
			col := utf8.RuneCountInString(line[:m[4]])
			op, detail := line[m[4]:m[5]], ""
			if m[6] >= 0 {
				detail = line[m[6]:m[7]]
			}
			n := &driver.PlanNode{Operation: op, Detail: strings.TrimSuffix(strings.TrimPrefix(detail, "["), "]"), Props: map[string]string{}}
			for len(stack) > 0 && stack[len(stack)-1].col >= col {
				stack = stack[:len(stack)-1]
			}
			if len(stack) == 0 {
				frag.Children = append(frag.Children, n)
			} else {
				p := stack[len(stack)-1].node
				p.Children = append(p.Children, n)
			}
			stack = append(stack, open{col, n})
			continue
		}
		n := frag
		if len(stack) > 0 {
			n = stack[len(stack)-1].node
		}
		text := strings.TrimSpace(strings.TrimLeft(line, " │"))
		if strings.HasPrefix(text, "CPU:") {
			if m := analyzeCPURe.FindStringSubmatch(text); m != nil {
				if d, err := time.ParseDuration(m[1] + m[2]); err == nil {
					ms := float64(d.Microseconds()) / 1000
					n.TimeMS = &ms
				}
			}
			if m := analyzeOutRe.FindStringSubmatch(text); m != nil {
				if v, err := strconv.ParseFloat(strings.ReplaceAll(m[1], ",", ""), 64); err == nil {
					n.ActualRows = &v
				}
			}
		}
		if k, v, ok := strings.Cut(text, ": "); ok && k != "" && !strings.Contains(k, " := ") {
			n.Props[k] = v
		}
		if strings.HasPrefix(n.Operation, "TableScan") || strings.HasPrefix(n.Operation, "ScanFilter") {
			if i := strings.Index(n.Detail, "table = "); i >= 0 {
				n.Object = strings.TrimPrefix(n.Detail[i:], "table = ")
			}
		}
	}
	if len(frags) > 0 {
		plan.Root = stitch(frags)
	}
	return plan
}

// ---- administration ---------------------------------------------------------------

func (c *conn) Processes(ctx context.Context) (*driver.Result, error) {
	res, err := sqlbase.Collect(ctx, dialect{}, c.db, 0, `SELECT query_id AS id, state, "user", source, resource_group_id AS resource_group,
		created, round(date_diff('millisecond', created, now()) / 1000.0, 1) AS seconds, query
		FROM system.runtime.queries
		WHERE state IN ('QUEUED', 'WAITING_FOR_RESOURCES', 'DISPATCHING', 'PLANNING', 'STARTING', 'RUNNING', 'FINISHING')
		AND query NOT LIKE '%system.runtime.queries%' ORDER BY created`)
	return res, mapError(err, sqlsplit.Statement{})
}

var queryIDRe = regexp.MustCompile(`^\d{8}_\d{6}_\d{5}_\w{5}$`)

func (c *conn) KillProcess(ctx context.Context, id string) error {
	if !queryIDRe.MatchString(id) {
		return fmt.Errorf("invalid query id %q", id)
	}
	_, err := c.db.ExecContext(ctx, "CALL system.runtime.kill_query(query_id => "+literal(id)+", message => 'Stopped from Rowsmith')")
	return mapError(err, sqlsplit.Statement{})
}

// Variables shows session properties ("variables") or the cluster's nodes ("status").
func (c *conn) Variables(ctx context.Context, kind string) (*driver.Result, error) {
	q := "SHOW SESSION"
	if kind == "status" {
		q = "SELECT node_id, http_uri, node_version, coordinator, state FROM system.runtime.nodes ORDER BY coordinator DESC, node_id"
	}
	res, err := sqlbase.Collect(ctx, dialect{}, c.db, 0, q)
	return res, mapError(err, sqlsplit.Statement{})
}

// Classify implements driver.Classifier.
func (c *conn) Classify(stmt string) driver.StatementKind {
	k, _ := sqlsplit.Classify(stmt, sqlsplit.Trino)
	return k
}
