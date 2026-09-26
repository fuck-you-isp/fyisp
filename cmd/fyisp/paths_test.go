package main

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestDataDirPermissions: a --data-dir that exists keeps its mode (fyisp
// warns if others can write to it); a directory fyisp creates is 0700.
func TestDataDirPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no mode bits")
	}
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(old)

	shared := filepath.Join(t.TempDir(), "shared")
	if err := os.Mkdir(shared, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(shared, 0o775); err != nil {
		t.Fatal(err)
	}
	d, _, err := resolveDataDir(config{dataDir: shared})
	if err != nil || d != shared {
		t.Fatal(d, err)
	}
	if fi, _ := os.Stat(shared); fi.Mode().Perm() != 0o775 {
		t.Errorf("existing --data-dir changed to %v", fi.Mode().Perm())
	}
	if !strings.Contains(logs.String(), "writable by group") {
		t.Errorf("no warning for a group-writable data directory: %q", logs.String())
	}

	logs.Reset()
	fresh := filepath.Join(t.TempDir(), "a", "fyisp")
	if _, _, err := resolveDataDir(config{dataDir: fresh}); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(fresh); fi.Mode().Perm() != 0o700 {
		t.Errorf("created data directory is %v, want 0700", fi.Mode().Perm())
	}
	if logs.Len() != 0 {
		t.Errorf("unexpected warning: %q", logs.String())
	}
}
