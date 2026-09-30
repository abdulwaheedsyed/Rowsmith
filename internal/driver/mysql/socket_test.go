package mysql

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestCheckSocket(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "mysqld.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Skip("unix sockets unavailable:", err)
	}
	defer l.Close()
	file := filepath.Join(dir, "plain")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := checkSocket(sock); err != nil {
		t.Errorf("real socket rejected: %v", err)
	}
	for _, p := range []string{file, dir, filepath.Join(dir, "missing.sock"), dir + "/./mysqld.sock", dir + "//mysqld.sock"} {
		if checkSocket(p) == nil {
			t.Errorf("%s accepted", p)
		}
	}
}
