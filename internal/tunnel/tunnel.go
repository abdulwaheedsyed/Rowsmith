// Package tunnel maintains SSH client chains (bastion → jump hosts) that
// database drivers dial through. Tunnels are shared between callers with the
// same configuration, kept alive with keepalive requests, and closed after an
// idle period. Host keys are verified against a trust store: unknown keys are
// surfaced to the user for explicit approval and changed keys are rejected.
package tunnel

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"
)

type Hop struct {
	Host       string `json:"host"`
	Port       int    `json:"port"`
	User       string `json:"user"`
	Password   string `json:"password,omitempty"`
	PrivateKey string `json:"privateKey,omitempty"`
	Passphrase string `json:"passphrase,omitempty"`
}

func (h Hop) addr() string {
	p := h.Port
	if p == 0 {
		p = 22
	}
	return net.JoinHostPort(h.Host, strconv.Itoa(p))
}

type Config struct {
	Hops      []Hop         // first hop is dialed directly; each next hop through the previous
	KeepAlive time.Duration // default 30s
}

type KnownKey struct {
	KeyType   string
	PublicKey string // base64 wire encoding
}

type HostKeyStore interface {
	HostKeys(ctx context.Context, host string, port int) ([]KnownKey, error)
}

// UnknownHostKeyError asks the user to verify and trust a first-seen host key.
type UnknownHostKeyError struct {
	Host        string `json:"host"`
	Port        int    `json:"port"`
	KeyType     string `json:"keyType"`
	Fingerprint string `json:"fingerprint"`
	PublicKey   string `json:"publicKey"`
}

func (e *UnknownHostKeyError) Error() string {
	return fmt.Sprintf("SSH host %s:%d is not trusted yet (%s %s)", e.Host, e.Port, e.KeyType, e.Fingerprint)
}

// HostKeyMismatchError means the server presented a different key than the
// trusted one: either the host was reinstalled or someone is intercepting.
type HostKeyMismatchError struct {
	Host        string `json:"host"`
	Port        int    `json:"port"`
	KeyType     string `json:"keyType"`
	Fingerprint string `json:"fingerprint"`
}

func (e *HostKeyMismatchError) Error() string {
	return fmt.Sprintf("SSH host key for %s:%d changed (now %s %s). Refusing to connect: this may be a man-in-the-middle attack",
		e.Host, e.Port, e.KeyType, e.Fingerprint)
}

type Manager struct {
	keys    HostKeyStore
	idle    time.Duration
	mu      sync.Mutex
	tunnels map[string]*tunnel
	log     *slog.Logger
}

func NewManager(keys HostKeyStore, log *slog.Logger) *Manager {
	m := &Manager{keys: keys, idle: 10 * time.Minute, tunnels: map[string]*tunnel{}, log: log}
	go m.reap()
	return m
}

type tunnel struct {
	key      string
	clients  []*ssh.Client
	refs     int
	lastUsed time.Time
	dead     chan struct{}
	once     sync.Once
}

func (t *tunnel) close() {
	t.once.Do(func() {
		close(t.dead)
		for i := len(t.clients) - 1; i >= 0; i-- {
			t.clients[i].Close()
		}
	})
}

func (t *tunnel) alive() bool {
	select {
	case <-t.dead:
		return false
	default:
		return true
	}
}

// Handle is a reference to a shared tunnel. Release it when the database
// connection pool that uses it is closed. If the SSH connection drops (the
// host restarted, the network failed), the next Dial reconnects.
type Handle struct {
	m    *Manager
	cfg  Config
	mu   sync.Mutex
	t    *tunnel
	once sync.Once
}

// Acquire returns a handle to a live tunnel for cfg, connecting if needed.
func (m *Manager) Acquire(ctx context.Context, cfg Config) (*Handle, error) {
	t, err := m.acquire(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &Handle{m: m, cfg: cfg, t: t}, nil
}

// acquire returns a live tunnel for cfg with a reference taken, connecting
// if needed.
func (m *Manager) acquire(ctx context.Context, cfg Config) (*tunnel, error) {
	if len(cfg.Hops) == 0 {
		return nil, errors.New("tunnel: no SSH host configured")
	}
	key := configKey(cfg)
	m.mu.Lock()
	if t, ok := m.tunnels[key]; ok && t.alive() {
		t.refs++
		t.lastUsed = time.Now()
		m.mu.Unlock()
		return t, nil
	}
	m.mu.Unlock()

	clients, err := m.connect(ctx, cfg)
	if err != nil {
		return nil, err
	}
	t := &tunnel{key: key, clients: clients, refs: 1, lastUsed: time.Now(), dead: make(chan struct{})}

	m.mu.Lock()
	if existing, ok := m.tunnels[key]; ok && existing.alive() {
		// Lost a race with a concurrent Acquire; use theirs.
		existing.refs++
		m.mu.Unlock()
		t.close()
		return existing, nil
	}
	m.tunnels[key] = t
	m.mu.Unlock()

	ka := cfg.KeepAlive
	if ka <= 0 {
		ka = 30 * time.Second
	}
	go m.keepalive(t, ka)
	return t, nil
}

func (m *Manager) release(t *tunnel) {
	m.mu.Lock()
	t.refs--
	t.lastUsed = time.Now()
	m.mu.Unlock()
}

// current returns the handle's tunnel, reconnecting first if it has died.
// Reconnecting verifies host keys again, so a changed key is still refused.
func (h *Handle) current(ctx context.Context) (*tunnel, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.t.alive() {
		return h.t, nil
	}
	t, err := h.m.acquire(ctx, h.cfg)
	if err != nil {
		return nil, err
	}
	h.m.release(h.t)
	h.t = t
	return t, nil
}

// Dial opens a TCP connection from the last hop to addr. The returned conn
// supports deadlines (SSH channels do not natively), which drivers rely on
// for query cancellation. If the SSH connection has gone away, Dial drops it
// and tries once more on a fresh one, so callers don't wait for the keepalive
// to notice.
func (h *Handle) Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	for retried := false; ; retried = true {
		t, err := h.current(ctx)
		if err != nil {
			return nil, err
		}
		h.m.mu.Lock()
		t.lastUsed = time.Now()
		h.m.mu.Unlock()
		ch, err := t.clients[len(t.clients)-1].DialContext(ctx, "tcp", addr)
		if err == nil {
			return bridge(ch)
		}
		// An OpenChannelError means the SSH server answered but couldn't reach
		// addr: the connection is fine and reconnecting wouldn't help.
		var refused *ssh.OpenChannelError
		if errors.As(err, &refused) || ctx.Err() != nil || retried {
			return nil, fmt.Errorf("tunnel: SSH server could not reach %s: %w", addr, err)
		}
		h.m.log.Info("ssh connection lost; reconnecting", "err", err)
		h.m.drop(t)
	}
}

func (h *Handle) Release() {
	h.once.Do(func() {
		h.mu.Lock()
		t := h.t
		h.mu.Unlock()
		h.m.release(t)
	})
}

func (h *Handle) Alive() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.t.alive()
}

func (m *Manager) connect(ctx context.Context, cfg Config) ([]*ssh.Client, error) {
	var clients []*ssh.Client
	fail := func(err error) ([]*ssh.Client, error) {
		for i := len(clients) - 1; i >= 0; i-- {
			clients[i].Close()
		}
		return nil, err
	}
	for i, hop := range cfg.Hops {
		if hop.Host == "" || hop.User == "" {
			return fail(fmt.Errorf("tunnel: hop %d needs a host and user", i+1))
		}
		port := hop.Port
		if port == 0 {
			port = 22
		}
		auth, err := authMethods(hop)
		if err != nil {
			return fail(err)
		}
		known, err := m.keys.HostKeys(ctx, hop.Host, port)
		if err != nil {
			return fail(err)
		}
		conf := &ssh.ClientConfig{
			User:            hop.User,
			Auth:            auth,
			HostKeyCallback: verifier(hop.Host, port, known),
			Timeout:         20 * time.Second,
		}
		if algos := algorithmsFor(known); len(algos) > 0 {
			conf.HostKeyAlgorithms = algos
		}

		var raw net.Conn
		if i == 0 {
			d := net.Dialer{Timeout: 20 * time.Second, KeepAlive: 30 * time.Second}
			raw, err = d.DialContext(ctx, "tcp", hop.addr())
		} else {
			raw, err = clients[i-1].DialContext(ctx, "tcp", hop.addr())
		}
		if err != nil {
			return fail(fmt.Errorf("tunnel: cannot reach SSH host %s: %w", hop.addr(), err))
		}
		c, err := handshake(ctx, raw, hop.addr(), conf)
		if err != nil {
			raw.Close()
			var uk *UnknownHostKeyError
			var mm *HostKeyMismatchError
			if errors.As(err, &uk) || errors.As(err, &mm) {
				return fail(err)
			}
			return fail(fmt.Errorf("tunnel: SSH login to %s@%s failed: %w", hop.User, hop.addr(), err))
		}
		clients = append(clients, c)
	}
	return clients, nil
}

// handshake runs the SSH handshake, aborting when ctx is cancelled (the
// library call itself takes no context).
func handshake(ctx context.Context, raw net.Conn, addr string, conf *ssh.ClientConfig) (*ssh.Client, error) {
	type result struct {
		c   *ssh.Client
		err error
	}
	done := make(chan result, 1)
	go func() {
		cc, chans, reqs, err := ssh.NewClientConn(raw, addr, conf)
		if err != nil {
			done <- result{err: err}
			return
		}
		done <- result{c: ssh.NewClient(cc, chans, reqs)}
	}()
	timer := time.NewTimer(conf.Timeout)
	defer timer.Stop()
	select {
	case r := <-done:
		return r.c, r.err
	case <-ctx.Done():
		raw.Close()
		return nil, ctx.Err()
	case <-timer.C:
		raw.Close()
		return nil, errors.New("SSH handshake timed out")
	}
}

func authMethods(h Hop) ([]ssh.AuthMethod, error) {
	var out []ssh.AuthMethod
	if strings.TrimSpace(h.PrivateKey) != "" {
		var signer ssh.Signer
		var err error
		if h.Passphrase != "" {
			signer, err = ssh.ParsePrivateKeyWithPassphrase([]byte(h.PrivateKey), []byte(h.Passphrase))
		} else {
			signer, err = ssh.ParsePrivateKey([]byte(h.PrivateKey))
		}
		if err != nil {
			var missing *ssh.PassphraseMissingError
			if errors.As(err, &missing) {
				return nil, errors.New("tunnel: the private key is encrypted; enter its passphrase")
			}
			return nil, fmt.Errorf("tunnel: cannot parse private key: %w", err)
		}
		out = append(out, ssh.PublicKeys(signer))
	}
	if h.Password != "" {
		pw := h.Password
		out = append(out, ssh.Password(pw), ssh.KeyboardInteractive(func(_, _ string, qs []string, _ []bool) ([]string, error) {
			ans := make([]string, len(qs))
			for i := range ans {
				ans[i] = pw
			}
			return ans, nil
		}))
	}
	if len(out) == 0 {
		return nil, errors.New("tunnel: provide an SSH password or private key")
	}
	return out, nil
}

func Fingerprint(k ssh.PublicKey) string { return ssh.FingerprintSHA256(k) }

func verifier(host string, port int, known []KnownKey) ssh.HostKeyCallback {
	return func(_ string, _ net.Addr, key ssh.PublicKey) error {
		wire := base64.StdEncoding.EncodeToString(key.Marshal())
		for _, k := range known {
			if k.KeyType == key.Type() {
				if k.PublicKey == wire {
					return nil
				}
				return &HostKeyMismatchError{Host: host, Port: port, KeyType: key.Type(), Fingerprint: Fingerprint(key)}
			}
		}
		if len(known) > 0 {
			// We restricted algorithms to known key types; a different type here is suspicious.
			return &HostKeyMismatchError{Host: host, Port: port, KeyType: key.Type(), Fingerprint: Fingerprint(key)}
		}
		return &UnknownHostKeyError{Host: host, Port: port, KeyType: key.Type(), Fingerprint: Fingerprint(key), PublicKey: wire}
	}
}

func algorithmsFor(known []KnownKey) []string {
	var out []string
	for _, k := range known {
		switch k.KeyType {
		case ssh.KeyAlgoRSA:
			out = append(out, ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSA)
		default:
			out = append(out, k.KeyType)
		}
	}
	return out
}

func (m *Manager) keepalive(t *tunnel, every time.Duration) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-t.dead:
			return
		case <-tick.C:
		}
		for _, c := range t.clients {
			errc := make(chan error, 1)
			go func() {
				_, _, err := c.SendRequest("keepalive@openssh.com", true, nil)
				errc <- err
			}()
			select {
			case err := <-errc:
				if err != nil {
					m.log.Warn("ssh keepalive failed; closing tunnel", "err", err)
					m.drop(t)
					return
				}
			case <-time.After(15 * time.Second):
				m.log.Warn("ssh keepalive timed out; closing tunnel")
				m.drop(t)
				return
			}
		}
	}
}

func (m *Manager) drop(t *tunnel) {
	m.mu.Lock()
	if m.tunnels[t.key] == t {
		delete(m.tunnels, t.key)
	}
	m.mu.Unlock()
	t.close()
}

func (m *Manager) reap() {
	for range time.Tick(time.Minute) {
		var idle []*tunnel
		m.mu.Lock()
		for k, t := range m.tunnels {
			if !t.alive() {
				delete(m.tunnels, k)
				continue
			}
			if t.refs <= 0 && time.Since(t.lastUsed) > m.idle {
				delete(m.tunnels, k)
				idle = append(idle, t)
			}
		}
		m.mu.Unlock()
		for _, t := range idle {
			t.close()
		}
	}
}

// Stats reports open tunnels for the admin dashboard.
func (m *Manager) Stats() (open, inUse int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, t := range m.tunnels {
		open++
		if t.refs > 0 {
			inUse++
		}
	}
	return
}

func (m *Manager) CloseAll() {
	m.mu.Lock()
	ts := m.tunnels
	m.tunnels = map[string]*tunnel{}
	m.mu.Unlock()
	for _, t := range ts {
		t.close()
	}
}

func configKey(cfg Config) string {
	b, _ := json.Marshal(cfg.Hops)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// bridge adapts an SSH channel into a kernel socket so that callers get full
// net.Conn semantics, including deadlines.
func bridge(ch net.Conn) (net.Conn, error) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		ch.Close()
		return nil, err
	}
	fa, fb := os.NewFile(uintptr(fds[0]), "rowsmith-tunnel-a"), os.NewFile(uintptr(fds[1]), "rowsmith-tunnel-b")
	a, errA := net.FileConn(fa)
	b, errB := net.FileConn(fb)
	fa.Close()
	fb.Close()
	if errA != nil || errB != nil {
		ch.Close()
		if a != nil {
			a.Close()
		}
		if b != nil {
			b.Close()
		}
		return nil, errors.Join(errA, errB)
	}
	var once sync.Once
	closeAll := func() { once.Do(func() { ch.Close(); b.Close() }) }
	go func() { _, _ = io.Copy(b, ch); closeAll() }()
	go func() { _, _ = io.Copy(ch, b); closeAll() }()
	return &tunneledConn{Conn: a, remote: ch.RemoteAddr()}, nil
}

type tunneledConn struct {
	net.Conn
	remote net.Addr
}

func (c *tunneledConn) RemoteAddr() net.Addr { return c.remote }
