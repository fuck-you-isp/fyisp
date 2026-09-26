package web

import (
	"context"
	"encoding/json"
	"errors"
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
	// Investigate starts (or extends) an on-demand trace of target for ttl;
	// see internal/trace.Tracer. The web layer calls it again on every
	// keepalive and then stops the previous session, so overlapping calls
	// for one target must extend, not duplicate, the trace.
	Investigate(target string, ttl time.Duration) (stop func(), err error)
	// Traced returns the targets with always-on traces.
	Traced() []string
}

// HopStat summarizes one hop address over a range. Times are milliseconds;
// Loss is a fraction (0..1). LossContinues reports that the loss seen here
// is also seen at every later hop up to the destination (real loss), as
// opposed to a router that rate-limits its own ICMP replies.
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
}

// Trace limits.
const (
	// InvestigateTTL is how long one POST /api/investigate keeps an
	// on-demand trace running; the UI re-posts every 60s while open.
	InvestigateTTL = 2 * time.Minute
	// MaxInvestigations bounds concurrent on-demand traces started from
	// the web UI (distinct targets).
	MaxInvestigations = 4
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

// ---------- redaction ----------

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// privateAddr reports addresses that identify the user's own network or
// the ISP's internal plant: RFC 1918, CGNAT, link-local, loopback, ULA.
func privateAddr(a netip.Addr) bool {
	a = a.Unmap()
	return a.IsPrivate() || a.IsLoopback() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() ||
		a.IsUnspecified() || a.IsMulticast() || a.IsInterfaceLocalMulticast() || cgnat.Contains(a) ||
		(a.Is4() && a.As4()[0] == 0)
}

// maskAddr hides the host part: a.b.c.x for IPv4, the /48 for IPv6.
func maskAddr(a netip.Addr) string {
	a = a.Unmap()
	if a.Is4() {
		b := a.As4()
		return strconv.Itoa(int(b[0])) + "." + strconv.Itoa(int(b[1])) + "." + strconv.Itoa(int(b[2])) + ".x"
	}
	p, _ := a.Prefix(48)
	return strings.TrimSuffix(p.Addr().String(), "::") + "::x"
}

// Redaction levels, from least to most strict.
const (
	lvlFull    = iota // later public hops: everything shown
	lvlNoRDNS         // 2nd and 3rd public hops: rDNS hidden
	lvlMasked         // 1st public hop (the ISP edge): IP masked to /24, no rDNS
	lvlPrivate        // private hop: no address at all
)

// redactor classifies every address in one response from all the hop
// sequences it contains, keeping the strictest level an address gets in
// any of them, so no address is masked in one place and shown elsewhere.
type redactor struct {
	public bool
	lvl    map[netip.Addr]int
	info   map[netip.Addr]model.HopInfo
}

func newRedactor(public bool) *redactor {
	return &redactor{public: public, lvl: map[netip.Addr]int{}, info: map[netip.Addr]model.HopInfo{}}
}

// seq classifies a path: seq[i] are the addresses seen at TTL i+1.
func (r *redactor) seq(seq [][]netip.Addr) {
	pub := 0
	for _, addrs := range seq {
		any := false
		for _, a := range addrs {
			if !a.IsValid() {
				continue
			}
			a = a.Unmap()
			l := lvlFull
			switch {
			case privateAddr(a):
				l = lvlPrivate
			case pub == 0:
				l, any = lvlMasked, true
			case pub < 3:
				l, any = lvlNoRDNS, true
			default:
				any = true
			}
			if old, ok := r.lvl[a]; !ok || l > old {
				r.lvl[a] = l
			}
		}
		if any {
			pub++
		}
	}
}

func (r *redactor) route(hops []netip.Addr) {
	seq := make([][]netip.Addr, len(hops))
	for i, a := range hops {
		seq[i] = []netip.Addr{a}
	}
	r.seq(seq)
}

// hopJSON is one hop address as served. Public: private hops carry only
// "private", the ISP edge's address is masked, rDNS is omitted for the
// first three public hops.
type hopJSON struct {
	Hop     int    `json:"hop"`
	IP      string `json:"ip,omitempty"`
	NoReply bool   `json:"no_reply,omitempty"`
	Private bool   `json:"private,omitempty"`
	Masked  bool   `json:"masked,omitempty"`
	RDNS    string `json:"rdns,omitempty"`
	ASN     uint32 `json:"asn,omitempty"`
	Owner   string `json:"owner,omitempty"`
}

func (r *redactor) hop(n int, a netip.Addr) hopJSON {
	j := hopJSON{Hop: n}
	if !a.IsValid() {
		j.NoReply = true
		return j
	}
	a = a.Unmap()
	info := r.info[a]
	if privateAddr(a) {
		j.Private = true
		if !r.public {
			j.IP, j.RDNS = a.String(), info.RDNS
		}
		return j
	}
	j.ASN, j.Owner = info.ASN, info.Owner
	if !r.public {
		j.IP, j.RDNS = a.String(), info.RDNS
		return j
	}
	l, ok := r.lvl[a]
	if !ok {
		l = lvlMasked // not classified: be strict
	}
	switch {
	case l >= lvlMasked:
		j.IP, j.Masked = maskAddr(a), true
	case l == lvlNoRDNS:
		j.IP = a.String()
	default:
		j.IP, j.RDNS = a.String(), info.RDNS
	}
	return j
}

func (r *redactor) hops(hops []netip.Addr) []hopJSON {
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
	// InvestigateUntil is when the on-demand trace started from this UI
	// ends (local only, unix ms; omitted when none is running).
	InvestigateUntil int64 `json:"investigate_until,omitempty"`
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

// changesJSON redacts route changes with r (already fed every sequence).
func changesJSON(r *redactor, list []model.RouteChange) []changeJSON {
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

	r := newRedactor(s.public)
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
	r.seq(seq)
	if hasRoute {
		r.route(route.Hops)
	}
	for _, c := range changes {
		r.route(c.From)
		r.route(c.To)
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
			LossContinues: h.LossContinues})
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

// runCached renders through the public cache (and concurrency bound) when
// there is one, and writes the result or a redacted error.
func (s *server) runCached(w http.ResponseWriter, r *http.Request, key string, render func(ctx context.Context) (*cached, error)) {
	fill := func(ctx context.Context) (*cached, error) {
		if s.sem != nil {
			select {
			case s.sem <- struct{}{}:
				defer func() { <-s.sem }()
			case <-ctx.Done():
				return nil, errBusy
			}
		}
		return render(ctx)
	}
	ctx, cancel := context.WithTimeout(r.Context(), traceTimeout)
	defer cancel()
	var c *cached
	var err error
	if s.cache != nil {
		c, err = s.cache.get(ctx, key, fill)
	} else {
		c, err = fill(ctx)
	}
	switch {
	case err == nil:
	case errors.Is(err, errUnknownTarget):
		writeErr(w, r, http.StatusNotFound, "unknown target")
		return
	case errors.Is(err, errUnknownGroup):
		writeErr(w, r, http.StatusNotFound, "unknown group")
		return
	case errors.Is(err, errBusy), errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		w.Header().Set("Retry-After", "2")
		writeErr(w, r, http.StatusServiceUnavailable, "busy, retry shortly")
		return
	default:
		s.d.log().Error("query", "key", strings.SplitN(key, "\x00", 2)[0], "err", err)
		msg := "query failed"
		if !s.public {
			msg += ": " + strings.TrimSpace(err.Error())
		}
		writeErr(w, r, http.StatusInternalServerError, msg)
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
	s.runCached(w, r, "trace\x00"+target+"\x00"+p.key, func(ctx context.Context) (*cached, error) {
		t, err := s.trace(ctx, target, p, now)
		if err != nil {
			return nil, err
		}
		if s.inv != nil {
			if until := s.inv.until(target); !until.IsZero() {
				t.InvestigateUntil = until.UnixMilli()
			}
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
	hk := make([]string, len(hops))
	for i, n := range hops {
		hk[i] = strconv.Itoa(n)
	}
	p.group = "trace\x00" + target + "\x00" + strings.Join(hk, ",")
	if s.public {
		p.quantize(now)
	} else {
		p.setKey()
	}
	s.runCached(w, r, "tpanel\x00"+p.key, func(ctx context.Context) (*cached, error) {
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

// serveRouteChanges answers /api/routes/changes?from=&to=: every target's
// route changes, newest first, redacted like /api/trace.
func (s *server) serveRouteChanges(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if err := checkParams(q, "from", "to"); err != nil {
		writeErr(w, r, http.StatusBadRequest, err.Error())
		return
	}
	p, err := parseRange(q, s.now())
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, err.Error())
		return
	}
	s.runCached(w, r, "rchg\x00"+p.key, func(ctx context.Context) (*cached, error) {
		all, err := s.d.Trace.RouteChanges(ctx, p.from, p.to)
		if err != nil {
			return nil, err
		}
		var list []model.RouteChange
		for _, c := range all {
			if s.knownTarget(c.Target) {
				list = append(list, c)
			}
		}
		list = newestFirst(list)
		rd := newRedactor(s.public)
		for _, c := range list {
			rd.route(c.From)
			rd.route(c.To)
		}
		return jsonCached(changesJSON(rd, list))
	})
}

// ---------- on-demand traces (local only) ----------

type invSession struct {
	stop  func()
	until time.Time
	timer *time.Timer
}

// investigations tracks on-demand traces started from the local UI.
type investigations struct {
	mu  sync.Mutex
	m   map[string]*invSession
	now func() time.Time
}

var errTooManyInvestigations = errors.New("too many on-demand traces running")

// start starts or extends target's on-demand trace for InvestigateTTL. The
// previous session of the same target is stopped after the new one starts.
func (iv *investigations) start(src TraceSource, target string) (time.Time, error) {
	iv.mu.Lock()
	defer iv.mu.Unlock()
	now := iv.now()
	for k, s := range iv.m {
		if !now.Before(s.until) {
			delete(iv.m, k)
		}
	}
	old := iv.m[target]
	if old == nil && len(iv.m) >= MaxInvestigations {
		return time.Time{}, errTooManyInvestigations
	}
	stop, err := src.Investigate(target, InvestigateTTL)
	if err != nil {
		return time.Time{}, err
	}
	if stop == nil {
		stop = func() {}
	}
	sess := &invSession{stop: stop, until: now.Add(InvestigateTTL)}
	sess.timer = time.AfterFunc(InvestigateTTL, func() {
		iv.mu.Lock()
		defer iv.mu.Unlock()
		if iv.m[target] == sess {
			delete(iv.m, target)
		}
	})
	iv.m[target] = sess
	if old != nil {
		old.timer.Stop()
		old.stop()
	}
	return sess.until, nil
}

// until reports when target's on-demand trace ends (zero if none).
func (iv *investigations) until(target string) time.Time {
	iv.mu.Lock()
	defer iv.mu.Unlock()
	if s := iv.m[target]; s != nil && iv.now().Before(s.until) {
		return s.until
	}
	return time.Time{}
}

// serveInvestigate answers POST /api/investigate?target= (local only).
func (h *localHandler) serveInvestigate(w http.ResponseWriter, r *http.Request) {
	if h.d.Trace == nil {
		writeErr(w, r, http.StatusNotFound, "tracing is not available")
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeErr(w, r, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	q := r.URL.Query()
	if err := checkParams(q, "target"); err != nil {
		writeErr(w, r, http.StatusBadRequest, err.Error())
		return
	}
	if !h.controlAllowed(w, r, "on-demand traces") {
		return
	}
	target := q.Get("target")
	if !h.checkTarget(w, r, target) {
		return
	}
	until, err := h.inv.start(h.d.Trace, target)
	switch {
	case errors.Is(err, errTooManyInvestigations):
		w.Header().Set("Retry-After", "30")
		writeErr(w, r, http.StatusTooManyRequests, err.Error())
		return
	case err != nil:
		h.d.log().Warn("investigate", "target", target, "err", err)
		writeErr(w, r, http.StatusInternalServerError, "could not start trace: "+err.Error())
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"target": target, "until": until.UnixMilli()})
}
