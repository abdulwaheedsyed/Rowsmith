// Package api is Rowsmith's HTTP interface: a JSON API under /api plus the
// embedded single-page app.
package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"rowsmith/internal/auth"
	"rowsmith/internal/config"
	"rowsmith/internal/driver"
	"rowsmith/internal/session"
	"rowsmith/internal/store"
	"rowsmith/internal/tunnel"
	"rowsmith/internal/vault"
)

const Version = "0.1.0"

type Server struct {
	cfg      *config.Config
	store    *store.Store
	vault    *vault.Vault
	sessions *session.Manager
	tunnels  *tunnel.Manager
	log      *slog.Logger
	static   http.Handler

	loginIP   *auth.Limiter
	loginAcct *auth.Limiter
	mfaLimit  *auth.Limiter

	setupMu    sync.Mutex
	setupToken string

	cookieName string
}

type Deps struct {
	Config   *config.Config
	Store    *store.Store
	Vault    *vault.Vault
	Sessions *session.Manager
	Tunnels  *tunnel.Manager
	Log      *slog.Logger
	Static   http.Handler
}

func New(d Deps) *Server {
	s := &Server{cfg: d.Config, store: d.Store, vault: d.Vault, sessions: d.Sessions, tunnels: d.Tunnels, log: d.Log, static: d.Static,
		loginIP: auth.NewLimiter(20, 10), loginAcct: auth.NewLimiter(10, 5), mfaLimit: auth.NewLimiter(10, 5)}
	s.cookieName = "rowsmith_session"
	if d.Config.BasePath == "/" && !d.Config.Insecure {
		// __Host- cookies must be Secure, host-only and Path=/, which blocks
		// subdomains and other paths from planting or reading them.
		s.cookieName = "__Host-rowsmith_session"
	}
	return s
}

// SetSetupToken arms the one-time setup flow used to create the first owner.
func (s *Server) SetSetupToken(t string) {
	s.setupMu.Lock()
	s.setupToken = t
	s.setupMu.Unlock()
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	s.routes(mux)
	var h http.Handler = mux
	h = s.securityHeaders(h)
	h = s.recoverer(h)
	if bp := strings.TrimSuffix(s.cfg.BasePath, "/"); bp != "" {
		inner := h
		h = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Accept both stripped (Caddy handle_path) and unstripped paths.
			if strings.HasPrefix(r.URL.Path, bp+"/") || r.URL.Path == bp {
				http.StripPrefix(bp, inner).ServeHTTP(w, r)
				return
			}
			inner.ServeHTTP(w, r)
		})
	}
	return h
}

func (s *Server) routes(mux *http.ServeMux) {
	// Public.
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]any{"ok": true}) })
	mux.HandleFunc("GET /api/bootstrap", s.bootstrap)
	mux.HandleFunc("POST /api/setup", s.setup)
	mux.HandleFunc("POST /api/auth/login", s.login)
	mux.HandleFunc("POST /api/auth/logout", s.logout)

	// Partially authenticated (MFA pending / enrollment required).
	mux.Handle("POST /api/auth/mfa", s.authed(stageMFA, s.mfaVerify))
	mux.Handle("GET /api/auth/me", s.authed(stageAny, s.me))
	mux.Handle("POST /api/me/mfa/setup", s.authed(stageEnrollOrFull, s.mfaSetup))
	mux.Handle("POST /api/me/mfa/enable", s.authed(stageEnrollOrFull, s.mfaEnable))

	// Fully authenticated.
	full := func(pattern string, h handlerFunc) { mux.Handle(pattern, s.authed(stageFull, h)) }
	full("POST /api/me/password", s.changePassword)
	full("POST /api/me/mfa/disable", s.mfaDisable)
	full("POST /api/me/recovery-codes", s.regenRecoveryCodes)
	full("GET /api/me/sessions", s.mySessions)
	full("DELETE /api/me/sessions/{id}", s.revokeSession)
	full("PATCH /api/me/prefs", s.updatePrefs)

	full("GET /api/users", s.listUsers)
	full("POST /api/users", s.createUser)
	full("PATCH /api/users/{id}", s.updateUser)
	full("DELETE /api/users/{id}", s.deleteUser)
	full("POST /api/users/{id}/reset-mfa", s.resetUserMFA)
	full("GET /api/audit", s.listAudit)
	full("GET /api/admin/overview", s.adminOverview)
	full("GET /api/settings", s.getSettings)
	full("PUT /api/settings", s.putSettings)

	full("GET /api/drivers", s.listDrivers)
	full("GET /api/connections", s.listConnections)
	full("POST /api/connections", s.createConnection)
	full("POST /api/connections/test", s.testConnection)
	full("GET /api/connections/{id}", s.getConnection)
	full("PATCH /api/connections/{id}", s.updateConnection)
	full("DELETE /api/connections/{id}", s.deleteConnection)
	full("POST /api/connections/{id}/test", s.testSavedConnection)
	full("GET /api/connections/{id}/shares", s.getShares)
	full("PUT /api/connections/{id}/shares", s.putShares)
	full("POST /api/connections/{id}/duplicate", s.duplicateConnection)

	full("POST /api/ssh/trust", s.trustHostKey)
	full("GET /api/ssh/known-hosts", s.listKnownHosts)
	full("DELETE /api/ssh/known-hosts", s.forgetHostKey)

	full("GET /api/c/{id}/server", s.wsServer)
	full("GET /api/c/{id}/databases", s.wsDatabases)
	full("GET /api/c/{id}/schemas", s.wsSchemas)
	full("GET /api/c/{id}/objects", s.wsObjects)
	full("GET /api/c/{id}/describe", s.wsDescribe)
	full("GET /api/c/{id}/definition", s.wsDefinition)
	full("GET /api/c/{id}/catalog", s.wsCatalog)
	full("POST /api/c/{id}/browse", s.wsBrowse)
	full("POST /api/c/{id}/count", s.wsCount)
	full("POST /api/c/{id}/edit", s.wsEdit)
	full("POST /api/c/{id}/query", s.wsQuery)
	full("POST /api/c/{id}/query/cancel", s.wsCancel)
	full("POST /api/c/{id}/console/close", s.wsCloseConsole)
	full("POST /api/c/{id}/analyze", s.wsAnalyze)
	full("POST /api/c/{id}/explain", s.wsExplain)
	full("GET /api/c/{id}/processes", s.wsProcesses)
	full("POST /api/c/{id}/processes/kill", s.wsKill)
	full("GET /api/c/{id}/variables", s.wsVariables)
	full("GET /api/c/{id}/db-users", s.wsDBUsers)
	full("GET /api/c/{id}/db-users/grants", s.wsDBUserGrants)
	full("POST /api/c/{id}/ddl", s.wsDDL)

	full("GET /api/history", s.listHistory)
	full("GET /api/saved-queries", s.listSaved)
	full("POST /api/saved-queries", s.createSaved)
	full("PUT /api/saved-queries/{id}", s.updateSaved)
	full("DELETE /api/saved-queries/{id}", s.deleteSaved)
	full("GET /api/connections/{id}/notes", s.listNotes)
	full("POST /api/connections/{id}/notes", s.addNote)
	full("DELETE /api/notes/{id}", s.deleteNote)

	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) { writeErr(w, 404, "not found") })
	if s.static != nil {
		mux.Handle("/", s.static)
	}
}

// ---- middleware ---------------------------------------------------------------

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	csp := strings.Join([]string{
		"default-src 'self'",
		"script-src 'self' 'wasm-unsafe-eval'",
		"style-src 'self' 'unsafe-inline'",
		"img-src 'self' data: blob: https:",
		"font-src 'self' data:",
		"connect-src 'self' https:",
		"worker-src 'self' blob:",
		"frame-ancestors 'none'",
		"base-uri 'self'",
		"form-action 'self'",
		"object-src 'none'",
	}, "; ")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=(), usb=()")
		if !s.cfg.Insecure {
			h.Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
		}
		if strings.HasPrefix(r.URL.Path, "/api/") || strings.Contains(r.URL.Path, "/api/") {
			h.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				s.log.Error("panic", "path", r.URL.Path, "err", v, "stack", string(debug.Stack()))
				writeErr(w, 500, "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// clientIP returns the caller address, trusting X-Forwarded-For only when the
// immediate peer is a configured proxy.
func (s *Server) clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil || !s.trusted(ip) {
		return host
	}
	xff := r.Header.Get("X-Forwarded-For")
	if xff == "" {
		return host
	}
	parts := strings.Split(xff, ",")
	for i := len(parts) - 1; i >= 0; i-- {
		p := strings.TrimSpace(parts[i])
		pip := net.ParseIP(p)
		if pip == nil {
			continue
		}
		if !s.trusted(pip) || i == 0 {
			return p
		}
	}
	return host
}

func (s *Server) trusted(ip net.IP) bool {
	for _, n := range s.cfg.TrustedProxies {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// sameOrigin rejects cross-site state-changing requests.
func (s *Server) sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		// Browsers always send Origin on cross-origin POSTs; fetch() from our
		// own page may omit it on same-origin GET-like requests. Fall back to
		// Sec-Fetch-Site when present.
		site := r.Header.Get("Sec-Fetch-Site")
		return site == "" || site == "same-origin" || site == "none"
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	if s.cfg.PublicURL != nil {
		return strings.EqualFold(u.Scheme, s.cfg.PublicURL.Scheme) && strings.EqualFold(u.Host, s.cfg.PublicURL.Host)
	}
	host := r.Host
	if fh := r.Header.Get("X-Forwarded-Host"); fh != "" && s.trustedPeer(r) {
		host = fh
	}
	return strings.EqualFold(u.Host, host)
}

func (s *Server) trustedPeer(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && s.trusted(ip)
}

// ---- auth plumbing ------------------------------------------------------------

type stageReq int

const (
	stageAny stageReq = iota
	stageMFA
	stageEnrollOrFull
	stageFull
)

type reqCtx struct {
	user *store.User
	sess *store.Session
	ip   string
}

type handlerFunc func(w http.ResponseWriter, r *http.Request, rc *reqCtx)

func (s *Server) authed(need stageReq, h handlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := s.clientIP(r)
		ck, err := r.Cookie(s.cookieName)
		if err != nil || ck.Value == "" {
			writeErr(w, 401, "sign in required")
			return
		}
		hash := auth.HashToken(ck.Value)
		sess, err := s.store.SessionByHash(r.Context(), hash)
		if err != nil {
			s.clearCookie(w)
			writeErr(w, 401, "session expired")
			return
		}
		now := time.Now().UnixMilli()
		if sess.ExpiresAt < now || now-sess.LastSeenAt > s.cfg.SessionIdle.Milliseconds() {
			_ = s.store.DeleteSession(r.Context(), hash)
			s.clearCookie(w)
			writeErr(w, 401, "session expired")
			return
		}
		user, err := s.store.UserByID(r.Context(), sess.UserID)
		if err != nil || user.Disabled {
			_ = s.store.DeleteSession(r.Context(), hash)
			s.clearCookie(w)
			writeErr(w, 401, "account unavailable")
			return
		}
		switch need {
		case stageMFA:
			if sess.Stage != store.StageMFA {
				writeErr(w, 409, "no second factor pending")
				return
			}
		case stageEnrollOrFull:
			if sess.Stage != store.StageEnroll && sess.Stage != store.StageFull {
				writeErr(w, 403, "complete sign-in first")
				return
			}
		case stageFull:
			if sess.Stage != store.StageFull {
				writeErr(w, 403, "complete sign-in first")
				return
			}
		}
		// A temporary password only lets the user choose a new one.
		if need == stageFull && user.MustChangePassword && r.URL.Path != "/api/me/password" {
			writeErrDetail(w, 403, "password_change_required", "choose a new password to continue", nil)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if !s.sameOrigin(r) {
				writeErr(w, 403, "cross-origin request blocked")
				return
			}
			tok := r.Header.Get("X-CSRF-Token")
			if subtle.ConstantTimeCompare([]byte(tok), []byte(sess.CSRF)) != 1 {
				writeErr(w, 403, "invalid CSRF token; reload the page")
				return
			}
		}
		if now-sess.LastSeenAt > 60_000 {
			_ = s.store.TouchSession(r.Context(), hash, now)
		}
		h(w, r, &reqCtx{user: user, sess: sess, ip: ip})
	})
}

func (s *Server) issueSession(w http.ResponseWriter, r *http.Request, u *store.User, stage store.SessionStage) (*store.Session, error) {
	token, hash := auth.NewSessionToken()
	now := time.Now()
	sess := &store.Session{IDHash: hash, UserID: u.ID, CSRF: auth.NewCSRFToken(), Stage: stage,
		CreatedAt: now.UnixMilli(), LastSeenAt: now.UnixMilli(), ExpiresAt: now.Add(s.cfg.SessionAbsolute).UnixMilli(),
		IP: s.clientIP(r), UserAgent: truncate(r.UserAgent(), 300)}
	if stage == store.StageMFA {
		sess.ExpiresAt = now.Add(10 * time.Minute).UnixMilli()
	}
	if err := s.store.CreateSession(r.Context(), sess); err != nil {
		return nil, err
	}
	http.SetCookie(w, &http.Cookie{Name: s.cookieName, Value: token, Path: s.cookiePath(), HttpOnly: true,
		Secure: !s.cfg.Insecure, SameSite: http.SameSiteStrictMode, MaxAge: int(s.cfg.SessionAbsolute.Seconds())})
	return sess, nil
}

func (s *Server) cookiePath() string {
	if s.cookieName == "__Host-rowsmith_session" {
		return "/"
	}
	return s.cfg.BasePath
}

func (s *Server) clearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: s.cookieName, Value: "", Path: s.cookiePath(), HttpOnly: true,
		Secure: !s.cfg.Insecure, SameSite: http.SameSiteStrictMode, MaxAge: -1})
}

func (s *Server) audit(ctx context.Context, rc *reqCtx, action, target string, detail any) {
	uid, ip := "", ""
	if rc != nil {
		if rc.user != nil {
			uid = rc.user.ID
		}
		ip = rc.ip
	}
	if err := s.store.Audit(context.WithoutCancel(ctx), uid, ip, action, target, detail); err != nil {
		s.log.Error("audit write failed", "err", err, "action", action)
	}
}

// ---- JSON helpers -------------------------------------------------------------

type apiError struct {
	Message string `json:"message"`
	Code    string `json:"code,omitempty"`
	Detail  any    `json:"detail,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": apiError{Message: msg}})
}

func writeErrDetail(w http.ResponseWriter, status int, code, msg string, detail any) {
	writeJSON(w, status, map[string]any{"error": apiError{Message: msg, Code: code, Detail: detail}})
}

// writeDBErr reports an error from a database/driver operation.
func writeDBErr(w http.ResponseWriter, err error) {
	var qe *driver.QueryError
	var uk *tunnel.UnknownHostKeyError
	var mm *tunnel.HostKeyMismatchError
	switch {
	case errors.As(err, &uk):
		writeErrDetail(w, 409, "ssh_unknown_host", uk.Error(), uk)
	case errors.As(err, &mm):
		writeErrDetail(w, 409, "ssh_host_changed", mm.Error(), mm)
	case errors.As(err, &qe):
		writeErrDetail(w, 400, "db_error", qe.Message, qe)
	case errors.Is(err, driver.ErrNotSupported):
		writeErr(w, 501, err.Error())
	case errors.Is(err, driver.ErrReadOnly):
		writeErr(w, 403, err.Error())
	case errors.Is(err, context.DeadlineExceeded):
		writeErr(w, 504, "the database did not respond in time")
	case errors.Is(err, context.Canceled):
		writeErr(w, 499, "request cancelled")
	default:
		writeErrDetail(w, 400, "db_error", err.Error(), nil)
	}
}

const maxBody = 16 << 20

func readJSON(r *http.Request, v any) error {
	ct := r.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "application/json") {
		return errors.New("expected application/json")
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, maxBody))
	dec.UseNumber()
	if err := dec.Decode(v); err != nil {
		return errors.New("invalid JSON body: " + err.Error())
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
