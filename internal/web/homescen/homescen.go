// Package homescen builds the Home's scenarios (/api/home): each is a tiles
// engine snapshot, its charts and problems written out by hand (Scenario is
// a web.TilesSource), the speed tests, the network path and who is looking.
// They pin the server's words for every state of the answer card, the
// strip, the tiles and the panels (internal/web/testdata/home) and are
// served by cmd/fyisp-webdev -home. The engine's own rules are tested in
// internal/tiles. Times are relative to now: the goldens use Friday
// 2026-10-09 10:30 am in New York, so "10:05 am" is 25 minutes ago. Not
// used by fyisp itself.
package homescen

import (
	"context"
	"fmt"
	"hash/fnv"
	"math"
	"net/netip"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/netinfo"
	"github.com/fuck-you-isp/fyisp/internal/profile"
	"github.com/fuck-you-isp/fyisp/internal/speedtest"
	"github.com/fuck-you-isp/fyisp/internal/tiles"
)

// Names lists the scenarios.
var Names = []string{
	// the mockup's states
	"good", "blip", "isp", "zoom", "zoom-working-again", "learning", "stale",
	// the other rows of the answer card
	"checking", "router-down", "upstream", "no-network", "drop-now", "dns", "udp-blocked", "udp-quiet",
	"udp-degraded", "broad-slow", "google-down", "no-data-yet",
	// working again
	"outage-2m-recovered", "trouble-recovered",
	// drops and call quality
	"drop-20s", "provider-loss-6", "provider-loss-3", "jitter",
	// who is looking
	"wifi-same-machine", "wifi-remote-viewer", "container-unknown", "edge-unknown",
	// coverage and history
	"calls-profile", "no-streaming", "custom-config", "week-with-problems", "not-running",
	// speed
	"speed-running", "speed-no-usual", "speed-done", "speed-upload-slow", "speed-download-slow", "speedtest-labelled",
	// the end of daylight saving time in New York (Sunday 2026-11-01)
	"dst",
}

// Tiles are the tiles whose panel (part=tile) a scenario pins: zoom and
// m365 always, and these.
var Tiles = map[string][]string{
	"good": {"teams", "meet", "youtube"}, "learning": {"gws"}, "isp": {"netflix"}, "jitter": {"teams"},
	"udp-degraded": {"meet"}, "google-down": {"meet", "youtube"}, "trouble-recovered": {"slack"},
	"blip": {"teams"}, "speedtest-labelled": {"gws"}, "not-running": {"github"}, "dst": {"teams"},
	"provider-loss-3": {"teams"}, "edge-unknown": {"onedrive"}, "udp-quiet": {"teams"}, "no-data-yet": {"teams"},
}

// DSTNow is the dst scenario's time: Sunday 2026-11-01 10:30 am in New
// York, the morning clocks went back.
var DSTNow = time.Date(2026, 11, 1, 15, 30, 0, 0, time.UTC)

// Scenario is one situation: a web.TilesSource, plus what else the Home reads.
type Scenario struct {
	Name    string
	Profile *model.Profile
	Snap    *tiles.Snapshot
	Eps     []tiles.Episode // newest first
	Charts  map[string]*tiles.Chart
	// NotReady: the engine is still replaying (the checking card).
	NotReady bool
	// Path is fyisp's network path; Container that it runs in one.
	Path      netinfo.Path
	Container bool
	// Viewer is who asks.
	Viewer Viewer
	// CustomProfile: the profile is a --config file.
	CustomProfile bool
	Speed         *Speed // nil: no speed tests
}

// Viewer is the request's origin: its RemoteAddr, proxy headers and Host,
// this host's own addresses, and the address fyisp listens on.
type Viewer struct {
	Listen     string
	RemoteAddr string
	Host       string
	Header     map[string]string
	Own        []netip.Addr
}

// Speed is the speed tests a scenario has (the web package's speed source
// for Home).
type Speed struct {
	Scheduled bool
	List      []model.SpeedTest
	Run       *speedtest.Entry
}

func (s *Speed) On() bool { return s.Scheduled }
func (s *Speed) Tests(_ context.Context, from, to time.Time) ([]model.SpeedTest, error) {
	var out []model.SpeedTest
	for _, t := range s.List {
		if !t.Start.Before(from) && t.Start.Before(to) {
			out = append(out, t)
		}
	}
	return out, nil
}
func (s *Speed) Running() *speedtest.Entry { return s.Run }

// Ready, Snapshot, Chart, Problems and LateSamples make Scenario a
// web.TilesSource.
func (v *Scenario) Ready() bool               { return !v.NotReady }
func (v *Scenario) Snapshot() *tiles.Snapshot { return v.Snap }
func (v *Scenario) LateSamples() int          { return 0 }
func (v *Scenario) Chart(id string) (*tiles.Chart, bool) {
	c, ok := v.Charts[id]
	return c, ok
}
func (v *Scenario) Problems(from time.Time) []tiles.Episode {
	var out []tiles.Episode
	for _, e := range v.Eps {
		if e.Ongoing() || !e.End.Before(from) {
			out = append(out, e)
		}
	}
	return out
}

// usualMs are the tiles' usual latencies, in profile.Tiles' order.
var usualMs = []float64{18, 14, 9, 22, 12, 8, 24, 15, 13, 7, 6}

const (
	bar     = 5 * time.Minute
	nBars   = 24
	nBucket = 288
)

// sb builds a scenario's snapshot.
type sb struct {
	now time.Time
	v   *Scenario
	s   *tiles.Snapshot
	off [][2]time.Time // fyisp not running
}

func (b *sb) ago(d time.Duration) time.Time { return b.now.Add(-d) }

func (b *sb) idx(id string) int {
	return slices.IndexFunc(profile.Tiles, func(d profile.TileDef) bool { return d.ID == id })
}

func (b *sb) tile(id string) *tiles.TileSnap { return &b.s.Tiles[b.idx(id)] }

// set gives tiles a word since a time (bars from then on follow it).
func (b *sb) set(st tiles.State, since time.Time, ids ...string) {
	for _, id := range ids {
		t := b.tile(id)
		t.State, t.Since = st, since
		for i := range t.Bars {
			if !t.Bars[i].From.Add(bar).After(since) {
				continue
			}
			t.Bars[i].State = st
		}
	}
}

// add records a problem: its bars (when it has tiles; a connection
// problem's bars come from the tiles' words or drops) and the list.
func (b *sb) add(e tiles.Episode) {
	e.ID = episodeID(e)
	b.v.Eps = append(b.v.Eps, e)
	ids := e.Tiles
	for _, id := range ids {
		t := b.tile(id)
		for i := range t.Bars {
			x := &t.Bars[i]
			if !overlaps(x.From, x.From.Add(bar), e.Start, e.End, b.now) {
				continue
			}
			// A bar notes the problem that coloured it.
			switch {
			case e.Kind == tiles.EpDrop:
				x.State, x.Drop, x.Episode = tiles.Down, e.DropDur, e.ID
				if e.Ongoing() {
					x.Drop = b.now.Sub(e.Start)
				}
			case e.Connection:
				// A connection problem colours no bar: the tiles' own minutes
				// do (set, paint); it only explains a bad bar.
				if x.Episode == "" && x.State.Bad() {
					x.Episode = e.ID
				}
			case rank(e.Sev) >= rank(x.State):
				x.State, x.Episode = e.Sev, e.ID
			case x.Episode == "":
				x.Episode = e.ID
			}
		}
	}
}

// overlaps reports [a0, a1) and [b0, b1) overlapping; an ongoing b (b1
// zero) reaches into the current bar.
// paint colours tiles' bars over [from, to) (minutes judged then).
func (b *sb) paint(st tiles.State, from, to time.Time, ids ...string) {
	for _, id := range ids {
		t := b.tile(id)
		for i := range t.Bars {
			if overlaps(t.Bars[i].From, t.Bars[i].From.Add(bar), from, to, b.now) {
				t.Bars[i].State = st
			}
		}
	}
}

func rank(s tiles.State) int {
	return map[tiles.State]int{tiles.Good: 1, tiles.Slow: 2, tiles.Down: 3}[s]
}

// noCalls: nothing gets through, so the call tiles' bursts all time out.
func (b *sb) noCalls() {
	for _, id := range callTiles() {
		if t := b.tile(id); t.Call != nil && t.State == tiles.Down {
			*t.Call = tiles.CallMetrics{Source: t.Call.Source}
		}
	}
}

func overlaps(a0, a1, b0, b1, now time.Time) bool {
	if b1.IsZero() {
		b1 = now.Add(time.Nanosecond)
	}
	return a0.Before(b1) && b0.Before(a1)
}

func episodeID(e tiles.Episode) string {
	ms := strconv.FormatInt(e.Start.UnixMilli(), 10)
	if e.Connection {
		return "c-" + ms
	}
	id := "t-" + ms + "-"
	for i, t := range e.Tiles {
		if i > 0 {
			id += ","
		}
		id += t
	}
	return id
}

func allTiles() []string {
	var out []string
	for _, d := range profile.Tiles {
		out = append(out, d.ID)
	}
	return out
}

func callTiles() []string { return []string{"zoom", "teams", "meet", "slack"} }

// drop is a connection drop that ended: every covered tile notes it.
func (b *sb) drop(start time.Time, dur time.Duration, who tiles.Who, ev string, verdict model.VerdictKind) tiles.Drop {
	d := tiles.Drop{Start: start, End: start.Add(dur), Dur: dur}
	b.add(tiles.Episode{Start: d.Start, End: d.End, Sev: tiles.Down, Kind: tiles.EpDrop, Tiles: b.covered(), Connection: true,
		Verdict: verdict, Cause: tiles.Cause{Who: who, Evidence: ev}, DropDur: dur})
	for i := range b.s.Tiles {
		t := &b.s.Tiles[i]
		if t.State == tiles.Good && b.now.Sub(d.End) < 30*time.Minute {
			c := d
			t.LastDrop = &c
		}
	}
	return d
}

// covered are the tiles that are not off.
func (b *sb) covered() []string {
	var out []string
	for _, t := range b.s.Tiles {
		if t.State != tiles.Off {
			out = append(out, t.ID)
		}
	}
	return out
}

// history are the problems of the mockup: Slack choppy this morning,
// Microsoft 365 slow last night, Google down yesterday afternoon, the
// provider lossy yesterday at 2:10 pm, the router down on Wednesday.
func (b *sb) history(only ...string) {
	has := func(ids ...string) []string {
		var out []string
		for _, id := range ids {
			if b.tile(id).State != tiles.Off {
				out = append(out, id)
			}
		}
		return out
	}
	app := func(v, ev string) tiles.Cause { return tiles.Cause{Who: tiles.WhoApp, Vendor: v, Evidence: ev} }
	if ts := has("slack"); len(ts) > 0 {
		b.add(tiles.Episode{Start: b.ago(49 * time.Minute), End: b.ago(45 * time.Minute), Sev: tiles.Slow, Kind: tiles.EpChoppy,
			Tiles: ts, Cause: app("slack", tiles.EvAppOnly)})
	}
	if ts := has("m365"); len(ts) > 0 {
		b.add(tiles.Episode{Start: b.ago(14*time.Hour + 16*time.Minute), End: b.ago(14*time.Hour + 8*time.Minute), Sev: tiles.Slow,
			Kind: tiles.EpSlow, Tiles: ts, Cause: app("microsoft", tiles.EvAppOnly)})
	}
	if ts := has("meet", "gws", "youtube"); len(ts) > 0 {
		b.add(tiles.Episode{Start: b.ago(18*time.Hour + 3*time.Minute), End: b.ago(18 * time.Hour), Sev: tiles.Down,
			Kind: tiles.EpOutage, Tiles: ts, Cause: app("google", tiles.EvVendorOnly)})
	}
	b.add(tiles.Episode{Start: b.ago(20*time.Hour + 20*time.Minute), End: b.ago(20*time.Hour + 5*time.Minute), Sev: tiles.Slow,
		Kind: tiles.EpSlow, Tiles: has(callTiles()...), Connection: true, Verdict: model.VerdictISP,
		Cause: tiles.Cause{Who: tiles.WhoProvider, Evidence: tiles.EvProviderLossy}})
	b.add(tiles.Episode{Start: b.ago(62*time.Hour + 28*time.Minute), End: b.ago(62*time.Hour + 26*time.Minute), Sev: tiles.Down,
		Kind: tiles.EpDrop, Tiles: b.covered(), Connection: true, Verdict: model.VerdictLAN,
		Cause: tiles.Cause{Who: tiles.WhoRouter, Evidence: tiles.EvRouterDown}, DropDur: 2 * time.Minute})
}

// conn sets the card to a connection row since a time.
func (b *sb) conn(kind string, key string, tone string, icon tiles.State, since time.Time) {
	b.s.Conn = tiles.ConnState{Kind: kind, Key: key + ":" + strconv.FormatInt(since.UnixMilli(), 10), Tone: tone, Icon: icon, Since: since}
}

// Build makes a scenario as of now.
func Build(name string, now time.Time) (*Scenario, error) {
	p, err := profile.Default()
	if err != nil {
		return nil, err
	}
	v := &Scenario{Name: name, Profile: p, Charts: map[string]*tiles.Chart{},
		Path:   netinfo.Path{Gateway: netip.MustParseAddr("192.168.1.1"), Iface: "eth0", Conn: model.ConnWired, Edge: netip.MustParseAddr("198.51.100.1"), EdgeHop: 2, Updated: now},
		Viewer: Viewer{Listen: "127.0.0.1:3000", RemoteAddr: "127.0.0.1:50000", Host: "127.0.0.1:3000"},
		Speed:  &Speed{Scheduled: true}}
	s := &tiles.Snapshot{At: now, LastMeasured: now.Add(-4 * time.Second), FirstData: now.Add(-220 * 24 * time.Hour),
		Conn: tiles.ConnState{Kind: tiles.ConnGood, Key: tiles.ConnGood, Tone: tiles.ToneOK, Icon: tiles.Good}}
	v.Snap = s
	b := &sb{now: now, v: v, s: s, off: [][2]time.Time{{now.Add(-7*time.Hour - 25*time.Minute), now.Add(-6*time.Hour - 40*time.Minute)}}}
	first := now.Truncate(bar).Add(-(nBars - 1) * bar)
	for i, d := range profile.Tiles {
		t := &s.Tiles[i]
		t.ID, t.State = d.ID, tiles.Good
		for j := range t.Bars {
			t.Bars[j] = tiles.Bar{From: first.Add(time.Duration(j) * bar), State: tiles.Good}
		}
		u := usualMs[i]
		t.UsualMs = &u
		switch d.Kind {
		case profile.TileCall:
			src := tiles.CallShared
			if len(d.UDP) > 0 {
				src = tiles.CallOwn
			}
			t.Call = &tiles.CallMetrics{Source: src, Bursts: 36, DelayMs: u + 3, JitterMs: 2, HasJitter: true}
		default:
			sl := max(2*u, u+30)
			t.SlowAboveMs = &sl
		}
	}
	for i := range s.Layers {
		s.Layers[i].State = tiles.Good
	}
	sp := func(daysAgo int, h, m int, down, up float64) model.SpeedTest {
		at := now.Add(-time.Duration(daysAgo)*24*time.Hour - time.Duration(h)*time.Hour - time.Duration(m)*time.Minute)
		d, u := down*1e6, up*1e6
		return model.SpeedTest{Start: at, End: at.Add(40 * time.Second), Trigger: "schedule", Outcome: model.SpeedOK, DownBps: &d, UpBps: &u}
	}
	// 925 / 938 usual; the latest at 8:00 am
	v.Speed.List = []model.SpeedTest{sp(6, 0, 0, 920, 935), sp(5, 0, 0, 925, 938), sp(4, 0, 0, 930, 940), sp(3, 0, 0, 925, 938),
		sp(1, 0, 0, 918, 936), sp(0, 2, 30, 930, 940)}
	hist := true

	switch name {
	case "good", "drop-20s":
		// drop-20s: a 20 s drop makes no problem, note or red bar.
	case "blip":
		b.drop(b.ago(16*time.Minute), 40*time.Second, tiles.WhoProvider, tiles.EvProviderDrop, model.VerdictISP)
		b.again(tiles.AgainDropped)
		b.layerDrop(1, 2)
	case "isp":
		since := b.ago(18 * time.Minute)
		b.conn(string(model.VerdictISP), string(model.VerdictISP), tiles.ToneBad, tiles.Down, since)
		s.Layers[1].State, s.Layers[2].State = tiles.Slow, tiles.Slow
		b.set(tiles.Slow, since, "zoom", "teams", "meet", "slack", "m365", "gws", "dropbox", "onedrive", "youtube")
		b.set(tiles.Down, since, "github", "netflix")
		b.callsBad(150, 41, 7)
		b.add(tiles.Episode{Start: since, Sev: tiles.Slow, Kind: tiles.EpSlow, Tiles: allTiles(), Connection: true, Verdict: model.VerdictISP,
			Cause: tiles.Cause{Who: tiles.WhoProvider, Evidence: tiles.EvProviderLossy}})
		v.Speed.List = append(v.Speed.List, sp(0, 0, 10, 140, 95))
	case "zoom", "zoom-working-again":
		since := b.ago(25 * time.Minute)
		b.set(tiles.Down, since, "zoom")
		b.tile("zoom").Cause = tiles.Cause{Who: tiles.WhoApp, Vendor: "zoom", Evidence: tiles.EvAppOnly}
		b.add(tiles.Episode{Start: since, Sev: tiles.Down, Kind: tiles.EpOutage, Tiles: []string{"zoom"}, Cause: b.tile("zoom").Cause})
		b.s.Conn = tiles.ConnState{Kind: tiles.ConnApp, Key: "app:zoom:" + ms(since), Tone: tiles.ToneOK, Icon: tiles.Good, Since: since, Tiles: []string{"zoom"}}
		if name == "zoom-working-again" {
			b.drop(b.ago(16*time.Minute), 40*time.Second, tiles.WhoProvider, tiles.EvProviderDrop, model.VerdictISP)
			b.layerDrop(1, 2)
			cs := b.s.Conn
			b.again(tiles.AgainDropped)
			cs.Again, cs.Key = b.s.Conn.Again, cs.Key+":"+ms(b.s.Conn.Again.End)
			b.s.Conn = cs
		}
	case "learning":
		b.learning(40 * time.Minute)
		hist = false
		v.Speed.List = nil
	case "stale":
		s.LastMeasured = b.ago(9 * time.Minute)
		s.Conn = tiles.ConnState{Kind: tiles.ConnStale, Key: tiles.ConnStale, Tone: tiles.ToneStale, Icon: tiles.None}
		for i := range s.Tiles {
			for j := nBars - 3; j < nBars; j++ {
				s.Tiles[i].Bars[j].State = tiles.None
			}
		}
	case "checking":
		v.NotReady, hist = true, false
		*s = tiles.Snapshot{At: now, Conn: tiles.ConnState{Kind: tiles.ConnChecking, Key: tiles.ConnChecking, Tone: tiles.ToneChecking, Icon: tiles.None}}
		for i, d := range profile.Tiles {
			t := &s.Tiles[i]
			t.ID, t.State = d.ID, tiles.None
			for j := range t.Bars {
				t.Bars[j] = tiles.Bar{From: first.Add(time.Duration(j) * bar), State: tiles.None}
			}
		}
		for i := range s.Layers {
			s.Layers[i].State = tiles.None
		}
	case "router-down", "wifi-same-machine", "wifi-remote-viewer", "container-unknown":
		since := b.ago(6 * time.Minute)
		b.conn(string(model.VerdictLAN), string(model.VerdictLAN), tiles.ToneBad, tiles.Down, since)
		ev, sev := tiles.EvRouterDown, tiles.Down
		kind := tiles.EpDrop // the router lost everything: a connection drop absorbs the incident
		if name == "router-down" {
			for i := range s.Layers {
				s.Layers[i].State, s.Layers[i].Since = tiles.Down, since
			}
			b.set(tiles.Down, since, allTiles()...)
			b.noCalls()
		} else {
			// Wi-Fi trouble: the router answers, but loses data.
			ev, sev, kind = tiles.EvRouterLossy, tiles.Slow, tiles.EpSlow
			s.Layers[0].State, s.Layers[1].State, s.Layers[2].State = tiles.Slow, tiles.Slow, tiles.Slow
			b.set(tiles.Slow, since, allTiles()...)
			b.callsBad(60, 45, 9)
			v.Path.Iface, v.Path.Conn = "wlan0", model.ConnWiFi
			switch name {
			case "wifi-remote-viewer":
				v.Viewer.Listen, v.Viewer.RemoteAddr, v.Viewer.Host = "0.0.0.0:3000", "192.168.1.20:50000", "192.168.1.10:3000"
			case "container-unknown":
				v.Container = true
			}
			v.Viewer.Own = []netip.Addr{netip.MustParseAddr("192.168.1.10")}
		}
		b.add(tiles.Episode{Start: since, Sev: sev, Kind: kind, Tiles: allTiles(), Connection: true, Verdict: model.VerdictLAN,
			Cause: tiles.Cause{Who: tiles.WhoRouter, Evidence: ev}})
	case "upstream":
		since := b.ago(10 * time.Minute)
		b.conn(string(model.VerdictUpstream), string(model.VerdictUpstream), tiles.ToneBad, tiles.Down, since)
		s.Layers[2].State = tiles.Slow
		b.set(tiles.Slow, since, "m365", "gws", "dropbox", "onedrive", "youtube")
		b.set(tiles.Down, since, "github", "netflix")
		b.add(tiles.Episode{Start: since, Sev: tiles.Down, Kind: tiles.EpOutage, Tiles: []string{"m365", "gws", "github", "dropbox", "onedrive", "youtube", "netflix"},
			Connection: true, Verdict: model.VerdictUpstream, Cause: tiles.Cause{Who: tiles.WhoInternet, Evidence: tiles.EvBeyond}})
	case "no-network":
		since := b.ago(3 * time.Minute)
		b.conn(string(model.VerdictNoNetwork), string(model.VerdictNoNetwork), tiles.ToneBad, tiles.Down, since)
		for i := range s.Layers {
			s.Layers[i].State, s.Layers[i].Since = tiles.Down, since
		}
		b.set(tiles.Down, since, allTiles()...)
		b.noCalls()
		b.add(tiles.Episode{Start: since, Sev: tiles.Down, Kind: tiles.EpDrop, Tiles: allTiles(), Connection: true, Verdict: model.VerdictNoNetwork,
			Cause: tiles.Cause{Who: tiles.WhoOffline, Evidence: tiles.EvNoNetwork}})
	case "drop-now":
		// 50 s past the router: a drop in progress (a problem from 30 s
		// on). The call tiles' 5 s bursts have been failing long enough
		// (3 in a row + holdFor); the web tiles' 15 s checks not yet.
		since := b.ago(50 * time.Second)
		s.Conn = tiles.ConnState{Kind: tiles.ConnDrop, Key: "drop:" + ms(since), Tone: tiles.ToneBad, Icon: tiles.Down, Since: since}
		s.Layers[1].State, s.Layers[1].Since = tiles.Down, since
		s.Layers[2].State, s.Layers[2].Since = tiles.Down, since
		b.set(tiles.Down, since.Add(15*time.Second), callTiles()...)
		b.noCalls()
		b.add(tiles.Episode{Start: since, Sev: tiles.Down, Kind: tiles.EpDrop, Tiles: callTiles(), Connection: true,
			Cause: tiles.Cause{Who: tiles.WhoProvider, Evidence: tiles.EvProviderDrop}})
	case "dns":
		since := b.ago(4 * time.Minute)
		b.conn(string(model.VerdictDNS), string(model.VerdictDNS), tiles.ToneWarn, tiles.Slow, since)
		// Every check starts with a name lookup: calls can't connect either.
		apps := allTiles()
		b.set(tiles.Down, since, apps...)
		b.noCalls()
		b.add(tiles.Episode{Start: since, Sev: tiles.Down, Kind: tiles.EpOutage, Tiles: apps, Connection: true, Verdict: model.VerdictDNS,
			Cause: tiles.Cause{Who: tiles.WhoNames, Evidence: tiles.EvDNS}})
	case "udp-blocked", "udp-quiet":
		since := b.ago(90 * time.Minute)
		if name == "udp-quiet" {
			since = b.ago(26 * time.Hour)
		}
		b.conn(string(model.VerdictUDPBlocked), string(model.VerdictUDPBlocked), tiles.ToneWarn, tiles.Slow, since)
		if name == "udp-quiet" {
			b.s.Conn.Tone, b.s.Conn.Quiet = tiles.ToneInfo, true
		}
		b.set(tiles.Down, since, callTiles()...)
		for _, id := range callTiles() {
			*b.tile(id).Call = tiles.CallMetrics{Source: b.tile(id).Call.Source}
		}
		b.add(tiles.Episode{Start: since, Sev: tiles.Down, Kind: tiles.EpBlocked, Tiles: callTiles(), Connection: true,
			Verdict: model.VerdictUDPBlocked, Cause: tiles.Cause{Who: tiles.WhoUnknown}})
	case "udp-degraded":
		since := b.ago(9 * time.Minute)
		b.conn(string(model.VerdictUDPDegraded), string(model.VerdictUDPDegraded), tiles.ToneWarn, tiles.Slow, since)
		b.set(tiles.Slow, since, callTiles()...)
		b.callsBad(70, 12, 6)
		c := tiles.Cause{Who: tiles.WhoCallsPath, Evidence: tiles.EvCallsPath}
		for _, id := range callTiles() {
			b.tile(id).Cause = c
		}
		b.add(tiles.Episode{Start: since, Sev: tiles.Slow, Kind: tiles.EpChoppy, Tiles: callTiles(), Cause: c})
	case "broad-slow":
		since := b.ago(15 * time.Minute)
		b.conn(tiles.ConnBroad, tiles.ConnBroad, tiles.ToneWarn, tiles.Slow, since)
		slow := []string{"zoom", "teams", "meet", "m365", "gws", "github", "dropbox", "youtube"}
		b.s.Conn.Tiles = slow
		b.set(tiles.Slow, since, slow...)
		b.callsBad(90, 20, 0)
		for _, id := range slow {
			b.tile(id).Cause = tiles.Cause{Who: tiles.WhoUnknown}
		}
		b.add(tiles.Episode{Start: since, Sev: tiles.Slow, Kind: tiles.EpSlow, Tiles: slow, Connection: true, Cause: tiles.Cause{Who: tiles.WhoUnknown}})
	case "google-down":
		since := b.ago(10 * time.Minute)
		ids := []string{"meet", "gws", "youtube"}
		c := tiles.Cause{Who: tiles.WhoApp, Vendor: "google", Evidence: tiles.EvVendorOnly}
		b.set(tiles.Down, since, ids...)
		for _, id := range ids {
			b.tile(id).Cause = c
		}
		*b.tile("meet").Call = tiles.CallMetrics{Source: tiles.CallOwn}
		b.add(tiles.Episode{Start: since, Sev: tiles.Down, Kind: tiles.EpOutage, Tiles: ids, Cause: c})
		b.s.Conn = tiles.ConnState{Kind: tiles.ConnApp, Key: "app:meet,gws,youtube:" + ms(since), Tone: tiles.ToneOK, Icon: tiles.Good, Since: since, Tiles: ids}
	case "outage-2m-recovered":
		b.drop(b.ago(10*time.Minute), 2*time.Minute, tiles.WhoProvider, tiles.EvProviderDrop, model.VerdictISP)
		b.again(tiles.AgainDown)
		b.layerDrop(1, 2)
	case "trouble-recovered":
		from, to := b.ago(28*time.Minute), b.ago(12*time.Minute)
		b.paint(tiles.Slow, from, to, callTiles()...) // the calls were choppy then
		b.add(tiles.Episode{Start: from, End: to, Sev: tiles.Slow, Kind: tiles.EpSlow, Tiles: callTiles(), Connection: true, Verdict: model.VerdictISP,
			Cause: tiles.Cause{Who: tiles.WhoProvider, Evidence: tiles.EvProviderLossy}})
		b.again(tiles.AgainTrouble)
	case "provider-loss-6":
		// 6% loss past the router for 6 minutes: the calls are choppy, and
		// the verdict's udp_degraded (5% on 2 providers for 3 minutes) has
		// the card; the problem's cause is the provider (layer evidence).
		since := b.ago(6 * time.Minute)
		b.conn(string(model.VerdictUDPDegraded), string(model.VerdictUDPDegraded), tiles.ToneWarn, tiles.Slow, since)
		b.set(tiles.Slow, since, callTiles()...)
		b.callsBad(30, 8, 6)
		b.add(tiles.Episode{Start: since, Sev: tiles.Slow, Kind: tiles.EpChoppy, Tiles: callTiles(),
			Cause: tiles.Cause{Who: tiles.WhoProvider, Evidence: tiles.EvProviderLossy}})
	case "provider-loss-3":
		// 3% loss past the router: choppy calls (2% or more), below the
		// verdict's 5%: no path verdict, so the card is row 9, amber, and
		// names the provider; the strip marks it.
		since := b.ago(4 * time.Minute)
		b.set(tiles.Slow, since, callTiles()...)
		b.callsBad(30, 8, 3)
		b.add(tiles.Episode{Start: since, Sev: tiles.Slow, Kind: tiles.EpChoppy, Tiles: callTiles(),
			Cause: tiles.Cause{Who: tiles.WhoProvider, Evidence: tiles.EvProviderLossy}})
		b.s.Conn = tiles.ConnState{Kind: tiles.ConnApp, Key: "app:zoom,teams,meet,slack:" + ms(since), Tone: tiles.ToneOK, Icon: tiles.Good, Since: since, Tiles: callTiles()}
	case "jitter":
		// 41 ms of jitter for 3 minutes: the call tiles just turned Choppy
		// (3 bad minutes), before the verdict's udp_degraded needs 3 minutes
		// and a window.
		since := b.ago(3 * time.Minute)
		b.set(tiles.Slow, since, callTiles()...)
		b.callsBad(40, 41, 0)
		c := tiles.Cause{Who: tiles.WhoCallsPath, Evidence: tiles.EvCallsPath}
		for _, id := range callTiles() {
			b.tile(id).Cause = c
		}
		b.add(tiles.Episode{Start: since, Sev: tiles.Slow, Kind: tiles.EpChoppy, Tiles: callTiles(), Cause: c})
		b.s.Conn = tiles.ConnState{Kind: tiles.ConnApp, Key: "app:zoom,teams,meet,slack:" + ms(since), Tone: tiles.ToneOK, Icon: tiles.Good, Since: since, Tiles: callTiles()}
	case "edge-unknown":
		// The provider's edge isn't found yet, and OneDrive has no data yet.
		s.Layers[1].State = tiles.None
		b.set(tiles.None, b.ago(2*time.Minute), "onedrive")
	case "no-data-yet":
		// Replayed, but no tile's first minute has closed (a restart):
		// still checking, never "all 11 apps look healthy".
		for i := range s.Tiles {
			s.Tiles[i].State, s.Tiles[i].Since = tiles.None, time.Time{}
		}
	case "calls-profile", "no-streaming", "custom-config":
		var off []string
		switch name {
		case "calls-profile":
			r, err := profile.Builtin()
			if err == nil {
				v.Profile, err = r.Resolve([]string{"calls"}, profile.ResolveOptions{})
			}
			if err != nil {
				return nil, err
			}
			off = []string{"m365", "gws", "github", "dropbox", "onedrive", "youtube", "netflix"}
		case "no-streaming":
			off = []string{"youtube", "netflix"}
		default:
			off = []string{"github", "dropbox", "youtube", "netflix"}
		}
		if name != "calls-profile" {
			v.Profile = without(p, off)
		}
		v.CustomProfile = name == "custom-config"
		for _, id := range off {
			*b.tile(id) = tiles.TileSnap{ID: id, State: tiles.Off}
		}
	case "week-with-problems":
		b.week()
	case "not-running":
		// fyisp was stopped from 6:00 to 9:20 this morning.
		from, to := b.ago(4*time.Hour+30*time.Minute), b.ago(70*time.Minute)
		b.off = append(b.off, [2]time.Time{from, to})
		for i := range s.Tiles {
			for j := range s.Tiles[i].Bars {
				x := &s.Tiles[i].Bars[j]
				if x.From.Add(3 * time.Minute).Before(to) {
					x.State = tiles.None
				}
			}
		}
	case "speed-running":
		v.Speed.Run = &speedtest.Entry{ID: 12, Kind: speedtest.KindSingle, State: "running", Started: b.ago(10 * time.Second), EstFinish: now.Add(20 * time.Second)}
	case "speed-no-usual":
		v.Speed.List = v.Speed.List[len(v.Speed.List)-3:]
	case "speed-done":
		x := sp(0, 0, 1, 925, 938) // Test now, a minute ago
		x.Trigger = "manual"
		v.Speed.List = append(v.Speed.List, x)
	case "speed-upload-slow", "speed-download-slow":
		// the directions apart: one faster or usual, the other slower
		x := sp(0, 0, 1, 1200, 400)
		if name == "speed-download-slow" {
			x = sp(0, 0, 1, 600, 950)
		}
		v.Speed.List = append(v.Speed.List, x)
	case "speedtest-labelled":
		from, to := b.ago(90*time.Minute), b.ago(87*time.Minute)
		c := tiles.Cause{Who: tiles.WhoSpeedtest, Evidence: tiles.EvSpeedtest}
		b.add(tiles.Episode{Start: from, End: to, Sev: tiles.Slow, Kind: tiles.EpSlow, Tiles: []string{"m365", "gws"}, Cause: c})
		for _, id := range allTiles() {
			for j := range b.tile(id).Bars {
				x := &b.tile(id).Bars[j]
				x.Speedtest = overlaps(x.From, x.From.Add(bar), from.Add(-30*time.Second), to, now)
			}
		}
		v.Speed.List = append(v.Speed.List, sp(0, 1, 30, 910, 930))
	case "dst":
		// Clocks went back at 2 am: a provider problem from 1:50 am EDT
		// to 1:10 am EST, 20 minutes later.
		b.add(tiles.Episode{Start: time.Date(2026, 11, 1, 5, 50, 0, 0, time.UTC), End: time.Date(2026, 11, 1, 6, 10, 0, 0, time.UTC),
			Sev: tiles.Slow, Kind: tiles.EpSlow, Tiles: callTiles(), Connection: true, Verdict: model.VerdictISP,
			Cause: tiles.Cause{Who: tiles.WhoProvider, Evidence: tiles.EvProviderLossy}})
	default:
		return nil, fmt.Errorf("unknown scenario %q (want one of %v)", name, Names)
	}
	if hist {
		b.history()
	}
	b.finish()
	return v, nil
}

func ms(t time.Time) string { return strconv.FormatInt(t.UnixMilli(), 10) }

// again makes the card "Working again" after the newest ended connection
// problem.
func (b *sb) again(variant string) {
	var e *tiles.Episode
	for i, x := range b.v.Eps {
		if x.Connection && !x.Ongoing() && (e == nil || x.End.After(e.End)) {
			e = &b.v.Eps[i]
		}
	}
	r := &tiles.Recovered{Start: e.Start, End: e.End, Dur: e.End.Sub(e.Start), Variant: variant, Cause: e.Cause, Episode: e.ID}
	if e.DropDur > 0 {
		r.Dur = e.DropDur
	}
	b.s.Conn = tiles.ConnState{Kind: tiles.ConnWorkingAgain, Key: tiles.ConnWorkingAgain + ":" + ms(e.End), Tone: tiles.ToneWarn,
		Icon: tiles.Slow, Since: e.End, Again: r}
}

// layerDrop notes the newest connection drop on the given path layers.
func (b *sb) layerDrop(ls ...int) {
	for _, e := range b.v.Eps {
		if e.Connection && e.Kind == tiles.EpDrop && !e.Ongoing() {
			for _, l := range ls {
				b.s.Layers[l].LastDrop = &tiles.Drop{Start: e.Start, End: e.End, Dur: e.DropDur}
			}
			return
		}
	}
}

// callsBad sets the call tiles' figures (those not Good).
func (b *sb) callsBad(delayMs, jitterMs, lossPct float64) {
	for _, id := range callTiles() {
		t := b.tile(id)
		if t.State.Bad() && t.Call != nil {
			t.Call.DelayMs, t.Call.JitterMs, t.Call.LossPct = delayMs, jitterMs, lossPct
		}
	}
}

// learning: fyisp started watching d ago.
func (b *sb) learning(d time.Duration) {
	start := b.ago(d)
	b.s.FirstData, b.s.Learning = start, true
	for i := range b.s.Tiles {
		t := &b.s.Tiles[i]
		t.UsualMs, t.SlowAboveMs = nil, nil
		for j := range t.Bars {
			if t.Bars[j].From.Add(2 * time.Minute).Before(start) {
				t.Bars[j].State = tiles.None
			}
		}
	}
}

// week is a busier week: every kind of cause.
func (b *sb) week() {
	day := 24 * time.Hour
	add := func(start, dur time.Duration, sev tiles.State, kind string, ts []string, conn bool, vk model.VerdictKind, c tiles.Cause) {
		e := tiles.Episode{Start: b.ago(start), End: b.ago(start - dur), Sev: sev, Kind: kind, Tiles: ts, Connection: conn, Verdict: vk, Cause: c}
		if kind == tiles.EpDrop {
			e.DropDur = dur
		}
		b.add(e)
	}
	add(2*day+10*time.Hour+40*time.Minute, 30*time.Minute, tiles.Slow, tiles.EpSlow, callTiles(), true, model.VerdictISP,
		tiles.Cause{Who: tiles.WhoProvider, Evidence: tiles.EvProviderLossy}) // Tue 11:50 pm – Wed 12:20 am
	add(3*day+2*time.Hour, 50*time.Second, tiles.Down, tiles.EpDrop, allTiles(), true, model.VerdictISP,
		tiles.Cause{Who: tiles.WhoProvider, Evidence: tiles.EvProviderDrop})
	add(4*day+5*time.Hour, 6*time.Minute, tiles.Down, tiles.EpOutage, []string{"m365", "gws", "github", "dropbox", "onedrive", "youtube", "netflix"}, true,
		model.VerdictDNS, tiles.Cause{Who: tiles.WhoNames, Evidence: tiles.EvDNS})
	add(5*day+3*time.Hour, 40*time.Minute, tiles.Down, tiles.EpBlocked, callTiles(), true, model.VerdictUDPBlocked, tiles.Cause{Who: tiles.WhoUnknown})
	add(5*day+8*time.Hour, 3*time.Minute, tiles.Slow, tiles.EpSlow, []string{"github"}, false, "", tiles.Cause{Who: tiles.WhoSpeedtest, Evidence: tiles.EvSpeedtest})
	add(6*day+1*time.Hour, 12*time.Minute, tiles.Down, tiles.EpOutage, allTiles(), true, model.VerdictUpstream,
		tiles.Cause{Who: tiles.WhoInternet, Evidence: tiles.EvBeyond})
	add(6*day+4*time.Hour, 45*time.Second, tiles.Down, tiles.EpDrop, []string{"dropbox"}, false, "", tiles.Cause{Who: tiles.WhoApp, Vendor: "dropbox", Evidence: tiles.EvAppOnly})
	add(3*time.Hour, 5*time.Minute, tiles.Slow, tiles.EpChoppy, []string{"teams", "meet"}, false, "", tiles.Cause{Who: tiles.WhoCallsPath, Evidence: tiles.EvCallsPath})
}

// without is p minus the targets of the given tiles.
func without(p *model.Profile, ids []string) *model.Profile {
	q := *p
	q.Targets = nil
	for _, t := range p.Targets {
		if !slices.Contains(ids, profile.TileOf(t)) {
			q.Targets = append(q.Targets, t)
		}
	}
	return &q
}

// finish sorts the problems, counts them and draws the charts.
func (b *sb) finish() {
	s, v := b.s, b.v
	slices.SortStableFunc(v.Eps, func(x, y tiles.Episode) int { return y.Start.Compare(x.Start) })
	h := fnv.New64a()
	for _, e := range v.Eps {
		if !e.Ongoing() && e.End.Before(b.now.Add(-24*time.Hour)) {
			continue
		}
		s.Day.Total++
		switch e.Cause.Kind() {
		case tiles.KindProvider:
			s.Day.Provider++
		case tiles.KindHome:
			s.Day.Home++
		case tiles.KindApp:
			s.Day.App++
		}
	}
	for _, e := range v.Eps {
		fmt.Fprint(h, e.ID, e.End.UnixMilli(), e.Cause, e.Tiles)
	}
	s.Rev = h.Sum64()
	s.Eps = v.Eps
	for _, t := range s.Tiles {
		if t.State != tiles.Off {
			s.Monitored++
		}
	}
	for i, t := range s.Tiles {
		if t.State != tiles.Off {
			v.Charts[t.ID] = b.chart(i)
		}
	}
}

// chart draws a tile's last 24 hours: around its usual, higher in its
// slow problems, gaps in its outages and while fyisp was off.
func (b *sb) chart(i int) *tiles.Chart {
	t, d := b.s.Tiles[i], profile.Tiles[i]
	u := usualMs[i]
	c := &tiles.Chart{ID: t.ID, Step: bar, From: b.now.Truncate(bar).Add(-(nBucket - 1) * bar)}
	if t.UsualMs != nil {
		x := *t.UsualMs
		c.UsualMs = &x
	}
	seed := uint64(7)
	for _, r := range t.ID {
		seed += uint64(r)
	}
	x := seed * 97
	rnd := func() float64 { x = x * 16807 % 2147483647; return float64(x) / 2147483647 }
	var mine []tiles.Episode
	for _, e := range b.v.Eps {
		if (e.Connection || slices.Contains(e.Tiles, t.ID)) && (e.Ongoing() || !e.End.Before(c.From)) {
			mine = append(mine, e)
		}
	}
	if b.v.NotReady { // the replay isn't done: nothing to draw yet
		for k := range c.Ms {
			c.Ms[k] = math.NaN()
		}
		return c
	}
	for k := range c.Ms {
		from := c.From.Add(time.Duration(k) * bar)
		v := u * (0.92 + rnd()*0.16)
		switch {
		case !from.Before(b.now.Truncate(bar)):
			v = math.NaN() // the current bucket: no closed minute yet
		case from.Before(b.s.FirstData.Truncate(bar)), b.isOff(from):
			v = math.NaN()
		}
		for _, e := range mine {
			if !overlaps(from, from.Add(bar), e.Start, e.End, b.now) || (!slices.Contains(e.Tiles, t.ID) && !e.Connection) {
				continue
			}
			switch {
			case e.Kind == tiles.EpDrop || e.Sev == tiles.Down && slices.Contains(e.Tiles, t.ID):
				v = math.NaN()
			case e.Connection && d.Kind == profile.TileCall:
				v = u + 12 + rnd()*6 // a lossy provider: the delay rises a little
			case slices.Contains(e.Tiles, t.ID):
				v = u*2.6 + rnd()*6
			}
		}
		c.Ms[k] = v
	}
	for _, e := range mine {
		if !e.Connection && !slices.Contains(e.Tiles, t.ID) {
			continue
		}
		c.Bands = append(c.Bands, tiles.Band{From: e.Start, To: e.End, Kind: tiles.BandProblem, Episode: e.ID, Sev: e.Sev, Cause: e.Cause})
	}
	if b.s.FirstData.After(c.From) {
		c.Bands = append(c.Bands, tiles.Band{From: c.From, To: b.s.FirstData, Kind: tiles.BandOff})
	}
	for _, o := range b.off {
		if o[1].After(c.From) && o[0].After(b.s.FirstData) {
			c.Bands = append(c.Bands, tiles.Band{From: o[0], To: o[1], Kind: tiles.BandOff})
		}
	}
	slices.SortStableFunc(c.Bands, func(x, y tiles.Band) int { return x.From.Compare(y.From) })
	return c
}

func (b *sb) isOff(t time.Time) bool {
	for _, o := range b.off {
		if !t.Before(o[0].Truncate(bar)) && t.Before(o[1]) {
			return true
		}
	}
	return false
}

// Live is a scenario that goes on (cmd/fyisp-webdev -home): it is
// rebuilt every minute as of then, and between rebuilds its checks stay
// a few seconds old.
type Live struct {
	Name string
	Now  func() time.Time

	mu    sync.Mutex
	at    time.Time
	built *Scenario
}

// Current is the scenario as of now.
func (l *Live) Current() *Scenario {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.Now()
	if t := now.Truncate(time.Minute); l.built == nil || !t.Equal(l.at) {
		v, err := Build(l.Name, t)
		if err != nil {
			panic(err) // the name was checked at start
		}
		l.at, l.built = t, v
	}
	v := *l.built
	s := *v.Snap
	shift := now.Sub(l.at)
	s.At = now
	if !s.LastMeasured.IsZero() && s.Conn.Kind != tiles.ConnStale {
		s.LastMeasured = s.LastMeasured.Add(shift)
	}
	v.Snap = &s
	return &v
}

func (l *Live) Ready() bool                             { return l.Current().Ready() }
func (l *Live) Snapshot() *tiles.Snapshot               { return l.Current().Snap }
func (l *Live) Chart(id string) (*tiles.Chart, bool)    { return l.Current().Chart(id) }
func (l *Live) Problems(from time.Time) []tiles.Episode { return l.Current().Problems(from) }
func (l *Live) LateSamples() int                        { return 0 }
