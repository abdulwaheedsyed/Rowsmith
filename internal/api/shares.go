package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"rowsmith/internal/driver"
	"rowsmith/internal/id"
	"rowsmith/internal/schedule"
	"rowsmith/internal/store"
)

const (
	snapshotRows  = 500
	snapshotBytes = 2 << 20
	maxShareBody  = 256 << 10
	maxComment    = 10_000
)

// mentionRe finds mentions stored in comments as <@user-id>.
var mentionRe = regexp.MustCompile(`<@([0-9a-z]{10,40})>`)

// shareAccess is what the current person may do with a share.
type shareAccess struct {
	author, admin bool
	results       bool // may see the result snapshot and run the query: has read access to the connection
	expired       bool
}

func canViewShare(x *store.QueryShare, u *store.User) bool {
	return x.AuthorID == u.ID || u.Role.AtLeast(store.RoleAdmin) || x.Audience == "team" || slices.Contains(x.People, u.ID)
}

func (s *Server) shareAccessFor(ctx context.Context, x *store.QueryShare, u *store.User) shareAccess {
	a := shareAccess{author: x.AuthorID == u.ID, admin: u.Role.AtLeast(store.RoleAdmin)}
	a.expired = x.ExpiresAt > 0 && store.Now() > x.ExpiresAt
	if x.ConnectionID != "" {
		if c, err := s.store.ConnectionByID(ctx, x.ConnectionID); err == nil {
			if acc, err := s.store.AccessFor(ctx, c, u.ID); err == nil && effectiveAccess(u, acc) != store.AccessNone {
				a.results = true
			}
		}
	}
	return a
}

// shareFor loads a share the current person may see.
func (s *Server) shareFor(w http.ResponseWriter, r *http.Request, rc *reqCtx, shareID string) (*store.QueryShare, shareAccess, bool) {
	x, err := s.store.ShareByID(r.Context(), shareID)
	if err != nil || !canViewShare(x, rc.user) {
		writeErr(w, 404, "this shared query does not exist, or it is not shared with you")
		return nil, shareAccess{}, false
	}
	a := s.shareAccessFor(r.Context(), x, rc.user)
	if a.expired && !a.author && !a.admin {
		writeErr(w, 410, "this link expired on "+time.UnixMilli(x.ExpiresAt).UTC().Format("2 Jan 2006")+"; ask "+x.AuthorName+" to share it again")
		return nil, shareAccess{}, false
	}
	return x, a, true
}

type personRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (s *Server) people(ctx context.Context) map[string]*store.User {
	out := map[string]*store.User{}
	if users, err := s.store.ListUsers(ctx); err == nil {
		for _, u := range users {
			out[u.ID] = u
		}
	}
	return out
}

func (s *Server) shareView(x *store.QueryShare, a shareAccess, users map[string]*store.User) map[string]any {
	b, _ := json.Marshal(x)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	people := []personRef{}
	for _, id := range x.People {
		if u, ok := users[id]; ok {
			people = append(people, personRef{u.ID, u.Name})
		}
	}
	out["people"] = people
	out["hasResult"] = x.Result != ""
	out["expired"] = a.expired
	out["can"] = map[string]bool{"edit": a.author, "delete": a.author || a.admin, "results": a.results, "run": a.results}
	return out
}

type shareReq struct {
	ConnectionID  string   `json:"connectionId"`
	Database      string   `json:"database"`
	Schema        string   `json:"schema"`
	Title         string   `json:"title"`
	Description   string   `json:"description"`
	Body          string   `json:"body"`
	Audience      string   `json:"audience"`
	People        []string `json:"people"`
	IncludeResult bool     `json:"includeResult"`
	ExpiresDays   *int     `json:"expiresDays"` // 0: never
}

// cleanPeople keeps active teammates other than the author.
func cleanPeople(ids []string, users map[string]*store.User, author string) ([]string, error) {
	out := []string{}
	for _, id := range ids {
		u, ok := users[id]
		if !ok || u.Disabled {
			return nil, statusErr{400, "one of the people is not an active member of the team"}
		}
		if id != author && !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	if len(out) > 200 {
		return nil, statusErr{400, "share with at most 200 people, or with the whole team"}
	}
	return out, nil
}

func expiry(days *int) (int64, error) {
	if days == nil || *days == 0 {
		return 0, nil
	}
	if *days < 0 || *days > 365 {
		return 0, statusErr{400, "a link can last up to 365 days"}
	}
	return time.Now().Add(time.Duration(*days) * 24 * time.Hour).UnixMilli(), nil
}

func (s *Server) listShares(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	ctx := r.Context()
	all := r.URL.Query().Get("all") == "1" && rc.user.Role.AtLeast(store.RoleAdmin)
	list, err := s.store.SharesFor(ctx, rc.user.ID, all, 300)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	users := s.people(ctx)
	unread := map[string]bool{}
	if items, _, err := s.store.Notifications(ctx, rc.user.ID, 200); err == nil {
		for _, n := range items {
			if n.ReadAt == 0 {
				unread[n.ShareID] = true
			}
		}
	}
	out := make([]map[string]any, 0, len(list))
	for _, x := range list {
		people, _ := s.store.SharePeople(ctx, x.ID)
		x.People = people
		a := s.shareAccessFor(ctx, x, rc.user)
		if a.expired && !a.author && !a.admin {
			continue
		}
		v := s.shareView(x, a, users)
		if len(x.Body) > 400 {
			v["body"] = x.Body[:400]
		}
		v["unread"] = unread[x.ID]
		out = append(out, v)
	}
	writeJSON(w, 200, out)
}

func (s *Server) createShare(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	var req shareReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if !s.allowTalk(w, rc) {
		return
	}
	if req.IncludeResult {
		if ok, _ := s.schedLimit.Allow(rc.user.ID); !ok {
			writeErr(w, 429, "too many queries run in a short time; wait a moment")
			return
		}
	}
	ctx := r.Context()
	req.Title = strings.TrimSpace(req.Title)
	switch {
	case req.Title == "":
		writeErr(w, 400, "give the shared query a title")
		return
	case len(req.Title) > 200 || len(req.Description) > 4000:
		writeErr(w, 400, "the title or description is too long")
		return
	case strings.TrimSpace(req.Body) == "":
		writeErr(w, 400, "there is no query to share")
		return
	case len(req.Body) > maxShareBody:
		writeErr(w, 400, "the query is too long to share")
		return
	}
	if req.Audience == "" {
		req.Audience = "team"
	}
	if req.Audience != "team" && req.Audience != "people" {
		writeErr(w, 400, "share with the team or with chosen people")
		return
	}
	c, err := s.readableConn(ctx, rc, req.ConnectionID)
	if err != nil {
		s.writeStatusErr(w, err)
		return
	}
	users := s.people(ctx)
	people, err := cleanPeople(req.People, users, rc.user.ID)
	if err != nil {
		s.writeStatusErr(w, err)
		return
	}
	if req.Audience == "people" && len(people) == 0 {
		writeErr(w, 400, "choose who to share it with")
		return
	}
	if req.Audience == "team" {
		people = []string{}
	}
	expires, err := expiry(req.ExpiresDays)
	if err != nil {
		s.writeStatusErr(w, err)
		return
	}
	x := &store.QueryShare{ID: id.New(), AuthorID: rc.user.ID, ConnectionID: c.ID, Connection: c.Name, Driver: c.Driver,
		Database: req.Database, Schema: req.Schema, Title: req.Title, Description: strings.TrimSpace(req.Description), Body: req.Body,
		Audience: req.Audience, People: people, ExpiresAt: expires}
	if req.IncludeResult {
		snap, err := s.snapshot(ctx, c, req.Database, req.Schema, req.Body)
		if err != nil {
			writeErr(w, 400, "could not include the result: "+err.Error())
			return
		}
		b, _ := json.Marshal(snap)
		if x.Result, err = s.vault.Seal(b, x.ResultAAD()); err != nil {
			writeErr(w, 500, "could not seal the result")
			return
		}
		x.ResultRows, x.ResultAt = snap.RowCount, store.Now()
	}
	if err := s.store.CreateShare(ctx, x); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	s.audit(ctx, rc, "query.shared", x.Title, map[string]any{"share": x.ID, "connection": c.Name, "audience": x.Audience,
		"people": len(people), "result": x.Result != "", "rows": x.ResultRows})
	s.notify(ctx, x, rc.user, "shared", nil, people)
	x, _ = s.store.ShareByID(ctx, x.ID)
	writeJSON(w, 201, s.shareView(x, s.shareAccessFor(ctx, x, rc.user), users))
}

type snapshotData struct {
	Columns   []driver.ResultColumn `json:"columns"`
	Rows      [][]any               `json:"rows"`
	RowCount  int64                 `json:"rowCount"`
	Truncated bool                  `json:"truncated"`
	MS        int64                 `json:"ms"`
}

// snapshot runs a read-only query once and keeps the start of its result.
func (s *Server) snapshot(ctx context.Context, c *store.Connection, database, schema, body string) (*snapshotData, error) {
	d, ok := driver.Get(c.Driver)
	if !ok {
		return nil, errors.New("this database type is not available")
	}
	if err := schedule.CheckStatements(d.Info(), body); err != nil {
		return nil, errors.New("results can only be included for queries that only read")
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	lease, err := s.sessions.Acquire(ctx, c, true)
	if err != nil {
		return nil, err
	}
	defer lease.Release()
	sess, err := lease.Conn.NewSession(ctx, driver.Scope{Database: database, Schema: schema})
	if err != nil {
		return nil, err
	}
	defer sess.Close()
	sink := &snapshotSink{max: snapshotRows}
	started := time.Now()
	err = sess.Execute(ctx, body, driver.ExecOptions{MaxRows: snapshotRows, ReadOnly: true, StopOnError: true}, sink)
	if err == nil {
		err = sink.err
	}
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, errors.New("the query took longer than a minute")
		}
		var qe *driver.QueryError
		if errors.As(err, &qe) {
			return nil, errors.New(qe.Message)
		}
		return nil, err
	}
	if sink.cols == nil {
		return nil, errors.New("the query returned no rows to include")
	}
	snap := &snapshotData{Columns: sink.cols, Rows: sink.rows, RowCount: max(sink.total, int64(len(sink.rows))), MS: time.Since(started).Milliseconds()}
	snap.Truncated = sink.truncated || snap.RowCount > int64(len(snap.Rows))
	for {
		b, _ := json.Marshal(snap)
		if len(b) <= snapshotBytes || len(snap.Rows) == 0 {
			break
		}
		snap.Rows, snap.Truncated = snap.Rows[:len(snap.Rows)/2], true
	}
	if snap.Rows == nil {
		snap.Rows = [][]any{}
	}
	return snap, nil
}

type snapshotSink struct {
	max       int
	state     int
	cols      []driver.ResultColumn
	rows      [][]any
	total     int64
	truncated bool
	err       error
}

func (k *snapshotSink) BeginStatement(driver.StatementInfo) error { return nil }
func (k *snapshotSink) Columns(c []driver.ResultColumn) error {
	if k.state == 0 {
		k.state, k.cols = 1, c
	} else if k.state == 1 {
		k.state = 2
	}
	return nil
}
func (k *snapshotSink) Rows(rows [][]any) error {
	if k.state != 1 {
		return nil
	}
	for _, row := range rows {
		if len(k.rows) >= k.max {
			k.truncated = true
			return nil
		}
		k.rows = append(k.rows, append([]any(nil), row...))
	}
	return nil
}
func (k *snapshotSink) EndResult(sum driver.ResultSummary) error {
	if k.state == 1 {
		k.state, k.total = 2, sum.RowCount
		k.truncated = k.truncated || sum.Truncated
	}
	return nil
}
func (k *snapshotSink) Notice(string, string) error { return nil }
func (k *snapshotSink) EndStatement(err error) error {
	if err != nil && k.err == nil {
		k.err = err
	}
	return nil
}

func (s *Server) getShare(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	x, a, ok := s.shareFor(w, r, rc, r.PathValue("id"))
	if !ok {
		return
	}
	ctx := r.Context()
	users := s.people(ctx)
	out := s.shareView(x, a, users)
	comments, err := s.store.Comments(ctx, x.ID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	out["comments"] = comments
	if x.Result != "" && a.results {
		var snap snapshotData
		if raw, err := s.vault.Open(x.Result, x.ResultAAD()); err == nil && json.Unmarshal(raw, &snap) == nil {
			out["result"] = snap
		}
	}
	names := map[string]string{}
	for id, u := range users {
		names[id] = u.Name
	}
	out["names"] = names // for rendering mentions
	_ = s.store.MarkShareRead(ctx, rc.user.ID, x.ID)
	writeJSON(w, 200, out)
}

func (s *Server) updateShare(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	x, a, ok := s.shareFor(w, r, rc, r.PathValue("id"))
	if !ok {
		return
	}
	if !a.author {
		writeErr(w, 403, "only "+x.AuthorName+" can change who sees this")
		return
	}
	var req struct {
		Title       *string  `json:"title"`
		Description *string  `json:"description"`
		Audience    *string  `json:"audience"`
		People      []string `json:"people"`
		ExpiresDays *int     `json:"expiresDays"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	ctx := r.Context()
	users := s.people(ctx)
	before := slices.Clone(x.People)
	if req.Title != nil {
		if t := strings.TrimSpace(*req.Title); t != "" && len(t) <= 200 {
			x.Title = t
		}
	}
	if req.Description != nil && len(*req.Description) <= 4000 {
		x.Description = strings.TrimSpace(*req.Description)
	}
	if req.Audience != nil {
		if *req.Audience != "team" && *req.Audience != "people" {
			writeErr(w, 400, "share with the team or with chosen people")
			return
		}
		x.Audience = *req.Audience
	}
	if req.People != nil {
		people, err := cleanPeople(req.People, users, x.AuthorID)
		if err != nil {
			s.writeStatusErr(w, err)
			return
		}
		x.People = people
	}
	if x.Audience == "team" {
		x.People = []string{}
	} else if len(x.People) == 0 {
		writeErr(w, 400, "choose who to share it with")
		return
	}
	if req.ExpiresDays != nil {
		exp, err := expiry(req.ExpiresDays)
		if err != nil {
			s.writeStatusErr(w, err)
			return
		}
		x.ExpiresAt = exp
	}
	if err := s.store.UpdateShare(ctx, x); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	var added []string
	for _, p := range x.People {
		if !slices.Contains(before, p) {
			added = append(added, p)
		}
	}
	s.notify(ctx, x, rc.user, "shared", nil, added)
	s.audit(ctx, rc, "query.share_updated", x.Title, map[string]any{"share": x.ID, "audience": x.Audience, "people": len(x.People)})
	x, _ = s.store.ShareByID(ctx, x.ID)
	writeJSON(w, 200, s.shareView(x, s.shareAccessFor(ctx, x, rc.user), users))
}

func (s *Server) deleteShare(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	x, a, ok := s.shareFor(w, r, rc, r.PathValue("id"))
	if !ok {
		return
	}
	if !a.author && !a.admin {
		writeErr(w, 403, "only "+x.AuthorName+" or an admin can delete this")
		return
	}
	if err := s.store.DeleteShare(r.Context(), x.ID); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	s.audit(r.Context(), rc, "query.share_deleted", x.Title, map[string]any{"share": x.ID, "author": x.AuthorName})
	writeJSON(w, 200, map[string]any{"ok": true})
}

// mentions returns the people a comment mentions, checking each can see it.
func (s *Server) mentions(x *store.QueryShare, body string, users map[string]*store.User) ([]string, error) {
	var out []string
	for _, m := range mentionRe.FindAllStringSubmatch(body, -1) {
		u, ok := users[m[1]]
		if !ok || u.Disabled {
			return nil, statusErr{400, "you mentioned someone who is not an active member of the team"}
		}
		if !canViewShare(x, u) {
			return nil, statusErr{400, u.Name + " can't see this query; ask " + x.AuthorName + " to share it with them first"}
		}
		if !slices.Contains(out, u.ID) {
			out = append(out, u.ID)
		}
	}
	return out, nil
}

func (s *Server) addComment(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	x, a, ok := s.shareFor(w, r, rc, r.PathValue("id"))
	if !ok {
		return
	}
	if a.expired {
		writeErr(w, 409, "this link has expired; extend it to continue the discussion")
		return
	}
	var req struct {
		Body     string `json:"body"`
		Line     int    `json:"line"`
		ParentID string `json:"parentId"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if !s.allowTalk(w, rc) {
		return
	}
	ctx := r.Context()
	body := strings.TrimSpace(req.Body)
	if body == "" || len(body) > maxComment {
		writeErr(w, 400, "write a comment of up to 10,000 characters")
		return
	}
	c := &store.Comment{ID: id.New(), ShareID: x.ID, AuthorID: rc.user.ID, AuthorName: rc.user.Name, Body: body, Line: req.Line}
	var thread []*store.Comment
	if req.ParentID != "" {
		parent, err := s.store.CommentByID(ctx, req.ParentID)
		if err != nil || parent.ShareID != x.ID || parent.ParentID != "" {
			writeErr(w, 400, "reply to a comment on this query")
			return
		}
		c.ParentID, c.Line = parent.ID, parent.Line
		all, _ := s.store.Comments(ctx, x.ID)
		for _, o := range all {
			if o.ID == parent.ID || o.ParentID == parent.ID {
				thread = append(thread, o)
			}
		}
	} else if lines := strings.Count(x.Body, "\n") + 1; c.Line < 0 || c.Line > lines {
		writeErr(w, 400, "that line is not in the query")
		return
	}
	users := s.people(ctx)
	mentioned, err := s.mentions(x, body, users)
	if err != nil {
		s.writeStatusErr(w, err)
		return
	}
	if err := s.store.AddComment(ctx, c); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	// Who hears about it, each once, most specific reason first.
	told := map[string]bool{rc.user.ID: true}
	pick := func(ids []string) []string {
		var out []string
		for _, id := range ids {
			if u, ok := users[id]; ok && !told[id] && !u.Disabled && canViewShare(x, u) {
				told[id] = true
				out = append(out, id)
			}
		}
		return out
	}
	s.notify(ctx, x, rc.user, "mention", c, pick(mentioned))
	var participants []string
	for _, o := range thread {
		participants = append(participants, o.AuthorID)
	}
	s.notify(ctx, x, rc.user, "reply", c, pick(participants))
	s.notify(ctx, x, rc.user, "comment", c, pick([]string{x.AuthorID}))
	writeJSON(w, 201, c)
}

// commentFor loads a comment and its share, if the current person may see them.
func (s *Server) commentFor(w http.ResponseWriter, r *http.Request, rc *reqCtx) (*store.Comment, *store.QueryShare, shareAccess, bool) {
	c, err := s.store.CommentByID(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, 404, "comment not found")
		return nil, nil, shareAccess{}, false
	}
	x, a, ok := s.shareFor(w, r, rc, c.ShareID)
	if !ok {
		return nil, nil, shareAccess{}, false
	}
	return c, x, a, true
}

func (s *Server) editComment(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	c, x, _, ok := s.commentFor(w, r, rc)
	if !ok {
		return
	}
	if c.AuthorID != rc.user.ID {
		writeErr(w, 403, "you can only edit your own comments")
		return
	}
	var req struct {
		Body string `json:"body"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	body := strings.TrimSpace(req.Body)
	if body == "" || len(body) > maxComment {
		writeErr(w, 400, "write a comment of up to 10,000 characters")
		return
	}
	ctx := r.Context()
	users := s.people(ctx)
	mentioned, err := s.mentions(x, body, users)
	if err != nil {
		s.writeStatusErr(w, err)
		return
	}
	before, _ := s.mentions(x, c.Body, users)
	if err := s.store.EditComment(ctx, c.ID, body); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	var added []string
	for _, m := range mentioned {
		if m != rc.user.ID && !slices.Contains(before, m) {
			added = append(added, m)
		}
	}
	c.Body = body
	s.notify(ctx, x, rc.user, "mention", c, added)
	c, _ = s.store.CommentByID(ctx, c.ID)
	writeJSON(w, 200, c)
}

func (s *Server) deleteComment(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	c, _, a, ok := s.commentFor(w, r, rc)
	if !ok {
		return
	}
	if c.AuthorID != rc.user.ID && !a.author && !a.admin {
		writeErr(w, 403, "you can only delete your own comments")
		return
	}
	if err := s.store.DeleteComment(r.Context(), c.ID); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) resolveComment(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	c, _, a, ok := s.commentFor(w, r, rc)
	if !ok {
		return
	}
	if c.ParentID != "" {
		writeErr(w, 400, "resolve the thread from its first comment")
		return
	}
	if c.AuthorID != rc.user.ID && !a.author && !a.admin {
		writeErr(w, 403, "the thread's author or the query's author can resolve it")
		return
	}
	var req struct {
		Resolved bool `json:"resolved"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := s.store.ResolveThread(r.Context(), c.ID, rc.user.ID, req.Resolved); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	c, _ = s.store.CommentByID(r.Context(), c.ID)
	writeJSON(w, 200, c)
}

func (s *Server) listNotifications(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	items, unread, err := s.store.Notifications(r.Context(), rc.user.ID, 50)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	users := s.people(r.Context())
	for _, n := range items {
		n.Excerpt = mentionText(n.Excerpt, users)
	}
	writeJSON(w, 200, map[string]any{"items": items, "unread": unread})
}

func (s *Server) readNotifications(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	var req struct {
		IDs []string `json:"ids"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if len(req.IDs) > 500 {
		req.IDs = req.IDs[:500]
	}
	if err := s.store.MarkRead(r.Context(), rc.user.ID, req.IDs); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) allowTalk(w http.ResponseWriter, rc *reqCtx) bool {
	if ok, _ := s.talkLimit.Allow(rc.user.ID); !ok {
		writeErr(w, 429, "you are posting quickly; wait a moment")
		return false
	}
	return true
}

// mentionText shows <@id> mentions as @Name, for emails and previews.
func mentionText(body string, users map[string]*store.User) string {
	return mentionRe.ReplaceAllStringFunc(body, func(m string) string {
		if u, ok := users[mentionRe.FindStringSubmatch(m)[1]]; ok {
			return "@" + u.Name
		}
		return "@someone"
	})
}
