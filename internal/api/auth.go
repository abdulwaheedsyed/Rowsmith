package api

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"rowsmith/internal/auth"
	"rowsmith/internal/id"
	"rowsmith/internal/store"
)

const (
	lockThreshold = 8
	lockDuration  = 15 * time.Minute
)

func (s *Server) bootstrap(w http.ResponseWriter, r *http.Request) {
	n, err := s.store.CountUsers(r.Context())
	if err != nil {
		writeErr(w, 500, "store unavailable")
		return
	}
	requireMFA, _, _ := s.store.Setting(r.Context(), "security.require_mfa")
	writeJSON(w, 200, map[string]any{
		"product":       "Rowsmith",
		"version":       Version,
		"setupRequired": n == 0,
		"requireMfa":    requireMFA == "true",
		"basePath":      s.cfg.BasePath,
	})
}

type setupReq struct {
	Token    string `json:"token"`
	Name     string `json:"name"`
	Email    string `json:"email"`
	Password string `json:"password"`
}

func (s *Server) setup(w http.ResponseWriter, r *http.Request) {
	if !s.sameOrigin(r) {
		writeErr(w, 403, "cross-origin request blocked")
		return
	}
	if ok, wait := s.loginIP.Allow("setup:" + s.clientIP(r)); !ok {
		w.Header().Set("Retry-After", itoa(int(wait.Seconds())+1))
		writeErr(w, 429, "too many attempts; wait a moment")
		return
	}
	var req setupReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	s.setupMu.Lock()
	defer s.setupMu.Unlock()
	n, err := s.store.CountUsers(r.Context())
	if err != nil || n > 0 {
		writeErr(w, 409, "setup has already been completed")
		return
	}
	if s.setupToken == "" || subtle.ConstantTimeCompare([]byte(strings.TrimSpace(req.Token)), []byte(s.setupToken)) != 1 {
		writeErr(w, 403, "invalid setup code — find it in the Rowsmith container log")
		return
	}
	req.Email = strings.TrimSpace(strings.ToLower(req.Email))
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || !validEmail(req.Email) {
		writeErr(w, 400, "enter your name and a valid email")
		return
	}
	if err := auth.CheckPasswordPolicy(req.Password, req.Email); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		writeErr(w, 500, "could not hash password")
		return
	}
	u := &store.User{ID: id.New(), Email: req.Email, Name: req.Name, PasswordHash: hash, Role: store.RoleOwner}
	if err := s.store.CreateUser(r.Context(), u); err != nil {
		writeErr(w, 500, "could not create user")
		return
	}
	s.setupToken = ""
	s.audit(r.Context(), &reqCtx{user: u, ip: s.clientIP(r)}, "setup.owner_created", u.Email, nil)
	if _, err := s.issueSession(w, r, u, store.StageFull); err != nil {
		writeErr(w, 500, "could not start session")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

type loginReq struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	ip := s.clientIP(r)
	if !s.sameOrigin(r) {
		writeErr(w, 403, "cross-origin request blocked")
		return
	}
	if ok, wait := s.loginIP.Allow("ip:" + ip); !ok {
		w.Header().Set("Retry-After", itoa(int(wait.Seconds())+1))
		writeErr(w, 429, "too many sign-in attempts; wait a moment and try again")
		return
	}
	var req loginReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	email := strings.TrimSpace(strings.ToLower(req.Email))
	if ok, _ := s.loginAcct.Allow("acct:" + email); !ok {
		writeErr(w, 429, "too many sign-in attempts for this account; wait a moment")
		return
	}
	const generic = "incorrect email or password"
	u, err := s.store.UserByEmail(r.Context(), email)
	if err != nil {
		auth.BurnPasswordCheck(req.Password)
		s.audit(r.Context(), &reqCtx{ip: ip}, "auth.login_failed", email, map[string]string{"reason": "unknown_user"})
		writeErr(w, 401, generic)
		return
	}
	now := time.Now().UnixMilli()
	if u.LockedUntil > now {
		auth.BurnPasswordCheck(req.Password)
		writeErr(w, 423, "account temporarily locked after repeated failures; try again later")
		return
	}
	ok, rehash := auth.VerifyPassword(u.PasswordHash, req.Password)
	if !ok || u.Disabled {
		if !ok {
			_ = s.store.RecordLoginFailure(r.Context(), u.ID, lockThreshold, lockDuration.Milliseconds())
		}
		s.audit(r.Context(), &reqCtx{user: u, ip: ip}, "auth.login_failed", email, map[string]string{"reason": map[bool]string{true: "disabled", false: "bad_password"}[u.Disabled]})
		writeErr(w, 401, generic)
		return
	}
	if rehash {
		if h, err := auth.HashPassword(req.Password); err == nil {
			_ = s.store.UpdateUser(r.Context(), u.ID, store.UserPatch{PasswordHash: &h})
		}
	}
	stage := store.StageFull
	if u.MFAEnabled {
		stage = store.StageMFA
	} else if req, _, _ := s.store.Setting(r.Context(), "security.require_mfa"); req == "true" {
		stage = store.StageEnroll
	}
	if stage != store.StageMFA {
		_ = s.store.RecordLoginSuccess(r.Context(), u.ID)
		s.audit(r.Context(), &reqCtx{user: u, ip: ip}, "auth.login", email, nil)
	}
	if _, err := s.issueSession(w, r, u, stage); err != nil {
		writeErr(w, 500, "could not start session")
		return
	}
	writeJSON(w, 200, map[string]any{"stage": stage})
}

type mfaReq struct {
	Code string `json:"code"`
}

func (s *Server) mfaVerify(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	if ok, _ := s.mfaLimit.Allow("mfa:" + rc.user.ID); !ok {
		writeErr(w, 429, "too many attempts; wait a moment")
		return
	}
	var req mfaReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	code := strings.TrimSpace(req.Code)
	valid := false
	method := "totp"
	if len(strings.ReplaceAll(code, " ", "")) == 6 {
		var secret []byte
		if rc.user.MFASecret != "" {
			secret, _ = s.vault.Open(rc.user.MFASecret, mfaAAD(rc.user.ID))
		}
		if step, ok := auth.VerifyTOTP(string(secret), code, time.Now()); ok {
			valid, _ = s.store.ConsumeTOTPStep(r.Context(), rc.user.ID, step)
		}
		clear(secret)
	} else {
		method = "recovery"
		valid, _ = s.store.UseRecoveryCode(r.Context(), rc.user.ID, auth.HashRecoveryCode(code))
	}
	if !valid {
		_ = s.store.RecordLoginFailure(r.Context(), rc.user.ID, lockThreshold, lockDuration.Milliseconds())
		s.audit(r.Context(), rc, "auth.mfa_failed", rc.user.Email, nil)
		writeErr(w, 401, "that code is not valid")
		return
	}
	// Rotate the session: the pre-MFA token is discarded.
	_ = s.store.DeleteSession(r.Context(), rc.sess.IDHash)
	if _, err := s.issueSession(w, r, rc.user, store.StageFull); err != nil {
		writeErr(w, 500, "could not start session")
		return
	}
	_ = s.store.RecordLoginSuccess(r.Context(), rc.user.ID)
	s.audit(r.Context(), rc, "auth.login", rc.user.Email, map[string]string{"mfa": method})
	left := -1
	if method == "recovery" {
		left, _ = s.store.RemainingRecoveryCodes(r.Context(), rc.user.ID)
	}
	writeJSON(w, 200, map[string]any{"stage": store.StageFull, "recoveryCodesLeft": left})
}

func mfaAAD(userID string) string { return "user:" + userID + ":totp" }

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if ck, err := r.Cookie(s.cookieName); err == nil && ck.Value != "" {
		_ = s.store.DeleteSession(r.Context(), auth.HashToken(ck.Value))
	}
	s.clearCookie(w)
	writeJSON(w, 200, map[string]any{"ok": true})
}

type meResp struct {
	User      *store.User        `json:"user"`
	Stage     store.SessionStage `json:"stage"`
	CSRF      string             `json:"csrf"`
	RecoveryLeft int             `json:"recoveryCodesLeft"`
}

func (s *Server) me(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	left, _ := s.store.RemainingRecoveryCodes(r.Context(), rc.user.ID)
	writeJSON(w, 200, meResp{User: rc.user, Stage: rc.sess.Stage, CSRF: rc.sess.CSRF, RecoveryLeft: left})
}

// mfaSetup generates a new pending secret; it becomes active on enable.
func (s *Server) mfaSetup(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	secret := auth.NewTOTPSecret()
	sealed, err := s.vault.Seal([]byte(secret), mfaAAD(rc.user.ID)+":pending")
	if err != nil {
		writeErr(w, 500, "could not create secret")
		return
	}
	if err := s.store.SetSetting(r.Context(), "mfa_pending:"+rc.user.ID, sealed, true); err != nil {
		writeErr(w, 500, "could not store secret")
		return
	}
	writeJSON(w, 200, map[string]any{"secret": secret, "uri": auth.TOTPURI(secret, "Rowsmith", rc.user.Email)})
}

func (s *Server) mfaEnable(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	var req mfaReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	sealed, ok, _ := s.store.Setting(r.Context(), "mfa_pending:"+rc.user.ID)
	if !ok {
		writeErr(w, 400, "start MFA setup first")
		return
	}
	secret, err := s.vault.Open(sealed, mfaAAD(rc.user.ID)+":pending")
	if err != nil {
		writeErr(w, 400, "setup expired; start again")
		return
	}
	defer clear(secret)
	if _, ok := auth.VerifyTOTP(string(secret), req.Code, time.Now()); !ok {
		writeErr(w, 400, "that code does not match; check your device clock and try again")
		return
	}
	active, err := s.vault.Seal(secret, mfaAAD(rc.user.ID))
	if err != nil {
		writeErr(w, 500, "could not store secret")
		return
	}
	if err := s.store.SetMFA(r.Context(), rc.user.ID, active, true); err != nil {
		writeErr(w, 500, "could not enable MFA")
		return
	}
	_ = s.store.DeleteSetting(r.Context(), "mfa_pending:"+rc.user.ID)
	codes := auth.NewRecoveryCodes(10)
	hashes := make([]string, len(codes))
	for i, c := range codes {
		hashes[i] = auth.HashRecoveryCode(c)
	}
	_ = s.store.ReplaceRecoveryCodes(r.Context(), rc.user.ID, hashes)
	// Upgrade an enrollment-only session and revoke all other sessions.
	_ = s.store.DeleteUserSessions(r.Context(), rc.user.ID, "")
	if _, err := s.issueSession(w, r, rc.user, store.StageFull); err != nil {
		writeErr(w, 500, "could not start session")
		return
	}
	s.audit(r.Context(), rc, "auth.mfa_enabled", rc.user.Email, nil)
	writeJSON(w, 200, map[string]any{"recoveryCodes": codes})
}

type passwordReq struct {
	Current string `json:"current"`
	Next    string `json:"next"`
}

func (s *Server) mfaDisable(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	var req passwordReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if ok, _ := auth.VerifyPassword(rc.user.PasswordHash, req.Current); !ok {
		writeErr(w, 403, "current password is incorrect")
		return
	}
	if v, _, _ := s.store.Setting(r.Context(), "security.require_mfa"); v == "true" {
		writeErr(w, 403, "your administrator requires two-factor authentication")
		return
	}
	_ = s.store.SetMFA(r.Context(), rc.user.ID, "", false)
	_ = s.store.ReplaceRecoveryCodes(r.Context(), rc.user.ID, nil)
	s.audit(r.Context(), rc, "auth.mfa_disabled", rc.user.Email, nil)
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) regenRecoveryCodes(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	var req passwordReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if ok, _ := auth.VerifyPassword(rc.user.PasswordHash, req.Current); !ok {
		writeErr(w, 403, "current password is incorrect")
		return
	}
	if !rc.user.MFAEnabled {
		writeErr(w, 400, "enable two-factor authentication first")
		return
	}
	codes := auth.NewRecoveryCodes(10)
	hashes := make([]string, len(codes))
	for i, c := range codes {
		hashes[i] = auth.HashRecoveryCode(c)
	}
	_ = s.store.ReplaceRecoveryCodes(r.Context(), rc.user.ID, hashes)
	s.audit(r.Context(), rc, "auth.recovery_codes_regenerated", rc.user.Email, nil)
	writeJSON(w, 200, map[string]any{"recoveryCodes": codes})
}

func (s *Server) changePassword(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	var req passwordReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if ok, _ := auth.VerifyPassword(rc.user.PasswordHash, req.Current); !ok {
		writeErr(w, 403, "current password is incorrect")
		return
	}
	if err := auth.CheckPasswordPolicy(req.Next, rc.user.Email); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	h, err := auth.HashPassword(req.Next)
	if err != nil {
		writeErr(w, 500, "could not hash password")
		return
	}
	f := false
	if err := s.store.UpdateUser(r.Context(), rc.user.ID, store.UserPatch{PasswordHash: &h, MustChangePassword: &f}); err != nil {
		writeErr(w, 500, "could not update password")
		return
	}
	_ = s.store.DeleteUserSessions(r.Context(), rc.user.ID, rc.sess.IDHash)
	s.audit(r.Context(), rc, "auth.password_changed", rc.user.Email, nil)
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) mySessions(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	list, err := s.store.ListUserSessions(r.Context(), rc.user.ID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, x := range list {
		out = append(out, map[string]any{"id": x.IDHash[:16], "createdAt": x.CreatedAt, "lastSeenAt": x.LastSeenAt,
			"ip": x.IP, "userAgent": x.UserAgent, "current": x.IDHash == rc.sess.IDHash})
	}
	writeJSON(w, 200, out)
}

func (s *Server) revokeSession(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	prefix := r.PathValue("id")
	list, _ := s.store.ListUserSessions(r.Context(), rc.user.ID)
	for _, x := range list {
		if len(prefix) == 16 && strings.HasPrefix(x.IDHash, prefix) {
			_ = s.store.DeleteSession(r.Context(), x.IDHash)
			s.audit(r.Context(), rc, "auth.session_revoked", rc.user.Email, nil)
			writeJSON(w, 200, map[string]any{"ok": true})
			return
		}
	}
	writeErr(w, 404, "session not found")
}

func (s *Server) updatePrefs(w http.ResponseWriter, r *http.Request, rc *reqCtx) {
	var raw json.RawMessage
	if err := readJSON(r, &raw); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if len(raw) > 64<<10 {
		writeErr(w, 400, "preferences too large")
		return
	}
	if err := s.store.UpdateUser(r.Context(), rc.user.ID, store.UserPatch{Prefs: &raw}); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func validEmail(e string) bool {
	at := strings.LastIndexByte(e, '@')
	return at > 0 && at < len(e)-3 && strings.Contains(e[at:], ".") && !strings.ContainsAny(e, " \t\r\n<>") && len(e) <= 254
}

func itoa(n int) string {
	if n <= 0 {
		return "1"
	}
	b := []byte{}
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
