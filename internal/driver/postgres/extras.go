package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"rowsmith/internal/driver"
	"rowsmith/internal/driver/sqlbase"
	"rowsmith/internal/sqlsplit"
)

// Explain runs EXPLAIN (FORMAT JSON). With analyze, the statement executes
// inside a transaction that is always rolled back, so writes never persist.
func (c *conn) Explain(ctx context.Context, s driver.Scope, stmt string, analyze bool) (*driver.Plan, error) {
	p, err := c.pool(ctx, s.Database)
	if err != nil {
		return nil, err
	}
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if s.Schema != "" {
		if _, err := tx.ExecContext(ctx, "SET LOCAL search_path TO "+quote(s.Schema)+", public"); err != nil {
			return nil, err
		}
	}
	opts := "FORMAT JSON, VERBOSE"
	if analyze {
		opts += ", ANALYZE, BUFFERS"
	}
	var raw string
	if err := tx.QueryRowContext(ctx, "EXPLAIN ("+opts+") "+stmt).Scan(&raw); err != nil {
		return nil, mapConnError(err)
	}
	var doc []map[string]any
	if err := json.Unmarshal([]byte(raw), &doc); err != nil || len(doc) == 0 {
		return &driver.Plan{Raw: raw, Format: "json"}, nil
	}
	plan := &driver.Plan{Raw: raw, Format: "json", Totals: map[string]any{}}
	if root, ok := doc[0]["Plan"].(map[string]any); ok {
		plan.Root = pgNode(root)
	}
	for _, k := range []string{"Planning Time", "Execution Time"} {
		if v, ok := doc[0][k]; ok {
			plan.Totals[k] = v
		}
	}
	return plan, nil
}

var pgNodeProps = []string{"Filter", "Index Cond", "Recheck Cond", "Hash Cond", "Merge Cond", "Join Filter", "Join Type",
	"Sort Key", "Group Key", "Strategy", "Scan Direction", "Rows Removed by Filter", "Shared Hit Blocks", "Shared Read Blocks",
	"Workers Planned", "Workers Launched", "Parent Relationship", "Subplan Name", "Sort Method", "Sort Space Used", "Peak Memory Usage"}

func pgNode(m map[string]any) *driver.PlanNode {
	n := &driver.PlanNode{Props: map[string]string{}}
	n.Operation, _ = m["Node Type"].(string)
	if rel, ok := m["Relation Name"].(string); ok {
		n.Object = rel
		if a, ok := m["Alias"].(string); ok && a != rel {
			n.Object += " " + a
		}
	}
	if ix, ok := m["Index Name"].(string); ok {
		n.Detail = "using " + ix
	}
	n.Cost = f(m["Total Cost"])
	n.Rows = f(m["Plan Rows"])
	n.ActualRows = f(m["Actual Rows"])
	n.TimeMS = f(m["Actual Total Time"])
	n.Loops = f(m["Actual Loops"])
	for _, k := range pgNodeProps {
		if v, ok := m[k]; ok {
			switch x := v.(type) {
			case string:
				n.Props[k] = x
			case []any:
				parts := make([]string, len(x))
				for i, e := range x {
					parts[i] = fmt.Sprint(e)
				}
				n.Props[k] = strings.Join(parts, ", ")
			default:
				n.Props[k] = fmt.Sprint(x)
			}
		}
	}
	if kids, ok := m["Plans"].([]any); ok {
		for _, k := range kids {
			if km, ok := k.(map[string]any); ok {
				n.Children = append(n.Children, pgNode(km))
			}
		}
	}
	return n
}

func f(v any) *float64 {
	if x, ok := v.(float64); ok {
		return &x
	}
	return nil
}

func (c *conn) Processes(ctx context.Context) (*driver.Result, error) {
	p, err := c.pool(ctx, "")
	if err != nil {
		return nil, err
	}
	return sqlbase.Collect(ctx, dialect{}, p.db, 0, `SELECT pid, usename AS "user", datname AS "database", client_addr::text AS client,
		application_name AS application, state, backend_start, xact_start, query_start,
		CASE WHEN state = 'active' THEN round(extract(epoch FROM now() - query_start)::numeric, 1) END AS "seconds",
		wait_event_type, wait_event, query
		FROM pg_stat_activity WHERE pid <> pg_backend_pid() AND backend_type = 'client backend' ORDER BY query_start DESC NULLS LAST`)
}

func (c *conn) KillProcess(ctx context.Context, id string) error {
	pid, err := strconv.Atoi(id)
	if err != nil {
		return fmt.Errorf("invalid pid %q", id)
	}
	p, err := c.pool(ctx, "")
	if err != nil {
		return err
	}
	var ok bool
	if err := p.db.QueryRowContext(ctx, "SELECT pg_terminate_backend($1)", pid).Scan(&ok); err != nil {
		return mapConnError(err)
	}
	if !ok {
		return fmt.Errorf("process %d was not terminated", pid)
	}
	return nil
}

func (c *conn) Variables(ctx context.Context, kind string) (*driver.Result, error) {
	p, err := c.pool(ctx, "")
	if err != nil {
		return nil, err
	}
	if kind == "status" {
		return sqlbase.Collect(ctx, dialect{}, p.db, 0, `SELECT datname AS "database", numbackends AS connections, xact_commit AS commits,
			xact_rollback AS rollbacks, blks_read, blks_hit,
			CASE WHEN blks_read + blks_hit > 0 THEN round(100.0 * blks_hit / (blks_read + blks_hit), 2) END AS cache_hit_pct,
			tup_returned, tup_fetched, tup_inserted, tup_updated, tup_deleted, conflicts, deadlocks, temp_bytes
			FROM pg_stat_database WHERE datname IS NOT NULL ORDER BY datname`)
	}
	return sqlbase.Collect(ctx, dialect{}, p.db, 0, `SELECT name, setting, unit, category, short_desc AS description, source FROM pg_settings ORDER BY name`)
}

func (c *conn) Users(ctx context.Context) (*driver.Result, error) {
	p, err := c.pool(ctx, "")
	if err != nil {
		return nil, err
	}
	return sqlbase.Collect(ctx, dialect{}, p.db, 0, `SELECT rolname AS role, rolcanlogin AS login, rolsuper AS superuser, rolcreatedb AS create_db,
		rolcreaterole AS create_role, rolreplication AS replication, rolconnlimit AS conn_limit, rolvaliduntil AS valid_until,
		ARRAY(SELECT b.rolname FROM pg_auth_members m JOIN pg_roles b ON m.roleid = b.oid WHERE m.member = r.oid)::text AS member_of
		FROM pg_roles r WHERE rolname !~ '^pg_' ORDER BY rolname`)
}

func (c *conn) UserGrants(ctx context.Context, user string) ([]string, error) {
	p, err := c.pool(ctx, "")
	if err != nil {
		return nil, err
	}
	return sqlbase.Strings(ctx, p.db, `SELECT 'GRANT ' || string_agg(privilege_type, ', ') || ' ON ' || quote_ident(table_schema) || '.' || quote_ident(table_name) || ' TO ' || quote_ident(grantee)
		FROM information_schema.role_table_grants WHERE grantee = $1 GROUP BY table_schema, table_name, grantee ORDER BY table_schema, table_name`, user)
}

// Classify implements driver.Classifier.
func (c *conn) Classify(stmt string) driver.StatementKind {
	k, _ := sqlsplit.Classify(stmt, sqlsplit.Postgres)
	return k
}
