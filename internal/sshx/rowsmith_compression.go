// Rowsmith: SSH compression (RFC 4253, section 6.2), which
// golang.org/x/crypto/ssh leaves out. This file and rowsmith_inflate.go are
// Rowsmith's own; the hooks into the upstream files are in rowsmith.patch.

package ssh

import (
	"bytes"
	"compress/zlib"
	"io"
	"slices"
	"sync/atomic"
)

const (
	// CompressionZlibOpenSSH is zlib compression that starts once the user
	// is authenticated. It is the only kind OpenSSH servers offer by default.
	CompressionZlibOpenSSH = "zlib@openssh.com"
	// CompressionZlib is zlib compression from the first key exchange on.
	CompressionZlib = "zlib"
)

// compressionsFor keeps the supported algorithms of want, in order, and
// allows "none" last if want doesn't name it.
func compressionsFor(want []string) []string {
	var out []string
	for _, c := range want {
		if (c == CompressionZlibOpenSSH || c == CompressionZlib || c == compressionNone) && !slices.Contains(out, c) {
			out = append(out, c)
		}
	}
	if !slices.Contains(out, compressionNone) {
		out = append(out, compressionNone)
	}
	return out
}

// Compression returns the compression algorithm agreed for this direction.
func (a DirectionAlgorithms) Compression() string { return a.compression }

// delayedCompression says when zlib@openssh.com is on in each direction: from
// the packet after the user authentication success message.
type delayedCompression struct {
	reading, writing atomic.Bool
}

func (d *delayedCompression) startReading() { d.reading.Store(true) }

func (d *delayedCompression) start() {
	d.reading.Store(true)
	d.writing.Store(true)
}

// compressing wraps the cipher for packets sent after a key exchange that
// agreed on algo. Each key exchange starts a new compression context.
func (t *transport) compressing(c packetCipher, algo string) packetCipher {
	switch algo {
	case CompressionZlib:
		return &zlibWriter{packetCipher: c}
	case CompressionZlibOpenSSH:
		return &zlibWriter{packetCipher: c, on: &t.delayed.writing}
	}
	return c
}

// decompressing is compressing's counterpart for packets read.
func (t *transport) decompressing(c packetCipher, algo string) packetCipher {
	switch algo {
	case CompressionZlib:
		return &zlibReader{packetCipher: c}
	case CompressionZlibOpenSSH:
		return &zlibReader{packetCipher: c, on: &t.delayed.reading}
	}
	return c
}

// zlibWriter compresses each packet's payload before it is encrypted,
// ending it with a sync flush so the peer can decode all of it.
type zlibWriter struct {
	packetCipher
	on  *atomic.Bool // nil: always on
	buf bytes.Buffer
	z   *zlib.Writer
}

func (c *zlibWriter) writeCipherPacket(seqnum uint32, w io.Writer, rand io.Reader, packet []byte) error {
	if c.on != nil && !c.on.Load() {
		return c.packetCipher.writeCipherPacket(seqnum, w, rand, packet)
	}
	c.buf.Reset()
	if c.z == nil {
		c.z = zlib.NewWriter(&c.buf)
	}
	if _, err := c.z.Write(packet); err != nil {
		return err
	}
	if err := c.z.Flush(); err != nil {
		return err
	}
	return c.packetCipher.writeCipherPacket(seqnum, w, rand, c.buf.Bytes())
}

// zlibReader decompresses each packet's payload after it is decrypted.
type zlibReader struct {
	packetCipher
	on *atomic.Bool // nil: always on
	z  inflater
}

func (c *zlibReader) readCipherPacket(seqnum uint32, r io.Reader) ([]byte, error) {
	p, err := c.packetCipher.readCipherPacket(seqnum, r)
	if err != nil || c.on != nil && !c.on.Load() {
		return p, err
	}
	return c.z.inflate(p)
}
