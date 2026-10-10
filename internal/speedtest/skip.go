package speedtest

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Servers can be left out: by the user (--speedtest-skip, or the Details
// toggle: an operator asked, or the user prefers not to), never contacted
// again until un-skipped; and by the busy backoff: a server that was busy
// BusyRun scheduled tests in a row rests for BusyRest (its operator's
// other users come first). Skipped servers are left out of every chain,
// the nearby picks, sweeps and Test now; resting ones only of scheduled
// chains and the picks.
const (
	BusyRun  = 3
	BusyRest = 6 * time.Hour
	// settingSkip is the saved skip list (comma-separated server IDs).
	settingSkip = "speedtest_skip"
)

// ErrSkipped is a server the user skipped.
var ErrSkipped = errors.New("this server is skipped")

// skippedLocked reports a server the user skipped (s.mu held).
func (s *Scheduler) skippedLocked(id string) bool {
	return s.skip[id] || slices.Contains(s.o.Skip, id)
}

// usableLocked reports a server a scheduled test may use now (s.mu held).
func (s *Scheduler) usableLocked(id string) bool {
	return !s.skippedLocked(id) && !s.o.Now().Before(s.rest[id])
}

func (s *Scheduler) skipped(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.skippedLocked(id)
}

// filter keeps the steps a test may use: not skipped, and for scheduled
// tests not resting.
func (s *Scheduler) filter(steps []Step, scheduled bool) []Step {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Step
	for _, st := range steps {
		if s.skippedLocked(st.Server.ID) || (scheduled && !s.usableLocked(st.Server.ID)) {
			continue
		}
		out = append(out, st)
	}
	return out
}

// noteBusy counts a scheduled test's busy and measured servers for the
// backoff.
func (s *Scheduler) noteBusy(id string, busy bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !busy {
		delete(s.busyRun, id)
		return
	}
	s.busyRun[id]++
	if s.busyRun[id] >= BusyRun {
		s.rest[id] = s.o.Now().Add(BusyRest)
		delete(s.busyRun, id)
		s.o.Log.Info("speed test: a server was busy several times in a row; resting it", "server", id, "hours", int(BusyRest/time.Hour))
	}
}

// SetSkip replaces the user's saved skip list (the servers --speedtest-skip
// names stay skipped) and saves it.
func (s *Scheduler) SetSkip(ctx context.Context, ids []string) error {
	m := map[string]bool{}
	for _, id := range ids {
		if _, ok := s.server(id); !ok {
			return fmt.Errorf("%w: no server %q", ErrMode, id)
		}
		m[id] = true
	}
	if st := s.o.Store; st != nil {
		var list []string
		for id := range m {
			list = append(list, id)
		}
		slices.Sort(list)
		if err := st.SetSetting(ctx, settingSkip, strings.Join(list, ",")); err != nil {
			return err
		}
	}
	s.mu.Lock()
	s.skip = m
	s.mu.Unlock()
	return nil
}
