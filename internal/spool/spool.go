// Package spool keeps short-lived files, finished exports and uploaded
// imports, on disk. Each file is encrypted with its own random key that
// exists only in memory, so spooled data cannot be read from a copy of the
// data directory or after a restart. Files expire after a fixed time.
package spool

import (
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/crypto/chacha20"
)

// ErrNotFound is returned for unknown, expired or foreign files.
var ErrNotFound = errors.New("the file has expired or does not exist")

type Spool struct {
	dir   string
	ttl   time.Duration
	mu    sync.Mutex
	files map[string]*File
}

// File is a finished spool entry.
type File struct {
	ID          string
	Owner       string // user ID; only the owner can read the file
	Name        string // suggested download name
	ContentType string
	Size        int64
	Expires     time.Time
	Meta        map[string]any

	path  string
	key   [chacha20.KeySize]byte
	nonce [chacha20.NonceSizeX]byte
}

// New prepares dir (removing anything left from a previous run, which can no
// longer be decrypted) and starts expiring files.
func New(dir string, ttl time.Duration) (*Spool, error) {
	if err := os.RemoveAll(dir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &Spool{dir: dir, ttl: ttl, files: map[string]*File{}}
	go s.janitor()
	return s, nil
}

func (s *Spool) janitor() {
	for range time.Tick(time.Minute) {
		now := time.Now()
		s.mu.Lock()
		var gone []*File
		for id, f := range s.files {
			if now.After(f.Expires) {
				delete(s.files, id)
				gone = append(gone, f)
			}
		}
		s.mu.Unlock()
		for _, f := range gone {
			os.Remove(f.path)
		}
	}
}

// Writer receives a new file's contents. Call Commit or Abort.
type Writer struct {
	s    *Spool
	f    *File
	fh   *os.File
	sw   cipher.StreamWriter
	n    int64
	done bool
}

// Create starts a new file owned by owner.
func (s *Spool) Create(owner, name, contentType string) (*Writer, error) {
	f := &File{Owner: owner, Name: name, ContentType: contentType, Meta: map[string]any{}}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return nil, err
	}
	f.ID = hex.EncodeToString(id[:])
	if _, err := rand.Read(f.key[:]); err != nil {
		return nil, err
	}
	if _, err := rand.Read(f.nonce[:]); err != nil {
		return nil, err
	}
	f.path = filepath.Join(s.dir, f.ID)
	fh, err := os.OpenFile(f.path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	c, err := chacha20.NewUnauthenticatedCipher(f.key[:], f.nonce[:])
	if err != nil {
		fh.Close()
		os.Remove(f.path)
		return nil, err
	}
	return &Writer{s: s, f: f, fh: fh, sw: cipher.StreamWriter{S: c, W: fh}}, nil
}

func (w *Writer) Write(p []byte) (int, error) {
	n, err := w.sw.Write(p)
	w.n += int64(n)
	return n, err
}

// Size is the number of bytes written so far.
func (w *Writer) Size() int64 { return w.n }

// Meta lets the caller attach details (row counts, formats) before Commit.
func (w *Writer) Meta() map[string]any { return w.f.Meta }

// Commit finishes the file and makes it available until it expires.
func (w *Writer) Commit() (*File, error) {
	if w.done {
		return nil, errors.New("spool file already finished")
	}
	w.done = true
	if err := w.fh.Close(); err != nil {
		os.Remove(w.f.path)
		return nil, err
	}
	w.f.Size = w.n
	w.f.Expires = time.Now().Add(w.s.ttl)
	w.s.mu.Lock()
	w.s.files[w.f.ID] = w.f
	w.s.mu.Unlock()
	return w.f, nil
}

// Abort discards the file.
func (w *Writer) Abort() {
	if w.done {
		return
	}
	w.done = true
	w.fh.Close()
	os.Remove(w.f.path)
}

// Get returns the owner's file.
func (s *Spool) Get(id, owner string) (*File, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.files[id]
	if !ok || f.Owner != owner || time.Now().After(f.Expires) {
		return nil, ErrNotFound
	}
	return f, nil
}

// Remove deletes a file now.
func (s *Spool) Remove(id string) {
	s.mu.Lock()
	f, ok := s.files[id]
	delete(s.files, id)
	s.mu.Unlock()
	if ok {
		os.Remove(f.path)
	}
}

// Open returns a reader over the decrypted contents.
func (f *File) Open() (io.ReadCloser, error) {
	fh, err := os.Open(f.path)
	if err != nil {
		return nil, ErrNotFound
	}
	c, err := chacha20.NewUnauthenticatedCipher(f.key[:], f.nonce[:])
	if err != nil {
		fh.Close()
		return nil, err
	}
	return readCloser{Reader: cipher.StreamReader{S: c, R: fh}, c: fh}, nil
}

type readCloser struct {
	io.Reader
	c io.Closer
}

func (r readCloser) Close() error { return r.c.Close() }
