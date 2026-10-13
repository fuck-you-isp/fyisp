package tiles

import (
	"math/rand/v2"
	"slices"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

// lossy returns events losing every second of [from, to) with probability p
// for the targets match picks.
func lossy(from, to time.Time, p float64, match func(string) bool, seed uint64) []eEvent {
	r := rand.New(rand.NewPCG(seed, 3))
	var out []eEvent
	for t := from; t.Before(to); t = t.Add(time.Second) {
		if r.Float64() < p {
			out = append(out, eEvent{t, t.Add(time.Second), match, 0})
		}
	}
	return out
}

func badTiles(s *Snapshot) int {
	n := 0
	for _, ts := range s.Tiles {
		if ts.State.Bad() {
			n++
		}
	}
	return n
}

// C1 (clean-blips-only filter): a 20 s blip of the whole connection, then
// the path's ICMP answers while every app endpoint is dead. The verdict
// says ok (path fine). The blip is a lagBlip (path clean after it): its
// tail must not excuse every tile.
func TestR34dBlipThenAppsDead(t *testing.T) {
	for _, kind := range []model.VerdictKind{model.VerdictOK, model.VerdictUpstream} {
		s := newLagScene(t)
		s.fast()
		isPath := pathTarget(s.p)
		d := s.start.Add(20 * time.Minute)
		end := d.Add(20 * time.Second)
		if kind != model.VerdictOK {
			s.inc = []model.Incident{{ID: 1, Kind: kind, Start: end.Add(30 * time.Second), End: end.Add(20 * time.Minute), PeakLoss: 1}}
		}
		apps := func(n string) bool { return n != "Gateway" && n != "ISP edge" && !isPath(n) }
		evs := []eEvent{{d, end, notGateway, 0}, {end, end.Add(12 * time.Minute), apps, 0}}
		hidden, first := 0, time.Duration(-1)
		s.run(t, evs, end, end.Add(5*time.Minute), func(e *engine, now time.Time) {
			snap := e.Snapshot()
			if now.Sub(end)%(15*time.Second) == 0 {
				t.Logf("%s +%v card %s/%s | bad tiles %d", kind, now.Sub(end), snap.Conn.Kind, snap.Conn.Tone, badTiles(snap))
			}
			if 2*badTiles(snap) > len(snap.Tiles) && (snap.Conn.Kind == ConnWorkingAgain || snap.Conn.Kind == ConnGood) {
				hidden++
				if first < 0 {
					first = now.Sub(end)
				}
			}
		})
		if hidden > 0 {
			t.Errorf("%s: apps dead after a blip: card working again/good with most tiles bad on %d ticks (first +%v)", kind, hidden, first)
		}
	}
}

// A1 (peak-loss card): 40% (and 32%) random loss of everything past the
// router from t0; the isp verdict fires at t0+8 s and its 60 s window's
// loss ramps up from 0. The card is red from t0+15 s on.
func TestR34dPeakLossRamp(t *testing.T) {
	for _, p := range []float64{0.4, 0.32} {
		s := newLagScene(t)
		t0 := s.start.Add(20 * time.Minute)
		since := t0.Add(8 * time.Second)
		s.inc = []model.Incident{{ID: 1, Kind: model.VerdictISP, Start: since, End: t0.Add(20 * time.Minute), PeakLoss: p}}
		s.loss = 0
		evs := lossy(t0, t0.Add(10*time.Minute), p, notGateway, 5)
		notRed, first := 0, time.Duration(-1)
		s.run(t, evs, t0.Add(-time.Minute), t0.Add(4*time.Minute), func(e *engine, now time.Time) {
			snap := e.Snapshot()
			if now.After(t0.Add(15*time.Second)) && snap.Conn.Tone != ToneBad {
				notRed++
				if first < 0 {
					first = now.Sub(t0)
					t.Logf("%.0f%%: +%v card %s/%s loss %.2f", 100*p, now.Sub(t0), snap.Conn.Kind, snap.Conn.Tone, s.loss)
				}
			}
			// the window's loss for the next tick
			s.loss = p * min(1, float64(now.Add(tickEvery).Sub(t0))/float64(time.Minute))
			if s.loss < 0 {
				s.loss = 0
			}
		})
		if notRed > 0 {
			t.Errorf("%.0f%% loss: card not red on %d ticks (first +%v)", 100*p, notRed, first)
		}
	}
}

// B1 (edge "losing data" exception): 40% loss of everything past the
// router for 2 minutes, then a total outage past the router. How long the
// edge's strip says losing data instead of not answering; and a flapping
// line (25 s dead, 10 s at 40%) inside one isp incident.
func TestR34dLossyRunOnset(t *testing.T) {
	s := newLagScene(t)
	t0 := s.start.Add(20 * time.Minute)
	o := t0.Add(2 * time.Minute)
	s.inc = []model.Incident{{ID: 1, Kind: model.VerdictISP, Start: t0.Add(8 * time.Second), End: t0.Add(20 * time.Minute), PeakLoss: 1}}
	s.loss = 0.4
	evs := append(lossy(t0, o, 0.4, notGateway, 9), eEvent{o, o.Add(10 * time.Minute), notGateway, 0})
	downAt := time.Duration(-1)
	notRed := 0
	s.run(t, evs, o, o.Add(2*time.Minute), func(e *engine, now time.Time) {
		snap := e.Snapshot()
		if downAt < 0 && snap.Layers[layerEdge].State == Down {
			downAt = now.Sub(o)
		}
		if snap.Conn.Tone != ToneBad {
			notRed++
		}
		s.loss = min(0.95, 0.4+0.6*float64(now.Add(tickEvery).Sub(o))/float64(time.Minute))
	})
	t.Logf("outage after 40%% loss: edge strip Down from +%v; card not red on %d ticks", downAt, notRed)
	if downAt < 0 || downAt > dropMin+2*tickEvery {
		t.Errorf("edge strip Down only from +%v after a total outage began", downAt)
	}
	if notRed > 0 {
		t.Errorf("card not red on %d ticks of the outage", notRed)
	}

	// flapping: 25 s dead, 10 s at 40% loss, for 8 minutes
	s = newLagScene(t)
	s.inc = []model.Incident{{ID: 1, Kind: model.VerdictISP, Start: t0.Add(8 * time.Second), End: t0.Add(20 * time.Minute), PeakLoss: 0.8}}
	s.loss = 0.8
	evs = nil
	for f := t0; f.Before(t0.Add(8 * time.Minute)); f = f.Add(35 * time.Second) {
		evs = append(evs, eEvent{f, f.Add(25 * time.Second), notGateway, 0})
		evs = append(evs, lossy(f.Add(25*time.Second), f.Add(35*time.Second), 0.4, notGateway, uint64(f.Unix()))...)
	}
	down, slow, ticks, notRed := 0, 0, 0, 0
	s.run(t, evs, t0.Add(2*time.Minute), t0.Add(7*time.Minute), func(e *engine, now time.Time) {
		snap := e.Snapshot()
		ticks++
		switch snap.Layers[layerEdge].State {
		case Down:
			down++
		case Slow:
			slow++
		}
		if snap.Conn.Tone != ToneBad {
			notRed++
		}
	})
	t.Logf("flapping 25 s dead / 10 s 40%%: edge strip Down %d, Slow %d of %d ticks; card not red %d", down, slow, ticks, notRed)
	if notRed > 0 {
		t.Errorf("flapping: card not red on %d of %d ticks", notRed, ticks)
	}
	if down == 0 {
		t.Errorf("flapping (71%% of the time totally dead): the edge strip never says not answering (Slow %d of %d)", slow, ticks)
	}
}

// D1 (call-window shift, ongoing): TestCallsLossBlamesProvider's loss
// going on for 15 minutes: while the calls problem is ongoing it is the
// provider's at every tick (the shifted window holds 2 clean minutes
// before the problem).
func TestR34dCallShiftOngoing(t *testing.T) {
	s := newLagScene(t)
	t0 := s.start.Add(20 * time.Minute)
	var stun []string
	for _, tg := range s.p.Targets {
		if slices.Contains(tg.ProbeKinds(), model.KindUDP) {
			stun = append(stun, tg.Name)
		}
	}
	var evs []eEvent
	for k := range 60 {
		f := t0.Add(time.Duration(k)*15*time.Second + time.Duration(k%5)*time.Second)
		evs = append(evs, eEvent{f, f.Add(1500 * time.Millisecond), eNames(stun...), 0})
	}
	for k := range 22 {
		f := t0.Add(time.Duration(k)*40*time.Second + 7*time.Second)
		evs = append(evs, eEvent{f, f.Add(time.Second), eNames("ISP edge"), 0})
	}
	who := map[Who]int{}
	firstNot := time.Duration(-1)
	s.run(t, evs, t0, t0.Add(15*time.Minute), func(e *engine, now time.Time) {
		snap := e.Snapshot()
		for _, x := range snap.Eps {
			if slices.Contains(x.Tiles, "zoom") && x.Ongoing() {
				who[x.Cause.Who]++
				if x.Cause.Who != WhoProvider && firstNot < 0 {
					firstNot = now.Sub(t0)
					t.Logf("+%v: %s", now.Sub(t0), epsText([]Episode{x}))
				}
			}
		}
	})
	t.Logf("ongoing calls problem causes by tick: %v", who)
	if firstNot >= 0 {
		t.Errorf("ongoing provider loss: calls problem not blamed on the provider from +%v (%v)", firstNot, who)
	}
}

// D2 (call-window shift, misblame): the edge loses 4 of 60 checks in the
// minute t0 (a lossy minute, no drop), clean after. From t0+90 s, Zoom's
// own STUN loses a burst in 3 for 4 minutes, the path clean. Zoom's
// problem is not the provider's.
func TestR34dCallShiftMisblame(t *testing.T) {
	s := newLagScene(t)
	t0 := s.start.Add(20 * time.Minute)
	var evs []eEvent
	for k := range 4 {
		f := t0.Add(time.Duration(k)*13*time.Second + 3*time.Second)
		evs = append(evs, eEvent{f, f.Add(time.Second), eNames("ISP edge"), 0})
	}
	var stun []string
	for _, tg := range s.p.Targets {
		if slices.Contains(tg.ProbeKinds(), model.KindUDP) {
			stun = append(stun, tg.Name)
		}
	}
	z := t0.Add(90 * time.Second)
	for k := range 16 { // about 10% of the bursts, as TestCallsLossBlamesProvider
		f := z.Add(time.Duration(k)*15*time.Second + time.Duration(k%5)*time.Second)
		evs = append(evs, eEvent{f, f.Add(1500 * time.Millisecond), eNames(stun...), 0})
	}
	whoDuring := map[Who]int{}
	var last *Snapshot
	s.run(t, evs, t0, t0.Add(14*time.Minute), func(e *engine, now time.Time) {
		snap := e.Snapshot()
		last = snap
		for _, x := range snap.Eps {
			if slices.Contains(x.Tiles, "zoom") && x.Ongoing() {
				whoDuring[x.Cause.Who]++
			}
		}
	})
	t.Logf("during: %v\nafter:\n%s", whoDuring, epsText(last.Eps))
	for _, x := range last.Eps {
		if slices.Contains(x.Tiles, "zoom") && x.Cause.Who == WhoProvider && x.Sev == Slow {
			t.Errorf("zoom's own STUN loss blamed on the provider (an edge loss minute 90 s before it): %s", epsText([]Episode{x}))
		}
	}
	if whoDuring[WhoProvider] > 0 {
		t.Errorf("zoom's own STUN loss blamed on the provider on %d ticks while ongoing", whoDuring[WhoProvider])
	}
}
