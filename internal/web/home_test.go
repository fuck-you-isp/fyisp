package web

import (
	"context"
	"encoding/json"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/store"
)

// homeProfile is a small Home profile: the path, two STUN targets, three
// call apps' websites, and work, web and one "other" target.
func homeProfile() *model.Profile {
	icmp := []model.ProbeKind{model.KindICMP}
	path := func(name, layer string) model.Target {
		return model.Target{Name: name, Host: strings.ToLower(name) + ".example.com", Group: "path", Layer: layer, Kinds: icmp, Interval: 15 * time.Second}
	}
	udp := func(name string, tags ...string) model.Target {
		return model.Target{Name: name, Host: strings.ToLower(name) + ".example.com", Port: 3478, Group: "calls",
			Kinds: []model.ProbeKind{model.KindUDP}, Interval: 5 * time.Second, Geo: "global", Tags: append([]string{"udp-path"}, tags...)}
	}
	tcp := func(name, group string, tags ...string) model.Target {
		return model.Target{Name: name, Host: strings.ToLower(name) + ".example.com", Group: group, Interval: 15 * time.Second, Tags: tags}
	}
	return &model.Profile{Name: "default", Targets: []model.Target{
		path("Gateway", model.LayerGateway), path("ISP edge", model.LayerEdge), path("Cloudflare DNS", model.LayerAnycast),
		udp("Meet-STUN", "app:meet"), udp("Cloudflare-STUN"),
		tcp("Google-Meet", "apps", "app:meet"), tcp("Microsoft-Teams", "apps", "app:teams"), tcp("Zoom", "apps", "app:zoom"),
		tcp("M365-Outlook", "work", "home:work"), tcp("Google-Gmail", "work", "home:work"), tcp("Google-Docs", "work", "home:work"),
		tcp("YouTube", "web", "home:web"), tcp("Netflix", "web", "home:web"), tcp("CDN-Cloudflare", "web", "home:web"),
		tcp("AWS-me-south-1", "aws"),
	}}
}

// homeFixture fills 30 minutes before now: healthy, except the targets in
// lost (every TCP/ICMP probe lost) and bad (bursts for the call path).
func homeFixture(t *testing.T, p *model.Profile, now time.Time, lost []string, choppy bool) (*server, *countingStore, *fakeVerdict) {
	t.Helper()
	f := store.NewFake()
	for _, tg := range p.Targets {
		for _, k := range tg.ProbeKinds() {
			iv := tg.Interval
			if k == model.KindICMP {
				iv = 5 * time.Second
			}
			for ts := now.Add(-30 * time.Minute).Truncate(iv); ts.Before(now); ts = ts.Add(iv) {
				if k == model.KindUDP {
					b := burst{rtt: 20, jitter: 2, answered: 5}
					if choppy && ts.After(now.Add(-4*time.Minute)) {
						b = burst{rtt: 60, jitter: 40, answered: 4}
					}
					writeBurst(f, tg.Name, ts, b)
					continue
				}
				s := model.Sample{Key: model.SeriesKey{Target: tg.Name, Kind: k}, Slot: ts, RTT: 20 * time.Millisecond}
				if slices.Contains(lost, tg.Name) {
					s.Lost, s.Reason, s.RTT = true, model.ReasonTimeout, 0
				}
				f.Observe(s)
			}
		}
	}
	cs := &countingStore{Reader: f}
	fv := &fakeVerdict{v: model.Verdict{Kind: model.VerdictOK, Since: now.Add(-time.Hour)}}
	d := Deps{Profile: func() *model.Profile { return p }, Store: cs, Verdict: fv,
		Status: func() Status { return Status{Version: "v9"} }}
	s := newServer(d, false, "")
	s.now = func() time.Time { return now }
	return s, cs, fv
}

func TestHomeParams(t *testing.T) {
	for _, tc := range []struct {
		q, part, span, voice string
		tz                   int
		public               bool
		bad                  bool
	}{
		{q: "", part: "live", span: "d", voice: "you"},
		{q: "part=history&span=w&tz=120", part: "history", span: "w", voice: "you", tz: 120},
		{q: "part=history&tz=-187", part: "history", span: "d", voice: "you", tz: -180},
		{q: "part=history&tz=338", part: "history", span: "d", voice: "you", tz: 345},
		{q: "part=live&voice=public", part: "live", span: "d", voice: "public"},
		{q: "part=live&voice=you", part: "live", span: "d", voice: "public", public: true},
		{q: "part=live&span=w", bad: true},
		{q: "part=day", bad: true},
		{q: "part=history&span=m", bad: true},
		{q: "tz=900", bad: true},
		{q: "tz=abc", bad: true},
		{q: "voice=them", bad: true},
		{q: "from=now-1h", bad: true},
		{q: "part=at&at=1760000000000", part: "at", span: "d", voice: "you"},
		{q: "part=at", bad: true},
		{q: "part=at&at=soon", bad: true},
		{q: "part=live&at=1760000000000", bad: true},
	} {
		q, _ := url.ParseQuery(tc.q)
		p, err := parseHome(q, tc.public)
		if (err != nil) != tc.bad {
			t.Errorf("%q: err %v", tc.q, err)
			continue
		}
		if !tc.bad && (p.part != tc.part || p.span != tc.span || p.voice != tc.voice || p.tz != tc.tz) {
			t.Errorf("%q: %+v", tc.q, p)
		}
	}
}

func TestHomeLiveGood(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	s, _, _ := homeFixture(t, homeProfile(), now, nil, false)
	l, err := s.homeLive(context.Background(), voiceYou, now)
	if err != nil {
		t.Fatal(err)
	}
	a := l.Answer
	if a.Level != levelGood || a.Who != whoNone || a.Calls != "good" || len(a.Try) != 0 || a.Fact != "" || a.Headline != "Your internet is fine for calls and work" {
		t.Errorf("answer %+v", a)
	}
	var ids []string
	for _, x := range l.Tiles {
		ids = append(ids, x.ID+":"+x.State)
	}
	if !slices.Equal(ids, []string{"calls:good", "work:working", "web:working", "other:working"}) {
		t.Errorf("tiles %v", ids)
	}
	var path []string
	for _, n := range l.Path {
		path = append(path, n.ID+":"+n.State)
	}
	if !slices.Equal(path, []string{"device:ok", "router:ok", "provider:ok", "internet:ok", "apps:ok"}) {
		t.Errorf("path %v", path)
	}
	// Teams without its relay: reachable, its calls take the path.
	i := slices.IndexFunc(l.Apps, func(x HomeApp) bool { return x.App == "teams" })
	if i < 0 || l.Apps[i].State != "reachable" || l.Apps[i].Note != "Calls use your call path (Good)." {
		t.Errorf("apps %+v", l.Apps)
	}
	if l.Calls == nil || l.Calls.Delay != "very_short" || l.Calls.Dropouts != "none" || l.Calls.Smoothness != "steady" || l.Calls.WorstAt != 0 {
		t.Errorf("calls %+v", l.Calls)
	}
	if l.Other != nil || l.Steps != nil {
		t.Errorf("other %+v steps %+v", l.Other, l.Steps)
	}
}

// A cloud region that doesn't answer is not Home's business: the answer
// stays good, with a muted line. One work app down out of three is
// "mostly working", a note on its tile.
func TestHomeLiveRelevance(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	s, _, fv := homeFixture(t, homeProfile(), now, []string{"AWS-me-south-1", "M365-Outlook"}, false)
	fv.v = model.Verdict{Kind: model.VerdictService, Since: now.Add(-10 * time.Minute), Targets: []string{"AWS-me-south-1"}}
	l, err := s.homeLive(context.Background(), voiceYou, now)
	if err != nil {
		t.Fatal(err)
	}
	if l.Answer.Level != levelGood || l.Answer.Who != whoNone || l.Answer.Since != 0 {
		t.Errorf("answer %+v", l.Answer)
	}
	if l.Other == nil || l.Other.Affected != 1 || l.Other.Line != "1 other service isn't working well from here." {
		t.Errorf("other %+v", l.Other)
	}
	w := l.Tiles[1]
	if w.ID != tileWork || w.State != tileMostly || !slices.Equal(w.Affected, []string{"M365 Outlook"}) || w.OK != 2 ||
		w.Note != "M365 Outlook isn't answering from here. That's usually a problem on their side." {
		t.Errorf("work %+v", w)
	}

	// The same verdict about one work app out of three is a hint.
	fv.v.Targets = []string{"M365-Outlook"}
	l, _ = s.homeLive(context.Background(), voiceYou, now)
	if a := l.Answer; a.Level != levelGood || a.Who != whoNone || a.Hint != "M365 Outlook isn't answering from here. That's most likely a problem on their side." {
		t.Errorf("answer %+v", a)
	}

	// Two of three raise it.
	s, _, fv = homeFixture(t, homeProfile(), now, []string{"M365-Outlook", "Google-Gmail"}, false)
	fv.v = model.Verdict{Kind: model.VerdictService, Since: now.Add(-10 * time.Minute), Targets: []string{"M365-Outlook", "Google-Gmail"}}
	l, _ = s.homeLive(context.Background(), voiceYou, now)
	if a := l.Answer; a.Level != levelBad || a.Who != whoApp || a.App != "M365 Outlook" || a.Headline != "M365 Outlook and Google Gmail aren't answering" || a.Since == 0 {
		t.Errorf("answer %+v", a)
	}
}

// With a profile that has nothing for the Home tiles (e.g. clouds), Other
// services is the user's set and escalates.
func TestHomeLiveOnlyOther(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	p := &model.Profile{Name: "clouds"}
	for _, r := range []string{"us-east-1", "eu-west-1", "ap-south-1", "me-south-1"} {
		p.Targets = append(p.Targets, model.Target{Name: "AWS-" + r, Host: r + ".example.com", Group: "aws", Interval: 15 * time.Second})
	}
	s, _, fv := homeFixture(t, p, now, []string{"AWS-ap-south-1", "AWS-me-south-1"}, false)
	fv.v = model.Verdict{Kind: model.VerdictService, Since: now.Add(-5 * time.Minute), Targets: []string{"AWS-ap-south-1", "AWS-me-south-1"}}
	l, err := s.homeLive(context.Background(), voiceYou, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Tiles) != 1 || l.Tiles[0].ID != tileOther || l.Tiles[0].State != tileDown {
		t.Errorf("tiles %+v", l.Tiles)
	}
	if a := l.Answer; a.Level != levelBad || a.Who != whoApp || a.Headline != "Some services aren't answering" {
		t.Errorf("answer %+v", a)
	}
	if l.Other != nil || l.Calls != nil || len(l.Apps) != 0 {
		t.Errorf("other %+v calls %+v apps %+v", l.Other, l.Calls, l.Apps)
	}
}

func TestHomeLiveChoppy(t *testing.T) {
	now := time.Now().Truncate(time.Minute).Add(30 * time.Second)
	s, _, fv := homeFixture(t, homeProfile(), now, nil, true)
	fv.v = model.Verdict{Kind: model.VerdictUDPDegraded, Since: now.Add(-3 * time.Minute)}
	l, err := s.homeLive(context.Background(), voiceYou, now)
	if err != nil {
		t.Fatal(err)
	}
	a := l.Answer
	if a.Level != levelBad || a.Who != whoCallTraffic || (a.Calls != "poor" && a.Calls != "unusable") || a.Headline != copyFor(voiceYou).callHead(a.Calls, false) ||
		a.Fact == "" || a.FactAt == 0 || len(a.Try) != 3 || !strings.HasPrefix(a.Cause, "Browsing looks fine") {
		t.Errorf("answer %+v", a)
	}
	if l.Calls == nil || l.Calls.Smoothness != "uneven" || l.Calls.WorstAt == 0 {
		t.Errorf("calls %+v", l.Calls)
	}
	// The public voice: third person, nothing to try.
	l, _ = s.homeLive(context.Background(), voicePublic, now)
	if a := l.Answer; len(a.Try) != 0 || strings.Contains(a.Cause, "your") || !strings.Contains(a.Cause, "call traffic") {
		t.Errorf("public answer %+v", a)
	}
}

func TestHomeLiveWarming(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	s, _, fv := homeFixture(t, homeProfile(), now, nil, false)
	fv.v = model.Verdict{Kind: model.VerdictWarmingUp}
	l, err := s.homeLive(context.Background(), voiceYou, now)
	if err != nil {
		t.Fatal(err)
	}
	if l.Answer.Level != levelChecking || l.Answer.Headline != "Checking your connection…" || l.Calls != nil || l.Answer.Fact != "" {
		t.Errorf("answer %+v calls %+v", l.Answer, l.Calls)
	}
	if len(l.Steps) != 4 || !l.Steps[0].Done || !l.Steps[3].Done {
		t.Errorf("steps %+v", l.Steps)
	}
}

// homeIn builds an answerIn for the table test.
func homeIn(p *model.Profile, v model.Verdict, own string, tiles ...HomeTile) answerIn {
	names := map[string]string{}
	for _, t := range p.Targets {
		names[t.Name] = homeName(t)
	}
	c := &callsJSON{State: own, Path: &callsGroup{State: own}}
	if own == "poor" {
		c.Cause = &callsCause{Source: "path", Factor: "loss"}
		c.Path.Factor = "loss"
		c.Path.Worst = &callsMinute{At: 1, State: "poor", MOS: 3, Loss: 0.14}
	}
	return answerIn{v: v, own: own, calls: c, tiles: tiles, hs: newHomeSet(p), names: names}
}

func TestHomeAnswer(t *testing.T) {
	p := homeProfile()
	ev := func(kv ...any) map[string]float64 {
		m := map[string]float64{}
		for i := 0; i < len(kv); i += 2 {
			m[kv[i].(string)] = kv[i+1].(float64)
		}
		return m
	}
	down := HomeTile{ID: tileWeb, Title: "Websites", State: tileDown, Level: levelBad, Affected: []string{"YouTube", "Netflix"},
		Note: "YouTube and Netflix aren't answering from here."}
	for _, tc := range []struct {
		name                  string
		v                     model.Verdict
		own                   string
		tiles                 []HomeTile
		level, who, conf, hdl string
	}{
		{"ok", model.Verdict{Kind: model.VerdictOK}, "good", nil, levelGood, whoNone, "", "Your internet is fine for calls and work"},
		{"no verdict", model.Verdict{Kind: VerdictUnknown}, "good", nil, levelGood, whoNone, "", "Your internet is fine for calls and work"},
		{"isp, apps losing", model.Verdict{Kind: model.VerdictISP, Evidence: ev("services_loss", 0.2)}, "good", nil,
			levelBad, whoProvider, confSure, "Your internet provider is having trouble"},
		{"isp, only the edge", model.Verdict{Kind: model.VerdictISP, Evidence: ev("edge_loss", 0.3, "services_loss", 0.0)}, "good", nil,
			levelInfo, whoProvider, confWeak, "Your internet is fine for calls and work"},
		{"isp, a little loss", model.Verdict{Kind: model.VerdictISP, Evidence: ev("services_loss", 0.02)}, "good", nil,
			levelWarn, whoProvider, confLikely, "Your internet provider is having trouble"},
		{"lan, calls choppy", model.Verdict{Kind: model.VerdictLAN, Evidence: ev("gateway_loss", 0.3)}, "poor", nil,
			levelBad, whoHome, confSure, "Calls may be choppy"},
		{"upstream", model.Verdict{Kind: model.VerdictUpstream, Evidence: ev("services_loss", 0.6)}, "good", nil,
			levelBad, whoBeyond, confSure, "Many websites and apps are hard to reach"},
		{"dns", model.Verdict{Kind: model.VerdictDNS}, "good", nil, levelWarn, whoDNS, confLikely, "Websites and apps may not open"},
		{"offline", model.Verdict{Kind: model.VerdictNoNetwork}, "unusable", nil, levelBad, whoOffline, "", "Calls will break up"},
		{"quiet block", model.Verdict{Kind: model.VerdictUDPBlocked, Quiet: true}, "blocked", nil,
			levelInfo, whoCallTraffic, "", "This network always blocks the fast connection calls use"},
		{"stun block", model.Verdict{Kind: model.VerdictSTUNBlocked}, "blocked", nil,
			levelWarn, whoCallTraffic, "", "This network blocks some of the connections calls use"},
		{"degraded", model.Verdict{Kind: model.VerdictUDPDegraded}, "fair", nil, levelWarn, whoCallTraffic, "", "Mostly fine for calls"},
		{"path only", model.Verdict{Kind: model.VerdictOK}, "poor", nil, levelBad, whoCallTraffic, "", "Calls may be choppy"},
		{"service, other", model.Verdict{Kind: model.VerdictService, Targets: []string{"AWS-me-south-1"}}, "good", nil,
			levelGood, whoNone, "", "Your internet is fine for calls and work"},
		{"service, two apps", model.Verdict{Kind: model.VerdictService, Targets: []string{"YouTube", "Netflix", "AWS-me-south-1"}}, "good",
			[]HomeTile{down}, levelBad, whoApp, "", "YouTube and Netflix aren't answering"},
		{"tile down, no verdict", model.Verdict{Kind: model.VerdictOK}, "good", []HomeTile{down}, levelBad, whoApp, "", "Some websites aren't loading"},
	} {
		a := homeAnswerOf(homeIn(p, tc.v, tc.own, tc.tiles...), copyFor(voiceYou))
		if a.Level != tc.level || a.Who != tc.who || a.Confidence != tc.conf || a.Headline != tc.hdl {
			t.Errorf("%s: level %s who %s conf %s headline %q (cause %q)", tc.name, a.Level, a.Who, a.Confidence, a.Headline, a.Cause)
		}
		if (a.Level == levelGood) != (len(a.Try) == 0) && a.Confidence != confWeak && a.Who != whoApp {
			t.Errorf("%s: try %v", tc.name, a.Try)
		}
		if a.Announce == "" || a.Cause == "" {
			t.Errorf("%s: %+v", tc.name, a)
		}
	}
}

func TestHomeTCPTile(t *testing.T) {
	ts := []model.Target{{Name: "A"}, {Name: "B"}, {Name: "C"}, {Name: "D-x"}}
	names := map[string]string{"A": "A", "B": "B", "C": "C", "D-x": "D x"}
	row := func(n, st string) overviewRow { return overviewRow{Target: n, State: st} }
	for _, tc := range []struct {
		states []string
		want   string
	}{
		{[]string{"ok", "ok", "ok", "ok"}, tileWorking},
		{[]string{"ok", "ok", "ok", "failing"}, tileMostly},
		{[]string{"ok", "ok", "slow", "failing"}, tileSlow},
		{[]string{"ok", "ok", "failing", "failing"}, tileDown},
		{[]string{"unmeasured", "unmeasured", "unmeasured", "ok"}, tileChecking},
	} {
		var rows []overviewRow
		for i, st := range tc.states {
			rows = append(rows, row(ts[i].Name, st))
		}
		got := tcpTile(tileWork, ts, rows, names, copyFor(voiceYou))
		if got.State != tc.want || got.Total != 4 || len(got.Names) != 3 || got.More != 1 {
			t.Errorf("%v: %+v", tc.states, got)
		}
	}
	// A set of two: one failing out of two is down, not "mostly".
	got := tcpTile(tileWeb, ts[:2], []overviewRow{row("A", "ok"), row("B", "failing")}, names, copyFor(voiceYou))
	if got.State != tileSlow && got.State != tileDown {
		t.Errorf("two: %+v", got)
	}
}

// historyFixture: the gateway probed every minute for 3 days, except
// 00:00-06:00 local two days ago (fyisp off), and three incidents.
func historyFixture(t *testing.T, now time.Time, loc *time.Location) (*server, *countingStore) {
	t.Helper()
	p := homeProfile()
	p.Targets[0].Interval = time.Minute
	f := store.NewFake()
	ln := now.In(loc)
	today := time.Date(ln.Year(), ln.Month(), ln.Day(), 0, 0, 0, 0, loc)
	offFrom, offTo := today.AddDate(0, 0, -2), today.AddDate(0, 0, -2).Add(6*time.Hour)
	for ts := today.AddDate(0, 0, -3).Add(12 * time.Hour); ts.Before(now); ts = ts.Add(time.Minute) {
		if !ts.Before(offFrom) && ts.Before(offTo) {
			continue
		}
		f.Observe(model.Sample{Key: model.SeriesKey{Target: "Gateway", Kind: model.KindICMP}, Slot: ts, RTT: time.Millisecond})
	}
	cs := &countingStore{Reader: f}
	fv := &fakeVerdict{v: model.Verdict{Kind: model.VerdictOK}, list: []model.Incident{ // newest first
		{ID: 3, Start: now.Add(-20 * time.Minute), Kind: model.VerdictLAN, PeakLoss: 0.4},
		{ID: 2, Start: today.AddDate(0, 0, -1).Add(10 * time.Hour), End: today.AddDate(0, 0, -1).Add(11 * time.Hour), Kind: model.VerdictService, Targets: []string{"AWS-me-south-1"}},
		{ID: 1, Start: today.AddDate(0, 0, -1).Add(-30 * time.Minute), End: today.AddDate(0, 0, -1).Add(15 * time.Minute), Kind: model.VerdictISP, PeakLoss: 1},
	}}
	d := Deps{Profile: func() *model.Profile { return p }, Store: cs, Verdict: fv, Status: func() Status { return Status{} }}
	s := newServer(d, false, "")
	s.now = func() time.Time { return now }
	return s, cs
}

func TestHomeHistory(t *testing.T) {
	loc := time.FixedZone("", 120*60)
	now := time.Date(2026, 10, 9, 14, 30, 0, 0, loc) // a Friday
	s, _ := historyFixture(t, now, loc)
	h, err := s.homeHistory(context.Background(), homeParams{part: "history", span: "w", voice: voiceYou, tz: 120}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.Days) != 7 || h.Days[6].Date != "2026-10-09" || h.Days[0].Date != "2026-10-03" || h.Days[6].To != now.UnixMilli() {
		t.Fatalf("days %+v", h.Days)
	}
	if !h.Days[0].NoRecord || h.Days[5].WorkFrom == 0 || h.Days[0].WorkFrom != 0 { // the 3rd is a Saturday
		t.Errorf("day 0 %+v day 5 %+v", h.Days[0], h.Days[5])
	}
	// The ISP incident crosses midnight: on both days, clipped.
	d4, d5 := h.Days[4], h.Days[5]
	if len(d4.Runs) != 1 || d4.Runs[0].Who != whoProvider || d4.Runs[0].To != d4.To || d4.Runs[0].Incident != 1 ||
		len(d5.Runs) != 1 || d5.Runs[0].From != d5.From || d5.Runs[0].Label != "Your provider" {
		t.Errorf("runs %+v / %+v", d4.Runs, d5.Runs)
	}
	// The cloud-region incident is left out; the ongoing LAN one is today.
	today := h.Days[6]
	if len(today.Runs) != 1 || today.Runs[0].Who != whoHome || !today.Runs[0].Ongoing || today.Runs[0].To != now.UnixMilli() {
		t.Errorf("today %+v", today.Runs)
	}
	// Off: 00:00-06:00 two days ago, hour-granular; nothing before the
	// first sample.
	d3 := h.Days[4-1]
	if len(d3.Off) != 0 {
		t.Errorf("day 3 off %+v", d3.Off)
	}
	d4off := h.Days[4].Off
	if len(d4off) != 1 || d4off[0].From != h.Days[4].From || d4off[0].To != h.Days[4].From+6*3600e3 {
		t.Errorf("off %+v (day from %d)", d4off, h.Days[4].From)
	}
	if h.Counts.Problems != 2 || h.Counts.Home != 1 || h.Counts.Provider != 1 || len(h.Problems) != 2 || h.Problems[0].ID != 3 || !h.Problems[0].Ongoing {
		t.Errorf("counts %+v problems %+v", h.Counts, h.Problems)
	}
	if h.Summary != "2 problems this week: 1 from your internet provider and 1 in your home network." {
		t.Errorf("summary %q", h.Summary)
	}
	if h.FirstData == 0 || h.Problems[1].Where != "Your provider's network (past your router)" {
		t.Errorf("first %d where %q", h.FirstData, h.Problems[1].Where)
	}

	h, _ = s.homeHistory(context.Background(), homeParams{part: "history", span: "d", voice: voicePublic, tz: 120}, now)
	if len(h.Days) != 1 || h.Summary != "1 problem today, in the home network." || h.Problems[0].Where != "Home network (Wi-Fi or router)" {
		t.Errorf("public today: %q %+v", h.Summary, h.Problems)
	}
}

// The public route: always the public voice, history cached, no relay
// region, no "your".
func TestHomePublic(t *testing.T) {
	now := time.Now()
	_, d, _ := callsFixture(t, now, true, nil)
	h := Public(d, testSecret)
	base := "/s/" + testSecret + "/api/home"
	w := do(h, "GET", base+"?part=live&voice=you", nil)
	if w.Code != 200 {
		t.Fatalf("live: %d %s", w.Code, w.Body.String())
	}
	var l HomeLive
	if err := json.Unmarshal(w.Body.Bytes(), &l); err != nil {
		t.Fatal(err)
	}
	if l.Voice != voicePublic || strings.Contains(w.Body.String(), "swedencentral") || regexp.MustCompile(`\b[Yy]our\b`).MatchString(w.Body.String()) {
		t.Errorf("public live: %s", w.Body.String())
	}
	cs := d.Store.(*countingStore)
	w = do(h, "GET", base+"?part=history&span=w&tz=60", nil)
	n := cs.calls.Load()
	w2 := do(h, "GET", base+"?part=history&span=w&tz=61", nil) // rounds to the same key
	if w.Code != 200 || w2.Code != 200 || cs.calls.Load() != n || w.Body.String() != w2.Body.String() {
		t.Errorf("history not cached: %d %d, %d -> %d panels", w.Code, w2.Code, n, cs.calls.Load())
	}
	if w := do(h, "GET", base+"?part=history&tz=9999", nil); w.Code != 400 {
		t.Errorf("bad tz: %d", w.Code)
	}
}

// bannedOnHome are words Home never says (docs/HOME-API.md). DNS may
// appear in parentheses and in the owner's tips.
var bannedOnHome = regexp.MustCompile(`(?i)\b(stun|udp|tcp|icmp|anycast|mos|jitter|p95|probes?|targets?|baseline|verdict|profile|gateway|isp edge|ms)\b|×\d|--[a-z]`)

// TestHomeCopyWords runs every answer the table can make, in both voices,
// and every other copy function, through the banned-word check; and the
// public voice never says "your".
func TestHomeCopyWords(t *testing.T) {
	p := homeProfile()
	kinds := []model.VerdictKind{model.VerdictOK, model.VerdictWarmingUp, model.VerdictLAN, model.VerdictISP, model.VerdictUpstream,
		model.VerdictDNS, model.VerdictService, model.VerdictNoNetwork, model.VerdictUDPBlocked, model.VerdictSTUNBlocked,
		model.VerdictUDPDegraded, VerdictUnknown}
	evs := []map[string]float64{nil, {"services_loss": 0.2}, {"services_loss": 0.02}}
	var all []string
	for _, voice := range []string{voiceYou, voicePublic} {
		cp := copyFor(voice)
		var texts []string
		for _, k := range kinds {
			for _, ev := range evs {
				for _, own := range []string{"good", "fair", "poor", "unusable", "blocked", "not_enough_data", "server_not_answering"} {
					for _, quiet := range []bool{false, true} {
						a := homeAnswerOf(homeIn(p, model.Verdict{Kind: k, Evidence: ev, Quiet: quiet, Targets: []string{"YouTube"}}, own), cp)
						texts = append(texts, a.Headline, a.Cause, a.Hint, a.Fact, a.Announce)
						if voice == voicePublic && len(a.Try) > 0 {
							t.Errorf("public try: %v", a.Try)
						}
						for _, x := range a.Try {
							all = append(all, strings.ReplaceAll(x, "DNS", ""))
						}
					}
				}
			}
		}
		for _, st := range []string{"good", "fair", "poor", "unusable", "blocked", "not_enough_data", "server_not_answering", "reachable", "not_answering"} {
			texts = append(texts, cp.callHead(st, false), cp.callLabel(st))
		}
		for _, st := range []string{tileWorking, tileMostly, tileSlow, tileDown, tileChecking} {
			texts = append(texts, cp.tileLabel(st, false), cp.tileNote(st, []string{"A", "B", "C", "D"}, 2, 4))
		}
		for _, id := range []string{"device", "router", "provider", "internet", "apps"} {
			for _, st := range []string{"ok", "warn", "bad", "unknown"} {
				texts = append(texts, cp.nodeLabel(id), cp.nodeTip(id), cp.nodeWords(id, st, 0.3, true))
			}
		}
		for _, who := range []string{whoHome, whoProvider, whoBeyond, whoDNS, whoApp, whoCallTraffic, whoOffline} {
			texts = append(texts, cp.where(who, ""), cp.runLabel(who, ""))
		}
		if got := cp.summary(HomeCounts{Problems: 7, Provider: 3, Home: 1, DNS: 1, App: 1, CallTraffic: 1}, "w", false); voice == voiceYou &&
			got != "7 problems this week: 3 from your internet provider, 1 in your home network and 3 others." {
			t.Errorf("summary %q", got)
		}
		for _, n := range []HomeCounts{{}, {Problems: 1, Provider: 1}, {Problems: 3, Provider: 2, Home: 1}, {Problems: 4, App: 1, DNS: 1, Offline: 1, CallTraffic: 1}} {
			texts = append(texts, cp.summary(n, "d", false), cp.summary(n, "w", false), cp.summary(n, "w", true))
		}
		for _, f := range []string{"loss", "jitter", "latency", ""} {
			texts = append(texts, cp.fact(f, 0.14), cp.pathCause(f))
		}
		for _, m := range []bool{false, true} {
			for _, st := range []string{"", "good", "fair", "poor", "unusable", "blocked", "server_not_answering"} {
				for _, who := range []string{whoNone, whoHome, whoProvider, whoBeyond, whoDNS, whoApp, whoCallTraffic, whoOffline} {
					for _, k := range []string{"", "udp_blocked"} {
						h, c := cp.atWords(m, st, who != whoNone, who, k)
						texts = append(texts, h, c)
					}
				}
			}
		}
		texts = append(texts, cp.serversDown(), cp.otherLine(1), cp.otherLine(3), cp.sharedPath("Good"), cp.picksServer("Zoom"),
			cp.callWords("short", "rare", "bit_uneven"), cp.appsWords("warn", 2), cp.quietHint())
		for _, x := range texts {
			if voice == voicePublic && regexp.MustCompile(`\b[Yy]our\b`).MatchString(x) {
				t.Errorf("public voice says your: %q", x)
			}
			if voice == voicePublic {
				x = strings.ReplaceAll(x, "(DNS)", "")
			}
			all = append(all, x)
		}
	}
	for _, x := range all {
		if m := bannedOnHome.FindString(x); m != "" {
			t.Errorf("%q says %q", x, m)
		}
		if regexp.MustCompile(`\d{1,2}:\d{2}|\d+ ?%`).MatchString(x) {
			t.Errorf("%q holds a time or a percentage", x)
		}
	}
}

func TestHomeAt(t *testing.T) {
	now := time.Now().Truncate(time.Minute).Add(30 * time.Second)
	s, _, fv := homeFixture(t, homeProfile(), now, nil, true)
	fv.list = []model.Incident{{ID: 7, Start: now.Add(-4 * time.Minute), Kind: model.VerdictISP, PeakLoss: 0.3}}
	a, err := s.homeAt(context.Background(), now.Add(-2*time.Minute), voiceYou, now)
	if err != nil {
		t.Fatal(err)
	}
	if !a.Measured || (a.Calls != "poor" && a.Calls != "unusable") || a.Level != levelBad || a.Who != whoProvider || a.Kind != "isp" ||
		a.BadFrom == 0 || a.BadTo <= a.BadFrom || a.Fact == "" || a.To != now.UnixMilli() ||
		a.Cause != "The problem was most likely with your internet provider. Your Wi-Fi and router looked fine." {
		t.Errorf("choppy: %+v", a)
	}
	// Twenty minutes earlier: good, no incident; the honest answer.
	fv.list = nil
	a, _ = s.homeAt(context.Background(), now.Add(-25*time.Minute), voicePublic, now)
	if !a.Measured || a.Calls != "good" || a.Level != levelGood || a.Headline != "The connection was good for calls then" ||
		!strings.Contains(a.Cause, "other side") {
		t.Errorf("good: %+v", a)
	}
	// Before the history: not measured.
	a, _ = s.homeAt(context.Background(), now.Add(-3*time.Hour), voiceYou, now)
	if a.Measured || a.Headline != "We weren't checking at that time" {
		t.Errorf("unmeasured: %+v", a)
	}
	// Through the handler: the future is a 400.
	h := Local(s.d, LocalOptions{Addr: "127.0.0.1:3000"})
	if w := do(h, "GET", "/api/home?part=at&at="+strconv.FormatInt(time.Now().Add(time.Hour).UnixMilli(), 10), localHdr); w.Code != 400 {
		t.Errorf("future: %d", w.Code)
	}
}

func TestHomeName(t *testing.T) {
	for _, tc := range []struct {
		t    model.Target
		want string
	}{
		{model.Target{Name: "WhatsApp-Chat", Tags: []string{"home:calls", "app:whatsapp"}}, "WhatsApp"},
		{model.Target{Name: "M365-Outlook", Tags: []string{"home:work"}}, "M365 Outlook"},
		{model.Target{Name: "CDN-Cloudflare"}, "CDN Cloudflare"},
	} {
		if got := homeName(tc.t); got != tc.want {
			t.Errorf("%s: %q", tc.t.Name, got)
		}
	}
}
