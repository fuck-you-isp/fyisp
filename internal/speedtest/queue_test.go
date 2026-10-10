package speedtest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeExec runs jobs that wait for release (or ctx), counting how many run
// at once.
type fakeExec struct {
	mu      sync.Mutex
	ran     []string // trigger:region/server
	now     atomic.Int64
	running atomic.Int32
	maxRun  atomic.Int32
	release chan struct{}
	started chan struct{}
}

func newQueueHarness(t *testing.T) (*Queue, *fakeExec) {
	t.Helper()
	f := &fakeExec{release: make(chan struct{}), started: make(chan struct{}, 100)}
	f.now.Store(time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC).UnixNano())
	servers := []Server{{ID: "A"}, {ID: "B"}, {ID: "C"}}
	q := NewQueue(QueueOptions{
		Now: func() time.Time { return time.Unix(0, f.now.Load()) },
		Resolve: func(kind Kind, id string) ([]Job, error) {
			switch {
			case kind == KindSweep:
				var out []Job
				for _, sv := range servers {
					out = append(out, Job{Chain: []Server{sv}})
				}
				return out, nil
			case id == "":
				return []Job{{}}, nil
			case id == "A" || id == "B" || id == "C":
				return []Job{{Chain: []Server{{ID: id}}}}, nil
			case id == "uk":
				return []Job{{Region: "uk", Chain: servers[:2]}}, nil
			}
			return nil, ErrUnknownID
		},
		Exec: func(ctx context.Context, j Job, trigger string) (int64, bool) {
			n := f.running.Add(1)
			defer f.running.Add(-1)
			for {
				m := f.maxRun.Load()
				if n <= m || f.maxRun.CompareAndSwap(m, n) {
					break
				}
			}
			name := j.Region
			if name == "" && len(j.Chain) > 0 {
				name = j.Chain[0].ID
			}
			f.mu.Lock()
			f.ran = append(f.ran, trigger+":"+name)
			id := int64(len(f.ran))
			f.mu.Unlock()
			f.started <- struct{}{}
			select {
			case <-ctx.Done():
				return 0, false
			case <-f.release:
				return id, true
			}
		},
	})
	return q, f
}

func (f *fakeExec) advance(d time.Duration) { f.now.Add(int64(d)) }

func (f *fakeExec) log() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.ran...)
}

func ids(es []Entry) []string {
	var out []string
	for _, e := range es {
		out = append(out, fmt.Sprintf("%s:%s:%s", e.Who, e.Kind, e.Target))
	}
	return out
}

func eq(a, b []string) bool { return fmt.Sprint(a) == fmt.Sprint(b) }

// TestQueueOrder: FIFO, the owner ahead of the share link and the
// schedule, dedupe per requester.
func TestQueueOrder(t *testing.T) {
	q, _ := newQueueHarness(t)
	enq := func(r Request) (Entry, bool) {
		t.Helper()
		e, dup, err := q.Enqueue(r)
		if err != nil {
			t.Fatalf("%+v: %v", r, err)
		}
		return e, dup
	}
	enq(Request{Kind: KindSingle, Target: "A", Who: WhoPublic, Visitor: "v1"})
	q.EnqueueScheduled(Job{Region: "uk"})
	enq(Request{Kind: KindSweep, Who: WhoPublic, Visitor: "v2"})
	enq(Request{Kind: KindSingle, Target: "B", Who: WhoOwner})
	enq(Request{Kind: KindSingle, Who: WhoOwner})
	got := ids(q.State().Waiting)
	want := []string{"owner:single:B", "owner:single:", "public:single:A", "schedule:scheduled:", "public:sweep:"}
	if !eq(got, want) {
		t.Fatalf("order %v, want %v", got, want)
	}
	// Dedupe: the same requester asking again gets the queued entry.
	if e, dup := enq(Request{Kind: KindSingle, Target: "B", Who: WhoOwner}); !dup || e.ID != 4 || e.Position != 1 {
		t.Errorf("owner duplicate: %+v %v", e, dup)
	}
	if e, dup := enq(Request{Kind: KindSweep, Who: WhoPublic, Visitor: "v2"}); !dup || e.ID != 3 {
		t.Errorf("visitor duplicate: %+v %v", e, dup)
	}
	if dup, err := q.Admit(Request{Kind: KindSingle, Target: "A", Who: WhoPublic, Visitor: "v1"}); !dup || err != nil {
		t.Errorf("Admit a duplicate: %v %v", dup, err)
	}
	if dup, err := q.Admit(Request{Kind: KindSingle, Target: "B", Who: WhoPublic, Visitor: "v1"}); dup || !errors.Is(err, ErrQueueFull) {
		t.Errorf("Admit a second entry: %v %v", dup, err)
	}
	if dup, err := q.Admit(Request{Kind: KindSingle, Target: "A", Who: WhoPublic, Visitor: "v9"}); dup || err != nil {
		t.Errorf("Admit a new visitor: %v %v", dup, err)
	}
	// Another visitor's equal request is an entry of its own.
	if e, dup := enq(Request{Kind: KindSweep, Who: WhoPublic, Visitor: "v3"}); dup || e.Position != 6 {
		t.Errorf("other visitor: %+v %v", e, dup)
	}
	// Estimates: back to back; a sweep's per test.
	ws := q.State().Waiting
	for i := 1; i < len(ws); i++ {
		if !ws[i].EstStart.Equal(ws[i-1].EstFinish) || ws[i].Position != i+1 {
			t.Errorf("estimates %d: %+v after %+v", i, ws[i], ws[i-1])
		}
	}
	if last := ws[len(ws)-1]; last.Kind != KindSweep || last.EstFinish.Sub(last.EstStart) != 3*EstJob || last.Steps != 3 {
		t.Errorf("sweep estimate %+v", last)
	}
	if _, _, err := q.Enqueue(Request{Kind: KindSingle, Target: "nope", Who: WhoOwner}); !errors.Is(err, ErrUnknownID) {
		t.Errorf("unknown: %v", err)
	}
}

// TestQueuePublicCaps: one waiting entry per visitor, MaxPublicSweeps
// sweeps, MaxPublicWaiting in all; the owner is never refused.
func TestQueuePublicCaps(t *testing.T) {
	q, _ := newQueueHarness(t)
	pub := func(kind Kind, target, v string) error {
		_, _, err := q.Enqueue(Request{Kind: kind, Target: target, Who: WhoPublic, Visitor: v})
		return err
	}
	if err := pub(KindSingle, "A", "v1"); err != nil {
		t.Fatal(err)
	}
	var full *QueueFullError
	if err := pub(KindSingle, "B", "v1"); !errors.As(err, &full) || !errors.Is(err, ErrQueueFull) {
		t.Errorf("second entry of a visitor: %v", err)
	}
	if err := pub(KindSweep, "", "v1"); !errors.As(err, &full) {
		t.Errorf("a visitor's sweep on top of a single: %v", err)
	}
	for i := range MaxPublicSweeps {
		if err := pub(KindSweep, "", fmt.Sprint("s", i)); err != nil {
			t.Fatalf("sweep %d: %v", i, err)
		}
	}
	if err := pub(KindSweep, "", "s9"); !errors.As(err, &full) || full.Reason != "enough full sweeps are already waiting" {
		t.Errorf("fourth sweep: %v", err)
	}
	if st := q.State(); !st.SweepsFull || st.PublicSweeps != MaxPublicSweeps {
		t.Errorf("state %+v", st)
	}
	if err := pub(KindSingle, "C", "s9"); err != nil {
		t.Errorf("a single still fits: %v", err)
	}
	for i := 0; ; i++ {
		err := pub(KindSingle, []string{"A", "B", "C"}[i%3], fmt.Sprint("v", 100+i))
		if err != nil {
			if !errors.As(err, &full) || full.Reason != "the queue is full" {
				t.Fatalf("filling: %v", err)
			}
			break
		}
		if i > 50 {
			t.Fatal("no cap")
		}
	}
	if st := q.State(); st.PublicWaiting != MaxPublicWaiting || !st.Full {
		t.Errorf("public waiting %d, full %v", st.PublicWaiting, st.Full)
	}
	if _, _, err := q.Enqueue(Request{Kind: KindSweep, Who: WhoOwner}); err != nil {
		t.Errorf("the owner is never refused: %v", err)
	}
	// Clearing the share link's entries leaves the owner's.
	if n := q.ClearPublic(); n != MaxPublicWaiting {
		t.Errorf("cleared %d", n)
	}
	if st := q.State(); len(st.Waiting) != 1 || st.Waiting[0].Who != WhoOwner {
		t.Errorf("after clear: %+v", st.Waiting)
	}
}

// TestQueueRun: one at a time (also under concurrent enqueues), in order,
// stale schedule slots dropped, cancel of waiting and running entries,
// and the last sweep kept.
func TestQueueRun(t *testing.T) {
	q, f := newQueueHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { q.Run(ctx); close(done) }()

	a, _, _ := q.Enqueue(Request{Kind: KindSingle, Target: "A", Who: WhoOwner})
	<-f.started
	if st := q.State(); st.Running == nil || st.Running.ID != a.ID || st.Running.State != StateRunning {
		t.Fatalf("running %+v", st.Running)
	}
	// The owner never pre-empts the running entry (the share link's,
	// here, is next anyway): a new one waits.
	b, _, _ := q.Enqueue(Request{Kind: KindSingle, Target: "B", Who: WhoOwner})
	if st := q.State(); st.Running.ID != a.ID || len(st.Waiting) != 1 || st.Waiting[0].ID != b.ID {
		t.Fatalf("pre-empted: %+v", st)
	}
	// A schedule slot that waits 10 minutes is dropped; the schedule
	// never stacks.
	if !q.EnqueueScheduled(Job{Region: "uk"}) || q.EnqueueScheduled(Job{Region: "uk"}) {
		t.Error("scheduled slots stacked")
	}
	f.advance(ScheduledStale)
	// Cancel the waiting B, then let A finish.
	if e, err := q.Cancel(b.ID); err != nil || e.State != StateCancelled {
		t.Fatalf("cancel waiting: %+v %v", e, err)
	}
	f.release <- struct{}{}
	// Next: nothing runnable but the stale slot, which is dropped.
	waitFor(t, func() bool { st := q.State(); return st.Running == nil && len(st.Waiting) == 0 })
	if got := f.log(); !eq(got, []string{"manual:A"}) {
		t.Errorf("ran %v", got)
	}
	if e, ok := q.Get(a.ID); !ok || e.State != StateDone || len(e.Tests) != 1 || e.Finished.IsZero() {
		t.Errorf("a: %+v", e)
	}
	// Cancel a running entry: its context ends, no result.
	c, _, _ := q.Enqueue(Request{Kind: KindSingle, Target: "C", Who: WhoPublic, Visitor: "v"})
	<-f.started
	if e, err := q.Cancel(c.ID); err != nil || e.State != StateCancelled {
		t.Fatalf("cancel running: %+v %v", e, err)
	}
	waitFor(t, func() bool { e, _ := q.Get(c.ID); return e.State == StateCancelled && !e.Finished.IsZero() })
	if e, _ := q.Get(c.ID); len(e.Tests) != 0 {
		t.Errorf("cancelled test kept a result: %+v", e)
	}
	// A sweep: one test per server, "server N of M", then the last sweep.
	s, _, _ := q.Enqueue(Request{Kind: KindSweep, Who: WhoOwner})
	for i := range 3 {
		<-f.started
		if st := q.State(); st.Running == nil || st.Running.Step != i+1 || st.Running.Steps != 3 {
			t.Errorf("step %d: %+v", i, st.Running)
		}
		f.release <- struct{}{}
	}
	waitFor(t, func() bool { st := q.State(); return st.LastSweep != nil && st.LastSweep.ID == s.ID })
	if st := q.State(); len(st.LastSweep.Tests) != 3 || st.LastSweep.State != StateDone {
		t.Errorf("last sweep %+v", st.LastSweep)
	}
	// Under -race: many concurrent requests, still one test at a time.
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := Request{Kind: KindSingle, Target: []string{"A", "B", "C", ""}[i%4], Who: WhoPublic, Visitor: fmt.Sprint("c", i)}
			if i%5 == 0 {
				r.Who, r.Visitor = WhoOwner, ""
			}
			_, _, _ = q.Enqueue(r)
		}()
	}
	wg.Wait()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-f.started:
				select {
				case f.release <- struct{}{}:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	waitFor(t, func() bool { st := q.State(); return st.Running == nil && len(st.Waiting) == 0 })
	if m := f.maxRun.Load(); m != 1 {
		t.Errorf("%d tests ran at once", m)
	}
	cancel()
	<-done
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(2 * time.Millisecond)
	}
}
