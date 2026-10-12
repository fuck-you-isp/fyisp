package web

import (
	"context"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/profile"
	"github.com/fuck-you-isp/fyisp/internal/speedtest"
	"github.com/fuck-you-isp/fyisp/internal/tiles"
)

// /api/home2 is the new Home (v0.9.0; it becomes /api/home when the old
// Home is removed): part=now, the answer, the strip, the 11 tiles, the speed
// and the problems of the week, from the tiles engine's snapshot; part=tile,
// a tile's panel. See docs/HOME-API.md.
//
// part=now is never cached as a whole: it is rendered from the snapshot of
// the engine's latest tick, with the parts that depend on the viewer (the
// Wi-Fi wording, can_control) decided per request. Only the problems block
// (by the snapshot's Rev, the zone and the local date) and the stored speed
// tests (for home2SpeedTTL, or until a test starts or ends) are kept.
// part=tile keeps only its chart block, per tile, zone and 5-minute bucket;
// its header follows the snapshot like the tile does.

const (
	home2Week     = 7 * 24 * time.Hour
	home2SpeedTTL = time.Minute
	home2Stale    = 2 * time.Minute // the engine's staleAfter
	home2Bucket   = 5 * time.Minute // the chart's step: its cache bucket
	home2Memo     = 32              // problems blocks and charts kept
)

// home2Params is a parsed /api/home2 query.
type home2Params struct {
	part, id string
	zone     zoneParam
}

func parseHome2(q url.Values, now time.Time) (home2Params, error) {
	p := home2Params{part: q.Get("part"), id: q.Get("id")}
	if err := checkParams(q, "part", "id", "tz"); err != nil {
		return p, err
	}
	switch p.part {
	case "now":
		if q.Has("id") {
			return p, badReq("id is for part=tile")
		}
	case "tile":
		if p.id == "" {
			return p, badReq("part=tile needs id")
		}
	default:
		return p, badReq("bad part %q: want now or tile", p.part)
	}
	z, err := parseZone(q.Get("tz"), now)
	if err != nil {
		return p, err
	}
	p.zone = z
	return p, nil
}

// home2SpeedSource is what Home's speed line reads: s.d.Speed, or a test's
// stand-in.
type home2SpeedSource interface {
	// On reports scheduled tests (the first one runs within the hour).
	On() bool
	Tests(ctx context.Context, from, to time.Time) ([]model.SpeedTest, error)
	Running() *speedtest.Entry
}

type speedOf struct{ SpeedSource }

func (s speedOf) On() bool { return s.State().Mode != speedtest.ModeOff }
func (s speedOf) Tests(ctx context.Context, from, to time.Time) ([]model.SpeedTest, error) {
	return s.SpeedTests(ctx, from, to)
}
func (s speedOf) Running() *speedtest.Entry { return s.Queue().State().Running }

// home2Cache keeps what part=now and part=tile may reuse.
type home2Cache struct {
	mu     sync.Mutex
	probs  map[string]Home2Problems
	charts map[string]home2Chart
	// the stored speed tests of the week, read at speedAt with speedRun
	// running (0: none)
	speedAt    time.Time
	speedRun   uint64
	speedTests []model.SpeedTest
	// the profile's tiles (for the panels' coverage)
	prof  *model.Profile
	tiles []profile.ResolvedTile
}

func newHome2Cache() *home2Cache {
	return &home2Cache{probs: map[string]Home2Problems{}, charts: map[string]home2Chart{}}
}

// resolved returns the tiles of p, resolved once per profile.
func (hc *home2Cache) resolved(p *model.Profile) []profile.ResolvedTile {
	hc.mu.Lock()
	defer hc.mu.Unlock()
	if p != hc.prof || hc.tiles == nil {
		hc.prof, hc.tiles = p, nil
		if p != nil {
			hc.tiles = profile.ResolveTiles(p)
		}
	}
	return hc.tiles
}

// memo returns m[key], filling it with fill on a miss; a full map is
// emptied first.
func memo[V any](mu *sync.Mutex, m map[string]V, key string, fill func() V) V {
	mu.Lock()
	v, ok := m[key]
	mu.Unlock()
	if ok {
		return v
	}
	v = fill()
	mu.Lock()
	if len(m) >= home2Memo {
		clear(m)
	}
	m[key] = v
	mu.Unlock()
	return v
}

// serveHome2 answers /api/home2.
func (s *server) serveHome2(w http.ResponseWriter, r *http.Request) {
	now := s.now()
	p, err := parseHome2(r.URL.Query(), now)
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, err.Error())
		return
	}
	if s.d.Tiles == nil {
		// No tiles engine: the page keeps the old Home.
		notFound(w, r)
		return
	}
	if s.d.Speed != nil {
		s.d.Speed.NoteZone(p.zone.name)
	}
	if p.part == "tile" {
		t, code, msg := s.home2Tile(p, now)
		if t == nil {
			writeErr(w, r, code, msg)
			return
		}
		writeJSON(w, r, http.StatusOK, t)
		return
	}
	writeJSON(w, r, http.StatusOK, s.home2Now(r, p, now))
}

// home2Snap is the engine's snapshot; before the engine's first tick, a
// checking one.
func (s *server) home2Snap(now time.Time) (*tiles.Snapshot, bool) {
	snap := s.d.Tiles.Snapshot()
	checking := !s.d.Tiles.Ready()
	if snap == nil {
		snap = &tiles.Snapshot{At: now, Conn: tiles.ConnState{Kind: tiles.ConnChecking, Key: tiles.ConnChecking}}
		for i, d := range profile.Tiles {
			snap.Tiles[i] = tiles.TileSnap{ID: d.ID, State: tiles.None}
		}
		checking = true
	}
	return snap, checking || snap.Conn.Kind == tiles.ConnChecking
}

// wifiWording reports the Wi-Fi wording for r: fyisp's own uplink is
// Wi-Fi (never inside a container, which sees its own virtual link) and r
// comes from this machine. Decided per request; never cached or logged.
func (s *server) wifiWording(r *http.Request) bool {
	if s.d.Path == nil || s.d.Path().Conn != model.ConnWiFi {
		return false
	}
	return (s.inContainer == nil || !s.inContainer()) && s.sameMachine != nil && s.sameMachine(r)
}

// home2Now builds part=now.
func (s *server) home2Now(r *http.Request, p home2Params, now time.Time) *Home2Now {
	snap, checking := s.home2Snap(now)
	c := h2copy{loc: p.zone.loc, now: now, wifi: s.wifiWording(r)}
	stale := !checking && (snap.Conn.Kind == tiles.ConnStale || (!snap.LastMeasured.IsZero() && now.Sub(snap.LastMeasured) >= home2Stale))
	learning := snap.Learning && !checking
	out := &Home2Now{V: 2, At: now.UnixMilli(), Stale: stale, Monitored: snap.Monitored}
	if !snap.LastMeasured.IsZero() {
		out.LastCheck = snap.LastMeasured.UnixMilli()
	}
	if !snap.FirstData.IsZero() {
		out.WatchingSince = snap.FirstData.UnixMilli()
		out.Footer = "Watching your connection since " + c.watching(snap.FirstData)
	}
	if learning && !snap.FirstData.IsZero() {
		out.Learning = &Home2Learning{Until: snap.FirstData.Add(24 * time.Hour).UnixMilli(),
			Text: "fyisp is still learning what’s usual for your connection. This takes about a day. Until then it only flags big problems, so small slowdowns won’t show yet."}
	}
	var eps []tiles.Episode
	if !checking {
		eps = s.d.Tiles.Problems(now.Add(-home2Week))
	}
	byID := make(map[string]tiles.Episode, len(eps))
	for _, e := range eps {
		byID[e.ID] = e
	}
	for _, t := range snap.Tiles {
		out.Tiles = append(out.Tiles, c.tile(t, byID, checking))
	}
	out.Answer = c.answer(snap, eps, checking, stale, learning)
	out.Path = c.path(snap, checking, stale)
	if !c.wifi {
		out.PathHint = "fyisp checks from the box it runs on, up to your router. It can’t see the Wi-Fi on the device you’re using now — if calls are choppy here, try moving closer to your router or plugging in."
	}
	out.Speed = s.home2Speed(r, c, snap.Conn, now)
	switch {
	case checking:
		out.Problems = Home2Problems{Summary: "Checking…", Items: []Home2Problem{}}
	default:
		key := strings.Join([]string{strconv.FormatUint(snap.Rev, 16), p.zone.key, now.In(p.zone.loc).Format(time.DateOnly),
			strconv.FormatBool(learning), strconv.FormatInt(snap.FirstData.UnixMilli(), 10)}, "\x00")
		out.Problems = memo(&s.h2.mu, s.h2.probs, key, func() Home2Problems { return c.problems(eps, learning, snap.FirstData) })
	}
	return out
}

// watching is the footer's date: "Mar 3", "today 9:50 am".
func (c h2copy) watching(t time.Time) string {
	switch n := c.daysAgo(t); {
	case n <= 1:
		return c.dayIn(t) + " " + c.clock(t)
	case c.in(t).Year() != c.in(c.now).Year():
		return c.in(t).Format("Jan 2, 2006")
	}
	return c.in(t).Format("Jan 2")
}

func iconOf(st tiles.State) string {
	switch st {
	case tiles.Good, tiles.Slow, tiles.Down:
		return string(st)
	}
	return "none"
}

// answer is the card (tiles.ConnState's rows, worded).
func (c h2copy) answer(snap *tiles.Snapshot, eps []tiles.Episode, checking, stale, learning bool) Home2Answer {
	cs := snap.Conn
	a := Home2Answer{Key: cs.Key, Tone: cs.Tone, Icon: iconOf(cs.Icon)}
	if !learning {
		a.Day = c.dayLine(snap.Day)
	}
	started := func() string { return " Started at " + c.when(cs.Since) + "." }
	switch {
	case checking:
		return Home2Answer{Key: tiles.ConnChecking, Tone: tiles.ToneChecking, Icon: "none", Tab: "Checking",
			Headline: "Checking your connection…", Sub: "fyisp just started. The first answer takes about a minute."}
	case stale:
		a.Key, a.Tone, a.Icon, a.Tab = tiles.ConnStale, tiles.ToneStale, "none", "? Not checking"
		if snap.LastMeasured.IsZero() {
			a.Headline, a.Sub = "fyisp hasn’t checked anything yet.", "Is the computer it runs on asleep or turned off?"
			return a
		}
		at := c.when(snap.LastMeasured)
		a.Headline = "fyisp hasn’t checked since " + at + "."
		a.Sub = "Is the computer it runs on asleep or turned off? Everything below is from " + at + " and may be out of date."
		return a
	}
	switch cs.Kind {
	case string(model.VerdictLAN), string(model.VerdictISP), string(model.VerdictUpstream), string(model.VerdictNoNetwork), tiles.ConnDrop:
		a.Headline, a.Sub, a.Tab = c.pathCard(cs.Kind, cs.Since)
	case string(model.VerdictDNS):
		a.Headline = "Websites can’t be found right now."
		a.Sub = "Your connection is up, but name lookups (DNS) are failing, so apps can’t find their servers." + started()
		a.Tab = "! Websites not found"
	case string(model.VerdictUDPBlocked):
		if cs.Quiet {
			a.Icon = "info" // known for a long time: information, not a warning
			a.Headline = "Calls can’t use their fast path on this network."
			a.Sub = "This network blocks the kind of traffic calls prefer, so calls fall back to a slower route. Websites and apps are fine."
			a.Tab = "! Calls limited"
			break
		}
		a.Headline = "Calls may not connect on this network."
		a.Sub = "This network is blocking the kind of traffic calls use, so calls may fail or fall back to a slower route. Websites and apps are fine." + started()
		a.Tab = "! Calls blocked"
	case string(model.VerdictUDPDegraded):
		a.Headline = "Calls may be choppy right now."
		a.Sub = "Calls are losing some data between your home and the internet, while websites and apps are fine." + started()
		a.Tab = "! Calls choppy"
	case tiles.ConnBroad:
		a.Headline = "Most apps are slower than usual."
		a.Sub = "Your router and your internet provider look fine, but most apps are slow at the same time." + started()
		a.Tab = "! Apps slow"
	case tiles.ConnApp:
		var bad []tiles.TileSnap
		for _, id := range cs.Tiles {
			if i := slices.IndexFunc(snap.Tiles[:], func(t tiles.TileSnap) bool { return t.ID == id }); i >= 0 {
				bad = append(bad, snap.Tiles[i])
			}
		}
		if len(bad) == 0 {
			break
		}
		var causes []tiles.Cause
		for _, t := range bad {
			causes = append(causes, causeOf(t, eps))
		}
		a.Headline, a.Sub, a.Tab = c.appCard(bad, causes, snap.Tiles[:])
		if cs.Again != nil {
			a.Sub += " " + c.againSentence(cs.Again)
		}
		return a
	case tiles.ConnWorkingAgain:
		g := cs.Again
		a.Headline = c.againSentence(g)
		a.Sub = "Likely cause: " + c.cause(g.Cause) + " — " + c.evidence(g.Cause, false) + "."
		switch g.Variant {
		case tiles.AgainDropped:
			a.Sub += " If a call cut out around then, this is why."
			a.Tab = "! Dropped at " + c.in(g.Start).Format("3:04")
		case tiles.AgainDown:
			a.Tab = "! Was down at " + c.in(g.Start).Format("3:04")
		default:
			a.Tab = "! Trouble at " + c.in(g.Start).Format("3:04")
		}
		return a
	}
	if a.Headline != "" {
		return a
	}
	calls := slices.ContainsFunc(snap.Tiles[:], func(t tiles.TileSnap) bool {
		return t.State != tiles.Off && tileDef(t.ID).Kind == profile.TileCall
	})
	a.Key, a.Tone, a.Icon, a.Tab = tiles.ConnGood, tiles.ToneOK, "good", "✓ All good"
	a.Headline = "Everything you use is working normally."
	a.Sub = c.healthy(snap.Monitored, calls, learning, snap.FirstData)
	return a
}

// causeOf is a bad tile's likely cause: its own (the app's side), else
// its newest ongoing problem's.
func causeOf(t tiles.TileSnap, eps []tiles.Episode) tiles.Cause {
	if t.Cause.Who != "" {
		return t.Cause
	}
	for _, e := range eps {
		if e.Ongoing() && slices.Contains(e.Tiles, t.ID) {
			return e.Cause
		}
	}
	return tiles.Cause{}
}

// path is the connection strip: the router, the provider, the internet.
func (c h2copy) path(snap *tiles.Snapshot, checking, stale bool) []Home2Hop {
	labels := [3]string{c.routerName(), "Your internet provider", "The internet"}
	ids := [3]string{"router", "provider", "internet"}
	var out [3]Home2Hop
	for i := range out {
		out[i] = Home2Hop{ID: ids[i], Label: labels[i]}
		if checking {
			out[i].State, out[i].Sub = "unknown", "Checking…"
			continue
		}
		l := snap.Layers[i]
		switch l.State {
		case tiles.Good:
			out[i].State, out[i].Sub = "good", "Working normally"
		case tiles.Slow:
			out[i].State, out[i].Sub = "slow", "Having some trouble"
		case tiles.Down:
			out[i].State, out[i].Sub = "down", "Not answering "+c.since(l.Since)
		default:
			out[i].State = "unknown"
			out[i].Sub = [3]string{"Can’t see your router from here", "Can’t see your provider from here", "Not checked yet"}[i]
		}
	}
	if checking {
		return out[:]
	}
	cs := snap.Conn
	culprit := -1
	switch cs.Kind {
	case string(model.VerdictLAN), string(model.VerdictNoNetwork):
		culprit = 0
	case string(model.VerdictISP):
		culprit = 1
	case string(model.VerdictUpstream):
		culprit = 2
	case tiles.ConnDrop:
		culprit = slices.IndexFunc(snap.Layers[:], func(l tiles.LayerSnap) bool { return l.State == tiles.Down })
	}
	through := [3]string{"Hard to reach through your router", "Hard to reach through your provider"}
	switch {
	case cs.Kind == string(model.VerdictNoNetwork):
		out[0].State, out[0].Sub = "down", "No network connection "+c.since(cs.Since)
		for i := 1; i < 3; i++ {
			out[i].State, out[i].Sub = "unknown", "Can’t check while offline"
		}
	case culprit >= 0:
		since := cs.Since
		switch {
		case snap.Layers[culprit].State == tiles.Down:
			out[culprit].Sub = "Not answering " + c.since(since)
		case culprit == 2:
			out[culprit].Sub = "Hard to reach " + c.since(since)
		default:
			out[culprit].Sub = "Losing data " + c.since(since)
		}
		out[culprit].State = "down"
		for i := culprit + 1; i < 3; i++ {
			out[i].State, out[i].Sub = "slow", through[culprit]
		}
	default:
		// The newest drop shows where it happened: on the first layer that lost everything.
		for i, l := range snap.Layers {
			if l.LastDrop != nil && out[i].State == "good" {
				out[i].Sub = "Dropped for " + short(l.LastDrop.Dur) + " at " + c.when(l.LastDrop.Start) + ", fine now"
				break
			}
		}
	}
	if stale {
		for i := range out {
			if out[i].Sub == "Working normally" {
				out[i].Sub = "Last known: working"
			} else {
				out[i].Sub = "Last known: " + strings.ToLower(out[i].Sub[:1]) + out[i].Sub[1:]
			}
		}
	}
	return out[:]
}

// tile is one tile of part=now.
func (c h2copy) tile(t tiles.TileSnap, eps map[string]tiles.Episode, checking bool) Home2Tile {
	d := tileDef(t.ID)
	st := t.State
	if checking && st != tiles.Off {
		st = tiles.None
	}
	out := Home2Tile{ID: d.ID, Name: d.Name, Kind: string(d.Kind), State: string(st), Word: c.word(st, d.Kind), Bars: []Home2Bar{}}
	t.State = st
	out.Reason = c.tileReason(d, t, checking)
	switch {
	case st == tiles.Off:
		out.Aria = d.Name + ": not checked on this setup"
		out.EnableHint = c.enableHint(d)
		return out
	case st == tiles.None:
		out.Aria = d.Name + ": no data yet"
	case st.Bad():
		out.Since = ms(t.Since)
		if !t.Since.IsZero() {
			out.Note = c.since(t.Since)
		}
		out.Aria = d.Name + ": " + out.Word
		if out.Note != "" {
			out.Aria += ", " + out.Note
		}
	default:
		out.Aria = d.Name + ": Good"
		if t.LastDrop != nil {
			out.Note = c.dropNote(t.LastDrop)
			out.Aria += ", d" + out.Note[1:]
		}
	}
	for _, b := range t.Bars {
		if b.From.IsZero() {
			continue
		}
		s := b.State
		if checking || (s != tiles.Good && s != tiles.Slow && s != tiles.Down) {
			s = tiles.None
		}
		out.Bars = append(out.Bars, Home2Bar{From: b.From.UnixMilli(), State: string(s), Note: c.barNote(b, s, d.Kind, eps)})
	}
	if !checking {
		out.BarsSummary = c.barsSummary(t.Bars[:])
	}
	return out
}

// barNote is a bar's note: "Dropped 40 s", "Down since 10:05 am",
// "Choppy 9:41–9:45 am — likely Slack’s side", "Slow during a speed
// test"; "" when the word says it.
func (c h2copy) barNote(b tiles.Bar, st tiles.State, kind profile.TileKind, eps map[string]tiles.Episode) string {
	if !st.Bad() {
		return ""
	}
	w := c.word(st, kind)
	switch e, ok := eps[b.Episode]; {
	case st == tiles.Down && b.Drop > 0:
		return "Dropped " + short(b.Drop)
	case b.Speedtest:
		return w + " during a speed test"
	case !ok:
		return ""
	case e.Ongoing():
		return w + " " + c.since(e.Start)
	default:
		return w + " " + c.span(e.Start, e.End) + c.likely(e.Cause)
	}
}

// likely is " — likely Slack’s side" ("" when unclear).
func (c h2copy) likely(x tiles.Cause) string {
	if x.Who == "" || x.Who == tiles.WhoUnknown {
		return ""
	}
	return " — likely " + c.cause(x)
}

// problems is the problems block.
func (c h2copy) problems(eps []tiles.Episode, learning bool, first time.Time) Home2Problems {
	out := Home2Problems{Summary: c.problemsSummary(eps, learning, first), Items: []Home2Problem{}}
	for _, e := range eps {
		p := Home2Problem{ID: e.ID, From: e.Start.UnixMilli(), Ongoing: e.Ongoing(), Level: string(e.Sev),
			When: c.problemWhen(e), What: c.problemWhat(e), Cause: c.cause(e.Cause), CauseKind: e.Cause.Kind(),
			Evidence: capFirst(c.evidence(e.Cause, e.Ongoing())) + "."}
		if e.Kind == tiles.EpBlocked && p.CauseKind == tiles.KindOther {
			// No path layer explains a block: the network does it.
			p.Cause, p.Evidence = "the network blocking call traffic", "Websites and apps were fine; only calls couldn’t use their usual route."
			if e.Ongoing() {
				p.Evidence = "Websites and apps are fine; only calls can’t use their usual route."
			}
		}
		if !e.Ongoing() {
			p.To = e.End.UnixMilli()
		}
		if p.Level != string(tiles.Down) {
			p.Level = string(tiles.Slow)
		}
		out.Items = append(out.Items, p)
	}
	return out
}

// ---------- speed ----------

// home2Speed is the speed line.
func (s *server) home2Speed(r *http.Request, c h2copy, cs tiles.ConnState, now time.Time) Home2Speed {
	out := Home2Speed{EmptyText: "No speed test yet."}
	src := s.h2speed
	if src == nil && s.d.Speed != nil {
		src = speedOf{s.d.Speed}
	}
	if src == nil {
		return out
	}
	out.Available = true
	out.CanControl = s.canControl != nil && s.canControl(r)
	if src.On() {
		out.EmptyText = "No speed test yet. The first one runs within the hour."
	}
	run := src.Running()
	var runID uint64
	if run != nil {
		runID = run.ID
		out.Running = &Home2SpeedRunning{Entry: run.ID, ExpectedEnd: run.EstFinish.UnixMilli()}
	}
	hc := s.h2
	hc.mu.Lock()
	ts := hc.speedTests
	fresh := !hc.speedAt.IsZero() && hc.speedRun == runID && !now.Before(hc.speedAt) && now.Sub(hc.speedAt) < home2SpeedTTL
	hc.mu.Unlock()
	if !fresh {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		xs, err := src.Tests(ctx, now.Add(-home2Week), now)
		cancel()
		if err != nil {
			s.d.log().Warn("home: reading the speed tests", "err", err)
			return out
		}
		ts = xs
		hc.mu.Lock()
		hc.speedAt, hc.speedRun, hc.speedTests = now, runID, xs
		hc.mu.Unlock()
	}
	// The OK tests with both directions, newest last.
	var ok []model.SpeedTest
	for _, t := range ts {
		if t.Outcome == model.SpeedOK && t.DownBps != nil && t.UpBps != nil && *t.DownBps > 0 && *t.UpBps > 0 {
			ok = append(ok, t)
		}
	}
	slices.SortStableFunc(ok, func(a, b model.SpeedTest) int { return a.Start.Compare(b.Start) })
	if len(ok) == 0 {
		return out
	}
	last := ok[len(ok)-1]
	if len(ok) > 3 {
		var down, up []float64
		for _, t := range ok[:len(ok)-1] {
			down, up = append(down, *t.DownBps/1e6), append(up, *t.UpBps/1e6)
		}
		out.Usual = &Home2SpeedUsual{DownMbps: roundMbps(medianOf(down)), UpMbps: roundMbps(medianOf(up))}
	}
	dn, up := roundMbps(*last.DownBps/1e6), roundMbps(*last.UpBps/1e6)
	during := cs.Tone == tiles.ToneBad && !cs.Since.IsZero() && !last.Start.Before(cs.Since)
	out.Latest = &Home2SpeedResult{At: last.Start.UnixMilli(), DownMbps: dn, UpMbps: up,
		Text: c.speedText(dn, up, out.Usual), Note: c.speedNote(last.Start, during)}
	return out
}

func roundMbps(v float64) float64 {
	if v < 10 {
		return math.Round(v*10) / 10
	}
	return math.Round(v)
}

func medianOf(v []float64) float64 {
	v = slices.Clone(v)
	slices.Sort(v)
	n := len(v)
	if n%2 == 1 {
		return v[n/2]
	}
	return (v[n/2-1] + v[n/2]) / 2
}

// ---------- part=tile ----------

// home2Tile builds part=tile: nil with the status and message of a 404.
func (s *server) home2Tile(p home2Params, now time.Time) (*Home2TileDetail, int, string) {
	i := slices.IndexFunc(profile.Tiles, func(d profile.TileDef) bool { return d.ID == p.id })
	if i < 0 {
		return nil, http.StatusNotFound, "unknown tile"
	}
	snap, checking := s.home2Snap(now)
	t := snap.Tiles[i]
	if t.State == tiles.Off {
		return nil, http.StatusNotFound, "tile not monitored"
	}
	c := h2copy{loc: p.zone.loc, now: now}
	d := tileDef(p.id)
	h := c.tile(t, nil, checking)
	out := &Home2TileDetail{V: 2, At: now.UnixMilli(), ID: d.ID, State: h.State, Word: h.Word, Reason: h.Reason, Metrics: []Home2Metric{}}
	if rs := s.h2.resolved(s.d.Profile()); i < len(rs) {
		src := ""
		if t.Call != nil {
			src = t.Call.Source
		}
		out.Coverage = c.coverage(rs[i], src)
	}
	if !checking {
		out.Metrics = c.metrics(d, t)
	}
	learning := snap.Learning && !checking
	key := strings.Join([]string{p.id, p.zone.key, strconv.FormatInt(now.Truncate(home2Bucket).UnixMilli(), 10), strconv.FormatBool(learning)}, "\x00")
	ch, ok := s.h2Chart(key, func() (home2Chart, bool) {
		x, ok := s.d.Tiles.Chart(p.id)
		if !ok {
			return home2Chart{}, false
		}
		eps := map[string]tiles.Episode{}
		for _, e := range s.d.Tiles.Problems(x.From) {
			eps[e.ID] = e
		}
		return c.chart(x, d.Kind, eps, snap, learning), true
	})
	if !ok {
		return nil, http.StatusNotFound, "tile not monitored"
	}
	out.home2Chart = ch
	return out, 0, ""
}

// h2Chart is the cached chart block of key.
func (s *server) h2Chart(key string, fill func() (home2Chart, bool)) (home2Chart, bool) {
	hc := s.h2
	hc.mu.Lock()
	ch, ok := hc.charts[key]
	hc.mu.Unlock()
	if ok {
		return ch, true
	}
	ch, ok = fill()
	if !ok {
		return ch, false
	}
	hc.mu.Lock()
	if len(hc.charts) >= home2Memo {
		clear(hc.charts)
	}
	hc.charts[key] = ch
	hc.mu.Unlock()
	return ch, true
}

// metrics are the panel's figures: a call tile's delay, jitter and loss
// against their limits; an app's usual and Slow limit. None while the
// usual is learned (call tiles keep theirs: their limits are fixed).
func (c h2copy) metrics(d profile.TileDef, t tiles.TileSnap) []Home2Metric {
	out := []Home2Metric{}
	switch d.Kind {
	case profile.TileCall:
		m := t.Call
		if m == nil || m.Source == tiles.CallTCPOnly {
			return out
		}
		delay, jitter, loss := "—", "—", "—"
		bd, bj, bl := false, false, false
		switch {
		case m.Bursts > 0:
			if m.DelayMs > 0 {
				delay, bd = msText(m.DelayMs), m.DelayMs > 150
			}
			if m.HasJitter {
				jitter, bj = msText(m.JitterMs), m.JitterMs > 30
			}
			loss, bl = pctText(m.LossPct), m.LossPct >= 2
		case t.State == tiles.Down:
			loss, bl = "100%", true
		}
		return []Home2Metric{{Label: "Delay", Value: delay, Limit: "choppy over 150 ms", Bad: &bd},
			{Label: "Jitter", Value: jitter, Limit: "over 30 ms", Bad: &bj},
			{Label: "Packet loss", Value: loss, Limit: "2% or more", Bad: &bl}}
	case profile.TileStream:
		if t.UsualMs == nil {
			return out
		}
		return []Home2Metric{{Label: "Usual", Value: msText(*t.UsualMs)}, {Label: "Only marked Down if it stops loading"}}
	}
	if t.UsualMs == nil || t.SlowAboveMs == nil {
		return out
	}
	return []Home2Metric{{Label: "Usual", Value: msText(*t.UsualMs)}, {Label: "Slow above", Value: msText(*t.SlowAboveMs)}}
}

// msText is "18 ms".
func msText(v float64) string {
	if v < 1 {
		return "under 1 ms"
	}
	return strconv.FormatFloat(math.Round(v), 'f', 0, 64) + " ms"
}

// pctText is "0%", "2.8%", "12%".
func pctText(v float64) string {
	if v >= 10 || v == math.Round(v) {
		return strconv.FormatFloat(math.Round(v), 'f', 0, 64) + "%"
	}
	return strconv.FormatFloat(math.Round(v*10)/10, 'f', -1, 64) + "%"
}

// chart is part=tile's chart block.
func (c h2copy) chart(x *tiles.Chart, kind profile.TileKind, eps map[string]tiles.Episode, snap *tiles.Snapshot, learning bool) home2Chart {
	out := home2Chart{From: x.From.UnixMilli(), Step: x.Step.Milliseconds(), Ms: make([]*float64, len(x.Ms)), Bands: []Home2Band{}}
	for i, v := range x.Ms {
		if !math.IsNaN(v) && !math.IsInf(v, 0) {
			r := math.Round(v*10) / 10
			out.Ms[i] = &r
		}
	}
	head := "Still learning what’s usual."
	if x.UsualMs != nil && !learning {
		u := math.Round(*x.UsualMs*10) / 10
		out.UsualMs = &u
		head = "Usually about " + msText(u) + "."
	}
	var texts []string
	for _, b := range x.Bands {
		hb := Home2Band{From: b.From.UnixMilli(), To: b.To.UnixMilli(), Kind: b.Kind}
		if b.To.IsZero() {
			hb.To = snap.At.UnixMilli()
		}
		switch b.Kind {
		case tiles.BandOff:
			if !b.From.After(x.From) && !snap.FirstData.IsZero() && !snap.FirstData.Before(x.From) {
				hb.Text = "Not watched before " + c.clock(snap.FirstData)
			} else {
				hb.Text = "fyisp wasn’t running " + c.span(b.From, b.To)
			}
		default:
			w := c.word(b.Sev, kind)
			e, ok := eps[b.Episode]
			switch {
			case ok && e.Kind == tiles.EpDrop && !e.Ongoing():
				hb.Text = "Dropped " + short(e.DropDur) + " at " + c.clock(e.Start)
			case b.To.IsZero():
				hb.Text = w + " " + c.since(b.From)
			default:
				hb.Text = w + " " + c.span(b.From, b.To)
			}
			hb.Text += c.likely(b.Cause)
		}
		out.Bands = append(out.Bands, hb)
		texts = append(texts, hb.Text)
	}
	if len(texts) == 0 {
		out.Text = head + " No problems in the last 24 hours."
	} else {
		out.Text = head + " " + strings.Join(texts, ". ") + "."
	}
	return out
}
