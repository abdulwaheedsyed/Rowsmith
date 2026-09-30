package mssql

import (
	"context"
	"database/sql"
	sqldriver "database/sql/driver"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/golang-sql/sqlexp"
	ms "github.com/microsoft/go-mssqldb"

	"rowsmith/internal/driver"
	"rowsmith/internal/driver/sqlbase"
	"rowsmith/internal/sqlsplit"
)

// session is a console bound to one server connection. Scripts run batch by
// batch (GO separates them); each batch may return several result sets,
// PRINT/RAISERROR messages and row counts, read through go-mssqldb's message
// queue so they reach the sink in server order.
type session struct {
	conn *sql.Conn
	live *liveness // nil when the connection could not be watched

	mu   sync.Mutex // serializes Execute
	inTx atomic.Bool
}

func (s *session) InTransaction() bool { return s.inTx.Load() }

// Close rolls back an open transaction and discards the connection.
func (s *session) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = s.conn.ExecContext(ctx, "IF @@TRANCOUNT > 0 ROLLBACK")
	discard(s.conn)
	return nil
}

func (s *session) Execute(ctx context.Context, script string, opts driver.ExecOptions, sink driver.Sink) error {
	if !s.mu.TryLock() {
		return errors.New("this console is still running a previous query")
	}
	defer s.mu.Unlock()

	for i, st := range sqlsplit.Split(script, sqlsplit.MSSQL) {
		if err := sink.BeginStatement(driver.StatementInfo{Index: i, SQL: st.SQL, Line: st.Line, Kind: st.Kind}); err != nil {
			return err
		}
		var err error
		if opts.ReadOnly && !st.Kind.Safe() {
			err = &driver.QueryError{Message: "Blocked: this connection is read-only for you, and the statement may modify data (" + string(st.Kind) + ")."}
		} else {
			err = s.runBatch(ctx, st, opts, sink)
		}
		if ctx.Err() != nil && err != nil {
			err = &driver.QueryError{Message: "Query cancelled"}
		}
		s.trackTx(ctx)
		if serr := sink.EndStatement(err); serr != nil {
			return serr
		}
		if err != nil && (opts.StopOnError || ctx.Err() != nil) {
			break
		}
	}
	return nil
}

func (s *session) trackTx(ctx context.Context) {
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	var n int
	if s.conn.QueryRowContext(pctx, "SELECT @@TRANCOUNT").Scan(&n) == nil {
		s.inTx.Store(n > 0)
	}
}

const (
	batchRows  = 500
	batchDelay = 80 * time.Millisecond
)

// runBatch executes one batch. The first server error becomes the batch
// error; later ones are reported as notices, since SQL Server keeps running
// the batch after most errors.
func (s *session) runBatch(ctx context.Context, st sqlsplit.Statement, opts driver.ExecOptions, sink driver.Sink) error {
	msgs := &sqlexp.ReturnMessage{}
	args := append(append([]any{}, opts.Params...), msgs)
	start := time.Now()
	rows, err := s.conn.QueryContext(ctx, st.SQL, args...)
	if err != nil {
		return batchError(err, st)
	}
	defer rows.Close()
	// Stop waiting for messages once the connection is gone: the queue gets
	// nothing more, and NextResultSet then surfaces the network error.
	mctx, stop := context.WithCancel(ctx)
	defer stop()
	if s.live != nil {
		go func() {
			select {
			case <-s.live.dead:
				stop()
			case <-mctx.Done():
			}
		}()
	}

	var first error
	var affected int64
	counted := false   // affected holds a count not yet reported
	results := 0       // summaries sent
	afterRows := false // the next count belongs to the result set just read
	flush := func() error {
		if !counted {
			return nil
		}
		n := affected
		affected, counted = 0, false
		results++
		return sink.EndResult(driver.ResultSummary{RowsAffected: &n, DurationMS: elapsed(start)})
	}
	for active := true; active; {
		switch m := msgs.Message(mctx).(type) {
		case sqlexp.MsgNotice:
			if err := sink.Notice("info", m.Message.String()); err != nil {
				return err
			}
		case sqlexp.MsgError:
			if !errors.As(m.Error, new(ms.Error)) {
				stop() // the driver lost the stream; no further messages follow
			}
			if first == nil {
				first = batchError(m.Error, st)
			} else if err := sink.Notice("error", noticeText(batchError(m.Error, st))); err != nil {
				return err
			}
		case sqlexp.MsgRowsAffected:
			if afterRows {
				afterRows = false
				continue
			}
			affected += m.Count
			counted = true
		case sqlexp.MsgNext:
			if err := flush(); err != nil {
				return err
			}
			if err := streamResult(rows, opts.MaxRows, start, sink); err != nil {
				return err
			}
			results++
			afterRows = true
			start = time.Now()
		case sqlexp.MsgNextResultSet:
			afterRows = false
			active = rows.NextResultSet()
		}
	}
	if err := rows.Err(); err != nil && first == nil {
		first = batchError(err, st)
	}
	if first == nil {
		first = ctx.Err()
	}
	if err := flush(); err != nil {
		return err
	}
	if results == 0 {
		if err := sink.EndResult(driver.ResultSummary{DurationMS: elapsed(start)}); err != nil {
			return err
		}
	}
	return first
}

// streamResult sends the current result set in batches. Rows beyond maxRows
// are read and counted but not sent, so the rest of the batch still runs.
func streamResult(rows *sql.Rows, maxRows int, start time.Time, sink driver.Sink) error {
	cts, err := rows.ColumnTypes()
	if err != nil {
		return err
	}
	d := dialect{}
	cols, kinds := sqlbase.Columns(d, cts)
	if err := sink.Columns(cols); err != nil {
		return err
	}
	var batch [][]any
	var count int64
	last := time.Now()
	for rows.Next() {
		count++
		if maxRows > 0 && count > int64(maxRows) {
			continue
		}
		r, err := sqlbase.ScanRow(d, rows, cts, kinds)
		if err != nil {
			return err
		}
		batch = append(batch, r)
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
	return sink.EndResult(driver.ResultSummary{RowCount: count, Truncated: maxRows > 0 && count > int64(maxRows), DurationMS: elapsed(start)})
}

// batchError maps a server error to a QueryError whose line refers to the
// script (errors raised inside a procedure keep the procedure's own line).
func batchError(err error, st sqlsplit.Statement) error {
	var me ms.Error
	if !errors.As(err, &me) {
		if errors.Is(err, sqldriver.ErrBadConn) {
			return &driver.QueryError{Message: "The connection to the server was lost; close this console to reconnect."}
		}
		return err
	}
	qe := &driver.QueryError{Message: me.Message, Code: fmt.Sprint(me.Number), Detail: fmt.Sprintf("Level %d, state %d", me.Class, me.State)}
	switch {
	case me.ProcName != "":
		qe.Detail += fmt.Sprintf(", procedure %s, line %d", me.ProcName, me.LineNo)
	case me.LineNo > 0:
		qe.Line = st.Line + int(me.LineNo) - 1
	}
	return qe
}

// noticeText formats an additional error of a batch for the message list.
func noticeText(err error) string {
	var qe *driver.QueryError
	switch {
	case !errors.As(err, &qe):
		return err.Error()
	case qe.Line > 0:
		return fmt.Sprintf("Msg %s, line %d: %s", qe.Code, qe.Line, qe.Message)
	}
	return fmt.Sprintf("Msg %s: %s", qe.Code, qe.Message)
}

func elapsed(start time.Time) float64 { return float64(time.Since(start).Microseconds()) / 1000 }
