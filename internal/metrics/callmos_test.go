package metrics

import (
	"math"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/callq"
	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/prometheus/client_golang/prometheus"
)

func callMOSProfile() *model.Profile {
	udp := func(name, geo string, tags ...string) model.Target {
		return model.Target{Name: name, Host: name + ".example.com", Port: 3478, Kinds: []model.ProbeKind{model.KindUDP},
			Interval: 5 * time.Second, Geo: geo, Tags: append([]string{"udp-path"}, tags...)}
	}
	return &model.Profile{Targets: []model.Target{
		udp("Meet-STUN", "global", "app:meet"),
		udp("Cloudflare-STUN", "global"),
		udp("Twilio-jp1", "as"),
		udp("Teams-Relay", "global", "app:teams"),
		{Name: "Zoom", Host: "zoom.example.com", Tags: []string{"app:zoom"}},
	}}
}

// feedBurst feeds one burst's sibling samples: answered of 5, or a socket
// failure (reason).
func feedBurst(c *CallMOS, target string, ts time.Time, rtt float64, answered int, reason model.Reason) {
	key := func(k model.ProbeKind) model.SeriesKey { return model.SeriesKey{Target: target, Kind: k} }
	u, j, l := model.Sample{Key: key(model.KindUDP), Slot: ts}, model.Sample{Key: key(model.KindUDPJitter), Slot: ts}, model.Sample{Key: key(model.KindUDPLoss), Slot: ts}
	switch {
	case reason != 0:
		u.Lost, u.Reason, j.Lost, j.Reason, l.Lost, l.Reason = true, reason, true, reason, true, reason
	case answered == 0:
		u.Lost, u.Reason, j.Lost, j.Reason = true, model.ReasonTimeout, true, model.ReasonGap
		l.RTT = 100 * time.Millisecond
	default:
		u.RTT = time.Duration(rtt * float64(time.Millisecond))
		j.RTT = 2 * time.Millisecond
		l.RTT = time.Duration((5-answered)*20) * time.Millisecond
	}
	for _, s := range []model.Sample{u, j, l} {
		c.Observe(s)
	}
}

func gatherMOS(t *testing.T, c *CallMOS) map[string]float64 {
	t.Helper()
	reg := prometheus.NewRegistry()
	reg.MustRegister(c)
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]float64{}
	for _, mf := range mfs {
		if mf.GetName() != "fyisp_call_mos" {
			t.Errorf("unexpected metric %s", mf.GetName())
		}
		for _, m := range mf.GetMetric() {
			out[m.GetLabel()[0].GetValue()] = m.GetGauge().GetValue()
		}
	}
	return out
}

func TestCallMOS(t *testing.T) {
	now := time.Date(2026, 10, 10, 14, 0, 30, 0, time.UTC)
	p := callMOSProfile()
	c := NewCallMOS(func() *model.Profile { return p })
	c.now = func() time.Time { return now }
	// Nothing yet: nothing exported.
	if got := gatherMOS(t, c); len(got) != 0 {
		t.Errorf("empty: %v", got)
	}
	start := now.Add(-20 * time.Minute)
	for ts := start; ts.Before(now); ts = ts.Add(5 * time.Second) {
		feedBurst(c, "Meet-STUN", ts, 20, 5, 0)
		feedBurst(c, "Cloudflare-STUN", ts, 10, 5, 0)
		feedBurst(c, "Twilio-jp1", ts, 260, 5, 0) // far away: not the path
		feedBurst(c, "Teams-Relay", ts, 0, 0, 0)  // never answers: left out
		// A lossy minute 3 minutes ago on the path (both servers).
		if m := now.Truncate(time.Minute).Add(-3 * time.Minute); !ts.Before(m) && ts.Before(m.Add(time.Minute)) {
			feedBurst(c, "Meet-STUN", ts, 20, 4, 0)
			feedBurst(c, "Cloudflare-STUN", ts, 10, 4, 0)
		}
	}
	c.Observe(model.Sample{Key: model.SeriesKey{Target: "Zoom", Kind: model.KindTCP}, Slot: now, RTT: time.Millisecond})
	got := gatherMOS(t, c)
	if _, ok := got["teams"]; ok || len(got) != 2 {
		t.Errorf("exported %v, want path and meet only", got)
	}
	// The worst minute (20% loss) sets the score: unusable, not a 5-minute mean.
	if v := got["path"]; v > 3.1 || v < 1 {
		t.Errorf("path MOS %v, want the lossy minute's", v)
	}
	if v := got["meet"]; math.Abs(v-got["path"]) > 0.2 {
		t.Errorf("meet MOS %v vs path %v", v, got["path"])
	}
	// Older bursts are pruned.
	c.mu.Lock()
	for target, m := range c.bursts {
		for slot := range m {
			if slot < now.Add(-CallMOSWindow-time.Minute-31*time.Second).UnixMilli() {
				t.Errorf("%s: burst at %v kept", target, time.UnixMilli(slot))
				break
			}
		}
	}
	c.mu.Unlock()

	// Five clean minutes later the path is good again; unknown failures
	// (refused) are not loss.
	now = now.Add(10 * time.Minute)
	for ts := now.Add(-6 * time.Minute); ts.Before(now); ts = ts.Add(5 * time.Second) {
		feedBurst(c, "Meet-STUN", ts, 20, 5, 0)
		feedBurst(c, "Cloudflare-STUN", ts, 10, 5, 0)
		if ts.Unix()%15 == 0 {
			feedBurst(c, "Cloudflare-STUN", ts.Add(time.Second), 0, 0, model.ReasonRefused)
		}
	}
	got = gatherMOS(t, c)
	if v := got["path"]; v < 4.3 {
		t.Errorf("clean path MOS %v", v)
	}
	// Too few bursts: absent.
	c2 := NewCallMOS(func() *model.Profile { return p })
	c2.now = func() time.Time { return now }
	feedBurst(c2, "Meet-STUN", now.Add(-time.Second), 20, 5, 0)
	if got := gatherMOS(t, c2); len(got) != 0 {
		t.Errorf("one burst: %v", got)
	}
}

// Jitter caps the score as it caps the Calls band: 35 ms of jitter on an
// otherwise clean, short path is at most Fair (3.9).
func TestCallMOSJitterCap(t *testing.T) {
	now := time.Date(2026, 10, 10, 14, 0, 30, 0, time.UTC)
	p := callMOSProfile()
	c := NewCallMOS(func() *model.Profile { return p })
	c.now = func() time.Time { return now }
	for ts := now.Add(-6 * time.Minute); ts.Before(now); ts = ts.Add(5 * time.Second) {
		for _, target := range []string{"Meet-STUN", "Cloudflare-STUN"} {
			key := func(k model.ProbeKind) model.SeriesKey { return model.SeriesKey{Target: target, Kind: k} }
			c.Observe(model.Sample{Key: key(model.KindUDP), Slot: ts, RTT: 20 * time.Millisecond})
			c.Observe(model.Sample{Key: key(model.KindUDPJitter), Slot: ts, RTT: 35 * time.Millisecond})
			c.Observe(model.Sample{Key: key(model.KindUDPLoss), Slot: ts})
		}
	}
	got := gatherMOS(t, c)
	if v, want := got["path"], callq.GoodMOS-callq.CapMargin; math.Abs(v-want) > 1e-9 || callq.BandOf(v) != callq.Fair {
		t.Errorf("path MOS %v under 35 ms of jitter, want %v (Fair)", v, want)
	}
}
