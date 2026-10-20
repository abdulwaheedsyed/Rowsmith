package redis

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"rowsmith/internal/driver"
)

// session is a console: one server connection of its own, so SELECT,
// MULTI … EXEC and WATCH carry from one run to the next.
type session struct {
	c      *conn
	mu     sync.Mutex
	db     atomic.Int64 // the database SELECT last chose
	cl     goredis.UniversalClient
	inTx   bool
	queued []string // commands queued since MULTI, to label EXEC's replies
}

func (c *conn) NewSession(_ context.Context, s driver.Scope) (driver.Session, error) {
	n, err := c.dbOf(s.Database)
	if err != nil {
		return nil, err
	}
	ss := &session{c: c}
	ss.db.Store(int64(n))
	return ss, nil
}

// client opens the session's connection on first use, and again after a
// cancelled command closed it.
func (s *session) client() goredis.UniversalClient {
	if s.cl != nil {
		return s.cl
	}
	if s.c.cluster != nil {
		// Commands go to the node that owns their key; nothing carries
		// between them, so the console refuses SELECT and transactions.
		opt := clusterOptions(s.c.opt)
		opt.PoolSize, opt.MaxRetries, opt.ReadTimeout, opt.ConnMaxIdleTime = 2, -1, -1, -1
		s.cl = goredis.NewClusterClient(opt)
		return s.cl
	}
	opt := s.c.opt
	opt.DB = int(s.db.Load())
	opt.PoolSize, opt.MinIdleConns = 1, 0
	opt.MaxRetries = -1      // never repeat a command that may have run
	opt.ReadTimeout = -1     // blocking commands wait as long as asked; cancelling closes the connection
	opt.ConnMaxIdleTime = -1 // keep the connection, and with it any transaction
	opt.OnConnect = func(ctx context.Context, cn *goredis.Conn) error {
		if db := int(s.db.Load()); db != 0 {
			return cn.Select(ctx, db).Err()
		}
		return nil
	}
	s.cl = goredis.NewClient(&opt)
	return s.cl
}

func (s *session) InTransaction() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inTx
}

func (s *session) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cl != nil {
		s.cl.Close()
		s.cl = nil
	}
	return nil
}

func (s *session) Execute(ctx context.Context, script string, opts driver.ExecOptions, sink driver.Sink) error {
	if !s.mu.TryLock() {
		return errors.New("this console is still running a previous command")
	}
	defer s.mu.Unlock()
	cmds, err := parseScript(script)
	if err != nil {
		if e := sink.BeginStatement(driver.StatementInfo{SQL: strings.TrimSpace(script), Line: 1, Kind: driver.StmtUnknown}); e != nil {
			return e
		}
		qe := &driver.QueryError{Message: err.Error()}
		var se *SyntaxError
		if errors.As(err, &se) {
			qe = &driver.QueryError{Message: se.Msg, Line: se.Line}
		}
		return sink.EndStatement(qe)
	}
	for i, cmd := range cmds {
		k := cmd.kind()
		if err := sink.BeginStatement(driver.StatementInfo{Index: i, SQL: cmd.text, Line: cmd.line, Kind: k}); err != nil {
			return err
		}
		var runErr error
		if what, ok := unsupported[cmd.name()]; ok {
			runErr = &driver.QueryError{Message: "The console cannot run " + what + "; use redis-cli for it."}
		} else if (opts.ReadOnly || s.c.ro) && !k.Safe() {
			runErr = &driver.QueryError{Message: "Blocked: this connection is read-only for you, and the command may modify data (" + string(k) + ")."}
		} else {
			runErr = s.run(ctx, cmd, opts, sink)
		}
		if runErr != nil {
			var qe *driver.QueryError
			if !errors.As(runErr, &qe) {
				runErr = &driver.QueryError{Message: runErr.Error()}
			}
		}
		if ctx.Err() != nil && runErr != nil {
			runErr = &driver.QueryError{Message: "Cancelled"}
		}
		if err := sink.EndStatement(runErr); err != nil {
			return err
		}
		if runErr != nil && (opts.StopOnError || ctx.Err() != nil) {
			break
		}
	}
	return nil
}

func (s *session) run(ctx context.Context, cmd command, opts driver.ExecOptions, sink driver.Sink) error {
	start := time.Now()
	cl := s.client()
	cc, _ := cl.(*goredis.ClusterClient)
	if cc != nil {
		if msg := clusterRefusal(cmd); msg != "" {
			return &driver.QueryError{Message: msg}
		}
	}
	args := make([]any, len(cmd.args))
	for i, a := range cmd.args {
		args[i] = a
	}
	type answer struct {
		v   any
		err error
	}
	done := make(chan answer, 1)
	go func() {
		bg := context.WithoutCancel(ctx)
		if cc != nil {
			if v, err, ok := clusterWide(bg, cc, cmd); ok {
				done <- answer{v, err}
				return
			}
		}
		v, err := cl.Do(bg, args...).Result()
		done <- answer{v, err}
	}()
	var a answer
	select {
	case a = <-done:
	case <-ctx.Done():
		// Closing the connection is the only way to stop a command in flight;
		// it ends any transaction too.
		cl.Close()
		s.cl, s.inTx, s.queued = nil, false, nil
		<-done
		return ctx.Err()
	}
	name := cmd.name()
	if a.err != nil && !errors.Is(a.err, goredis.Nil) {
		if s.inTx && (name == "EXEC" || strings.HasPrefix(a.err.Error(), "EXECABORT")) {
			s.inTx, s.queued = false, nil
		}
		return mapError(a.err)
	}
	switch name {
	case "SELECT":
		if n, err := strconv.Atoi(cmd.args[1]); err == nil {
			s.db.Store(int64(n))
		}
	case "MULTI":
		s.inTx, s.queued = true, nil
	case "DISCARD":
		s.inTx, s.queued = false, nil
	case "EXEC":
		labels := s.queued
		s.inTx, s.queued = false, nil
		return emit(sink, execResult(labels, a.v), opts, start)
	default:
		if s.inTx {
			s.queued = append(s.queued, cmd.text)
		}
	}
	rs := render(cmd, a.v)
	if cc != nil && nodeLocal(name) && rs.notice == "" {
		rs.notice = "On a cluster this ran on one node, which answers for itself only."
	}
	return emit(sink, rs, opts, start)
}

// clusterRefusal explains the console commands a cluster cannot run from
// here: the console has no connection of its own to carry state between
// commands, and each node holds only its own keys.
func clusterRefusal(cmd command) string {
	switch cmd.name() {
	case "SELECT":
		if len(cmd.args) > 1 && cmd.args[1] == "0" {
			return ""
		}
		return "A Redis Cluster has only database 0."
	case "MULTI", "EXEC", "DISCARD", "WATCH", "UNWATCH":
		return "The console runs each command on the node that owns its key, so it cannot hold a transaction on a cluster. " +
			"Grid edits are still checked and applied atomically, key by key."
	case "SCAN":
		return "On a cluster SCAN walks one node; use KEYS, which runs on every primary, or the navigator's key search, which scans them all."
	}
	return ""
}

// clusterWide runs the commands that concern every key on every primary,
// combining their replies.
func clusterWide(ctx context.Context, cc *goredis.ClusterClient, cmd command) (any, error, bool) {
	switch cmd.name() {
	case "DBSIZE":
		n, err := cc.DBSize(ctx).Result()
		return n, err, true
	case "KEYS":
		if len(cmd.args) != 2 {
			return nil, nil, false
		}
		var mu sync.Mutex
		var keys []string
		err := cc.ForEachMaster(ctx, func(ctx context.Context, node *goredis.Client) error {
			found, err := node.Keys(ctx, cmd.args[1]).Result()
			mu.Lock()
			keys = append(keys, found...)
			mu.Unlock()
			return err
		})
		sort.Strings(keys)
		out := make([]any, len(keys))
		for i, k := range keys {
			out[i] = k
		}
		return out, err, true
	case "FLUSHDB", "FLUSHALL":
		args := make([]any, len(cmd.args))
		for i, a := range cmd.args {
			args[i] = a
		}
		err := cc.ForEachMaster(ctx, func(ctx context.Context, node *goredis.Client) error {
			return node.Do(ctx, args...).Err()
		})
		return "OK", err, true
	}
	return nil, nil, false
}

// nodeLocal says whether a command reports on the node it runs on rather
// than on keys.
func nodeLocal(name string) bool {
	switch name {
	case "INFO", "RANDOMKEY", "LASTSAVE", "TIME", "ROLE":
		return true
	}
	for _, p := range []string{"CONFIG ", "CLIENT ", "SLOWLOG ", "MEMORY STATS", "LATENCY ", "ACL ", "COMMAND"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// resultSet is one table the console shows.
type resultSet struct {
	cols   []driver.ResultColumn
	rows   [][]any
	notice string
}

func emit(sink driver.Sink, rs resultSet, opts driver.ExecOptions, start time.Time) error {
	if rs.notice != "" {
		if err := sink.Notice("info", rs.notice); err != nil {
			return err
		}
	}
	return driver.EmitRows(sink, rs.cols, rs.rows, opts.MaxRows, start)
}

func rc(name string, kind driver.ValueKind) driver.ResultColumn {
	return driver.ResultColumn{Name: name, Kind: kind, Type: string(kind)}
}

// render shapes a reply into a table, following what the command returns:
// HGETALL gives field and value columns, ZRANGE … WITHSCORES member and
// score, XRANGE id and fields, INFO its sections, and so on.
func render(cmd command, v any) resultSet {
	name := cmd.name()
	has := func(word string) bool {
		for _, a := range cmd.args[1:] {
			if strings.EqualFold(a, word) {
				return true
			}
		}
		return false
	}
	switch x := v.(type) {
	case string:
		switch name {
		case "INFO":
			return infoTable(x)
		case "CLIENT LIST", "CLIENT INFO":
			return clientTable(x)
		}
	case []any:
		switch name {
		case "HGETALL":
			return pairs(x, "field", "value", false)
		case "HRANDFIELD":
			if has("WITHVALUES") {
				return pairs(x, "field", "value", false)
			}
			return column(x, "field")
		case "CONFIG GET":
			return pairs(x, "name", "value", false)
		case "ZRANGE", "ZREVRANGE", "ZRANGEBYSCORE", "ZREVRANGEBYSCORE", "ZRANDMEMBER", "ZUNION", "ZINTER", "ZDIFF":
			if has("WITHSCORES") {
				return pairs(x, "member", "score", true)
			}
			return column(x, "member")
		case "ZPOPMIN", "ZPOPMAX":
			return pairs(x, "member", "score", true)
		case "LRANGE":
			from, _ := strconv.ParseInt(cmd.args[min(2, len(cmd.args)-1)], 10, 64)
			if from < 0 {
				from = 0
			}
			rs := resultSet{cols: []driver.ResultColumn{rc("index", driver.KindInt), rc("value", driver.KindText)}}
			for i, e := range x {
				rs.rows = append(rs.rows, []any{from + int64(i), cell(e)})
			}
			return rs
		case "SMEMBERS", "SINTER", "SUNION", "SDIFF", "SRANDMEMBER", "SPOP":
			return column(x, "member")
		case "KEYS":
			return column(x, "key")
		case "SCAN", "HSCAN", "SSCAN", "ZSCAN":
			if len(x) == 2 {
				items, _ := x[1].([]any)
				var rs resultSet
				switch name {
				case "SCAN":
					rs = column(items, "key")
				case "HSCAN":
					rs = pairs(items, "field", "value", false)
				case "SSCAN":
					rs = column(items, "member")
				case "ZSCAN":
					rs = pairs(items, "member", "score", true)
				}
				if c, _ := x[0].(string); c == "0" {
					rs.notice = "The scan is complete (cursor 0)."
				} else {
					rs.notice = "Continue the scan from cursor " + c + "."
				}
				return rs
			}
		case "XRANGE", "XREVRANGE":
			rs := resultSet{cols: []driver.ResultColumn{rc("id", driver.KindString), rc("fields", driver.KindObject)}}
			for _, e := range x {
				if row, ok := streamEntry(e); ok {
					rs.rows = append(rs.rows, []any{row[0], encodeValue(row[1])})
				}
			}
			return rs
		case "XREAD", "XREADGROUP":
			rs := resultSet{cols: []driver.ResultColumn{rc("stream", driver.KindText), rc("id", driver.KindString), rc("fields", driver.KindObject)}}
			for _, s := range x {
				pair, ok := s.([]any)
				if !ok || len(pair) != 2 {
					continue
				}
				entries, _ := pair[1].([]any)
				for _, e := range entries {
					if row, ok := streamEntry(e); ok {
						rs.rows = append(rs.rows, []any{cell(pair[0]), row[0], encodeValue(row[1])})
					}
				}
			}
			return rs
		}
		return column(x, "value")
	}
	// A single reply: a status, a number, a string or nil.
	kind := driver.KindText
	if _, ok := v.(int64); ok {
		kind = driver.KindInt
	}
	return resultSet{cols: []driver.ResultColumn{rc("result", kind)}, rows: [][]any{{cell(v)}}}
}

// cell encodes one reply element; nested arrays become array cells.
func cell(v any) any {
	switch x := v.(type) {
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = cell(e)
		}
		return out
	case error:
		return "(error) " + x.Error()
	}
	return encodeValue(v)
}

func column(x []any, name string) resultSet {
	kind := driver.KindText
	for _, e := range x {
		switch e.(type) {
		case []any:
			kind = driver.KindArray
		case int64:
			if kind == driver.KindText {
				kind = driver.KindInt
			}
		}
	}
	rs := resultSet{cols: []driver.ResultColumn{rc(name, kind)}, rows: make([][]any, 0, len(x))}
	for _, e := range x {
		rs.rows = append(rs.rows, []any{cell(e)})
	}
	return rs
}

func pairs(x []any, a, b string, score bool) resultSet {
	kb := driver.KindText
	if score {
		kb = driver.KindFloat
	}
	rs := resultSet{cols: []driver.ResultColumn{rc(a, driver.KindText), rc(b, kb)}, rows: make([][]any, 0, len(x)/2)}
	for i := 0; i+1 < len(x); i += 2 {
		second := cell(x[i+1])
		if score {
			if f, err := strconv.ParseFloat(strings.TrimSpace(textOf(x[i+1])), 64); err == nil {
				second = driver.EncodeFloat(f)
			}
		}
		rs.rows = append(rs.rows, []any{cell(x[i]), second})
	}
	return rs
}

func textOf(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return text(v)
}

// execResult pairs each queued command with its reply.
func execResult(labels []string, v any) resultSet {
	replies, _ := v.([]any)
	rs := resultSet{cols: []driver.ResultColumn{rc("command", driver.KindText), rc("reply", driver.KindOther)}}
	if v == nil {
		rs.notice = "The transaction was not run: a watched key changed."
		return rs
	}
	for i, r := range replies {
		label := ""
		if i < len(labels) {
			label = labels[i]
		}
		rs.rows = append(rs.rows, []any{label, cell(r)})
	}
	return rs
}

func infoTable(s string) resultSet {
	rs := resultSet{cols: []driver.ResultColumn{rc("section", driver.KindText), rc("name", driver.KindText), rc("value", driver.KindText)}}
	section := ""
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			section = strings.TrimSpace(strings.TrimPrefix(line, "#"))
			continue
		}
		if k, v, ok := strings.Cut(line, ":"); ok {
			rs.rows = append(rs.rows, []any{section, k, v})
		}
	}
	return rs
}

// clientTable reads CLIENT LIST lines of name=value pairs.
func clientTable(s string) resultSet {
	var rs resultSet
	idx := map[string]int{}
	for _, line := range strings.Split(strings.TrimSpace(s), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		row := make([]any, len(rs.cols))
		for _, part := range strings.Fields(line) {
			k, v, _ := strings.Cut(part, "=")
			i, ok := idx[k]
			if !ok {
				i = len(rs.cols)
				idx[k] = i
				kind := driver.KindText
				if k == "id" || k == "age" || k == "idle" || k == "db" {
					kind = driver.KindInt
				}
				rs.cols = append(rs.cols, rc(k, kind))
				row = append(row, nil)
			}
			if n, err := strconv.ParseInt(v, 10, 64); err == nil && rs.cols[i].Kind == driver.KindInt {
				row[i] = n
			} else {
				row[i] = v
			}
		}
		rs.rows = append(rs.rows, row)
	}
	for i, r := range rs.rows {
		for len(r) < len(rs.cols) {
			r = append(r, nil)
		}
		rs.rows[i] = r
	}
	return rs
}
