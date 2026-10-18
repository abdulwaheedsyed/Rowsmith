package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"rowsmith/internal/driver"
	"rowsmith/internal/sqlsplit"
	"rowsmith/internal/store"
)

// Access management: who can do what on the connection's server. Changes
// run here rather than through a console, so passwords never reach query
// history or the audit log; previews and the audit log show them redacted.

func accessManager(w http.ResponseWriter, x *ws) (driver.AccessManager, bool) {
	am, ok := x.lease.Conn.(driver.AccessManager)
	if !ok {
		writeDBErr(w, driver.ErrNotSupported)
	}
	return am, ok
}

func (s *Server) accessModel(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	x, done, ok := s.open(w, r, rc, store.AccessRead)
	if !ok {
		return
	}
	defer done()
	am, ok := x.lease.Conn.(driver.AccessManager)
	if !ok {
		writeJSON(w, 200, map[string]any{"supported": false})
		return
	}
	m := am.AccessModel()
	// Lists an engine leaves out go out empty, not null.
	if m.Attributes == nil {
		m.Attributes = []driver.AccountAttr{}
	}
	if m.Presets == nil {
		m.Presets = []driver.AccessPreset{}
	}
	if m.CanCreate == nil {
		m.CanCreate = []string{}
	}
	writeJSON(w, 200, map[string]any{"supported": true, "model": m, "canChange": !x.readOnly})
}

func (s *Server) accessPrincipals(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	x, done, ok := s.open(w, r, rc, store.AccessRead)
	if !ok {
		return
	}
	defer done()
	am, ok := accessManager(w, x)
	if !ok {
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	ps, err := am.Principals(ctx)
	if err != nil {
		writeDBErr(w, err)
		return
	}
	if ps == nil {
		ps = []driver.Principal{}
	}
	writeJSON(w, 200, ps)
}

func (s *Server) accessGrants(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	x, done, ok := s.open(w, r, rc, store.AccessRead)
	if !ok {
		return
	}
	defer done()
	am, ok := accessManager(w, x)
	if !ok {
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	gs, err := am.Access(ctx, r.URL.Query().Get("principal"), scopeOf(r))
	if err != nil {
		writeDBErr(w, err)
		return
	}
	if gs == nil {
		gs = []driver.Grant{}
	}
	writeJSON(w, 200, gs)
}

func (s *Server) accessObject(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	x, done, ok := s.open(w, r, rc, store.AccessRead)
	if !ok {
		return
	}
	defer done()
	am, ok := accessManager(w, x)
	if !ok {
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	gs, err := am.ObjectAccess(ctx, refOf(r))
	if err != nil {
		writeDBErr(w, err)
		return
	}
	if gs == nil {
		gs = []driver.Grant{}
	}
	writeJSON(w, 200, gs)
}

type accessReq struct {
	Grants   []driver.GrantChange `json:"grants"`
	Account  *driver.AccountOp    `json:"account"`
	Database string               `json:"database"` // where the statements run
	Confirm  bool                 `json:"confirm"`
}

// statements builds the request's statements, with passwords redacted when
// redact is set.
func (req accessReq) statements(am driver.AccessManager, redact bool) ([]string, error) {
	if req.Account != nil {
		op := *req.Account
		if redact && op.Password != "" {
			op.Password = driver.Redacted
		}
		return am.AccountSQL(op)
	}
	if len(req.Grants) == 0 {
		return nil, errors.New("nothing to change")
	}
	return am.GrantSQL(req.Grants)
}

func (s *Server) accessPreview(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	var req accessReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	x, done, ok := s.open(w, r, rc, store.AccessRead)
	if !ok {
		return
	}
	defer done()
	am, ok := accessManager(w, x)
	if !ok {
		return
	}
	stmts, err := req.statements(am, true)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"statements": stmts, "script": sqlsplit.Join(x.info.Dialect, stmts)})
}

func (s *Server) accessApply(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	var req accessReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	x, done, ok := s.open(w, r, rc, store.AccessWrite)
	if !ok {
		return
	}
	defer done()
	if x.readOnly {
		writeErr(w, 403, "this connection is read-only")
		return
	}
	am, ok := accessManager(w, x)
	if !ok {
		return
	}
	if x.c.Environment == "production" && !req.Confirm {
		writeErrDetail(w, 409, "confirm_required", "changes to a production connection need confirmation", map[string]any{"environment": x.c.Environment})
		return
	}
	shown, err := req.statements(am, true)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	stmts, _ := req.statements(am, false)
	database := req.Database
	for _, g := range req.Grants {
		if g.Database != "" {
			database = g.Database
			break
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	sess, err := x.lease.Conn.NewSession(ctx, driver.Scope{Database: database})
	if err != nil {
		writeDBErr(w, err)
		return
	}
	defer sess.Close()
	sink := &discardSink{}
	runErr := sess.Execute(ctx, sqlsplit.Join(x.info.Dialect, stmts), driver.ExecOptions{StopOnError: true}, sink)
	if runErr == nil {
		runErr = sink.err
	}
	detail := map[string]any{"statements": shown, "ok": runErr == nil}
	if req.Account != nil {
		detail["account"] = req.Account.Name
	}
	s.audit(r.Context(), rc, "access.changed", x.c.ID, detail)
	if runErr != nil {
		writeDBErr(w, runErr)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "statements": shown})
}
