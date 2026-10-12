package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/profile"
	"github.com/fuck-you-isp/fyisp/internal/speedtest"
	"github.com/fuck-you-isp/fyisp/internal/tiles"
	"github.com/fuck-you-isp/fyisp/internal/web/homescen"
)

func scenario(t *testing.T, name string) *homescen.V2 {
	t.Helper()
	sc, err := homescen.BuildV2(name, home2Now)
	if err != nil {
		t.Fatal(err)
	}
	return sc
}

func decode[T any](t *testing.T, b []byte) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("%v: %s", err, b)
	}
	return v
}

func TestHome2Params(t *testing.T) {
	h := home2Server(t, scenario(t, "calls-profile"), home2Now)
	v := homescen.Viewer{RemoteAddr: "127.0.0.1:5000", Host: "127.0.0.1:3000"}
	for _, c := range []struct {
		q    string
		code int
		msg  string
	}{
		{"part=now", 200, ""},
		{"part=now&tz=America/New_York", 200, ""},
		{"part=now&tz=-240", 200, ""},
		{"", 400, "bad part"},
		{"part=live", 400, "bad part"},
		{"part=now&id=zoom", 400, "id is for part=tile"},
		{"part=now&span=d", 400, "unknown parameter"},
		{"part=now&part=now", 400, "repeated parameter"},
		{"part=now&tz=Mars/Olympus", 400, "unknown tz"},
		{"part=now&tz=9999", 400, "bad tz"},
		{"part=tile", 400, "needs id"},
		{"part=tile&id=skype", 404, "unknown tile"},
		{"part=tile&id=github", 404, "tile not monitored"},
		{"part=tile&id=zoom", 200, ""},
	} {
		code, b := home2Get(t, h, v, c.q)
		if code != c.code || !strings.Contains(string(b), c.msg) {
			t.Errorf("%q: %d %s, want %d %q", c.q, code, b, c.code, c.msg)
		}
	}
	// Without the tiles engine there is no new Home: the page keeps the old one.
	nh := Local(Deps{Profile: func() *model.Profile { return nil }, Status: func() Status { return Status{} }}, LocalOptions{Addr: "127.0.0.1:3000"})
	if code, b := home2Get(t, nh, v, "part=now"); code != 404 {
		t.Errorf("no engine: %d %s", code, b)
	}
}

func TestHome2CanControl(t *testing.T) {
	sc := scenario(t, "good")
	lo := func(addr, admin string, extra ...string) LocalOptions {
		return LocalOptions{Addr: addr, AdminToken: admin, ExtraHosts: extra, CSRFToken: "t"}
	}
	for _, c := range []struct {
		name string
		o    LocalOptions
		host string
		hdr  map[string]string
		want bool
	}{
		{"loopback", lo("127.0.0.1:3000", ""), "127.0.0.1:3000", nil, true},
		{"loopback through a proxy", lo("127.0.0.1:3000", ""), "127.0.0.1:3000", map[string]string{"X-Forwarded-For": "203.0.113.9"}, false},
		{"tunnel", lo("127.0.0.1:3000", ""), "127.0.0.1:3000", map[string]string{"Cf-Connecting-Ip": "203.0.113.9"}, false},
		{"admin token, none sent", lo("127.0.0.1:3000", "tok"), "127.0.0.1:3000", nil, false},
		{"admin token in the header", lo("127.0.0.1:3000", "tok"), "127.0.0.1:3000", map[string]string{HeaderAdmin: "tok"}, true},
		{"admin cookie", lo("127.0.0.1:3000", "tok"), "127.0.0.1:3000", map[string]string{"Cookie": "fyisp_admin_3000=tok"}, true},
		{"admin cookie, wrong", lo("127.0.0.1:3000", "tok"), "127.0.0.1:3000", map[string]string{"Cookie": "fyisp_admin_3000=nope"}, false},
		{"--listen 0.0.0.0 without a token", lo("0.0.0.0:3000", ""), "127.0.0.1:3000", nil, false},
		{"--allow-host without a token", lo("127.0.0.1:3000", "", "fyisp.lan"), "fyisp.lan", nil, false},
	} {
		d := Deps{Profile: func() *model.Profile { return sc.Profile }, Status: func() Status { return Status{} }, Tiles: sc}
		h := Local(d, c.o).(*localHandler)
		h.now = func() time.Time { return home2Now }
		h.h2speed = sc.Speed
		code, b := home2Get(t, h, homescen.Viewer{RemoteAddr: "127.0.0.1:5000", Host: c.host, Header: c.hdr}, "part=now")
		if code != 200 {
			t.Fatalf("%s: %d %s", c.name, code, b)
		}
		if got := decode[Home2Now](t, b).Speed.CanControl; got != c.want {
			t.Errorf("%s: can_control %v, want %v", c.name, got, c.want)
		}
	}
}

func TestHome2SameMachine(t *testing.T) {
	h := Local(Deps{Profile: func() *model.Profile { return nil }}, LocalOptions{Addr: "0.0.0.0:3000", ExtraHosts: []string{"fyisp.lan:3000", "nas.local"}}).(*localHandler)
	reads := 0
	h.own = &ownAddrs{now: func() time.Time { return home2Now }, list: func() ([]netip.Addr, error) {
		reads++
		return []netip.Addr{netip.MustParseAddr("192.168.1.10"), netip.MustParseAddr("fe80::10%eth0")}, nil
	}}
	for _, c := range []struct {
		remote, host string
		hdr          string
		want         bool
	}{
		{"127.0.0.1:5000", "127.0.0.1:3000", "", true},
		{"[::1]:5000", "[::1]:3000", "", true},
		{"[::ffff:127.0.0.1]:5000", "127.0.0.1:3000", "", true},
		{"192.168.1.10:5000", "192.168.1.10:3000", "", true},           // this host's own LAN address
		{"[fe80::10%eth0]:5000", "127.0.0.1:3000", "", true},           // its link-local one, zone and all
		{"192.168.1.20:5000", "192.168.1.10:3000", "", false},          // another machine
		{"127.0.0.1:5000", "localhost:3000", "", true},                 // a loopback name
		{"127.0.0.1:5000", "fyisp.lan:3000", "", false},                // an --allow-host name: reached from elsewhere
		{"127.0.0.1:5000", "NAS.local:3000", "", false},                // (any port, any case)
		{"127.0.0.1:5000", "127.0.0.1:3000", "X-Forwarded-For", false}, // a proxy on this machine
		{"127.0.0.1:5000", "127.0.0.1:3000", "Forwarded", false},
		{"127.0.0.1:5000", "127.0.0.1:3000", "X-Real-Ip", false},
		{"127.0.0.1:5000", "127.0.0.1:3000", "Cf-Connecting-Ip", false}, // cloudflared
		{"127.0.0.1:5000", "127.0.0.1:3000", "Tailscale-User-Login", false},
		{"garbage", "127.0.0.1:3000", "", false},
	} {
		r := httptest.NewRequest(http.MethodGet, "/api/home2?part=now", nil)
		r.RemoteAddr, r.Host = c.remote, c.host
		if c.hdr != "" {
			r.Header.Set(c.hdr, "203.0.113.9")
		}
		if got := h.sameMachine(r); got != c.want {
			t.Errorf("%s Host %s %s: %v, want %v", c.remote, c.host, c.hdr, got, c.want)
		}
	}
	if reads != 1 {
		t.Errorf("the interface addresses were read %d times in one 30 s window", reads)
	}
}

// The Wi-Fi wording is decided per request, never cached across viewers.
func TestHome2WiFiPerViewer(t *testing.T) {
	sc := scenario(t, "wifi-same-machine")
	sc.Viewer.Listen = "0.0.0.0:3000"
	h := home2Server(t, sc, home2Now)
	type res struct{ label, hint, headline string }
	get := func(remote, xff string) res {
		v := homescen.Viewer{RemoteAddr: remote, Host: "192.168.1.10:3000"}
		if xff != "" {
			v.Header = map[string]string{"X-Forwarded-For": xff}
		}
		code, b := home2Get(t, h, v, "part=now")
		if code != 200 {
			t.Fatalf("%d %s", code, b)
		}
		d := decode[Home2Now](t, b)
		return res{d.Path[0].Label, d.PathHint, d.Answer.Headline}
	}
	wifi := res{"Your Wi-Fi and router", "", "Your Wi-Fi and router are likely having trouble."}
	for i, c := range []struct {
		remote, xff string
		wifi        bool
	}{
		{"127.0.0.1:5000", "", true},
		{"192.168.1.20:5000", "", false},
		{"127.0.0.1:5000", "203.0.113.9", false},
		{"192.168.1.10:5000", "", true},
		{"192.168.1.20:5000", "", false},
	} {
		got := get(c.remote, c.xff)
		if (got == wifi) != c.wifi || !c.wifi && (got.label != "Your home router" || got.hint == "" || !strings.HasPrefix(got.headline, "Your home router")) {
			t.Errorf("request %d from %s (X-Forwarded-For %q): %+v", i, c.remote, c.xff, got)
		}
	}
	// In a container the uplink fyisp sees is its own virtual link.
	h.inContainer = func() bool { return true }
	if got := get("127.0.0.1:5000", ""); got == wifi {
		t.Errorf("in a container: %+v", got)
	}
}

// counting counts a TilesSource's chart reads; its snapshot can change.
type counting struct {
	*homescen.V2
	charts atomic.Int32
}

func (c *counting) Chart(id string) (*tiles.Chart, bool) {
	c.charts.Add(1)
	return c.V2.Chart(id)
}

// part=tile: the chart block is cached per tile, zone and 5-minute bucket;
// the header follows the snapshot at once.
func TestHome2TileCache(t *testing.T) {
	sc := scenario(t, "good")
	src := &counting{V2: sc}
	now := home2Now.Add(30 * time.Second)
	d := Deps{Profile: func() *model.Profile { return sc.Profile }, Status: func() Status { return Status{} }, Tiles: src}
	h := Local(d, LocalOptions{Addr: "127.0.0.1:3000"}).(*localHandler)
	h.now = func() time.Time { return now }
	get := func(q string) Home2TileDetail {
		code, b := home2Get(t, h, sc.Viewer, q)
		if code != 200 {
			t.Fatalf("%s: %d %s", q, code, b)
		}
		return decode[Home2TileDetail](t, b)
	}
	a := get("part=tile&id=zoom&tz=America/New_York")
	// A minute later the call path is slow: the figures say so, the chart is the cached one.
	snap := *sc.Snap
	call := *snap.Tiles[0].Call
	call.DelayMs = 200
	snap.Tiles[0].Call = &call
	sc.Snap = &snap
	now = now.Add(time.Minute)
	b := get("part=tile&id=zoom&tz=America/New_York")
	if a.Metrics[1].Value != "21 ms" || b.Metrics[1].Value != "200 ms" || !*b.Metrics[1].Bad {
		t.Errorf("figures: %+v then %+v", a.Metrics[1], b.Metrics[1])
	}
	if n := src.charts.Load(); n != 1 || b.Text != a.Text || b.From != a.From {
		t.Errorf("same bucket: %d chart reads", n)
	}
	// The tile goes down: the header at once, and the chart again (its
	// ongoing band is worded with the tile's word).
	snap2 := *sc.Snap
	snap2.Tiles[0].State, snap2.Tiles[0].Since = tiles.Down, now
	sc.Snap = &snap2
	c := get("part=tile&id=zoom&tz=America/New_York")
	if c.State != "down" || c.Word != "Down" || !strings.HasPrefix(c.Reason, "Not connecting since 10:31 am.") {
		t.Errorf("header: %s %s %q", c.State, c.Word, c.Reason)
	}
	if n := src.charts.Load(); n != 2 {
		t.Errorf("a new word: %d chart reads, want 2", n)
	}
	get("part=tile&id=zoom&tz=America/New_York")
	get("part=tile&id=zoom&tz=Europe/Berlin")
	if n := src.charts.Load(); n != 3 {
		t.Errorf("another zone: %d chart reads, want 3", n)
	}
	now = now.Add(4 * time.Minute) // 10:35:30: the next bucket
	get("part=tile&id=zoom&tz=America/New_York")
	if n := src.charts.Load(); n != 4 {
		t.Errorf("the next bucket: %d chart reads, want 4", n)
	}
	get("part=tile&id=m365&tz=America/New_York")
	if n := src.charts.Load(); n != 5 {
		t.Errorf("another tile: %d chart reads, want 5", n)
	}
}

// The problems block is kept per snapshot Rev, zone and local date.
func TestHome2ProblemsMemo(t *testing.T) {
	sc := scenario(t, "good")
	now := home2Now
	h := home2Server(t, sc, now)
	h.now = func() time.Time { return now }
	summary := func() string {
		code, b := home2Get(t, h, sc.Viewer, "part=now&tz=America/New_York")
		if code != 200 {
			t.Fatalf("%d %s", code, b)
		}
		return decode[Home2Now](t, b).Problems.Summary
	}
	first := summary()
	snap := *sc.Snap
	snap.Eps = snap.Eps[1:]
	sc.Snap = &snap
	if got := summary(); got != first {
		t.Errorf("same Rev: %q, then %q", first, got)
	}
	snap.Rev++
	if got := summary(); got == first || !strings.HasPrefix(got, "4 problems:") {
		t.Errorf("new Rev: %q", got)
	}
	snap.Eps = nil
	snap.LastMeasured = now.Add(14*time.Hour - 4*time.Second)
	now = now.Add(14 * time.Hour) // after midnight in New York
	if got := summary(); got != "No problems in the last 7 days." {
		t.Errorf("the next day: %q", got)
	}
}

// The speed line: the usual leaves the latest test out and needs 3 others;
// the stored tests are re-read after a minute, or when a test starts or ends.
func TestHome2Speed(t *testing.T) {
	sc := scenario(t, "good")
	now := home2Now
	h := home2Server(t, sc, now)
	h.now = func() time.Time { return now }
	speed := func() Home2Speed {
		code, b := home2Get(t, h, sc.Viewer, "part=now&tz=America/New_York")
		if code != 200 {
			t.Fatalf("%d %s", code, b)
		}
		return decode[Home2Now](t, b).Speed
	}
	sp := speed()
	if sp.Usual == nil || *sp.Usual != (Home2SpeedUsual{925, 938}) || sp.Latest.DownMbps != 930 || sp.Running != nil || !sp.CanControl {
		t.Fatalf("%+v %+v", sp.Usual, sp.Latest)
	}
	slow := func(at time.Time, down float64) model.SpeedTest {
		d, u := down*1e6, 400e6
		return model.SpeedTest{Start: at, End: at.Add(time.Minute), Outcome: model.SpeedOK, DownBps: &d, UpBps: &u}
	}
	sc.Speed.List = append(sc.Speed.List, slow(now.Add(-time.Minute), 300))
	now = now.Add(30 * time.Second)
	if sp := speed(); sp.Latest.DownMbps != 930 {
		t.Errorf("re-read within a minute: %+v", sp.Latest)
	}
	sc.Speed.Run = &speedtest.Entry{ID: 7, EstFinish: now.Add(20 * time.Second)}
	sp = speed()
	if sp.Latest.DownMbps != 300 || sp.Running == nil || sp.Running.Entry != 7 {
		t.Errorf("a test started: %+v %+v", sp.Latest, sp.Running)
	}
	if want := "Your speed: about 300 Mbps down / 400 Mbps up — much slower than usual (usually about 925 / 938 Mbps)."; sp.Latest.Text != want {
		t.Errorf("%q", sp.Latest.Text)
	}
	sc.Speed.Run = nil
	sc.Speed.List = sc.Speed.List[len(sc.Speed.List)-3:]
	now = now.Add(time.Minute)
	if sp := speed(); sp.Usual != nil || sp.Latest.Text != "Your speed: about 300 Mbps down / 400 Mbps up." {
		t.Errorf("2 tests besides the latest: %+v %q", sp.Usual, sp.Latest.Text)
	}
	h.h2speed = nil
	if sp := speed(); sp.Available || sp.Latest != nil {
		t.Errorf("without speed tests: %+v", sp)
	}
}

// The engine's snapshot before its first tick: checking.
func TestHome2NilSnapshot(t *testing.T) {
	sc := scenario(t, "good")
	sc.Snap, sc.NotReady = nil, true
	h := home2Server(t, sc, home2Now)
	code, b := home2Get(t, h, sc.Viewer, "part=now")
	if code != 200 {
		t.Fatalf("%d %s", code, b)
	}
	d := decode[Home2Now](t, b)
	if !strings.HasPrefix(d.Answer.Key, "checking:") || d.Problems.Summary != "Checking…" || len(d.Tiles) != 11 || d.Tiles[0].State != "none" {
		t.Errorf("%+v", d.Answer)
	}
}

// One answer key, one text: a change of the words is a change of the key.
func TestHome2AnswerKey(t *testing.T) {
	key := func(sc *homescen.V2, now time.Time) Home2Answer {
		h := home2Server(t, sc, now)
		code, b := home2Get(t, h, sc.Viewer, "part=now&tz=America/New_York")
		if code != 200 {
			t.Fatalf("%d %s", code, b)
		}
		return decode[Home2Now](t, b).Answer
	}
	sc := scenario(t, "provider-loss-3")
	a := key(sc, home2Now)
	if a.Tone != "warn" || a.Headline != "Calls may be choppy." {
		t.Fatalf("%+v", a)
	}
	// Teams stops connecting: the engine's key (the same tiles, since) stays, the words don't.
	snap := *sc.Snap
	snap.Tiles[1].State = tiles.Down
	sc.Snap = &snap
	if b := key(sc, home2Now); b.Key == a.Key || b.Headline == a.Headline {
		t.Errorf("Teams down: %q %q, then %q %q", a.Key, a.Headline, b.Key, b.Headline)
	}
	// Past midnight "Started at 10:12 am." becomes "Started yesterday at 10:12 am.".
	sc = scenario(t, "isp")
	a = key(sc, home2Now)
	later := *sc.Snap
	later.LastMeasured = later.LastMeasured.Add(14 * time.Hour)
	sc.Snap = &later
	b := key(sc, home2Now.Add(14*time.Hour))
	if a.Key == b.Key || !strings.Contains(b.Sub, "Started yesterday at 10:12 am.") {
		t.Errorf("midnight: %q %q, then %q %q", a.Key, a.Sub, b.Key, b.Sub)
	}
	sc = scenario(t, "isp")
	if c := key(sc, home2Now.Add(time.Minute)); c.Key != a.Key {
		t.Errorf("a minute later: %q, then %q", a.Key, c.Key)
	}
}

// Copy that no scenario shows, pinned here.
func TestHome2CopyPins(t *testing.T) {
	c := h2copy{loc: time.UTC, now: home2Now}
	def := func(id string) profile.TileDef { return tileDef(id) }
	check := func(what, got, want string) {
		t.Helper()
		if got != want {
			t.Errorf("%s:\n got %q\nwant %q", what, got, want)
		}
	}
	check("no data yet", c.tileReason(def("zoom"), tiles.TileSnap{ID: "zoom", State: tiles.None}, false, false), "No data yet.")
	snap := scenario(t, "good").Snap
	s2 := *snap
	s2.Layers[1].State = tiles.Slow
	check("a slow layer", c.path(&s2, appRow{hop: -1}, false, false)[1].Sub, "Having some trouble")
	teams := profile.ResolvedTile{TileDef: def("teams"), Web: []model.Target{{Name: "Microsoft-Teams"}}}
	check("tcp only", c.coverage(teams, tiles.CallTCPOnly), "Can connect; call quality isn’t checked on this setup.")
	teams.UDP, teams.SharedUDP = []model.Target{{Name: "Cloudflare-STUN"}}, true
	check("teams on the shared path", c.coverage(teams, tiles.CallShared),
		"Call quality is judged from the general call path, because this setup doesn’t check Microsoft’s own call servers.")
	slack := profile.ResolvedTile{TileDef: def("slack"), Web: []model.Target{{Name: "Slack"}}, UDP: []model.Target{{Name: "Cloudflare-STUN"}}, SharedUDP: true}
	check("slack", c.coverage(slack, tiles.CallShared), "Call quality is judged from the general call path; fyisp can’t test Slack’s own servers before a call.")
	check("onedrive", c.coverage(profile.ResolvedTile{TileDef: def("onedrive")}, ""),
		"Checks that Microsoft’s OneDrive front door answers. Work OneDrive on your company’s SharePoint isn’t checked.")
	m365 := tiles.TileSnap{ID: "m365", State: tiles.Slow}
	gh := tiles.TileSnap{ID: "github", State: tiles.Down}
	_, sub, _, warn := c.appCard([]tiles.TileSnap{m365, gh}, []tiles.Cause{{Who: tiles.WhoApp, Vendor: "microsoft", Evidence: tiles.EvAppOnly},
		{Who: tiles.WhoApp, Vendor: "github", Evidence: tiles.EvAppOnly}}, nil)
	check("two vendors", sub, "Likely cause: the apps’ own side — your router and your internet provider look fine.")
	if warn {
		t.Error("two vendors: amber")
	}
	h, _, _, _ := c.appCard([]tiles.TileSnap{{ID: "teams", State: tiles.Down}, {ID: "meet", State: tiles.Down}},
		[]tiles.Cause{{Who: tiles.WhoUnknown}, {Who: tiles.WhoUnknown}}, nil)
	check("calls down", h, "Your internet is fine. Calls aren’t connecting.")
	u := &Home2SpeedUsual{DownMbps: 925, UpMbps: 938}
	check("faster", c.speedText(1200, 1250, u), "Your speed: about 1200 Mbps down / 1250 Mbps up — faster than usual.")
	check("a bit slower", c.speedText(700, 900, u), "Your speed: about 700 Mbps down / 900 Mbps up — a bit slower than usual (usually about 925 / 938 Mbps).")
	check("dropping", c.problemWhat(tiles.Episode{Start: home2Now.Add(-time.Minute), Kind: tiles.EpDrop, Connection: true, Sev: tiles.Down}), "Everything is dropping.")
	s3 := *snap
	s3.LastMeasured, s3.Conn = time.Time{}, tiles.ConnState{Kind: tiles.ConnStale}
	a := c.answer(&s3, nil, appRow{hop: -1}, false, true, false)
	check("stale, never checked", a.Headline+" "+a.Sub, "fyisp hasn’t checked anything yet. Is the computer it runs on asleep or turned off?")
	check("custom config", c.enableHint(def("github"), true), "To check GitHub, add the tag tile:github to a target in your --config file, or base the file on extends: [default].")
}

// A process that just started is checking, not stale, whatever its last
// stored sample (a restart after a night off).
func TestHome2RestartNotStale(t *testing.T) {
	sc := scenario(t, "stale")
	started := home2Now.Add(-30 * time.Second)
	d := Deps{Profile: func() *model.Profile { return sc.Profile }, Status: func() Status { return Status{Started: started} }, Tiles: sc}
	h := Local(d, LocalOptions{Addr: "127.0.0.1:3000"}).(*localHandler)
	h.now = func() time.Time { return home2Now }
	get := func() Home2Now {
		code, b := home2Get(t, h, sc.Viewer, "part=now")
		if code != 200 {
			t.Fatalf("%d %s", code, b)
		}
		return decode[Home2Now](t, b)
	}
	if a := get(); a.Stale || a.Answer.Tone != "checking" || a.LastCheck != 0 {
		// last_check 0: the page's own stale rule (an old last check) can't fire either
		t.Errorf("30 s after the start: stale %v, tone %s, last_check %d", a.Stale, a.Answer.Tone, a.LastCheck)
	}
	started = home2Now.Add(-3 * time.Minute)
	if a := get(); !a.Stale || a.Answer.Tone != "stale" {
		t.Errorf("3 min after the start: stale %v, tone %s", a.Stale, a.Answer.Tone)
	}
}

// On a network that blocks call traffic, a call tile whose own servers are
// down stays Down (only the block is softened to Choppy).
func TestHome2BlockedKeepsAppOutage(t *testing.T) {
	sc := scenario(t, "udp-blocked")
	snap := *sc.Snap
	since := home2Now.Add(-10 * time.Minute)
	snap.Tiles[0].Cause = tiles.Cause{Who: tiles.WhoApp, Vendor: "zoom", Evidence: tiles.EvAppOnly}
	snap.Eps = append([]tiles.Episode{{ID: "t-1-zoom", Start: since, Sev: tiles.Down, Kind: tiles.EpOutage, Tiles: []string{"zoom"},
		Cause: snap.Tiles[0].Cause}}, snap.Eps...)
	sc.Snap = &snap
	h := home2Server(t, sc, home2Now)
	code, b := home2Get(t, h, sc.Viewer, "part=now&tz=America/New_York")
	if code != 200 {
		t.Fatalf("%d %s", code, b)
	}
	d := decode[Home2Now](t, b)
	if z, tm := d.Tiles[0], d.Tiles[1]; z.State != "down" || !strings.HasPrefix(z.Reason, "Not connecting since") || tm.State != "slow" {
		t.Errorf("zoom %s %q; teams %s", z.State, z.Reason, tm.State)
	}
	code, b = home2Get(t, h, sc.Viewer, "part=tile&id=zoom&tz=America/New_York")
	if p := decode[Home2TileDetail](t, b); code != 200 || p.State != "down" {
		t.Errorf("panel: %d %s", code, p.State)
	}
}
