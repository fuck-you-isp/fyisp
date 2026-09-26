package store

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

// TestLockChild is the lock holder for TestLock (a re-exec of the test
// binary): it opens the store, sets its URL, prints "locked" and holds the
// lock until stdin closes.
func TestLockChild(t *testing.T) {
	dir := os.Getenv("FYISP_LOCK_CHILD")
	if dir == "" {
		t.Skip("child process only")
	}
	s, err := Open(dir, Options{URL: "http://first"})
	if err != nil {
		fmt.Println("error", err)
		os.Exit(1)
	}
	if err := s.SetURL("http://127.0.0.1:3000"); err != nil {
		fmt.Println("error", err)
		os.Exit(1)
	}
	fmt.Println("locked")
	bufio.NewReader(os.Stdin).ReadString('\n')
	s.Close()
	os.Exit(0)
}

func startLockChild(t *testing.T, dir string) (*exec.Cmd, func()) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestLockChild$")
	cmd.Env = append(os.Environ(), "FYISP_LOCK_CHILD="+dir)
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(stdout)
	for sc.Scan() {
		if sc.Text() == "locked" {
			return cmd, func() { stdin.Close(); cmd.Wait() }
		}
		if strings.HasPrefix(sc.Text(), "error") {
			t.Fatal(sc.Text())
		}
	}
	cmd.Process.Kill()
	cmd.Wait()
	t.Fatal("lock child exited")
	return nil, nil
}

func TestLock(t *testing.T) {
	dir := t.TempDir()
	cmd, stop := startLockChild(t, dir)
	_, err := Open(dir, Options{})
	var le *LockedError
	if !errors.Is(err, ErrLocked) || !errors.As(err, &le) {
		stop()
		t.Fatalf("second Open: %v", err)
	}
	t.Logf("second Open: %v", err)
	if le.PID != cmd.Process.Pid || le.URL != "http://127.0.0.1:3000" {
		stop()
		t.Fatalf("lock info %+v, want pid %d", le, cmd.Process.Pid)
	}
	// A read-only open (fyisp export) works while the lock is held.
	ro, err := OpenReadOnly(dir)
	if err != nil {
		stop()
		t.Fatal(err)
	}
	if _, err := ro.Series(ctx); err != nil {
		t.Fatal(err)
	}
	ro.Observe(model.Sample{Key: model.SeriesKey{Target: "x", Kind: 1}, Slot: t0})
	if ro.Flush(ctx) == nil {
		t.Fatal("read-only store flushed")
	}
	ro.Close()
	stop()

	// Released on clean exit...
	s := openT(t, dir, Options{})
	s.Close()
	// ...and when the holder is killed.
	cmd, stop = startLockChild(t, dir)
	cmd.Process.Kill()
	stop()
	deadline := time.Now().Add(5 * time.Second)
	for {
		s, err := Open(dir, Options{})
		if err == nil {
			s.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("lock not released after kill: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, LockName)); len(b) != 0 {
		t.Fatalf("lock file not cleared on Close: %q", b)
	}
}
