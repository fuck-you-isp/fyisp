package web

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/callq"
	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/store"
)

// Calls view limits. A window is scored in one-minute buckets from the raw
// tier, so it is at most store.RawMaxRange long (from is moved up), and at
// least CallsMinRange (from is moved back) so that buckets are whole minutes.
const (
	CallsMaxRange = store.RawMaxRange
	CallsMinRange = 5 * time.Minute
	// callsMinSamples: fewer bursts (answered or timed out) in the window
	// than this is "not enough data yet".
	callsMinSamples = 10
)

// Verdict kinds of the UDP checks (internal/verdict): lower priority than
// every path kind, so they are current only while the path is healthy.
const (
	verdictUDPBlocked  model.VerdictKind = "udp_blocked"
	verdictSTUNBlocked model.VerdictKind = "stun_blocked"
	verdictUDPDegraded model.VerdictKind = "udp_degraded"
)

// Call states, worst first (callRank). The bands are callq's.
const (
	callBlocked       = "blocked"
	callNotAnswering  = "server_not_answering"
	callNotEnoughData = "not_enough_data"
)

// callRank orders call states worst first. Blocked outranks unusable: a
// network that blocks UDP is "Blocked", not "Unusable".
func callRank(s string) int {
	switch s {
	case callBlocked:
		return 0
	case string(callq.Unusable):
		return 1
	case string(callq.Poor):
		return 2
	case string(callq.Fair):
		return 3
	case callNotAnswering:
		return 4
	case callNotEnoughData:
		return 5
	case string(callq.Good):
		return 6
	}
	return 7
}

// num is a number in the Calls JSON: rounded to 2 decimals, null when NaN.
type num float64

func (n num) MarshalJSON() ([]byte, error) {
	f := float64(n)
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return []byte("null"), nil
	}
	return strconv.AppendFloat(nil, round2(f), 'f', -1, 64), nil
}

// callsCause says why the top state is not good. Source is "verdict" (the
// path verdict: Kind lan, isp, upstream, dns or no_network; Summary is its
// sentence), "udp" (Kind udp_blocked, stun_blocked or udp_degraded) or
// "path" (the call path's own score; Factor is loss, jitter or latency).
type callsCause struct {
	Source  string            `json:"source"`
	Kind    model.VerdictKind `json:"kind,omitempty"`
	Summary string            `json:"summary,omitempty"`
	Factor  string            `json:"factor,omitempty"`
}

// callsMinute is a one-minute bucket of the worst-minute report.
type callsMinute struct {
	At     int64  `json:"at"`
	State  string `json:"state"`
	MOS    num    `json:"mos"`
	RTT    num    `json:"rtt_ms"`
	Jitter num    `json:"jitter_ms"`
	Loss   num    `json:"loss"`
}

// callsSeries is a group's per-minute series from callsJSON.Start, one
// entry per minute; null (state "") where the minute is not scored.
type callsSeries struct {
	State  []string `json:"state"`
	MOS    []num    `json:"mos"`
	RTT    []num    `json:"rtt_ms"`
	Jitter []num    `json:"jitter_ms"`
	Loss   []num    `json:"loss"`
}

// callsTarget is one STUN target of a group (local only: the public link
// never names targets).
type callsTarget struct {
	Name         string `json:"name"`
	NotAnswering bool   `json:"not_answering,omitempty"`
	Samples      int    `json:"samples"`
	Unknown      int    `json:"unknown,omitempty"`
	RTT          num    `json:"rtt_ms"`
	Loss         num    `json:"loss"`
}

// callsGroup is the call path or one measured app over the window. Loss is a
// fraction; MOS and Worst are the worst scored minute's; Samples counts
// bursts (answered or timed out) and Unknown the bursts that failed
// otherwise (DNS, refused), which are not loss.
type callsGroup struct {
	State   string        `json:"state"`
	MOS     num           `json:"mos"`
	RTT     num           `json:"rtt_ms"`
	RTT95   num           `json:"rtt_p95_ms"`
	Jitter  num           `json:"jitter_ms"`
	Loss    num           `json:"loss"`
	Samples int           `json:"samples"`
	Unknown int           `json:"unknown,omitempty"`
	Factor  string        `json:"factor,omitempty"`
	Worst   *callsMinute  `json:"worst,omitempty"`
	Minutes callsSeries   `json:"minutes"`
	Targets []callsTarget `json:"targets,omitempty"`
}

// callsApp is one app. UDP is "measured" (Group set; Region is the Teams
// relay's cloud region, local only) or "not_measurable". Web* is the TCP
// connect time to the app's websites over the window (omitted without TCP
// targets): mean RTT, loss fraction and samples.
type callsApp struct {
	App    string `json:"app"`
	UDP    string `json:"udp"`
	Region string `json:"region,omitempty"`
	*callsGroup
	WebMs      *num `json:"web_ms,omitempty"`
	WebLoss    *num `json:"web_loss,omitempty"`
	WebSamples int  `json:"web_samples,omitempty"`
}

// callsJSON is /api/calls. State is the top state: the worst of the path
// verdict, the UDP verdicts and the call path's score, with its Cause.
// Quiet: the UDP block has lasted long enough to be shown as information,
// not an alarm.
type callsJSON struct {
	From   int64       `json:"from"`
	To     int64       `json:"to"`
	Start  int64       `json:"start"`
	Step   int64       `json:"step"`
	Capped bool        `json:"capped,omitempty"`
	State  string      `json:"state"`
	Quiet  bool        `json:"quiet,omitempty"`
	Cause  *callsCause `json:"cause,omitempty"`
	Path   *callsGroup `json:"path,omitempty"`
	Apps   []callsApp  `json:"apps"`
}

// callsParams is a parsed /api/calls query.
type callsParams struct {
	from, to time.Time
	capped   bool
	key      string
}

// parseCalls parses /api/calls?from=&to= with /api/panel's time rules
// (default now-5m..now). On the public link absolute bounds are rounded
// outward to the minute so that viewers share cache entries.
func parseCalls(q url.Values, now time.Time, public bool) (callsParams, error) {
	var p callsParams
	if err := checkParams(q, "from", "to"); err != nil {
		return p, err
	}
	fs, ts := q.Get("from"), q.Get("to")
	if fs == "" {
		fs = "now-5m"
	}
	if ts == "" {
		ts = "now"
	}
	var fk, tk string
	var err error
	if p.from, fk, err = parseTime(fs, now); err != nil {
		return p, err
	}
	if p.to, tk, err = parseTime(ts, now); err != nil {
		return p, err
	}
	if p.to.After(now) {
		p.to, tk = now, "now"
	}
	if public {
		if !strings.HasPrefix(tk, "now") {
			to := p.to.Truncate(time.Minute)
			if to.Before(p.to) {
				to = to.Add(time.Minute)
			}
			if to.After(now) {
				to, tk = now, "now"
			} else {
				tk = strconv.FormatInt(to.UnixMilli(), 10)
			}
			p.to = to
		}
		if !strings.HasPrefix(fk, "now") {
			p.from = p.from.Truncate(time.Minute)
			fk = strconv.FormatInt(p.from.UnixMilli(), 10)
		}
	}
	if !p.from.Before(p.to) {
		return p, badReq("from must be before to")
	}
	if p.to.Sub(p.from) > CallsMaxRange {
		p.from, fk, p.capped = p.to.Add(-CallsMaxRange), tk+"-cap", true
	}
	if p.to.Sub(p.from) < CallsMinRange {
		p.from, fk = p.to.Add(-CallsMinRange), tk+"-min"
	}
	p.key = "calls\x00" + fk + "\x00" + tk
	return p, nil
}

// serveCalls answers /api/calls: the call path and per-app call quality
// over the window. One store query; numbers, app and state names (and, on
// the local listener only, target names and the Teams relay region).
func (s *server) serveCalls(w http.ResponseWriter, r *http.Request) {
	p, err := parseCalls(r.URL.Query(), s.now(), s.public)
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, err.Error())
		return
	}
	s.runCached(w, r, p.key, func(ctx context.Context) (*cached, error) {
		c, err := s.calls(ctx, p)
		if err != nil {
			return nil, err
		}
		return jsonCached(c)
	})
}

// webKind is the kind an app's website is measured with: TCP, else HTTPS.
func webKind(t model.Target) (model.ProbeKind, bool) {
	for _, k := range []model.ProbeKind{model.KindTCP, model.KindHTTPS} {
		if hasKind(t, k) {
			return k, true
		}
	}
	return 0, false
}

// udpMinutes reads a STUN target's three sibling series into minutes.
func udpMinutes(cols map[model.SeriesKey]*store.SeriesCols, name string, n int) []callq.Minute {
	get := func(k model.ProbeKind) *store.SeriesCols { return cols[model.SeriesKey{Target: name, Kind: k}] }
	u, j, l := get(model.KindUDP), get(model.KindUDPJitter), get(model.KindUDPLoss)
	out := make([]callq.Minute, n)
	for i := range out {
		m := callq.Empty
		if u != nil {
			ans, lost, to := int(at(u.N, i)), int(at(u.Lost, i)), int(at(u.LostBy[model.ReasonTimeout], i))
			m.Bursts, m.Answered, m.Unknown = ans+to, ans, lost-to
			if ans > 0 && i < len(u.Mean) {
				m.RTT = float64(u.Mean[i])
			}
		}
		if j != nil && at(j.N, i) > 0 && i < len(j.Mean) {
			m.Jitter = float64(j.Mean[i])
		}
		if l != nil && at(l.N, i) > 0 && i < len(l.Mean) {
			m.Loss = float64(l.Mean[i])
		}
		out[i] = m
	}
	return out
}

func (s *server) calls(ctx context.Context, p callsParams) (*callsJSON, error) {
	out := &callsJSON{From: p.from.UnixMilli(), To: p.to.UnixMilli(), Step: time.Minute.Milliseconds(), Capped: p.capped,
		State: callNotEnoughData, Apps: []callsApp{}}
	prof := s.d.Profile()
	var udp, web []model.Target
	if prof != nil {
		for _, t := range prof.Targets {
			switch {
			case t.IsUDP():
				udp = append(udp, t)
			case !isPath(t) && callq.IsApp(t):
				web = append(web, t)
			}
		}
	}
	var keys []model.SeriesKey
	for _, t := range udp {
		for _, k := range model.UDPKinds {
			keys = append(keys, model.SeriesKey{Target: t.Name, Kind: k})
		}
	}
	webKeys := map[string]model.SeriesKey{}
	for _, t := range web {
		if k, ok := webKind(t); ok {
			webKeys[t.Name] = model.SeriesKey{Target: t.Name, Kind: k}
			keys = append(keys, webKeys[t.Name])
		}
	}
	span := p.to.Sub(p.from)
	pts := int((span + time.Minute - 1) / time.Minute)
	start, n := p.from.Truncate(time.Minute), 0
	cols := map[model.SeriesKey]*store.SeriesCols{}
	if len(keys) > 0 {
		res, err := s.d.Store.Panel(ctx, store.PanelQuery{Keys: keys, From: p.from, To: p.to, MaxPoints: pts})
		if err != nil {
			return nil, err
		}
		start = res.Start
		out.Step = res.Step.Milliseconds()
		for i := range res.Series {
			c := &res.Series[i]
			cols[c.Key] = c
			n = max(n, len(c.N))
		}
	}
	out.Start = start.UnixMilli()

	// Every STUN target's minutes; a target that never answered while
	// another one did is not answering (its server, not the connection).
	mins := make([][]callq.Minute, len(udp))
	idx := map[string]int{}
	for i, t := range udp {
		mins[i], idx[t.Name] = udpMinutes(cols, t.Name, n), i
	}
	dead := callq.NotAnswering(mins)

	v := model.Verdict{Kind: VerdictUnknown}
	if s.d.Verdict != nil {
		v = s.d.Verdict.Current()
	}
	blocked := v.Kind == verdictUDPBlocked || v.Kind == verdictSTUNBlocked

	group := func(ts []model.Target) *callsGroup {
		g := &callsGroup{}
		var tm [][]callq.Minute
		var td []bool
		for _, t := range ts {
			i := idx[t.Name]
			tm, td = append(tm, mins[i]), append(td, dead[i])
			if !s.public {
				sm := callq.Summarize(mins[i])
				g.Targets = append(g.Targets, callsTarget{Name: t.Name, NotAnswering: dead[i], Samples: sm.Bursts, Unknown: sm.Unknown,
					RTT: num(sm.RTT50), Loss: num(sm.Loss / 100)})
			}
		}
		comb, alive := callq.Group(tm, td, n)
		g.Minutes = callsSeries{State: make([]string, n), MOS: make([]num, n), RTT: make([]num, n), Jitter: make([]num, n), Loss: make([]num, n)}
		for i, c := range comb {
			nan := num(math.NaN())
			g.Minutes.MOS[i], g.Minutes.RTT[i], g.Minutes.Jitter[i], g.Minutes.Loss[i] = nan, nan, nan, nan
			if c.Scored() {
				mos := c.MOS()
				g.Minutes.State[i] = string(callq.BandOf(mos))
				g.Minutes.MOS[i], g.Minutes.RTT[i], g.Minutes.Jitter[i], g.Minutes.Loss[i] = num(mos), num(c.RTT), num(c.Jitter), num(c.Loss/100)
			}
		}
		sm := callq.Summarize(comb)
		g.MOS, g.RTT, g.RTT95, g.Jitter, g.Loss = num(sm.MOS), num(sm.RTT50), num(sm.RTT95), num(sm.Jitter), num(sm.Loss/100)
		g.Samples, g.Unknown = sm.Bursts, sm.Unknown
		if sm.Worst >= 0 {
			w := comb[sm.Worst]
			g.Factor = sm.Factor
			g.Worst = &callsMinute{At: start.Add(time.Duration(sm.Worst) * time.Minute).UnixMilli(), State: string(sm.Band),
				MOS: num(sm.MOS), RTT: num(w.RTT), Jitter: num(w.Jitter), Loss: num(w.Loss / 100)}
		}
		switch {
		case blocked:
			g.State = callBlocked
		case !alive:
			g.State = callNotAnswering
		case sm.Bursts < callsMinSamples || sm.Worst < 0:
			g.State = callNotEnoughData
		default:
			g.State = string(sm.Band)
		}
		return g
	}

	var core []model.Target
	for _, t := range udp {
		if callq.IsCorePath(t) {
			core = append(core, t)
		}
	}
	if len(core) > 0 {
		out.Path = group(core)
	}

	var regions map[string]string
	if s.d.UDP != nil && !s.public {
		regions = map[string]string{}
		for _, st := range s.d.UDP.UDPState() {
			regions[st.Target] = st.Region
		}
	}
	for _, a := range callq.Apps {
		var ut, wt []model.Target
		for _, t := range udp {
			if callq.HasApp(t, a.ID) {
				ut = append(ut, t)
			}
		}
		for _, t := range web {
			if callq.HasApp(t, a.ID) {
				wt = append(wt, t)
			}
		}
		app := callsApp{App: a.ID, UDP: "not_measurable"}
		switch {
		case a.Measurable && len(ut) > 0:
			app.UDP = "measured"
			app.callsGroup = group(ut)
			for _, t := range ut {
				if r := regions[t.Name]; r != "" && app.Region == "" {
					app.Region = r
				}
			}
		case a.Measurable || len(wt) == 0:
			continue // Teams without the relay (opt-in), or an app not in the profile
		}
		var sum float64
		var ok, lost int
		for _, t := range wt {
			c := cols[webKeys[t.Name]]
			if c == nil {
				continue
			}
			for i, m := range c.Mean {
				k := int(at(c.N, i))
				lost += int(at(c.Lost, i))
				if k > 0 && !math.IsNaN(float64(m)) {
					sum += float64(m) * float64(k)
					ok += k
				}
			}
		}
		if ok+lost > 0 {
			ms, ls := num(math.NaN()), num(float64(lost)/float64(ok+lost))
			if ok > 0 {
				ms = num(sum / float64(ok))
			}
			app.WebMs, app.WebLoss, app.WebSamples = &ms, &ls, ok+lost
		}
		out.Apps = append(out.Apps, app)
	}

	// The top state: the worst of the path verdict, the UDP verdicts and
	// the path's score. Ties keep the earlier one, which names a cause.
	type cand struct {
		state string
		cause *callsCause
	}
	var cs []cand
	switch v.Kind {
	case model.VerdictLAN, model.VerdictISP, model.VerdictUpstream:
		cs = append(cs, cand{string(callq.Poor), &callsCause{Source: "verdict", Kind: v.Kind, Summary: v.Summary}})
	case model.VerdictDNS:
		cs = append(cs, cand{string(callq.Fair), &callsCause{Source: "verdict", Kind: v.Kind, Summary: v.Summary}})
	case model.VerdictNoNetwork:
		cs = append(cs, cand{string(callq.Unusable), &callsCause{Source: "verdict", Kind: v.Kind, Summary: v.Summary}})
	case verdictUDPBlocked, verdictSTUNBlocked:
		cs = append(cs, cand{callBlocked, &callsCause{Source: "udp", Kind: v.Kind, Summary: v.Summary}})
		out.Quiet = verdictQuiet(v)
	case verdictUDPDegraded:
		cs = append(cs, cand{string(callq.Poor), &callsCause{Source: "udp", Kind: v.Kind, Summary: v.Summary}})
	}
	if out.Path != nil {
		var c *callsCause
		if r := callRank(out.Path.State); r >= callRank(string(callq.Unusable)) && r <= callRank(string(callq.Fair)) {
			c = &callsCause{Source: "path", Factor: out.Path.Factor}
		}
		cs = append(cs, cand{out.Path.State, c})
	}
	best := cand{state: callNotEnoughData} // no path and no verdict
	for i, c := range cs {
		if i == 0 || callRank(c.state) < callRank(best.state) {
			best = c
		}
	}
	out.State = best.state
	if best.state != string(callq.Good) {
		out.Cause = best.cause
	}
	return out, nil
}

// verdictQuiet reports the verdict's quiet flag (a UDP block demoted to
// information after hours; see internal/verdict). It is read through the
// verdict's JSON so that this package does not depend on the field's
// arrival order.
func verdictQuiet(v model.Verdict) bool {
	b, err := json.Marshal(v)
	if err != nil {
		return false
	}
	var q struct {
		Quiet bool `json:"quiet"`
	}
	return json.Unmarshal(b, &q) == nil && q.Quiet
}
