package report

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

// kindInfo describes a verdict kind in plain English.
type kindInfo struct {
	Label   string // outage log
	Subject string // summary sentences
	Color   string
}

var kinds = map[model.VerdictKind]kindInfo{
	model.VerdictISP:       {"ISP problem", "Your ISP's network", "#cc2f2f"},
	model.VerdictUpstream:  {"Internet problem beyond your ISP", "The internet beyond your ISP (its upstream links or peering)", "#d97a14"},
	model.VerdictLAN:       {"Home network problem", "Your home network (Wi-Fi, cables or router)", "#d0601a"},
	model.VerdictNoNetwork: {"No network", "This device's own connection", "#8a1c1c"},
	model.VerdictDNS:       {"DNS problem", "Name resolution (DNS)", "#5b4bc8"},
	model.VerdictService:   {"Some services down", "Individual services (their fault, not your ISP's)", "#b58300"},
}

// kindOrder is the order of the summary: the ISP first.
var kindOrder = []model.VerdictKind{model.VerdictISP, model.VerdictUpstream, model.VerdictLAN,
	model.VerdictNoNetwork, model.VerdictDNS, model.VerdictService}

func kindOf(k model.VerdictKind) kindInfo {
	if i, ok := kinds[k]; ok {
		return i
	}
	return kindInfo{Label: string(k), Subject: "Other problems (" + string(k) + ")", Color: "#6f6e69"}
}

// Light-theme colours, as the dashboard's light theme.
var seriesColors = []string{"#2a78d6", "#eb6834", "#1baf7a", "#eda100", "#e87ba4", "#008300", "#4a3aa7", "#e34948"}

var reasonColors = map[model.Reason]string{
	model.ReasonTimeout: "#d03b3b", model.ReasonRefused: "#ec835a", model.ReasonReset: "#ec835a",
	model.ReasonUnreachable: "#e87ba4", model.ReasonDNS: "#6a5bd6", model.ReasonTLS: "#e0a000",
	model.ReasonHTTP: "#1baf7a", model.ReasonNoNetwork: "#2a8fc4", model.ReasonOther: "#b08968",
}

func reasonColor(r model.Reason) string {
	if c, ok := reasonColors[r]; ok {
		return c
	}
	return reasonColors[model.ReasonOther]
}

// ---------- formatting ----------

func fmtDur(d time.Duration) string {
	if d < time.Minute {
		return strconv.Itoa(int(d.Round(time.Second)/time.Second)) + "s"
	}
	d = d.Round(time.Minute)
	days, h, m := int(d/(24*time.Hour)), int(d/time.Hour)%24, int(d/time.Minute)%60
	switch {
	case days > 0 && h > 0:
		return fmt.Sprintf("%dd %dh", days, h)
	case days > 0:
		return fmt.Sprintf("%dd", days)
	case h > 0 && m > 0:
		return fmt.Sprintf("%dh %dm", h, m)
	case h > 0:
		return fmt.Sprintf("%dh", h)
	}
	return fmt.Sprintf("%dm", m)
}

func fmtPct(f float64) string {
	switch {
	case math.IsNaN(f):
		return "–"
	case f <= 0:
		return "0%"
	case f < 0.001:
		return "<0.1%"
	case f >= 0.995 && f < 1:
		return ">99%"
	case f < 0.1:
		return strconv.FormatFloat(f*100, 'f', 1, 64) + "%"
	}
	return strconv.FormatFloat(f*100, 'f', 0, 64) + "%"
}

func fmtMs(v float64) string {
	switch {
	case math.IsNaN(v) || v <= 0:
		return "–"
	case v < 10:
		return strconv.FormatFloat(v, 'f', 1, 64) + " ms"
	}
	return strconv.FormatFloat(v, 'f', 0, 64) + " ms"
}

func fmtRatio(r float64) string {
	if r <= 0 || math.IsNaN(r) {
		return "–"
	}
	return strconv.FormatFloat(r, 'f', 1, 64) + "×"
}

func fmtLongTime(t time.Time) string { return t.UTC().Format("Mon 2 Jan 2006 15:04 UTC") }

func fmtTableTime(t time.Time) string { return t.UTC().Format("2006-01-02 15:04:05") }

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}

// utcDays returns the UTC calendar days touched by [a, b] (at least a's).
func utcDays(a, b time.Time) []time.Time {
	d := a.UTC().Truncate(24 * time.Hour)
	out := []time.Time{d}
	for d = d.Add(24 * time.Hour); d.Before(b); d = d.Add(24 * time.Hour) {
		out = append(out, d)
	}
	return out
}

// ---------- summary ----------

type kindTotal struct {
	Kind     model.VerdictKind
	Info     kindInfo
	Count    int
	Dur      time.Duration
	Days     int
	PeakLoss float64
}

func (r *run) kindTotals() []kindTotal {
	byKind := map[model.VerdictKind]*kindTotal{}
	days := map[model.VerdictKind]map[time.Time]bool{}
	for _, in := range r.incidents {
		t := byKind[in.Kind]
		if t == nil {
			t = &kindTotal{Kind: in.Kind, Info: kindOf(in.Kind)}
			byKind[in.Kind] = t
			days[in.Kind] = map[time.Time]bool{}
		}
		t.Count++
		t.Dur += in.dur()
		t.PeakLoss = max(t.PeakLoss, in.PeakLoss)
		for _, d := range utcDays(in.start, in.end) {
			days[in.Kind][d] = true
		}
	}
	var out []kindTotal
	seen := map[model.VerdictKind]bool{}
	for _, k := range kindOrder {
		if t := byKind[k]; t != nil {
			t.Days = len(days[k])
			out = append(out, *t)
			seen[k] = true
		}
	}
	var rest []model.VerdictKind
	for k := range byKind {
		if !seen[k] {
			rest = append(rest, k)
		}
	}
	sort.Slice(rest, func(i, j int) bool { return rest[i] < rest[j] })
	for _, k := range rest {
		t := byKind[k]
		t.Days = len(days[k])
		out = append(out, *t)
	}
	return out
}

// problemTime is the union of all incidents' time in the range.
func (r *run) problemTime() time.Duration {
	type iv struct{ a, b time.Time }
	var ivs []iv
	for _, in := range r.incidents {
		ivs = append(ivs, iv{in.start, in.end})
	}
	sort.Slice(ivs, func(i, j int) bool { return ivs[i].a.Before(ivs[j].a) })
	var total time.Duration
	var cur *iv
	for i := range ivs {
		x := ivs[i]
		if cur == nil || x.a.After(cur.b) {
			if cur != nil {
				total += cur.b.Sub(cur.a)
			}
			cur = &x
			continue
		}
		if x.b.After(cur.b) {
			cur.b = x.b
		}
	}
	if cur != nil {
		total += cur.b.Sub(cur.a)
	}
	return total
}

// summary returns the headline and the other findings.
func (r *run) summary(totals []kindTotal) (string, []string) {
	var lines []string
	nDays := len(utcDays(r.from, r.to))
	for _, t := range totals {
		verb := "had problems"
		if t.PeakLoss > 0 {
			verb = "dropped packets"
		}
		if t.Kind == model.VerdictService {
			verb = "failed"
		}
		what := fmt.Sprintf("%s in %s", fmtDur(t.Dur), plural(t.Count, "incident", "incidents"))
		var s string
		if nDays > 1 {
			s = fmt.Sprintf("%s %s on %d of %d days (%s", t.Info.Subject, verb, t.Days, nDays, what)
		} else {
			s = fmt.Sprintf("%s %s (%s", t.Info.Subject, verb, what)
		}
		if t.PeakLoss > 0 {
			s += fmt.Sprintf(", losing up to %s of probes in a minute", fmtPct(t.PeakLoss))
		}
		lines = append(lines, s+").")
	}
	var headline string
	if len(lines) > 0 {
		headline, lines = lines[0], lines[1:]
	} else if r.hasData {
		headline = "No problems were detected in this range."
	} else {
		headline = "No data: fyisp has no measurements in this range."
	}
	if len(r.incidents) > 0 {
		worst := r.incidents[0]
		for _, in := range r.incidents {
			if in.dur() > worst.dur() {
				worst = in
			}
		}
		s := fmt.Sprintf("The longest incident (%s) started %s and lasted %s",
			kindOf(worst.Kind).Label, fmtLongTime(worst.Start), fmtDur(worst.dur()))
		if worst.ongoing {
			s += " (still ongoing)"
		}
		if worst.PeakLoss > 0 {
			s += fmt.Sprintf(", with up to %s loss", fmtPct(worst.PeakLoss))
		}
		s += "."
		lines = append(lines, s)
	}
	rangeDur := r.to.Sub(r.from)
	if r.hasData && rangeDur > 0 {
		ok := 1 - float64(r.problemTime())/float64(rangeDur)
		s := fmt.Sprintf("No problem was detected during %s of the range", fmtShare(ok))
		if r.measured >= 0 && r.measured < 0.995 {
			s += fmt.Sprintf("; fyisp was measuring during %s of it (the rest is “not measured”, never counted as loss)", fmtShare(r.measured))
		}
		lines = append(lines, s+".")
	}
	lines = append(lines, r.baselineLines()...)
	return headline, lines
}

func fmtShare(f float64) string {
	f = math.Max(0, math.Min(1, f))
	if f > 0.999 && f < 1 {
		return ">99.9%"
	}
	return strconv.FormatFloat(f*100, 'f', 1, 64) + "%"
}

// baselineLines compares latency with the user's normal.
func (r *run) baselineLines() []string {
	if !r.baselines {
		return nil
	}
	var out []string
	var slow []*group
	for _, g := range r.groups {
		if g.ratio >= 1.25 {
			slow = append(slow, g)
		}
	}
	sort.SliceStable(slow, func(i, j int) bool { return slow[i].ratio > slow[j].ratio })
	for i, g := range slow {
		if i == 3 {
			break
		}
		out = append(out, fmt.Sprintf("Latency to %s was %s your normal (typically %s).", g.g.Title, fmtRatio(g.ratio), fmtMs(g.typical)))
	}
	var win *group
	for _, g := range r.groups {
		if g.winRatio >= 1.3 && (win == nil || g.winRatio > win.winRatio) {
			win = g
		}
	}
	if win != nil {
		out = append(out, fmt.Sprintf("The worst time of day was %02d:00–%02d:00 UTC, when latency to %s was %s your normal.",
			win.winHour, (win.winHour+3)%24, win.g.Title, fmtRatio(win.winRatio)))
	}
	if len(out) == 0 {
		out = append(out, "Latency stayed within your normal range (the week before this report) for every group.")
	}
	return out
}

// ---------- method ----------

func fmtIv(d time.Duration) string {
	if d%time.Second == 0 {
		return strconv.Itoa(int(d/time.Second)) + " s"
	}
	return d.String()
}

func ivText(set map[time.Duration]bool) string {
	var ds []time.Duration
	for d := range set {
		ds = append(ds, d)
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	switch len(ds) {
	case 0:
		return ""
	case 1:
		return "every " + fmtIv(ds[0])
	}
	return "every " + fmtIv(ds[0]) + " to " + fmtIv(ds[len(ds)-1])
}

func (r *run) method() []string {
	p := r.prof
	var services, path, anycast, traced int
	tcp, https, icmp := map[time.Duration]bool{}, map[time.Duration]bool{}, map[time.Duration]bool{}
	pathIcmp, anyIcmp, anyTCP := map[time.Duration]bool{}, map[time.Duration]bool{}, map[time.Duration]bool{}
	for _, t := range p.Targets {
		iv := t.Interval
		if iv <= 0 {
			iv = 15 * time.Second
		}
		switch t.Layer {
		case model.LayerGateway, model.LayerEdge:
			path++
			pathIcmp[iv/3] = true
			continue
		case model.LayerAnycast:
			anycast++
			for _, k := range t.ProbeKinds() {
				switch k {
				case model.KindICMP:
					anyIcmp[iv/3] = true
				case model.KindTCP:
					anyTCP[iv] = true
				}
			}
			continue
		}
		services++
		for _, k := range t.ProbeKinds() {
			switch k {
			case model.KindTCP:
				tcp[iv] = true
			case model.KindHTTPS:
				https[iv] = true
			case model.KindICMP:
				icmp[iv/3] = true
			}
		}
		if t.Trace {
			traced++
		}
	}
	var out []string
	s := fmt.Sprintf("fyisp measured %s in %s from this connection", plural(services, "target", "targets"), plural(len(p.Groups), "group", "groups"))
	var parts []string
	if len(tcp) > 0 {
		parts = append(parts, "TCP connects (time to open a connection) "+ivText(tcp))
	}
	if len(https) > 0 {
		parts = append(parts, "HTTPS requests (time to the first response byte) "+ivText(https))
	}
	if len(icmp) > 0 {
		parts = append(parts, "ICMP echo (ping) "+ivText(icmp))
	}
	if len(parts) > 0 {
		s += ": " + strings.Join(parts, ", and ")
	}
	s += "."
	if path > 0 {
		s += " Your router (the gateway) and the first hop on your ISP's side (the ISP edge) are pinged " + ivText(pathIcmp) + ", so a problem can be placed in your home, at your ISP or beyond it."
	}
	if anycast > 0 {
		var how []string
		if len(anyIcmp) > 0 {
			how = append(how, "pinged "+ivText(anyIcmp))
		}
		if len(anyTCP) > 0 {
			how = append(how, "connected to over TCP "+ivText(anyTCP))
		}
		if len(how) > 0 {
			s += fmt.Sprintf(" %s (big anycast DNS resolvers) %s %s.", plural(anycast, "public resolver", "public resolvers"), map[bool]string{true: "is", false: "are"}[anycast == 1], strings.Join(how, " and "))
		}
	}
	out = append(out, s)
	if traced > 0 {
		out = append(out, fmt.Sprintf("%s also %s an always-on traceroute, which shows loss and latency at every router on the way and when the route changes.",
			plural(traced, "target", "targets"), map[bool]string{true: "has", false: "have"}[traced == 1]))
	}
	out = append(out, "All probes run without administrator rights (unprivileged ICMP sockets). A lost probe is one that failed: a timeout, a refused or reset connection, an unreachable network, a failed DNS lookup or TLS handshake. Time when fyisp was not running or the computer was asleep is “not measured” and is never counted as loss.")
	out = append(out, "Problems are classified automatically every few seconds by comparing the layers of the path (see “How to read this report” at the end). All times are in UTC.")
	return out
}
