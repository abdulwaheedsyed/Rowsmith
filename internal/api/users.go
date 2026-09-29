package api

import (
	"net/http"
	"strconv"
	"strings"

	"rowsmith/internal/auth"
	"rowsmith/internal/id"
	"rowsmith/internal/store"
)

func requireAdmin(w http.ResponseWriter, rc *reqCtx) bool {
	if !rc.user.Role.AtLeast(store.RoleAdmin) {
		writeErr(w, 403, "administrator access required")
		return false
	}
	return true
}

func (s *Server) listUsers(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	users, err := s.store.ListUsers(r.Context())
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	// Members see a directory (for sharing); admins see account state too.
	out := make([]map[string]any, 0, len(users))
	for _, u := range users {
		row := map[string]any{"id": u.ID, "name": u.Name, "email": u.Email, "role": u.Role}
		if rc.user.Role.AtLeast(store.RoleAdmin) {
			row["disabled"] = u.Disabled
			row["mfaEnabled"] = u.MFAEnabled
			row["lastLoginAt"] = u.LastLoginAt
			row["createdAt"] = u.CreatedAt
			row["mustChangePassword"] = u.MustChangePassword
		}
		if !u.Disabled || rc.user.Role.AtLeast(store.RoleAdmin) {
			out = append(out, row)
		}
	}
	writeJSON(w, 200, out)
}

type userReq struct {
	Name     *string     `json:"name"`
	Email    *string     `json:"email"`
	Role     *store.Role `json:"role"`
	Password *string     `json:"password"`
	Disabled *bool       `json:"disabled"`
}

func (s *Server) createUser(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	if !requireAdmin(w, rc) {
		return
	}
	var req userReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if req.Name == nil || req.Email == nil || req.Role == nil {
		writeErr(w, 400, "name, email and role are required")
		return
	}
	email := strings.ToLower(strings.TrimSpace(*req.Email))
	if !validEmail(email) || strings.TrimSpace(*req.Name) == "" || !req.Role.Valid() {
		writeErr(w, 400, "enter a name, a valid email and a role")
		return
	}
	if *req.Role == store.RoleOwner && rc.user.Role != store.RoleOwner {
		writeErr(w, 403, "only an owner can create another owner")
		return
	}
	// Admins set a temporary password; the user must change it on first sign-in.
	temp := ""
	if req.Password != nil && *req.Password != "" {
		temp = *req.Password
		if err := auth.CheckPasswordPolicy(temp, email); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
	} else {
		temp = generatePassword()
	}
	hash, err := auth.HashPassword(temp)
	if err != nil {
		writeErr(w, 500, "could not hash password")
		return
	}
	u := &store.User{ID: id.New(), Email: email, Name: strings.TrimSpace(*req.Name), PasswordHash: hash, Role: *req.Role, MustChangePassword: true}
	if err := s.store.CreateUser(r.Context(), u); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			writeErr(w, 409, "a user with that email already exists")
			return
		}
		writeErr(w, 500, err.Error())
		return
	}
	s.audit(r.Context(), rc, "user.created", u.Email, map[string]any{"role": u.Role})
	resp := map[string]any{"id": u.ID}
	if req.Password == nil || *req.Password == "" {
		resp["temporaryPassword"] = temp
	}
	writeJSON(w, 201, resp)
}

func (s *Server) updateUser(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	if !requireAdmin(w, rc) {
		return
	}
	target, err := s.store.UserByID(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, 404, "user not found")
		return
	}
	var req userReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if target.Role == store.RoleOwner && rc.user.Role != store.RoleOwner {
		writeErr(w, 403, "only an owner can modify an owner")
		return
	}
	p := store.UserPatch{Name: req.Name, Disabled: req.Disabled}
	if req.Email != nil {
		e := strings.ToLower(strings.TrimSpace(*req.Email))
		if !validEmail(e) {
			writeErr(w, 400, "invalid email")
			return
		}
		p.Email = &e
	}
	if req.Role != nil {
		if !req.Role.Valid() {
			writeErr(w, 400, "invalid role")
			return
		}
		if *req.Role == store.RoleOwner && rc.user.Role != store.RoleOwner {
			writeErr(w, 403, "only an owner can grant the owner role")
			return
		}
		p.Role = req.Role
	}
	demoting := target.Role == store.RoleOwner && ((req.Role != nil && *req.Role != store.RoleOwner) || (req.Disabled != nil && *req.Disabled))
	if demoting {
		if n, _ := s.store.CountRole(r.Context(), store.RoleOwner); n <= 1 {
			writeErr(w, 409, "there must always be at least one active owner")
			return
		}
	}
	if req.Password != nil && *req.Password != "" {
		if err := auth.CheckPasswordPolicy(*req.Password, target.Email); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		h, err := auth.HashPassword(*req.Password)
		if err != nil {
			writeErr(w, 500, "could not hash password")
			return
		}
		t := true
		p.PasswordHash, p.MustChangePassword = &h, &t
	}
	if err := s.store.UpdateUser(r.Context(), target.ID, p); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if (req.Disabled != nil && *req.Disabled) || p.PasswordHash != nil || p.Role != nil {
		_ = s.store.DeleteUserSessions(r.Context(), target.ID, "")
	}
	s.audit(r.Context(), rc, "user.updated", target.Email, map[string]any{"role": req.Role, "disabled": req.Disabled, "passwordReset": p.PasswordHash != nil})
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) deleteUser(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	if !requireAdmin(w, rc) {
		return
	}
	target, err := s.store.UserByID(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, 404, "user not found")
		return
	}
	if target.ID == rc.user.ID {
		writeErr(w, 409, "you cannot delete your own account")
		return
	}
	if target.Role == store.RoleOwner {
		if rc.user.Role != store.RoleOwner {
			writeErr(w, 403, "only an owner can delete an owner")
			return
		}
		if n, _ := s.store.CountRole(r.Context(), store.RoleOwner); n <= 1 {
			writeErr(w, 409, "there must always be at least one active owner")
			return
		}
	}
	// Their connections move to the admin performing the deletion so shared
	// resources are not lost.
	if err := s.store.TransferConnections(r.Context(), target.ID, rc.user.ID); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if err := s.store.DeleteUser(r.Context(), target.ID); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	s.audit(r.Context(), rc, "user.deleted", target.Email, nil)
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) resetUserMFA(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	if !requireAdmin(w, rc) {
		return
	}
	target, err := s.store.UserByID(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, 404, "user not found")
		return
	}
	if target.Role == store.RoleOwner && rc.user.Role != store.RoleOwner {
		writeErr(w, 403, "only an owner can reset an owner's two-factor authentication")
		return
	}
	_ = s.store.SetMFA(r.Context(), target.ID, "", false)
	_ = s.store.ReplaceRecoveryCodes(r.Context(), target.ID, nil)
	_ = s.store.DeleteUserSessions(r.Context(), target.ID, "")
	s.audit(r.Context(), rc, "user.mfa_reset", target.Email, nil)
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) listAudit(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	if !requireAdmin(w, rc) {
		return
	}
	q := r.URL.Query()
	before, _ := strconv.ParseInt(q.Get("before"), 10, 64)
	limit, _ := strconv.Atoi(q.Get("limit"))
	list, err := s.store.ListAudit(r.Context(), store.AuditFilter{UserID: q.Get("user"), Action: q.Get("action"), Before: before, Limit: limit})
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if list == nil {
		list = []store.AuditEntry{}
	}
	writeJSON(w, 200, list)
}

func (s *Server) adminOverview(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	if !requireAdmin(w, rc) {
		return
	}
	users, _ := s.store.ListUsers(r.Context())
	conns, _ := s.store.AllConnections(r.Context())
	mfa := 0
	for _, u := range users {
		if u.MFAEnabled {
			mfa++
		}
	}
	writeJSON(w, 200, map[string]any{
		"users": len(users), "usersWithMfa": mfa, "connections": len(conns),
		"live": s.sessions.Stats(), "version": Version, "keyId": s.vault.KeyID(),
	})
}

// Settings exposed to admins. Secret values are write-only.
var settingKeys = map[string]bool{
	"security.require_mfa": false,
	"smtp.host":            false, "smtp.port": false, "smtp.username": false, "smtp.password": true, "smtp.from": false, "smtp.tls": false,
	"ai.provider": false, "ai.model": false, "ai.api_key": true, "ai.send_samples": false, "ai.enabled": false,
	"map.tiles": false,
}

func (s *Server) getSettings(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	if !requireAdmin(w, rc) {
		return
	}
	out := map[string]any{}
	for k, secret := range settingKeys {
		v, ok, _ := s.store.Setting(r.Context(), k)
		if !ok {
			continue
		}
		if secret {
			out[k] = map[string]bool{"set": v != ""}
		} else {
			out[k] = v
		}
	}
	writeJSON(w, 200, out)
}

func (s *Server) putSettings(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	if !requireAdmin(w, rc) {
		return
	}
	var req map[string]*string
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	changed := []string{}
	for k, v := range req {
		secret, ok := settingKeys[k]
		if !ok {
			writeErr(w, 400, "unknown setting "+k)
			return
		}
		if v == nil {
			_ = s.store.DeleteSetting(r.Context(), k)
			changed = append(changed, k)
			continue
		}
		val := *v
		if secret {
			if val == "" {
				continue
			}
			sealed, err := s.vault.Seal([]byte(val), "setting:"+k)
			if err != nil {
				writeErr(w, 500, "could not seal secret")
				return
			}
			val = sealed
		}
		if err := s.store.SetSetting(r.Context(), k, val, secret); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		changed = append(changed, k)
	}
	s.audit(r.Context(), rc, "settings.updated", "", map[string]any{"keys": changed})
	writeJSON(w, 200, map[string]any{"ok": true})
}

// SecretSetting returns a decrypted secret setting ("" when unset).
func (s *Server) SecretSetting(r *http.Request, key string) string {
	v, ok, _ := s.store.Setting(r.Context(), key)
	if !ok || v == "" {
		return ""
	}
	b, err := s.vault.Open(v, "setting:"+key)
	if err != nil {
		return ""
	}
	return string(b)
}

func generatePassword() string {
	const alpha = "abcdefghjkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	b := id.Token(18)
	out := make([]byte, 0, 22)
	for i, c := range b {
		if i > 0 && i%6 == 0 {
			out = append(out, '-')
		}
		out = append(out, alpha[int(c)%len(alpha)])
	}
	return string(out)
}

