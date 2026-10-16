package tiles

import (
	"slices"
	"sort"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

// vModel mimics the verdict engine for an edge losing whole seconds (lost):
// it judges every 5 s over the window (T-60 s, T]; isp when the edge lost at
// least 5 checks and minFrac of them (lossInner, the anycast lossy too), held
// 2 evaluations (Since: the first), back to ok after 60 s of ok evaluations.
type vModel struct {
	incs []model.Incident
	loss map[int64]float64 // eval time (unix s) -> window loss
}

func newVModel(lost map[int64]bool, from, to time.Time, minFrac float64) *vModel {
	m := &vModel{loss: map[int64]float64{}}
	var open *model.Incident
	cand := 0
	var candSince, okSince time.Time
	for T := from.Truncate(5 * time.Second); T.Before(to); T = T.Add(5 * time.Second) {
		n := 0
		for s := T.Unix() - 59; s <= T.Unix(); s++ {
			if lost[s] {
				n++
			}
		}
		l := float64(n) / 60
		m.loss[T.Unix()] = l
		isp := n >= 5 && l >= minFrac
		switch {
		case open == nil && isp:
			cand++
			if cand == 1 {
				candSince = T
			}
			if cand >= 2 {
				open = &model.Incident{ID: int64(len(m.incs) + 1), Kind: model.VerdictISP, Start: candSince, PeakLoss: l}
				okSince = time.Time{}
			}
		case open == nil:
			cand = 0
		case isp:
			okSince = time.Time{}
			open.PeakLoss = max(open.PeakLoss, l)
		default:
			if okSince.IsZero() {
				okSince = T
			}
			if T.Sub(okSince) >= time.Minute {
				open.End = okSince
				m.incs = append(m.incs, *open)
				open, cand = nil, 0
			}
		}
	}
	if open != nil {
		open.End = to.Add(time.Hour)
		m.incs = append(m.incs, *open)
	}
	return m
}

func (m *vModel) at(t time.Time) float64 {
	return m.loss[t.Truncate(5*time.Second).Unix()]
}

func lostSecs(evs []eEvent) map[int64]bool {
	out := map[int64]bool{}
	for _, ev := range evs {
		if ev.targets("ISP edge") && ev.mul == 0 {
			for t := ev.from; t.Before(ev.to); t = t.Add(time.Second) {
				out[t.Unix()] = true
			}
		}
	}
	return out
}

// runModelled runs evs with the verdict modelled from the edge's losses.
func runModelled(t *testing.T, evs []eEvent, from, to time.Time, minFrac float64, at func(e *engine, now time.Time, s *lagScene, m *vModel)) *vModel {
	s := newLagScene(t)
	m := newVModel(lostSecs(evs), s.start, to.Add(time.Minute), minFrac)
	s.inc = m.incs
	s.loss = 0
	s.run(t, evs, from, to, func(e *engine, now time.Time) {
		at(e, now, s, m)
		s.loss = m.at(now.Add(tickEvery))
	})
	return m
}

// E2: 40% and 35% random loss from t0 for 5 minutes, 20 seeds each, the
// verdict modelled. Time to red after onset; amber ticks after red.
func TestR34eTimeToRed(t *testing.T) {
	t0 := time.Date(2026, 10, 9, 15, 20, 0, 0, time.UTC)
	for _, p := range []float64{0.4, 0.35} {
		var firsts []time.Duration
		backAmber, lateAmber, never := 0, 0, 0
		for seed := uint64(1); seed <= 20; seed++ {
			evs := lossy(t0, t0.Add(5*time.Minute), p, notGateway, 200+seed)
			first := time.Duration(-1)
			runModelled(t, evs, t0.Add(-time.Minute), t0.Add(4*time.Minute), 0.1, func(e *engine, now time.Time, s *lagScene, m *vModel) {
				snap := e.Snapshot()
				if !now.After(t0) {
					return
				}
				if snap.Conn.Tone == ToneBad {
					if first < 0 {
						first = now.Sub(t0)
					}
					return
				}
				if first >= 0 {
					backAmber++
				}
				if now.Sub(t0) > 75*time.Second {
					lateAmber++
					if lateAmber < 3 {
						t.Logf("%.0f%% seed %d: +%v card %s/%s window %.3f peak %.3f", 100*p, seed, now.Sub(t0), snap.Conn.Kind, snap.Conn.Tone, s.loss, e.peak.loss)
					}
				}
			})
			if first < 0 {
				never++
				continue
			}
			firsts = append(firsts, first)
		}
		sort.Slice(firsts, func(i, j int) bool { return firsts[i] < firsts[j] })
		t.Logf("%.0f%%: time to red after onset (sorted) %v; never red %d; amber after red %d ticks; amber after +75s %d ticks", 100*p, firsts, never, backAmber, lateAmber)
		if never > 0 || lateAmber > 0 {
			t.Errorf("%.0f%%: never red in %d runs; amber after +75 s on %d ticks", 100*p, never, lateAmber)
		}
		if p >= 0.4 && len(firsts) > 0 && firsts[len(firsts)-1] > 30*time.Second {
			t.Errorf("40%%: red only after %v in the worst run", firsts[len(firsts)-1])
		}
	}
}

// E4: a long-running 35% loss (20 minutes): the card's Since and Key stay
// the incident's start, and it does not flip between amber and red.
func TestR34eLongLossCard(t *testing.T) {
	t0 := time.Date(2026, 10, 9, 15, 20, 0, 0, time.UTC)
	evs := lossy(t0, t0.Add(25*time.Minute), 0.35, notGateway, 77)
	keys := map[string]int{}
	var firstSince time.Time
	m := runModelled(t, evs, t0, t0.Add(20*time.Minute), 0.1, func(e *engine, now time.Time, s *lagScene, m *vModel) {
		snap := e.Snapshot()
		if snap.Conn.Kind == "" || snap.Conn.Kind == ConnGood {
			return
		}
		keys[snap.Conn.Kind+"/"+snap.Conn.Tone+"@"+snap.Conn.Since.Sub(t0).String()]++
		if firstSince.IsZero() {
			firstSince = snap.Conn.Since
		}
	})
	t.Logf("incidents %d (first start +%v); cards %v", len(m.incs), m.incs[0].Start.Sub(t0), keys)
	if len(m.incs) == 1 && (len(keys) > 2 || !firstSince.Equal(m.incs[0].Start)) {
		t.Errorf("one incident from +%v: cards %v", m.incs[0].Start.Sub(t0), keys)
	}
}

// E5 (lossier of two windows, ended): Zoom's own STUN loses a burst in 3
// for 4 minutes, the path clean; while its problem's last minutes are the
// call window emptying, the edge loses 10 checks (no drop). The calls'
// loss is not the provider's: it ended before the edge lost anything.
func TestR34eCallShiftTail(t *testing.T) {
	s := newLagScene(t)
	t0 := s.start.Add(20 * time.Minute)
	var stun []string
	for _, tg := range s.p.Targets {
		if slices.Contains(tg.ProbeKinds(), model.KindUDP) {
			stun = append(stun, tg.Name)
		}
	}
	var evs []eEvent
	for k := range 16 {
		f := t0.Add(time.Duration(k)*15*time.Second + time.Duration(k%5)*time.Second)
		evs = append(evs, eEvent{f, f.Add(1500 * time.Millisecond), eNames(stun...), 0})
	}
	tail := t0.Add(4*time.Minute + 20*time.Second)
	for k := range 10 {
		f := tail.Add(time.Duration(k)*7*time.Second + 2*time.Second)
		evs = append(evs, eEvent{f, f.Add(time.Second), eNames("ISP edge"), 0})
	}
	var last *Snapshot
	s.run(t, evs, t0, t0.Add(14*time.Minute), func(e *engine, now time.Time) { last = e.Snapshot() })
	t.Logf("after:\n%s", epsText(last.Eps))
	for _, x := range last.Eps {
		if slices.Contains(x.Tiles, "zoom") && x.Cause.Who == WhoProvider && x.Sev == Slow {
			t.Errorf("zoom's own STUN loss blamed on the provider (edge loss only in its window-emptying tail): %s", epsText([]Episode{x}))
		}
	}
}

// E2b: 40% random loss (TestR34eTimeToRed seed 15): the card must not turn
// ok while the isp verdict's own window says 40%.
func TestR34eGreenInLoss(t *testing.T) {
	t0 := time.Date(2026, 10, 9, 15, 20, 0, 0, time.UTC)
	evs := lossy(t0, t0.Add(5*time.Minute), 0.4, notGateway, 215)
	lost := lostSecs(evs)
	green := 0
	runModelled(t, evs, t0.Add(-time.Minute), t0.Add(4*time.Minute), 0.1, func(e *engine, now time.Time, s *lagScene, m *vModel) {
		snap := e.Snapshot()
		if now.Sub(t0) < 75*time.Second || snap.Conn.Tone == ToneBad {
			return
		}
		green++
		run, best := 0, 0
		for x := now.Add(-2 * time.Minute).Unix(); x <= now.Unix(); x++ {
			if lost[x] {
				run++
				best = max(best, run)
			} else {
				run = 0
			}
		}
		v := e.src.Verdict()
		e.mu.Lock()
		vw := e.view(now)
		t.Logf("connDrops %v connCur %v blips %v lagBlips %v edgeEnded %v gwEnded %v", vw.connDrops, vw.connCur, vw.blips, vw.lagBlips, vw.layerEnded[layerEdge], vw.layerEnded[layerGateway])
		for _, b := range vw.blips {
			for _, l := range []int{layerGateway, layerEdge, layerAnycast} {
				n, lost, gap := 0, 0, 0
				for _, i := range e.paths[l] {
					for _, c := range e.ss[i].raw {
						if c.ms > b.End.UnixMilli() && c.ms <= now.UnixMilli() {
							n++
							if c.lost {
								lost++
							}
							if c.gap {
								gap++
							}
						}
					}
				}
				t.Logf("blip %v-%v layer %d clean %v raw n %d lost %d gap %d windows %v", b.Start.Format("15:04:05"), b.End.Format("15:04:05"), l, e.layersClean(vw, b.End, b.End.Add(lagMax), l), n, lost, gap, e.layerWindows(vw, l))
			}
		}
		for _, x := range vw.eps {
			t.Logf("ep %s", epsText([]Episode{x}))
		}
		e.mu.Unlock()
		t.Logf("+%v card %s/%s since %v again %+v; verdict %s since +%v loss %.3f; longest edge run in 2 min %ds; bad tiles %d of %d; edge strip %s",
			now.Sub(t0), snap.Conn.Kind, snap.Conn.Tone, snap.Conn.Since.Sub(t0), snap.Conn.Again, v.Kind, v.Since.Sub(t0), s.loss, best, badTiles(snap), len(snap.Tiles), snap.Layers[layerEdge].State)
	})
	if green > 0 {
		t.Errorf("40%% loss: card not red on %d ticks after +75 s", green)
	}
}
