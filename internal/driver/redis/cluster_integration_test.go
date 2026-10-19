package redis

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"

	"rowsmith/internal/driver"
)

// The cluster test runs against the dev stack's three-primary cluster
// (dev/compose.yaml, seeded from dev/seed/redis-cluster.txt):
//
//	ROWSMITH_TEST_REDIS_CLUSTER_HOST, _PORT (7001), _PASSWORD
func TestIntegrationCluster(t *testing.T) {
	if os.Getenv("ROWSMITH_TEST_REDIS_CLUSTER_PORT") == "" {
		os.Setenv("ROWSMITH_TEST_REDIS_CLUSTER_PORT", "7001")
	}
	// A database number is ignored on a cluster, which has only db0.
	c := openTestConn(t, target{"redis", "ROWSMITH_TEST_REDIS_CLUSTER_"}, map[string]any{"database": 3})
	ctx := context.Background()
	if c.cluster == nil {
		t.Fatal("the cluster was not recognised")
	}
	srv, err := c.Server(ctx)
	if err != nil || !strings.HasPrefix(srv.Extras["Mode"], "cluster of 3 primaries, ok") || srv.Database != "db0" {
		t.Errorf("server = %+v, %v", srv, err)
	}
	dbs, err := c.Databases(ctx)
	if err != nil || len(dbs) != 1 || *dbs[0].Tables < 23 {
		t.Fatalf("databases = %+v, %v", dbs, err)
	}

	// Keys come from every primary, not just the one connected to.
	objs, err := c.Objects(ctx, driver.Scope{Database: "db0"})
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]string{}
	for _, o := range objs {
		names[o.Name] = o.Kind
	}
	for k, kind := range map[string]string{"user:1": kindHash, "user:3": kindHash, "leaderboard": kindZSet, "orders": kindStream,
		"queue:email": kindList, "tags": kindSet, "product:12": kindString, "{cart:7}:items": kindHash} {
		if names[k] != kind {
			t.Errorf("%s = %q (found %d keys)", k, names[k], len(objs))
		}
	}
	products, err := c.ObjectsMatching(ctx, driver.Scope{}, "product:*", 100)
	if err != nil || len(products) != 12 {
		t.Errorf("product:* = %d keys, %v", len(products), err)
	}
	if _, err := c.Objects(ctx, driver.Scope{Database: "db1"}); err == nil {
		t.Error("db1 listed on a cluster")
	}

	// Browsing and edits go to the node that owns each key.
	res := browse(t, c, driver.BrowseRequest{Ref: driver.ObjectRef{Name: "leaderboard"}, Limit: 10})
	if len(res.Rows) != 3 {
		t.Errorf("leaderboard = %v", res.Rows)
	}
	ref := driver.ObjectRef{Database: "db0", Name: "rowsmith:test:cluster"}
	t.Cleanup(func() { c.cluster.Del(context.Background(), ref.Name) })
	c.cluster.Del(ctx, ref.Name)
	c.cluster.HSet(ctx, ref.Name, "a", "1", "b", "2")
	if _, err := c.ApplyEdits(ctx, ref, []driver.RowEdit{
		{Op: "update", Key: map[string]any{"field": "a"}, Values: map[string]any{"value": "one"}},
		{Op: "delete", Key: map[string]any{"field": "b"}},
		{Op: "insert", Values: map[string]any{"field": "c", "value": "3"}},
	}); err != nil {
		t.Fatalf("edit: %v", err)
	}
	if got, _ := c.cluster.HGetAll(ctx, ref.Name).Result(); len(got) != 2 || got["a"] != "one" || got["c"] != "3" {
		t.Errorf("after edit = %v", got)
	}

	// The console: keys on different nodes, cluster-wide KEYS and DBSIZE,
	// and plain refusals for what a cluster cannot do here.
	sess, err := c.NewSession(ctx, driver.Scope{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	out := &recSink{}
	sess.Execute(ctx, "GET product:1\nGET product:2\nHGET user:3 name\nKEYS product:*\nDBSIZE\nINFO server", driver.ExecOptions{}, out)
	for i, err := range out.errs {
		if err != nil {
			t.Fatalf("%s: %v", out.stmts[i].SQL, err)
		}
	}
	if out.rows[0][0][0] != "Espresso" || out.rows[2][0][0] != "Linus" || len(out.rows[3]) != 12 || out.rows[4][0][0].(int64) < 23 {
		t.Errorf("console = %v", out.rows)
	}
	if !slices.ContainsFunc(out.notices, func(n string) bool { return strings.Contains(n, "one node") }) {
		t.Errorf("notices = %q", out.notices)
	}
	for _, cmd := range []string{"MULTI", "SELECT 1", "SCAN 0", "RENAME product:1 product:99"} {
		out := &recSink{}
		sess.Execute(ctx, cmd, driver.ExecOptions{}, out)
		if len(out.errs) != 1 || out.errs[0] == nil {
			t.Errorf("%s ran on a cluster: %v", cmd, out.errs)
		} else {
			t.Logf("%s: %v", cmd, out.errs[0])
		}
	}
	out = &recSink{}
	sess.Execute(ctx, "RENAME {cart:7}:owner {cart:7}:buyer\nRENAME {cart:7}:buyer {cart:7}:owner", driver.ExecOptions{}, out)
	if len(out.errs) != 2 || out.errs[0] != nil || out.errs[1] != nil {
		t.Errorf("rename within a hash tag: %v", out.errs)
	}

	// Clients of every node, and stopping one by node/id.
	procs, err := c.Processes(ctx)
	if err != nil || len(procs.Rows) < 3 {
		t.Fatalf("processes = %v, %v", procs, err)
	}
	nodes := map[any]bool{}
	for _, r := range procs.Rows {
		nodes[r[1]] = true
	}
	if len(nodes) != 3 {
		t.Errorf("clients from %d nodes", len(nodes))
	}
	if err := c.KillProcess(ctx, "redis-cluster:7001/999999999"); err == nil || !strings.Contains(err.Error(), "no longer connected") {
		t.Errorf("kill unknown client: %v", err)
	}
}
