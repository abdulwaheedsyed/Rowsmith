package ssh

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

// TestIntegrationOpenSSHCompression runs compressed sessions against a real
// OpenSSH server (the dev stack's bastion) when ROWSMITH_TEST_SSH_HOST is
// set, with user tunnel and ROWSMITH_TEST_SSH_PASSWORD. A low rekey threshold
// makes both sides restart their compression many times.
func TestIntegrationOpenSSHCompression(t *testing.T) {
	host := os.Getenv("ROWSMITH_TEST_SSH_HOST")
	if host == "" {
		t.Skip("ROWSMITH_TEST_SSH_HOST not set")
	}
	addr := net.JoinHostPort(host, "22")
	key := serverKey(t, addr)
	for _, algo := range []string{CompressionZlibOpenSSH, "none"} {
		t.Run(algo, func(t *testing.T) {
			raw, err := net.Dial("tcp", addr)
			if err != nil {
				t.Fatal(err)
			}
			var wire atomic.Int64
			conf := &ClientConfig{User: "tunnel", Auth: []AuthMethod{Password(os.Getenv("ROWSMITH_TEST_SSH_PASSWORD"))},
				HostKeyCallback: FixedHostKey(key)}
			conf.Compressions = []string{algo}
			conf.RekeyThreshold = 1 << 20
			cc, chans, reqs, err := NewClientConn(countingConn{raw, &wire}, addr, conf)
			if err != nil {
				t.Fatal(err)
			}
			client := NewClient(cc, chans, reqs)
			defer client.Close()
			if got := cc.(AlgorithmsConnMetadata).Algorithms(); got.Read.Compression() != algo || got.Write.Compression() != algo {
				t.Fatalf("agreed %q and %q", got.Read.Compression(), got.Write.Compression())
			}

			// Down: about 20 MB of output.
			s, err := client.NewSession()
			if err != nil {
				t.Fatal(err)
			}
			before := wire.Load()
			out, err := s.Output("seq 1 2800000")
			if err != nil {
				t.Fatal(err)
			}
			read := wire.Load() - before
			var want bytes.Buffer
			for i := 1; i <= 2800000; i++ {
				fmt.Fprintf(&want, "%d\n", i)
			}
			if !bytes.Equal(out, want.Bytes()) {
				t.Fatalf("seq: got %d bytes, want %d", len(out), want.Len())
			}
			t.Logf("%s: %d bytes of output, %d on the wire", algo, len(out), read)
			if compressed := read < int64(len(out))/2; compressed != (algo != "none") {
				t.Errorf("%d bytes on the wire for %d", read, len(out))
			}

			// Up: the server hashes what it was sent.
			s, err = client.NewSession()
			if err != nil {
				t.Fatal(err)
			}
			s.Stdin = bytes.NewReader(want.Bytes())
			sum, err := s.Output("sha256sum")
			if err != nil {
				t.Fatal(err)
			}
			h := sha256.Sum256(want.Bytes())
			if got := strings.Fields(string(sum)); len(got) == 0 || got[0] != hex.EncodeToString(h[:]) {
				t.Fatalf("sha256sum = %q", sum)
			}
		})
	}
}

var errGotKey = errors.New("got the host key")

// serverKey reads the server's host key, trusting the dev stack's bastion
// on first use.
func serverKey(t *testing.T, addr string) PublicKey {
	var key PublicKey
	conf := &ClientConfig{User: "tunnel", HostKeyCallback: func(_ string, _ net.Addr, k PublicKey) error {
		key = k
		return errGotKey
	}}
	c, err := Dial("tcp", addr, conf)
	if err == nil {
		c.Close()
	}
	if key == nil {
		t.Fatalf("no host key from %s: %v", addr, err)
	}
	return key
}
