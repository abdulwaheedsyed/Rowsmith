package api

import (
	"net/http"
	"strconv"
	"strings"

	"rowsmith/internal/id"
	"rowsmith/internal/store"
)

func (s *Server) listHistory(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	q := r.URL.Query()
	before, _ := strconv.ParseInt(q.Get("before"), 10, 64)
	limit, _ := strconv.Atoi(q.Get("limit"))
	f := store.HistoryFilter{UserID: rc.user.ID, ConnectionID: q.Get("connection"), Search: q.Get("q"), Before: before, Limit: limit}
	if q.Get("scope") == "team" {
		// Team history of a connection: requires manage access (or admin).
		if f.ConnectionID == "" {
			writeErr(w, 400, "team history needs a connection")
			return
		}
		c, err := s.store.ConnectionByID(r.Context(), f.ConnectionID)
		if err != nil {
			writeErr(w, 404, "connection not found")
			return
		}
		acc, _ := s.store.AccessFor(r.Context(), c, rc.user.ID)
		if !acc.AtLeast(store.AccessManage) && !rc.user.Role.AtLeast(store.RoleAdmin) {
			writeErr(w, 403, "team history requires manage access")
			return
		}
		f.UserID = ""
	}
	list, err := s.store.ListHistory(r.Context(), f)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if list == nil {
		list = []store.HistoryEntry{}
	}
	writeJSON(w, 200, list)
}

func (s *Server) listSaved(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	list, err := s.store.SavedQueriesFor(r.Context(), rc.user.ID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if list == nil {
		list = []*store.SavedQuery{}
	}
	writeJSON(w, 200, list)
}

type savedReq struct {
	ConnectionID string   `json:"connectionId"`
	Database     string   `json:"database"`
	Name         string   `json:"name"`
	Description  string   `json:"description"`
	Body         string   `json:"body"`
	Tags         []string `json:"tags"`
	Visibility   string   `json:"visibility"`
}

func (req *savedReq) validate() string {
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || strings.TrimSpace(req.Body) == "" {
		return "a saved query needs a name and a body"
	}
	if req.Visibility != "team" {
		req.Visibility = "private"
	}
	if len(req.Body) > 1<<20 {
		return "query too large"
	}
	if len(req.Tags) > 20 {
		req.Tags = req.Tags[:20]
	}
	return ""
}

func (s *Server) createSaved(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	var req savedReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if msg := req.validate(); msg != "" {
		writeErr(w, 400, msg)
		return
	}
	q := &store.SavedQuery{ID: id.New(), OwnerID: rc.user.ID, ConnectionID: req.ConnectionID, Database: req.Database, Name: req.Name,
		Description: req.Description, Body: req.Body, Tags: req.Tags, Visibility: req.Visibility}
	if err := s.store.SaveQuery(r.Context(), q); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	q.OwnerName = rc.user.Name
	writeJSON(w, 201, q)
}

func (s *Server) updateSaved(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	q, err := s.store.SavedQueryByID(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, 404, "saved query not found")
		return
	}
	if q.OwnerID != rc.user.ID && !rc.user.Role.AtLeast(store.RoleAdmin) {
		writeErr(w, 403, "only the author can edit this query")
		return
	}
	var req savedReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if msg := req.validate(); msg != "" {
		writeErr(w, 400, msg)
		return
	}
	q.ConnectionID, q.Database, q.Name, q.Description, q.Body, q.Tags, q.Visibility =
		req.ConnectionID, req.Database, req.Name, req.Description, req.Body, req.Tags, req.Visibility
	if err := s.store.SaveQuery(r.Context(), q); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, q)
}

func (s *Server) deleteSaved(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	q, err := s.store.SavedQueryByID(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, 404, "saved query not found")
		return
	}
	if q.OwnerID != rc.user.ID && !rc.user.Role.AtLeast(store.RoleAdmin) {
		writeErr(w, 403, "only the author can delete this query")
		return
	}
	_ = s.store.DeleteSavedQuery(r.Context(), q.ID)
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) listNotes(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	c, _, ok := s.connFor(w, r, rc, store.AccessRead)
	if !ok {
		return
	}
	path := r.URL.Query().Get("path")
	if path == "" {
		path = "*"
	}
	list, err := s.store.Notes(r.Context(), c.ID, path)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if list == nil {
		list = []store.Note{}
	}
	writeJSON(w, 200, list)
}

func (s *Server) addNote(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	c, _, ok := s.connFor(w, r, rc, store.AccessRead)
	if !ok {
		return
	}
	var req struct {
		Path string `json:"path"`
		Body string `json:"body"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	body := strings.TrimSpace(req.Body)
	if body == "" || len(body) > 20000 {
		writeErr(w, 400, "note must be 1 to 20000 characters")
		return
	}
	n := &store.Note{ID: id.New(), ConnectionID: c.ID, ObjectPath: truncate(req.Path, 500), AuthorID: rc.user.ID, Body: body}
	if err := s.store.AddNote(r.Context(), n); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	n.AuthorName = rc.user.Name
	writeJSON(w, 201, n)
}

func (s *Server) deleteNote(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	n, err := s.store.NoteByID(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, 404, "note not found")
		return
	}
	if n.AuthorID != rc.user.ID && !rc.user.Role.AtLeast(store.RoleAdmin) {
		writeErr(w, 403, "only the author can delete this note")
		return
	}
	_ = s.store.DeleteNote(r.Context(), n.ID)
	writeJSON(w, 200, map[string]any{"ok": true})
}
