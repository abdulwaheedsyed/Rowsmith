package spool

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRoundTripEncryptedAndOwned(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "spool")
	s, err := New(dir, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	w, err := s.Create("u1", "rows.csv", "text/csv")
	if err != nil {
		t.Fatal(err)
	}
	secret := bytes.Repeat([]byte("id,email\n1,ada@example.com\n"), 1000)
	w.Write(secret)
	f, err := w.Commit()
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, f.ID))
	if bytes.Contains(raw, []byte("ada@example.com")) || len(raw) != len(secret) {
		t.Fatal("spooled bytes are not encrypted")
	}
	if _, err := s.Get(f.ID, "u2"); err != ErrNotFound {
		t.Fatal("another user could read the file")
	}
	g, err := s.Get(f.ID, "u1")
	if err != nil {
		t.Fatal(err)
	}
	r, _ := g.Open()
	got, _ := io.ReadAll(r)
	r.Close()
	if !bytes.Equal(got, secret) || g.Size != int64(len(secret)) {
		t.Fatal("decrypted contents differ")
	}
	s.Remove(f.ID)
	if _, err := os.Stat(filepath.Join(dir, f.ID)); !os.IsNotExist(err) {
		t.Fatal("removed file still on disk")
	}
}

func TestAbortRemovesFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "spool")
	s, _ := New(dir, time.Minute)
	w, _ := s.Create("u1", "x", "")
	w.Write([]byte("partial"))
	w.Abort()
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("aborted file left behind: %v", entries)
	}
}
