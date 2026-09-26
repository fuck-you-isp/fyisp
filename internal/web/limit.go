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

type cacheEntry struct {
	done    chan struct{}
	val     *cached
	err     error
	expires time.Time
}

// respCache keeps rendered panel responses for ttl and collapses concurrent
// identical queries into one store call.
type respCache struct {
	mu      sync.Mutex
	m       map[string]*cacheEntry
	ttl     time.Duration
	maxSize int
	now     func() time.Time
}

func newRespCache(ttl time.Duration, maxSize int) *respCache {
	return &respCache{m: map[string]*cacheEntry{}, ttl: ttl, maxSize: maxSize, now: time.Now}
}

func (c *respCache) get(ctx context.Context, key string, fill func(context.Context) (*cached, error)) (*cached, error) {
	c.mu.Lock()
	now := c.now()
	if e, ok := c.m[key]; ok {
		select {
		case <-e.done:
			if now.Before(e.expires) {
				c.mu.Unlock()
				return e.val, nil
			}
		default:
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
	}
	if len(c.m) >= c.maxSize {
		for k, e := range c.m {
			select {
			case <-e.done:
				if !now.Before(e.expires) || len(c.m) >= c.maxSize {
					delete(c.m, k)
				}
			default:
			}
		}
	}
	e := &cacheEntry{done: make(chan struct{})}
	c.m[key] = e
	c.mu.Unlock()

	e.val, e.err = fill(ctx)
	c.mu.Lock()
	e.expires = c.now().Add(c.ttl)
	if e.err != nil {
		delete(c.m, key) // never cache failures
	}
	close(e.done)
	c.mu.Unlock()
	return e.val, e.err
}
