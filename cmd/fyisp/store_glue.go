package main

import (
	"log/slog"

	"github.com/fuck-you-isp/fyisp/internal/store"
)

// storeT is what main needs from the store.
type storeT = store.Store

// lockedError reports another running instance.
type lockedError struct{ Holder string }

func (e *lockedError) Error() string { return "already running: " + e.Holder }

// openStore opens the data store. TEMPORARY: in-memory until the SQLite
// store (feat/store) is merged.
func openStore(dir string, log *slog.Logger) (storeT, error) {
	log.Warn("using the in-memory store; history is not saved yet", "dir", dir)
	return store.NewFake(), nil
}

// setLockURL records the dashboard URL in the lock file for `already running`.
func setLockURL(st storeT, url string) {}

func cmdExport(args []string) int {
	println("fyisp export: not available in this build yet")
	return 1
}
