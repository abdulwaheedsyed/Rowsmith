package api

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"rowsmith/internal/driver"
	"rowsmith/internal/session"
	"rowsmith/internal/sqlsplit"
	"rowsmith/internal/store"
)

func fingerprintOf(b64 string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return "", err
	}
	pk, err := ssh.ParsePublicKey(raw)
	if err != nil {
		return "", err
	}
	return ssh.FingerprintSHA256(pk), nil
}

// ws bundles what a workspace handler needs.
type ws struct {
	c        *store.Connection
	access   store.Access
	readOnly bool
	lease    *session.Lease
	info     driver.Info
}

// open resolves the connection, checks access and leases a live connection.
func (s *Server) open(w http.ResponseWriter, r *http.Request, rc *reqCtx, need store.Access) (*ws, func(), bool) {
	c, acc, ok := s.connFor(w, r, rc, need)
	if !ok {
		return nil, nil, false
	}
	ro := acc == store.AccessRead || c.ReadOnly
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	lease, err := s.sessions.Acquire(ctx, c, ro)
	if err != nil {
		writeDBErr(w, err)
		return nil, nil, false
	}
	return &ws{c: c, access: acc, readOnly: ro, lease: lease, info: lease.Driver.Info()}, lease.Release, true
}

func scopeOf(r *http.Request) driver.Scope {
	q := r.URL.Query()
	return driver.Scope{Database: q.Get("db"), Schema: q.Get("schema")}
}

func refOf(r *http.Request) driver.ObjectRef {
	q := r.URL.Query()
	return driver.ObjectRef{Database: q.Get("db"), Schema: q.Get("schema"), Name: q.Get("name"), Kind: q.Get("kind")}
}

func catalogCtx(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), driver.CatalogTimeout)
}

func (s *Server) wsServer(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	x, done, ok := s.open(w, r, rc, store.AccessRead)
	if !ok {
		return
	}
	defer done()
	ctx, cancel := catalogCtx(r)
	defer cancel()
	info, err := x.lease.Conn.Server(ctx)
	if err != nil {
		writeDBErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"server": info, "driver": x.info, "readOnly": x.readOnly, "access": x.access,
		"environment": x.c.Environment, "name": x.c.Name, "color": x.c.Color})
}

func (s *Server) wsDatabases(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	x, done, ok := s.open(w, r, rc, store.AccessRead)
	if !ok {
		return
	}
	defer done()
	ctx, cancel := catalogCtx(r)
	defer cancel()
	list, err := x.lease.Conn.Databases(ctx)
	if err != nil {
		writeDBErr(w, err)
		return
	}
	if list == nil {
		list = []driver.Database{}
	}
	writeJSON(w, 200, list)
}

func (s *Server) wsSchemas(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	x, done, ok := s.open(w, r, rc, store.AccessRead)
	if !ok {
		return
	}
	defer done()
	ctx, cancel := catalogCtx(r)
	defer cancel()
	list, err := x.lease.Conn.Schemas(ctx, r.URL.Query().Get("db"))
	if err != nil {
		writeDBErr(w, err)
		return
	}
	if list == nil {
		list = []driver.Schema{}
	}
	writeJSON(w, 200, list)
}

func (s *Server) wsObjects(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	x, done, ok := s.open(w, r, rc, store.AccessRead)
	if !ok {
		return
	}
	defer done()
	ctx, cancel := catalogCtx(r)
	defer cancel()
	list, err := x.lease.Conn.Objects(ctx, scopeOf(r))
	if err != nil {
		writeDBErr(w, err)
		return
	}
	if list == nil {
		list = []driver.Object{}
	}
	writeJSON(w, 200, list)
}

func (s *Server) wsDescribe(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	x, done, ok := s.open(w, r, rc, store.AccessRead)
	if !ok {
		return
	}
	defer done()
	ctx, cancel := catalogCtx(r)
	defer cancel()
	t, err := x.lease.Conn.Describe(ctx, refOf(r))
	if err != nil {
		writeDBErr(w, err)
		return
	}
	if x.readOnly {
		t.Editable = false
	}
	writeJSON(w, 200, t)
}

func (s *Server) wsDefinition(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	x, done, ok := s.open(w, r, rc, store.AccessRead)
	if !ok {
		return
	}
	defer done()
	d, ok := x.lease.Conn.(driver.Definer)
	if !ok {
		writeDBErr(w, driver.ErrNotSupported)
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	def, err := d.Definition(ctx, refOf(r))
	if err != nil {
		writeDBErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"definition": def})
}

func (s *Server) wsCatalog(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	x, done, ok := s.open(w, r, rc, store.AccessRead)
	if !ok {
		return
	}
	defer done()
	cat, ok := x.lease.Conn.(driver.Catalog)
	if !ok {
		writeJSON(w, 200, []driver.CatalogTable{})
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	list, err := cat.CatalogColumns(ctx, scopeOf(r))
	if err != nil {
		writeDBErr(w, err)
		return
	}
	if list == nil {
		list = []driver.CatalogTable{}
	}
	writeJSON(w, 200, list)
}

func (s *Server) wsBrowse(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	var req driver.BrowseRequest
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	x, done, ok := s.open(w, r, rc, store.AccessRead)
	if !ok {
		return
	}
	defer done()
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	if strings.TrimSpace(req.Where) != "" && x.info.Caps.SQL {
		if k, _ := sqlsplit.Classify("SELECT 1 WHERE "+req.Where, sqlsplit.Dialect(x.info.Dialect)); k != driver.StmtRead {
			writeErr(w, 400, "the WHERE condition must not modify data")
			return
		}
	}
	res, err := x.lease.Conn.Browse(ctx, req)
	if err != nil {
		writeDBErr(w, err)
		return
	}
	writeJSON(w, 200, res)
}

func (s *Server) wsCount(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	var req driver.BrowseRequest
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	x, done, ok := s.open(w, r, rc, store.AccessRead)
	if !ok {
		return
	}
	defer done()
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	n, err := x.lease.Conn.Count(ctx, req)
	if err != nil {
		writeDBErr(w, err)
		return
	}
	writeJSON(w, 200, n)
}

type editReq struct {
	Ref     driver.ObjectRef `json:"ref"`
	Edits   []driver.RowEdit `json:"edits"`
	Confirm bool             `json:"confirm"`
}

func (s *Server) wsEdit(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	var req editReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if len(req.Edits) == 0 {
		writeJSON(w, 200, driver.EditResult{})
		return
	}
	if len(req.Edits) > 5000 {
		writeErr(w, 400, "too many changes in one save (max 5000)")
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
	if x.c.Environment == "production" && !req.Confirm {
		writeErrDetail(w, 409, "confirm_required", "confirm changes to a production database", map[string]any{"edits": len(req.Edits)})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	res, err := x.lease.Conn.ApplyEdits(ctx, req.Ref, req.Edits)
	if err != nil {
		s.audit(r.Context(), rc, "data.edit_failed", x.c.ID, map[string]any{"object": req.Ref, "error": err.Error()})
		writeDBErr(w, err)
		return
	}
	counts := map[string]int{}
	for _, e := range req.Edits {
		counts[e.Op]++
	}
	s.audit(r.Context(), rc, "data.edited", x.c.ID, map[string]any{"object": req.Ref, "changes": counts})
	writeJSON(w, 200, res)
}

type explainReq struct {
	Database string `json:"database"`
	Schema   string `json:"schema"`
	SQL      string `json:"sql"`
	Analyze  bool   `json:"analyze"`
}

func (s *Server) wsExplain(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	var req explainReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	need := store.AccessRead
	x, done, ok := s.open(w, r, rc, need)
	if !ok {
		return
	}
	defer done()
	ex, ok := x.lease.Conn.(driver.Explainer)
	if !ok {
		writeDBErr(w, driver.ErrNotSupported)
		return
	}
	stmt := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(req.SQL), ";"))
	if req.Analyze {
		k, _ := sqlsplit.Classify(stmt, sqlsplit.Dialect(x.info.Dialect))
		if k != driver.StmtRead && (x.readOnly || x.c.Environment == "production") {
			writeErr(w, 403, "EXPLAIN ANALYZE executes the statement; only read queries are allowed here")
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	plan, err := ex.Explain(ctx, driver.Scope{Database: req.Database, Schema: req.Schema}, stmt, req.Analyze)
	if err != nil {
		writeDBErr(w, err)
		return
	}
	writeJSON(w, 200, plan)
}

func (s *Server) wsProcesses(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	x, done, ok := s.open(w, r, rc, store.AccessRead)
	if !ok {
		return
	}
	defer done()
	pm, ok := x.lease.Conn.(driver.ProcessManager)
	if !ok {
		writeDBErr(w, driver.ErrNotSupported)
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	res, err := pm.Processes(ctx)
	if err != nil {
		writeDBErr(w, err)
		return
	}
	writeJSON(w, 200, res)
}

func (s *Server) wsKill(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	var req struct {
		ID string `json:"id"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	x, done, ok := s.open(w, r, rc, store.AccessWrite)
	if !ok {
		return
	}
	defer done()
	pm, ok := x.lease.Conn.(driver.ProcessManager)
	if !ok {
		writeDBErr(w, driver.ErrNotSupported)
		return
	}
	if err := pm.KillProcess(r.Context(), req.ID); err != nil {
		writeDBErr(w, err)
		return
	}
	s.audit(r.Context(), rc, "db.process_killed", x.c.ID, map[string]any{"process": req.ID})
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) wsVariables(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	x, done, ok := s.open(w, r, rc, store.AccessRead)
	if !ok {
		return
	}
	defer done()
	vr, ok := x.lease.Conn.(driver.VariablesReader)
	if !ok {
		writeDBErr(w, driver.ErrNotSupported)
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	res, err := vr.Variables(ctx, r.URL.Query().Get("kind"))
	if err != nil {
		writeDBErr(w, err)
		return
	}
	writeJSON(w, 200, res)
}

func (s *Server) wsDBUsers(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	x, done, ok := s.open(w, r, rc, store.AccessRead)
	if !ok {
		return
	}
	defer done()
	um, ok := x.lease.Conn.(driver.UserManager)
	if !ok {
		writeDBErr(w, driver.ErrNotSupported)
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	res, err := um.Users(ctx)
	if err != nil {
		writeDBErr(w, err)
		return
	}
	writeJSON(w, 200, res)
}

func (s *Server) wsDBUserGrants(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	x, done, ok := s.open(w, r, rc, store.AccessRead)
	if !ok {
		return
	}
	defer done()
	um, ok := x.lease.Conn.(driver.UserManager)
	if !ok {
		writeDBErr(w, driver.ErrNotSupported)
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	g, err := um.UserGrants(ctx, r.URL.Query().Get("user"))
	if err != nil {
		writeDBErr(w, err)
		return
	}
	if g == nil {
		g = []string{}
	}
	writeJSON(w, 200, map[string]any{"grants": g})
}

// wsDDL generates (never executes) SQL for structural changes. The client
// previews it and runs it through the console, where safety checks apply.
type ddlReq struct {
	Action  string            `json:"action"` // create_table alter_table drop truncate rename create_database drop_database create_schema drop_schema
	Ref     driver.ObjectRef  `json:"ref"`
	Def     *driver.TableDef  `json:"def"`
	NewName string            `json:"newName"`
	Cascade bool              `json:"cascade"`
	Options map[string]string `json:"options"`
}

func (s *Server) wsDDL(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	var req ddlReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	x, done, ok := s.open(w, r, rc, store.AccessRead)
	if !ok {
		return
	}
	defer done()
	gen, ok := x.lease.Conn.(driver.DDLGenerator)
	if !ok {
		writeDBErr(w, driver.ErrNotSupported)
		return
	}
	var stmts []string
	var err error
	switch req.Action {
	case "create_table":
		if req.Def == nil {
			err = errors.New("missing table definition")
			break
		}
		stmts, err = gen.CreateTableSQL(*req.Def)
	case "alter_table":
		if req.Def == nil {
			err = errors.New("missing table definition")
			break
		}
		ctx, cancel := catalogCtx(r)
		cur, derr := x.lease.Conn.Describe(ctx, req.Ref)
		cancel()
		if derr != nil {
			err = derr
			break
		}
		stmts, err = gen.AlterTableSQL(cur, *req.Def)
	case "drop":
		stmts, err = gen.DropObjectSQL(req.Ref, req.Cascade)
	case "truncate":
		stmts, err = gen.TruncateSQL(req.Ref)
	case "rename":
		stmts, err = gen.RenameObjectSQL(req.Ref, req.NewName)
	case "create_database":
		stmts, err = gen.CreateDatabaseSQL(req.NewName, req.Options)
	case "drop_database":
		stmts, err = gen.DropDatabaseSQL(req.Ref.Database)
	case "create_schema", "drop_schema":
		sd, ok := x.lease.Conn.(driver.SchemaDDL)
		if !ok {
			err = driver.ErrNotSupported
			break
		}
		if req.Action == "create_schema" {
			stmts, err = sd.CreateSchemaSQL(req.Ref.Database, req.NewName)
		} else {
			stmts, err = sd.DropSchemaSQL(req.Ref.Database, req.Ref.Schema, req.Cascade)
		}
	default:
		err = errors.New("unknown DDL action")
	}
	if err != nil {
		writeDBErr(w, err)
		return
	}
	if stmts == nil {
		stmts = []string{}
	}
	writeJSON(w, 200, map[string]any{"statements": stmts})
}
