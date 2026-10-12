package tiles

import (
	"strconv"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/profile"
)

// tilesNow is every tile Good, except those in set (Off for off tiles).
func tilesNow(set map[string]State) []tileNow {
	var out []tileNow
	for _, d := range profile.Tiles {
		tn := tileNow{id: d.ID, vendor: d.Vendors[0], state: Good}
		if s, ok := set[d.ID]; ok {
			tn.state = s
			if s.Bad() {
				tn.since = at(20 * time.Minute)
			}
		}
		out = append(out, tn)
	}
	return out
}

func TestConnState(t *testing.T) {
	m := time.Minute
	now := at(time.Hour)
	ms := func(d time.Duration) string { return strconv.FormatInt(at(d).UnixMilli(), 10) }
	ok := model.Verdict{Kind: model.VerdictOK, Since: t0}
	v := func(k model.VerdictKind, since time.Duration) model.Verdict {
		return model.Verdict{Kind: k, Since: at(since)}
	}
	ended := Drop{Start: at(40 * m), End: at(40*m + 40*time.Second), Dur: 40 * time.Second}
	dropEp := dropEpisode(ended, "")
	dropEp.ID = "c-" + ms(40*m)
	calls := map[string]State{"m365": Off, "gws": Off, "github": Off, "dropbox": Off, "onedrive": Off, "youtube": Off, "netflix": Off}
	with := func(base map[string]State, add map[string]State) map[string]State {
		out := map[string]State{}
		for k, s := range base {
			out[k] = s
		}
		for k, s := range add {
			out[k] = s
		}
		return out
	}
	broad := map[string]State{"zoom": Slow, "teams": Slow, "meet": Slow, "slack": Slow, "m365": Slow, "gws": Slow, "github": Slow}
	cases := []struct {
		name string
		in   connIn
		kind string
		key  string
		tone string
		icon State
	}{
		{"1 stale", connIn{stale: true, ready: true, verdict: v(model.VerdictISP, 0)}, ConnStale, "stale", ToneStale, None},
		{"2 not ready", connIn{verdict: ok}, ConnChecking, "checking", ToneChecking, None},
		{"2 warming up", connIn{ready: true, verdict: v(model.VerdictWarmingUp, 0)}, ConnChecking, "checking", ToneChecking, None},
		{"3 no_network", connIn{ready: true, verdict: v(model.VerdictNoNetwork, 5*m)}, "no_network", "no_network:" + ms(5*m), ToneBad, Down},
		{"3 lan", connIn{ready: true, verdict: v(model.VerdictLAN, 5*m)}, "lan", "lan:" + ms(5*m), ToneBad, Down},
		{"3 isp", connIn{ready: true, verdict: v(model.VerdictISP, 5*m)}, "isp", "isp:" + ms(5*m), ToneBad, Down},
		{"3 upstream", connIn{ready: true, verdict: v(model.VerdictUpstream, 5*m)}, "upstream", "upstream:" + ms(5*m), ToneBad, Down},
		{"3 beats a drop in progress", connIn{ready: true, verdict: v(model.VerdictISP, 5*m), dropNow: &Drop{Start: at(5 * m)}}, "isp", "isp:" + ms(5*m), ToneBad, Down},
		{"3 an incident inside an ended drop: Working again",
			connIn{ready: true, now: now, verdict: v(model.VerdictISP, 40*m+15*time.Second), drops: []Drop{ended}, eps: []Episode{dropEp}},
			ConnWorkingAgain, "working_again:" + ms(40*m+40*time.Second), ToneWarn, Slow},
		{"3 an incident after a much older drop", connIn{ready: true, verdict: v(model.VerdictISP, 50*m), drops: []Drop{ended}}, "isp", "isp:" + ms(50*m), ToneBad, Down},
		{"4 drop in progress", connIn{ready: true, verdict: ok, dropNow: &Drop{Start: at(59 * m)}}, ConnDrop, "drop:" + ms(59*m), ToneBad, Down},
		{"4 beats dns", connIn{ready: true, verdict: v(model.VerdictDNS, 0), dropNow: &Drop{Start: at(59 * m)}}, ConnDrop, "drop:" + ms(59*m), ToneBad, Down},
		{"5 dns", connIn{ready: true, verdict: v(model.VerdictDNS, 5*m)}, "dns", "dns:" + ms(5*m), ToneWarn, Slow},
		{"6 udp_blocked", connIn{ready: true, verdict: v(model.VerdictUDPBlocked, 5*m)}, "udp_blocked", "udp_blocked:" + ms(5*m), ToneWarn, Slow},
		{"6 stun_blocked", connIn{ready: true, verdict: v(model.VerdictSTUNBlocked, 5*m)}, "stun_blocked", "udp_blocked:" + ms(5*m), ToneWarn, Slow},
		{"6 quiet", connIn{ready: true, verdict: model.Verdict{Kind: model.VerdictUDPBlocked, Since: at(5 * m), Quiet: true}}, "udp_blocked", "udp_blocked:" + ms(5*m), ToneInfo, Slow},
		{"7 udp_degraded", connIn{ready: true, verdict: v(model.VerdictUDPDegraded, 5*m)}, "udp_degraded", "udp_degraded:" + ms(5*m), ToneWarn, Slow},
		{"8 broad slowdown", connIn{ready: true, verdict: ok, layersOK: true, tiles: tilesNow(broad)}, ConnBroad, "broad:" + ms(20*m), ToneWarn, Slow},
		{"8 not while a layer is unhealthy", connIn{ready: true, verdict: ok, tiles: tilesNow(broad)},
			ConnApp, "app:zoom,teams,meet,slack,m365,gws,github:" + ms(20*m), ToneOK, Good},
		{"8 not with 2 vendors", connIn{ready: true, verdict: ok, layersOK: true, tiles: tilesNow(map[string]State{
			"zoom": Off, "slack": Off, "github": Off, "dropbox": Off, "youtube": Off, "netflix": Off,
			"teams": Slow, "meet": Slow, "m365": Slow, "gws": Slow})},
			ConnApp, "app:teams,meet,m365,gws:" + ms(20*m), ToneOK, Good},
		{"8 off tiles don't count: 3 of 4 covered", connIn{ready: true, verdict: ok, layersOK: true, tiles: tilesNow(with(calls, map[string]State{"zoom": Slow, "teams": Slow, "meet": Slow}))},
			ConnBroad, "broad:" + ms(20*m), ToneWarn, Slow},
		{"8 off tiles don't count: 2 of 4 covered", connIn{ready: true, verdict: ok, layersOK: true, tiles: tilesNow(with(calls, map[string]State{"zoom": Slow, "teams": Slow}))},
			ConnApp, "app:zoom,teams:" + ms(20*m), ToneOK, Good},
		{"9 app only", connIn{ready: true, verdict: ok, layersOK: true, tiles: tilesNow(map[string]State{"zoom": Down})}, ConnApp, "app:zoom:" + ms(20*m), ToneOK, Good},
		{"9 no data is not a problem", connIn{ready: true, verdict: ok, tiles: tilesNow(map[string]State{"zoom": None})}, ConnGood, "good", ToneOK, Good},
		{"10 working again", connIn{ready: true, now: now, verdict: ok, eps: []Episode{dropEp}}, ConnWorkingAgain, "working_again:" + ms(40*m+40*time.Second), ToneWarn, Slow},
		{"11 good", connIn{ready: true, now: now, verdict: ok, tiles: tilesNow(nil)}, ConnGood, "good", ToneOK, Good},
	}
	for _, c := range cases {
		if c.in.now.IsZero() {
			c.in.now = now
		}
		got := connState(c.in)
		if got.Kind != c.kind || got.Key != c.key || got.Tone != c.tone || got.Icon != c.icon {
			t.Errorf("%s: got %s %q %s %s, want %s %q %s %s", c.name, got.Kind, got.Key, got.Tone, got.Icon, c.kind, c.key, c.tone, c.icon)
		}
	}

	// app-only problems win the headline; Working again goes with them
	got := connState(connIn{ready: true, now: now, verdict: ok, tiles: tilesNow(map[string]State{"zoom": Down}), eps: []Episode{dropEp}})
	if got.Kind != ConnApp || got.Again == nil || got.Again.Variant != AgainDropped || got.Again.Episode != dropEp.ID {
		t.Errorf("app + working again: got %+v %+v", got, got.Again)
	}
	if got.Tiles == nil || got.Tiles[0] != "zoom" || !got.Since.Equal(at(20*m)) {
		t.Errorf("app tiles: got %+v", got)
	}
}

func TestWorkingAgain(t *testing.T) {
	m := time.Minute
	end := at(50 * m)
	ep := func(kind string, sev State, verdict model.VerdictKind, dur, drop time.Duration) Episode {
		return Episode{Start: end.Add(-dur), End: end, Sev: sev, Kind: kind, Connection: true, Verdict: verdict, DropDur: drop}
	}
	cases := []struct {
		name    string
		ep      Episode
		variant string
		dur     time.Duration
	}{
		{"drop 40 s", ep(EpDrop, Down, "", 40*time.Second, 40*time.Second), AgainDropped, 40 * time.Second},
		{"drop 90 s", ep(EpDrop, Down, "", 90*time.Second, 90*time.Second), AgainDown, 90 * time.Second},
		{"drop with the incident it absorbed", ep(EpDrop, Down, model.VerdictISP, 2*m, 40*time.Second), AgainDropped, 40 * time.Second},
		{"isp outage 2 min", ep(EpOutage, Down, model.VerdictISP, 2*m, 0), AgainDown, 2 * m},
		{"lan outage", ep(EpOutage, Down, model.VerdictLAN, 3*m, 0), AgainDown, 3 * m},
		{"no_network", ep(EpOutage, Down, model.VerdictNoNetwork, 5*m, 0), AgainDown, 5 * m},
		{"upstream, lossy", ep(EpSlow, Slow, model.VerdictUpstream, 20*m, 0), AgainTrouble, 20 * m},
		{"isp, lossy", ep(EpSlow, Slow, model.VerdictISP, 2*time.Hour, 0), AgainTrouble, 2 * time.Hour},
		{"dns: never", ep(EpOutage, Down, model.VerdictDNS, 5*m, 0), "", 0},
		{"udp_blocked: never", ep(EpBlocked, Down, model.VerdictUDPBlocked, 5*m, 0), "", 0},
		{"broad slowdown: never", ep(EpSlow, Slow, "", 5*m, 0), "", 0},
		{"a tile episode: never", Episode{Start: end.Add(-m), End: end, Sev: Down, Kind: EpDrop, Tiles: []string{"zoom"}, DropDur: m}, "", 0},
	}
	for _, c := range cases {
		r := workingAgain([]Episode{c.ep}, end.Add(time.Minute))
		switch {
		case c.variant == "" && r != nil:
			t.Errorf("%s: got %+v", c.name, r)
		case c.variant != "" && (r == nil || r.Variant != c.variant || r.Dur != c.dur || !r.End.Equal(end)):
			t.Errorf("%s: got %+v, want %s %v", c.name, r, c.variant, c.dur)
		}
	}

	// 30 minutes, then gone
	e := ep(EpDrop, Down, "", 40*time.Second, 40*time.Second)
	if workingAgain([]Episode{e}, end.Add(recoveredFor-time.Second)) == nil {
		t.Error("29:59 after: gone")
	}
	if workingAgain([]Episode{e}, end.Add(recoveredFor)) != nil {
		t.Error("30:00 after: still there")
	}
	// ongoing: not yet
	e.End = time.Time{}
	if workingAgain([]Episode{e}, end) != nil {
		t.Error("ongoing")
	}
	// the newest that ended
	a := ep(EpOutage, Down, model.VerdictISP, 2*m, 0)
	b := ep(EpDrop, Down, "", 40*time.Second, 40*time.Second)
	b.Start, b.End = end.Add(5*m), end.Add(5*m+40*time.Second)
	if r := workingAgain([]Episode{b, a}, end.Add(10*m)); r == nil || r.Variant != AgainDropped {
		t.Errorf("newest: got %+v", r)
	}
}

func TestMonitored(t *testing.T) {
	if n := monitored(tilesNow(map[string]State{"github": Off, "dropbox": Off, "zoom": None})); n != 9 {
		t.Errorf("got %d, want 9", n)
	}
}
