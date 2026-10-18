package mssql

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	"rowsmith/internal/driver"
)

// Access management. Accounts are server logins (and server roles); a login
// reaches a database through a database user mapped to it, which is created
// with the login's name the first time the login is given access there.

var (
	serverPrivs = []string{"CONNECT SQL", "VIEW SERVER STATE", "VIEW ANY DATABASE", "VIEW ANY DEFINITION", "CREATE ANY DATABASE",
		"ALTER ANY DATABASE", "ALTER ANY LOGIN", "ALTER TRACE", "CONTROL SERVER"}
	dbPrivs = []string{"CONNECT", "SELECT", "INSERT", "UPDATE", "DELETE", "EXECUTE", "REFERENCES", "VIEW DEFINITION", "SHOWPLAN",
		"CREATE TABLE", "CREATE VIEW", "CREATE PROCEDURE", "CREATE FUNCTION", "CREATE SCHEMA", "ALTER", "CONTROL"}
	schemaPrivs = []string{"SELECT", "INSERT", "UPDATE", "DELETE", "EXECUTE", "REFERENCES", "VIEW DEFINITION", "ALTER", "CONTROL"}
	tablePrivs  = []string{"SELECT", "INSERT", "UPDATE", "DELETE", "REFERENCES", "VIEW DEFINITION", "ALTER", "CONTROL"}
)

// What membership of the fixed database roles gives, shown as held through them.
var fixedRoles = map[string][]string{
	"db_datareader": {"SELECT"},
	"db_datawriter": {"INSERT", "UPDATE", "DELETE"},
	"db_owner":      {"CONTROL"},
}

var accessModel = driver.AccessModel{
	Levels: []driver.AccessLevel{
		{Level: driver.LevelServer, Label: "Server-wide", Privileges: serverPrivs, Help: "Granted to the login; applies across the server."},
		{Level: driver.LevelDatabase, Label: "Database", Privileges: dbPrivs,
			Help: "SELECT, INSERT, UPDATE, DELETE and EXECUTE here apply to everything in the database, including objects created later."},
		{Level: driver.LevelSchema, Label: "Schema", Privileges: schemaPrivs, Help: "Applies to every object in the schema, including ones created later."},
		{Level: driver.LevelTable, Label: "Table", Privileges: tablePrivs},
	},
	Covers: map[string][]string{
		driver.LevelSchema: {driver.LevelDatabase},
		driver.LevelTable:  {driver.LevelSchema, driver.LevelDatabase},
	},
	Attributes: []driver.AccountAttr{
		{Key: "locked", Label: "Disabled", Type: "bool", Help: "A disabled login can't sign in."},
		{Key: "check_policy", Label: "Enforce password policy", Type: "bool", Help: "The Windows password policy applies to the password."},
		{Key: "check_expiration", Label: "Enforce password expiration", Type: "bool", Help: "Needs the password policy."},
		{Key: "default_database", Label: "Default database", Type: "text", Help: "Where the login's sessions start."},
	},
	Presets: []driver.AccessPreset{
		{ID: "read", Label: "Read only", Description: "Read everything in the database, including objects created later.", Grants: []driver.PresetGrant{
			{Level: driver.LevelDatabase, Privileges: []string{"CONNECT", "SELECT"}},
		}},
		{ID: "write", Label: "Read & write", Description: "Read and change rows everywhere in the database, and run its procedures.", Grants: []driver.PresetGrant{
			{Level: driver.LevelDatabase, Privileges: []string{"CONNECT", "SELECT", "INSERT", "UPDATE", "DELETE", "EXECUTE"}},
		}},
		{ID: "owner", Label: "Full access", Description: "CONTROL of the database: everything, as db_owner has.", Grants: []driver.PresetGrant{
			{Level: driver.LevelDatabase, Privileges: []string{"CONNECT", "CONTROL"}},
		}},
	},
	DropOptions: []driver.AccountAttr{
		{Key: "users", Label: "Also drop its user in this database", Type: "bool", Default: "true",
			Help: "A login's database users stay behind, orphaned, when only the login is dropped."},
	},
	Roles:     true,
	CanCreate: []string{"user", "role"},
	Password:  true,
	Note:      "A login gets a database user with its name when it's first given access to a database. DENY entries aren't shown; revoking a privilege clears them too.",
}

func (c *conn) AccessModel() driver.AccessModel { return accessModel }

func (c *conn) Principals(ctx context.Context) ([]driver.Principal, error) {
	rows, err := c.db.QueryContext(ctx, `SELECT p.name, p.type, p.is_disabled, CAST(COALESCE(l.is_policy_checked, 0) AS bit),
		CAST(COALESCE(l.is_expiration_checked, 0) AS bit), COALESCE(p.default_database_name, N'')
		FROM sys.server_principals p LEFT JOIN sys.sql_logins l ON l.principal_id = p.principal_id
		WHERE p.type IN ('S', 'U', 'G', 'R', 'E', 'X') AND p.name NOT LIKE N'##%'
			AND p.name NOT LIKE N'NT SERVICE\%' AND p.name NOT LIKE N'NT AUTHORITY\%'
		ORDER BY p.name`)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	var out []driver.Principal
	for rows.Next() {
		var name, typ, defDB string
		var disabled, policy, expiration bool
		if err := rows.Scan(&name, &typ, &disabled, &policy, &expiration, &defDB); err != nil {
			return nil, err
		}
		p := driver.Principal{Name: name, Kind: "user", Login: !disabled, MemberOf: []string{}}
		if typ == "R" {
			p.Kind, p.Login, p.ServerOnly, p.System = "role", false, true, name == "public"
		} else {
			p.Attributes = map[string]any{"locked": disabled, "check_policy": policy, "check_expiration": expiration, "default_database": defDB}
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	members, err := c.db.QueryContext(ctx, `SELECT u.name, r.name FROM sys.server_role_members m
		JOIN sys.server_principals r ON r.principal_id = m.role_principal_id
		JOIN sys.server_principals u ON u.principal_id = m.member_principal_id ORDER BY r.name`)
	if err != nil {
		return nil, mapError(err)
	}
	defer members.Close()
	for members.Next() {
		var u, r string
		if err := members.Scan(&u, &r); err != nil {
			return nil, err
		}
		if i := slices.IndexFunc(out, func(p driver.Principal) bool { return p.Name == u }); i >= 0 {
			out[i].MemberOf = append(out[i].MemberOf, r)
		}
	}
	return out, members.Err()
}

func (c *conn) Access(ctx context.Context, principal string, scope driver.Scope) ([]driver.Grant, error) {
	var out []driver.Grant
	// Server-wide: the login's own permissions, its server roles' and public's.
	rows, err := c.db.QueryContext(ctx, `WITH r AS (
			SELECT principal_id FROM sys.server_principals WHERE name = @p1
			UNION ALL SELECT m.role_principal_id FROM sys.server_role_members m JOIN r ON m.member_principal_id = r.principal_id
		)
		SELECT g.name COLLATE DATABASE_DEFAULT, p.permission_name COLLATE DATABASE_DEFAULT, p.state FROM sys.server_permissions p
		JOIN sys.server_principals g ON g.principal_id = p.grantee_principal_id
		WHERE p.class = 100 AND p.state IN ('G', 'W') AND (p.grantee_principal_id IN (SELECT principal_id FROM r) OR g.name = N'public')
		UNION ALL
		SELECT g.name COLLATE DATABASE_DEFAULT, N'CONTROL SERVER' COLLATE DATABASE_DEFAULT, 'G' FROM sys.server_principals g WHERE g.name = N'sysadmin'
			AND g.principal_id IN (SELECT principal_id FROM r)`, principal)
	if err != nil {
		return nil, mapError(err)
	}
	for rows.Next() {
		var who, priv, state string
		if err := rows.Scan(&who, &priv, &state); err != nil {
			rows.Close()
			return nil, err
		}
		g := driver.Grant{Principal: principal, Level: driver.LevelServer, Privilege: priv, WithGrant: state == "W"}
		if who != principal {
			g.Via = who
		}
		out = append(out, g)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	db := scope.Database
	if db == "" {
		db = c.defaultDB
	}
	var uid int
	var uname string
	err = c.db.QueryRowContext(ctx, `SELECT principal_id, name FROM `+sysView(db, "database_principals")+`
		WHERE sid = SUSER_SID(@p1) AND type IN ('S', 'U', 'G', 'E', 'X')`, principal).Scan(&uid, &uname)
	if err != nil {
		// No user in the database (or a server role): server-wide access only.
		return out, nil
	}
	if uname == "dbo" {
		out = append(out, driver.Grant{Principal: principal, Level: driver.LevelDatabase, Database: db, Privilege: "CONTROL", Via: "owner"})
	}
	// The database roles it belongs to, directly or through other roles, and public.
	holders := map[int]string{uid: "", 0: "public"}
	roles, err := c.db.QueryContext(ctx, `WITH r AS (
			SELECT CAST(@p1 AS int) AS id
			UNION ALL SELECT m.role_principal_id FROM `+sysView(db, "database_role_members")+` m JOIN r ON m.member_principal_id = r.id
		)
		SELECT DISTINCT r.id, dp.name FROM r JOIN `+sysView(db, "database_principals")+` dp ON dp.principal_id = r.id WHERE r.id <> @p1`, uid)
	if err != nil {
		return nil, mapError(err)
	}
	for roles.Next() {
		var id int
		var name string
		if err := roles.Scan(&id, &name); err != nil {
			roles.Close()
			return nil, err
		}
		holders[id] = name
		for _, priv := range fixedRoles[name] {
			out = append(out, driver.Grant{Principal: principal, Level: driver.LevelDatabase, Database: db, Privilege: priv, Via: name})
		}
	}
	roles.Close()
	ids := make([]string, 0, len(holders))
	for id := range holders {
		ids = append(ids, strconv.Itoa(id))
	}
	perms, err := c.db.QueryContext(ctx, `SELECT p.class, COALESCE(o.name, N''), p.permission_name, p.state, p.grantee_principal_id
		FROM `+sysView(db, "database_permissions")+` p
		LEFT JOIN `+sysView(db, "schemas")+` s ON p.class = 3 AND s.schema_id = p.major_id
		LEFT JOIN `+sysView(db, "objects")+` o ON p.class = 1 AND o.object_id = p.major_id
		LEFT JOIN `+sysView(db, "schemas")+` os ON os.schema_id = o.schema_id
		WHERE p.state IN ('G', 'W') AND p.grantee_principal_id IN (`+strings.Join(ids, ", ")+`)
			AND (p.class = 0 OR (p.class = 3 AND s.name = @p1) OR (p.class = 1 AND p.minor_id = 0 AND os.name = @p1))`, scope.Schema)
	if err != nil {
		return nil, mapError(err)
	}
	defer perms.Close()
	for perms.Next() {
		var class, grantee int
		var object, priv, state string
		if err := perms.Scan(&class, &object, &priv, &state, &grantee); err != nil {
			return nil, err
		}
		g := driver.Grant{Principal: principal, Database: db, Privilege: priv, WithGrant: state == "W", Via: holders[grantee]}
		switch class {
		case 0:
			g.Level = driver.LevelDatabase
		case 3:
			g.Level, g.Schema = driver.LevelSchema, scope.Schema
		default:
			g.Level, g.Schema, g.Object = driver.LevelTable, scope.Schema, object
		}
		out = append(out, g)
	}
	return out, perms.Err()
}

func (c *conn) ObjectAccess(ctx context.Context, ref driver.ObjectRef) ([]driver.Grant, error) {
	db := ref.Database
	if db == "" {
		db = c.defaultDB
	}
	schema := ref.Schema
	if schema == "" {
		schema = "dbo"
	}
	// The catalog's collation can differ from the database's: union the text in one.
	rows, err := c.db.QueryContext(ctx, `SELECT g.name COLLATE DATABASE_DEFAULT, p.permission_name COLLATE DATABASE_DEFAULT, p.state, p.class
		FROM `+sysView(db, "database_permissions")+` p
		JOIN `+sysView(db, "database_principals")+` g ON g.principal_id = p.grantee_principal_id
		LEFT JOIN `+sysView(db, "schemas")+` s ON p.class = 3 AND s.schema_id = p.major_id
		LEFT JOIN `+sysView(db, "objects")+` o ON p.class = 1 AND o.object_id = p.major_id
		LEFT JOIN `+sysView(db, "schemas")+` os ON os.schema_id = o.schema_id
		WHERE p.state IN ('G', 'W') AND (p.class = 0 OR (p.class = 3 AND s.name = @p1) OR (p.class = 1 AND p.minor_id = 0 AND os.name = @p1 AND o.name = @p2))
		UNION ALL
		SELECT u.name COLLATE DATABASE_DEFAULT, r.name COLLATE DATABASE_DEFAULT, 'G', -1 FROM `+sysView(db, "database_role_members")+` m
		JOIN `+sysView(db, "database_principals")+` r ON r.principal_id = m.role_principal_id
		JOIN `+sysView(db, "database_principals")+` u ON u.principal_id = m.member_principal_id
		WHERE r.name IN (N'db_datareader', N'db_datawriter', N'db_owner')
		UNION ALL
		SELECT dp.name COLLATE DATABASE_DEFAULT, N'OWNER' COLLATE DATABASE_DEFAULT, 'G', -2 FROM `+sysView(db, "schemas")+` s
		JOIN `+sysView(db, "database_principals")+` dp ON dp.principal_id = s.principal_id WHERE s.name = @p1`, schema, ref.Name)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	var out []driver.Grant
	add := func(who, priv, via string, grantable bool) {
		if slices.Contains(tablePrivs, priv) {
			out = append(out, driver.Grant{Principal: who, Level: driver.LevelTable, Database: db, Schema: schema, Object: ref.Name, Privilege: priv, WithGrant: grantable, Via: via})
		}
	}
	for rows.Next() {
		var who, priv, state string
		var class int
		if err := rows.Scan(&who, &priv, &state, &class); err != nil {
			return nil, err
		}
		switch class {
		case 1:
			add(who, priv, "", state == "W")
		case 3:
			add(who, priv, "the whole schema", false)
		case 0:
			add(who, priv, "the whole database", false)
		case -1: // a fixed role's member
			for _, p := range fixedRoles[priv] {
				if p == "CONTROL" {
					for _, tp := range tablePrivs {
						add(who, tp, priv, false)
					}
					continue
				}
				add(who, p, priv, false)
			}
		case -2: // the schema's owner owns the table
			for _, tp := range tablePrivs {
				add(who, tp, "owner", false)
			}
		}
	}
	return out, rows.Err()
}

func privilegesFor(level string) []string {
	for _, l := range accessModel.Levels {
		if l.Level == level {
			return l.Privileges
		}
	}
	return nil
}

func (c *conn) GrantSQL(changes []driver.GrantChange) ([]string, error) {
	type key struct {
		level, schema, object, who string
		revoke, withGrant          bool
	}
	groups := map[key][]string{}
	var order []key
	db := ""
	for _, ch := range changes {
		allowed := privilegesFor(ch.Level)
		if allowed == nil {
			return nil, fmt.Errorf("SQL Server has no %s level here", ch.Level)
		}
		priv := strings.ToUpper(strings.TrimSpace(ch.Privilege))
		if !slices.Contains(allowed, priv) {
			return nil, fmt.Errorf("%s can't be granted on %s", ch.Privilege, ch.Level)
		}
		if ch.Principal == "" {
			return nil, errors.New("choose who to grant to")
		}
		if (ch.Level == driver.LevelSchema || ch.Level == driver.LevelTable) && ch.Schema == "" {
			return nil, errors.New("choose a schema")
		}
		if ch.Level == driver.LevelTable && ch.Object == "" {
			return nil, errors.New("choose a table")
		}
		if db == "" && ch.Level != driver.LevelServer {
			db = ch.Database
		}
		k := key{ch.Level, ch.Schema, ch.Object, ch.Principal, ch.Revoke, ch.WithGrant && !ch.Revoke}
		if _, ok := groups[k]; !ok {
			order = append(order, k)
		}
		if !slices.Contains(groups[k], priv) {
			groups[k] = append(groups[k], priv)
		}
	}
	rank := map[string]int{}
	for i, l := range accessModel.Levels {
		rank[l.Level] = i
	}
	// Server-wide changes first (they're made from master), then the
	// database's: revokes before grants, broad levels before narrow.
	sort.SliceStable(order, func(i, j int) bool {
		si, sj := order[i].level == driver.LevelServer, order[j].level == driver.LevelServer
		if si != sj {
			return si
		}
		if order[i].revoke != order[j].revoke {
			return order[i].revoke
		}
		return rank[order[i].level] < rank[order[j].level]
	})
	var out []string
	server, users := false, map[string]bool{}
	for _, k := range order {
		if k.level == driver.LevelServer && !server {
			out = append(out, "USE [master]")
			server = true
		}
		if k.level != driver.LevelServer && server {
			if db == "" {
				return nil, errors.New("choose a database")
			}
			out = append(out, "USE "+quote(db))
			server = false
		}
		// A login reaches a database through a user mapped to it.
		if k.level != driver.LevelServer && !k.revoke && !users[k.who] {
			out = append(out, "IF USER_ID("+literal(k.who)+") IS NULL CREATE USER "+quote(k.who)+" FOR LOGIN "+quote(k.who))
			users[k.who] = true
		}
		var on string
		switch k.level {
		case driver.LevelSchema:
			on = " ON SCHEMA::" + quote(k.schema)
		case driver.LevelTable:
			on = " ON OBJECT::" + quote(k.schema) + "." + quote(k.object)
		}
		privs := strings.Join(groups[k], ", ")
		if k.revoke {
			out = append(out, "REVOKE "+privs+on+" FROM "+quote(k.who)+" CASCADE")
			continue
		}
		s := "GRANT " + privs + on + " TO " + quote(k.who)
		if k.withGrant {
			s += " WITH GRANT OPTION"
		}
		out = append(out, s)
	}
	return out, nil
}

func (c *conn) AccountSQL(op driver.AccountOp) ([]string, error) {
	name := strings.TrimSpace(op.Name)
	if name == "" {
		return nil, errors.New("enter a name")
	}
	login := quote(name)
	switch op.Op {
	case "create":
		if op.Kind == "role" {
			return []string{"CREATE SERVER ROLE " + login}, nil
		}
		if op.Password == "" {
			return nil, errors.New("a SQL Server login needs a password")
		}
		s := "CREATE LOGIN " + login + " WITH PASSWORD = " + literal(op.Password)
		if opts := loginOptions(op.Attrs); len(opts) > 0 {
			s += ", " + strings.Join(opts, ", ")
		}
		return []string{s}, nil
	case "alter":
		var out []string
		if v, ok := op.Attrs["locked"]; ok {
			out = append(out, "ALTER LOGIN "+login+map[bool]string{true: " DISABLE", false: " ENABLE"}[v == "true"])
		}
		if opts := loginOptions(op.Attrs); len(opts) > 0 {
			out = append(out, "ALTER LOGIN "+login+" WITH "+strings.Join(opts, ", "))
		}
		if len(out) == 0 {
			return nil, errors.New("nothing to change")
		}
		return out, nil
	case "password":
		if op.Password == "" {
			return nil, errors.New("enter a password")
		}
		return []string{"ALTER LOGIN " + login + " WITH PASSWORD = " + literal(op.Password)}, nil
	case "drop":
		if op.Kind == "role" {
			return []string{"DROP SERVER ROLE " + login}, nil
		}
		var out []string
		if op.Attrs["users"] == "true" {
			out = append(out, "IF USER_ID("+literal(name)+") IS NOT NULL DROP USER "+login)
		}
		return append(out, "DROP LOGIN "+login), nil
	case "add_role", "remove_role":
		if strings.TrimSpace(op.Role) == "" {
			return nil, errors.New("choose a role")
		}
		verb := " ADD MEMBER "
		if op.Op == "remove_role" {
			verb = " DROP MEMBER "
		}
		return []string{"ALTER SERVER ROLE " + quote(op.Role) + verb + login}, nil
	}
	return nil, fmt.Errorf("unknown account change %q", op.Op)
}

// loginOptions renders the WITH options of CREATE and ALTER LOGIN, the
// password policy before expiration, which needs it.
func loginOptions(attrs map[string]string) []string {
	var out []string
	onOff := map[bool]string{true: "ON", false: "OFF"}
	if v, ok := attrs["check_policy"]; ok {
		out = append(out, "CHECK_POLICY = "+onOff[v == "true"])
	}
	if v, ok := attrs["check_expiration"]; ok {
		out = append(out, "CHECK_EXPIRATION = "+onOff[v == "true"])
	}
	if v := strings.TrimSpace(attrs["default_database"]); v != "" {
		out = append(out, "DEFAULT_DATABASE = "+quote(v))
	}
	return out
}
