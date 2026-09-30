package mssql

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	"rowsmith/internal/driver"
	"rowsmith/internal/driver/sqlbase"
)

// ---- execution plans -----------------------------------------------------------

// showplanColumn names the extra result set STATISTICS XML adds per statement.
const showplanColumn = "Microsoft SQL Server 2005 XML Showplan"

// Explain returns the estimated plan (SHOWPLAN_XML: nothing executes) or,
// with analyze, the actual plan of a run inside a transaction that is always
// rolled back, so writes never persist.
func (c *conn) Explain(ctx context.Context, s driver.Scope, stmt string, analyze bool) (*driver.Plan, error) {
	sc, err := c.db.Conn(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	defer discard(sc) // it carries SHOWPLAN / STATISTICS settings
	if s.Database != "" {
		if _, err := sc.ExecContext(ctx, "USE "+quote(s.Database)); err != nil {
			return nil, mapError(err)
		}
	}
	var docs []string
	if analyze {
		docs, err = actualPlan(ctx, sc, stmt)
	} else {
		docs, err = estimatedPlan(ctx, sc, stmt)
	}
	if err != nil {
		return nil, mapError(err)
	}
	return parsePlan(docs)
}

func estimatedPlan(ctx context.Context, sc *sql.Conn, stmt string) ([]string, error) {
	// SET SHOWPLAN_XML must be alone in its batch.
	if _, err := sc.ExecContext(ctx, "SET SHOWPLAN_XML ON"); err != nil {
		return nil, err
	}
	rows, err := sc.QueryContext(ctx, stmt)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var docs []string
	for {
		for rows.Next() {
			var doc string
			if err := rows.Scan(&doc); err != nil {
				return nil, err
			}
			docs = append(docs, doc)
		}
		if !rows.NextResultSet() {
			break
		}
	}
	return docs, rows.Err()
}

func actualPlan(ctx context.Context, sc *sql.Conn, stmt string) ([]string, error) {
	if _, err := sc.ExecContext(ctx, "SET STATISTICS XML ON"); err != nil {
		return nil, err
	}
	if _, err := sc.ExecContext(ctx, "BEGIN TRANSACTION"); err != nil {
		return nil, err
	}
	defer func() {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_, _ = sc.ExecContext(rctx, "IF @@TRANCOUNT > 0 ROLLBACK")
	}()
	rows, err := sc.QueryContext(ctx, stmt)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var docs []string
	for {
		cols, err := rows.Columns()
		if err != nil {
			return nil, err
		}
		plan := len(cols) == 1 && cols[0] == showplanColumn
		for rows.Next() {
			if !plan {
				continue // the statement's own results
			}
			var doc string
			if err := rows.Scan(&doc); err != nil {
				return nil, err
			}
			docs = append(docs, doc)
		}
		if !rows.NextResultSet() {
			break
		}
	}
	return docs, rows.Err()
}

// ---- server administration -----------------------------------------------------

func (c *conn) Processes(ctx context.Context) (*driver.Result, error) {
	res, err := sqlbase.Collect(ctx, dialect{}, c.db, 0, `SELECT s.session_id, s.login_name AS [user],
		DB_NAME(COALESCE(r.database_id, s.database_id)) AS [database], s.host_name AS client, s.program_name AS application,
		COALESCE(r.status, s.status) AS state, r.command, r.wait_type, r.wait_time AS wait_ms, r.blocking_session_id AS blocked_by,
		s.open_transaction_count AS open_transactions, s.login_time, COALESCE(r.start_time, s.last_request_start_time) AS query_start,
		CAST(r.total_elapsed_time / 1000.0 AS decimal(18, 1)) AS seconds, COALESCE(r.cpu_time, s.cpu_time) AS cpu_ms,
		COALESCE(r.logical_reads, s.logical_reads) AS logical_reads, t.text AS query
		FROM sys.dm_exec_sessions s
		LEFT JOIN sys.dm_exec_requests r ON r.session_id = s.session_id
		OUTER APPLY (SELECT TOP (1) cn.most_recent_sql_handle FROM sys.dm_exec_connections cn WHERE cn.session_id = s.session_id) cn
		OUTER APPLY sys.dm_exec_sql_text(COALESCE(r.sql_handle, cn.most_recent_sql_handle)) t
		WHERE s.is_user_process = 1 AND s.session_id <> @@SPID
		ORDER BY CASE WHEN r.session_id IS NULL THEN 1 ELSE 0 END, r.total_elapsed_time DESC, s.session_id`)
	return res, mapError(err)
}

func (c *conn) KillProcess(ctx context.Context, id string) error {
	n, err := strconv.Atoi(strings.TrimSpace(id))
	if err != nil || n <= 0 {
		return fmt.Errorf("invalid session id %q", id)
	}
	_, err = c.db.ExecContext(ctx, "KILL "+strconv.Itoa(n))
	return mapError(err)
}

// statusCounters is the subset of performance counters shown as server status.
var statusCounters = []string{
	"Batch Requests/sec", "SQL Compilations/sec", "SQL Re-Compilations/sec", "User Connections", "Logins/sec",
	"Transactions/sec", "Active Transactions", "Processes blocked", "Lock Waits/sec", "Lock Timeouts/sec",
	"Number of Deadlocks/sec", "Page life expectancy", "Buffer cache hit ratio", "Page reads/sec", "Page writes/sec",
	"Lazy writes/sec", "Checkpoint pages/sec", "Full Scans/sec", "Index Searches/sec", "Page Splits/sec",
	"Forwarded Records/sec", "Memory Grants Pending", "Target Server Memory (KB)", "Total Server Memory (KB)",
	"Log Flushes/sec", "Errors/sec", "Temp Tables Creation Rate",
}

func (c *conn) Variables(ctx context.Context, kind string) (*driver.Result, error) {
	if kind != "status" {
		res, err := sqlbase.Collect(ctx, dialect{}, c.db, 0, `SELECT name, value_in_use AS value, value AS configured, minimum, maximum,
			is_dynamic AS dynamic, is_advanced AS advanced, description FROM sys.configurations ORDER BY name`)
		return res, mapError(err)
	}
	names := make([]string, len(statusCounters))
	for i, n := range statusCounters {
		names[i] = literal(n)
	}
	// Ratio counters are only meaningful divided by their "base" counter;
	// "/sec" counters are totals since the server started.
	res, err := sqlbase.Collect(ctx, dialect{}, c.db, 0, `SELECT RTRIM(c.object_name) AS [object], RTRIM(c.counter_name) AS counter,
		NULLIF(RTRIM(c.instance_name), '') AS instance,
		CAST(CASE WHEN c.cntr_type = 537003264 THEN 100.0 * c.cntr_value / NULLIF(b.cntr_value, 0) ELSE c.cntr_value END AS float) AS value,
		CASE c.cntr_type WHEN 272696576 THEN 'total since start' WHEN 537003264 THEN 'percent' ELSE 'current' END AS kind
		FROM sys.dm_os_performance_counters c
		LEFT JOIN sys.dm_os_performance_counters b ON c.cntr_type = 537003264 AND b.cntr_type = 1073939712
			AND b.object_name = c.object_name AND b.instance_name = c.instance_name AND RTRIM(b.counter_name) = RTRIM(c.counter_name) + ' base'
		WHERE RTRIM(c.counter_name) IN (`+strings.Join(names, ", ")+`) AND RTRIM(c.instance_name) IN ('', '_Total')
		ORDER BY [object], counter`)
	return res, mapError(err)
}

// Users lists server logins and the principals of the connection's database.
func (c *conn) Users(ctx context.Context) (*driver.Result, error) {
	res, err := sqlbase.Collect(ctx, dialect{}, c.db, 0, `SELECT name, [type], scope, [default], disabled, created, modified FROM (
		SELECT p.name, LOWER(REPLACE(p.type_desc, '_', ' ')) AS [type], N'server' AS scope, p.default_database_name AS [default],
			p.is_disabled AS disabled, p.create_date AS created, p.modify_date AS modified
		FROM sys.server_principals p WHERE p.type IN ('S', 'U', 'G', 'E', 'X', 'C', 'K') AND p.name NOT LIKE '##%'
		UNION ALL
		SELECT p.name, LOWER(REPLACE(p.type_desc, '_', ' ')), @p1, p.default_schema_name, NULL, p.create_date, p.modify_date
		FROM `+sysView(c.defaultDB, "database_principals")+` p
		WHERE p.type IN ('S', 'U', 'G', 'E', 'X', 'C', 'K', 'R', 'A') AND p.is_fixed_role = 0
		AND p.principal_id NOT IN (0, 3, 4) AND p.name <> 'public'
		) u ORDER BY CASE WHEN scope = N'server' THEN 0 ELSE 1 END, name`, c.defaultDB)
	return res, mapError(err)
}

// UserGrants renders the server-level permissions of a login and the
// database-level permissions of a principal with that name in the
// connection's database as GRANT/DENY statements.
func (c *conn) UserGrants(ctx context.Context, user string) ([]string, error) {
	var out []string
	server, err := c.grants(ctx, user, `SELECT p.state_desc, p.permission_name,
		CASE p.class WHEN 100 THEN N''
			WHEN 101 THEN CASE sp.type WHEN 'R' THEN N'SERVER ROLE::' ELSE N'LOGIN::' END + QUOTENAME(sp.name)
			WHEN 105 THEN N'ENDPOINT::' + QUOTENAME(e.name) END,
		N'', p.class_desc, p.major_id
		FROM sys.server_permissions p JOIN sys.server_principals g ON g.principal_id = p.grantee_principal_id
		LEFT JOIN sys.server_principals sp ON p.class = 101 AND sp.principal_id = p.major_id
		LEFT JOIN sys.endpoints e ON p.class = 105 AND e.endpoint_id = p.major_id
		WHERE g.name = @p1 ORDER BY p.class, 3, p.permission_name`)
	if err != nil {
		return nil, mapError(err)
	}
	out = append(out, server...)
	roles, err := sqlbase.Strings(ctx, c.db, `SELECT r.name FROM sys.server_role_members m
		JOIN sys.server_principals r ON r.principal_id = m.role_principal_id
		JOIN sys.server_principals u ON u.principal_id = m.member_principal_id WHERE u.name = @p1 ORDER BY r.name`, user)
	if err != nil {
		return nil, mapError(err)
	}
	for _, r := range roles {
		out = append(out, "ALTER SERVER ROLE "+quote(r)+" ADD MEMBER "+quote(user)+";")
	}

	db := c.defaultDB
	local, err := c.grants(ctx, user, `SELECT p.state_desc, p.permission_name,
		CASE p.class WHEN 0 THEN N''
			WHEN 1 THEN QUOTENAME(os.name) + N'.' + QUOTENAME(o.name)
			WHEN 3 THEN N'SCHEMA::' + QUOTENAME(s.name)
			WHEN 4 THEN CASE dp.type WHEN 'R' THEN N'ROLE::' WHEN 'A' THEN N'APPLICATION ROLE::' ELSE N'USER::' END + QUOTENAME(dp.name)
			WHEN 6 THEN N'TYPE::' + QUOTENAME(ts.name) + N'.' + QUOTENAME(t.name) END,
		COALESCE(col.name, N''), p.class_desc, p.major_id
		FROM `+sysView(db, "database_permissions")+` p
		JOIN `+sysView(db, "database_principals")+` g ON g.principal_id = p.grantee_principal_id
		LEFT JOIN `+sysView(db, "objects")+` o ON p.class = 1 AND o.object_id = p.major_id
		LEFT JOIN `+sysView(db, "schemas")+` os ON os.schema_id = o.schema_id
		LEFT JOIN `+sysView(db, "columns")+` col ON p.class = 1 AND p.minor_id > 0 AND col.object_id = p.major_id AND col.column_id = p.minor_id
		LEFT JOIN `+sysView(db, "schemas")+` s ON p.class = 3 AND s.schema_id = p.major_id
		LEFT JOIN `+sysView(db, "database_principals")+` dp ON p.class = 4 AND dp.principal_id = p.major_id
		LEFT JOIN `+sysView(db, "types")+` t ON p.class = 6 AND t.user_type_id = p.major_id
		LEFT JOIN `+sysView(db, "schemas")+` ts ON ts.schema_id = t.schema_id
		WHERE g.name = @p1 ORDER BY p.class, 3, 4, p.permission_name`)
	if err != nil {
		return nil, mapError(err)
	}
	roles, err = sqlbase.Strings(ctx, c.db, `SELECT r.name FROM `+sysView(db, "database_role_members")+` m
		JOIN `+sysView(db, "database_principals")+` r ON r.principal_id = m.role_principal_id
		JOIN `+sysView(db, "database_principals")+` u ON u.principal_id = m.member_principal_id WHERE u.name = @p1 ORDER BY r.name`, user)
	if err != nil {
		return nil, mapError(err)
	}
	for _, r := range roles {
		local = append(local, "ALTER ROLE "+quote(r)+" ADD MEMBER "+quote(user)+";")
	}
	if len(local) > 0 {
		out = append(out, "USE "+quote(db)+";")
		out = append(out, local...)
	}
	return out, nil
}

// grants runs a permission query returning (state, permission, securable,
// column, class, id) and merges permissions on the same securable.
func (c *conn) grants(ctx context.Context, grantee, q string) ([]string, error) {
	rows, err := c.db.QueryContext(ctx, q, grantee)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type key struct{ state, securable, column string }
	var order []key
	perms := map[key][]string{}
	for rows.Next() {
		var state, perm, column, class string
		var securable sql.NullString
		var id int64
		if err := rows.Scan(&state, &perm, &securable, &column, &class, &id); err != nil {
			return nil, err
		}
		sec := securable.String
		if !securable.Valid {
			sec = "-- " + strings.ToLower(strings.ReplaceAll(class, "_", " ")) + " #" + strconv.FormatInt(id, 10)
		}
		k := key{state, sec, column}
		if _, ok := perms[k]; !ok {
			order = append(order, k)
		}
		perms[k] = append(perms[k], perm)
	}
	out := make([]string, 0, len(order))
	for _, k := range order {
		out = append(out, grantSQL(k.state, strings.Join(perms[k], ", "), k.securable, k.column, grantee))
	}
	return out, rows.Err()
}

// grantSQL renders one permission row. Securables the catalog query does
// not resolve arrive as a "-- class #id" comment and yield a comment line.
func grantSQL(state, perms, securable, column, grantee string) string {
	verb, suffix := state, ""
	if state == "GRANT_WITH_GRANT_OPTION" {
		verb, suffix = "GRANT", " WITH GRANT OPTION"
	}
	s := verb + " " + perms
	if strings.HasPrefix(securable, "-- ") {
		return "-- " + s + " ON " + securable[3:] + " TO " + quote(grantee)
	}
	if securable != "" {
		s += " ON " + securable
		if column != "" {
			s += " (" + quote(column) + ")"
		}
	}
	to := " TO "
	if verb == "REVOKE" {
		to = " FROM "
	}
	return s + to + quote(grantee) + suffix + ";"
}
