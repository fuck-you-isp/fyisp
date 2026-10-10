package web

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/fuck-you-isp/fyisp/internal/callq"
	"github.com/fuck-you-isp/fyisp/internal/model"
)

// Home's words (docs/HOME-API.md; wording from the v1.0 content review).
// Every string is in two voices: "you" for the owner, and the neutral third
// person for the share link, whose reader may be the provider's support.
// No string holds a time, a number of milliseconds or a technical term
// (TestHomeCopyWords keeps it that way).

// homeCopy picks the voice.
type homeCopy struct{ public bool }

func copyFor(voice string) homeCopy { return homeCopy{public: voice == voicePublic} }

// v returns the owner's or the public wording.
func (c homeCopy) v(you, public string) string {
	if c.public {
		return public
	}
	return you
}

func (c homeCopy) tileTitle(id string) string {
	return map[string]string{tileCalls: "Calls", tileWork: "Work apps", tileWeb: "Websites", tileOther: "Other services"}[id]
}

// callHead is a call state as a headline.
func (c homeCopy) callHead(st string, quiet bool) string {
	switch st {
	case string(callq.Good):
		return "Good for calls"
	case string(callq.Fair):
		return "Mostly fine for calls"
	case string(callq.Poor):
		return "Calls may be choppy"
	case string(callq.Unusable):
		return "Calls will break up"
	case callBlocked:
		if quiet {
			return "This network always blocks call traffic"
		}
		return "This network blocks call traffic"
	case callNotAnswering:
		return "Can't test calls right now"
	case "reachable":
		return "Call apps are reachable"
	case "not_answering":
		return "Some call apps aren't answering"
	}
	return "Checking…"
}

// callLabel is a call state in a word or two (badges, app rows).
func (c homeCopy) callLabel(st string) string {
	switch st {
	case string(callq.Good):
		return "Good"
	case string(callq.Fair):
		return "Some glitches"
	case string(callq.Poor):
		return "Choppy"
	case string(callq.Unusable):
		return "Breaking up"
	case callBlocked:
		return "Blocked"
	case callNotAnswering:
		return "Test servers down"
	case "reachable":
		return "Reachable"
	case "not_answering":
		return "Not answering"
	}
	return "Not checked yet"
}

func (c homeCopy) serversDown() string {
	return c.v("Our call test servers aren't answering. That isn't a problem with your connection.",
		"The call test servers aren't answering. That isn't a problem with this connection.")
}

// tileLabel: dead says most of the tile's targets get nothing through
// (rather than losing some of it).
func (c homeCopy) tileLabel(state string, other, dead bool) string {
	switch state {
	case tileWorking:
		return "Working"
	case tileMostly:
		return "Mostly working"
	case tileSlow:
		return "Slow"
	case tileDown:
		switch {
		case other && dead:
			return "Many aren't answering"
		case other:
			return "Many are unreliable"
		case dead:
			return "Not working"
		}
		return "Unreliable"
	}
	return "Checking…"
}

// displayNames are Home's names for targets, by target name: what a home
// user calls the service, never a host. A CDN stands for the websites that
// use it (see subject). Targets not listed fall back to their call app's
// title, then their name with spaces (homeName).
var displayNames = map[string]string{
	"M365-Outlook":    "Outlook",
	"M365-Portal":     "Microsoft 365",
	"M365-Login":      "Microsoft sign-in",
	"Google-Gmail":    "Gmail",
	"Google-Docs":     "Google Docs",
	"Google-Drive":    "Google Drive",
	"Microsoft-Teams": "Teams",
	"Google-Meet":     "Google Meet",
	"Zoom":            "Zoom",
	"Slack":           "Slack",
	"Webex":           "Webex",
	"Discord":         "Discord",
	"WhatsApp-Chat":   "WhatsApp",
	"CDN-Cloudflare":  cdnPrefix + "Cloudflare",
	"CDN-Akamai":      cdnPrefix + "Akamai",
	"CDN-Fastly":      cdnPrefix + "Fastly",
	"CDN-CloudFront":  cdnPrefix + "Amazon CloudFront",
	"CDN-Google":      cdnPrefix + "Google",
}

// cdnPrefix marks a display name that stands for the websites behind a CDN.
const cdnPrefix = "websites using "

// subject makes display names the subject of a sentence, capitalized:
// "Outlook", "Outlook and Gmail", "Some websites (those using Akamai and
// Fastly)". plural says which verb form follows.
func subject(ns []string) (s string, plural bool) {
	var cdn, parts []string
	for _, n := range ns {
		if c, ok := strings.CutPrefix(n, cdnPrefix); ok {
			cdn = append(cdn, c)
		} else {
			parts = append(parts, n)
		}
	}
	if len(cdn) > 0 {
		parts = append(parts, "some websites (those using "+joinNames(cdn)+")")
	}
	s = joinNames(parts)
	if s != "" {
		s = strings.ToUpper(s[:1]) + s[1:]
	}
	return s, len(ns) > 1 || len(cdn) > 0
}

// says is subject plus a verb in the right form: says(ns, " isn't", " aren't").
func says(ns []string, one, many string) string {
	s, plural := subject(ns)
	if plural {
		return s + many
	}
	return s + one
}

// joinNames joins display names: "A", "A and B", "A, B and 2 more".
func joinNames(ns []string) string {
	switch len(ns) {
	case 0:
		return ""
	case 1:
		return ns[0]
	case 2:
		return ns[0] + " and " + ns[1]
	}
	return ns[0] + ", " + ns[1] + " and " + strconv.Itoa(len(ns)-2) + " more"
}

func (c homeCopy) tileNote(state string, affected []string, failing, dead, measured int) string {
	// Failing targets that get nothing through "aren't answering"; the
	// others lose some of it.
	failWords := func() (string, string) {
		if dead >= failing {
			return " isn't answering", " aren't answering"
		}
		return " isn't loading reliably", " aren't loading reliably"
	}
	switch state {
	case tileMostly:
		if failing > 0 {
			one, many := failWords()
			return says(affected[:1], one, many) + " from here. That's usually a problem on their side."
		}
		return says(affected[:1], " is", " are") + " slower than usual."
	case tileSlow:
		if failing == 0 {
			return says(affected, " is", " are") + " slower than usual."
		}
		return says(affected, " isn't", " aren't") + " working well from here."
	case tileDown:
		one, many := failWords()
		if len(affected) > 3 {
			return fmt.Sprintf("%d of %d%s from here.", len(affected), measured, many)
		}
		return says(affected, one, many) + " from here."
	}
	return ""
}

func verb(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// otherNote is the Other services tile's note: counts only.
func (c homeCopy) otherNote(state string, affected, failing, dead, total int) string {
	if affected == 0 {
		return ""
	}
	what := " aren't working well from here."
	switch {
	case failing > 0 && dead >= failing:
		what = " aren't answering from here."
	case failing > 0:
		what = " aren't loading reliably from here."
	case failing == 0:
		what = " are slower than usual."
	}
	if affected == 1 {
		what = strings.NewReplacer("aren't", "isn't", " are ", " is ").Replace(what)
	}
	return thousands(affected) + " of " + thousands(total) + what
}

// thousands writes n with thousands separators: 1,330.
func thousands(n int) string {
	s := strconv.Itoa(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// callPatch is a calls-only rough patch (no incident explains it).
func (c homeCopy) callPatch(level string) string {
	if level == levelBad {
		return c.v("Calls were choppy. Nothing on your side looked wrong.", "Calls were choppy. Nothing on this side looked wrong.")
	}
	return c.v("Calls had some glitches. Nothing on your side looked wrong.", "Calls had some glitches. Nothing on this side looked wrong.")
}

func (c homeCopy) otherLine(n int) string {
	if n == 1 {
		return "1 other service isn't working well from here."
	}
	return strconv.Itoa(n) + " other services aren't working well from here."
}

func (c homeCopy) sharedPath(label string) string {
	return c.v("Calls use your call path ("+label+").", "Calls use the same call path ("+label+").")
}

func (c homeCopy) picksServer(app string) string {
	return c.v(app+" picks a server when each meeting starts, so we can't test it ahead of time.",
		app+" picks a server when each meeting starts, so it can't be tested ahead of time.")
}

var (
	delayWords      = map[string]string{"very_short": "very short", "short": "short", "noticeable": "noticeable", "long": "long"}
	dropoutWords    = map[string]string{"none": "none", "rare": "rare", "some": "some", "frequent": "frequent"}
	smoothnessWords = map[string]string{"steady": "steady", "bit_uneven": "a bit uneven", "uneven": "uneven"}
)

func (c homeCopy) callWords(delay, drop, smooth string) string {
	var out []string
	if delay != "" {
		out = append(out, "Delay "+delayWords[delay])
	}
	if drop != "" {
		out = append(out, "Dropouts "+dropoutWords[drop])
	}
	if smooth != "" {
		out = append(out, "Smoothness "+smoothnessWords[smooth])
	}
	return strings.Join(out, " · ")
}

// ---------- path strip ----------

func (c homeCopy) nodeLabel(id string) string {
	switch id {
	case "device":
		return c.v("This device", "Monitoring device")
	case "router":
		return c.v("Your router", "Home router")
	case "provider":
		return c.v("Your provider", "Provider network")
	case "internet":
		return c.v("The internet", "The wider internet")
	}
	return "Apps & websites"
}

func (c homeCopy) nodeTip(id string) string {
	switch id {
	case "router":
		return c.v("The box that connects your home to the internet.", "The router that connects the home to the internet.")
	case "provider":
		return c.v("The first piece of your internet provider's equipment, just outside your home.",
			"The provider's first piece of equipment past the home router.")
	case "internet":
		return "Big services that are close to almost every provider. If they're fine, the internet itself is fine."
	case "apps":
		return c.v("The apps and websites we check for you.", "The apps and websites checked from this connection.")
	}
	return ""
}

func (c homeCopy) nodeWords(id, state string, loss float64, slow bool) string {
	switch state {
	case "ok":
		return "OK"
	case "unknown":
		if id == "provider" {
			return c.v("Can't see it from here", "Not visible from here")
		}
		return "No recent checks"
	case levelWarn:
		if loss >= 0.05 || !slow {
			return "Dropping some data"
		}
		return "Slow"
	case levelBad:
		if id == "device" {
			return "Offline"
		}
		if loss >= 0.99 {
			return "Not answering"
		}
		return "Dropping data"
	}
	return ""
}

func (c homeCopy) appsWords(state string, affected int) string {
	switch {
	case state == "unknown":
		return "Checking…"
	case state == levelBad:
		return "Most aren't answering"
	case affected == 1:
		return "1 app has problems"
	case affected > 1:
		return strconv.Itoa(affected) + " apps have problems"
	case state == levelWarn:
		return "Some have problems"
	}
	return "OK"
}

// ---------- the answer ----------

func (c homeCopy) checking() (headline, cause string) {
	return c.v("Checking your connection…", "Checking this connection…"),
		c.v("This takes about a minute the first time.", "Results appear after about a minute.")
}

func (c homeCopy) fine(home bool) string {
	return c.v("Your internet is fine", "This connection is fine")
}

func (c homeCopy) everything(home bool) string {
	if !home {
		return c.v("Everything we check is working.", "Everything checked is working.")
	}
	return "Calls, work apps and websites are all working."
}

func (c homeCopy) verdictHead(k model.VerdictKind, quiet bool, affected []string, onlyOther bool) string {
	switch k {
	case model.VerdictLAN, model.VerdictISP:
		// The cause names the place; the headline says what it means.
		return c.v("Your internet is having trouble", "This connection is having trouble")
	case model.VerdictUpstream:
		return "Many websites and apps are hard to reach"
	case model.VerdictDNS:
		return "Websites and apps may not open"
	case model.VerdictNoNetwork:
		return c.v("No internet connection", "The monitoring device is offline")
	case model.VerdictService:
		if onlyOther || len(affected) > 2 {
			return "Some services aren't answering"
		}
		return says(affected, " isn't", " aren't") + " answering"
	case model.VerdictUDPBlocked:
		if quiet {
			return "This network always blocks the fast connection calls use"
		}
		return "This network blocks the fast connection calls use"
	case model.VerdictSTUNBlocked:
		if quiet {
			return "This network always blocks some of the connections calls use"
		}
		return "This network blocks some of the connections calls use"
	case model.VerdictUDPDegraded:
		return "Calls may sound choppy"
	}
	return c.fine(true)
}

func (c homeCopy) serviceNote(affected []string) string {
	return says(affected, " isn't", " aren't") + " answering from here. That's most likely a problem on their side."
}

func (c homeCopy) verdictCause(k model.VerdictKind, conf string, quiet bool, affected []string, onlyOther bool) string {
	switch k {
	case model.VerdictLAN:
		switch conf {
		case confSure:
			return c.v("The problem is inside your home: your Wi-Fi or router. Your internet provider looks fine from here.",
				"The problem is in the home network (Wi-Fi or router). The provider's network looks fine from here.")
		case confWeak:
			return c.v("Your router is slow to answer our checks. Your calls and apps aren't affected right now.",
				"The home router is slow to answer checks. Calls and apps aren't affected right now.")
		}
		return c.v("The problem is most likely inside your home: your Wi-Fi or router. Your internet provider looks fine from here.",
			"The problem is most likely in the home network (Wi-Fi or router). The provider's network looks fine from here.")
	case model.VerdictISP:
		switch conf {
		case confSure:
			return c.v("The problem is with your internet provider. Your Wi-Fi and router are fine.",
				"The problem is in the provider's network. The home network (Wi-Fi and router) is fine.")
		case confWeak:
			return c.v("Your internet provider's equipment is slow to answer our checks. Your calls and apps aren't affected right now.",
				"The provider's equipment is slow to answer checks. Calls and apps aren't affected right now.")
		}
		return c.v("The problem is most likely with your internet provider. Your Wi-Fi and router look fine.",
			"The problem is most likely in the provider's network. The home network (Wi-Fi and router) looks fine.")
	case model.VerdictUpstream:
		return c.v("Your Wi-Fi and your provider's nearest equipment look fine. The problem is further out: either your provider's links to the rest of the internet, or a wider outage.",
			"The home network and the provider's nearest equipment look fine. The problem is further out: either the provider's links to the rest of the internet, or a wider outage.")
	case model.VerdictDNS:
		return c.v("Your connection works, but looking up apps and websites by name is failing. That lookup service is usually run by your router or your provider.",
			"The connection works, but name lookups (DNS) are failing. That service is usually run by the home router or the provider.")
	case model.VerdictNoNetwork:
		return c.v("The device running this monitor is offline.", "The device running this monitor has no network connection.")
	case model.VerdictService:
		// The headline names them; this says whose problem it is.
		return c.v("Your connection looks fine, so this is most likely a problem on their side.",
			"The connection looks fine, so this is most likely a problem on the services' side.")
	case model.VerdictUDPBlocked:
		return c.v("Calls may still connect, but sound and video can lag or drop. This is common on hotel, office and school Wi-Fi, or with some security software.",
			"Calls may still connect, but sound and video can lag or drop. This is common on hotel, office and school Wi-Fi, or with security software on the device.")
	case model.VerdictSTUNBlocked:
		return "Most call apps will still work, but may be slower to connect or lower quality."
	case model.VerdictUDPDegraded:
		return c.pathCause("")
	}
	return ""
}

// pathCause explains a bad call path with no layer verdict.
func (c homeCopy) pathCause(factor string) string {
	what := c.v("the connection calls use", "call traffic")
	switch factor {
	case "loss":
		what += " is losing bits of sound and video."
	case "jitter":
		what += " is delivering sound and video unevenly."
	case "latency":
		what += " has a long delay."
	default:
		what += " is losing bits of sound and video, or delivering them unevenly."
	}
	return "Browsing looks fine, but " + what + c.v(" We didn't see a cause on your side.", " No cause was seen in the home network.")
}

func (c homeCopy) appWorse(app string) string {
	return c.v(app+" is worse than your other call apps right now.", app+" is worse than the other call apps right now.")
}

// tileWhat names a tile's things in a sentence.
var tileWhat = map[string]string{tileWork: "work apps", tileWeb: "websites", tileOther: "services"}

func (c homeCopy) tileHead(t HomeTile) string {
	if t.State == tileDown {
		return "Some " + tileWhat[t.ID] + " aren't loading"
	}
	return "Some " + tileWhat[t.ID] + " are slow"
}

func (c homeCopy) tileCause(t HomeTile) string {
	return c.v("Your connection looks fine, so this is most likely a problem on their side.",
		"The connection looks fine, so this is most likely a problem on the services' side.")
}

func (c homeCopy) quietHint() string {
	return c.v("It's been this way for a while. We'll keep checking, but there's nothing new.",
		"It has been this way for a while; there's nothing new.")
}

// try lists what the owner can do (nothing in the public voice).
func (c homeCopy) try(who string, k model.VerdictKind, conf string) []string {
	if c.public {
		return []string{}
	}
	switch who {
	case whoHome:
		if conf == confWeak {
			return []string{}
		}
		return []string{"Move closer to your router, or plug in a cable.", "Restart the router.",
			"If other people are streaming or downloading, try again when they stop."}
	case whoProvider:
		if conf == confWeak {
			return []string{}
		}
		return []string{"Restarting your router sometimes helps.", "If it keeps happening, make a report and send it to your provider."}
	case whoBeyond:
		return []string{"There's usually nothing to fix at home.", "If it lasts, check your provider's status page, or make a report."}
	case whoDNS:
		return []string{"Restart your router.", "If you changed your DNS settings, change them back."}
	case whoOffline:
		return []string{"Check that Wi-Fi is on or the cable is plugged in.", "Restart your router."}
	case whoCallTraffic:
		if k == model.VerdictUDPBlocked || k == model.VerdictSTUNBlocked {
			return []string{"Try another network (for example, your phone's hotspot) to compare.",
				"If it's your home network, check your router's firewall or security app settings."}
		}
		return []string{"Use a cable instead of Wi-Fi if you can.", "Pause big downloads or video streams during calls.", "If it lasts, make a report."}
	}
	return []string{}
}

// fact is the worst minute in words. loss is a fraction.
func (c homeCopy) fact(factor string, loss float64) string {
	switch factor {
	case "jitter":
		return "In the worst minute, sound arrived unevenly."
	case "latency":
		return "In the worst minute, there was a long delay."
	}
	switch {
	case loss >= 0.9:
		return "In the worst minute, almost all of the sound was lost."
	case loss >= 0.4:
		return "In the worst minute, about half of the sound was lost."
	case loss >= 0.01:
		return "In the worst minute, about 1 in " + strconv.Itoa(int(1/loss+0.5)) + " bits of sound were lost."
	case loss > 0:
		return "In the worst minute, a few bits of sound were lost."
	}
	return "In the worst minute, calls would have sounded worse than usual."
}

// ---------- history ----------

func (c homeCopy) runLabel(who, name string) string {
	switch who {
	case whoHome:
		return "Home network"
	case whoProvider:
		return c.v("Your provider", "Provider network")
	case whoBeyond:
		return "Further out"
	case whoDNS:
		return "Name lookups"
	case whoApp:
		if name != "" {
			n, _ := subject([]string{name})
			return n
		}
		return "Some apps"
	case whoCallTraffic:
		return "Call traffic"
	case whoOffline:
		return "Offline"
	}
	return ""
}

func (c homeCopy) where(who, name string) string {
	switch who {
	case whoHome:
		return c.v("Your home network (Wi-Fi or router)", "Home network (Wi-Fi or router)")
	case whoProvider:
		return c.v("Your provider's network (past your router)", "Provider network (past the router)")
	case whoBeyond:
		return c.v("Further out on the internet", "Further out than the provider's first hop")
	case whoDNS:
		return c.v("Looking up names", "Name lookups (DNS)")
	case whoApp:
		if name != "" {
			n, _ := subject([]string{name})
			return n
		}
		return "Some apps and websites"
	case whoCallTraffic:
		return c.v("The connection calls use", "Call traffic")
	case whoOffline:
		return c.v("No internet connection", "The monitoring device was offline")
	}
	return ""
}

// summary is the one sentence under the timeline (no times: the client
// lists them).
func (c homeCopy) summary(n HomeCounts, span string, fresh bool) string {
	period := "today"
	if span == "w" {
		period = "this week"
	}
	patches := ""
	if n.CallPatches > 0 {
		patches = "Calls had " + strings.Replace(plural(n.CallPatches, "rough patch"), "patchs", "patches", 1) + " today"
	}
	switch {
	case fresh && n.Problems == 0 && patches == "":
		return c.v("We just started, so the history begins now.", "Monitoring just started, so the history begins now.")
	case n.Problems == 0 && patches != "" && span == "w":
		return "No other problems in the last 7 days. " + patches + "."
	case n.Problems == 0 && patches != "":
		return patches + c.v(", with nothing wrong on your side.", ", with nothing wrong on this side.")
	case n.Problems == 0 && span == "w":
		return "No problems in the last 7 days."
	case n.Problems == 0:
		return "No problems today."
	}
	also := ""
	if n.CallPatches > 0 {
		also = " Calls also had " + strings.Replace(plural(n.CallPatches, "other rough patch"), "patchs", "patches", 1) + " today."
	}
	type part struct {
		n      int
		phrase string
	}
	parts := []part{
		{n.Provider, c.v("from your internet provider", "in the provider's network")},
		{n.Home, c.v("in your home network", "in the home network")},
		{n.Beyond, "further out on the internet"},
		{n.CallTraffic, c.v("with the connection calls use", "with call traffic")},
		{n.DNS, "with name lookups"},
		{n.App, "with apps or websites"},
		{n.Offline, c.v("with no connection", "while the device was offline")},
	}
	var used []part
	for _, p := range parts {
		if p.n > 0 {
			used = append(used, p)
		}
	}
	head := plural(n.Problems, "problem") + " " + period
	if len(used) == 1 {
		switch n.Problems {
		case 1:
			return head + ", " + used[0].phrase + "." + also
		case 2:
			return head + ", both " + used[0].phrase + "." + also
		}
		return head + ", all " + used[0].phrase + "." + also
	}
	slices.SortStableFunc(used, func(a, b part) int { return b.n - a.n })
	var bits []string
	rest := 0
	for i, p := range used {
		if i >= 2 && len(used) > 3 {
			rest += p.n
			continue
		}
		bits = append(bits, strconv.Itoa(p.n)+" "+p.phrase)
	}
	if rest > 0 {
		bits = append(bits, strconv.Itoa(rest)+verb(rest, " other", " others"))
	}
	return head + ": " + strings.Join(bits[:len(bits)-1], ", ") + " and " + bits[len(bits)-1] + "." + also
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return strconv.Itoa(n) + " " + word + "s"
}

// atWords is "what happened then": the call state in the past tense, and
// where the problem was (past observation: "looked fine", never "was").
func (c homeCopy) atWords(measured bool, calls string, incident bool, who, kind string) (headline, cause string) {
	switch {
	case !measured && !incident:
		return c.v("We weren't checking at that time", "Nothing was being checked at that time"),
			c.v("The monitor wasn't running, so there's no record. Leave it running to catch the next one.",
				"The monitor wasn't running, so there's no record.")
	case !measured:
		headline = c.v("We weren't checking calls then, but we did see a problem", "Calls weren't being checked then, but there was a problem")
	case calls == "" && incident:
		headline = c.v("There was a problem then", "There was a problem then")
	case calls == "":
		return c.v("Everything we check was working then", "Everything checked was working then"), ""
	default:
		headline = map[string]string{
			string(callq.Good):     c.v("Your connection was good for calls then", "The connection was good for calls then"),
			string(callq.Fair):     "Mostly fine for calls, with a few glitches",
			string(callq.Poor):     "Calls would have been choppy",
			string(callq.Unusable): "Calls would have broken up",
			callBlocked:            "This network was blocking call traffic",
			callNotAnswering:       c.v("Our call test servers weren't answering then", "The call test servers weren't answering then"),
		}[calls]
		if headline == "" {
			headline = c.v("We weren't checking calls then", "Calls weren't being checked then")
		}
	}
	switch {
	case incident && calls == callBlocked:
		cause = "Calls may still have connected, through a slower backup route."
	case incident:
		cause = c.wasWhere(who, model.VerdictKind(kind))
	case calls == string(callq.Good) || calls == callNotAnswering:
		cause = c.v("If the call was bad, the problem was probably the app or the other person's connection.",
			"If a call was bad then, the problem was probably the app or the other side's connection.")
	default:
		cause = c.v("We didn't see a cause on your side: your Wi-Fi, router and provider looked fine.",
			"No cause was seen on this side: the home network and the provider looked fine.")
	}
	return headline, cause
}

// wasWhere is a past incident's place, in a sentence.
func (c homeCopy) wasWhere(who string, k model.VerdictKind) string {
	switch who {
	case whoHome:
		return c.v("The problem was most likely your Wi-Fi or router.", "The problem was most likely in the home network (Wi-Fi or router).")
	case whoProvider:
		return c.v("The problem was most likely with your internet provider. Your Wi-Fi and router looked fine.",
			"The problem was most likely in the provider's network. The home network looked fine.")
	case whoBeyond:
		return c.v("Your Wi-Fi, router and provider's nearest equipment looked fine. The problem was further out on the internet.",
			"The home network and the provider's nearest equipment looked fine. The problem was further out on the internet.")
	case whoDNS:
		return "At the same time, apps and websites couldn't be found by name."
	case whoOffline:
		return c.v("The device running this monitor was offline.", "The monitoring device was offline.")
	case whoApp:
		return "Some online services had trouble at the time. That was on their side."
	case whoCallTraffic:
		if k == model.VerdictUDPBlocked || k == model.VerdictSTUNBlocked {
			return "The network was blocking call traffic."
		}
		return c.v("Your Wi-Fi, router and provider looked fine. The problem was on the way to the call servers.",
			"The home network and the provider looked fine. The problem was on the way to the call servers.")
	}
	return ""
}
