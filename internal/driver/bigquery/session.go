package bigquery

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/bigquery"
	"google.golang.org/api/iterator"

	"rowsmith/internal/driver"
	"rowsmith/internal/sqlsplit"
)

const (
	batchRows  = 500
	batchDelay = 80 * time.Millisecond
)

// session is a console. BigQuery keeps no server connection to pin: every
// run is one query job, so the session only carries the default dataset.
type session struct {
	c       *conn
	project string
	dataset string
	mu      sync.Mutex // serializes Execute
}

func (c *conn) NewSession(ctx context.Context, s driver.Scope) (driver.Session, error) {
	sess := &session{c: c}
	if s.Schema != "" || c.dataset != "" {
		p, d, err := c.datasetOf(s.Schema)
		if err != nil {
			return nil, err
		}
		sess.project, sess.dataset = p, d
	}
	return sess, nil
}

func cancelled() error { return &driver.QueryError{Message: "Query cancelled"} }

func (s *session) InTransaction() bool { return false }
func (s *session) Close() error        { return nil }

// query builds the job for a console script.
func (s *session) query(script string, opts driver.ExecOptions) *bigquery.Query {
	q := s.c.newQuery(script, "")
	q.DefaultProjectID, q.DefaultDatasetID = s.project, s.dataset
	if opts.MaxBytesBilled > 0 {
		q.MaxBytesBilled = opts.MaxBytesBilled
	}
	for _, p := range opts.Params {
		q.Parameters = append(q.Parameters, bigquery.QueryParameter{Value: p})
	}
	return q
}

// Execute runs a script as one BigQuery job. A dry run first reports the
// estimated bytes and enforces the cost guard and read-only mode; then the
// job runs and its results stream to sink. For multi-statement scripts the
// script itself is reported first, followed by one statement per child job
// that returned rows or changed data.
func (s *session) Execute(ctx context.Context, script string, opts driver.ExecOptions, sink driver.Sink) error {
	if !s.mu.TryLock() {
		return fmt.Errorf("this console is still running a previous query")
	}
	defer s.mu.Unlock()

	stmts := sqlsplit.Split(script, sqlsplit.BigQuery)
	if len(stmts) == 0 {
		return nil
	}
	x := &execution{s: s, opts: opts, sink: sink, stmts: stmts, script: script}
	x.opts.ReadOnly = opts.ReadOnly || s.c.ro
	info := driver.StatementInfo{Index: 0, SQL: stmts[0].SQL, Line: stmts[0].Line, Kind: stmts[0].Kind}
	if len(stmts) > 1 {
		info.SQL, info.Kind = strings.TrimSpace(script), scriptKind(stmts)
	}
	if err := sink.BeginStatement(info); err != nil {
		return err
	}
	err := x.run(ctx)
	var se sinkError
	if errors.As(err, &se) {
		return se.err
	}
	if ctx.Err() != nil && err != nil {
		err = cancelled()
	}
	if err := sink.EndStatement(mapError(err)); err != nil {
		return err
	}
	for i, child := range x.children {
		if err := x.emitChild(ctx, i+1, child); err != nil {
			return err
		}
	}
	return nil
}

// sinkError marks failures to write to the client, which abort execution.
type sinkError struct{ err error }

func (e sinkError) Error() string { return e.err.Error() }

func wrapSink(err error) error {
	if err != nil {
		return sinkError{err}
	}
	return nil
}

type execution struct {
	s        *session
	opts     driver.ExecOptions
	sink     driver.Sink
	stmts    []sqlsplit.Statement
	script   string
	children []*bigquery.Job // child jobs with results, reported after the script
}

func (x *execution) multi() bool { return len(x.stmts) > 1 }

func (x *execution) noun() string {
	if x.multi() {
		return "script"
	}
	return "query"
}

// run executes the script. Results of a single statement (or the final
// result of a script whose child jobs cannot be listed) go to the current
// statement; child jobs are collected for Execute to report afterwards.
func (x *execution) run(ctx context.Context) error {
	c := x.s.c
	if x.opts.ReadOnly {
		for _, st := range x.stmts {
			if !st.Kind.Safe() {
				return &driver.QueryError{Message: "Blocked: this connection is read-only for you, and the statement may modify data (" + string(st.Kind) + ").", Line: st.Line}
			}
		}
	}
	limit := c.maxBytes
	if x.opts.MaxBytesBilled > 0 {
		limit = x.opts.MaxBytesBilled
	}

	est, err := c.dryRun(ctx, x.s.query(x.script, x.opts))
	switch {
	case err != nil && ctx.Err() != nil:
		return cancelled()
	case err != nil && dryRunUnavailable(err):
		if x.opts.ReadOnly {
			return &driver.QueryError{Message: "Blocked: this connection is read-only for you, and the statement type could not be verified because the dry run failed: " + mapError(err).Error()}
		}
		if err := x.sink.Notice("warning", "The cost estimate is unavailable ("+mapError(err).Error()+"); running with the bytes-billed limit only."); err != nil {
			return sinkError{err}
		}
	case err != nil:
		return err
	default:
		if err := x.sink.Notice("info", estimateText(x.noun(), est)); err != nil {
			return sinkError{err}
		}
		if x.opts.ReadOnly && est.StatementType != "SELECT" {
			return &driver.QueryError{Message: "Blocked: this connection is read-only for you, and BigQuery reports a " +
				strings.ReplaceAll(est.StatementType, "_", " ") + " statement; only SELECT queries can run."}
		}
		if limit > 0 && est.TotalBytesProcessed > limit {
			return refusal(est.TotalBytesProcessed, limit)
		}
		if x.opts.DryRun {
			n := est.TotalBytesProcessed
			return wrapSink(x.sink.EndResult(driver.ResultSummary{BytesProcessed: &n}))
		}
	}
	if x.opts.DryRun {
		return nil
	}

	start := time.Now()
	q := x.s.query(x.script, x.opts)
	q.MaxBytesBilled = limit
	job, st, err := c.runJob(ctx, q)
	if err != nil {
		return err
	}
	qs := queryStats(st)
	if x.multi() || qs.StatementType == "SCRIPT" || st.Statistics != nil && st.Statistics.NumChildJobs > 0 {
		if kids, ok := c.childJobs(ctx, job); ok {
			x.children = kids
			if err := x.billing(qs); err != nil {
				return err
			}
			return wrapSink(x.sink.EndResult(summary(qs, ms(start))))
		}
		if x.multi() {
			if err := x.sink.Notice("info", "Per-statement results are unavailable; showing the result of the last statement."); err != nil {
				return sinkError{err}
			}
		}
	}
	if err := x.emitResult(ctx, job, qs, func() float64 { return ms(start) }); err != nil {
		return err
	}
	return x.billing(qs)
}

// estimateText phrases a dry-run estimate like the BigQuery console does.
func estimateText(noun string, qs *bigquery.QueryStatistics) string {
	qual := ""
	switch qs.TotalBytesProcessedAccuracy {
	case "LOWER_BOUND":
		qual = "at least "
	case "UPPER_BOUND":
		qual = "at most "
	}
	return "This " + noun + " will process " + qual + formatBytes(qs.TotalBytesProcessed) + " when run."
}

// billing reports what the finished job cost.
func (x *execution) billing(qs *bigquery.QueryStatistics) error {
	var msg string
	switch {
	case qs.CacheHit:
		msg = "Results were served from cache; nothing was billed."
	case qs.TotalBytesBilled > 0:
		msg = "Billed " + formatBytes(qs.TotalBytesBilled) + " (processed " + formatBytes(qs.TotalBytesProcessed) + ")."
	default:
		return nil
	}
	return wrapSink(x.sink.Notice("info", msg))
}

func summary(qs *bigquery.QueryStatistics, durationMS float64) driver.ResultSummary {
	bytes, hit := qs.TotalBytesProcessed, qs.CacheHit
	return driver.ResultSummary{DurationMS: durationMS, BytesProcessed: &bytes, CacheHit: &hit}
}

func ms(start time.Time) float64 { return float64(time.Since(start).Microseconds()) / 1000 }

// emitResult reports one finished job: rows affected for DML, a notice for
// DDL, otherwise its result set. elapsed yields the duration to report.
func (x *execution) emitResult(ctx context.Context, job *bigquery.Job, qs *bigquery.QueryStatistics, elapsed func() float64) error {
	sum := summary(qs, 0)
	switch {
	case isDML(qs.StatementType) || qs.DMLStats != nil:
		n := qs.NumDMLAffectedRows
		sum.RowsAffected, sum.DurationMS = &n, elapsed()
		return wrapSink(x.sink.EndResult(sum))
	case statementKind(qs.StatementType) == driver.StmtDDL:
		if t := qs.DDLTargetTable; qs.DDLOperationPerformed != "" && t != nil {
			if err := x.sink.Notice("info", qs.DDLOperationPerformed+" "+t.DatasetID+"."+t.TableID); err != nil {
				return sinkError{err}
			}
		}
		sum.DurationMS = elapsed()
		return wrapSink(x.sink.EndResult(sum))
	}
	it, err := job.Read(ctx)
	if err != nil {
		return err
	}
	if len(it.Schema) > 0 {
		if err := x.sink.Columns(resultColumns(it.Schema)); err != nil {
			return sinkError{err}
		}
		if sum.RowCount, sum.Truncated, err = x.stream(it); err != nil {
			return err
		}
	}
	sum.DurationMS = elapsed()
	return wrapSink(x.sink.EndResult(sum))
}

// stream forwards rows in batches, stopping after MaxRows.
func (x *execution) stream(it *bigquery.RowIterator) (int64, bool, error) {
	max := x.opts.MaxRows
	it.PageInfo().MaxSize = pageSize(max)
	var batch [][]any
	var count int64
	truncated := false
	last := time.Now()
	for {
		var row []bigquery.Value
		err := it.Next(&row)
		if err == iterator.Done {
			break
		}
		if err != nil {
			return count, false, err
		}
		if max > 0 && count >= int64(max) {
			truncated = true
			break
		}
		batch = append(batch, encodeRow(row, it.Schema))
		count++
		if len(batch) >= batchRows || time.Since(last) > batchDelay {
			if err := x.sink.Rows(batch); err != nil {
				return count, false, sinkError{err}
			}
			batch, last = nil, time.Now()
		}
	}
	if len(batch) > 0 {
		if err := x.sink.Rows(batch); err != nil {
			return count, false, sinkError{err}
		}
	}
	return count, truncated, nil
}

// emitChild reports one child job of a script as its own statement.
func (x *execution) emitChild(ctx context.Context, index int, job *bigquery.Job) error {
	st := job.LastStatus()
	qs := queryStats(st)
	info := driver.StatementInfo{Index: index, Kind: statementKind(qs.StatementType)}
	if ss := st.Statistics.ScriptStatistics; ss != nil && len(ss.StackFrames) > 0 {
		// The first frame is the statement itself; the last is its position in the script.
		info.SQL = strings.TrimSpace(ss.StackFrames[0].Text)
		info.Line = int(ss.StackFrames[len(ss.StackFrames)-1].StartLine)
	}
	if info.SQL == "" {
		if cfg, err := job.Config(); err == nil {
			if qc, ok := cfg.(*bigquery.QueryConfig); ok {
				info.SQL = qc.Q
			}
		}
	}
	if info.Kind == driver.StmtUnknown && info.SQL != "" {
		info.Kind, _ = sqlsplit.Classify(info.SQL, sqlsplit.BigQuery)
	}
	if err := x.sink.BeginStatement(info); err != nil {
		return err
	}
	// Report the child's own run time, not the time since it started.
	took := float64(st.Statistics.EndTime.Sub(st.Statistics.StartTime).Microseconds()) / 1000
	err := x.emitResult(ctx, job, qs, func() float64 { return max(took, 0) })
	var se sinkError
	if errors.As(err, &se) {
		return se.err
	}
	if ctx.Err() != nil && err != nil {
		err = cancelled()
	}
	return x.sink.EndStatement(mapError(err))
}

// childJobs lists the statements a script ran that returned rows or changed
// data, oldest first. ok is false when the server cannot list child jobs
// (emulators return unrelated jobs), so callers fall back to the final result.
func (c *conn) childJobs(ctx context.Context, parent *bigquery.Job) ([]*bigquery.Job, bool) {
	it := parent.Children(ctx)
	var out []*bigquery.Job
	for n := 0; n < 1000; n++ {
		j, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, false
		}
		st := j.LastStatus()
		if st == nil || st.Statistics == nil || st.Statistics.ParentJobID != parent.ID() {
			return nil, false
		}
		if st.Err() != nil {
			continue
		}
		qs := queryStats(st)
		if qs.StatementType == "SELECT" || isDML(qs.StatementType) || qs.DMLStats != nil {
			out = append(out, j)
		}
	}
	sort.SliceStable(out, func(a, b int) bool {
		return out[a].LastStatus().Statistics.CreationTime.Before(out[b].LastStatus().Statistics.CreationTime)
	})
	return out, true
}

// scriptKind is the most consequential kind among a script's statements.
func scriptKind(stmts []sqlsplit.Statement) driver.StatementKind {
	rank := map[driver.StatementKind]int{driver.StmtDDL: 6, driver.StmtDCL: 5, driver.StmtWrite: 4, driver.StmtUnknown: 3,
		driver.StmtSession: 2, driver.StmtTCL: 1, driver.StmtRead: 0}
	k := driver.StmtRead
	for _, st := range stmts {
		if rank[st.Kind] > rank[k] {
			k = st.Kind
		}
	}
	return k
}
