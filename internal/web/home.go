package web

import (
	"context"
	"hash/fnv"
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

// /api/home is the Home (v0.9.0): part=now, the answer, the strip, the 11
// tiles, the speed and the problems of the week, from the tiles engine's
// snapshot; part=tile, a tile's panel. See docs/HOME-API.md.
//
// part=now is never cached as a whole: it is rendered from the snapshot of
// the engine's latest tick, with the parts that depend on the viewer (the
// Wi-Fi wording, can_control) decided per request. Only the problems block
// (by the snapshot's Rev, the zone and the local date) and the stored speed
// tests (for homeSpeedTTL, or until a test starts or ends) are kept.
// part=tile keeps only its chart block, per tile, zone and 5-minute bucket;
// its header follows the snapshot like the tile does.

const (
	homeWeek     = 7 * 24 * time.Hour
	homeSpeedTTL = time.Minute
	homeStale    = 2 * time.Minute // the engine's staleAfter
	homeBucket   = 5 * time.Minute // the chart's step: its cache bucket
	homeMemo     = 32              // problems blocks and charts kept
)

// homeParams is a parsed /api/home query.
type homeParams struct {
	part, id string
	zone     zoneParam
}

func parseHome(q url.Values, now time.Time) (homeParams, error) {
	p := homeParams{part: q.Get("part"), id: q.Get("id")}
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

// homeSpeedSource is what Home's speed block reads: s.d.Speed, or a test's
// stand-in.
type homeSpeedSource interface {
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

// homeCache keeps what part=now and part=tile may reuse.
type homeCache struct {
	mu     sync.Mutex
	probs  map[string]HomeProblems
	charts map[string]homeChart
	// the stored speed tests of the week, read at speedAt with speedRun
	// running (0: none)
	speedAt    time.Time
	speedRun   uint64
	speedTests []model.SpeedTest
	// the profile's tiles (for the panels' coverage)
	prof  *model.Profile
	tiles []profile.ResolvedTile
	// the store's oldest sample (the footer), read at oldestAt
	oldestAt time.Time
	oldest   time.Time
}

func newHomeCache() *homeCache {
	return &homeCache{probs: map[string]HomeProblems{}, charts: map[string]homeChart{}}
}

// resolved returns the tiles of p, resolved once per profile.
func (hc *homeCache) resolved(p *model.Profile) []profile.ResolvedTile {
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

// memo returns m[key], filling it with fill on a miss (ok false: not kept);
// a full map loses one entry first.
func memo[V any](mu *sync.Mutex, m map[string]V, key string, fill func() (V, bool)) (V, bool) {
	mu.Lock()
	v, ok := m[key]
	mu.Unlock()
	if ok {
		return v, true
	}
	if v, ok = fill(); !ok {
		return v, false
	}
	mu.Lock()
	for k := range m {
		if len(m) < homeMemo {
			break
		}
		delete(m, k)
	}
	m[key] = v
	mu.Unlock()
	return v, true
}

// serveHome answers /api/home.
func (s *server) serveHome(w http.ResponseWriter, r *http.Request) {
	now := s.now()
	p, err := parseHome(r.URL.Query(), now)
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, err.Error())
		return
	}
	if s.d.Tiles == nil {
		// No tiles engine (only in tests and tools): no Home.
		notFound(w, r)
		return
	}
	if p.part == "tile" {
		t, code, msg := s.homeTile(p, now)
		if t == nil {
			writeErr(w, r, code, msg)
			return
		}
		writeJSON(w, r, http.StatusOK, t)
		return
	}
	writeJSON(w, r, http.StatusOK, s.homeNow(r, p, now))
}

// homeSnap is the engine's snapshot; before the engine's first tick, a
// checking one.
func (s *server) homeSnap(now time.Time) (*tiles.Snapshot, bool) {
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

// homeState is the snapshot with the page's checking and stale: a
// process that started less than homeStale ago is checking, not stale
// (its last stored sample can be hours old), and so is one whose covered
// tiles have no word yet (their first minutes haven't closed).
func (s *server) homeState(now time.Time) (snap *tiles.Snapshot, checking, stale bool) {
	snap, checking = s.homeSnap(now)
	stale = !checking && (snap.Conn.Kind == tiles.ConnStale || (!snap.LastMeasured.IsZero() && now.Sub(snap.LastMeasured) >= homeStale))
	if stale && s.d.Status != nil && now.Sub(s.d.Status().Started) < homeStale {
		stale, checking = false, true
	}
	checking = checking || !stale && noData(snap)
	return snap, checking, stale
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

// noData reports covered tiles none of which has a word yet: right after
// the replay, until their first minutes close.
func noData(snap *tiles.Snapshot) bool {
	n := 0
	for _, t := range snap.Tiles {
		switch t.State {
		case tiles.Off:
		case tiles.None:
			n++
		default:
			return false
		}
	}
	return n > 0
}

// ongoingFirst sorts problems newest first, ongoing ones before the rest.
func ongoingFirst(eps []tiles.Episode) []tiles.Episode {
	eps = slices.Clone(eps)
	slices.SortStableFunc(eps, func(a, b tiles.Episode) int {
		switch {
		case a.Ongoing() && !b.Ongoing():
			return -1
		case b.Ongoing() && !a.Ongoing():
			return 1
		}
		return b.Start.Compare(a.Start)
	})
	return eps
}

// watchingSince is when fyisp started watching: the store's oldest sample
// (read once a minute), which the engine's first data (capped by its
// replay) only refines within the first stored hour.
func (s *server) watchingSince(ctx context.Context, first, now time.Time) time.Time {
	if s.d.Store == nil {
		return first
	}
	hc := s.home
	hc.mu.Lock()
	oldest, at := hc.oldest, hc.oldestAt
	hc.mu.Unlock()
	if at.IsZero() || now.Sub(at) >= time.Minute || now.Before(at) {
		if st, err := s.d.Store.Stats(ctx); err == nil {
			oldest = st.Oldest
		}
		hc.mu.Lock()
		hc.oldest, hc.oldestAt = oldest, now
		hc.mu.Unlock()
	}
	// The store keeps whole hours: within the first one the engine knows better.
	if oldest.IsZero() || !first.IsZero() && !oldest.Before(first.Truncate(time.Hour)) {
		return first
	}
	return oldest
}

// homeNow builds part=now.
func (s *server) homeNow(r *http.Request, p homeParams, now time.Time) *HomeNow {
	snap, checking, stale := s.homeState(now)
	c := homeCopy{loc: p.zone.loc, now: now, wifi: s.wifiWording(r), stun: snap.Conn.Kind == string(model.VerdictSTUNBlocked)}
	learning := snap.Learning && !checking
	out := &HomeNow{V: 2, At: now.UnixMilli(), Stale: stale, Monitored: snap.Monitored}
	if !snap.LastMeasured.IsZero() && !checking { // a (re)start's last stored check isn't news
		out.LastCheck = snap.LastMeasured.UnixMilli()
	}
	if w := s.watchingSince(r.Context(), snap.FirstData, now); !w.IsZero() {
		out.WatchingSince = w.UnixMilli()
		out.Footer = "Watching your connection since " + c.watching(w)
	}
	if learning && !snap.FirstData.IsZero() {
		out.Learning = &HomeLearning{Until: snap.FirstData.Add(24 * time.Hour).UnixMilli(),
			Text: "fyisp is still learning what’s usual for your connection. This takes about a day. Until then it only flags big problems, so small slowdowns won’t show yet."}
	}
	var eps []tiles.Episode
	if !checking {
		eps = ongoingFirst(snap.Problems(now.Add(-homeWeek))) // the tiles' own tick
	}
	byID := make(map[string]tiles.Episode, len(eps))
	for _, e := range eps {
		byID[e.ID] = e
	}
	v := homeView{checking: checking, stale: stale, blocked: !checking && !stale && udpBlocked(snap.Conn.Kind),
		first: snap.FirstData, custom: s.d.CustomProfile}
	for _, t := range snap.Tiles {
		out.Tiles = append(out.Tiles, c.tile(t, byID, v))
	}
	app := c.appState(snap, eps, checking || stale)
	out.Answer = c.answer(snap, eps, app, checking, stale, learning)
	out.Path = c.path(snap, app, checking, stale)
	if !c.wifi {
		out.PathHint = "fyisp checks from the box it runs on, up to your router. It can’t see the Wi-Fi on the device you’re using now — if calls are choppy here, try moving closer to your router or plugging in."
	}
	out.Speed = s.homeSpeed(r, c, snap.Conn, now)
	switch {
	case checking:
		out.Problems = HomeProblems{Summary: "Checking…", Items: []HomeProblem{}}
	default:
		key := strings.Join([]string{strconv.FormatUint(snap.Rev, 16), p.zone.key, now.In(p.zone.loc).Format(time.DateOnly),
			strconv.FormatBool(learning), strconv.FormatInt(snap.FirstData.UnixMilli(), 10)}, "\x00")
		out.Problems, _ = memo(&s.home.mu, s.home.probs, key, func() (HomeProblems, bool) { return c.problems(eps, learning, snap.FirstData), true })
	}
	return out
}

// homeView is how part=now's tiles are shown.
type homeView struct {
	checking, stale bool
	blocked         bool      // udp_blocked: call tiles have lost their fast path, not their calls
	first           time.Time // the engine's first data
	custom          bool      // the profile is a --config file
}

// appRow is the card's row 9 (problems with some apps, no path verdict):
// the bad tiles, their likely causes, and the strip's hop their common
// cause is on (-1: none).
type appRow struct {
	bad      []tiles.TileSnap
	causes   []tiles.Cause
	hop      int
	allCalls bool
}

// appState works out row 9 of the card.
func (c homeCopy) appState(snap *tiles.Snapshot, eps []tiles.Episode, skip bool) appRow {
	r := appRow{hop: -1}
	if !skip && snap.Conn.Kind == string(model.VerdictUDPDegraded) {
		// Choppy calls with no path verdict: their problems' common cause
		// (e.g. the provider's loss) is the strip's too.
		r.allCalls = true
		for _, e := range eps {
			if e.Ongoing() && !e.Connection && e.Cause.Who != "" {
				r.causes = append(r.causes, e.Cause)
			}
		}
		r.hop = hopOf(r.causes)
		r.causes = nil
		return r
	}
	if skip || snap.Conn.Kind != tiles.ConnApp {
		return r
	}
	r.allCalls = true
	for _, id := range snap.Conn.Tiles {
		if i := slices.IndexFunc(snap.Tiles[:], func(t tiles.TileSnap) bool { return t.ID == id }); i >= 0 {
			t := snap.Tiles[i]
			r.bad = append(r.bad, t)
			r.causes = append(r.causes, causeOf(t, eps))
			r.allCalls = r.allCalls && tileDef(id).Kind == profile.TileCall
		}
	}
	r.hop = hopOf(r.causes)
	return r
}

// hopOf is the strip's hop of causes' common connection cause (-1: none).
func hopOf(causes []tiles.Cause) int {
	if len(causes) == 0 || slices.ContainsFunc(causes, func(y tiles.Cause) bool { return y.Who != causes[0].Who }) {
		return -1
	}
	switch causes[0].Who {
	case tiles.WhoRouter, tiles.WhoOffline:
		return 0
	case tiles.WhoProvider:
		return 1
	case tiles.WhoInternet:
		return 2
	}
	return -1
}

// watching is the footer's date: "Mar 3", "today 9:50 am".
func (c homeCopy) watching(t time.Time) string {
	switch n := c.daysAgo(t); {
	case n <= 1:
		return c.dayIn(t) + " " + c.clock(t)
	case c.in(t).Year() != c.in(c.now).Year():
		return c.in(t).Format("Jan 2, 2006")
	}
	return c.in(t).Format("Jan 2")
}

// udpBlocked: the card's connection state is a call-traffic block
// (stun_blocked keys and reads as udp_blocked).
func udpBlocked(kind string) bool {
	return kind == string(model.VerdictUDPBlocked) || kind == string(model.VerdictSTUNBlocked)
}

func iconOf(st tiles.State) string {
	switch st {
	case tiles.Good, tiles.Slow, tiles.Down:
		return string(st)
	}
	return "none"
}

// answer is the card (tiles.ConnState's rows, worded).
func (c homeCopy) answer(snap *tiles.Snapshot, eps []tiles.Episode, app appRow, checking, stale, learning bool) HomeAnswer {
	a := c.card(snap, eps, app, checking, stale, learning)
	// One key, one text: the key carries a hash of the words (so a new
	// day, which turns "Started at" into "Started yesterday at", is a new key).
	h := fnv.New32a()
	h.Write([]byte(a.Headline + "\x00" + a.Sub))
	a.Key += ":" + strconv.FormatUint(uint64(h.Sum32()), 36)
	return a
}

// card is the answer card, before its key gets the text's hash.
func (c homeCopy) card(snap *tiles.Snapshot, eps []tiles.Episode, app appRow, checking, stale, learning bool) HomeAnswer {
	cs := snap.Conn
	a := HomeAnswer{Key: cs.Key, Tone: cs.Tone, Icon: iconOf(cs.Icon)}
	if !learning {
		a.Day = c.dayLine(eps)
	}
	started := func() string { return c.started(cs.Since) }
	switch {
	case checking:
		return HomeAnswer{Key: tiles.ConnChecking, Tone: tiles.ToneChecking, Icon: "none", Tab: "Checking",
			Headline: "Checking your connection…", Sub: "fyisp just started. The first answer takes about a minute."}
	case stale:
		a.Key, a.Tone, a.Icon, a.Tab = tiles.ConnStale, tiles.ToneStale, "none", "? Not checking"
		if snap.LastMeasured.IsZero() {
			a.Headline, a.Sub, a.Day = "fyisp hasn’t checked anything yet.", "Is the computer it runs on asleep or turned off?", nil
			return a
		}
		at := c.when(snap.LastMeasured)
		a.Headline = "fyisp hasn’t checked since " + at + "."
		a.Sub = "Is the computer it runs on asleep or turned off? Everything below is from " + at + " and may be out of date."
		return a
	}
	switch cs.Kind {
	case string(model.VerdictISP):
		if cs.Lossy {
			a.Headline = "Your internet provider is losing some data."
			a.Sub = "Your router is answering normally, but some data is lost in your provider’s network past it. Calls may be choppy and some pages slow to load." + started()
			a.Tab = "! Provider losing data"
			break
		}
		a.Headline, a.Sub, a.Tab = c.pathCard(cs.Kind, cs.Since)
	case string(model.VerdictLAN), string(model.VerdictUpstream), string(model.VerdictNoNetwork), tiles.ConnDrop:
		a.Headline, a.Sub, a.Tab = c.pathCard(cs.Kind, cs.Since)
	case string(model.VerdictDNS):
		a.Headline = "Websites can’t be found right now."
		a.Sub = "Your connection is up, but name lookups (DNS) are failing, so apps can’t find their servers." + started()
		a.Tab = "! Websites not found"
	case string(model.VerdictUDPBlocked), string(model.VerdictSTUNBlocked):
		a.Headline = "Calls can’t use their fast path on this network."
		what := "the kind of traffic calls prefer. Most call apps fall back to a slower route"
		if cs.Kind == string(model.VerdictSTUNBlocked) {
			what = "the ports calls prefer. Most call apps fall back to another port or a slower route"
		}
		if cs.Quiet {
			a.Icon = "info" // known for a long time: information, not a warning
			a.Sub = "This network blocks " + what + ", so calls usually still work but may be choppier. Websites and apps are fine."
			a.Tab = "ⓘ Calls limited"
			break
		}
		a.Sub = "This network is blocking " + what + ", so calls usually still work but may be choppier. Websites and apps are fine." + started()
		a.Tab = "! Calls limited"
	case string(model.VerdictUDPDegraded):
		a.Headline = "Calls may be choppy right now."
		a.Sub = "Calls are losing some data between your home and the internet, while websites and apps are fine." + started()
		a.Tab = "! Calls choppy"
	case tiles.ConnBroad:
		a.Headline = "Most apps are slower than usual."
		a.Sub = "Your router and your internet provider look fine, but most apps are slow at the same time." + started()
		a.Tab = "! Apps slow"
	case tiles.ConnApp:
		if len(app.bad) == 0 {
			break
		}
		var warn bool
		a.Headline, a.Sub, a.Tab, warn = c.appCard(app.bad, app.causes, snap.Tiles[:])
		if warn { // the connection or the calls' route, not the apps: amber
			a.Tone, a.Icon = tiles.ToneWarn, "slow"
		}
		if cs.Again != nil {
			a.Sub += " " + c.againAside(cs.Again)
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
	measured := 0
	for _, t := range snap.Tiles {
		if t.State != tiles.Off && t.State != tiles.None {
			measured++
		}
	}
	a.Sub = c.healthy(snap.Monitored, measured, calls, learning, snap.Layers[1].State != tiles.None, snap.FirstData)
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
func (c homeCopy) path(snap *tiles.Snapshot, app appRow, checking, stale bool) []HomeHop {
	labels := [3]string{c.routerName(), "Your internet provider", "The internet"}
	ids := [3]string{"router", "provider", "internet"}
	var out [3]HomeHop
	for i := range out {
		out[i] = HomeHop{ID: ids[i], Label: labels[i]}
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
		// Down only when the layer lost everything; a lossy one is slow.
		since := cs.Since
		switch {
		case snap.Layers[culprit].State == tiles.Down:
			out[culprit].State, out[culprit].Sub = "down", "Not answering "+c.since(since)
		case culprit == 2:
			out[culprit].State, out[culprit].Sub = "slow", "Hard to reach "+c.since(since)
		default:
			out[culprit].State, out[culprit].Sub = "slow", "Losing some data "+c.since(since)
		}
		for i := culprit + 1; i < 3; i++ {
			out[i].State, out[i].Sub = "slow", through[culprit]
		}
	case app.hop >= 0:
		// Apps (calls) suffer from a layer no verdict blames: the strip says so too.
		what := "Losing some data "
		if app.allCalls {
			what = "Losing some call data "
		}
		out[app.hop].State, out[app.hop].Sub = "slow", what+c.since(cs.Since)
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
			out[i].State = "unknown" // last known: no colour
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
func (c homeCopy) tile(t tiles.TileSnap, eps map[string]tiles.Episode, v homeView) HomeTile {
	d := tileDef(t.ID)
	st := t.State
	if v.checking && st != tiles.Off {
		st = tiles.None
	}
	// A blocked fast path is choppier calls over a slower route, not calls
	// that can't connect: only when the block is the tile's only problem
	// (an app's own outage on such a network stays Down).
	blocked := v.blocked && d.Kind == profile.TileCall && st == tiles.Down && onlyBlocked(t, eps)
	if blocked {
		st = tiles.Slow
		for i := range t.Bars {
			if t.Bars[i].State == tiles.Down && t.Bars[i].From.Add(5*time.Minute).After(t.Since) {
				t.Bars[i].State = tiles.Slow
			}
		}
	}
	out := HomeTile{ID: d.ID, Name: d.Name, Kind: string(d.Kind), State: string(st), Word: c.word(st, d.Kind), Bars: []HomeBar{}}
	t.State = st
	out.Reason = c.tileReason(d, t, v.checking, blocked)
	switch {
	case st == tiles.Off:
		out.Aria = d.Name + ": not checked on this setup"
		out.EnableHint = c.enableHint(d, v.custom)
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
	if v.stale {
		out.Aria += " (last known)"
	}
	for _, b := range t.Bars {
		if b.From.IsZero() {
			continue
		}
		s := b.State
		if v.checking || (s != tiles.Good && s != tiles.Slow && s != tiles.Down) {
			s = tiles.None
		}
		out.Bars = append(out.Bars, HomeBar{From: b.From.UnixMilli(), State: string(s), Note: c.barNote(b, s, d.Kind, eps)})
	}
	if !v.checking {
		out.BarsSummary = c.barsSummary(t.Bars[:], v.first)
	}
	return out
}

// onlyBlocked reports a tile whose ongoing problems are all a blocked
// fast path (EpBlocked), with no app-side cause of its own.
func onlyBlocked(t tiles.TileSnap, eps map[string]tiles.Episode) bool {
	if t.Cause.Who == tiles.WhoApp || t.Cause.Who == tiles.WhoCallsPath {
		return false
	}
	n := 0
	for _, e := range eps {
		if !e.Ongoing() || !slices.Contains(e.Tiles, t.ID) {
			continue
		}
		if e.Kind != tiles.EpBlocked {
			return false
		}
		n++
	}
	return n > 0
}

// barNote is a bar's note: "Dropped 40 s", "Down since 10:05 am",
// "Choppy 9:41–9:45 am — likely Slack’s side", "Slow during a speed
// test"; "" when the word says it.
func (c homeCopy) barNote(b tiles.Bar, st tiles.State, kind profile.TileKind, eps map[string]tiles.Episode) string {
	if !st.Bad() {
		return ""
	}
	w := c.word(st, kind)
	switch e, ok := eps[b.Episode]; {
	case ok && e.Ongoing():
		return w + " " + c.since(e.Start)
	case st == tiles.Down && b.Drop > 0:
		return "Dropped " + short(b.Drop)
	case b.Speedtest:
		return w + " during a speed test"
	case !ok:
		return ""
	default:
		return w + " " + c.span(e.Start, e.End) + c.likely(e.Cause)
	}
}

// likely is " — likely Slack’s side" ("" when unclear).
func (c homeCopy) likely(x tiles.Cause) string {
	if x.Who == "" || x.Who == tiles.WhoUnknown {
		return ""
	}
	return " — likely " + c.cause(x)
}

// problems is the problems block.
func (c homeCopy) problems(eps []tiles.Episode, learning bool, first time.Time) HomeProblems {
	out := HomeProblems{Summary: c.problemsSummary(eps, learning, first), Items: []HomeProblem{}}
	for _, e := range eps {
		p := HomeProblem{ID: e.ID, From: e.Start.UnixMilli(), Ongoing: e.Ongoing(), Level: string(e.Sev),
			When: c.problemWhen(e), What: c.problemWhat(e), Cause: c.cause(e.Cause), CauseKind: e.Cause.Kind(),
			Evidence: capFirst(c.evidence(e.Cause, e.Ongoing())) + "."}
		if e.Kind == tiles.EpBlocked {
			// Calls lost their fast path, not their calls: amber. No path
			// layer explains a block: the network does it.
			p.Level = string(tiles.Slow)
			if p.CauseKind == tiles.KindOther {
				p.Cause, p.Evidence = "a firewall or VPN on this network blocking call traffic", "Websites and apps were fine; only calls couldn’t use their usual route."
				if e.Ongoing() {
					p.Evidence = "Websites and apps are fine; only calls can’t use their usual route."
				}
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

// homeSpeed is the speed line.
func (s *server) homeSpeed(r *http.Request, c homeCopy, cs tiles.ConnState, now time.Time) HomeSpeed {
	out := HomeSpeed{EmptyText: "No speed test yet."}
	src := s.homeSpeedSrc
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
		out.Running = &HomeSpeedRunning{Entry: run.ID, ExpectedEnd: run.EstFinish.UnixMilli()}
	}
	hc := s.home
	hc.mu.Lock()
	ts := hc.speedTests
	fresh := !hc.speedAt.IsZero() && hc.speedRun == runID && !now.Before(hc.speedAt) && now.Sub(hc.speedAt) < homeSpeedTTL
	hc.mu.Unlock()
	if !fresh {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		xs, err := src.Tests(ctx, now.Add(-homeWeek), now)
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
		out.Usual = &HomeSpeedUsual{DownMbps: roundMbps(medianOf(down)), UpMbps: roundMbps(medianOf(up))}
	}
	dn, up := roundMbps(*last.DownBps/1e6), roundMbps(*last.UpBps/1e6)
	during := cs.Tone == tiles.ToneBad && !cs.Since.IsZero() && !last.Start.Before(cs.Since)
	out.Latest = &HomeSpeedResult{At: last.Start.UnixMilli(), DownMbps: dn, UpMbps: up,
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

// homeTile builds part=tile: nil with the status and message of a 404.
func (s *server) homeTile(p homeParams, now time.Time) (*HomeTileDetail, int, string) {
	i := slices.IndexFunc(profile.Tiles, func(d profile.TileDef) bool { return d.ID == p.id })
	if i < 0 {
		return nil, http.StatusNotFound, "unknown tile"
	}
	snap, checking, stale := s.homeState(now)
	t := snap.Tiles[i]
	if t.State == tiles.Off {
		return nil, http.StatusNotFound, "tile not monitored"
	}
	c := homeCopy{loc: p.zone.loc, now: now, stun: snap.Conn.Kind == string(model.VerdictSTUNBlocked)}
	d := tileDef(p.id)
	v := homeView{checking: checking, blocked: !checking && !stale && udpBlocked(snap.Conn.Kind), first: snap.FirstData}
	byID := map[string]tiles.Episode{}
	for _, e := range snap.Problems(now.Add(-homeWeek)) {
		byID[e.ID] = e
	}
	h := c.tile(t, byID, v)
	out := &HomeTileDetail{V: 2, At: now.UnixMilli(), ID: d.ID, State: h.State, Word: h.Word, Reason: h.Reason, Metrics: []HomeMetric{}}
	if rs := s.home.resolved(s.d.Profile()); i < len(rs) {
		src := ""
		if t.Call != nil {
			src = t.Call.Source
		}
		out.Coverage = c.coverage(rs[i], src)
	}
	if !checking {
		out.Metrics = c.metrics(d, t)
	}
	if t.Call != nil && t.Call.Source == tiles.CallShared {
		out.Legend = "How quickly the general call path answers (lower is better)"
	}
	learning := snap.Learning && !checking
	key := strings.Join([]string{p.id, p.zone.key, strconv.FormatInt(now.Truncate(homeBucket).UnixMilli(), 10),
		strconv.FormatBool(learning), strconv.FormatBool(checking), h.State}, "\x00")
	ch, ok := memo(&s.home.mu, s.home.charts, key, func() (homeChart, bool) {
		x, ok := s.d.Tiles.Chart(p.id)
		if !ok {
			return homeChart{}, false
		}
		eps := map[string]tiles.Episode{}
		for _, e := range snap.Problems(x.From) {
			eps[e.ID] = e
		}
		return c.chart(x, d, tiles.State(h.State), eps, snap, learning, checking), true
	})
	if !ok {
		return nil, http.StatusNotFound, "tile not monitored"
	}
	out.homeChart = ch
	return out, 0, ""
}

// metrics are the panel's figures: a call tile's delay, jitter and loss
// against their limits (the general call path's, when the app has no
// target of its own: the row says so); an app's usual and Slow limit. App
// and stream tiles show none while the usual is learned (call tiles keep
// theirs: their limits are fixed).
func (c homeCopy) metrics(d profile.TileDef, t tiles.TileSnap) []HomeMetric {
	out := []HomeMetric{}
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
		if m.Source == tiles.CallShared {
			out = append(out, HomeMetric{Label: "General call path:"})
		}
		return append(out, HomeMetric{Label: "Delay", Value: delay, Limit: "choppy over 150 ms", Bad: &bd},
			HomeMetric{Label: "Jitter", Value: jitter, Limit: "over 30 ms", Bad: &bj},
			HomeMetric{Label: "Packet loss", Value: loss, Limit: "2% or more", Bad: &bl})
	}
	if t.UsualMs == nil {
		return out
	}
	out = append(out, HomeMetric{Label: "Usual", Value: msText(*t.UsualMs)})
	if t.SlowAboveMs != nil {
		out = append(out, HomeMetric{Label: "Slow above", Value: msText(*t.SlowAboveMs)})
	}
	if d.Kind == profile.TileStream {
		out = append(out, HomeMetric{Label: "Only marked Down if it stops loading"})
	}
	return out
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

// chart is part=tile's chart block. state is the tile's word now: an
// ongoing band is worded with it.
func (c homeCopy) chart(x *tiles.Chart, d profile.TileDef, state tiles.State, eps map[string]tiles.Episode, snap *tiles.Snapshot, learning, checking bool) homeChart {
	out := homeChart{From: x.From.UnixMilli(), Step: x.Step.Milliseconds(), Ms: make([]*float64, len(x.Ms)), Bands: []HomeBand{}}
	for i, v := range x.Ms {
		if !math.IsNaN(v) && !math.IsInf(v, 0) {
			r := math.Round(v*10) / 10
			out.Ms[i] = &r
		}
	}
	if checking {
		out.Text = "fyisp is still checking."
		return out
	}
	head := "Still learning what’s usual."
	if x.UsualMs != nil && !learning {
		u := math.Round(*x.UsualMs*10) / 10
		out.UsualMs = &u
		head = "Usually about " + msText(u) + "."
	}
	var texts []string
	for _, b := range x.Bands {
		hb := HomeBand{From: b.From.UnixMilli(), To: b.To.UnixMilli(), Kind: b.Kind}
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
			e, ok := eps[b.Episode]
			// A connection problem that never touched this tile isn't its band.
			if ok && e.Connection && len(e.Tiles) > 0 && !slices.Contains(e.Tiles, d.ID) {
				continue
			}
			w := c.word(b.Sev, d.Kind)
			switch {
			case b.To.IsZero() && state.Bad():
				hb.Text = c.word(state, d.Kind) + " " + c.since(b.From)
			case b.To.IsZero():
				hb.Text = w + " " + c.since(b.From)
			case ok && e.Kind == tiles.EpDrop:
				hb.Text = "Dropped " + short(e.DropDur) + " at " + c.clock(e.Start)
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
