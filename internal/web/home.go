package web

import (
	"context"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/callq"
	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/profile"
	"github.com/fuck-you-isp/fyisp/internal/speedtest"
	"github.com/fuck-you-isp/fyisp/internal/store"
	"github.com/fuck-you-isp/fyisp/internal/verdict"
)

// Home windows and limits (docs/HOME-API.md).
const (
	// HomeHistoryTTL is how long a history response is cached. The live
	// part is not cached.
	HomeHistoryTTL = time.Minute
	// homeCallsWindow is the call path's live window (as the Calls card);
	// homeTileWindow the TCP tiles'; homePathWindow the path strip's.
	homeCallsWindow = 5 * time.Minute
	homeTileWindow  = 15 * time.Minute
	homePathWindow  = 2 * time.Minute
	homeMaxProblems = 50
	homeMaxNames    = 3
	// Working hours (local, Monday to Friday), as the Calls view's strip.
	homeWorkFrom, homeWorkTo = 9, 17
	// homeHeartbeat caps the series read to find when fyisp was not running.
	homeHeartbeat = 8
)

// nodeAffected is a path node beyond the culprit: it shows the trouble but
// is not to blame (neutral, not amber or red).
const nodeAffected = "affected"

// Home tile ids, in display order: profile's Home categories.
const (
	tileCalls = profile.HomeCalls
	tileWork  = profile.HomeWork
	tileWeb   = profile.HomeWeb
	tileOther = profile.HomeOther
)

var homeTiles = []string{tileCalls, tileWork, tileWeb, tileOther}

// TCP tile states.
const (
	tileWorking  = "working"
	tileMostly   = "mostly"
	tileSlow     = "slow"
	tileDown     = "down"
	tileChecking = "checking"
)

// homeCategory returns a target's Home tile (profile.HomeCategory: calls,
// work, web or other). A variable for tests.
var homeCategory = profile.HomeCategory

// homeName is a target's display name: never a host.
// homeName is a target's display name: displayNames, else its call app's
// title, else its name with "-" as spaces. Never a host.
func homeName(t model.Target) string {
	if n := displayNames[t.Name]; n != "" {
		return n
	}
	for _, tag := range t.Tags {
		if id, ok := strings.CutPrefix(tag, callq.TagApp); ok && appTitles[id] != "" {
			return appTitles[id]
		}
	}
	return strings.ReplaceAll(t.Name, "-", " ")
}

// appTitles are the call apps' display names (app.js APP_TITLE).
var appTitles = map[string]string{"teams": "Teams", "meet": "Google Meet", "zoom": "Zoom", "slack": "Slack",
	"discord": "Discord", "whatsapp": "WhatsApp", "webex": "Webex"}

func appTitle(id string) string {
	if t := appTitles[id]; t != "" {
		return t
	}
	return id
}

// homeParams is a parsed /api/home query.
type homeParams struct {
	part, span string
	tz         int            // minutes east of UTC, rounded to 15 (now, for an IANA zone)
	loc        *time.Location // the zone: the speed line's hours (DST-correct with an IANA name)
	tzKey      string
	at         time.Time // part=at
	key        string
}

func parseHome(q url.Values) (homeParams, error) {
	p := homeParams{part: q.Get("part"), span: q.Get("span")}
	if err := checkParams(q, "part", "span", "tz", "at"); err != nil {
		return p, err
	}
	switch p.part {
	case "":
		p.part = "live"
	case "live", "history", "at":
	default:
		return p, badReq("bad part %q: want live, history or at", p.part)
	}
	switch as := q.Get("at"); {
	case p.part != "at" && as != "":
		return p, badReq("at is for part=at")
	case p.part == "at":
		ms, err := strconv.ParseInt(as, 10, 64)
		if err != nil || ms <= 0 {
			return p, badReq("bad at %q: want Unix milliseconds", as)
		}
		p.at = time.UnixMilli(ms)
	}
	switch {
	case p.span != "" && p.part != "history":
		return p, badReq("span is for part=history")
	case p.span == "":
		p.span = "d"
	case p.span != "d" && p.span != "w":
		return p, badReq("bad span %q: want d or w", p.span)
	}
	p.loc = time.Local // the speed line's hours without tz: the server's zone
	if s := q.Get("tz"); s != "" {
		z, err := parseZone(s, time.Now())
		if err != nil {
			return p, err
		}
		p.tz, p.loc, p.tzKey = z.min, z.loc, z.key
	}
	switch p.part {
	case "live":
		p.key = "home\x00live\x00" + p.tzKey
	case "at":
		p.key = "home\x00at\x00" + strconv.FormatInt(p.at.UnixMilli(), 10)
	default:
		p.key = "home\x00history\x00" + p.span + "\x00" + p.tzKey
	}
	return p, nil
}

// serveHome answers /api/home (docs/HOME-API.md).
func (s *server) serveHome(w http.ResponseWriter, r *http.Request) {
	p, err := parseHome(r.URL.Query())
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, err.Error())
		return
	}
	if s.d.Speed != nil {
		s.d.Speed.NoteZone(p.tzKey) // the report's zone (only an IANA name loads)
	}
	if p.part == "at" {
		if now := s.now(); p.at.After(now) || now.Sub(p.at) > MaxRange {
			writeErr(w, r, http.StatusBadRequest, "at must be within the last 90 days")
			return
		}
		s.runCached(w, r, p.key, func(ctx context.Context) (*cached, error) {
			a, err := s.homeAt(ctx, p.at, s.now())
			if err != nil {
				return nil, err
			}
			return jsonCached(a)
		})
		return
	}
	if p.part == "live" {
		s.runCached(w, r, p.key, func(ctx context.Context) (*cached, error) {
			now := s.now()
			l, err := s.homeLive(ctx, now)
			if err != nil {
				return nil, err
			}
			// Speed by time of day: one plain line (its hours are local,
			// hence tz), never while the answer card shows a problem.
			if l.Answer.Level != levelWarn && l.Answer.Level != levelBad {
				l.Speed = s.speedHome(ctx, now, p.loc)
			}
			return jsonCached(l)
		})
		return
	}
	s.runCachedIn(s.hcache, w, r, p.key, func(ctx context.Context) (*cached, error) {
		h, err := s.homeHistory(ctx, p, s.now())
		if err != nil {
			return nil, err
		}
		return jsonCached(h)
	})
}

// homeSet sorts a profile's targets into Home tiles.
type homeSet struct {
	byTile map[string][]model.Target // TCP/HTTPS targets of each tile (calls: the apps' websites)
	cat    map[string]string         // target name -> tile (calls includes STUN targets)
	path   []model.Target
	home   int // targets in the calls, work and web tiles
}

func newHomeSet(p *model.Profile) homeSet {
	hs := homeSet{byTile: map[string][]model.Target{}, cat: map[string]string{}}
	if p == nil {
		return hs
	}
	for _, t := range p.Targets {
		if isPath(t) {
			hs.path = append(hs.path, t)
			continue
		}
		c := homeCategory(t)
		if !slices.Contains(homeTiles, c) || c == tileOther {
			c = tileOther
		} else {
			hs.home++
		}
		hs.cat[t.Name] = c
		if !t.IsUDP() {
			hs.byTile[c] = append(hs.byTile[c], t)
		}
	}
	return hs
}

// onlyOther reports a profile with nothing in the Home tiles (e.g.
// --profile clouds): its Other tile is then the user's set and may raise
// the answer.
func (hs homeSet) onlyOther() bool { return hs.home == 0 && len(hs.cat) > 0 }

// relevant keeps the targets that belong to a Home tile (all of them with
// onlyOther).
func (hs homeSet) relevant(names []string) []string {
	var out []string
	for _, n := range names {
		if c, ok := hs.cat[n]; ok && (c != tileOther || hs.onlyOther()) {
			out = append(out, n)
		}
	}
	return out
}

// ---------- live ----------

func (s *server) homeLive(ctx context.Context, now time.Time) (*HomeLive, error) {
	prof := s.d.Profile()
	hs := newHomeSet(prof)
	v := model.Verdict{Kind: VerdictUnknown}
	if s.d.Verdict != nil {
		v = s.d.Verdict.Current()
	}
	// Outages that already ended don't count against the apps or calls
	// while their samples age out of the windows.
	tcpSkip, callSkip, recovered, err := s.explained(ctx, now)
	if err != nil {
		return nil, err
	}
	// Whole minutes, so a minute scores the same here and in part=at.
	c, err := s.calls(ctx, callsParams{from: now.Add(-homeCallsWindow).Truncate(time.Minute), to: now, skip: callSkip})
	if err != nil {
		return nil, err
	}
	names := map[string]string{}
	if prof != nil {
		for _, t := range prof.Targets {
			names[t.Name] = homeName(t)
		}
	}
	out := &HomeLive{Part: "live", At: now.UnixMilli(), Apps: []HomeApp{}, Tiles: []HomeTile{}}
	cp := homeCopy{}

	// The TCP tiles: one rows query per kind in use (TCP for every shipped
	// profile).
	tileRows := map[string][]overviewRow{}
	byKind := map[model.ProbeKind][]model.Target{}
	for _, id := range homeTiles {
		if id == tileCalls {
			continue
		}
		for _, t := range hs.byTile[id] {
			if k, ok := webKind(t); ok {
				byKind[k] = append(byKind[k], t)
			}
		}
	}
	rowOf := map[string]overviewRow{}
	for k, ts := range byKind {
		rows, err := s.overviewRowsMasked(ctx, k, now.Add(-homeTileWindow), now, ts, tcpSkip)
		if err != nil {
			return nil, err
		}
		for i, t := range ts {
			rowOf[t.Name] = rows[i]
		}
	}
	for _, id := range homeTiles[1:] {
		for _, t := range hs.byTile[id] {
			if r, ok := rowOf[t.Name]; ok {
				tileRows[id] = append(tileRows[id], r)
			}
		}
	}

	own := ownCallState(c)
	hasCalls := len(hs.byTile[tileCalls]) > 0 || c.Path != nil || slices.ContainsFunc(c.Apps, func(a callsApp) bool { return a.callsGroup != nil })
	if hasCalls {
		out.Tiles = append(out.Tiles, callsTile(c, own, v.Quiet, cp))
	}
	for _, id := range homeTiles[1:] {
		if len(hs.byTile[id]) == 0 || (id == tileOther && len(tileRows[id]) == 0) {
			continue
		}
		out.Tiles = append(out.Tiles, tcpTile(id, hs.byTile[id], tileRows[id], names, cp))
	}
	if !hs.onlyOther() {
		out.Other = otherLine(tileRows[tileOther], cp)
	}
	out.Apps, out.AppsNote = homeApps(c, cp)
	if c.Path != nil {
		out.Calls = homeCallsWords(c.Path, own, cp)
	}

	path, err := s.homePathRows(ctx, hs.path, now)
	if err != nil {
		return nil, err
	}
	in := answerIn{v: v, own: own, calls: c, tiles: out.Tiles, hs: hs, names: names, now: now}
	out.Answer = homeAnswerOf(in, cp)
	if out.Answer.Level == levelGood && recovered > 0 {
		out.Answer.Recovered = recovered
	}
	out.Path = homePath(path, out.Answer, out.Tiles, cp)
	// During a path problem a website that answers says little about the
	// app: don't show it as fine.
	if a := out.Answer; slices.Contains([]string{whoHome, whoProvider, whoBeyond, whoOffline, whoDNS}, a.Who) &&
		(a.Level == levelBad || a.Level == levelWarn) {
		for i := range out.Apps {
			switch x := &out.Apps[i]; {
			case a.Who == whoOffline:
				x.State, x.Level, x.Label, x.Note = "cant_tell", levelChecking, cp.cantCheckOffline(), ""
			case x.State == "reachable" || x.State == "not_answering":
				x.State, x.Level, x.Label, x.Note = "cant_tell", levelChecking, cp.callLabel("cant_tell"), cp.cantTell()
			}
		}
	}
	// Tiles hit by a path problem say so instead of their own trouble.
	if a := out.Answer; slices.Contains([]string{whoHome, whoProvider, whoBeyond, whoOffline}, a.Who) &&
		a.Confidence != confWeak && (a.Level == levelBad || a.Level == levelWarn) {
		for i := range out.Tiles {
			t := &out.Tiles[i]
			if a.Who == whoOffline || (t.ID != tileCalls && (t.Level == levelWarn || t.Level == levelBad)) {
				t.Level, t.Label, t.Note = nodeAffected, cp.affectedBy(a.Who), ""
			}
		}
	}
	if out.Answer.Level == levelChecking {
		out.Steps = homeSteps(path, out.Tiles, cp)
		out.Calls = nil
		out.Answer.Fact, out.Answer.FactAt = "", 0
		// Warming up: nothing is Good yet.
		for i := range out.Tiles {
			if t := &out.Tiles[i]; t.ID == tileCalls {
				t.Level, t.State, t.Label, t.Note = levelChecking, callNotEnoughData, cp.callHead(callNotEnoughData, false), ""
			}
		}
		for i := range out.Apps {
			x := &out.Apps[i]
			x.Level, x.State, x.Label, x.Note = levelChecking, callNotEnoughData, "Checking…", ""
		}
		out.AppsNote = ""
	}
	return out, nil
}

// homeRecoveryGrace extends an ended outage: the verdict needs about a
// minute of clean samples to clear, and so do the windows.
const homeRecoveryGrace = time.Minute

// How long before an incident's start its fault began: the verdict
// engine's one-minute window for the path kinds; for a block, 10 timed-out
// bursts in a row at up to 15 s plus its hold (netns: since came ~68 s
// after the first timeout); for degraded calls, its 3 minutes of poor
// bursts plus the window.
const (
	homePathLead     = time.Minute
	homeBlockLead    = 3 * time.Minute
	homeDegradedLead = 4 * time.Minute
)

// explained returns the spans of the outages that ended within the live
// windows and explain the samples in them: path, DNS and no-network
// incidents for the tiles (tcp), those and the UDP ones for calls; and
// when the last of them ended (0: none).
func (s *server) explained(ctx context.Context, now time.Time) (tcp, calls []HomeSpan, recovered int64, err error) {
	if s.d.Verdict == nil {
		return nil, nil, 0, nil
	}
	list, err := s.d.Verdict.Incidents(ctx, now.Add(-homeTileWindow-homeRecoveryGrace), now)
	if err != nil {
		return nil, nil, 0, err
	}
	for _, in := range list {
		if in.End.IsZero() || in.End.After(now) {
			continue // ongoing: the verdict says so itself
		}
		// The verdict starts after the fault does: it needs a window of bad
		// samples first. Those leading minutes are the same outage.
		sp := HomeSpan{To: in.End.Add(homeRecoveryGrace).UnixMilli()}
		switch in.Kind {
		case model.VerdictLAN, model.VerdictISP, model.VerdictUpstream, model.VerdictDNS, model.VerdictNoNetwork:
			sp.From = in.Start.Add(-homePathLead).UnixMilli()
			tcp = append(tcp, sp)
		case model.VerdictUDPBlocked, model.VerdictSTUNBlocked:
			sp.From = in.Start.Add(-homeBlockLead).UnixMilli()
		case model.VerdictUDPDegraded:
			sp.From = in.Start.Add(-homeDegradedLead).UnixMilli()
		default:
			continue
		}
		calls = append(calls, sp)
		recovered = max(recovered, in.End.UnixMilli())
	}
	return tcp, calls, recovered, nil
}

// ownCallState is the call state the call path and the measured apps
// score themselves, without the path verdict's say (calls() lets a lan,
// isp or upstream verdict make the top state poor; Home names that cause
// separately and must not claim calls are choppy when they are not).
func ownCallState(c *callsJSON) string {
	st := callNotEnoughData
	if c.Path != nil {
		st = c.Path.State
	}
	for _, a := range c.Apps {
		if a.callsGroup == nil {
			continue
		}
		if r := callRank(a.State); r >= callRank("unusable") && r <= callRank("fair") && r < callRank(st) {
			st = a.State
		}
	}
	return st
}

// callLevel maps a call state to a Home level ("" when it says nothing).
func callLevel(st string, quiet bool) string {
	switch st {
	case string(callq.Good):
		return levelGood
	case string(callq.Fair):
		return levelWarn
	case string(callq.Poor), string(callq.Unusable):
		return levelBad
	case callBlocked:
		if quiet {
			return levelInfo
		}
		return levelWarn
	case callNotEnoughData:
		return levelChecking
	case callNotAnswering:
		return levelInfo
	}
	return ""
}

// levelRank orders levels worst first.
func levelRank(l string) int {
	switch l {
	case levelBad:
		return 0
	case levelWarn:
		return 1
	case levelInfo:
		return 2
	case levelGood:
		return 3
	case levelChecking:
		return 4
	}
	return 5
}

func callsTile(c *callsJSON, own string, quiet bool, cp homeCopy) HomeTile {
	t := HomeTile{ID: tileCalls, Title: cp.tileTitle(tileCalls), Names: []string{}, Details: "#v=calls"}
	st := own
	if c.Path == nil && !slices.ContainsFunc(c.Apps, func(a callsApp) bool { return a.callsGroup != nil }) {
		// No call path in the profile: the apps' websites only.
		st = "reachable"
		for _, a := range c.Apps {
			if webState(a) == "not_answering" {
				st = "not_answering"
			}
		}
	}
	t.State, t.Level, t.Label = st, callLevel(st, quiet), cp.callHead(st, quiet)
	if t.Level == "" {
		t.Level = map[string]string{"reachable": levelGood, "not_answering": levelWarn}[st]
	}
	for _, a := range c.Apps {
		t.Total++
		title := appTitle(a.App)
		if len(t.Names) < homeMaxNames {
			t.Names = append(t.Names, title)
		} else {
			t.More++
		}
		switch {
		case a.callsGroup != nil && callLevel(a.State, quiet) == levelGood:
			t.OK++
		case a.callsGroup == nil && webState(a) == "reachable":
			t.OK++
		case a.callsGroup == nil && webState(a) == "not_answering":
			t.Affected = append(t.Affected, title)
		case a.callsGroup != nil && callLevel(a.State, quiet) == levelBad:
			t.Affected = append(t.Affected, title)
		}
	}
	if st == callNotAnswering {
		t.Note = cp.serversDown()
	}
	return t
}

// webState is a call app's website state over the live window.
func webState(a callsApp) string {
	switch {
	case a.WebSamples == 0 || a.WebLoss == nil:
		return callNotEnoughData
	case float64(*a.WebLoss) >= 0.5:
		return "not_answering"
	}
	return "reachable"
}

// tcpTile rolls up a TCP tile: one unhealthy target out of 3 or more is
// "mostly working"; more, or any of a smaller set, is slow or down.
func tcpTile(id string, ts []model.Target, rows []overviewRow, names map[string]string, cp homeCopy) HomeTile {
	t := HomeTile{ID: id, Title: cp.tileTitle(id), Names: []string{}, Total: len(ts), Details: "#v=o&ot=" + id}
	for _, x := range ts {
		if id == tileOther {
			break // cloud regions and the like: counts only, never names
		}
		if len(t.Names) < homeMaxNames {
			n, _ := strings.CutPrefix(homeName(x), cdnPrefix) // under the Websites title, the CDN's name alone
			t.Names = append(t.Names, n)
		} else {
			t.More++
		}
	}
	measured, failing, dead := 0, 0, 0
	var affected []string
	for _, r := range rows {
		switch r.State {
		case stateUnmeasured:
			continue
		case stateOK:
			t.OK++
		case stateFailing:
			failing++
			if r.N == 0 {
				dead++ // nothing got through: not answering at all
			}
			affected = append(affected, names[r.Target])
		default:
			affected = append(affected, names[r.Target])
		}
		measured++
	}
	t.Affected = affected
	if id == tileOther {
		t.Affected = nil
	}
	switch {
	case 2*measured < len(rows) || measured == 0:
		t.State = tileChecking
	case len(affected) == 0:
		t.State = tileWorking
	case len(affected) == 1 && measured >= 3:
		t.State = tileMostly
	case failing == measured || (failing >= 2 && 2*failing >= measured):
		t.State = tileDown // most of the tile (13 of 38 is not most)
	default:
		t.State = tileSlow
	}
	t.Level = map[string]string{tileWorking: levelGood, tileMostly: levelWarn, tileSlow: levelWarn, tileDown: levelBad, tileChecking: levelChecking}[t.State]
	t.Label = cp.tileLabel(t.State, id == tileOther, 2*dead >= measured, failing > 0)
	if id == tileOther {
		t.Note = cp.otherNote(t.State, len(affected), failing, dead, len(ts))
	} else {
		t.Note = cp.tileNote(id, t.State, affected, failing, dead, measured)
	}
	return t
}

// otherLine is the muted line about unhealthy targets outside the Home
// tiles; nil when there are none.
func otherLine(rows []overviewRow, cp homeCopy) *HomeOther {
	o := &HomeOther{Total: len(rows)}
	for _, r := range rows {
		if r.State != stateOK && r.State != stateUnmeasured {
			o.Affected++
		}
	}
	if o.Affected == 0 {
		return nil
	}
	o.Line = cp.otherLine(o.Affected)
	return o
}

// homeApps lists the call apps in callq.Apps order.
func homeApps(c *callsJSON, cp homeCopy) ([]HomeApp, string) {
	out := []HomeApp{}
	var picks []string
	pathState := callNotEnoughData
	if c.Path != nil {
		pathState = c.Path.State
	}
	for _, a := range c.Apps {
		x := HomeApp{App: a.App, Title: appTitle(a.App)}
		switch {
		case a.callsGroup != nil:
			x.State = a.State
		default:
			x.State = webState(a)
		}
		x.Level = callLevel(x.State, false)
		switch x.State {
		case "reachable":
			x.Level = levelGood
		case "not_answering":
			x.Level = levelWarn
		}
		x.Label = cp.callLabel(x.State)
		switch a.UDP {
		case "shared":
			x.Note = cp.sharedPath(cp.callLabel(pathState), a.OwnDown)
		case "not_measurable":
			picks = append(picks, x.Title) // one line for all of them: AppsNote
		}
		out = append(out, x)
	}
	note := ""
	if len(picks) > 0 {
		note = cp.picksServer(picks)
	}
	return out, note
}

// homeCallsWords describes the call path in words.
func homeCallsWords(g *callsGroup, own string, cp homeCopy) *HomeCalls {
	out := &HomeCalls{State: g.State}
	switch g.State {
	case callBlocked, callNotEnoughData, callNotAnswering:
		return out
	}
	rtt, loss, jit := float64(g.RTT), float64(g.Loss), float64(g.Jitter)
	if !math.IsNaN(rtt) {
		out.Delay = wordFor(rtt, []float64{50, 150, 300}, []string{"very_short", "short", "noticeable", "long"})
	}
	if !math.IsNaN(loss) {
		out.Dropouts = "none"
		if loss > 0 {
			out.Dropouts = wordFor(loss, []float64{0.01, 0.05}, []string{"rare", "some", "frequent"})
		}
	}
	if !math.IsNaN(jit) {
		out.Smoothness = wordFor(jit, []float64{10, 30}, []string{"steady", "bit_uneven", "uneven"})
	}
	out.Words = cp.callWords(out.Delay, out.Dropouts, out.Smoothness)
	if g.Worst != nil && own != string(callq.Good) {
		out.WorstAt = g.Worst.At
	}
	return out
}

func wordFor(x float64, cuts []float64, names []string) string {
	for i, c := range cuts {
		if x < c {
			return names[i]
		}
	}
	return names[len(names)-1]
}

// layerStat is a path layer over homePathWindow.
type layerStat struct {
	n, lost int64
	slow    bool
}

func (l layerStat) measured() bool { return l.n+l.lost > 0 }
func (l layerStat) loss() float64 {
	if !l.measured() {
		return 0
	}
	return float64(l.lost) / float64(l.n+l.lost)
}

// homePathRows reads the path targets (ICMP where they have it, else TCP)
// per layer.
func (s *server) homePathRows(ctx context.Context, path []model.Target, now time.Time) (map[string]layerStat, error) {
	out := map[string]layerStat{}
	byKind := map[model.ProbeKind][]model.Target{}
	for _, t := range path {
		k := model.KindICMP
		if !hasKind(t, k) {
			k = model.KindTCP
		}
		byKind[k] = append(byKind[k], t)
	}
	for k, ts := range byKind {
		rows, err := s.overviewRows(ctx, k, now.Add(-homePathWindow), now, ts)
		if err != nil {
			return nil, err
		}
		for i, t := range ts {
			l := out[t.Layer]
			l.n += rows[i].N
			l.lost += rows[i].Lost
			l.slow = l.slow || rows[i].State == stateSlow || rows[i].State == stateVerySlow
			out[t.Layer] = l
		}
	}
	return out, nil
}

// homePath builds the strip: device, router, provider, internet, apps.
func homePath(ls map[string]layerStat, a HomeAnswer, tiles []HomeTile, cp homeCopy) []HomeNode {
	blamed := map[string]string{whoOffline: "device", whoHome: "router", whoProvider: "provider", whoBeyond: "internet", whoApp: "apps", whoDNS: "apps"}[a.Who]
	if a.Confidence == confWeak {
		blamed = "" // slow to answer checks is not a culprit
	}
	order := []string{"device", "router", "provider", "internet", "apps"}
	past := func(id string) bool { // beyond the culprit: shows its trouble, isn't to blame
		bi := slices.Index(order, blamed)
		return blamed != "" && bi >= 0 && slices.Index(order, id) > bi
	}
	node := func(id string, l layerStat) HomeNode {
		n := HomeNode{ID: id, Label: cp.nodeLabel(id), Tooltip: cp.nodeTip(id)}
		loss := l.loss()
		switch {
		case !l.measured():
			n.State = "unknown"
		case loss >= 0.5:
			n.State = levelBad
		case loss >= 0.05 || l.slow:
			n.State = levelWarn
		default:
			n.State = "ok"
		}
		if id == blamed && (n.State == "ok" || n.State == "unknown") {
			n.State = levelWarn
		}
		if past(id) && (n.State == levelWarn || n.State == levelBad) {
			n.State = nodeAffected
		}
		n.Words = cp.nodeWords(id, n.State, loss, l.slow)
		if a.Who == whoOffline {
			n.State, n.Words = nodeAffected, cp.cantCheckOffline()
		}
		n.Blamed = id == blamed
		if n.Blamed {
			n.Culprit = cp.culprit(a.Confidence == confSure)
		}
		return n
	}
	out := []HomeNode{{ID: "device", Label: cp.nodeLabel("device"), State: "ok", Words: cp.nodeWords("device", "ok", 0, false)}}
	if a.Who == whoOffline {
		out[0].State, out[0].Words, out[0].Blamed, out[0].Culprit = levelBad, cp.nodeWords("device", levelBad, 1, false), true, cp.culprit(true)
	}
	out = append(out, node("router", ls[model.LayerGateway]), node("provider", ls[model.LayerEdge]), node("internet", ls[model.LayerAnycast]))

	// Apps & websites: from the tiles, so the strip and the tiles agree.
	apps := HomeNode{ID: "apps", Label: cp.nodeLabel("apps"), Tooltip: cp.nodeTip("apps"), State: "ok", Blamed: blamed == "apps"}
	var hurt []string
	worst, total, checking := levelGood, 0, 0
	for _, t := range tiles {
		if t.ID == tileOther && len(tiles) > 1 {
			continue
		}
		total++
		switch t.Level {
		case levelChecking:
			checking++
		case levelWarn, levelBad:
			hurt = append(hurt, strings.ToLower(t.Title))
			if levelRank(t.Level) < levelRank(worst) {
				worst = t.Level
			}
		}
	}
	switch {
	case total == 0 || checking == total:
		apps.State = "unknown"
	case len(hurt) == 0:
		apps.State = "ok"
	case past("apps"):
		apps.State = nodeAffected
	default:
		apps.State = worst
	}
	apps.Words = cp.appsWords(apps.State, hurt)
	if a.Who == whoOffline {
		apps.State, apps.Words = nodeAffected, cp.cantCheckOffline()
	}
	if apps.Blamed {
		apps.Culprit = cp.culprit(a.Confidence == confSure)
	}
	return append(out, apps)
}

// homeSteps is the warm-up progress.
func homeSteps(ls map[string]layerStat, tiles []HomeTile, cp homeCopy) []HomeStep {
	out := []HomeStep{
		{ID: "router", Label: cp.nodeLabel("router"), Done: ls[model.LayerGateway].measured()},
		{ID: "provider", Label: cp.nodeLabel("provider"), Done: ls[model.LayerEdge].measured()},
		{ID: "internet", Label: cp.nodeLabel("internet"), Done: ls[model.LayerAnycast].measured()},
		{ID: "apps", Label: cp.nodeLabel("apps")},
	}
	for _, t := range tiles {
		if t.Level != levelChecking {
			out[3].Done = true
		}
	}
	return out
}

// ---------- the answer ----------

// answerIn is what the answer is made of.
type answerIn struct {
	v     model.Verdict
	own   string // ownCallState
	calls *callsJSON
	tiles []HomeTile
	hs    homeSet
	names map[string]string // target name -> display name
	now   time.Time
}

// whoOf maps a verdict kind to Home's who.
func whoOf(k model.VerdictKind) string {
	switch k {
	case model.VerdictLAN:
		return whoHome
	case model.VerdictISP:
		return whoProvider
	case model.VerdictUpstream:
		return whoBeyond
	case model.VerdictDNS:
		return whoDNS
	case model.VerdictService:
		return whoApp
	case model.VerdictUDPBlocked, model.VerdictSTUNBlocked, model.VerdictUDPDegraded:
		return whoCallTraffic
	case model.VerdictNoNetwork:
		return whoOffline
	case VerdictUnknown:
		return whoUnknown
	}
	return whoNone
}

// confidenceOf picks the blame tier for a layer verdict: sure when the user
// notices it (internet targets losing ≥ 5%, calls poor or worse, or a Home
// tile slow or down), weak when only a router or provider hop answers
// checks badly and nothing the user uses is affected, else likely.
func confidenceOf(in answerIn) string {
	ev := in.v.Evidence
	sl, hasSL := ev["services_loss"]
	impact := (hasSL && sl >= 0.05) || in.own == string(callq.Poor) || in.own == string(callq.Unusable)
	quiet := (!hasSL || sl < 0.01) && callLevel(in.own, false) != levelBad && callLevel(in.own, false) != levelWarn
	for _, t := range in.tiles {
		if t.State == tileSlow || t.State == tileDown {
			impact = true
		}
		if t.Level == levelWarn || t.Level == levelBad {
			quiet = false
		}
	}
	switch {
	case impact:
		return confSure
	case quiet && confined(in.v):
		return confWeak
	}
	return confLikely
}

// confined reports a lan or isp verdict whose loss stays on the blamed hop:
// every known layer past it is clean. Loss that carries on outward is a
// real fault even when apps don't show it (TCP retransmits hide loss from
// services_loss); without the outer layers' evidence it can't be told.
func confined(v model.Verdict) bool {
	var outer []string
	switch v.Kind {
	case model.VerdictLAN:
		outer = []string{"edge_loss", "anycast_loss"}
	case model.VerdictISP:
		outer = []string{"anycast_loss"}
	default:
		return false
	}
	known := false
	for _, k := range outer {
		l, ok := v.Evidence[k]
		if !ok {
			continue
		}
		if l >= confinedLoss {
			return false
		}
		known = true
	}
	return known
}

// confinedLoss: an outer layer losing this much shares the fault.
const confinedLoss = 0.02

// homeAnswerOf makes the one answer: the worst of the verdict, the call
// state and the tiles. The headline comes from the worst; the cause from
// the verdict when it names a place (it knows the layer).
func homeAnswerOf(in answerIn, cp homeCopy) HomeAnswer {
	a := HomeAnswer{Kind: string(in.v.Kind), Try: []string{}}
	if in.calls != nil && in.own != callNotEnoughData {
		a.Calls = in.own
	}
	if in.v.Kind == model.VerdictWarmingUp {
		a.Level, a.Who = levelChecking, whoNone
		a.Headline, a.Cause = cp.checking()
		a.Announce = a.Headline
		return a
	}

	// The verdict's level and who.
	who := whoOf(in.v.Kind)
	vLevel := levelGood
	var affected []string
	switch who {
	case whoHome, whoProvider, whoBeyond:
		a.Confidence = confidenceOf(in)
		vLevel = map[string]string{confSure: levelBad, confLikely: levelWarn, confWeak: levelInfo}[a.Confidence]
		if who == whoBeyond && vLevel == levelInfo {
			vLevel = levelWarn
		}
	case whoDNS:
		a.Confidence = confidenceOf(in)
		if a.Confidence == confWeak {
			a.Confidence = confLikely
		}
		vLevel = levelWarn
	case whoOffline:
		vLevel = levelBad
	case whoApp:
		for _, n := range in.hs.relevant(in.v.Targets) {
			affected = append(affected, in.names[n])
		}
		if len(affected) == 0 {
			who = whoNone // a cloud region: Details only (and the muted line)
			break
		}
		// As bad as the worst tile it touches: a call app always warns;
		// one target out of a tile that is "mostly working" is a note,
		// not an alarm.
		vLevel = levelGood
		touched := false
		for _, t := range in.tiles {
			// The Other tile names nobody: with onlyOther it is the set the verdict is about.
			if !(t.ID == tileOther && in.hs.onlyOther()) && !slices.ContainsFunc(t.Affected, func(n string) bool { return slices.Contains(affected, n) }) {
				continue
			}
			touched = true
			l := t.Level
			switch {
			case t.ID == tileCalls:
				l = levelWarn
			case t.State == tileMostly:
				l = levelGood
			}
			if levelRank(l) < levelRank(vLevel) {
				vLevel = l
			}
		}
		if !touched {
			vLevel = levelWarn
		}
		if vLevel == levelGood {
			a.Cause = cp.oneApp(in.tiles, affected)
			who = whoNone
		}
	case whoCallTraffic:
		vLevel = levelWarn
		if in.v.Quiet {
			vLevel = levelInfo
		}
	case whoUnknown:
		who = whoNone
	}

	// The call state's level and the tiles' (only slow or down raise the
	// answer; "mostly working" is a note on its tile).
	cLevel := callLevel(in.own, in.v.Quiet)
	if cLevel == levelChecking {
		cLevel = ""
	}
	tLevel, worstTile := "", HomeTile{}
	for _, t := range in.tiles {
		if t.ID == tileCalls || (t.ID == tileOther && !in.hs.onlyOther()) {
			continue
		}
		if (t.State == tileSlow || t.State == tileDown) && (tLevel == "" || levelRank(t.Level) < levelRank(tLevel)) {
			tLevel, worstTile = t.Level, t
		}
	}
	level := vLevel
	for _, l := range []string{cLevel, tLevel} {
		if l != "" && levelRank(l) < levelRank(level) {
			level = l
		}
	}
	a.Level = level

	// Who: the verdict's, else the call path's, else the tile's.
	callsWho, callsApp := "", ""
	if in.calls != nil && in.calls.Cause != nil && (cLevel == levelBad || cLevel == levelWarn) {
		switch in.calls.Cause.Source {
		case "path", "udp":
			callsWho = whoCallTraffic
		case "app":
			callsWho, callsApp = whoApp, in.calls.Cause.App
		}
	}
	if (cLevel == levelBad || cLevel == levelWarn) && callsWho == "" {
		callsWho = whoCallTraffic
	}
	switch {
	case who != whoNone:
	case callsWho != "":
		who = callsWho
		a.App = callsApp
	case tLevel != "":
		who = whoApp
		affected = worstTile.Affected
	}
	a.Who = who
	if who == whoApp && a.App == "" && len(affected) > 0 && !in.hs.onlyOther() {
		a.App = affected[0]
	}

	// Since: the verdict's, or the start of the current bad call run.
	if who != whoNone && whoOf(in.v.Kind) == who && !in.v.Since.IsZero() {
		a.Since = in.v.Since.UnixMilli()
	} else if callsWho != "" && who == callsWho {
		a.Since = callRunStart(in.calls)
	}

	// Headline: from the worst source; cause from the verdict when it
	// names a place.
	// A path verdict (home network, provider, further out, offline, name
	// lookups) names the place in the headline; the Calls tile says what
	// it does to calls.
	layer := who == whoOf(in.v.Kind) && slices.Contains([]string{whoHome, whoProvider, whoBeyond, whoOffline, whoDNS}, who) && a.Confidence != confWeak
	callsDrive := !layer && cLevel != "" && cLevel == level && (level == levelBad || level == levelWarn) &&
		in.v.Kind != model.VerdictUDPBlocked && in.v.Kind != model.VerdictSTUNBlocked
	switch {
	case level == levelGood || (level == levelInfo && a.Confidence == confWeak):
		a.Headline = cp.fine(in.hs.home > 0)
	case layer:
		a.Headline = cp.verdictHead(in.v.Kind, a.Confidence, in.v.Quiet, affected, in.hs.onlyOther())
	case callsDrive:
		a.Headline = cp.callHead(in.own, false)
	case vLevel == level && whoOf(in.v.Kind) == who && who != whoNone:
		a.Headline = cp.verdictHead(in.v.Kind, a.Confidence, in.v.Quiet, affected, in.hs.onlyOther())
	case tLevel != "":
		a.Headline = cp.tileHead(worstTile)
	default:
		a.Headline = cp.callHead(in.own, in.v.Quiet)
	}
	switch {
	case who == whoNone && a.Cause != "":
	case who == whoNone:
		a.Cause = cp.everything(in.hs.home > 0, in.tiles)
	case whoOf(in.v.Kind) == who:
		a.Cause = cp.verdictCause(in.v.Kind, a.Confidence, in.v.Quiet, affected, in.hs.onlyOther())
	case who == whoCallTraffic:
		factor := ""
		if in.calls != nil && in.calls.Cause != nil {
			factor = in.calls.Cause.Factor
		}
		a.Cause = cp.pathCause(factor)
	case who == whoApp && callsApp != "":
		a.Cause = cp.appWorse(appTitle(callsApp))
	default:
		a.Cause = cp.tileCause(worstTile)
	}
	if in.v.Quiet && who == whoCallTraffic {
		a.Hint = cp.quietHint()
	}
	if who != whoNone && level != levelGood {
		a.Try = cp.try(who, in.v.Kind, a.Confidence)
	}
	if a.Calls != "" && (cLevel == levelBad || cLevel == levelWarn) {
		a.Fact, a.FactAt = worstFact(in.calls, cp)
	}
	a.Announce = strings.TrimSpace(a.Headline + ". " + firstSentence(a.Cause))
	return a
}

// callRunStart is the first minute of the current run of fair-or-worse
// call-path minutes (0 when the last scored minute is good).
func callRunStart(c *callsJSON) int64 {
	if c == nil || c.Path == nil {
		return 0
	}
	st := c.Path.Minutes.State
	start := int64(0)
	for i := len(st) - 1; i >= 0; i-- {
		switch st[i] {
		case "":
			continue
		case string(callq.Good):
			return start
		}
		start = c.Start + int64(i)*c.Step
	}
	return start
}

// worstFact is the one labelled fact about the worst minute, in words.
func worstFact(c *callsJSON, cp homeCopy) (string, int64) {
	if c == nil {
		return "", 0
	}
	// The call path's worst minute, so every view gives one figure for a
	// minute; an app's only without a path.
	g := c.Path
	for _, a := range c.Apps {
		if a.callsGroup != nil && a.Worst != nil && (g == nil || g.Worst == nil) {
			g = a.callsGroup
		}
	}
	if g == nil || g.Worst == nil || g.Worst.State == string(callq.Good) {
		return "", 0
	}
	return cp.fact(g.Factor, float64(g.Worst.Loss)), g.Worst.At
}

func firstSentence(s string) string {
	if i := strings.Index(s, ". "); i >= 0 {
		return s[:i+1]
	}
	return s
}

// ---------- history ----------

// homeIncident is an outage-log incident as Home sees it.
type homeIncident struct {
	model.Incident
	who   string
	level string
	label string
	// services: an app incident about "other" targets only (cloud regions
	// and the like), worded as services, never apps or websites.
	services bool
}

func (s *server) homeHistory(ctx context.Context, p homeParams, now time.Time) (*HomeHistory, error) {
	prof := s.d.Profile()
	hs := newHomeSet(prof)
	cp := homeCopy{}
	loc := time.FixedZone("", p.tz*60)
	ln := now.In(loc)
	today := time.Date(ln.Year(), ln.Month(), ln.Day(), 0, 0, 0, 0, loc)
	days := 1
	if p.span == "w" {
		days = 7
	}
	from := today.AddDate(0, 0, 1-days)
	out := &HomeHistory{Part: "history", At: now.UnixMilli(), TZ: p.tz, Span: p.span,
		From: from.UnixMilli(), To: now.UnixMilli(), Days: []HomeDay{}, Problems: []HomeProblem{}}

	// When fyisp was measuring: a few heartbeat series.
	beat := heartbeatKeys(prof)
	firsts, ok := s.d.firstSamples(ctx, now)
	var first time.Time
	if ok {
		for _, k := range beat {
			if f, ok := firsts[k]; ok && (first.IsZero() || f.Before(first)) {
				first = f
			}
		}
	}
	if !first.IsZero() {
		out.FirstData = first.UnixMilli()
	}
	var off []HomeSpan
	if !first.IsZero() && len(beat) > 0 {
		var err error
		if off, err = s.offSpans(ctx, beat, later(from, first), today, now); err != nil {
			return nil, err
		}
		off = s.withoutSpeedTests(ctx, off, from, now)
	}

	// Incidents, relevant to Home.
	var incs []homeIncident
	if s.d.Verdict != nil {
		list, err := s.d.Verdict.Incidents(ctx, from, now)
		if err != nil {
			return nil, err
		}
		for _, in := range list {
			if !in.Start.Before(now) || (!in.End.IsZero() && !in.End.After(from)) {
				continue
			}
			who := whoOf(in.Kind)
			if who == whoNone || who == whoUnknown {
				continue
			}
			x := homeIncident{Incident: in, who: who, level: levelWarn}
			switch in.Kind {
			case model.VerdictNoNetwork, model.VerdictLAN, model.VerdictISP, model.VerdictUpstream:
				x.level = levelBad
			case model.VerdictUDPBlocked, model.VerdictSTUNBlocked:
				end := now
				if !in.End.IsZero() {
					end = in.End
				}
				if end.Sub(in.Start) >= verdict.UDPQuietAfter {
					x.level = levelInfo // a network that always blocks calls: information, not a problem
				}
			}
			if who == whoApp {
				rel := hs.relevant(in.Targets)
				if len(rel) == 0 {
					continue // a cloud region the user doesn't use
				}
				x.services = !slices.ContainsFunc(rel, func(n string) bool { return hs.cat[n] != tileOther })
				if len(rel) == 1 && !x.services {
					x.label = homeNameOf(prof, rel[0])
				}
				if x.services {
					x.label = cp.someServices()
				}
			}
			incs = append(incs, x)
		}
	}

	// Today's bad call minutes that no incident covers.
	var callRuns []HomeRun
	var work *HomeWorkCalls
	if c, err := s.callsToday(ctx, today, now); err != nil {
		return nil, err
	} else if c != nil && c.Path != nil {
		callRuns = callMinuteRuns(c, incs, cp)
		work = workCalls(c, incs, today, now)
	}

	for d := days - 1; d >= 0; d-- {
		ds := today.AddDate(0, 0, -d)
		de := ds.AddDate(0, 0, 1)
		day := HomeDay{Date: ds.Format("2006-01-02"), From: ds.UnixMilli(), To: de.UnixMilli(), Runs: []HomeRun{}, Off: []HomeSpan{}}
		if d == 0 {
			day.To = now.UnixMilli()
		}
		if wd := ds.Weekday(); wd != time.Saturday && wd != time.Sunday {
			day.WorkFrom = ds.Add(homeWorkFrom * time.Hour).UnixMilli()
			day.WorkTo = ds.Add(homeWorkTo * time.Hour).UnixMilli()
		}
		day.NoRecord = first.IsZero() || !de.After(first)
		for _, sp := range off {
			if sp.From < day.To && sp.To > day.From {
				day.Off = append(day.Off, HomeSpan{From: max(sp.From, day.From), To: min(sp.To, day.To)})
			}
		}
		for _, in := range incs {
			end := now
			if !in.End.IsZero() {
				end = in.End
			}
			if !in.Start.Before(de) || !end.After(ds) {
				continue
			}
			day.Runs = append(day.Runs, HomeRun{From: max(in.Start.UnixMilli(), day.From), To: min(end.UnixMilli(), day.To),
				Level: in.level, Who: in.who, Kind: string(in.Kind), Label: cp.runLabel(in.who, in.label), Source: "incident",
				Text:     cp.wasWhere(in.who, in.Kind),
				Incident: in.ID, Ongoing: in.End.IsZero()})
		}
		if d == 0 {
			day.Runs = append(day.Runs, callRuns...)
		}
		slices.SortFunc(day.Runs, func(a, b HomeRun) int { return int(a.From - b.From) })
		out.Days = append(out.Days, day)
	}

	for _, in := range incs { // newest first
		switch {
		case in.level == levelInfo:
			// not a problem; still in the table, the timeline and runs
		case in.services:
			out.Counts.Problems++
			out.Counts.Services++
		default:
			out.Counts.add(in.who)
		}
		if len(out.Problems) >= homeMaxProblems {
			continue
		}
		pr := HomeProblem{ID: in.ID, From: in.Start.UnixMilli(), Ongoing: in.End.IsZero(), Kind: string(in.Kind), Who: in.who,
			Where: cp.where(in.who, in.label), Side: sideOf(in.who), Level: in.level, Text: cp.wasWhere(in.who, in.Kind),
			PeakLoss: clamp01(in.PeakLoss)}
		if in.who == whoHome || in.who == whoProvider {
			pr.Confidence = confLikely
		}
		end := now
		if !in.End.IsZero() {
			pr.To = in.End.UnixMilli()
			end = in.End
		}
		pr.Details = "#v=calls&from=" + strconv.FormatInt(in.Start.Add(-30*time.Minute).UnixMilli(), 10) +
			"&to=" + strconv.FormatInt(min(end.Add(30*time.Minute).UnixMilli(), now.UnixMilli()), 10) + "&o=1"
		out.Problems = append(out.Problems, pr)
	}
	out.Counts.CallPatches = len(callRuns)
	// One app named when every app problem was about the same one; a
	// long-standing block is a known issue, not a problem.
	app, apps, known := "", map[string]bool{}, false
	for _, in := range incs {
		if in.who == whoApp && !in.services {
			apps[in.label] = true
			app = in.label
		}
		if in.level == levelInfo {
			known = true
			if in.End.IsZero() && (out.Known == nil || in.Start.UnixMilli() < out.Known.Since) {
				out.Known = &HomeKnown{Since: in.Start.UnixMilli(), Text: cp.knownBlock()}
			}
		}
	}
	if len(apps) != 1 {
		app = ""
	}
	out.Summary = cp.summary(out.Counts, p.span, first.IsZero() || (first.After(now.Add(-10*time.Minute)) && out.Counts.Problems == 0), app, known)
	if work != nil && work.Measured > 0 {
		out.Work = work
	}
	return out, nil
}

func (c *HomeCounts) add(who string) {
	c.Problems++
	switch who {
	case whoHome:
		c.Home++
	case whoProvider:
		c.Provider++
	case whoBeyond:
		c.Beyond++
	case whoDNS:
		c.DNS++
	case whoApp:
		c.App++
	case whoCallTraffic:
		c.CallTraffic++
	case whoOffline:
		c.Offline++
	}
}

func homeNameOf(p *model.Profile, name string) string {
	if p != nil {
		for _, t := range p.Targets {
			if t.Name == name {
				return homeName(t)
			}
		}
	}
	return strings.ReplaceAll(name, "-", " ")
}

// heartbeatKeys are the series that tell whether fyisp was running: the
// path targets' main kind, else the first few targets'.
func heartbeatKeys(p *model.Profile) []model.SeriesKey {
	var out []model.SeriesKey
	if p == nil {
		return nil
	}
	add := func(t model.Target) {
		if len(out) < homeHeartbeat {
			out = append(out, model.SeriesKey{Target: t.Name, Kind: t.ProbeKinds()[0]})
		}
	}
	for _, t := range p.Targets {
		if isPath(t) {
			add(t)
		}
	}
	if len(out) == 0 {
		for _, t := range p.Targets {
			if len(t.ProbeKinds()) > 0 {
				add(t)
			}
		}
	}
	return out
}

// offSpans finds when nothing was measured between from and now: per
// minute today (raw tier), per hour before (hourly tier; a partly measured
// hour counts as running). The last minute is never off (it may still be
// arriving).
func (s *server) offSpans(ctx context.Context, keys []model.SeriesKey, from, today, now time.Time) ([]HomeSpan, error) {
	var out []HomeSpan
	addRun := func(a, b int64) {
		if n := len(out); n > 0 && out[n-1].To >= a {
			out[n-1].To = max(out[n-1].To, b)
			return
		}
		out = append(out, HomeSpan{From: a, To: b})
	}
	scan := func(qf, qt time.Time, maxPoints int) error {
		if !qf.Before(qt) {
			return nil
		}
		res, err := s.d.Store.Panel(ctx, store.PanelQuery{Keys: keys, From: qf, To: qt, MaxPoints: maxPoints})
		if err != nil {
			return err
		}
		step := res.Step
		if step <= 0 {
			return nil
		}
		nb := 0
		for i := range res.Series {
			nb = max(nb, len(res.Series[i].N))
		}
		for i := range nb {
			bs := res.Start.Add(time.Duration(i) * step)
			be := bs.Add(step)
			if !be.After(qf) || !bs.Before(qt) {
				continue
			}
			seen := false
			for j := range res.Series {
				c := &res.Series[j]
				if at(c.N, i)+at(c.Lost, i) > 0 {
					seen = true
					break
				}
			}
			if !seen {
				addRun(later(bs, qf).UnixMilli(), min(be.UnixMilli(), qt.UnixMilli()))
			}
		}
		return nil
	}
	if from.Before(today) {
		hours := int(today.Sub(from)/time.Hour) + 2
		if err := scan(from, today, hours); err != nil {
			return nil, err
		}
	}
	settled := now.Add(-time.Minute).Truncate(time.Minute)
	start := later(from, today)
	if err := scan(start, settled, int(settled.Sub(start)/time.Minute)+2); err != nil {
		return nil, err
	}
	return out, nil
}

// callsToday reads today's call minutes (at least CallsMinRange, at most
// CallsMaxRange).
func (s *server) callsToday(ctx context.Context, today, now time.Time) (*callsJSON, error) {
	from := today
	if now.Sub(from) < CallsMinRange {
		from = now.Add(-CallsMinRange)
	}
	if now.Sub(from) > CallsMaxRange {
		from = now.Add(-CallsMaxRange)
	}
	return s.calls(ctx, callsParams{from: from, to: now})
}

// callMinuteRuns are today's runs of fair (warn) or poor/unusable (bad)
// call-path minutes, outside incidents.
func callMinuteRuns(c *callsJSON, incs []homeIncident, cp homeCopy) []HomeRun {
	covered := func(t int64) bool {
		for _, in := range incs {
			end := int64(math.MaxInt64)
			if !in.End.IsZero() {
				end = in.End.UnixMilli()
			}
			if t >= in.Start.UnixMilli() && t < end {
				return true
			}
		}
		return false
	}
	var out []HomeRun
	for i, st := range c.Path.Minutes.State {
		lv := ""
		switch st {
		case string(callq.Fair):
			lv = levelWarn
		case string(callq.Poor), string(callq.Unusable):
			lv = levelBad
		}
		t := c.Start + int64(i)*c.Step
		if lv == "" || covered(t) {
			continue
		}
		if n := len(out); n > 0 && out[n-1].To == t {
			out[n-1].To = t + c.Step
			if levelRank(lv) < levelRank(out[n-1].Level) {
				out[n-1].Level, out[n-1].Kind, out[n-1].Text = lv, st, cp.callPatch(lv)
			}
			continue
		}
		out = append(out, HomeRun{From: t, To: t + c.Step, Level: lv, Who: whoCallTraffic, Kind: st,
			Label: cp.runLabel(whoCallTraffic, ""), Source: "calls", Text: cp.callPatch(lv)})
	}
	return out
}

// workCalls is today's call quality in working hours so far.
// Minutes while the network blocked call traffic don't count: calls take
// a relay then, which this can't judge.
func workCalls(c *callsJSON, incs []homeIncident, today, now time.Time) *HomeWorkCalls {
	if wd := today.Weekday(); wd == time.Saturday || wd == time.Sunday {
		return nil
	}
	wf, wt := today.Add(homeWorkFrom*time.Hour).UnixMilli(), today.Add(homeWorkTo*time.Hour).UnixMilli()
	w := &HomeWorkCalls{}
	good := 0
	for i, st := range c.Path.Minutes.State {
		t := c.Start + int64(i)*c.Step
		if st == "" || t < wf || t >= wt || t >= now.UnixMilli() || blockedAt(incs, t) {
			continue
		}
		w.Measured++
		switch st {
		case string(callq.Good), string(callq.Fair):
			good++
		default:
			w.Bad++
		}
	}
	if w.Measured > 0 {
		w.Good = round2(float64(good) / float64(w.Measured))
	}
	return w
}

// ---------- what happened at ----------

// homeAtWindow is the span around a picked time ("What happened at…?",
// "I just had a bad call").
const homeAtWindow = 15 * time.Minute

// homeAt answers "what happened around at?": the worst call minute and the
// incidents in at±15 min, in one headline and one cause.
func (s *server) homeAt(ctx context.Context, at time.Time, now time.Time) (*HomeAt, error) {
	cp := homeCopy{}
	prof := s.d.Profile()
	hs := newHomeSet(prof)
	from, to := at.Add(-homeAtWindow).Truncate(time.Minute), at.Add(homeAtWindow)
	if to.After(now) {
		to = now
	}
	if to.Sub(from) < CallsMinRange {
		from = to.Add(-CallsMinRange)
	}
	out := &HomeAt{Part: "at", At: now.UnixMilli(), Time: at.UnixMilli(), From: from.UnixMilli(), To: to.UnixMilli()}

	c, err := s.calls(ctx, callsParams{from: from, to: to})
	if err != nil {
		return nil, err
	}
	worst, scored := "", 0
	var bad []int
	if c.Path != nil {
		for i, st := range c.Path.Minutes.State {
			if st == "" {
				continue
			}
			scored++
			if worst == "" || callRank(st) < callRank(worst) {
				worst, bad = st, bad[:0]
			}
			if st == worst {
				bad = append(bad, i)
			}
		}
	}
	out.Measured = scored > 0
	if scored > 0 {
		out.Calls = worst
		if worst != string(callq.Good) && len(bad) > 0 {
			out.BadFrom = c.Start + int64(bad[0])*c.Step
			out.BadTo = c.Start + int64(bad[len(bad)-1]+1)*c.Step
			out.Fact, out.FactAt = worstFact(c, cp)
		}
	}

	// The worst relevant incident overlapping the window names the cause.
	var inc *model.Incident
	if s.d.Verdict != nil {
		list, err := s.d.Verdict.Incidents(ctx, from, to)
		if err != nil {
			return nil, err
		}
		for i := range list {
			in := &list[i]
			if !in.Start.Before(to) || (!in.End.IsZero() && !in.End.After(from)) {
				continue
			}
			who := whoOf(in.Kind)
			if who == whoNone || who == whoUnknown || (who == whoApp && len(hs.relevant(in.Targets)) == 0) {
				continue
			}
			if inc == nil || incidentRank(in.Kind) < incidentRank(inc.Kind) {
				inc = in
			}
		}
	}
	out.Level, out.Who = levelGood, whoNone
	if l := callLevel(worst, false); l != "" && l != levelChecking && l != levelInfo {
		out.Level = l
	}
	if inc != nil {
		out.Kind, out.Who = string(inc.Kind), whoOf(inc.Kind)
		l := levelWarn
		switch inc.Kind {
		case model.VerdictNoNetwork, model.VerdictLAN, model.VerdictISP, model.VerdictUpstream:
			l = levelBad
		}
		if levelRank(l) < levelRank(out.Level) {
			out.Level = l
		}
	} else if out.Level != levelGood {
		out.Who = whoCallTraffic
	}
	if inc != nil && (inc.Kind == model.VerdictUDPBlocked || inc.Kind == model.VerdictSTUNBlocked) && callRank(worst) <= callRank(string(callq.Poor)) {
		out.Calls, worst = callBlocked, callBlocked // the block, not the call quality
		out.Fact, out.FactAt, out.BadFrom, out.BadTo = "", 0, 0, 0
	}
	if !hasCallPath(prof) {
		out.Measured = true // nothing to say about calls; the incidents answer
		worst = ""
	}
	out.Headline, out.Cause = cp.atWords(out.Measured, worst, inc != nil, out.Who, out.Kind)
	s.speedAt(ctx, out, from, to)
	return out, nil
}

// speedAt notes a speed test that ran in the window: it fills the line for
// a few seconds and can make calls stutter; at 10 s or more of overlap it
// is the most likely explanation.
func (s *server) speedAt(ctx context.Context, out *HomeAt, from, to time.Time) {
	if s.d.Speed == nil {
		return
	}
	ts, err := s.d.Speed.SpeedTests(ctx, from.Add(-time.Minute), to)
	if err != nil {
		return
	}
	var overlap time.Duration
	for _, t := range ts {
		a, b := t.Start, t.End.Add(2*time.Second)
		if a.Before(from) {
			a = from
		}
		if b.After(to) {
			b = to
		}
		if b.After(a) {
			overlap += b.Sub(a)
		}
	}
	if overlap <= 0 {
		return
	}
	out.SpeedTestOverlapS = int(overlap.Round(time.Second) / time.Second)
	note := "A speed test was running then. It fills the line for a few seconds and can make calls stutter; we don't count that against the connection."
	if overlap >= 10*time.Second {
		note = "Most likely our speed test: it was running then, and it fills the line for a few seconds. We don't count that against the connection."
	}
	out.SpeedNote = note
}

// incidentRank orders incident kinds by how much they explain, worst first.
func incidentRank(k model.VerdictKind) int {
	for i, x := range []model.VerdictKind{model.VerdictNoNetwork, model.VerdictLAN, model.VerdictISP, model.VerdictUpstream,
		model.VerdictUDPBlocked, model.VerdictSTUNBlocked, model.VerdictUDPDegraded, model.VerdictDNS, model.VerdictService} {
		if x == k {
			return i
		}
	}
	return 99
}

// blockedAt reports a UDP or STUN block incident at t (ms).
func blockedAt(incs []homeIncident, t int64) bool {
	for _, in := range incs {
		if in.Kind != model.VerdictUDPBlocked && in.Kind != model.VerdictSTUNBlocked {
			continue
		}
		if t >= in.Start.UnixMilli() && (in.End.IsZero() || t < in.End.UnixMilli()) {
			return true
		}
	}
	return false
}

// hasCallPath reports a profile with STUN targets on the call path.
func hasCallPath(p *model.Profile) bool {
	if p == nil {
		return false
	}
	return slices.ContainsFunc(p.Targets, callq.IsCorePath)
}

// sideOf groups who for the report.
func sideOf(who string) string {
	switch who {
	case whoHome, whoOffline:
		return "home"
	case whoProvider:
		return "provider"
	}
	return "other"
}

// withoutSpeedTests removes speed test windows from "not running" spans:
// probes in a test window are stored as not measured, but fyisp was
// running. A window is widened to the whole minutes it touches (and the
// mask's tail), the buckets it can empty.
func (s *server) withoutSpeedTests(ctx context.Context, off []HomeSpan, from, now time.Time) []HomeSpan {
	if s.d.Speed == nil || len(off) == 0 {
		return off
	}
	ts, err := s.d.Speed.SpeedTests(ctx, from.Add(-time.Minute), now)
	if err != nil || len(ts) == 0 {
		return off
	}
	var out []HomeSpan
	for _, sp := range off {
		parts := []HomeSpan{sp}
		for _, t := range ts {
			a := t.Start.Truncate(time.Minute).UnixMilli()
			b := t.End.Add(speedtest.MaskTail).Truncate(time.Minute).Add(time.Minute).UnixMilli()
			var next []HomeSpan
			for _, p := range parts {
				if b <= p.From || a >= p.To {
					next = append(next, p)
					continue
				}
				if p.From < a {
					next = append(next, HomeSpan{From: p.From, To: a})
				}
				if b < p.To {
					next = append(next, HomeSpan{From: b, To: p.To})
				}
			}
			parts = next
		}
		out = append(out, parts...)
	}
	return out
}
