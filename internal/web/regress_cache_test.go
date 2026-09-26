package web

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/store"
)

// bigProfile is one group with many series, so panel responses are large.
func bigProfile(targets int) *model.Profile {
	p := &model.Profile{Name: "big", Groups: []model.Group{{ID: "big", Title: "Big"}}}
	for i := range targets {
		p.Targets = append(p.Targets, model.Target{Name: fmt.Sprintf("T%02d", i), Host: "t.example", Group: "big"})
	}
	return p
}

func cacheBytes(c *respCache) (entries, bytes, expired int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	for _, e := range c.m {
		select {
		case <-e.done:
		default:
			continue
		}
		entries++
		if e.val != nil {
			bytes += len(e.val.body)
		}
		if !now.Before(e.expires) {
			expired++
		}
	}
	return
}

// Finding 1: the public cache must be bounded by bytes, not only by entry
// count, and must drop expired entries without waiting to be full.
func TestPublicCacheBoundedBytes(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	p := bigProfile(20) // 60 series
	d := Deps{Profile: func() *model.Profile { return p }, Store: store.NewFake(), Status: func() Status { return Status{} }}
	h := Public(d, testSecret).(*publicHandler)
	h.lim = newLimiter(1e9, 1e9, 1e9, 1e9) // measure the cache, not the limiter
	clock := now
	h.now = func() time.Time { return clock }
	h.cache.now = func() time.Time { return clock }
	const limit = 32 << 20
	var one int
	for i := range 100 {
		// 9h at 1000..1099 points: ~1.2 MB of JSON each, all distinct.
		u := fmt.Sprintf("/s/%s/api/panel?group=big&from=now-9h&points=%d", testSecret, 1000+i)
		w := do(h, "GET", u, nil)
		if w.Code != 200 {
			t.Fatalf("code %d %s", w.Code, w.Body.String())
		}
		one = w.Body.Len()
	}
	n, b, _ := cacheBytes(h.cache)
	t.Logf("one response %d bytes; cache holds %d entries, %d bytes", one, n, b)
	if 100*one <= limit {
		t.Fatalf("test does not exercise the limit: 100 x %d bytes <= %d", one, limit)
	}
	if b > limit {
		t.Errorf("cache holds %d bytes, want <= %d", b, limit)
	}
	// Past the TTL, one small insert must evict everything expired.
	clock = clock.Add(PublicCacheTTL + time.Second)
	if w := do(h, "GET", "/s/"+testSecret+"/api/panel?group=big&from=now-5m&points=10", nil); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if n, b, exp := cacheBytes(h.cache); exp > 0 {
		t.Errorf("after TTL: %d expired entries (%d entries, %d bytes) still held", exp, n, b)
	}
}

// Finding 1: absolute from/to that differ by milliseconds share one cache
// entry (and one store query).
func TestPublicCacheQuantizesAbsoluteRanges(t *testing.T) {
	now := time.Now()
	cs, d, _ := fixture(t, now)
	h := Public(d, testSecret).(*publicHandler)
	h.lim = newLimiter(1e9, 1e9, 1e9, 1e9)
	h.now = func() time.Time { return now }
	to := now.Add(-5 * time.Minute).Truncate(time.Minute).UnixMilli()
	for i := range 64 {
		u := fmt.Sprintf("/s/%s/api/panel?group=common&from=%d&to=%d&points=500", testSecret, to-int64(i)-30*60*1000, to-int64(i))
		if w := do(h, "GET", u, nil); w.Code != 200 {
			t.Fatalf("code %d %s", w.Code, w.Body.String())
		}
	}
	if n := cs.calls.Load(); n > 2 {
		t.Errorf("64 queries differing by milliseconds made %d store calls, want <= 2", n)
	}
}

// Finding 1: the public CSV export is capped in points.
func TestPublicCSVPointsCapped(t *testing.T) {
	now := time.Now()
	cs, d, _ := fixture(t, now)
	h := Public(d, testSecret)
	u := "/s/" + testSecret + "/api/panel.csv?group=common&from=now-90d&points=2000"
	if w := do(h, "GET", u, nil); w.Code != 200 {
		t.Fatalf("code %d %s", w.Code, w.Body.String())
	}
	cs.mu.Lock()
	got := cs.last.MaxPoints
	cs.mu.Unlock()
	if got > 500 {
		t.Errorf("public CSV asked the store for %d points, want <= 500", got)
	}
	// The local CSV is not capped.
	hl := Local(d, LocalOptions{Addr: "127.0.0.1:3000"})
	if w := do(hl, "GET", "/api/panel.csv?group=common&from=now-90d&points=2000", map[string]string{"Host": "127.0.0.1:3000"}); w.Code != 200 {
		t.Fatal(w.Code)
	}
	cs.mu.Lock()
	got = cs.last.MaxPoints
	cs.mu.Unlock()
	if got <= 500 {
		t.Errorf("local CSV asked the store for %d points, want the requested resolution", got)
	}
}

// Finding 1: respCache accounting. Oversized responses are served but not
// kept, the byte total never exceeds the limit, and it matches the entries.
func TestRespCacheLimits(t *testing.T) {
	clock := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	c := newRespCache(10*time.Second, 8, 1000, 300)
	c.now = func() time.Time { return clock }
	body := func(n int) func(context.Context) (*cached, error) {
		return func(context.Context) (*cached, error) { return &cached{body: make([]byte, n)}, nil }
	}
	check := func(when string) {
		t.Helper()
		c.mu.Lock()
		defer c.mu.Unlock()
		sum := 0
		for _, e := range c.m {
			sum += e.size
		}
		if sum != c.bytes || c.bytes > c.maxBytes || len(c.m) > c.maxEntries {
			t.Errorf("%s: bytes=%d (entries sum %d, max %d), entries=%d", when, c.bytes, sum, c.maxBytes, len(c.m))
		}
	}
	if v, err := c.get(context.Background(), "big", body(301)); err != nil || len(v.body) != 301 {
		t.Fatalf("oversized response not served: %v", err)
	}
	if _, ok := c.m["big"]; ok {
		t.Error("oversized response retained")
	}
	for i := range 50 {
		if _, err := c.get(context.Background(), fmt.Sprint("k", i), body(100+i*4)); err != nil {
			t.Fatal(err)
		}
		check(fmt.Sprint("insert ", i))
		clock = clock.Add(time.Second)
	}
	// Everything expires; the next insert drops it all.
	clock = clock.Add(time.Minute)
	if _, err := c.get(context.Background(), "last", body(10)); err != nil {
		t.Fatal(err)
	}
	check("after expiry")
	if len(c.m) != 1 || c.bytes != 10 {
		t.Errorf("after expiry: %d entries, %d bytes; want 1, 10", len(c.m), c.bytes)
	}
	// A hit on a fresh entry does not refill.
	calls := 0
	for range 3 {
		_, _ = c.get(context.Background(), "hit", func(context.Context) (*cached, error) { calls++; return &cached{body: []byte("x")}, nil })
	}
	if calls != 1 {
		t.Errorf("fresh entry refilled %d times", calls)
	}
}
