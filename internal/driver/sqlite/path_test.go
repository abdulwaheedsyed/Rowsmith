package sqlite

import (
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestResolve(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "sqlite")
	outside := filepath.Join(base, "outside")
	for _, d := range []string{filepath.Join(dir, "sub"), outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	touch := func(p string) {
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	link := func(target, name string) {
		if err := os.Symlink(target, filepath.Join(dir, name)); err != nil {
			if runtime.GOOS == "windows" {
				t.Skip("creating symbolic links needs Developer Mode or an administrator on Windows:", err)
			}
			t.Fatal(err)
		}
	}
	touch(filepath.Join(dir, "sub", "ok.db"))
	touch(filepath.Join(dir, "rowsmith.db"))
	touch(filepath.Join(dir, "app.db-wal"))
	touch(filepath.Join(outside, "secret.db"))
	link(filepath.Join(outside, "secret.db"), "escape.db")
	link(filepath.Join(outside, "missing.db"), "dangling.db")
	link(outside, "linkdir")
	link(filepath.Join(dir, "sub", "ok.db"), "alias.db")
	link("../outside/secret.db", "relative.db")
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		create bool
		want   string // resolved path relative to dir, or "" when an error is expected
		err    string
	}{
		{name: "sub/ok.db", want: "sub/ok.db"},
		{name: "./sub//ok.db", want: "sub/ok.db"},
		{name: "alias.db", want: "sub/ok.db"},
		{name: "new.db", create: true, want: "new.db"},
		{name: "sub/new.db", create: true, want: "sub/new.db"},
		{name: "", err: "required"},
		{name: "/etc/passwd", err: "absolute"},
		{name: filepath.Join(dir, "sub", "ok.db"), err: "absolute"},
		{name: "../outside/secret.db", err: `".."`},
		{name: "sub/../../outside/secret.db", err: `".."`},
		{name: "sub/../sub/ok.db", err: `".."`},
		{name: "ok\x00.db", err: "NUL"},
		{name: "escape.db", err: "inside the SQLite directory"},
		{name: "relative.db", err: "inside the SQLite directory"},
		{name: "linkdir/secret.db", err: "inside the SQLite directory"},
		{name: "linkdir/new.db", create: true, err: "inside the SQLite directory"},
		{name: "dangling.db", create: true, err: "broken symbolic link"},
		{name: "rowsmith.db", err: "metadata database"},
		{name: "ROWSMITH.DB", create: true, err: "metadata database"},
		{name: "sub/rowsmith.db-shm", create: true, err: "metadata database"},
		{name: "app.db-wal", err: "journal file"},
		{name: "missing.db", err: "does not exist"},
		{name: "nodir/new.db", create: true, err: "folder nodir does not exist"},
		{name: "sub", err: "not a regular file"},
		{name: ".", err: "not a regular file"},
	}
	for _, tt := range tests {
		got, err := resolve(dir, tt.name, tt.create)
		if tt.err != "" {
			if err == nil || !strings.Contains(err.Error(), tt.err) {
				t.Errorf("resolve(%q) = %q, %v; want error containing %q", tt.name, got, err, tt.err)
			}
			continue
		}
		if want := filepath.Join(realDir, tt.want); err != nil || got != want {
			t.Errorf("resolve(%q) = %q, %v; want %q", tt.name, got, err, want)
		}
	}
}

func TestResolveDriveNames(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("only Windows has drive names")
	}
	dir := t.TempDir()
	for _, name := range []string{`\Windows\win.ini`, "C:a.db", `\\server\share\a.db`} {
		if _, err := resolve(dir, name, true); err == nil || !strings.Contains(err.Error(), "absolute") {
			t.Errorf("resolve(%q): %v; want it refused as absolute", name, err)
		}
	}
}

func TestFileURI(t *testing.T) {
	path, want := "/data/sqlite/my shop.db", "file:///data/sqlite/my%20shop.db?mode=rw"
	if runtime.GOOS == "windows" {
		path, want = `C:\data\sqlite\my shop.db`, "file:///C:/data/sqlite/my%20shop.db?mode=rw"
	}
	if got := fileURI(path, url.Values{"mode": {"rw"}}); got != want {
		t.Errorf("fileURI(%q) = %q, want %q", path, got, want)
	}
}

func TestResolveRoot(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "missing")
	if _, err := resolve(dir, "a.db", false); err == nil || !strings.Contains(err.Error(), "ROWSMITH_SQLITE_DIR") {
		t.Fatalf("missing directory: %v", err)
	}
	got, err := resolve(dir, "a.db", true)
	if err != nil || filepath.Base(got) != "a.db" {
		t.Fatalf("create in missing directory: %q, %v", got, err)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Fatalf("directory not created: %v", err)
	}

	t.Setenv("ROWSMITH_SQLITE_DIR", "")
	t.Setenv("ROWSMITH_DATA_DIR", "/srv/rowsmith")
	if got := root(); got != filepath.Join("/srv/rowsmith", "sqlite") {
		t.Errorf("root() = %q", got)
	}
	t.Setenv("ROWSMITH_DATA_DIR", "")
	if got := root(); got != filepath.Join("/data", "sqlite") {
		t.Errorf("root() = %q", got)
	}
	t.Setenv("ROWSMITH_SQLITE_DIR", "/files")
	if got := root(); got != "/files" {
		t.Errorf("root() = %q", got)
	}
}
