package clickhouse

import (
	"context"
	sqldriver "database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	ch "github.com/ClickHouse/clickhouse-go/v2"

	"rowsmith/internal/driver"
	"rowsmith/internal/driver/sqlbase"
	"rowsmith/internal/sqlsplit"
)

// Explain shows the query plan with the indexes it uses and the rows it
// expects to read. With analyze, a read-only query also runs (its rows are
// discarded) and what it actually read is reported.
func (c *conn) Explain(ctx context.Context, s driver.Scope, stmt string, analyze bool) (*driver.Plan, error) {
	stmt = strings.TrimRight(strings.TrimSpace(stmt), ";")
	ctx = c.stateful(ctx)
	sc, err := c.db.Conn(ctx)
	if err != nil {
		return nil, mapError(err, sqlsplit.Statement{})
	}
	// USE changes the connection's database: it isn't returned to the pool.
	defer func() {
		_ = sc.Raw(func(any) error { return sqldriver.ErrBadConn })
		sc.Close()
	}()
	if s.Database != "" {
		if _, err := sc.ExecContext(ctx, "USE "+quote(s.Database)); err != nil {
			return nil, mapError(err, sqlsplit.Statement{})
		}
	}
	lines, err := sqlbase.Strings(ctx, sc, "EXPLAIN json = 1, indexes = 1, description = 1 "+stmt)
	if err != nil {
		return nil, mapError(err, sqlsplit.Statement{Line: 1})
	}
	raw := strings.Join(lines, "\n")
	plan := &driver.Plan{Raw: raw, Format: "json", Totals: map[string]any{}}
	var doc []struct {
		Plan map[string]any `json:"Plan"`
	}
	if err := json.Unmarshal([]byte(raw), &doc); err == nil && len(doc) > 0 {
		plan.Root = planNode(doc[0].Plan)
	}
	// The plan's totals are times in milliseconds, or text.
	if est, err := sc.QueryContext(ctx, "EXPLAIN ESTIMATE "+stmt); err == nil {
		var rowsTotal, parts, marks uint64
		perTable := map[string]float64{}
		for est.Next() {
			var db, table string
			var p, r, m uint64
			if est.Scan(&db, &table, &p, &r, &m) == nil {
				parts, rowsTotal, marks = parts+p, rowsTotal+r, marks+m
				perTable[db+"."+table] += float64(r)
			}
		}
		est.Close()
		if parts > 0 {
			plan.Totals["rows to read (est.)"] = thousands(rowsTotal)
			plan.Totals["parts"] = thousands(parts)
			plan.Totals["granules"] = thousands(marks)
			setReadRows(plan.Root, perTable)
		}
	}
	if analyze {
		if kind, _ := sqlsplit.Classify(stmt, sqlsplit.ClickHouse); kind != driver.StmtRead {
			return nil, errors.New("only a read-only query can be analyzed: ClickHouse runs it to measure it")
		}
		p := &progress{}
		start := time.Now()
		rows, err := sc.QueryContext(ch.Context(ctx, ch.WithProgress(p.add)), stmt)
		if err != nil {
			return nil, mapError(err, sqlsplit.Statement{Line: 1})
		}
		var n float64
		for rows.Next() {
			n++
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, mapError(err, sqlsplit.Statement{Line: 1})
		}
		read, bytes, _ := p.take()
		plan.Totals["time"] = float64(time.Since(start).Microseconds()) / 1000
		plan.Totals["rows read"] = thousands(read)
		plan.Totals["read"] = readableSize(bytes)
		plan.Totals["rows returned"] = thousands(uint64(n))
		if plan.Root != nil {
			plan.Root.ActualRows = &n
		}
	}
	return plan, nil
}

// planNode turns {"Node Type", "Description", "Indexes", "Plans"} into a node.
func planNode(m map[string]any) *driver.PlanNode {
	n := &driver.PlanNode{Props: map[string]string{}}
	n.Operation, _ = m["Node Type"].(string)
	desc, _ := m["Description"].(string)
	if n.Operation == "ReadFromMergeTree" || strings.HasPrefix(n.Operation, "ReadFrom") {
		n.Object = desc
	} else {
		n.Detail = desc
	}
	if ixs, ok := m["Indexes"].([]any); ok {
		var notes []string
		for _, it := range ixs {
			ix, _ := it.(map[string]any)
			typ, _ := ix["Type"].(string)
			sel, init := num(ix["Selected Granules"]), num(ix["Initial Granules"])
			keys := []string{}
			if ks, ok := ix["Keys"].([]any); ok {
				for _, k := range ks {
					keys = append(keys, fmt.Sprint(k))
				}
			}
			notes = append(notes, fmt.Sprintf("%s (%s): %v of %v granules", typ, strings.Join(keys, ", "), sel, init))
			if cond, ok := ix["Condition"].(string); ok && cond != "true" {
				n.Props[typ+" condition"] = cond
			}
		}
		n.Props["indexes"] = strings.Join(notes, "; ")
	}
	for k, v := range m {
		switch k {
		case "Node Type", "Node Id", "Description", "Indexes", "Plans":
			continue
		}
		if s, ok := v.(string); ok {
			n.Props[strings.ToLower(k)] = s
		} else if f, ok := v.(float64); ok {
			n.Props[strings.ToLower(k)] = fmt.Sprint(f)
		}
	}
	if kids, ok := m["Plans"].([]any); ok {
		for _, k := range kids {
			if km, ok := k.(map[string]any); ok {
				n.Children = append(n.Children, planNode(km))
			}
		}
	}
	if len(n.Props) == 0 {
		n.Props = nil
	}
	return n
}

// setReadRows puts each table's estimated rows on the steps that read it.
func setReadRows(n *driver.PlanNode, perTable map[string]float64) {
	if n == nil {
		return
	}
	if r, ok := perTable[n.Object]; ok && strings.HasPrefix(n.Operation, "ReadFrom") {
		n.Rows = &r
	}
	for _, c := range n.Children {
		setReadRows(c, perTable)
	}
}

// thousands writes 1234567 as 1,234,567.
func thousands(n uint64) string {
	s := fmt.Sprint(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

func readableSize(b uint64) string {
	units := []string{"B", "KB", "MB", "GB", "TB"}
	f, u := float64(b), 0
	for f >= 1024 && u < len(units)-1 {
		f, u = f/1024, u+1
	}
	if u == 0 {
		return fmt.Sprintf("%d B", b)
	}
	return fmt.Sprintf("%.1f %s", f, units[u])
}

func num(v any) float64 {
	f, _ := v.(float64)
	return f
}

func (c *conn) Processes(ctx context.Context) (*driver.Result, error) {
	res, err := sqlbase.Collect(ctx, dialect{}, c.db, 0, `SELECT query_id AS id, user, address AS client, round(elapsed, 1) AS seconds,
		read_rows, formatReadableSize(read_bytes) AS read, formatReadableSize(memory_usage) AS memory, current_database AS database, query
		FROM system.processes WHERE query_id != queryID() ORDER BY elapsed DESC`)
	return res, mapError(err, sqlsplit.Statement{})
}

var queryIDRe = regexp.MustCompile(`^[\w.:-]{1,128}$`)

func (c *conn) KillProcess(ctx context.Context, id string) error {
	if !queryIDRe.MatchString(id) {
		return fmt.Errorf("invalid query id %q", id)
	}
	_, err := c.db.ExecContext(ctx, "KILL QUERY WHERE query_id = "+literal(id))
	return mapError(err, sqlsplit.Statement{})
}

func (c *conn) Variables(ctx context.Context, kind string) (*driver.Result, error) {
	q := `SELECT name, value, changed, description FROM system.settings ORDER BY changed DESC, name`
	if kind == "status" {
		q = `SELECT metric AS name, toString(value) AS value, 'current' AS kind, description FROM system.metrics
			UNION ALL SELECT event, toString(value), 'since start', description FROM system.events
			UNION ALL SELECT metric, toString(value), 'sampled', description FROM system.asynchronous_metrics
			ORDER BY name`
	}
	res, err := sqlbase.Collect(ctx, dialect{}, c.db, 0, q)
	return res, mapError(err, sqlsplit.Statement{})
}

func (c *conn) Users(ctx context.Context) (*driver.Result, error) {
	res, err := sqlbase.Collect(ctx, dialect{}, c.db, 0, `SELECT name, 'user' AS kind, toString(auth_type) AS authentication, storage,
		arrayStringConcat(default_roles_list, ', ') AS default_roles FROM system.users
		UNION ALL SELECT name, 'role', '', storage, '' FROM system.roles ORDER BY kind DESC, name`)
	return res, mapError(err, sqlsplit.Statement{})
}

func (c *conn) UserGrants(ctx context.Context, user string) ([]string, error) {
	lines, err := sqlbase.Strings(ctx, c.db, "SHOW GRANTS FOR "+quote(user))
	return lines, mapError(err, sqlsplit.Statement{})
}
