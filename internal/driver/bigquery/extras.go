package bigquery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/bigquery"
	bq "google.golang.org/api/bigquery/v2"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"

	"rowsmith/internal/driver"
	"rowsmith/internal/sqlsplit"
)

// ---- explain -------------------------------------------------------------------

// Explain without analyze is a dry run: bytes processed, statement type and
// referenced tables, at no cost. With analyze the query runs (reads only,
// cache disabled, under the cost guard) and its execution stages become the
// plan tree.
func (c *conn) Explain(ctx context.Context, s driver.Scope, stmt string, analyze bool) (*driver.Plan, error) {
	var project, dataset string
	if s.Schema != "" || c.dataset != "" {
		var err error
		if project, dataset, err = c.datasetOf(s.Schema); err != nil {
			return nil, err
		}
	}
	mk := func() *bigquery.Query {
		q := c.newQuery(stmt, "")
		q.DefaultProjectID, q.DefaultDatasetID = project, dataset
		return q
	}
	est, err := c.dryRun(ctx, mk())
	if err != nil {
		return nil, mapError(err)
	}
	if !analyze {
		return dryRunPlan(est), nil
	}
	if k, _ := sqlsplit.Classify(stmt, sqlsplit.BigQuery); k != driver.StmtRead || est.StatementType != "SELECT" {
		return nil, errors.New("analyzing runs the statement, so only SELECT queries can be analyzed")
	}
	if c.maxBytes > 0 && est.TotalBytesProcessed > c.maxBytes {
		return nil, refusal(est.TotalBytesProcessed, c.maxBytes)
	}
	q := mk()
	q.DisableQueryCache = true // a cached result has no plan
	job, _, err := c.runJob(ctx, q)
	if err != nil {
		return nil, mapError(err)
	}
	call := c.svc.Jobs.Get(job.ProjectID(), job.ID()).Fields("statistics").Context(ctx)
	if job.Location() != "" {
		call = call.Location(job.Location())
	}
	raw, err := call.Do()
	if err != nil {
		return nil, mapError(err)
	}
	return analyzePlan(raw.Statistics), nil
}

func tableNames(ts []*bigquery.Table) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		if t != nil {
			out = append(out, qualifiedName(t))
		}
	}
	return out
}

// Plan totals are displayed as given, except that numbers are shown as
// durations in milliseconds; sizes and counts are therefore sent as text.

// dryRunPlan presents a dry run: the statement with the tables it reads.
func dryRunPlan(qs *bigquery.QueryStatistics) *driver.Plan {
	tables := tableNames(qs.ReferencedTables)
	root := &driver.PlanNode{Operation: strings.ReplaceAll(qs.StatementType, "_", " "), Detail: "will process " + formatBytes(qs.TotalBytesProcessed),
		Props: map[string]string{"Bytes processed": strconv.FormatInt(qs.TotalBytesProcessed, 10)}}
	if root.Operation == "" {
		root.Operation = "Query"
	}
	setOpt(root.Props, "Estimate accuracy", qs.TotalBytesProcessedAccuracy)
	for _, t := range tables {
		root.Children = append(root.Children, &driver.PlanNode{Operation: "Read table", Object: t})
	}
	totals := map[string]any{"Bytes processed": formatBytes(qs.TotalBytesProcessed), "Statement": qs.StatementType}
	if len(tables) > 0 {
		totals["Tables"] = strings.Join(tables, ", ")
	}
	raw, _ := json.MarshalIndent(map[string]any{"statementType": qs.StatementType, "totalBytesProcessed": qs.TotalBytesProcessed,
		"totalBytesProcessedAccuracy": qs.TotalBytesProcessedAccuracy, "referencedTables": tables}, "", "  ")
	return &driver.Plan{Root: root, Raw: string(raw), Format: "json", Totals: totals}
}

// analyzePlan turns the execution stages of a finished job into a tree: each
// stage's children are the stages it reads from, and the root holds the
// stages nothing else reads (normally the output stage). Node times are
// inclusive of their inputs, like EXPLAIN ANALYZE elsewhere, so the plan
// view can derive each stage's own share; Raw is the stages as JSON.
func analyzePlan(stats *bq.JobStatistics) *driver.Plan {
	root := &driver.PlanNode{Operation: "Query", Props: map[string]string{}}
	plan := &driver.Plan{Root: root, Format: "json", Raw: "[]", Totals: map[string]any{}}
	if stats == nil || stats.Query == nil {
		root.Detail = "no execution statistics were reported"
		return plan
	}
	qs := stats.Query
	if stats.EndTime > 0 && stats.StartTime > 0 {
		t := float64(stats.EndTime - stats.StartTime)
		root.TimeMS = &t
		plan.Totals["Elapsed"] = t
	}
	plan.Totals["Slot time"] = float64(qs.TotalSlotMs)
	plan.Totals["Bytes processed"] = formatBytes(qs.TotalBytesProcessed)
	plan.Totals["Bytes billed"] = formatBytes(qs.TotalBytesBilled)
	root.Detail = "processed " + formatBytes(qs.TotalBytesProcessed) + ", billed " + formatBytes(qs.TotalBytesBilled)
	root.Props["Slot time (ms)"] = strconv.FormatInt(qs.TotalSlotMs, 10)
	setOpt(root.Props, "Statement", qs.StatementType)
	if len(qs.QueryPlan) == 0 {
		root.Detail += "; no query plan was reported"
		return plan
	}
	if raw, err := json.MarshalIndent(qs.QueryPlan, "", "  "); err == nil {
		plan.Raw = string(raw)
	}
	plan.Totals["Stages"] = strconv.Itoa(len(qs.QueryPlan))

	byID := map[int64]*bq.ExplainQueryStage{}
	read := map[int64]bool{}
	for _, s := range qs.QueryPlan {
		byID[s.Id] = s
		for _, in := range s.InputStages {
			read[in] = true
		}
	}
	placed := map[int64]bool{}
	var build func(s *bq.ExplainQueryStage) *driver.PlanNode
	build = func(s *bq.ExplainQueryStage) *driver.PlanNode {
		placed[s.Id] = true
		n := stageNode(s)
		total := *n.TimeMS
		for _, in := range s.InputStages {
			if src, ok := byID[in]; ok && !placed[in] {
				child := build(src)
				n.Children = append(n.Children, child)
				total += *child.TimeMS
			}
		}
		n.TimeMS = &total
		return n
	}
	for i := len(qs.QueryPlan) - 1; i >= 0; i-- {
		if s := qs.QueryPlan[i]; !read[s.Id] && !placed[s.Id] {
			root.Children = append(root.Children, build(s))
		}
	}
	for _, s := range qs.QueryPlan { // defensive: stages reachable from no output
		if !placed[s.Id] {
			root.Children = append(root.Children, build(s))
		}
	}
	return plan
}

// stageNode describes one execution stage: its steps, records, timing
// ratios, shuffle volume and slot time. TimeMS is the stage's own wall time.
func stageNode(s *bq.ExplainQueryStage) *driver.PlanNode {
	n := &driver.PlanNode{Operation: s.Name, Props: map[string]string{}}
	var own float64
	if s.EndMs > 0 && s.StartMs > 0 && s.EndMs >= s.StartMs {
		own = float64(s.EndMs - s.StartMs)
	}
	n.TimeMS = &own
	written := float64(s.RecordsWritten)
	n.ActualRows = &written
	slot := float64(s.SlotMs)
	n.Cost = &slot
	var kinds, steps []string
	for _, st := range s.Steps {
		kinds = append(kinds, st.Kind)
		steps = append(steps, st.Kind+": "+strings.Join(st.Substeps, "; "))
		if st.Kind == "READ" && n.Object == "" {
			for _, sub := range st.Substeps {
				if strings.HasPrefix(sub, "FROM ") {
					n.Object = strings.TrimPrefix(sub, "FROM ")
					break
				}
			}
		}
	}
	n.Detail = strings.Join(kinds, " → ")
	p := n.Props
	setOpt(p, "Status", s.Status)
	p["Records read"] = strconv.FormatInt(s.RecordsRead, 10)
	p["Records written"] = strconv.FormatInt(s.RecordsWritten, 10)
	p["Parallel inputs"] = fmt.Sprintf("%d of %d completed", s.CompletedParallelInputs, s.ParallelInputs)
	p["Stage time (ms)"] = strconv.FormatFloat(own, 'f', -1, 64)
	p["Slot time (ms)"] = strconv.FormatInt(s.SlotMs, 10)
	p["Wait ratio (avg / max)"] = ratios(s.WaitRatioAvg, s.WaitRatioMax)
	p["Read ratio (avg / max)"] = ratios(s.ReadRatioAvg, s.ReadRatioMax)
	p["Compute ratio (avg / max)"] = ratios(s.ComputeRatioAvg, s.ComputeRatioMax)
	p["Write ratio (avg / max)"] = ratios(s.WriteRatioAvg, s.WriteRatioMax)
	p["Shuffle output"] = formatBytes(s.ShuffleOutputBytes)
	if s.ShuffleOutputBytesSpilled > 0 {
		p["Shuffle spilled to disk"] = formatBytes(s.ShuffleOutputBytesSpilled)
	}
	setOpt(p, "Compute mode", s.ComputeMode)
	setOpt(p, "Steps", strings.Join(steps, "\n"))
	return n
}

func ratios(avg, max float64) string {
	return strconv.FormatFloat(avg, 'f', 2, 64) + " / " + strconv.FormatFloat(max, 'f', 2, 64)
}

// ---- jobs ----------------------------------------------------------------------

var stateNames = map[bigquery.State]string{bigquery.Pending: "PENDING", bigquery.Running: "RUNNING", bigquery.Done: "DONE"}

const (
	maxActiveJobs = 200
	maxRecentJobs = 50
)

// Processes lists running and pending jobs of the project, then the jobs
// that finished in the last hour. All users' jobs are shown when the
// credentials may list them (bigquery.jobs.listAll), otherwise only their own.
func (c *conn) Processes(ctx context.Context) (*driver.Result, error) {
	start := time.Now()
	jobs, err := c.listJobs(ctx, true)
	var ge *googleapi.Error
	if errors.As(err, &ge) && ge.Code == 403 {
		jobs, err = c.listJobs(ctx, false)
	}
	if err != nil {
		return nil, mapError(err)
	}
	res := &driver.Result{Columns: []driver.ResultColumn{
		{Name: "id", Type: "STRING", Kind: driver.KindString},
		{Name: "state", Type: "STRING", Kind: driver.KindString},
		{Name: "user", Type: "STRING", Kind: driver.KindString},
		{Name: "type", Type: "STRING", Kind: driver.KindString},
		{Name: "created", Type: "TIMESTAMP", Kind: driver.KindTimestamp},
		{Name: "seconds", Type: "FLOAT64", Kind: driver.KindFloat},
		{Name: "bytes_processed", Type: "INT64", Kind: driver.KindInt},
		{Name: "bytes_billed", Type: "INT64", Kind: driver.KindInt},
		{Name: "slot_ms", Type: "INT64", Kind: driver.KindInt},
		{Name: "cache_hit", Type: "BOOL", Kind: driver.KindBool},
		{Name: "query", Type: "STRING", Kind: driver.KindString},
		{Name: "error", Type: "STRING", Kind: driver.KindString},
	}, Rows: [][]any{}}
	now := time.Now()
	for _, j := range jobs {
		res.Rows = append(res.Rows, jobRow(j, now))
	}
	res.Duration = time.Since(start)
	res.DurationMS = float64(res.Duration.Microseconds()) / 1000
	return res, nil
}

func (c *conn) listJobs(ctx context.Context, allUsers bool) ([]*bigquery.Job, error) {
	var out []*bigquery.Job
	collect := func(state bigquery.State, since time.Time, limit int) error {
		it := c.client.Jobs(ctx)
		it.AllUsers, it.State, it.MinCreationTime = allUsers, state, since
		for n := 0; n < limit; n++ {
			j, err := it.Next()
			if err == iterator.Done {
				return nil
			}
			if err != nil {
				return err
			}
			out = append(out, j)
		}
		return nil
	}
	for _, state := range []bigquery.State{bigquery.Running, bigquery.Pending} {
		if err := collect(state, time.Time{}, maxActiveJobs); err != nil {
			return nil, err
		}
	}
	if err := collect(bigquery.Done, time.Now().Add(-time.Hour), maxRecentJobs); err != nil {
		return nil, err
	}
	return out, nil
}

// jobID renders "LOCATION.job_id", the form KillProcess accepts.
func jobID(j *bigquery.Job) string {
	if j.Location() != "" {
		return j.Location() + "." + j.ID()
	}
	return j.ID()
}

func jobRow(j *bigquery.Job, now time.Time) []any {
	row := []any{jobID(j), nil, driver.EncodeText(j.Email()), nil, nil, nil, nil, nil, nil, nil, nil, nil}
	st := j.LastStatus()
	if st == nil {
		return row
	}
	row[1] = stateNames[st.State]
	if err := st.Err(); err != nil {
		row[11] = driver.EncodeText(mapError(err).Error())
	}
	cfg, _ := j.Config()
	switch x := cfg.(type) {
	case *bigquery.QueryConfig:
		row[3], row[10] = "QUERY", driver.EncodeText(x.Q)
	case *bigquery.LoadConfig:
		row[3] = "LOAD"
	case *bigquery.CopyConfig:
		row[3] = "COPY"
	case *bigquery.ExtractConfig:
		row[3] = "EXTRACT"
	}
	s := st.Statistics
	if s == nil {
		return row
	}
	if !s.CreationTime.IsZero() {
		row[4] = driver.EncodeTime(s.CreationTime.UTC(), driver.KindTimestamp)
	}
	if !s.StartTime.IsZero() {
		end := now
		if st.State == bigquery.Done && !s.EndTime.IsZero() {
			end = s.EndTime
		}
		row[5] = end.Sub(s.StartTime).Round(100 * time.Millisecond).Seconds()
	}
	row[6] = driver.EncodeInt(s.TotalBytesProcessed)
	if qs, ok := s.Details.(*bigquery.QueryStatistics); ok && qs != nil {
		if qs.StatementType != "" {
			row[3] = qs.StatementType
		}
		row[7], row[8], row[9] = driver.EncodeInt(qs.TotalBytesBilled), driver.EncodeInt(qs.SlotMillis), qs.CacheHit
	}
	return row
}

// KillProcess cancels a job.
func (c *conn) KillProcess(ctx context.Context, id string) error {
	project, location, job, err := parseJobID(id, c.project, c.location)
	if err != nil {
		return err
	}
	j, err := c.client.JobFromProject(ctx, project, job, location)
	if err != nil {
		return mapError(err)
	}
	return mapError(j.Cancel(ctx))
}

// parseJobID accepts "job_id", "LOCATION.job_id" or BigQuery's full
// "project:LOCATION.job_id" (project IDs may themselves contain a colon).
func parseJobID(id, project, location string) (string, string, string, error) {
	job := strings.TrimSpace(id)
	if i := strings.LastIndexByte(job, ':'); i >= 0 {
		project, job = job[:i], job[i+1:]
	}
	if i := strings.IndexByte(job, '.'); i >= 0 {
		location, job = job[:i], job[i+1:]
	}
	if job == "" || project == "" {
		return "", "", "", fmt.Errorf("invalid job id %q", id)
	}
	return project, location, job, nil
}

// Classify implements driver.Classifier.
func (c *conn) Classify(stmt string) driver.StatementKind {
	k, _ := sqlsplit.Classify(stmt, sqlsplit.BigQuery)
	return k
}
