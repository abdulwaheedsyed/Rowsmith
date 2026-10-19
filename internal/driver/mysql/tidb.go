package mysql

import (
	"context"
	"database/sql"
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"time"

	"rowsmith/internal/driver"
)

// TiDB speaks MySQL's protocol and dialect but plans, stores and changes
// schemas its own way. These are the places the driver asks it differently.

// tidbExplain runs EXPLAIN [ANALYZE] FORMAT='tidb_json'. With analyze the
// statement runs inside a transaction that is rolled back.
func (c *conn) tidbExplain(ctx context.Context, sc *sql.Conn, stmt string, analyze bool) (*driver.Plan, error) {
	q := "EXPLAIN FORMAT='tidb_json' " + stmt
	if analyze {
		if _, err := sc.ExecContext(ctx, "BEGIN"); err != nil {
			return nil, err
		}
		defer sc.ExecContext(context.WithoutCancel(ctx), "ROLLBACK")
		q = "EXPLAIN ANALYZE FORMAT='tidb_json' " + stmt
	}
	var raw string
	if err := sc.QueryRowContext(ctx, q).Scan(&raw); err != nil {
		return nil, err
	}
	plan := &driver.Plan{Raw: raw, Format: "json"}
	var ops []tidbOp
	if err := json.Unmarshal([]byte(raw), &ops); err != nil || len(ops) == 0 {
		return plan, nil
	}
	if len(ops) == 1 {
		plan.Root = ops[0].node()
	} else {
		plan.Root = &driver.PlanNode{Operation: "Query"}
		for _, o := range ops {
			plan.Root.Children = append(plan.Root.Children, o.node())
		}
	}
	return plan, nil
}

type tidbOp struct {
	ID           string   `json:"id"`
	EstRows      string   `json:"estRows"`
	ActRows      string   `json:"actRows"`
	TaskType     string   `json:"taskType"`
	AccessObject string   `json:"accessObject"`
	OperatorInfo string   `json:"operatorInfo"`
	ExecuteInfo  string   `json:"executeInfo"`
	MemoryInfo   string   `json:"memoryInfo"`
	DiskInfo     string   `json:"diskInfo"`
	Sub          []tidbOp `json:"subOperators"`
}

var (
	tidbOpRe    = regexp.MustCompile(`^(.+?)_\d+(?:\((\w+)\))?$`)
	tidbTimeRe  = regexp.MustCompile(`^time:([\d.]+[µmn]?s)`)
	tidbTableRe = regexp.MustCompile(`table:([^,]+)(?:, index:(.+))?`)
)

// node turns "IndexRangeScan_15(Build)" with "table:orders, index:idx(…)"
// into an operation, object and detail.
func (o tidbOp) node() *driver.PlanNode {
	n := &driver.PlanNode{Operation: o.ID, Props: map[string]string{}}
	if m := tidbOpRe.FindStringSubmatch(o.ID); m != nil {
		n.Operation = m[1]
		if m[2] != "" {
			n.Props["side"] = m[2]
		}
	}
	if m := tidbTableRe.FindStringSubmatch(o.AccessObject); m != nil {
		n.Object = m[1]
		if m[2] != "" {
			n.Detail = "using " + m[2]
		}
	} else if o.AccessObject != "" {
		n.Detail = o.AccessObject
	}
	n.Rows = num(o.EstRows)
	n.ActualRows = num(o.ActRows)
	if m := tidbTimeRe.FindStringSubmatch(o.ExecuteInfo); m != nil {
		if d, err := time.ParseDuration(strings.ReplaceAll(m[1], "µ", "u")); err == nil {
			ms := float64(d.Microseconds()) / 1000
			n.TimeMS = &ms
		}
	}
	for k, v := range map[string]string{"task": o.TaskType, "operator": o.OperatorInfo, "execution": o.ExecuteInfo, "memory": o.MemoryInfo, "disk": o.DiskInfo} {
		if v != "" && v != "N/A" {
			n.Props[k] = v
		}
	}
	for _, s := range o.Sub {
		n.Children = append(n.Children, s.node())
	}
	return n
}

// tidbAlter splits an ALTER TABLE into one statement per change: TiDB's
// multi-schema change rejects some combinations, such as placing a new
// column after one renamed in the same statement.
func tidbAlter(target string, clauses []string) []string {
	out := make([]string, len(clauses))
	for i, c := range clauses {
		out[i] = "ALTER TABLE " + target + "\n  " + c
	}
	return out
}

// TiDB leaves information_schema.SCHEMA_PRIVILEGES and TABLE_PRIVILEGES
// empty, so database and table grants are read from mysql.db and
// mysql.tables_priv instead, in the same shape.

type privRow struct{ grantee, db, table, priv, grantable string }

var tidbDBPrivCols = [][2]string{
	{"Select_priv", "SELECT"}, {"Insert_priv", "INSERT"}, {"Update_priv", "UPDATE"}, {"Delete_priv", "DELETE"},
	{"Create_priv", "CREATE"}, {"Drop_priv", "DROP"}, {"References_priv", "REFERENCES"}, {"Index_priv", "INDEX"},
	{"Alter_priv", "ALTER"}, {"Create_tmp_table_priv", "CREATE TEMPORARY TABLES"}, {"Lock_tables_priv", "LOCK TABLES"},
	{"Create_view_priv", "CREATE VIEW"}, {"Show_view_priv", "SHOW VIEW"}, {"Create_routine_priv", "CREATE ROUTINE"},
	{"Alter_routine_priv", "ALTER ROUTINE"}, {"Execute_priv", "EXECUTE"}, {"Event_priv", "EVENT"}, {"Trigger_priv", "TRIGGER"},
}

func tidbGrantee(user, host string) string { return "'" + user + "'@'" + host + "'" }

func (c *conn) tidbSchemaPrivs(ctx context.Context, db string) ([]privRow, error) {
	cols := make([]string, len(tidbDBPrivCols))
	for i, p := range tidbDBPrivCols {
		cols[i] = p[0]
	}
	rows, err := c.db.QueryContext(ctx, "SELECT User, Host, DB, Grant_priv, "+strings.Join(cols, ", ")+" FROM mysql.db WHERE LOWER(DB) = LOWER(?)", db)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []privRow
	for rows.Next() {
		vals := make([]string, 4+len(cols))
		ptrs := make([]any, len(vals))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		grantable := map[bool]string{true: "YES", false: "NO"}[vals[3] == "Y"]
		for i, p := range tidbDBPrivCols {
			if vals[4+i] == "Y" {
				out = append(out, privRow{grantee: tidbGrantee(vals[0], vals[1]), db: vals[2], priv: p[1], grantable: grantable})
			}
		}
	}
	return out, rows.Err()
}

// tidbTablePrivs lists table grants in a database; table "" means all tables.
func (c *conn) tidbTablePrivs(ctx context.Context, db, table string) ([]privRow, error) {
	rows, err := c.db.QueryContext(ctx, `SELECT User, Host, DB, Table_name, Table_priv FROM mysql.tables_priv
		WHERE LOWER(DB) = LOWER(?) AND (? = '' OR LOWER(Table_name) = LOWER(?))`, db, table, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []privRow
	for rows.Next() {
		var user, host, d, t, privs string
		if err := rows.Scan(&user, &host, &d, &t, &privs); err != nil {
			return nil, err
		}
		set := strings.Split(privs, ",")
		grantable := "NO"
		if slices.ContainsFunc(set, func(p string) bool { return strings.EqualFold(p, "Grant") }) {
			grantable = "YES"
		}
		for _, p := range set {
			if p = strings.ToUpper(strings.TrimSpace(p)); p != "" && p != "GRANT" {
				out = append(out, privRow{grantee: tidbGrantee(user, host), db: d, table: t, priv: p, grantable: grantable})
			}
		}
	}
	return out, rows.Err()
}
