package databricks

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"rowsmith/internal/driver"
	"rowsmith/internal/driver/sqlbase"
	"rowsmith/internal/sqlsplit"
)

// NewSession pins one warehouse session for the console, so USE CATALOG,
// USE SCHEMA and SET carry from one run to the next.
func (c *conn) NewSession(ctx context.Context, s driver.Scope) (driver.Session, error) {
	sc, err := c.db.Conn(ctx)
	if err != nil {
		return nil, mapError(err, sqlsplit.Statement{})
	}
	for _, use := range [][2]string{{"CATALOG", s.Database}, {"SCHEMA", s.Schema}} {
		if use[1] == "" {
			continue
		}
		if _, err := sc.ExecContext(ctx, "USE "+use[0]+" "+quote(use[1])); err != nil {
			sc.Close()
			return nil, mapError(err, sqlsplit.Statement{})
		}
	}
	return sqlbase.NewSession(sc, dialect{}, sqlbase.SessionHooks{MapError: mapError}), nil
}

func (c *conn) Classify(stmt string) driver.StatementKind {
	k, _ := sqlsplit.Classify(stmt, sqlsplit.Databricks)
	return k
}

// ---- plans -------------------------------------------------------------------------

// Explain shows the physical plan (EXPLAIN). Databricks has no EXPLAIN
// ANALYZE: run-time figures are in the query profile, in the workspace.
func (c *conn) Explain(ctx context.Context, s driver.Scope, stmt string, analyze bool) (*driver.Plan, error) {
	if analyze {
		return nil, errors.New("Databricks has no EXPLAIN ANALYZE: run the query, then open its query profile in the workspace (Query history) for rows and times per step")
	}
	sc, err := c.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer sc.Close()
	for _, use := range [][2]string{{"CATALOG", s.Database}, {"SCHEMA", s.Schema}} {
		if use[1] != "" {
			if _, err := sc.ExecContext(ctx, "USE "+use[0]+" "+quote(use[1])); err != nil {
				return nil, mapError(err, sqlsplit.Statement{})
			}
		}
	}
	rows, err := sc.QueryContext(ctx, "EXPLAIN "+strings.TrimSuffix(strings.TrimSpace(stmt), ";"))
	if err != nil {
		return nil, mapError(err, sqlsplit.Statement{SQL: stmt, Line: 1})
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var line string
		if rows.Scan(&line) == nil {
			b.WriteString(line)
		}
	}
	raw := b.String()
	// EXPLAIN reports errors as a plan: "== Physical Plan ==\nError occurred…".
	if i := strings.Index(raw, "Error occurred during query planning"); i >= 0 {
		return nil, &driver.QueryError{Message: strings.TrimSpace(strings.TrimPrefix(raw[i:], "Error occurred during query planning:"))}
	}
	return parsePlan(raw), nil
}

var (
	sectionRe = regexp.MustCompile(`^== (.+) ==\s*$`)
	nodeRe    = regexp.MustCompile(`^([ :|]*)(?:[+:]- )?(\S.*)$`)
	scanRe    = regexp.MustCompile(`Scan (?:\w+ )?([\w.` + "`" + `]+)\[`)
)

// parsePlan reads the physical plan section of EXPLAIN: one operator per
// line, children drawn with +- and :- under their parent.
func parsePlan(raw string) *driver.Plan {
	plan := &driver.Plan{Raw: raw, Format: "text", Totals: map[string]any{}}
	type open struct {
		col  int
		node *driver.PlanNode
	}
	var stack []open
	section := ""
	for _, line := range strings.Split(raw, "\n") {
		if m := sectionRe.FindStringSubmatch(line); m != nil {
			section = m[1]
			continue
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		switch section {
		case "Physical Plan":
		case "Photon Explanation":
			plan.Totals["Photon"] = strings.TrimSpace(line)
			continue
		default:
			continue
		}
		text := strings.TrimLeft(line, " :|+-")
		if strings.HasPrefix(text, "== ") {
			continue // "+- == Initial Plan ==" inside an adaptive plan
		}
		col := strings.Index(line, text)
		op, detail, _ := strings.Cut(text, " ")
		if i := strings.IndexAny(op, "(["); i > 0 {
			op, detail = op[:i], op[i:]+" "+detail
		}
		n := &driver.PlanNode{Operation: op, Detail: strings.TrimSpace(detail)}
		if m := scanRe.FindStringSubmatch(text); m != nil {
			n.Object = m[1]
		}
		if len(n.Detail) > 400 {
			n.Detail = n.Detail[:400] + "…"
		}
		for len(stack) > 0 && stack[len(stack)-1].col >= col {
			stack = stack[:len(stack)-1]
		}
		if len(stack) == 0 {
			if plan.Root == nil {
				plan.Root = n
			} else {
				plan.Root.Children = append(plan.Root.Children, n)
			}
		} else {
			p := stack[len(stack)-1].node
			p.Children = append(p.Children, n)
		}
		stack = append(stack, open{col, n})
	}
	return plan
}

// ---- administration -----------------------------------------------------------------

// Processes lists the warehouse's queued and running queries from the
// query history.
func (c *conn) Processes(ctx context.Context) (*driver.Result, error) {
	if c.warehouse == "" {
		return nil, errors.New("the HTTP path names no SQL warehouse")
	}
	q := url.Values{}
	q.Set("filter_by.warehouse_ids", c.warehouse)
	q.Add("filter_by.statuses", "QUEUED")
	q.Add("filter_by.statuses", "RUNNING")
	q.Set("max_results", "100")
	var reply struct {
		Res []struct {
			QueryID   string `json:"query_id"`
			Status    string `json:"status"`
			User      string `json:"user_name"`
			Query     string `json:"query_text"`
			StartMS   int64  `json:"query_start_time_ms"`
			Statement string `json:"statement_type"`
			Source    struct {
				Name string `json:"client_application"`
			} `json:"query_source"`
		} `json:"res"`
	}
	if err := c.api(ctx, "GET", "/api/2.0/sql/history/queries?"+q.Encode(), &reply); err != nil {
		return nil, err
	}
	res := &driver.Result{Columns: []driver.ResultColumn{
		{Name: "id", Type: "string", Kind: driver.KindString}, {Name: "status", Type: "string", Kind: driver.KindString},
		{Name: "user", Type: "string", Kind: driver.KindString}, {Name: "seconds", Type: "double", Kind: driver.KindFloat},
		{Name: "statement", Type: "string", Kind: driver.KindString}, {Name: "query", Type: "string", Kind: driver.KindText}}, Rows: [][]any{}}
	for _, r := range reply.Res {
		secs := float64(time.Now().UnixMilli()-r.StartMS) / 1000
		res.Rows = append(res.Rows, []any{r.QueryID, r.Status, r.User, float64(int(secs*10)) / 10, r.Statement, r.Query})
	}
	return res, nil
}

func (c *conn) KillProcess(ctx context.Context, id string) error {
	if !regexp.MustCompile(`^[0-9a-f-]+$`).MatchString(id) {
		return fmt.Errorf("invalid query id %q", id)
	}
	var out struct{}
	if err := c.api(ctx, "POST", "/api/2.0/sql/statements/"+id+"/cancel", &out); err != nil {
		return fmt.Errorf("the query could not be stopped from here (%v); stop it from Query history in the workspace", err)
	}
	return nil
}

// Variables shows the session's settings ("variables") or the warehouse's
// state ("status").
func (c *conn) Variables(ctx context.Context, kind string) (*driver.Result, error) {
	if kind == "status" {
		w, err := c.warehouseState(ctx)
		if err != nil {
			return nil, err
		}
		pairs := map[string]string{
			"name": w.Name, "state": w.State, "size": w.ClusterSize, "type": strings.ToLower(w.WarehouseType),
			"serverless": strconv.FormatBool(w.Serverless), "clusters": fmt.Sprintf("%d (min %d, max %d)", w.NumClusters, w.MinClusters, w.MaxClusters),
			"auto stop": fmt.Sprintf("%d minutes", w.AutoStopMins), "active sessions": strconv.Itoa(w.ActiveSessions), "channel": strings.ToLower(w.Channel.Name),
		}
		keys := make([]string, 0, len(pairs))
		for k := range pairs {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		res := &driver.Result{Columns: []driver.ResultColumn{{Name: "name", Type: "string", Kind: driver.KindString},
			{Name: "value", Type: "string", Kind: driver.KindString}}, Rows: [][]any{}}
		for _, k := range keys {
			res.Rows = append(res.Rows, []any{k, pairs[k]})
		}
		return res, nil
	}
	res, err := sqlbase.Collect(ctx, dialect{}, c.db, 0, "SET -v")
	return res, mapError(err, sqlsplit.Statement{})
}
