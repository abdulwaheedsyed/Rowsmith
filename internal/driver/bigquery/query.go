package bigquery

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/bigquery"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"

	"rowsmith/internal/driver"
	"rowsmith/internal/id"
)

// pageRows caps rows requested per results page; the API also limits pages to ~10 MB.
const pageRows = 10000

// newQuery prepares a job with the connection defaults: a recognizable job
// ID (so a job whose insert was interrupted can still be cancelled), labels,
// location and the per-query cost guard.
func (c *conn) newQuery(sql, location string) *bigquery.Query {
	q := c.client.Query(sql)
	q.JobID = "rowsmith_" + id.New()
	q.Location = location
	if q.Location == "" {
		q.Location = c.location
	}
	q.Labels = c.labels
	q.MaxBytesBilled = c.maxBytes
	return q
}

// runJob starts q and waits for it to finish. Cancelling ctx cancels the job
// server-side, so an abandoned query stops consuming slots; the context's
// error is returned.
func (c *conn) runJob(ctx context.Context, q *bigquery.Query) (*bigquery.Job, *bigquery.JobStatus, error) {
	job, err := q.Run(ctx)
	if err != nil {
		if ctx.Err() != nil {
			c.cancelJob(q.JobID, q.Location)
			return nil, nil, ctx.Err()
		}
		return nil, nil, err
	}
	st, err := job.Wait(ctx)
	if err != nil {
		if ctx.Err() != nil {
			c.cancelJob(job.ID(), job.Location())
			return job, nil, ctx.Err()
		}
		return job, nil, err
	}
	return job, st, st.Err()
}

// cancelJob is best-effort and outlives the request that started the job.
func (c *conn) cancelJob(jobID, location string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	call := c.svc.Jobs.Cancel(c.project, jobID).Context(ctx)
	if location != "" {
		call = call.Location(location)
	}
	_, _ = call.Do()
}

// queryStats returns the query statistics of a job status, never nil.
func queryStats(st *bigquery.JobStatus) *bigquery.QueryStatistics {
	if st != nil && st.Statistics != nil {
		if qs, ok := st.Statistics.Details.(*bigquery.QueryStatistics); ok && qs != nil {
			return qs
		}
	}
	return &bigquery.QueryStatistics{}
}

// dryRun validates q and estimates its cost without running it.
func (c *conn) dryRun(ctx context.Context, q *bigquery.Query) (*bigquery.QueryStatistics, error) {
	q.DryRun = true
	job, err := q.Run(ctx)
	if err != nil {
		return nil, err
	}
	st := job.LastStatus()
	if st == nil {
		return nil, errors.New("the dry run returned no job status")
	}
	if err := st.Err(); err != nil {
		return nil, err
	}
	return queryStats(st), nil
}

// dryRunUnavailable reports whether a dry run failed for reasons unrelated
// to the query itself (server errors, emulators without dry-run support).
func dryRunUnavailable(err error) bool {
	var ge *googleapi.Error
	return errors.As(err, &ge) && ge.Code >= 500
}

// collect runs q and materializes up to max rows (0 = all).
func (c *conn) collect(ctx context.Context, q *bigquery.Query, max int) (*driver.Result, error) {
	start := time.Now()
	job, _, err := c.runJob(ctx, q)
	if err != nil {
		return nil, mapError(err)
	}
	it, err := job.Read(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	res, err := readRows(it, max)
	if err != nil {
		return nil, err
	}
	res.Duration = time.Since(start)
	res.DurationMS = float64(res.Duration.Microseconds()) / 1000
	return res, nil
}

// readRows drains an iterator into a Result, reading at most max rows
// (0 = all); Truncated reports that more rows exist.
func readRows(it *bigquery.RowIterator, max int) (*driver.Result, error) {
	it.PageInfo().MaxSize = pageSize(max)
	res := &driver.Result{Rows: [][]any{}}
	for {
		var row []bigquery.Value
		err := it.Next(&row)
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, mapError(err)
		}
		if max > 0 && len(res.Rows) >= max {
			res.Truncated = true
			break
		}
		res.Rows = append(res.Rows, encodeRow(row, it.Schema))
	}
	res.Columns = resultColumns(it.Schema)
	return res, nil
}

// pageSize asks for one row more than needed, to learn whether more exist.
func pageSize(max int) int {
	if max <= 0 || max >= pageRows {
		return pageRows
	}
	return max + 1
}

// ---- errors ------------------------------------------------------------------

// posRe finds error positions: "at [3:15]" (BigQuery) or "[at 3:15]" (emulator).
var posRe = regexp.MustCompile(`\[(?:at )?(\d+):(\d+)\]`)

var hints = map[string]string{
	"bytesBilledLimitExceeded": "Filter on partitioned or clustered columns, select fewer columns, or raise the limit in the connection settings.",
	"accessDenied":             "Running queries needs the BigQuery Job User role on the project; reading data needs BigQuery Data Viewer.",
	"notFound":                 "Check the name, and that the dataset is in the connection's location.",
	"responseTooLarge":         "Add a LIMIT, or write the results to a table.",
	"rateLimitExceeded":        "Too many requests in a short time; wait a moment and retry.",
	"quotaExceeded":            "A project quota was reached; see Quotas in the Google Cloud console.",
}

// mapError converts client and API errors into *driver.QueryError with the
// BigQuery reason as code and the line from "[line:col]" in the message.
func mapError(err error) error {
	if err == nil {
		return nil
	}
	var qe *driver.QueryError
	if errors.As(err, &qe) {
		return qe
	}
	var be *bigquery.Error
	if errors.As(err, &be) {
		return queryError(be.Message, be.Reason)
	}
	var ge *googleapi.Error
	if errors.As(err, &ge) {
		msg, reason := ge.Message, ""
		if len(ge.Errors) > 0 {
			reason = ge.Errors[0].Reason
			if msg == "" {
				msg = ge.Errors[0].Message
			}
		}
		if msg == "" {
			msg = http.StatusText(ge.Code)
		}
		return queryError(msg, reason)
	}
	return err
}

func queryError(msg, reason string) *driver.QueryError {
	qe := &driver.QueryError{Message: msg, Code: reason, Hint: hints[reason]}
	if m := posRe.FindStringSubmatch(msg); m != nil {
		qe.Line, _ = strconv.Atoi(m[1])
	}
	return qe
}

// refusal is the cost-guard error raised before a query runs.
func refusal(bytes, limit int64) error {
	return &driver.QueryError{
		Message: fmt.Sprintf("Refused: this query would process %s, more than the %s limit of this connection.", formatBytes(bytes), formatBytes(limit)),
		Code:    "bytesBilledLimitExceeded", Hint: hints["bytesBilledLimitExceeded"],
	}
}

// ---- statement types -----------------------------------------------------------

// statementKind classifies a statement type reported by a dry run or job.
func statementKind(t string) driver.StatementKind {
	switch t {
	case "SELECT":
		return driver.StmtRead
	case "INSERT", "UPDATE", "DELETE", "MERGE", "CALL", "EXPORT_DATA", "LOAD_DATA", "EXPORT_MODEL":
		return driver.StmtWrite
	case "GRANT", "REVOKE":
		return driver.StmtDCL
	case "BEGIN_TRANSACTION", "COMMIT_TRANSACTION", "ROLLBACK_TRANSACTION":
		return driver.StmtTCL
	}
	for _, p := range []string{"CREATE_", "DROP_", "ALTER_", "UNDROP_", "TRUNCATE_"} {
		if strings.HasPrefix(t, p) {
			return driver.StmtDDL
		}
	}
	return driver.StmtUnknown
}

func isDML(t string) bool {
	switch t {
	case "INSERT", "UPDATE", "DELETE", "MERGE":
		return true
	}
	return false
}
