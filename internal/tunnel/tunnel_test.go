package tunnel

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// sshServer is a minimal bastion for tests: password sign-in and port
// forwarding, with a way to cut every connection as a restart would.
type sshServer struct {
	ln    net.Listener
	conf  *ssh.ServerConfig
	key   ssh.Signer
	mu    sync.Mutex
	conns []net.Conn
}

func newSSHServer(t *testing.T) *sshServer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	conf := &ssh.ServerConfig{PasswordCallback: func(c ssh.ConnMetadata, pw []byte) (*ssh.Permissions, error) {
		if c.User() == "tunnel" && string(pw) == "secret" {
			return nil, nil
		}
		return nil, errors.New("wrong password")
	}}
	conf.AddHostKey(signer)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &sshServer{ln: ln, conf: conf, key: signer}
	go s.serve()
	t.Cleanup(func() { ln.Close(); s.dropAll() })
	return s
}

func (s *sshServer) serve() {
	for {
		raw, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.conns = append(s.conns, raw)
		s.mu.Unlock()
		go s.handle(raw)
	}
}

func (s *sshServer) handle(raw net.Conn) {
	_, chans, reqs, err := ssh.NewServerConn(raw, s.conf)
	if err != nil {
		raw.Close()
		return
	}
	go ssh.DiscardRequests(reqs)
	for nc := range chans {
		if nc.ChannelType() != "direct-tcpip" {
			nc.Reject(ssh.UnknownChannelType, "only port forwarding")
			continue
		}
		var to struct {
			Host     string
			Port     uint32
			FromHost string
			FromPort uint32
		}
		if err := ssh.Unmarshal(nc.ExtraData(), &to); err != nil {
			nc.Reject(ssh.ConnectionFailed, "bad request")
			continue
		}
		target, err := net.Dial("tcp", net.JoinHostPort(to.Host, strconv.Itoa(int(to.Port))))
		if err != nil {
			nc.Reject(ssh.ConnectionFailed, err.Error())
			continue
		}
		ch, chReqs, err := nc.Accept()
		if err != nil {
			target.Close()
			continue
		}
		go ssh.DiscardRequests(chReqs)
		go func() { io.Copy(ch, target); ch.CloseWrite() }()
		go func() { io.Copy(target, ch); target.Close() }()
	}
}

// dropAll cuts every SSH connection, as a bastion restart does.
func (s *sshServer) dropAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.conns {
		c.Close()
	}
	s.conns = nil
}

func (s *sshServer) hop() Hop {
	host, port, _ := net.SplitHostPort(s.ln.Addr().String())
	p, _ := strconv.Atoi(port)
	return Hop{Host: host, Port: p, User: "tunnel", Password: "secret"}
}

type trusted []KnownKey

func (k trusted) HostKeys(context.Context, string, int) ([]KnownKey, error) { return k, nil }

func echoServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()
	return ln.Addr().String()
}

// TestDialReconnectsAfterTheConnectionDrops: when the bastion restarts, the
// next dial reconnects instead of failing until the keepalive notices.
func TestDialReconnectsAfterTheConnectionDrops(t *testing.T) {
	srv := newSSHServer(t)
	echo := echoServer(t)
	pub := srv.key.PublicKey()
	m := NewManager(trusted{{KeyType: pub.Type(), PublicKey: base64.StdEncoding.EncodeToString(pub.Marshal())}}, slog.New(slog.DiscardHandler))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	h, err := m.Acquire(ctx, Config{Hops: []Hop{srv.hop()}})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Release()
	roundTrip := func(step string) {
		t.Helper()
		c, err := h.Dial(ctx, "tcp", echo)
		if err != nil {
			t.Fatalf("%s: %v", step, err)
		}
		defer c.Close()
		if _, err := c.Write([]byte("ping")); err != nil {
			t.Fatalf("%s: %v", step, err)
		}
		buf := make([]byte, 4)
		if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "ping" {
			t.Fatalf("%s: read %q, %v", step, buf, err)
		}
	}
	roundTrip("first dial")
	first := h.t

	srv.dropAll()
	time.Sleep(100 * time.Millisecond) // let the client see the connection close
	roundTrip("dial after the SSH connection dropped")
	if h.t == first || !h.Alive() {
		t.Fatal("the handle should be on a new, live tunnel")
	}
	if open, _ := m.Stats(); open != 1 {
		t.Fatalf("%d tunnels open, want 1 (the dead one dropped)", open)
	}

	// A target the SSH server can't reach is an answer, not a dead connection:
	// the error comes back and the tunnel stays.
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	closed := ln.Addr().String()
	ln.Close()
	current := h.t
	_, err = h.Dial(ctx, "tcp", closed)
	var refused *ssh.OpenChannelError
	if !errors.As(err, &refused) {
		t.Fatalf("dial to a closed port: %v, want an OpenChannelError", err)
	}
	if h.t != current || !h.Alive() {
		t.Fatal("a refused target must not drop the tunnel")
	}
}

// TestReconnectStillChecksTheHostKey: a bastion that comes back with a
// different key is refused, not trusted because the tunnel was reconnecting.
func TestReconnectStillChecksTheHostKey(t *testing.T) {
	srv := newSSHServer(t)
	echo := echoServer(t)
	pub := srv.key.PublicKey()
	m := NewManager(trusted{{KeyType: pub.Type(), PublicKey: base64.StdEncoding.EncodeToString(pub.Marshal())}}, slog.New(slog.DiscardHandler))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	h, err := m.Acquire(ctx, Config{Hops: []Hop{srv.hop()}})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Release()
	if c, err := h.Dial(ctx, "tcp", echo); err != nil {
		t.Fatal(err)
	} else {
		c.Close()
	}

	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	other, _ := ssh.NewSignerFromKey(priv)
	srv.conf.AddHostKey(other) // replaces the key of the same type
	srv.dropAll()
	time.Sleep(100 * time.Millisecond)
	_, err = h.Dial(ctx, "tcp", echo)
	var changed *HostKeyMismatchError
	if !errors.As(err, &changed) || changed.Fingerprint != Fingerprint(other.PublicKey()) {
		t.Fatalf("reconnecting to a changed key: %v, want a HostKeyMismatchError", err)
	}
}
