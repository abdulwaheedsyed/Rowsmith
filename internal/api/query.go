package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"rowsmith/internal/driver"
	"rowsmith/internal/sqlsplit"
	"rowsmith/internal/store"
)

type queryReq struct {
	Tab         string `json:"tab"`
	Database    string `json:"database"`
	Schema      string `json:"schema"`
	SQL         string `json:"sql"`
	Mode        string `json:"mode"`   // "all" (default) | "statement"
	Cursor      int    `json:"cursor"` // byte offset for mode=statement
	MaxRows     int    `json:"maxRows"`
	Confirm     bool   `json:"confirm"`
	StopOnError *bool  `json:"stopOnError"`
}

// ndjsonSink streams execution events as newline-delimited JSON.
type ndjsonSink struct {
	mu    sync.Mutex
	w     http.ResponseWriter
	fl    http.Flusher
	enc   *json.Encoder
	start time.Time

	stmtStart time.Time
	rows      int64
	firstErr  string
	kinds     []driver.StatementKind
	failed    int
	stmts     int
}

func newSink(w http.ResponseWriter) *ndjsonSink {
	w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	fl, _ := w.(http.Flusher)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return &ndjsonSink{w: w, fl: fl, enc: enc, start: time.Now()}
}

func (s *ndjsonSink) emit(v map[string]any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.enc.Encode(v); err != nil {
		return err
	}
	if s.fl != nil {
		s.fl.Flush()
	}
	return nil
}

func (s *ndjsonSink) BeginStatement(st driver.StatementInfo) error {
	s.stmtStart = time.Now()
	s.stmts++
	s.kinds = append(s.kinds, st.Kind)
	return s.emit(map[string]any{"t": "stmt", "i": st.Index, "sql": st.SQL, "line": st.Line, "kind": st.Kind})
}

func (s *ndjsonSink) Columns(c []driver.ResultColumn) error {
	return s.emit(map[string]any{"t": "cols", "cols": c})
}

func (s *ndjsonSink) Rows(r [][]any) error {
	s.rows += int64(len(r))
	return s.emit(map[string]any{"t": "rows", "rows": r})
}

func (s *ndjsonSink) EndResult(sum driver.ResultSummary) error {
	return s.emit(map[string]any{"t": "end", "summary": sum})
}

func (s *ndjsonSink) Notice(level, text string) error {
	return s.emit(map[string]any{"t": "notice", "level": level, "text": text})
}

func (s *ndjsonSink) EndStatement(err error) error {
	ev := map[string]any{"t": "stmtEnd", "ms": float64(time.Since(s.stmtStart).Microseconds()) / 1000}
	if err != nil {
		s.failed++
		var qe *driver.QueryError
		if errors.As(err, &qe) {
			ev["error"] = qe
		} else {
			ev["error"] = &driver.QueryError{Message: err.Error()}
		}
		if s.firstErr == "" {
			s.firstErr = err.Error()
		}
	}
	return s.emit(ev)
}

func (s *Server) wsQuery(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	var req queryReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if strings.TrimSpace(req.SQL) == "" {
		writeErr(w, 400, "nothing to run")
		return
	}
	c, acc, ok := s.connFor(w, r, rc, store.AccessRead)
	if !ok {
		return
	}
	drv, ok := driver.Get(c.Driver)
	if !ok {
		writeErr(w, 400, "unknown database type")
		return
	}
	info := drv.Info()
	readOnly := acc == store.AccessRead || c.ReadOnly
	dialect := sqlsplit.Dialect(info.Dialect)

	script := req.SQL
	var stmts []sqlsplit.Statement
	if sp, ok := drv.(driver.ScriptSplitter); ok && !info.Caps.SQL {
		// A script that does not parse still goes to the session, which
		// reports the syntax error (with its line) like any other failure.
		parsed, _ := sp.SplitScript(req.SQL)
		for _, p := range parsed {
			stmts = append(stmts, sqlsplit.Statement{SQL: p.SQL, Start: p.Start, End: p.End, Line: p.Line, Kind: p.Kind, Danger: p.Danger})
		}
		if req.Mode == "statement" {
			if st, ok := statementAt(stmts, req.Cursor); ok {
				script, stmts = st.SQL, []sqlsplit.Statement{st}
			}
		}
		if !readOnly {
			if pending := needsConfirmation(stmts, c.Environment); len(pending) > 0 && !req.Confirm {
				writeErrDetail(w, 409, "confirm_required", "these statements need confirmation", map[string]any{
					"environment": c.Environment, "statements": pending})
				return
			}
		}
	} else if info.Caps.SQL {
		if req.Mode == "statement" {
			st, ok := sqlsplit.StatementAt(req.SQL, dialect, req.Cursor)
			if !ok {
				writeErr(w, 400, "no statement at the cursor")
				return
			}
			script = st.SQL
			stmts = []sqlsplit.Statement{st}
		} else {
			stmts = sqlsplit.Split(script, dialect)
		}
		if len(stmts) == 0 {
			writeErr(w, 400, "nothing to run (only comments?)")
			return
		}
		if !readOnly {
			if pending := needsConfirmation(stmts, c.Environment); len(pending) > 0 && !req.Confirm {
				writeErrDetail(w, 409, "confirm_required", "these statements need confirmation", map[string]any{
					"environment": c.Environment, "statements": pending})
				return
			}
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 6*time.Hour)
	defer cancel()
	con, err := s.sessions.Console(ctx, rc.user.ID, c, req.Tab, driver.Scope{Database: req.Database, Schema: req.Schema}, readOnly)
	if err != nil {
		writeDBErr(w, err)
		return
	}
	maxRows := req.MaxRows
	if maxRows <= 0 {
		maxRows = 1000
	}
	if maxRows > 200_000 {
		maxRows = 200_000
	}
	stop := true
	if req.StopOnError != nil {
		stop = *req.StopOnError
	}
	sink := newSink(w)
	_ = sink.emit(map[string]any{"t": "start", "console": con.ID})
	started := time.Now()
	runErr := con.Run(ctx, script, driver.ExecOptions{MaxRows: maxRows, StopOnError: stop, ReadOnly: readOnly}, sink)
	dur := time.Since(started)
	done := map[string]any{"t": "done", "ms": float64(dur.Microseconds()) / 1000, "inTx": con.InTransaction(), "console": con.ID}
	if runErr != nil {
		done["error"] = &driver.QueryError{Message: runErr.Error()}
		if sink.firstErr == "" {
			sink.firstErr = runErr.Error()
		}
	}
	_ = sink.emit(done)

	status := "ok"
	if sink.failed > 0 || runErr != nil {
		status = "error"
	}
	if ctx.Err() != nil {
		status = "cancelled"
	}
	bg := context.WithoutCancel(r.Context())
	_ = s.store.AddHistory(bg, &store.HistoryEntry{UserID: rc.user.ID, ConnectionID: c.ID, Database: req.Database,
		Body: truncate(script, 100_000), StartedAt: started.UnixMilli(), DurationMS: dur.Milliseconds(), RowCount: sink.rows,
		Status: status, Error: truncate(sink.firstErr, 2000)})
	writes := 0
	for _, k := range sink.kinds {
		if !k.Safe() {
			writes++
		}
	}
	if writes > 0 {
		s.audit(bg, rc, "query.write", c.ID, map[string]any{"database": req.Database, "statements": writes,
			"status": status, "sql": truncate(script, 4000), "environment": c.Environment})
	}
}

type pendingStmt struct {
	Index  int           `json:"index"`
	SQL    string        `json:"sql"`
	Line   int           `json:"line"`
	Danger driver.Danger `json:"danger"`
}

func needsConfirmation(stmts []sqlsplit.Statement, env string) []pendingStmt {
	var out []pendingStmt
	for i, st := range stmts {
		switch {
		case st.Danger.Level == "destructive":
		case env == "production" && !st.Kind.Safe():
		default:
			continue
		}
		out = append(out, pendingStmt{Index: i, SQL: truncate(st.SQL, 2000), Line: st.Line, Danger: st.Danger})
	}
	return out
}

func (s *Server) wsCancel(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	var req struct {
		Console string `json:"console"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"cancelled": s.sessions.Cancel(rc.user.ID, req.Console)})
}

func (s *Server) wsCloseConsole(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	var req struct {
		Console string `json:"console"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	s.sessions.CloseConsole(rc.user.ID, req.Console)
	writeJSON(w, 200, map[string]any{"ok": true})
}

// wsDisconnect ends the caller's sessions on a connection.
func (s *Server) wsDisconnect(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	c, _, ok := s.connFor(w, r, rc, store.AccessRead)
	if !ok {
		return
	}
	writeJSON(w, 200, map[string]any{"consoles": s.sessions.Disconnect(rc.user.ID, c.ID)})
}

func statementAt(stmts []sqlsplit.Statement, cursor int) (sqlsplit.Statement, bool) {
	best := -1
	for i, st := range stmts {
		if cursor >= st.Start && cursor <= st.End {
			return st, true
		}
		if st.Start <= cursor {
			best = i
		}
	}
	if best < 0 {
		if len(stmts) == 0 {
			return sqlsplit.Statement{}, false
		}
		best = 0
	}
	return stmts[best], true
}

// wsAnalyze splits and classifies a script without running it: the editor
// uses it for statement boundaries, gutter markers and safety hints.
func (s *Server) wsAnalyze(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	var req queryReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	c, _, ok := s.connFor(w, r, rc, store.AccessRead)
	if !ok {
		return
	}
	drv, ok := driver.Get(c.Driver)
	if ok {
		if sp, isSplitter := drv.(driver.ScriptSplitter); isSplitter && !drv.Info().Caps.SQL {
			parsed, err := sp.SplitScript(req.SQL)
			if err != nil || parsed == nil {
				parsed = []driver.ScriptStatement{}
			}
			writeJSON(w, 200, map[string]any{"statements": parsed})
			return
		}
	}
	if !ok || !drv.Info().Caps.SQL {
		writeJSON(w, 200, map[string]any{"statements": []any{}})
		return
	}
	stmts := sqlsplit.Split(req.SQL, sqlsplit.Dialect(drv.Info().Dialect))
	if stmts == nil {
		stmts = []sqlsplit.Statement{}
	}
	writeJSON(w, 200, map[string]any{"statements": stmts})
}
