package main

import (
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// defaultDataDir is where history lives unless --data-dir or --ephemeral.
func defaultDataDir() (string, error) {
	if d := os.Getenv("STATE_DIRECTORY"); d != "" { // systemd StateDirectory=
		return d, nil
	}
	switch runtime.GOOS {
	case "windows":
		if d := os.Getenv("LOCALAPPDATA"); d != "" {
			return filepath.Join(d, "fyisp"), nil
		}
	case "darwin":
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, "Library", "Application Support", "fyisp"), nil
		}
	default:
		if d := os.Getenv("XDG_STATE_HOME"); d != "" {
			return filepath.Join(d, "fyisp"), nil
		}
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, ".local", "state", "fyisp"), nil
		}
	}
	return "", errors.New("cannot determine a data directory; pass --data-dir")
}

// resolveDataDir creates the data directory (0700) and returns a cleanup func
// that removes it only in --ephemeral mode. The permissions of a --data-dir
// that already exists are the user's choice and are not changed; fyisp
// warns if it is writable by group or others (the database files themselves
// are always 0600).
func resolveDataDir(c config) (string, func(), error) {
	noop := func() {}
	if c.ephemeral {
		d, err := os.MkdirTemp("", "fyisp-")
		if err != nil {
			return "", noop, err
		}
		return d, func() { os.RemoveAll(d) }, nil
	}
	d := c.dataDir
	own := d == "" // fyisp's default directory: always kept private
	if own {
		var err error
		if d, err = defaultDataDir(); err != nil {
			return "", noop, err
		}
	}
	if _, err := os.Stat(d); errors.Is(err, fs.ErrNotExist) {
		own = true // created below
	}
	if err := os.MkdirAll(d, 0o700); err != nil {
		return "", noop, fmt.Errorf("creating data directory: %w", err)
	}
	if own {
		_ = os.Chmod(d, 0o700)
	} else if w := writableByOthers(d); w != "" {
		slog.Warn("data directory is writable by "+w+"; consider chmod 700", "dir", d)
	}
	return d, noop, nil
}

// writableByOthers returns "group", "others" or "group and others" when dir
// is writable by them, else "" (always "" on Windows, which has no mode bits).
func writableByOthers(dir string) string {
	fi, err := os.Stat(dir)
	if err != nil || runtime.GOOS == "windows" {
		return ""
	}
	switch m := fi.Mode().Perm(); {
	case m&0o022 == 0o022:
		return "group and others"
	case m&0o020 != 0:
		return "group"
	case m&0o002 != 0:
		return "others"
	}
	return ""
}

func checkFreeSpace(dir string, need uint64) error {
	free, err := freeBytes(dir)
	if err != nil || free >= need {
		return nil // unknown: don't block startup
	}
	return fmt.Errorf("only %d MB free in %s (need 200 MB); free some space or pass --force", free>>20, dir)
}

func cmdPaths(args []string) int {
	fs := flag.NewFlagSet("fyisp paths", flag.ContinueOnError)
	dataDir := fs.String("data-dir", "", "data directory override")
	if fs.Parse(args) != nil {
		return 2
	}
	d := *dataDir
	if d == "" {
		var err error
		if d, err = defaultDataDir(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}
	fmt.Printf("data directory: %s\n  database:     %s\n  lock file:    %s\n", d, filepath.Join(d, "fyisp.db"), filepath.Join(d, "fyisp.lock"))
	fmt.Println("To uninstall: stop fyisp, delete the data directory above, and delete the fyisp binary.")
	return 0
}

// isTerminal reports whether f is an interactive terminal.
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// openBrowser opens url in the default browser, best effort.
func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
			return
		}
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}
