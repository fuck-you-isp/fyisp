//go:build !windows

package storage

import (
	"fmt"
	"os"
	"syscall"
)

// createPrivate creates path for writing, exclusively, readable by this
// user only.
func createPrivate(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
}

// openOwn opens path for reading without following a symlink, refuses a
// file another user owns, and takes the group's and others' access away
// from fyisp's own (a copy restored with wider permissions). The checks
// are on the open file, so it can't be swapped in between.
func openOwn(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	switch {
	case err != nil:
	case !fi.Mode().IsRegular():
		err = fmt.Errorf("%s is not a regular file", path)
	case fi.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()):
		err = fmt.Errorf("%s belongs to another user", path)
	case fi.Mode().Perm()&0o077 != 0:
		err = f.Chmod(0o600)
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
}
