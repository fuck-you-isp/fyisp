//go:build !(linux || darwin || freebsd || netbsd || openbsd || dragonfly || windows)

package store

import "os"

// No advisory locking on this platform: single-instance is not enforced.
func lockFile(*os.File) error   { return nil }
func unlockFile(*os.File) error { return nil }
