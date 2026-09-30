package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"rowsmith/internal/driver"
	"rowsmith/internal/id"
	"rowsmith/internal/migrate"
	"rowsmith/internal/session"
	"rowsmith/internal/store"
)

const maxMigrations = 3 // running at once, server-wide

// migrations tracks runs in progress.
type migrations struct {
	mu   sync.Mutex
	jobs map[string]*migrationJob
}

type migrationJob struct {
	job    *migrate.Job
	userID string
	target string
}

type endpointReq struct {
	ConnectionID string `json:"connectionId"`
	Database     string `json:"database"`
	Schema       string `json:"schema"`
}

// endpoint opens one side of a migration after checking access.
type openEndpoint struct {
	conn  *store.Connection
	lease *session.Lease
	ep    migrate.Endpoint
}

func (s *Server) openEndpoint(ctx context.Context, rc *reqCtx, req endpointReq, write bool) (*openEndpoint, error) {
	c, err := s.readableConn(ctx, rc, req.ConnectionID)
	if err != nil {
		return nil, err
	}
	if write {
		acc, err := s.store.AccessFor(ctx, c, rc.user.ID)
		if err != nil {
			return nil, err
		}
		if !effectiveAccess(rc.user, acc).AtLeast(store.AccessWrite) {
			return nil, statusErr{403, "you need write access to " + c.Name + " to migrate into it"}
		}
		if c.ReadOnly {
			return nil, statusErr{400, c.Name + " is read-only"}
		}
	}
	octx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	lease, err := s.sessions.Acquire(octx, c, !write)
	if err != nil {
		return nil, statusErr{502, "could not connect to " + c.Name + ": " + err.Error()}
	}
	srv, _ := lease.Conn.Server(octx)
	return &openEndpoint{conn: c, lease: lease, ep: migrate.Endpoint{Conn: lease.Conn, Info: lease.Driver.Info(), Server: srv,
		Scope: driver.Scope{Database: req.Database, Schema: req.Schema}}}, nil
}

func endpointLabel(o *openEndpoint) string {
	parts := []string{o.conn.Name}
	if sc := strings.Trim(o.ep.Scope.Database+"."+o.ep.Scope.Schema, "."); sc != "" {
		parts = append(parts, sc)
	}
	return strings.Join(parts, " · ")
}

func sameScope(a, b endpointReq) bool {
	return a.ConnectionID == b.ConnectionID && a.Database == b.Database && a.Schema == b.Schema
}

func (s *Server) planMigration(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	var req struct {
		Source  endpointReq     `json:"source"`
		Target  endpointReq     `json:"target"`
		Options migrate.Options `json:"options"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if sameScope(req.Source, req.Target) {
		writeErr(w, 400, "the source and the target are the same; choose another database or schema to copy into")
		return
	}
	ctx := r.Context()
	src, err := s.openEndpoint(ctx, rc, req.Source, false)
	if err != nil {
		s.writeStatusErr(w, err)
		return
	}
	defer src.lease.Release()
	dst, err := s.openEndpoint(ctx, rc, req.Target, true)
	if err != nil {
		s.writeStatusErr(w, err)
		return
	}
	defer dst.lease.Release()
	pctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	plan, err := migrate.BuildPlan(pctx, src.ep, dst.ep, req.Options)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if dst.conn.Environment == "production" {
		plan.Warnings = append([]string{dst.conn.Name + " is a production connection."}, plan.Warnings...)
	}
	writeJSON(w, 200, map[string]any{"plan": plan, "source": map[string]any{"label": endpointLabel(src), "engine": src.ep.Info.Name,
		"version": serverVersion(src.ep.Server), "environment": src.conn.Environment},
		"target": map[string]any{"label": endpointLabel(dst), "engine": dst.ep.Info.Name, "version": serverVersion(dst.ep.Server),
			"environment": dst.conn.Environment, "name": dst.conn.Name}})
}

func serverVersion(s *driver.ServerInfo) string {
	if s == nil {
		return ""
	}
	return strings.TrimSpace(s.Product + " " + s.Version)
}

func (s *Server) previewMigration(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	var req struct {
		Target endpointReq   `json:"target"`
		Plan   *migrate.Plan `json:"plan"`
		Table  string        `json:"table"`
	}
	if err := readJSON(r, &req); err != nil || req.Plan == nil {
		writeErr(w, 400, "send the plan and the table to preview")
		return
	}
	dst, err := s.openEndpoint(r.Context(), rc, req.Target, true)
	if err != nil {
		s.writeStatusErr(w, err)
		return
	}
	defer dst.lease.Release()
	stmts, err := migrate.Preview(req.Plan, req.Table, dst.ep)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"statements": stmts})
}

func (s *Server) startMigration(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	var req struct {
		Source  endpointReq   `json:"source"`
		Target  endpointReq   `json:"target"`
		Plan    *migrate.Plan `json:"plan"`
		Confirm string        `json:"confirm"`
	}
	if err := readJSON(r, &req); err != nil || req.Plan == nil {
		writeErr(w, 400, "send the reviewed plan")
		return
	}
	if sameScope(req.Source, req.Target) {
		writeErr(w, 400, "the source and the target are the same")
		return
	}
	s.migs.mu.Lock()
	running, mine := 0, false
	for _, j := range s.migs.jobs {
		running++
		mine = mine || j.userID == rc.user.ID
		if j.target == req.Target.ConnectionID+"|"+req.Target.Database+"|"+req.Target.Schema {
			s.migs.mu.Unlock()
			writeErr(w, 409, "a migration into this target is already running")
			return
		}
	}
	s.migs.mu.Unlock()
	if mine {
		writeErr(w, 409, "you have a migration running; wait for it to finish or cancel it")
		return
	}
	if running >= maxMigrations {
		writeErr(w, 429, "too many migrations are running; try again when one finishes")
		return
	}
	ctx := r.Context()
	src, err := s.openEndpoint(ctx, rc, req.Source, false)
	if err != nil {
		s.writeStatusErr(w, err)
		return
	}
	dst, err := s.openEndpoint(ctx, rc, req.Target, true)
	if err != nil {
		src.lease.Release()
		s.writeStatusErr(w, err)
		return
	}
	release := func() { src.lease.Release(); dst.lease.Release() }
	p := req.Plan
	if err := migrate.Validate(p, src.ep, dst.ep); err != nil {
		release()
		writeErr(w, 400, err.Error())
		return
	}
	// Which targets exist is checked here, not taken from the plan: the
	// browser may have renamed tables since it was made.
	existing := map[string]bool{}
	if objs, err := dst.ep.Conn.Objects(ctx, dst.ep.Scope); err == nil {
		for _, o := range objs {
			existing[strings.ToLower(o.Name)] = true
		}
	}
	replacing := false
	for _, t := range p.Tables {
		t.Exists = existing[strings.ToLower(t.Target)]
		replacing = replacing || (t.Include && t.Exists && p.Options.Existing == "replace")
	}
	if (dst.conn.Environment == "production" || replacing) && strings.TrimSpace(req.Confirm) != dst.conn.Name {
		release()
		writeErrDetail(w, 409, "confirm_required", "type the target connection's name to confirm", map[string]any{"name": dst.conn.Name,
			"production": dst.conn.Environment == "production", "replacing": replacing})
		return
	}

	j := migrate.NewJob(id.New(), p)
	planJSON, _ := json.Marshal(p)
	m := &store.Migration{ID: j.ID, UserID: rc.user.ID, SourceID: src.conn.ID, SourceLabel: endpointLabel(src), TargetID: dst.conn.ID,
		TargetLabel: endpointLabel(dst), Status: "running", Tables: len(j.View().Tables), Plan: string(planJSON), StartedAt: store.Now()}
	if err := s.store.CreateMigration(ctx, m); err != nil {
		release()
		writeErr(w, 500, err.Error())
		return
	}
	s.migs.mu.Lock()
	s.migs.jobs[j.ID] = &migrationJob{job: j, userID: rc.user.ID, target: req.Target.ConnectionID + "|" + req.Target.Database + "|" + req.Target.Schema}
	s.migs.mu.Unlock()
	s.audit(ctx, rc, "migration.started", m.SourceLabel+" → "+m.TargetLabel, map[string]any{"migration": j.ID, "tables": m.Tables,
		"data": p.Options.Data, "existing": p.Options.Existing})
	uid, ip := rc.user.ID, rc.ip
	go func() {
		defer release()
		j.Run(context.Background(), p, src.ep, dst.ep)
		v := j.View()
		for _, t := range v.Tables {
			m.Rows += t.Rows
		}
		m.Status, m.FinishedAt = v.Status, v.Finished
		report, _ := json.Marshal(v)
		m.Report = string(report)
		bg := context.Background()
		if err := s.store.FinishMigration(bg, m); err != nil {
			s.log.Error("could not record a migration", "err", err)
		}
		s.migs.mu.Lock()
		delete(s.migs.jobs, j.ID)
		s.migs.mu.Unlock()
		_ = s.store.Audit(bg, uid, ip, "migration."+v.Status, m.SourceLabel+" → "+m.TargetLabel, map[string]any{"migration": j.ID,
			"tables": m.Tables, "rows": m.Rows, "error": v.Error})
	}()
	writeJSON(w, 202, map[string]any{"id": j.ID})
}

func (s *Server) migrationFor(w http.ResponseWriter, r *http.Request, rc *reqCtx) (*store.Migration, bool) {
	m, err := s.store.MigrationByID(r.Context(), r.PathValue("id"))
	if err != nil || (m.UserID != rc.user.ID && !rc.user.Role.AtLeast(store.RoleAdmin)) {
		writeErr(w, 404, "migration not found")
		return nil, false
	}
	return m, true
}

func (s *Server) migrationView(m *store.Migration) map[string]any {
	b, _ := json.Marshal(m)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	s.migs.mu.Lock()
	live, ok := s.migs.jobs[m.ID]
	s.migs.mu.Unlock()
	if ok {
		v := live.job.View()
		out["progress"] = v
		var rows int64
		for _, t := range v.Tables {
			rows += t.Rows
		}
		out["rows"] = rows
	} else {
		var v migrate.JobView
		if json.Unmarshal([]byte(m.Report), &v) == nil && v.Started > 0 {
			out["progress"] = v
		}
	}
	return out
}

func (s *Server) listMigrations(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	owner := rc.user.ID
	if r.URL.Query().Get("all") == "1" && rc.user.Role.AtLeast(store.RoleAdmin) {
		owner = ""
	}
	list, err := s.store.ListMigrations(r.Context(), owner, 50)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, m := range list {
		v := s.migrationView(m)
		delete(v, "progress")
		out = append(out, v)
	}
	writeJSON(w, 200, out)
}

func (s *Server) getMigration(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	m, ok := s.migrationFor(w, r, rc)
	if !ok {
		return
	}
	out := s.migrationView(m)
	var p migrate.Plan
	if json.Unmarshal([]byte(m.Plan), &p) == nil {
		out["plan"] = p
	}
	writeJSON(w, 200, out)
}

func (s *Server) cancelMigration(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	m, ok := s.migrationFor(w, r, rc)
	if !ok {
		return
	}
	s.migs.mu.Lock()
	live, running := s.migs.jobs[m.ID]
	s.migs.mu.Unlock()
	if !running {
		writeErr(w, 409, "this migration is not running")
		return
	}
	live.job.Cancel()
	s.audit(r.Context(), rc, "migration.cancel", m.SourceLabel+" → "+m.TargetLabel, map[string]any{"migration": m.ID})
	writeJSON(w, 200, map[string]any{"ok": true})
}

// StopMigrations cancels running migrations at shutdown.
func (s *Server) StopMigrations() {
	s.migs.mu.Lock()
	jobs := make([]*migrationJob, 0, len(s.migs.jobs))
	for _, j := range s.migs.jobs {
		jobs = append(jobs, j)
	}
	s.migs.mu.Unlock()
	for _, j := range jobs {
		j.job.Cancel()
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		s.migs.mu.Lock()
		n := len(s.migs.jobs)
		s.migs.mu.Unlock()
		if n == 0 {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
}
