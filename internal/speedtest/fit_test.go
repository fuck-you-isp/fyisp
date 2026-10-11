package speedtest

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The share link's budget is a hard limit (v0.7.1 A1): one fit check
// (fitsLocked) at admit, at dequeue and before each sweep test.

var pub = func(v string) Request { return Request{Kind: KindSingle, Target: "A", Who: WhoPublic, Visitor: v} }

// withLedger sets the budget.
func withLedger(limit int64) func(*QueueOptions) {
	return func(o *QueueOptions) { o.PublicBudget = limit }
}

func setLedger(q *Queue, uses ...usage) {
	q.mu.Lock()
	q.ledger = append([]usage(nil), uses...)
	q.mu.Unlock()
}

// TestFitResetsWhenATestFits: 300 MB with 105 MB used leaves 195 MB, less
// than a whole test: 429 budget_exhausted until the 105 MB ages out.
func TestFitResetsWhenATestFits(t *testing.T) {
	q, f := newQueueHarness(t, withLedger(300e6))
	now := time.Unix(0, f.now.Load())
	if n := q.State().FitsTests; n != 1 {
		t.Errorf("an empty 300 MB fits %d tests, want 1", n)
	}
	setLedger(q, usage{At: now.Add(-time.Hour), Bytes: 105e6})
	var be *BudgetError
	_, err := q.Admit(pub("v1"))
	if !errors.As(err, &be) || be.Time || !be.ResetsAt.Equal(now.Add(23*time.Hour)) || RefusalCode(err) != "budget_exhausted" {
		t.Fatalf("admit: %v (%+v)", err, be)
	}
	if _, _, err := q.Enqueue(pub("v1")); !errors.As(err, &be) {
		t.Errorf("enqueue: %v", err)
	}
	if _, _, r := q.Budget(); !r.Equal(be.ResetsAt) {
		t.Errorf("Budget resets %v, refusal %v", r, be.ResetsAt)
	}
	f.advance(23 * time.Hour)
	if _, err := q.Admit(pub("v1")); err != nil {
		t.Errorf("at resets_at: %v", err)
	}
}

// TestFitDropsAtDequeue: three visitors queued with 250 MB left: one runs,
// and the others are dropped when their turn comes (a whole test no longer
// fits), with the code and when it clears.
func TestFitDropsAtDequeue(t *testing.T) {
	q, f := newQueueHarness(t, withLedger(500e6))
	now := time.Unix(0, f.now.Load())
	setLedger(q, usage{At: now.Add(-time.Hour), Bytes: 250e6})
	f.bytes.Store(150e6)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Run(ctx)
	q.Enqueue(Request{Kind: KindSingle, Target: "B", Who: WhoOwner}) // holds the line while the visitors queue
	<-f.started
	var es []Entry
	for _, v := range []string{"v1", "v2", "v3"} {
		e, _, err := q.Enqueue(pub(v))
		if err != nil {
			t.Fatalf("%s: %v", v, err)
		}
		es = append(es, e)
	}
	f.release <- struct{}{}
	<-f.started // v1
	if st := q.State(); st.Running == nil || st.Running.ID != es[0].ID || st.CanStart || st.FitsTests != 0 {
		t.Errorf("running %+v; can start %v (the running test holds 210 MB)", st.Running, st.CanStart)
	}
	f.release <- struct{}{}
	for _, e := range es[1:] {
		waitFor(t, func() bool { x, _ := q.Get(e.ID); return x.State == StateDropped })
		if x, _ := q.Get(e.ID); x.Stopped != StopLimit || x.StopCode != "budget_exhausted" || x.ResetsAt.IsZero() {
			t.Errorf("dropped %+v", x)
		}
	}
	if got := f.log(); len(got) != 2 {
		t.Errorf("ran %v", got)
	}
}

// TestFitStopsSweep: a sweep with 250 MB left runs one test, then stops
// before the next with "limit"; the test it ran stays.
func TestFitStopsSweep(t *testing.T) {
	q, f := newQueueHarness(t, withLedger(500e6))
	setLedger(q, usage{At: time.Unix(0, f.now.Load()).Add(-time.Hour), Bytes: 250e6})
	f.bytes.Store(100e6)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Run(ctx)
	e, _, err := q.Enqueue(Request{Kind: KindSweep, Who: WhoPublic, Visitor: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	<-f.started
	f.release <- struct{}{}
	waitFor(t, func() bool { x, _ := q.Get(e.ID); return x.State == StateDone })
	if x, _ := q.Get(e.ID); x.Ran != 1 || len(x.Tests) != 1 || x.Stopped != StopLimit || x.StopCode != "budget_exhausted" {
		t.Errorf("sweep %+v", x)
	}
}

// TestFitResetsAtLatest: when the bytes, the time caps and the sweep gap
// disagree, the refusal is the one that clears last, and a retry then is
// admitted.
func TestFitResetsAtLatest(t *testing.T) {
	for _, c := range []struct {
		name  string
		uses  func(now time.Time) []usage
		sweep bool // with a recent public sweep, for a sweep request
		code  string
		wait  time.Duration
	}{
		{"time after bytes", func(now time.Time) []usage {
			return []usage{
				{At: now.Add(-23 * time.Hour), Bytes: 300e6},         // fits again in 1 h
				{At: now.Add(-2 * time.Hour), Dur: PublicTimePerDay}, // the day cap frees in 22 h
			}
		}, false, "time_cap", 22 * time.Hour},
		{"bytes after time", func(now time.Time) []usage {
			return []usage{
				{At: now.Add(-time.Hour), Bytes: 300e6},                  // fits again in 23 h
				{At: now.Add(-30 * time.Minute), Dur: PublicTimePerHour}, // the hour cap frees in 30 min
			}
		}, false, "budget_exhausted", 23 * time.Hour},
		{"the gap after both", func(now time.Time) []usage {
			return []usage{{At: now.Add(-50 * time.Minute), Dur: PublicTimePerHour}} // frees in 10 min
		}, true, "cooldown", 25 * time.Minute},
	} {
		t.Run(c.name, func(t *testing.T) {
			q, f := newQueueHarness(t, withLedger(500e6))
			now := time.Unix(0, f.now.Load())
			setLedger(q, c.uses(now)...)
			r := pub("v1")
			if c.sweep {
				r = Request{Kind: KindSweep, Who: WhoPublic, Visitor: "v1"}
				q.mu.Lock()
				q.sweepEnd = now.Add(-5 * time.Minute) // the gap ends in 25 min
				q.mu.Unlock()
			}
			_, err := q.Admit(r)
			var be *BudgetError
			var ge *SweepWaitError
			var at time.Time
			switch {
			case errors.As(err, &be):
				at = be.ResetsAt
			case errors.As(err, &ge):
				at = ge.Until
			}
			if RefusalCode(err) != c.code || !at.Equal(now.Add(c.wait)) {
				t.Fatalf("refused %v (%s) until %v, want %s in %v", err, RefusalCode(err), at.Sub(now), c.code, c.wait)
			}
			f.advance(c.wait)
			if _, err := q.Admit(r); err != nil {
				t.Errorf("a retry at resets_at: %v", err)
			}
		})
	}
}

// TestFitBudgetTooSmall: a budget under one test turns the share link's
// tests off with one warning; never raised, never a failure to start.
func TestFitBudgetTooSmall(t *testing.T) {
	var buf bytes.Buffer
	q, _ := newQueueHarness(t, withLedger(150e6), func(o *QueueOptions) { o.Log = slog.New(slog.NewTextHandler(&buf, nil)) })
	if !strings.Contains(buf.String(), "level=WARN") ||
		!strings.Contains(buf.String(), "share-link speed tests are off: the budget (150 MB) is below the 210 MB one test needs") {
		t.Errorf("log %q", buf.String())
	}
	_, err := q.Admit(pub("v1"))
	if !errors.Is(err, ErrBudgetTooSmall) || !errors.Is(err, ErrPublicDisabled) || RefusalCode(err) != "budget_too_small" {
		t.Errorf("admit: %v", err)
	}
	if _, _, err := q.Enqueue(Request{Kind: KindSweep, Who: WhoPublic, Visitor: "v1"}); !errors.Is(err, ErrBudgetTooSmall) {
		t.Errorf("enqueue: %v", err)
	}
	if st := q.State(); !st.BudgetTooSmall || st.CanStart || !st.Public || st.BudgetLimit != 150e6 {
		t.Errorf("state %+v", st)
	}
	if _, _, err := q.Enqueue(Request{Kind: KindSingle, Target: "A", Who: WhoOwner}); err != nil {
		t.Errorf("the owner: %v", err)
	}
	// At the minimum: on.
	buf.Reset()
	q2, _ := newQueueHarness(t, withLedger(PublicTestBytes), func(o *QueueOptions) { o.Log = slog.New(slog.NewTextHandler(&buf, nil)) })
	if _, err := q2.Admit(pub("v1")); err != nil || buf.Len() != 0 {
		t.Errorf("210 MB: %v, log %q", err, buf.String())
	}
}

// TestFitPropertyLedgerWithinLimit: across 1,000 random sequences of
// share-link tests and sweeps (each test moving up to PublicTestBytes, as a
// server that ignores TEST_END does until the backstop cuts it), the bytes
// charged in any 24 hours never exceed the budget.
func TestFitPropertyLedgerWithinLimit(t *testing.T) {
	if testing.Short() {
		t.Skip("1,000 sequences")
	}
	for seq := range 1000 {
		r := rand.New(rand.NewPCG(uint64(seq), 71))
		limit := PublicTestBytes + r.Int64N(1.5e9)
		var clock atomicTime
		clock.set(time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC))
		type charge struct {
			at    time.Time
			bytes int64
		}
		var mu sync.Mutex
		var charges []charge
		on := true
		q := NewQueue(QueueOptions{Public: &on, PublicBudget: limit, Now: clock.get, Log: slog.New(slog.DiscardHandler),
			Resolve: func(k Kind, _ string) ([]Job, error) {
				if k == KindSweep {
					return make([]Job, 1+r.IntN(4)), nil
				}
				return []Job{{}}, nil
			},
			Exec: func(ctx context.Context, _ Job, _ string) (int64, bool, int64) {
				mu.Lock()
				b := r.Int64N(PublicTestBytes + 1) // the backstop's bound: never more
				if r.IntN(8) == 0 {
					b = PublicTestBytes // a server that ignores TEST_END, cut at the backstop
				}
				clock.add(time.Duration(5+r.IntN(40)) * time.Second)
				charges = append(charges, charge{clock.get(), b})
				mu.Unlock()
				return 0, r.IntN(5) != 0, b
			}})
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { q.Run(ctx); close(done) }()
		for i := range 4 + r.IntN(12) {
			mu.Lock()
			req := pub("v" + string(rune('a'+i)))
			if r.IntN(4) == 0 {
				req = Request{Kind: KindSweep, Who: WhoPublic, Visitor: req.Visitor}
			}
			clock.add(time.Duration(r.Int64N(int64(6 * time.Hour))))
			mu.Unlock()
			q.Enqueue(req) // refusals are part of the sequence
			for {
				st := q.State()
				if st.Running == nil && len(st.Waiting) == 0 {
					break
				}
				if st.Running == nil { // a sweep held for the gap
					clock.add(PublicSweepGap)
					q.poke()
				}
				time.Sleep(20 * time.Microsecond)
			}
		}
		cancel()
		<-done
		for i, c := range charges {
			var sum int64
			for _, d := range charges[:i+1] {
				if c.at.Sub(d.at) < budgetSpan {
					sum += d.bytes
				}
			}
			if sum > limit {
				t.Fatalf("sequence %d: %d bytes charged in the 24 h to %v, over the %d budget", seq, sum, c.at, limit)
			}
		}
	}
}

// atomicTime is a fake clock safe for the queue's goroutines.
type atomicTime struct {
	mu sync.Mutex
	t  time.Time
}

func (a *atomicTime) get() time.Time  { a.mu.Lock(); defer a.mu.Unlock(); return a.t }
func (a *atomicTime) set(t time.Time) { a.mu.Lock(); a.t = t; a.mu.Unlock() }
func (a *atomicTime) add(d time.Duration) {
	a.mu.Lock()
	a.t = a.t.Add(d)
	a.mu.Unlock()
}

// TestFitDeafServerWithinBudget: real tests through the scheduler against
// a server that ignores TEST_END and keeps sending (the backstop cuts it):
// each test charges at most TestMaxBytes, and the share link's charges
// never pass the budget; a visitor whose test no longer fits is dropped.
func TestFitDeafServerWithinBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("moves about 1 GB over loopback")
	}
	f := newFakeServer(t, "deaf")
	f.rate = 1 << 20
	host, port, _ := net.SplitHostPort(f.addr())
	p, _ := strconv.Atoi(port)
	sv := Server{ID: "Local", Host: host, PortMin: p, PortMax: p, Custom: true, LAN: true}
	s := New(Options{Catalog: &Catalog{Servers: []Server{sv}, byID: map[string]int{"Local": 0}}})
	on := true
	var limit int64 = 2*TestMaxBytes + 100e6 // room for two whole tests, not three
	q := NewQueue(QueueOptions{Public: &on, PublicBudget: limit, Exec: s.execJob, Live: s.live, Log: slog.New(slog.DiscardHandler),
		Resolve: func(Kind, string) ([]Job, error) { return []Job{{Chain: []Server{sv}}}, nil }})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Run(ctx)
	var es []Entry
	for _, v := range []string{"v1", "v2", "v3", "v4"} {
		e, _, err := q.Enqueue(Request{Kind: KindSingle, Target: "Local", Who: WhoPublic, Visitor: v})
		if err != nil {
			t.Fatalf("%s: %v", v, err)
		}
		es = append(es, e)
	}
	deadline := time.Now().Add(3 * time.Minute)
	for st := q.State(); st.Running != nil || len(st.Waiting) > 0; st = q.State() {
		if time.Now().After(deadline) {
			t.Fatalf("not idle: %+v", st)
		}
		time.Sleep(50 * time.Millisecond)
	}
	q.mu.Lock()
	ledger := slices.Clone(q.ledger)
	q.mu.Unlock()
	var sum, most int64
	for _, u := range ledger {
		if u.Bytes > TestMaxBytes {
			t.Errorf("one test charged %d bytes, over %d", u.Bytes, TestMaxBytes)
		}
		sum, most = sum+u.Bytes, max(most, u.Bytes)
	}
	t.Logf("charged %d of %d; the largest test %d", sum, limit, most)
	if sum > limit || most <= MaxBytes {
		t.Errorf("charged %d of %d; the largest test %d (the deaf server should push past %d)", sum, limit, most, int64(MaxBytes))
	}
	ran, dropped := 0, 0
	for _, e := range es {
		switch x, _ := q.Get(e.ID); x.State {
		case StateDone:
			ran++
		case StateDropped:
			dropped++
			if x.StopCode != "budget_exhausted" {
				t.Errorf("dropped %+v", x)
			}
		}
	}
	if ran != 2 || dropped != 2 {
		t.Errorf("ran %d, dropped %d; want 2 and 2", ran, dropped)
	}
}
