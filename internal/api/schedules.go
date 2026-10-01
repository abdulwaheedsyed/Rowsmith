package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"rowsmith/internal/driver"
	"rowsmith/internal/id"
	"rowsmith/internal/mail"
	"rowsmith/internal/schedule"
	"rowsmith/internal/store"
)

// scheduleSettings reads email and delivery policy for the scheduler.
func (s *Server) scheduleSettings(ctx context.Context) schedule.Settings {
	get := func(k string) string {
		v, _, _ := s.store.Setting(ctx, k)
		return v
	}
	port, _ := strconv.Atoi(get("smtp.port"))
	days, _ := strconv.Atoi(get("schedules.retention_days"))
	return schedule.Settings{
		Mail: mail.Config{Host: get("smtp.host"), Port: port, Username: get("smtp.username"), Password: s.secretSetting(ctx, "smtp.password"),
			From: get("smtp.from"), Security: strOr(get("smtp.tls"), mail.StartTLS)},
		Domains:         splitList(get("schedules.email_domains")),
		Webhooks:        get("schedules.webhooks") != "off",
		PrivateWebhooks: get("schedules.private_webhooks") == "true",
		RetentionDays:   days,
		MaxAttachBytes:  10 << 20,
	}
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == ';' }) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func strOr(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

// baseURL is the app's public address with its base path, for links in
// emails; empty when the public URL is not configured.
func (s *Server) baseURL() string {
	u := s.cfg.PublicURL
	if u == nil || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host + s.cfg.BasePath
}

// StartScheduler begins running due schedules.
func (s *Server) StartScheduler() { s.sched.Start() }

// StopScheduler cancels running schedules and waits briefly for them.
func (s *Server) StopScheduler() { s.sched.Stop() }

type scheduleReq struct {
	ConnectionID string          `json:"connectionId"`
	Database     string          `json:"database"`
	Schema       string          `json:"schema"`
	Name         string          `json:"name"`
	Description  string          `json:"description"`
	Body         string          `json:"body"`
	Cron         string          `json:"cron"`
	Timezone     string          `json:"timezone"`
	Config       schedule.Config `json:"config"`
	Webhook      *string         `json:"webhook"` // nil keeps the saved one, "" removes it
	Enabled      *bool           `json:"enabled"`
}

type statusErr struct {
	code int
	msg  string
}

func (e statusErr) Error() string { return e.msg }

func (s *Server) writeStatusErr(w http.ResponseWriter, err error) {
	var se statusErr
	if errors.As(err, &se) {
		writeErr(w, se.code, se.msg)
		return
	}
	writeErr(w, 500, err.Error())
}

// readableConn returns a connection the current person may read.
func (s *Server) readableConn(ctx context.Context, rc *reqCtx, connID string) (*store.Connection, error) {
	c, err := s.store.ConnectionByID(ctx, connID)
	if err != nil {
		return nil, statusErr{404, "connection not found"}
	}
	acc, err := s.store.AccessFor(ctx, c, rc.user.ID)
	if err != nil {
		return nil, err
	}
	if effectiveAccess(rc.user, acc) == store.AccessNone {
		return nil, statusErr{404, "connection not found"}
	}
	return c, nil
}

// checkSchedule validates a create or update request.
func (s *Server) checkSchedule(ctx context.Context, rc *reqCtx, req *scheduleReq) (*store.Connection, error) {
	req.Name = strings.TrimSpace(req.Name)
	req.Description = strings.TrimSpace(req.Description)
	switch {
	case req.Name == "":
		return nil, statusErr{400, "give the schedule a name"}
	case len(req.Name) > 200:
		return nil, statusErr{400, "the name is too long"}
	case len(req.Description) > 2000:
		return nil, statusErr{400, "the description is too long"}
	case len(req.Body) > 1<<20:
		return nil, statusErr{400, "the query is too long to schedule"}
	}
	c, err := s.readableConn(ctx, rc, req.ConnectionID)
	if err != nil {
		return nil, err
	}
	d, ok := driver.Get(c.Driver)
	if !ok {
		return nil, statusErr{400, "this connection's database type is not available"}
	}
	if err := schedule.CheckStatements(d.Info(), req.Body); err != nil {
		return nil, statusErr{400, err.Error()}
	}
	req.Timezone = strOr(req.Timezone, "UTC")
	if _, _, err := schedule.Validate(req.Cron, req.Timezone); err != nil {
		return nil, statusErr{400, err.Error()}
	}
	if err := req.Config.Normalize(); err != nil {
		return nil, statusErr{400, err.Error()}
	}
	set := s.scheduleSettings(ctx)
	if bad := s.outsidePolicy(ctx, req.Config.Delivery.Emails, set); len(bad) > 0 {
		return nil, statusErr{400, "your admin only allows sending to team members" + domainsText(set.Domains) + "; remove " + strings.Join(bad, ", ")}
	}
	if req.Webhook != nil && *req.Webhook != "" {
		if !set.Webhooks {
			return nil, statusErr{400, "an admin turned webhooks off"}
		}
		if err := schedule.ValidateWebhook(*req.Webhook, set.PrivateWebhooks); err != nil {
			return nil, statusErr{400, err.Error()}
		}
	}
	return c, nil
}

func domainsText(domains []string) string {
	if len(domains) == 0 {
		return ""
	}
	return " and addresses at " + strings.Join(domains, ", ")
}

func (s *Server) outsidePolicy(ctx context.Context, emails []string, set schedule.Settings) []string {
	if len(emails) == 0 {
		return nil
	}
	team := map[string]bool{}
	users, _ := s.store.ListUsers(ctx)
	for _, u := range users {
		if !u.Disabled {
			team[strings.ToLower(u.Email)] = true
		}
	}
	var bad []string
	for _, e := range emails {
		if !team[strings.ToLower(e)] && !schedule.DomainAllowed(e, set.Domains) {
			bad = append(bad, e)
		}
	}
	return bad
}

func (s *Server) scheduleView(ctx context.Context, rc *reqCtx, x *store.Schedule) map[string]any {
	b, _ := json.Marshal(x)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	out["config"] = schedule.ParseConfig(x.Config)
	hook := map[string]any{"set": x.Webhook != ""}
	if x.Webhook != "" {
		if raw, err := s.vault.Open(x.Webhook, x.WebhookAAD()); err == nil {
			if u, err := url.Parse(string(raw)); err == nil {
				hook["host"] = u.Host
			}
		}
	}
	out["webhook"] = hook
	out["isOwner"] = x.OwnerID == rc.user.ID
	return out
}

// scheduleFor loads a schedule the current person owns or administers.
func (s *Server) scheduleFor(w http.ResponseWriter, r *http.Request, rc *reqCtx) (*store.Schedule, bool) {
	x, err := s.store.ScheduleByID(r.Context(), r.PathValue("id"))
	if err != nil || (x.OwnerID != rc.user.ID && !rc.user.Role.AtLeast(store.RoleAdmin)) {
		writeErr(w, 404, "schedule not found")
		return nil, false
	}
	return x, true
}

func (s *Server) listSchedules(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	owner := rc.user.ID
	if r.URL.Query().Get("all") == "1" && rc.user.Role.AtLeast(store.RoleAdmin) {
		owner = ""
	}
	list, err := s.store.ListSchedules(r.Context(), owner)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, x := range list {
		out = append(out, s.scheduleView(r.Context(), rc, x))
	}
	writeJSON(w, 200, out)
}

func (s *Server) getSchedule(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	x, ok := s.scheduleFor(w, r, rc)
	if !ok {
		return
	}
	writeJSON(w, 200, s.scheduleView(r.Context(), rc, x))
}

func (s *Server) createSchedule(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	if rc.user.Role == store.RoleViewer {
		writeErr(w, 403, "viewers can receive scheduled results but not create schedules")
		return
	}
	var req scheduleReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	ctx := r.Context()
	c, err := s.checkSchedule(ctx, rc, &req)
	if err != nil {
		s.writeStatusErr(w, err)
		return
	}
	x := &store.Schedule{ID: id.New(), OwnerID: rc.user.ID, ConnectionID: c.ID, Database: req.Database, Schema: req.Schema,
		Name: req.Name, Description: req.Description, Body: req.Body, Cron: strings.TrimSpace(req.Cron), Timezone: req.Timezone,
		Config: req.Config.JSON(), Enabled: req.Enabled == nil || *req.Enabled}
	if req.Webhook != nil && *req.Webhook != "" {
		if x.Webhook, err = s.vault.Seal([]byte(strings.TrimSpace(*req.Webhook)), x.WebhookAAD()); err != nil {
			writeErr(w, 500, "could not seal the webhook")
			return
		}
	}
	x.NextRunAt = schedule.NextRun(x.Cron, x.Timezone, time.Now())
	if err := s.store.CreateSchedule(ctx, x); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	s.audit(ctx, rc, "schedule.created", x.Name, map[string]any{"schedule": x.ID, "connection": c.Name, "cron": x.Cron,
		"timezone": x.Timezone, "recipients": req.Config.Delivery.Emails, "webhook": x.Webhook != ""})
	x, _ = s.store.ScheduleByID(ctx, x.ID)
	writeJSON(w, 201, s.scheduleView(ctx, rc, x))
}

func (s *Server) updateSchedule(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	x, ok := s.scheduleFor(w, r, rc)
	if !ok {
		return
	}
	if x.OwnerID != rc.user.ID {
		// It runs with its owner's access, so only they may change what it does.
		writeErr(w, 403, "only "+x.OwnerName+" can edit this schedule; you can pause or delete it")
		return
	}
	var req scheduleReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	ctx := r.Context()
	c, err := s.checkSchedule(ctx, rc, &req)
	if err != nil {
		s.writeStatusErr(w, err)
		return
	}
	old := schedule.ParseConfig(x.Config)
	if x.Body != req.Body || x.ConnectionID != c.ID || x.Database != req.Database || x.Schema != req.Schema || old.Alert != req.Config.Alert {
		x.LastHash, x.LastAlert = "", false // a new question starts a new baseline
	}
	x.ConnectionID, x.Database, x.Schema = c.ID, req.Database, req.Schema
	x.Name, x.Description, x.Body = req.Name, req.Description, req.Body
	x.Cron, x.Timezone, x.Config = strings.TrimSpace(req.Cron), req.Timezone, req.Config.JSON()
	if req.Webhook != nil {
		x.Webhook = ""
		if *req.Webhook != "" {
			if x.Webhook, err = s.vault.Seal([]byte(strings.TrimSpace(*req.Webhook)), x.WebhookAAD()); err != nil {
				writeErr(w, 500, "could not seal the webhook")
				return
			}
		}
	}
	if req.Enabled != nil {
		if *req.Enabled && !x.Enabled {
			x.PausedReason, x.Failures = "", 0
		}
		x.Enabled = *req.Enabled
	}
	x.NextRunAt = schedule.NextRun(x.Cron, x.Timezone, time.Now())
	if err := s.store.UpdateSchedule(ctx, x); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	s.audit(ctx, rc, "schedule.updated", x.Name, map[string]any{"schedule": x.ID, "connection": c.Name, "cron": x.Cron,
		"timezone": x.Timezone, "recipients": req.Config.Delivery.Emails, "webhook": x.Webhook != "", "enabled": x.Enabled})
	x, _ = s.store.ScheduleByID(ctx, x.ID)
	writeJSON(w, 200, s.scheduleView(ctx, rc, x))
}

// toggleSchedule pauses or resumes; admins may do this for anyone's.
func (s *Server) toggleSchedule(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	x, ok := s.scheduleFor(w, r, rc)
	if !ok {
		return
	}
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	ctx := r.Context()
	if req.Enabled {
		if _, _, err := schedule.Validate(x.Cron, x.Timezone); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		if x.OwnerID != rc.user.ID {
			if owner, err := s.store.UserByID(ctx, x.OwnerID); err != nil || owner.Disabled {
				writeErr(w, 400, "the owner's account is disabled; the schedule cannot run as them")
				return
			}
		}
		x.PausedReason, x.Failures = "", 0
		x.NextRunAt = schedule.NextRun(x.Cron, x.Timezone, time.Now())
	} else if x.OwnerID != rc.user.ID {
		x.PausedReason = "Paused by " + rc.user.Name + "."
	} else {
		x.PausedReason = ""
	}
	x.Enabled = req.Enabled
	if err := s.store.UpdateSchedule(ctx, x); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	action := "schedule.paused"
	if req.Enabled {
		action = "schedule.resumed"
	}
	s.audit(ctx, rc, action, x.Name, map[string]any{"schedule": x.ID})
	x, _ = s.store.ScheduleByID(ctx, x.ID)
	writeJSON(w, 200, s.scheduleView(ctx, rc, x))
}

func (s *Server) deleteSchedule(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	x, ok := s.scheduleFor(w, r, rc)
	if !ok {
		return
	}
	ctx := r.Context()
	s.sched.Cancel(x.ID)
	files, _ := s.store.RunIDsWithFiles(ctx, x.ID)
	if err := s.store.DeleteSchedule(ctx, x.ID); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	s.sched.RemoveFiles(files)
	s.audit(ctx, rc, "schedule.deleted", x.Name, map[string]any{"schedule": x.ID, "owner": x.OwnerName})
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) runSchedule(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	x, ok := s.scheduleFor(w, r, rc)
	if !ok {
		return
	}
	if ok, _ := s.schedLimit.Allow(rc.user.ID); !ok {
		writeErr(w, 429, "too many runs in a short time; wait a moment")
		return
	}
	runID, err := s.sched.RunNow(r.Context(), x.ID, rc.user.ID)
	if err != nil {
		writeErr(w, 409, err.Error())
		return
	}
	s.audit(r.Context(), rc, "schedule.run_now", x.Name, map[string]any{"schedule": x.ID, "run": runID})
	writeJSON(w, 202, map[string]any{"runId": runID})
}

func (s *Server) listRuns(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	x, ok := s.scheduleFor(w, r, rc)
	if !ok {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	runs, err := s.store.ListRuns(r.Context(), x.ID, limit)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	out := make([]map[string]any, 0, len(runs))
	for _, run := range runs {
		b, _ := json.Marshal(run)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		var d any
		_ = json.Unmarshal([]byte(strOr(run.Delivery, "{}")), &d)
		m["delivery"] = d
		m["hasFile"] = run.FileKey != ""
		out = append(out, m)
	}
	writeJSON(w, 200, out)
}

func (s *Server) runFile(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	x, ok := s.scheduleFor(w, r, rc)
	if !ok {
		return
	}
	ctx := r.Context()
	if x.OwnerID != rc.user.ID {
		// The file holds what the owner can read; admins need their own access.
		if _, err := s.readableConn(ctx, rc, x.ConnectionID); err != nil {
			writeErr(w, 403, "you need access to "+x.Connection+" to download this result")
			return
		}
	}
	run, err := s.store.RunByID(ctx, r.PathValue("run"))
	if err != nil || run.ScheduleID != x.ID {
		writeErr(w, 404, "run not found")
		return
	}
	rd, err := s.sched.OpenFile(run)
	if err != nil {
		writeErr(w, 404, err.Error())
		return
	}
	defer rd.Close()
	s.audit(ctx, rc, "schedule.downloaded", x.Name, map[string]any{"schedule": x.ID, "run": run.ID, "rows": run.RowCount})
	w.Header().Set("Content-Type", run.FileType)
	w.Header().Set("Content-Length", strconv.FormatInt(run.FileSize, 10))
	w.Header().Set("Content-Disposition", `attachment; filename="`+strings.ReplaceAll(run.FileName, `"`, "")+`"; filename*=UTF-8''`+url.PathEscape(run.FileName))
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.Copy(w, rd)
}

func (s *Server) previewSchedule(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	var req struct {
		Cron     string `json:"cron"`
		Timezone string `json:"timezone"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	c, loc, err := schedule.Validate(req.Cron, strOr(req.Timezone, "UTC"))
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	now := time.Now()
	runs := []int64{}
	for _, t := range c.Runs(now, loc, 5) {
		runs = append(runs, t.UnixMilli())
	}
	// Every run in the coming week, for the timeline (every 5 minutes is 2016).
	week := []int64{}
	end := now.Add(7 * 24 * time.Hour)
	for t := c.Next(now, loc); !t.IsZero() && t.Before(end) && len(week) < 2100; t = c.Next(t, loc) {
		week = append(week, t.UnixMilli())
	}
	writeJSON(w, 200, map[string]any{"runs": runs, "week": week})
}

// testSchedule runs a draft's query and judges its alert, delivering nothing.
func (s *Server) testSchedule(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	var req struct {
		ScheduleID   string          `json:"scheduleId"`
		ConnectionID string          `json:"connectionId"`
		Database     string          `json:"database"`
		Schema       string          `json:"schema"`
		Body         string          `json:"body"`
		Config       schedule.Config `json:"config"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if ok, _ := s.schedLimit.Allow(rc.user.ID); !ok {
		writeErr(w, 429, "too many test runs in a short time; wait a moment")
		return
	}
	ctx := r.Context()
	c, err := s.readableConn(ctx, rc, req.ConnectionID)
	if err != nil {
		s.writeStatusErr(w, err)
		return
	}
	if err := req.Config.Normalize(); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	prev := ""
	if req.ScheduleID != "" {
		if x, err := s.store.ScheduleByID(ctx, req.ScheduleID); err == nil && x.OwnerID == rc.user.ID {
			prev = x.LastHash
		}
	}
	tctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	res, err := s.sched.Test(tctx, c, req.Database, req.Schema, req.Body, req.Config, prev)
	if err != nil {
		if errors.Is(tctx.Err(), context.DeadlineExceeded) {
			err = errors.New("the test stopped after 90 seconds; scheduled runs may take up to 15 minutes")
		}
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, res)
}

// schedulePolicy tells the schedule dialog what delivery is possible.
func (s *Server) schedulePolicy(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	set := s.scheduleSettings(r.Context())
	anyone := false
	for _, d := range set.Domains {
		if d == "*" {
			anyone = true
		}
	}
	domains := []string{}
	for _, d := range set.Domains {
		if d != "*" {
			domains = append(domains, strings.TrimPrefix(d, "@"))
		}
	}
	writeJSON(w, 200, map[string]any{"email": set.Mail.Ready(), "domains": domains, "anyone": anyone, "webhooks": set.Webhooks,
		"privateWebhooks": set.PrivateWebhooks, "retentionDays": max(set.RetentionDays, 0), "maxAttachMB": set.MaxAttachBytes >> 20,
		"canCreate": rc.user.Role != store.RoleViewer, "minIntervalMinutes": int(schedule.MinInterval.Minutes())})
}

// testMail sends a test message to the admin with draft SMTP settings.
func (s *Server) testMail(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	if !requireAdmin(w, rc) {
		return
	}
	var req struct {
		Host     string `json:"host"`
		Port     int    `json:"port"`
		Username string `json:"username"`
		Password string `json:"password"`
		From     string `json:"from"`
		Security string `json:"security"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	ctx := r.Context()
	cfg := mail.Config{Host: strings.TrimSpace(req.Host), Port: req.Port, Username: strings.TrimSpace(req.Username), Password: req.Password,
		From: strings.TrimSpace(req.From), Security: strOr(req.Security, mail.StartTLS)}
	if cfg.Password == "" {
		// The saved password only goes back to the server it was saved for.
		saved := s.scheduleSettings(ctx).Mail
		if strings.EqualFold(saved.Host, cfg.Host) && saved.Username == cfg.Username {
			cfg.Password = saved.Password
		}
	}
	if !cfg.Ready() {
		writeErr(w, 400, "enter the server and the sender address")
		return
	}
	sctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	html := fmt.Sprintf(`<div style="font-family:-apple-system,Segoe UI,Roboto,Arial,sans-serif;font-size:15px;color:#1B1F24;">`+mail.Brand+`
<p style="margin-top:18px;"><b>Email from Rowsmith works.</b></p><p>Scheduled reports and alerts will be sent from %s through %s.</p></div>`,
		htmlEscape(cfg.From), htmlEscape(cfg.Host))
	err := mail.Send(sctx, cfg, mail.Message{To: []string{rc.user.Email}, Subject: "Rowsmith™ test email",
		Text: "Email from Rowsmith works. Scheduled reports and alerts will be sent from " + cfg.From + " through " + cfg.Host + ".", HTML: html})
	if err != nil {
		writeErr(w, 502, err.Error())
		return
	}
	s.audit(ctx, rc, "settings.mail_tested", cfg.Host, nil)
	writeJSON(w, 200, map[string]any{"ok": true, "to": rc.user.Email})
}

func htmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;").Replace(s)
}
