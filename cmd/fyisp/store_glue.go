package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/export"
	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/store"
)

// storeT is what main needs from the store.
type storeT = store.Store

// lockedError reports another running instance.
type lockedError struct{ Holder string }

func (e *lockedError) Error() string { return "already running: " + e.Holder }

const defaultProbeInterval = 15 * time.Second

// openStore opens the SQLite store. Series intervals must match the probe
// scheduler exactly: the target's interval (default 15s) and a third of it
// for ICMP.
func openStore(dir string, prof *model.Profile, log *slog.Logger) (*store.SQLite, error) {
	iv := map[string]time.Duration{}
	for _, t := range prof.Targets {
		d := t.Interval
		if d == 0 {
			d = defaultProbeInterval
		}
		iv[t.Name] = d
	}
	st, err := store.Open(dir, store.Options{
		Version: version,
		Log:     log,
		Interval: func(k model.SeriesKey) time.Duration {
			d, ok := iv[k.Target]
			if !ok {
				d = defaultProbeInterval
			}
			if k.Kind == model.KindICMP {
				d /= 3
			}
			return d
		},
	})
	var le *store.LockedError
	if errors.As(err, &le) {
		holder := fmt.Sprintf("pid %d", le.PID)
		if le.URL != "" {
			holder += ", dashboard " + le.URL
		}
		return nil, &lockedError{Holder: holder}
	}
	return st, err
}

// setLockURL records the dashboard URL in the lock file for `already running`.
func setLockURL(st *store.SQLite, url string) { st.SetURL(url) }

// cmdExport implements `fyisp export`: it reads the saved data (safe while
// fyisp runs; only data saved so far, at most a minute old, is visible).
func cmdExport(args []string) int {
	fs := flag.NewFlagSet("fyisp export", flag.ContinueOnError)
	dataDir := fs.String("data-dir", "", "data directory (default: per-OS state directory)")
	opts := export.Flags(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	o, err := opts(time.Now())
	if err != nil {
		fmt.Fprintln(os.Stderr, "fyisp export:", err)
		return 2
	}
	d := *dataDir
	if d == "" {
		if d, err = defaultDataDir(); err != nil {
			fmt.Fprintln(os.Stderr, "fyisp export:", err)
			return 1
		}
	}
	ro, err := store.OpenReadOnly(d)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fyisp export:", err)
		return 1
	}
	defer ro.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := export.Run(ctx, ro, o, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "fyisp export:", err)
		return 1
	}
	return 0
}
