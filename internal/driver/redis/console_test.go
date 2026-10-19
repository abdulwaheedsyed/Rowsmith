package redis

import (
	"reflect"
	"testing"
	"time"

	"rowsmith/internal/driver"
)

func TestParseScript(t *testing.T) {
	script := "SET greeting \"hello \\\"world\\\"\\n\"\n\n# comment\n  HSET user:1 name 'Ada L' tag \"\\x00\\xff\"\n// another\nSET doc \"line one\nline two\"\nPING"
	cmds, err := parseScript(script)
	if err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"SET", "greeting", "hello \"world\"\n"},
		{"HSET", "user:1", "name", "Ada L", "tag", "\x00\xff"},
		{"SET", "doc", "line one\nline two"},
		{"PING"},
	}
	if len(cmds) != len(want) {
		t.Fatalf("got %d commands: %+v", len(cmds), cmds)
	}
	for i, c := range cmds {
		if !reflect.DeepEqual(c.args, want[i]) {
			t.Errorf("command %d = %q, want %q", i, c.args, want[i])
		}
	}
	if cmds[1].line != 4 || cmds[2].line != 6 || cmds[3].line != 8 {
		t.Errorf("lines = %d %d %d", cmds[1].line, cmds[2].line, cmds[3].line)
	}
	if cmds[1].text != `HSET user:1 name 'Ada L' tag "\x00\xff"` || script[cmds[1].start:cmds[1].end] != "HSET user:1 name 'Ada L' tag \"\\x00\\xff\"" {
		t.Errorf("text = %q", cmds[1].text)
	}

	for _, bad := range []string{`GET "open`, "GET 'open", `GET "a"b`} {
		if _, err := parseScript(bad); err == nil {
			t.Errorf("%s parsed", bad)
		}
	}
	if _, err := parseScript("PING\nGET \"open"); err == nil || err.(*SyntaxError).Line != 2 {
		t.Errorf("error line: %v", err)
	}
}

func TestQuoteArg(t *testing.T) {
	for _, s := range []string{"plain", "", "with space", `say "hi"`, "it's", "tab\there", "new\nline", "\x00\xff\x01", "naïve café", "#hash", "//x", `back\slash`} {
		q := quoteArg(s)
		cmds, err := parseScript("GET " + q)
		if err != nil {
			t.Fatalf("%q quoted as %s: %v", s, q, err)
		}
		if len(cmds) != 1 || len(cmds[0].args) != 2 || cmds[0].args[1] != s {
			t.Errorf("%q quoted as %s read back as %q", s, q, cmds[0].args)
		}
	}
	if quoteArg("user:1") != "user:1" || quoteArg("naïve") != `"naïve"` {
		t.Errorf("quoting is not minimal: %s %s", quoteArg("user:1"), quoteArg("naïve"))
	}
}

func TestClassify(t *testing.T) {
	cases := map[string]struct {
		kind   driver.StatementKind
		danger string
	}{
		"GET a":                    {driver.StmtRead, ""},
		"hgetall a":                {driver.StmtRead, ""},
		"CONFIG GET maxmemory":     {driver.StmtRead, ""},
		"config set maxmemory 1gb": {driver.StmtDDL, "caution"},
		"SET a b":                  {driver.StmtWrite, "caution"},
		"DEL a b":                  {driver.StmtWrite, "caution"},
		"FLUSHDB":                  {driver.StmtDDL, "destructive"},
		"flushall async":           {driver.StmtDDL, "destructive"},
		"SELECT 2":                 {driver.StmtSession, ""},
		"MULTI":                    {driver.StmtTCL, ""},
		"ACL SETUSER bob on":       {driver.StmtDCL, "caution"},
		"ACL WHOAMI":               {driver.StmtRead, ""},
		"CLIENT KILL ID 4":         {driver.StmtDDL, "caution"},
		"CLIENT LIST":              {driver.StmtRead, ""},
		"KEYS *":                   {driver.StmtRead, "caution"},
		"EVAL \"return 1\" 0":      {driver.StmtWrite, "caution"},
		"MADEUP x":                 {driver.StmtWrite, "caution"},
	}
	for src, want := range cases {
		stmts, err := redisDriver{}.SplitScript(src)
		if err != nil || len(stmts) != 1 {
			t.Fatalf("%s: %v %v", src, stmts, err)
		}
		if stmts[0].Kind != want.kind || stmts[0].Danger.Level != want.danger {
			t.Errorf("%s = %s/%q, want %s/%q", src, stmts[0].Kind, stmts[0].Danger.Level, want.kind, want.danger)
		}
	}
}

func TestRender(t *testing.T) {
	cmd := func(s string) command {
		c, _ := parseScript(s)
		return c[0]
	}
	rs := render(cmd("HGETALL h"), []any{"a", "1", "b", "2"})
	if rs.cols[0].Name != "field" || len(rs.rows) != 2 {
		t.Fatalf("HGETALL = %+v", rs)
	}
	rs = render(cmd("ZRANGE z 0 -1 WITHSCORES"), []any{"a", "1.5"})
	if rs.rows[0][1] != 1.5 {
		t.Fatalf("WITHSCORES = %+v", rs)
	}
	rs = render(cmd("SCAN 0"), []any{"17", []any{"k1", "k2"}})
	if len(rs.rows) != 2 || rs.notice == "" {
		t.Fatalf("SCAN = %+v", rs)
	}
	rs = render(cmd("INCR n"), int64(3))
	if rs.cols[0].Kind != driver.KindInt || rs.rows[0][0] != int64(3) {
		t.Fatalf("INCR = %+v", rs)
	}
	rs = render(cmd("GET missing"), nil)
	if rs.rows[0][0] != nil {
		t.Fatalf("nil = %+v", rs)
	}
	rs = clientTable("id=3 addr=1.2.3.4:5 name= db=0\nid=4 addr=x name=app db=2 extra=1\n")
	if len(rs.cols) != 5 || rs.rows[0][0] != int64(3) || rs.rows[1][2] != "app" || len(rs.rows[0]) != 5 {
		t.Fatalf("CLIENT LIST = %+v", rs)
	}
}

func TestTTLText(t *testing.T) {
	for d, want := range map[time.Duration]string{-time.Millisecond: "never", 2592000 * time.Second: "in 30d 0h", 90 * time.Minute: "in 1h 30m",
		250 * time.Second: "in 4m 10s", 1400 * time.Millisecond: "in 1s"} {
		if got := ttlText(d); got != want {
			t.Errorf("%v = %q, want %q", d, got, want)
		}
	}
}
