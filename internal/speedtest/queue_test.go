package speedtest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
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
	step    atomic.Int64 // each finished test advances the clock this much
}

func newQueueHarness(t *testing.T, opts ...func(*QueueOptions)) (*Queue, *fakeExec) {
	t.Helper()
	f := &fakeExec{release: make(chan struct{}), started: make(chan struct{}, 100)}
	f.now.Store(time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC).UnixNano())
	servers := []Server{{ID: "A"}, {ID: "B"}, {ID: "C"}}
	o := QueueOptions{
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
				f.advance(time.Duration(f.step.Load()))
				return id, true
			}
		},
	}
	for _, fn := range opts {
		fn(&o)
	}
	return NewQueue(o), f
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

// TestQueueOrder: FIFO, the owner ahead of the schedule, dedupe per
// requester.
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
	q.EnqueueScheduled(Job{Region: "uk"})
	enq(Request{Kind: KindSingle, Target: "B", Who: WhoOwner})
	enq(Request{Kind: KindSingle, Who: WhoOwner})
	enq(Request{Kind: KindSweep, Who: WhoOwner})
	got := ids(q.State().Waiting)
	want := []string{"owner:single:B", "owner:single:", "owner:sweep:", "schedule:scheduled:"}
	if !eq(got, want) {
		t.Fatalf("order %v, want %v", got, want)
	}
	// Dedupe: the same requester asking again gets the queued entry.
	if e, dup := enq(Request{Kind: KindSingle, Target: "B", Who: WhoOwner}); !dup || e.ID != 2 || e.Position != 1 {
		t.Errorf("owner duplicate: %+v %v", e, dup)
	}
	if e, dup := enq(Request{Kind: KindSweep, Who: WhoOwner}); !dup || e.ID != 4 {
		t.Errorf("sweep duplicate: %+v %v", e, dup)
	}
	// Estimates: back to back; a sweep's per test.
	ws := q.State().Waiting
	for i := 1; i < len(ws); i++ {
		if !ws[i].EstStart.Equal(ws[i-1].EstFinish) || ws[i].Position != i+1 {
			t.Errorf("estimates %d: %+v after %+v", i, ws[i], ws[i-1])
		}
	}
	if sw := ws[2]; sw.Kind != KindSweep || sw.EstFinish.Sub(sw.EstStart) != 3*EstJob || sw.Steps != 3 {
		t.Errorf("sweep estimate %+v", sw)
	}
	if _, _, err := q.Enqueue(Request{Kind: KindSingle, Target: "nope", Who: WhoOwner}); !errors.Is(err, ErrUnknownID) {
		t.Errorf("unknown: %v", err)
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
	// The owner never pre-empts the running entry: a new one waits.
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
	c, _, _ := q.Enqueue(Request{Kind: KindSingle, Target: "C", Who: WhoOwner})
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
			if i%5 == 0 {
				q.EnqueueScheduled(Job{Region: "uk"})
				return
			}
			_, _, _ = q.Enqueue(Request{Kind: KindSingle, Target: []string{"A", "B", "C", ""}[i%4], Who: WhoOwner})
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

// TestQueueCancelBetweenTests: the owner cancels a sweep between two of its
// tests; the cancel wins and the next test never starts.
func TestQueueCancelBetweenTests(t *testing.T) {
	q, f := newQueueHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Run(ctx)
	sw, _, err := q.Enqueue(Request{Kind: KindSweep, Who: WhoOwner})
	if err != nil {
		t.Fatal(err)
	}
	<-f.started
	if _, _, err := q.Enqueue(Request{Kind: KindSingle, Target: "B", Who: WhoOwner}); err != nil {
		t.Fatal(err)
	}
	// The cancel lands after the sweep's context check, before its next
	// test.
	beforeNextJob = func(id uint64, i int) {
		if id == sw.ID && i == 1 {
			q.Cancel(id)
		}
	}
	defer func() { beforeNextJob = nil }()
	f.release <- struct{}{}
	<-f.started // the owner's test
	if x, _ := q.Get(sw.ID); x.State != StateCancelled {
		t.Errorf("sweep after cancel: %s", x.State)
	}
	if st := q.State(); len(st.Waiting) != 0 || st.Running == nil || st.Running.Who != WhoOwner {
		t.Errorf("queue %+v", st)
	}
	f.release <- struct{}{}
	waitFor(t, func() bool { return q.State().Running == nil })
	if got := f.log(); len(got) != 2 {
		t.Errorf("ran %v; the cancelled sweep went on", got)
	}
}

// TestQueuePauseCancel: a Pause before each job (a sweep's gap for the
// probes) and a cancel during it stops the sweep.
func TestQueuePauseCancel(t *testing.T) {
	var paused atomic.Int32
	cancelAt := make(chan struct{})
	var q *Queue
	var f *fakeExec
	q, f = newQueueHarness(t, func(o *QueueOptions) {
		o.Pause = func(ctx context.Context, j Job, trigger string) string {
			if trigger != "sweep" {
				t.Errorf("pause for %s", trigger)
			}
			if paused.Add(1) == 3 {
				close(cancelAt) // the test cancels during the third pause
				<-ctx.Done()
				return "ctx"
			}
			f.advance(time.Minute) // the gap: a minute each
			return ""
		}
	})
	f.step.Store(int64(10 * time.Second))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Run(ctx)
	e, _, err := q.Enqueue(Request{Kind: KindSweep, Who: WhoOwner})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		<-f.started
		f.release <- struct{}{}
	}
	<-cancelAt
	if _, err := q.Cancel(e.ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { x, _ := q.Get(e.ID); return x.State == StateCancelled && !x.Finished.IsZero() })
	if got := f.log(); len(got) != 2 {
		t.Errorf("ran %v after a cancel in the pause", got)
	}
	if x, _ := q.Get(e.ID); x.Stopped != StopCancelled {
		t.Errorf("stopped %q, want cancelled (not Pause's reason)", x.Stopped)
	}
}

// TestQueueEstimatesSweepPause: a sweep's estimates include the pause
// before each of its tests.
func TestQueueEstimatesSweepPause(t *testing.T) {
	q, _ := newQueueHarness(t, func(o *QueueOptions) { o.EstSweepPause = 6 * time.Second })
	now := q.o.Now()
	sw, _, _ := q.Enqueue(Request{Kind: KindSweep, Who: WhoOwner})
	one, _, _ := q.Enqueue(Request{Kind: KindSingle, Target: "A", Who: WhoOwner})
	x, _ := q.Get(sw.ID)
	if want := now.Add(3 * (EstJob + 6*time.Second)); !x.EstFinish.Equal(want) {
		t.Errorf("sweep finishes %v, want %v", x.EstFinish, want)
	}
	if y, _ := q.Get(one.ID); !y.EstStart.Equal(x.EstFinish) || !y.EstFinish.Equal(x.EstFinish.Add(EstJob)) {
		t.Errorf("single %v–%v after the sweep's %v", y.EstStart, y.EstFinish, x.EstFinish)
	}
}

// TestQueueStoppedReason: a sweep that Pause stops ends done with that
// reason and the jobs it never reached unrun; one that ran every job has no
// reason.
func TestQueueStoppedReason(t *testing.T) {
	var n atomic.Int32
	q, f := newQueueHarness(t, func(o *QueueOptions) {
		o.Pause = func(context.Context, Job, string) string {
			if n.Add(1) == 2 {
				return StopUnreachable // e.g. 3 unreachable servers in a row
			}
			return ""
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Run(ctx)
	e, _, _ := q.Enqueue(Request{Kind: KindSweep, Who: WhoOwner})
	<-f.started
	f.release <- struct{}{}
	waitFor(t, func() bool { x, _ := q.Get(e.ID); return x.State == StateDone })
	if x, _ := q.Get(e.ID); x.Stopped != StopUnreachable || x.Ran != 1 || x.Steps != 3 {
		t.Errorf("stopped sweep: %+v", x)
	}
	n.Store(-100)
	e2, _, _ := q.Enqueue(Request{Kind: KindSingle, Target: "A", Who: WhoOwner})
	<-f.started
	f.release <- struct{}{}
	waitFor(t, func() bool { x, _ := q.Get(e2.ID); return x.State == StateDone })
	if x, _ := q.Get(e2.ID); x.Stopped != "" {
		t.Errorf("complete entry stopped %q", x.Stopped)
	}
}

// TestQueueCancelledSweepIsLast: a sweep cancelled while running is the
// last sweep, with its reason.
func TestQueueCancelledSweepIsLast(t *testing.T) {
	q, f := newQueueHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Run(ctx)
	// Running: cancelled during its second test.
	sw, _, _ := q.Enqueue(Request{Kind: KindSweep, Who: WhoOwner})
	<-f.started
	f.release <- struct{}{}
	<-f.started
	x, _ := q.Cancel(sw.ID)
	if x.Stopped != StopCancelled || x.CancelledBy != "owner" {
		t.Errorf("the cancel's answer: stopped %q by %q", x.Stopped, x.CancelledBy)
	}
	waitFor(t, func() bool { x, _ := q.Get(sw.ID); return !x.Finished.IsZero() })
	if l := q.State().LastSweep; l == nil || l.ID != sw.ID || l.Ran != 2 || l.Stopped != StopCancelled || len(l.Tests) != 1 || l.Cut != 2 {
		t.Fatalf("last sweep %+v", l)
	}
}

// TestTriggerOrigin: the stored trigger keeps who asked; rows the share
// link stored before v0.8.0 still read as on demand.
func TestTriggerOrigin(t *testing.T) {
	for _, c := range []struct {
		k              Kind
		who            Who
		trigger, owner string
	}{
		{KindScheduled, WhoSchedule, "schedule", "scheduled"},
		{KindSingle, WhoOwner, "manual", "owner"},
		{KindSweep, WhoOwner, "sweep", "owner"},
	} {
		tr := trigger(c.k, c.who)
		st := model.SpeedTest{Trigger: tr}
		if tr != c.trigger || st.Origin() != c.owner || st.OnDemand() != (c.k != KindScheduled) {
			t.Errorf("%s/%s: trigger %q origin %q on-demand %v", c.k, c.who, tr, st.Origin(), st.OnDemand())
		}
	}
}
