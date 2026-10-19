package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"rowsmith/internal/driver"
	"rowsmith/internal/driver/sqlbase"
)

// CockroachDB speaks the PostgreSQL protocol and emulates pg_catalog, but
// keeps sizes, statistics, sessions and plans in its own form. These are
// the places the driver asks it differently.

// crdbRowCounts returns the estimated rows of each table in a schema, from
// table statistics (pg_class.reltuples is always NULL there). SHOW TABLES
// reads them without crdb_internal, which recent versions restrict.
func crdbRowCounts(ctx context.Context, db *sql.DB, schema string) map[string]int64 {
	out := map[string]int64{}
	rows, err := db.QueryContext(ctx, `SELECT table_name, estimated_row_count FROM [SHOW TABLES FROM `+quote(schema)+`] WHERE type = 'table'`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var n sql.NullInt64
		if rows.Scan(&name, &n) == nil && n.Valid {
			out[name] = n.Int64
		}
	}
	return out
}

// crdbHideColumns removes hidden columns from a described table: the rowid
// CockroachDB adds to tables without a primary key, and its primary index.
// Such tables are then edited by rowid like PostgreSQL's by ctid, and
// exports and migrations leave the column out.
func crdbHideColumns(ctx context.Context, db *sql.DB, schema, name string, t *driver.Table) {
	hidden, err := sqlbase.Strings(ctx, db, `SELECT column_name FROM information_schema.columns
		WHERE table_schema = $1 AND table_name = $2 AND is_hidden = 'YES'`, schema, name)
	if err != nil || len(hidden) == 0 {
		return
	}
	isHidden := func(c string) bool { return slices.Contains(hidden, c) }
	t.Columns = slices.DeleteFunc(t.Columns, func(c driver.Column) bool { return isHidden(c.Name) })
	t.Indexes = slices.DeleteFunc(t.Indexes, func(ix driver.Index) bool { return slices.ContainsFunc(ix.Columns, isHidden) })
	if slices.ContainsFunc(t.PrimaryKey, isHidden) {
		t.PrimaryKey = nil
	}
}

// crdbExplain runs EXPLAIN (VERBOSE) or EXPLAIN ANALYZE (VERBOSE) inside a
// transaction that is rolled back, and reads its text tree.
func (c *conn) crdbExplain(ctx context.Context, p *pool, s driver.Scope, stmt string, analyze bool) (*driver.Plan, error) {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if s.Schema != "" {
		if _, err := tx.ExecContext(ctx, "SET LOCAL search_path TO "+quote(s.Schema)+", public"); err != nil {
			return nil, mapConnError(err)
		}
	}
	q := "EXPLAIN (VERBOSE) "
	if analyze {
		q = "EXPLAIN ANALYZE (VERBOSE) "
	}
	lines, err := sqlbase.Strings(ctx, tx, q+stmt)
	if err != nil {
		return nil, mapConnError(err)
	}
	return parseCrdbPlan(lines), nil
}

var crdbPropRe = regexp.MustCompile(`^([a-zA-Z][a-zA-Z0-9 _/()-]*?):\s*(.*)$`)

// parseCrdbPlan reads CockroachDB's plan tree:
//
//	planning time: 432µs
//	• sort
//	│ estimated row count: 10
//	└── • scan
//	      table: orders@idx_orders_customer
//
// Lines before the first node are totals; "key: value" lines belong to the
// node above them; a node's depth is the column of its bullet.
func parseCrdbPlan(lines []string) *driver.Plan {
	plan := &driver.Plan{Raw: strings.Join(lines, "\n"), Format: "text", Totals: map[string]any{}}
	type open struct {
		col  int
		node *driver.PlanNode
	}
	var stack []open
	for _, line := range lines {
		col := strings.Index(line, "•")
		if col >= 0 {
			n := &driver.PlanNode{Operation: strings.TrimSpace(line[col+len("•"):]), Props: map[string]string{}}
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
				parent := stack[len(stack)-1].node
				parent.Children = append(parent.Children, n)
			}
			stack = append(stack, open{col, n})
			continue
		}
		text := strings.TrimSpace(strings.TrimLeft(line, " │├└─"))
		m := crdbPropRe.FindStringSubmatch(text)
		if m == nil {
			continue
		}
		key, val := m[1], m[2]
		if len(stack) == 0 {
			if plan.Root == nil && !strings.HasPrefix(key, "index recommendations") {
				plan.Totals[key] = val
			}
			continue
		}
		n := stack[len(stack)-1].node
		switch key {
		case "table":
			obj, idx, _ := strings.Cut(val, "@")
			n.Object = obj
			if idx != "" {
				n.Detail = "using " + idx
			}
		case "estimated row count":
			n.Rows = crdbNumber(val)
		case "actual row count":
			n.ActualRows = crdbNumber(val)
		case "execution time":
			if d, err := time.ParseDuration(strings.ReplaceAll(strings.Fields(val)[0], "µ", "u")); err == nil {
				ms := float64(d.Microseconds()) / 1000
				n.TimeMS = &ms
			}
		}
		n.Props[key] = val
	}
	return plan
}

// crdbNumber reads the leading number of "6,000 (100% of the table; …)".
func crdbNumber(s string) *float64 {
	f := strings.Fields(s)
	if len(f) == 0 {
		return nil
	}
	v, err := strconv.ParseFloat(strings.ReplaceAll(f[0], ",", ""), 64)
	if err != nil {
		return nil
	}
	return &v
}

func (c *conn) crdbProcesses(ctx context.Context, p *pool) (*driver.Result, error) {
	return sqlbase.Collect(ctx, dialect{}, p.db, 0, `SELECT session_id AS id, user_name AS "user", client_address AS client,
		application_name AS application, status, session_start, active_query_start,
		CASE WHEN active_queries <> '' THEN round(extract(epoch FROM now() - active_query_start)::numeric, 1) END AS "seconds",
		active_queries AS query
		FROM [SHOW CLUSTER SESSIONS] WHERE status <> 'CLOSED' ORDER BY active_query_start DESC NULLS LAST`)
}

var crdbSessionRe = regexp.MustCompile(`^[0-9a-f]{16,40}$`)

func (c *conn) crdbKill(ctx context.Context, p *pool, id string) error {
	if !crdbSessionRe.MatchString(id) {
		return fmt.Errorf("invalid session id %q", id)
	}
	_, err := p.db.ExecContext(ctx, "CANCEL SESSION "+literal(id))
	return mapConnError(err)
}

// crdbVariables shows cluster and session settings ("variables"), or
// per-statement statistics ("status"): CockroachDB keeps node metrics in
// crdb_internal, which recent versions restrict, and shows them in its DB
// Console instead.
func (c *conn) crdbVariables(ctx context.Context, p *pool, kind string) (*driver.Result, error) {
	if kind == "status" {
		return sqlbase.Collect(ctx, dialect{}, p.db, 0, `SELECT job_type AS kind, count(*) AS jobs,
			count(*) FILTER (WHERE status = 'running') AS running, count(*) FILTER (WHERE status = 'failed') AS failed,
			max(created) AS last_created
			FROM [SHOW JOBS] GROUP BY job_type ORDER BY job_type`)
	}
	res, err := sqlbase.Collect(ctx, dialect{}, p.db, 0, `SELECT variable AS name, value, setting_type AS type, description, 'cluster' AS scope
		FROM [SHOW ALL CLUSTER SETTINGS] UNION ALL
		SELECT name, setting, NULL, short_desc, 'session' FROM pg_settings ORDER BY scope, name`)
	if err != nil {
		// Reading cluster settings needs the VIEWCLUSTERSETTING privilege.
		return sqlbase.Collect(ctx, dialect{}, p.db, 0, `SELECT name, setting AS value, short_desc AS description, 'session' AS scope FROM pg_settings ORDER BY name`)
	}
	return res, nil
}
