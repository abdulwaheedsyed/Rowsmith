package sqlite

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// root is the only directory SQLite files are opened from:
// ROWSMITH_SQLITE_DIR, or "sqlite" inside ROWSMITH_DATA_DIR (default /data).
func root() string {
	if dir := os.Getenv("ROWSMITH_SQLITE_DIR"); dir != "" {
		return dir
	}
	data := os.Getenv("ROWSMITH_DATA_DIR")
	if data == "" {
		data = "/data"
	}
	return filepath.Join(data, "sqlite")
}

// resolve maps a user-supplied relative file name to an absolute path inside
// dir. Symbolic links are followed before the containment check, so a link
// cannot lead outside dir; with create set, the file (and dir itself) may be
// missing, but its parent folder must exist inside dir.
func resolve(dir, name string, create bool) (string, error) {
	switch {
	case name == "":
		return "", errors.New("database file is required")
	case strings.ContainsRune(name, 0):
		return "", errors.New("the file name contains a NUL byte")
	case filepath.IsAbs(name):
		return "", errors.New("use a path relative to the SQLite directory, not an absolute path")
	}
	for _, part := range strings.FieldsFunc(name, func(r rune) bool { return r == '/' || r == filepath.Separator }) {
		if part == ".." {
			return "", errors.New(`the file name must not contain ".."`)
		}
	}

	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	if create {
		if err := os.MkdirAll(abs, 0o750); err != nil {
			return "", fmt.Errorf("cannot create the SQLite directory: %w", err)
		}
	}
	base, err := filepath.EvalSymlinks(abs)
	if errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("the SQLite directory %s does not exist (see ROWSMITH_SQLITE_DIR)", abs)
	}
	if err != nil {
		return "", err
	}

	full := filepath.Join(base, name)
	real, err := filepath.EvalSymlinks(full)
	switch {
	case err == nil:
		fi, err := os.Stat(real)
		if err != nil {
			return "", err
		}
		if !fi.Mode().IsRegular() {
			return "", fmt.Errorf("%s is not a regular file", name)
		}
	case errors.Is(err, fs.ErrNotExist):
		if _, lerr := os.Lstat(full); lerr == nil {
			// SQLite would create the link's target, wherever it points.
			return "", fmt.Errorf("%s is a broken symbolic link", name)
		}
		if !create {
			return "", fmt.Errorf("%s does not exist; enable \"Create if missing\" to create it", name)
		}
		parent, err := filepath.EvalSymlinks(filepath.Dir(full))
		if errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("folder %s does not exist", filepath.Dir(name))
		}
		if err != nil {
			return "", err
		}
		real = filepath.Join(parent, filepath.Base(full))
	default:
		return "", err
	}

	if !within(base, real) {
		return "", errors.New("the file must be inside the SQLite directory")
	}
	file := strings.ToLower(filepath.Base(real))
	switch {
	case file == "rowsmith.db" || strings.HasPrefix(file, "rowsmith.db-"):
		return "", errors.New("Rowsmith's own metadata database cannot be opened")
	case strings.HasSuffix(file, "-wal") || strings.HasSuffix(file, "-shm") || strings.HasSuffix(file, "-journal"):
		return "", fmt.Errorf("%s is a SQLite journal file, not a database", name)
	}
	return real, nil
}

// within reports whether path lies strictly inside dir.
func within(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil || filepath.IsAbs(rel) {
		return false
	}
	return rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
