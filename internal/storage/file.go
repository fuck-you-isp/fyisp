package storage

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// FileName is the targets file in the data directory.
const FileName = "storage-targets.json"

// fileVersion is the file format's version.
const fileVersion = 1

const maxFile = 1 << 20

// file is the targets file: the targets, and the keys recurring campaigns
// keep (deleted when their schedule ends). Only fyisp's user can read it.
type file struct {
	Version int        `json:"version"`
	Targets []Target   `json:"targets"`
	Keys    []SavedKey `json:"keys"`
}

// SavedKey is a recurring campaign's keys for one target, bound to the
// target's settings when saved (a change to them deletes the keys).
type SavedKey struct {
	Target       string `json:"target"`
	Campaign     int64  `json:"campaign"`
	Binding      string `json:"binding"`
	AccessKey    string `json:"access_key"`
	Secret       string `json:"secret"`
	SessionToken string `json:"session_token"`
	SavedMs      int64  `json:"saved_ms"`
}

// SavedKey prints as [redacted] in every fmt verb and in logs (only the
// targets file holds its secret).
func (SavedKey) Format(f fmt.State, verb rune) { _, _ = f.Write([]byte("[redacted]")) }
func (SavedKey) LogValue() slog.Value          { return slog.StringValue("[redacted]") }

// ErrFile is a targets file fyisp won't use (and never overwrites): it is
// unreadable, not JSON, of an unknown version, a symlink, too large,
// another user's, or holds a target fyisp wouldn't save.
var ErrFile = errors.New("storage: the targets file can't be used")

// loadFile reads path; a missing file is an empty one.
func loadFile(path string) (*file, error) {
	fi, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return &file{Version: fileVersion}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrFile, err)
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: %s is not a regular file", ErrFile, path)
	}
	r, err := openOwn(path)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrFile, err)
	}
	b, err := io.ReadAll(io.LimitReader(r, maxFile+1))
	r.Close()
	switch {
	case err != nil:
		return nil, fmt.Errorf("%w: %v", ErrFile, err)
	case len(b) > maxFile:
		return nil, fmt.Errorf("%w: %s is over 1 MiB", ErrFile, path)
	}
	var f file
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("%w: %s is not valid JSON", ErrFile, path)
	}
	if f.Version != fileVersion {
		return nil, fmt.Errorf("%w: %s has version %d, this build reads %d", ErrFile, path, f.Version, fileVersion)
	}
	if len(f.Targets) > MaxTargets {
		return nil, fmt.Errorf("%w: %s has more than %d targets", ErrFile, path, MaxTargets)
	}
	ids, names := map[string]bool{}, map[string]bool{}
	for _, t := range f.Targets {
		if err := t.valid(); err != nil {
			return nil, fmt.Errorf("%w: %s: target %q: %v", ErrFile, path, t.ID, err)
		}
		name := strings.ToLower(t.Name)
		if ids[t.ID] || names[name] {
			return nil, fmt.Errorf("%w: %s has two targets with the id %q or the name %q", ErrFile, path, t.ID, t.Name)
		}
		ids[t.ID], names[name] = true, true
	}
	return &f, nil
}

// saveFile writes f to path atomically: a new file only fyisp's user can
// read (created exclusively), synced, renamed over the old one, and the
// directory synced.
func saveFile(path string, f *file) error {
	f.Version = fileVersion
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	// A crash's leftovers (removing a planted symlink removes the link).
	if old, err := filepath.Glob(filepath.Join(dir, "."+FileName+".*.tmp")); err == nil {
		for _, o := range old {
			_ = os.Remove(o)
		}
	}
	var rnd [8]byte
	_, _ = rand.Read(rnd[:])
	tmp := filepath.Join(dir, "."+FileName+"."+hex.EncodeToString(rnd[:])+".tmp")
	w, err := createPrivate(tmp)
	if err != nil {
		return err
	}
	if _, err := w.Write(append(b, '\n')); err != nil {
		w.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := w.Sync(); err != nil {
		w.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := w.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	syncDir(dir)
	return nil
}
