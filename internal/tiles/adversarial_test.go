package tiles

import (
	"slices"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/profile"
)

func pathTarget(p *model.Profile) func(string) bool {
	return func(n string) bool {
		for _, tg := range p.Targets {
			if tg.Name == n {
				return tg.Layer != ""
			}
		}
		return false
	}
}

func tileStates(s *Snapshot) string {
	out := ""
	for _, ts := range s.Tiles {
		out += ts.ID + "=" + string(ts.State) + " "
	}
	return out
}

func layerStates(s *Snapshot) string {
	out := ""
	for _, l := range s.Layers {
		out += string(l.State) + " "
	}
	return out
}

// A1: a 40 s total drop, then the path's ICMP answers (gateway, edge, and
// optionally anycast) while every app endpoint (TCP/HTTPS/STUN) stays dead
// for 10 more minutes. The verdict's incident goes on.
func TestAdvICMPOkAppsDead(t *testing.T) {
	for _, tc := range []struct {
		name   string
		kind   model.VerdictKind
		acDead bool
	}{{"isp", model.VerdictISP, false}, {"upstream-ac-ok", model.VerdictUpstream, false}, {"upstream-ac-dead", model.VerdictUpstream, true}} {
		t.Run(tc.name, func(t *testing.T) {
			s := newLagScene(t)
			s.fast()
			s.acDown = tc.acDead
			isPath := pathTarget(s.p)
			d := s.start.Add(20 * time.Minute)
			end := d.Add(40 * time.Second)
			s.inc = []model.Incident{{ID: 1, Kind: tc.kind, Start: d.Add(9 * time.Second), End: end.Add(13 * time.Minute), PeakLoss: 1}}
			apps := func(n string) bool {
				if n == "Gateway" || n == "ISP edge" {
					return false
				}
				return !isPath(n) || tc.acDead
			}
			evs := []eEvent{{d, end, notGateway, 0}, {end, end.Add(12 * time.Minute), apps, 0}}
			notRed := 0
			first := time.Duration(-1)
			s.run(t, evs, d.Add(-30*time.Second), end.Add(6*time.Minute), func(e *engine, now time.Time) {
				snap := e.Snapshot()
				if now.Sub(end)%(15*time.Second) == 0 && now.After(end) {
					t.Logf("+%v card %s/%s | tiles %s | layers %s", now.Sub(end), snap.Conn.Kind, snap.Conn.Tone, tileStates(snap), layerStates(snap))
				}
				if now.After(end) && snap.Conn.Tone != ToneBad {
					notRed++
					if first < 0 {
						first = now.Sub(end)
					}
				}
			})
			if notRed > 0 {
				t.Errorf("apps dead all along after the drop: card not red on %d ticks (first at +%v)", notRed, first)
			}
		})
	}
}

// A2: a 40 s drop whose incident is the drop's lag (ends 105 s after);
// then a new full outage 149 s after the drop, with its own incident.
// A2b: the same, but the verdict's incident never ends (second outage at
// +100 s, before the lag incident would have ended).
func TestAdvOutageAfterLag(t *testing.T) {
	for _, gap := range []time.Duration{100 * time.Second, 140 * time.Second, 149 * time.Second, 151 * time.Second} {
		for _, cont := range []bool{false, true} {
			s := newLagScene(t)
			s.fast()
			d := s.start.Add(20 * time.Minute)
			end := d.Add(40 * time.Second)
			o := end.Add(gap)
			if cont {
				s.inc = []model.Incident{{ID: 1, Kind: model.VerdictISP, Start: d.Add(9 * time.Second), End: o.Add(11 * time.Minute), PeakLoss: 1}}
			} else {
				s.inc = []model.Incident{{ID: 1, Kind: model.VerdictISP, Start: d.Add(9 * time.Second), End: end.Add(min(105*time.Second, gap)), PeakLoss: 1},
					{ID: 2, Kind: model.VerdictISP, Start: o.Add(9 * time.Second), End: o.Add(11 * time.Minute), PeakLoss: 1}}
			}
			evs := []eEvent{{d, end, notGateway, 0}, {o, o.Add(10 * time.Minute), notGateway, 0}}
			bad := 0
			var firstBad time.Duration = -1
			var last *Snapshot
			s.run(t, evs, o, o.Add(5*time.Minute), func(e *engine, now time.Time) {
				snap := e.Snapshot()
				last = snap
				if now.After(o.Add(30*time.Second)) && snap.Conn.Tone != ToneBad {
					bad++
					if firstBad < 0 {
						firstBad = now.Sub(o)
						t.Logf("gap %v cont %v: +%v card %s/%s tiles %s", gap, cont, now.Sub(o), snap.Conn.Kind, snap.Conn.Tone, tileStates(snap))
					}
				}
			})
			if bad > 0 {
				t.Errorf("gap %v cont %v: second outage, card not red on %d ticks from +%v", gap, cont, bad, firstBad)
			}
			if !slices.ContainsFunc(last.Eps, func(x Episode) bool { return x.Connection && x.Ongoing() }) {
				t.Errorf("gap %v cont %v: no ongoing connection problem 5 min into the 2nd outage:\n%s", gap, cont, epsText(last.Eps))
			}
		}
	}
}

// A3: the whole connection flaps: off for `off`, on for `on`, for 10
// minutes (router rebooting radio / bad cable). Real calls lose most of
// their bursts. The verdict says lan with heavy loss.
func TestAdvFlapping(t *testing.T) {
	for _, tc := range []struct{ off, on time.Duration }{{6 * time.Second, 6 * time.Second}, {6 * time.Second, 3 * time.Second}, {8 * time.Second, 12 * time.Second}} {
		s := newLagScene(t)
		s.loss = float64(tc.off) / float64(tc.off+tc.on)
		t0 := s.start.Add(20 * time.Minute)
		s.inc = []model.Incident{{ID: 1, Kind: model.VerdictLAN, Start: t0.Add(20 * time.Second), End: t0.Add(11 * time.Minute), PeakLoss: s.loss}}
		var evs []eEvent
		for f := t0; f.Before(t0.Add(10 * time.Minute)); f = f.Add(tc.off + tc.on) {
			evs = append(evs, eEvent{f, f.Add(tc.off), eAll, 0})
		}
		var last *Snapshot
		s.run(t, evs, t0.Add(8*time.Minute), t0.Add(8*time.Minute), func(e *engine, now time.Time) { last = e.Snapshot() })
		t.Logf("off %v on %v: card %s/%s | tiles %s | layers %s\n%s", tc.off, tc.on, last.Conn.Kind, last.Conn.Tone, tileStates(last), layerStates(last), epsText(last.Eps))
		good := 0
		for _, ts := range last.Tiles {
			if ts.State == Good {
				good++
			}
		}
		if good > 0 {
			t.Errorf("off %v on %v (%.0f%% loss everywhere): %d tiles Good", tc.off, tc.on, 100*s.loss, good)
		}
		if last.Conn.Tone != ToneBad {
			t.Errorf("off %v on %v: card %s %s, want red", tc.off, tc.on, last.Conn.Kind, last.Conn.Tone)
		}
	}
}

// A4: the router stops answering ICMP (rate limit) for 10 minutes; the
// internet is fine, but Zoom's STUN loses 1 burst in 3 (real call loss)
// and Teams' web endpoints are down. Their tiles must still show it.
func TestAdvGatewayICMPDead(t *testing.T) {
	s := newLagScene(t)
	s.fast()
	t0 := s.start.Add(20 * time.Minute)
	var zoomStun []string
	for _, r := range profile.ResolveTiles(s.p) {
		if r.ID == "zoom" {
			for _, tg := range r.UDP {
				zoomStun = append(zoomStun, tg.Name)
			}
		}
	}
	evs := []eEvent{{t0, t0.Add(10 * time.Minute), eNames("Gateway"), 0}}
	for f := t0; f.Before(t0.Add(10 * time.Minute)); f = f.Add(15 * time.Second) {
		evs = append(evs, eEvent{f, f.Add(5 * time.Second), eNames(zoomStun...), 0})
	}
	evs = append(evs, eEvent{t0, t0.Add(10 * time.Minute), tileWeb(s.p, "teams"), 0})
	var last *Snapshot
	s.run(t, evs, t0.Add(8*time.Minute), t0.Add(8*time.Minute), func(e *engine, now time.Time) { last = e.Snapshot() })
	t.Logf("card %s/%s | tiles %s | layers %s\n%s zoom stun %v", last.Conn.Kind, last.Conn.Tone, tileStates(last), layerStates(last), epsText(last.Eps), zoomStun)
	for _, ts := range last.Tiles {
		if (ts.ID == "zoom" || ts.ID == "teams") && ts.State == Good {
			t.Errorf("gateway ICMP dead: %s Good though it is losing data", ts.ID)
		}
	}
	// control: the same without the gateway
	s2 := newLagScene(t)
	s2.fast()
	s2.run(t, evs[1:], t0.Add(8*time.Minute), t0.Add(8*time.Minute), func(e *engine, now time.Time) { last = e.Snapshot() })
	t.Logf("control: card %s/%s | tiles %s", last.Conn.Kind, last.Conn.Tone, tileStates(last))
}

// A5: the edge stops answering ICMP for 10 minutes (ISP rate limit),
// while Zoom's STUN really loses 1 burst in 3.
func TestAdvEdgeICMPDead(t *testing.T) {
	s := newLagScene(t)
	s.fast()
	t0 := s.start.Add(20 * time.Minute)
	var zoomStun []string
	for _, r := range profile.ResolveTiles(s.p) {
		if r.ID == "zoom" {
			for _, tg := range r.UDP {
				zoomStun = append(zoomStun, tg.Name)
			}
		}
	}
	evs := []eEvent{{t0, t0.Add(10 * time.Minute), eNames("ISP edge"), 0}}
	for f := t0; f.Before(t0.Add(10 * time.Minute)); f = f.Add(15 * time.Second) {
		evs = append(evs, eEvent{f, f.Add(5 * time.Second), eNames(zoomStun...), 0})
	}
	var last *Snapshot
	s.run(t, evs, t0.Add(8*time.Minute), t0.Add(8*time.Minute), func(e *engine, now time.Time) { last = e.Snapshot() })
	t.Logf("card %s/%s | tiles %s | layers %s\n%s", last.Conn.Kind, last.Conn.Tone, tileStates(last), layerStates(last), epsText(last.Eps))
	for _, ts := range last.Tiles {
		if ts.ID == "zoom" && ts.State == Good {
			t.Errorf("edge ICMP dead: zoom Good though a third of its bursts are lost")
		}
	}
}

// A6: an outage of the provider (edge answers ICMP: its own router is up,
// the uplink beyond is dead, anycast dead) starting 9 s after a 40 s drop
// ended, its incident being the same one: lan/isp/upstream.
func TestAdvUpstreamRightAfterDrop(t *testing.T) {
	s := newLagScene(t)
	s.fast()
	s.acDown = true
	d := s.start.Add(20 * time.Minute)
	end := d.Add(40 * time.Second)
	s.inc = []model.Incident{{ID: 1, Kind: model.VerdictISP, Start: d.Add(9 * time.Second), End: end.Add(12 * time.Minute), PeakLoss: 1}}
	evs := []eEvent{{d, end, notGateway, 0}, {end.Add(9 * time.Second), end.Add(11 * time.Minute), beyondEdge, 0}}
	notRed := 0
	s.run(t, evs, end, end.Add(5*time.Minute), func(e *engine, now time.Time) {
		snap := e.Snapshot()
		if now.Sub(end)%(15*time.Second) == 0 {
			t.Logf("+%v card %s/%s | tiles %s | layers %s", now.Sub(end), snap.Conn.Kind, snap.Conn.Tone, tileStates(snap), layerStates(snap))
		}
		if now.After(end.Add(40*time.Second)) && snap.Conn.Tone != ToneBad {
			notRed++
		}
	})
	if notRed > 0 {
		t.Errorf("outage 9 s after a drop: card not red on %d ticks", notRed)
	}
}

// A7: the realistic verdict for A6: the isp incident of the drop ends
// once the edge's window is clean (~60 s after), then an upstream one
// starts (anycast dead). And A7b: anycast answers, only the apps are dead;
// the verdict ends its incident 105 s after the drop and says ok.
func TestAdvRealisticAfterDrop(t *testing.T) {
	for _, ac := range []bool{true, false} {
		s := newLagScene(t)
		s.fast()
		s.acDown = ac
		isPath := pathTarget(s.p)
		d := s.start.Add(20 * time.Minute)
		end := d.Add(40 * time.Second)
		if ac {
			s.inc = []model.Incident{{ID: 1, Kind: model.VerdictISP, Start: d.Add(9 * time.Second), End: end.Add(60 * time.Second), PeakLoss: 1},
				{ID: 2, Kind: model.VerdictUpstream, Start: end.Add(60 * time.Second), End: end.Add(20 * time.Minute), PeakLoss: 1}}
		} else {
			s.inc = []model.Incident{{ID: 1, Kind: model.VerdictISP, Start: d.Add(9 * time.Second), End: end.Add(105 * time.Second), PeakLoss: 1}}
		}
		dead := beyondEdge
		if !ac {
			dead = func(n string) bool { return !isPath(n) }
		}
		evs := []eEvent{{d, end, notGateway, 0}, {end, end.Add(15 * time.Minute), dead, 0}}
		s.run(t, evs, end, end.Add(4*time.Minute), func(e *engine, now time.Time) {
			snap := e.Snapshot()
			if now.Sub(end)%(15*time.Second) == 0 {
				bad := 0
				for _, ts := range snap.Tiles {
					if ts.State.Bad() {
						bad++
					}
				}
				t.Logf("ac dead %v +%v card %s/%s | bad tiles %d | layers %s | eps %d", ac, now.Sub(end), snap.Conn.Kind, snap.Conn.Tone, bad, layerStates(snap), len(snap.Eps))
			}
		})
	}
}
