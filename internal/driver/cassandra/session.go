package cassandra

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	gocql "github.com/apache/cassandra-gocql-driver/v2"

	"rowsmith/internal/driver"
	"rowsmith/internal/sqlsplit"
)

// session is a console. CQL has no transactions; what carries between runs
// is cqlsh's client-side state: the keyspace (USE), the consistency levels
// and tracing.
type session struct {
	c       *conn
	mu      sync.Mutex
	ks      string
	cons    gocql.Consistency
	serial  gocql.Consistency
	tracing bool
}

func (c *conn) NewSession(_ context.Context, s driver.Scope) (driver.Session, error) {
	return &session{c: c, ks: c.ksOf(s.Database), cons: c.cons, serial: gocql.LocalSerial}, nil
}

func (s *session) InTransaction() bool { return false }
func (s *session) Close() error        { return nil }

// cqlshOnly are cqlsh commands that need a terminal or local files.
var cqlshOnly = map[string]string{
	"COPY":   "COPY reads and writes files on your computer; use Import and Export instead",
	"SOURCE": "SOURCE reads a file on your computer; paste its statements here instead", "CAPTURE": "CAPTURE writes to a file on your computer",
	"LOGIN": "LOGIN changes the signed-in user; edit the connection instead", "EXIT": "", "QUIT": "", "CLEAR": "", "CLS": "",
	"HELP": "HELP is cqlsh's built-in manual; see the CQL reference instead", "SHOW": "SHOW is a cqlsh command; SELECT from system.local instead",
}

func (s *session) Execute(ctx context.Context, script string, opts driver.ExecOptions, sink driver.Sink) error {
	if !s.mu.TryLock() {
		return errors.New("this console is still running a previous statement")
	}
	defer s.mu.Unlock()
	for i, st := range sqlsplit.Split(script, sqlsplit.CQL) {
		if err := sink.BeginStatement(driver.StatementInfo{Index: i, SQL: st.SQL, Line: st.Line, Kind: st.Kind}); err != nil {
			return err
		}
		var runErr error
		if (opts.ReadOnly || s.c.ro) && !st.Kind.Safe() {
			runErr = &driver.QueryError{Message: "Blocked: this connection is read-only for you, and the statement may modify data (" + string(st.Kind) + ")."}
		} else {
			runErr = s.run(ctx, st, opts, sink)
		}
		if runErr != nil {
			var qe *driver.QueryError
			if !errors.As(runErr, &qe) {
				runErr = &driver.QueryError{Message: runErr.Error()}
			}
			if qe != nil {
				// Lines count from the statement; without one, point at it.
				qe.Line = max(qe.Line, 1) + st.Line - 1
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

func (s *session) run(ctx context.Context, st sqlsplit.Statement, opts driver.ExecOptions, sink driver.Sink) error {
	start := time.Now()
	text := strings.TrimSpace(st.SQL)
	words := strings.Fields(strings.ToUpper(text))
	if len(words) == 0 {
		return nil
	}
	switch first := words[0]; first {
	case "USE":
		ks := strings.TrimSpace(text[3:])
		if strings.HasPrefix(ks, `"`) && strings.HasSuffix(ks, `"`) && len(ks) > 1 {
			ks = strings.ReplaceAll(ks[1:len(ks)-1], `""`, `"`)
		} else {
			ks = strings.ToLower(ks)
		}
		var n int
		if err := s.c.query(ctx, "SELECT count(*) FROM system_schema.keyspaces WHERE keyspace_name = ?", ks).Scan(&n); err != nil {
			return mapError(err)
		}
		if n == 0 {
			return &driver.QueryError{Message: fmt.Sprintf("Keyspace %q does not exist", ks)}
		}
		s.ks = ks
		return sink.Notice("info", "Now using keyspace "+ks+".")
	case "CONSISTENCY", "SERIAL":
		serial := first == "SERIAL"
		args := words[1:]
		if serial {
			if len(args) == 0 || args[0] != "CONSISTENCY" {
				return &driver.QueryError{Message: "Write SERIAL CONSISTENCY [SERIAL | LOCAL_SERIAL]."}
			}
			args = args[1:]
		}
		cur := &s.cons
		if serial {
			cur = &s.serial
		}
		if len(args) == 0 {
			return sink.Notice("info", "Current "+map[bool]string{true: "serial consistency", false: "consistency"}[serial]+" level is "+cur.String()+".")
		}
		c, err := parseConsistency(args[0])
		if err != nil {
			return &driver.QueryError{Message: err.Error()}
		}
		if serial && c != gocql.Serial && c != gocql.LocalSerial {
			return &driver.QueryError{Message: "Serial consistency is SERIAL or LOCAL_SERIAL."}
		}
		*cur = c
		return sink.Notice("info", map[bool]string{true: "Serial consistency", false: "Consistency"}[serial]+" level set to "+c.String()+".")
	case "TRACING":
		if len(words) > 1 {
			s.tracing = words[1] == "ON"
		}
		return sink.Notice("info", "Tracing is "+map[bool]string{true: "on: each statement's trace follows its result.", false: "off."}[s.tracing])
	case "PAGING", "EXPAND":
		return sink.Notice("info", "Rowsmith pages results and shows them as a table already, so "+first+" isn't needed.")
	default:
		if why, ok := cqlshOnly[first]; ok {
			if why == "" {
				why = first + " is a cqlsh command"
			}
			return &driver.QueryError{Message: why + "."}
		}
	}

	sess, err := s.c.session(s.ks)
	if err != nil {
		return err
	}
	q := sess.Query(text, opts.Params...).WithContext(ctx).Consistency(s.cons).SerialConsistency(s.serial)
	var trace *traceID
	if s.tracing {
		trace = &traceID{}
		q = q.Trace(trace)
	}
	pageSize := 1000
	if opts.MaxRows > 0 && opts.MaxRows < pageSize {
		pageSize = opts.MaxRows + 1
	}
	iter := q.PageSize(pageSize).Iter()
	cols := iter.Columns()
	if len(cols) > 0 {
		if err := streamRows(iter, cols, opts, sink, start); err != nil {
			iter.Close()
			return err
		}
	}
	for _, w := range iter.Warnings() {
		if err := sink.Notice("warning", w); err != nil {
			return err
		}
	}
	if err := iter.Close(); err != nil {
		return mapError(err)
	}
	if trace != nil && trace.id != nil {
		plan, err := s.c.readTrace(ctx, trace.id)
		if err == nil {
			return emitTrace(sink, plan)
		}
		return sink.Notice("warning", "The trace could not be read: "+err.Error())
	}
	return nil
}

func streamRows(iter *gocql.Iter, cols []gocql.ColumnInfo, opts driver.ExecOptions, sink driver.Sink, start time.Time) error {
	rd := newRowReader(cols)
	if err := sink.Columns(rd.columns()); err != nil {
		return err
	}
	full := driver.WantsFullValues(sink)
	sum := driver.ResultSummary{}
	batch := make([][]any, 0, 500)
	size := 0
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		err := sink.Rows(batch)
		batch = make([][]any, 0, 500)
		return err
	}
	for {
		row, ok := rd.next(iter)
		if !ok {
			break
		}
		if opts.MaxRows > 0 && sum.RowCount >= int64(opts.MaxRows) {
			// More rows exist; reading them all would scan the table.
			sum.Truncated = true
			break
		}
		if !full {
			driver.PreviewRow(row)
		}
		sum.RowCount++
		batch = append(batch, row)
		if size += driver.RowSize(row); size > driver.StreamBudget {
			sum.Truncated, sum.Clipped = true, true
			break
		}
		if len(batch) == cap(batch) {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	if err := flush(); err != nil {
		return err
	}
	sum.DurationMS = float64(time.Since(start).Microseconds()) / 1000
	return sink.EndResult(sum)
}

// ---- traces as plans ------------------------------------------------------------

type traceID struct{ id []byte }

func (t *traceID) Trace(id []byte) { t.id = id }

// readTrace reads a query's trace, which the nodes write a moment after it
// ends, as a plan: each node's steps, the coordinator's first.
func (c *conn) readTrace(ctx context.Context, id []byte) (*driver.Plan, error) {
	var coordinator net.IP
	var duration int
	var request string
	var err error
	for wait := 0; wait < 20; wait++ {
		err = c.s.Query("SELECT coordinator, duration, request FROM system_traces.sessions WHERE session_id = ?", id).
			WithContext(ctx).Consistency(gocql.One).Scan(&coordinator, &duration, &request)
		if err == nil && duration > 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		return nil, mapError(err)
	}
	root := &driver.PlanNode{Operation: request, Detail: "coordinator " + coordinator.String(), Props: map[string]string{}}
	ms := float64(duration) / 1000
	root.TimeMS = &ms
	nodes := map[string]*driver.PlanNode{}
	iter := c.s.Query("SELECT activity, source, source_elapsed, thread FROM system_traces.events WHERE session_id = ?", id).
		WithContext(ctx).Consistency(gocql.One).Iter()
	var activity, thread string
	var src net.IP
	var elapsed int
	var raw strings.Builder
	for iter.Scan(&activity, &src, &elapsed, &thread) {
		addr := src.String()
		n := nodes[addr]
		if n == nil {
			n = &driver.PlanNode{Operation: "Node " + addr, Props: map[string]string{}}
			if addr == coordinator.String() {
				n.Operation += " (coordinator)"
			}
			nodes[addr] = n
			root.Children = append(root.Children, n)
		}
		t := float64(elapsed) / 1000
		n.Children = append(n.Children, &driver.PlanNode{Operation: activity, Detail: thread, TimeMS: &t})
		n.TimeMS = &t
		fmt.Fprintf(&raw, "%8.3f ms  %-15s %s  [%s]\n", t, addr, activity, thread)
	}
	if err := iter.Close(); err != nil {
		return nil, mapError(err)
	}
	return &driver.Plan{Root: root, Raw: raw.String(), Format: "text",
		Totals: map[string]any{"Duration (ms)": ms, "Coordinator": coordinator.String(), "Nodes": len(nodes)}}, nil
}

func emitTrace(sink driver.Sink, plan *driver.Plan) error {
	if err := sink.Columns([]driver.ResultColumn{{Name: "elapsed_ms", Type: "double", Kind: driver.KindFloat},
		{Name: "node", Type: "text", Kind: driver.KindString}, {Name: "activity", Type: "text", Kind: driver.KindText},
		{Name: "thread", Type: "text", Kind: driver.KindString}}); err != nil {
		return err
	}
	var rows [][]any
	for _, n := range plan.Root.Children {
		node := strings.TrimSuffix(strings.TrimPrefix(n.Operation, "Node "), " (coordinator)")
		for _, ev := range n.Children {
			rows = append(rows, []any{*ev.TimeMS, node, ev.Operation, ev.Detail})
		}
	}
	if len(rows) > 0 {
		if err := sink.Rows(rows); err != nil {
			return err
		}
	}
	return sink.EndResult(driver.ResultSummary{RowCount: int64(len(rows))})
}

// Explain runs a SELECT with tracing (Cassandra has no EXPLAIN) and shows
// what each node did. It reads one page of rows.
func (c *conn) Explain(ctx context.Context, scope driver.Scope, stmt string, analyze bool) (*driver.Plan, error) {
	if k, _ := sqlsplit.Classify(stmt, sqlsplit.CQL); k != driver.StmtRead {
		return nil, errors.New("Cassandra has no EXPLAIN; Rowsmith traces a SELECT instead, and only reads can be traced here")
	}
	sess, err := c.session(c.ksOf(scope.Database))
	if err != nil {
		return nil, err
	}
	trace := &traceID{}
	iter := sess.Query(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(stmt), ";"))).WithContext(ctx).Consistency(c.cons).
		Trace(trace).PageSize(100).Iter()
	rows := iter.NumRows()
	if err := iter.Close(); err != nil {
		return nil, mapError(err)
	}
	if trace.id == nil {
		return nil, errors.New("the server returned no trace")
	}
	plan, err := c.readTrace(ctx, trace.id)
	if err != nil {
		return nil, err
	}
	plan.Totals["Rows (first page)"] = rows
	return plan, nil
}

// ---- administration ------------------------------------------------------------

func (c *conn) collect(ctx context.Context, stmt string) (*driver.Result, error) {
	iter := c.query(ctx, stmt).PageSize(1000).Iter()
	rd := newRowReader(iter.Columns())
	res := &driver.Result{Columns: rd.columns(), Rows: [][]any{}}
	for {
		row, ok := rd.next(iter)
		if !ok {
			break
		}
		res.Rows = append(res.Rows, row)
	}
	return res, mapError(iter.Close())
}

// Processes lists the client connections the node knows of (virtual tables:
// system_views.clients on Cassandra, system.clients on ScyllaDB).
func (c *conn) Processes(ctx context.Context) (*driver.Result, error) {
	if c.product == "ScyllaDB" {
		return c.collect(ctx, "SELECT address, port, client_type, connection_stage, driver_name, driver_version, protocol_version, username, shard_id, ssl_enabled FROM system.clients")
	}
	return c.collect(ctx, "SELECT address, port, hostname, username, connection_stage, driver_name, driver_version, protocol_version, request_count, ssl_enabled FROM system_views.clients")
}

func (c *conn) KillProcess(context.Context, string) error {
	return errors.New("CQL can't close another client's connection; use nodetool on the node")
}

// Variables shows the node's settings ("variables") or the cluster's nodes
// ("status").
func (c *conn) Variables(ctx context.Context, kind string) (*driver.Result, error) {
	if kind == "status" {
		res := &driver.Result{Columns: []driver.ResultColumn{
			{Name: "address", Type: "inet", Kind: driver.KindString}, {Name: "data_center", Type: "text", Kind: driver.KindString},
			{Name: "rack", Type: "text", Kind: driver.KindString}, {Name: "release_version", Type: "text", Kind: driver.KindString},
			{Name: "tokens", Type: "int", Kind: driver.KindInt}, {Name: "host_id", Type: "uuid", Kind: driver.KindUUID}}, Rows: [][]any{}}
		var addr net.IP
		var dc, rack, release string
		var tokens []string
		var host gocql.UUID
		if err := c.query(ctx, "SELECT broadcast_address, data_center, rack, release_version, tokens, host_id FROM system.local").
			Scan(&addr, &dc, &rack, &release, &tokens, &host); err != nil {
			return nil, mapError(err)
		}
		res.Rows = append(res.Rows, []any{addr.String() + " (this node)", dc, rack, release, int64(len(tokens)), host.String()})
		iter := c.query(ctx, "SELECT peer, data_center, rack, release_version, tokens, host_id FROM system.peers").Iter()
		for iter.Scan(&addr, &dc, &rack, &release, &tokens, &host) {
			res.Rows = append(res.Rows, []any{addr.String(), dc, rack, release, int64(len(tokens)), host.String()})
		}
		return res, mapError(iter.Close())
	}
	if c.product == "ScyllaDB" {
		return c.collect(ctx, "SELECT name, value, source, type FROM system.config")
	}
	return c.collect(ctx, "SELECT name, value FROM system_views.settings")
}
