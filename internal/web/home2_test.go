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
	wifi := res{"Your Wi-Fi & router", "", "Your Wi-Fi or router is likely having trouble."}
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
	// The tile goes down a minute later: the header says so, the chart is the cached one.
	snap := *sc.Snap
	snap.Tiles[0].State, snap.Tiles[0].Since = tiles.Down, now
	sc.Snap = &snap
	now = now.Add(time.Minute)
	b := get("part=tile&id=zoom&tz=America/New_York")
	if a.State != "good" || b.State != "down" || b.Word != "Down" || !strings.HasPrefix(b.Reason, "Not connecting since 10:30 am.") {
		t.Errorf("header: %s %s then %s %s %q", a.State, a.Word, b.State, b.Word, b.Reason)
	}
	if n := src.charts.Load(); n != 1 || b.Text != a.Text || b.From != a.From {
		t.Errorf("same bucket: %d chart reads", n)
	}
	get("part=tile&id=zoom&tz=Europe/Berlin")
	if n := src.charts.Load(); n != 2 {
		t.Errorf("another zone: %d chart reads, want 2", n)
	}
	now = now.Add(4 * time.Minute) // 10:35:30: the next bucket
	get("part=tile&id=zoom&tz=America/New_York")
	if n := src.charts.Load(); n != 3 {
		t.Errorf("the next bucket: %d chart reads, want 3", n)
	}
	get("part=tile&id=m365&tz=America/New_York")
	if n := src.charts.Load(); n != 4 {
		t.Errorf("another tile: %d chart reads, want 4", n)
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
	sc.Eps = sc.Eps[1:]
	if got := summary(); got != first {
		t.Errorf("same Rev: %q, then %q", first, got)
	}
	snap := *sc.Snap
	snap.Rev++
	sc.Snap = &snap
	if got := summary(); got == first || !strings.HasPrefix(got, "4 problems:") {
		t.Errorf("new Rev: %q", got)
	}
	sc.Eps = nil
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
	if d.Answer.Key != "checking" || d.Problems.Summary != "Checking…" || len(d.Tiles) != 11 || d.Tiles[0].State != "none" {
		t.Errorf("%+v", d.Answer)
	}
}
