package mssql

import (
	"context"
	"slices"
	"testing"
	"time"

	"rowsmith/internal/driver"
)

func TestIntegrationAccess(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	c := openConn(t, testParams(t, "master"))
	defer c.Close()
	// One connection, so USE carries from one statement to the next as it does in a session.
	sc, err := c.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer sc.Close()
	run := func(stmts []string, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range stmts {
			if _, err := sc.ExecContext(ctx, s); err != nil {
				t.Fatalf("%s: %v", s, err)
			}
		}
	}
	cleanup := func() {
		for _, s := range []string{"USE [master]", "IF DB_ID(N'rs_acc') IS NOT NULL BEGIN ALTER DATABASE [rs_acc] SET SINGLE_USER WITH ROLLBACK IMMEDIATE; DROP DATABASE [rs_acc] END",
			"IF SUSER_ID(N'rs_acc_user') IS NOT NULL DROP LOGIN [rs_acc_user]", "IF SUSER_ID(N'rs_acc_srv') IS NOT NULL DROP SERVER ROLE [rs_acc_srv]"} {
			_, _ = sc.ExecContext(ctx, s)
		}
	}
	cleanup()
	defer cleanup()
	run([]string{"CREATE DATABASE [rs_acc]", "USE [rs_acc]", "CREATE TABLE dbo.a (id int PRIMARY KEY)", "CREATE TABLE dbo.b (id int PRIMARY KEY)", "USE [master]"}, nil)

	// A login made through AccountSQL signs in with its password.
	if _, err := c.AccountSQL(driver.AccountOp{Op: "create", Kind: "user", Name: "rs_acc_user"}); err == nil {
		t.Fatal("created a login without a password")
	}
	run(c.AccountSQL(driver.AccountOp{Op: "create", Kind: "user", Name: "rs_acc_user", Password: "Wild-Otter-Lantern-42", Attrs: map[string]string{"check_policy": "false"}}))
	signIn := func() error {
		p := testParams(t, "master")
		p.Params["user"], p.Secrets["password"] = "rs_acc_user", "Wild-Otter-Lantern-42"
		dc, err := mustDriver(t).Open(ctx, p)
		if err == nil {
			dc.Close()
		}
		return err
	}
	if err := signIn(); err != nil {
		t.Fatalf("signing in as the new login: %v", err)
	}
	run(c.AccountSQL(driver.AccountOp{Op: "create", Kind: "role", Name: "rs_acc_srv"}))
	run(c.AccountSQL(driver.AccountOp{Op: "add_role", Name: "rs_acc_user", Role: "rs_acc_srv"}))
	ps, err := c.Principals(ctx)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(ps, func(p driver.Principal) bool { return p.Name == "rs_acc_user" })
	j := slices.IndexFunc(ps, func(p driver.Principal) bool { return p.Name == "rs_acc_srv" })
	if i < 0 || j < 0 || !slices.Contains(ps[i].MemberOf, "rs_acc_srv") || !ps[j].ServerOnly || ps[i].Attributes["check_policy"] != false {
		t.Fatalf("principals: %+v", ps)
	}

	// Server-wide, database, schema and table grants; the database user is made on the way.
	stmts, err := c.GrantSQL([]driver.GrantChange{
		{Grant: driver.Grant{Principal: "rs_acc_user", Level: driver.LevelServer, Privilege: "VIEW SERVER STATE"}},
		{Grant: driver.Grant{Principal: "rs_acc_user", Level: driver.LevelDatabase, Database: "rs_acc", Privilege: "SELECT"}},
		{Grant: driver.Grant{Principal: "rs_acc_user", Level: driver.LevelSchema, Database: "rs_acc", Schema: "dbo", Privilege: "EXECUTE"}},
		{Grant: driver.Grant{Principal: "rs_acc_user", Level: driver.LevelTable, Database: "rs_acc", Schema: "dbo", Object: "a", Privilege: "INSERT"}},
		{Grant: driver.Grant{Principal: "rs_acc_srv", Level: driver.LevelServer, Privilege: "VIEW ANY DEFINITION"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	run(stmts, nil)
	scope := driver.Scope{Database: "rs_acc", Schema: "dbo"}
	has := func(gs []driver.Grant, level, object, priv, via string) bool {
		return slices.ContainsFunc(gs, func(g driver.Grant) bool {
			return g.Level == level && g.Object == object && g.Privilege == priv && g.Via == via
		})
	}
	gs, err := c.Access(ctx, "rs_acc_user", scope)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []struct{ level, object, priv, via string }{
		{driver.LevelServer, "", "VIEW SERVER STATE", ""}, {driver.LevelServer, "", "VIEW ANY DEFINITION", "rs_acc_srv"},
		{driver.LevelDatabase, "", "CONNECT", ""}, {driver.LevelDatabase, "", "SELECT", ""},
		{driver.LevelSchema, "", "EXECUTE", ""}, {driver.LevelTable, "a", "INSERT", ""},
	} {
		if !has(gs, want.level, want.object, want.priv, want.via) {
			t.Errorf("missing %+v in %+v", want, gs)
		}
	}

	og, err := c.ObjectAccess(ctx, driver.ObjectRef{Database: "rs_acc", Schema: "dbo", Name: "a"})
	if err != nil {
		t.Fatal(err)
	}
	holds := func(who, priv, via string) bool {
		return slices.ContainsFunc(og, func(g driver.Grant) bool { return g.Principal == who && g.Privilege == priv && g.Via == via })
	}
	if !holds("rs_acc_user", "INSERT", "") || !holds("rs_acc_user", "SELECT", "the whole database") || !holds("dbo", "DELETE", "owner") {
		t.Fatalf("object access: %+v", og)
	}

	// Revoking, and access through a fixed database role.
	run(c.GrantSQL([]driver.GrantChange{{Grant: driver.Grant{Principal: "rs_acc_user", Level: driver.LevelTable, Database: "rs_acc", Schema: "dbo", Object: "a", Privilege: "INSERT"}, Revoke: true}}))
	run([]string{"USE [rs_acc]", "ALTER ROLE db_datareader ADD MEMBER [rs_acc_user]", "USE [master]"}, nil)
	gs, _ = c.Access(ctx, "rs_acc_user", scope)
	if has(gs, driver.LevelTable, "a", "INSERT", "") || !has(gs, driver.LevelDatabase, "", "SELECT", "db_datareader") {
		t.Fatalf("after revoking INSERT and joining db_datareader: %+v", gs)
	}

	// Disabling, then dropping the login with its database user.
	run(c.AccountSQL(driver.AccountOp{Op: "alter", Name: "rs_acc_user", Attrs: map[string]string{"locked": "true"}}))
	if signIn() == nil {
		t.Fatal("a disabled login signed in")
	}
	if _, err := c.GrantSQL([]driver.GrantChange{{Grant: driver.Grant{Principal: "x", Level: driver.LevelDatabase, Database: "rs_acc", Privilege: "SELECT; DROP DATABASE rs_acc"}}}); err == nil {
		t.Fatal("an unknown privilege was accepted")
	}
	stmts, _ = c.AccountSQL(driver.AccountOp{Op: "drop", Kind: "user", Name: "rs_acc_user", Attrs: map[string]string{"users": "true"}})
	run(append([]string{"USE [rs_acc]"}, stmts...), nil)
	var users int
	_ = sc.QueryRowContext(ctx, "SELECT COUNT(*) FROM [rs_acc].sys.database_principals WHERE name = N'rs_acc_user'").Scan(&users)
	if users != 0 || sc.QueryRowContext(ctx, "SELECT 1 FROM sys.server_principals WHERE name = N'rs_acc_user'").Scan(new(int)) == nil {
		t.Fatal("the login or its database user is still there")
	}
}

func mustDriver(t *testing.T) driver.Driver {
	t.Helper()
	d, ok := driver.Get("mssql")
	if !ok {
		t.Fatal("driver not registered")
	}
	return d
}
