package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"rowsmith/internal/driver"
	"rowsmith/internal/id"
	"rowsmith/internal/session"
	"rowsmith/internal/store"
	"rowsmith/internal/tunnel"
)

func (s *Server) listDrivers(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	writeJSON(w, 200, driver.All())
}

// connView is what clients see: never secret values, only which are set.
type connView struct {
	*store.Connection
	Access     store.Access    `json:"access"`
	OwnerName  string          `json:"ownerName,omitempty"`
	SecretsSet map[string]bool `json:"secretsSet"`
}

func (s *Server) view(c *store.Connection, acc store.Access, owner string) connView {
	v := connView{Connection: c, Access: acc, OwnerName: owner, SecretsSet: map[string]bool{}}
	if acc.AtLeast(store.AccessManage) {
		if sec, err := s.sessions.Secrets(c); err == nil {
			for k, val := range sec {
				v.SecretsSet[k] = val != ""
			}
		}
	}
	return v
}

func (s *Server) listConnections(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	list, err := s.store.ConnectionsFor(r.Context(), rc.user.ID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	out := make([]connView, 0, len(list))
	for _, vc := range list {
		acc := effectiveAccess(rc.user, vc.Access)
		v := connView{Connection: vc.Connection, Access: acc, OwnerName: vc.OwnerName, SecretsSet: map[string]bool{}}
		if !acc.AtLeast(store.AccessManage) {
			v.Connection = redacted(vc.Connection)
		}
		out = append(out, v)
	}
	writeJSON(w, 200, out)
}

// redacted hides connection details from users who may only use it.
func redacted(c *store.Connection) *store.Connection {
	cp := *c
	var params map[string]any
	_ = json.Unmarshal(c.Params, &params)
	keep := map[string]any{}
	for _, k := range []string{"host", "port", "database", "project", "service", "file"} {
		if v, ok := params[k]; ok {
			keep[k] = v
		}
	}
	cp.Params, _ = json.Marshal(keep)
	cp.SSH = json.RawMessage(`{}`)
	return &cp
}

func effectiveAccess(u *store.User, a store.Access) store.Access {
	if u.Role == store.RoleViewer && a.AtLeast(store.AccessRead) {
		return store.AccessRead
	}
	return a
}

type connReq struct {
	Name        *string              `json:"name"`
	Driver      *string              `json:"driver"`
	Color       *string              `json:"color"`
	Environment *string              `json:"environment"`
	Folder      *string              `json:"folder"`
	ReadOnly    *bool                `json:"readOnly"`
	TeamAccess  *store.Access        `json:"teamAccess"`
	Notes       *string              `json:"notes"`
	Params      map[string]any       `json:"params"`
	Secrets     map[string]*string   `json:"secrets"` // null = clear, absent = keep
	SSH         *session.SSHSettings `json:"ssh"`
}

var environments = map[string]bool{"production": true, "staging": true, "development": true, "local": true}

// apply merges a request into c, returning the merged secrets map.
func (s *Server) apply(c *store.Connection, req connReq, existing map[string]string) (map[string]string, error) {
	if req.Name != nil {
		c.Name = strings.TrimSpace(*req.Name)
	}
	if req.Driver != nil {
		c.Driver = *req.Driver
	}
	drv, ok := driver.Get(c.Driver)
	if !ok {
		return nil, errors.New("choose a database type")
	}
	if c.Name == "" {
		return nil, errors.New("give the connection a name")
	}
	if req.Color != nil {
		c.Color = truncate(*req.Color, 32)
	}
	if req.Environment != nil {
		if !environments[*req.Environment] {
			return nil, errors.New("invalid environment")
		}
		c.Environment = *req.Environment
	}
	if c.Environment == "" {
		c.Environment = "development"
	}
	if req.Folder != nil {
		c.Folder = truncate(strings.TrimSpace(*req.Folder), 120)
	}
	if req.ReadOnly != nil {
		c.ReadOnly = *req.ReadOnly
	}
	if req.Notes != nil {
		c.Notes = truncate(*req.Notes, 20000)
	}
	if req.TeamAccess != nil {
		if *req.TeamAccess != "" && !req.TeamAccess.Valid() {
			return nil, errors.New("invalid team access level")
		}
		c.TeamAccess = *req.TeamAccess
	}

	secrets := map[string]string{}
	for k, v := range existing {
		secrets[k] = v
	}
	secretField := map[string]bool{}
	known := map[string]bool{}
	for _, f := range drv.Info().Fields {
		known[f.Key] = true
		if f.Secret {
			secretField[f.Key] = true
		}
	}
	if req.Params != nil {
		params := map[string]any{}
		for k, v := range req.Params {
			if !known[k] {
				continue
			}
			if secretField[k] {
				// A secret sent in params is still treated as a secret.
				if sv, ok := v.(string); ok && sv != "" {
					secrets[k] = sv
				}
				continue
			}
			params[k] = v
		}
		raw, _ := json.Marshal(params)
		c.Params = raw
	}
	for k, v := range req.Secrets {
		if !secretField[k] && !strings.HasPrefix(k, "ssh.") {
			continue
		}
		if v == nil {
			delete(secrets, k)
		} else if *v != "" {
			secrets[k] = *v
		}
	}
	if req.SSH != nil {
		ssh := *req.SSH
		if len(ssh.Hops) > 4 {
			return nil, errors.New("at most 4 SSH hops are supported")
		}
		for i := range ssh.Hops {
			h := &ssh.Hops[i]
			h.Host = strings.TrimSpace(h.Host)
			if h.Port == 0 {
				h.Port = 22
			}
			if h.Auth != "key" {
				h.Auth = "password"
			}
			if ssh.Enabled && (h.Host == "" || h.User == "") {
				return nil, errors.New("each SSH hop needs a host and a user")
			}
		}
		// Drop secrets of removed hops.
		for k := range secrets {
			if strings.HasPrefix(k, "ssh.") {
				parts := strings.SplitN(k, ".", 3)
				if len(parts) == 3 {
					if n := atoiSafe(parts[1]); n >= len(ssh.Hops) {
						delete(secrets, k)
					}
				}
			}
		}
		raw, _ := json.Marshal(ssh)
		c.SSH = raw
	}
	return secrets, nil
}

func atoiSafe(s string) int {
	n := 0
	for _, ch := range s {
		if ch < '0' || ch > '9' {
			return 1 << 30
		}
		n = n*10 + int(ch-'0')
	}
	return n
}

func (s *Server) seal(c *store.Connection, secrets map[string]string) error {
	if len(secrets) == 0 {
		c.Secrets = ""
		return nil
	}
	env, err := s.vault.SealJSON(secrets, c.SecretsAAD())
	if err != nil {
		return err
	}
	c.Secrets = env
	return nil
}

func (s *Server) createConnection(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	if rc.user.Role == store.RoleViewer {
		writeErr(w, 403, "viewers cannot create connections")
		return
	}
	var req connReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	c := &store.Connection{ID: id.New(), OwnerID: rc.user.ID}
	secrets, err := s.apply(c, req, nil)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := s.seal(c, secrets); err != nil {
		writeErr(w, 500, "could not encrypt secrets")
		return
	}
	if err := s.store.CreateConnection(r.Context(), c); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	s.audit(r.Context(), rc, "connection.created", c.ID, map[string]any{"name": c.Name, "driver": c.Driver})
	writeJSON(w, 201, s.view(c, store.AccessManage, rc.user.Name))
}

// connFor loads a connection and checks the caller's access.
func (s *Server) connFor(w http.ResponseWriter, r *http.Request, rc *reqCtx, need store.Access) (*store.Connection, store.Access, bool) {
	c, err := s.store.ConnectionByID(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, 404, "connection not found")
		return nil, "", false
	}
	acc, err := s.store.AccessFor(r.Context(), c, rc.user.ID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return nil, "", false
	}
	acc = effectiveAccess(rc.user, acc)
	if !acc.AtLeast(need) || acc == store.AccessNone {
		if acc == store.AccessNone {
			writeErr(w, 404, "connection not found")
		} else {
			writeErr(w, 403, "you do not have "+string(need)+" access to this connection")
		}
		return nil, "", false
	}
	return c, acc, true
}

func (s *Server) getConnection(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	c, acc, ok := s.connFor(w, r, rc, store.AccessRead)
	if !ok {
		return
	}
	if !acc.AtLeast(store.AccessManage) {
		c = redacted(c)
	}
	writeJSON(w, 200, s.view(c, acc, ""))
}

func (s *Server) updateConnection(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	c, acc, ok := s.connFor(w, r, rc, store.AccessManage)
	if !ok {
		return
	}
	var req connReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	existing, err := s.sessions.Secrets(c)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if req.TeamAccess != nil && c.OwnerID != rc.user.ID && !rc.user.Role.AtLeast(store.RoleAdmin) {
		writeErr(w, 403, "only the owner can change team sharing")
		return
	}
	secrets, err := s.apply(c, req, existing)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := s.seal(c, secrets); err != nil {
		writeErr(w, 500, "could not encrypt secrets")
		return
	}
	if err := s.store.UpdateConnection(r.Context(), c); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	s.sessions.Invalidate(c.ID)
	changedSecrets := []string{}
	for k := range req.Secrets {
		changedSecrets = append(changedSecrets, k)
	}
	s.audit(r.Context(), rc, "connection.updated", c.ID, map[string]any{"name": c.Name, "secretsChanged": changedSecrets})
	writeJSON(w, 200, s.view(c, acc, ""))
}

func (s *Server) deleteConnection(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	c, _, ok := s.connFor(w, r, rc, store.AccessManage)
	if !ok {
		return
	}
	if c.OwnerID != rc.user.ID && !rc.user.Role.AtLeast(store.RoleAdmin) {
		writeErr(w, 403, "only the owner or an administrator can delete this connection")
		return
	}
	s.sessions.Invalidate(c.ID)
	if err := s.store.DeleteConnection(r.Context(), c.ID); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	s.audit(r.Context(), rc, "connection.deleted", c.ID, map[string]any{"name": c.Name})
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) duplicateConnection(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	c, _, ok := s.connFor(w, r, rc, store.AccessManage)
	if !ok {
		return
	}
	secrets, err := s.sessions.Secrets(c)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	cp := *c
	cp.ID, cp.OwnerID, cp.Name, cp.TeamAccess = id.New(), rc.user.ID, c.Name+" (copy)", ""
	if err := s.seal(&cp, secrets); err != nil {
		writeErr(w, 500, "could not encrypt secrets")
		return
	}
	if err := s.store.CreateConnection(r.Context(), &cp); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	s.audit(r.Context(), rc, "connection.duplicated", cp.ID, map[string]any{"from": c.ID})
	writeJSON(w, 201, s.view(&cp, store.AccessManage, rc.user.Name))
}

type testReq struct {
	connReq
	ID string `json:"id"` // when editing: reuse stored secrets not re-entered
}

func (s *Server) testConnection(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	if rc.user.Role == store.RoleViewer {
		writeErr(w, 403, "viewers cannot create connections")
		return
	}
	var req testReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	c := &store.Connection{ID: "test", OwnerID: rc.user.ID, UpdatedAt: time.Now().UnixMilli()}
	var existing map[string]string
	if req.ID != "" {
		stored, err := s.store.ConnectionByID(r.Context(), req.ID)
		if err == nil {
			if acc, _ := s.store.AccessFor(r.Context(), stored, rc.user.ID); acc.AtLeast(store.AccessManage) {
				*c = *stored
				existing, _ = s.sessions.Secrets(stored)
			}
		}
	}
	secrets, err := s.apply(c, req.connReq, existing)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	s.runTest(w, r, rc, c, secrets)
}

func (s *Server) testSavedConnection(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	c, _, ok := s.connFor(w, r, rc, store.AccessRead)
	if !ok {
		return
	}
	secrets, err := s.sessions.Secrets(c)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	s.runTest(w, r, rc, c, secrets)
}

func (s *Server) runTest(w http.ResponseWriter, r *http.Request, rc *reqCtx, c *store.Connection, secrets map[string]string) {
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	info, dur, err := s.sessions.Test(ctx, c, secrets)
	if err != nil {
		writeDBErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"server": info, "latencyMs": dur.Milliseconds()})
}

func (s *Server) getShares(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	c, _, ok := s.connFor(w, r, rc, store.AccessManage)
	if !ok {
		return
	}
	shares, err := s.store.Shares(r.Context(), c.ID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if shares == nil {
		shares = []store.Share{}
	}
	writeJSON(w, 200, map[string]any{"teamAccess": c.TeamAccess, "shares": shares})
}

func (s *Server) putShares(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	c, _, ok := s.connFor(w, r, rc, store.AccessManage)
	if !ok {
		return
	}
	var req struct {
		TeamAccess *store.Access `json:"teamAccess"`
		Shares     []store.Share `json:"shares"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	for _, sh := range req.Shares {
		if !sh.Access.Valid() || sh.UserID == "" {
			writeErr(w, 400, "invalid share")
			return
		}
		if _, err := s.store.UserByID(r.Context(), sh.UserID); err != nil {
			writeErr(w, 400, "unknown user in shares")
			return
		}
	}
	if err := s.store.ReplaceShares(r.Context(), c.ID, req.Shares); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if req.TeamAccess != nil {
		if *req.TeamAccess != "" && !req.TeamAccess.Valid() {
			writeErr(w, 400, "invalid team access")
			return
		}
		c.TeamAccess = *req.TeamAccess
		if err := s.store.UpdateConnection(r.Context(), c); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
	}
	s.sessions.Invalidate(c.ID)
	s.audit(r.Context(), rc, "connection.shared", c.ID, map[string]any{"shares": req.Shares, "teamAccess": req.TeamAccess})
	writeJSON(w, 200, map[string]any{"ok": true})
}

// ---- SSH host keys ----------------------------------------------------------------

func (s *Server) trustHostKey(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	if rc.user.Role == store.RoleViewer {
		writeErr(w, 403, "viewers cannot trust SSH hosts")
		return
	}
	var req tunnel.UnknownHostKeyError
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if req.Host == "" || req.Port == 0 || req.KeyType == "" || req.PublicKey == "" {
		writeErr(w, 400, "incomplete host key")
		return
	}
	if fp, err := fingerprintOf(req.PublicKey); err != nil || fp != req.Fingerprint {
		writeErr(w, 400, "fingerprint does not match the key")
		return
	}
	if err := s.store.TrustHostKey(r.Context(), store.KnownHost{Host: req.Host, Port: req.Port, KeyType: req.KeyType,
		Fingerprint: req.Fingerprint, PublicKey: req.PublicKey, AddedBy: rc.user.Name}); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	s.audit(r.Context(), rc, "ssh.host_trusted", req.Host, map[string]any{"port": req.Port, "fingerprint": req.Fingerprint})
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) listKnownHosts(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	list, err := s.store.ListKnownHosts(r.Context())
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if list == nil {
		list = []store.KnownHost{}
	}
	writeJSON(w, 200, list)
}

func (s *Server) forgetHostKey(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	if !requireAdmin(w, rc) {
		return
	}
	var req struct {
		Host    string `json:"host"`
		Port    int    `json:"port"`
		KeyType string `json:"keyType"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := s.store.ForgetHostKey(r.Context(), req.Host, req.Port, req.KeyType); err != nil {
		writeErr(w, 404, "host key not found")
		return
	}
	s.audit(r.Context(), rc, "ssh.host_forgotten", req.Host, map[string]any{"port": req.Port})
	writeJSON(w, 200, map[string]any{"ok": true})
}
