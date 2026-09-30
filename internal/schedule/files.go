package schedule

import (
	"bufio"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"

	"golang.org/x/crypto/chacha20poly1305"
)

// Result files are kept for days, so unlike the spool they are sealed with
// an AEAD in 64 KiB chunks (the STREAM construction): each chunk's nonce
// carries its position and whether it is the last one, so chunks cannot be
// altered, reordered, dropped or cut off without detection.

const (
	chunkSize   = 64 << 10
	fileVersion = 1
	prefixSize  = 16 // random nonce prefix; the remaining 8 bytes are counter + final flag
)

var errCorrupt = errors.New("the result file is damaged or was altered")

// NewFileKey returns a random key for one file.
func NewFileKey() []byte {
	k := make([]byte, chacha20poly1305.KeySize)
	if _, err := rand.Read(k); err != nil {
		panic(err)
	}
	return k
}

type sealWriter struct {
	w      io.Writer
	aead   cipher.AEAD
	nonce  [chacha20poly1305.NonceSizeX]byte
	ad     []byte
	buf    []byte
	out    []byte
	count  uint64
	closed bool
}

// SealWriter encrypts everything written to it into w. ad binds the file
// to its record. Close writes the final chunk.
func SealWriter(w io.Writer, key []byte, ad string) (io.WriteCloser, error) {
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, err
	}
	s := &sealWriter{w: w, aead: aead, ad: []byte(ad), buf: make([]byte, 0, chunkSize)}
	if _, err := rand.Read(s.nonce[:prefixSize]); err != nil {
		return nil, err
	}
	head := append([]byte{fileVersion}, s.nonce[:prefixSize]...)
	if _, err := w.Write(head); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *sealWriter) Write(p []byte) (int, error) {
	if s.closed {
		return 0, errors.New("write after close")
	}
	n := 0
	for len(p) > 0 {
		if len(s.buf) == chunkSize {
			// Only flush a full chunk once more data arrives, so the final
			// chunk is never empty unless the whole file is.
			if err := s.flush(false); err != nil {
				return n, err
			}
		}
		k := copy(s.buf[len(s.buf):chunkSize], p)
		s.buf = s.buf[:len(s.buf)+k]
		p = p[k:]
		n += k
	}
	return n, nil
}

func (s *sealWriter) flush(last bool) error {
	setNonce(&s.nonce, s.count, last)
	s.out = s.aead.Seal(s.out[:0], s.nonce[:], s.buf, s.ad)
	s.count++
	s.buf = s.buf[:0]
	_, err := s.w.Write(s.out)
	return err
}

func (s *sealWriter) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	return s.flush(true)
}

func setNonce(n *[chacha20poly1305.NonceSizeX]byte, count uint64, last bool) {
	var c [8]byte
	binary.BigEndian.PutUint64(c[:], count<<8)
	if last {
		c[7] = 1
	}
	copy(n[prefixSize:], c[:])
}

type openReader struct {
	r     *bufio.Reader
	aead  cipher.AEAD
	nonce [chacha20poly1305.NonceSizeX]byte
	ad    []byte
	in    []byte
	plain []byte
	pos   int
	count uint64
	done  bool
}

// OpenReader decrypts a file written by SealWriter, failing on any change.
func OpenReader(r io.Reader, key []byte, ad string) (io.Reader, error) {
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, err
	}
	o := &openReader{r: bufio.NewReaderSize(r, chunkSize+aead.Overhead()+1), aead: aead, ad: []byte(ad),
		in: make([]byte, chunkSize+aead.Overhead())}
	head := make([]byte, 1+prefixSize)
	if _, err := io.ReadFull(o.r, head); err != nil || head[0] != fileVersion {
		return nil, errCorrupt
	}
	copy(o.nonce[:prefixSize], head[1:])
	return o, nil
}

func (o *openReader) Read(p []byte) (int, error) {
	for o.pos >= len(o.plain) {
		if o.done {
			return 0, io.EOF
		}
		if err := o.next(); err != nil {
			return 0, err
		}
	}
	n := copy(p, o.plain[o.pos:])
	o.pos += n
	return n, nil
}

func (o *openReader) next() error {
	n, err := io.ReadFull(o.r, o.in)
	last := false
	switch {
	case errors.Is(err, io.ErrUnexpectedEOF):
		last = true
	case errors.Is(err, io.EOF):
		return errCorrupt // the final chunk is missing
	case err != nil:
		return err
	default:
		if _, perr := o.r.Peek(1); errors.Is(perr, io.EOF) {
			last = true
		}
	}
	setNonce(&o.nonce, o.count, last)
	plain, oerr := o.aead.Open(o.plain[:0], o.nonce[:], o.in[:n], o.ad)
	if oerr != nil {
		return errCorrupt
	}
	o.plain, o.pos, o.count, o.done = plain, 0, o.count+1, last
	return nil
}
