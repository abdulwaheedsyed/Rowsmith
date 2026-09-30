package sqlbase

import (
	"context"
	"database/sql"
	sqldriver "database/sql/driver"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"rowsmith/internal/driver"
	"rowsmith/internal/sqlsplit"
)

// SessionHooks let engines customize console execution.
type SessionHooks struct {
	// Prepare rewrites a statement right before it runs (e.g. to enforce a
	// row cap server-side). Returning "" keeps the original.
	Prepare func(ctx context.Context, c *sql.Conn, st sqlsplit.Statement, opts driver.ExecOptions) (string, error)
	// After runs after each statement, e.g. to forward server notices.
	After func(ctx context.Context, c *sql.Conn, sink driver.Sink)
	// InTx asks the server whether a transaction is open.
	InTx func(ctx context.Context, c *sql.Conn) (bool, error)
	// MapError converts driver errors into *driver.QueryError.
	MapError func(err error, st sqlsplit.Statement) error
	// ReturnsRows overrides the Query-vs-Exec decision.
	ReturnsRows func(st sqlsplit.Statement) bool
	// Close runs before the connection is returned (e.g. ROLLBACK).
	Close func(c *sql.Conn)
}

type Session struct {
	Conn  *sql.Conn
	D     Dialect
	Hooks SessionHooks

	mu   sync.Mutex // serializes Execute
	inTx atomic.Bool
}

func NewSession(c *sql.Conn, d Dialect, h SessionHooks) *Session {
	return &Session{Conn: c, D: d, Hooks: h}
}

func (s *Session) InTransaction() bool { return s.inTx.Load() }

// Close discards the underlying connection instead of returning it to the
// pool, so open transactions, temp tables and session settings never leak
// into browse queries or another user's console.
func (s *Session) Close() error {
	if s.Hooks.Close != nil {
		s.Hooks.Close(s.Conn)
	}
	_ = s.Conn.Raw(func(any) error { return sqldriver.ErrBadConn })
	return s.Conn.Close()
}

// Execute runs a script statement by statement, streaming results to sink.
func (s *Session) Execute(ctx context.Context, script string, opts driver.ExecOptions, sink driver.Sink) error {
	if !s.mu.TryLock() {
		return fmt.Errorf("this console is still running a previous query")
	}
	defer s.mu.Unlock()

	stmts := sqlsplit.Split(script, s.D.Split())
	for i, st := range stmts {
		info := driver.StatementInfo{Index: i, SQL: st.SQL, Line: st.Line, Kind: st.Kind}
		if err := sink.BeginStatement(info); err != nil {
			return err
		}
		var err error
		if opts.ReadOnly && !st.Kind.Safe() {
			err = &driver.QueryError{Message: "Blocked: this connection is read-only for you, and the statement may modify data (" + string(st.Kind) + ")."}
		} else {
			err = s.runOne(ctx, st, opts, sink)
			if s.Hooks.After != nil {
				s.Hooks.After(ctx, s.Conn, sink)
			}
		}
		if err != nil && s.Hooks.MapError != nil {
			err = s.Hooks.MapError(err, st)
		}
		if ctx.Err() != nil && err != nil {
			err = &driver.QueryError{Message: "Query cancelled"}
		}
		s.trackTx(ctx, st)
		if serr := sink.EndStatement(err); serr != nil {
			return serr
		}
		if err != nil && (opts.StopOnError || ctx.Err() != nil) {
			break
		}
	}
	return nil
}

func (s *Session) trackTx(ctx context.Context, st sqlsplit.Statement) {
	if s.Hooks.InTx != nil {
		pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer cancel()
		if in, err := s.Hooks.InTx(pctx, s.Conn); err == nil {
			s.inTx.Store(in)
			return
		}
	}
	first := strings.ToUpper(firstWord(st.SQL))
	switch {
	case first == "BEGIN" || first == "START":
		s.inTx.Store(true)
	case first == "COMMIT" || first == "ROLLBACK" || first == "END" || first == "ABORT":
		s.inTx.Store(false)
	case st.Kind == driver.StmtDDL && s.D.Split() == sqlsplit.MySQL:
		s.inTx.Store(false) // implicit commit
	}
}

func firstWord(sql string) string {
	f := strings.FieldsFunc(sql, func(r rune) bool { return !(r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') })
	if len(f) == 0 {
		return ""
	}
	return f[0]
}

func (s *Session) returnsRows(st sqlsplit.Statement) bool {
	if s.Hooks.ReturnsRows != nil {
		return s.Hooks.ReturnsRows(st)
	}
	return DefaultReturnsRows(st)
}

// DefaultReturnsRows decides whether to run a statement with Query (rows) or
// Exec (rows affected).
func DefaultReturnsRows(st sqlsplit.Statement) bool {
	switch st.Kind {
	case driver.StmtRead, driver.StmtUnknown:
		return true
	case driver.StmtWrite:
		u := strings.ToUpper(st.SQL)
		if strings.Contains(u, "RETURNING") || strings.Contains(u, "OUTPUT") {
			return true
		}
		switch strings.ToUpper(firstWord(st.SQL)) {
		case "CALL", "EXEC", "EXECUTE", "DO":
			return true
		}
	}
	return false
}

const (
	batchRows  = 500
	batchDelay = 80 * time.Millisecond
)

func (s *Session) runOne(ctx context.Context, st sqlsplit.Statement, opts driver.ExecOptions, sink driver.Sink) error {
	query := st.SQL
	if s.Hooks.Prepare != nil {
		q, err := s.Hooks.Prepare(ctx, s.Conn, st, opts)
		if err != nil {
			return err
		}
		if q != "" {
			query = q
		}
	}
	start := time.Now()
	if !s.returnsRows(st) {
		res, err := s.Conn.ExecContext(ctx, query, opts.Params...)
		if err != nil {
			return err
		}
		sum := driver.ResultSummary{DurationMS: ms(start)}
		if n, err := res.RowsAffected(); err == nil {
			sum.RowsAffected = &n
		}
		return sink.EndResult(sum)
	}

	rows, err := s.Conn.QueryContext(ctx, query, opts.Params...)
	if err != nil {
		return err
	}
	defer rows.Close()
	return StreamRows(ctx, s.D, rows, opts.MaxRows, start, sink)
}

// StreamRows forwards every result set of rows to sink in batches.
func StreamRows(ctx context.Context, d Dialect, rows *sql.Rows, maxRows int, start time.Time, sink driver.Sink) error {
	sawResult := false
	full := driver.WantsFullValues(sink)
	for {
		cts, err := rows.ColumnTypes()
		if err != nil {
			return err
		}
		if len(cts) > 0 {
			sawResult = true
			cols, kinds := Columns(d, cts)
			if err := sink.Columns(cols); err != nil {
				return err
			}
			var batch [][]any
			var count int64
			var size int
			truncated, clipped := false, false
			last := time.Now()
			for rows.Next() {
				if maxRows > 0 && count >= int64(maxRows) {
					truncated = true
					break
				}
				r, err := ScanRow(d, rows, cts, kinds)
				if err != nil {
					return err
				}
				if !full {
					driver.PreviewRow(r)
					if size += driver.RowSize(r); size > driver.StreamBudget {
						truncated, clipped = true, true // keep the console responsive with huge values
					}
				}
				batch = append(batch, r)
				count++
				if clipped {
					break
				}
				if len(batch) >= batchRows || time.Since(last) > batchDelay {
					if err := sink.Rows(batch); err != nil {
						return err
					}
					batch, last = nil, time.Now()
				}
			}
			if len(batch) > 0 {
				if err := sink.Rows(batch); err != nil {
					return err
				}
			}
			if !truncated {
				if err := rows.Err(); err != nil {
					return err
				}
			}
			if err := sink.EndResult(driver.ResultSummary{RowCount: count, Truncated: truncated, Clipped: clipped, DurationMS: ms(start)}); err != nil {
				return err
			}
			if truncated {
				return nil
			}
			start = time.Now()
		}
		if !rows.NextResultSet() {
			break
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if !sawResult {
		return sink.EndResult(driver.ResultSummary{DurationMS: ms(start)})
	}
	return nil
}

func ms(start time.Time) float64 { return float64(time.Since(start).Microseconds()) / 1000 }
