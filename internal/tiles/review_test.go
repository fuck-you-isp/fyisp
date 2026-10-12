package tiles

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

func beyondEdge(name string) bool { return name != "Gateway" && name != "ISP edge" }

func cardLog(t *testing.T, s *lagScene, evs []eEvent, from, end time.Time, every time.Duration, ref time.Time) *Snapshot {
	var last *Snapshot
	next := from
	s.run(t, evs, from, end, func(e *engine, now time.Time) {
		snap := e.Snapshot()
		last = snap
		if !now.Before(next) {
			next = next.Add(every)
			ongoing := 0
			for _, x := range snap.Eps {
				if x.Connection && x.Ongoing() {
					ongoing++
				}
			}
			bad := 0
			for _, ts := range snap.Tiles {
				if ts.State.Bad() {
					bad++
				}
			}
			t.Logf("+%v card %s tone %s key %s | bad tiles %d | ongoing conn eps %d | eps %d", now.Sub(ref), snap.Conn.Kind, snap.Conn.Tone, snap.Conn.Key, bad, ongoing, len(snap.Eps))
		}
	})
	return last
}

// R1: an upstream outage (edge answers) that flaps once: 40 s down, 20 s
// up, then down for 10 minutes. The verdict's upstream incident started in
// the first part.
func TestRev34UpstreamFlap(t *testing.T) {
	s := newLagScene(t)
	s.fast()
	s.acDown = true
	d := s.start.Add(20 * time.Minute)
	s.inc = []model.Incident{{ID: 1, Kind: model.VerdictUpstream, Start: d.Add(9 * time.Second), End: d.Add(13 * time.Minute), PeakLoss: 1}}
	evs := []eEvent{{d, d.Add(40 * time.Second), beyondEdge, 0}, {d.Add(60 * time.Second), d.Add(12 * time.Minute), beyondEdge, 0}}
	last := cardLog(t, s, evs, d.Add(-30*time.Second), d.Add(8*time.Minute), 30*time.Second, d)
	t.Logf("eps at +8m:\n%s", epsText(last.Eps))
	if last.Conn.Tone != ToneBad {
		t.Errorf("8 min into a real upstream outage: card %s tone %s, want red", last.Conn.Kind, last.Conn.Tone)
	}
}

// R2: an ISP latency problem right after a drop: the edge answers 30x
// slower for 10 minutes; the verdict's isp incident goes on (same kind).
func TestRev34ISPSlowAfterDrop(t *testing.T) {
	s := newLagScene(t)
	s.fast()
	s.loss = 0
	d := s.start.Add(20 * time.Minute)
	end := d.Add(40 * time.Second)
	s.inc = []model.Incident{{ID: 1, Kind: model.VerdictISP, Start: d.Add(9 * time.Second), End: end.Add(12 * time.Minute), PeakLoss: 1}}
	evs := []eEvent{{d, end, notGateway, 0}, {end, end.Add(11 * time.Minute), notGateway, 30}}
	last := cardLog(t, s, evs, d.Add(-30*time.Second), end.Add(8*time.Minute), 30*time.Second, end)
	t.Logf("eps:\n%s", epsText(last.Eps))
	if last.Conn.Kind != string(model.VerdictISP) {
		t.Errorf("8 min into an isp slowdown after a drop: card %s, want the isp card", last.Conn.Kind)
	}
}

// R3: 3 minutes of provider loss (~10%) with one 8 s total-loss run in it
// (a blip). After it ends, the problems list should still hold it.
func TestRev34LossyIncidentWithBlip(t *testing.T) {
	s := newLagScene(t)
	s.loss = 0.5
	t0 := s.start.Add(20 * time.Minute)
	s.inc = []model.Incident{{ID: 1, Kind: model.VerdictISP, Start: t0.Add(20 * time.Second), End: t0.Add(3 * time.Minute), PeakLoss: 0.5}}
	var evs []eEvent
	for k := 0; k < 30; k++ {
		f := t0.Add(time.Duration(k*6) * time.Second)
		evs = append(evs, eEvent{f, f.Add(3 * time.Second), eNames("ISP edge"), 0})
	}
	evs = append(evs, eEvent{t0.Add(72 * time.Second), t0.Add(84 * time.Second), notGateway, 0})
	var during, after []Episode
	s.run(t, evs, t0, t0.Add(10*time.Minute), func(e *engine, now time.Time) {
		snap := e.Snapshot()
		if now.Equal(t0.Add(2 * time.Minute)) {
			during = snap.Eps
		}
		after = snap.Eps
	})
	t.Logf("during:\n%s\nafter:\n%s", epsText(during), epsText(after))
	if !slices.ContainsFunc(after, func(x Episode) bool { return x.Connection && x.Verdict == model.VerdictISP }) {
		t.Errorf("a 2m40s provider loss incident is gone from the problems once over")
	}
}

// R4: bursty STUN loss: each STUN target loses 3 bursts in a row once a
// minute (staggered), ~25% of its bursts, for 10 minutes.
func TestRev34BurstySTUN(t *testing.T) {
	s := newLagScene(t)
	t0 := s.start.Add(20 * time.Minute)
	var stun []string
	for _, tg := range s.p.Targets {
		if slices.Contains(tg.ProbeKinds(), model.KindUDP) {
			stun = append(stun, tg.Name)
		}
	}
	var evs []eEvent
	for m := 0; m < 12; m++ {
		for i, n := range stun {
			f := t0.Add(time.Duration(m)*time.Minute + time.Duration(i*7%45)*time.Second)
			evs = append(evs, eEvent{f, f.Add(15 * time.Second), eNames(n), 0})
		}
	}
	var last *Snapshot
	s.run(t, evs, t0.Add(10*time.Minute), t0.Add(10*time.Minute), func(e *engine, now time.Time) { last = e.Snapshot() })
	for _, ts := range last.Tiles {
		t.Logf("tile %s %s", ts.ID, ts.State)
	}
	i := slices.IndexFunc(last.Tiles[:], func(ts TileSnap) bool { return ts.ID == "zoom" })
	if i < 0 || last.Tiles[i].State == Good {
		t.Errorf("zoom Good with 25%% of its STUN bursts lost in 15 s runs every minute")
	}
}

// R5: fuzz groupEpisodes with random drops, blips, path incidents,
// slowdowns, blocked incidents and tile parts.
func TestRev34FuzzGroup(t *testing.T) {
	m := time.Minute
	r := rand.New(rand.NewPCG(1, 2))
	now := at(120 * m)
	tids := []string{"zoom", "teams", "slack", "meet", "youtube"}
	causes := []func(Episode) Cause{byVendor, func(x Episode) Cause {
		if x.Connection {
			return Cause{Who: WhoCallsPath}
		}
		return Cause{Who: WhoUnknown}
	}}
	uniq := 0
	rt := func() (time.Time, time.Time) {
		uniq++
		a := at(time.Duration(r.IntN(110*60))*time.Second + time.Duration(uniq)*time.Millisecond)
		if r.IntN(6) == 0 {
			return a, time.Time{}
		}
		b := a.Add(time.Duration(1+r.IntN(600)) * time.Second)
		if b.After(now) {
			return a, time.Time{}
		}
		return a, b
	}
	for it := 0; it < 20000; it++ {
		var conn, tl []Episode
		for range r.IntN(4) {
			a, b := rt()
			if b.IsZero() && r.IntN(2) == 0 {
				b = a.Add(40 * time.Second)
			}
			conn = append(conn, dropEpisode(Drop{Start: a, End: b, Dur: 40 * time.Second}, ""))
		}
		for range r.IntN(3) {
			a, _ := rt()
			x := dropEpisode(Drop{Start: a, End: a.Add(20 * time.Second), Dur: 20 * time.Second}, "")
			x.Kind = epBlip
			conn = append(conn, x)
		}
		kinds := []model.VerdictKind{model.VerdictISP, model.VerdictLAN, model.VerdictUpstream, model.VerdictNoNetwork, model.VerdictDNS, model.VerdictUDPBlocked}
		var ongoingIn []Episode
		for range r.IntN(4) {
			a, b := rt()
			x, _ := incidentEpisode(model.Incident{Kind: kinds[r.IntN(len(kinds))], Start: a, End: b, PeakLoss: float64(r.IntN(2))}, false)
			conn = append(conn, x)
			if x.Ongoing() {
				ongoingIn = append(ongoingIn, x)
			}
		}
		if r.IntN(3) == 0 {
			a, b := rt()
			conn = append(conn, broadEpisode(a, b))
		}
		for range r.IntN(5) {
			a, b := rt()
			sev := []State{Down, Slow}[r.IntN(2)]
			x := tileRun(tids[r.IntN(len(tids))], sev, a.Sub(t0), 0)
			x.End = b
			tl = append(tl, x)
			if x.Ongoing() {
				ongoingIn = append(ongoingIn, x)
			}
		}
		cause := causes[r.IntN(2)]
		out := groupEpisodes(conn, tl, cause, now)
		// determinism vs shuffled input
		c2, t2 := slices.Clone(conn), slices.Clone(tl)
		r.Shuffle(len(c2), func(i, j int) { c2[i], c2[j] = c2[j], c2[i] })
		r.Shuffle(len(t2), func(i, j int) { t2[i], t2[j] = t2[j], t2[i] })
		out2 := groupEpisodes(c2, t2, cause, now)
		ctx := fmt.Sprintf("it %d\nconn:\n%s\ntiles:\n%s\nout:\n%s", it, epsText(conn), epsText(tl), epsText(out))
		if epsText(out) != epsText(out2) {
			t.Fatalf("order-dependent: %s\nout2:\n%s", ctx, epsText(out2))
		}
		for _, x := range out {
			if x.Kind == epBlip {
				t.Fatalf("blip in output: %s", ctx)
			}
			if !x.Ongoing() && x.End.Before(x.Start) {
				t.Fatalf("to < from: %s", ctx)
			}
		}
		// every ongoing input stays visible as an ongoing problem
		for _, in := range ongoingIn {
			if !slices.ContainsFunc(out, func(o Episode) bool {
				return o.Ongoing() && (len(in.Tiles) == 0 || slices.Contains(o.Tiles, in.Tiles[0]) || o.Connection)
			}) {
				t.Fatalf("ongoing input %+v hidden: %s", in, ctx)
			}
		}
	}
}
