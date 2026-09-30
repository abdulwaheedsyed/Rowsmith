package schedule

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"rowsmith/internal/driver"
	"rowsmith/internal/export"
	"rowsmith/internal/id"
	"rowsmith/internal/mail"
	"rowsmith/internal/session"
	"rowsmith/internal/sqlsplit"
	"rowsmith/internal/store"
	"rowsmith/internal/vault"
)

const (
	MinInterval    = 5 * time.Minute
	runTimeout     = 15 * time.Minute
	maxFailures    = 5   // consecutive failures before a schedule pauses itself
	keepRuns       = 200 // history kept per schedule
	concurrentRuns = 3
)

// Settings are read at every run, so admin changes apply at once.
type Settings struct {
	Mail            mail.Config
	Domains         []string // external recipient domains; "*" allows any address
	Webhooks        bool
	PrivateWebhooks bool
	RetentionDays   int
	MaxAttachBytes  int64
}

// Runner executes due schedules in the background.
type Runner struct {
	Store    *store.Store
	Vault    *vault.Vault
	Sessions *session.Manager
	Log      *slog.Logger
	Dir      string // result files
	BaseURL  string // the app's public URL ending in "/", for links; may be ""
	Settings func(ctx context.Context) Settings

	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	slots   chan struct{}
	mu      sync.Mutex
	running map[string]context.CancelFunc
}

// Validate parses a timetable and checks that it runs, and not too often.
func Validate(expr, tz string) (*Cron, *time.Location, error) {
	c, err := ParseCron(expr)
	if err != nil {
		return nil, nil, err
	}
	loc, err := time.LoadLocation(strOr(tz, "UTC"))
	if err != nil {
		return nil, nil, fmt.Errorf("unknown time zone %q", tz)
	}
	now := time.Now()
	if c.Next(now, loc).IsZero() {
		return nil, nil, errors.New("this schedule never runs: no date matches it")
	}
	if g := c.MinGap(now, loc); g > 0 && g < MinInterval {
		return nil, nil, fmt.Errorf("a schedule can run at most every %d minutes", int(MinInterval.Minutes()))
	}
	return c, loc, nil
}

// NextRun returns the next run time in Unix milliseconds, or 0.
func NextRun(expr, tz string, after time.Time) int64 {
	c, err := ParseCron(expr)
	if err != nil {
		return 0
	}
	loc, err := time.LoadLocation(strOr(tz, "UTC"))
	if err != nil {
		return 0
	}
	if t := c.Next(after, loc); !t.IsZero() {
		return t.UnixMilli()
	}
	return 0
}

// CheckStatements allows only statements that read, plus session settings.
func CheckStatements(info driver.Info, body string) error {
	if strings.TrimSpace(body) == "" {
		return errors.New("enter a query to schedule")
	}
	if !info.Caps.SQL {
		return nil // document stores: the read-only session refuses writes
	}
	stmts := sqlsplit.Split(body, sqlsplit.Dialect(info.Dialect))
	reads := 0
	for i, st := range stmts {
		switch st.Kind {
		case driver.StmtRead:
			reads++
		case driver.StmtSession:
		default:
			return fmt.Errorf("scheduled queries only read data, but statement %d (line %d) may change something (%s)", i+1, st.Line, st.Kind)
		}
	}
	if reads == 0 {
		return errors.New("the query has no statement that returns rows")
	}
	return nil
}

// Start recovers from an unclean stop and begins polling for due runs.
func (r *Runner) Start() {
	r.ctx, r.cancel = context.WithCancel(context.Background())
	r.slots = make(chan struct{}, concurrentRuns)
	r.running = map[string]context.CancelFunc{}
	_ = os.MkdirAll(r.Dir, 0o700)
	if n, err := r.Store.RecoverSchedules(r.ctx); err == nil && n > 0 {
		r.Log.Warn("marked interrupted schedule runs as failed", "runs", n)
	}
	r.removeOrphans()
	go r.loop()
}

// Stop cancels running work and waits briefly for it to record itself.
func (r *Runner) Stop() {
	if r.cancel == nil {
		return
	}
	r.cancel()
	done := make(chan struct{})
	go func() { r.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
	}
}

func (r *Runner) loop() {
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	janitor := time.NewTicker(10 * time.Minute)
	defer janitor.Stop()
	r.tick()
	r.expireFiles()
	for {
		select {
		case <-r.ctx.Done():
			return
		case <-tick.C:
			r.tick()
		case <-janitor.C:
			r.expireFiles()
		}
	}
}

func (r *Runner) tick() {
	now := time.Now()
	due, err := r.Store.DueSchedules(r.ctx, now.UnixMilli(), 20)
	if err != nil {
		if r.ctx.Err() == nil {
			r.Log.Error("could not read due schedules", "err", err)
		}
		return
	}
	for _, x := range due {
		// Next run counts from now: a schedule missed while Rowsmith was
		// down runs once, not once per missed slot.
		next := NextRun(x.Cron, x.Timezone, now)
		ok, err := r.Store.ClaimSchedule(r.ctx, x.ID, now.UnixMilli(), next, true)
		if err != nil || !ok {
			continue
		}
		if next == 0 {
			_ = r.Store.PauseSchedule(r.ctx, x.ID, "Paused: the timetable or time zone is no longer valid.")
			continue
		}
		run := &store.ScheduleRun{ID: id.New(), ScheduleID: x.ID, Trigger: "schedule", StartedAt: store.Now(), Status: "running"}
		if err := r.Store.CreateRun(r.ctx, run); err != nil {
			_ = r.Store.FinishSchedule(r.ctx, x.ID, store.ScheduleOutcome{At: store.Now(), Status: x.LastStatus, Failures: x.Failures, KeepState: true})
			continue
		}
		r.wg.Add(1)
		go r.run(x, run)
	}
}

// RunNow starts a run outside the timetable and returns its ID.
func (r *Runner) RunNow(ctx context.Context, scheduleID, userID string) (string, error) {
	x, err := r.Store.ScheduleByID(ctx, scheduleID)
	if err != nil {
		return "", err
	}
	ok, err := r.Store.ClaimSchedule(ctx, x.ID, store.Now(), 0, false)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", errors.New("this schedule is running already")
	}
	run := &store.ScheduleRun{ID: id.New(), ScheduleID: x.ID, Trigger: "manual", TriggeredBy: userID, StartedAt: store.Now(), Status: "running"}
	if err := r.Store.CreateRun(ctx, run); err != nil {
		_ = r.Store.FinishSchedule(ctx, x.ID, store.ScheduleOutcome{At: x.LastRunAt, Status: x.LastStatus, Failures: x.Failures, KeepState: true})
		return "", err
	}
	r.wg.Add(1)
	go r.run(x, run)
	return run.ID, nil
}

// Cancel stops a schedule's current run, e.g. before deleting it.
func (r *Runner) Cancel(scheduleID string) {
	r.mu.Lock()
	if c, ok := r.running[scheduleID]; ok {
		c()
	}
	r.mu.Unlock()
}

// pauseError fails a run and pauses its schedule: retrying cannot help.
type pauseError struct{ msg string }

func (e pauseError) Error() string { return e.msg }

type outcome struct {
	obs       Observation
	preview   [][]string
	total     int64 // rows the query returned
	written   int64 // rows in the file
	truncated bool
	ms        int64
	file      *resultFile
}

type resultFile struct {
	name, ctype string
	size        int64
	key         []byte
}

func (r *Runner) run(x *store.Schedule, run *store.ScheduleRun) {
	defer r.wg.Done()
	select {
	case r.slots <- struct{}{}:
		defer func() { <-r.slots }()
	case <-r.ctx.Done():
		r.fail(context.Background(), x, run, ParseConfig(x.Config), errors.New("Rowsmith stopped before this could run"))
		return
	}
	ctx, cancel := context.WithTimeout(r.ctx, runTimeout)
	defer cancel()
	r.mu.Lock()
	r.running[x.ID] = cancel
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.running, x.ID)
		r.mu.Unlock()
	}()

	cfg := ParseConfig(x.Config)
	out, err := r.execute(ctx, x, cfg, run, true)
	// Recording must survive a cancelled run.
	rec := context.WithoutCancel(ctx)
	if err == nil {
		fired, observed, aerr := cfg.Alert.Check(out.obs, x.LastHash)
		if aerr != nil {
			r.removeFile(run.ID)
			err = aerr
		} else {
			r.succeed(rec, x, run, cfg, out, fired, observed)
			return
		}
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		err = fmt.Errorf("stopped after %d minutes; make the query faster or return fewer rows", int(runTimeout.Minutes()))
	} else if r.ctx.Err() != nil {
		err = errors.New("stopped: Rowsmith was shutting down, or the schedule was deleted")
	}
	r.fail(rec, x, run, cfg, err)
}

// execute runs the query as the schedule's owner and writes the result
// file (when keep is set).
func (r *Runner) execute(ctx context.Context, x *store.Schedule, cfg Config, run *store.ScheduleRun, keep bool) (*outcome, error) {
	owner, err := r.Store.UserByID(ctx, x.OwnerID)
	if err != nil {
		return nil, pauseError{"Paused: the person who created this schedule no longer exists."}
	}
	if owner.Disabled {
		return nil, pauseError{fmt.Sprintf("Paused: %s's account is disabled.", owner.Name)}
	}
	conn, err := r.Store.ConnectionByID(ctx, x.ConnectionID)
	if err != nil {
		return nil, pauseError{"Paused: the connection was deleted."}
	}
	acc, err := r.Store.AccessFor(ctx, conn, owner.ID)
	if err != nil {
		return nil, err
	}
	if acc == store.AccessNone {
		return nil, pauseError{fmt.Sprintf("Paused: %s no longer has access to %s.", owner.Name, conn.Name)}
	}
	return r.query(ctx, conn, x.Database, x.Schema, x.Name, x.Body, cfg, run, keep)
}

func (r *Runner) query(ctx context.Context, conn *store.Connection, database, schema, name, body string, cfg Config, run *store.ScheduleRun, keep bool) (*outcome, error) {
	openCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	lease, err := r.Sessions.Acquire(openCtx, conn, true) // scheduled queries only ever read
	cancel()
	if err != nil {
		return nil, fmt.Errorf("could not connect to %s: %v", conn.Name, err)
	}
	defer lease.Release()
	info := lease.Driver.Info()
	if err := CheckStatements(info, body); err != nil {
		return nil, pauseError{"Paused: " + err.Error()}
	}
	sess, err := lease.Conn.NewSession(ctx, driver.Scope{Database: database, Schema: schema})
	if err != nil {
		return nil, fmt.Errorf("could not open a session on %s: %v", conn.Name, err)
	}
	defer sess.Close()

	out := &outcome{}
	var w export.Writer = discard{}
	var finish func() error
	var fail func()
	if keep {
		f, fw, fin, abort, err := r.createFile(run, name, cfg)
		if err != nil {
			return nil, err
		}
		out.file, finish, fail = f, fin, abort
		ew, err := export.New(fw, export.Options{Format: cfg.Output.Format, Header: true, SheetName: sheetName(name),
			Documents: info.Caps.Documents, Dialect: info.Dialect})
		if err != nil {
			abort()
			return nil, err
		}
		w = ew
	}
	sink := &capture{Sink: &export.Sink{W: w}, max: max(cfg.Output.Preview, 10), h: sha256.New()}
	started := time.Now()
	err = sess.Execute(ctx, body, driver.ExecOptions{MaxRows: cfg.Output.MaxRows, ReadOnly: true, StopOnError: true}, sink)
	if err == nil {
		err = sink.Err()
	}
	if err == nil && !sink.Wrote() {
		err = errors.New("the query returned no result set")
	}
	if cerr := w.Close(); err == nil {
		err = cerr
	}
	if keep {
		if err == nil {
			err = finish()
		} else {
			fail()
		}
	}
	if err != nil {
		var qe *driver.QueryError
		if errors.As(err, &qe) {
			return nil, errors.New(qe.Message)
		}
		return nil, err
	}
	out.ms = time.Since(started).Milliseconds()
	out.written = sink.Count()
	out.total = max(sink.total, out.written)
	out.truncated = sink.truncated || out.total > out.written
	out.preview = sink.preview
	out.obs = Observation{Rows: out.total, Columns: sink.cols, First: sink.first, Hash: hex.EncodeToString(sink.h.Sum(nil))}
	return out, nil
}

// createFile opens an encrypted result file. Writes go through gzip when
// asked; size counts what a download will contain.
func (r *Runner) createFile(run *store.ScheduleRun, name string, cfg Config) (*resultFile, io.Writer, func() error, func(), error) {
	path := filepath.Join(r.Dir, run.ID)
	fh, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("could not create the result file: %v", err)
	}
	key := NewFileKey()
	sw, err := SealWriter(fh, key, run.FileAAD())
	if err != nil {
		fh.Close()
		os.Remove(path)
		return nil, nil, nil, nil, err
	}
	ext := export.Extension(cfg.Output.Format)
	f := &resultFile{name: fileName(name, ext, cfg.Output.Gzip), ctype: export.ContentType(cfg.Output.Format), key: key}
	cw := &counter{w: sw}
	var dst io.Writer = cw
	var gz *gzip.Writer
	if cfg.Output.Gzip {
		gz = gzip.NewWriter(cw)
		dst = gz
		f.ctype = "application/gzip"
	}
	abort := func() {
		fh.Close()
		os.Remove(path)
	}
	finish := func() error {
		if gz != nil {
			if err := gz.Close(); err != nil {
				abort()
				return err
			}
		}
		if err := sw.Close(); err != nil {
			abort()
			return err
		}
		if err := fh.Close(); err != nil {
			os.Remove(path)
			return err
		}
		f.size = cw.n
		return nil
	}
	return f, dst, finish, abort, nil
}

func (r *Runner) succeed(ctx context.Context, x *store.Schedule, run *store.ScheduleRun, cfg Config, out *outcome, fired bool, observed string) {
	set := r.Settings(ctx)
	run.FinishedAt = store.Now()
	run.RowCount, run.Truncated, run.Observed = out.total, out.truncated, observed
	if f := out.file; f != nil {
		sealed, err := r.Vault.Seal(f.key, run.FileAAD())
		if err == nil {
			run.FileName, run.FileType, run.FileSize, run.FileKey = f.name, f.ctype, f.size, sealed
			days := set.RetentionDays
			if days <= 0 {
				days = 14
			}
			run.FileExpires = time.Now().Add(time.Duration(days) * 24 * time.Hour).UnixMilli()
		} else {
			r.removeFile(run.ID)
		}
	}
	run.Status = "ok"
	deliver := true
	d := deliveryRecord{}
	if cfg.Alert.Kind != "" {
		run.Status = "quiet"
		deliver = fired
		if fired {
			run.Status = "alert"
			if cfg.Alert.Edge && x.LastAlert {
				deliver = false
				d.Note = "Not sent again: the condition already held on the previous run."
			}
		} else {
			d.Note = "Nothing sent: the condition did not hold."
		}
	}
	if deliver {
		kind := "result"
		if fired {
			kind = "alert"
		}
		r.deliver(ctx, x, cfg, set, run, out, kind, &d)
	}
	b, _ := json.Marshal(d)
	run.Delivery = string(b)
	if err := r.Store.FinishRun(ctx, run); err != nil {
		r.Log.Error("could not record a schedule run", "schedule", x.ID, "err", err)
	}
	_ = r.Store.FinishSchedule(ctx, x.ID, store.ScheduleOutcome{At: run.StartedAt, Status: run.Status, Hash: out.obs.Hash, Alert: fired, Failures: 0})
	r.prune(ctx, x.ID)
	_ = r.Store.Audit(ctx, x.OwnerID, "", "schedule.ran", x.Name, map[string]any{"schedule": x.ID, "trigger": run.Trigger,
		"status": run.Status, "rows": run.RowCount, "emailed": d.Emailed, "webhook": d.Webhook == "sent"})
}

func (r *Runner) fail(ctx context.Context, x *store.Schedule, run *store.ScheduleRun, cfg Config, err error) {
	run.FinishedAt = store.Now()
	run.Status = "failed"
	run.Error = clip(err.Error(), 4000)
	failures := x.Failures + 1
	var pe pauseError
	paused := errors.As(err, &pe)
	reason := ""
	if paused {
		reason = pe.msg
	} else if failures >= maxFailures && run.Trigger == "schedule" {
		paused = true
		reason = fmt.Sprintf("Paused after %d failed runs in a row. Fix the problem, then resume it.", failures)
	}
	d := deliveryRecord{}
	if cfg.Delivery.OnFailure {
		kind := "failure"
		if paused {
			kind = "paused"
		}
		r.notifyOwner(ctx, x, run, kind, reason, &d)
	}
	b, _ := json.Marshal(d)
	run.Delivery = string(b)
	_ = r.Store.FinishRun(ctx, run)
	_ = r.Store.FinishSchedule(ctx, x.ID, store.ScheduleOutcome{At: run.StartedAt, Status: "failed", Failures: failures,
		Disable: paused, Reason: reason, KeepState: true})
	r.prune(ctx, x.ID)
	_ = r.Store.Audit(ctx, x.OwnerID, "", "schedule.failed", x.Name, map[string]any{"schedule": x.ID, "trigger": run.Trigger,
		"error": clip(run.Error, 300), "paused": paused})
}

// deliveryRecord is stored with each run and shown in its history.
type deliveryRecord struct {
	Emailed    []string `json:"emailed,omitempty"`
	Skipped    []string `json:"skipped,omitempty"` // not allowed by the recipient policy
	EmailError string   `json:"emailError,omitempty"`
	Attached   bool     `json:"attached,omitempty"`
	Webhook    string   `json:"webhook,omitempty"` // sent, off, or an error
	Note       string   `json:"note,omitempty"`
}

func (r *Runner) link(x *store.Schedule) string {
	if r.BaseURL == "" {
		return ""
	}
	return r.BaseURL + "schedules/" + x.ID
}

func (r *Runner) connLabel(ctx context.Context, x *store.Schedule) string {
	label := x.Connection
	if c, err := r.Store.ConnectionByID(ctx, x.ConnectionID); err == nil {
		if d, ok := driver.Get(c.Driver); ok {
			label += " (" + d.Info().Name + ")"
		}
	}
	if scope := strings.Trim(x.Database+"."+x.Schema, "."); scope != "" {
		label += " · " + scope
	}
	return label
}

func ranAt(x *store.Schedule, ms int64) string {
	loc, err := time.LoadLocation(strOr(x.Timezone, "UTC"))
	if err != nil {
		loc = time.UTC
	}
	return time.UnixMilli(ms).In(loc).Format("Mon 2 Jan 2006, 15:04 MST")
}

func (r *Runner) deliver(ctx context.Context, x *store.Schedule, cfg Config, set Settings, run *store.ScheduleRun, out *outcome, kind string, d *deliveryRecord) {
	url := r.link(x)
	headline := plural(out.total, "row", "rows")
	if out.truncated {
		headline += fmt.Sprintf(", the first %s in the file", groupDigits(out.written))
	}
	cond := cfg.Alert.Describe()
	if kind == "alert" {
		headline = "The condition was met: " + cond + "."
	}

	// Email.
	to, skipped := r.allowed(ctx, cfg.Delivery.Emails, set)
	d.Skipped = skipped
	if len(to) > 0 {
		data := emailData{Kind: kind, Name: x.Name, Headline: headline, Connection: r.connLabel(ctx, x), RanAt: ranAt(x, run.StartedAt),
			Rows: plural(out.total, "row", "rows"), Duration: tookText(out.ms),
			URL: url, Why: fmt.Sprintf("Sent because %s scheduled “%s” in Rowsmith.", x.OwnerName, x.Name)}
		if cfg.Alert.Kind != "" {
			data.Condition, data.Observed = cond, run.Observed
		}
		if cfg.Output.Preview > 0 && len(out.obs.Columns) > 0 {
			cols := out.obs.Columns
			if len(cols) > 12 {
				data.MoreCols = len(cols) - 12
				cols = cols[:12]
			}
			data.Columns = cols
			rows := out.preview[:min(len(out.preview), cfg.Output.Preview)]
			for _, row := range rows {
				data.Preview = append(data.Preview, row[:min(len(row), len(cols))])
			}
			if n := out.total - int64(len(rows)); n > 0 {
				data.MoreRows = plural(n, "more row", "more rows") + " not shown"
			}
		}
		msg := mail.Message{To: to, Subject: subject(kind, x.Name, out.total, run.Observed)}
		if f := out.file; f != nil && run.FileKey != "" {
			limit := set.MaxAttachBytes
			if limit <= 0 {
				limit = 10 << 20
			}
			switch {
			case !cfg.Output.Attach:
				data.FileNote = fmt.Sprintf("The result file (%s) is in Rowsmith for %d days.", f.name, retentionDays(set))
			case f.size <= limit:
				runID, key := run.ID, f.key
				msg.Attachments = []mail.Attachment{{Name: f.name, ContentType: f.ctype, Open: func() (io.ReadCloser, error) { return r.openFile(runID, key) }}}
				data.FileNote = fmt.Sprintf("Attached: %s (%s).", f.name, humanBytes(f.size))
				d.Attached = true
			default:
				data.FileNote = fmt.Sprintf("The file (%s) is too large to attach; download it from Rowsmith.", humanBytes(f.size))
			}
		}
		html, text, err := renderEmail(data)
		if err == nil {
			msg.HTML, msg.Text = html, text
			sctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
			err = mail.Send(sctx, set.Mail, msg)
			cancel()
		}
		if err != nil {
			d.EmailError = err.Error()
		} else {
			d.Emailed = to
		}
	}

	// Webhook.
	if x.Webhook != "" {
		if !set.Webhooks {
			d.Webhook = "off: an admin turned webhooks off"
			return
		}
		raw, err := r.Vault.Open(x.Webhook, x.WebhookAAD())
		if err != nil {
			d.Webhook = "could not read the saved webhook"
			return
		}
		ev := Event{Event: kind, Schedule: EventRef{ID: x.ID, Name: x.Name, URL: url}, Connection: x.Connection,
			RanAt: time.UnixMilli(run.StartedAt).UTC().Format(time.RFC3339), Rows: out.total}
		ev.Text = fmt.Sprintf("%s: %s", x.Name, headline)
		if kind == "alert" {
			ev.Text = fmt.Sprintf("Alert: %s. %s, saw %s.", x.Name, capitalize(cond), run.Observed)
			ev.Condition, ev.Observed = cond, run.Observed
		}
		if cfg.Output.Preview > 0 {
			ev.Columns = out.obs.Columns
			ev.Preview = out.preview[:min(len(out.preview), cfg.Output.Preview)]
		}
		wctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err = postWebhook(wctx, string(raw), set.PrivateWebhooks, ev)
		cancel()
		if err != nil {
			d.Webhook = err.Error()
		} else {
			d.Webhook = "sent"
		}
	}
}

// notifyOwner tells the schedule's owner that a run failed.
func (r *Runner) notifyOwner(ctx context.Context, x *store.Schedule, run *store.ScheduleRun, kind, reason string, d *deliveryRecord) {
	set := r.Settings(ctx)
	headline := "The scheduled query did not finish."
	if kind == "paused" {
		headline = reason
	}
	data := emailData{Kind: kind, Name: x.Name, Headline: headline, Connection: r.connLabel(ctx, x), RanAt: ranAt(x, run.StartedAt),
		Error: run.Error, URL: r.link(x), Why: fmt.Sprintf("Sent to you because you own the schedule “%s” in Rowsmith.", x.Name)}
	if set.Mail.Ready() && x.OwnerEmail != "" {
		html, text, err := renderEmail(data)
		if err == nil {
			sctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			err = mail.Send(sctx, set.Mail, mail.Message{To: []string{x.OwnerEmail}, Subject: subject(kind, x.Name, 0, ""), HTML: html, Text: text})
			cancel()
		}
		if err != nil {
			d.EmailError = err.Error()
		} else {
			d.Emailed = []string{x.OwnerEmail}
		}
	}
	if x.Webhook != "" && set.Webhooks {
		if raw, err := r.Vault.Open(x.Webhook, x.WebhookAAD()); err == nil {
			ev := Event{Event: "failure", Schedule: EventRef{ID: x.ID, Name: x.Name, URL: r.link(x)}, Connection: x.Connection,
				RanAt: time.UnixMilli(run.StartedAt).UTC().Format(time.RFC3339), Error: run.Error,
				Text: fmt.Sprintf("Failed: %s. %s", x.Name, clip(run.Error, 300))}
			wctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			if err := postWebhook(wctx, string(raw), set.PrivateWebhooks, ev); err != nil {
				d.Webhook = err.Error()
			} else {
				d.Webhook = "sent"
			}
			cancel()
		}
	}
}

func subject(kind, name string, rows int64, observed string) string {
	switch kind {
	case "alert":
		if observed != "" {
			return fmt.Sprintf("Alert: %s (%s)", name, observed)
		}
		return "Alert: " + name
	case "failure":
		return "Failed: " + name
	case "paused":
		return "Paused: " + name
	}
	return fmt.Sprintf("%s: %s", name, plural(rows, "row", "rows"))
}

// allowed applies the recipient policy: team members always, other
// addresses only in the domains an admin listed.
func (r *Runner) allowed(ctx context.Context, emails []string, set Settings) (ok, skipped []string) {
	if len(emails) == 0 {
		return nil, nil
	}
	team := map[string]bool{}
	if users, err := r.Store.ListUsers(ctx); err == nil {
		for _, u := range users {
			if !u.Disabled {
				team[strings.ToLower(u.Email)] = true
			}
		}
	}
	for _, e := range emails {
		if team[strings.ToLower(e)] || DomainAllowed(e, set.Domains) {
			ok = append(ok, e)
		} else {
			skipped = append(skipped, e)
		}
	}
	return ok, skipped
}

// DomainAllowed reports whether an address is in an allowed domain (or a
// subdomain of one); "*" allows every address.
func DomainAllowed(email string, domains []string) bool {
	at := strings.LastIndexByte(email, '@')
	if at < 0 {
		return false
	}
	host := strings.ToLower(email[at+1:])
	for _, d := range domains {
		d = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(d), "@"))
		if d == "*" || host == d || strings.HasSuffix(host, "."+d) {
			return true
		}
	}
	return false
}

func retentionDays(set Settings) int {
	if set.RetentionDays <= 0 {
		return 14
	}
	return set.RetentionDays
}

// OpenFile decrypts a run's result file.
func (r *Runner) OpenFile(run *store.ScheduleRun) (io.ReadCloser, error) {
	if run.FileKey == "" {
		return nil, errors.New("the file has expired")
	}
	key, err := r.Vault.Open(run.FileKey, run.FileAAD())
	if err != nil {
		return nil, err
	}
	return r.openFile(run.ID, key)
}

func (r *Runner) openFile(runID string, key []byte) (io.ReadCloser, error) {
	fh, err := os.Open(filepath.Join(r.Dir, runID))
	if err != nil {
		return nil, errors.New("the file has expired")
	}
	rd, err := OpenReader(fh, key, (&store.ScheduleRun{ID: runID}).FileAAD())
	if err != nil {
		fh.Close()
		return nil, err
	}
	return struct {
		io.Reader
		io.Closer
	}{rd, fh}, nil
}

// RemoveFiles deletes result files, e.g. with their schedule.
func (r *Runner) RemoveFiles(runIDs []string) {
	for _, id := range runIDs {
		r.removeFile(id)
	}
}

func (r *Runner) removeFile(runID string) {
	if runID == "" || strings.ContainsAny(runID, `/\.`) {
		return
	}
	_ = os.Remove(filepath.Join(r.Dir, runID))
}

func (r *Runner) prune(ctx context.Context, scheduleID string) {
	ids, err := r.Store.PruneRuns(ctx, scheduleID, keepRuns)
	if err == nil {
		r.RemoveFiles(ids)
	}
}

func (r *Runner) expireFiles() {
	runs, err := r.Store.RunsWithFiles(r.ctx, time.Now().UnixMilli())
	if err != nil {
		return
	}
	for _, run := range runs {
		r.removeFile(run.ID)
		_ = r.Store.ForgetRunFile(r.ctx, run.ID)
	}
}

// removeOrphans deletes files no run refers to (left by a crash).
func (r *Runner) removeOrphans() {
	entries, err := os.ReadDir(r.Dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		run, err := r.Store.RunByID(r.ctx, e.Name())
		if errors.Is(err, store.ErrNotFound) || (err == nil && run.FileKey == "") {
			r.removeFile(e.Name())
		}
	}
}

// TestResult is a dry run for the schedule dialog: nothing is delivered.
type TestResult struct {
	Columns   []string   `json:"columns"`
	Rows      [][]string `json:"rows"`
	RowCount  int64      `json:"rowCount"`
	Truncated bool       `json:"truncated"`
	MS        int64      `json:"ms"`
	Alert     *struct {
		Fired     bool   `json:"fired"`
		Observed  string `json:"observed"`
		Condition string `json:"condition"`
		Error     string `json:"error,omitempty"`
	} `json:"alert,omitempty"`
}

// Test runs a draft schedule's query and judges its alert.
func (r *Runner) Test(ctx context.Context, conn *store.Connection, database, schema, body string, cfg Config, prevHash string) (*TestResult, error) {
	out, err := r.query(ctx, conn, database, schema, "", body, cfg, nil, false)
	if err != nil {
		var pe pauseError
		if errors.As(err, &pe) {
			return nil, errors.New(strings.TrimPrefix(pe.msg, "Paused: "))
		}
		return nil, err
	}
	res := &TestResult{Columns: out.obs.Columns, Rows: out.preview, RowCount: out.total, Truncated: out.truncated, MS: out.ms}
	if res.Columns == nil {
		res.Columns = []string{}
	}
	if cfg.Alert.Kind != "" {
		fired, observed, aerr := cfg.Alert.Check(out.obs, prevHash)
		res.Alert = &struct {
			Fired     bool   `json:"fired"`
			Observed  string `json:"observed"`
			Condition string `json:"condition"`
			Error     string `json:"error,omitempty"`
		}{fired, observed, cfg.Alert.Describe(), ""}
		if aerr != nil {
			res.Alert.Error = aerr.Error()
		}
	}
	return res, nil
}

// capture keeps what alerts and emails need while the export sink writes
// the file: columns, the first row, a preview, a fingerprint.
type capture struct {
	*export.Sink
	state     int
	cols      []string
	first     []any
	preview   [][]string
	max       int
	h         hash.Hash
	total     int64
	truncated bool
}

func (c *capture) Columns(cols []driver.ResultColumn) error {
	if c.state == 0 {
		c.state = 1
		c.cols = make([]string, len(cols))
		for i, col := range cols {
			c.cols[i] = col.Name
		}
		for _, n := range c.cols {
			c.h.Write([]byte(n + "\x1f"))
		}
		c.h.Write([]byte{'\x1e'})
	} else if c.state == 1 {
		c.state = 2
	}
	return c.Sink.Columns(cols)
}

func (c *capture) Rows(rows [][]any) error {
	if c.state == 1 {
		for _, row := range rows {
			texts := make([]string, len(row))
			for i, v := range row {
				s, null := export.Text(v)
				if null {
					c.h.Write([]byte{0})
					s = "NULL"
				} else {
					c.h.Write([]byte{1})
					c.h.Write([]byte(s))
				}
				c.h.Write([]byte{'\x1f'})
				texts[i] = clip(s, 80)
			}
			c.h.Write([]byte{'\x1e'})
			if c.first == nil {
				c.first = append([]any(nil), row...)
			}
			if len(c.preview) < c.max {
				c.preview = append(c.preview, texts)
			}
		}
	}
	return c.Sink.Rows(rows)
}

func (c *capture) EndResult(sum driver.ResultSummary) error {
	if c.state == 1 {
		c.state = 2
		c.total, c.truncated = sum.RowCount, sum.Truncated
	}
	return c.Sink.EndResult(sum)
}

type discard struct{}

func (discard) Begin([]driver.ResultColumn) error { return nil }
func (discard) Row([]any) error                   { return nil }
func (discard) Close() error                      { return nil }

type counter struct {
	w io.Writer
	n int64
}

func (c *counter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

var unsafeName = regexp.MustCompile(`[^\p{L}\p{N} ._()-]+`)

func fileName(name, ext string, gz bool) string {
	base := strings.TrimSpace(unsafeName.ReplaceAllString(name, "-"))
	if base == "" {
		base = "result"
	}
	if len(base) > 80 {
		base = base[:80]
	}
	out := base + "-" + time.Now().UTC().Format("20060102-1504") + "." + ext
	if gz {
		out += ".gz"
	}
	return out
}

func sheetName(name string) string {
	s := strings.Map(func(r rune) rune {
		if strings.ContainsRune(`[]:*?/\`, r) {
			return '-'
		}
		return r
	}, name)
	if s == "" {
		return "Result"
	}
	if r := []rune(s); len(r) > 31 {
		s = string(r[:31])
	}
	return s
}

func tookText(ms int64) string {
	switch {
	case ms < 1000:
		return fmt.Sprintf("%d ms", max(ms, 1))
	case ms < 60_000:
		return fmt.Sprintf("%.1f s", float64(ms)/1000)
	}
	return time.Duration(ms * int64(time.Millisecond)).Round(time.Second).String()
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
