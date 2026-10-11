package speedtest

import (
	"context"
	"log/slog"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

// TestQueuedDeafServer: a queued test through the scheduler against a
// server that ignores TEST_END and keeps sending has no byte cap: the time
// backstop cuts each direction and the window still counts.
func TestQueuedDeafServer(t *testing.T) {
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
	q := NewQueue(QueueOptions{Exec: s.execJob, Live: s.live, Log: slog.New(slog.DiscardHandler),
		Resolve: func(Kind, string) ([]Job, error) { return []Job{{Chain: []Server{sv}}}, nil }})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Run(ctx)
	e, _, err := q.Enqueue(Request{Kind: KindSingle, Target: "Local", Who: WhoOwner})
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

// TestDeafServerRealConfig: with the scheduler's own config (the early
// stop at 3 s or more, then TEST_END), a server that ignores TEST_END and
// never sends its results still gives a result: the client's windows
// stand, the streams are cut, and the test is time-bounded.
func TestDeafServerRealConfig(t *testing.T) {
	if testing.Short() {
		t.Skip("about 20 s")
	}
	f := newFakeServer(t, "deaf")
	host, port, _ := net.SplitHostPort(f.addr())
	p, _ := strconv.Atoi(port)
	sv := Server{ID: "Local", Host: host, PortMin: p, PortMax: p, Custom: true, LAN: true}
	st := &MemStore{}
	var cfgs []Config
	s := New(Options{Catalog: &Catalog{byID: map[string]int{}}, Custom: []Server{sv}, Store: st, Log: slog.New(slog.DiscardHandler),
		Fake: &Fake{Run: func(ctx context.Context, c Config) (Result, error) {
			cfgs = append(cfgs, c)
			return Run(ctx, c)
		}}})
	t0 := time.Now()
	id, saved := s.execJob(context.Background(), s.slotJob(target{server: sv.ID, nearby: -1}), "manual")
	el := time.Since(t0)
	ts, _ := st.SpeedTests(context.Background(), time.Time{}, time.Now().Add(time.Hour))
	if !saved || id == 0 || len(ts) != 1 || ts[0].Outcome != model.SpeedOK || ts[0].DownBps == nil || ts[0].UpBps == nil {
		t.Fatalf("after %v: %+v", el, ts)
	}
	if c := cfgs[0]; c.MinDuration != MinMeasure || c.Duration != MaxMeasure || c.Backstop != TimeBackstop {
		t.Errorf("not the scheduler's config: %+v", c)
	}
	if el > 2*DirTimeout+relisten {
		t.Errorf("took %v", el)
	}
}
