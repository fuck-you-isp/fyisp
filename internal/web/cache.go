package web

import (
	"context"
	"sync"
	"time"
)

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
