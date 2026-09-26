package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

// ErrLocked means another process holds the data directory. The error
// returned by Open is a *LockedError, which matches ErrLocked with errors.Is.
var ErrLocked = errors.New("store: data directory is in use by another fyisp")

// LockedError describes the process holding the lock, as it recorded itself
// in the lock file (PID and URL may be empty while it is starting).
type LockedError struct {
	Path string
	PID  int
	URL  string
}

func (e *LockedError) Error() string {
	s := fmt.Sprintf("%v (%s", ErrLocked, e.Path)
	if e.PID != 0 {
		s += fmt.Sprintf(", pid %d", e.PID)
	}
	if e.URL != "" {
		s += ", " + e.URL
	}
	return s + ")"
}

func (e *LockedError) Is(target error) bool { return target == ErrLocked }

// lockInfo is the lock file content.
type lockInfo struct {
	PID int    `json:"pid"`
	URL string `json:"url,omitempty"`
}

// errWouldBlock is returned by lockFile when another process holds the lock.
var errWouldBlock = errors.New("lock held")

type dirLock struct {
	path string
	f    *os.File
}

// acquireLock takes the single-instance lock at path without blocking. The
// lock is held on the open file (flock / LockFileEx), so it is released by
// the OS when the process dies, however it dies.
func acquireLock(path string) (*dirLock, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := lockFile(f); err != nil {
		defer f.Close()
		if errors.Is(err, errWouldBlock) {
			le := &LockedError{Path: path}
			if b, _ := io.ReadAll(io.LimitReader(f, 4096)); len(b) > 0 {
				var li lockInfo
				if json.Unmarshal(b, &li) == nil {
					le.PID, le.URL = li.PID, li.URL
				}
			}
			return nil, le
		}
		return nil, fmt.Errorf("store: lock %s: %w", path, err)
	}
	return &dirLock{path: path, f: f}, nil
}

// write records the holder's PID and URL in the lock file.
func (l *dirLock) write(url string) error {
	b, _ := json.Marshal(lockInfo{PID: os.Getpid(), URL: url})
	if err := l.f.Truncate(0); err != nil {
		return err
	}
	if _, err := l.f.WriteAt(append(b, '\n'), 0); err != nil {
		return err
	}
	return nil
}

// release clears the lock file and releases the lock. The file itself stays:
// removing a flock'ed file races with a concurrent opener.
func (l *dirLock) release() error {
	l.f.Truncate(0)
	return errors.Join(unlockFile(l.f), l.f.Close())
}
