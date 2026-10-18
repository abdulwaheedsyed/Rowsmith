package clickhouse

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"sync"

	ch "github.com/ClickHouse/clickhouse-go/v2"

	"rowsmith/internal/driver"
	"rowsmith/internal/driver/sqlbase"
	"rowsmith/internal/sqlsplit"
)

// session is a console: one server connection, so SET and USE carry from
// one run to the next. Over HTTP, which has no connection state, a server
// session id does that.
type session struct {
	inner    *sqlbase.Session
	settings ch.Settings // for every query: the HTTP session
	db       *sql.DB
}

func (c *conn) NewSession(ctx context.Context, s driver.Scope) (driver.Session, error) {
	sc, err := c.db.Conn(ctx)
	if err != nil {
		return nil, mapError(err, sqlsplit.Statement{})
	}
	sess := &session{db: c.db, settings: ch.Settings{}}
	if c.http {
		sess.settings["session_id"], sess.settings["session_timeout"] = sessionID(), 3600
	}
	if s.Database == "" {
		s.Database = c.defaultDB
	}
	if s.Database != "" {
		if _, err := sc.ExecContext(sess.context(ctx, nil), "USE "+quote(s.Database)); err != nil {
			sc.Close()
			return nil, mapError(err, sqlsplit.Statement{})
		}
	}
	sess.inner = sqlbase.NewSession(sc, dialect{}, sqlbase.SessionHooks{
		MapError:    mapError,
		ReturnsRows: returnsRows,
		Context:     sess.statementContext,
	})
	return sess, nil
}

func sessionID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return "rowsmith-" + hex.EncodeToString(b)
}

// stateful gives HTTP queries a server session, so that USE in one request
// holds for the next; the native protocol keeps it on the connection.
func (c *conn) stateful(ctx context.Context) context.Context {
	if !c.http {
		return ctx
	}
	return ch.Context(ctx, ch.WithSettings(ch.Settings{"session_id": sessionID(), "session_timeout": 60}))
}

// context adds the session's settings and progress reporting to a query's context.
func (s *session) context(ctx context.Context, p *progress) context.Context {
	opts := []ch.QueryOption{ch.WithSettings(s.with(nil))}
	if p != nil {
		opts = append(opts, ch.WithProgress(p.add))
	}
	return ch.Context(ctx, opts...)
}

// with copies the session's settings and adds extra.
func (s *session) with(extra ch.Settings) ch.Settings {
	out := ch.Settings{}
	for k, v := range s.settings {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

// statementContext caps what the server sends for a statement with rows at
// the console's limit (plus one, to tell it was cut off), so a huge result
// isn't read to its end. The limit setting applies only to the final result,
// not to subqueries.
func (s *session) statementContext(ctx context.Context, st sqlsplit.Statement, opts driver.ExecOptions) context.Context {
	if opts.MaxRows <= 0 || !returnsRows(st) {
		return ctx
	}
	return ch.Context(ctx, ch.WithSettings(s.with(ch.Settings{"limit": opts.MaxRows + 1})))
}

func (s *session) Execute(ctx context.Context, script string, opts driver.ExecOptions, sink driver.Sink) error {
	p := &progress{}
	return s.inner.Execute(s.context(ctx, p), script, opts, &progressSink{Sink: sink, p: p})
}

func (s *session) InTransaction() bool { return false }
func (s *session) Close() error        { return s.inner.Close() }

// returnsRows: the statements with a result read it; everything else runs
// as a command, whose result ClickHouse doesn't count.
func returnsRows(st sqlsplit.Statement) bool {
	switch firstKeyword(st.SQL) {
	case "SELECT", "WITH", "SHOW", "DESCRIBE", "DESC", "EXPLAIN", "EXISTS", "CHECK", "WATCH", "VALUES", "(":
		return true
	}
	return false
}

func firstKeyword(s string) string {
	toks := sqlsplit.Words(s, sqlsplit.ClickHouse, 1)
	if len(toks) == 0 {
		return ""
	}
	return toks[0]
}

// progress adds up what the server reports reading for a statement.
type progress struct {
	mu          sync.Mutex
	rows, bytes uint64
	seen        bool
}

func (p *progress) add(pr *ch.Progress) {
	p.mu.Lock()
	p.rows += pr.Rows
	p.bytes += pr.Bytes
	p.seen = true
	p.mu.Unlock()
}

func (p *progress) take() (rows, bytes uint64, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	rows, bytes, ok = p.rows, p.bytes, p.seen
	p.rows, p.bytes, p.seen = 0, 0, false
	return
}

// progressSink adds what each statement read to its result summary.
type progressSink struct {
	driver.Sink
	p *progress
}

func (s *progressSink) BeginStatement(info driver.StatementInfo) error {
	s.p.take()
	return s.Sink.BeginStatement(info)
}

func (s *progressSink) EndResult(sum driver.ResultSummary) error {
	if rows, bytes, ok := s.p.take(); ok && (rows > 0 || bytes > 0) {
		r, b := int64(rows), int64(bytes)
		sum.RowsRead, sum.BytesProcessed = &r, &b
	}
	return s.Sink.EndResult(sum)
}

// FullValues passes through what an export sink asks for.
func (s *progressSink) FullValues() bool { return driver.WantsFullValues(s.Sink) }
