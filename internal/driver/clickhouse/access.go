package clickhouse

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"rowsmith/internal/driver"
	"rowsmith/internal/sqlsplit"
)

// Access management: SQL-driven users and roles. Accounts defined in the
// server's configuration files show as built in: they can't be changed with SQL.

var (
	tablePrivs  = []string{"SELECT", "INSERT", "ALTER UPDATE", "ALTER DELETE", "ALTER TABLE", "OPTIMIZE", "TRUNCATE", "DROP TABLE", "dictGet"}
	dbPrivs     = append(slices.Clone(tablePrivs), "CREATE TABLE", "CREATE VIEW", "CREATE DICTIONARY", "SHOW TABLES", "DROP DATABASE")
	serverPrivs = []string{"SELECT", "INSERT", "ALTER TABLE", "CREATE DATABASE", "CREATE TABLE", "DROP DATABASE", "DROP TABLE", "TRUNCATE",
		"OPTIMIZE", "SHOW DATABASES", "CREATE TEMPORARY TABLE", "KILL QUERY", "SYSTEM", "ACCESS MANAGEMENT", "INTROSPECTION", "SOURCES", "dictGet"}
)

var accessModel = driver.AccessModel{
	Levels: []driver.AccessLevel{
		{Level: driver.LevelServer, Label: "Server-wide", Privileges: serverPrivs, Help: "On every database (*.*), including ones created later."},
		{Level: driver.LevelDatabase, Label: "Database", Privileges: dbPrivs, Help: "On every table in the database (db.*), including tables created later."},
		{Level: driver.LevelTable, Label: "Table", Privileges: tablePrivs},
	},
	Covers: map[string][]string{driver.LevelDatabase: {driver.LevelServer}, driver.LevelTable: {driver.LevelDatabase, driver.LevelServer}},
	Attributes: []driver.AccountAttr{
		{Key: "default_database", Label: "Default database", Type: "text", Help: "Where its sessions start."},
	},
	Presets: []driver.AccessPreset{
		{ID: "read", Label: "Read only", Description: "Read every table in the database, including tables created later.",
			Grants: []driver.PresetGrant{{Level: driver.LevelDatabase, Privileges: []string{"SELECT", "dictGet"}}}},
		{ID: "write", Label: "Read & write", Description: "Read, insert, update and delete rows in every table in the database.",
			Grants: []driver.PresetGrant{{Level: driver.LevelDatabase, Privileges: []string{"SELECT", "dictGet", "INSERT", "ALTER UPDATE", "ALTER DELETE"}}}},
		{ID: "owner", Label: "Full access", Description: "Everything in the database, including creating, changing and dropping tables.",
			Grants: []driver.PresetGrant{{Level: driver.LevelDatabase, Privileges: dbPrivs}}},
	},
	Roles:     true,
	CanCreate: []string{"user", "role"},
	Password:  true,
	Note:      "ALTER TABLE includes ALTER UPDATE and ALTER DELETE. Accounts defined in the server's configuration files can't be changed here.",
}

func (c *conn) AccessModel() driver.AccessModel { return accessModel }

func (c *conn) Principals(ctx context.Context) ([]driver.Principal, error) {
	rows, err := c.db.QueryContext(ctx, `SELECT name, 'user', storage, default_database FROM system.users
		UNION ALL SELECT name, 'role', storage, '' FROM system.roles ORDER BY 1`)
	if err != nil {
		return nil, mapError(err, sqlsplit.Statement{})
	}
	defer rows.Close()
	var out []driver.Principal
	for rows.Next() {
		var name, kind, storage, defDB string
		if err := rows.Scan(&name, &kind, &storage, &defDB); err != nil {
			return nil, err
		}
		p := driver.Principal{Name: name, Kind: kind, Login: kind == "user", MemberOf: []string{},
			System: storage != "local_directory" && storage != "replicated"}
		if kind == "user" {
			p.Attributes = map[string]any{"default_database": defDB}
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	members, err := c.db.QueryContext(ctx, `SELECT coalesce(user_name, role_name), granted_role_name FROM system.role_grants ORDER BY 2`)
	if err != nil {
		return out, nil
	}
	defer members.Close()
	for members.Next() {
		var who, role string
		if members.Scan(&who, &role) == nil {
			if i := slices.IndexFunc(out, func(p driver.Principal) bool { return p.Name == who }); i >= 0 {
				out[i].MemberOf = append(out[i].MemberOf, role)
			}
		}
	}
	return out, members.Err()
}

// privilegeTree maps each privilege to the ones it includes (ALL → ALTER →
// ALTER TABLE → ALTER UPDATE…), to read group grants as what they allow.
func (c *conn) privilegeTree(ctx context.Context) map[string][]string {
	tree := map[string][]string{}
	rows, err := c.db.QueryContext(ctx, `SELECT toString(privilege), toString(parent_group) FROM system.privileges WHERE parent_group IS NOT NULL`)
	if err != nil {
		return tree
	}
	defer rows.Close()
	for rows.Next() {
		var p, parent string
		if rows.Scan(&p, &parent) == nil {
			tree[parent] = append(tree[parent], p)
		}
	}
	return tree
}

// expand lists a privilege and everything it includes.
func expand(tree map[string][]string, p string) []string {
	out := []string{p}
	for _, child := range tree[p] {
		out = append(out, expand(tree, child)...)
	}
	return out
}

type grantRow struct {
	who, access, db, table string
	partial, grantOption   bool
}

// grants reads system.grants for some grantees (all when nil).
func (c *conn) grants(ctx context.Context, where string, args ...any) ([]grantRow, error) {
	rows, err := c.db.QueryContext(ctx, `SELECT coalesce(user_name, role_name), toString(access_type), coalesce(database, ''), coalesce(table, ''),
		is_partial_revoke, grant_option FROM system.grants WHERE column IS NULL AND `+where, args...)
	if err != nil {
		return nil, mapError(err, sqlsplit.Statement{})
	}
	defer rows.Close()
	var out []grantRow
	for rows.Next() {
		var g grantRow
		var partial, opt uint8
		if err := rows.Scan(&g.who, &g.access, &g.db, &g.table, &partial, &opt); err != nil {
			return nil, err
		}
		g.partial, g.grantOption = partial == 1, opt == 1
		out = append(out, g)
	}
	return out, rows.Err()
}

func levelOf(g grantRow) string {
	switch {
	case g.db == "":
		return driver.LevelServer
	case g.table == "":
		return driver.LevelDatabase
	}
	return driver.LevelTable
}

// effective turns grant rows into the privileges they give, listed ones
// only, after partial revokes.
func effective(tree map[string][]string, rows []grantRow) map[[4]string]bool {
	type key = [4]string // who, level-object, privilege, with grant
	out := map[[4]string]bool{}
	for _, pass := range []bool{false, true} {
		for _, g := range rows {
			if g.partial != pass {
				continue
			}
			obj := levelOf(g) + "\x00" + g.db + "\x00" + g.table
			for _, p := range expand(tree, g.access) {
				k := key{g.who, obj, p, ""}
				if pass {
					delete(out, k) // a partial revoke takes it back here
				} else {
					out[k] = out[k] || g.grantOption
				}
			}
		}
	}
	return out
}

func listed(level, priv string) bool {
	switch level {
	case driver.LevelServer:
		return slices.Contains(serverPrivs, priv)
	case driver.LevelDatabase:
		return slices.Contains(dbPrivs, priv)
	}
	return slices.Contains(tablePrivs, priv)
}

// holders lists a principal and the roles it has, directly or through roles.
func (c *conn) holders(ctx context.Context, principal string) []string {
	out := []string{principal}
	for i := 0; i < len(out) && i < 64; i++ {
		rows, err := c.db.QueryContext(ctx, `SELECT granted_role_name FROM system.role_grants WHERE coalesce(user_name, role_name) = ?`, out[i])
		if err != nil {
			break
		}
		for rows.Next() {
			var r string
			if rows.Scan(&r) == nil && !slices.Contains(out, r) {
				out = append(out, r)
			}
		}
		rows.Close()
	}
	return out
}

func (c *conn) Access(ctx context.Context, principal string, scope driver.Scope) ([]driver.Grant, error) {
	holders := c.holders(ctx, principal)
	rows, err := c.grants(ctx, "has(?, coalesce(user_name, role_name)) AND (database IS NULL OR database = ?)", holders, scope.Database)
	if err != nil {
		return nil, err
	}
	var out []driver.Grant
	for k, withGrant := range effective(c.privilegeTree(ctx), rows) {
		parts := strings.SplitN(k[1], "\x00", 3)
		level, db, table := parts[0], parts[1], parts[2]
		if !listed(level, k[2]) {
			continue
		}
		g := driver.Grant{Principal: principal, Level: level, Database: db, Object: table, Privilege: k[2], WithGrant: withGrant}
		if k[0] != principal {
			g.Via = k[0]
		}
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Level+out[i].Object+out[i].Privilege < out[j].Level+out[j].Object+out[j].Privilege
	})
	return out, nil
}

func (c *conn) ObjectAccess(ctx context.Context, ref driver.ObjectRef) ([]driver.Grant, error) {
	rows, err := c.grants(ctx, "(database IS NULL OR (database = ? AND (table IS NULL OR table = ?)))", ref.Database, ref.Name)
	if err != nil {
		return nil, err
	}
	var out []driver.Grant
	for k, withGrant := range effective(c.privilegeTree(ctx), rows) {
		level := strings.SplitN(k[1], "\x00", 2)[0]
		if !slices.Contains(tablePrivs, k[2]) {
			continue
		}
		g := driver.Grant{Principal: k[0], Level: driver.LevelTable, Database: ref.Database, Object: ref.Name, Privilege: k[2], WithGrant: withGrant}
		switch level {
		case driver.LevelDatabase:
			g.Via = "the whole database"
		case driver.LevelServer:
			g.Via = "server-wide"
		}
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Principal+out[i].Privilege < out[j].Principal+out[j].Privilege })
	return out, nil
}

func (c *conn) GrantSQL(changes []driver.GrantChange) ([]string, error) {
	type key struct {
		on, who           string
		revoke, withGrant bool
	}
	groups := map[key][]string{}
	var order []key
	for _, ch := range changes {
		if !listed(ch.Level, ch.Privilege) {
			return nil, fmt.Errorf("%s can't be granted on %s here", ch.Privilege, ch.Level)
		}
		if ch.Principal == "" {
			return nil, errors.New("choose who to grant to")
		}
		var on string
		switch ch.Level {
		case driver.LevelServer:
			on = "*.*"
		case driver.LevelDatabase:
			if ch.Database == "" {
				return nil, errors.New("choose a database")
			}
			on = quote(ch.Database) + ".*"
		case driver.LevelTable:
			if ch.Database == "" || ch.Object == "" {
				return nil, errors.New("choose a table")
			}
			on = qualify(ch.Database, ch.Object)
		default:
			return nil, fmt.Errorf("ClickHouse has no %s level", ch.Level)
		}
		k := key{on, ch.Principal, ch.Revoke, ch.WithGrant && !ch.Revoke}
		if _, ok := groups[k]; !ok {
			order = append(order, k)
		}
		if !slices.Contains(groups[k], ch.Privilege) {
			groups[k] = append(groups[k], ch.Privilege)
		}
	}
	sort.SliceStable(order, func(i, j int) bool { return order[i].revoke && !order[j].revoke })
	var out []string
	for _, k := range order {
		privs := strings.Join(groups[k], ", ")
		if k.revoke {
			out = append(out, "REVOKE "+privs+" ON "+k.on+" FROM "+quote(k.who))
			continue
		}
		s := "GRANT " + privs + " ON " + k.on + " TO " + quote(k.who)
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
	who := quote(name)
	defaultDB := func(v string) string {
		if v = strings.TrimSpace(v); v == "" {
			return " DEFAULT DATABASE NONE"
		}
		return " DEFAULT DATABASE " + quote(v)
	}
	switch op.Op {
	case "create":
		if op.Kind == "role" {
			return []string{"CREATE ROLE " + who}, nil
		}
		if op.Password == "" {
			return nil, errors.New("a ClickHouse user needs a password")
		}
		s := "CREATE USER " + who + " IDENTIFIED BY " + literal(op.Password)
		if v, ok := op.Attrs["default_database"]; ok && strings.TrimSpace(v) != "" {
			s += defaultDB(v)
		}
		return []string{s}, nil
	case "alter":
		v, ok := op.Attrs["default_database"]
		if !ok {
			return nil, errors.New("nothing to change")
		}
		return []string{"ALTER USER " + who + defaultDB(v)}, nil
	case "password":
		if op.Password == "" {
			return nil, errors.New("enter a password")
		}
		return []string{"ALTER USER " + who + " IDENTIFIED BY " + literal(op.Password)}, nil
	case "drop":
		if op.Kind == "role" {
			return []string{"DROP ROLE " + who}, nil
		}
		return []string{"DROP USER " + who}, nil
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
