package trace

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/asn"
	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/netinfo"
	"github.com/fuck-you-isp/fyisp/internal/netinfo/hops"
)

// Defaults of Options.
const (
	DefaultInterval    = 5 * time.Second
	DefaultInvestigate = 2500 * time.Millisecond
	DefaultMaxHops     = 20
	DefaultTimeout     = time.Second
	DefaultParallel    = 4  // probes in flight per target
	DefaultMaxInFlight = 32 // probes in flight in total
	DefaultRouterRate  = 1  // probes per second per router
	DefaultRevalidate  = 10 // rounds
	resolveTimeout     = 5 * time.Second
	rdnsTimeout        = time.Second
	hopRefresh         = 24 * time.Hour
	// growRounds is how many rounds in a row the destination must answer
	// only past its known TTL, behind silent hops, before the route counts
	// as longer (see round).
	growRounds = 5
)

// Options configures a Tracer. Zero fields use the defaults.
type Options struct {
	Interval    time.Duration // always-on traces: one round per Interval (default 5s)
	Investigate time.Duration // targets under Investigate (default 2.5s)
	MaxHops     int           // highest TTL (default 20, at most 255)
	Timeout     time.Duration // per hop (default 1s)
	Parallel    int           // probes in flight per target (default 4)
	MaxInFlight int           // probes in flight over all targets (default 32)

	// RouterRate caps the probes answered by one router, over all traces
	// (always-on and Investigate), per second (default 1; negative: no
	// cap). A probe over the cap is not sent and its hop is not measured
	// that round (see share.go).
	RouterRate float64
	// Revalidate: every Revalidate-th round, a trace probes its whole path
	// itself instead of using hops shared with other traces (default 10;
	// 1 never shares hops). See share.go.
	Revalidate int

	Now func() time.Time // wall clock for slots, route times and DNS refresh; default time.Now
	Log *slog.Logger     // default slog.Default()

	// Lookup maps a hop address to its AS (typically asn.Lookup); nil
	// leaves HopInfo.ASN/Owner empty.
	Lookup func(netip.Addr) asn.Info
	// ReverseDNS names a hop address; "" when it has none. Default:
	// net.DefaultResolver.LookupAddr. Called off the probing path with a 1s
	// timeout; results are cached (refreshed daily).
	ReverseDNS func(ctx context.Context, ip netip.Addr) string
	// Resolve resolves a target's hostname to IPv4 addresses, every
	// ResolveEvery (default 60s; after a failure every 10s). Default:
	// net.DefaultResolver.LookupNetIP(ctx, "ip4", host).
	Resolve      func(ctx context.Context, host string) ([]netip.Addr, error)
	ResolveEvery time.Duration
	// Path resolves the special hosts model.HostGateway and model.HostEdge
	// (typically netinfo.Watcher.Current). Nil: those targets are not traced.
	Path func() netinfo.Path

	// open opens the prober (tests: a fake). Default hops.Open.
	open func() (hops.Prober, error)
}

// New returns a Tracer. It opens its ICMP socket when Run starts.
func New(o Options) Tracer { return newTracer(o) }

func newTracer(o Options) *tracer {
	if o.Interval <= 0 {
		o.Interval = DefaultInterval
	}
	if o.Investigate <= 0 {
		o.Investigate = DefaultInvestigate
	}
	if o.MaxHops <= 0 {
		o.MaxHops = DefaultMaxHops
	}
	o.MaxHops = min(o.MaxHops, 255)
	if o.Timeout <= 0 {
		o.Timeout = DefaultTimeout
	}
	if o.Parallel <= 0 {
		o.Parallel = DefaultParallel
	}
	if o.MaxInFlight <= 0 {
		o.MaxInFlight = DefaultMaxInFlight
	}
	if o.RouterRate == 0 {
		o.RouterRate = DefaultRouterRate
	}
	if o.Revalidate <= 0 {
		o.Revalidate = DefaultRevalidate
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.ReverseDNS == nil {
		o.ReverseDNS = func(ctx context.Context, ip netip.Addr) string {
			names, err := net.DefaultResolver.LookupAddr(ctx, ip.String())
			if err != nil || len(names) == 0 {
				return ""
			}
			return strings.TrimSuffix(names[0], ".")
		}
	}
	if o.Resolve == nil {
		o.Resolve = func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip4", host)
		}
	}
	if o.ResolveEvery <= 0 {
		o.ResolveEvery = time.Minute
	}
	if o.open == nil {
		o.open = hops.Open
	}
	return &tracer{o: o, inflight: make(chan struct{}, o.MaxInFlight),
		share: newShareCache(), lim: newLimiter(o.RouterRate)}
}

type tracer struct {
	o        Options
	inflight chan struct{} // global probe budget
	share    *shareCache   // hops shared between traces (share.go)
	lim      *limiter      // per-router probe cap (share.go)
	gen      atomic.Uint64 // bumped on every route change: traces re-validate

	mu      sync.Mutex
	running bool
	ctx     context.Context
	sink    Sink
	prober  hops.Prober
	targets map[string]*target
	hops    *hopCache
	wg      sync.WaitGroup
	nextID  uint64
}

// target is one profile target. Fields under "tracer.mu" are guarded by
// tracer.mu; the round state is owned by the target's loop goroutine, and
// the route (read by Routes) by rmu.
type target struct {
	t     model.Target
	host  string
	phase float64 // fraction of the interval: spreads targets
	idx   int     // profile order: staggers re-validation rounds
	wake  chan struct{}

	// tracer.mu
	sessions map[uint64]time.Time // Investigate callers -> deadline
	looping  bool
	curIv    time.Duration

	// loop goroutine
	lastSlot  time.Time
	knownLen  int // hop count of the route (destination TTL once reached)
	grow      int // consecutive rounds the destination answered only beyond knownLen
	resolved  netip.Addr
	resolveAt time.Time // next lookup
	resolving atomic.Bool
	res       atomic.Pointer[resolution]
	gen       uint64 // tracer.gen at the last round

	umu  sync.Mutex
	used map[int]time.Time // TTL -> time of the newest measurement emitted

	rmu      sync.Mutex
	hasRoute bool
	route    model.Route
	pending  []netip.Addr // a different route seen in the previous round
	dst      netip.Addr   // the address the route leads to
}

type resolution struct {
	addrs []netip.Addr
	err   error
}

var errNotRunning = errors.New("trace: not running")

func (tr *tracer) Run(ctx context.Context, p *model.Profile, sink Sink) error {
	if p == nil {
		return errors.New("trace: nil profile")
	}
	if sink == nil {
		return errors.New("trace: nil sink")
	}
	pr, err := tr.o.open()
	if err != nil {
		return fmt.Errorf("trace: TTL-limited ICMP unavailable: %w", err)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	tr.mu.Lock()
	if tr.running {
		tr.mu.Unlock()
		_ = pr.Close()
		return errors.New("trace: already running")
	}
	tr.running, tr.ctx, tr.sink, tr.prober = true, ctx, sink, pr
	tr.hops = newHopCache(tr.o, sink)
	tr.targets = map[string]*target{}
	for i, t := range p.Targets {
		tr.targets[t.Name] = &target{
			t: t, host: t.HostFor(model.KindICMP),
			phase: float64(i) / float64(len(p.Targets)), idx: i,
			wake: make(chan struct{}, 1), sessions: map[uint64]time.Time{},
		}
	}
	n := 0
	for _, st := range tr.targets {
		if st.t.Trace {
			n++
			tr.startLoop(st)
		}
	}
	tr.mu.Unlock()
	tr.o.Log.Info("tracing", "targets", n, "interval", tr.o.Interval, "max_hops", tr.o.MaxHops,
		"router_rate", tr.o.RouterRate, "revalidate", tr.o.Revalidate)

	hopsDone := make(chan struct{})
	go func() { defer close(hopsDone); tr.hops.run(ctx) }()
	<-ctx.Done()

	tr.mu.Lock()
	tr.running = false
	tr.mu.Unlock()
	tr.wg.Wait()
	<-hopsDone
	_ = pr.Close()
	return nil
}

// startLoop starts st's loop goroutine. tr.mu is held.
func (tr *tracer) startLoop(st *target) {
	st.looping = true
	tr.wg.Add(1)
	go func() {
		defer tr.wg.Done()
		tr.loop(tr.ctx, st)
	}()
}

func (tr *tracer) Investigate(name string, ttl time.Duration) (func(), error) {
	if ttl <= 0 {
		return nil, errors.New("trace: ttl must be positive")
	}
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if !tr.running {
		return nil, errNotRunning
	}
	st := tr.targets[name]
	if st == nil {
		return nil, fmt.Errorf("trace: unknown target %q", name)
	}
	if isSpecial(st.host) && tr.o.Path == nil {
		return nil, fmt.Errorf("trace: target %q: %s cannot be resolved", name, st.host)
	}
	tr.nextID++
	id := tr.nextID
	st.sessions[id] = tr.o.Now().Add(ttl)
	if !st.looping {
		tr.startLoop(st)
	} else {
		poke(st.wake)
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			tr.mu.Lock()
			delete(st.sessions, id)
			tr.mu.Unlock()
			poke(st.wake)
		})
	}, nil
}

func poke(c chan struct{}) {
	select {
	case c <- struct{}{}:
	default:
	}
}

func (tr *tracer) Routes() []model.Route {
	tr.mu.Lock()
	ts := make([]*target, 0, len(tr.targets))
	for _, st := range tr.targets {
		ts = append(ts, st)
	}
	tr.mu.Unlock()
	var out []model.Route
	for _, st := range ts {
		st.rmu.Lock()
		if st.hasRoute {
			r := st.route
			r.Hops = slices.Clone(r.Hops)
			out = append(out, r)
		}
		st.rmu.Unlock()
	}
	slices.SortFunc(out, func(a, b model.Route) int { return strings.Compare(a.Target, b.Target) })
	return out
}

// Interval returns the slot interval of target's trace series right now:
// Options.Investigate while it is investigated, else Options.Interval.
func (tr *tracer) Interval(name string) time.Duration {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if st := tr.targets[name]; st != nil && st.curIv > 0 {
		return st.curIv
	}
	return tr.o.Interval
}

// interval is st's current interval; ok is false when st should not be
// traced any more (then its loop ends).
func (tr *tracer) interval(st *target) (time.Duration, bool) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	now := tr.o.Now()
	for id, dl := range st.sessions {
		if !now.Before(dl) {
			delete(st.sessions, id)
		}
	}
	switch {
	case !tr.running:
		st.looping = false
		return 0, false
	case len(st.sessions) > 0:
		st.curIv = tr.o.Investigate
	case st.t.Trace:
		st.curIv = tr.o.Interval
	default:
		st.looping = false
		st.curIv = 0
		return 0, false
	}
	return st.curIv, true
}

// loop runs one round per slot, one at a time, until st is no longer traced.
// Slots are aligned to multiples of the interval since the Unix epoch (like
// package probe); each target fires at a phase within the slot.
func (tr *tracer) loop(ctx context.Context, st *target) {
	for {
		iv, ok := tr.interval(st)
		if !ok || ctx.Err() != nil {
			return
		}
		now := tr.o.Now()
		phase := time.Duration(st.phase * float64(iv))
		n := slotIndex(now, iv, phase)
		slot := time.Unix(0, n*int64(iv)).UTC()
		if !st.lastSlot.IsZero() && st.lastSlot.Sub(slot) > time.Minute {
			st.lastSlot = time.Time{} // the clock was stepped back
		}
		if slot.After(st.lastSlot) {
			st.lastSlot = slot
			tr.round(ctx, st, slot)
			continue
		}
		wait := time.Unix(0, (n+1)*int64(iv)+int64(phase)).Sub(now)
		wait = max(min(wait, iv), time.Millisecond)
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-st.wake:
		case <-t.C:
		}
		t.Stop()
	}
}

func slotIndex(now time.Time, iv, phase time.Duration) int64 {
	x := now.UnixNano() - int64(phase)
	n := x / int64(iv)
	if x%int64(iv) < 0 {
		n--
	}
	return n
}

func isSpecial(h string) bool { return h == model.HostGateway || h == model.HostEdge }

// dest returns the address to trace to.
func (tr *tracer) dest(ctx context.Context, st *target) (netip.Addr, error) {
	switch {
	case isSpecial(st.host):
		if tr.o.Path == nil {
			return netip.Addr{}, errors.New("no network path")
		}
		p := tr.o.Path()
		a := p.Gateway
		if st.host == model.HostEdge {
			a = p.Edge
		}
		if !a.IsValid() {
			return netip.Addr{}, errors.New(st.host + " not known yet")
		}
		return a, nil
	}
	if a, err := netip.ParseAddr(st.host); err == nil {
		return a.Unmap(), nil
	}
	now := tr.o.Now()
	if !st.resolved.IsValid() {
		tr.resolve(ctx, st) // first round: wait for the address
	} else if now.After(st.resolveAt) && st.resolving.CompareAndSwap(false, true) {
		go func() { defer st.resolving.Store(false); tr.resolve(ctx, st) }()
	}
	r := st.res.Load()
	if r == nil {
		return netip.Addr{}, errors.New("not resolved")
	}
	// Stick to the current address while DNS still returns it, so that
	// round-robin DNS does not look like a route change.
	if st.resolved.IsValid() && !slices.Contains(r.addrs, st.resolved) && len(r.addrs) > 0 {
		st.resolved = r.addrs[0]
	}
	if !st.resolved.IsValid() {
		if len(r.addrs) == 0 {
			return netip.Addr{}, fmt.Errorf("dns: %v", r.err)
		}
		st.resolved = r.addrs[0]
	}
	if r.err == nil {
		st.resolveAt = now.Add(tr.o.ResolveEvery)
	} else {
		st.resolveAt = now.Add(min(10*time.Second, tr.o.ResolveEvery))
	}
	return st.resolved, nil
}

// resolve looks st.host up. A failed lookup keeps the previous addresses.
func (tr *tracer) resolve(ctx context.Context, st *target) {
	ctx, cancel := context.WithTimeout(ctx, resolveTimeout)
	defer cancel()
	ips, err := tr.o.Resolve(ctx, st.host)
	var addrs []netip.Addr
	for _, a := range ips {
		if a = a.Unmap(); a.Is4() {
			addrs = append(addrs, a)
		}
	}
	if err == nil && len(addrs) == 0 {
		err = errors.New("no IPv4 address")
	}
	if err != nil {
		if prev := st.res.Load(); prev != nil {
			addrs = prev.addrs
		}
		tr.o.Log.Debug("trace: dns lookup failed", "target", st.t.Name, "err", err)
	}
	st.res.Store(&resolution{addrs: addrs, err: err})
}

// hopResult is the outcome of one TTL in a round.
type hopResult struct {
	h       hops.Hop
	err     error
	skipped bool // not probed (router cap): not measured
}

// plan is what a round needs to probe: where, for which slot, and what the
// target's route says about each TTL.
type plan struct {
	st       *target
	dst      netip.Addr
	from, to time.Time    // the slot's window: [slot, slot+interval)
	hops     []netip.Addr // the route, when it leads to dst (else nil): hops[k-1] is TTL k
	full     bool         // probe every TTL (re-validation): do not use others' measurements
}

// within returns t moved into p's window.
func (p *plan) within(t time.Time) time.Time {
	if t.Before(p.from) {
		return p.from
	}
	if !t.Before(p.to) {
		return p.to.Add(-time.Nanosecond)
	}
	return t
}

// dstTTL is the destination's TTL (0: unknown).
func (p *plan) dstTTL() int { return len(p.hops) }

// shareKey returns the prefix key of ttl, if ttl may be shared (before the
// known destination).
func (p *plan) shareKey(ttl int) (string, bool) {
	if ttl >= p.dstTTL() {
		return "", false
	}
	return prefixKey(p.hops[:ttl]), true
}

// limitKey returns the budget a probe at ttl is charged to; exempt: the
// destination answers it (not capped).
func (p *plan) limitKey(tr *tracer, ttl int) (k limitKey, exempt bool) {
	if n := p.dstTTL(); n > 0 {
		if ttl >= n {
			return limitKey{}, true
		}
		if a := p.hops[ttl-1]; a.IsValid() {
			return limitKey{addr: a}, a == p.dst
		}
	}
	if a := tr.lim.predict(ttl); a.IsValid() {
		return limitKey{addr: a}, a == p.dst
	}
	return limitKey{ttl: ttl}, false
}

// use records that st emits a measurement of ttl made at at; false (and
// nothing recorded) if st already emitted one made at or after at, so that
// one measurement never fills two slots of a series.
func (st *target) use(ttl int, at time.Time) bool {
	st.umu.Lock()
	defer st.umu.Unlock()
	if last, ok := st.used[ttl]; ok && !at.After(last) {
		return false
	}
	if st.used == nil {
		st.used = map[int]time.Time{}
	}
	st.used[ttl] = at
	return true
}

// round traces st once and reports samples, hop sightings and the route.
func (tr *tracer) round(ctx context.Context, st *target, slot time.Time) {
	dst, err := tr.dest(ctx, st)
	if err != nil {
		tr.o.Log.Debug("trace: skipping round", "target", st.t.Name, "err", err)
		return
	}
	tr.mu.Lock()
	iv := st.curIv
	tr.mu.Unlock()
	if iv <= 0 {
		iv = tr.o.Interval
	}
	p := &plan{st: st, dst: dst, from: slot, to: slot.Add(iv)}
	st.rmu.Lock()
	if st.hasRoute && st.dst == dst {
		if h := st.route.Hops; len(h) > 0 && h[len(h)-1] == dst {
			p.hops = slices.Clone(h)
		}
	}
	pending := st.pending != nil
	st.rmu.Unlock()
	gen := tr.gen.Load()
	rv := int64(tr.o.Revalidate)
	turn := (slot.UnixNano()/int64(iv) + int64(st.idx)) % rv
	p.full = p.hops == nil || pending || st.gen != gen || turn == 0 || turn == -rv
	st.gen = gen

	res := tr.probeAll(ctx, p)
	if ctx.Err() != nil {
		return // cancelled mid-round: not measured
	}

	st.rmu.Lock()
	if st.dst.IsValid() && st.dst != dst {
		// A new address (DNS) is a new path, not a route change.
		tr.o.Log.Info("trace: target address changed; route reset", "target", st.t.Name)
		st.hasRoute, st.pending, st.knownLen = false, nil, 0
	}
	st.dst = dst
	st.rmu.Unlock()

	// n: the hop count to report. The destination (or an "unreachable"
	// answer) ends the route; otherwise report up to the longest route seen
	// since, so loss at the destination shows as loss there.
	n, end := 0, 0
	for ttl := 1; ttl < len(res); ttl++ {
		if h := res[ttl].h; h.Addr.IsValid() {
			n = ttl
			if h.Reached || h.Unreach {
				end = ttl
				break
			}
		}
	}
	if end > 0 && (p.dstTTL() == 0 || end < p.dstTTL()) && skipped(res, 1, end) {
		// A TTL before the answer was not probed (router cap), and nothing
		// says it was not the destination: the route's length is unknown.
		tr.o.Log.Debug("trace: round incomplete (router cap)", "target", st.t.Name)
		return
	}
	if end > 0 {
		// The destination answers every TTL from its own on. If the probe
		// at its known TTL got lost, a higher TTL may still reach it: that
		// is loss at the destination, not a longer route. Only when this
		// persists (a new router that never answers) is the route longer.
		if st.knownLen > 0 && end > st.knownLen && silent(res, st.knownLen, end) {
			st.grow++
			if st.grow < growRounds {
				end = st.knownLen
			} else {
				st.grow = 0
			}
		} else {
			st.grow = 0
		}
		st.knownLen, n = end, end
	} else {
		n = min(max(n+1, st.knownLen), tr.o.MaxHops)
		st.knownLen, st.grow = n, 0
	}

	sink := tr.sink
	addrs := make([]netip.Addr, n)
	for ttl := 1; ttl <= n; ttl++ {
		r := res[ttl]
		if r.skipped {
			continue // not measured: no sample
		}
		s := model.Sample{Key: model.SeriesKey{Target: st.t.Name, Kind: model.KindTrace, Hop: uint8(ttl)}, Slot: slot}
		switch {
		case r.h.Addr.IsValid():
			s.RTT = max(r.h.RTT, 0)
			addrs[ttl-1] = r.h.Addr
			tr.hops.seen(r.h.Addr)
		case r.err != nil:
			s.Lost, s.Reason, s.Err = true, reasonOf(r.err), r.err.Error()
		default:
			s.Lost, s.Reason = true, model.ReasonTimeout
		}
		sink.Observe(s)
	}
	tr.updateRoute(st, addrs, skipped(res, 1, n+1))
}

// probeAll sends TTL 1.. towards p.dst, at most Parallel at a time per
// target and MaxInFlight overall, until the destination (or an unreachable)
// answers or MaxHops. res[ttl] is the answer for ttl; res[0] is unused.
func (tr *tracer) probeAll(ctx context.Context, p *plan) []hopResult {
	res := make([]hopResult, tr.o.MaxHops+1)
	var stop atomic.Int32 // lowest TTL that ended the route
	stop.Store(int32(tr.o.MaxHops + 1))
	per := make(chan struct{}, tr.o.Parallel)
	var wg sync.WaitGroup
	// With a known destination TTL, TTLs past it are sent only once it
	// did not answer (it answers them all).
	d := p.dstTTL()
	dstDone := make(chan struct{})
launch:
	for ttl := 1; ttl <= tr.o.MaxHops; ttl++ {
		if d > 0 && ttl == d+1 {
			select {
			case <-dstDone:
			case <-ctx.Done():
				break launch
			}
			if int32(ttl) > stop.Load() {
				break
			}
		}
		select {
		case per <- struct{}{}:
		case <-ctx.Done():
			break launch
		}
		select {
		case tr.inflight <- struct{}{}:
		case <-ctx.Done():
			<-per
			break launch
		}
		if int32(ttl) > stop.Load() {
			<-tr.inflight
			<-per
			break
		}
		wg.Add(1)
		go func() {
			defer func() { <-tr.inflight; <-per; wg.Done() }()
			if ttl == d {
				defer close(dstDone)
			}
			r := tr.hop(ctx, p, ttl)
			res[ttl] = r
			if h := r.h; h.Addr.IsValid() && (h.Reached || h.Unreach) {
				for {
					cur := stop.Load()
					if int32(ttl) >= cur || stop.CompareAndSwap(cur, int32(ttl)) {
						break
					}
				}
			}
		}()
	}
	wg.Wait()
	// Answers past the end of the route (the destination answering a
	// higher TTL too) are not hops.
	for ttl := int(stop.Load()) + 1; ttl <= tr.o.MaxHops; ttl++ {
		res[ttl] = hopResult{}
	}
	return res
}

// hop measures ttl for p: a measurement shared by another trace in this slot
// when p's prefix allows, else a probe of its own (capped per router), else
// a recent measurement of the same hop, else nothing (skipped). See share.go.
func (tr *tracer) hop(ctx context.Context, p *plan, ttl int) hopResult {
	key, share := p.shareKey(ttl)
	var r hopResult
	var at time.Time
	sent := false
	for share {
		sp, owner := tr.share.claim(key, p.from, p.to, tr.o.Now())
		if owner {
			r, at = tr.send(ctx, p, ttl, sp.at)
			sp.finish(r, !r.skipped && agrees(r, p.hops[ttl-1]))
			sent = true
			break
		}
		if p.full {
			break
		}
		select {
		case <-sp.done:
		case <-ctx.Done():
			return hopResult{skipped: true}
		}
		if !sp.ok {
			continue // not shareable: claim again (probe it, or use another's)
		}
		if p.st.use(ttl, sp.at) {
			return sp.res
		}
		break
	}
	if !sent {
		r, at = tr.send(ctx, p, ttl, time.Time{})
	}
	if !r.skipped {
		p.st.use(ttl, at)
		return r
	}
	// Over the cap: a measurement of this hop made since the slot started
	// or within the cap's interval stands in, if not emitted yet.
	since := tr.o.Now().Add(-tr.lim.maxAge())
	if p.from.Before(since) {
		since = p.from
	}
	var sp *shareProbe
	if share {
		sp = tr.share.recent(key, since)
	}
	if k, _ := p.limitKey(tr, ttl); sp == nil && k.addr.IsValid() && k.addr != p.dst {
		sp = tr.share.recentRouter(ttl, k.addr, since)
	}
	if sp != nil && p.st.use(ttl, sp.at) {
		r = sp.res
	}
	return r
}

// agrees reports whether r may stand for every trace whose route has expect
// at that TTL: expect answered, or nothing did.
func agrees(r hopResult, expect netip.Addr) bool {
	if r.err != nil || r.h.Reached || r.h.Unreach {
		return false
	}
	return !r.h.Addr.IsValid() || r.h.Addr == expect
}

// send probes ttl towards p.dst unless the router cap refuses (skipped). at
// is the time the measurement is filed under (within p's window); zero: now.
func (tr *tracer) send(ctx context.Context, p *plan, ttl int, at time.Time) (hopResult, time.Time) {
	now := tr.o.Now()
	if at.IsZero() {
		at = p.within(now)
	}
	k, exempt := p.limitKey(tr, ttl)
	var keys []limitKey
	if !exempt {
		var ok bool
		if keys, ok = tr.lim.take(k, now); !ok {
			return hopResult{skipped: true}, at
		}
	}
	tr.mu.Lock()
	pr := tr.prober
	tr.mu.Unlock()
	h, err := pr.Probe(ctx, p.dst, ttl, tr.o.Timeout)
	if h.Addr.IsValid() {
		h.Addr = h.Addr.Unmap()
	}
	r := hopResult{h: h, err: err}
	if h.Addr.IsValid() && !h.Reached {
		tr.lim.answered(ttl, k.addr, h.Addr, keys, now)
		if !h.Unreach {
			tr.share.answered(ttl, r, at)
		}
	}
	return r, at
}

// skipped reports whether a TTL in [from, to) was not probed.
func skipped(res []hopResult, from, to int) bool {
	for ttl := max(from, 1); ttl < to && ttl < len(res); ttl++ {
		if res[ttl].skipped {
			return true
		}
	}
	return false
}

// silent reports whether no TTL in [from, to) answered.
func silent(res []hopResult, from, to int) bool {
	for ttl := max(from, 1); ttl < to && ttl < len(res); ttl++ {
		if res[ttl].h.Addr.IsValid() {
			return false
		}
	}
	return true
}

func reasonOf(err error) model.Reason {
	switch {
	case errors.Is(err, syscall.ENETUNREACH), errors.Is(err, syscall.ENETDOWN):
		return model.ReasonNoNetwork
	case errors.Is(err, syscall.EHOSTUNREACH):
		return model.ReasonUnreachable
	}
	return model.ReasonOther
}

// updateRoute compares a round's hops with st's route. A different route
// (see firstDiff) replaces it once it was seen in 2 consecutive rounds. A
// round with hops that were not measured (partial) that fits both the route
// and a pending different route neither confirms nor drops the pending one.
func (tr *tracer) updateRoute(st *target, obs []netip.Addr, partial bool) {
	now := tr.o.Now()
	var change *model.RouteChange
	st.rmu.Lock()
	switch {
	case !st.hasRoute:
		st.hasRoute = true
		st.route = model.Route{Target: st.t.Name, Hops: obs, Since: now}
		st.pending = nil
	case firstDiff(st.route.Hops, obs) == 0:
		st.route.Hops = merge(st.route.Hops, obs)
		if !partial || st.pending == nil || firstDiff(st.pending, obs) != 0 {
			st.pending = nil
		}
	case st.pending != nil && firstDiff(st.pending, obs) == 0:
		to := merge(st.pending, obs)
		change = &model.RouteChange{Target: st.t.Name, At: now, From: slices.Clone(st.route.Hops), To: slices.Clone(to),
			FirstDiff: firstDiff(st.route.Hops, to)}
		st.route = model.Route{Target: st.t.Name, Hops: to, Since: now}
		st.pending = nil
	default:
		st.pending = obs
	}
	r := st.route
	r.Hops = slices.Clone(r.Hops)
	st.rmu.Unlock()
	if change != nil {
		tr.gen.Add(1) // every trace re-validates its path (share.go)
		tr.o.Log.Info("trace: route changed", "target", change.Target, "first_diff", change.FirstDiff,
			"from_hops", len(change.From), "to_hops", len(change.To))
		tr.sink.ObserveRouteChange(*change)
	}
	tr.sink.ObserveRoute(r)
}

// firstDiff returns the first TTL (1-based) at which route b differs from
// route a, or 0 when they are the same. A hop that did not answer (invalid
// address) matches any address: silent hops come and go (ICMP rate limits)
// without the route changing. Routes of different lengths always differ.
func firstDiff(a, b []netip.Addr) int {
	n := min(len(a), len(b))
	for i := range n {
		if a[i].IsValid() && b[i].IsValid() && a[i] != b[i] {
			return i + 1
		}
	}
	if len(a) == len(b) {
		return 0
	}
	for i := range n {
		if a[i].IsValid() != b[i].IsValid() {
			return i + 1
		}
	}
	return n + 1
}

// merge returns b with its silent hops filled in from a (same length).
func merge(a, b []netip.Addr) []netip.Addr {
	out := slices.Clone(b)
	for i := range out {
		if !out[i].IsValid() && i < len(a) {
			out[i] = a[i]
		}
	}
	return out
}
