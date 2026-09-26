package trace

import (
	"context"
	"net/netip"
	"slices"
	"sync"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

const (
	hopWorkers  = 2
	hopQueue    = 256
	maxHopCache = 4096
)

// hopCache reports every hop address once (and again daily) with its
// reverse DNS name and AS. Lookups run on worker goroutines, never on the
// probing path; a full queue drops the lookup until the address is seen
// again.
type hopCache struct {
	o    Options
	sink Sink
	q    chan netip.Addr

	mu sync.Mutex
	m  map[netip.Addr]*hopEntry
}

type hopEntry struct {
	first, last time.Time
	reported    time.Time // last ObserveHop; zero: never
	queued      bool
}

func newHopCache(o Options, sink Sink) *hopCache {
	return &hopCache{o: o, sink: sink, q: make(chan netip.Addr, hopQueue), m: map[netip.Addr]*hopEntry{}}
}

// seen records a sighting of a hop address. It never blocks.
func (c *hopCache) seen(a netip.Addr) {
	now := c.o.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.m[a]
	if e == nil {
		if len(c.m) >= maxHopCache {
			c.evict()
		}
		e = &hopEntry{first: now}
		c.m[a] = e
	}
	e.last = now
	if e.queued || (!e.reported.IsZero() && now.Sub(e.reported) < hopRefresh) {
		return
	}
	select {
	case c.q <- a:
		e.queued = true
	default:
	}
}

// evict drops the least recently seen half of the cache. c.mu is held.
func (c *hopCache) evict() {
	ts := make([]time.Time, 0, len(c.m))
	for _, e := range c.m {
		ts = append(ts, e.last)
	}
	slices.SortFunc(ts, time.Time.Compare)
	cut := ts[len(ts)/2]
	for a, e := range c.m {
		if e.last.Before(cut) && !e.queued {
			delete(c.m, a)
		}
	}
}

func (c *hopCache) run(ctx context.Context) {
	var wg sync.WaitGroup
	for range hopWorkers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case a := <-c.q:
					c.lookup(ctx, a)
				}
			}
		}()
	}
	wg.Wait()
}

func (c *hopCache) lookup(ctx context.Context, a netip.Addr) {
	info := model.HopInfo{IP: a}
	rctx, cancel := context.WithTimeout(ctx, rdnsTimeout)
	info.RDNS = c.o.ReverseDNS(rctx, a)
	cancel()
	if ctx.Err() != nil {
		return
	}
	if c.o.Lookup != nil {
		ai := c.o.Lookup(a)
		info.ASN, info.Owner = ai.ASN, ai.Owner
	}
	now := c.o.Now()
	c.mu.Lock()
	e := c.m[a]
	if e == nil {
		e = &hopEntry{first: now, last: now}
		c.m[a] = e
	}
	e.queued = false
	e.reported = now
	info.FirstSeen, info.LastSeen = e.first, e.last
	c.mu.Unlock()
	c.sink.ObserveHop(info)
}
