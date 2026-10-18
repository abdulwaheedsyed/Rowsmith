package mongodb

import (
	"context"
	"os"
	"slices"
	"testing"
	"time"

	"rowsmith/internal/driver"
	"rowsmith/internal/sqlsplit"
)

func TestIntegrationAccess(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	c := openTestConn(t)
	sess, err := c.NewSession(ctx, driver.Scope{Database: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	// Statements run as the apply endpoint runs them: one console script.
	run := func(stmts []string, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		runScript(t, sess, []string{sqlsplit.Join("mongodb", stmts)})
	}
	cleanup := func() {
		for _, s := range []string{`db.getSiblingDB("admin").runCommand({ dropUser: "rs_acc_user" })`, `db.getSiblingDB("admin").runCommand({ dropRole: "rs_acc_readers" })`} {
			_ = sess.Execute(ctx, s, driver.ExecOptions{}, &recSink{})
		}
	}
	cleanup()
	defer cleanup()

	if _, err := c.AccountSQL(driver.AccountOp{Op: "create", Kind: "user", Name: "rs_acc_user@admin"}); err == nil {
		t.Fatal("created a user without a password")
	}
	run(c.AccountSQL(driver.AccountOp{Op: "create", Kind: "user", Name: "rs_acc_user@admin", Password: "Wild-Otter-Lantern-42"}))
	signIn := func(password string) error {
		d, _ := driver.Get("mongodb")
		cn, err := d.Open(ctx, driver.OpenParams{AppName: "rowsmith-test",
			Params:  map[string]any{"mode": "fields", "host": os.Getenv("ROWSMITH_TEST_MONGO_HOST"), "port": "27017", "user": "rs_acc_user", "authSource": "admin", "tls": "disable"},
			Secrets: map[string]string{"password": password}})
		if err == nil {
			cn.Close()
		}
		return err
	}
	if err := signIn("Wild-Otter-Lantern-42"); err != nil {
		t.Fatalf("signing in as the new user: %v", err)
	}
	run(c.AccountSQL(driver.AccountOp{Op: "create", Kind: "role", Name: "rs_acc_readers@admin"}))
	run(c.GrantSQL([]driver.GrantChange{{Grant: driver.Grant{Principal: "rs_acc_readers@admin", Level: driver.LevelDatabase, Database: "rs_acc", Privilege: "read"}, Kind: "role"}}))
	run(c.AccountSQL(driver.AccountOp{Op: "add_role", Kind: "user", Name: "rs_acc_user@admin", Role: "rs_acc_readers@admin"}))
	run(c.GrantSQL([]driver.GrantChange{
		{Grant: driver.Grant{Principal: "rs_acc_user@admin", Level: driver.LevelDatabase, Database: "rs_acc", Privilege: "readWrite"}, Kind: "user"},
		{Grant: driver.Grant{Principal: "rs_acc_user@admin", Level: driver.LevelServer, Privilege: "clusterMonitor"}, Kind: "user"},
	}))

	ps, err := c.Principals(ctx)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(ps, func(p driver.Principal) bool { return p.Name == "rs_acc_user@admin" })
	j := slices.IndexFunc(ps, func(p driver.Principal) bool { return p.Name == "rs_acc_readers@admin" })
	if i < 0 || j < 0 || ps[j].Kind != "role" || !slices.Equal(ps[i].MemberOf, []string{"rs_acc_readers@admin"}) {
		t.Fatalf("principals: %+v", ps)
	}
	has := func(gs []driver.Grant, level, priv, via string) bool {
		return slices.ContainsFunc(gs, func(g driver.Grant) bool { return g.Level == level && g.Privilege == priv && g.Via == via })
	}
	gs, err := c.Access(ctx, "rs_acc_user@admin", driver.Scope{Database: "rs_acc"})
	if err != nil {
		t.Fatal(err)
	}
	if !has(gs, driver.LevelDatabase, "readWrite", "") || !has(gs, driver.LevelServer, "clusterMonitor", "") || !has(gs, driver.LevelDatabase, "read", "rs_acc_readers@admin") {
		t.Fatalf("access: %+v", gs)
	}
	if gs, _ := c.Access(ctx, "rs_acc_user@admin", driver.Scope{Database: "other"}); has(gs, driver.LevelDatabase, "readWrite", "") {
		t.Fatalf("a role on rs_acc showed on another database: %+v", gs)
	}

	run(c.GrantSQL([]driver.GrantChange{{Grant: driver.Grant{Principal: "rs_acc_user@admin", Level: driver.LevelDatabase, Database: "rs_acc", Privilege: "readWrite"}, Revoke: true, Kind: "user"}}))
	if gs, _ = c.Access(ctx, "rs_acc_user@admin", driver.Scope{Database: "rs_acc"}); has(gs, driver.LevelDatabase, "readWrite", "") {
		t.Fatalf("readWrite still granted: %+v", gs)
	}
	run(c.AccountSQL(driver.AccountOp{Op: "password", Name: "rs_acc_user@admin", Password: "Quiet-Heron-Basin-77"}))
	if err := signIn("Quiet-Heron-Basin-77"); err != nil {
		t.Fatalf("signing in with the new password: %v", err)
	}
	if _, err := c.GrantSQL([]driver.GrantChange{{Grant: driver.Grant{Principal: "x", Level: driver.LevelDatabase, Database: "d", Privilege: "root"}}}); err == nil {
		t.Fatal("root was accepted as a database role")
	}
	run(c.AccountSQL(driver.AccountOp{Op: "drop", Kind: "user", Name: "rs_acc_user@admin"}))
	run(c.AccountSQL(driver.AccountOp{Op: "drop", Kind: "role", Name: "rs_acc_readers@admin"}))
}
