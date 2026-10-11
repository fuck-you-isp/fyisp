package web

import (
	"testing"

	"github.com/fuck-you-isp/fyisp/internal/speedtest"
)

// TestJobStatus: a sweep's servers while it runs, and after it stopped
// early (unreached servers are skipped, never left waiting).
func TestJobStatus(t *testing.T) {
	for _, c := range []struct {
		state string
		ran   int
		step  int
		want  []string
	}{
		{speedtest.StateWaiting, 0, 0, []string{"waiting", "waiting", "waiting", "waiting"}},
		{speedtest.StateRunning, 1, 2, []string{"skipped", "testing", "waiting", "waiting"}},
		{speedtest.StateDone, 2, 2, []string{"skipped", "skipped", "skipped", "skipped"}},
		{speedtest.StateCancelled, 1, 2, []string{"skipped", "skipped", "skipped", "skipped"}},
		{speedtest.StateCancelled, 2, 2, []string{"skipped", "cancelled", "skipped", "skipped"}}, // cut mid-test (Cut 2)
	} {
		e := speedtest.Entry{State: c.state, Ran: c.ran, Step: c.step, Steps: 4}
		if c.ran == 2 && c.state == speedtest.StateCancelled {
			e.Cut = 2
		}
		for i, want := range c.want {
			if got := jobStatus(e, i); got != want {
				t.Errorf("%s ran %d: job %d is %q, want %q", c.state, c.ran, i, got, want)
			}
		}
	}
}
