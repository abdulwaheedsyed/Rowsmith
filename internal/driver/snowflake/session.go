package snowflake

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	sf "github.com/snowflakedb/gosnowflake/v2"

	"rowsmith/internal/driver"
	"rowsmith/internal/driver/sqlbase"
	"rowsmith/internal/sqlsplit"
)

// NewSession pins one Snowflake session for the console, so USE, ALTER
// SESSION and transactions carry from one run to the next.
func (c *conn) NewSession(ctx context.Context, s driver.Scope) (driver.Session, error) {
	sc, err := c.db.Conn(ctx)
	if err != nil {
		return nil, mapError(err, sqlsplit.Statement{})
	}
	use := func(q string) error {
		if _, err := sc.ExecContext(ctx, q); err != nil {
			sc.Close()
			return mapError(err, sqlsplit.Statement{})
		}
		return nil
	}
	if s.Database != "" {
		if err := use("USE DATABASE " + quote(s.Database)); err != nil {
			return nil, err
		}
		if s.Schema != "" {
			if err := use("USE SCHEMA " + quote(s.Database) + "." + quote(s.Schema)); err != nil {
				return nil, err
			}
		}
	}
	return sqlbase.NewSession(sc, dialect{}, sqlbase.SessionHooks{
		Prepare: func(_ context.Context, _ *sql.Conn, st sqlsplit.Statement, _ driver.ExecOptions) (string, error) {
			if fileTransfer(st.SQL) {
				return "", errFileTransfer
			}
			return "", nil
		},
		InTx: func(ctx context.Context, sc *sql.Conn) (bool, error) {
			var id sql.NullString
			err := sc.QueryRowContext(ctx, "SELECT CURRENT_TRANSACTION()").Scan(&id)
			return id.Valid && id.String != "", err
		},
		MapError: mapError,
		Context: func(ctx context.Context, _ sqlsplit.Statement, _ driver.ExecOptions) context.Context {
			return noFileTransfer(ctx)
		},
		Close: func(sc *sql.Conn) {
			_, _ = sc.ExecContext(context.Background(), "ROLLBACK")
		},
	}), nil
}

// PUT and GET copy files between a stage and the client's file system, here
// the Rowsmith server's: the console refuses them.

var errFileTransfer = &driver.QueryError{Message: "PUT and GET read and write files on the machine running Rowsmith, so the console does not run them. Load files into a stage with SnowSQL, the Snowflake CLI or Snowsight, then COPY INTO from the stage here."}

// noFileTransfer tells gosnowflake not to act on a PUT or GET that reaches
// the server anyway (its own check differs from the console's): the server's
// transfer instructions are then ignored and no local file is touched.
func noFileTransfer(ctx context.Context) context.Context {
	return context.WithValue(ctx, sf.ContextKey("SKIP_FILE_TRANSFER"), true)
}

func fileTransfer(stmt string) bool {
	w := sqlsplit.Words(stmt, sqlsplit.Snowflake, 1)
	return len(w) == 1 && (w[0] == "PUT" || w[0] == "GET")
}

// ---- plans ------------------------------------------------------------------------

// Explain runs EXPLAIN USING JSON. Snowflake has no EXPLAIN ANALYZE; with
// analyze, the plan carries the query's partitions and bytes, which is what
// pruning decides.
func (c *conn) Explain(ctx context.Context, s driver.Scope, stmt string, _ bool) (*driver.Plan, error) {
	sc, err := c.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer sc.Close()
	if s.Database != "" {
		q := "USE DATABASE " + quote(s.Database)
		if s.Schema != "" {
			q = "USE SCHEMA " + quote(s.Database) + "." + quote(s.Schema)
		}
		if _, err := sc.ExecContext(ctx, q); err != nil {
			return nil, mapError(err, sqlsplit.Statement{})
		}
	}
	var raw string
	if err := sc.QueryRowContext(ctx, "EXPLAIN USING JSON "+stmt).Scan(&raw); err != nil {
		return nil, mapError(err, sqlsplit.Statement{SQL: stmt, Line: 1})
	}
	return parsePlan(raw), nil
}

type sfPlan struct {
	GlobalStats map[string]any `json:"GlobalStats"`
	Operations  [][]sfOp       `json:"Operations"`
}

type sfOp struct {
	ID          int      `json:"id"`
	Operation   string   `json:"operation"`
	Objects     []string `json:"objects"`
	Expressions []string `json:"expressions"`
	Parents     []int    `json:"parentOperators"`
	Partitions  *float64 `json:"partitionsAssigned"`
	Total       *float64 `json:"partitionsTotal"`
	Bytes       *float64 `json:"bytesAssigned"`
}

// parsePlan builds the operator tree from EXPLAIN's flat list: each
// operator names its parents.
func parsePlan(raw string) *driver.Plan {
	plan := &driver.Plan{Raw: raw, Format: "json", Totals: map[string]any{}}
	var p sfPlan
	if json.Unmarshal([]byte(raw), &p) != nil {
		return plan
	}
	for k, v := range p.GlobalStats {
		plan.Totals[k] = v
	}
	nodes := map[int]*driver.PlanNode{}
	var ops []sfOp
	for _, step := range p.Operations {
		ops = append(ops, step...)
	}
	for _, o := range ops {
		n := &driver.PlanNode{Operation: o.Operation, Props: map[string]string{}}
		if len(o.Objects) > 0 {
			n.Object = strings.Join(o.Objects, ", ")
		}
		if len(o.Expressions) > 0 {
			n.Props["expressions"] = strings.Join(o.Expressions, ", ")
		}
		if o.Partitions != nil && o.Total != nil {
			n.Detail = fmt.Sprintf("%.0f of %.0f partitions", *o.Partitions, *o.Total)
		}
		if o.Bytes != nil {
			n.Props["bytes assigned"] = fmt.Sprintf("%.0f", *o.Bytes)
		}
		nodes[o.ID] = n
	}
	for _, o := range ops {
		n := nodes[o.ID]
		if len(o.Parents) == 0 {
			if plan.Root == nil {
				plan.Root = n
			} else {
				plan.Root.Children = append(plan.Root.Children, n)
			}
			continue
		}
		if parent, ok := nodes[o.Parents[0]]; ok {
			parent.Children = append(parent.Children, n)
		}
	}
	return plan
}

// ---- administration ---------------------------------------------------------------

// Processes lists queries running or queued in the last hour that the role
// can see. Reading query history runs on the warehouse.
func (c *conn) Processes(ctx context.Context) (*driver.Result, error) {
	res, err := sqlbase.Collect(ctx, dialect{}, c.db, 0, `SELECT query_id AS id, user_name AS "user", warehouse_name AS warehouse,
		execution_status AS status, start_time, ROUND(DATEDIFF(millisecond, start_time, CURRENT_TIMESTAMP()) / 1000, 1) AS seconds,
		query_text AS query
		FROM TABLE(INFORMATION_SCHEMA.QUERY_HISTORY(END_TIME_RANGE_START => DATEADD(hour, -1, CURRENT_TIMESTAMP()), RESULT_LIMIT => 1000))
		WHERE execution_status IN ('RUNNING', 'QUEUED', 'RESUMING_WAREHOUSE', 'BLOCKED') AND query_id <> LAST_QUERY_ID(0)
		ORDER BY start_time`)
	if err != nil && c.database == "" {
		return nil, errors.New("set a database on the connection: Snowflake reads running queries from a database's INFORMATION_SCHEMA")
	}
	return res, mapError(err, sqlsplit.Statement{})
}

var queryIDRe = regexp.MustCompile(`^[0-9a-f-]{20,40}$`)

func (c *conn) KillProcess(ctx context.Context, id string) error {
	if !queryIDRe.MatchString(id) {
		return fmt.Errorf("invalid query id %q", id)
	}
	var out string
	if err := c.db.QueryRowContext(ctx, "SELECT SYSTEM$CANCEL_QUERY(?)", id).Scan(&out); err != nil {
		return mapError(err, sqlsplit.Statement{})
	}
	if !strings.Contains(strings.ToLower(out), "terminated") && !strings.Contains(strings.ToLower(out), "cancel") {
		return errors.New(out)
	}
	return nil
}

// Variables shows session parameters ("variables") or the warehouses the
// role can see, with their state and load ("status").
func (c *conn) Variables(ctx context.Context, kind string) (*driver.Result, error) {
	q := "SHOW PARAMETERS"
	if kind == "status" {
		q = "SHOW WAREHOUSES"
	}
	res, err := sqlbase.Collect(ctx, dialect{}, c.db, 0, q)
	return res, mapError(err, sqlsplit.Statement{})
}

// Users lists users (needs MANAGE GRANTS or a role that owns them) and
// otherwise the roles visible to the connection's role.
func (c *conn) Users(ctx context.Context) (*driver.Result, error) {
	res, err := sqlbase.Collect(ctx, dialect{}, c.db, 0, "SHOW USERS")
	if err == nil {
		return res, nil
	}
	res, err = sqlbase.Collect(ctx, dialect{}, c.db, 0, "SHOW ROLES")
	return res, mapError(err, sqlsplit.Statement{})
}

func (c *conn) UserGrants(ctx context.Context, user string) ([]string, error) {
	var out []string
	for _, q := range []string{"SHOW GRANTS TO USER " + quote(user), "SHOW GRANTS TO ROLE " + quote(user)} {
		rows, err := c.show(ctx, q)
		if err != nil {
			continue
		}
		for _, r := range rows {
			on := r.str("granted_on")
			if r.str("role") != "" && on == "" {
				out = append(out, "GRANT ROLE "+r.str("role")+" TO USER "+quote(user))
				continue
			}
			out = append(out, fmt.Sprintf("GRANT %s ON %s %s TO %s %s", r.str("privilege"), on, r.str("name"), r.str("granted_to"), r.str("grantee_name")))
		}
	}
	return out, nil
}

// Classify implements driver.Classifier.
func (c *conn) Classify(stmt string) driver.StatementKind {
	k, _ := sqlsplit.Classify(stmt, sqlsplit.Snowflake)
	return k
}
