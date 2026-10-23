//go:build !windows

package storage

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestFileModes: the file is created 0600, a copy restored with wider
// permissions is narrowed, a crash's temporary file is removed, and a
// symlink is never followed.
func TestFileModes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	if err := saveFile(path, &file{}); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", fi.Mode())
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if s := Open(Options{Dir: dir}); s.Err() != nil {
		t.Fatal(s.Err())
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode after open %v", fi.Mode())
	}
	if es, _ := os.ReadDir(dir); len(es) != 1 {
		t.Errorf("dir: %v", es)
	}
	// A leftover temporary file from a crash is removed.
	if err := os.WriteFile(filepath.Join(dir, "."+FileName+".0123456789abcdef.tmp"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := saveFile(path, &file{}); err != nil {
		t.Fatal(err)
	}
	if es, _ := os.ReadDir(dir); len(es) != 1 {
		t.Errorf("dir after a save: %v", es)
	}
	// A symlink is never followed.
	other := filepath.Join(dir, "other.json")
	if err := os.WriteFile(other, []byte(`{"version":1,"targets":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := openOwn(filepath.Join(dir, "link")); err == nil {
		t.Error("opened a missing file")
	}
	if err := os.Symlink(other, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if f, err := openOwn(filepath.Join(dir, "link")); err == nil {
		f.Close()
		t.Error("openOwn followed a symlink")
	}
	if fi, _ := os.Stat(other); fi.Mode().Perm() != 0o644 {
		t.Errorf("the symlink's target was changed to %v", fi.Mode())
	}
	if _, err := loadFile(path); errors.Is(err, ErrFile) {
		t.Error(err)
	}
}
