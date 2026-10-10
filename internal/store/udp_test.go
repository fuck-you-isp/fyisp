package store

import (
	"math/rand/v2"
	"slices"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

// udpBursts returns synthetic bursts of target every iv in [from, to): the
// three sibling samples per slot, as probe.burstSamples writes them (jitter
// not measured with fewer than 2 answers, loss in percent as milliseconds).
func udpBursts(target string, iv time.Duration, from, to time.Time, rnd *rand.Rand) []model.Sample {
	var out []model.Sample
	key := func(k model.ProbeKind) model.SeriesKey { return model.SeriesKey{Target: target, Kind: k} }
	for ts := from; ts.Before(to); ts = ts.Add(iv) {
		answered := 5
		switch r := rnd.IntN(40); {
		case r == 0:
			answered = 0
		case r == 1:
			answered = 1
		case r < 5:
			answered = 4
		}
		u := model.Sample{Key: key(model.KindUDP), Slot: ts}
		j := model.Sample{Key: key(model.KindUDPJitter), Slot: ts}
		if answered == 0 {
			u.Lost, u.Reason = true, model.ReasonTimeout
		} else {
			u.RTT = time.Duration(2_000+rnd.IntN(500)) * 10 * time.Microsecond // blob units
		}
		if answered < 2 {
			j.Lost, j.Reason = true, model.ReasonGap
		} else {
			j.RTT = time.Duration(rnd.IntN(300)) * 10 * time.Microsecond
		}
		l := model.Sample{Key: key(model.KindUDPLoss), Slot: ts, RTT: time.Duration((5-answered)*100/5) * time.Millisecond}
		out = append(out, u, j, l)
	}
	return out
}

func udpKeys(targets ...string) []model.SeriesKey {
	var out []model.SeriesKey
	for _, t := range targets {
		for _, k := range model.UDPKinds {
			out = append(out, model.SeriesKey{Target: t, Kind: k})
		}
	}
	return out
}

func udpInterval(k model.SeriesKey) time.Duration {
	if k.Target == "Twilio-jp1" {
		return 30 * time.Second
	}
	return 5 * time.Second
}

// TestUDPBursts writes bursts of a 5s and a 30s STUN target over two
// days: raw points, the raw tier and the hourly summaries (mean, max of
// jitter and loss) match the reference Fake, and jitter gaps are "not
// measured", never loss.
func TestUDPBursts(t *testing.T) {
	rnd := rand.New(rand.NewPCG(5, 6))
	end := t0.Add(50*time.Hour + 20*time.Minute) // past RawMaxRange: hourly summaries
	s := openT(t, t.TempDir(), Options{Interval: udpInterval, Now: func() time.Time { return end }})
	defer s.Close()
	f := NewFake()
	var all []model.Sample
	all = append(all, udpBursts("Meet-STUN", 5*time.Second, t0, end, rnd)...)
	all = append(all, udpBursts("Twilio-jp1", 30*time.Second, t0, end, rnd)...)
	for _, x := range all {
		s.Observe(x)
		f.Observe(x)
	}
	if err := s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	keys := udpKeys("Meet-STUN", "Twilio-jp1")
	equalRaw(t, "udp raw", rawAll(t, s, keys, t0, end), rawAll(t, f, keys, t0, end))
	comparePanels(t, "udp", s, f, keys, t0, end, rnd)

	// A whole-range hourly panel: loss is a value (percent), jitter gaps
	// are neither samples nor loss.
	p := panel(t, s, PanelQuery{Keys: keys[:3], From: t0, To: t0.Add(50 * time.Hour), MaxPoints: 50})
	if p.Tier != "1h" || len(p.Series[0].N) < 50 {
		t.Fatalf("tier %s, %d buckets", p.Tier, len(p.Series[0].N))
	}
	u, j, l := p.Series[0], p.Series[1], p.Series[2]
	for b := range 50 {
		if l.Lost[b] != 0 || j.Lost[b] != 0 || l.N[b] != 720 || u.N[b]+u.Lost[b] != 720 || j.N[b] >= l.N[b] {
			t.Errorf("hour %d: udp n=%d lost=%d, jitter n=%d lost=%d, loss n=%d lost=%d", b, u.N[b], u.Lost[b], j.N[b], j.Lost[b], l.N[b], l.Lost[b])
		}
		if l.Max[b] != 100 || l.Mean[b] <= 0 || l.Mean[b] > 10 || j.Max[b] > 3 || u.Min[b] < 20 {
			t.Errorf("hour %d: loss mean %v max %v, jitter max %v, rtt min %v", b, l.Mean[b], l.Max[b], j.Max[b], u.Min[b])
		}
	}
	for _, k := range keys {
		var iv time.Duration
		for _, si := range mustSeries(t, s) {
			if si.Key == k {
				iv = si.Interval
			}
		}
		if iv != udpInterval(k) {
			t.Errorf("%v: interval %v", k, iv)
		}
	}
}

func mustSeries(t *testing.T, s *SQLite) []SeriesInfo {
	t.Helper()
	ss, err := s.Series(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return ss
}

// TestFixtureWithUDP: a database from before UDP probing (the v3 fixture,
// upgraded on open to the format fyisp v0.3 and v0.4 write) takes the new
// kinds next to its old series, and both read back after a reopen.
func TestFixtureWithUDP(t *testing.T) {
	dir := fixtureDirV3(t)
	now := t0.Add(2*time.Hour + 30*time.Minute)
	s := openT(t, dir, Options{Interval: func(k model.SeriesKey) time.Duration {
		if k.Kind.IsUDP() {
			return 5 * time.Second
		}
		if k.Kind == model.KindICMP {
			return 5 * time.Second
		}
		return 15 * time.Second
	}, Now: func() time.Time { return now }})
	rnd := rand.New(rand.NewPCG(1, 1))
	bursts := udpBursts("Teams-Relay", 5*time.Second, t0.Add(2*time.Hour+20*time.Minute), now, rnd)
	for _, x := range bursts {
		s.Observe(x)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openT(t, dir, Options{Now: func() time.Time { return now }})
	defer s.Close()
	checkFixture(t, s)
	f := NewFake()
	for _, x := range append(fixtureSamples(), bursts...) {
		f.Observe(x)
	}
	keys := udpKeys("Teams-Relay")
	measured := func(ps []RawPoint) []RawPoint {
		return slices.DeleteFunc(ps, func(p RawPoint) bool { return p.Lost && p.Reason == model.ReasonGap })
	}
	got := measured(rawAll(t, s, keys, t0, now))
	if len(got) < len(bursts)*9/10 {
		t.Fatalf("%d measured UDP points of %d samples", len(got), len(bursts))
	}
	equalRaw(t, "udp after reopen", got, measured(rawAll(t, f, keys, t0, now)))
	q := PanelQuery{Keys: append(keys, model.SeriesKey{Target: "github", Kind: model.KindTCP}), From: t0.Add(2 * time.Hour), To: now, MaxPoints: 10}
	equalPanel(t, "mixed panel", panel(t, s, q), panel(t, f, q), 1e-6)
}

// TestFixtureIncidentsWithUDPKinds: a v0.4.x database (the v3 fixture,
// upgraded on open) stores and lists incidents of the call path kinds next
// to its old ones, also after a reopen.
func TestFixtureIncidentsWithUDPKinds(t *testing.T) {
	dir := fixtureDirV3(t)
	now := t0.Add(2*time.Hour + 30*time.Minute)
	s := openT(t, dir, Options{Now: func() time.Time { return now }})
	kinds := []model.VerdictKind{model.VerdictUDPBlocked, model.VerdictSTUNBlocked, model.VerdictUDPDegraded}
	for i, k := range kinds {
		in := &model.Incident{Start: t0.Add(2*time.Hour + time.Duration(i)*time.Minute), Kind: k, Summary: string(k), Targets: []string{"Meet-STUN"}, PeakLoss: 0.1}
		if i < 2 {
			in.End = in.Start.Add(30 * time.Second)
		}
		if err := s.SaveIncident(ctx, in); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openT(t, dir, Options{Now: func() time.Time { return now }})
	defer s.Close()
	got, err := s.Incidents(ctx, t0, now)
	if err != nil || len(got) != len(fixtureIncidents())+len(kinds) {
		t.Fatalf("incidents %+v %v", got, err)
	}
	for i, k := range kinds {
		in := got[len(kinds)-1-i] // newest first
		if in.Kind != k || in.Summary != string(k) || !slices.Equal(in.Targets, []string{"Meet-STUN"}) || (i < 2) == in.End.IsZero() {
			t.Errorf("incident %d: %+v", i, in)
		}
	}
}
