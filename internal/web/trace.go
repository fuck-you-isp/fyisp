package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/netinfo"
	"github.com/fuck-you-isp/fyisp/internal/store"
)

// TraceSource is what the Investigate view reads: per-hop statistics from
// the store, the tracer's current routes and on-demand traces. Wire it as
// Deps.Trace (an adapter over internal/trace.Tracer and the store). Nil
// hides the feature: every trace route answers 404.
type TraceSource interface {
	// Hops returns per-hop statistics of target's trace with mode over
	// [from, to): one entry per (hop, address) seen, so a hop with ECMP
	// paths has several.
	Hops(ctx context.Context, target string, mode model.TraceMode, from, to time.Time) ([]HopStat, error)
	// Route returns the current route of target's trace with mode, if it
	// has one.
	Route(ctx context.Context, target string, mode model.TraceMode) (model.Route, bool)
	// RouteChanges returns every target's ICMP route changes in [from, to)
	// (TCP and UDP traces record none).
	RouteChanges(ctx context.Context, from, to time.Time) ([]model.RouteChange, error)
	// Investigate starts an on-demand trace session of target with mode
	// for at most rounds rounds; see internal/trace.Tracer. Concurrent
	// sessions of one target and mode share one trace.
	Investigate(target string, mode model.TraceMode, rounds int) (Investigation, error)
	// Traced returns the targets with always-on (ICMP) traces.
	Traced() []string
	// Modes says which trace modes this machine supports, in
	// model.TraceModes order.
	Modes() []TraceModeInfo
}

// TraceModeInfo is one trace mode in /api/trace's "modes".
type TraceModeInfo struct {
	Mode      string `json:"mode"`           // icmp | tcp | udp
	Port      int    `json:"port,omitempty"` // TCP and UDP: the fixed destination port
	Available bool   `json:"available"`
	// HopsVia "icmp": the routers are found with ICMP echo requests and
	// only the destination is probed with this mode (TCP on Windows).
	HopsVia string `json:"hops_via,omitempty"`
	// Reason a mode is not available: "windows_firewall" (UDP on Windows),
	// "os" (this OS can't send it unprivileged) or "icmp_unavailable" (no
	// ICMP socket: the tracer is not running, every mode is off).
	Reason string `json:"reason,omitempty"`
}

// HopStat summarizes one hop address over a range. Times are milliseconds;
// Loss is a fraction (0..1). LossContinues and RateLimited are the store's
// classification of the hop's loss (store.ClassifyLoss): real loss that
// continues to the destination, or a router that rate-limits its own ICMP
// replies. Both are false for loss below store.HopLossMin. Last (the newest
// RTT), StDev and Jmax (the largest jitter) are NaN when not known (the
// store's hourly tier); NaN is served as null.
type HopStat struct {
	Hop           int
	IP            netip.Addr // invalid: the hop did not answer
	Info          model.HopInfo
	N, Lost       int
	Loss          float64
	Min, Mean     float64
	Max, P95      float64
	Jitter        float64
	Last, StDev   float64
	Jmax          float64
	LossContinues bool
	RateLimited   bool
}

// Trace limits.
const (
	// InvestigateRounds is how many rounds one on-demand trace runs
	// (about 10.4 minutes at InvestigateInterval); it then stops by
	// itself. A POST while it runs doesn't extend it.
	InvestigateRounds = 250
	// InvestigateInterval is the tracer's on-demand round interval
	// (internal/trace.DefaultInvestigate), reported to the UI.
	InvestigateInterval = 2500 * time.Millisecond
	// MaxInvestigations bounds concurrent on-demand traces (distinct
	// targets).
	MaxInvestigations = 4
	// InvestigateStartsPerHour bounds new on-demand traces in a rolling
	// hour, across all targets and viewers (client addresses behind a
	// tunnel can't be trusted, and are never logged). Requests carrying
	// the admin token skip it, not MaxInvestigations.
	InvestigateStartsPerHour = 12
	// InvestigationKeep is how long a finished trace's state is shown.
	InvestigationKeep = time.Hour
	// MaxTraceHops bounds hop numbers accepted by /api/trace/panel.
	MaxTraceHops = 40
	// MaxHopStats and MaxRouteChanges cap list lengths in responses.
	MaxHopStats     = 256
	MaxRouteChanges = 500
	// DefaultTraceInterval is the assumed interval between trace rounds
	// when the TraceSource does not report one (see traceIntervaler).
	DefaultTraceInterval = 10 * time.Second
	traceTimeout         = 20 * time.Second
)

// traceIntervaler is optionally implemented by a TraceSource to report the
// interval between trace rounds (the finest useful timeline bucket).
type traceIntervaler interface{ Interval() time.Duration }

func (d *Deps) traceInterval() time.Duration {
	if ti, ok := d.Trace.(traceIntervaler); ok {
		if iv := ti.Interval(); iv > 0 {
			return iv
		}
	}
	return DefaultTraceInterval
}

var errUnknownTarget = errors.New("unknown target")

// ---------- hop addresses ----------

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// privateAddr reports addresses that identify the user's own network or
// the ISP's internal plant: RFC 1918, CGNAT, link-local, loopback, ULA.
func privateAddr(a netip.Addr) bool {
	a = a.Unmap()
	return a.IsPrivate() || a.IsLoopback() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() ||
		a.IsUnspecified() || a.IsMulticast() || a.IsInterfaceLocalMulticast() || cgnat.Contains(a) ||
		(a.Is4() && a.As4()[0] == 0)
}

// hopView names the addresses of one response from what is known about
// them (reverse DNS, ASN, owner).
type hopView struct {
	info map[netip.Addr]model.HopInfo
}

func newHopView() *hopView { return &hopView{info: map[netip.Addr]model.HopInfo{}} }

// hopJSON is one hop address as served. Private marks an address of the
// home or the ISP's internal network (RFC 1918, CGNAT, link-local, ...).
type hopJSON struct {
	Hop     int    `json:"hop"`
	IP      string `json:"ip,omitempty"`
	NoReply bool   `json:"no_reply,omitempty"`
	Private bool   `json:"private,omitempty"`
	RDNS    string `json:"rdns,omitempty"`
	ASN     uint32 `json:"asn,omitempty"`
	Owner   string `json:"owner,omitempty"`
}

func (r *hopView) hop(n int, a netip.Addr) hopJSON {
	j := hopJSON{Hop: n}
	if !a.IsValid() {
		j.NoReply = true
		return j
	}
	a = a.Unmap()
	info := r.info[a]
	j.IP, j.RDNS = a.String(), info.RDNS
	if privateAddr(a) {
		j.Private = true
		return j
	}
	j.ASN, j.Owner = info.ASN, info.Owner
	return j
}

func (r *hopView) hops(hops []netip.Addr) []hopJSON {
	out := make([]hopJSON, len(hops))
	for i, a := range hops {
		out[i] = r.hop(i+1, a)
	}
	return out
}

// ---------- JSON ----------

// jf is a float rounded to 1 µs; NaN and ±Inf are null.
type jf float64

func (f jf) MarshalJSON() ([]byte, error) {
	v := float64(f)
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return []byte("null"), nil
	}
	return strconv.AppendFloat(nil, math.Round(v*1000)/1000, 'f', -1, 64), nil
}

type hopStatJSON struct {
	hopJSON
	N             int  `json:"n"`
	Lost          int  `json:"lost"`
	Loss          jf   `json:"loss"`
	Min           jf   `json:"min"`
	Mean          jf   `json:"mean"`
	Max           jf   `json:"max"`
	P95           jf   `json:"p95"`
	Jitter        jf   `json:"jitter"`
	Last          jf   `json:"last"`
	StDev         jf   `json:"stdev"`
	Jmax          jf   `json:"jmax"`
	LossContinues bool `json:"loss_continues"`
	RateLimited   bool `json:"rate_limited"`
}

type routeJSON struct {
	Since time.Time `json:"since"`
	Hops  []hopJSON `json:"hops"`
}

type changeJSON struct {
	ID        int64     `json:"id"`
	Target    string    `json:"target"`
	At        time.Time `json:"at"`
	FirstDiff int       `json:"first_diff"`
	From      []hopJSON `json:"from"`
	To        []hopJSON `json:"to"`
}

// traceJSON is /api/trace.
type traceJSON struct {
	Target string `json:"target"`
	// Mode is the trace's probe type (icmp, tcp or udp), Port its fixed
	// destination port (TCP, UDP).
	Mode string `json:"mode"`
	Port int    `json:"port,omitempty"`
	// Host is the bare name (or address) the target's trace goes to, for
	// running mtr yourself: never a port, path or query. Omitted for the
	// gateway and the ISP edge.
	Host   string `json:"host,omitempty"`
	Traced bool   `json:"traced"` // always-on trace (ICMP only)
	// IntervalMs is the always-on traces' round interval (on-demand
	// traces report theirs in Investigation).
	IntervalMs int64         `json:"interval_ms"`
	From       int64         `json:"from"`
	To         int64         `json:"to"`
	Now        int64         `json:"now"`
	ISPASN     uint32        `json:"isp_asn,omitempty"` // ASN of the first public hop
	Route      *routeJSON    `json:"route"`
	Hops       []hopStatJSON `json:"hops"`
	// Changes of this target's route in the range, newest first.
	Changes []changeJSON `json:"changes"`
	// Investigation is the target's on-demand trace with Mode: running,
	// or finished less than InvestigationKeep ago (omitted otherwise).
	Investigation *investigationJSON `json:"investigation,omitempty"`
	// Modes are the trace modes this machine supports.
	Modes []TraceModeInfo `json:"modes"`
}

func clamp01(f float64) float64 {
	if math.IsNaN(f) || f < 0 {
		return 0
	}
	return min(f, 1)
}

func (s *server) traced() map[string]bool {
	m := map[string]bool{}
	if s.d.Trace != nil {
		for _, n := range s.d.Trace.Traced() {
			m[n] = true
		}
	}
	return m
}

// knownTarget reports a profile target name, or the name of a trace of a
// host outside the profile (model.HostTarget, normalised).
func (s *server) knownTarget(name string) bool {
	if name == "" || len(name) > len(model.HostTargetPrefix)+maxHostLen {
		return false
	}
	if h, ok := model.HostOf(name); ok {
		n, err := normHost(h)
		return err == nil && n == h
	}
	if p := s.d.Profile(); p != nil {
		for _, t := range p.Targets {
			if t.Name == name {
				return true
			}
		}
	}
	return false
}

// traceHost is the host target's trace goes to (its ICMP host), bare (see
// bareHost), or "" for the gateway and the ISP edge, whose addresses are
// discovered. A host outside the profile is its own.
func (s *server) traceHost(name string) string {
	if h, ok := model.HostOf(name); ok {
		return h
	}
	if p := s.d.Profile(); p != nil {
		for _, t := range p.Targets {
			if t.Name == name {
				if h := t.HostFor(model.KindICMP); h != model.HostGateway && h != model.HostEdge {
					return bareHost(h)
				}
			}
		}
	}
	return ""
}

// bareHost is the host name or address in h with any scheme, user, port,
// path, query or fragment removed; "" if nothing usable is left.
func bareHost(h string) string {
	if _, rest, ok := strings.Cut(h, "://"); ok {
		h = rest
	}
	if i := strings.IndexAny(h, "/?#"); i >= 0 {
		h = h[:i]
	}
	if i := strings.LastIndexByte(h, '@'); i >= 0 {
		h = h[i+1:]
	}
	if host, _, err := net.SplitHostPort(h); err == nil {
		h = host
	}
	if h == "" || strings.ContainsAny(h, " \t[]:") {
		return ""
	}
	return h
}

// checkTarget answers 400 without a target and 404 for one not in the
// profile.
func (s *server) checkTarget(w http.ResponseWriter, r *http.Request, target string) bool {
	if target == "" {
		writeErr(w, r, http.StatusBadRequest, "target is required")
		return false
	}
	if !s.knownTarget(target) {
		writeErr(w, r, http.StatusNotFound, "unknown target")
		return false
	}
	return true
}

// changesJSON describes route changes with r (already fed every sequence).
func changesJSON(r *hopView, list []model.RouteChange) []changeJSON {
	out := make([]changeJSON, 0, len(list))
	for _, c := range list {
		out = append(out, changeJSON{ID: c.ID, Target: c.Target, At: c.At.UTC(), FirstDiff: c.FirstDiff,
			From: r.hops(c.From), To: r.hops(c.To)})
	}
	return out
}

// newestFirst sorts and caps route changes.
func newestFirst(list []model.RouteChange) []model.RouteChange {
	list = append([]model.RouteChange(nil), list...)
	sort.SliceStable(list, func(i, j int) bool { return list[i].At.After(list[j].At) })
	if len(list) > MaxRouteChanges {
		list = list[:MaxRouteChanges]
	}
	return list
}

// trace builds /api/trace for target's trace with mode over p.
func (s *server) trace(ctx context.Context, target string, mode model.TraceMode, p incidentParams, now time.Time) (*traceJSON, error) {
	src := s.d.Trace
	stats, err := src.Hops(ctx, target, mode, p.from, p.to)
	if err != nil {
		return nil, err
	}
	var changes []model.RouteChange
	if mode == model.TraceICMP {
		all, err := src.RouteChanges(ctx, p.from, p.to)
		if err != nil {
			return nil, err
		}
		for _, c := range all {
			if c.Target == target {
				changes = append(changes, c)
			}
		}
	}
	changes = newestFirst(changes)
	route, hasRoute := src.Route(ctx, target, mode)

	stats = append([]HopStat(nil), stats...)
	sort.SliceStable(stats, func(i, j int) bool {
		if stats[i].Hop != stats[j].Hop {
			return stats[i].Hop < stats[j].Hop
		}
		return stats[i].N+stats[i].Lost > stats[j].N+stats[j].Lost
	})
	if len(stats) > MaxHopStats {
		stats = stats[:MaxHopStats]
	}

	r := newHopView()
	maxHop := 0
	for _, h := range stats {
		maxHop = max(maxHop, h.Hop)
		if h.IP.IsValid() {
			info := h.Info
			if old, ok := r.info[h.IP.Unmap()]; ok && info.ASN == 0 && info.RDNS == "" {
				info = old
			}
			r.info[h.IP.Unmap()] = info
		}
	}
	seq := make([][]netip.Addr, min(maxHop, MaxTraceHops*2))
	for _, h := range stats {
		if h.Hop >= 1 && h.Hop <= len(seq) {
			seq[h.Hop-1] = append(seq[h.Hop-1], h.IP)
		}
	}

	out := &traceJSON{Target: target, Mode: mode.String(), Port: mode.Port(), Host: s.traceHost(target),
		Traced: mode == model.TraceICMP && s.traced()[target], IntervalMs: s.d.traceInterval().Milliseconds(),
		From: p.from.UnixMilli(), To: p.to.UnixMilli(), Now: now.UnixMilli(), Hops: []hopStatJSON{}, Changes: changesJSON(r, changes),
		Modes: s.d.Trace.Modes()}
	// The ISP's ASN: the first public hop with a known ASN.
	for _, sq := range [][][]netip.Addr{seq, routeSeq(route.Hops)} {
		for _, addrs := range sq {
			for _, a := range addrs {
				if a.IsValid() && !privateAddr(a) && r.info[a.Unmap()].ASN != 0 && out.ISPASN == 0 {
					out.ISPASN = r.info[a.Unmap()].ASN
				}
			}
		}
	}
	for _, h := range stats {
		out.Hops = append(out.Hops, hopStatJSON{hopJSON: r.hop(h.Hop, h.IP), N: max(0, h.N), Lost: max(0, h.Lost),
			Loss: jf(clamp01(h.Loss)), Min: jf(h.Min), Mean: jf(h.Mean), Max: jf(h.Max), P95: jf(h.P95), Jitter: jf(h.Jitter),
			Last: jf(h.Last), StDev: jf(h.StDev), Jmax: jf(h.Jmax), LossContinues: h.LossContinues, RateLimited: h.RateLimited})
	}
	if hasRoute {
		out.Route = &routeJSON{Since: route.Since.UTC(), Hops: r.hops(route.Hops)}
	}
	return out, nil
}

func routeSeq(hops []netip.Addr) [][]netip.Addr {
	seq := make([][]netip.Addr, len(hops))
	for i, a := range hops {
		seq[i] = []netip.Addr{a}
	}
	return seq
}

// ---------- handlers ----------

// cached is a rendered response.
type cached struct {
	code        int
	ctype       string
	disposition string
	body        []byte
}

// runCached renders and writes the result or the error; label names the
// query in the error log.
func (s *server) runCached(w http.ResponseWriter, r *http.Request, label string, render func(ctx context.Context) (*cached, error)) {
	ctx, cancel := context.WithTimeout(r.Context(), traceTimeout)
	defer cancel()
	c, err := render(ctx)
	switch {
	case err == nil:
	case errors.Is(err, errUnknownTarget):
		writeErr(w, r, http.StatusNotFound, "unknown target")
		return
	case errors.Is(err, errUnknownGroup):
		writeErr(w, r, http.StatusNotFound, "unknown group")
		return
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		w.Header().Set("Retry-After", "2")
		writeErr(w, r, http.StatusServiceUnavailable, "busy, retry shortly")
		return
	default:
		s.d.log().Error("query", "key", label, "err", err)
		writeErr(w, r, http.StatusInternalServerError, "query failed: "+strings.TrimSpace(err.Error()))
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeBody(w, r, c.code, c.ctype, c.body)
}

func jsonCached(v any) (*cached, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return &cached{code: http.StatusOK, ctype: "application/json; charset=utf-8", body: append(b, '\n')}, nil
}

// parseMode parses the mode parameter (default icmp).
func parseMode(q url.Values) (model.TraceMode, error) {
	v, ok := q["mode"]
	if !ok {
		return model.TraceICMP, nil
	}
	if len(v) == 1 {
		if m, ok := model.ParseTraceMode(v[0]); ok {
			return m, nil
		}
	}
	return 0, badReq("bad mode: want one of icmp, tcp or udp")
}

// serveTrace answers /api/trace?target=&mode=&from=&to=.
func (s *server) serveTrace(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if err := checkParams(q, "target", "mode", "from", "to"); err != nil {
		writeErr(w, r, http.StatusBadRequest, err.Error())
		return
	}
	target := q.Get("target")
	if !s.checkTarget(w, r, target) {
		return
	}
	mode, err := parseMode(q)
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, err.Error())
		return
	}
	now := s.now()
	p, err := parseRange(q, now)
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, err.Error())
		return
	}
	s.runCached(w, r, "trace", func(ctx context.Context) (*cached, error) {
		t, err := s.trace(ctx, target, mode, p, now)
		if err != nil {
			return nil, err
		}
		if s.inv != nil {
			t.Investigation = s.inv.state(invKey{target, mode})
		}
		return jsonCached(t)
	})
}

// parseHops parses "1,2,12" into sorted distinct hop numbers.
func parseHops(s string) ([]int, error) {
	if s == "" {
		return nil, nil
	}
	seen := map[int]bool{}
	var out []int
	for _, f := range strings.Split(s, ",") {
		n, err := strconv.Atoi(f)
		if err != nil || n < 1 || n > MaxTraceHops {
			return nil, badReq("bad hops %q: want comma-separated hop numbers 1..%d", s, MaxTraceHops)
		}
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	sort.Ints(out)
	return out, nil
}

// serveTracePanel answers /api/trace/panel?target=&mode=&hops=1,4,12&from=&to=&points=
// in /api/panel's format with one series per hop (kind "trace",
// "trace_tcp" or "trace_udp", "hop": n). hops defaults to every hop of the
// current route. One store query.
func (s *server) serveTracePanel(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if err := checkParams(q, "target", "mode", "hops", "from", "to", "points"); err != nil {
		writeErr(w, r, http.StatusBadRequest, err.Error())
		return
	}
	target := q.Get("target")
	if !s.checkTarget(w, r, target) {
		return
	}
	mode, err := parseMode(q)
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, err.Error())
		return
	}
	hops, err := parseHops(q.Get("hops"))
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, err.Error())
		return
	}
	pq := url.Values{"group": {"trace"}}
	for _, k := range []string{"from", "to", "points"} {
		if v, ok := q[k]; ok {
			pq[k] = v
		}
	}
	now := s.now()
	p, err := parsePanelParams(pq, now)
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, err.Error())
		return
	}
	if hops == nil {
		if rt, ok := s.d.Trace.Route(r.Context(), target, mode); ok {
			for i := range min(len(rt.Hops), MaxTraceHops) {
				hops = append(hops, i+1)
			}
		}
	}
	s.runCached(w, r, "tpanel", func(ctx context.Context) (*cached, error) {
		pd, err := s.d.queryTracePanel(ctx, target, mode.Kind(), hops, p, now)
		if err != nil {
			return nil, err
		}
		return &cached{code: http.StatusOK, ctype: "application/json; charset=utf-8", body: pd.JSON()}, nil
	})
}

// queryTracePanel runs exactly one store.Panel call over the hop series.
// Trace series report no "not measured" counts: on-demand traces run only
// now and then, so missing rounds are expected, not an outage of fyisp.
func (d *Deps) queryTracePanel(ctx context.Context, target string, kind model.ProbeKind, hops []int, p panelParams, now time.Time) (*panelData, error) {
	iv := d.traceInterval()
	series := make([]panelSeries, len(hops))
	keys := make([]model.SeriesKey, len(hops))
	for i, n := range hops {
		keys[i] = model.SeriesKey{Target: target, Kind: kind, Hop: uint8(n)}
		series[i] = panelSeries{key: keys[i], interval: iv}
	}
	points := min(p.points, max(1, int(p.to.Sub(p.from)/iv)))
	res := &store.PanelResult{Tier: store.TierRaw, Start: p.from, Step: p.to.Sub(p.from)}
	if len(keys) > 0 {
		var err error
		res, err = d.Store.Panel(ctx, store.PanelQuery{Keys: keys, From: p.from, To: p.to, MaxPoints: points})
		if err != nil {
			return nil, err
		}
	}
	pd := &panelData{group: model.Group{ID: "trace", Title: target}, params: p, now: now, res: res, series: series}
	byKey := make(map[model.SeriesKey]*store.SeriesCols, len(res.Series))
	for i := range res.Series {
		byKey[res.Series[i].Key] = &res.Series[i]
		pd.nb = max(pd.nb, len(res.Series[i].Mean))
	}
	pd.cols = make([]*store.SeriesCols, len(series))
	pd.gap = make([][]uint32, len(series))
	for i, sr := range series {
		pd.cols[i] = byKey[sr.key]
		pd.gap[i] = make([]uint32, pd.nb)
	}
	return pd, nil
}

// ---------- on-demand traces (local only) ----------

// Investigation is one on-demand trace session (internal/trace.Investigation).
type Investigation interface {
	Stop()
	Progress() (rounds int, ended time.Time)
}

// investigationJSON is an on-demand trace's state, in POST /api/investigate
// and /api/trace.
type investigationJSON struct {
	Target     string `json:"target"`
	Mode       string `json:"mode"`  // icmp | tcp | udp
	State      string `json:"state"` // running | finished
	RoundsDone int    `json:"rounds_done"`
	RoundsMax  int    `json:"rounds_max"`
	IntervalMs int64  `json:"interval_ms"`
	StartedAt  int64  `json:"started_at"`
	FinishedAt int64  `json:"finished_at,omitempty"`
}

type invSession struct {
	inv     Investigation
	started time.Time
}

func (se *invSession) json(k invKey) *investigationJSON {
	n, ended := se.inv.Progress()
	j := &investigationJSON{Target: k.target, Mode: k.mode.String(), State: "running", RoundsDone: n, RoundsMax: InvestigateRounds,
		IntervalMs: InvestigateInterval.Milliseconds(), StartedAt: se.started.UnixMilli()}
	if !ended.IsZero() {
		j.State, j.FinishedAt = "finished", ended.UnixMilli()
	}
	return j
}

func (se *invSession) running() bool { _, ended := se.inv.Progress(); return ended.IsZero() }

// invKey names an on-demand trace: a target and a mode.
type invKey struct {
	target string
	mode   model.TraceMode
}

// Investigations tracks on-demand traces.
type Investigations struct {
	mu     sync.Mutex
	m      map[invKey]*invSession
	starts []time.Time // budgeted starts in the last hour, oldest first
	now    func() time.Time
}

// NewInvestigations returns an empty set.
func NewInvestigations() *Investigations {
	return &Investigations{m: map[invKey]*invSession{}, now: time.Now}
}

// invError is a refused start: a stable code for the UI (docs/TRACE-API.md)
// and, for the limits, how long to wait.
type invError struct {
	status int
	code   string
	msg    string
	retry  time.Duration
}

func (e *invError) Error() string { return e.msg }

// errNoSlot: every slot is taken.
var errNoSlot = &invError{http.StatusTooManyRequests, "busy", "too many on-demand traces running", 30 * time.Second}

// budgetWindow is the start budget's rolling window.
const budgetWindow = time.Hour

// pruneLocked drops finished traces older than InvestigationKeep, and
// starts that left the budget's window. mu is held.
func (iv *Investigations) pruneLocked(now time.Time) {
	for k, se := range iv.m {
		if _, ended := se.inv.Progress(); !ended.IsZero() && now.Sub(ended) >= InvestigationKeep {
			delete(iv.m, k)
		}
	}
	n := 0
	for n < len(iv.starts) && now.Sub(iv.starts[n]) >= budgetWindow {
		n++
	}
	iv.starts = iv.starts[n:]
}

// start starts k's trace for InvestigateRounds rounds. A running one is
// returned as is (its count is not reset); a finished one starts afresh.
// Unless unbudgeted (the admin token), a new trace needs room in the
// hourly start budget; only starts that succeed use it. Traces of one
// target with different modes are different traces (slots, budget).
func (iv *Investigations) start(src TraceSource, k invKey, unbudgeted bool) (st *investigationJSON, created bool, err error) {
	iv.mu.Lock()
	defer iv.mu.Unlock()
	now := iv.now()
	iv.pruneLocked(now)
	if se := iv.m[k]; se != nil && se.running() {
		return se.json(k), false, nil
	}
	if e := iv.refusedLocked(now, unbudgeted); e != nil {
		return nil, false, e
	}
	inv, err := src.Investigate(k.target, k.mode, InvestigateRounds)
	if err != nil {
		return nil, false, err
	}
	se := &invSession{inv: inv, started: now}
	iv.m[k] = se
	if !unbudgeted {
		iv.starts = append(iv.starts, now)
	}
	return se.json(k), true, nil
}

// refusedLocked is why a new trace can't start now: the hourly budget
// (unless unbudgeted) or the slots. mu is held.
func (iv *Investigations) refusedLocked(now time.Time, unbudgeted bool) *invError {
	if !unbudgeted && len(iv.starts) >= InvestigateStartsPerHour {
		return &invError{http.StatusTooManyRequests, "rate_limited",
			"too many on-demand traces started in the last hour", iv.starts[0].Add(budgetWindow).Sub(now)}
	}
	all := 0
	for _, se := range iv.m {
		if se.running() {
			all++
		}
	}
	if all >= MaxInvestigations {
		return errNoSlot
	}
	return nil
}

// check is start's refusal for the budget or the slots, without starting
// (nil while k runs: start returns its state).
func (iv *Investigations) check(k invKey, unbudgeted bool) *invError {
	iv.mu.Lock()
	defer iv.mu.Unlock()
	now := iv.now()
	iv.pruneLocked(now)
	if se := iv.m[k]; se != nil && se.running() {
		return nil
	}
	return iv.refusedLocked(now, unbudgeted)
}

// state is k's trace, if it runs or finished recently.
func (iv *Investigations) state(k invKey) *investigationJSON {
	iv.mu.Lock()
	defer iv.mu.Unlock()
	iv.pruneLocked(iv.now())
	if se := iv.m[k]; se != nil {
		return se.json(k)
	}
	return nil
}

// writeInvErr writes a refused investigate request: {"error", "code",
// "retry_after" (seconds, with Retry-After)}.
func writeInvErr(w http.ResponseWriter, r *http.Request, status int, code, msg string, retry time.Duration) {
	body := map[string]any{"error": msg, "code": code}
	if retry > 0 {
		secs := int((retry + time.Second - 1) / time.Second)
		w.Header().Set("Retry-After", strconv.Itoa(secs))
		body["retry_after"] = secs
	}
	writeJSON(w, r, status, body)
}

// investigateBodyMax caps the request body; the route takes none.
const investigateBodyMax = 1 << 10

// invRequest is a POST /api/investigate that passed investigateRequest:
// a profile target name, or a host (as typed, not checked yet).
type invRequest struct {
	target, host string
	mode         model.TraceMode
}

// investigateRequest applies the request checks: POST, no body, exactly
// one target or host parameter and at most one mode (icmp, tcp, udp;
// default icmp). It writes the refusal and returns false on failure. The
// response is never cached. The handler then checks the origin, and
// investigate the target name or the host, and the mode.
func (s *server) investigateRequest(w http.ResponseWriter, r *http.Request) (invRequest, bool) {
	w.Header().Set("Cache-Control", "no-store")
	if s.d.Trace == nil {
		writeInvErr(w, r, http.StatusNotFound, "unavailable", "tracing is not available", 0)
		return invRequest{}, false
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeInvErr(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed", 0)
		return invRequest{}, false
	}
	// No body: refused by its declared length or chunking before reading,
	// and anything that still arrives (at most investigateBodyMax is read).
	if r.ContentLength != 0 || len(r.TransferEncoding) > 0 {
		writeInvErr(w, r, http.StatusBadRequest, "bad_request", "this request takes no body", 0)
		return invRequest{}, false
	}
	if n, _ := io.Copy(io.Discard, http.MaxBytesReader(w, r.Body, investigateBodyMax)); n > 0 {
		writeInvErr(w, r, http.StatusBadRequest, "bad_request", "this request takes no body", 0)
		return invRequest{}, false
	}
	q := r.URL.Query()
	if err := checkParams(q, "target", "host", "mode"); err != nil || len(q["target"])+len(q["host"]) != 1 || q.Get("target")+q.Get("host") == "" {
		writeInvErr(w, r, http.StatusBadRequest, "bad_request", "want exactly one target or host parameter, and at most one mode", 0)
		return invRequest{}, false
	}
	mode, err := parseMode(q)
	if err != nil {
		writeInvErr(w, r, http.StatusBadRequest, "bad_request", err.Error(), 0)
		return invRequest{}, false
	}
	return invRequest{target: q.Get("target"), host: q.Get("host"), mode: mode}, true
}

// ---------- hosts outside the profile ----------

// maxHostLen is the longest host name accepted (DNS: 253).
const maxHostLen = 253

// hostResolveTimeout bounds the lookup of a host to trace.
const hostResolveTimeout = 5 * time.Second

// normHost checks and normalises a host to trace: an IPv4 address, or a DNS
// name of letters, digits and hyphens, lowercased and without a trailing
// dot, so that repeated traces of one host share a name.
func normHost(s string) (string, error) {
	h := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(s)), ".")
	if a, err := netip.ParseAddr(h); err == nil {
		if !a.Is4() {
			return "", errors.New("IPv6 addresses can’t be traced: enter a name or an IPv4 address")
		}
		return a.String(), nil
	}
	bad := errors.New("not a host name or IPv4 address")
	if h == "" || len(h) > maxHostLen {
		return "", bad
	}
	labels := strings.Split(h, ".")
	for _, l := range labels {
		if l == "" || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
			return "", bad
		}
		for _, c := range l {
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
				return "", bad
			}
		}
	}
	// No top-level domain is all digits: "127.1" or "010.0.0.1" are
	// addresses in disguise, not names.
	if strings.Trim(labels[len(labels)-1], "0123456789") == "" {
		return "", bad
	}
	return h, nil
}

// resolveHost returns host's IPv4 addresses: host itself for an address,
// else Deps.Resolve.
func (d *Deps) resolveHost(ctx context.Context, host string) ([]netip.Addr, error) {
	if a, err := netip.ParseAddr(host); err == nil {
		return []netip.Addr{a}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, hostResolveTimeout)
	defer cancel()
	if d.Resolve != nil {
		return d.Resolve(ctx, host)
	}
	return net.DefaultResolver.LookupNetIP(ctx, "ip4", host)
}

// checkHost resolves host and refuses it unless every IPv4 address it has
// is public (netinfo.Traceable): no private, loopback, link-local, CGNAT,
// multicast, broadcast, reserved or documentation address. The tracer
// checks every later resolution too. The refusal never names the address.
func (d *Deps) checkHost(ctx context.Context, host string) *invError {
	addrs, err := d.resolveHost(ctx, host)
	var v4 []netip.Addr
	for _, a := range addrs {
		if a = a.Unmap(); a.Is4() {
			v4 = append(v4, a)
		}
	}
	if err != nil || len(v4) == 0 {
		return &invError{http.StatusNotFound, "unresolved", host + " has no IPv4 address (IPv6 can’t be traced)", 0}
	}
	for _, a := range v4 {
		if !netinfo.Traceable(a) {
			what := host + " resolves to"
			if a.String() == host {
				what = host + " is"
			}
			return &invError{http.StatusForbidden, "not_public", what + " a private, local or otherwise non-public address: only public hosts can be traced", 0}
		}
	}
	return nil
}

// modeAvailable reports whether src can trace with mode on this machine.
func modeAvailable(src TraceSource, mode model.TraceMode) bool {
	for _, m := range src.Modes() {
		if m.Mode == mode.String() {
			return m.Available
		}
	}
	return false
}

// investigate starts the trace once the handler's checks passed;
// unbudgeted skips the hourly start budget. A host (or a model.HostTarget
// name) is checked and resolved first: public addresses only. Logs name the
// target and the error, never the client.
func (s *server) investigate(w http.ResponseWriter, r *http.Request, q invRequest, unbudgeted bool) {
	target, mode := q.target, q.mode
	if h, ok := model.HostOf(target); ok && q.host == "" {
		q.host = h
	}
	if q.host != "" {
		h, err := normHost(q.host)
		if err != nil {
			writeInvErr(w, r, http.StatusBadRequest, "bad_host", err.Error(), 0)
			return
		}
		target = model.HostTarget(h)
	} else if !s.knownTarget(target) {
		writeInvErr(w, r, http.StatusNotFound, "unknown_target", "unknown target", 0)
		return
	}
	if !modeAvailable(s.d.Trace, mode) {
		writeInvErr(w, r, http.StatusBadRequest, "unsupported_mode", mode.String()+" traces are not available on this machine", 0)
		return
	}
	k := invKey{target, mode}
	var ie *invError
	if h, ok := model.HostOf(target); ok {
		// Refused for the budget or the slots before any DNS lookup.
		if err := s.inv.check(k, unbudgeted); err != nil {
			ie = err
		} else {
			ie = s.d.checkHost(r.Context(), h)
		}
		if ie != nil {
			s.d.log().Debug("investigate refused", "target", target, "code", ie.code)
			writeInvErr(w, r, ie.status, ie.code, ie.msg, ie.retry)
			return
		}
	}
	st, _, err := s.inv.start(s.d.Trace, k, unbudgeted)
	switch {
	case errors.As(err, &ie):
		s.d.log().Debug("investigate refused", "target", target, "code", ie.code)
		writeInvErr(w, r, ie.status, ie.code, ie.msg, ie.retry)
		return
	case err != nil:
		s.d.log().Warn("investigate", "target", target, "err", err)
		writeInvErr(w, r, http.StatusInternalServerError, "failed", "could not start trace: "+err.Error(), 0)
		return
	}
	writeJSON(w, r, http.StatusOK, st)
}

// serveInvestigate answers POST /api/investigate?target=|host= for any
// viewer: same origin and the CSRF token, then the trace limits (profile
// targets, or hosts whose every address is public; InvestigateRounds,
// MaxInvestigations, InvestigateStartsPerHour and the tracer's per-router
// cap). No admin token: a trace runs the same traceroute anyone can run
// against a public host, from this machine, within those budgets. A request
// carrying the token skips the start budget.
func (h *localHandler) serveInvestigate(w http.ResponseWriter, r *http.Request) {
	q, ok := h.investigateRequest(w, r)
	if !ok {
		return
	}
	if !sameOrigin(r) {
		writeInvErr(w, r, http.StatusForbidden, "forbidden", "cross-origin request refused", 0)
		return
	}
	if !tokenEqual(r.Header.Get(HeaderCSRF), h.csrf) {
		writeInvErr(w, r, http.StatusForbidden, "forbidden", "missing or bad "+HeaderCSRF, 0)
		return
	}
	h.investigate(w, r, q, h.adminOK(r))
}
