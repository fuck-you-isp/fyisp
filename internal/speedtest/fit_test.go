package speedtest

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

// The share link's tests are limited by time, not data (v0.7.2): one fit
// check (fitsLocked) at admit, at dequeue and before each sweep test, on
// the time caps and the sweep gap.

var pub = func(v string) Request { return Request{Kind: KindSingle, Target: "A", Who: WhoPublic, Visitor: v} }

func setLedger(q *Queue, uses ...usage) {
	q.mu.Lock()
	q.ledger = append([]usage(nil), uses...)
	q.mu.Unlock()
}

// TestFitResetsAtLatest: when the time caps and the sweep gap disagree,
// the refusal is the one that clears last, and a retry then is admitted.
func TestFitResetsAtLatest(t *testing.T) {
	for _, c := range []struct {
		name  string
		uses  func(now time.Time) []usage
		sweep bool // with a recent public sweep, for a sweep request
		code  string
		wait  time.Duration
	}{
		{"the day cap after the hour cap", func(now time.Time) []usage {
			return []usage{
				{At: now.Add(-2 * time.Hour), Dur: PublicTimePerDay - PublicTimePerHour}, // the day cap frees in 22 h
				{At: now.Add(-30 * time.Minute), Dur: PublicTimePerHour},                 // the hour cap frees in 30 min
			}
		}, false, "time_cap", 22 * time.Hour},
		{"the gap after both", func(now time.Time) []usage {
			return []usage{{At: now.Add(-50 * time.Minute), Dur: PublicTimePerHour}} // frees in 10 min
		}, true, "cooldown", 25 * time.Minute},
	} {
		t.Run(c.name, func(t *testing.T) {
			q, f := newQueueHarness(t)
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

// TestPublicDeafServer: a share-link test through the scheduler against a
// server that ignores TEST_END and keeps sending has no byte cap: the time
// backstop cuts each direction, the window still counts, and its time is
// charged.
func TestPublicDeafServer(t *testing.T) {
	f := newFakeServer(t, "deaf")
	host, port, _ := net.SplitHostPort(f.addr())
	p, _ := strconv.Atoi(port)
	sv := Server{ID: "Local", Host: host, PortMin: p, PortMax: p, Custom: true, LAN: true}
	st := &MemStore{}
	var caps []int64
	s := New(Options{Catalog: &Catalog{Servers: []Server{sv}, byID: map[string]int{"Local": 0}}, Store: st, Log: slog.New(slog.DiscardHandler),
		Fake: &Fake{
			Run: func(ctx context.Context, c Config) (Result, error) {
				caps = append(caps, c.MaxBytes)
				c.Duration, c.MinDuration, c.Omit, c.Backstop = time.Second, 0, 300*time.Millisecond, 2*time.Second // short, for the test
				return Run(ctx, c)
			},
			Sleep: func(ctx context.Context, _ time.Duration) bool { return ctx.Err() == nil },
		}})
	on := true
	q := NewQueue(QueueOptions{Public: &on, Exec: s.execJob, Live: s.live, Log: slog.New(slog.DiscardHandler),
		Resolve: func(Kind, string) ([]Job, error) { return []Job{{Chain: []Server{sv}}}, nil }})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Run(ctx)
	e, _, err := q.Enqueue(Request{Kind: KindSingle, Target: "Local", Who: WhoPublic, Visitor: "v"})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for x, _ := q.Get(e.ID); x.State != StateDone; x, _ = q.Get(e.ID) {
		if time.Now().After(deadline) {
			t.Fatalf("not done: %+v", x)
		}
		time.Sleep(20 * time.Millisecond)
	}
	ts, _ := st.SpeedTests(context.Background(), time.Time{}, time.Now().Add(time.Hour))
	if len(ts) != 1 || ts[0].Outcome != model.SpeedOK || ts[0].DownBps == nil || ts[0].DownShort || len(caps) == 0 || caps[0] != 0 {
		t.Errorf("rows %+v, caps %v", ts, caps)
	}
	if qs := q.State(); qs.TimeHour <= 0 {
		t.Errorf("no test time charged: %+v", qs)
	}
}

// TestQueueOwnerCancelCause: the owner's cancel reaches the running test
// with ErrCancelledByOwner, so the stored cancelled row says cancelled_by
// "owner"; a shutdown leaves it unset.
func TestQueueOwnerCancelCause(t *testing.T) {
	for _, c := range []struct {
		name  string
		owner bool
		want  string
	}{{"owner", true, "owner"}, {"shutdown", false, ""}} {
		t.Run(c.name, func(t *testing.T) {
			sv := Server{ID: "Local", Host: "127.0.0.1", PortMin: 5201, PortMax: 5201, Custom: true, LAN: true}
			st := &MemStore{}
			started := make(chan struct{}, 4)
			s := New(Options{Catalog: &Catalog{byID: map[string]int{}}, Custom: []Server{sv}, Store: st, Log: slog.New(slog.DiscardHandler),
				Fake: &Fake{Run: func(ctx context.Context, cf Config) (Result, error) {
					started <- struct{}{}
					<-ctx.Done() // a download that only a cancel ends
					return Result{Direction: cf.Direction, WireBytes: 5e6}, ctx.Err()
				}}})
			q := s.Queue()
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { q.Run(ctx); close(done) }()
			e, _, err := q.Enqueue(Request{Kind: KindSingle, Target: sv.ID, Who: WhoOwner})
			if err != nil {
				t.Fatal(err)
			}
			<-started
			if c.owner {
				q.Cancel(e.ID)
				waitFor(t, func() bool { x, _ := q.Get(e.ID); return !x.Finished.IsZero() })
			}
			cancel()
			<-done
			ts, _ := st.SpeedTests(context.Background(), time.Time{}, time.Now().Add(time.Hour))
			if len(ts) != 1 || ts[0].Outcome != model.SpeedCancelled || ParseExtra(ts[0].Extra).CancelledBy != c.want {
				t.Errorf("rows %+v, want one cancelled by %q", ts, c.want)
			}
		})
	}
}

// TestQueueChargesTheWindow: a share-link test cut short is masked past its
// end (the abort tail); its time is charged to the window's end, and the
// next test is charged from there on, so no second is counted twice.
func TestQueueChargesTheWindow(t *testing.T) {
	var winEnd atomic.Int64
	q, f := newQueueHarness(t, func(o *QueueOptions) {
		o.WindowEnd = func() time.Time { return time.Unix(0, winEnd.Load()) }
	})
	f.step.Store(int64(8 * time.Second))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Run(ctx)
	run := func(v string, tail time.Duration) {
		t.Helper()
		e, _, err := q.Enqueue(Request{Kind: KindSingle, Target: "A", Who: WhoPublic, Visitor: v})
		if err != nil {
			t.Fatal(err)
		}
		<-f.started
		winEnd.Store(time.Unix(0, f.now.Load()).Add(8*time.Second + tail).UnixNano()) // the window ends tail past the test
		f.release <- struct{}{}
		waitFor(t, func() bool { x, _ := q.Get(e.ID); return !x.Finished.IsZero() })
	}
	run("v1", 3*time.Second) // cut short: 8 s run, 3 s tail
	if st := q.State(); st.TimeHour != 11*time.Second {
		t.Fatalf("charged %v, want the 11 s window", st.TimeHour)
	}
	run("v2", 0) // starts at once, inside v1's tail: charged from the tail's end
	if st := q.State(); st.TimeHour != 16*time.Second {
		t.Errorf("charged %v, want 11 + 5 s (the tail counted once)", st.TimeHour)
	}
}
