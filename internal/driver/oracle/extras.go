package oracle

import (
	"context"
	"crypto/rand"
	"database/sql"
	sqldriver "database/sql/driver"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"rowsmith/internal/driver"
	"rowsmith/internal/driver/sqlbase"
	"rowsmith/internal/sqlsplit"
)

// ---- EXPLAIN -------------------------------------------------------------------

// planRow is one row of PLAN_TABLE or V$SQL_PLAN_STATISTICS_ALL.
type planRow struct {
	id                          int
	parent                      sql.NullInt64
	operation, options          string
	owner, object, alias        string
	cost, rows, bytes, time     sql.NullFloat64
	access, filter              string
	actualRows, starts, elapsed sql.NullFloat64 // row source statistics (analyze)
}

// Explain runs EXPLAIN PLAN into PLAN_TABLE under a private statement id and
// removes its rows afterwards. With analyze, the statement is executed with
// row source statistics inside a transaction that is always rolled back.
func (c *conn) Explain(ctx context.Context, s driver.Scope, stmt string, analyze bool) (*driver.Plan, error) {
	stmt = strings.TrimRightFunc(strings.TrimSpace(stmt), isTrailing)
	sc, err := c.db.Conn(ctx)
	if err != nil {
		return nil, cleanErr(err)
	}
	discard := analyze // STATISTICS_LEVEL must not leak into the pool
	defer func() {
		if discard {
			_ = sc.Raw(func(any) error { return sqldriver.ErrBadConn })
		}
		sc.Close()
	}()
	if schema := c.schema(s.Schema); schema != c.user {
		if _, err := sc.ExecContext(ctx, "ALTER SESSION SET CURRENT_SCHEMA = "+quote(schema)); err != nil {
			return nil, cleanErr(err)
		}
		defer func() {
			rctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, err := sc.ExecContext(rctx, "ALTER SESSION SET CURRENT_SCHEMA = "+quote(c.user)); err != nil {
				discard = true
			}
		}()
	}
	if analyze {
		return c.explainAnalyze(ctx, sc, stmt)
	}
	id := statementID()
	if _, err := sc.ExecContext(ctx, "EXPLAIN PLAN SET STATEMENT_ID = "+literal(id)+" FOR "+stmt); err != nil {
		return nil, cleanErr(err)
	}
	defer func() {
		dctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = sc.ExecContext(dctx, "DELETE FROM PLAN_TABLE WHERE STATEMENT_ID = :1", id)
	}()
	rows, err := planRows(ctx, sc, `SELECT ID, PARENT_ID, OPERATION, OPTIONS, OBJECT_OWNER, OBJECT_NAME, OBJECT_ALIAS, COST, CARDINALITY,
		BYTES, TIME, ACCESS_PREDICATES, FILTER_PREDICATES, NULL, NULL, NULL
		FROM PLAN_TABLE WHERE STATEMENT_ID = :1 ORDER BY ID`, id)
	if err != nil {
		return nil, cleanErr(err)
	}
	lines, _ := sqlbase.Strings(ctx, sc, "SELECT PLAN_TABLE_OUTPUT FROM TABLE(DBMS_XPLAN.DISPLAY('PLAN_TABLE', :1, 'TYPICAL'))", id)
	return &driver.Plan{Root: planTree(rows), Raw: strings.Join(lines, "\n"), Format: "text"}, nil
}

var sqlIDRe = regexp.MustCompile(`SQL_ID\s+(\w+), child number (\d+)`)

func (c *conn) explainAnalyze(ctx context.Context, sc *sql.Conn, stmt string) (*driver.Plan, error) {
	switch w := leadingWords(stmt, 1); {
	case len(w) == 0:
		return nil, errors.New("nothing to explain")
	case w[0] != "SELECT" && w[0] != "WITH" && w[0] != "INSERT" && w[0] != "UPDATE" && w[0] != "DELETE" && w[0] != "MERGE":
		// DDL commits implicitly and could not be rolled back.
		return nil, errors.New("EXPLAIN ANALYZE supports SELECT, INSERT, UPDATE, DELETE and MERGE")
	}
	tx, err := sc.BeginTx(ctx, nil)
	if err != nil {
		return nil, cleanErr(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "ALTER SESSION SET STATISTICS_LEVEL = ALL"); err != nil {
		return nil, cleanErr(err)
	}
	start := time.Now()
	if returnsRows(sqlsplit.Statement{SQL: stmt}) {
		rows, err := tx.QueryContext(ctx, stmt)
		if err != nil {
			return nil, cleanErr(err)
		}
		for rows.Next() {
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, cleanErr(err)
		}
	} else if _, err := tx.ExecContext(ctx, stmt); err != nil {
		return nil, cleanErr(err)
	}
	elapsed := float64(time.Since(start).Microseconds()) / 1000
	// With no SQL_ID, DISPLAY_CURSOR shows the session's previous statement.
	lines, err := sqlbase.Strings(ctx, tx, "SELECT PLAN_TABLE_OUTPUT FROM TABLE(DBMS_XPLAN.DISPLAY_CURSOR(NULL, NULL, 'ALLSTATS LAST +COST +BYTES'))")
	if err != nil {
		return nil, privErr(err, "EXPLAIN ANALYZE", "V$SESSION, V$SQL, V$SQL_PLAN and V$SQL_PLAN_STATISTICS_ALL")
	}
	raw := strings.Join(lines, "\n")
	m := sqlIDRe.FindStringSubmatch(raw)
	if m == nil {
		msg := strings.TrimSpace(raw)
		if i := strings.IndexByte(msg, '\n'); i > 0 {
			msg = msg[:i]
		}
		return nil, fmt.Errorf("EXPLAIN ANALYZE needs SELECT access to V$SESSION, V$SQL, V$SQL_PLAN and V$SQL_PLAN_STATISTICS_ALL "+
			"(for example through the SELECT_CATALOG_ROLE role): %s", msg)
	}
	rows, err := planRows(ctx, tx, `SELECT ID, PARENT_ID, OPERATION, OPTIONS, OBJECT_OWNER, OBJECT_NAME, OBJECT_ALIAS, COST, CARDINALITY,
		BYTES, TIME, ACCESS_PREDICATES, FILTER_PREDICATES, LAST_OUTPUT_ROWS, LAST_STARTS, LAST_ELAPSED_TIME
		FROM V$SQL_PLAN_STATISTICS_ALL WHERE SQL_ID = :1 AND CHILD_NUMBER = :2 ORDER BY ID`, m[1], m[2])
	if err != nil {
		return nil, privErr(err, "EXPLAIN ANALYZE", "V$SQL_PLAN_STATISTICS_ALL")
	}
	return &driver.Plan{Root: planTree(rows), Raw: raw, Format: "text", Totals: map[string]any{"Execution Time": elapsed}}, nil
}

func statementID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "rowsmith_" + hex.EncodeToString(b)
}

func planRows(ctx context.Context, c sqlbase.Conn, q string, args ...any) ([]planRow, error) {
	rows, err := c.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []planRow
	for rows.Next() {
		var r planRow
		var op, opt, owner, object, alias, access, filter sql.NullString
		if err := rows.Scan(&r.id, &r.parent, &op, &opt, &owner, &object, &alias, &r.cost, &r.rows, &r.bytes, &r.time,
			&access, &filter, &r.actualRows, &r.starts, &r.elapsed); err != nil {
			return nil, err
		}
		r.operation, r.options, r.owner, r.object, r.alias = op.String, opt.String, owner.String, object.String, alias.String
		r.access, r.filter = access.String, filter.String
		out = append(out, r)
	}
	return out, rows.Err()
}

// planTree links plan rows into a tree by ID and PARENT_ID; children keep
// their ID order, which is the order Oracle prints them in.
func planTree(rows []planRow) *driver.PlanNode {
	nodes := map[int]*driver.PlanNode{}
	var roots []*driver.PlanNode
	for _, r := range rows {
		n := &driver.PlanNode{Operation: strings.TrimSpace(r.operation + " " + r.options), Props: map[string]string{}}
		if r.object != "" {
			n.Object = r.object
			if r.owner != "" {
				n.Object = r.owner + "." + r.object
			}
		}
		n.Cost, n.Rows = nullFloat(r.cost), nullFloat(r.rows)
		n.ActualRows, n.Loops = nullFloat(r.actualRows), nullFloat(r.starts)
		if r.elapsed.Valid {
			ms := r.elapsed.Float64 / 1000 // microseconds
			n.TimeMS = &ms
		}
		for k, v := range map[string]string{"Access predicates": r.access, "Filter predicates": r.filter, "Alias": r.alias} {
			if v != "" {
				n.Props[k] = v
			}
		}
		if r.bytes.Valid {
			n.Props["Bytes"] = strconv.FormatFloat(r.bytes.Float64, 'f', -1, 64)
		}
		if r.time.Valid {
			n.Props["Time (s)"] = strconv.FormatFloat(r.time.Float64, 'f', -1, 64)
		}
		nodes[r.id] = n
		if p, ok := nodes[int(r.parent.Int64)]; r.parent.Valid && ok {
			p.Children = append(p.Children, n)
		} else {
			roots = append(roots, n)
		}
	}
	switch len(roots) {
	case 0:
		return nil
	case 1:
		return roots[0]
	}
	return &driver.PlanNode{Operation: "Plan", Children: roots}
}

func nullFloat(f sql.NullFloat64) *float64 {
	if !f.Valid {
		return nil
	}
	v := f.Float64
	return &v
}

// ---- server administration -----------------------------------------------------

func (c *conn) Processes(ctx context.Context) (*driver.Result, error) {
	res, err := c.collect(ctx, `SELECT s.SID || ',' || s.SERIAL# AS "id", s.USERNAME AS "user", s.STATUS AS "status",
		s.OSUSER AS "os_user", s.MACHINE AS "machine", s.PROGRAM AS "program", s.MODULE AS "module", s.LOGON_TIME AS "logon_time",
		s.LAST_CALL_ET AS "seconds", s.EVENT AS "event", s.WAIT_CLASS AS "wait_class", s.BLOCKING_SESSION AS "blocked_by",
		s.SQL_ID AS "sql_id", q.SQL_TEXT AS "query"
		FROM V$SESSION s LEFT JOIN V$SQL q ON q.SQL_ID = s.SQL_ID AND q.CHILD_NUMBER = s.SQL_CHILD_NUMBER
		WHERE s.TYPE = 'USER' AND s.SID <> SYS_CONTEXT('USERENV', 'SID')
		ORDER BY CASE s.STATUS WHEN 'ACTIVE' THEN 0 ELSE 1 END, s.LAST_CALL_ET DESC`)
	if err != nil {
		return nil, privErr(err, "Listing sessions", "V$SESSION and V$SQL")
	}
	return res, nil
}

var sessionIDRe = regexp.MustCompile(`^(\d{1,10}),(\d{1,10})$`)

// KillProcess ends a session identified as "sid,serial#".
func (c *conn) KillProcess(ctx context.Context, id string) error {
	m := sessionIDRe.FindStringSubmatch(strings.TrimSpace(id))
	if m == nil {
		return fmt.Errorf("invalid session id %q (expected sid,serial#)", id)
	}
	_, err := c.db.ExecContext(ctx, "ALTER SYSTEM KILL SESSION '"+m[1]+","+m[2]+"' IMMEDIATE")
	return cleanErr(err)
}

// statusStats is the V$SYSSTAT subset shown as server status.
var statusStats = []string{
	"logons cumulative", "logons current", "opened cursors current", "user calls", "user commits", "user rollbacks",
	"execute count", "parse count (total)", "parse count (hard)", "session logical reads", "physical reads", "physical writes",
	"redo size", "sorts (memory)", "sorts (disk)", "table scans (long tables)", "DB time", "CPU used by this session",
	"bytes sent via SQL*Net to client", "bytes received via SQL*Net from client",
}

func (c *conn) Variables(ctx context.Context, kind string) (*driver.Result, error) {
	if kind == "status" {
		ph := make([]string, len(statusStats))
		args := make([]any, len(statusStats))
		for i, s := range statusStats {
			ph[i], args[i] = ":"+strconv.Itoa(i+1), s
		}
		res, err := c.collect(ctx, `SELECT NAME AS "name", VALUE AS "value" FROM V$SYSSTAT WHERE NAME IN (`+strings.Join(ph, ", ")+`) ORDER BY NAME`, args...)
		if err != nil {
			return nil, privErr(err, "Server status", "V$SYSSTAT")
		}
		return res, nil
	}
	res, err := c.collect(ctx, `SELECT NAME AS "name", DISPLAY_VALUE AS "value", ISDEFAULT AS "default", ISMODIFIED AS "modified",
		ISSYS_MODIFIABLE AS "system_modifiable", DESCRIPTION AS "description" FROM V$PARAMETER ORDER BY NAME`)
	if err != nil {
		return nil, privErr(err, "Server parameters", "V$PARAMETER")
	}
	return res, nil
}

// Users lists accounts from DBA_USERS when visible, otherwise ALL_USERS.
func (c *conn) Users(ctx context.Context) (*driver.Result, error) {
	res, err := c.collect(ctx, `SELECT USERNAME AS "user", ACCOUNT_STATUS AS "status", DEFAULT_TABLESPACE AS "default_tablespace",
		PROFILE AS "profile", AUTHENTICATION_TYPE AS "authentication", CREATED AS "created", LOCK_DATE AS "locked",
		EXPIRY_DATE AS "expires", ORACLE_MAINTAINED AS "system"
		FROM DBA_USERS ORDER BY ORACLE_MAINTAINED, USERNAME`)
	if err == nil {
		return res, nil
	}
	res, err = c.collect(ctx, `SELECT USERNAME AS "user", USER_ID AS "user_id", CREATED AS "created", COMMON AS "common",
		ORACLE_MAINTAINED AS "system" FROM ALL_USERS ORDER BY ORACLE_MAINTAINED, USERNAME`)
	return res, cleanErr(err)
}

// UserGrants renders system privileges, roles and object privileges as
// GRANT statements. Without DBA views only the session user's own grants and
// object grants visible through ALL_TAB_PRIVS are shown.
func (c *conn) UserGrants(ctx context.Context, user string) ([]string, error) {
	grantee := ident(user)
	var out []string
	add := func(q string, row func(cols []sql.NullString) string, args ...any) error {
		rows, err := c.db.QueryContext(ctx, q, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		cols, _ := rows.Columns()
		for rows.Next() {
			vals := make([]sql.NullString, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				return err
			}
			out = append(out, row(vals))
		}
		return rows.Err()
	}
	sysPriv := func(v []sql.NullString) string {
		g := "GRANT " + v[0].String + " TO " + grantee
		if v[1].String == "YES" {
			g += " WITH ADMIN OPTION"
		}
		return g
	}
	role := func(v []sql.NullString) string {
		g := "GRANT " + ident(v[0].String) + " TO " + grantee
		if v[1].String == "YES" {
			g += " WITH ADMIN OPTION"
		}
		return g
	}
	objPriv := func(v []sql.NullString) string {
		on := ident(v[1].String) + "." + ident(v[2].String)
		if v[4].String == "DIRECTORY" {
			on = "DIRECTORY " + ident(v[2].String)
		}
		g := "GRANT " + v[0].String + " ON " + on + " TO " + grantee
		if v[3].String == "YES" {
			g += " WITH GRANT OPTION"
		}
		return g
	}
	err := add(`SELECT PRIVILEGE, ADMIN_OPTION FROM DBA_SYS_PRIVS WHERE GRANTEE = :1 ORDER BY PRIVILEGE`, sysPriv, user)
	if err == nil {
		err = add(`SELECT GRANTED_ROLE, ADMIN_OPTION FROM DBA_ROLE_PRIVS WHERE GRANTEE = :1 ORDER BY GRANTED_ROLE`, role, user)
	}
	if err == nil {
		err = add(`SELECT PRIVILEGE, OWNER, TABLE_NAME, GRANTABLE, TYPE FROM DBA_TAB_PRIVS WHERE GRANTEE = :1
			ORDER BY OWNER, TABLE_NAME, PRIVILEGE`, objPriv, user)
	}
	if err == nil {
		return out, nil
	}
	out = nil // no DBA views: fall back to what this user can see
	if user == c.user {
		if err := add(`SELECT PRIVILEGE, ADMIN_OPTION FROM USER_SYS_PRIVS ORDER BY PRIVILEGE`, sysPriv); err != nil {
			return nil, cleanErr(err)
		}
		if err := add(`SELECT GRANTED_ROLE, ADMIN_OPTION FROM USER_ROLE_PRIVS ORDER BY GRANTED_ROLE`, role); err != nil {
			return nil, cleanErr(err)
		}
	} else {
		out = append(out, "-- System privileges and roles of other users need SELECT access to DBA_SYS_PRIVS and DBA_ROLE_PRIVS")
	}
	if err := add(`SELECT PRIVILEGE, TABLE_SCHEMA, TABLE_NAME, GRANTABLE, TYPE FROM ALL_TAB_PRIVS WHERE GRANTEE = :1
		ORDER BY TABLE_SCHEMA, TABLE_NAME, PRIVILEGE`, objPriv, user); err != nil {
		return nil, cleanErr(err)
	}
	return out, nil
}

// Classify implements driver.Classifier.
func (c *conn) Classify(stmt string) driver.StatementKind {
	k, _ := sqlsplit.Classify(stmt, sqlsplit.Oracle)
	return k
}
