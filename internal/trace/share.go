package trace

// Shared hops and the per-router probe cap.
//
// Traces to different targets usually start with the same routers: the home
// gateway and the ISP's access routers. Probing those once per trace and per
// round multiplies the ICMP time-exceeded messages they must send by the
// number of traces, and routers rate-limit those messages (Linux: about 1/s
// per destination host; many home gateways less). Seven traces every 5s ask
// the gateway for 1.4 answers/s, and hop 1 then shows loss that is not real.
//
// # Shared hops: measured once per round
//
// A trace whose route reached its destination may use another trace's
// measurement of TTL k when both routes agree on hops 1..k (the prefix,
// silent hops included) and k is before the destination. The first trace
// that needs TTL k of a prefix in a slot (the window [slot, slot+interval))
// probes it, towards its own destination; every other trace with that prefix
// emits that one measurement as its own sample for the slot (same Slot, RTT,
// loss). So a hop shared by every trace (gateway, ISP) is probed once per
// round, whatever the number of traces, and a hop shared by some of them
// only (a CDN's routers) once per round among those. Traces that diverge at
// TTL k keep sharing TTLs below k and probe from k on themselves.
//
// A measurement is shared only when it agrees with the prefix: the expected
// router answered, or nothing answered (loss is shared: it is the same hop
// in the same slot). Another router, the destination itself, "unreachable" or
// a local error are not shared; every other trace then probes that TTL
// itself, so a route change at a shared hop is seen in each trace's own
// probes and reported once per trace (updateRoute).
//
// Re-validation: a trace probes its whole path itself (still offering its
// measurements to the others) until it has a route, while a different route
// is pending, in its first round after any trace's route changed, and every
// Options.Revalidate-th round (staggered over the traces). A path that
// diverges from the shared prefix towards one destination only is so found
// within Revalidate rounds.
//
// # Per-router cap
//
// Every probe also takes a token from a bucket per router (Options.RouterRate
// probes/s, burst 2) shared by all traces, always-on and Investigate: over
// any period T a router gets at most 2+T*RouterRate probes. (The burst of 2
// lets a trace re-validate a shared hop right after another trace probed it;
// Linux routers allow a burst of 6.) The
// router is the one expected to answer: the trace's own route at that TTL,
// else the router that last answered that TTL for any trace, else (unknown)
// the TTL itself. A router that answered instead of the expected one is
// charged too, and checked along with it until the routes catch up. Probes at
// or past the destination's TTL are answered by the destination with an echo
// reply (not an ICMP error) and are not capped.
//
// A probe over the cap is not sent, and its hop gets no sample for that round
// (not measured: a gap, not loss, since nothing was lost) — unless a
// measurement of the same hop at most 1/RouterRate old (same prefix, or the
// expected router at the same TTL) exists that this trace has not emitted
// yet: then that one is emitted, as for a shared hop. Emitting loss would
// report the cap as packet loss, the very noise the cap removes; reusing a
// measurement of the same router made within the last second loses nothing.
// The route treats a hop that was not measured as unknown (it neither
// confirms nor contradicts a pending route change), and a round without a
// known route to its destination that skipped a TTL before the destination
// answered is not reported (the skipped TTL might have been the
// destination).

import (
	"net/netip"
	"strings"
	"sync"
	"time"
)

const (
	shareKeep   = 4           // probes kept per prefix
	shareMaxAge = time.Minute // prefixes not probed for this long are dropped
	maxBuckets  = 4096
	burst       = 2 // tokens per router bucket
)

// shareProbe is one measurement of a TTL, offered to every trace with the
// same prefix.
type shareProbe struct {
	at   time.Time     // inside the window of the round that made it
	done chan struct{} // closed once res and ok are set
	res  hopResult
	ok   bool // res may be used by other traces
}

func (p *shareProbe) finish(r hopResult, ok bool) {
	p.res, p.ok = r, ok
	close(p.done)
}

func (p *shareProbe) finished() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

type routerKey struct {
	ttl  int
	addr netip.Addr
}

// shareCache holds recent measurements by prefix, and the last measurement
// answered by each router at each TTL.
type shareCache struct {
	mu     sync.Mutex
	m      map[string][]*shareProbe // prefix key -> recent probes, oldest first
	router map[routerKey]*shareProbe
	swept  time.Time
}

func newShareCache() *shareCache {
	return &shareCache{m: map[string][]*shareProbe{}, router: map[routerKey]*shareProbe{}}
}

// prefixKey is the key of TTL len(hops): the addresses of hops 1..len(hops).
func prefixKey(hops []netip.Addr) string {
	var b strings.Builder
	for _, a := range hops {
		if a.IsValid() {
			b.WriteString(a.String())
		}
		b.WriteByte(',')
	}
	return b.String()
}

// claim returns the newest probe of key made in [from, to) that is in
// flight or shareable (owner false), or a new one that the caller must probe
// and finish (owner true).
func (c *shareCache) claim(key string, from, to, now time.Time) (*shareProbe, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ps := c.m[key]
	for i := len(ps) - 1; i >= 0; i-- {
		p := ps[i]
		if at := p.at; !at.Before(from) && at.Before(to) && (!p.finished() || p.ok) {
			return p, false
		}
	}
	at := now
	if at.Before(from) {
		at = from
	}
	if !at.Before(to) {
		at = to.Add(-time.Nanosecond)
	}
	p := &shareProbe{at: at, done: make(chan struct{})}
	ps = append(ps, p)
	if len(ps) > shareKeep {
		ps = ps[len(ps)-shareKeep:]
	}
	c.m[key] = ps
	c.sweep(now)
	return p, true
}

// recent returns the newest finished, shareable probe of key made at or
// after since.
func (c *shareCache) recent(key string, since time.Time) *shareProbe {
	c.mu.Lock()
	defer c.mu.Unlock()
	ps := c.m[key]
	for i := len(ps) - 1; i >= 0; i-- {
		p := ps[i]
		if p.at.Before(since) {
			break
		}
		if p.finished() && p.ok {
			return p
		}
	}
	return nil
}

// answered records r (a router's answer at ttl, measured at at) for recentRouter.
func (c *shareCache) answered(ttl int, r hopResult, at time.Time) {
	p := &shareProbe{at: at, done: make(chan struct{})}
	p.finish(r, true)
	c.mu.Lock()
	c.router[routerKey{ttl, r.h.Addr}] = p
	c.mu.Unlock()
}

// recentRouter returns the newest answer of router at ttl made at or after
// since.
func (c *shareCache) recentRouter(ttl int, router netip.Addr, since time.Time) *shareProbe {
	c.mu.Lock()
	defer c.mu.Unlock()
	if p := c.router[routerKey{ttl, router}]; p != nil && !p.at.Before(since) {
		return p
	}
	return nil
}

// sweep drops old entries (at most once a minute). c.mu is held.
func (c *shareCache) sweep(now time.Time) {
	if d := now.Sub(c.swept); d >= 0 && d < shareMaxAge {
		return
	}
	c.swept = now
	for k, ps := range c.m {
		if now.Sub(ps[len(ps)-1].at) > shareMaxAge {
			delete(c.m, k)
		}
	}
	for k, p := range c.router {
		if now.Sub(p.at) > shareMaxAge {
			delete(c.router, k)
		}
	}
}

// limitKey names a probe budget: a router address, or a TTL whose router is
// not known (addr invalid).
type limitKey struct {
	addr netip.Addr
	ttl  int
}

type bucket struct {
	tokens float64
	last   time.Time
}

type alias struct {
	to netip.Addr
	at time.Time
}

// limiter caps probes per router: a token bucket per limitKey, rate per
// second, burst 2. rate < 0: no cap.
type limiter struct {
	rate float64

	mu    sync.Mutex
	b     map[limitKey]*bucket
	alias map[netip.Addr]alias // expected router -> the router that answered instead
	atTTL map[int]netip.Addr   // router that last answered each TTL (any trace)
}

func newLimiter(rate float64) *limiter {
	return &limiter{rate: rate, b: map[limitKey]*bucket{}, alias: map[netip.Addr]alias{}, atTTL: map[int]netip.Addr{}}
}

// maxAge is how old a measurement may be to stand in for a capped probe.
func (l *limiter) maxAge() time.Duration {
	if l.rate <= 0 {
		return 0
	}
	return time.Duration(float64(time.Second) / l.rate)
}

// predict returns the router that last answered ttl for any trace.
func (l *limiter) predict(ttl int) netip.Addr {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.atTTL[ttl]
}

// bucket returns k's bucket refilled to now. l.mu is held.
func (l *limiter) bucket(k limitKey, now time.Time) *bucket {
	b := l.b[k]
	if b == nil {
		if len(l.b) >= maxBuckets {
			for k2, b2 := range l.b {
				if now.Sub(b2.last).Seconds()*l.rate+b2.tokens >= burst {
					delete(l.b, k2)
				}
			}
		}
		b = &bucket{tokens: burst, last: now}
		l.b[k] = b
		return b
	}
	if d := now.Sub(b.last); d > 0 {
		b.tokens = min(burst, b.tokens+d.Seconds()*l.rate)
		b.last = now
	}
	return b
}

// take takes a token for k (and for the router that recently answered in k's
// place) if all have one. It returns the keys charged.
func (l *limiter) take(k limitKey, now time.Time) ([]limitKey, bool) {
	if l.rate < 0 {
		return nil, true
	}
	keys := []limitKey{k}
	l.mu.Lock()
	defer l.mu.Unlock()
	if k.addr.IsValid() {
		if al, ok := l.alias[k.addr]; ok {
			if now.Sub(al.at) < shareMaxAge {
				keys = append(keys, limitKey{addr: al.to})
			} else {
				delete(l.alias, k.addr)
			}
		}
	}
	for _, key := range keys {
		if l.bucket(key, now).tokens < 1 {
			return nil, false
		}
	}
	for _, key := range keys {
		l.b[key].tokens--
	}
	return keys, true
}

// answered records that router answered ttl (a time-exceeded or unreachable
// message) for a probe charged to keys; expect is the router that was
// expected (may be invalid).
func (l *limiter) answered(ttl int, expect, router netip.Addr, keys []limitKey, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.atTTL[ttl] = router
	if expect.IsValid() {
		if router != expect {
			l.alias[expect] = alias{to: router, at: now}
		} else {
			delete(l.alias, expect)
		}
	}
	if l.rate < 0 {
		return
	}
	for _, k := range keys {
		if k.addr == router {
			return
		}
	}
	b := l.bucket(limitKey{addr: router}, now)
	b.tokens = max(b.tokens-1, -1)
}
