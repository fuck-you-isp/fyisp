//go:build unix

package store

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func mode(path string) string {
	fi, err := os.Stat(path)
	if err != nil {
		return "missing"
	}
	return fmt.Sprintf("%04o", fi.Mode().Perm())
}

// TestFileModes: directory 0700, database, WAL, SHM and lock files 0600,
// even under a permissive umask.
func TestFileModes(t *testing.T) {
	old := syscall.Umask(0o022)
	defer syscall.Umask(old)
	dir := filepath.Join(t.TempDir(), "state", "fyisp")
	s := openT(t, dir, Options{})
	defer s.Close()
	s.Observe(fixtureSamples()[0])
	if err := s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, DBName)
	got := map[string]string{"dir": mode(dir), "db": mode(p), "wal": mode(p + "-wal"), "shm": mode(p + "-shm"), "lock": mode(filepath.Join(dir, LockName))}
	t.Logf("modes: %v", got)
	for k, v := range got {
		want := "0600"
		if k == "dir" {
			want = "0700"
		}
		if v != want {
			t.Errorf("%s mode %s, want %s", k, v, want)
		}
	}
}
