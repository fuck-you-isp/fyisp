package verdict

import (
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/speedtest"
)

// TestSpeedTestMasked: a speed test fills the link, so for 40 seconds every
// probe is slow and lossy, the router's included. Through the speed test
// mask (as cmd/fyisp wires it) that never becomes a verdict or an incident;
// without it, the same window does.
func TestSpeedTestMasked(t *testing.T) {
	start := t0.Add(4 * time.Minute)
	end := start.Add(40 * time.Second)
	loaded := func(_ model.Target, _ model.ProbeKind, at time.Time) (outcome, bool) {
		if at.Before(start) || at.After(end) {
			return outcome{}, false
		}
		if at.Unix()%3 != 0 {
			return outcome{lost: true, reason: model.ReasonTimeout}, true
		}
		return outcome{rtt: 400 * time.Millisecond}, true
	}
	for _, masked := range []bool{true, false} {
		h := newHarness(t)
		h.with(loaded)
		if masked {
			w := &speedtest.Windows{}
			w.Add(speedtest.Window{Start: start, End: end})
			h.sink = speedtest.Mask{Inner: model.SinkFunc(h.e.Observe), Windows: w,
				Interval: func(k model.SeriesKey) time.Duration {
					if k.Kind == model.KindICMP {
						return 5 * time.Second
					}
					return 15 * time.Second
				}}
		}
		h.run(8 * time.Minute)
		bad := false
		for k := range h.seen {
			bad = bad || (k != model.VerdictOK && k != model.VerdictWarmingUp)
		}
		switch {
		case masked && (bad || len(h.st.all()) != 0):
			t.Errorf("masked: verdicts %v, incidents %+v", h.seen, h.st.all())
		case !masked && (!bad || len(h.st.all()) == 0):
			t.Errorf("unmasked: the loaded window raised nothing (verdicts %v): the test proves nothing", h.seen)
		}
	}
}
