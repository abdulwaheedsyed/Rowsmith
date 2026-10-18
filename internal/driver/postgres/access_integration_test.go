package postgres

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"rowsmith/internal/driver"
)

// Integration tests run against a real server when ROWSMITH_TEST_PG_HOST is
// set (the dev stack's postgis). They create and drop their own roles and
// schema.
//
//	ROWSMITH_TEST_PG_HOST, _PORT (5432), _USER (postgres), _PASSWORD, _DATABASE (postgres)

func pgParams(t *testing.T, user, password string) driver.OpenParams {
	t.Helper()
	host := os.Getenv("ROWSMITH_TEST_PG_HOST")
	if host == "" {
		t.Skip("ROWSMITH_TEST_PG_HOST not set")
	}
	port := os.Getenv("ROWSMITH_TEST_PG_PORT")
	if port == "" {
		port = "5432"
	}
	db := os.Getenv("ROWSMITH_TEST_PG_DATABASE")
	if db == "" {
		db = "postgres"
	}
	return driver.OpenParams{Params: map[string]any{"host": host, "port": port, "user": user, "database": db, "tls": "disable"},
		Secrets: map[string]string{"password": password}, AppName: "rowsmith-test"}
}

func TestIntegrationAccess(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	admin := os.Getenv("ROWSMITH_TEST_PG_USER")
	if admin == "" {
		admin = "postgres"
	}
	dc, err := pgDriver{}.Open(ctx, pgParams(t, admin, os.Getenv("ROWSMITH_TEST_PG_PASSWORD")))
	if err != nil {
		t.Fatal(err)
	}
	c := dc.(*conn)
	defer c.Close()
	p, err := c.pool(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	run := func(stmts []string, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range stmts {
			if _, err := p.db.ExecContext(ctx, s); err != nil {
				t.Fatalf("%s: %v", s, err)
			}
		}
	}
	cleanup := func() {
		for _, s := range []string{`DROP SCHEMA IF EXISTS rs_acc CASCADE`, `DROP OWNED BY rs_acc_user`, `DROP ROLE IF EXISTS rs_acc_user`, `DROP ROLE IF EXISTS rs_acc_readers`} {
			_, _ = p.db.ExecContext(ctx, s)
		}
	}
	cleanup()
	defer cleanup()
	run([]string{`CREATE SCHEMA rs_acc`, `CREATE TABLE rs_acc.a (id serial PRIMARY KEY, v text)`, `CREATE TABLE rs_acc.b (id int)`}, nil)

	// An account made through AccountSQL can sign in with its password:
	// the SCRAM-SHA-256 verifier is right.
	run(c.AccountSQL(driver.AccountOp{Op: "create", Kind: "user", Name: "rs_acc_user", Password: "Wild-Otter-Lantern-42", Attrs: map[string]string{"conn_limit": "3"}}))
	run(c.AccountSQL(driver.AccountOp{Op: "create", Kind: "role", Name: "rs_acc_readers"}))
	uc, err := pgDriver{}.Open(ctx, pgParams(t, "rs_acc_user", "Wild-Otter-Lantern-42"))
	if err != nil {
		t.Fatalf("signing in with the password set through AccountSQL: %v", err)
	}
	uc.Close()
	if _, err := (pgDriver{}).Open(ctx, pgParams(t, "rs_acc_user", "wrong")); err == nil {
		t.Fatal("a wrong password signed in")
	}

	// Principals reports attributes and memberships.
	run(c.AccountSQL(driver.AccountOp{Op: "add_role", Name: "rs_acc_user", Role: "rs_acc_readers"}))
	ps, err := c.Principals(ctx)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(ps, func(x driver.Principal) bool { return x.Name == "rs_acc_user" })
	if i < 0 || !ps[i].Login || ps[i].Attributes["conn_limit"] != 3 || !slices.Contains(ps[i].MemberOf, "rs_acc_readers") {
		t.Fatalf("principal: %+v", ps)
	}

	scope := driver.Scope{Schema: "rs_acc"}
	has := func(gs []driver.Grant, level, object, priv, via string) bool {
		return slices.ContainsFunc(gs, func(g driver.Grant) bool {
			return g.Level == level && g.Object == object && g.Privilege == priv && g.Via == via
		})
	}
	access := func(who string) []driver.Grant {
		t.Helper()
		gs, err := c.Access(ctx, who, scope)
		if err != nil {
			t.Fatal(err)
		}
		return gs
	}

	// Grants to a role show on its members as held through it.
	run(c.GrantSQL([]driver.GrantChange{{Grant: driver.Grant{Principal: "rs_acc_readers", Level: driver.LevelTable, Schema: "rs_acc", Object: "b", Privilege: "SELECT"}}}))
	gs := access("rs_acc_user")
	if !has(gs, driver.LevelTable, "b", "SELECT", "rs_acc_readers") || !has(gs, driver.LevelDatabase, "", "CONNECT", "PUBLIC") {
		t.Fatalf("inherited and PUBLIC access missing: %+v", gs)
	}

	// The write preset, applied level by level.
	var changes []driver.GrantChange
	for _, pr := range accessModel.Presets {
		if pr.ID == "write" {
			for _, pg := range pr.Grants {
				for _, priv := range pg.Privileges {
					changes = append(changes, driver.GrantChange{Grant: driver.Grant{Principal: "rs_acc_user", Level: pg.Level, Schema: "rs_acc", Privilege: priv}})
				}
			}
		}
	}
	stmts, err := c.GrantSQL(changes)
	if err != nil {
		t.Fatal(err)
	}
	const future = `ALTER DEFAULT PRIVILEGES IN SCHEMA "rs_acc" GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO "rs_acc_user"`
	if !slices.ContainsFunc(stmts, func(s string) bool { return strings.HasPrefix(s, future) }) {
		t.Fatalf("statements: %q", stmts)
	}
	run(stmts, nil)
	gs = access("rs_acc_user")
	for _, want := range []struct{ level, object, priv string }{
		{driver.LevelSchema, "", "USAGE"}, {driver.LevelTables, "", "INSERT"}, {driver.LevelTable, "a", "DELETE"},
		{driver.LevelFuture, "", "UPDATE"}, {driver.LevelSequences, "", "USAGE"}, {driver.LevelFutureSequences, "", "SELECT"},
	} {
		if !has(gs, want.level, want.object, want.priv, "") {
			t.Errorf("after the write preset, missing %+v in %+v", want, gs)
		}
	}
	// Tables created later get the default privileges.
	run([]string{`CREATE TABLE rs_acc.c (id int)`}, nil)
	if !has(access("rs_acc_user"), driver.LevelTable, "c", "INSERT", "") {
		t.Error("a table created later didn't get the default privileges")
	}

	// Revoking one privilege on one table drops it from the "all tables" level.
	run(c.GrantSQL([]driver.GrantChange{{Grant: driver.Grant{Principal: "rs_acc_user", Level: driver.LevelTable, Schema: "rs_acc", Object: "a", Privilege: "DELETE"}, Revoke: true}}))
	gs = access("rs_acc_user")
	if has(gs, driver.LevelTable, "a", "DELETE", "") || has(gs, driver.LevelTables, "", "DELETE", "") || !has(gs, driver.LevelTables, "", "SELECT", "") {
		t.Fatalf("after revoking DELETE on a: %+v", gs)
	}

	// The table's view: who can do what on it.
	og, err := c.ObjectAccess(ctx, driver.ObjectRef{Schema: "rs_acc", Name: "b"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(og, func(g driver.Grant) bool { return g.Principal == "rs_acc_readers" && g.Privilege == "SELECT" }) ||
		!slices.ContainsFunc(og, func(g driver.Grant) bool { return g.Principal == admin && g.Via == "owner" }) {
		t.Fatalf("object access: %+v", og)
	}

	// Attributes, then dropping the role once it owns nothing.
	run(c.AccountSQL(driver.AccountOp{Op: "alter", Name: "rs_acc_user", Attrs: map[string]string{"login": "false", "valid_until": "2030-01-31"}}))
	var login bool
	var until string
	_ = p.db.QueryRowContext(ctx, `SELECT rolcanlogin, to_char(rolvaliduntil, 'YYYY-MM-DD') FROM pg_roles WHERE rolname = 'rs_acc_user'`).Scan(&login, &until)
	if login || until != "2030-01-31" {
		t.Fatalf("alter: login=%v until=%s", login, until)
	}
	if _, err := c.GrantSQL([]driver.GrantChange{{Grant: driver.Grant{Principal: "x", Level: driver.LevelTable, Schema: "s", Object: "t", Privilege: "SELECT; DROP TABLE t"}}}); err == nil {
		t.Fatal("an unknown privilege was accepted")
	}

	// A role with privileges and objects can't simply be dropped; with
	// reassign, what it owns is handed over and its privileges go first.
	run([]string{`CREATE TABLE rs_acc.owned (id int)`, `ALTER TABLE rs_acc.owned OWNER TO rs_acc_user`}, nil)
	plain, _ := c.AccountSQL(driver.AccountOp{Op: "drop", Name: "rs_acc_user"})
	if _, err := p.db.ExecContext(ctx, plain[0]); err == nil {
		t.Fatal("dropped a role that still had privileges")
	}
	run(c.AccountSQL(driver.AccountOp{Op: "drop", Name: "rs_acc_user", Attrs: map[string]string{"reassign": "true"}}))
	var owner string
	if err := p.db.QueryRowContext(ctx, `SELECT tableowner FROM pg_tables WHERE schemaname = 'rs_acc' AND tablename = 'owned'`).Scan(&owner); err != nil || owner != admin {
		t.Fatalf("the table it owned: owner %q, %v", owner, err)
	}
}
