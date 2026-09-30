// Package session turns saved connections into live, pooled driver
// connections and console sessions. It decrypts secrets on demand, routes
// through SSH tunnels, separates read-only and read-write pools, and closes
// idle resources.
package session

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"rowsmith/internal/driver"
	"rowsmith/internal/id"
	"rowsmith/internal/store"
	"rowsmith/internal/tunnel"
	"rowsmith/internal/vault"
)

// SSHSettings is the non-secret part of a connection's tunnel config.
type SSHSettings struct {
	Enabled bool     `json:"enabled"`
	Hops    []SSHHop `json:"hops"`
}

type SSHHop struct {
	Host string `json:"host"`
	Port int    `json:"port"`
	User string `json:"user"`
	Auth string `json:"auth"` // "password" | "key"
}

// Secret keys for SSH hops inside a connection's sealed secrets map.
func SSHSecretKey(hop int, field string) string { return fmt.Sprintf("ssh.%d.%s", hop, field) }

type Manager struct {
	store   *store.Store
	vault   *vault.Vault
	tunnels *tunnel.Manager
	log     *slog.Logger
	appName string

	mu       sync.Mutex
	live     map[string]*live // key: connectionID|ro
	consoles map[string]*Console

	idleConn    time.Duration
	idleConsole time.Duration
}

type live struct {
	key      string
	connID   string
	version  int64 // connection.UpdatedAt when opened
	conn     driver.Conn
	drv      driver.Driver
	tunnel   *tunnel.Handle
	refs     int
	lastUsed time.Time
	opening  chan struct{}
	err      error
}

func New(st *store.Store, v *vault.Vault, t *tunnel.Manager, log *slog.Logger) *Manager {
	m := &Manager{store: st, vault: v, tunnels: t, log: log, appName: "Rowsmith",
		live: map[string]*live{}, consoles: map[string]*Console{},
		idleConn: 15 * time.Minute, idleConsole: 30 * time.Minute}
	go m.reap()
	return m
}

// Lease is a borrowed live connection. Always call Release.
type Lease struct {
	Conn   driver.Conn
	Driver driver.Driver
	m      *Manager
	l      *live
	once   sync.Once
}

func (l *Lease) Release() {
	l.once.Do(func() {
		l.m.mu.Lock()
		l.l.refs--
		l.l.lastUsed = time.Now()
		l.m.mu.Unlock()
	})
}

// Acquire returns a live connection for c, opening one if necessary.
// readOnly selects an engine-level read-only pool.
func (m *Manager) Acquire(ctx context.Context, c *store.Connection, readOnly bool) (*Lease, error) {
	key := c.ID + "|rw"
	if readOnly {
		key = c.ID + "|ro"
	}
	for {
		m.mu.Lock()
		l, ok := m.live[key]
		if ok && l.version != c.UpdatedAt {
			// Settings changed since this pool was opened: retire it.
			delete(m.live, key)
			go m.closeWhenUnused(l)
			ok = false
		}
		if ok {
			if l.opening != nil {
				ch := l.opening
				m.mu.Unlock()
				select {
				case <-ch:
					continue
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			if l.err == nil {
				l.refs++
				l.lastUsed = time.Now()
				m.mu.Unlock()
				return &Lease{Conn: l.conn, Driver: l.drv, m: m, l: l}, nil
			}
			delete(m.live, key)
		}
		l = &live{key: key, connID: c.ID, version: c.UpdatedAt, opening: make(chan struct{}), refs: 1}
		m.live[key] = l
		m.mu.Unlock()

		conn, drv, th, err := m.open(ctx, c, readOnly)
		m.mu.Lock()
		close(l.opening)
		l.opening = nil
		if err != nil {
			delete(m.live, key)
			m.mu.Unlock()
			return nil, err
		}
		l.conn, l.drv, l.tunnel, l.lastUsed = conn, drv, th, time.Now()
		m.mu.Unlock()
		go m.store.TouchConnection(context.Background(), c.ID)
		return &Lease{Conn: conn, Driver: drv, m: m, l: l}, nil
	}
}

// Secrets decrypts a connection's secrets.
func (m *Manager) Secrets(c *store.Connection) (map[string]string, error) {
	out := map[string]string{}
	if c.Secrets == "" {
		return out, nil
	}
	if err := m.vault.OpenJSON(c.Secrets, c.SecretsAAD(), &out); err != nil {
		return nil, fmt.Errorf("cannot decrypt saved secrets: %w", err)
	}
	return out, nil
}

// Test opens a throwaway connection (used by the connection form).
func (m *Manager) Test(ctx context.Context, c *store.Connection, secrets map[string]string) (*driver.ServerInfo, time.Duration, error) {
	start := time.Now()
	conn, _, th, err := m.openWith(ctx, c, secrets, false)
	if err != nil {
		return nil, 0, err
	}
	defer func() {
		conn.Close()
		if th != nil {
			th.Release()
		}
	}()
	info, err := conn.Server(ctx)
	return info, time.Since(start), err
}

func (m *Manager) open(ctx context.Context, c *store.Connection, readOnly bool) (driver.Conn, driver.Driver, *tunnel.Handle, error) {
	secrets, err := m.Secrets(c)
	if err != nil {
		return nil, nil, nil, err
	}
	return m.openWith(ctx, c, secrets, readOnly)
}

func (m *Manager) openWith(ctx context.Context, c *store.Connection, secrets map[string]string, readOnly bool) (driver.Conn, driver.Driver, *tunnel.Handle, error) {
	drv, ok := driver.Get(c.Driver)
	if !ok {
		return nil, nil, nil, fmt.Errorf("unknown database type %q", c.Driver)
	}
	params := map[string]any{}
	if len(c.Params) > 0 {
		if err := json.Unmarshal(c.Params, &params); err != nil {
			return nil, nil, nil, fmt.Errorf("invalid connection parameters: %w", err)
		}
	}
	op := driver.OpenParams{Params: params, Secrets: secrets, ReadOnly: readOnly || c.ReadOnly, AppName: m.appName}

	var th *tunnel.Handle
	var ssh SSHSettings
	if len(c.SSH) > 0 {
		_ = json.Unmarshal(c.SSH, &ssh)
	}
	if ssh.Enabled && len(ssh.Hops) > 0 {
		if !drv.Info().SSH {
			return nil, nil, nil, errors.New("this database type does not support SSH tunnels")
		}
		cfg := tunnel.Config{}
		for i, h := range ssh.Hops {
			hop := tunnel.Hop{Host: h.Host, Port: h.Port, User: h.User}
			switch h.Auth {
			case "key":
				hop.PrivateKey = secrets[SSHSecretKey(i, "key")]
				hop.Passphrase = secrets[SSHSecretKey(i, "passphrase")]
			default:
				hop.Password = secrets[SSHSecretKey(i, "password")]
			}
			cfg.Hops = append(cfg.Hops, hop)
		}
		var err error
		th, err = m.tunnels.Acquire(ctx, cfg)
		if err != nil {
			return nil, nil, nil, err
		}
		op.Dial = th.Dial
	}
	octx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	conn, err := drv.Open(octx, op)
	if err != nil {
		if th != nil {
			th.Release()
		}
		return nil, nil, nil, err
	}
	return conn, drv, th, nil
}

// Invalidate closes pools and consoles of a connection (after edit/delete).
func (m *Manager) Invalidate(connID string) {
	m.mu.Lock()
	var olds []*live
	for k, l := range m.live {
		if l.connID == connID {
			delete(m.live, k)
			olds = append(olds, l)
		}
	}
	var cons []*Console
	for k, c := range m.consoles {
		if c.ConnID == connID {
			delete(m.consoles, k)
			cons = append(cons, c)
		}
	}
	m.mu.Unlock()
	for _, c := range cons {
		c.close()
	}
	for _, l := range olds {
		go m.closeWhenUnused(l)
	}
}

func (m *Manager) closeWhenUnused(l *live) {
	for i := 0; i < 600; i++ {
		m.mu.Lock()
		refs := l.refs
		m.mu.Unlock()
		if refs <= 0 {
			break
		}
		time.Sleep(time.Second)
	}
	m.closeLive(l)
}

func (m *Manager) closeLive(l *live) {
	if l.conn != nil {
		l.conn.Close()
	}
	if l.tunnel != nil {
		l.tunnel.Release()
	}
}

func (m *Manager) reap() {
	for range time.Tick(time.Minute) {
		var idle []*live
		var cons []*Console
		m.mu.Lock()
		for k, l := range m.live {
			if l.opening == nil && l.refs <= 0 && time.Since(l.lastUsed) > m.idleConn {
				delete(m.live, k)
				idle = append(idle, l)
			} else if l.tunnel != nil && !l.tunnel.Alive() && l.refs <= 0 {
				delete(m.live, k)
				idle = append(idle, l)
			}
		}
		for k, c := range m.consoles {
			if !c.busy() && time.Since(c.lastUsed()) > m.idleConsole {
				delete(m.consoles, k)
				cons = append(cons, c)
			}
		}
		m.mu.Unlock()
		for _, c := range cons {
			c.close()
		}
		for _, l := range idle {
			m.closeLive(l)
		}
	}
}

func (m *Manager) Shutdown() {
	m.mu.Lock()
	lives := m.live
	cons := m.consoles
	m.live, m.consoles = map[string]*live{}, map[string]*Console{}
	m.mu.Unlock()
	for _, c := range cons {
		c.close()
	}
	for _, l := range lives {
		m.closeLive(l)
	}
}

// Stats for the admin overview.
type Stats struct {
	Pools    int `json:"pools"`
	Consoles int `json:"consoles"`
	Tunnels  int `json:"tunnels"`
}

func (m *Manager) Stats() Stats {
	m.mu.Lock()
	defer m.mu.Unlock()
	open, _ := m.tunnels.Stats()
	return Stats{Pools: len(m.live), Consoles: len(m.consoles), Tunnels: open}
}

// ---- consoles -------------------------------------------------------------------

// Console is a pinned session owned by one user for one editor tab.
type Console struct {
	ID       string
	UserID   string
	ConnID   string
	Scope    driver.Scope
	ReadOnly bool

	mu      sync.Mutex
	sess    driver.Session
	lease   *Lease
	used    time.Time
	running bool
	cancel  context.CancelFunc
	closed  bool
}

func (c *Console) lastUsed() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.used
}

func (c *Console) busy() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.running
}

func (c *Console) InTransaction() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sess != nil && c.sess.InTransaction()
}

func (c *Console) close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	if c.cancel != nil {
		c.cancel()
	}
	sess, lease := c.sess, c.lease
	c.mu.Unlock()
	if sess != nil {
		sess.Close()
	}
	if lease != nil {
		lease.Release()
	}
}

// consoleID derives a stable console key from the client's tab id, so a
// refreshed tab reattaches to its open transaction.
func consoleID(userID, connID, tab string, scope driver.Scope, ro bool) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{userID, connID, tab, scope.Database, scope.Schema, fmt.Sprint(ro)}, "\x00")))
	return hex.EncodeToString(sum[:16])
}

// Console returns (creating if needed) the console for a user's editor tab.
func (m *Manager) Console(ctx context.Context, userID string, c *store.Connection, tab string, scope driver.Scope, readOnly bool) (*Console, error) {
	if tab == "" {
		tab = id.New()
	}
	cid := consoleID(userID, c.ID, tab, scope, readOnly)
	m.mu.Lock()
	if con, ok := m.consoles[cid]; ok {
		m.mu.Unlock()
		return con, nil
	}
	n := 0
	for _, con := range m.consoles {
		if con.UserID == userID && con.ConnID == c.ID {
			n++
		}
	}
	m.mu.Unlock()
	if n >= 12 {
		return nil, errors.New("too many open consoles for this connection; close some editor tabs")
	}

	lease, err := m.Acquire(ctx, c, readOnly)
	if err != nil {
		return nil, err
	}
	sess, err := lease.Conn.NewSession(ctx, scope)
	if err != nil {
		lease.Release()
		return nil, err
	}
	con := &Console{ID: cid, UserID: userID, ConnID: c.ID, Scope: scope, ReadOnly: readOnly, sess: sess, lease: lease, used: time.Now()}
	m.mu.Lock()
	if existing, ok := m.consoles[cid]; ok {
		m.mu.Unlock()
		con.close()
		return existing, nil
	}
	m.consoles[cid] = con
	m.mu.Unlock()
	return con, nil
}

// CloseConsole ends a console (tab closed). Any open transaction rolls back.
func (m *Manager) CloseConsole(userID, cid string) {
	m.mu.Lock()
	con, ok := m.consoles[cid]
	if ok && con.UserID == userID {
		delete(m.consoles, cid)
	} else {
		ok = false
	}
	m.mu.Unlock()
	if ok {
		con.close()
	}
}

// Run executes a script on the console, streaming into sink.
func (c *Console) Run(ctx context.Context, script string, opts driver.ExecOptions, sink driver.Sink) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return errors.New("console was closed; run again to reconnect")
	}
	if c.running {
		c.mu.Unlock()
		return errors.New("a query is already running in this tab")
	}
	ctx, cancel := context.WithCancel(ctx)
	c.running, c.cancel, c.used = true, cancel, time.Now()
	sess := c.sess
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.running, c.cancel, c.used = false, nil, time.Now()
		c.mu.Unlock()
		cancel()
	}()
	opts.ReadOnly = opts.ReadOnly || c.ReadOnly
	return sess.Execute(ctx, script, opts, sink)
}

// Cancel stops the running query, if any.
func (m *Manager) Cancel(userID, cid string) bool {
	m.mu.Lock()
	con, ok := m.consoles[cid]
	m.mu.Unlock()
	if !ok || con.UserID != userID {
		return false
	}
	con.mu.Lock()
	defer con.mu.Unlock()
	if con.cancel != nil {
		con.cancel()
		return true
	}
	return false
}
