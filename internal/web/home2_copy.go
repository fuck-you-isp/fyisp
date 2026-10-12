package web

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/profile"
	"github.com/fuck-you-isp/fyisp/internal/tiles"
)

// h2copy writes the new Home's words (docs/HOME-API.md, v2) in en-US, with
// times of day in the viewer's zone ("10:14 am"). The rules, pinned by the
// home2 golden files: "internet provider", never "ISP"; no ms, jitter or
// loss outside a tile panel's figures; always "Likely cause"; no plan or
// fiber; US spelling; no relative times in the headline or the sub line
// (they never change under one answer key).
type h2copy struct {
	loc *time.Location
	now time.Time
	// wifi: the Wi-Fi wording ("Your Wi-Fi & router"): fyisp's own uplink
	// is Wi-Fi and the page is viewed on the same machine.
	wifi bool
}

// ---------- times ----------

func (c h2copy) in(t time.Time) time.Time { return t.In(c.loc) }

// clock is "10:14 am".
func (c h2copy) clock(t time.Time) string { return c.in(t).Format("3:04 pm") }

// span is "9:41–9:45 am", or "11:50 am–12:10 pm" across noon or midnight;
// across a change of the clocks it names the zones: "1:50 am EDT–1:10 am
// EST".
func (c h2copy) span(a, b time.Time) string {
	pa, pb := c.clock(a), c.clock(b)
	if za, zb := c.in(a).Format("MST"), c.in(b).Format("MST"); za != zb {
		return pa + " " + za + "–" + pb + " " + zb
	}
	if pa[len(pa)-2:] == pb[len(pb)-2:] {
		return c.in(a).Format("3:04") + "–" + pb
	}
	return pa + "–" + pb
}

// daysAgo counts the local midnights between t and now.
func (c h2copy) daysAgo(t time.Time) int {
	y1, m1, d1 := c.in(t).Date()
	y2, m2, d2 := c.in(c.now).Date()
	a := time.Date(y1, m1, d1, 12, 0, 0, 0, time.UTC)
	b := time.Date(y2, m2, d2, 12, 0, 0, 0, time.UTC)
	return int(math.Round(b.Sub(a).Hours() / 24))
}

// day is "Today", "Yesterday", "Thu" (this week) or "Oct 3".
func (c h2copy) day(t time.Time) string {
	switch n := c.daysAgo(t); {
	case n == 0:
		return "Today"
	case n == 1:
		return "Yesterday"
	case n > 1 && n < 7:
		return c.in(t).Format("Mon")
	}
	return c.in(t).Format("Jan 2")
}

// dayIn is day in a sentence: "today", "yesterday", "Thu", "Oct 3".
func (c h2copy) dayIn(t time.Time) string {
	d := c.day(t)
	if d == "Today" || d == "Yesterday" {
		return strings.ToLower(d)
	}
	return d
}

// when is a time in a sentence: "10:12 am" today, else "yesterday 11:50 pm".
func (c h2copy) when(t time.Time) string {
	if c.daysAgo(t) == 0 {
		return c.clock(t)
	}
	return c.dayIn(t) + " " + c.clock(t)
}

// since is a tile's note: "since 10:05 am" (another day: "since yesterday
// 11:50 pm").
func (c h2copy) since(t time.Time) string { return "since " + c.when(t) }

// about is a duration in words: "about 40 seconds", "about 4 minutes",
// "about 2 hours".
func about(d time.Duration) string {
	switch {
	case d < time.Minute:
		s := max(10, int(math.Round(d.Seconds()/10))*10)
		if s >= 60 {
			return "about 1 minute"
		}
		return fmt.Sprintf("about %d seconds", s)
	case d < 90*time.Minute:
		return "about " + count(int(math.Round(d.Minutes())), "minute")
	case d < 48*time.Hour:
		return "about " + count(int(math.Round(d.Hours())), "hour")
	}
	return "about " + count(int(math.Round(d.Hours()/24)), "day")
}

// short is a duration for a note: "40 s", "2 min", "1 h 20 min".
func short(d time.Duration) string {
	switch {
	case d < time.Minute:
		return strconv.Itoa(int(d.Round(time.Second).Seconds())) + " s"
	case d < time.Hour:
		return strconv.Itoa(int(math.Round(d.Minutes()))) + " min"
	}
	h, m := int(d.Hours()), int(math.Round(d.Minutes()))%60
	if m == 0 {
		return strconv.Itoa(h) + " h"
	}
	return fmt.Sprintf("%d h %d min", h, m)
}

// count is "1 minute", "4 minutes".
func count(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return strconv.Itoa(n) + " " + word + "s"
}

// ---------- names ----------

func tileDef(id string) profile.TileDef {
	for _, d := range profile.Tiles {
		if d.ID == id {
			return d
		}
	}
	return profile.TileDef{ID: id, Name: id}
}

var vendorNames = map[string]string{"zoom": "Zoom", "microsoft": "Microsoft", "google": "Google", "slack": "Slack",
	"github": "GitHub", "dropbox": "Dropbox", "netflix": "Netflix"}

func vendorName(v string) string {
	if n, ok := vendorNames[v]; ok {
		return n
	}
	return capFirst(v)
}

// vendorSide is "Zoom’s side".
func vendorSide(v string) string { return vendorName(v) + "’s side" }

// andJoin is "A", "A and B", "A, B and C".
func andJoin(xs []string) string {
	switch len(xs) {
	case 0:
		return ""
	case 1:
		return xs[0]
	}
	return strings.Join(xs[:len(xs)-1], ", ") + " and " + xs[len(xs)-1]
}

func capFirst(s string) string {
	if s == "" || strings.HasPrefix(s, "fyisp") {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// switchNames are the meeting apps a stuck call can move to.
var switchNames = map[string]string{"zoom": "Zoom", "teams": "Teams", "meet": "Google Meet"}

// ---------- tiles ----------

// word is a tile's (or bar's) word.
func (c h2copy) word(st tiles.State, kind profile.TileKind) string {
	switch st {
	case tiles.Good:
		return "Good"
	case tiles.Slow:
		if kind == profile.TileCall {
			return "Choppy"
		}
		return "Slow"
	case tiles.Down:
		return "Down"
	case tiles.Off:
		return "Not checked"
	}
	return "No data"
}

// dropNote is a Good tile's note after a drop: "Dropped 40 s at 10:14 am".
func (c h2copy) dropNote(d *tiles.Drop) string {
	return "Dropped " + short(d.Dur) + " at " + c.when(d.Start)
}

// tileReason is a tile's plain-language state, with its likely cause when
// it is on the app's side (a router or provider cause is the card's).
func (c h2copy) tileReason(d profile.TileDef, t tiles.TileSnap, checking bool) string {
	if checking {
		return "fyisp is still checking."
	}
	var r string
	switch t.State {
	case tiles.Off:
		return "fyisp isn’t checking " + d.Name + " on this setup."
	case tiles.Good:
		r = "Working normally."
		if d.Kind == profile.TileCall {
			r = "Calls should be smooth."
		}
	case tiles.Slow:
		r = "Slower than usual " + c.since(t.Since) + "."
		if d.Kind == profile.TileCall {
			r = "Calls may be choppy " + c.since(t.Since) + "."
		}
	case tiles.Down:
		r = "Not loading " + c.since(t.Since) + "."
		if d.Kind == profile.TileCall {
			r = "Not connecting " + c.since(t.Since) + "."
		}
	default:
		return "No data yet."
	}
	if d.Kind == profile.TileStream && t.State != tiles.Down {
		r += " " + d.Name + " is reachable. If it’s buffering, test your speed below."
	}
	if t.State.Bad() && t.Cause.Who != "" && t.Cause.Who != tiles.WhoUnknown {
		r += " Likely cause: " + c.cause(t.Cause) + " — " + c.tileEvidence(t.Cause) + "."
	}
	if t.State == tiles.Good && t.LastDrop != nil {
		r += " It dropped for " + about(t.LastDrop.Dur) + " at " + c.when(t.LastDrop.Start) + "."
	}
	return r
}

// tileEvidence backs an app-side cause in a tile's reason.
func (c h2copy) tileEvidence(x tiles.Cause) string {
	switch x.Evidence {
	case tiles.EvAppOnly:
		return "everything else is normal"
	case tiles.EvVendorOnly:
		return "the non-" + vendorName(x.Vendor) + " apps are normal"
	case tiles.EvCallsPath:
		return "websites and other apps are fine"
	case tiles.EvSpeedtest:
		return "it uses most of your connection while it runs"
	}
	return c.evidence(x, true)
}

// barsSummary sums up a tile's bars for screen readers.
func (c h2copy) barsSummary(bs []tiles.Bar) string {
	var down, slow, none int
	for _, b := range bs {
		switch b.State {
		case tiles.Down:
			down++
		case tiles.Slow:
			slow++
		case tiles.Good:
		default:
			none++
		}
	}
	if none == len(bs) {
		return ""
	}
	var parts []string
	of := fmt.Sprintf(" of %d five-minute periods", len(bs))
	if down > 0 {
		parts = append(parts, fmt.Sprintf("down or dropped in %d%s", down, of))
		of = ""
	}
	if slow > 0 {
		parts = append(parts, fmt.Sprintf("slow in %d%s", slow, of))
	}
	if none > 0 {
		parts = append(parts, fmt.Sprintf("not watched for %d minutes", none*5))
	}
	if len(parts) == 0 {
		return "Last 2 hours: no problems."
	}
	return "Last 2 hours: " + strings.Join(parts, ", ") + "."
}

// enableHint says which profile adds an off tile's targets.
func (c h2copy) enableHint(d profile.TileDef) string {
	return "To check " + d.Name + ", add " + d.Enable + " to fyisp’s --profile setting (FYISP_PROFILE in Docker)."
}

// coverage is what a tile's figures are judged on, when that is not
// simply its own servers ("" otherwise).
func (c h2copy) coverage(r profile.ResolvedTile, src string) string {
	switch {
	case r.Kind == profile.TileCall && (src == tiles.CallTCPOnly || len(r.UDP) == 0):
		return "Can connect; call quality isn’t checked on this setup."
	case r.Kind == profile.TileCall && len(r.TileDef.UDP) == 0:
		return "Call quality is judged from the general call path; fyisp can’t test " + r.Name + "’s own servers before a meeting."
	case r.Kind == profile.TileCall && r.SharedUDP:
		return "Call quality is judged from the general call path, because this setup doesn’t check " + vendorName(r.Vendors[0]) + "’s own call servers."
	case r.ID == "onedrive":
		return "Checks that Microsoft’s OneDrive front door answers. Work OneDrive on your company’s SharePoint isn’t checked."
	}
	return ""
}

// ---------- causes ----------

// cause names a likely cause; it follows "Likely cause: ".
func (c h2copy) cause(x tiles.Cause) string {
	switch x.Who {
	case tiles.WhoRouter:
		return "your home router"
	case tiles.WhoOffline:
		return "no network connection"
	case tiles.WhoProvider:
		return "your internet provider"
	case tiles.WhoInternet:
		return "the internet past your provider"
	case tiles.WhoNames:
		return "name lookups (DNS)"
	case tiles.WhoApp:
		return vendorSide(x.Vendor)
	case tiles.WhoCallsPath:
		return "the path calls take"
	case tiles.WhoSpeedtest:
		return "the speed test fyisp was running"
	}
	return "unclear"
}

// evidence backs a cause, lower case: over (the problem ended) or now.
func (c h2copy) evidence(x tiles.Cause, now bool) string {
	pick := func(past, present string) string {
		if now {
			return present
		}
		return past
	}
	switch x.Evidence {
	case tiles.EvRouterDown:
		return pick("your router stopped responding, so fyisp couldn’t reach anything past it",
			"your router isn’t responding, so fyisp can’t reach anything past it")
	case tiles.EvRouterLossy:
		return pick("your router was dropping some data", "your router is dropping some data")
	case tiles.EvNoNetwork:
		return pick("the computer fyisp runs on had no network connection", "the computer fyisp runs on has no network connection")
	case tiles.EvProviderDrop:
		return pick("your router kept answering, but nothing past it did", "your router is answering, but nothing past it is")
	case tiles.EvProviderLossy:
		return pick("your router was fine, but your provider’s connection was dropping some data",
			"your router is fine; the problem starts just past it, inside your provider’s network")
	case tiles.EvBeyond:
		return pick("your router and your provider answered, but sites past them didn’t",
			"your router and your provider answer, but sites past them don’t")
	case tiles.EvDNS:
		return pick("your connection worked, but name lookups failed", "your connection works, but name lookups fail")
	case tiles.EvVendorOnly:
		return pick("your internet and the non-"+vendorName(x.Vendor)+" apps were normal",
			"your internet and the non-"+vendorName(x.Vendor)+" apps are normal")
	case tiles.EvAppOnly:
		return pick("your internet and every other app were normal", "your internet and all other apps are normal")
	case tiles.EvCallsPath:
		return pick("websites and other apps were fine; only calls were affected", "websites and other apps are fine; only calls are affected")
	case tiles.EvSpeedtest:
		return pick("fyisp was running a speed test then, which uses most of your connection for a short while",
			"fyisp is running a speed test, which uses most of your connection for a short while")
	}
	return pick("your router and your internet provider looked fine", "your router and your internet provider look fine")
}

// ---------- the answer card ----------

// routerName is the first node: the Wi-Fi wording only on the same
// machine over Wi-Fi.
func (c h2copy) routerName() string {
	if c.wifi {
		return "Your Wi-Fi & router"
	}
	return "Your home router"
}

// againSentence is "Working again. Your internet dropped for about 40
// seconds at 10:14 am." and its variants.
func (c h2copy) againSentence(a *tiles.Recovered) string {
	switch a.Variant {
	case tiles.AgainDropped:
		return "Working again. Your internet dropped for " + about(a.Dur) + " at " + c.clock(a.Start) + "."
	case tiles.AgainDown:
		return "Working again. Your internet was down for " + about(a.Dur) + " (" + c.span(a.Start, a.End) + ")."
	}
	return "Working again. Your internet had trouble for " + about(a.Dur) + " (" + c.span(a.Start, a.End) + ")."
}

// healthy is the good card's sub line.
func (c h2copy) healthy(monitored int, calls bool, learning bool, first time.Time) string {
	who := "Your router, your internet provider"
	if c.wifi {
		who = "Your Wi-Fi and router, your internet provider"
	}
	var s string
	switch monitored {
	case 0:
		s = strings.Replace(who, ", your", " and your", 1) + " look healthy."
	case 1:
		s = who + " and the app fyisp checks look healthy."
	default:
		s = fmt.Sprintf("%s and all %d apps look healthy.", who, monitored)
	}
	switch {
	case learning && !first.IsZero():
		s += " fyisp started watching at " + c.when(first) + "."
	case calls:
		s += " Calls and video should be smooth."
	}
	return s
}

// appCard is the card of problems with some apps while the connection
// is fine (the card stays green). causes are the bad tiles' likely causes:
// their own, or, when the connection is to blame (6% loss past the router
// makes calls choppy without a path verdict), their problem's.
func (c h2copy) appCard(bad []tiles.TileSnap, causes []tiles.Cause, all []tiles.TileSnap) (headline, sub, tab string) {
	var names []string
	calls, down := 0, 0
	for _, t := range bad {
		d := tileDef(t.ID)
		names = append(names, d.Name)
		if d.Kind == profile.TileCall {
			calls++
		}
		if t.State == tiles.Down {
			down++
		}
	}
	switch d, t := tileDef(bad[0].ID), bad[0]; {
	case len(bad) == 1:
		switch {
		case t.State == tiles.Down && d.Kind == profile.TileCall:
			headline = d.Name + " isn’t connecting."
		case t.State == tiles.Down:
			headline = d.Name + " isn’t loading."
		case d.Kind == profile.TileCall:
			headline = d.Name + " calls may be choppy."
		default:
			headline = d.Name + " is slower than usual."
		}
		tab = "! " + d.Name + " " + strings.ToLower(c.word(t.State, d.Kind))
	case calls == len(bad) && down == 0:
		headline, tab = "Calls may be choppy.", "! Calls choppy"
	case calls == len(bad) && down == len(bad):
		headline, tab = "Calls aren’t connecting.", "! Calls down"
	case len(bad) <= 3:
		headline, tab = andJoin(names)+" have problems.", fmt.Sprintf("! %d apps with problems", len(bad))
	default:
		headline, tab = fmt.Sprintf("%d apps have problems.", len(bad)), fmt.Sprintf("! %d apps with problems", len(bad))
	}
	x := causes[0]
	same := !slices.ContainsFunc(causes, func(y tiles.Cause) bool { return y != x })
	switch x.Kind() {
	case tiles.KindApp, tiles.KindSpeedtest:
		headline = "Your internet is fine. " + headline
	case tiles.KindOther:
		if x.Who == tiles.WhoCallsPath || x.Who == tiles.WhoUnknown || x.Who == "" {
			headline = "Your internet is fine. " + headline
		}
	}
	switch {
	case !same && x.Who == tiles.WhoApp:
		sub = "Likely cause: the apps’ own side — your router and your internet provider look fine."
	case !same, x.Who == "", x.Who == tiles.WhoUnknown:
		sub = "Likely cause: unclear — your router and your internet provider look fine."
	case x.Evidence == tiles.EvAppOnly:
		sub = "Likely cause: " + c.cause(x) + " — every other app is working normally."
	case x.Evidence == tiles.EvVendorOnly:
		sub = "Likely cause: " + c.cause(x) + " — the non-" + vendorName(x.Vendor) + " apps are working normally."
	case x.Evidence == tiles.EvCallsPath:
		sub = "Likely cause: " + c.cause(x) + " — websites and other apps are working normally."
	case x.Kind() == tiles.KindApp, x.Who == tiles.WhoSpeedtest:
		sub = "Likely cause: " + c.cause(x) + " — " + c.tileEvidence(x) + "."
	default:
		sub = "Likely cause: " + c.cause(x) + " — " + c.evidence(x, true) + "."
	}
	// A meeting app that isn't connecting: name the ones that work.
	stuck := slices.ContainsFunc(bad, func(t tiles.TileSnap) bool {
		_, meeting := switchNames[t.ID]
		return meeting && t.State == tiles.Down
	})
	var alt []string
	for _, t := range all {
		if n, ok := switchNames[t.ID]; ok && stuck && t.State == tiles.Good {
			alt = append(alt, n)
		}
	}
	switch len(alt) {
	case 0:
	case 1:
		sub += " " + alt[0] + " is working if you need to switch."
	default:
		sub += " " + andJoin(alt) + " are working if you need to switch."
	}
	return headline, sub, tab
}

// pathCard is a broken path's card (rows 3 and 4 of the connection state).
func (c h2copy) pathCard(kind string, since time.Time) (headline, sub, tab string) {
	started := " Started at " + c.when(since) + "."
	switch kind {
	case "lan":
		if c.wifi {
			return "Your Wi-Fi or router is likely having trouble.",
				"fyisp can’t reliably reach your router, so every app is affected at once." + started, "✕ Wi-Fi or router problem"
		}
		return "Your home router is likely having trouble.",
			"fyisp can’t reliably reach your router, so every app is affected at once." + started, "✕ Router problem"
	case "isp":
		return "Your internet provider is likely having trouble.",
			"Your router is answering normally, but your provider’s network past it isn’t, so every app is affected at once. Calls may be choppy and some sites won’t load." + started,
			"✕ Provider problem"
	case "upstream":
		return "The internet past your provider is likely having trouble.",
			"Your router and your internet provider are answering normally, but sites past them are hard to reach, so many apps are affected at once." + started,
			"✕ Internet problem"
	case "no_network":
		return "fyisp has no network connection.",
			"The computer fyisp runs on isn’t connected to any network, so nothing can be checked." + started, "✕ Offline"
	}
	return "Your internet just dropped.", "Nothing is getting through right now." + started, "✕ Internet down"
}

// ---------- problems ----------

// problemWhen is "Today 9:41–9:45 am", "Today since 10:05 am (still
// happening)" or "Today 10:14 am (about 40 seconds)".
func (c h2copy) problemWhen(e tiles.Episode) string {
	if e.Ongoing() {
		return c.day(e.Start) + " since " + c.clock(e.Start) + " (still happening)"
	}
	d := e.End.Sub(e.Start)
	if e.DropDur > 0 {
		d = e.DropDur
	}
	if d < time.Minute {
		return c.day(e.Start) + " " + c.clock(e.Start) + " (" + about(d) + ")"
	}
	if c.daysAgo(e.Start) != c.daysAgo(e.End) {
		return c.day(e.Start) + " " + c.clock(e.Start) + " – " + c.dayIn(e.End) + " " + c.clock(e.End)
	}
	return c.day(e.Start) + " " + c.span(e.Start, e.End)
}

// problemWhat says what the user noticed.
func (c h2copy) problemWhat(e tiles.Episode) string {
	on := e.Ongoing()
	pick := func(past, present string) string {
		if on {
			return present
		}
		return past
	}
	if e.Connection {
		switch {
		case e.Kind == tiles.EpDrop:
			if on {
				return "Everything is dropping."
			}
			return "Everything dropped for " + about(e.DropDur) + "."
		case e.Kind == tiles.EpBlocked:
			return pick("Calls couldn’t use their fast path.", "Calls can’t use their fast path.")
		case e.Verdict == "dns":
			return pick("Websites couldn’t be found.", "Websites can’t be found.")
		case e.Verdict == "":
			return pick("Most apps were slow.", "Most apps are slow.")
		case e.Verdict == "upstream" && e.Sev == tiles.Down:
			return pick("Many sites didn’t load.", "Many sites aren’t loading.")
		case e.Verdict == "upstream":
			return pick("Many sites were slow.", "Many sites are slow.")
		case e.Sev == tiles.Down:
			return pick("Nothing could get through.", "Nothing is getting through.")
		}
		return pick("Calls may have been choppy and some sites slow.", "Calls choppy, some sites slow or not loading.")
	}
	var names []string
	calls, apps := 0, 0
	for _, id := range e.Tiles {
		d := tileDef(id)
		names = append(names, d.Name)
		if d.Kind == profile.TileCall {
			calls++
		} else {
			apps++
		}
	}
	who := andJoin(names)
	one := len(names) == 1
	switch {
	case e.Kind == tiles.EpDrop && !on:
		return who + " dropped for " + about(e.DropDur) + "."
	case e.Sev == tiles.Down && apps == 0:
		return pick(who+" couldn’t connect.", who+" not connecting.")
	case e.Sev == tiles.Down && calls == 0:
		return pick(who+" didn’t load.", who+" not loading.")
	case e.Sev == tiles.Down:
		return pick(who+" didn’t load.", who+" not loading.")
	case apps == 0:
		return pick(who+" calls may have been choppy.", who+" calls may be choppy.")
	case calls == 0 && one:
		return pick(who+" was slow to load.", who+" is slow to load.")
	case calls == 0:
		return pick(who+" were slow to load.", who+" are slow to load.")
	}
	return pick(who+" were slow or choppy.", who+" are slow or choppy.")
}

// problemsSummary is "5 problems: 2 likely from your internet provider,
// 1 from your home router, 2 from the apps themselves." Speed tests and
// unclear causes count in the total only.
func (c h2copy) problemsSummary(eps []tiles.Episode, learning bool, first time.Time) string {
	if len(eps) == 0 {
		if learning && !first.IsZero() {
			return "No problems since fyisp started at " + c.when(first) + "."
		}
		return "No problems in the last 7 days."
	}
	var prov, home, app int
	for _, e := range eps {
		switch e.Cause.Kind() {
		case tiles.KindProvider:
			prov++
		case tiles.KindHome:
			home++
		case tiles.KindApp:
			app++
		}
	}
	var parts []string
	if prov > 0 {
		parts = append(parts, fmt.Sprintf("%d from your internet provider", prov))
	}
	if home > 0 {
		parts = append(parts, fmt.Sprintf("%d from your home router", home))
	}
	if app > 0 {
		parts = append(parts, fmt.Sprintf("%d from the apps themselves", app))
	}
	s := count(len(eps), "problem")
	if len(parts) == 0 {
		return s + "."
	}
	n, rest, _ := strings.Cut(parts[0], " ")
	parts[0] = n + " likely " + rest
	return s + ": " + strings.Join(parts, ", ") + "."
}

// dayLine is the card's "Last 24 hours: …" line.
func (c h2copy) dayLine(n tiles.Counts) *Home2Day {
	if n.Total == 0 {
		return &Home2Day{Text: "Last 24 hours: no problems.", Link: ""}
	}
	link := count(n.Total, "short problem")
	var parts []string
	if n.Provider > 0 {
		parts = append(parts, fmt.Sprintf("%d likely from your internet provider", n.Provider))
	}
	if n.Home > 0 {
		w := "likely "
		if len(parts) > 0 {
			w = ""
		}
		parts = append(parts, fmt.Sprintf("%d %sfrom your home router", n.Home, w))
	}
	text := "Last 24 hours: " + link
	if len(parts) > 0 {
		text += ", " + strings.Join(parts, ", ")
	}
	return &Home2Day{Text: text + ".", Link: link}
}

// ---------- the speed line ----------

// mbps is "930", "4.2".
func mbps(v float64) string {
	if v < 10 {
		return strconv.FormatFloat(math.Round(v*10)/10, 'f', -1, 64)
	}
	return strconv.FormatFloat(math.Round(v), 'f', 0, 64)
}

// speedText is "Your speed: about 930 Mbps down / 940 Mbps up — about
// your usual speed." (no comparison without a usual).
func (c h2copy) speedText(down, up float64, usual *Home2SpeedUsual) string {
	s := "Your speed: about " + mbps(down) + " Mbps down / " + mbps(up) + " Mbps up"
	if usual == nil {
		return s + "."
	}
	r := min(down/usual.DownMbps, up/usual.UpMbps)
	was := " (usually about " + mbps(usual.DownMbps) + " / " + mbps(usual.UpMbps) + " Mbps)"
	switch {
	case r < 0.6:
		return s + " — much slower than usual" + was + "."
	case r < 0.85:
		return s + " — a bit slower than usual" + was + "."
	case down > 1.25*usual.DownMbps && up > 1.25*usual.UpMbps:
		return s + " — faster than usual."
	}
	return s + " — about your usual speed."
}

// speedNote is when the test ran: "Today 8:00 am".
func (c h2copy) speedNote(t time.Time, during bool) string {
	s := c.day(t) + " " + c.clock(t)
	if during {
		s += ", during the problem"
	}
	return s
}
