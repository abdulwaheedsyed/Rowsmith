// Command rowsmith runs the Rowsmith server and its maintenance commands.
package main

import (
	"context"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"rowsmith/internal/api"
	"rowsmith/internal/auth"
	"rowsmith/internal/config"
	_ "rowsmith/internal/driver/all"
	"rowsmith/internal/id"
	"rowsmith/internal/session"
	"rowsmith/internal/store"
	"rowsmith/internal/tunnel"
	"rowsmith/internal/vault"
	"rowsmith/internal/web"
)

func main() {
	cmd := "serve"
	args := os.Args[1:]
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	var err error
	switch cmd {
	case "serve":
		err = serve()
	case "healthcheck":
		err = healthcheck()
	case "create-user":
		err = createUser(args)
	case "reset-password":
		err = resetPassword(args)
	case "rotate-key":
		err = rotateKey()
	case "version":
		fmt.Println("rowsmith", api.Version)
	default:
		err = fmt.Errorf("unknown command %q (serve, healthcheck, create-user, reset-password, rotate-key, version)", cmd)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "rowsmith:", err)
		os.Exit(1)
	}
}

func logger(level string) *slog.Logger {
	var l slog.Level
	_ = l.UnmarshalText([]byte(level))
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: l}))
}

type openState struct {
	cfg   *config.Config
	store *store.Store
	vault *vault.Vault
	log   *slog.Logger
}

func openAll() (*openState, bool, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, false, err
	}
	log := logger(cfg.LogLevel)
	v, generated, err := vault.LoadOrCreate(cfg.MasterKey, cfg.MasterKeyFile, cfg.DataDir)
	if err != nil {
		return nil, false, err
	}
	st, err := store.Open(cfg.DataDir)
	if err != nil {
		return nil, false, err
	}
	return &openState{cfg: cfg, store: st, vault: v, log: log}, generated, nil
}

// hostKeys adapts the store to the tunnel's trust store interface.
type hostKeys struct{ s *store.Store }

func (h hostKeys) HostKeys(ctx context.Context, host string, port int) ([]tunnel.KnownKey, error) {
	list, err := h.s.KnownHostKeys(ctx, host, port)
	if err != nil {
		return nil, err
	}
	out := make([]tunnel.KnownKey, len(list))
	for i, k := range list {
		out[i] = tunnel.KnownKey{KeyType: k.KeyType, PublicKey: k.PublicKey}
	}
	return out, nil
}

func serve() error {
	st, generated, err := openAll()
	if err != nil {
		return err
	}
	defer st.store.Close()
	log := st.log
	if generated {
		log.Warn("generated a new master key; back it up separately from the database",
			"path", filepath.Join(st.cfg.DataDir, "keys", "master.key"))
	}
	tunnels := tunnel.NewManager(hostKeys{st.store}, log)
	sessions := session.New(st.store, st.vault, tunnels, log)
	srv := api.New(api.Deps{Config: st.cfg, Store: st.store, Vault: st.vault, Sessions: sessions, Tunnels: tunnels, Log: log,
		Static: web.Handler(st.cfg.BasePath)})

	if n, err := st.store.CountUsers(context.Background()); err == nil && n == 0 {
		token := strings.ToUpper(base64.RawStdEncoding.EncodeToString(id.Token(9)))
		token = strings.NewReplacer("+", "X", "/", "Y").Replace(token)
		token = token[:4] + "-" + token[4:8] + "-" + token[8:12]
		srv.SetSetupToken(token)
		fmt.Fprintf(os.Stdout, "\n  ┌──────────────────────────────────────────────┐\n  │  Rowsmith first-run setup code: %s │\n  └──────────────────────────────────────────────┘\n\n", token)
		log.Info("waiting for first-run setup", "hint", "open the web UI and enter the setup code printed above")
	}

	go func() {
		for range time.Tick(15 * time.Minute) {
			_ = st.store.PurgeExpiredSessions(context.Background(), time.Now().Add(-st.cfg.SessionIdle).UnixMilli())
		}
	}()

	hs := &http.Server{
		Addr:              st.cfg.Addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}
	errc := make(chan error, 1)
	go func() {
		log.Info("rowsmith listening", "addr", st.cfg.Addr, "basePath", st.cfg.BasePath, "version", api.Version)
		errc <- hs.ListenAndServe()
	}()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case s := <-sig:
		log.Info("shutting down", "signal", s.String())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_ = hs.Shutdown(ctx)
	sessions.Shutdown()
	tunnels.CloseAll()
	return nil
}

func healthcheck() error {
	addr := os.Getenv("ROWSMITH_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	c := http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://" + addr + "/api/health")
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}

func createUser(args []string) error {
	fs := flag.NewFlagSet("create-user", flag.ExitOnError)
	email := fs.String("email", "", "email address")
	name := fs.String("name", "", "display name")
	role := fs.String("role", "member", "owner | admin | member | viewer")
	_ = fs.Parse(args)
	if *email == "" || *name == "" {
		return errors.New("usage: rowsmith create-user -email you@example.com -name 'Your Name' [-role owner]")
	}
	r := store.Role(*role)
	if !r.Valid() {
		return fmt.Errorf("invalid role %q", *role)
	}
	st, _, err := openAll()
	if err != nil {
		return err
	}
	defer st.store.Close()
	pw := strings.NewReplacer("+", "", "/", "").Replace(base64.RawStdEncoding.EncodeToString(id.Token(18)))
	hash, err := auth.HashPassword(pw)
	if err != nil {
		return err
	}
	u := &store.User{ID: id.New(), Email: strings.ToLower(*email), Name: *name, PasswordHash: hash, Role: r, MustChangePassword: true}
	if err := st.store.CreateUser(context.Background(), u); err != nil {
		return err
	}
	_ = st.store.Audit(context.Background(), "", "cli", "user.created", u.Email, map[string]any{"role": r, "via": "cli"})
	fmt.Printf("Created %s (%s). Temporary password: %s\nThe user must change it at first sign-in.\n", u.Email, r, pw)
	return nil
}

func resetPassword(args []string) error {
	fs := flag.NewFlagSet("reset-password", flag.ExitOnError)
	email := fs.String("email", "", "email address")
	_ = fs.Parse(args)
	if *email == "" {
		return errors.New("usage: rowsmith reset-password -email you@example.com")
	}
	st, _, err := openAll()
	if err != nil {
		return err
	}
	defer st.store.Close()
	ctx := context.Background()
	u, err := st.store.UserByEmail(ctx, strings.ToLower(*email))
	if err != nil {
		return fmt.Errorf("no user %s", *email)
	}
	pw := strings.NewReplacer("+", "", "/", "").Replace(base64.RawStdEncoding.EncodeToString(id.Token(18)))
	hash, _ := auth.HashPassword(pw)
	t, f := true, false
	if err := st.store.UpdateUser(ctx, u.ID, store.UserPatch{PasswordHash: &hash, MustChangePassword: &t, Disabled: &f}); err != nil {
		return err
	}
	_ = st.store.DeleteUserSessions(ctx, u.ID, "")
	_ = st.store.RecordLoginSuccess(ctx, u.ID) // clears lockout
	_ = st.store.Audit(ctx, "", "cli", "user.password_reset", u.Email, map[string]any{"via": "cli"})
	fmt.Printf("Temporary password for %s: %s\n", u.Email, pw)
	return nil
}

// rotateKey prepends a new master key and re-wraps every sealed value.
func rotateKey() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if cfg.MasterKey != "" {
		return errors.New("the master key comes from ROWSMITH_MASTER_KEY; add the new key there manually (first line = current)")
	}
	file := cfg.MasterKeyFile
	if file == "" {
		file = filepath.Join(cfg.DataDir, "keys", "master.key")
	}
	old, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	oldKeys, err := vault.ParseKeys(string(old))
	if err != nil {
		return err
	}
	newKey := vault.GenerateKey()
	v, err := vault.New(append([][]byte{newKey}, oldKeys...)...)
	if err != nil {
		return err
	}
	st, err := store.Open(cfg.DataDir)
	if err != nil {
		return err
	}
	defer st.Close()
	ctx := context.Background()
	n, err := rewrapAll(ctx, st, v)
	if err != nil {
		return err
	}
	body := "# Rowsmith master keys: first line is current, older lines decrypt legacy data.\n" + vault.EncodeKey(newKey) + "\n"
	for _, k := range oldKeys {
		body += vault.EncodeKey(k) + "\n"
	}
	if err := os.WriteFile(file+".new", []byte(body), 0o600); err != nil {
		return err
	}
	if err := os.Rename(file+".new", file); err != nil {
		return err
	}
	fmt.Printf("Rotated master key (new id %s); re-wrapped %d secrets. Older keys remain in the key file for recovery.\n", v.KeyID(), n)
	return nil
}

func rewrapAll(ctx context.Context, st *store.Store, v *vault.Vault) (int, error) {
	n := 0
	conns, err := st.AllConnections(ctx)
	if err != nil {
		return 0, err
	}
	for _, c := range conns {
		if c.Secrets == "" {
			continue
		}
		re, err := v.Rewrap(c.Secrets, c.SecretsAAD())
		if err != nil {
			return n, fmt.Errorf("connection %s: %w", c.ID, err)
		}
		if _, err := st.DB().ExecContext(ctx, `UPDATE connections SET secrets = ? WHERE id = ?`, re, c.ID); err != nil {
			return n, err
		}
		n++
	}
	users, err := st.ListUsers(ctx)
	if err != nil {
		return n, err
	}
	for _, u := range users {
		if u.MFASecret == "" {
			continue
		}
		re, err := v.Rewrap(u.MFASecret, "user:"+u.ID+":totp")
		if err != nil {
			return n, fmt.Errorf("user %s: %w", u.Email, err)
		}
		if _, err := st.DB().ExecContext(ctx, `UPDATE users SET mfa_secret = ? WHERE id = ?`, re, u.ID); err != nil {
			return n, err
		}
		n++
	}
	secrets, err := st.SecretSettings(ctx)
	if err != nil {
		return n, err
	}
	for k, val := range secrets {
		aad := "setting:" + k
		if strings.HasPrefix(k, "mfa_pending:") {
			aad = "user:" + strings.TrimPrefix(k, "mfa_pending:") + ":totp:pending"
		}
		re, err := v.Rewrap(val, aad)
		if err != nil {
			continue // stale pending enrollments may be unreadable; skip
		}
		if err := st.SetSetting(ctx, k, re, true); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
