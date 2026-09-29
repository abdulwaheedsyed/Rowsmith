package bigquery

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	bq "google.golang.org/api/bigquery/v2"

	"rowsmith/internal/driver"
)

// fakeStmt is what the fake server does for a query (or one child statement).
type fakeStmt struct {
	stmtType string
	fields   []*bq.TableFieldSchema
	rows     [][]string
	dml      int64
	line     int64 // position in the script, for child jobs
	text     string
}

// fakePlan scripts the fake server's answer to one query text.
type fakePlan struct {
	dryType  string
	dryBytes int64
	fakeStmt
	children []fakeStmt
}

type fakeJob struct {
	job    *bq.Job
	result fakeStmt
}

// fakeBQ is a minimal BigQuery REST server: job insert (dry and real),
// status, results, child listing and cancel.
type fakeBQ struct {
	mu        sync.Mutex
	plan      func(query string) fakePlan
	jobs      map[string]*fakeJob
	order     []string
	inserted  []*bq.Job // real (non-dry) job inserts as received
	dryRuns   int
	cancelled []string
	dryStatus int      // HTTP status to fail dry runs with
	hang      bool     // jobs never complete
	dataReqs  []string // tabledata.list query strings
}

// fakeTable is the one stored table of the fake: ds.people with five rows.
var fakeTable = &bq.Table{Type: "TABLE", Location: "EU", NumRows: 5, TableReference: &bq.TableReference{ProjectId: "p", DatasetId: "ds", TableId: "people"},
	Schema: &bq.TableSchema{Fields: []*bq.TableFieldSchema{field("id", "INTEGER"), field("name", "STRING")}}}

func newFake(t *testing.T, plan func(string) fakePlan) (*fakeBQ, driver.Conn) {
	t.Helper()
	f := &fakeBQ{plan: plan, jobs: map[string]*fakeJob{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /projects/p/datasets", func(w http.ResponseWriter, r *http.Request) { reply(w, &bq.DatasetList{}) })
	mux.HandleFunc("POST /projects/p/jobs", f.insert)
	mux.HandleFunc("GET /projects/p/jobs", f.list)
	mux.HandleFunc("GET /projects/p/jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		if j := f.get(r.PathValue("id")); j != nil {
			reply(w, j.job)
			return
		}
		http.Error(w, `{"error":{"code":404,"message":"Not found: Job"}}`, 404)
	})
	mux.HandleFunc("GET /projects/p/queries/{id}", f.results)
	mux.HandleFunc("GET /projects/p/datasets/ds/tables/people", func(w http.ResponseWriter, r *http.Request) { reply(w, fakeTable) })
	mux.HandleFunc("GET /projects/p/datasets/ds/tables/people/data", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.dataReqs = append(f.dataReqs, r.URL.RawQuery)
		f.mu.Unlock()
		start, _ := strconv.Atoi(r.URL.Query().Get("startIndex"))
		n, _ := strconv.Atoi(r.URL.Query().Get("maxResults"))
		out := &bq.TableDataList{TotalRows: 5}
		for i := start; i < 5 && i < start+n; i++ {
			out.Rows = append(out.Rows, &bq.TableRow{F: []*bq.TableCell{{V: strconv.Itoa(i + 1)}, {V: "name" + strconv.Itoa(i+1)}}})
		}
		reply(w, out)
	})
	mux.HandleFunc("POST /projects/p/jobs/{id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.cancelled = append(f.cancelled, r.PathValue("id"))
		f.mu.Unlock()
		reply(w, &bq.JobCancelResponse{})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	d, _ := driver.Get("bigquery")
	cn, err := d.Open(context.Background(), driver.OpenParams{AppName: "Rowsmith", Params: map[string]any{
		"project": "p", "dataset": "ds", "location": "EU", "endpoint": srv.URL, "maxBilledGB": float64(10)}})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { cn.Close() })
	return f, cn
}

func reply(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakeBQ) get(id string) *fakeJob {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.jobs[id]
}

func (f *fakeBQ) insert(w http.ResponseWriter, r *http.Request) {
	var in bq.Job
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	p := f.plan(in.Configuration.Query.Query)
	f.mu.Lock()
	defer f.mu.Unlock()
	if in.Configuration.DryRun {
		f.dryRuns++
		if f.dryStatus != 0 {
			w.WriteHeader(f.dryStatus)
			reply(w, map[string]any{"error": map[string]any{"code": f.dryStatus, "message": "dry runs are not implemented"}})
			return
		}
		reply(w, &bq.Job{JobReference: in.JobReference, Configuration: in.Configuration, Status: &bq.JobStatus{State: "DONE"},
			Statistics: &bq.JobStatistics{Query: &bq.JobStatistics2{StatementType: p.dryType, TotalBytesProcessed: p.dryBytes}}})
		return
	}
	f.inserted = append(f.inserted, &in)
	now := time.Now().UnixMilli()
	id := in.JobReference.JobId
	state := "DONE"
	if f.hang {
		state = "RUNNING"
	}
	job := &bq.Job{JobReference: in.JobReference, Configuration: in.Configuration, Status: &bq.JobStatus{State: state},
		Statistics: &bq.JobStatistics{CreationTime: now, StartTime: now, EndTime: now + 5, TotalBytesProcessed: 2048,
			Query: &bq.JobStatistics2{StatementType: p.stmtType, TotalBytesProcessed: 2048, TotalBytesBilled: 10 << 20, NumDmlAffectedRows: p.dml}}}
	if len(p.children) > 0 {
		job.Statistics.Query.StatementType = "SCRIPT"
		job.Statistics.NumChildJobs = int64(len(p.children))
	}
	f.jobs[id] = &fakeJob{job: job, result: p.fakeStmt}
	f.order = append(f.order, id)
	for i, c := range p.children {
		cid := id + "_child_" + string(rune('a'+i))
		cj := &bq.Job{JobReference: &bq.JobReference{ProjectId: "p", JobId: cid, Location: "EU"},
			Configuration: &bq.JobConfiguration{Query: &bq.JobConfigurationQuery{Query: c.text}}, Status: &bq.JobStatus{State: "DONE"},
			Statistics: &bq.JobStatistics{CreationTime: now + int64(i), StartTime: now + int64(i), EndTime: now + int64(i) + 3, ParentJobId: id,
				ScriptStatistics: &bq.ScriptStatistics{StackFrames: []*bq.ScriptStackFrame{{StartLine: c.line, StartColumn: 1, Text: c.text}}},
				Query:            &bq.JobStatistics2{StatementType: c.stmtType, NumDmlAffectedRows: c.dml}}}
		if c.stmtType == "INSERT" {
			cj.Statistics.Query.DmlStats = &bq.DmlStatistics{InsertedRowCount: c.dml}
		}
		f.jobs[cid] = &fakeJob{job: cj, result: c}
	}
	reply(w, job)
}

// list returns child jobs newest first, like the real API.
func (f *fakeBQ) list(w http.ResponseWriter, r *http.Request) {
	parent, state := r.URL.Query().Get("parentJobId"), strings.ToUpper(r.URL.Query().Get("stateFilter"))
	f.mu.Lock()
	defer f.mu.Unlock()
	var out bq.JobList
	for _, fj := range f.jobs {
		j := fj.job
		if j.Statistics.ParentJobId == parent && (state == "" || state == j.Status.State) {
			out.Jobs = append(out.Jobs, &bq.JobListJobs{JobReference: j.JobReference, Configuration: j.Configuration, Status: j.Status,
				Statistics: j.Statistics, State: j.Status.State, UserEmail: "me@example.com"})
		}
	}
	for i := 0; i < len(out.Jobs); i++ {
		for k := i + 1; k < len(out.Jobs); k++ {
			if out.Jobs[k].Statistics.CreationTime > out.Jobs[i].Statistics.CreationTime {
				out.Jobs[i], out.Jobs[k] = out.Jobs[k], out.Jobs[i]
			}
		}
	}
	reply(w, &out)
}

func (f *fakeBQ) results(w http.ResponseWriter, r *http.Request) {
	fj := f.get(r.PathValue("id"))
	if fj == nil {
		http.Error(w, `{"error":{"code":404,"message":"Not found: Job"}}`, 404)
		return
	}
	f.mu.Lock()
	hang := f.hang
	f.mu.Unlock()
	res := &bq.GetQueryResultsResponse{JobReference: fj.job.JobReference, JobComplete: !hang}
	if !hang {
		res.Schema = &bq.TableSchema{Fields: fj.result.fields}
		for _, row := range fj.result.rows {
			tr := &bq.TableRow{}
			for _, v := range row {
				tr.F = append(tr.F, &bq.TableCell{V: v})
			}
			res.Rows = append(res.Rows, tr)
		}
		res.TotalRows = uint64(len(res.Rows))
	}
	reply(w, res)
}

func field(name, typ string) *bq.TableFieldSchema {
	return &bq.TableFieldSchema{Name: name, Type: typ, Mode: "NULLABLE"}
}

func run(t *testing.T, cn driver.Conn, script string, opts driver.ExecOptions) *recSink {
	t.Helper()
	sess, err := cn.NewSession(context.Background(), driver.Scope{})
	if err != nil {
		t.Fatal(err)
	}
	return exec(t, sess, script, opts)
}

func selectPlan(q string) fakePlan {
	return fakePlan{dryType: "SELECT", dryBytes: 1 << 30, fakeStmt: fakeStmt{stmtType: "SELECT",
		fields: []*bq.TableFieldSchema{field("n", "INTEGER"), field("s", "STRING")},
		rows:   [][]string{{"1", "a"}, {"2", "b"}, {"3", "c"}}}}
}

func TestExecuteSelect(t *testing.T) {
	f, cn := newFake(t, selectPlan)
	sink := run(t, cn, "SELECT n, s FROM t", driver.ExecOptions{MaxRows: 2})
	mustOK(t, sink)
	st := sink.stmts[0]
	if len(sink.stmts) != 1 || len(st.cols) != 2 || len(st.rows) != 2 || st.rows[1][1] != "b" {
		t.Fatalf("result = %+v", st)
	}
	sum := st.sums[0]
	if !sum.Truncated || sum.RowCount != 2 || *sum.BytesProcessed != 2048 || *sum.CacheHit {
		t.Errorf("summary = %+v", sum)
	}
	if st.notices[0] != "info: This query will process 1 GB when run." || !hasNotice(st, "Billed 10 MB") {
		t.Errorf("notices = %v", st.notices)
	}
	if len(f.inserted) != 1 {
		t.Fatalf("%d jobs inserted", len(f.inserted))
	}
	job := f.inserted[0]
	q := job.Configuration.Query
	if q.MaximumBytesBilled != 10<<30 || job.JobReference.Location != "EU" || job.Configuration.Labels["client"] != "rowsmith" ||
		q.DefaultDataset == nil || q.DefaultDataset.DatasetId != "ds" || !strings.HasPrefix(job.JobReference.JobId, "rowsmith_") {
		t.Errorf("job config = %+v / %+v", job.JobReference, q)
	}
}

func TestExecuteScriptChildren(t *testing.T) {
	script := "SELECT 1 AS a;\nINSERT INTO t VALUES (1);\nCREATE TABLE u (x INT64);\nSELECT 'z' AS b"
	_, cn := newFake(t, func(string) fakePlan {
		return fakePlan{dryType: "SCRIPT", dryBytes: 100, children: []fakeStmt{
			{stmtType: "SELECT", line: 1, text: "SELECT 1 AS a", fields: []*bq.TableFieldSchema{field("a", "INTEGER")}, rows: [][]string{{"1"}}},
			{stmtType: "INSERT", line: 2, text: "INSERT INTO t VALUES (1)", dml: 1},
			{stmtType: "CREATE_TABLE", line: 3, text: "CREATE TABLE u (x INT64)"},
			{stmtType: "SELECT", line: 4, text: "SELECT 'z' AS b", fields: []*bq.TableFieldSchema{field("b", "STRING")}, rows: [][]string{{"z"}}},
		}}
	})
	sink := run(t, cn, script, driver.ExecOptions{MaxRows: 100})
	mustOK(t, sink)
	if len(sink.stmts) != 4 {
		t.Fatalf("got %d statements: %+v", len(sink.stmts), sink.stmts)
	}
	head := sink.stmts[0]
	if head.info.SQL != script || head.info.Kind != driver.StmtDDL || !hasNotice(head, "This script will process 100 B") || len(head.sums) != 1 {
		t.Errorf("script entry = %+v", head)
	}
	first, ins, last := sink.stmts[1], sink.stmts[2], sink.stmts[3]
	if first.info.Line != 1 || first.info.Kind != driver.StmtRead || len(first.rows) != 1 || first.rows[0][0] != int64(1) {
		t.Errorf("first child = %+v", first)
	}
	if ins.info.Line != 2 || ins.info.Kind != driver.StmtWrite || ins.sums[0].RowsAffected == nil || *ins.sums[0].RowsAffected != 1 {
		t.Errorf("insert child = %+v", ins)
	}
	if last.info.Line != 4 || last.info.SQL != "SELECT 'z' AS b" || last.rows[0][0] != "z" || last.sums[0].DurationMS != 3 {
		t.Errorf("last child = %+v %+v", last, last.sums)
	}
	seen := map[int]bool{}
	for _, st := range sink.stmts {
		if seen[st.info.Index] {
			t.Errorf("duplicate statement index %d", st.info.Index)
		}
		seen[st.info.Index] = true
	}
}

func TestExecuteGuards(t *testing.T) {
	plan := selectPlan
	f, cn := newFake(t, func(q string) fakePlan { return plan(q) })

	// The dry run is the source of truth for read-only mode.
	plan = func(string) fakePlan { return fakePlan{dryType: "DELETE", dryBytes: 10} }
	ro := run(t, cn, "SELECT * FROM t", driver.ExecOptions{ReadOnly: true})
	if err := ro.stmts[0].err; err == nil || !strings.Contains(err.Error(), "reports a DELETE statement") {
		t.Errorf("read-only = %v", err)
	}
	// The classifier refuses before anything is sent.
	dry := f.dryRuns
	if err := run(t, cn, "UPDATE t SET a = 1 WHERE TRUE", driver.ExecOptions{ReadOnly: true}).stmts[0].err; err == nil || f.dryRuns != dry {
		t.Errorf("classifier guard = %v (dry runs %d -> %d)", err, dry, f.dryRuns)
	}

	plan = func(string) fakePlan { return fakePlan{dryType: "SELECT", dryBytes: 20 << 30} }
	var qe *driver.QueryError
	if err := run(t, cn, "SELECT * FROM big", driver.ExecOptions{}).stmts[0].err; !errors.As(err, &qe) || qe.Code != "bytesBilledLimitExceeded" {
		t.Errorf("cost guard = %#v", err)
	}
	if err := run(t, cn, "SELECT * FROM big", driver.ExecOptions{MaxBytesBilled: 30 << 30}).stmts[0].err; err != nil {
		t.Errorf("raised limit = %v", err)
	}

	plan = selectPlan
	before := len(f.inserted)
	est := run(t, cn, "SELECT 1", driver.ExecOptions{DryRun: true})
	mustOK(t, est)
	if len(f.inserted) != before || len(est.stmts[0].sums) != 1 || *est.stmts[0].sums[0].BytesProcessed != 1<<30 {
		t.Errorf("dry-run option ran a job or lost the estimate: %+v", est.stmts[0])
	}

	f.mu.Lock()
	f.dryStatus = http.StatusNotImplemented
	f.mu.Unlock()
	degraded := run(t, cn, "SELECT 1", driver.ExecOptions{})
	mustOK(t, degraded)
	if !hasNotice(degraded.stmts[0], "cost estimate is unavailable") || len(degraded.stmts[0].rows) != 3 {
		t.Errorf("degraded run = %+v", degraded.stmts[0])
	}
	if err := run(t, cn, "SELECT 1", driver.ExecOptions{ReadOnly: true}).stmts[0].err; err == nil {
		t.Error("read-only run without a verifiable dry run was allowed")
	}
}

func TestExecuteCancel(t *testing.T) {
	f, cn := newFake(t, selectPlan)
	f.mu.Lock()
	f.hang = true
	f.mu.Unlock()
	sess, _ := cn.NewSession(context.Background(), driver.Scope{})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	sink := &recSink{}
	if err := sess.Execute(ctx, "SELECT 1", driver.ExecOptions{}, sink); err != nil {
		t.Fatal(err)
	}
	if err := sink.stmts[0].err; err == nil || err.Error() != "Query cancelled" {
		t.Errorf("error = %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.cancelled) != 1 || len(f.order) != 1 || f.cancelled[0] != f.order[0] {
		t.Errorf("cancelled %v, jobs %v", f.cancelled, f.order)
	}
}

func TestProcessesAndKill(t *testing.T) {
	f, cn := newFake(t, selectPlan)
	mustOK(t, run(t, cn, "SELECT 1", driver.ExecOptions{}))
	pm := cn.(driver.ProcessManager)
	res, err := pm.Processes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) == 0 || res.Columns[0].Name != "id" {
		t.Fatalf("processes = %+v", res)
	}
	row := res.Rows[0]
	if id := row[0].(string); id != "EU."+f.order[0] || row[1] != "DONE" || row[3] != "SELECT" || row[10] != "SELECT 1" {
		t.Errorf("row = %v", row)
	}
	if err := pm.KillProcess(context.Background(), row[0].(string)); err != nil {
		t.Fatal(err)
	}
	if len(f.cancelled) != 1 || f.cancelled[0] != f.order[0] {
		t.Errorf("cancelled = %v", f.cancelled)
	}
}

func TestBrowseAndCountFree(t *testing.T) {
	f, cn := newFake(t, selectPlan)
	ref := driver.ObjectRef{Schema: "ds", Name: "people"}
	res, err := cn.Browse(context.Background(), driver.BrowseRequest{Ref: ref, Offset: 2, Limit: 2, Columns: []string{"name"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.SQL != freeReadSQL || len(res.Rows) != 2 || !res.Truncated || res.Rows[0][0] != "name3" || len(res.Columns) != 1 {
		t.Fatalf("browse = %+v", res)
	}
	if len(f.dataReqs) != 1 || !strings.Contains(f.dataReqs[0], "startIndex=2") || !strings.Contains(f.dataReqs[0], "maxResults=3") {
		t.Errorf("tabledata.list requests = %v", f.dataReqs)
	}
	tail, err := cn.Browse(context.Background(), driver.BrowseRequest{Ref: ref, Offset: 4, Limit: 2})
	if err != nil || len(tail.Rows) != 1 || tail.Truncated {
		t.Errorf("last page = %+v, %v", tail, err)
	}
	n, err := cn.Count(context.Background(), driver.BrowseRequest{Ref: ref})
	if err != nil || n.Rows != 5 || !n.Exact {
		t.Errorf("count = %+v, %v", n, err)
	}
	if len(f.inserted) != 0 || f.dryRuns != 0 {
		t.Errorf("free paths started %d jobs and %d dry runs", len(f.inserted), f.dryRuns)
	}

	// Filtering needs a query, guarded by the bytes-billed limit.
	if _, err := cn.Browse(context.Background(), driver.BrowseRequest{Ref: ref, Search: "x"}); err != nil {
		t.Fatal(err)
	}
	if len(f.inserted) != 1 || f.inserted[0].Configuration.Query.MaximumBytesBilled != 10<<30 || len(f.inserted[0].Configuration.Query.QueryParameters) != 2 {
		t.Errorf("filtered browse job = %+v", f.inserted)
	}
}
