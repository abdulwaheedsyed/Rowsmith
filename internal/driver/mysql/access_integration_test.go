package mysql

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"rowsmith/internal/driver"
)

// Integration tests run against real servers when ROWSMITH_TEST_MYSQL_HOST
// is set; ROWSMITH_TEST_MARIADB_HOST and ROWSMITH_TEST_TIDB_HOST add
// MariaDB and TiDB runs. They create and drop their own accounts and
// database.
//
//	ROWSMITH_TEST_MYSQL_HOST, ROWSMITH_TEST_MARIADB_HOST, ROWSMITH_TEST_TIDB_HOST (port 4000), _PASSWORD (for root)

var testPorts = map[string]string{"mysql": "3306", "mariadb": "3306", "tidb": "4000"}

func TestIntegrationAccess(t *testing.T) {
	for flavor, env := range map[string]string{"mysql": "ROWSMITH_TEST_MYSQL_HOST", "mariadb": "ROWSMITH_TEST_MARIADB_HOST", "tidb": "ROWSMITH_TEST_TIDB_HOST"} {
		t.Run(flavor, func(t *testing.T) {
			host := os.Getenv(env)
			if host == "" {
				t.Skip(env + " not set")
			}
			testAccess(t, flavor, host)
		})
	}
}

func testAccess(t *testing.T, flavor, host string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	open := func(user, password string) (*conn, error) {
		dc, err := (&mysqlDriver{id: flavor}).Open(ctx, driver.OpenParams{Params: map[string]any{"host": host, "port": testPorts[flavor], "user": user, "tls": "disable"},
			Secrets: map[string]string{"password": password}, AppName: "rowsmith-test"})
		if err != nil {
			return nil, err
		}
		return dc.(*conn), nil
	}
	c, err := open("root", os.Getenv("ROWSMITH_TEST_MYSQL_PASSWORD"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	run := func(stmts []string, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range stmts {
			if _, err := c.db.ExecContext(ctx, s); err != nil {
				t.Fatalf("%s: %v", s, err)
			}
		}
	}
	role := "rs_acc_readers@%"
	if flavor == "mariadb" {
		role = "rs_acc_readers"
	}
	cleanup := func() {
		for _, s := range []string{"DROP DATABASE IF EXISTS rs_acc", "DROP USER IF EXISTS 'rs_acc_user'@'%'", "DROP ROLE IF EXISTS " + c.acct(role)} {
			_, _ = c.db.ExecContext(ctx, s)
		}
	}
	cleanup()
	defer cleanup()
	run([]string{"CREATE DATABASE rs_acc", "CREATE TABLE rs_acc.a (id int PRIMARY KEY)", "CREATE TABLE rs_acc.b (id int PRIMARY KEY)"}, nil)

	// A new user without a host may connect from anywhere, and signs in with its password.
	run(c.AccountSQL(driver.AccountOp{Op: "create", Kind: "user", Name: "rs_acc_user", Password: "Wild-Otter-Lantern-42", Attrs: map[string]string{"conn_limit": "4"}}))
	if uc, err := open("rs_acc_user", "Wild-Otter-Lantern-42"); err != nil {
		t.Fatalf("signing in as the new user: %v", err)
	} else {
		uc.Close()
	}
	run(c.AccountSQL(driver.AccountOp{Op: "create", Kind: "role", Name: role}))
	run(c.AccountSQL(driver.AccountOp{Op: "add_role", Name: "rs_acc_user@%", Role: role}))

	ps, err := c.Principals(ctx)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(ps, func(p driver.Principal) bool { return p.Name == "rs_acc_user@%" })
	j := slices.IndexFunc(ps, func(p driver.Principal) bool { return p.Name == role })
	if i < 0 || j < 0 || (flavor != "tidb" && ps[i].Attributes["conn_limit"] != 4) || !slices.Contains(ps[i].MemberOf, role) || ps[j].Kind != "role" {
		t.Fatalf("principals: %+v", ps)
	}

	scope := driver.Scope{Database: "rs_acc"}
	has := func(gs []driver.Grant, level, object, priv, via string) bool {
		return slices.ContainsFunc(gs, func(g driver.Grant) bool {
			return g.Level == level && g.Object == object && g.Privilege == priv && g.Via == via
		})
	}
	run(c.GrantSQL([]driver.GrantChange{
		{Grant: driver.Grant{Principal: "rs_acc_user@%", Level: driver.LevelDatabase, Database: "rs_acc", Privilege: "SELECT"}},
		{Grant: driver.Grant{Principal: "rs_acc_user@%", Level: driver.LevelTable, Database: "rs_acc", Object: "a", Privilege: "INSERT"}},
		{Grant: driver.Grant{Principal: "rs_acc_user@%", Level: driver.LevelTable, Database: "rs_acc", Object: "a", Privilege: "UPDATE"}},
		{Grant: driver.Grant{Principal: role, Level: driver.LevelTable, Database: "rs_acc", Object: "b", Privilege: "DELETE"}},
	}))
	gs, err := c.Access(ctx, "rs_acc_user@%", scope)
	if err != nil {
		t.Fatal(err)
	}
	if !has(gs, driver.LevelDatabase, "", "SELECT", "") || !has(gs, driver.LevelTable, "a", "UPDATE", "") || !has(gs, driver.LevelTable, "b", "DELETE", role) {
		t.Fatalf("access: %+v", gs)
	}

	og, err := c.ObjectAccess(ctx, driver.ObjectRef{Database: "rs_acc", Name: "a"})
	if err != nil {
		t.Fatal(err)
	}
	holds := func(priv, via string) bool {
		return slices.ContainsFunc(og, func(g driver.Grant) bool {
			return g.Principal == "rs_acc_user@%" && g.Privilege == priv && g.Via == via
		})
	}
	if !holds("INSERT", "") || !holds("SELECT", "the whole database") {
		t.Fatalf("object access: %+v", og)
	}

	run(c.GrantSQL([]driver.GrantChange{{Grant: driver.Grant{Principal: "rs_acc_user@%", Level: driver.LevelTable, Database: "rs_acc", Object: "a", Privilege: "INSERT"}, Revoke: true}}))
	gs, _ = c.Access(ctx, "rs_acc_user@%", scope)
	if has(gs, driver.LevelTable, "a", "INSERT", "") || !has(gs, driver.LevelTable, "a", "UPDATE", "") {
		t.Fatalf("after revoking INSERT: %+v", gs)
	}

	// Locking: the account can no longer sign in.
	run(c.AccountSQL(driver.AccountOp{Op: "alter", Name: "rs_acc_user@%", Attrs: map[string]string{"locked": "true"}}))
	if uc, err := open("rs_acc_user", "Wild-Otter-Lantern-42"); err == nil {
		uc.Close()
		t.Fatal("a locked account signed in")
	}
	stmts, err := c.AccountSQL(driver.AccountOp{Op: "password", Name: "rs_acc_user@%", Password: driver.Redacted})
	if err != nil || !strings.Contains(strings.Join(stmts, ""), driver.Redacted) {
		t.Fatalf("redacted preview: %q %v", stmts, err)
	}
	if _, err := c.GrantSQL([]driver.GrantChange{{Grant: driver.Grant{Principal: "x@%", Level: driver.LevelServer, Privilege: "ALL; DROP DATABASE x"}}}); err == nil {
		t.Fatal("an unknown privilege was accepted")
	}
	run(c.AccountSQL(driver.AccountOp{Op: "drop", Name: "rs_acc_user@%"}))
}
