package speedtest

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
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
	bytes   atomic.Int64 // per finished test
	step    atomic.Int64 // each finished test advances the clock this much
}

func newQueueHarness(t *testing.T, opts ...func(*QueueOptions)) (*Queue, *fakeExec) {
	t.Helper()
	f := &fakeExec{release: make(chan struct{}), started: make(chan struct{}, 100)}
	f.bytes.Store(1e6)
	f.now.Store(time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC).UnixNano())
	servers := []Server{{ID: "A"}, {ID: "B"}, {ID: "C"}}
	on := true // the share link's tests are on unless a test says otherwise
	o := QueueOptions{Public: &on,
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
		Exec: func(ctx context.Context, j Job, trigger string) (int64, bool, int64) {
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
				return 0, false, 1e6
			case <-f.release:
				f.advance(time.Duration(f.step.Load()))
				return id, true, f.bytes.Load()
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
	want := []string{"owner:single:B", "owner:single:", "schedule:scheduled:", "public:single:A", "public:sweep:"}
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

// memSettings is an in-memory Settings.
type memSettings struct {
	mu sync.Mutex
	m  map[string]string
}

func (s *memSettings) Setting(_ context.Context, k string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.m[k]
	return v, ok, nil
}

func (s *memSettings) SetSetting(_ context.Context, k, v string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string]string{}
	}
	s.m[k] = v
	return nil
}

// TestQueuePublicOptIn: off by default; the Details toggle is saved; the
// flag wins at start; turning it off drops the share link's entries.
func TestQueuePublicOptIn(t *testing.T) {
	set := &memSettings{}
	off := func(o *QueueOptions) { o.Public, o.Settings = nil, set }
	q, _ := newQueueHarness(t, off)
	q.load(context.Background())
	pub := Request{Kind: KindSingle, Target: "A", Who: WhoPublic, Visitor: "v"}
	if _, _, err := q.Enqueue(pub); !errors.Is(err, ErrPublicDisabled) {
		t.Errorf("off by default: %v", err)
	}
	if _, err := q.Admit(pub); !errors.Is(err, ErrPublicDisabled) {
		t.Errorf("Admit while off: %v", err)
	}
	if _, _, err := q.Enqueue(Request{Kind: KindSingle, Target: "A", Who: WhoOwner}); err != nil {
		t.Errorf("the owner: %v", err)
	}
	if err := q.SetPublic(context.Background(), true); err != nil || set.m[SettingPublic] != "1" {
		t.Fatalf("toggle: %v %v", err, set.m)
	}
	if _, _, err := q.Enqueue(pub); err != nil {
		t.Errorf("on: %v", err)
	}
	// Saved: a restart keeps it on; a --speedtest-public=false flag wins.
	q2, _ := newQueueHarness(t, off)
	q2.load(context.Background())
	if !q2.PublicEnabled() {
		t.Error("not restored")
	}
	no := false
	q3, _ := newQueueHarness(t, func(o *QueueOptions) { o.Public, o.Settings = &no, set })
	q3.load(context.Background())
	if q3.PublicEnabled() {
		t.Error("the flag didn't win")
	}
	// Off again: the share link's waiting entries go.
	if err := q.SetPublic(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if st := q.State(); st.PublicWaiting != 0 || len(st.Waiting) != 1 || st.Public {
		t.Errorf("after off: %+v", st)
	}
}

// TestQueueBudget: the share link's tests spend a rolling 24-hour budget
// of bytes actually transferred, kept across restarts; past it, requests
// are refused until enough use ages out, waiting entries are dropped, and
// a sweep stops. The owner is never limited by it.
func TestQueueBudget(t *testing.T) {
	set := &memSettings{}
	withBudget := func(o *QueueOptions) { o.Settings, o.PublicBudget = set, 5e6 }
	q, f := newQueueHarness(t, withBudget)
	f.bytes.Store(3e6)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { q.Run(ctx); close(done) }()
	run := func(r Request) Entry {
		t.Helper()
		e, _, err := q.Enqueue(r)
		if err != nil {
			t.Fatalf("%+v: %v", r, err)
		}
		<-f.started
		f.release <- struct{}{}
		waitFor(t, func() bool { x, _ := q.Get(e.ID); return x.State == StateDone })
		return e
	}
	start := time.Unix(0, f.now.Load())
	run(Request{Kind: KindSingle, Target: "A", Who: WhoPublic, Visitor: "v1"})
	f.advance(time.Hour)
	if _, used, resets := q.Budget(); used != 3e6 || !resets.IsZero() {
		t.Errorf("after one: %d %v", used, resets)
	}
	// A sweep stops once the budget is spent (3 + 3 > 5): one test, not three.
	sw, _, err := q.Enqueue(Request{Kind: KindSweep, Who: WhoPublic, Visitor: "v2"})
	if err != nil {
		t.Fatal(err)
	}
	<-f.started
	f.release <- struct{}{}
	waitFor(t, func() bool { x, _ := q.Get(sw.ID); return x.State == StateDone })
	if x, _ := q.Get(sw.ID); len(x.Tests) != 1 {
		t.Errorf("sweep ran %d tests past the budget", len(x.Tests))
	}
	limit, used, resets := q.Budget()
	if limit != 5e6 || used != 6e6 || !resets.Equal(start.Add(24*time.Hour)) {
		t.Errorf("spent: %d %d %v (start %v)", limit, used, resets, start)
	}
	var be *BudgetError
	if _, _, err := q.Enqueue(Request{Kind: KindSingle, Target: "B", Who: WhoPublic, Visitor: "v3"}); !errors.As(err, &be) || !be.ResetsAt.Equal(resets) {
		t.Errorf("refused: %v", err)
	}
	if _, err := q.Admit(Request{Kind: KindSingle, Target: "B", Who: WhoPublic, Visitor: "v3"}); !errors.As(err, &be) {
		t.Errorf("Admit: %v", err)
	}
	run(Request{Kind: KindSingle, Target: "C", Who: WhoOwner}) // the owner is not limited
	cancel()
	<-done
	// Kept across a restart.
	q2, f2 := newQueueHarness(t, withBudget)
	f2.now.Store(f.now.Load())
	q2.load(context.Background())
	if _, used, resets := q2.Budget(); used != 6e6 || resets.IsZero() {
		t.Errorf("restored: %d %v", used, resets)
	}
	// A waiting share-link entry is dropped when its turn comes past the
	// budget (it got in before).
	q2.mu.Lock()
	q2.nextID++
	q2.waiting = append(q2.waiting, &qentry{Entry: Entry{ID: q2.nextID, Kind: KindSingle, Target: "A", Who: WhoPublic, Visitor: "v9", State: StateWaiting, JobTests: []int64{0}}, jobs: []Job{{}}})
	q2.mu.Unlock()
	ctx2, cancel2 := context.WithCancel(context.Background())
	done2 := make(chan struct{})
	go func() { q2.Run(ctx2); close(done2) }()
	waitFor(t, func() bool { x, ok := q2.Get(1); return ok && x.State == StateDropped })
	cancel2()
	<-done2
	// 24 hours after the first test (the sweep's ran an hour later), it
	// ages out: room again.
	f2.advance(23*time.Hour + 30*time.Minute)
	if _, used, resets := q2.Budget(); used != 3e6 || !resets.IsZero() {
		t.Errorf("aged: %d %v", used, resets)
	}
}

// TestQueueScheduledAheadOfPublic: a schedule slot queued behind public
// sweeps runs next: the running public sweep yields after its current
// test, then resumes ahead of the other public sweeps.
func TestQueueScheduledAheadOfPublic(t *testing.T) {
	q, f := newQueueHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Run(ctx)
	for i := range MaxPublicSweeps {
		if _, _, err := q.Enqueue(Request{Kind: KindSweep, Who: WhoPublic, Visitor: fmt.Sprint("v", i)}); err != nil {
			t.Fatal(err)
		}
	}
	<-f.started // the first sweep's first test runs
	q.EnqueueScheduled(Job{Region: "uk"})
	if st := q.State(); st.Waiting[0].Kind != KindScheduled || st.Waiting[0].Position != 1 {
		t.Fatalf("slot not next: %v", ids(st.Waiting))
	}
	// The running test isn't pre-empted; the sweep yields after it.
	for range 3 {
		f.release <- struct{}{}
		<-f.started
	}
	f.release <- struct{}{}
	waitFor(t, func() bool { return len(f.log()) >= 5 })
	if got := f.log(); !eq(got[:5], []string{"public_sweep:A", "schedule:uk", "public_sweep:B", "public_sweep:C", "public_sweep:A"}) {
		t.Errorf("ran %v", got)
	}
	if st := q.State(); st.Running == nil || st.Running.Visitor != "v1" || st.Running.Step != 1 {
		t.Errorf("then the next public sweep: %+v", st.Running)
	}
	cancel()
}

// TestQueuePublicTime: the share link's tests may keep the line under test
// at most PublicTimePerHour per hour (and PublicTimePerDay per day), and a
// public sweep waits PublicSweepGap after the last one.
func TestQueuePublicTime(t *testing.T) {
	q, f := newQueueHarness(t)
	f.step.Store(int64(4 * time.Minute))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Run(ctx)
	run := func(r Request) Entry {
		t.Helper()
		e, _, err := q.Enqueue(r)
		if err != nil {
			t.Fatalf("%+v: %v", r, err)
		}
		for {
			<-f.started
			f.release <- struct{}{}
			x, _ := q.Get(e.ID)
			if x.Step >= x.Steps {
				break
			}
		}
		waitFor(t, func() bool { x, _ := q.Get(e.ID); return x.State == StateDone })
		x, _ := q.Get(e.ID)
		return x
	}
	first := run(Request{Kind: KindSingle, Target: "A", Who: WhoPublic, Visitor: "v1"})
	run(Request{Kind: KindSingle, Target: "B", Who: WhoPublic, Visitor: "v2"})
	run(Request{Kind: KindSingle, Target: "C", Who: WhoPublic, Visitor: "v3"}) // 12 min in the hour
	var be *BudgetError
	if _, _, err := q.Enqueue(Request{Kind: KindSingle, Target: "A", Who: WhoPublic, Visitor: "v4"}); !errors.As(err, &be) || !be.Time ||
		!be.ResetsAt.Equal(first.Finished.Add(time.Hour)) {
		t.Fatalf("hour cap: %v (%+v)", err, be)
	}
	if st := q.State(); st.TimeHour != 12*time.Minute || st.TimeResets.IsZero() {
		t.Errorf("state %+v", st)
	}
	run(Request{Kind: KindSingle, Target: "A", Who: WhoOwner}) // the owner is not capped
	// An hour later there is room; a public sweep, then the gap.
	f.advance(time.Hour)
	f.step.Store(int64(time.Minute))
	run(Request{Kind: KindSweep, Who: WhoPublic, Visitor: "v5"})
	var gap *SweepWaitError
	if _, _, err := q.Enqueue(Request{Kind: KindSweep, Who: WhoPublic, Visitor: "v6"}); !errors.As(err, &gap) {
		t.Fatalf("sweep gap: %v", err)
	}
	if st := q.State(); !st.SweepAt.Equal(gap.Until) {
		t.Errorf("sweep at %v, want %v", st.SweepAt, gap.Until)
	}
	if _, _, err := q.Enqueue(Request{Kind: KindSingle, Target: "B", Who: WhoPublic, Visitor: "v6"}); err != nil {
		t.Errorf("a single during the gap: %v", err)
	}
	// The day cap: 60 minutes in 24 hours.
	q.mu.Lock()
	q.ledger = nil
	for i := range 6 {
		q.ledger = append(q.ledger, usage{At: time.Unix(0, f.now.Load()).Add(-time.Duration(20-i*2) * time.Hour), Dur: 10 * time.Minute})
	}
	q.mu.Unlock()
	if _, _, err := q.Enqueue(Request{Kind: KindSingle, Target: "C", Who: WhoPublic, Visitor: "v7"}); !errors.As(err, &be) || !be.Time {
		t.Errorf("day cap: %v", err)
	}
}

// TestBudgetChargesTheWire: a share-link test against a real (fake) iperf3
// server charges the budget at least every byte the server moved, both
// directions, ramp-up included.
func TestBudgetChargesTheWire(t *testing.T) {
	f := newFakeServer(t, "")
	f.rate = 32 << 10
	host, port, _ := net.SplitHostPort(f.addr())
	p, _ := strconv.Atoi(port)
	sv := Server{ID: "Local", Host: host, PortMin: p, PortMax: p, Custom: true, LAN: true} // catalog servers must be public
	s := New(Options{Catalog: &Catalog{Servers: []Server{sv}, byID: map[string]int{"Local": 0}}})
	on := true
	q := NewQueue(QueueOptions{Public: &on, Exec: s.execJob, Live: s.live,
		Resolve: func(Kind, string) ([]Job, error) { return []Job{{Chain: []Server{sv}}}, nil }})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Run(ctx)
	e, _, err := q.Enqueue(Request{Kind: KindSingle, Target: "Local", Who: WhoPublic, Visitor: "v"})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(60 * time.Second)
	for {
		if x, _ := q.Get(e.ID); x.State == StateDone {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("test did not finish")
		}
		time.Sleep(20 * time.Millisecond)
	}
	f.mu.Lock()
	moved := f.moved
	f.mu.Unlock()
	if _, used, _ := q.Budget(); moved == 0 || used < moved {
		t.Errorf("budget charged %d bytes, the server moved %d", used, moved)
	}
	if st := q.State(); st.TimeHour <= 0 {
		t.Errorf("no test time charged: %+v", st)
	}
}

// TestTriggerOrigin: the stored trigger keeps who asked, so windows can say
// a share-link visitor's test masked them.
func TestTriggerOrigin(t *testing.T) {
	for _, c := range []struct {
		k              Kind
		who            Who
		trigger, owner string
	}{
		{KindScheduled, WhoSchedule, "schedule", "scheduled"},
		{KindSingle, WhoOwner, "manual", "owner"},
		{KindSweep, WhoOwner, "sweep", "owner"},
		{KindSingle, WhoPublic, "public", "public"},
		{KindSweep, WhoPublic, "public_sweep", "public"},
	} {
		tr := trigger(c.k, c.who)
		st := model.SpeedTest{Trigger: tr}
		if tr != c.trigger || st.Origin() != c.owner || st.OnDemand() != (c.k != KindScheduled) {
			t.Errorf("%s/%s: trigger %q origin %q on-demand %v", c.k, c.who, tr, st.Origin(), st.OnDemand())
		}
	}
}
