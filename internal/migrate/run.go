package migrate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"rowsmith/internal/driver"
	"rowsmith/internal/export"
)

const (
	batchRows   = 500
	batchBytes  = 4 << 20
	commitEvery = 20_000 // rows per target transaction
)

// Job is one migration run and its live progress.
type Job struct {
	ID     string
	mu     sync.Mutex
	v      JobView
	cancel context.CancelFunc
}

type JobView struct {
	Status   string       `json:"status"` // running, done, failed, cancelled
	Phase    string       `json:"phase"`
	Started  int64        `json:"started"`
	Finished int64        `json:"finished"`
	Tables   []*TableRun  `json:"tables"`
	Objects  []*ObjectRun `json:"objects"`
	Log      []LogLine    `json:"log"`
	Error    string       `json:"error,omitempty"`
}

type TableRun struct {
	Source   string        `json:"source"`
	Target   string        `json:"target"`
	Status   string        `json:"status"` // waiting, creating, copying, indexing, verifying, done, failed, skipped, cancelled
	Rows     int64         `json:"rows"`
	Total    int64         `json:"total"`
	Started  int64         `json:"started"`
	Finished int64         `json:"finished"`
	Error    string        `json:"error,omitempty"`
	Notes    []string      `json:"notes"`
	Verify   *Verification `json:"verify,omitempty"`
}

type Verification struct {
	SourceRows int64    `json:"sourceRows"`
	TargetRows int64    `json:"targetRows"`
	Counts     string   `json:"counts"`   // match or differ
	Contents   string   `json:"contents"` // match, differ or skipped
	Columns    []string `json:"columns,omitempty"`
	Note       string   `json:"note,omitempty"`
}

type ObjectRun struct {
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	Status string `json:"status"` // waiting, done, failed
	Error  string `json:"error,omitempty"`
}

type LogLine struct {
	At    int64  `json:"at"`
	Level string `json:"level"` // info, warn, error
	Text  string `json:"text"`
}

// NewJob prepares progress for the tables a plan includes.
func NewJob(id string, p *Plan) *Job {
	j := &Job{ID: id, v: JobView{Status: "running", Phase: "Starting", Started: time.Now().UnixMilli(), Tables: []*TableRun{}, Objects: []*ObjectRun{}, Log: []LogLine{}}}
	for _, t := range p.Tables {
		if !t.Include {
			continue
		}
		tr := &TableRun{Source: t.Source.Name, Target: t.Target, Status: "waiting", Notes: slices.Clone(t.Notes)}
		if tr.Notes == nil {
			tr.Notes = []string{}
		}
		if t.Rows != nil {
			tr.Total = *t.Rows
		}
		j.v.Tables = append(j.v.Tables, tr)
	}
	for _, o := range p.Objects {
		j.v.Objects = append(j.v.Objects, &ObjectRun{Name: o.Name, Kind: o.Kind, Status: "waiting"})
	}
	return j
}

// View returns a copy of the progress, safe to encode.
func (j *Job) View() JobView {
	j.mu.Lock()
	defer j.mu.Unlock()
	b, _ := json.Marshal(j.v)
	var v JobView
	_ = json.Unmarshal(b, &v)
	return v
}

// Cancel stops the run; the table being copied rolls back its last chunk.
func (j *Job) Cancel() {
	j.mu.Lock()
	c := j.cancel
	j.mu.Unlock()
	if c != nil {
		c()
	}
}

func (j *Job) update(fn func(v *JobView)) {
	j.mu.Lock()
	fn(&j.v)
	j.mu.Unlock()
}

func (j *Job) log(level, format string, args ...any) {
	j.update(func(v *JobView) {
		v.Log = append(v.Log, LogLine{At: time.Now().UnixMilli(), Level: level, Text: fmt.Sprintf(format, args...)})
		if len(v.Log) > 500 {
			v.Log = v.Log[len(v.Log)-500:]
		}
	})
}

func (j *Job) table(name string) *TableRun {
	for _, t := range j.v.Tables {
		if t.Source == name {
			return t
		}
	}
	return nil
}

func (j *Job) setTable(name string, fn func(t *TableRun)) {
	j.update(func(v *JobView) {
		if t := j.table(name); t != nil {
			fn(t)
		}
	})
}

// run holds what every step needs.
type run struct {
	j        *Job
	p        *Plan
	src, dst Endpoint
	srcSess  driver.Session
	dstSess  driver.Session
	byName   map[string]*TablePlan // included tables by source name
	created  map[string]*driver.Table
	digests  map[string]*digest
	canons   map[string][]canon // source canon per copied column, by table
	copied   map[string][]string
	appended map[string]bool
	failed   map[string]bool
}

// Run executes the plan. It blocks until the migration ends.
func (j *Job) Run(ctx context.Context, p *Plan, src, dst Endpoint) {
	ctx, cancel := context.WithCancel(ctx)
	j.mu.Lock()
	j.cancel = cancel
	j.mu.Unlock()
	defer cancel()
	r := &run{j: j, p: p, src: src, dst: dst, byName: map[string]*TablePlan{}, created: map[string]*driver.Table{}, digests: map[string]*digest{},
		canons: map[string][]canon{}, copied: map[string][]string{}, appended: map[string]bool{}, failed: map[string]bool{}}
	err := r.all(ctx)
	j.update(func(v *JobView) {
		v.Finished = time.Now().UnixMilli()
		v.Phase = ""
		switch {
		case errors.Is(ctx.Err(), context.Canceled):
			v.Status = "cancelled"
			for _, t := range v.Tables {
				if t.Status != "done" && t.Status != "failed" && t.Status != "skipped" {
					t.Status = "cancelled"
				}
			}
		case err != nil:
			v.Status, v.Error = "failed", err.Error()
		case len(r.failed) > 0:
			v.Status = "failed"
			v.Error = fmt.Sprintf("%d of %d tables failed", len(r.failed), len(v.Tables))
		default:
			v.Status = "done"
		}
	})
	if r.srcSess != nil {
		r.srcSess.Close()
	}
	if r.dstSess != nil {
		r.dstSess.Close()
	}
}

func (r *run) phase(name string) {
	r.j.update(func(v *JobView) { v.Phase = name })
	r.j.log("info", "%s", name)
}

func (r *run) all(ctx context.Context) error {
	var err error
	if r.srcSess, err = r.src.Conn.NewSession(ctx, r.src.Scope); err != nil {
		return fmt.Errorf("could not open the source: %w", err)
	}
	if r.dstSess, err = r.dst.Conn.NewSession(ctx, r.dst.Scope); err != nil {
		return fmt.Errorf("could not open the target: %w", err)
	}
	var order []*TablePlan
	for _, t := range r.p.Tables {
		if t.Include {
			r.byName[t.Source.Name] = t
			order = append(order, t)
		}
	}
	if len(order) == 0 {
		return ErrNothing
	}
	order = parentsFirst(order, r.byName)

	// Replace: drop the existing tables first, children before parents, so
	// foreign keys between them do not block the drops.
	if r.p.Options.Existing == "replace" {
		r.dropExisting(ctx, order)
	}

	// Types and sequences the tables need (same engine).
	if pre := r.objectsOf(true); len(pre) > 0 {
		r.phase("Creating types and sequences")
		r.definitions(ctx, pre)
	}

	for _, tp := range order {
		if ctx.Err() != nil {
			return nil
		}
		if err := r.table(ctx, tp); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			r.failed[tp.Source.Name] = true
			r.j.setTable(tp.Source.Name, func(t *TableRun) { t.Status, t.Error, t.Finished = "failed", err.Error(), time.Now().UnixMilli() })
			r.j.log("error", "%s: %v", tp.Source.Name, err)
			if r.p.Options.StopOnError {
				r.skipRest(order)
				break
			}
		}
	}

	// SQLite and MongoDB get their keys with the table (see createDef).
	if (r.p.Options.Indexes || r.p.Options.ForeignKeys) && ctx.Err() == nil && r.dst.engine() != SQLite && r.dst.engine() != MongoDB {
		r.phase("Adding indexes and foreign keys")
		for _, tp := range order {
			if ctx.Err() != nil {
				return nil
			}
			if _, ok := r.created[tp.Source.Name]; ok && !r.failed[tp.Source.Name] {
				r.keys(ctx, tp)
			}
		}
	}

	if post := r.objectsOf(false); len(post) > 0 && ctx.Err() == nil {
		r.phase("Creating views, routines and triggers")
		r.definitions(ctx, post)
	}

	if r.p.Options.Data && ctx.Err() == nil {
		r.phase("Verifying")
		for _, tp := range order {
			if ctx.Err() != nil {
				return nil
			}
			if _, ok := r.created[tp.Source.Name]; ok && !r.failed[tp.Source.Name] {
				r.verify(ctx, tp)
			}
		}
	}
	r.j.update(func(v *JobView) {
		for _, t := range v.Tables {
			if t.Status != "failed" && t.Status != "skipped" && t.Status != "cancelled" {
				t.Status = "done"
				if t.Finished == 0 {
					t.Finished = time.Now().UnixMilli()
				}
			}
		}
	})
	return nil
}

func (r *run) skipRest(order []*TablePlan) {
	r.j.update(func(v *JobView) {
		for _, t := range v.Tables {
			if t.Status == "waiting" {
				t.Status, t.Error = "skipped", "stopped after an earlier table failed"
			}
		}
	})
}

// parentsFirst orders tables so referenced tables load before the tables
// that reference them (cycles keep their order).
func parentsFirst(tables []*TablePlan, byName map[string]*TablePlan) []*TablePlan {
	out := make([]*TablePlan, 0, len(tables))
	state := map[string]int{} // 1 visiting, 2 done
	var visit func(t *TablePlan)
	visit = func(t *TablePlan) {
		if state[t.Source.Name] != 0 {
			return
		}
		state[t.Source.Name] = 1
		for _, fk := range t.ForeignKeys {
			if p, ok := byName[fk.RefTable]; ok && p != t && state[p.Source.Name] == 0 {
				visit(p)
			}
		}
		state[t.Source.Name] = 2
		out = append(out, t)
	}
	for _, t := range tables {
		visit(t)
	}
	return out
}

func (r *run) targetRef(tp *TablePlan) driver.ObjectRef {
	kind := "table"
	if r.dst.engine() == MongoDB {
		kind = "collection"
	}
	return driver.ObjectRef{Database: r.dst.Scope.Database, Schema: r.dst.Scope.Schema, Name: tp.Target, Kind: kind}
}

// exec runs statements on the target, one at a time.
func (r *run) exec(ctx context.Context, stmts []string) error {
	for _, s := range stmts {
		if strings.TrimSpace(s) == "" {
			continue
		}
		sink := &errSink{}
		if err := r.dstSess.Execute(ctx, s, driver.ExecOptions{StopOnError: true}, sink); err != nil {
			return queryErr(err)
		}
		if sink.err != nil {
			return queryErr(sink.err)
		}
	}
	return nil
}

func queryErr(err error) error {
	var qe *driver.QueryError
	if errors.As(err, &qe) {
		return errors.New(qe.Message)
	}
	return err
}

type errSink struct{ err error }

func (s *errSink) BeginStatement(driver.StatementInfo) error { return nil }
func (s *errSink) Columns([]driver.ResultColumn) error       { return nil }
func (s *errSink) Rows([][]any) error                        { return nil }
func (s *errSink) EndResult(driver.ResultSummary) error      { return nil }
func (s *errSink) Notice(string, string) error               { return nil }
func (s *errSink) EndStatement(err error) error {
	if err != nil && s.err == nil {
		s.err = err
	}
	return nil
}

func (r *run) dropExisting(ctx context.Context, order []*TablePlan) {
	objs, err := r.dst.Conn.Objects(ctx, r.dst.Scope)
	if err != nil {
		return
	}
	existing := map[string]string{}
	for _, o := range objs {
		existing[strings.ToLower(o.Name)] = o.Name
	}
	for i := len(order) - 1; i >= 0; i-- {
		name, ok := existing[strings.ToLower(order[i].Target)]
		if !ok {
			continue
		}
		ref := r.targetRef(order[i])
		ref.Name = name
		if err := r.drop(ctx, ref); err != nil {
			r.j.log("warn", "%s: could not drop the existing table: %v", name, err)
		}
	}
}

// drop removes a target table, with CASCADE where the engine has it.
func (r *run) drop(ctx context.Context, ref driver.ObjectRef) error {
	gen := r.dst.Conn.(driver.DDLGenerator)
	stmts, err := gen.DropObjectSQL(ref, true)
	if err != nil {
		if stmts, err = gen.DropObjectSQL(ref, false); err != nil {
			return err
		}
	}
	if err := r.exec(ctx, stmts); err != nil {
		return err
	}
	r.j.log("info", "%s: dropped the existing table", ref.Name)
	return nil
}

// includedColumns are the plan's columns that exist on the target.
func includedColumns(tp *TablePlan) []*ColumnPlan {
	var out []*ColumnPlan
	for _, c := range tp.Columns {
		if c.Include {
			out = append(out, c)
		}
	}
	return out
}

func (r *run) targetName(tp *TablePlan, source string) (string, bool) {
	for _, c := range tp.Columns {
		if c.Source == source && c.Include {
			return c.Target, true
		}
	}
	return "", false
}

// createDef is the table as first created: columns, primary key and
// checks. Indexes and foreign keys follow the data, which loads faster
// without them (SQLite, which rebuilds tables to add keys, gets them now).
func (r *run) createDef(tp *TablePlan) driver.TableDef {
	def := driver.TableDef{Ref: r.targetRef(tp), Comment: tp.Comment, Options: tp.Options, Checks: slices.Clone(tp.Checks)}
	q := export.QuoteFor(r.dst.Info.Dialect)
	for _, c := range includedColumns(tp) {
		col := driver.Column{Name: c.Target, Type: c.Type, Nullable: c.Nullable, Default: c.Default, AutoIncrement: c.AutoIncrement,
			Generated: c.Generated, GeneratedStored: c.GeneratedStored, OnUpdate: c.OnUpdate, Comment: c.Comment, Collation: c.Collation, SRID: c.SRID}
		col.Kind = canonOf(r.dst.engine(), driver.Column{Type: c.Type, BaseType: baseOf(c.Type)}).kind()
		def.Columns = append(def.Columns, driver.ColumnDef{Column: col})
		if len(c.Values) > 0 && r.dst.engine() != MongoDB {
			def.Checks = append(def.Checks, driver.Check{Name: clipIdent(r.dst.engine(), tp.Target+"_"+c.Target+"_check"),
				Expression: q(c.Target) + " IN (" + quoteValues(c.Values, sqlString) + ")"})
		}
	}
	for _, pk := range tp.PrimaryKey {
		if n, ok := r.targetName(tp, pk); ok {
			def.PrimaryKey = append(def.PrimaryKey, n)
			for i := range def.Columns {
				if def.Columns[i].Name == n {
					def.Columns[i].Nullable = false
				}
			}
		}
	}
	if r.dst.engine() == SQLite || r.dst.engine() == MongoDB {
		def.Indexes = r.indexes(tp)
	}
	if r.dst.engine() == SQLite && r.p.Options.ForeignKeys {
		def.ForeignKeys = r.foreignKeys(tp)
	}
	return def
}

func baseOf(t string) string {
	t = strings.ToLower(strings.TrimSpace(t))
	if i := strings.IndexAny(t, "( "); i > 0 {
		return t[:i]
	}
	return t
}

func (r *run) indexes(tp *TablePlan) []driver.Index {
	if !r.p.Options.Indexes {
		return nil
	}
	var out []driver.Index
	for _, ip := range tp.Indexes {
		if !ip.Include {
			continue
		}
		ix := driver.Index{Name: ip.Name, Unique: ip.Unique, Type: ip.Type, Where: ip.Where, Lengths: ip.Lengths, Desc: ip.Desc}
		ok := true
		for _, c := range ip.Columns {
			if strings.HasPrefix(c, "(") {
				ix.Columns = append(ix.Columns, c)
				continue
			}
			n, found := r.targetName(tp, c)
			if !found {
				ok = false
				break
			}
			ix.Columns = append(ix.Columns, n)
		}
		if ok {
			out = append(out, ix)
		}
	}
	return out
}

func (r *run) foreignKeys(tp *TablePlan) []driver.ForeignKey {
	var out []driver.ForeignKey
	for _, fp := range tp.ForeignKeys {
		if !fp.Include {
			continue
		}
		ref, ok := r.byName[fp.RefTable]
		if !ok || r.failed[fp.RefTable] {
			if r.j != nil {
				why := fp.RefTable + " is not part of this migration"
				if ok {
					why = fp.RefTable + " failed to copy"
				}
				r.j.setTable(tp.Source.Name, func(t *TableRun) { t.Notes = append(t.Notes, "foreign key "+fp.Name+" was left out: "+why) })
			}
			continue
		}
		fk := driver.ForeignKey{Name: fp.Name, RefTable: driver.ObjectRef{Schema: r.dst.Scope.Schema, Name: ref.Target}, OnDelete: fp.OnDelete, OnUpdate: fp.OnUpdate}
		if r.dst.engine() == SQLite {
			fk.RefTable.Schema = ""
		}
		good := true
		for _, c := range fp.Columns {
			n, found := r.targetName(tp, c)
			good = good && found
			fk.Columns = append(fk.Columns, n)
		}
		for _, c := range fp.RefColumns {
			n, found := r.targetName(ref, c)
			good = good && found
			fk.RefColumns = append(fk.RefColumns, n)
		}
		if good {
			out = append(out, fk)
		}
	}
	return out
}

// table creates one table and copies its rows.
func (r *run) table(ctx context.Context, tp *TablePlan) error {
	name := tp.Source.Name
	r.j.setTable(name, func(t *TableRun) { t.Status, t.Started = "creating", time.Now().UnixMilli() })
	gen := r.dst.Conn.(driver.DDLGenerator)
	ref := r.targetRef(tp)
	exists := false
	if objs, err := r.dst.Conn.Objects(ctx, r.dst.Scope); err == nil {
		for _, o := range objs {
			if strings.EqualFold(o.Name, tp.Target) {
				exists = true
				ref.Name = o.Name
			}
		}
	}
	keep := false
	switch {
	case exists && r.p.Options.Existing == "replace":
		if err := r.drop(ctx, ref); err != nil {
			return fmt.Errorf("could not drop the existing %s: %w", tp.Target, err)
		}
		exists = false
	case exists && r.p.Options.Existing == "keep":
		keep = true
	case exists:
		return fmt.Errorf("%s already exists on the target; choose to replace or keep existing tables", tp.Target)
	}
	if !exists {
		stmts, err := gen.CreateTableSQL(r.createDef(tp))
		if err != nil {
			return err
		}
		if err := r.exec(ctx, stmts); err != nil {
			return fmt.Errorf("could not create %s: %w", tp.Target, err)
		}
	}
	dt, err := r.dst.Conn.Describe(ctx, ref)
	if err != nil {
		return fmt.Errorf("could not read the new %s: %w", tp.Target, err)
	}
	r.created[name] = dt
	if !r.p.Options.Data {
		r.j.setTable(name, func(t *TableRun) { t.Status = "done"; t.Finished = time.Now().UnixMilli() })
		return nil
	}
	if keep && r.p.Options.ExistingRows == "append" {
		r.appended[name] = true
	}
	r.j.setTable(name, func(t *TableRun) { t.Status = "copying" })
	if err := r.copy(ctx, tp, dt, keep && r.p.Options.ExistingRows == "replace"); err != nil {
		return err
	}
	r.j.setTable(name, func(t *TableRun) { t.Status = "indexing" })
	return nil
}

// copy streams the source rows into the target table.
func (r *run) copy(ctx context.Context, tp *TablePlan, dt *driver.Table, empty bool) error {
	st, err := r.src.Conn.Describe(ctx, tp.Source)
	if err != nil {
		return fmt.Errorf("could not read the source table: %w", err)
	}
	srcCols := map[string]driver.Column{}
	for _, c := range st.Columns {
		srcCols[c.Name] = c
	}
	dstCols := map[string]driver.Column{}
	for _, c := range dt.Columns {
		dstCols[strings.ToLower(c.Name)] = c
	}
	toMongo := r.dst.engine() == MongoDB
	var cols []copyCol
	var names []string
	var hints []driver.Column
	for _, c := range includedColumns(tp) {
		if c.NoCopy || c.Generated != "" {
			continue
		}
		sc, ok := srcCols[c.Source]
		if !ok && c.Source != MongoExtra {
			return fmt.Errorf("the source column %s no longer exists", c.Source)
		}
		sk := canonOf(r.src.engine(), sc)
		if c.Source == MongoExtra {
			sk = canon{T: "object"}
		}
		var dk canon
		if toMongo {
			dk = sk
			hints = append(hints, driver.Column{Name: c.Target, Type: mongoType(sk)})
		} else {
			dc, ok := dstCols[strings.ToLower(c.Target)]
			if !ok {
				return fmt.Errorf("the target has no column %s", c.Target)
			}
			if dc.Generated != "" {
				continue
			}
			dk = canonOf(r.dst.engine(), dc)
			c.Target = dc.Name
		}
		cols = append(cols, copyCol{src: c.Source, conv: newConverter(r.src.engine(), r.dst.engine(), sk, dk), sk: sk, target: c.Target})
		names = append(names, c.Target)
	}
	if len(cols) == 0 {
		return errors.New("no columns to copy")
	}
	sks := make([]canon, len(cols))
	for i, c := range cols {
		sks[i] = c.sk
	}
	r.canons[tp.Source.Name] = sks
	r.copied[tp.Source.Name] = names

	bi := r.dst.Conn.(driver.BulkImporter)
	importTable := dt
	if toMongo {
		importTable = &driver.Table{Ref: dt.Ref, Columns: hints}
	}
	if empty && toMongo {
		if err := r.exec(ctx, []string{"db.getSiblingDB(" + jsString(dt.Ref.Database) + ").getCollection(" + jsString(dt.Ref.Name) + ").deleteMany({})"}); err != nil {
			return fmt.Errorf("could not empty %s: %w", tp.Target, err)
		}
		empty = false
	}
	begin := func(first bool) (driver.RowImporter, error) {
		return bi.BeginImport(ctx, importTable, names, first && empty)
	}
	bq := r.src.Conn.(driver.BrowseQuerier)
	req := driver.BrowseRequest{Ref: tp.Source}
	if r.src.engine() != MongoDB {
		for _, c := range cols {
			req.Columns = append(req.Columns, c.src)
		}
	}
	query, args, err := bq.BrowseQuery(ctx, st, req)
	if err != nil {
		return err
	}
	cp := &copier{cols: cols, dg: newDigest(len(cols)), begin: begin, mongo: r.src.engine() == MongoDB,
		progress: func(n int64) { r.j.setTable(tp.Source.Name, func(t *TableRun) { t.Rows = n }) }}
	cp.ctx = ctx
	if cp.imp, err = begin(true); err != nil {
		return err
	}
	err = r.srcSess.Execute(ctx, query, driver.ExecOptions{Params: args, ReadOnly: true, StopOnError: true}, cp)
	if err == nil {
		err = cp.err
	}
	if err == nil {
		err = cp.flush(true)
	}
	if err != nil {
		cp.imp.Rollback()
		copied := cp.committed
		if copied > 0 {
			return fmt.Errorf("%v (the first %d rows were copied; run again with “replace” to start over)", queryErr(err), copied)
		}
		return queryErr(err)
	}
	r.digests[tp.Source.Name] = cp.dg
	r.j.setTable(tp.Source.Name, func(t *TableRun) { t.Rows = cp.dg.Rows; t.Total = max(t.Total, cp.dg.Rows) })

	// Move auto-numbering past the copied keys.
	var auto []string
	for _, c := range dt.Columns {
		if c.AutoIncrement {
			auto = append(auto, c.Name)
		}
	}
	if len(auto) > 0 && cp.dg.Rows > 0 && !toMongo {
		resets := export.IdentityResets(r.dst.Info.Dialect, dt, export.QualifyFor(r.dst.Info.Dialect)(dt.Ref), auto)
		if err := r.exec(ctx, resets); err != nil {
			r.j.setTable(tp.Source.Name, func(t *TableRun) {
				t.Notes = append(t.Notes, "could not move auto-numbering past the copied rows: "+err.Error())
			})
		}
	}
	r.j.log("info", "%s: copied %d rows", tp.Target, cp.dg.Rows)
	return nil
}

func jsString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

type copyCol struct {
	src    string
	target string
	conv   conv
	sk     canon
}

// copier receives source rows, converts them, and loads them in batches.
type copier struct {
	ctx       context.Context
	cols      []copyCol
	idx       []int // stream position per column, -1 when absent
	unmapped  []int // MongoDB: stream columns not in the plan, folded into the extra column
	names     []string
	state     int
	imp       driver.RowImporter
	begin     func(first bool) (driver.RowImporter, error)
	batch     [][]any
	bytes     int
	since     int
	committed int64
	dg        *digest
	mongo     bool
	progress  func(int64)
	last      time.Time
	err       error
}

func (c *copier) FullValues() bool                          { return true }
func (c *copier) BeginStatement(driver.StatementInfo) error { return nil }
func (c *copier) Notice(string, string) error               { return nil }
func (c *copier) EndResult(driver.ResultSummary) error {
	if c.state == 1 {
		c.state = 2
	}
	return nil
}
func (c *copier) EndStatement(err error) error {
	if err != nil && c.err == nil {
		c.err = err
	}
	return nil
}

func (c *copier) Columns(rc []driver.ResultColumn) error {
	if c.state != 0 {
		c.state = 2
		return nil
	}
	c.state = 1
	pos := map[string]int{}
	c.names = make([]string, len(rc))
	for i, col := range rc {
		pos[col.Name] = i
		c.names[i] = col.Name
	}
	c.idx = make([]int, len(c.cols))
	planned := map[string]bool{}
	for i, col := range c.cols {
		planned[col.src] = true
		if p, ok := pos[col.src]; ok {
			c.idx[i] = p
		} else {
			c.idx[i] = -1
		}
	}
	if c.mongo {
		for i, col := range rc {
			if !planned[col.Name] && col.Name != MongoExtra {
				c.unmapped = append(c.unmapped, i)
			}
		}
	}
	return nil
}

// extra merges the fields a MongoDB stream carried outside the plan.
func (c *copier) extra(row []any, own any) any {
	d := driver.Doc{Values: map[string]any{}}
	if doc, ok := own.(driver.Doc); ok {
		d.Keys = append(d.Keys, doc.Keys...)
		for k, v := range doc.Values {
			d.Values[k] = v
		}
	}
	for _, i := range c.unmapped {
		if i < len(row) && row[i] != nil {
			d.Keys = append(d.Keys, c.names[i])
			d.Values[c.names[i]] = row[i]
		}
	}
	if len(d.Keys) == 0 {
		return nil
	}
	return d
}

func (c *copier) Rows(rows [][]any) error {
	if c.state != 1 {
		return nil
	}
	norm := make([]string, len(c.cols))
	for _, row := range rows {
		out := make([]any, len(c.cols))
		for i, col := range c.cols {
			var v any
			if p := c.idx[i]; p >= 0 && p < len(row) {
				v = row[p]
			}
			if col.src == MongoExtra {
				v = c.extra(row, v)
			}
			norm[i] = normalize(v, col.sk)
			cv, err := col.conv(v)
			if err != nil {
				return fmt.Errorf("row %d, column %s: %w", c.dg.Rows+1, col.src, err)
			}
			out[i] = cv
		}
		c.dg.add(norm)
		c.batch = append(c.batch, out)
		c.bytes += driver.RowSize(row)
		if len(c.batch) >= batchRows || c.bytes >= batchBytes {
			if err := c.flush(false); err != nil {
				return err
			}
		}
	}
	if time.Since(c.last) > 300*time.Millisecond {
		c.last = time.Now()
		c.progress(c.dg.Rows)
	}
	return nil
}

func (c *copier) flush(final bool) error {
	if len(c.batch) > 0 {
		if err := c.imp.Insert(c.ctx, c.batch); err != nil {
			return err
		}
		c.since += len(c.batch)
		c.batch, c.bytes = c.batch[:0], 0
	}
	if c.since >= commitEvery || final {
		if err := c.imp.Commit(); err != nil {
			return err
		}
		c.committed += int64(c.since)
		c.since = 0
		if !final {
			imp, err := c.begin(false)
			if err != nil {
				return err
			}
			c.imp = imp
		} else {
			c.imp = noopImporter{}
		}
	}
	return nil
}

type noopImporter struct{}

func (noopImporter) Insert(context.Context, [][]any) error { return nil }
func (noopImporter) Commit() error                         { return nil }
func (noopImporter) Rollback() error                       { return nil }

// keys adds indexes and foreign keys once the rows are in.
func (r *run) keys(ctx context.Context, tp *TablePlan) {
	name := tp.Source.Name
	r.j.setTable(name, func(t *TableRun) {
		if t.Status != "done" {
			t.Status = "indexing"
		}
	})
	gen := r.dst.Conn.(driver.DDLGenerator)
	ref := r.created[name].Ref
	// Each key is its own change, built on the table as it is now, so a
	// failure only costs that key.
	for _, ix := range r.indexes(tp) {
		from, err := r.dst.Conn.Describe(ctx, ref)
		if err != nil {
			r.note(name, "could not read the table to add its keys: "+err.Error())
			return
		}
		to := defFrom(from)
		to.Indexes = append(to.Indexes, ix)
		if err := r.alter(ctx, gen, from, to); err != nil {
			r.note(name, "index "+ix.Name+" was not created: "+err.Error())
		}
	}
	if !r.p.Options.ForeignKeys || r.dst.engine() == MongoDB {
		return
	}
	for _, fk := range r.foreignKeys(tp) {
		from, err := r.dst.Conn.Describe(ctx, ref)
		if err != nil {
			r.note(name, "could not read the table to add its keys: "+err.Error())
			return
		}
		to := defFrom(from)
		to.ForeignKeys = append(to.ForeignKeys, fk)
		if err := r.alter(ctx, gen, from, to); err != nil {
			r.note(name, "foreign key "+fk.Name+" was not created: "+err.Error())
		}
	}
}

// defFrom is a table's current definition, to change one thing at a time.
func defFrom(t *driver.Table) driver.TableDef {
	def := driver.TableDef{Ref: t.Ref, PrimaryKey: t.PrimaryKey, Checks: t.Checks, Comment: t.Comment, Options: t.Options,
		ForeignKeys: slices.Clone(t.ForeignKeys)}
	for _, c := range t.Columns {
		def.Columns = append(def.Columns, driver.ColumnDef{Column: c, OriginalName: c.Name})
	}
	for _, ix := range t.Indexes {
		if !ix.Primary {
			def.Indexes = append(def.Indexes, ix)
		}
	}
	return def
}

func (r *run) alter(ctx context.Context, gen driver.DDLGenerator, from *driver.Table, to driver.TableDef) error {
	stmts, err := gen.AlterTableSQL(from, to)
	if err != nil {
		return err
	}
	return r.exec(ctx, stmts)
}

func (r *run) note(table, text string) {
	r.j.setTable(table, func(t *TableRun) { t.Notes = append(t.Notes, text) })
	r.j.log("warn", "%s: %s", table, text)
}

// objectsOf returns the definitions to copy before (types, sequences) or
// after the tables.
func (r *run) objectsOf(before bool) []ObjectNote {
	var out []ObjectNote
	for _, o := range r.p.Objects {
		pre := o.Kind == "type" || o.Kind == "sequence"
		if pre == before {
			out = append(out, o)
		}
	}
	return out
}

var definerRe = regexp.MustCompile(`(?i)\s+DEFINER\s*=\s*(\x60[^\x60]*\x60|'[^']*'|\S+)@(\x60[^\x60]*\x60|'[^']*'|\S+)`)

// definitions recreates objects from their source, retrying those that
// failed while others succeed (views that depend on views).
func (r *run) definitions(ctx context.Context, objs []ObjectNote) {
	def, ok := r.src.Conn.(driver.Definer)
	if !ok {
		return
	}
	pending := map[string]string{}
	var order []string
	for _, o := range objs {
		key := o.Kind + ":" + o.Name
		s, err := def.Definition(ctx, driver.ObjectRef{Database: r.src.Scope.Database, Schema: r.src.Scope.Schema, Name: o.Name, Kind: o.Kind})
		if err != nil || strings.TrimSpace(s) == "" {
			r.setObject(o, "failed", "could not read its definition")
			continue
		}
		s = strings.TrimSpace(s)
		if family(r.src.engine()) == MySQL {
			s = definerRe.ReplaceAllString(s, "")
			if db := r.src.Scope.Database; db != "" && db != r.dst.Scope.Database {
				s = strings.ReplaceAll(s, export.QuoteFor(r.src.Info.Dialect)(db)+".", "")
			}
		}
		if src, dst := r.src.Scope.Schema, r.dst.Scope.Schema; family(r.src.engine()) == Postgres && src != "" && dst != "" && src != dst {
			s = requalify(s, src, dst)
		}
		pending[key] = strings.TrimSuffix(s, ";")
		order = append(order, key)
	}
	errs := map[string]string{}
	for pass := 0; pass < 5 && len(pending) > 0; pass++ {
		progress := false
		for _, key := range order {
			s, ok := pending[key]
			if !ok {
				continue
			}
			if err := r.exec(ctx, []string{s}); err != nil {
				errs[key] = err.Error()
				continue
			}
			delete(pending, key)
			progress = true
			kind, name, _ := strings.Cut(key, ":")
			r.setObject(ObjectNote{Name: name, Kind: kind}, "done", "")
		}
		if !progress {
			break
		}
	}
	for key := range pending {
		kind, name, _ := strings.Cut(key, ":")
		r.setObject(ObjectNote{Name: name, Kind: kind}, "failed", errs[key])
		r.j.log("warn", "%s %s was not created: %s", kind, name, errs[key])
	}
}

// requalify moves schema-qualified names (public.x or "public"."x") in a
// PostgreSQL definition to another schema.
func requalify(def, from, to string) string {
	re := regexp.MustCompile(`(^|[^A-Za-z0-9_$."])("` + regexp.QuoteMeta(from) + `"|` + regexp.QuoteMeta(from) + `)\.`)
	return re.ReplaceAllString(def, `${1}"`+strings.ReplaceAll(to, `"`, `""`)+`".`)
}

func (r *run) setObject(o ObjectNote, status, errText string) {
	r.j.update(func(v *JobView) {
		for _, x := range v.Objects {
			if x.Name == o.Name && x.Kind == o.Kind {
				x.Status, x.Error = status, errText
			}
		}
	})
}

// verify compares row counts and, when asked, every value.
func (r *run) verify(ctx context.Context, tp *TablePlan) {
	name := tp.Source.Name
	dg := r.digests[name]
	if dg == nil {
		return
	}
	r.j.setTable(name, func(t *TableRun) { t.Status = "verifying" })
	dt := r.created[name]
	v := &Verification{SourceRows: dg.Rows, Contents: "skipped"}
	n, err := r.dst.Conn.Count(ctx, driver.BrowseRequest{Ref: dt.Ref})
	if err != nil {
		v.Note = "could not count the target rows: " + err.Error()
	}
	v.TargetRows = n.Rows
	v.Counts = map[bool]string{true: "match", false: "differ"}[v.TargetRows == v.SourceRows]
	switch {
	case r.appended[name]:
		v.Counts, v.Note = "", "rows were added to an existing table, so its contents were not compared"
	case r.p.Options.Verify != "contents":
	case v.Counts == "differ":
		v.Note = "the row counts differ, so the contents were not compared"
	default:
		got, err := r.readBack(ctx, tp, dt)
		if err != nil {
			v.Note = "could not read the rows back: " + err.Error()
			break
		}
		v.Contents = "match"
		for i, s := range dg.Sums {
			if i < len(got.Sums) && got.Sums[i] != s {
				v.Contents = "differ"
				v.Columns = append(v.Columns, r.copied[name][i])
			}
		}
	}
	r.j.setTable(name, func(t *TableRun) {
		t.Verify, t.Status, t.Finished = v, "done", time.Now().UnixMilli()
	})
	switch {
	case v.Counts == "differ":
		r.j.log("warn", "%s: %d rows on the source, %d on the target", tp.Target, v.SourceRows, v.TargetRows)
	case v.Contents == "differ":
		r.j.log("warn", "%s: values differ in %s", tp.Target, strings.Join(v.Columns, ", "))
	}
}

// readBack digests the target's copy with the source's normalization.
func (r *run) readBack(ctx context.Context, tp *TablePlan, dt *driver.Table) (*digest, error) {
	names := r.copied[tp.Source.Name]
	bq, ok := r.dst.Conn.(driver.BrowseQuerier)
	if !ok {
		return nil, errors.New("the target cannot be read back")
	}
	query, args, err := bq.BrowseQuery(ctx, dt, driver.BrowseRequest{Ref: dt.Ref, Columns: names})
	if err != nil {
		return nil, err
	}
	rd := &reader{names: names, canons: r.canons[tp.Source.Name], dg: newDigest(len(names))}
	sess, err := r.dst.Conn.NewSession(ctx, r.dst.Scope)
	if err != nil {
		return nil, err
	}
	defer sess.Close()
	if err := sess.Execute(ctx, query, driver.ExecOptions{Params: args, ReadOnly: true, StopOnError: true}, rd); err != nil {
		return nil, queryErr(err)
	}
	if rd.err != nil {
		return nil, queryErr(rd.err)
	}
	return rd.dg, nil
}

type reader struct {
	names  []string
	canons []canon
	idx    []int
	state  int
	dg     *digest
	err    error
}

func (d *reader) FullValues() bool                          { return true }
func (d *reader) BeginStatement(driver.StatementInfo) error { return nil }
func (d *reader) Notice(string, string) error               { return nil }
func (d *reader) EndResult(driver.ResultSummary) error {
	if d.state == 1 {
		d.state = 2
	}
	return nil
}
func (d *reader) EndStatement(err error) error {
	if err != nil && d.err == nil {
		d.err = err
	}
	return nil
}
func (d *reader) Columns(rc []driver.ResultColumn) error {
	if d.state != 0 {
		d.state = 2
		return nil
	}
	d.state = 1
	pos := map[string]int{}
	for i, c := range rc {
		pos[strings.ToLower(c.Name)] = i
	}
	d.idx = make([]int, len(d.names))
	for i, n := range d.names {
		if p, ok := pos[strings.ToLower(n)]; ok {
			d.idx[i] = p
		} else {
			d.idx[i] = -1
		}
	}
	return nil
}
func (d *reader) Rows(rows [][]any) error {
	if d.state != 1 {
		return nil
	}
	norm := make([]string, len(d.names))
	for _, row := range rows {
		for i := range d.names {
			var v any
			if p := d.idx[i]; p >= 0 && p < len(row) {
				v = row[p]
			}
			norm[i] = normalize(v, d.canons[i])
		}
		d.dg.add(norm)
	}
	return nil
}
