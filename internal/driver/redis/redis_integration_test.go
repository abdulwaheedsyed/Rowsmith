package redis

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"rowsmith/internal/driver"
)

// Integration tests run against real servers seeded by dev/compose.yaml
// when their hosts are set. They write only keys under rowsmith:test: in
// database 2, and delete them afterwards.
//
//	ROWSMITH_TEST_REDIS_HOST, _PORT (6379), _PASSWORD          the default user
//	ROWSMITH_TEST_VALKEY_HOST, _PORT (6379), _USER, _PASSWORD  an ACL user

type target struct{ id, prefix string }

var targets = []target{{"redis", "ROWSMITH_TEST_REDIS_"}, {"valkey", "ROWSMITH_TEST_VALKEY_"}}

func openTestConn(t *testing.T, tg target, params map[string]any) *conn {
	t.Helper()
	host := os.Getenv(tg.prefix + "HOST")
	if host == "" {
		t.Skip(tg.prefix + "HOST not set")
	}
	p := map[string]any{"host": host, "port": os.Getenv(tg.prefix + "PORT"), "user": os.Getenv(tg.prefix + "USER"), "tls": "disable"}
	for k, v := range params {
		p[k] = v
	}
	d, _ := driver.Get(tg.id)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cn, err := d.Open(ctx, driver.OpenParams{AppName: "rowsmith test", Params: p,
		Secrets: map[string]string{"password": os.Getenv(tg.prefix + "PASSWORD")}})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { cn.Close() })
	return cn.(*conn)
}

func each(t *testing.T, fn func(t *testing.T, c *conn)) {
	for _, tg := range targets {
		t.Run(tg.id, func(t *testing.T) { fn(t, openTestConn(t, tg, nil)) })
	}
}

// recSink records each statement's outcome and result sets.
type recSink struct {
	stmts   []driver.StatementInfo
	errs    []error
	cols    [][]driver.ResultColumn
	rows    [][][]any
	notices []string
}

func (s *recSink) BeginStatement(i driver.StatementInfo) error {
	s.stmts = append(s.stmts, i)
	return nil
}
func (s *recSink) Columns(c []driver.ResultColumn) error {
	s.cols = append(s.cols, c)
	s.rows = append(s.rows, nil)
	return nil
}
func (s *recSink) Rows(r [][]any) error {
	s.rows[len(s.rows)-1] = append(s.rows[len(s.rows)-1], r...)
	return nil
}
func (s *recSink) EndResult(driver.ResultSummary) error { return nil }
func (s *recSink) Notice(_, text string) error          { s.notices = append(s.notices, text); return nil }
func (s *recSink) EndStatement(err error) error         { s.errs = append(s.errs, err); return nil }

func run(t *testing.T, sess driver.Session, script string, opts driver.ExecOptions) *recSink {
	t.Helper()
	sink := &recSink{}
	if err := sess.Execute(context.Background(), script, opts, sink); err != nil {
		t.Fatalf("execute: %v", err)
	}
	return sink
}

// mustRun runs a script and fails on any statement error.
func mustRun(t *testing.T, sess driver.Session, script string) *recSink {
	t.Helper()
	sink := run(t, sess, script, driver.ExecOptions{StopOnError: true})
	for i, err := range sink.errs {
		if err != nil {
			t.Fatalf("%s: %v", sink.stmts[i].SQL, err)
		}
	}
	return sink
}

func cleanup(t *testing.T, c *conn) {
	t.Cleanup(func() {
		ctx := context.Background()
		cl := c.client(2)
		keys, _ := cl.Keys(ctx, "rowsmith:test:*").Result()
		if len(keys) > 0 {
			cl.Del(ctx, keys...)
		}
	})
}

func TestIntegrationCatalog(t *testing.T) {
	each(t, func(t *testing.T, c *conn) {
		ctx := context.Background()
		srv, err := c.Server(ctx)
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]string{"redis": "Redis", "valkey": "Valkey"}[t.Name()[strings.LastIndex(t.Name(), "/")+1:]]
		if srv.Product != want || srv.Version == "" || srv.Database != "db0" {
			t.Fatalf("server = %+v", srv)
		}
		dbs, err := c.Databases(ctx)
		if err != nil || len(dbs) < 2 || dbs[0].Name != "db0" || *dbs[0].Tables < 16 || *dbs[1].Tables != 2 {
			t.Fatalf("databases = %+v, %v", dbs, err)
		}

		objs, err := c.Objects(ctx, driver.Scope{Database: "db0"})
		if err != nil {
			t.Fatal(err)
		}
		byName := map[string]driver.Object{}
		for _, o := range objs {
			byName[o.Name] = o
		}
		for name, kind := range map[string]string{"shop:name": "string", "user:1": "hash", "queue:email": "list", "tags:espresso": "set",
			"leaderboard": "zset", "orders": "stream", "big:hash": "hash"} {
			if byName[name].Kind != kind {
				t.Errorf("%s: kind %q, want %q", name, byName[name].Kind, kind)
			}
		}
		if r := byName["big:hash"].Rows; r == nil || *r != 600 {
			t.Errorf("big:hash rows = %v", r)
		}
		users, err := c.ObjectsMatching(ctx, driver.Scope{Database: "db0"}, "user:*", 0)
		if err != nil || len(users) != 3 || users[0].Name != "user:1" {
			t.Fatalf("user:* = %+v, %v", users, err)
		}
		if few, _ := c.ObjectsMatching(ctx, driver.Scope{Database: "db0"}, "*", 5); len(few) != 5 {
			t.Fatalf("limit 5 gave %d keys", len(few))
		}
		if other, _ := c.Objects(ctx, driver.Scope{Database: "db1"}); len(other) != 2 {
			t.Fatalf("db1 keys = %+v", other)
		}

		tb, err := c.Describe(ctx, driver.ObjectRef{Database: "db0", Name: "leaderboard"})
		if err != nil {
			t.Fatal(err)
		}
		if tb.Kind != "zset" || len(tb.Columns) != 2 || tb.RowKey[0] != "member" || *tb.RowEstimate != 5 || !tb.Editable ||
			tb.Options["Expires"] != "never" || tb.Options["Encoding"] == "" {
			t.Fatalf("describe leaderboard = %+v", tb)
		}
		if tb, _ := c.Describe(ctx, driver.ObjectRef{Database: "db0", Name: "session:abc123"}); !strings.HasPrefix(tb.Options["Expires"], "in ") {
			t.Fatalf("session TTL = %q", tb.Options["Expires"])
		}
		if _, err := c.Describe(ctx, driver.ObjectRef{Database: "db0", Name: "no:such:key"}); err == nil {
			t.Fatal("describe of a missing key succeeded")
		}
	})
}

func browse(t *testing.T, c *conn, req driver.BrowseRequest) *driver.Result {
	t.Helper()
	if req.Ref.Database == "" {
		req.Ref.Database = "db0"
	}
	res, err := c.Browse(context.Background(), req)
	if err != nil {
		t.Fatalf("browse %s: %v", req.Ref.Name, err)
	}
	return res
}

func TestIntegrationBrowse(t *testing.T) {
	each(t, func(t *testing.T, c *conn) {
		ctx := context.Background()
		res := browse(t, c, driver.BrowseRequest{Ref: driver.ObjectRef{Name: "shop:name"}, Limit: 10})
		if len(res.Rows) != 1 || res.Rows[0][0] != "Rowsmith Coffee" {
			t.Fatalf("string = %v", res.Rows)
		}

		// Hash fields sort by name and page.
		res = browse(t, c, driver.BrowseRequest{Ref: driver.ObjectRef{Name: "big:hash"}, Offset: 100, Limit: 50})
		if len(res.Rows) != 50 || res.Rows[0][0] != "field:0101" || res.Rows[0][1] != "303" || !res.Truncated {
			t.Fatalf("hash page = %v… truncated %v", res.Rows[0], res.Truncated)
		}
		n, err := c.Count(ctx, driver.BrowseRequest{Ref: driver.ObjectRef{Database: "db0", Name: "big:hash"},
			Filters: []driver.Filter{{Column: "value", Op: ">", Value: "1700"}}})
		if err != nil || n.Rows != 34 || !n.Exact {
			t.Fatalf("filtered count = %+v, %v", n, err)
		}
		res = browse(t, c, driver.BrowseRequest{Ref: driver.ObjectRef{Name: "big:hash"}, Limit: 5,
			Sort: []driver.Sort{{Column: "value", Desc: true}}})
		if res.Rows[0][0] != "field:0600" {
			t.Fatalf("hash sorted by value = %v", res.Rows[0])
		}
		res = browse(t, c, driver.BrowseRequest{Ref: driver.ObjectRef{Name: "user:2"}, Search: "GRACE", Limit: 10})
		if len(res.Rows) != 2 {
			t.Fatalf("search = %v", res.Rows)
		}

		// Lists page from either end.
		res = browse(t, c, driver.BrowseRequest{Ref: driver.ObjectRef{Name: "big:list"}, Offset: 10, Limit: 3})
		if res.Rows[0][0] != int64(10) || res.Rows[0][1] != "item 11" {
			t.Fatalf("list page = %v", res.Rows)
		}
		res = browse(t, c, driver.BrowseRequest{Ref: driver.ObjectRef{Name: "big:list"}, Limit: 2, Sort: []driver.Sort{{Column: "index", Desc: true}}})
		if res.Rows[0][0] != int64(299) || res.Rows[1][1] != "item 299" {
			t.Fatalf("list from the end = %v", res.Rows)
		}

		res = browse(t, c, driver.BrowseRequest{Ref: driver.ObjectRef{Name: "leaderboard"}, Limit: 2, Sort: []driver.Sort{{Column: "score", Desc: true}}})
		if res.Rows[0][0] != "ada" || res.Rows[0][1] != float64(1520) {
			t.Fatalf("zset = %v", res.Rows)
		}
		res = browse(t, c, driver.BrowseRequest{Ref: driver.ObjectRef{Name: "tags:espresso"}, Limit: 10})
		if len(res.Rows) != 3 || res.Rows[0][0] != "dark" {
			t.Fatalf("set = %v", res.Rows)
		}
		res = browse(t, c, driver.BrowseRequest{Ref: driver.ObjectRef{Name: "orders"}, Offset: 1, Limit: 10})
		if len(res.Rows) != 2 {
			t.Fatalf("stream = %v", res.Rows)
		}
		if b, _ := json.Marshal(res.Rows[0][1]); string(b) != `{"customer":"2","total":"4.20","status":"pending"}` {
			t.Fatalf("stream fields = %s", b)
		}
		res = browse(t, c, driver.BrowseRequest{Ref: driver.ObjectRef{Name: "orders"}, Limit: 1, Sort: []driver.Sort{{Column: "id", Desc: true}}})
		if b, _ := json.Marshal(res.Rows[0][1]); !strings.Contains(string(b), `"30.00"`) {
			t.Fatalf("newest entry = %s", b)
		}
	})
}

func TestIntegrationEdits(t *testing.T) {
	each(t, func(t *testing.T, c *conn) {
		cleanup(t, c)
		ctx := context.Background()
		cl := c.client(2)
		ref := func(name string) driver.ObjectRef { return driver.ObjectRef{Database: "db2", Name: name} }
		apply := func(name string, edits ...driver.RowEdit) error {
			_, err := c.ApplyEdits(ctx, ref(name), edits)
			return err
		}
		ok := func(err error) {
			t.Helper()
			if err != nil {
				t.Fatal(err)
			}
		}

		// Strings keep their expiry when edited.
		cl.Set(ctx, "rowsmith:test:s", "one", time.Hour)
		ok(apply("rowsmith:test:s", driver.RowEdit{Op: "update", Values: map[string]any{"value": "two"}}))
		if v, _ := cl.Get(ctx, "rowsmith:test:s").Result(); v != "two" {
			t.Fatalf("string = %q", v)
		}
		if d, _ := cl.TTL(ctx, "rowsmith:test:s").Result(); d <= 0 {
			t.Fatal("editing the string dropped its expiry")
		}

		// Hashes: insert, change, rename and delete fields.
		cl.HSet(ctx, "rowsmith:test:h", "a", "1", "b", "2", "c", "3")
		ok(apply("rowsmith:test:h",
			driver.RowEdit{Op: "insert", Values: map[string]any{"field": "d", "value": float64(4)}},
			driver.RowEdit{Op: "update", Key: map[string]any{"field": "a"}, Values: map[string]any{"value": "uno"}},
			driver.RowEdit{Op: "update", Key: map[string]any{"field": "b"}, Values: map[string]any{"field": "bee"}},
			driver.RowEdit{Op: "delete", Key: map[string]any{"field": "c"}}))
		if m, _ := cl.HGetAll(ctx, "rowsmith:test:h").Result(); fmt.Sprint(m) != "map[a:uno bee:2 d:4]" {
			t.Fatalf("hash = %v", m)
		}
		if err := apply("rowsmith:test:h", driver.RowEdit{Op: "delete", Key: map[string]any{"field": "zzz"}},
			driver.RowEdit{Op: "delete", Key: map[string]any{"field": "a"}}); err == nil {
			t.Fatal("deleting a missing field succeeded")
		}
		if n, _ := cl.HLen(ctx, "rowsmith:test:h").Result(); n != 3 {
			t.Fatal("a failed save changed the hash")
		}
		if err := apply("rowsmith:test:h", driver.RowEdit{Op: "insert", Values: map[string]any{"field": "a", "value": "x"}}); err == nil {
			t.Fatal("inserting an existing field succeeded")
		}

		// Lists: set, delete (several at once) and append.
		cl.RPush(ctx, "rowsmith:test:l", "a", "b", "c", "d", "b")
		ok(apply("rowsmith:test:l",
			driver.RowEdit{Op: "update", Key: map[string]any{"index": float64(0)}, Values: map[string]any{"value": "A"}},
			driver.RowEdit{Op: "delete", Key: map[string]any{"index": float64(1)}},
			driver.RowEdit{Op: "delete", Key: map[string]any{"index": float64(3)}},
			driver.RowEdit{Op: "insert", Values: map[string]any{"value": "e"}}))
		if l, _ := cl.LRange(ctx, "rowsmith:test:l", 0, -1).Result(); strings.Join(l, ",") != "A,c,b,e" {
			t.Fatalf("list = %v", l)
		}
		if err := apply("rowsmith:test:l", driver.RowEdit{Op: "delete", Key: map[string]any{"index": float64(9)}}); err == nil {
			t.Fatal("deleting past the end succeeded")
		}

		// Sets and sorted sets.
		cl.SAdd(ctx, "rowsmith:test:set", "x", "y")
		ok(apply("rowsmith:test:set",
			driver.RowEdit{Op: "insert", Values: map[string]any{"member": "z"}},
			driver.RowEdit{Op: "update", Key: map[string]any{"member": "x"}, Values: map[string]any{"member": "ex"}},
			driver.RowEdit{Op: "delete", Key: map[string]any{"member": "y"}}))
		if m, _ := cl.SMembers(ctx, "rowsmith:test:set").Result(); len(m) != 2 {
			t.Fatalf("set = %v", m)
		}
		cl.ZAdd(ctx, "rowsmith:test:z", zm("a", 1), zm("b", 2))
		ok(apply("rowsmith:test:z",
			driver.RowEdit{Op: "insert", Values: map[string]any{"member": "c", "score": "3.5"}},
			driver.RowEdit{Op: "update", Key: map[string]any{"member": "a"}, Values: map[string]any{"score": float64(10)}},
			driver.RowEdit{Op: "update", Key: map[string]any{"member": "b"}, Values: map[string]any{"member": "bee"}}))
		if z, _ := cl.ZRangeWithScores(ctx, "rowsmith:test:z", 0, -1).Result(); fmt.Sprint(z) != "[{2 bee} {3.5 c} {10 a}]" {
			t.Fatalf("zset = %v", z)
		}
		if err := apply("rowsmith:test:z", driver.RowEdit{Op: "update", Key: map[string]any{"member": "a"}, Values: map[string]any{"score": "lots"}}); err == nil {
			t.Fatal("a non-numeric score was saved")
		}

		// Streams: add entries (fields as an object or JSON text) and delete them.
		id, _ := cl.XAdd(ctx, xa("rowsmith:test:x", "k", "v")).Result()
		ok(apply("rowsmith:test:x",
			driver.RowEdit{Op: "insert", Values: map[string]any{"fields": map[string]any{"status": "paid", "total": float64(12.5)}}},
			driver.RowEdit{Op: "insert", Values: map[string]any{"id": "", "fields": `{"note": "hi"}`}},
			driver.RowEdit{Op: "delete", Key: map[string]any{"id": id}}))
		if n, _ := cl.XLen(ctx, "rowsmith:test:x").Result(); n != 2 {
			t.Fatalf("stream length = %d", n)
		}
		if err := apply("rowsmith:test:x", driver.RowEdit{Op: "update", Key: map[string]any{"id": id}, Values: map[string]any{"fields": "{}"}}); err == nil {
			t.Fatal("a stream entry was changed")
		}

		// JSON documents (Redis 8, Redis Stack, valkey-bundle) edit as one value.
		if err := cl.Do(ctx, "JSON.SET", "rowsmith:test:j", "$", `{"a":1,"tags":["x"]}`).Err(); err == nil {
			res := browse(t, c, driver.BrowseRequest{Ref: ref("rowsmith:test:j"), Limit: 10})
			if b, _ := json.Marshal(res.Rows[0][0]); string(b) != `{"a":1,"tags":["x"]}` {
				t.Fatalf("json = %s", b)
			}
			ok(apply("rowsmith:test:j", driver.RowEdit{Op: "update", Values: map[string]any{"value": map[string]any{"a": float64(2)}}}))
			if v, _ := cl.Do(ctx, "JSON.GET", "rowsmith:test:j").Text(); v != `{"a":2}` {
				t.Fatalf("json after edit = %s", v)
			}
			if tb, _ := c.Describe(ctx, ref("rowsmith:test:j")); tb.Kind != kindJSON || !tb.Editable {
				t.Fatalf("describe json = %+v", tb)
			}
		} else {
			t.Logf("no JSON module: %v", err)
		}

		// Binary values arrive as {"$bin": base64}.
		ok(apply("rowsmith:test:h", driver.RowEdit{Op: "insert", Values: map[string]any{"field": "bin", "value": map[string]any{"$bin": "AP8B"}}}))
		if v, _ := cl.HGet(ctx, "rowsmith:test:h", "bin").Result(); v != "\x00\xff\x01" {
			t.Fatalf("binary = %q", v)
		}
		res := browse(t, c, driver.BrowseRequest{Ref: ref("rowsmith:test:h"), Limit: 10})
		if m, ok := res.Rows[2][1].(map[string]any); res.Rows[2][0] != "bin" || !ok || m["$bin"] != "AP8B" {
			t.Fatalf("binary cell = %v", res.Rows[2])
		}

		// A read-only connection refuses edits.
		ro := &conn{opt: c.opt, ro: true, clients: map[int]*goredis.Client{}}
		defer ro.Close()
		if _, err := ro.ApplyEdits(ctx, ref("rowsmith:test:h"), []driver.RowEdit{{Op: "delete", Key: map[string]any{"field": "a"}}}); err == nil {
			t.Fatal("a read-only connection saved an edit")
		}
	})
}

func TestIntegrationConsole(t *testing.T) {
	each(t, func(t *testing.T, c *conn) {
		cleanup(t, c)
		sess, err := c.NewSession(context.Background(), driver.Scope{Database: "db2"})
		if err != nil {
			t.Fatal(err)
		}
		defer sess.Close()

		sink := mustRun(t, sess, `SET rowsmith:test:greeting "hello world"
# a comment
GET rowsmith:test:greeting
HSET rowsmith:test:h name 'Ada L' n 1
HGETALL rowsmith:test:h
ZADD rowsmith:test:z 1 a 2 b
ZRANGE rowsmith:test:z 0 -1 WITHSCORES
RPUSH rowsmith:test:l x y z
LRANGE rowsmith:test:l 1 -1`)
		if len(sink.stmts) != 8 || sink.stmts[2].Line != 4 || sink.stmts[1].Kind != driver.StmtRead || sink.stmts[0].Kind != driver.StmtWrite {
			t.Fatalf("statements = %+v", sink.stmts)
		}
		if sink.rows[1][0][0] != "hello world" {
			t.Fatalf("GET = %v", sink.rows[1])
		}
		if sink.cols[3][0].Name != "field" || fmt.Sprint(sink.rows[3]) != "[[name Ada L] [n 1]]" {
			t.Fatalf("HGETALL = %v %v", sink.cols[3], sink.rows[3])
		}
		if sink.cols[5][1].Name != "score" || sink.rows[5][1][1] != float64(2) {
			t.Fatalf("ZRANGE = %v", sink.rows[5])
		}
		if fmt.Sprint(sink.rows[7]) != "[[1 y] [2 z]]" {
			t.Fatalf("LRANGE = %v", sink.rows[7])
		}

		// SELECT and transactions carry between runs.
		mustRun(t, sess, "SELECT 3")
		if s := mustRun(t, sess, "GET rowsmith:test:greeting"); s.rows[0][0][0] != nil {
			t.Fatalf("SELECT 3 did not stick: %v", s.rows)
		}
		mustRun(t, sess, "SELECT 2\nMULTI\nINCR rowsmith:test:n\nINCR rowsmith:test:n")
		if !sess.InTransaction() {
			t.Fatal("MULTI did not open a transaction")
		}
		s := mustRun(t, sess, "EXEC")
		if sess.InTransaction() || len(s.rows[0]) != 2 || s.rows[0][1][0] != "INCR rowsmith:test:n" || s.rows[0][1][1] != int64(2) {
			t.Fatalf("EXEC = %v", s.rows)
		}

		// SCAN reports its cursor.
		s = mustRun(t, sess, "SCAN 0 MATCH rowsmith:test:* COUNT 1000")
		if len(s.rows[0]) < 4 || len(s.notices) != 1 {
			t.Fatalf("SCAN = %v %v", s.rows, s.notices)
		}
		s = mustRun(t, sess, "INFO server")
		if len(s.rows[0]) < 5 || s.cols[0][0].Name != "section" {
			t.Fatalf("INFO = %v", s.cols)
		}

		// Errors, blocked and unsupported commands.
		s = run(t, sess, "HGET rowsmith:test:l x\nSUBSCRIBE news\nPING", driver.ExecOptions{})
		if s.errs[0] == nil || !strings.Contains(s.errs[0].Error(), "WRONGTYPE") || s.errs[1] == nil || s.errs[2] != nil {
			t.Fatalf("errors = %v", s.errs)
		}
		s = run(t, sess, "GET a\nDEL rowsmith:test:greeting", driver.ExecOptions{ReadOnly: true})
		if s.errs[0] != nil || s.errs[1] == nil {
			t.Fatalf("read-only = %v", s.errs)
		}
		s = run(t, sess, `SET a "unclosed`, driver.ExecOptions{})
		var qe *driver.QueryError
		if len(s.errs) != 1 || s.errs[0] == nil || !asQE(s.errs[0], &qe) || qe.Line != 1 {
			t.Fatalf("syntax error = %v", s.errs)
		}

		// Cancelling a blocking command closes the connection; the next run reconnects to the same database.
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		started := time.Now()
		bs := &recSink{}
		sess.Execute(ctx, "BLPOP rowsmith:test:never 30", driver.ExecOptions{}, bs)
		if time.Since(started) > 5*time.Second || bs.errs[0] == nil {
			t.Fatalf("BLPOP was not cancelled: %v", bs.errs)
		}
		if s := mustRun(t, sess, "GET rowsmith:test:greeting"); s.rows[0][0][0] != "hello world" {
			t.Fatalf("after cancel: %v", s.rows)
		}

		// Exports read a whole key with the grid's columns.
		tb, _ := c.Describe(context.Background(), driver.ObjectRef{Database: "db2", Name: "rowsmith:test:h"})
		q, _, err := c.BrowseQuery(context.Background(), tb, driver.BrowseRequest{})
		if err != nil {
			t.Fatal(err)
		}
		if s := mustRun(t, sess, q); s.cols[0][0].Name != "field" || s.cols[0][1].Name != "value" {
			t.Fatalf("export query %q gave %v", q, s.cols)
		}

		// Renames refuse to overwrite; drops and renames run as console commands.
		if _, err := c.RenameObjectSQL(driver.ObjectRef{Database: "db2", Name: "rowsmith:test:h"}, "rowsmith:test:l"); err == nil {
			t.Fatal("rename over an existing key was allowed")
		}
		stmts, _ := c.RenameObjectSQL(driver.ObjectRef{Database: "db2", Name: "rowsmith:test:h"}, "rowsmith:test:h 2")
		mustRun(t, sess, stmts[0])
		stmts, _ = c.DropObjectSQL(driver.ObjectRef{Database: "db2", Name: "rowsmith:test:h 2"}, false)
		if s := mustRun(t, sess, stmts[0]); s.rows[0][0][0] != int64(1) {
			t.Fatalf("%s = %v", stmts[0], s.rows)
		}
	})
}

func TestIntegrationAdmin(t *testing.T) {
	each(t, func(t *testing.T, c *conn) {
		ctx := context.Background()
		p, err := c.Processes(ctx)
		if err != nil || len(p.Rows) == 0 || p.Columns[0].Name != "id" {
			t.Fatalf("processes = %v, %v", p, err)
		}
		v, err := c.Variables(ctx, "variables")
		if err != nil || len(v.Rows) < 50 {
			t.Fatalf("variables = %d rows, %v", len(v.Rows), err)
		}
		st, err := c.Variables(ctx, "status")
		if err != nil || len(st.Rows) < 50 {
			t.Fatalf("status = %d rows, %v", len(st.Rows), err)
		}
		if err := c.KillProcess(ctx, "999999999"); err == nil {
			t.Fatal("killing a missing client succeeded")
		}
	})
}

func TestIntegrationWrongPassword(t *testing.T) {
	for _, tg := range targets {
		t.Run(tg.id, func(t *testing.T) {
			host := os.Getenv(tg.prefix + "HOST")
			if host == "" {
				t.Skip()
			}
			d, _ := driver.Get(tg.id)
			_, err := d.Open(context.Background(), driver.OpenParams{Params: map[string]any{"host": host, "user": os.Getenv(tg.prefix + "USER")},
				Secrets: map[string]string{"password": "wrong"}})
			if err == nil || !strings.Contains(strings.ToUpper(err.Error()), "WRONGPASS") {
				t.Fatalf("wrong password: %v", err)
			}
		})
	}
}

func zm(m string, score float64) goredis.Z { return goredis.Z{Member: m, Score: score} }

func xa(key string, kv ...string) *goredis.XAddArgs {
	vals := make([]any, len(kv))
	for i, v := range kv {
		vals[i] = v
	}
	return &goredis.XAddArgs{Stream: key, Values: vals}
}

func asQE(err error, qe **driver.QueryError) bool { return errors.As(err, qe) }
