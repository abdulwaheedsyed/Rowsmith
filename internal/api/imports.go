package api

import (
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"rowsmith/internal/driver"
	"rowsmith/internal/export"
	"rowsmith/internal/importer"
	"rowsmith/internal/spool"
	"rowsmith/internal/sqlsplit"
	"rowsmith/internal/store"
)

// maxScript bounds SQL files, which are split in memory.
const maxScript = 256 << 20

// upload stores a file for a later import. The body is the raw file; its
// name comes in X-File-Name (URL-encoded).
func (s *Server) upload(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	if s.spool == nil {
		writeErr(w, 503, "imports are unavailable: the spool directory could not be created")
		return
	}
	if rc.user.Role == store.RoleViewer {
		writeErr(w, 403, "viewers cannot import data")
		return
	}
	name, _ := url.PathUnescape(r.Header.Get("X-File-Name"))
	name = strings.TrimSpace(name)
	if name == "" {
		name = "upload"
	}
	if len(name) > 200 {
		name = name[:200]
	}
	max := s.cfg.MaxUpload
	if r.ContentLength > max {
		writeErr(w, 413, "the file is larger than the upload limit")
		return
	}
	sw, err := s.spool.Create(rc.user.ID, name, r.Header.Get("Content-Type"))
	if err != nil {
		writeErr(w, 500, "could not store the upload")
		return
	}
	if _, err := io.Copy(sw, http.MaxBytesReader(w, r.Body, max)); err != nil {
		sw.Abort()
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeErr(w, 413, "the file is larger than the upload limit")
			return
		}
		writeErr(w, 400, "the upload was interrupted")
		return
	}
	f, err := sw.Commit()
	if err != nil {
		writeErr(w, 500, "could not store the upload")
		return
	}
	writeJSON(w, 201, map[string]any{"id": f.ID, "name": f.Name, "size": f.Size, "format": importer.DetectFormat(f.Name)})
}

type importReq struct {
	Upload      string             `json:"upload"`
	Ref         driver.ObjectRef   `json:"ref"`
	Database    string             `json:"database"`
	Schema      string             `json:"schema"`
	Options     importer.Options   `json:"options"`
	Mapping     []importer.Mapping `json:"mapping"`
	Empty       bool               `json:"empty"`
	Confirm     bool               `json:"confirm"`
	StopOnError *bool              `json:"stopOnError"`
}

// openUpload returns the decrypted (and, for .gz names, decompressed) file.
func (s *Server) openUpload(rc *reqCtx, id string) (*spool.File, io.ReadCloser, error) {
	if s.spool == nil {
		return nil, nil, spool.ErrNotFound
	}
	f, err := s.spool.Get(id, rc.user.ID)
	if err != nil {
		return nil, nil, errors.New("the uploaded file has expired; choose it again")
	}
	rd, err := f.Open()
	if err != nil {
		return nil, nil, err
	}
	if strings.HasSuffix(strings.ToLower(f.Name), ".gz") {
		gz, err := gzip.NewReader(rd)
		if err != nil {
			rd.Close()
			return nil, nil, errors.New("the file is not valid gzip")
		}
		return f, struct {
			io.Reader
			io.Closer
		}{gz, rd}, nil
	}
	return f, rd, nil
}

func (s *Server) readScript(rc *reqCtx, id string) (string, error) {
	_, rd, err := s.openUpload(rc, id)
	if err != nil {
		return "", err
	}
	defer rd.Close()
	b, err := io.ReadAll(io.LimitReader(rd, maxScript+1))
	if err != nil {
		return "", err
	}
	if len(b) > maxScript {
		return "", errors.New("SQL files larger than 256 MB cannot be imported here; use the database's own client")
	}
	return string(b), nil
}

// splitScript splits a script the way the console does; impl is the driver
// or connection, whichever implements driver.ScriptSplitter.
func splitScript(impl any, info driver.Info, script string) []sqlsplit.Statement {
	if sp, ok := impl.(driver.ScriptSplitter); ok && !info.Caps.SQL {
		parsed, _ := sp.SplitScript(script)
		out := make([]sqlsplit.Statement, len(parsed))
		for i, p := range parsed {
			out[i] = sqlsplit.Statement{SQL: p.SQL, Start: p.Start, End: p.End, Line: p.Line, Kind: p.Kind, Danger: p.Danger}
		}
		return out
	}
	return sqlsplit.Split(script, sqlsplit.Dialect(info.Dialect))
}

func (s *Server) wsImportPreview(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	var req importReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	c, _, ok := s.connFor(w, r, rc, store.AccessWrite)
	if !ok {
		return
	}
	if req.Options.Format == "sql" {
		drv, ok := driver.Get(c.Driver)
		if !ok {
			writeErr(w, 400, "unknown database type")
			return
		}
		script, err := s.readScript(rc, req.Upload)
		if err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		info := drv.Info()
		stmts := splitScript(drv, info, script)
		kinds := map[driver.StatementKind]int{}
		destructive := 0
		var sample []map[string]any
		for _, st := range stmts {
			kinds[st.Kind]++
			if st.Danger.Level == "destructive" {
				destructive++
			}
			if len(sample) < 12 {
				sample = append(sample, map[string]any{"line": st.Line, "kind": st.Kind, "sql": truncate(strings.Join(strings.Fields(st.SQL), " "), 160)})
			}
		}
		writeJSON(w, 200, map[string]any{"format": "sql", "statements": len(stmts), "kinds": kinds, "destructive": destructive,
			"sample": sample, "needsConfirm": len(needsConfirmation(stmts, c.Environment)) > 0 || c.Environment == "production"})
		return
	}
	_, rd, err := s.openUpload(rc, req.Upload)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	defer rd.Close()
	src, err := importer.Open(rd, req.Options)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	rows := [][]any{}
	for len(rows) < 20 {
		rec, err := src.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		for i, v := range rec {
			if s, ok := v.(string); ok && len(s) > 300 {
				rec[i] = s[:300] + "…"
			}
		}
		rows = append(rows, rec)
	}
	writeJSON(w, 200, map[string]any{"format": req.Options.Format, "columns": src.Columns(), "rows": rows,
		"needsConfirm": c.Environment == "production"})
}

func (s *Server) wsImport(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	var req importReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	c, _, ok := s.connFor(w, r, rc, store.AccessWrite)
	if !ok {
		return
	}
	if c.ReadOnly {
		writeErr(w, 403, "this connection is read-only")
		return
	}
	if c.Environment == "production" && !req.Confirm {
		writeErrDetail(w, 409, "confirm_required", "importing into a production connection needs confirmation",
			map[string]any{"environment": c.Environment})
		return
	}
	ctx := r.Context()
	openCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	lease, err := s.sessions.Acquire(openCtx, c, false)
	cancel()
	if err != nil {
		writeDBErr(w, err)
		return
	}
	defer lease.Release()
	info := lease.Driver.Info()

	if req.Options.Format == "sql" {
		s.importScript(w, r, rc, c, lease.Conn, info, req)
		return
	}
	bi, ok := lease.Conn.(driver.BulkImporter)
	if !ok {
		writeErr(w, 501, "importing into tables is not available for "+info.Name+" yet")
		return
	}
	dctx, dcancel := context.WithTimeout(ctx, driver.CatalogTimeout)
	t, err := lease.Conn.Describe(dctx, req.Ref)
	dcancel()
	if err != nil {
		writeDBErr(w, err)
		return
	}
	f, rd, err := s.openUpload(rc, req.Upload)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	defer rd.Close()
	req.Options.Documents = info.Caps.Documents
	counted := &countingReader{r: rd}
	src, err := importer.Open(counted, req.Options)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}

	p := newProgress(w)
	defer p.close()
	started := time.Now()
	job := importer.Job{Source: src, Table: t, Mapping: req.Mapping, Empty: req.Empty, Options: req.Options, Interval: 300 * time.Millisecond,
		Progress: func(n int64) { p.send(map[string]any{"t": "progress", "rows": n, "bytes": counted.n, "size": f.Size}) }}
	n, err := importer.Run(ctx, bi, job)
	if err != nil {
		p.fail(err)
		return
	}
	// Move auto-numbering past explicitly imported keys.
	var mapped []string
	for _, m := range req.Mapping {
		if m.Column != "" {
			mapped = append(mapped, m.Column)
		}
	}
	var warning string
	if fix := export.IdentityResets(info.Dialect, t, export.QualifyFor(info.Dialect)(driver.ObjectRef{Schema: t.Ref.Schema, Name: t.Ref.Name}), mapped); len(fix) > 0 {
		if sess, err := lease.Conn.NewSession(ctx, driver.Scope{Database: t.Ref.Database, Schema: t.Ref.Schema}); err == nil {
			ds := &discardSink{}
			for _, st := range fix {
				if e := sess.Execute(ctx, st, driver.ExecOptions{}, ds); e != nil || ds.err != nil {
					warning = "the rows were imported, but the auto-numbering could not be advanced; run a sequence reset before inserting new rows"
				}
			}
			sess.Close()
		}
	}
	s.spool.Remove(f.ID)
	s.audit(ctx, rc, "data.imported", c.Name, map[string]any{"table": t.Ref.Name, "rows": n, "format": req.Options.Format, "file": f.Name, "emptied": req.Empty})
	done := map[string]any{"t": "done", "rows": n, "ms": time.Since(started).Milliseconds()}
	if warning != "" {
		done["warnings"] = []string{warning}
	}
	p.send(done)
}

// importScript runs an uploaded SQL (or console) script statement by statement.
func (s *Server) importScript(w http.ResponseWriter, r *http.Request, rc *reqCtx, c *store.Connection, conn driver.Conn, info driver.Info, req importReq) {
	ctx := r.Context()
	script, err := s.readScript(rc, req.Upload)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	drv, _ := driver.Get(c.Driver)
	stmts := splitScript(drv, info, script)
	if len(stmts) == 0 {
		writeErr(w, 400, "the file contains no statements")
		return
	}
	if pending := needsConfirmation(stmts, c.Environment); len(pending) > 0 && !req.Confirm {
		writeErrDetail(w, 409, "confirm_required", "these statements need confirmation", map[string]any{"environment": c.Environment, "statements": pending})
		return
	}
	sess, err := conn.NewSession(ctx, driver.Scope{Database: req.Database, Schema: req.Schema})
	if err != nil {
		writeDBErr(w, err)
		return
	}
	defer sess.Close()
	stop := true
	if req.StopOnError != nil {
		stop = *req.StopOnError
	}
	p := newProgress(w)
	defer p.close()
	started := time.Now()
	failures := []map[string]any{}
	ok := 0
	last := time.Now()
	for i, st := range stmts {
		if ctx.Err() != nil {
			p.fail(ctx.Err())
			return
		}
		ds := &discardSink{}
		err := sess.Execute(ctx, st.SQL, driver.ExecOptions{StopOnError: true, MaxRows: 1}, ds)
		if err == nil {
			err = ds.err
		}
		if err != nil {
			msg := err.Error()
			var qe *driver.QueryError
			if errors.As(err, &qe) {
				msg = qe.Message
			}
			if len(failures) < 100 {
				failures = append(failures, map[string]any{"line": st.Line, "message": msg, "sql": truncate(strings.Join(strings.Fields(st.SQL), " "), 160)})
			}
			if stop {
				break
			}
		} else {
			ok++
		}
		if time.Since(last) > 300*time.Millisecond || i == len(stmts)-1 {
			last = time.Now()
			p.send(map[string]any{"t": "progress", "statements": i + 1, "total": len(stmts), "failed": len(failures)})
		}
	}
	var warnings []string
	if sess.InTransaction() {
		warnings = append(warnings, "the script left a transaction open; it was rolled back")
	}
	s.spool.Remove(req.Upload)
	s.audit(ctx, rc, "query.imported", c.Name, map[string]any{"statements": len(stmts), "succeeded": ok, "failed": len(failures)})
	done := map[string]any{"t": "done", "statements": len(stmts), "succeeded": ok, "failures": failures, "ms": time.Since(started).Milliseconds()}
	if len(warnings) > 0 {
		done["warnings"] = warnings
	}
	p.send(done)
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// discardSink keeps only the first statement error.
type discardSink struct{ err error }

func (d *discardSink) BeginStatement(driver.StatementInfo) error { return nil }
func (d *discardSink) Columns([]driver.ResultColumn) error       { return nil }
func (d *discardSink) Rows([][]any) error                        { return nil }
func (d *discardSink) EndResult(driver.ResultSummary) error      { return nil }
func (d *discardSink) Notice(string, string) error               { return nil }
func (d *discardSink) EndStatement(err error) error {
	if err != nil && d.err == nil {
		d.err = err
	}
	return nil
}
