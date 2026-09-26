//go:build windows

package store

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// The locked byte sits at offset 4 GiB, far past the JSON content: Windows
// byte-range locks are mandatory, and a lock over the content would stop a
// second instance from reading the holder's PID and URL.
func lockOverlapped() *windows.Overlapped { return &windows.Overlapped{OffsetHigh: 1} }

func lockFile(f *os.File) error {
	err := windows.LockFileEx(windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, lockOverlapped())
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_IO_PENDING) {
		return errWouldBlock
	}
	return err
}

func unlockFile(f *os.File) error {
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, lockOverlapped())
}
