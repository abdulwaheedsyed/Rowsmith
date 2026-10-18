package postgres

import (
	"context"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"rowsmith/internal/driver"
)

var (
	dbPrivs    = []string{"CONNECT", "CREATE", "TEMPORARY"}
	schemaPriv = []string{"USAGE", "CREATE"}
	tablePrivs = []string{"SELECT", "INSERT", "UPDATE", "DELETE", "TRUNCATE", "REFERENCES", "TRIGGER"}
	seqPrivs   = []string{"USAGE", "SELECT", "UPDATE"}
)

var accessModel = driver.AccessModel{
	Levels: []driver.AccessLevel{
		{Level: driver.LevelDatabase, Label: "Database", Privileges: dbPrivs, Help: "CONNECT opens sessions in it; CREATE adds schemas; TEMPORARY creates temporary tables."},
		{Level: driver.LevelSchema, Label: "Schema", Privileges: schemaPriv, Help: "USAGE is needed to reach anything in the schema; CREATE adds tables to it."},
		{Level: driver.LevelTables, Label: "All tables", Privileges: tablePrivs, Help: "Every table and view in the schema now."},
		{Level: driver.LevelFuture, Label: "Tables created later", Privileges: tablePrivs, Help: "Tables you create in the schema from now on (default privileges for your role)."},
		{Level: driver.LevelSequences, Label: "All sequences", Privileges: seqPrivs, Help: "USAGE lets inserts take the next value of serial and identity columns."},
		{Level: driver.LevelFutureSequences, Label: "Sequences created later", Privileges: seqPrivs},
		{Level: driver.LevelTable, Label: "Table", Privileges: tablePrivs},
	},
	Attributes: []driver.AccountAttr{
		{Key: "login", Label: "Can log in", Type: "bool", Help: "Roles that can't log in are groups that other accounts join."},
		{Key: "superuser", Label: "Superuser", Type: "bool", Help: "Bypasses every permission check."},
		{Key: "createdb", Label: "Can create databases", Type: "bool"},
		{Key: "createrole", Label: "Can create roles", Type: "bool"},
		{Key: "replication", Label: "Replication", Type: "bool", Help: "Can connect for streaming replication."},
		{Key: "conn_limit", Label: "Connection limit", Type: "int", Help: "-1 means no limit."},
		{Key: "valid_until", Label: "Password valid until", Type: "date", Help: "Leave empty for no expiry."},
	},
	Presets: []driver.AccessPreset{
		{ID: "read", Label: "Read only", Description: "Read every table, including tables created later.", Grants: []driver.PresetGrant{
			{Level: driver.LevelDatabase, Privileges: []string{"CONNECT"}},
			{Level: driver.LevelSchema, Privileges: []string{"USAGE"}},
			{Level: driver.LevelTables, Privileges: []string{"SELECT"}},
			{Level: driver.LevelFuture, Privileges: []string{"SELECT"}},
			{Level: driver.LevelSequences, Privileges: []string{"SELECT"}},
		}},
		{ID: "write", Label: "Read & write", Description: "Read and change rows in every table, including tables created later.", Grants: []driver.PresetGrant{
			{Level: driver.LevelDatabase, Privileges: []string{"CONNECT", "TEMPORARY"}},
			{Level: driver.LevelSchema, Privileges: []string{"USAGE"}},
			{Level: driver.LevelTables, Privileges: []string{"SELECT", "INSERT", "UPDATE", "DELETE"}},
			{Level: driver.LevelFuture, Privileges: []string{"SELECT", "INSERT", "UPDATE", "DELETE"}},
			{Level: driver.LevelSequences, Privileges: []string{"USAGE", "SELECT"}},
			{Level: driver.LevelFutureSequences, Privileges: []string{"USAGE", "SELECT"}},
		}},
		{ID: "owner", Label: "Full access", Description: "Everything, including creating tables and emptying them.", Grants: []driver.PresetGrant{
			{Level: driver.LevelDatabase, Privileges: dbPrivs},
			{Level: driver.LevelSchema, Privileges: schemaPriv},
			{Level: driver.LevelTables, Privileges: tablePrivs},
			{Level: driver.LevelFuture, Privileges: tablePrivs},
			{Level: driver.LevelSequences, Privileges: seqPrivs},
			{Level: driver.LevelFutureSequences, Privileges: seqPrivs},
		}},
	},
	DropOptions: []driver.AccountAttr{
		{Key: "reassign", Label: "First hand what it owns over to you and revoke its privileges in this database", Type: "bool", Default: "true",
			Help: "REASSIGN OWNED and DROP OWNED. Privileges it has in other databases have to be revoked there."},
	},
	Roles:     true,
	CanCreate: []string{"user", "role"},
	Password:  true,
	Note:      "Access through roles an account belongs to is shown but changed on the role.",
}

func (c *conn) AccessModel() driver.AccessModel { return accessModel }

func (c *conn) Principals(ctx context.Context) ([]driver.Principal, error) {
	p, err := c.pool(ctx, "")
	if err != nil {
		return nil, err
	}
	rows, err := p.db.QueryContext(ctx, `SELECT r.rolname, r.rolcanlogin, r.rolsuper, r.rolcreatedb, r.rolcreaterole, r.rolreplication,
		r.rolconnlimit, COALESCE(to_char(r.rolvaliduntil, 'YYYY-MM-DD'), ''),
		array_to_json(ARRAY(SELECT b.rolname FROM pg_auth_members m JOIN pg_roles b ON m.roleid = b.oid WHERE m.member = r.oid ORDER BY 1))::text
		FROM pg_roles r WHERE r.rolname !~ '^pg_' ORDER BY r.rolname`)
	if err != nil {
		return nil, mapConnError(err)
	}
	defer rows.Close()
	var out []driver.Principal
	for rows.Next() {
		var name, until, member string
		var login, super, createdb, createrole, repl bool
		var limit int
		if err := rows.Scan(&name, &login, &super, &createdb, &createrole, &repl, &limit, &until, &member); err != nil {
			return nil, err
		}
		pr := driver.Principal{Name: name, Kind: map[bool]string{true: "user", false: "role"}[login], Login: login, MemberOf: []string{},
			Attributes: map[string]any{"login": login, "superuser": super, "createdb": createdb, "createrole": createrole,
				"replication": repl, "conn_limit": limit, "valid_until": until}}
		_ = json.Unmarshal([]byte(member), &pr.MemberOf)
		out = append(out, pr)
	}
	return out, rows.Err()
}

// held lists the roles whose privileges a principal holds: itself, the
// roles it belongs to (recursively) and PUBLIC (oid 0).
const held = `WITH RECURSIVE me(oid) AS (
		SELECT oid FROM pg_roles WHERE rolname = $1
		UNION SELECT m.roleid FROM pg_auth_members m JOIN me ON m.member = me.oid
	), holders AS (SELECT oid FROM me UNION SELECT 0::oid)`

func (c *conn) Access(ctx context.Context, principal string, scope driver.Scope) ([]driver.Grant, error) {
	p, err := c.pool(ctx, scope.Database)
	if err != nil {
		return nil, err
	}
	rows, err := p.db.QueryContext(ctx, held+`
		SELECT 'database', '', a.privilege_type, a.is_grantable, a.grantee, ''
		FROM pg_database d, aclexplode(COALESCE(d.datacl, acldefault('d', d.datdba))) a
		WHERE d.datname = current_database() AND a.grantee IN (SELECT oid FROM holders)
		UNION ALL
		SELECT 'schema', '', a.privilege_type, a.is_grantable, a.grantee, ''
		FROM pg_namespace n, aclexplode(COALESCE(n.nspacl, acldefault('n', n.nspowner))) a
		WHERE n.nspname = $2 AND a.grantee IN (SELECT oid FROM holders)
		UNION ALL
		SELECT CASE WHEN c.relkind = 'S' THEN 'sequence' ELSE 'table' END, c.relname, a.privilege_type, a.is_grantable, a.grantee, ''
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace,
			aclexplode(COALESCE(c.relacl, acldefault((CASE WHEN c.relkind = 'S' THEN 's' ELSE 'r' END)::"char", c.relowner))) a
		WHERE n.nspname = $2 AND c.relkind IN ('r', 'v', 'm', 'p', 'f', 'S') AND a.grantee IN (SELECT oid FROM holders)
		UNION ALL
		SELECT CASE d.defaclobjtype WHEN 'r' THEN 'future' ELSE 'future_sequences' END, '', a.privilege_type, a.is_grantable, a.grantee,
			CASE WHEN d.defaclrole = (SELECT oid FROM pg_roles WHERE rolname = current_user) THEN '' ELSE pg_get_userbyid(d.defaclrole) END
		FROM pg_default_acl d JOIN pg_namespace n ON n.oid = d.defaclnamespace, aclexplode(d.defaclacl) a
		WHERE n.nspname = $2 AND d.defaclobjtype IN ('r', 'S') AND a.grantee IN (SELECT oid FROM holders)`, principal, scope.Schema)
	if err != nil {
		return nil, mapConnError(err)
	}
	defer rows.Close()
	self, err := roleOID(ctx, p.db, principal)
	if err != nil {
		return nil, err
	}
	names := map[uint32]string{0: "PUBLIC"}
	var out []driver.Grant
	// direct[privilege] counts tables (or sequences) granted directly, for the "all" levels.
	direct := map[string]map[string]int{"table": {}, "sequence": {}}
	for rows.Next() {
		var level, object, priv, creator string
		var grantable bool
		var grantee uint32
		if err := rows.Scan(&level, &object, &priv, &grantable, &grantee, &creator); err != nil {
			return nil, err
		}
		g := driver.Grant{Principal: principal, Level: level, Database: scope.Database, Privilege: priv, WithGrant: grantable}
		if level != driver.LevelDatabase {
			g.Schema = scope.Schema
		}
		if grantee != self {
			if _, ok := names[grantee]; !ok {
				names[grantee] = roleName(ctx, p.db, grantee)
			}
			g.Via = names[grantee]
		}
		if creator != "" {
			g.Via = "tables created by " + creator
		}
		switch level {
		case "table", "sequence":
			if g.Via == "" {
				direct[level][priv]++
			}
			if level == "sequence" {
				continue // sequences are managed together
			}
			g.Level, g.Object = driver.LevelTable, object
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// A privilege on every table (or sequence) shows on the "all" level.
	if scope.Schema != "" {
		var tables, seqs int
		_ = p.db.QueryRowContext(ctx, `SELECT count(*) FILTER (WHERE c.relkind <> 'S'), count(*) FILTER (WHERE c.relkind = 'S')
			FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname = $1 AND c.relkind IN ('r', 'v', 'm', 'p', 'f', 'S')`, scope.Schema).Scan(&tables, &seqs)
		for kind, n := range map[string]int{"table": tables, "sequence": seqs} {
			level := map[string]string{"table": driver.LevelTables, "sequence": driver.LevelSequences}[kind]
			for priv, have := range direct[kind] {
				if n > 0 && have == n {
					out = append(out, driver.Grant{Principal: principal, Level: level, Database: scope.Database, Schema: scope.Schema, Privilege: priv})
				}
			}
		}
	}
	return out, nil
}

func roleOID(ctx context.Context, db *sql.DB, name string) (uint32, error) {
	var oid uint32
	err := db.QueryRowContext(ctx, `SELECT oid FROM pg_roles WHERE rolname = $1`, name).Scan(&oid)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("there is no role named %s", name)
	}
	return oid, err
}

func roleName(ctx context.Context, db *sql.DB, oid uint32) string {
	var name string
	if db.QueryRowContext(ctx, `SELECT rolname FROM pg_roles WHERE oid = $1`, oid).Scan(&name) != nil {
		return strconv.Itoa(int(oid))
	}
	return name
}

func (c *conn) ObjectAccess(ctx context.Context, ref driver.ObjectRef) ([]driver.Grant, error) {
	p, err := c.pool(ctx, ref.Database)
	if err != nil {
		return nil, err
	}
	rows, err := p.db.QueryContext(ctx, `SELECT CASE WHEN a.grantee = 0 THEN 'PUBLIC' ELSE pg_get_userbyid(a.grantee) END,
		a.privilege_type, a.is_grantable, a.grantee = c.relowner
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace, aclexplode(COALESCE(c.relacl, acldefault('r', c.relowner))) a
		WHERE n.nspname = $1 AND c.relname = $2
		ORDER BY 1, 2`, ref.Schema, ref.Name)
	if err != nil {
		return nil, mapConnError(err)
	}
	defer rows.Close()
	var out []driver.Grant
	for rows.Next() {
		var who, priv string
		var grantable, owner bool
		if err := rows.Scan(&who, &priv, &grantable, &owner); err != nil {
			return nil, err
		}
		g := driver.Grant{Principal: who, Level: driver.LevelTable, Database: ref.Database, Schema: ref.Schema, Object: ref.Name, Privilege: priv, WithGrant: grantable}
		if owner {
			g.Via = "owner"
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func grantee(p string) string {
	if strings.EqualFold(p, "PUBLIC") {
		return "PUBLIC"
	}
	return quote(p)
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
	for _, ch := range changes {
		allowed := privilegesFor(ch.Level)
		if allowed == nil {
			return nil, fmt.Errorf("PostgreSQL has no %s level", ch.Level)
		}
		priv := strings.ToUpper(strings.TrimSpace(ch.Privilege))
		if !slices.Contains(allowed, priv) {
			return nil, fmt.Errorf("%s can't be granted on %s", ch.Privilege, ch.Level)
		}
		if ch.Principal == "" {
			return nil, errors.New("choose who to grant to")
		}
		if ch.Level != driver.LevelDatabase && ch.Schema == "" {
			return nil, errors.New("choose a schema")
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
	sort.SliceStable(order, func(i, j int) bool {
		if order[i].revoke != order[j].revoke {
			return order[i].revoke // revokes first
		}
		return rank[order[i].level] < rank[order[j].level]
	})
	var out []string
	for _, k := range order {
		privs := strings.Join(groups[k], ", ")
		verb, prep := "GRANT", " TO "
		if k.revoke {
			verb, prep = "REVOKE", " FROM "
		}
		var on string
		switch k.level {
		case driver.LevelDatabase:
			on = "DATABASE " + quote(c.dbFor(changes))
		case driver.LevelSchema:
			on = "SCHEMA " + quote(k.schema)
		case driver.LevelTables:
			on = "ALL TABLES IN SCHEMA " + quote(k.schema)
		case driver.LevelSequences:
			on = "ALL SEQUENCES IN SCHEMA " + quote(k.schema)
		case driver.LevelTable:
			if k.object == "" {
				return nil, errors.New("choose a table")
			}
			on = "TABLE " + quote(k.schema) + "." + quote(k.object)
		}
		tail := ""
		if k.withGrant {
			tail = " WITH GRANT OPTION"
		}
		switch k.level {
		case driver.LevelFuture, driver.LevelFutureSequences:
			what := map[string]string{driver.LevelFuture: "TABLES", driver.LevelFutureSequences: "SEQUENCES"}[k.level]
			out = append(out, "ALTER DEFAULT PRIVILEGES IN SCHEMA "+quote(k.schema)+" "+verb+" "+privs+" ON "+what+prep+grantee(k.who)+tail)
		default:
			out = append(out, verb+" "+privs+" ON "+on+prep+grantee(k.who)+tail)
		}
	}
	return out, nil
}

// dbFor is the database database-level changes name: the one they were
// read from, or the connection's default.
func (c *conn) dbFor(changes []driver.GrantChange) string {
	for _, ch := range changes {
		if ch.Database != "" {
			return ch.Database
		}
	}
	return c.defaultDB
}

func (c *conn) AccountSQL(op driver.AccountOp) ([]string, error) {
	name := strings.TrimSpace(op.Name)
	if name == "" {
		return nil, errors.New("enter a name")
	}
	role := quote(name)
	switch op.Op {
	case "create":
		login := op.Kind != "role"
		attrs := map[string]string{}
		for k, v := range op.Attrs {
			attrs[k] = v
		}
		if _, ok := attrs["login"]; !ok {
			attrs["login"] = strconv.FormatBool(login)
		}
		opts, err := roleOptions(attrs)
		if err != nil {
			return nil, err
		}
		s := "CREATE ROLE " + role + " " + opts
		if op.Password != "" {
			s += " PASSWORD " + passwordLiteral(op.Password)
		}
		return []string{s}, nil
	case "alter":
		opts, err := roleOptions(op.Attrs)
		if err != nil {
			return nil, err
		}
		if opts == "" {
			return nil, errors.New("nothing to change")
		}
		return []string{"ALTER ROLE " + role + " " + opts}, nil
	case "password":
		if op.Password == "" {
			return []string{"ALTER ROLE " + role + " PASSWORD NULL"}, nil
		}
		return []string{"ALTER ROLE " + role + " PASSWORD " + passwordLiteral(op.Password)}, nil
	case "drop":
		if op.Attrs["reassign"] == "true" {
			// Objects it owns stay, owned by whoever runs this; its privileges go.
			return []string{"REASSIGN OWNED BY " + role + " TO CURRENT_USER", "DROP OWNED BY " + role, "DROP ROLE " + role}, nil
		}
		return []string{"DROP ROLE " + role}, nil
	case "add_role", "remove_role":
		if strings.TrimSpace(op.Role) == "" {
			return nil, errors.New("choose a role")
		}
		if op.Op == "add_role" {
			return []string{"GRANT " + quote(op.Role) + " TO " + role}, nil
		}
		return []string{"REVOKE " + quote(op.Role) + " FROM " + role}, nil
	}
	return nil, fmt.Errorf("unknown account change %q", op.Op)
}

func roleOptions(attrs map[string]string) (string, error) {
	var parts []string
	for _, f := range []struct{ key, on, off string }{
		{"login", "LOGIN", "NOLOGIN"}, {"superuser", "SUPERUSER", "NOSUPERUSER"}, {"createdb", "CREATEDB", "NOCREATEDB"},
		{"createrole", "CREATEROLE", "NOCREATEROLE"}, {"replication", "REPLICATION", "NOREPLICATION"},
	} {
		if v, ok := attrs[f.key]; ok {
			parts = append(parts, map[bool]string{true: f.on, false: f.off}[v == "true"])
		}
	}
	if v, ok := attrs["conn_limit"]; ok {
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil || n < -1 {
			return "", errors.New("the connection limit must be a whole number, or -1 for no limit")
		}
		parts = append(parts, "CONNECTION LIMIT "+strconv.Itoa(n))
	}
	if v, ok := attrs["valid_until"]; ok {
		v = strings.TrimSpace(v)
		if v == "" {
			parts = append(parts, "VALID UNTIL 'infinity'")
		} else if _, err := time.Parse("2006-01-02", v); err != nil {
			return "", errors.New("enter the expiry date as YYYY-MM-DD")
		} else {
			parts = append(parts, "VALID UNTIL "+literal(v))
		}
	}
	return strings.Join(parts, " "), nil
}

// passwordLiteral hashes a password as SCRAM-SHA-256, so the server, and any
// statement log on it, never sees it in clear. The redaction placeholder used
// for previews passes through as is.
func passwordLiteral(pw string) string {
	if pw == driver.Redacted {
		return literal(pw)
	}
	return literal(scramSHA256(pw))
}

// scramSHA256 builds the stored form PostgreSQL keeps for SCRAM-SHA-256
// passwords: SCRAM-SHA-256$<iterations>:<salt>$<StoredKey>:<ServerKey>.
func scramSHA256(pw string) string {
	const iterations = 4096
	salt := make([]byte, 16)
	_, _ = rand.Read(salt)
	salted, _ := pbkdf2.Key(sha256.New, pw, salt, iterations, sha256.Size)
	mac := func(key []byte, msg string) []byte {
		h := hmac.New(sha256.New, key)
		h.Write([]byte(msg))
		return h.Sum(nil)
	}
	clientKey := mac(salted, "Client Key")
	stored := sha256.Sum256(clientKey)
	server := mac(salted, "Server Key")
	b64 := base64.StdEncoding.EncodeToString
	return fmt.Sprintf("SCRAM-SHA-256$%d:%s$%s:%s", iterations, b64(salt), b64(stored[:]), b64(server))
}
