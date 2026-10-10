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

// callsProfile: four call-path STUN targets (Meet and the opt-in Teams relay
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
		Source, Kind, Summary, Factor string
	}
	Path *callsGroupResp
	Apps []callsAppResp
}

type callsAppResp struct {
	App    string `json:"app"`
	UDP    string `json:"udp"`
	Region string `json:"region"`
	callsGroupResp
	WebMs      *float64 `json:"web_ms"`
	WebLoss    *float64 `json:"web_loss"`
	WebSamples int      `json:"web_samples"`
}

func (c callsResp) app(id string) int {
	return slices.IndexFunc(c.Apps, func(a callsAppResp) bool { return a.App == id })
}

func TestCallsGood(t *testing.T) {
	now := time.Now()
	cs, d, _ := callsFixture(t, now, true, nil)
	h := Local(d, LocalOptions{Addr: "127.0.0.1:3000"})
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

	// Without the opt-in Teams relay there is no Teams row (the UI says
	// how to add it).
	_, d, _ = callsFixture(t, now, false, nil)
	c, _ = getCalls(t, Local(d, LocalOptions{Addr: "127.0.0.1:3000"}), "/api/calls", localHdr)
	if c.app("teams") >= 0 || c.app("meet") < 0 || c.State != "good" {
		t.Errorf("without the relay: %+v", c.Apps)
	}
}

// TestCallsStates: "not enough data", "server not answering" and "blocked"
// are told apart; non-timeout failures are unknown, never loss; the top
// state is the worst of the path verdict, the UDP verdicts and the path's
// score, with its cause.
func TestCallsStates(t *testing.T) {
	now := time.Now()
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
			top: "good", path: "good", apps: map[string]string{"meet": "server_not_answering", "teams": "server_not_answering"},
			check: func(t *testing.T, c callsResp) {
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
			name: "jittery path",
			fill: func(string, time.Time) burst { return burst{rtt: 30, jitter: 130, answered: 5} },
			top:  "fair", cause: "path:jitter", path: "fair",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, d, fv := callsFixture(t, now, true, tc.fill)
			if tc.verdict != "" {
				fv.v = model.Verdict{Kind: tc.verdict, Since: now.Add(-time.Hour), Summary: "Something."}
			}
			c, _ := getCalls(t, Local(d, LocalOptions{Addr: "127.0.0.1:3000"}), "/api/calls?from=now-10m", localHdr)
			if c.State != tc.top {
				t.Errorf("top state %q, want %q", c.State, tc.top)
			}
			cause := ""
			if c.Cause != nil {
				cause = c.Cause.Source + ":" + c.Cause.Kind + c.Cause.Factor
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

// TestCallsPublic: the public link serves /api/calls GET-only, cached per
// minute-rounded window, without target names or the relay region.
func TestCallsPublic(t *testing.T) {
	now := time.Now()
	cs, d, _ := callsFixture(t, now, true, func(name string, ts time.Time) burst {
		if name == "Meet-STUN" {
			return burst{}
		}
		return burst{rtt: 20, jitter: 2, answered: 5}
	})
	h := Public(d, testSecret)
	base := "/s/" + testSecret + "/api/calls"
	var body string
	for i := range 3 {
		_, body = getCalls(t, h, base+"?from=now-15m", pubHdr(i))
	}
	if n := cs.calls.Load(); n != 1 {
		t.Errorf("3 identical requests made %d store calls, want 1", n)
	}
	m := now.Add(-time.Hour).Truncate(time.Minute)
	for i, off := range []time.Duration{time.Second, 31 * time.Second} {
		q := fmt.Sprintf("?from=%d&to=%d", m.Add(off).UnixMilli(), m.Add(30*time.Minute+off).UnixMilli())
		c, _ := getCalls(t, h, base+q, pubHdr(10+i))
		if c.From != m.UnixMilli() || c.To != m.Add(31*time.Minute).UnixMilli() {
			t.Errorf("bounds not rounded to the minute: %d..%d", c.From, c.To)
		}
	}
	if n := cs.calls.Load(); n != 2 {
		t.Errorf("absolute ranges in one minute: %d store calls in total, want 2", n)
	}
	c, _ := getCalls(t, h, base+"?from=now-15m", pubHdr(20))
	if c.Path == nil || c.Path.State != "good" || len(c.Path.Targets) != 0 || c.app("teams") < 0 || c.Apps[c.app("teams")].Region != "" ||
		c.Apps[c.app("meet")].State != "server_not_answering" {
		t.Errorf("public calls: %+v", c)
	}
	for _, leak := range []string{"swedencentral", "region", "Meet-STUN", "Teams-Relay", "Twilio", "Cloudflare", "Zoom", "targets", "example.com"} {
		if strings.Contains(body, leak) {
			t.Errorf("public /api/calls contains %q:\n%s", leak, body)
		}
	}
	if m := leakRE.FindString(body); m != "" {
		t.Errorf("public /api/calls leaks %q", m)
	}
	for i, method := range []string{"POST", "PUT", "DELETE"} {
		if w := do(h, method, base, pubHdr(30+i)); w.Code != 405 {
			t.Errorf("%s = %d", method, w.Code)
		}
	}
}
