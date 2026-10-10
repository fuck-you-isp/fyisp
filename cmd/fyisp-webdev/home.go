package main

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/web"
)

// Canned /api/home responses (-home <scenario>) for building the Home view
// before the real roll-up exists, and for screenshots of states that are
// hard to provoke. See docs/HOME-API.md, "Development".
var homeScenarios = []string{"good", "warming", "wifi", "isp", "upstream", "one-app-down", "calls-choppy",
	"udp-blocked", "no-network", "week-with-gaps", "public", "clouds-other"}

// homeVerdict is the fake verdict (and -calls mode) that matches a scenario,
// so the Details view tells the same story.
func homeVerdict(scenario string) (kind, calls string) {
	switch scenario {
	case "warming":
		return "warming_up", callsGood
	case "wifi":
		return "lan", callsPoor
	case "isp", "public":
		return "isp", callsPoor
	case "upstream":
		return "upstream", callsPoor
	case "one-app-down", "clouds-other":
		return "service", callsGood
	case "calls-choppy":
		return "udp_degraded", callsPoor
	case "udp-blocked":
		return "stun_blocked", callsBlocked
	case "no-network":
		return "no_network", callsPoor
	}
	return "ok", callsGood
}

// homeHandler serves the canned scenario on .../api/home (local and public)
// and passes everything else to next. The share link always gets the
// public voice.
func homeHandler(scenario string, public bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/api/home") {
			next.ServeHTTP(w, r)
			return
		}
		q := r.URL.Query()
		tz, _ := strconv.Atoi(q.Get("tz"))
		tz = max(-840, min(840, int(math.Round(float64(tz)/15))*15))
		voice := "you"
		if public || q.Get("voice") == "public" || scenario == "public" {
			voice = "public"
		}
		var body any
		now := time.Now()
		switch q.Get("part") {
		case "", "live":
			body = cannedLive(scenario, voice, now)
		case "history":
			span := q.Get("span")
			if span == "" {
				span = "d"
			}
			body = cannedHistory(scenario, voice, span, tz, now)
		default:
			http.Error(w, `{"error":"bad part"}`, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(body)
	})
}

// pv picks the owner's or the public wording.
func pv(voice, you, public string) string {
	if voice == "public" {
		return public
	}
	return you
}

func cannedPath(voice string, router, provider, internet, apps string, blamed string, appsWords string) []web.HomeNode {
	words := map[string]string{"ok": "OK", "warn": "Dropping some data", "bad": "Not answering", "unknown": "No recent checks", "finding": "Finding…"}
	node := func(id, label, state, tip string) web.HomeNode {
		return web.HomeNode{ID: id, Label: label, State: state, Words: words[state], Blamed: id == blamed, Tooltip: tip}
	}
	out := []web.HomeNode{
		node("device", pv(voice, "This device", "Monitoring device"), "ok", ""),
		node("router", pv(voice, "Your router", "Home router"), router, pv(voice, "The box that connects your home to the internet.", "The router that connects the home to the internet.")),
		node("provider", pv(voice, "Your provider", "Provider network"), provider, pv(voice, "The first piece of your internet provider's equipment, just outside your home.", "The provider's first piece of equipment past the home router.")),
		node("internet", pv(voice, "The internet", "The wider internet"), internet, "Big services that are close to almost every provider. If they're fine, the internet itself is fine."),
		node("apps", pv(voice, "Apps & websites", "Apps & websites"), apps, ""),
	}
	if appsWords != "" {
		out[4].Words = appsWords
	}
	return out
}

func tile(id, title, level, state, label string, names []string, ok, total int) web.HomeTile {
	t := web.HomeTile{ID: id, Title: title, Level: level, State: state, Label: label, OK: ok, Total: total}
	if len(names) > 3 {
		t.Names, t.More = names[:3], len(names)-3
	} else {
		t.Names = names
	}
	t.Details = "#v=o&oq=home:" + id
	if id == "calls" {
		t.Details = "#v=calls"
	}
	return t
}

var (
	callNames = []string{"Teams", "Google Meet", "Zoom", "Slack", "Discord", "WhatsApp", "Webex"}
	workNames = []string{"M365 Outlook", "M365 Portal", "M365 Login", "Google Gmail", "Google Docs", "Google Drive"}
	webNames  = []string{"CDN Cloudflare", "CDN Akamai", "CDN Fastly", "CDN CloudFront", "CDN Google"}
)

// cannedTiles are the default profile's three tiles with the given call
// state and the same level everywhere else (good: working).
func cannedTiles(callLevel, callState, callLabel, rest string) []web.HomeTile {
	label := map[string]string{"working": "Working", "mostly": "Mostly working", "slow": "Slow", "down": "Not working", "checking": "Checking…"}
	level := map[string]string{"working": "good", "mostly": "warn", "slow": "warn", "down": "bad", "checking": "checking"}
	ok := func(n int) int {
		if rest == "working" {
			return n
		}
		return 0
	}
	return []web.HomeTile{
		tile("calls", "Calls", callLevel, callState, callLabel, callNames, 7, 7),
		tile("work", "Work apps", level[rest], rest, label[rest], workNames, ok(6), 6),
		tile("web", "Websites", level[rest], rest, label[rest], webNames, ok(5), 5),
	}
}

func cannedApps(voice, meetState, meetLabel, meetLevel string) []web.HomeApp {
	path := fmt.Sprintf("Calls use %s call path (%s).", pv(voice, "your", "the"), meetLabel)
	pick := "picks a server when each meeting starts, so it can't be tested ahead of time."
	return []web.HomeApp{
		{App: "teams", Title: "Teams", Level: "good", State: "reachable", Label: "Reachable", Note: path},
		{App: "meet", Title: "Google Meet", Level: meetLevel, State: meetState, Label: meetLabel},
		{App: "zoom", Title: "Zoom", Level: "good", State: "reachable", Label: "Reachable", Note: "Zoom " + pick},
		{App: "slack", Title: "Slack", Level: "good", State: "reachable", Label: "Reachable", Note: "Slack " + pick},
		{App: "discord", Title: "Discord", Level: "good", State: "reachable", Label: "Reachable", Note: "Discord " + pick},
		{App: "whatsapp", Title: "WhatsApp", Level: "good", State: "reachable", Label: "Reachable", Note: "WhatsApp " + pick},
		{App: "webex", Title: "Webex", Level: "good", State: "reachable", Label: "Reachable", Note: "Webex " + pick},
	}
}

func goodCalls() *web.HomeCalls {
	return &web.HomeCalls{State: "good", Delay: "very_short", Dropouts: "none", Smoothness: "steady",
		Words: "Delay very short · Dropouts none · Smoothness steady"}
}

func choppyCalls(now time.Time) *web.HomeCalls {
	return &web.HomeCalls{State: "poor", Delay: "short", Dropouts: "frequent", Smoothness: "uneven",
		Words: "Delay short · Dropouts frequent · Smoothness uneven", WorstAt: now.Add(-3 * time.Minute).Truncate(time.Minute).UnixMilli()}
}

func cannedLive(scenario, voice string, now time.Time) web.HomeLive {
	ms := now.UnixMilli()
	since := now.Add(-14 * time.Minute).Truncate(time.Minute).UnixMilli()
	worst := now.Add(-3 * time.Minute).Truncate(time.Minute).UnixMilli()
	l := web.HomeLive{Part: "live", Voice: voice, At: ms, Apps: cannedApps(voice, "good", "Good", "good"), Calls: goodCalls()}
	a := &l.Answer
	a.Try = []string{}
	try := func(s ...string) {
		if voice != "public" {
			a.Try = s
		}
	}
	choppy := func() {
		l.Tiles = cannedTiles("bad", "poor", "Calls may be choppy", "working")
		l.Calls = choppyCalls(now)
		l.Apps = cannedApps(voice, "poor", "Choppy", "bad")
		a.Calls, a.Fact, a.FactAt = "poor", "In the worst minute, about 1 in 7 bits of sound were lost.", worst
	}
	switch scenario {
	case "warming":
		*a = web.HomeAnswer{Level: "checking", Who: "none", Kind: "warming_up", Calls: "not_enough_data", Try: []string{},
			Headline: pv(voice, "Checking your connection…", "Checking this connection…"),
			Cause:    pv(voice, "This takes about a minute the first time.", "Results appear after about a minute.")}
		l.Path = cannedPath(voice, "ok", "finding", "unknown", "unknown", "", "")
		l.Tiles = cannedTiles("checking", "not_enough_data", "Checking…", "checking")
		l.Calls = &web.HomeCalls{State: "not_enough_data"}
		for i := range l.Apps {
			l.Apps[i].Level, l.Apps[i].State, l.Apps[i].Label, l.Apps[i].Note = "checking", "not_enough_data", "Not checked yet", ""
		}
		l.Steps = []web.HomeStep{{ID: "router", Label: pv(voice, "Your router", "Home router"), Done: true},
			{ID: "provider", Label: pv(voice, "Your provider", "Provider network"), Done: true},
			{ID: "internet", Label: "The internet"}, {ID: "apps", Label: "Apps & websites"}}
	case "wifi":
		choppy()
		*a = web.HomeAnswer{Level: "bad", Who: "home", Confidence: "likely", Kind: "lan", Calls: "poor", Since: since,
			Headline: "Calls may be choppy",
			Cause: pv(voice, "The problem is most likely inside your home: your Wi-Fi or router. Your internet provider looks fine from here.",
				"The problem is most likely in the home network (Wi-Fi or router). The provider's network looks fine from here."),
			Fact: a.Fact, FactAt: a.FactAt, Try: []string{}}
		try("Move closer to your router, or plug in a cable.", "Restart the router.", "If other people are streaming or downloading, try again when they stop.")
		l.Path = cannedPath(voice, "bad", "warn", "warn", "warn", "router", "Slow to load")
	case "isp", "public":
		choppy()
		*a = web.HomeAnswer{Level: "bad", Who: "provider", Confidence: "sure", Kind: "isp", Calls: "poor", Since: since,
			Headline: "Calls may be choppy",
			Cause: pv(voice, "The problem is with your internet provider. Your Wi-Fi and router are fine.",
				"The problem is in the provider's network. The home network (Wi-Fi and router) is fine."),
			Fact: a.Fact, FactAt: a.FactAt, Try: []string{}}
		try("Restarting your router sometimes helps.", "If it keeps happening, make a report and send it to your provider.")
		l.Path = cannedPath(voice, "ok", "bad", "bad", "warn", "provider", "Slow to load")
	case "upstream":
		choppy()
		l.Tiles = cannedTiles("bad", "poor", "Calls may be choppy", "down")
		*a = web.HomeAnswer{Level: "bad", Who: "beyond", Confidence: "sure", Kind: "upstream", Calls: "poor", Since: since,
			Headline: "Many websites and apps are hard to reach",
			Cause: pv(voice, "Your Wi-Fi and your provider's nearest equipment look fine. The problem is further out: either your provider's links to the rest of the internet, or a wider outage.",
				"The home network and the provider's nearest equipment look fine. The problem is further out: either the provider's links to the rest of the internet, or a wider outage."),
			Fact: a.Fact, FactAt: a.FactAt, Try: []string{}}
		try("There's usually nothing to fix at home.", "If it lasts, check your provider's status page, or make a report.")
		l.Path = cannedPath(voice, "ok", "ok", "bad", "bad", "internet", "Most aren't answering")
	case "one-app-down":
		l.Tiles = cannedTiles("good", "good", "Good for calls", "working")
		w := &l.Tiles[1]
		w.Level, w.State, w.Label, w.OK, w.Affected = "warn", "mostly", "Mostly working", 5, []string{"M365 Outlook"}
		w.Note = "M365 Outlook isn't answering from here. That's usually a problem on their side."
		*a = web.HomeAnswer{Level: "good", Who: "none", Kind: "service", Calls: "good", Try: []string{},
			Headline: pv(voice, "Your internet is fine for calls and work", "This connection is fine for calls and work"),
			Cause:    "Calls, work apps and websites are all working."}
		l.Path = cannedPath(voice, "ok", "ok", "ok", "warn", "", "1 app has problems")
	case "calls-choppy":
		choppy()
		*a = web.HomeAnswer{Level: "bad", Who: "calltraffic", Kind: "udp_degraded", Calls: "poor", Since: since,
			Headline: "Calls may sound choppy",
			Cause: pv(voice, "Browsing looks fine, but the connection calls use is losing bits of sound and video, or delivering them unevenly. We didn't see a cause on your side.",
				"Browsing looks fine, but call traffic is losing bits of sound and video, or delivering them unevenly. No cause was seen in the home network."),
			Fact: a.Fact, FactAt: a.FactAt, Try: []string{}}
		try("Use a cable instead of Wi-Fi if you can.", "Pause big downloads or video streams during calls.", "If it lasts, make a report.")
		l.Path = cannedPath(voice, "ok", "ok", "ok", "ok", "", "")
	case "udp-blocked":
		l.Tiles = cannedTiles("info", "blocked", "This network blocks call traffic", "working")
		l.Calls = &web.HomeCalls{State: "blocked"}
		l.Apps = cannedApps(voice, "blocked", "Blocked", "info")
		*a = web.HomeAnswer{Level: "info", Who: "calltraffic", Kind: "stun_blocked", Calls: "blocked", Since: now.Add(-26 * time.Hour).UnixMilli(),
			Headline: pv(voice, "This network always blocks some of the connections calls use", "This network blocks some of the connections calls use"),
			Cause:    "Most call apps still work, but may be slower to connect or lower quality. We'll keep checking, but there's nothing new.",
			Try:      []string{}}
		try("Try another network (for example, your phone's hotspot) to compare.", "If it's your home network, check your router's firewall or security app settings.")
		l.Path = cannedPath(voice, "ok", "ok", "ok", "ok", "", "")
	case "no-network":
		l.Tiles = cannedTiles("bad", "unusable", "Calls will break up", "down")
		l.Calls = &web.HomeCalls{State: "unusable"}
		l.Apps = cannedApps(voice, "unusable", "Breaking up", "bad")
		for i := range l.Apps {
			if l.Apps[i].State == "reachable" {
				l.Apps[i].Level, l.Apps[i].State, l.Apps[i].Label, l.Apps[i].Note = "bad", "not_answering", "Not answering", ""
			}
		}
		*a = web.HomeAnswer{Level: "bad", Who: "offline", Kind: "no_network", Calls: "unusable", Since: since,
			Headline: pv(voice, "No internet connection", "The monitoring device is offline"),
			Cause:    pv(voice, "The device running this monitor is offline.", "The device running this monitor has no network connection."),
			Try:      []string{}}
		try("Check that Wi-Fi is on or the cable is plugged in.", "Restart your router.")
		l.Path = cannedPath(voice, "bad", "unknown", "unknown", "bad", "router", "")
	case "clouds-other":
		l.Calls, l.Apps = nil, []web.HomeApp{}
		o := tile("other", "Other services", "warn", "mostly", "Some aren't answering", []string{"AWS us-east-1", "AWS eu-west-1", "Google Cloud europe-west1"}, 54, 84)
		o.More, o.Affected, o.Details = 81, []string{"AWS me-south-1", "AWS ap-east-1", "AWS il-central-1"}, "#v=o&op=1"
		o.Note = "30 of 84 services aren't answering from here."
		l.Tiles = []web.HomeTile{o}
		*a = web.HomeAnswer{Level: "warn", Who: "app", Kind: "service", Since: since, Try: []string{},
			Headline: "Some services aren't answering",
			Cause:    pv(voice, "Your connection is fine: 30 of 84 services aren't answering from here. That's most likely on their side.", "The connection is fine: 30 of 84 services aren't answering from here. That's most likely on their side.")}
		l.Path = cannedPath(voice, "ok", "ok", "ok", "warn", "", "30 services have problems")
	default: // good, week-with-gaps
		*a = web.HomeAnswer{Level: "good", Who: "none", Kind: "ok", Calls: "good", Try: []string{},
			Headline: pv(voice, "Your internet is fine for calls and work", "This connection is fine for calls and work"),
			Cause:    pv(voice, "Your internet is working normally.", "This connection is working normally.")}
		l.Tiles = cannedTiles("good", "good", "Good for calls", "working")
		l.Path = cannedPath(voice, "ok", "ok", "ok", "ok", "", "")
	}
	if a.Announce == "" {
		a.Announce = strings.TrimSpace(a.Headline + ". " + a.Cause)
	}
	return l
}

// cannedHistory builds days from local midnight in tz, with problems
// placed relative to now.
func cannedHistory(scenario, voice, span string, tz int, now time.Time) web.HomeHistory {
	loc := time.FixedZone("", tz*60)
	ln := now.In(loc)
	today := time.Date(ln.Year(), ln.Month(), ln.Day(), 0, 0, 0, 0, loc)
	n := 1
	if span == "w" {
		n = 7
	}
	h := web.HomeHistory{Part: "history", Voice: voice, At: now.UnixMilli(), TZ: tz, Span: span,
		From: today.AddDate(0, 0, 1-n).UnixMilli(), To: now.UnixMilli(), Problems: []web.HomeProblem{}}
	first := today.AddDate(0, 0, -30)
	switch scenario {
	case "warming":
		first = now.Add(-30 * time.Second)
	case "week-with-gaps":
		first = today.AddDate(0, 0, -4).Add(10 * time.Hour)
	}
	h.FirstData = first.UnixMilli()
	where := map[string]string{
		"provider": pv(voice, "Your provider's network (past your router)", "Provider network (past the router)"),
		"home":     pv(voice, "Your home network (Wi-Fi or router)", "Home network (Wi-Fi or router)"),
		"beyond":   "Further out on the internet",
	}
	label := map[string]string{"provider": pv(voice, "Your provider", "Provider network"), "home": "Home network",
		"beyond": "Further out", "calltraffic": "Call traffic", "offline": "Offline", "app": "An app"}
	kinds := map[string]string{"provider": "isp", "home": "lan", "beyond": "upstream", "calltraffic": "udp_degraded", "offline": "no_network", "app": "service"}
	type inc struct {
		from, to time.Time
		who      string
		ongoing  bool
		loss     float64
	}
	var incs []inc
	add := func(daysAgo int, hh, mm, mins int, who string, loss float64) {
		f := today.AddDate(0, 0, -daysAgo).Add(time.Duration(hh)*time.Hour + time.Duration(mm)*time.Minute)
		if f.After(now) {
			f = now.Add(-time.Duration(mins+30) * time.Minute)
		}
		incs = append(incs, inc{f, f.Add(time.Duration(mins) * time.Minute), who, false, loss})
	}
	live := func(who string) {
		incs = append(incs, inc{now.Add(-14 * time.Minute).Truncate(time.Minute), now, who, true, 0.62})
	}
	switch scenario {
	case "isp", "public":
		add(4, 14, 41, 15, "provider", 1)
		live("provider")
	case "wifi":
		add(2, 10, 5, 6, "home", 0.4)
		live("home")
	case "upstream":
		live("beyond")
	case "calls-choppy":
		live("calltraffic")
	case "no-network":
		live("offline")
	case "week-with-gaps":
		add(3, 14, 5, 15, "provider", 1)
		add(1, 9, 40, 12, "provider", 0.7)
		add(1, 16, 2, 4, "home", 0.35)
	case "clouds-other":
		live("app")
	}
	var callRun *web.HomeRun
	if scenario == "good" || scenario == "week-with-gaps" || scenario == "one-app-down" {
		f := now.Add(-35 * time.Minute).Truncate(time.Minute)
		if !f.Before(today) {
			callRun = &web.HomeRun{From: f.UnixMilli(), To: f.Add(7 * time.Minute).UnixMilli(), Level: "warn", Who: "calltraffic",
				Kind: "fair", Label: "Call traffic", Source: "calls"}
		}
	}
	var id int64 = 40
	for d := n - 1; d >= 0; d-- {
		ds := today.AddDate(0, 0, -d)
		de := ds.AddDate(0, 0, 1)
		day := web.HomeDay{Date: ds.Format("2006-01-02"), From: ds.UnixMilli(), To: de.UnixMilli(), Runs: []web.HomeRun{}, Off: []web.HomeSpan{}}
		if d == 0 {
			day.To = now.UnixMilli()
		}
		if wd := ds.Weekday(); wd != time.Saturday && wd != time.Sunday {
			day.WorkFrom, day.WorkTo = ds.Add(9*time.Hour).UnixMilli(), ds.Add(17*time.Hour).UnixMilli()
		}
		switch {
		case !de.After(first):
			day.NoRecord = true
		case scenario == "week-with-gaps":
			if d == 2 { // switched off overnight
				day.Off = append(day.Off, web.HomeSpan{From: ds.UnixMilli(), To: ds.Add(9*time.Hour + 15*time.Minute).UnixMilli()})
			}
			if d == 3 {
				day.Off = append(day.Off, web.HomeSpan{From: ds.Add(18*time.Hour + 2*time.Minute).UnixMilli(), To: de.UnixMilli()})
			}
		}
		for _, in := range incs {
			if !in.from.Before(de) || !in.to.After(ds) {
				continue
			}
			level := "bad"
			if in.who == "calltraffic" || in.who == "app" {
				level = "warn"
			}
			day.Runs = append(day.Runs, web.HomeRun{From: max(in.from.UnixMilli(), ds.UnixMilli()), To: min(in.to.UnixMilli(), de.UnixMilli()), Level: level,
				Who: in.who, Kind: kinds[in.who], Label: label[in.who], Source: "incident", Ongoing: in.ongoing})
		}
		if d == 0 && callRun != nil {
			day.Runs = append(day.Runs, *callRun)
		}
		h.Days = append(h.Days, day)
	}
	for _, in := range incs {
		if in.to.UnixMilli() <= h.From {
			continue
		}
		id++
		p := web.HomeProblem{ID: id, From: in.from.UnixMilli(), Ongoing: in.ongoing, Kind: kinds[in.who], Who: in.who,
			Where: where[in.who], PeakLoss: in.loss,
			Details: fmt.Sprintf("#v=calls&from=%d&to=%d&o=1", in.from.Add(-30*time.Minute).UnixMilli(), min(in.to.Add(30*time.Minute).UnixMilli(), now.UnixMilli()))}
		if p.Where == "" {
			p.Where = label[in.who]
		}
		if !in.ongoing {
			p.To = in.to.UnixMilli()
		}
		h.Problems = append(h.Problems, p)
		h.Counts.Problems++
		switch in.who {
		case "provider":
			h.Counts.Provider++
		case "home":
			h.Counts.Home++
		case "beyond":
			h.Counts.Beyond++
		case "calltraffic":
			h.Counts.CallTraffic++
		case "offline":
			h.Counts.Offline++
		case "app":
			h.Counts.App++
		}
	}
	slices.Reverse(h.Problems)
	// Link runs to their incident ids (newest first in Problems).
	for i := range h.Days {
		for j := range h.Days[i].Runs {
			r := &h.Days[i].Runs[j]
			for _, p := range h.Problems {
				if r.Source == "incident" && p.Who == r.Who && r.From >= p.From && (p.Ongoing || r.To <= p.To) {
					r.Incident = p.ID
				}
			}
		}
	}
	period := "today"
	if span == "w" {
		period = "this week"
	}
	c := h.Counts
	switch {
	case scenario == "warming":
		h.Summary = pv(voice, "We just started, so the history begins now.", "Monitoring just started, so the history begins now.")
	case c.Problems == 0 && span == "w":
		h.Summary = "No problems in the last 7 days."
	case c.Problems == 0:
		h.Summary = "No problems today."
	case c.Problems == c.Provider:
		h.Summary = fmt.Sprintf("%s %s, %s.", plural(c.Problems, "problem"), period,
			pv(voice, map[bool]string{true: "from your internet provider", false: "all from your internet provider"}[c.Problems == 1],
				map[bool]string{true: "in the provider's network", false: "all in the provider's network"}[c.Problems == 1]))
	case c.Provider > 0 && c.Home > 0:
		h.Summary = fmt.Sprintf("%s %s: %d %s, %d %s.", plural(c.Problems, "problem"), period,
			c.Provider, pv(voice, "from your internet provider", "in the provider's network"),
			c.Home, pv(voice, "in your home network", "in the home network"))
	default:
		h.Summary = fmt.Sprintf("%s %s.", plural(c.Problems, "problem"), period)
	}
	if scenario != "warming" && scenario != "clouds-other" {
		if wd := ln.Weekday(); wd != time.Saturday && wd != time.Sunday && ln.Hour() >= 9 {
			m := min(ln.Hour(), 17)*60 + ln.Minute() - 9*60
			if ln.Hour() >= 17 {
				m = 8 * 60
			}
			bad := 0
			if slices.Contains([]string{"isp", "public", "wifi", "calls-choppy", "upstream", "no-network"}, scenario) {
				bad = min(m, 9)
			}
			h.Work = &web.HomeWorkCalls{Measured: m, Bad: bad, Good: float64(m-bad) / float64(max(m, 1))}
		}
	}
	return h
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return strconv.Itoa(n) + " " + word + "s"
}
