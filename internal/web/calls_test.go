package web

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/probe"
	"github.com/fuck-you-isp/fyisp/internal/store"
)

// burst is one synthetic STUN burst: answered of 5 requests with this median
// RTT and jitter; reason (not timeout) fails the whole burst otherwise.
type burst struct {
	rtt, jitter float64
	answered    int
	reason      model.Reason
	skip        bool // not measured
}

// writeBurst writes a burst's three sibling samples, as the probe does.
func writeBurst(sink model.Sink, target string, ts time.Time, b burst) {
	key := func(k model.ProbeKind) model.SeriesKey { return model.SeriesKey{Target: target, Kind: k} }
	u := model.Sample{Key: key(model.KindUDP), Slot: ts}
	j := model.Sample{Key: key(model.KindUDPJitter), Slot: ts}
	l := model.Sample{Key: key(model.KindUDPLoss), Slot: ts}
	if b.reason != 0 {
		for _, s := range []*model.Sample{&u, &j, &l} {
			s.Lost, s.Reason = true, b.reason
		}
	} else {
		if b.answered > 0 {
			u.RTT = time.Duration(b.rtt * float64(time.Millisecond))
		} else {
			u.Lost, u.Reason = true, model.ReasonTimeout
		}
		if b.answered >= 2 {
			j.RTT = time.Duration(b.jitter * float64(time.Millisecond))
		} else {
			j.Lost, j.Reason = true, model.ReasonGap
		}
		l.RTT = time.Duration((5-b.answered)*20) * time.Millisecond
	}
	sink.Observe(u)
	sink.Observe(j)
	sink.Observe(l)
}

type fakeUDPState []probe.UDPState

func (f fakeUDPState) UDPState() []probe.UDPState { return f }

// callsProfile: four call-path STUN targets (Meet and the Teams relay
// among them), a far Twilio region that is not part of the path, and the
// call apps' websites over TCP.
func callsProfile(teams bool) *model.Profile {
	udp := func(name string, iv time.Duration, geo string, tags ...string) model.Target {
		return model.Target{Name: name, Host: strings.ToLower(name) + ".example.com", Port: 3478, Group: "calls",
			Kinds: []model.ProbeKind{model.KindUDP}, Interval: iv, Geo: geo, Tags: append([]string{"udp-path"}, tags...)}
	}
	web := func(name, app string) model.Target {
		return model.Target{Name: name, Host: strings.ToLower(name) + ".example.com", Group: "apps", Port: 8443, Tags: []string{"app:" + app}}
	}
	p := &model.Profile{Name: "calls", Groups: []model.Group{{ID: "path", Title: "Network path"}, {ID: "calls", Title: "Call path"}, {ID: "apps", Title: "Apps"}}}
	p.Targets = []model.Target{
		{Name: "Gateway", Host: "192.168.1.1", Group: "path", Layer: model.LayerGateway, Kinds: []model.ProbeKind{model.KindICMP}},
		udp("Meet-STUN", 5*time.Second, "global", "app:meet"),
		udp("Cloudflare-STUN", 5*time.Second, "global"),
		udp("Twilio-STUN-global", 5*time.Second, ""),
		udp("Twilio-jp1", 30*time.Second, "as"),
		web("Google-Meet", "meet"), web("Microsoft-Teams", "teams"), web("Zoom", "zoom"), web("Discord", "discord"),
	}
	if teams {
		p.Targets = append(p.Targets, udp("Teams-Relay", 5*time.Second, "global", "app:teams"))
	}
	return p
}

// callsFixture fills 30 minutes of bursts (fill decides each one; nil: all
// healthy) and 15s TCP samples of the websites.
func callsFixture(t *testing.T, now time.Time, teams bool, fill func(target string, ts time.Time) burst) (*countingStore, Deps, *fakeVerdict) {
	t.Helper()
	p := callsProfile(teams)
	f := store.NewFake()
	for _, tg := range p.Targets {
		switch {
		case tg.IsUDP():
			for ts := now.Add(-30 * time.Minute).Truncate(tg.Interval); ts.Before(now); ts = ts.Add(tg.Interval) {
				b := burst{rtt: 20, jitter: 2, answered: 5}
				if tg.Name == "Twilio-jp1" {
					b.rtt = 260
				}
				if fill != nil {
					b = fill(tg.Name, ts)
				}
				if !b.skip {
					writeBurst(f, tg.Name, ts, b)
				}
			}
		case tg.Layer == "":
			for ts := now.Add(-30 * time.Minute).Truncate(15 * time.Second); ts.Before(now); ts = ts.Add(15 * time.Second) {
				f.Observe(model.Sample{Key: model.SeriesKey{Target: tg.Name, Kind: model.KindTCP}, Slot: ts, RTT: 24 * time.Millisecond})
			}
		}
	}
	cs := &countingStore{Reader: f}
	fv := &fakeVerdict{v: model.Verdict{Kind: model.VerdictOK, Since: now.Add(-time.Hour), Summary: "Everything looks healthy."}}
	d := Deps{
		Profile: func() *model.Profile { return p },
		Store:   cs,
		Status:  func() Status { return Status{Version: "v9.9.9", Targets: len(p.Targets)} },
		Verdict: fv,
		UDP:     fakeUDPState{{Target: "Teams-Relay", Region: "swedencentral", Epoch: 2}, {Target: "Meet-STUN"}},
	}
	return cs, d, fv
}

func getCalls(t *testing.T, h http.Handler, path string, hdr map[string]string) (callsResp, string) {
	t.Helper()
	w := do(h, "GET", path, hdr)
	if w.Code != 200 {
		t.Fatalf("GET %s = %d: %s", path, w.Code, w.Body.String())
	}
	var c callsResp
	if err := json.Unmarshal(w.Body.Bytes(), &c); err != nil {
		t.Fatalf("GET %s: %v\n%s", path, err, w.Body.String())
	}
	return c, w.Body.String()
}

var localHdr = map[string]string{"Host": "127.0.0.1:3000"}

type callsGroupResp struct {
	State   string   `json:"state"`
	MOS     *float64 `json:"mos"`
	RTT     *float64 `json:"rtt_ms"`
	RTT95   *float64 `json:"rtt_p95_ms"`
	Jitter  *float64 `json:"jitter_ms"`
	Loss    *float64 `json:"loss"`
	Samples int      `json:"samples"`
	Unknown int      `json:"unknown"`
	Factor  string   `json:"factor"`
	Worst   *struct {
		At    int64    `json:"at"`
		State string   `json:"state"`
		MOS   *float64 `json:"mos"`
		Loss  *float64 `json:"loss"`
	} `json:"worst"`
	Minutes struct {
		State []*string  `json:"state"`
		MOS   []*float64 `json:"mos"`
	} `json:"minutes"`
	Targets []struct {
		Name         string `json:"name"`
		NotAnswering bool   `json:"not_answering"`
		Samples      int    `json:"samples"`
		Unknown      int    `json:"unknown"`
	} `json:"targets"`
}

type callsResp struct {
	From, To, Start, Step int64
	Capped                bool `json:"capped"`
	State                 string
	Quiet                 bool
	Cause                 *struct {
		Source, App, Kind, Summary, Factor string
	}
	Path *callsGroupResp
	Apps []callsAppResp
}

type callsAppResp struct {
	App     string `json:"app"`
	UDP     string `json:"udp"`
	Region  string `json:"region"`
	OwnDown bool   `json:"own_down"`
	callsGroupResp
	WebMs      *float64 `json:"web_ms"`
	WebLoss    *float64 `json:"web_loss"`
	WebSamples int      `json:"web_samples"`
}

func (c callsResp) app(id string) int {
	return slices.IndexFunc(c.Apps, func(a callsAppResp) bool { return a.App == id })
}

func TestCallsGood(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 30, 30, 0, time.UTC) // fixed: the minute count must not depend on when the test runs
	cs, d, _ := callsFixture(t, now, true, nil)
	lh := Local(d, LocalOptions{Addr: "127.0.0.1:3000"}).(*localHandler)
	lh.now = func() time.Time { return now }
	h := http.Handler(lh)
	c, _ := getCalls(t, h, "/api/calls?from=now-10m", localHdr)
	if c.State != "good" || c.Cause != nil || c.Quiet {
		t.Errorf("top state %q cause %+v", c.State, c.Cause)
	}
	if c.Step != 60000 || c.Start%60000 != 0 || c.To-c.From != (10*time.Minute).Milliseconds() {
		t.Errorf("window %d..%d start %d step %d", c.From, c.To, c.Start, c.Step)
	}
	if q := cs.calls.Load(); q != 1 {
		t.Errorf("%d store queries, want 1", q)
	}
	// One query over the three UDP kinds of every STUN target (the far
	// region too) and the websites' TCP, with one-minute buckets.
	if n := len(cs.last.Keys); n != 5*3+4 {
		t.Errorf("%d keys: %v", n, cs.last.Keys)
	}
	if store.BucketStep(cs.last.From, cs.last.To, cs.last.MaxPoints, store.TierRaw) != time.Minute {
		t.Errorf("query %+v does not bucket by the minute", cs.last)
	}
	p := c.Path
	if p == nil || p.State != "good" || p.MOS == nil || *p.MOS < 4.3 || math.Abs(*p.RTT-20) > 0.01 || *p.Loss != 0 || math.Abs(*p.Jitter-2) > 0.01 {
		t.Fatalf("path %+v", p)
	}
	// The far Twilio region is not part of the path.
	var names []string
	for _, x := range p.Targets {
		names = append(names, x.Name)
	}
	if !slices.Equal(names, []string{"Meet-STUN", "Cloudflare-STUN", "Twilio-STUN-global", "Teams-Relay"}) {
		t.Errorf("path targets %v", names)
	}
	if p.Samples < 4*100 || p.Worst == nil || p.Worst.State != "good" || len(p.Minutes.State) != 11 || len(p.Minutes.MOS) != 11 {
		t.Errorf("samples %d worst %+v minutes %d", p.Samples, p.Worst, len(p.Minutes.State))
	}
	var apps []string
	for _, a := range c.Apps {
		apps = append(apps, a.App+":"+a.UDP)
	}
	if !slices.Equal(apps, []string{"teams:measured", "meet:measured", "zoom:not_measurable", "discord:not_measurable"}) {
		t.Errorf("apps %v", apps)
	}
	tm := c.Apps[c.app("teams")]
	if tm.Region != "swedencentral" || tm.State != "good" || tm.WebMs == nil || *tm.WebMs != 24 {
		t.Errorf("teams %+v", tm)
	}
	z := c.Apps[c.app("zoom")]
	if z.WebMs == nil || *z.WebMs != 24 || *z.WebLoss != 0 || z.WebSamples < 39 || z.State != "" || z.MOS != nil {
		t.Errorf("zoom %+v", z)
	}

	// Without the Teams relay, Teams is a "shared" row: its calls
	// take the call path, its website is measured, and it never changes
	// the top state.
	_, d, _ = callsFixture(t, now, false, nil)
	lh = Local(d, LocalOptions{Addr: "127.0.0.1:3000"}).(*localHandler)
	lh.now = func() time.Time { return now }
	c, _ = getCalls(t, lh, "/api/calls", localHdr)
	i := c.app("teams")
	if i < 0 || c.app("meet") < 0 || c.State != "good" {
		t.Fatalf("without the relay: %+v", c.Apps)
	}
	if tm := c.Apps[i]; tm.UDP != "shared" || tm.OwnDown || tm.State != "" || tm.WebSamples == 0 || tm.WebMs == nil || *tm.WebMs != 24 {
		t.Errorf("teams without the relay: %+v", tm)
	}
	apps = nil
	for _, a := range c.Apps {
		apps = append(apps, a.App+":"+a.UDP)
	}
	if !slices.Equal(apps, []string{"teams:shared", "meet:measured", "zoom:not_measurable", "discord:not_measurable"}) {
		t.Errorf("apps without the relay: %v", apps)
	}
}

// TestCallsStates: "not enough data", "server not answering" and "blocked"
// are told apart; non-timeout failures are unknown, never loss; the top
// state is the worst of the path verdict, the UDP verdicts and the path's
// score, with its cause.
func TestCallsStates(t *testing.T) {
	// A fixed clock: which bursts lose a request (ts%25) and how full the
	// last minute is must not depend on when the test runs.
	now := time.Date(2026, 10, 9, 12, 30, 30, 0, time.UTC)
	ok := burst{rtt: 20, jitter: 2, answered: 5}
	cases := []struct {
		name    string
		fill    func(string, time.Time) burst
		verdict model.VerdictKind
		top     string
		cause   string // source:kind or source:factor
		path    string
		apps    map[string]string
		check   func(t *testing.T, c callsResp)
	}{
		{
			name: "not enough data",
			fill: func(_ string, ts time.Time) burst {
				if now.Sub(ts) > 10*time.Second {
					return burst{skip: true}
				}
				return ok
			},
			top: "not_enough_data", path: "not_enough_data", apps: map[string]string{"meet": "not_enough_data", "teams": "not_enough_data"},
		},
		{
			name: "server not answering",
			fill: func(name string, ts time.Time) burst {
				switch name {
				case "Meet-STUN":
					return burst{} // every request times out
				case "Teams-Relay":
					return burst{reason: model.ReasonRefused} // refused: unknown, not loss
				}
				return ok
			},
			// The apps whose own servers are down fall back to the shared
			// call path, like an app without servers of its own.
			top: "good", path: "good",
			check: func(t *testing.T, c callsResp) {
				for _, app := range []string{"meet", "teams"} {
					if i := c.app(app); i < 0 || c.Apps[i].UDP != "shared" || !c.Apps[i].OwnDown || c.Apps[i].State != "" {
						t.Errorf("app %s: %+v, want shared", app, c.Apps)
					}
				}
				p := c.Path
				if *p.Loss != 0 || p.Unknown != 0 {
					t.Errorf("path loss %v unknown %d: dead servers are left out", *p.Loss, p.Unknown)
				}
				for _, x := range p.Targets {
					if x.NotAnswering != (x.Name == "Meet-STUN" || x.Name == "Teams-Relay") {
						t.Errorf("target %+v", x)
					}
					if x.Name == "Teams-Relay" && (x.Samples != 0 || x.Unknown < 100) {
						t.Errorf("refused bursts are unknown, not loss: %+v", x)
					}
				}
			},
		},
		{
			name: "unknown failures are not loss",
			fill: func(name string, ts time.Time) burst {
				if name == "Cloudflare-STUN" && ts.Unix()%10 == 0 {
					return burst{reason: model.ReasonDNS}
				}
				return ok
			},
			top: "good", path: "good",
			check: func(t *testing.T, c callsResp) {
				if *c.Path.Loss != 0 || c.Path.Unknown == 0 {
					t.Errorf("loss %v unknown %d", *c.Path.Loss, c.Path.Unknown)
				}
			},
		},
		{
			name: "blocked", fill: func(string, time.Time) burst { return burst{} }, verdict: model.VerdictUDPBlocked,
			top: "blocked", cause: "udp:udp_blocked", path: "blocked", apps: map[string]string{"meet": "blocked", "teams": "blocked"},
		},
		{
			name: "STUN ports blocked", verdict: model.VerdictSTUNBlocked,
			fill: func(name string, ts time.Time) burst {
				if name == "Cloudflare-STUN" {
					return ok // stands in for UDP 53
				}
				return burst{}
			},
			top: "blocked", cause: "udp:stun_blocked", path: "blocked",
		},
		{
			name: "all lost without a UDP verdict", fill: func(string, time.Time) burst { return burst{} },
			top: "unusable", cause: "path:loss", path: "unusable", apps: map[string]string{"meet": "unusable"},
		},
		{
			name: "ISP verdict over a good path", verdict: model.VerdictISP,
			top: "poor", cause: "verdict:isp", path: "good",
		},
		{
			name: "DNS verdict", verdict: model.VerdictDNS,
			top: "fair", cause: "verdict:dns", path: "good",
		},
		{
			name: "UDP degraded", verdict: model.VerdictUDPDegraded,
			top: "poor", cause: "udp:udp_degraded", path: "good",
		},
		{
			name: "no network ties with an unusable path: the verdict names the cause", verdict: model.VerdictNoNetwork,
			fill: func(string, time.Time) burst { return burst{} },
			top:  "unusable", cause: "verdict:no_network", path: "unusable",
		},
		{
			name: "lossy path beats a DNS verdict", verdict: model.VerdictDNS,
			fill: func(name string, ts time.Time) burst {
				if now.Sub(ts) < 3*time.Minute && name != "Twilio-jp1" {
					return burst{rtt: 20, jitter: 2, answered: 4} // 20% loss
				}
				return ok
			},
			top: "unusable", cause: "path:loss", path: "unusable",
			check: func(t *testing.T, c callsResp) {
				w := c.Path.Worst
				if w == nil || now.UnixMilli()-w.At > (4*time.Minute).Milliseconds() || math.Abs(*w.Loss-0.2) > 0.001 {
					t.Errorf("worst minute %+v", w)
				}
				// A few good minutes do not hide the bad ones: the state is the worst minute's.
				var good int
				for _, s := range c.Path.Minutes.State {
					if s != nil && *s == "good" {
						good++
					}
				}
				if good < 5 {
					t.Errorf("%d good minutes", good)
				}
			},
		},
		{
			name: "one lossy app: the top state is never better than an app",
			fill: func(name string, ts time.Time) burst {
				if name == "Teams-Relay" {
					return burst{rtt: 30, jitter: 2, answered: 4} // 20% loss
				}
				return ok
			},
			top: "unusable", cause: "app:teams:loss", path: "good", apps: map[string]string{"teams": "unusable", "meet": "good"},
		},
		{
			// Moderate jitter and a little loss (two of the four servers
			// lose a request in every 5th burst: the path's median loss is
			// 2%, at most 3.3% in a partial minute): the MOS alone is Good,
			// the jitter cap makes it Fair.
			name: "jittery path",
			fill: func(target string, ts time.Time) burst {
				b := burst{rtt: 30, jitter: 35, answered: 5}
				if (target == "Meet-STUN" || target == "Cloudflare-STUN") && ts.Unix()%25 == 0 {
					b.answered = 4
				}
				return b
			},
			top: "fair", cause: "path:jitter", path: "fair",
		},
		{
			name: "very jittery path",
			fill: func(string, time.Time) burst { return burst{rtt: 30, jitter: 60, answered: 5} },
			top:  "poor", cause: "path:jitter", path: "poor",
		},
		{
			name: "unusable jitter",
			fill: func(string, time.Time) burst { return burst{rtt: 30, jitter: 130, answered: 5} },
			top:  "unusable", cause: "path:jitter", path: "unusable",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, d, fv := callsFixture(t, now, true, tc.fill)
			if tc.verdict != "" {
				fv.v = model.Verdict{Kind: tc.verdict, Since: now.Add(-time.Hour), Summary: "Something."}
			}
			h := Local(d, LocalOptions{Addr: "127.0.0.1:3000"}).(*localHandler)
			h.now = func() time.Time { return now }
			c, _ := getCalls(t, h, "/api/calls?from=now-10m", localHdr)
			if c.State != tc.top {
				t.Errorf("top state %q, want %q", c.State, tc.top)
			}
			cause := ""
			if c.Cause != nil {
				cause = c.Cause.Source + ":" + c.Cause.Kind + c.Cause.Factor
				if c.Cause.App != "" {
					cause = c.Cause.Source + ":" + c.Cause.App + ":" + c.Cause.Factor
				}
			}
			// Invariant: the top state is never better than a measured
			// app's band (or the path's).
			bands := map[string]int{"unusable": 0, "poor": 1, "fair": 2, "good": 3}
			top, topBand := bands[c.State]
			if c.State == "blocked" {
				top, topBand = -1, true
			}
			for _, a := range c.Apps {
				if r, ok := bands[a.State]; ok && (!topBand || r < top) {
					t.Errorf("top state %q is better than %s's %q", c.State, a.App, a.State)
				}
			}
			if cause != tc.cause {
				t.Errorf("cause %q, want %q", cause, tc.cause)
			}
			if c.Path == nil || c.Path.State != tc.path {
				t.Errorf("path %+v, want state %q", c.Path, tc.path)
			}
			for app, st := range tc.apps {
				if i := c.app(app); i < 0 || c.Apps[i].State != st {
					t.Errorf("app %s: %+v, want %q", app, c.Apps, st)
				}
			}
			if tc.check != nil {
				tc.check(t, c)
			}
		})
	}
}

func TestCallsParams(t *testing.T) {
	now := time.Now()
	_, d, _ := callsFixture(t, now, true, nil)
	h := Local(d, LocalOptions{Addr: "127.0.0.1:3000"})
	c, _ := getCalls(t, h, "/api/calls?from=now-7d", localHdr)
	if !c.Capped || c.To-c.From != CallsMaxRange.Milliseconds() || c.Step != 60000 {
		t.Errorf("7 days: capped %v %d..%d step %d", c.Capped, c.From, c.To, c.Step)
	}
	c, _ = getCalls(t, h, "/api/calls?from=now-1m", localHdr)
	if c.To-c.From != CallsMinRange.Milliseconds() || c.Step != 60000 {
		t.Errorf("1 minute: %d..%d step %d", c.From, c.To, c.Step)
	}
	c, _ = getCalls(t, h, fmt.Sprintf("/api/calls?from=%d&to=%d", now.Add(-2*time.Hour).UnixMilli(), now.Add(-time.Hour).UnixMilli()), localHdr)
	if c.Path == nil || c.Path.State != "not_enough_data" || c.Path.Samples != 0 {
		t.Errorf("before the data: %+v", c.Path)
	}
	for _, q := range []string{"?kind=tcp", "?from=x", "?from=now-1h&from=now-2h", "?from=now&to=now-1h"} {
		if w := do(h, "GET", "/api/calls"+q, localHdr); w.Code != 400 {
			t.Errorf("%s = %d", q, w.Code)
		}
	}
	if w := do(h, "POST", "/api/calls", localHdr); w.Code != 405 {
		t.Errorf("POST = %d", w.Code)
	}
}
