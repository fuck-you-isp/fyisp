package web

import (
	"context"
	"fmt"
	"testing"
	"time"
)

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
