package oracle

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/sijms/go-ora/v2/network"

	"rowsmith/internal/driver"
)

// Access management. Accounts are users and roles; schemas are users, so a
// schema's owner can do anything in it. Reading other accounts' privileges
// needs the DBA_ views; without them only grants on objects you own show.

var (
	sysPrivs = []string{"CREATE SESSION", "CREATE TABLE", "CREATE VIEW", "CREATE SEQUENCE", "CREATE PROCEDURE", "CREATE TRIGGER",
		"CREATE SYNONYM", "CREATE TYPE", "UNLIMITED TABLESPACE", "SELECT ANY TABLE", "INSERT ANY TABLE", "UPDATE ANY TABLE",
		"DELETE ANY TABLE", "EXECUTE ANY PROCEDURE", "SELECT ANY DICTIONARY", "CREATE USER", "ALTER USER", "DROP USER",
		"GRANT ANY PRIVILEGE", "GRANT ANY ROLE"}
	objPrivs = []string{"SELECT", "INSERT", "UPDATE", "DELETE", "ALTER", "INDEX", "REFERENCES"}
	// Schema privileges (23ai) by the name of what they allow on each table.
	schemaPrivs = []string{"SELECT", "INSERT", "UPDATE", "DELETE", "EXECUTE", "CREATE TABLE", "ALTER", "DROP"}
	schemaPriv  = map[string]string{"SELECT": "SELECT ANY TABLE", "INSERT": "INSERT ANY TABLE", "UPDATE": "UPDATE ANY TABLE",
		"DELETE": "DELETE ANY TABLE", "EXECUTE": "EXECUTE ANY PROCEDURE", "CREATE TABLE": "CREATE ANY TABLE",
		"ALTER": "ALTER ANY TABLE", "DROP": "DROP ANY TABLE"}
)

func (c *conn) schemaPrivileges() bool {
	major, _ := strconv.Atoi(strings.SplitN(c.version, ".", 2)[0])
	return major >= 23
}

func (c *conn) AccessModel() driver.AccessModel {
	m := driver.AccessModel{
		Levels: []driver.AccessLevel{
			{Level: driver.LevelServer, Label: "System privileges", Privileges: sysPrivs, Help: "Apply across the database, in every schema."},
		},
		Attributes: []driver.AccountAttr{{Key: "locked", Label: "Locked", Type: "bool", Help: "A locked account can't sign in."}},
		DropOptions: []driver.AccountAttr{{Key: "cascade", Label: "Also drop everything it owns (CASCADE)", Type: "bool",
			Help: "Without it, a user that owns tables or other objects can't be dropped."}},
		Roles:     true,
		CanCreate: []string{"user", "role"},
		Password:  true,
		Note:      "Names without quotes are upper case in Oracle: app_user becomes APP_USER. Without access to the DBA_ views, only grants on objects this connection's user owns show.",
	}
	if c.schemaPrivileges() {
		m.Levels = append(m.Levels, driver.AccessLevel{Level: driver.LevelSchema, Label: "Schema", Privileges: schemaPrivs,
			Help: "Schema privileges, such as SELECT ANY TABLE ON SCHEMA: every table in the schema, including ones created later."})
		m.Covers = map[string][]string{driver.LevelTable: {driver.LevelSchema}}
		m.Presets = []driver.AccessPreset{
			{ID: "read", Label: "Read only", Description: "Read every table in the schema, including tables created later.",
				Grants: []driver.PresetGrant{{Level: driver.LevelSchema, Privileges: []string{"SELECT"}}}},
			{ID: "write", Label: "Read & write", Description: "Read and change rows in every table in the schema.",
				Grants: []driver.PresetGrant{{Level: driver.LevelSchema, Privileges: []string{"SELECT", "INSERT", "UPDATE", "DELETE"}}}},
			{ID: "owner", Label: "Full access", Description: "Everything in the schema, including creating, altering and dropping tables.",
				Grants: []driver.PresetGrant{{Level: driver.LevelSchema, Privileges: schemaPrivs}}},
		}
	}
	m.Levels = append(m.Levels, driver.AccessLevel{Level: driver.LevelTable, Label: "Table", Privileges: objPrivs})
	return m
}

// noDictionary reports a query on DBA_ views this user can't read.
func noDictionary(err error) bool {
	var oe *network.OracleError
	return errors.As(err, &oe) && (oe.ErrCode == 942 || oe.ErrCode == 1031)
}

// query runs q on the DBA_ views, or fallback when they can't be read.
func (c *conn) query(ctx context.Context, q, fallback string, args ...any) (*sql.Rows, error) {
	rows, err := c.db.QueryContext(ctx, q, args...)
	if err != nil && noDictionary(err) && fallback != "" {
		rows, err = c.db.QueryContext(ctx, fallback, args...)
	}
	return rows, cleanErr(err)
}

func (c *conn) Principals(ctx context.Context) ([]driver.Principal, error) {
	rows, err := c.query(ctx, `SELECT USERNAME, 'user', ACCOUNT_STATUS, ORACLE_MAINTAINED FROM DBA_USERS
		UNION ALL SELECT ROLE, 'role', NULL, ORACLE_MAINTAINED FROM DBA_ROLES ORDER BY 1`,
		`SELECT USERNAME, 'user', NULL, ORACLE_MAINTAINED FROM ALL_USERS ORDER BY 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []driver.Principal
	for rows.Next() {
		var name, kind string
		var status, maintained sql.NullString
		if err := rows.Scan(&name, &kind, &status, &maintained); err != nil {
			return nil, err
		}
		p := driver.Principal{Name: name, Kind: kind, Login: kind == "user", MemberOf: []string{}, System: maintained.String == "Y"}
		if kind == "user" {
			p.Attributes = map[string]any{"locked": strings.Contains(status.String, "LOCKED")}
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	members, err := c.query(ctx, `SELECT GRANTEE, GRANTED_ROLE FROM DBA_ROLE_PRIVS ORDER BY 2`,
		`SELECT USERNAME, GRANTED_ROLE FROM USER_ROLE_PRIVS ORDER BY 2`)
	if err != nil {
		return out, nil // memberships unknown; the accounts still list
	}
	defer members.Close()
	for members.Next() {
		var who, role string
		if err := members.Scan(&who, &role); err != nil {
			return nil, err
		}
		if i := slices.IndexFunc(out, func(p driver.Principal) bool { return p.Name == who }); i >= 0 {
			out[i].MemberOf = append(out[i].MemberOf, role)
		}
	}
	return out, members.Err()
}

// holders lists who a principal's privileges come from: itself, the roles it
// has (directly or through other roles) and PUBLIC.
func (c *conn) holders(ctx context.Context, principal string) []string {
	out := []string{principal, "PUBLIC"}
	rows, err := c.db.QueryContext(ctx, `SELECT DISTINCT GRANTED_ROLE FROM DBA_ROLE_PRIVS
		START WITH GRANTEE = :1 CONNECT BY NOCYCLE PRIOR GRANTED_ROLE = GRANTEE`, principal)
	if err != nil && strings.EqualFold(principal, c.user) {
		rows, err = c.db.QueryContext(ctx, `SELECT ROLE FROM SESSION_ROLES`)
	}
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var r string
		if rows.Scan(&r) == nil {
			out = append(out, r)
		}
	}
	return out
}

func inList(names []string) string {
	q := make([]string, len(names))
	for i, n := range names {
		q[i] = literal(n)
	}
	return strings.Join(q, ", ")
}

func (c *conn) Access(ctx context.Context, principal string, scope driver.Scope) ([]driver.Grant, error) {
	schema := scope.Schema
	if schema == "" {
		schema = c.user
	}
	holders := c.holders(ctx, principal)
	in := inList(holders)
	via := func(grantee string) string {
		if grantee == principal {
			return ""
		}
		return grantee
	}
	var out []driver.Grant
	sys, err := c.query(ctx, `SELECT GRANTEE, PRIVILEGE, ADMIN_OPTION FROM DBA_SYS_PRIVS WHERE GRANTEE IN (`+in+`)`,
		`SELECT USERNAME, PRIVILEGE, ADMIN_OPTION FROM USER_SYS_PRIVS WHERE USERNAME IN (`+in+`)`)
	if err == nil {
		for sys.Next() {
			var who, priv, admin string
			if sys.Scan(&who, &priv, &admin) == nil {
				out = append(out, driver.Grant{Principal: principal, Level: driver.LevelServer, Privilege: priv, WithGrant: admin == "YES", Via: via(who)})
			}
		}
		sys.Close()
	}
	if c.schemaPrivileges() {
		if strings.EqualFold(principal, schema) {
			for _, p := range schemaPrivs {
				out = append(out, driver.Grant{Principal: principal, Level: driver.LevelSchema, Schema: schema, Privilege: p, Via: "owner"})
			}
		}
		if sp, err := c.db.QueryContext(ctx, `SELECT GRANTEE, PRIVILEGE, ADMIN_OPTION FROM DBA_SCHEMA_PRIVS
			WHERE SCHEMA = :1 AND GRANTEE IN (`+in+`)`, schema); err == nil {
			for sp.Next() {
				var who, priv, admin string
				if sp.Scan(&who, &priv, &admin) != nil {
					continue
				}
				for short, long := range schemaPriv {
					if long == priv {
						out = append(out, driver.Grant{Principal: principal, Level: driver.LevelSchema, Schema: schema, Privilege: short, WithGrant: admin == "YES", Via: via(who)})
					}
				}
			}
			sp.Close()
		}
	}
	tabs, err := c.query(ctx, `SELECT GRANTEE, TABLE_NAME, PRIVILEGE, GRANTABLE FROM DBA_TAB_PRIVS WHERE OWNER = :1 AND GRANTEE IN (`+in+`)`,
		`SELECT GRANTEE, TABLE_NAME, PRIVILEGE, GRANTABLE FROM ALL_TAB_PRIVS WHERE TABLE_SCHEMA = :1 AND GRANTEE IN (`+in+`)`, schema)
	if err != nil {
		return nil, err
	}
	defer tabs.Close()
	for tabs.Next() {
		var who, table, priv, grantable string
		if err := tabs.Scan(&who, &table, &priv, &grantable); err != nil {
			return nil, err
		}
		if slices.Contains(objPrivs, priv) {
			out = append(out, driver.Grant{Principal: principal, Level: driver.LevelTable, Schema: schema, Object: table, Privilege: priv, WithGrant: grantable == "YES", Via: via(who)})
		}
	}
	return out, tabs.Err()
}

func (c *conn) ObjectAccess(ctx context.Context, ref driver.ObjectRef) ([]driver.Grant, error) {
	schema := ref.Schema
	if schema == "" {
		schema = c.user
	}
	rows, err := c.query(ctx, `SELECT GRANTEE, PRIVILEGE, GRANTABLE FROM DBA_TAB_PRIVS WHERE OWNER = :1 AND TABLE_NAME = :2`,
		`SELECT GRANTEE, PRIVILEGE, GRANTABLE FROM ALL_TAB_PRIVS WHERE TABLE_SCHEMA = :1 AND TABLE_NAME = :2`, schema, ref.Name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	grant := func(who, priv, via string, grantable bool) driver.Grant {
		return driver.Grant{Principal: who, Level: driver.LevelTable, Schema: schema, Object: ref.Name, Privilege: priv, WithGrant: grantable, Via: via}
	}
	var out []driver.Grant
	for _, p := range objPrivs {
		out = append(out, grant(schema, p, "owner", true))
	}
	for rows.Next() {
		var who, priv, grantable string
		if err := rows.Scan(&who, &priv, &grantable); err != nil {
			return nil, err
		}
		if slices.Contains(objPrivs, priv) {
			out = append(out, grant(who, priv, "", grantable == "YES"))
		}
	}
	if c.schemaPrivileges() {
		if sp, err := c.db.QueryContext(ctx, `SELECT GRANTEE, PRIVILEGE FROM DBA_SCHEMA_PRIVS WHERE SCHEMA = :1`, schema); err == nil {
			defer sp.Close()
			for sp.Next() {
				var who, priv string
				if sp.Scan(&who, &priv) != nil {
					continue
				}
				for short, long := range schemaPriv {
					if long == priv && slices.Contains(objPrivs, short) {
						out = append(out, grant(who, short, "the whole schema", false))
					}
				}
			}
		}
	}
	return out, rows.Err()
}

func (c *conn) GrantSQL(changes []driver.GrantChange) ([]string, error) {
	model := c.AccessModel()
	allowed := func(level string) []string {
		for _, l := range model.Levels {
			if l.Level == level {
				return l.Privileges
			}
		}
		return nil
	}
	type key struct {
		level, schema, object, who string
		revoke, withGrant          bool
	}
	groups := map[key][]string{}
	var order []key
	for _, ch := range changes {
		privs := allowed(ch.Level)
		if privs == nil {
			return nil, fmt.Errorf("this Oracle database has no %s level", ch.Level)
		}
		priv := strings.ToUpper(strings.TrimSpace(ch.Privilege))
		if !slices.Contains(privs, priv) {
			return nil, fmt.Errorf("%s can't be granted on %s", ch.Privilege, ch.Level)
		}
		if ch.Principal == "" {
			return nil, errors.New("choose who to grant to")
		}
		if ch.Level != driver.LevelServer && ch.Schema == "" {
			return nil, errors.New("choose a schema")
		}
		if ch.Level == driver.LevelTable && ch.Object == "" {
			return nil, errors.New("choose a table")
		}
		if ch.Level == driver.LevelSchema {
			priv = schemaPriv[priv]
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
	for i, l := range model.Levels {
		rank[l.Level] = i
	}
	sort.SliceStable(order, func(i, j int) bool {
		if order[i].revoke != order[j].revoke {
			return order[i].revoke
		}
		return rank[order[i].level] < rank[order[j].level]
	})
	var out []string
	for _, k := range order {
		var on string
		switch k.level {
		case driver.LevelSchema:
			on = " ON SCHEMA " + quote(k.schema)
		case driver.LevelTable:
			on = " ON " + qualify(k.schema, k.object)
		}
		privs := strings.Join(groups[k], ", ")
		if k.revoke {
			out = append(out, "REVOKE "+privs+on+" FROM "+grantee(k.who))
			continue
		}
		s := "GRANT " + privs + on + " TO " + grantee(k.who)
		if k.withGrant {
			s += map[bool]string{true: " WITH ADMIN OPTION", false: " WITH GRANT OPTION"}[k.level == driver.LevelServer]
		}
		out = append(out, s)
	}
	return out, nil
}

func grantee(name string) string {
	if strings.EqualFold(name, "PUBLIC") {
		return "PUBLIC"
	}
	return quote(name)
}

var plainName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_$#]*$`)

// accountName is how a new account is named: a plain name is upper-cased, as
// Oracle does for unquoted names, so it signs in without quotes.
func accountName(name string) string {
	if plainName.MatchString(name) {
		return strings.ToUpper(name)
	}
	return name
}

func password(p string) (string, error) {
	if strings.ContainsAny(p, "\"\n\r") {
		return "", errors.New("an Oracle password can't contain double quotes or line breaks")
	}
	return `"` + p + `"`, nil
}

func (c *conn) AccountSQL(op driver.AccountOp) ([]string, error) {
	name := strings.TrimSpace(op.Name)
	if name == "" {
		return nil, errors.New("enter a name")
	}
	who := quote(name)
	switch op.Op {
	case "create":
		who = quote(accountName(name))
		if op.Kind == "role" {
			return []string{"CREATE ROLE " + who}, nil
		}
		auth := " NO AUTHENTICATION"
		if op.Password != "" {
			pw, err := password(op.Password)
			if err != nil {
				return nil, err
			}
			auth = " IDENTIFIED BY " + pw
		}
		// Without CREATE SESSION the account can't sign in at all.
		return []string{"CREATE USER " + who + auth, "GRANT CREATE SESSION TO " + who}, nil
	case "alter":
		v, ok := op.Attrs["locked"]
		if !ok {
			return nil, errors.New("nothing to change")
		}
		return []string{"ALTER USER " + who + map[bool]string{true: " ACCOUNT LOCK", false: " ACCOUNT UNLOCK"}[v == "true"]}, nil
	case "password":
		pw, err := password(op.Password)
		if err != nil || op.Password == "" {
			if err == nil {
				err = errors.New("enter a password")
			}
			return nil, err
		}
		return []string{"ALTER USER " + who + " IDENTIFIED BY " + pw}, nil
	case "drop":
		if op.Kind == "role" {
			return []string{"DROP ROLE " + who}, nil
		}
		s := "DROP USER " + who
		if op.Attrs["cascade"] == "true" {
			s += " CASCADE"
		}
		return []string{s}, nil
	case "add_role", "remove_role":
		if strings.TrimSpace(op.Role) == "" {
			return nil, errors.New("choose a role")
		}
		if op.Op == "add_role" {
			return []string{"GRANT " + quote(op.Role) + " TO " + who}, nil
		}
		return []string{"REVOKE " + quote(op.Role) + " FROM " + who}, nil
	}
	return nil, fmt.Errorf("unknown account change %q", op.Op)
}
