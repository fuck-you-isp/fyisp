// Package homescen builds the Home view's scenarios: a profile, stored
// samples and a verdict with its outage log, so that the real /api/home
// roll-up can be shown (cmd/fyisp-webdev -home) and pinned (the golden
// files of internal/web) for the same situations. Not used by fyisp itself.
package homescen

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/profile"
	"github.com/fuck-you-isp/fyisp/internal/store"
)

// Names lists the scenarios.
var Names = []string{"good", "warming", "wifi", "isp", "upstream", "one-app-down", "calls-choppy",
	"udp-blocked", "no-network", "week-with-gaps", "clouds-other"}

// Scenario is one situation. Store holds its samples up to the time Build
// or Fill was last called with; Verdict is a verdict.Engine stand-in.
type Scenario struct {
	Name    string
	Profile *model.Profile
	Store   *store.Fake
	Verdict *Verdict

	loc   *time.Location
	start time.Time // the scenario's "now" at Build
	first time.Time // first data
	off   [][2]time.Time
	// lost says whether a probe at ts is lost; burst how a STUN burst goes.
	lost  func(t model.Target, k model.ProbeKind, ts time.Time) bool
	burst func(t model.Target, ts time.Time) (answered int, rtt, jitter float64)
	mu    sync.Mutex
	until time.Time // samples are written up to here
}

// Verdict is a fixed verdict and outage log (web.VerdictSource).
type Verdict struct {
	V    model.Verdict
	List []model.Incident // newest first
}

func (v *Verdict) Current() model.Verdict { return v.V }

func (v *Verdict) Incidents(_ context.Context, from, to time.Time) ([]model.Incident, error) {
	var out []model.Incident
	for _, in := range v.List {
		if in.Start.Before(to) && (in.End.IsZero() || in.End.After(from)) {
			out = append(out, in)
		}
	}
	return out, nil
}

// Build makes a scenario as of now; local days and times of day are in loc.
func Build(name string, now time.Time, loc *time.Location) (*Scenario, error) {
	p, err := profile.Default()
	if err != nil {
		return nil, err
	}
	ln := now.In(loc)
	today := time.Date(ln.Year(), ln.Month(), ln.Day(), 0, 0, 0, 0, loc)
	day := func(daysAgo, h, m int) time.Time {
		return today.AddDate(0, 0, -daysAgo).Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute)
	}
	s := &Scenario{Name: name, Profile: p, Store: store.NewFake(), loc: loc, start: now, first: now.Add(-6 * 24 * time.Hour),
		Verdict: &Verdict{V: model.Verdict{Kind: model.VerdictOK, Since: now.Add(-2 * time.Hour)}}}
	recent := func(ts time.Time, d time.Duration) bool { return !ts.Before(now.Add(-d)) }
	is := func(t model.Target, names ...string) bool { return slices.Contains(names, t.Name) }
	group := func(t model.Target, groups ...string) bool { return slices.Contains(groups, t.Group) }
	healthy := func(model.Target, time.Time) (int, float64, float64) { return 5, 18, 2 }
	choppy := func(d time.Duration) func(model.Target, time.Time) (int, float64, float64) {
		return func(_ model.Target, ts time.Time) (int, float64, float64) {
			if recent(ts, d) {
				if ts.Unix()%15 != 0 {
					return 4, 60, 45 // 2 bursts in 3 lose a request: about 1 in 7 lost
				}
				return 5, 60, 45
			}
			return 5, 18, 2
		}
	}
	s.burst = healthy
	s.lost = func(model.Target, model.ProbeKind, time.Time) bool { return false }
	since := func(d time.Duration) time.Time { return now.Add(-d).Truncate(time.Minute) }
	inc := func(id int64, k model.VerdictKind, from, to time.Time, loss float64, targets ...string) model.Incident {
		return model.Incident{ID: id, Start: from, End: to, Kind: k, PeakLoss: loss, Targets: targets,
			Summary: fmt.Sprintf("Scenario %s incident %d.", name, id)}
	}
	v := s.Verdict
	switch name {
	case "good":
		// A short fair stretch this morning, with no incident.
		s.burst = func(_ model.Target, ts time.Time) (int, float64, float64) {
			if !ts.Before(day(0, 11, 0)) && ts.Before(day(0, 11, 4)) {
				return 5, 40, 32
			}
			return 5, 18, 2
		}
	case "warming":
		s.first = now.Add(-40 * time.Second)
		v.V = model.Verdict{Kind: model.VerdictWarmingUp}
	case "wifi":
		v.V = model.Verdict{Kind: model.VerdictLAN, Since: since(10 * time.Minute),
			Evidence: map[string]float64{"gateway_loss": 0.3, "services_loss": 0.08}}
		s.lost = func(t model.Target, _ model.ProbeKind, ts time.Time) bool {
			return recent(ts, 10*time.Minute) && ts.Unix()%3 == 0
		}
		s.burst = choppy(10 * time.Minute)
		v.List = []model.Incident{inc(12, model.VerdictLAN, since(10*time.Minute), time.Time{}, 0.35),
			inc(11, model.VerdictLAN, day(2, 10, 5), day(2, 10, 11), 0.4)}
	case "isp":
		v.V = model.Verdict{Kind: model.VerdictISP, Since: since(14 * time.Minute),
			Evidence: map[string]float64{"edge_loss": 0.3, "services_loss": 0.1}}
		s.lost = func(t model.Target, _ model.ProbeKind, ts time.Time) bool {
			return recent(ts, 14*time.Minute) && t.Layer != model.LayerGateway && ts.Unix()%4 == 0
		}
		s.burst = choppy(14 * time.Minute)
		v.List = []model.Incident{inc(22, model.VerdictISP, since(14*time.Minute), time.Time{}, 0.62),
			inc(21, model.VerdictISP, day(4, 14, 41), day(4, 14, 56), 1)}
	case "upstream":
		v.V = model.Verdict{Kind: model.VerdictUpstream, Since: since(10 * time.Minute),
			Evidence: map[string]float64{"anycast_loss": 0.6, "services_loss": 0.6}}
		s.lost = func(t model.Target, _ model.ProbeKind, ts time.Time) bool {
			return recent(ts, 10*time.Minute) && (t.Layer == model.LayerAnycast || group(t, "work", "web"))
		}
		s.burst = choppy(10 * time.Minute)
		v.List = []model.Incident{inc(31, model.VerdictUpstream, since(10*time.Minute), time.Time{}, 0.6)}
	case "one-app-down":
		v.V = model.Verdict{Kind: model.VerdictService, Since: since(8 * time.Minute), Targets: []string{"M365-Outlook"}}
		s.lost = func(t model.Target, _ model.ProbeKind, ts time.Time) bool {
			return recent(ts, 8*time.Minute) && is(t, "M365-Outlook")
		}
		v.List = []model.Incident{inc(41, model.VerdictService, since(8*time.Minute), time.Time{}, 1, "M365-Outlook")}
	case "calls-choppy":
		v.V = model.Verdict{Kind: model.VerdictUDPDegraded, Since: since(9 * time.Minute)}
		s.burst = choppy(10 * time.Minute)
		v.List = []model.Incident{inc(51, model.VerdictUDPDegraded, since(9*time.Minute), time.Time{}, 0.2)}
	case "udp-blocked":
		v.V = model.Verdict{Kind: model.VerdictSTUNBlocked, Since: now.Add(-26 * time.Hour), Quiet: true}
		s.burst = func(_ model.Target, ts time.Time) (int, float64, float64) {
			if ts.After(now.Add(-26 * time.Hour)) {
				return 0, 0, 0
			}
			return 5, 18, 2
		}
		v.List = []model.Incident{inc(61, model.VerdictSTUNBlocked, now.Add(-26*time.Hour), time.Time{}, 1)}
	case "no-network":
		v.V = model.Verdict{Kind: model.VerdictNoNetwork, Since: since(8 * time.Minute)}
		s.lost = func(_ model.Target, _ model.ProbeKind, ts time.Time) bool { return recent(ts, 8*time.Minute) }
		s.burst = func(_ model.Target, ts time.Time) (int, float64, float64) {
			if recent(ts, 8*time.Minute) {
				return 0, 0, 0
			}
			return 5, 18, 2
		}
		v.List = []model.Incident{inc(71, model.VerdictNoNetwork, since(8*time.Minute), time.Time{}, 1)}
	case "week-with-gaps":
		s.first = day(4, 10, 0)
		s.off = [][2]time.Time{{day(3, 18, 2), day(2, 9, 15)}}
		v.List = []model.Incident{
			inc(83, model.VerdictLAN, day(1, 16, 2), day(1, 16, 6), 0.35),
			inc(82, model.VerdictISP, day(1, 9, 40), day(1, 9, 52), 0.7),
			inc(81, model.VerdictISP, day(3, 14, 5), day(3, 14, 20), 1),
		}
	case "clouds-other":
		r, err := profile.Builtin()
		if err != nil {
			return nil, err
		}
		if s.Profile, err = r.Resolve([]string{"aws"}, profile.ResolveOptions{}); err != nil {
			return nil, err
		}
		var down []string
		for i, t := range s.Profile.Targets {
			if t.Layer == "" && i%3 == 0 {
				down = append(down, t.Name)
			}
		}
		v.V = model.Verdict{Kind: model.VerdictService, Since: since(12 * time.Minute), Targets: down}
		s.lost = func(t model.Target, _ model.ProbeKind, ts time.Time) bool {
			return recent(ts, 12*time.Minute) && slices.Contains(down, t.Name)
		}
		v.List = []model.Incident{inc(91, model.VerdictService, since(12*time.Minute), time.Time{}, 1, down...)}
	default:
		return nil, fmt.Errorf("unknown scenario %q (want one of %v)", name, Names)
	}
	s.until = s.first
	s.Fill(now)
	return s, nil
}

// Fill writes the scenario's samples up to now (the way it "goes on"); the
// first call writes its history. Safe for concurrent use.
func (s *Scenario) Fill(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	from := s.until
	if !now.After(from) {
		return
	}
	s.until = now
	live := s.start.Add(-30 * time.Minute) // full rate from here; sparser before
	ln := s.start.In(s.loc)
	morning := time.Date(ln.Year(), ln.Month(), ln.Day(), 8, 0, 0, 0, s.loc)
	off := func(ts time.Time) bool {
		for _, o := range s.off {
			if !ts.Before(o[0]) && ts.Before(o[1]) {
				return true
			}
		}
		return false
	}
	for _, t := range s.Profile.Targets {
		for _, k := range t.ProbeKinds() {
			iv := t.Interval
			if iv == 0 {
				iv = 15 * time.Second
			}
			sparse, begin := time.Minute, from
			switch {
			case k == model.KindICMP:
				iv = 5 * time.Second
			case k == model.KindUDP:
				sparse = 15 * time.Second
				begin = later(from, morning) // today's calls only
			case t.Layer == "":
				begin = later(from, live) // services: the live windows only
			}
			for ts := begin.Truncate(iv); ts.Before(now); {
				if !ts.Before(from) && !off(ts) {
					s.observe(t, k, ts)
				}
				if ts.Before(live) {
					ts = ts.Add(sparse)
				} else {
					ts = ts.Add(iv)
				}
			}
		}
	}
}

func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func (s *Scenario) observe(t model.Target, k model.ProbeKind, ts time.Time) {
	key := func(k model.ProbeKind) model.SeriesKey { return model.SeriesKey{Target: t.Name, Kind: k} }
	if k != model.KindUDP {
		x := model.Sample{Key: key(k), Slot: ts, RTT: rttOf(t)}
		if s.lost(t, k, ts) {
			x.Lost, x.Reason, x.RTT = true, model.ReasonTimeout, 0
		}
		s.Store.Observe(x)
		return
	}
	ans, rtt, jit := s.burst(t, ts)
	u := model.Sample{Key: key(model.KindUDP), Slot: ts}
	j := model.Sample{Key: key(model.KindUDPJitter), Slot: ts}
	l := model.Sample{Key: key(model.KindUDPLoss), Slot: ts, RTT: time.Duration((5-ans)*20) * time.Millisecond}
	if ans > 0 {
		u.RTT = time.Duration(rtt * float64(time.Millisecond))
	} else {
		u.Lost, u.Reason = true, model.ReasonTimeout
	}
	if ans >= 2 {
		j.RTT = time.Duration(jit * float64(time.Millisecond))
	} else {
		j.Lost, j.Reason = true, model.ReasonGap
	}
	s.Store.Observe(u)
	s.Store.Observe(j)
	s.Store.Observe(l)
}

// rttOf is a plausible, steady RTT per layer.
func rttOf(t model.Target) time.Duration {
	switch t.Layer {
	case model.LayerGateway:
		return 2 * time.Millisecond
	case model.LayerEdge:
		return 8 * time.Millisecond
	case model.LayerAnycast:
		return 12 * time.Millisecond
	}
	return 24 * time.Millisecond
}
