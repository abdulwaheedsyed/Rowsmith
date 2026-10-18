package oracle

import (
	"context"
	"os"
	"slices"
	"testing"
	"time"

	"rowsmith/internal/driver"
)

func TestIntegrationAccess(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	c := mustOpen(t, testParams(t, "system", systemPassword()))
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
	cleanup := func() {
		for _, s := range []string{"DROP USER RS_ACC_USER CASCADE", "DROP USER RS_ACC_OWNER CASCADE", "DROP ROLE RS_ACC_READERS"} {
			_, _ = c.db.ExecContext(ctx, s)
		}
	}
	cleanup()
	defer cleanup()
	run([]string{"CREATE USER RS_ACC_OWNER NO AUTHENTICATION QUOTA UNLIMITED ON USERS",
		"CREATE TABLE RS_ACC_OWNER.A (ID NUMBER PRIMARY KEY)", "CREATE TABLE RS_ACC_OWNER.B (ID NUMBER PRIMARY KEY)"}, nil)

	// A plain name is upper-cased, and the new user can sign in.
	run(c.AccountSQL(driver.AccountOp{Op: "create", Kind: "user", Name: "rs_acc_user", Password: "Wild-Otter-Lantern-42"}))
	signIn := func() error {
		dc, err := oracleDriver{}.Open(ctx, testParams(t, "RS_ACC_USER", "Wild-Otter-Lantern-42"))
		if err == nil {
			dc.Close()
		}
		return err
	}
	if err := signIn(); err != nil {
		t.Fatalf("signing in as the new user: %v", err)
	}
	run(c.AccountSQL(driver.AccountOp{Op: "create", Kind: "role", Name: "rs_acc_readers"}))
	run(c.AccountSQL(driver.AccountOp{Op: "add_role", Name: "RS_ACC_USER", Role: "RS_ACC_READERS"}))
	ps, err := c.Principals(ctx)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(ps, func(p driver.Principal) bool { return p.Name == "RS_ACC_USER" })
	sys := slices.IndexFunc(ps, func(p driver.Principal) bool { return p.Name == "SYS" })
	if i < 0 || sys < 0 || !slices.Contains(ps[i].MemberOf, "RS_ACC_READERS") || !ps[sys].System || ps[i].Attributes["locked"] != false {
		t.Fatalf("principals: %+v", ps)
	}

	changes := []driver.GrantChange{
		{Grant: driver.Grant{Principal: "RS_ACC_USER", Level: driver.LevelServer, Privilege: "CREATE TABLE"}},
		{Grant: driver.Grant{Principal: "RS_ACC_USER", Level: driver.LevelTable, Schema: "RS_ACC_OWNER", Object: "A", Privilege: "INSERT"}},
		{Grant: driver.Grant{Principal: "RS_ACC_READERS", Level: driver.LevelTable, Schema: "RS_ACC_OWNER", Object: "B", Privilege: "DELETE"}},
	}
	if c.schemaPrivileges() {
		changes = append(changes, driver.GrantChange{Grant: driver.Grant{Principal: "RS_ACC_USER", Level: driver.LevelSchema, Schema: "RS_ACC_OWNER", Privilege: "SELECT"}})
	}
	run(c.GrantSQL(changes))
	scope := driver.Scope{Schema: "RS_ACC_OWNER"}
	has := func(gs []driver.Grant, level, object, priv, via string) bool {
		return slices.ContainsFunc(gs, func(g driver.Grant) bool {
			return g.Level == level && g.Object == object && g.Privilege == priv && g.Via == via
		})
	}
	gs, err := c.Access(ctx, "RS_ACC_USER", scope)
	if err != nil {
		t.Fatal(err)
	}
	if !has(gs, driver.LevelServer, "", "CREATE SESSION", "") || !has(gs, driver.LevelServer, "", "CREATE TABLE", "") ||
		!has(gs, driver.LevelTable, "A", "INSERT", "") || !has(gs, driver.LevelTable, "B", "DELETE", "RS_ACC_READERS") {
		t.Fatalf("access: %+v", gs)
	}
	og, err := c.ObjectAccess(ctx, driver.ObjectRef{Schema: "RS_ACC_OWNER", Name: "A"})
	if err != nil {
		t.Fatal(err)
	}
	holds := func(who, priv, via string) bool {
		return slices.ContainsFunc(og, func(g driver.Grant) bool { return g.Principal == who && g.Privilege == priv && g.Via == via })
	}
	if !holds("RS_ACC_USER", "INSERT", "") || !holds("RS_ACC_OWNER", "SELECT", "owner") {
		t.Fatalf("object access: %+v", og)
	}
	if c.schemaPrivileges() {
		if !has(gs, driver.LevelSchema, "", "SELECT", "") || !holds("RS_ACC_USER", "SELECT", "the whole schema") {
			t.Fatalf("schema privileges: %+v / %+v", gs, og)
		}
		run(c.GrantSQL([]driver.GrantChange{{Grant: driver.Grant{Principal: "RS_ACC_USER", Level: driver.LevelSchema, Schema: "RS_ACC_OWNER", Privilege: "SELECT"}, Revoke: true}}))
		if gs, _ = c.Access(ctx, "RS_ACC_USER", scope); has(gs, driver.LevelSchema, "", "SELECT", "") {
			t.Fatal("SELECT ANY TABLE ON SCHEMA is still granted")
		}
	}

	// Locking stops sign-in; quotes in a password are refused.
	run(c.AccountSQL(driver.AccountOp{Op: "alter", Name: "RS_ACC_USER", Attrs: map[string]string{"locked": "true"}}))
	if signIn() == nil {
		t.Fatal("a locked account signed in")
	}
	if _, err := c.AccountSQL(driver.AccountOp{Op: "password", Name: "RS_ACC_USER", Password: `a" ACCOUNT UNLOCK --`}); err == nil {
		t.Fatal("a password with a double quote was accepted")
	}
	run(c.AccountSQL(driver.AccountOp{Op: "drop", Kind: "user", Name: "RS_ACC_USER"}))

	// Without the DBA_ views, a schema owner still sees the accounts and grants on its own tables.
	if pw := os.Getenv("ROWSMITH_TEST_ORACLE_PASSWORD"); pw != "" {
		app := mustOpen(t, testParams(t, appUser(), pw))
		ps, err := app.Principals(ctx)
		if err != nil || len(ps) == 0 {
			t.Fatalf("principals without DBA views: %v %v", len(ps), err)
		}
		if _, err := app.Access(ctx, app.user, driver.Scope{Schema: app.user}); err != nil {
			t.Fatalf("access without DBA views: %v", err)
		}
	}
}
