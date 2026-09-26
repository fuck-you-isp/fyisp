package web

import (
	"context"
	"sync"
	"time"
)

// bucket is a token bucket: rate tokens per second, up to burst.
// golang.org/x/time/rate is not a dependency, and this is all we need.
type bucket struct {
	tokens float64
	last   time.Time
}

func (b *bucket) allow(now time.Time, rate, burst float64) bool {
	if b.last.IsZero() {
		b.tokens = burst
	} else if d := now.Sub(b.last).Seconds(); d > 0 {
		b.tokens = min(burst, b.tokens+d*rate)
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// limiter applies a global bucket and one bucket per client key.
type limiter struct {
	mu                sync.Mutex
	global            bucket
	per               map[string]*bucket
	gRate, gBurst     float64
	perRate, perBurst float64
	maxKeys           int
	lastSweep         time.Time
}

func newLimiter(gRate, gBurst, perRate, perBurst float64) *limiter {
	return &limiter{per: map[string]*bucket{}, gRate: gRate, gBurst: gBurst, perRate: perRate, perBurst: perBurst, maxKeys: 10000}
}

// allow checks the client's bucket first so one noisy client does not drain
// the global budget with requests that are refused anyway.
func (l *limiter) allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.lastSweep) > time.Minute || len(l.per) >= l.maxKeys {
		l.sweep(now)
	}
	b := l.per[key]
	if b == nil {
		if len(l.per) >= l.maxKeys {
			// Under a flood of distinct keys, fall back to one shared bucket.
			key = "\x00overflow"
			if b = l.per[key]; b == nil {
				b = &bucket{}
				l.per[key] = b
			}
		} else {
			b = &bucket{}
			l.per[key] = b
		}
	}
	if !b.allow(now, l.perRate, l.perBurst) {
		return false
	}
	return l.global.allow(now, l.gRate, l.gBurst)
}

// sweep forgets buckets that have refilled completely.
func (l *limiter) sweep(now time.Time) {
	l.lastSweep = now
	full := time.Duration(l.perBurst/l.perRate*float64(time.Second)) + time.Second
	for k, b := range l.per {
		if now.Sub(b.last) > full {
			delete(l.per, k)
		}
	}
}

// cached is a rendered response.
type cached struct {
	code        int
	ctype       string
	disposition string
	body        []byte
}

func (c *cached) size() int {
	if c == nil {
		return 0
	}
	return len(c.body) + len(c.ctype) + len(c.disposition)
}

type cacheEntry struct {
	done    chan struct{}
	val     *cached
	err     error
	expires time.Time
	size    int // bytes accounted in respCache.bytes; 0 until retained
}

// respCache keeps rendered panel responses for ttl and collapses concurrent
// identical queries into one store call. It is bounded by entry count and by
// the total size of the bodies it retains: a response larger than maxEntry
// is served to everyone waiting for it but not kept, and expired entries are
// dropped on every insert.
type respCache struct {
	mu         sync.Mutex
	m          map[string]*cacheEntry
	ttl        time.Duration
	maxEntries int
	maxBytes   int
	maxEntry   int
	bytes      int // sum of size over retained entries
	// fillTimeout bounds a fill, which runs detached from the request that
	// started it.
	fillTimeout time.Duration
	now         func() time.Time
}

func newRespCache(ttl time.Duration, maxEntries, maxBytes, maxEntry int) *respCache {
	return &respCache{m: map[string]*cacheEntry{}, ttl: ttl, maxEntries: maxEntries,
		maxBytes: maxBytes, maxEntry: min(maxEntry, maxBytes), fillTimeout: panelTimeout, now: time.Now}
}

func isDone(e *cacheEntry) bool {
	select {
	case <-e.done:
		return true
	default:
		return false
	}
}

// remove drops key if it still maps to e. Called with mu held.
func (c *respCache) remove(key string, e *cacheEntry) {
	if c.m[key] == e {
		delete(c.m, key)
		c.bytes -= e.size
		e.size = 0
	}
}

// evict drops expired entries, then completed entries soonest to expire
// first while need more bytes do not fit (or, with slot, while there is no
// room for one more entry). In-flight entries are never evicted. Called with
// mu held.
func (c *respCache) evict(now time.Time, need int, slot bool) {
	for k, e := range c.m {
		if isDone(e) && !now.Before(e.expires) {
			c.remove(k, e)
		}
	}
	for (slot && len(c.m) >= c.maxEntries) || c.bytes+need > c.maxBytes {
		var vk string
		var ve *cacheEntry
		for k, e := range c.m {
			if isDone(e) && (ve == nil || e.expires.Before(ve.expires)) {
				vk, ve = k, e
			}
		}
		if ve == nil {
			return
		}
		c.remove(vk, ve)
	}
}

func (c *respCache) get(ctx context.Context, key string, fill func(context.Context) (*cached, error)) (*cached, error) {
	c.mu.Lock()
	now := c.now()
	if e, ok := c.m[key]; ok {
		if !isDone(e) {
			c.mu.Unlock()
			select {
			case <-e.done:
				if e.err != nil {
					return nil, e.err
				}
				return e.val, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		if now.Before(e.expires) {
			c.mu.Unlock()
			return e.val, nil
		}
		c.remove(key, e)
	}
	c.evict(now, 0, true)
	e := &cacheEntry{done: make(chan struct{})}
	c.m[key] = e
	c.mu.Unlock()

	// The fill serves everyone waiting on this entry, so it must not die
	// with the request that happened to start it.
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.fillTimeout)
	e.val, e.err = fill(fctx)
	cancel()
	c.mu.Lock()
	now = c.now()
	e.expires = now.Add(c.ttl)
	switch size := e.val.size(); {
	case e.err != nil, size > c.maxEntry:
		c.remove(key, e) // never cache failures or oversized responses
	default:
		c.evict(now, size, false)
		if c.m[key] == e && c.bytes+size <= c.maxBytes {
			e.size = size
			c.bytes += size
		} else {
			c.remove(key, e)
		}
	}
	close(e.done)
	c.mu.Unlock()
	return e.val, e.err
}
