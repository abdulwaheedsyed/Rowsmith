package ssh

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	mrand "math/rand/v2"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"testing"
)

// The fixture holds zlib streams written the way OpenSSH writes them, each
// packet ending with Z_PARTIAL_FLUSH, so a packet's last bits often arrive
// with the next one. Every packet must still decode in full on its own.
func TestInflatePartialFlush(t *testing.T) {
	f, err := os.Open("rowsmith_testdata/partial_flush.bin.gz")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(gz)
	if err != nil {
		t.Fatal(err)
	}
	var z *inflater
	level, packets := -1, 0
	for len(data) > 0 {
		lv, n, m := int(data[0]), int(binary.BigEndian.Uint32(data[1:])), int(binary.BigEndian.Uint32(data[5:]))
		data = data[9:]
		p, want := data[:n], data[n:n+m]
		data = data[n+m:]
		if lv != level {
			z, level = &inflater{}, lv
		}
		got, err := z.inflate(p)
		if err != nil {
			t.Fatalf("level %d packet %d: %v", lv, packets, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("level %d packet %d: got %d bytes, want %d", lv, packets, len(got), len(want))
		}
		packets++
	}
	if packets < 20 {
		t.Fatalf("only %d packets in the fixture", packets)
	}
}

func sample(r *mrand.Rand, n int) []byte {
	words := []string{"order", "customer", "Riyadh", "2026-11-01", "NULL", "4.50", "alpha", "\x00\x01\x02"}
	var b bytes.Buffer
	for b.Len() < n {
		if r.IntN(10) == 0 {
			var x [64]byte
			for i := range x {
				x[i] = byte(r.Uint32())
			}
			b.Write(x[:r.IntN(64)])
		}
		fmt.Fprintf(&b, "%s %d,", words[r.IntN(len(words))], r.IntN(100000))
	}
	return b.Bytes()[:n]
}

// Packets from compress/zlib end with a sync flush; the stream is also cut at
// arbitrary bytes, mid-symbol included, which the decoder must resume from.
func TestInflateSplits(t *testing.T) {
	r := mrand.New(mrand.NewPCG(1, 2))
	for _, level := range []int{zlib.NoCompression, zlib.BestSpeed, zlib.DefaultCompression, zlib.BestCompression, zlib.HuffmanOnly} {
		var stream, plain bytes.Buffer
		zw, _ := zlib.NewWriterLevel(&stream, level)
		var cuts []int
		for range 40 {
			m := sample(r, r.IntN(20000)+1)
			zw.Write(m)
			zw.Flush()
			plain.Write(m)
			cuts = append(cuts, stream.Len())
		}
		// Whole packets: each decodes to exactly its message.
		z, prev, done := &inflater{}, 0, 0
		for _, c := range cuts {
			got, err := z.inflate(stream.Bytes()[prev:c])
			if err != nil {
				t.Fatalf("level %d: %v", level, err)
			}
			if !bytes.Equal(got, plain.Bytes()[done:done+len(got)]) || done+len(got) > plain.Len() {
				t.Fatalf("level %d: wrong output at %d", level, done)
			}
			done += len(got)
			prev = c
		}
		if done != plain.Len() {
			t.Fatalf("level %d: %d of %d bytes", level, done, plain.Len())
		}
		// Arbitrary pieces, down to single bytes.
		z, prev = &inflater{}, 0
		var out []byte
		for prev < stream.Len() {
			n := min(stream.Len()-prev, 1+r.IntN(3)*r.IntN(700))
			got, err := z.inflate(stream.Bytes()[prev : prev+n])
			if err != nil {
				t.Fatalf("level %d split: %v", level, err)
			}
			out = append(out, got...)
			prev += n
		}
		if !bytes.Equal(out, plain.Bytes()) {
			t.Fatalf("level %d split: output differs", level)
		}
	}
}

// Damaged input fails or decodes to something, but never panics or overruns
// the packet limit.
func TestInflateCorrupt(t *testing.T) {
	r := mrand.New(mrand.NewPCG(3, 4))
	var stream bytes.Buffer
	zw := zlib.NewWriter(&stream)
	zw.Write(sample(r, 50000))
	zw.Flush()
	good := stream.Bytes()
	for range 3000 {
		b := bytes.Clone(good)
		for range 1 + r.IntN(4) {
			b[2+r.IntN(len(b)-2)] ^= byte(1 << r.IntN(8))
		}
		z := &inflater{}
		for len(b) > 0 {
			n := min(len(b), 1+r.IntN(4000))
			out, err := z.inflate(b[:n])
			if err != nil {
				break
			}
			if len(out) > maxPacket+258 {
				t.Fatalf("%d bytes from one packet", len(out))
			}
			b = b[n:]
		}
	}
	// A run of matches that expands far beyond a packet's size.
	var bomb bytes.Buffer
	zw = zlib.NewWriter(&bomb)
	zw.Write(make([]byte, 2*maxPacket))
	zw.Flush()
	if _, err := (&inflater{}).inflate(bomb.Bytes()); err == nil {
		t.Fatal("a packet expanding to 512 KiB was accepted")
	}
}

// Streams zlib rejects are rejected here too.
func TestInflateInvalidCodes(t *testing.T) {
	// A dynamic block whose only distance code is 8 bits long: an incomplete
	// code is allowed only as a single 1-bit code.
	stream := append([]byte{0x78, 0x9c}, "$ $ 2A72B72B72B72B72B72B7ABZB72B700"...)
	if out, err := (&inflater{}).inflate(stream); err == nil {
		t.Errorf("an incomplete 8-bit code decoded to %q", out)
	}
	// A code with no symbols must not decode with an earlier code's table.
	var h huffman
	h.build([]uint8{1, 1})
	h.build([]uint8{0})
	if h.fast != [1 << fastBits]uint16{} {
		t.Error("an empty code kept the previous code's lookup table")
	}
}

func FuzzInflate(f *testing.F) {
	var b bytes.Buffer
	zw := zlib.NewWriter(&b)
	zw.Write([]byte("SSH_MSG_CHANNEL_DATA hello hello hello"))
	zw.Flush()
	f.Add(b.Bytes(), 5)
	f.Add([]byte{0x78, 0x9c, 0x02, 0x00}, 1)
	f.Fuzz(func(t *testing.T, data []byte, split int) {
		z := &inflater{}
		if split > 0 && split < len(data) {
			if _, err := z.inflate(data[:split]); err != nil {
				return
			}
			data = data[split:]
		}
		z.inflate(data)
	})
}

// countingConn counts the bytes read from the network.
type countingConn struct {
	net.Conn
	n *atomic.Int64
}

func (c countingConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.n.Add(int64(n))
	return n, err
}

// echoServer accepts one connection with a password login and echoes every
// "echo" channel. It returns its host key.
func echoServer(t *testing.T, ln net.Listener, compressions []string) PublicKey {
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	signer, err := NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	conf := &ServerConfig{PasswordCallback: func(ConnMetadata, []byte) (*Permissions, error) { return nil, nil }}
	conf.Compressions = compressions
	conf.AddHostKey(signer)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		_, chans, reqs, err := NewServerConn(c, conf)
		if err != nil {
			c.Close()
			return
		}
		go DiscardRequests(reqs)
		for nc := range chans {
			ch, chReqs, err := nc.Accept()
			if err != nil {
				continue
			}
			go DiscardRequests(chReqs)
			go func() {
				io.Copy(ch, ch)
				ch.CloseWrite()
			}()
		}
	}()
	return signer.PublicKey()
}

func TestCompressedConnection(t *testing.T) {
	for _, tc := range []struct {
		name           string
		client, server []string
		rekey          uint64
		compressed     bool
	}{
		{"delayed", []string{CompressionZlibOpenSSH}, []string{CompressionZlibOpenSSH, "none"}, 0, true},
		{"from the start", []string{CompressionZlib, CompressionZlibOpenSSH}, []string{CompressionZlib}, 0, true},
		{"rekeying", []string{CompressionZlibOpenSSH}, []string{CompressionZlibOpenSSH}, 100 << 10, true},
		{"server without", []string{CompressionZlibOpenSSH, CompressionZlib}, nil, 0, false},
		{"client without", nil, []string{CompressionZlibOpenSSH}, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer ln.Close()
			hostKey := echoServer(t, ln, tc.server)
			raw, err := net.Dial("tcp", ln.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			var wire atomic.Int64
			conf := &ClientConfig{User: "u", Auth: []AuthMethod{Password("p")}, HostKeyCallback: FixedHostKey(hostKey)}
			conf.Compressions = tc.client
			conf.RekeyThreshold = tc.rekey
			cc, chans, reqs, err := NewClientConn(countingConn{raw, &wire}, ln.Addr().String(), conf)
			if err != nil {
				t.Fatal(err)
			}
			client := NewClient(cc, chans, reqs)
			defer client.Close()

			msg := sample(mrand.New(mrand.NewPCG(5, 6)), 1<<20)
			ch, chReqs, err := client.OpenChannel("echo", nil)
			if err != nil {
				t.Fatal(err)
			}
			go DiscardRequests(chReqs)
			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				defer wg.Done()
				ch.Write(msg)
				ch.CloseWrite()
			}()
			before := wire.Load()
			got, err := io.ReadAll(ch)
			wg.Wait()
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, msg) {
				t.Fatalf("echo: got %d bytes, want %d", len(got), len(msg))
			}
			read := wire.Load() - before
			if compressed := read < int64(len(msg))*3/4; compressed != tc.compressed {
				t.Errorf("%d bytes on the wire for %d echoed, compressed = %v", read, len(msg), compressed)
			}
		})
	}
}

func TestCompressionsFor(t *testing.T) {
	got := compressionsFor([]string{"zstd", CompressionZlibOpenSSH, CompressionZlib, CompressionZlibOpenSSH})
	if fmt.Sprint(got) != "[zlib@openssh.com zlib none]" {
		t.Errorf("got %v", got)
	}
	if got := compressionsFor(nil); fmt.Sprint(got) != "[none]" {
		t.Errorf("got %v", got)
	}
}

func BenchmarkInflate(b *testing.B) {
	r := mrand.New(mrand.NewPCG(7, 8))
	var stream bytes.Buffer
	zw := zlib.NewWriter(&stream)
	var packets [][]byte
	total := 0
	for range 200 {
		m := sample(r, 32<<10)
		prev := stream.Len()
		zw.Write(m)
		zw.Flush()
		packets = append(packets, bytes.Clone(stream.Bytes()[prev:]))
		total += len(m)
	}
	b.SetBytes(int64(total))
	for b.Loop() {
		z := &inflater{}
		for _, p := range packets {
			if _, err := z.inflate(p); err != nil {
				b.Fatal(err)
			}
		}
	}
}
