package tiles

import (
	"fmt"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/profile"
)

func TestIncidentEpisode(t *testing.T) {
	cases := []struct {
		kind   model.VerdictKind
		peak   float64
		ok     bool
		sev    State
		epKind string
	}{
		{model.VerdictLAN, 1, true, Down, EpOutage},
		{model.VerdictLAN, 0.3, true, Slow, EpSlow},
		{model.VerdictISP, 0.95, true, Down, EpOutage},
		{model.VerdictUpstream, 0.5, true, Slow, EpSlow},
		{model.VerdictNoNetwork, 0, true, Down, EpOutage},
		{model.VerdictDNS, 0, true, Down, EpOutage},
		{model.VerdictUDPBlocked, 0, true, Down, EpBlocked},
		{model.VerdictSTUNBlocked, 0, true, Down, EpBlocked},
		{model.VerdictUDPDegraded, 0.1, false, "", ""},
		{model.VerdictService, 1, false, "", ""},
	}
	for _, c := range cases {
		e, ok := incidentEpisode(model.Incident{Kind: c.kind, Start: t0, PeakLoss: c.peak}, false)
		if ok != c.ok || ok && (e.Sev != c.sev || e.Kind != c.epKind || !e.Connection || e.Verdict != c.kind) {
			t.Errorf("%s %.2f: got %+v %v", c.kind, c.peak, e, ok)
		}
	}
	// a layer that dropped makes it a full loss whatever the peak loss
	if e, _ := incidentEpisode(model.Incident{Kind: model.VerdictLAN, Start: t0, PeakLoss: 0.3}, true); e.Sev != Down || e.Kind != EpOutage {
		t.Errorf("lan, gateway dropped: got %+v", e)
	}
	if e := runEpisode("zoom", profile.TileCall, wordRun{Start: t0, Sev: Slow}); e.Kind != EpChoppy {
		t.Errorf("call slow: got %s", e.Kind)
	}
	if e := runEpisode("m365", profile.TileApp, wordRun{Start: t0, Sev: Slow}); e.Kind != EpSlow {
		t.Errorf("app slow: got %s", e.Kind)
	}
	if e := runEpisode("m365", profile.TileApp, wordRun{Start: t0, Sev: Down}); e.Kind != EpOutage {
		t.Errorf("app down: got %s", e.Kind)
	}
}

// byVendor attributes a tile episode to its first tile's vendor, a
// connection episode to the provider.
func byVendor(e Episode) Cause {
	if e.Connection {
		return Cause{Who: WhoProvider, Evidence: EvProviderDrop}
	}
	return Cause{Who: WhoApp, Vendor: profile.Tiles[tileOrder(e.Tiles[0])].Vendors[0], Evidence: EvAppOnly}
}

func tileRun(id string, sev State, from, to time.Duration) Episode {
	kind := profile.Tiles[tileOrder(id)].Kind
	r := wordRun{Start: at(from), Sev: sev}
	if to > 0 {
		r.End = at(to)
	}
	return runEpisode(id, kind, r)
}

func ids(eps []Episode) []string {
	var out []string
	for _, e := range eps {
		out = append(out, fmt.Sprintf("%s %v", e.ID, e.Tiles))
	}
	return out
}

func TestGroupEpisodes(t *testing.T) {
	m := time.Minute
	ms := func(d time.Duration) string { return strconv.FormatInt(at(d).UnixMilli(), 10) }
	now := at(time.Hour)
	drop := dropEpisode(Drop{Start: at(10 * m), End: at(10*m + 40*time.Second), Dur: 40 * time.Second}, "")
	isp, _ := incidentEpisode(model.Incident{Kind: model.VerdictISP, Start: at(10*m + 15*time.Second), End: at(11*m + 30*time.Second), PeakLoss: 0.7}, false)
	// the engine ends an incident that is the drop's lag with the drop
	lagged, _ := incidentEpisode(model.Incident{Kind: model.VerdictISP, Start: at(10*m + 15*time.Second), End: drop.End, PeakLoss: 0.7}, false)
	cases := []struct {
		name        string
		conn, tiles []Episode
		want        []string
	}{
		{"one vendor: one problem",
			nil, []Episode{tileRun("meet", Slow, 0, 20*m), tileRun("gws", Down, 1*m, 15*m), tileRun("youtube", Slow, 2*m, 10*m)},
			[]string{"t-" + ms(0) + "-meet,gws,youtube [meet gws youtube]"}},
		{"two vendors at once: two problems",
			nil, []Episode{tileRun("zoom", Down, 0, 10*m), tileRun("gws", Down, 0, 10*m)},
			[]string{"t-" + ms(0) + "-zoom [zoom]", "t-" + ms(0) + "-gws [gws]"}},
		{"same cause starting within 2 minutes",
			nil, []Episode{tileRun("meet", Down, 0, m), tileRun("gws", Down, 2*m, 3*m)},
			[]string{"t-" + ms(0) + "-meet,gws [meet gws]"}},
		{"same cause starting 3 minutes apart",
			nil, []Episode{tileRun("meet", Down, 0, m), tileRun("gws", Down, 3*m, 4*m)},
			[]string{"t-" + ms(3*m) + "-gws [gws]", "t-" + ms(0) + "-meet [meet]"}},
		{"one tile, 4 minutes apart: one problem",
			nil, []Episode{tileRun("zoom", Down, 0, 5*m), tileRun("zoom", Slow, 9*m, 12*m)},
			[]string{"t-" + ms(0) + "-zoom [zoom]"}},
		{"one tile, 6 minutes apart: two",
			nil, []Episode{tileRun("zoom", Down, 0, 5*m), tileRun("zoom", Slow, 11*m, 12*m)},
			[]string{"t-" + ms(11*m) + "-zoom [zoom]", "t-" + ms(0) + "-zoom [zoom]"}},
		{"a connection drop absorbs the tiles it covers",
			[]Episode{drop},
			[]Episode{
				dropEpisode(Drop{Start: at(10 * m), End: at(10*m + 40*time.Second), Dur: 40 * time.Second}, "zoom"),
				tileRun("meet", Down, 10*m+5*time.Second, 11*m),
				tileRun("teams", Slow, 20*m, 25*m),
			},
			[]string{"t-" + ms(20*m) + "-teams [teams]", "c-" + ms(10*m) + " [zoom meet]"}},
		{"a drop absorbs the incident that is its lag", []Episode{lagged, drop}, nil, []string{"c-" + ms(10*m) + " []"}},
		{"a drop keeps an incident that outlasts it apart", []Episode{isp, drop}, nil, []string{"c-" + ms(10*m+15*time.Second) + " []", "c-" + ms(10*m) + " []"}},
		{"an incident later is its own problem", []Episode{drop, func() Episode { e := isp; e.Start, e.End = at(30*m), at(31*m); return e }()}, nil,
			[]string{"c-" + ms(30*m) + " []", "c-" + ms(10*m) + " []"}},
		{"a drop doesn't absorb a long UDP block",
			[]Episode{drop, func() Episode {
				e, _ := incidentEpisode(model.Incident{Kind: model.VerdictUDPBlocked, Start: at(10*m + 10*time.Second), End: at(5*time.Hour + 10*m)}, false)
				return e
			}()}, nil,
			[]string{"c-" + ms(10*m+10*time.Second) + " []", "c-" + ms(10*m) + " []"}},
		{"a drop doesn't absorb dns", []Episode{drop, func() Episode {
			e, _ := incidentEpisode(model.Incident{Kind: model.VerdictDNS, Start: at(10*m + 10*time.Second), End: at(11 * m)}, false)
			return e
		}()}, nil,
			[]string{"c-" + ms(10*m+10*time.Second) + " []", "c-" + ms(10*m) + " []"}},
		{"a drop doesn't absorb an incident that outlasts it", []Episode{drop, func() Episode { e := isp; e.End = time.Time{}; return e }()}, nil,
			[]string{"c-" + ms(10*m+15*time.Second) + " []", "c-" + ms(10*m) + " []"}},
		{"a slowdown is not absorbed", []Episode{drop, broadEpisode(at(10*m), at(15*m))}, nil,
			[]string{"c-" + ms(10*m) + " []", "c-" + ms(10*m) + "-2 []"}},
		{"an ongoing tile run is not covered by an ended drop",
			[]Episode{drop}, []Episode{tileRun("zoom", Down, 10*m+5*time.Second, 0)},
			[]string{"t-" + ms(10*m+5*time.Second) + "-zoom [zoom]", "c-" + ms(10*m) + " []"}},
	}
	for _, c := range cases {
		got := groupEpisodes(c.conn, c.tiles, byVendor, now)
		if !slices.Equal(ids(got), c.want) {
			t.Errorf("%s:\n got %q\nwant %q", c.name, ids(got), c.want)
		}
		// stable across runs (replays and restarts)
		if again := groupEpisodes(c.conn, c.tiles, byVendor, now); !slices.Equal(ids(again), ids(got)) {
			t.Errorf("%s: unstable: %q then %q", c.name, ids(got), ids(again))
		}
	}

	// the merged drop keeps the drop, its span included, and gains the
	// incident (the verdict's window catching up)
	got := groupEpisodes([]Episode{lagged, drop}, nil, byVendor, now)
	if e := got[0]; e.Kind != EpDrop || e.Verdict != model.VerdictISP || !e.Start.Equal(drop.Start) || !e.End.Equal(drop.End) || e.DropDur != 40*time.Second || e.Sev != Down || e.Cause.Who != WhoProvider {
		t.Errorf("drop + incident: got %+v", e)
	}
	// the drop keeps its own end next to a long UDP block
	blocked, _ := incidentEpisode(model.Incident{Kind: model.VerdictUDPBlocked, Start: at(10*m + 10*time.Second), End: at(5 * time.Hour)}, false)
	got = groupEpisodes([]Episode{drop, blocked}, nil, byVendor, now)
	if i := slices.IndexFunc(got, func(e Episode) bool { return e.Kind == EpDrop }); i < 0 || !got[i].End.Equal(drop.End) {
		t.Errorf("drop + block: got %+v", got)
	}
	// severity is the worst, ongoing wins
	got = groupEpisodes(nil, []Episode{tileRun("zoom", Slow, 0, 5*m), tileRun("zoom", Down, 7*m, 0)}, byVendor, now)
	if e := got[0]; len(got) != 1 || e.Sev != Down || e.Kind != EpOutage || !e.Ongoing() {
		t.Errorf("worst and ongoing: got %+v", got)
	}
}

func TestDayCounts(t *testing.T) {
	now := at(48 * time.Hour)
	eps := []Episode{
		{Start: at(47 * time.Hour), Cause: Cause{Who: WhoProvider}},                                         // ongoing
		{Start: at(40 * time.Hour), End: at(41 * time.Hour), Cause: Cause{Who: WhoRouter}},                  // home
		{Start: at(30 * time.Hour), End: at(31 * time.Hour), Cause: Cause{Who: WhoOffline}},                 // home
		{Start: at(25 * time.Hour), End: at(25*time.Hour + time.Minute), Cause: Cause{Who: WhoApp}},         // app
		{Start: at(26 * time.Hour), End: at(27 * time.Hour), Cause: Cause{Who: WhoSpeedtest}},               // total only
		{Start: at(28 * time.Hour), End: at(29 * time.Hour), Cause: Cause{Who: WhoUnknown}},                 // total only
		{Start: at(10 * time.Hour), End: at(23*time.Hour + 59*time.Minute), Cause: Cause{Who: WhoProvider}}, // too old
		{Start: at(10 * time.Hour), End: at(24 * time.Hour), Cause: Cause{Who: WhoProvider}},                // ended 24 h ago
	}
	want := Counts{Total: 7, Provider: 2, Home: 2, App: 1}
	if got := dayCounts(eps, now); got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// TestFoldKeepsConnectionSeverity: tile drops folded into a lossy
// connection episode (10-09: a 1 min upstream incident, apps dropped for
// 90 s around it) widen its span, but it stays a lossy problem: "Working
// again" says the internet had trouble, not that it was down.
func TestFoldKeepsConnectionSeverity(t *testing.T) {
	m, s := time.Minute, time.Second
	for _, k := range []model.VerdictKind{model.VerdictUpstream, model.VerdictISP} {
		inc, _ := incidentEpisode(model.Incident{Kind: k, Start: at(10*m + 28*s), End: at(11*m + 28*s), PeakLoss: 0.48}, false)
		parts := []Episode{
			dropEpisode(Drop{Start: at(10 * m), End: at(11*m + 30*s), Dur: 90 * s}, "gws"),
			dropEpisode(Drop{Start: at(10*m + 15*s), End: at(11*m + 25*s), Dur: 70 * s}, "slack"),
			tileRun("gws", Down, 10*m, 11*m+30*s),
		}
		eps := groupEpisodes([]Episode{inc}, parts, byVendor, at(20*m))
		if len(eps) != 1 {
			t.Fatalf("%s: %d problems, want 1: %+v", k, len(eps), eps)
		}
		e := eps[0]
		if !e.Start.Equal(at(10*m)) || !e.End.Equal(at(11*m+30*s)) {
			t.Errorf("%s: span %v..%v, want the tile drops' 10:00..11:30", k, e.Start, e.End)
		}
		if e.Sev != Slow || e.Kind != EpSlow || e.DropDur != 0 || !slices.Equal(e.Tiles, []string{"slack", "gws"}) {
			t.Errorf("%s: %+v, want a lossy connection problem of slack and gws", k, e)
		}
		if r := workingAgain(eps, at(20*m)); r == nil || r.Variant != AgainTrouble {
			t.Errorf("%s: working again %+v, want %s", k, r, AgainTrouble)
		}
	}
}
