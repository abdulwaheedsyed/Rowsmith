package mongodb

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"

	"rowsmith/internal/driver"
)

// Access management. MongoDB gives access with roles: the built-in ones
// (read, readWrite, dbOwner…) show as privileges on a database or on every
// database; roles you define show as memberships. Accounts are named
// user@db, after the database they're defined in.

var (
	anyDBRoles = []string{"readAnyDatabase", "readWriteAnyDatabase", "dbAdminAnyDatabase", "userAdminAnyDatabase",
		"clusterMonitor", "clusterManager", "clusterAdmin", "hostManager", "backup", "restore", "root"}
	dbRoles  = []string{"read", "readWrite", "dbAdmin", "userAdmin", "dbOwner"}
	builtins = append(append([]string{"enableSharding", "directShardOperations", "searchCoordinator", "__system", "__queryableBackup"}, anyDBRoles...), dbRoles...)
)

var accessModel = driver.AccessModel{
	Levels: []driver.AccessLevel{
		{Level: driver.LevelServer, Label: "Every database", Privileges: anyDBRoles, Help: "Built-in roles on admin that apply to every database and the cluster."},
		{Level: driver.LevelDatabase, Label: "Database", Privileges: dbRoles,
			Help: "Built-in roles on this database: read, readWrite (read and change), dbAdmin (indexes, statistics), userAdmin (accounts), dbOwner (all of these)."},
	},
	Presets: []driver.AccessPreset{
		{ID: "read", Label: "Read only", Description: "Read every collection in the database.", Grants: []driver.PresetGrant{{Level: driver.LevelDatabase, Privileges: []string{"read"}}}},
		{ID: "write", Label: "Read & write", Description: "Read and change documents in every collection.", Grants: []driver.PresetGrant{{Level: driver.LevelDatabase, Privileges: []string{"readWrite"}}}},
		{ID: "owner", Label: "Full access", Description: "dbOwner: everything in the database, including indexes and its users.", Grants: []driver.PresetGrant{{Level: driver.LevelDatabase, Privileges: []string{"dbOwner"}}}},
	},
	Roles:       true,
	HostPart:    true,
	HostLabel:   "Defined in database",
	HostDefault: "admin",
	HostHelp:    "Where the account lives and signs in (its authentication database); its roles can be on any database.",
	CanCreate:   []string{"user", "role"},
	Password:    true,
	Note:        "Built-in roles show as privileges; roles you define show as memberships, on the Account tab.",
}

func (c *conn) AccessModel() driver.AccessModel { return accessModel }

// account splits user@db; a name without a database is defined in admin.
func account(name string) (user, db string) {
	if i := strings.LastIndex(name, "@"); i > 0 {
		return name[:i], name[i+1:]
	}
	return name, "admin"
}

type roleRef struct{ role, db string }

func rolesOf(d bson.D, field string) []roleRef {
	var out []roleRef
	rs, _ := get(d, field).(bson.A)
	for _, r := range rs {
		if rd, ok := r.(bson.D); ok {
			out = append(out, roleRef{fmt.Sprint(get(rd, "role")), fmt.Sprint(get(rd, "db"))})
		}
	}
	return out
}

func (c *conn) Principals(ctx context.Context) ([]driver.Principal, error) {
	var users bson.D
	if err := c.client.Database("admin").RunCommand(ctx, bson.D{{Key: "usersInfo", Value: bson.D{{Key: "forAllDBs", Value: true}}}}).Decode(&users); err != nil {
		return nil, mapError(err)
	}
	var out []driver.Principal
	member := func(rs []roleRef) []string {
		m := []string{}
		for _, r := range rs {
			if !slices.Contains(builtins, r.role) {
				m = append(m, r.role+"@"+r.db)
			}
		}
		return m
	}
	list, _ := get(users, "users").(bson.A)
	for _, u := range list {
		if d, ok := u.(bson.D); ok {
			out = append(out, driver.Principal{Name: fmt.Sprintf("%v@%v", get(d, "user"), get(d, "db")), Kind: "user", Login: true, MemberOf: member(rolesOf(d, "roles"))})
		}
	}
	// Roles you define live in a database; list each one's.
	names, err := c.client.ListDatabaseNames(ctx, bson.D{})
	if err != nil {
		return out, nil
	}
	for _, db := range names {
		var roles bson.D
		if c.client.Database(db).RunCommand(ctx, bson.D{{Key: "rolesInfo", Value: 1}}).Decode(&roles) != nil {
			continue
		}
		rl, _ := get(roles, "roles").(bson.A)
		for _, r := range rl {
			if d, ok := r.(bson.D); ok {
				out = append(out, driver.Principal{Name: fmt.Sprintf("%v@%v", get(d, "role"), get(d, "db")), Kind: "role", MemberOf: member(rolesOf(d, "roles"))})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// roleInfo reads a role you defined, or a principal's roles.
func (c *conn) roleInfo(ctx context.Context, name, db string, user bool) (bson.D, error) {
	key, list := "rolesInfo", "roles"
	if user {
		key, list = "usersInfo", "users"
	}
	ident := "role"
	if user {
		ident = "user"
	}
	var out bson.D
	cmd := bson.D{{Key: key, Value: bson.D{{Key: ident, Value: name}, {Key: "db", Value: db}}}}
	if err := c.client.Database(db).RunCommand(ctx, cmd).Decode(&out); err != nil {
		return nil, mapError(err)
	}
	items, _ := get(out, list).(bson.A)
	if len(items) == 0 {
		return nil, fmt.Errorf("there is no %s named %s@%s", ident, name, db)
	}
	d, _ := items[0].(bson.D)
	return d, nil
}

func (c *conn) Access(ctx context.Context, principal string, scope driver.Scope) ([]driver.Grant, error) {
	name, db := account(principal)
	d, err := c.roleInfo(ctx, name, db, true)
	if err != nil {
		if d, err = c.roleInfo(ctx, name, db, false); err != nil {
			return nil, err
		}
	}
	var out []driver.Grant
	add := func(r roleRef, via string) {
		switch {
		case r.db == "admin" && slices.Contains(anyDBRoles, r.role):
			out = append(out, driver.Grant{Principal: principal, Level: driver.LevelServer, Privilege: r.role, Via: via})
		case r.db == scope.Database && slices.Contains(dbRoles, r.role):
			out = append(out, driver.Grant{Principal: principal, Level: driver.LevelDatabase, Database: r.db, Privilege: r.role, Via: via})
		}
	}
	for _, r := range rolesOf(d, "roles") {
		if slices.Contains(builtins, r.role) {
			add(r, "")
			continue
		}
		// A role you defined: what it gives, held through it.
		if rd, err := c.roleInfo(ctx, r.role, r.db, false); err == nil {
			for _, in := range rolesOf(rd, "inheritedRoles") {
				add(in, r.role+"@"+r.db)
			}
			for _, in := range rolesOf(rd, "roles") {
				add(in, r.role+"@"+r.db)
			}
		}
	}
	return out, nil
}

// ObjectAccess isn't offered: collection-level access needs privileges in a
// role you define, which Rowsmith doesn't edit.
func (c *conn) ObjectAccess(context.Context, driver.ObjectRef) ([]driver.Grant, error) {
	return nil, driver.ErrNotSupported
}

func runCmd(db string, cmd string) string {
	return "db.getSiblingDB(" + quoteStr(db) + ").runCommand(" + cmd + ")"
}

func roleList(rs []roleRef) string {
	parts := make([]string, len(rs))
	for i, r := range rs {
		parts[i] = "{ role: " + quoteStr(r.role) + ", db: " + quoteStr(r.db) + " }"
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// grantCmd grants or revokes roles to a user or a role, in the database it's defined in.
func grantCmd(principal, kind string, revoke bool, rs []roleRef) string {
	name, db := account(principal)
	verb := map[bool]string{false: "grantRolesTo", true: "revokeRolesFrom"}[revoke] + map[bool]string{false: "User", true: "Role"}[kind == "role"]
	return runCmd(db, "{ "+verb+": "+quoteStr(name)+", roles: "+roleList(rs)+" }")
}

func (c *conn) GrantSQL(changes []driver.GrantChange) ([]string, error) {
	type key struct {
		who, kind string
		revoke    bool
	}
	groups := map[key][]roleRef{}
	var order []key
	for _, ch := range changes {
		var r roleRef
		switch ch.Level {
		case driver.LevelServer:
			if !slices.Contains(anyDBRoles, ch.Privilege) {
				return nil, fmt.Errorf("%s isn't a role for every database", ch.Privilege)
			}
			r = roleRef{ch.Privilege, "admin"}
		case driver.LevelDatabase:
			if !slices.Contains(dbRoles, ch.Privilege) {
				return nil, fmt.Errorf("%s isn't a database role", ch.Privilege)
			}
			if ch.Database == "" {
				return nil, errors.New("choose a database")
			}
			r = roleRef{ch.Privilege, ch.Database}
		default:
			return nil, fmt.Errorf("MongoDB has no %s level", ch.Level)
		}
		if ch.Principal == "" {
			return nil, errors.New("choose who to grant to")
		}
		k := key{ch.Principal, ch.Kind, ch.Revoke}
		if _, ok := groups[k]; !ok {
			order = append(order, k)
		}
		groups[k] = append(groups[k], r)
	}
	sort.SliceStable(order, func(i, j int) bool { return order[i].revoke && !order[j].revoke })
	out := make([]string, 0, len(order))
	for _, k := range order {
		out = append(out, grantCmd(k.who, k.kind, k.revoke, groups[k]))
	}
	return out, nil
}

func (c *conn) AccountSQL(op driver.AccountOp) ([]string, error) {
	name, db := account(strings.TrimSpace(op.Name))
	if name == "" {
		return nil, errors.New("enter a name")
	}
	switch op.Op {
	case "create":
		if op.Kind == "role" {
			return []string{runCmd(db, "{ createRole: "+quoteStr(name)+", privileges: [], roles: [] }")}, nil
		}
		if op.Password == "" {
			return nil, errors.New("a MongoDB user needs a password")
		}
		return []string{runCmd(db, "{ createUser: "+quoteStr(name)+", pwd: "+quoteStr(op.Password)+", roles: [] }")}, nil
	case "password":
		if op.Password == "" {
			return nil, errors.New("enter a password")
		}
		return []string{runCmd(db, "{ updateUser: "+quoteStr(name)+", pwd: "+quoteStr(op.Password)+" }")}, nil
	case "drop":
		verb := "dropUser"
		if op.Kind == "role" {
			verb = "dropRole"
		}
		return []string{runCmd(db, "{ "+verb+": "+quoteStr(name)+" }")}, nil
	case "add_role", "remove_role":
		if strings.TrimSpace(op.Role) == "" {
			return nil, errors.New("choose a role")
		}
		role, rdb := account(op.Role)
		return []string{grantCmd(op.Name, op.Kind, op.Op == "remove_role", []roleRef{{role, rdb}})}, nil
	case "alter":
		return nil, errors.New("MongoDB accounts have no settings to change here")
	}
	return nil, fmt.Errorf("unknown account change %q", op.Op)
}
