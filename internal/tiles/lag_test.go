package tiles

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/profile"
)

// The shapes the netns lab (test/netns, TestHome10) found: the verdict's
// 60 s window holds a drop for a minute or two after it ended.

// lagScene is the default profile fed live from start, with a verdict and
// incidents that follow a script: inc as the verdict engine would report
// them at now (ongoing until their End).
type lagScene struct {
	p      *model.Profile
	start  time.Time
	now    time.Time
	inc    []model.Incident
	loss   float64 // the verdict's evidence "loss"
	acDown bool
}

func newLagScene(t *testing.T) *lagScene {
	t.Helper()
	p, err := profile.Default()
	if err != nil {
		t.Fatal(err)
	}
	return &lagScene{p: p, start: time.Date(2026, 10, 9, 15, 0, 0, 0, time.UTC), loss: 1}
}

// fast probes every target but the path's every 6 s, as the netns lab
// does: the tiles go Down within some 25 s of a drop.
func (s *lagScene) fast() {
	for i := range s.p.Targets {
		if s.p.Targets[i].Layer == "" {
			s.p.Targets[i].Interval = 6 * time.Second
		}
	}
}

func (s *lagScene) current() *model.Incident {
	for i, in := range s.inc {
		if !in.Start.After(s.now) && in.End.After(s.now) {
			return &s.inc[i]
		}
	}
	return nil
}

func (s *lagScene) sources() Sources {
	return Sources{
		Incidents: func(_ context.Context, from, to time.Time) ([]model.Incident, error) {
			var out []model.Incident
			for _, in := range s.inc {
				if in.Start.After(s.now) {
					continue
				}
				if in.End.After(s.now) {
					in.End = time.Time{}
				}
				out = append(out, in)
			}
			return out, nil
		},
		Verdict: func() model.Verdict {
			if in := s.current(); in != nil {
				return model.Verdict{Kind: in.Kind, Since: in.Start, Evidence: map[string]float64{"loss": s.loss}}
			}
			return model.Verdict{Kind: model.VerdictOK}
		},
		Layers: func() map[string]*bool {
			gw, edge, ac := true, true, true
			if in := s.current(); in != nil && in.Kind == model.VerdictISP {
				edge, ac = false, false
			}
			if in := s.current(); in != nil && in.Kind == model.VerdictUpstream && s.acDown {
				ac = false
			}
			return map[string]*bool{model.LayerGateway: &gw, model.LayerEdge: &edge, model.LayerAnycast: &ac}
		},
	}
}

// run feeds the samples of evs from start to end, ticking every 5 s, and
// calls at with a fresh snapshot at every tick from from on.
func (s *lagScene) run(t *testing.T, evs []eEvent, from, end time.Time, at func(e *engine, now time.Time)) {
	t.Helper()
	ctx := context.Background()
	e := newEngine(s.p, profile.ResolveTiles(s.p), s.sources(), Options{Replay: time.Hour})
	if err := e.replay(ctx, s.start); err != nil {
		t.Fatal(err)
	}
	e.goLive(ctx, s.start)
	arr := eGen(eSeries(e), s.start, end, evs, 7)
	i := 0
	for tk := s.start.Add(tickEvery); !tk.After(end); tk = tk.Add(tickEvery) {
		s.now = tk
		for ; i < len(arr) && !arr[i].arrive.After(tk); i++ {
			e.Observe(arr[i].s)
		}
		if tk.Before(from) {
			e.mu.Lock()
			e.tickTo(tk)
			e.mu.Unlock()
			continue
		}
		e.advance(ctx, tk)
		at(e, tk)
	}
}

// notGateway is everything past the router: the provider dropped.
func notGateway(name string) bool { return name != "Gateway" }

func tileWeb(p *model.Profile, id string) func(string) bool {
	var names []string
	for _, r := range profile.ResolveTiles(p) {
		if r.ID == id {
			for _, tg := range r.Web {
				names = append(names, tg.Name)
			}
		}
	}
	return eNames(names...)
}

func slowdown(x Episode) bool { return x.Connection && x.Verdict == "" && x.Kind == EpSlow }

// TestDropVerdictLag is netns S1: a 40 s provider drop whose isp incident
// starts in it and ends 105 s after it (the verdict's window), then, 70 s
// after the drop, Zoom's own outage; the lab's 6 s probes. Whatever the
// drop's phase in its minute:
//   - the card never goes red again once the drop is over, and its
//     Working again key never changes (B1);
//   - the incident is one problem with the drop, never a second ongoing
//     one, and the drop keeps its span (B1);
//   - no "most apps were slow" problem appears before the drop (B2);
//   - the provider's hop is Good once the path is clean, though the
//     verdict still calls it unhealthy (e);
//   - Zoom's outage, starting more than dropLate (and causeSlack) after
//     the drop, while the lagging incident goes on, is its own problem,
//     from Zoom's side (d).
func TestDropVerdictLag(t *testing.T) {
	for _, phase := range []time.Duration{0, 5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second} {
		s := newLagScene(t)
		s.fast()
		d := s.start.Add(20*time.Minute + phase)
		end := d.Add(40 * time.Second)
		s.inc = []model.Incident{{ID: 1, Kind: model.VerdictISP, Start: d.Add(9 * time.Second), End: end.Add(105 * time.Second), PeakLoss: 1}}
		zoom := window{end.Add(70 * time.Second), end.Add(130 * time.Second)}
		evs := []eEvent{{d, end, notGateway, 0}, {zoom.from, zoom.to, tileWeb(s.p, "zoom"), 0}}
		name := "phase " + phase.String()
		var againKey string
		failed := map[string]bool{}
		errorf := func(what string, now time.Time, format string, args ...any) {
			t.Helper()
			if !failed[what] {
				failed[what] = true
				t.Errorf("%s, %v after the drop: "+format, append([]any{name, now.Sub(end)}, args...)...)
			}
		}
		var last *Snapshot
		s.run(t, evs, d.Add(-time.Minute), end.Add(6*time.Minute), func(e *engine, now time.Time) {
			snap := e.Snapshot()
			last = snap
			if i := slices.IndexFunc(snap.Eps, slowdown); i >= 0 {
				errorf("slowdown", now, "a slowdown problem %+v", snap.Eps[i])
			}
			if now.Before(end.Add(20 * time.Second)) {
				return
			}
			c := snap.Conn
			if c.Tone == ToneBad {
				errorf("red", now, "card %s %s, want no red card after the drop", c.Kind, c.Key)
			}
			if c.Again != nil {
				k := strings.TrimPrefix(c.Key, ConnWorkingAgain+":")
				if c.Kind == ConnApp {
					k = c.Key[strings.LastIndex(c.Key, ":")+1:]
				}
				if againKey == "" {
					againKey = k
				} else if k != againKey {
					errorf("key", now, "working again key %s, then %s", againKey, k)
				}
			}
			for _, x := range snap.Eps {
				if x.Connection && x.Ongoing() {
					errorf("ongoing", now, "ongoing connection problem %+v", x)
				}
			}
			if now.After(end.Add(30*time.Second)) && snap.Layers[layerEdge].State != Good {
				errorf("edge", now, "provider hop %s, want good", snap.Layers[layerEdge].State)
			}
		})
		if againKey == "" {
			t.Errorf("%s: never Working again", name)
		}
		var conn, zoomEps []Episode
		for _, x := range last.Eps {
			if x.Connection {
				conn = append(conn, x)
			} else if slices.Contains(x.Tiles, "zoom") {
				zoomEps = append(zoomEps, x)
			}
		}
		switch {
		case len(conn) != 1:
			t.Errorf("%s: %d connection problems, want 1:\n%s", name, len(conn), epsText(last.Eps))
		case conn[0].Kind != EpDrop || conn[0].Verdict != model.VerdictISP || conn[0].End.After(end.Add(20*time.Second)) || conn[0].Cause.Who != WhoProvider:
			t.Errorf("%s: the drop %+v, want the provider's drop with its incident, ending by the drop's end", name, conn[0])
		}
		if len(zoomEps) != 1 || !slices.Equal(zoomEps[0].Tiles, []string{"zoom"}) || zoomEps[0].Cause.Who != WhoApp {
			t.Errorf("%s: Zoom's outage %+v, want its own problem from Zoom's side", name, zoomEps)
		}
	}
}

// TestBlipLeavesNothing is netns S1b and B6: a 20 s provider drop (too
// short for a drop) with an isp incident that lasts 100 s after it, inside
// a minute or straddling a minute boundary (B6: its lost bursts were loss
// in both minutes, calls Down for 5 minutes). From a minute after it, the
// card is good, so are the strip and every tile, and nothing is listed (a).
func TestBlipLeavesNothing(t *testing.T) {
	for _, off := range []time.Duration{15 * time.Second, 50 * time.Second} {
		s := newLagScene(t)
		s.fast()
		d := s.start.Add(20*time.Minute + off)
		end := d.Add(20 * time.Second)
		s.inc = []model.Incident{{ID: 1, Kind: model.VerdictISP, Start: d.Add(9 * time.Second), End: end.Add(100 * time.Second), PeakLoss: 1}}
		failed := map[string]bool{}
		s.run(t, []eEvent{{d, end, notGateway, 0}}, end.Add(60*time.Second), end.Add(6*time.Minute), func(e *engine, now time.Time) {
			snap := e.Snapshot()
			errorf := func(what, format string, args ...any) {
				t.Helper()
				if !failed[what] {
					failed[what] = true
					t.Errorf("drop at :%02d, %v after it: "+format, append([]any{d.Second(), now.Sub(end)}, args...)...)
				}
			}
			if snap.Conn.Kind != ConnGood {
				errorf("card", "card %s %s, want good", snap.Conn.Kind, snap.Conn.Key)
			}
			if len(snap.Eps) != 0 {
				errorf("eps", "problems:\n%s", epsText(snap.Eps))
			}
			for l, ls := range snap.Layers {
				if ls.State != Good {
					errorf("layers", "layer %d %s, want good", l, ls.State)
				}
			}
			for _, ts := range snap.Tiles {
				if ts.LastDrop != nil || ts.State != Good {
					errorf("tiles", "tile %s %s, drop %+v", ts.ID, ts.State, ts.LastDrop)
				}
			}
		})
	}
}

// TestProviderLossCard: an isp verdict from partial loss is the amber
// "losing some data" card, an outage (a drop in progress) the red one (b).
func TestProviderLossCard(t *testing.T) {
	v := model.Verdict{Kind: model.VerdictISP, Since: at(5 * time.Minute), Evidence: map[string]float64{"loss": 0.06}}
	c := connState(connIn{ready: true, now: at(10 * time.Minute), verdict: v})
	if c.Kind != string(model.VerdictISP) || !c.Lossy || c.Tone != ToneWarn || c.Icon != Slow || !strings.HasPrefix(c.Key, "isp_lossy:") {
		t.Errorf("6%% loss: %+v, want the amber lossy isp card", c)
	}
	c = connState(connIn{ready: true, now: at(10 * time.Minute), verdict: v, dropNow: &Drop{Start: at(9 * time.Minute)}})
	if c.Lossy || c.Tone != ToneBad || !strings.HasPrefix(c.Key, "isp:") {
		t.Errorf("a drop in progress: %+v, want the red isp card", c)
	}
	for _, loss := range []float64{lossyBelow, 0.4, 1} {
		v.Evidence["loss"] = loss
		if c = connState(connIn{ready: true, now: at(10 * time.Minute), verdict: v}); c.Lossy || c.Tone != ToneBad {
			t.Errorf("%.0f%% loss: %+v, want the red isp card", 100*loss, c)
		}
	}
	v.Evidence["loss"] = 0.29
	if c = connState(connIn{ready: true, now: at(10 * time.Minute), verdict: v}); !c.Lossy || c.Tone != ToneWarn {
		t.Errorf("29%% loss: %+v, want the amber card", c)
	}
	// 40% loss that just began: the 60 s window says 15%, the edge's
	// latest checks 40%
	v.Evidence["loss"] = 0.15
	if c = connState(connIn{ready: true, now: at(10 * time.Minute), verdict: v, edgeLoss: 0.4}); c.Lossy || c.Tone != ToneBad {
		t.Errorf("40%% at the edge now: %+v, want the red isp card", c)
	}
	// a lagging incident is skipped while the path has been clean since the drop
	v = model.Verdict{Kind: model.VerdictISP, Since: at(10*time.Minute + 9*time.Second), Evidence: map[string]float64{"loss": 0.2}}
	drop := Drop{Start: at(10 * time.Minute), End: at(10*time.Minute + 40*time.Second), Dur: 40 * time.Second}
	in := connIn{ready: true, now: at(13 * time.Minute), verdict: v, drops: []Drop{drop}}
	if c = connState(in); c.Kind != string(model.VerdictISP) {
		t.Errorf("not known to be clean, long after: %+v, want the isp card", c)
	}
	in.clean = func(_ model.VerdictKind, from time.Time) bool { return from.Equal(drop.End) }
	if c = connState(in); c.Kind != ConnGood {
		t.Errorf("clean since the drop: %+v, want good", c)
	}
}

// TestLossDownLifts: a call tile Down from loss alone lifts once the loss
// rule (10% in each of 2 minutes) no longer holds, to Choppy by the usual
// rule (c); Down from delay keeps the 5-minute rule.
func TestLossDownLifts(t *testing.T) {
	m := time.Minute
	seq := []State{Good, Good, Down, Down, Slow, Slow}
	for _, loss := range []bool{true, false} {
		var w word
		for i, st := range seq {
			w.stepLoss(at(time.Duration(i)*m), st, loss && st == Down)
		}
		got, since := w.state()
		switch {
		case loss && (got != Slow || !since.Equal(at(2*m))):
			t.Errorf("loss: %s since %v, want Slow since the first bad minute", got, since)
		case !loss && got != Down:
			t.Errorf("delay: %s, want still Down", got)
		}
	}
	// the first minute the loss rule (over 2 minutes) no longer finds:
	// netns S2b wants calls back within 3 minutes of the loss ending
	var w word
	for i, st := range []State{Good, Down, Down, Slow} {
		w.stepLoss(at(time.Duration(i)*m), st, st == Down)
	}
	if got, _ := w.state(); got != Slow {
		t.Errorf("one minute below: %s, want Slow", got)
	}
}

// TestBlockedTakesCallTiles is netns S10 and S10b: the call tiles go Down
// a minute before the verdict calls UDP blocked. Once it does, their
// problem is the block's (B3), and their cause is the connection's;
// Zoom's own outage (an app cause) stays apart.
func TestBlockedTakesCallTiles(t *testing.T) {
	m := time.Minute
	now := at(20 * m)
	blocked, _ := incidentEpisode(model.Incident{Kind: model.VerdictSTUNBlocked, Start: at(10*m + 62*time.Second)}, false)
	calls := []Episode{tileRun("zoom", Down, 10*m, 0), tileRun("teams", Down, 10*m, 0), tileRun("slack", Down, 10*m, 0)}
	cause := func(x Episode) Cause {
		if x.Connection {
			return Cause{Who: WhoCallsPath, Evidence: EvCallsPath}
		}
		return Cause{Who: WhoUnknown}
	}
	eps := groupEpisodes([]Episode{blocked}, calls, cause, now)
	if len(eps) != 1 || eps[0].Kind != EpBlocked || !eps[0].Start.Equal(at(10*m)) || !eps[0].Ongoing() ||
		!slices.Equal(eps[0].Tiles, []string{"zoom", "teams", "slack"}) {
		t.Fatalf("got\n%s, want one ongoing blocked problem of zoom, teams and slack", epsText(eps))
	}
	if c := tileCause(eps, "zoom", Down); c != (Cause{}) {
		t.Errorf("tile cause %+v, want the connection's (none)", c)
	}
	// before the verdict: the calls' own problem
	if eps := groupEpisodes(nil, calls, cause, now); len(eps) != 1 || eps[0].Connection {
		t.Errorf("before the verdict:\n%s", epsText(eps))
	}
	// an app's own outage stays its own
	app := func(x Episode) Cause {
		if x.Connection {
			return Cause{Who: WhoUnknown}
		}
		return Cause{Who: WhoApp, Vendor: "zoom", Evidence: EvAppOnly}
	}
	eps = groupEpisodes([]Episode{blocked}, calls[:1], app, now)
	if len(eps) != 2 || eps[0].Connection == eps[1].Connection {
		t.Errorf("app outage:\n%s, want two problems", epsText(eps))
	}
	// too long before the block: not the block's
	eps = groupEpisodes([]Episode{blocked}, []Episode{tileRun("zoom", Down, 10*m-blockedLead, 0)}, cause, now)
	if len(eps) != 2 {
		t.Errorf("long before:\n%s, want two problems", epsText(eps))
	}
}

// TestDropLateTiles: a drop absorbs the tile parts that start by its end +
// dropLate, not later ones (d); a blip goes with all it absorbed.
func TestDropLateTiles(t *testing.T) {
	m, sec := time.Minute, time.Second
	now := at(30 * m)
	drop := dropEpisode(Drop{Start: at(10 * m), End: at(10*m + 40*sec), Dur: 40 * sec}, "")
	early := tileRun("zoom", Down, 10*m+60*sec, 11*m+30*sec)
	late := tileRun("zoom", Down, 10*m+80*sec, 11*m+30*sec)
	if eps := groupEpisodes([]Episode{drop}, []Episode{early}, byVendor, now); len(eps) != 1 || !slices.Equal(eps[0].Tiles, []string{"zoom"}) {
		t.Errorf("20 s after: got\n%s, want it folded", epsText(eps))
	}
	if eps := groupEpisodes([]Episode{drop}, []Episode{late}, byVendor, now); len(eps) != 2 {
		t.Errorf("40 s after: got\n%s, want two problems", epsText(eps))
	}
	blip := drop
	blip.Kind, blip.End, blip.DropDur = epBlip, at(10*m+20*sec), 20*sec
	inc, _ := incidentEpisode(model.Incident{Kind: model.VerdictISP, Start: at(10*m + 9*sec), End: blip.End, PeakLoss: 1}, false)
	slow := broadEpisode(at(9*m+30*sec), at(11*m))
	if eps := groupEpisodes([]Episode{blip, inc, slow}, []Episode{tileRun("meet", Down, 10*m, 10*m+50*sec)}, byVendor, now); len(eps) != 0 {
		t.Errorf("blip: got\n%s, want nothing", epsText(eps))
	}
	// a lossy spell's incident, a blip inside it (netns S2b): it stays
	inc.End = at(12 * m)
	if eps := groupEpisodes([]Episode{blip, inc}, nil, byVendor, now); len(eps) != 1 || eps[0].Verdict != model.VerdictISP {
		t.Errorf("blip in a lossy spell: got\n%s, want the incident", epsText(eps))
	}
}

// TestLaggedNeedsCleanPath: the incident is the drop's lag only while the
// path has been clean since; loss after the drop makes it a problem of
// its own again.
func TestLaggedNeedsCleanPath(t *testing.T) {
	s := newLagScene(t)
	d := s.start.Add(20 * time.Minute)
	end := d.Add(40 * time.Second)
	s.inc = []model.Incident{{ID: 1, Kind: model.VerdictISP, Start: d.Add(9 * time.Second), End: end.Add(5 * time.Minute), PeakLoss: 1}}
	// a second, 10 s loss of the edge 2 minutes after the drop
	evs := []eEvent{{d, end, notGateway, 0}, {end.Add(2 * time.Minute), end.Add(2*time.Minute + 10*time.Second), eNames("ISP edge"), 0}}
	var last *Snapshot
	s.run(t, evs, end.Add(4*time.Minute), end.Add(4*time.Minute), func(e *engine, now time.Time) { last = e.Snapshot() })
	if last.Conn.Kind != string(model.VerdictISP) {
		t.Errorf("card %s, want the isp card again", last.Conn.Kind)
	}
	if !slices.ContainsFunc(last.Eps, func(x Episode) bool { return x.Connection && x.Ongoing() && x.Verdict == model.VerdictISP }) {
		t.Errorf("want the incident ongoing on its own:\n%s", epsText(last.Eps))
	}
}

// TestBlockedBeforeIncidentRead is netns S10: every STUN target stops
// answering, the call tiles go Down, and a minute later the verdict says
// udp_blocked. Its incident is read only every incEvery: from the first
// tick of the verdict the call tiles' problem is the block's, so the card
// and the tiles agree at once (B3).
func TestBlockedBeforeIncidentRead(t *testing.T) {
	s := newLagScene(t)
	var stun []string
	for _, tg := range s.p.Targets {
		if slices.Contains(tg.ProbeKinds(), model.KindUDP) {
			stun = append(stun, tg.Name)
		}
	}
	b := s.start.Add(20 * time.Minute)
	v := b.Add(62 * time.Second)
	src := s.sources()
	src.Incidents = func(context.Context, time.Time, time.Time) ([]model.Incident, error) { return nil, nil }
	src.Verdict = func() model.Verdict {
		if s.now.Before(v) {
			return model.Verdict{Kind: model.VerdictOK}
		}
		return model.Verdict{Kind: model.VerdictUDPBlocked, Since: v}
	}
	ctx := context.Background()
	e := newEngine(s.p, profile.ResolveTiles(s.p), src, Options{Replay: time.Hour})
	if err := e.replay(ctx, s.start); err != nil {
		t.Fatal(err)
	}
	e.goLive(ctx, s.start)
	end := v.Add(tickEvery)
	arr := eGen(eSeries(e), s.start, end, []eEvent{{b, end, eNames(stun...), 0}}, 3)
	i := 0
	for tk := s.start.Add(tickEvery); !tk.After(end); tk = tk.Add(tickEvery) {
		s.now = tk
		for ; i < len(arr) && !arr[i].arrive.After(tk); i++ {
			e.Observe(arr[i].s)
		}
		e.advance(ctx, tk)
	}
	snap := e.Snapshot()
	var blocked []Episode
	for _, x := range snap.Eps {
		if x.Ongoing() && slices.Contains(x.Tiles, "zoom") {
			blocked = append(blocked, x)
		}
	}
	if len(blocked) != 1 || blocked[0].Kind != EpBlocked || !blocked[0].Connection {
		t.Fatalf("the verdict's first tick: zoom's problems\n%s, want the block's", epsText(blocked))
	}
	if z := snap.Tiles[0]; z.State != Down || z.Cause != (Cause{}) {
		t.Errorf("zoom %s, cause %+v, want Down with the connection's cause (none)", z.State, z.Cause)
	}
}

// TestLaggedNeedsCleanPathNow: loss right after the drop (20% of the
// edge's checks, no run long enough for a drop) is seen at once, before
// its minute closes: the incident is no lag (netns S2b: heavy loss whose
// runs made a drop).
func TestLaggedNeedsCleanPathNow(t *testing.T) {
	s := newLagScene(t)
	d := s.start.Add(20 * time.Minute)
	end := d.Add(40 * time.Second)
	s.inc = []model.Incident{{ID: 1, Kind: model.VerdictISP, Start: d.Add(-30 * time.Second), End: end.Add(5 * time.Minute), PeakLoss: 0.2}}
	s.loss = 0.2
	evs := []eEvent{{d, end, notGateway, 0}}
	for k := range 6 {
		from := end.Add(time.Duration(5+10*k) * time.Second)
		evs = append(evs, eEvent{from, from.Add(2 * time.Second), eNames("ISP edge"), 0})
	}
	var last *Snapshot
	at := end.Add(30 * time.Second)
	s.run(t, evs, at, at, func(e *engine, now time.Time) { last = e.Snapshot() })
	if c := last.Conn; c.Kind != string(model.VerdictISP) || !c.Lossy {
		t.Errorf("card %s %s, want the lossy isp card", c.Kind, c.Key)
	}
}

// TestShortOutageNoLossDown is B6: a 10 s total loss straddling a minute
// boundary (a burst lost in each minute, over 10% of both) is no call
// Down; Choppy at most.
func TestShortOutageNoLossDown(t *testing.T) {
	const every = 5 * time.Second
	cs := pattern(every, t0, rep(".", 23)+"xx"+rep(".", 23), ms10)
	_, s := feed(every, cs)
	var ms []minute
	for i := range 3 {
		f := at(time.Duration(i) * time.Minute)
		ms = append(ms, summarize(cs, s.runs, ruleUDP, f, f.Add(80*time.Second)))
	}
	if ms[1].lost != 1 || ms[2].lost != 1 {
		t.Fatalf("minutes %+v, want a lost burst in each of the last 2", ms)
	}
	if st, _ := judgeCall(ms, every); st == Down {
		t.Errorf("one burst lost in each of 2 minutes: %s, want no Down", st)
	}
	// real loss: 2 lossy bursts or more in each minute is Down
	lossy := callMin(12, 30, 2, 240)
	if st, _ := judgeCall([]minute{lossy, lossy, lossy}, every); st != Down {
		t.Errorf("20%% loss: %s, want Down", st)
	}
}

// TestWorkingAgainOwnSpan is B7: 150 s of provider loss, with the call
// tiles Down from loss for 4 minutes folded in: Working again says the
// connection had trouble for its own 150 s, not 4 minutes down.
func TestWorkingAgainOwnSpan(t *testing.T) {
	m, sec := time.Minute, time.Second
	inc, _ := incidentEpisode(model.Incident{Kind: model.VerdictISP, Start: at(10 * m), End: at(12*m + 30*sec), PeakLoss: 0.6}, false)
	parts := []Episode{tileRun("zoom", Down, 10*m+30*sec, 14*m), tileRun("teams", Down, 10*m+30*sec, 14*m)}
	eps := groupEpisodes([]Episode{inc}, parts, byVendor, at(20*m))
	if len(eps) != 1 || !eps[0].End.Equal(at(14*m)) {
		t.Fatalf("got\n%s, want one problem widened by the tiles", epsText(eps))
	}
	r := workingAgain(eps, at(20*m))
	if r == nil || r.Variant != AgainTrouble || r.Dur != 150*sec || !r.End.Equal(at(12*m+30*sec)) {
		t.Errorf("working again %+v, want trouble for the incident's 150 s", r)
	}
}

// TestBlipLiveVsReplay: a 20 s provider blip and a 3-burst STUN run, each
// across a minute boundary: the minutes leave the path's outage out (and
// keep the STUN run as loss) alike live and in replay, checked while they
// are fresh and later.
func TestBlipLiveVsReplay(t *testing.T) {
	s := newScene(t)
	var stun []string
	for _, tg := range s.p.Targets {
		if slices.Contains(tg.ProbeKinds(), model.KindUDP) {
			stun = append(stun, tg.Name)
		}
	}
	b := s.drop.to.Add(5*time.Minute + 50*time.Second)
	u := b.Add(3 * time.Minute)
	s.evs = append(s.evs, eEvent{b, b.Add(20 * time.Second), notGateway, 0}, eEvent{u, u.Add(15 * time.Second), eNames(stun...), 0})
	arr := eGen(eSeries(s.engine(nil)), s.start, s.end, s.evs, 4)
	checks := []time.Time{b.Add(45 * time.Second).Truncate(tickEvery), u.Add(50 * time.Second).Truncate(tickEvery), s.end}
	s.live(t, arr, checks, func(a *engine, now time.Time) {
		sameEngines(t, now.Format("15:04:05"), a, s.replayed(t, stored(arr, now), now))
	})
}

// TestRecentFrom: the card's edge window starts at the incident (less
// lagSlack) when that is within edgeRecent.
func TestRecentFrom(t *testing.T) {
	now := at(10 * time.Minute)
	if got := recentFrom(now, now.Add(-2*time.Second)); !got.Equal(now.Add(-12 * time.Second)) {
		t.Errorf("young incident: %v", got)
	}
	if got := recentFrom(now, now.Add(-5*time.Minute)); !got.Equal(now.Add(-edgeRecent)) {
		t.Errorf("old incident: %v", got)
	}
}
