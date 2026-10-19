package mysql

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

var (
	tablePrivs = []string{"SELECT", "INSERT", "UPDATE", "DELETE", "CREATE", "DROP", "ALTER", "INDEX", "REFERENCES", "CREATE VIEW", "SHOW VIEW", "TRIGGER"}
	dbPrivs    = append(slices.Clone(tablePrivs), "EXECUTE", "CREATE ROUTINE", "ALTER ROUTINE", "EVENT", "CREATE TEMPORARY TABLES", "LOCK TABLES")
	serverPriv = append(slices.Clone(dbPrivs), "SHOW DATABASES", "PROCESS", "RELOAD", "FILE", "REPLICATION CLIENT", "REPLICATION SLAVE", "CREATE USER", "SUPER")
)

var accessModel = driver.AccessModel{
	Levels: []driver.AccessLevel{
		{Level: driver.LevelServer, Label: "Server-wide", Privileges: serverPriv, Help: "Applies to every database on the server, including ones created later."},
		{Level: driver.LevelDatabase, Label: "Database", Privileges: dbPrivs, Help: "Applies to every table in the database, including tables created later."},
		{Level: driver.LevelTable, Label: "Table", Privileges: tablePrivs},
	},
	Covers: map[string][]string{driver.LevelDatabase: {driver.LevelServer}, driver.LevelTable: {driver.LevelDatabase, driver.LevelServer}},
	Attributes: []driver.AccountAttr{
		{Key: "locked", Label: "Locked", Type: "bool", Help: "A locked account can't sign in."},
		{Key: "conn_limit", Label: "Connection limit", Type: "int", Help: "0 means no limit."},
		{Key: "require_tls", Label: "Require TLS", Type: "bool", Help: "Refuse connections that aren't encrypted."},
	},
	Presets: []driver.AccessPreset{
		{ID: "read", Label: "Read only", Description: "Read every table in the database, including tables created later.", Grants: []driver.PresetGrant{
			{Level: driver.LevelDatabase, Privileges: []string{"SELECT", "SHOW VIEW"}},
		}},
		{ID: "write", Label: "Read & write", Description: "Read and change rows in every table in the database.", Grants: []driver.PresetGrant{
			{Level: driver.LevelDatabase, Privileges: []string{"SELECT", "INSERT", "UPDATE", "DELETE", "SHOW VIEW", "EXECUTE", "CREATE TEMPORARY TABLES", "LOCK TABLES"}},
		}},
		{ID: "owner", Label: "Full access", Description: "Everything in the database, including creating and dropping tables.", Grants: []driver.PresetGrant{
			{Level: driver.LevelDatabase, Privileges: dbPrivs},
		}},
	},
	Roles:       true,
	HostPart:    true,
	HostLabel:   "Signs in from",
	HostDefault: "%",
	HostHelp:    "A host name or IP address; % means anywhere.",
	CanCreate:   []string{"user", "role"},
	Password:    true,
	Note:        "Accounts are user@host: the same user name from another host is a different account.",
}

func (c *conn) AccessModel() driver.AccessModel {
	m := accessModel
	if c.flavor == "mysql" && c.major < 8 {
		m.Roles = false // roles arrived in MySQL 8.0
		m.CanCreate = []string{"user"}
	}
	if c.flavor == "tidb" {
		m.Attributes = slices.DeleteFunc(slices.Clone(m.Attributes), func(a driver.AccountAttr) bool { return a.Key == "conn_limit" })
	}
	return m
}

// account splits a principal into user and host. MariaDB roles have no host.
func account(principal string) (user, host string, role bool) {
	i := strings.LastIndex(principal, "@")
	if i < 0 {
		return principal, "", true
	}
	return principal[:i], principal[i+1:], false
}

// acct renders a principal for SQL: 'user'@'host', or a MariaDB role's name.
func (c *conn) acct(principal string) string {
	u, h, noHost := account(principal)
	if noHost {
		if c.flavor == "mariadb" {
			return lit(u)
		}
		h = "%"
	}
	return lit(u) + "@" + lit(h)
}

// grantee is how information_schema writes an account: 'user'@'host', or
// for a MariaDB role the name with an empty host.
func (c *conn) grantee(principal string) string {
	u, h, noHost := account(principal)
	if noHost && c.flavor != "mariadb" {
		h = "%"
	}
	return "'" + u + "'@'" + h + "'"
}

// fromGrantee turns information_schema's 'user'@'host' back into user@host,
// or a MariaDB role's bare name.
func fromGrantee(g string) string {
	u, h, ok := strings.Cut(g, "@")
	u, h = strings.Trim(u, "'"), strings.Trim(h, "'")
	if !ok || h == "" {
		return u
	}
	return u + "@" + h
}

func (c *conn) Principals(ctx context.Context) ([]driver.Principal, error) {
	q := `SELECT User, Host, account_locked = 'Y', max_user_connections, ssl_type <> '', account_locked = 'Y' AND authentication_string = '' FROM mysql.user ORDER BY User, Host`
	switch c.flavor {
	case "mariadb":
		q = `SELECT u.User, u.Host, COALESCE(JSON_VALUE(g.Priv, '$.account_locked'), 'false') = 'true', u.max_user_connections, u.ssl_type <> '', u.is_role = 'Y'
			FROM mysql.user u LEFT JOIN mysql.global_priv g ON g.User = u.User AND g.Host = u.Host ORDER BY u.User, u.Host`
	case "tidb":
		// TiDB keeps the TLS requirement in mysql.global_priv, and has no
		// connection limit (it accepts MAX_USER_CONNECTIONS and ignores it).
		q = `SELECT u.User, u.Host, u.Account_locked = 'Y', 0, COALESCE(JSON_EXTRACT(g.Priv, '$.ssl_type'), 0) <> 0,
			u.Account_locked = 'Y' AND u.authentication_string = ''
			FROM mysql.user u LEFT JOIN mysql.global_priv g ON g.User = u.User AND g.Host = u.Host ORDER BY u.User, u.Host`
	}
	rows, err := c.db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []driver.Principal
	byName := map[string]int{}
	for rows.Next() {
		var user, host string
		var locked, tls, role bool
		var limit int
		if err := rows.Scan(&user, &host, &locked, &limit, &tls, &role); err != nil {
			return nil, err
		}
		name := user + "@" + host
		if c.flavor == "mariadb" && role {
			name = user
		}
		system := strings.HasPrefix(user, "mysql.") || user == "mariadb.sys"
		kind := "user"
		if role {
			kind = "role"
		}
		byName[name] = len(out)
		out = append(out, driver.Principal{Name: name, Kind: kind, Login: !role && !locked, MemberOf: []string{}, System: system,
			Attributes: map[string]any{"locked": locked, "conn_limit": limit, "require_tls": tls}})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, m := range c.memberships(ctx) {
		if i, ok := byName[m[0]]; ok {
			out[i].MemberOf = append(out[i].MemberOf, m[1])
		}
	}
	return out, nil
}

// memberships lists [member, role] pairs.
func (c *conn) memberships(ctx context.Context) [][2]string {
	q := `SELECT CONCAT(TO_USER, '@', TO_HOST), CONCAT(FROM_USER, '@', FROM_HOST) FROM mysql.role_edges`
	if c.flavor == "mariadb" {
		q = `SELECT CONCAT(User, '@', Host), Role FROM mysql.roles_mapping WHERE Role <> ''`
	}
	rows, err := c.db.QueryContext(ctx, q)
	if err != nil {
		return nil // no roles on this server
	}
	defer rows.Close()
	var out [][2]string
	for rows.Next() {
		var m, r string
		if rows.Scan(&m, &r) == nil {
			out = append(out, [2]string{m, r})
		}
	}
	return out
}

func (c *conn) Access(ctx context.Context, principal string, scope driver.Scope) ([]driver.Grant, error) {
	holders := []string{principal}
	for _, m := range c.memberships(ctx) {
		if m[0] == principal {
			holders = append(holders, m[1])
		}
	}
	var out []driver.Grant
	for _, h := range holders {
		via := ""
		if h != principal {
			via = h
		}
		add := func(level, object, priv, grantable string) {
			out = append(out, driver.Grant{Principal: principal, Level: level, Database: scope.Database, Object: object,
				Privilege: priv, WithGrant: grantable == "YES", Via: via})
		}
		g := c.grantee(h)
		rows, err := c.db.QueryContext(ctx, `SELECT PRIVILEGE_TYPE, IS_GRANTABLE FROM information_schema.USER_PRIVILEGES WHERE GRANTEE = ? AND PRIVILEGE_TYPE <> 'USAGE'`, g)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var p, gr string
			if rows.Scan(&p, &gr) == nil {
				add(driver.LevelServer, "", p, gr)
			}
		}
		rows.Close()
		if scope.Database == "" {
			continue
		}
		if c.flavor == "tidb" {
			dbp, err := c.tidbSchemaPrivs(ctx, scope.Database)
			if err != nil {
				return nil, err
			}
			tp, err := c.tidbTablePrivs(ctx, scope.Database, "")
			if err != nil {
				return nil, err
			}
			for _, r := range dbp {
				if r.grantee == g {
					add(driver.LevelDatabase, "", r.priv, r.grantable)
				}
			}
			for _, r := range tp {
				if r.grantee == g {
					add(driver.LevelTable, r.table, r.priv, r.grantable)
				}
			}
			continue
		}
		rows, err = c.db.QueryContext(ctx, `SELECT PRIVILEGE_TYPE, IS_GRANTABLE FROM information_schema.SCHEMA_PRIVILEGES WHERE GRANTEE = ? AND TABLE_SCHEMA = ?`, g, scope.Database)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var p, gr string
			if rows.Scan(&p, &gr) == nil {
				add(driver.LevelDatabase, "", p, gr)
			}
		}
		rows.Close()
		rows, err = c.db.QueryContext(ctx, `SELECT TABLE_NAME, PRIVILEGE_TYPE, IS_GRANTABLE FROM information_schema.TABLE_PRIVILEGES WHERE GRANTEE = ? AND TABLE_SCHEMA = ?`, g, scope.Database)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var t, p, gr string
			if rows.Scan(&t, &p, &gr) == nil {
				add(driver.LevelTable, t, p, gr)
			}
		}
		rows.Close()
	}
	return out, nil
}

func (c *conn) ObjectAccess(ctx context.Context, ref driver.ObjectRef) ([]driver.Grant, error) {
	var out []driver.Grant
	collect := func(q, via string, args ...any) error {
		rows, err := c.db.QueryContext(ctx, q, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var who, p, gr string
			if err := rows.Scan(&who, &p, &gr); err != nil {
				return err
			}
			if via != "" && !slices.Contains(tablePrivs, p) {
				continue
			}
			out = append(out, driver.Grant{Principal: fromGrantee(who), Level: driver.LevelTable, Database: ref.Database, Object: ref.Name,
				Privilege: p, WithGrant: gr == "YES", Via: via})
		}
		return rows.Err()
	}
	if c.flavor == "tidb" {
		add := func(rs []privRow, via string) {
			for _, r := range rs {
				if via != "" && !slices.Contains(tablePrivs, r.priv) {
					continue
				}
				out = append(out, driver.Grant{Principal: fromGrantee(r.grantee), Level: driver.LevelTable, Database: ref.Database, Object: ref.Name,
					Privilege: r.priv, WithGrant: r.grantable == "YES", Via: via})
			}
		}
		tp, err := c.tidbTablePrivs(ctx, ref.Database, ref.Name)
		if err != nil {
			return nil, err
		}
		dbp, err := c.tidbSchemaPrivs(ctx, ref.Database)
		if err != nil {
			return nil, err
		}
		add(tp, "")
		add(dbp, "the whole database")
	} else {
		if err := collect(`SELECT GRANTEE, PRIVILEGE_TYPE, IS_GRANTABLE FROM information_schema.TABLE_PRIVILEGES WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?`, "", ref.Database, ref.Name); err != nil {
			return nil, err
		}
		if err := collect(`SELECT GRANTEE, PRIVILEGE_TYPE, IS_GRANTABLE FROM information_schema.SCHEMA_PRIVILEGES WHERE TABLE_SCHEMA = ?`, "the whole database", ref.Database); err != nil {
			return nil, err
		}
	}
	if err := collect(`SELECT GRANTEE, PRIVILEGE_TYPE, IS_GRANTABLE FROM information_schema.USER_PRIVILEGES WHERE PRIVILEGE_TYPE <> 'USAGE'`, "server-wide"); err != nil {
		return nil, err
	}
	return out, nil
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
		level, db, object, who string
		revoke, withGrant      bool
	}
	groups := map[key][]string{}
	var order []key
	for _, ch := range changes {
		allowed := privilegesFor(ch.Level)
		if allowed == nil {
			return nil, fmt.Errorf("%s has no %s level", map[string]string{"mysql": "MySQL", "mariadb": "MariaDB"}[c.flavor], ch.Level)
		}
		priv := strings.ToUpper(strings.TrimSpace(ch.Privilege))
		if !slices.Contains(allowed, priv) {
			return nil, fmt.Errorf("%s can't be granted on %s", ch.Privilege, ch.Level)
		}
		if ch.Principal == "" {
			return nil, errors.New("choose who to grant to")
		}
		if ch.Level != driver.LevelServer && ch.Database == "" {
			return nil, errors.New("choose a database")
		}
		if ch.Level == driver.LevelTable && ch.Object == "" {
			return nil, errors.New("choose a table")
		}
		k := key{ch.Level, ch.Database, ch.Object, ch.Principal, ch.Revoke, ch.WithGrant && !ch.Revoke}
		if _, ok := groups[k]; !ok {
			order = append(order, k)
		}
		if !slices.Contains(groups[k], priv) {
			groups[k] = append(groups[k], priv)
		}
	}
	rank := map[string]int{driver.LevelServer: 0, driver.LevelDatabase: 1, driver.LevelTable: 2}
	sort.SliceStable(order, func(i, j int) bool {
		if order[i].revoke != order[j].revoke {
			return order[i].revoke
		}
		return rank[order[i].level] < rank[order[j].level]
	})
	var out []string
	for _, k := range order {
		on := "*.*"
		switch k.level {
		case driver.LevelDatabase:
			on = quote(k.db) + ".*"
		case driver.LevelTable:
			on = quote(k.db) + "." + quote(k.object)
		}
		privs := strings.Join(groups[k], ", ")
		if k.revoke {
			out = append(out, "REVOKE "+privs+" ON "+on+" FROM "+c.acct(k.who))
			continue
		}
		s := "GRANT " + privs + " ON " + on + " TO " + c.acct(k.who)
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
	u, h, noHost := account(name)
	if strings.TrimSpace(u) == "" {
		return nil, errors.New("enter a user name")
	}
	if op.Op == "create" && op.Kind != "role" && noHost {
		name, h, noHost = u+"@%", "%", false // a new user without a host may connect from anywhere
	}
	_ = h
	who := c.acct(name)
	isRole := op.Kind == "role" || (noHost && c.flavor == "mariadb")
	switch op.Op {
	case "create":
		if op.Kind == "role" {
			if c.flavor == "mariadb" {
				return []string{"CREATE ROLE " + lit(u)}, nil
			}
			return []string{"CREATE ROLE " + who}, nil
		}
		s := "CREATE USER " + who
		if op.Password != "" {
			s += " IDENTIFIED BY " + lit(op.Password)
		}
		opts, err := c.userOptions(op.Attrs)
		if err != nil {
			return nil, err
		}
		if opts != "" {
			s += " " + opts
		}
		return []string{s}, nil
	case "alter":
		opts, err := c.userOptions(op.Attrs)
		if err != nil {
			return nil, err
		}
		if opts == "" {
			return nil, errors.New("nothing to change")
		}
		return []string{"ALTER USER " + who + " " + opts}, nil
	case "password":
		return []string{"ALTER USER " + who + " IDENTIFIED BY " + lit(op.Password)}, nil
	case "drop":
		if isRole {
			return []string{"DROP ROLE " + who}, nil
		}
		return []string{"DROP USER " + who}, nil
	case "add_role", "remove_role":
		if strings.TrimSpace(op.Role) == "" {
			return nil, errors.New("choose a role")
		}
		role := c.acct(op.Role)
		if op.Op == "remove_role" {
			return []string{"REVOKE " + role + " FROM " + who}, nil
		}
		// Granted roles only apply once active: make them default on sign-in.
		if c.flavor == "mariadb" {
			return []string{"GRANT " + role + " TO " + who, "SET DEFAULT ROLE " + role + " FOR " + who}, nil
		}
		return []string{"GRANT " + role + " TO " + who, "SET DEFAULT ROLE ALL TO " + who}, nil
	}
	return nil, fmt.Errorf("unknown account change %q", op.Op)
}

// userOptions renders ALTER/CREATE USER options in the order MySQL expects:
// REQUIRE, then WITH resource limits, then ACCOUNT LOCK.
func (c *conn) userOptions(attrs map[string]string) (string, error) {
	var parts []string
	if v, ok := attrs["require_tls"]; ok {
		parts = append(parts, map[bool]string{true: "REQUIRE SSL", false: "REQUIRE NONE"}[v == "true"])
	}
	if v, ok := attrs["conn_limit"]; ok && c.flavor != "tidb" {
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil || n < 0 {
			return "", errors.New("the connection limit must be a whole number, 0 for no limit")
		}
		parts = append(parts, "WITH MAX_USER_CONNECTIONS "+strconv.Itoa(n))
	}
	if v, ok := attrs["locked"]; ok {
		parts = append(parts, map[bool]string{true: "ACCOUNT LOCK", false: "ACCOUNT UNLOCK"}[v == "true"])
	}
	return strings.Join(parts, " "), nil
}
