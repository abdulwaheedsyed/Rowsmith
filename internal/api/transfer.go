package api

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"rowsmith/internal/driver"
	"rowsmith/internal/export"
	"rowsmith/internal/spool"
	"rowsmith/internal/store"
)

// Exports and imports run in two steps. An export streams progress while the
// file is written to the spool, then the browser downloads the finished file
// with an ordinary GET. An import uploads the file to the spool first, then
// previews and runs it with progress. Spooled files are encrypted and expire.

// progressStream is an NDJSON response for long transfers, with a heartbeat
// so proxies keep the connection open while the database is quiet.
type progressStream struct {
	mu   sync.Mutex
	w    http.ResponseWriter
	fl   http.Flusher
	enc  *json.Encoder
	stop chan struct{}
}

func newProgress(w http.ResponseWriter) *progressStream {
	w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	fl, _ := w.(http.Flusher)
	p := &progressStream{w: w, fl: fl, enc: json.NewEncoder(w), stop: make(chan struct{})}
	go func() {
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				p.send(map[string]any{"t": "tick"})
			case <-p.stop:
				return
			}
		}
	}()
	return p
}

func (p *progressStream) send(v map[string]any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.enc.Encode(v) == nil && p.fl != nil {
		p.fl.Flush()
	}
}

func (p *progressStream) close() { close(p.stop) }

func (p *progressStream) fail(err error) {
	msg := err.Error()
	var qe *driver.QueryError
	if errors.As(err, &qe) {
		msg = qe.Message
	}
	if errors.Is(err, context.Canceled) {
		msg = "cancelled"
	}
	p.send(map[string]any{"t": "error", "message": msg})
}

type exportReq struct {
	Source    string                `json:"source"` // table | query | dump
	Ref       driver.ObjectRef      `json:"ref"`
	Browse    *driver.BrowseRequest `json:"browse"`
	SQL       string                `json:"sql"`
	Database  string                `json:"database"`
	Schema    string                `json:"schema"`
	Objects   []driver.ObjectRef    `json:"objects"`
	Structure bool                  `json:"structure"`
	Data      bool                  `json:"data"`
	DropFirst bool                  `json:"dropFirst"`
	Options   export.Options        `json:"options"`
	Gzip      bool                  `json:"gzip"`
	FileName  string                `json:"fileName"`
}

var unsafeName = regexp.MustCompile(`[^\p{L}\p{N}._-]+`)

func exportFileName(base, ext string, gz bool) string {
	base = strings.Trim(unsafeName.ReplaceAllString(base, "_"), "_.")
	if base == "" {
		base = "export"
	}
	if len(base) > 80 {
		base = base[:80]
	}
	name := base + "." + ext
	if gz {
		name += ".gz"
	}
	return name
}

// countingWriter tracks bytes for progress events.
type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

func (s *Server) wsExport(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	var req exportReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if s.spool == nil {
		writeErr(w, 503, "exports are unavailable: the spool directory could not be created")
		return
	}
	c, _, ok := s.connFor(w, r, rc, store.AccessRead)
	if !ok {
		return
	}
	switch req.Source {
	case "table":
		if req.Ref.Name == "" {
			writeErr(w, 400, "choose a table to export")
			return
		}
	case "query":
		if strings.TrimSpace(req.SQL) == "" {
			writeErr(w, 400, "enter a query to export")
			return
		}
	case "dump":
		req.Options.Format = export.SQL
		if !req.Structure && !req.Data {
			writeErr(w, 400, "choose structure, data or both")
			return
		}
	default:
		writeErr(w, 400, "unknown export source")
		return
	}
	format := req.Options.Format
	if format == "" {
		format = export.CSV
		req.Options.Format = format
	}

	ctx := r.Context()
	openCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	lease, err := s.sessions.Acquire(openCtx, c, true) // exports always read through a read-only pool
	cancel()
	if err != nil {
		writeDBErr(w, err)
		return
	}
	defer lease.Release()
	info := lease.Driver.Info()
	if req.Source == "dump" && info.Dialect == "mongodb" {
		writeErr(w, 400, "SQL dumps are not available for MongoDB; export collections as JSON instead")
		return
	}
	scope := driver.Scope{Database: req.Database, Schema: req.Schema}
	if req.Source == "table" {
		scope = driver.Scope{Database: req.Ref.Database, Schema: req.Ref.Schema}
	}
	sess, err := lease.Conn.NewSession(ctx, scope)
	if err != nil {
		writeDBErr(w, err)
		return
	}
	defer sess.Close()

	base := c.Name
	switch req.Source {
	case "table":
		base = req.Ref.Name
	case "dump":
		base = strings.Trim(c.Name+"-"+req.Database+"-"+req.Schema, "-")
	}
	name := req.FileName
	if strings.TrimSpace(name) == "" {
		name = base + "-" + time.Now().Format("20060102-1504")
	}
	name = exportFileName(strings.TrimSuffix(strings.TrimSuffix(name, ".gz"), "."+export.Extension(format)), export.Extension(format), req.Gzip)
	ctype := export.ContentType(format)
	if req.Gzip {
		ctype = "application/gzip"
	}
	sw, err := s.spool.Create(rc.user.ID, name, ctype)
	if err != nil {
		writeErr(w, 500, "could not create the export file")
		return
	}
	committed := false
	defer func() {
		if !committed {
			sw.Abort()
		}
	}()

	p := newProgress(w)
	defer p.close()
	cw := &countingWriter{w: sw}
	var dst io.Writer = cw
	var gz *gzip.Writer
	if req.Gzip {
		gz = gzip.NewWriter(cw)
		dst = gz
	}
	started := time.Now()
	lastObj := ""
	progress := func(object string, rows int64) {
		if object != "" {
			lastObj = object
		}
		p.send(map[string]any{"t": "progress", "object": lastObj, "rows": rows, "bytes": cw.n})
	}

	var rows int64
	var detail = map[string]any{"source": req.Source, "format": format}
	var runErr error
	switch req.Source {
	case "dump":
		d := &export.Dumper{Conn: lease.Conn, Session: sess, Dialect: info.Dialect, Product: info.Name}
		var total int64
		d.Progress = func(object string, n int64) { progress(object, total+n) }
		dreq := driver.DumpRequest{Scope: scope, Objects: req.Objects, Structure: req.Structure, Data: req.Data,
			DropFirst: req.DropFirst, BatchSize: req.Options.BatchRows}
		sum, err := d.Dump(ctx, dreq, dst)
		runErr = err
		if sum != nil {
			rows = sum.Rows
			detail["tables"] = sum.Tables
			if len(sum.Warnings) > 0 {
				detail["warnings"] = sum.Warnings
			}
		}
	default:
		opts := req.Options
		opts.Dialect = info.Dialect
		opts.Documents = info.Caps.Documents
		opts.QuoteIdent = export.QuoteFor(info.Dialect)
		qualify := export.QualifyFor(info.Dialect)
		opts.Qualify = func(ref driver.ObjectRef) string { ref.Database = ""; return qualify(ref) }
		if opts.Table.Name == "" {
			if req.Source == "table" {
				opts.Table = driver.ObjectRef{Schema: req.Ref.Schema, Name: req.Ref.Name}
			} else {
				opts.Table = driver.ObjectRef{Name: "exported_rows"}
			}
		}
		if opts.SheetName == "" {
			opts.SheetName = base
		}
		query, args := req.SQL, []any(nil)
		if req.Source == "table" {
			var t *driver.Table
			query, args, t, runErr = s.tableQuery(ctx, lease.Conn, info, req)
			detail["table"] = req.Ref.Name
			if t != nil {
				opts.Hints = map[string]driver.Column{}
				for _, col := range t.Columns {
					opts.Hints[col.Name] = col
				}
			}
		}
		if runErr != nil {
			p.fail(runErr)
			return
		}
		wr, err := export.New(dst, opts)
		if err != nil {
			p.fail(err)
			return
		}
		sink := &export.Sink{W: wr, Interval: 300 * time.Millisecond, Progress: func(n int64) { progress("", n) }}
		{
			runErr = sess.Execute(ctx, query, driver.ExecOptions{Params: args, ReadOnly: true, StopOnError: true}, sink)
			if runErr == nil {
				runErr = sink.Err()
			}
			if runErr == nil && !sink.Wrote() {
				runErr = errors.New("the statement returned no rows to export")
			}
		}
		if cerr := wr.Close(); runErr == nil {
			runErr = cerr
		}
		rows = sink.Count()
	}
	if gz != nil {
		if cerr := gz.Close(); runErr == nil {
			runErr = cerr
		}
	}
	if runErr != nil {
		p.fail(runErr)
		return
	}
	f, err := sw.Commit()
	if err != nil {
		p.fail(errors.New("could not finish the export file"))
		return
	}
	committed = true
	detail["rows"], detail["bytes"] = rows, f.Size
	s.audit(ctx, rc, "data.exported", c.Name, detail)
	done := map[string]any{"t": "done", "rows": rows, "ms": time.Since(started).Milliseconds(),
		"file": map[string]any{"id": f.ID, "name": f.Name, "size": f.Size, "url": "api/files/" + f.ID}}
	if w, ok := detail["warnings"]; ok {
		done["warnings"] = w
	}
	p.send(done)
}

// tableQuery builds the statement that streams a whole (filtered) table.
func (s *Server) tableQuery(ctx context.Context, conn driver.Conn, info driver.Info, req exportReq) (string, []any, *driver.Table, error) {
	dctx, cancel := context.WithTimeout(ctx, driver.CatalogTimeout)
	t, err := conn.Describe(dctx, req.Ref)
	cancel()
	if err != nil {
		return "", nil, nil, err
	}
	br := driver.BrowseRequest{}
	if req.Browse != nil {
		br = *req.Browse
	}
	br.Ref = req.Ref
	if bq, ok := conn.(driver.BrowseQuerier); ok {
		q, args, err := bq.BrowseQuery(ctx, t, br)
		return q, args, t, err
	}
	if info.Caps.Documents || len(br.Filters) > 0 || br.Search != "" || br.Where != "" {
		return "", nil, t, fmt.Errorf("exporting whole tables is not available for %s yet; export a query instead", info.Name)
	}
	return "SELECT * FROM " + export.QualifyFor(info.Dialect)(req.Ref), nil, t, nil
}

// fileDownload serves a finished spool file to its owner.
func (s *Server) fileDownload(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	if s.spool == nil {
		writeErr(w, 404, spool.ErrNotFound.Error())
		return
	}
	f, err := s.spool.Get(r.PathValue("id"), rc.user.ID)
	if err != nil {
		writeErr(w, 404, err.Error())
		return
	}
	rd, err := f.Open()
	if err != nil {
		writeErr(w, 404, err.Error())
		return
	}
	defer rd.Close()
	w.Header().Set("Content-Type", f.ContentType)
	w.Header().Set("Content-Length", strconv.FormatInt(f.Size, 10))
	w.Header().Set("Content-Disposition", `attachment; filename="`+strings.ReplaceAll(f.Name, `"`, "")+`"; filename*=UTF-8''`+url.PathEscape(f.Name))
	w.Header().Set("Cache-Control", "no-store")
	io.Copy(w, rd)
}
