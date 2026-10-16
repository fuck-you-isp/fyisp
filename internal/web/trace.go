package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/netip"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/store"
)

// TraceSource is what the Investigate view reads: per-hop statistics from
// the store, the tracer's current routes and on-demand traces. Wire it as
// Deps.Trace (an adapter over internal/trace.Tracer and the store). Nil
// hides the feature: every trace route answers 404.
type TraceSource interface {
	// Hops returns per-hop statistics of target over [from, to): one entry
	// per (hop, address) seen, so a hop with ECMP paths has several.
	Hops(ctx context.Context, target string, from, to time.Time) ([]HopStat, error)
	// Route returns target's current route, if it has one.
	Route(ctx context.Context, target string) (model.Route, bool)
	// RouteChanges returns every target's route changes in [from, to).
	RouteChanges(ctx context.Context, from, to time.Time) ([]model.RouteChange, error)
	// Investigate starts an on-demand trace session of target for at most
	// rounds rounds; see internal/trace.Tracer. Concurrent sessions of one
	// target share one trace.
	Investigate(target string, rounds int) (Investigation, error)
	// Traced returns the targets with always-on traces.
	Traced() []string
}

// HopStat summarizes one hop address over a range. Times are milliseconds;
// Loss is a fraction (0..1). LossContinues and RateLimited are the store's
// classification of the hop's loss (store.ClassifyLoss): real loss that
// continues to the destination, or a router that rate-limits its own ICMP
// replies. Both are false for loss below store.HopLossMin.
type HopStat struct {
	Hop           int
	IP            netip.Addr // invalid: the hop did not answer
	Info          model.HopInfo
	N, Lost       int
	Loss          float64
	Min, Mean     float64
	Max, P95      float64
	Jitter        float64
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
	Target string        `json:"target"`
	Traced bool          `json:"traced"` // always-on trace
	From   int64         `json:"from"`
	To     int64         `json:"to"`
	Now    int64         `json:"now"`
	ISPASN uint32        `json:"isp_asn,omitempty"` // ASN of the first public hop
	Route  *routeJSON    `json:"route"`
	Hops   []hopStatJSON `json:"hops"`
	// Changes of this target's route in the range, newest first.
	Changes []changeJSON `json:"changes"`
	// Investigation is the target's on-demand trace: running, or finished
	// less than InvestigationKeep ago (omitted otherwise).
	Investigation *investigationJSON `json:"investigation,omitempty"`
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

func (s *server) knownTarget(name string) bool {
	if name == "" || len(name) > 128 {
		return false
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

// trace builds /api/trace for target over p.
func (s *server) trace(ctx context.Context, target string, p incidentParams, now time.Time) (*traceJSON, error) {
	src := s.d.Trace
	stats, err := src.Hops(ctx, target, p.from, p.to)
	if err != nil {
		return nil, err
	}
	all, err := src.RouteChanges(ctx, p.from, p.to)
	if err != nil {
		return nil, err
	}
	var changes []model.RouteChange
	for _, c := range all {
		if c.Target == target {
			changes = append(changes, c)
		}
	}
	changes = newestFirst(changes)
	route, hasRoute := src.Route(ctx, target)

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

	out := &traceJSON{Target: target, Traced: s.traced()[target], From: p.from.UnixMilli(), To: p.to.UnixMilli(),
		Now: now.UnixMilli(), Hops: []hopStatJSON{}, Changes: changesJSON(r, changes)}
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
			LossContinues: h.LossContinues, RateLimited: h.RateLimited})
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

// serveTrace answers /api/trace?target=&from=&to=.
func (s *server) serveTrace(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if err := checkParams(q, "target", "from", "to"); err != nil {
		writeErr(w, r, http.StatusBadRequest, err.Error())
		return
	}
	target := q.Get("target")
	if !s.checkTarget(w, r, target) {
		return
	}
	now := s.now()
	p, err := parseRange(q, now)
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, err.Error())
		return
	}
	s.runCached(w, r, "trace", func(ctx context.Context) (*cached, error) {
		t, err := s.trace(ctx, target, p, now)
		if err != nil {
			return nil, err
		}
		if s.inv != nil {
			t.Investigation = s.inv.state(target)
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

// serveTracePanel answers /api/trace/panel?target=&hops=1,4,12&from=&to=&points=
// in /api/panel's format with one series per hop (kind "trace", "hop": n).
// hops defaults to every hop of the current route. One store query.
func (s *server) serveTracePanel(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if err := checkParams(q, "target", "hops", "from", "to", "points"); err != nil {
		writeErr(w, r, http.StatusBadRequest, err.Error())
		return
	}
	target := q.Get("target")
	if !s.checkTarget(w, r, target) {
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
		if rt, ok := s.d.Trace.Route(r.Context(), target); ok {
			for i := range min(len(rt.Hops), MaxTraceHops) {
				hops = append(hops, i+1)
			}
		}
	}
	s.runCached(w, r, "tpanel", func(ctx context.Context) (*cached, error) {
		pd, err := s.d.queryTracePanel(ctx, target, hops, p, now)
		if err != nil {
			return nil, err
		}
		return &cached{code: http.StatusOK, ctype: "application/json; charset=utf-8", body: pd.JSON()}, nil
	})
}

// queryTracePanel runs exactly one store.Panel call over the hop series.
// Trace series report no "not measured" counts: on-demand traces run only
// now and then, so missing rounds are expected, not an outage of fyisp.
func (d *Deps) queryTracePanel(ctx context.Context, target string, hops []int, p panelParams, now time.Time) (*panelData, error) {
	iv := d.traceInterval()
	series := make([]panelSeries, len(hops))
	keys := make([]model.SeriesKey, len(hops))
	for i, n := range hops {
		keys[i] = model.SeriesKey{Target: target, Kind: model.KindTrace, Hop: uint8(n)}
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

func (se *invSession) json(target string) *investigationJSON {
	n, ended := se.inv.Progress()
	j := &investigationJSON{Target: target, State: "running", RoundsDone: n, RoundsMax: InvestigateRounds,
		IntervalMs: InvestigateInterval.Milliseconds(), StartedAt: se.started.UnixMilli()}
	if !ended.IsZero() {
		j.State, j.FinishedAt = "finished", ended.UnixMilli()
	}
	return j
}

func (se *invSession) running() bool { _, ended := se.inv.Progress(); return ended.IsZero() }

// Investigations tracks on-demand traces.
type Investigations struct {
	mu     sync.Mutex
	m      map[string]*invSession
	starts []time.Time // budgeted starts in the last hour, oldest first
	now    func() time.Time
}

// NewInvestigations returns an empty set.
func NewInvestigations() *Investigations {
	return &Investigations{m: map[string]*invSession{}, now: time.Now}
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

// start starts target's trace for InvestigateRounds rounds. A running one
// is returned as is (its count is not reset); a finished one starts
// afresh. Unless unbudgeted (the admin token), a new trace needs room in
// the hourly start budget; only starts that succeed use it.
func (iv *Investigations) start(src TraceSource, target string, unbudgeted bool) (st *investigationJSON, created bool, err error) {
	iv.mu.Lock()
	defer iv.mu.Unlock()
	now := iv.now()
	iv.pruneLocked(now)
	if se := iv.m[target]; se != nil && se.running() {
		return se.json(target), false, nil
	}
	if !unbudgeted && len(iv.starts) >= InvestigateStartsPerHour {
		return nil, false, &invError{http.StatusTooManyRequests, "rate_limited",
			"too many on-demand traces started in the last hour", iv.starts[0].Add(budgetWindow).Sub(now)}
	}
	all := 0
	for _, se := range iv.m {
		if se.running() {
			all++
		}
	}
	if all >= MaxInvestigations {
		return nil, false, errNoSlot
	}
	inv, err := src.Investigate(target, InvestigateRounds)
	if err != nil {
		return nil, false, err
	}
	se := &invSession{inv: inv, started: now}
	iv.m[target] = se
	if !unbudgeted {
		iv.starts = append(iv.starts, now)
	}
	return se.json(target), true, nil
}

// state is target's trace, if it runs or finished recently.
func (iv *Investigations) state(target string) *investigationJSON {
	iv.mu.Lock()
	defer iv.mu.Unlock()
	iv.pruneLocked(iv.now())
	if se := iv.m[target]; se != nil {
		return se.json(target)
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

// investigateRequest applies the request checks: POST, no body, exactly
// one target parameter. It writes the refusal and returns "" on failure.
// The response is never cached. The handler then checks the origin, and
// investigate the target name.
func (s *server) investigateRequest(w http.ResponseWriter, r *http.Request) string {
	w.Header().Set("Cache-Control", "no-store")
	if s.d.Trace == nil {
		writeInvErr(w, r, http.StatusNotFound, "unavailable", "tracing is not available", 0)
		return ""
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeInvErr(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed", 0)
		return ""
	}
	// No body: refused by its declared length or chunking before reading,
	// and anything that still arrives (at most investigateBodyMax is read).
	if r.ContentLength != 0 || len(r.TransferEncoding) > 0 {
		writeInvErr(w, r, http.StatusBadRequest, "bad_request", "this request takes no body", 0)
		return ""
	}
	if n, _ := io.Copy(io.Discard, http.MaxBytesReader(w, r.Body, investigateBodyMax)); n > 0 {
		writeInvErr(w, r, http.StatusBadRequest, "bad_request", "this request takes no body", 0)
		return ""
	}
	q := r.URL.Query()
	if err := checkParams(q, "target"); err != nil || len(q["target"]) != 1 || q.Get("target") == "" {
		writeInvErr(w, r, http.StatusBadRequest, "bad_request", "want exactly one target parameter", 0)
		return ""
	}
	return q.Get("target")
}

// investigate starts the trace once the handler's checks passed;
// unbudgeted skips the hourly start budget. Logs name the target and the
// error, never the client.
func (s *server) investigate(w http.ResponseWriter, r *http.Request, target string, unbudgeted bool) {
	if !s.knownTarget(target) { // profile target names only, never a host or an address
		writeInvErr(w, r, http.StatusNotFound, "unknown_target", "unknown target", 0)
		return
	}
	st, _, err := s.inv.start(s.d.Trace, target, unbudgeted)
	var ie *invError
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

// serveInvestigate answers POST /api/investigate?target= for any viewer:
// same origin and the CSRF token, then the trace limits (profile targets
// only, InvestigateRounds, MaxInvestigations, InvestigateStartsPerHour and
// the tracer's per-router cap). No admin token: a trace re-runs the
// always-available traceroute against a target the viewer already sees,
// and reveals nothing the dashboard doesn't. A request carrying the token
// skips the start budget.
func (h *localHandler) serveInvestigate(w http.ResponseWriter, r *http.Request) {
	target := h.investigateRequest(w, r)
	if target == "" {
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
	h.investigate(w, r, target, h.adminOK(r))
}
