package speedtest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"
)

// The speed-test queue: every test (Test now, a sweep of every server,
// scheduled slots) waits in one process-wide FIFO and exactly one runs at a
// time. The owner's requests go first, then schedule slots (never
// pre-empting the running test). See docs/SPEEDTEST-API.md.

// Kind is what an entry tests.
type Kind string

const (
	KindSingle    Kind = "single"    // one server, one region's chain, or the nearest servers
	KindSweep     Kind = "sweep"     // every server, one test each
	KindScheduled Kind = "scheduled" // a schedule slot
)

// Who asked for an entry.
type Who string

const (
	WhoOwner    Who = "owner"    // the dashboard
	WhoSchedule Who = "schedule" // the schedule
)

// Entry states.
const (
	StateWaiting   = "waiting"
	StateRunning   = "running"
	StateDone      = "done"
	StateCancelled = "cancelled"
	StateDropped   = "dropped" // a scheduled slot that waited too long
)

// Queue limits.
const (
	// ScheduledStale: a scheduled slot still waiting this long is dropped
	// (the schedule never stacks; the next slot comes anyway).
	ScheduledStale = 10 * time.Minute
	// KeepFinished is how long a finished entry can be polled by id.
	KeepFinished = time.Hour
	// EstJob is a typical test, for the queue's estimates.
	EstJob = EstTestTime + 5*time.Second
)

// Job is one test: a region's chain, one server, or (no chain) the nearest
// servers.
type Job struct {
	Region string
	Chain  []Server
	// steps, when set, resolves the job's servers when it runs (the
	// schedule's rotation and fallback levels, the nearest servers).
	steps func(ctx context.Context) (region string, steps []Step)
}

// Request asks for an entry. Target is a server or region ID ("" with
// KindSingle: the nearest servers).
type Request struct {
	Kind   Kind
	Target string
	Who    Who
}

// Entry is a snapshot of one queue entry.
type Entry struct {
	ID        uint64
	Kind      Kind
	Who       Who
	Target    string // a server or region ID; "" for the nearest, a sweep or a slot
	State     string
	Position  int // waiting: 1 is next; 0 otherwise
	Enqueued  time.Time
	Started   time.Time
	Finished  time.Time
	EstStart  time.Time
	EstFinish time.Time
	Step      int // the test running or last run, 1-based
	Steps     int
	Server    string // running: the server under test
	Direction string // running: picking, download, upload
	Tests     []int64
	// Per job, in order: the server tested (a sweep's jobs are single
	// servers; a region's is its first choice), the stored test (0: none
	// yet, or none saved), and how many jobs have run.
	Servers  []string
	JobTests []int64
	Ran      int
	// Stopped: why a finished entry ended before its last job
	// (StopCancelled, StopUnreachable); "" when it ran them all.
	Stopped string
	// CancelledBy: "owner" when the owner cancelled it (Cancel); ""
	// otherwise (shutdown).
	CancelledBy string
	// Cut: the job (1-based) a cancel stopped mid-test, without a result;
	// 0 when none was.
	Cut int
}

// Why an entry stopped early (Entry.Stopped).
const (
	StopCancelled   = "cancelled"   // the owner (or shutdown)
	StopUnreachable = "unreachable" // a sweep's servers stopped answering (Pause)
)

// Refusals.
var (
	ErrUnknownID = errors.New("no such server or region")
	ErrNoEntry   = errors.New("no such queue entry")
)

// QueueOptions configure a Queue.
type QueueOptions struct {
	// Resolve turns a request into its tests: single (a server, a region's
	// chain, "" for the nearest), sweep (every server), scheduled (filled
	// in by EnqueueScheduled). An unknown ID is ErrUnknownID.
	Resolve func(kind Kind, id string) ([]Job, error)
	// Exec runs one test with trigger ("manual", "sweep" or "schedule")
	// and returns the stored test's ID, if one was saved. It must return
	// promptly once ctx ends.
	Exec func(ctx context.Context, j Job, trigger string) (id int64, saved bool)
	// Pause, when set, runs before each job and may wait (e.g. to leave
	// the probes unmasked time between a sweep's tests). A non-empty result stops the entry with
	// that reason (e.g. StopUnreachable); when ctx ended it is "cancelled"
	// whatever Pause returns.
	Pause func(ctx context.Context, j Job, trigger string) (stop string)
	// EstSweepPause is Pause's typical wait before each of a sweep's
	// tests, for the estimates (0: none).
	EstSweepPause time.Duration
	// Live reports the running test's direction and server (optional).
	Live func() (direction, server string, expectedEnd time.Time)
	Log  *slog.Logger
	Now  func() time.Time
}

type qentry struct {
	Entry
	jobs   []Job
	cancel context.CancelCauseFunc
}

// Queue runs speed tests one at a time, in order.
type Queue struct {
	o    QueueOptions
	wake chan struct{}

	mu        sync.Mutex
	nextID    uint64
	waiting   []*qentry
	running   *qentry
	finished  map[uint64]*qentry
	lastSweep *qentry
}

// NewQueue returns a Queue; Run drives it.
func NewQueue(o QueueOptions) *Queue {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Queue{o: o, wake: make(chan struct{}, 1), finished: map[uint64]*qentry{}}
}

func (q *Queue) poke() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

func trigger(k Kind, who Who) string {
	switch {
	case k == KindScheduled:
		return "schedule"
	case k == KindSweep:
		return "sweep"
	}
	return "manual"
}

// same reports an equivalent request from the same requester (the owner or
// the schedule): asking again returns the entry.
func (e *qentry) same(r Request) bool {
	if e.Kind != r.Kind || e.Who != r.Who {
		return false
	}
	return e.Kind != KindSingle || e.Target == r.Target
}

// Enqueue adds a request, or returns the same requester's equivalent entry
// already queued or running (duplicate). The owner's entries go after the
// owner's waiting ones and ahead of schedule slots.
func (q *Queue) Enqueue(r Request) (Entry, bool, error) {
	if r.Kind != KindSingle && r.Kind != KindSweep {
		return Entry{}, false, fmt.Errorf("speed test queue: kind %q", r.Kind)
	}
	if r.Kind == KindSweep {
		r.Target = ""
	}
	jobs, err := q.o.Resolve(r.Kind, r.Target)
	if err != nil {
		return Entry{}, false, err
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	now := q.o.Now()
	if e := q.running; e != nil && e.same(r) {
		return q.snapshotLocked(e, now), true, nil
	}
	for _, e := range q.waiting {
		if e.same(r) {
			return q.snapshotLocked(e, now), true, nil
		}
	}
	q.nextID++
	e := &qentry{Entry: Entry{ID: q.nextID, Kind: r.Kind, Who: r.Who, Target: r.Target,
		State: StateWaiting, Enqueued: now, Steps: len(jobs), Servers: jobServers(jobs), JobTests: make([]int64, len(jobs))}, jobs: jobs}
	q.insertLocked(e)
	q.poke()
	return q.snapshotLocked(e, now), false, nil
}

// EnqueueScheduled adds a schedule slot, unless one is already waiting (the
// schedule never stacks). It reports whether it was added.
func (q *Queue) EnqueueScheduled(j Job) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, e := range q.waiting {
		if e.Kind == KindScheduled {
			return false
		}
	}
	q.nextID++
	q.insertLocked(&qentry{Entry: Entry{ID: q.nextID, Kind: KindScheduled, Who: WhoSchedule, State: StateWaiting,
		Enqueued: q.o.Now(), Steps: 1, Servers: jobServers([]Job{j}), JobTests: make([]int64, 1)}, jobs: []Job{j}})
	q.poke()
	return true
}

// insertLocked places e by class: the owner's, then schedule slots; FIFO
// within a class. Nothing pre-empts the running entry.
func (q *Queue) insertLocked(e *qentry) {
	rank := func(w Who) int {
		if w == WhoOwner {
			return 0
		}
		return 1
	}
	i := 0
	for i < len(q.waiting) && rank(q.waiting[i].Who) <= rank(e.Who) {
		i++
	}
	q.waiting = slices.Insert(q.waiting, i, e)
}

// finishLocked moves e to the finished entries in state st.
func (q *Queue) finishLocked(e *qentry, st string, now time.Time) {
	e.State, e.Finished, e.cancel = st, now, nil
	if st == StateCancelled && e.Stopped == "" {
		e.Stopped = StopCancelled
	}
	if e.Kind == KindSweep && !e.Started.IsZero() {
		q.lastSweep = e // done, stopped early, cancelled or dropped part-way: its results so far stay readable
	}
	q.finished[e.ID] = e
	for id, f := range q.finished {
		if now.Sub(f.Finished) >= KeepFinished {
			delete(q.finished, id)
		}
	}
}

// Cancel cancels a waiting or running entry (the owner's control).
func (q *Queue) Cancel(id uint64) (Entry, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	now := q.o.Now()
	if e := q.running; e != nil && e.ID == id {
		e.State, e.Stopped, e.CancelledBy = StateCancelled, StopCancelled, "owner" // the runner finishes it once the test returns
		if e.cancel != nil {
			e.cancel(ErrCancelledByOwner) // the stored row says who (cancelled_by)
		}
		return q.snapshotLocked(e, now), nil
	}
	for i, e := range q.waiting {
		if e.ID == id {
			q.waiting = slices.Delete(q.waiting, i, i+1)
			e.CancelledBy = "owner"
			q.finishLocked(e, StateCancelled, now)
			return q.snapshotLocked(e, now), nil
		}
	}
	if e, ok := q.finished[id]; ok {
		return q.snapshotLocked(e, now), nil
	}
	return Entry{}, ErrNoEntry
}

// dropStaleLocked drops scheduled slots that waited ScheduledStale.
func (q *Queue) dropStaleLocked(now time.Time) {
	q.waiting = slices.DeleteFunc(q.waiting, func(e *qentry) bool {
		if e.Kind == KindScheduled && now.Sub(e.Enqueued) >= ScheduledStale {
			q.finishLocked(e, StateDropped, now)
			q.o.Log.Info("speed test: dropped a scheduled test that waited too long")
			return true
		}
		return false
	})
}

// Run executes entries one at a time until ctx ends.
func (q *Queue) Run(ctx context.Context) {
	for {
		q.mu.Lock()
		now := q.o.Now()
		q.dropStaleLocked(now)
		var e *qentry
		if len(q.waiting) > 0 {
			e = q.waiting[0]
			q.waiting = q.waiting[1:]
			e.State = StateRunning
			if e.Started.IsZero() {
				e.Started = now
			}
			q.running = e
		}
		var ectx context.Context
		if e != nil {
			ectx, e.cancel = context.WithCancelCause(ctx)
		}
		q.mu.Unlock()
		if e != nil {
			q.execute(ectx, e)
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-q.wake:
		case <-time.After(time.Minute): // stale slots are dropped even when idle
		}
	}
}

// beforeNextJob, in tests, runs before execute takes the lock for job i
// (to race a Cancel against a sweep's next test).
var beforeNextJob func(id uint64, i int)

func (q *Queue) execute(ctx context.Context, e *qentry) {
	for i := range e.jobs {
		if ctx.Err() != nil {
			break
		}
		if q.o.Pause != nil {
			if why := q.o.Pause(ctx, e.jobs[i], trigger(e.Kind, e.Who)); why != "" {
				if ctx.Err() == nil {
					q.mu.Lock()
					e.Stopped = why
					q.mu.Unlock()
				}
				break
			}
		}
		if beforeNextJob != nil {
			beforeNextJob(e.ID, i)
		}
		q.mu.Lock()
		if e.State == StateCancelled { // cancelled since the check
			q.mu.Unlock()
			break
		}
		e.Step = i + 1
		q.mu.Unlock()
		id, saved := q.o.Exec(ctx, e.jobs[i], trigger(e.Kind, e.Who))
		q.mu.Lock()
		e.Ran = i + 1
		if saved {
			e.Tests = append(e.Tests, id)
			e.JobTests[i] = id
		} else if ctx.Err() != nil {
			e.Cut = i + 1
		}
		q.mu.Unlock()
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	cancel := e.cancel
	st := StateDone
	if e.State == StateCancelled || ctx.Err() != nil {
		st, e.Stopped = StateCancelled, StopCancelled
	}
	q.running = nil
	now := q.o.Now()
	q.finishLocked(e, st, now)
	if cancel != nil {
		cancel(nil)
	}
}

// jobServers is each job's first-choice server.
func jobServers(jobs []Job) []string {
	out := make([]string, len(jobs))
	for i, j := range jobs {
		if len(j.Chain) > 0 {
			out[i] = j.Chain[0].ID
		}
	}
	return out
}

// QueueState is the queue for the API.
type QueueState struct {
	Running   *Entry
	Waiting   []Entry
	LastSweep *Entry
}

// State reports the running entry, the waiting ones in order with their
// estimates, and the last finished sweep.
func (q *Queue) State() QueueState {
	q.mu.Lock()
	defer q.mu.Unlock()
	now := q.o.Now()
	var st QueueState
	if q.running != nil {
		e := q.snapshotLocked(q.running, now)
		st.Running = &e
	}
	for _, e := range q.waiting {
		st.Waiting = append(st.Waiting, q.snapshotLocked(e, now))
	}
	if q.lastSweep != nil {
		e := q.snapshotLocked(q.lastSweep, now)
		st.LastSweep = &e
	}
	return st
}

// Get returns an entry by id: waiting, running, or finished less than
// KeepFinished ago.
func (q *Queue) Get(id uint64) (Entry, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	now := q.o.Now()
	if e := q.running; e != nil && e.ID == id {
		return q.snapshotLocked(e, now), true
	}
	for _, e := range q.waiting {
		if e.ID == id {
			return q.snapshotLocked(e, now), true
		}
	}
	if e, ok := q.finished[id]; ok && now.Sub(e.Finished) < KeepFinished {
		return q.snapshotLocked(e, now), true
	}
	return Entry{}, false
}

// runningLeftLocked estimates the running entry's remaining time.
func (q *Queue) runningLeftLocked(now time.Time) time.Duration {
	e := q.running
	if e == nil {
		return 0
	}
	left := time.Duration(max(e.Steps-e.Step, 0)) * q.estJob(e)
	cur := EstJob
	if q.o.Live != nil {
		if _, _, end := q.o.Live(); !end.IsZero() {
			cur = max(end.Sub(now), 0)
		}
	}
	return left + cur
}

// snapshotLocked copies e with its position, estimates and live detail.
func (q *Queue) snapshotLocked(e *qentry, now time.Time) Entry {
	out := e.Entry
	out.Tests = slices.Clone(e.Tests)
	out.JobTests = slices.Clone(e.JobTests)
	switch e.State {
	case StateRunning:
		out.EstStart = e.Started
		out.EstFinish = now.Add(q.runningLeftLocked(now))
		if q.o.Live != nil {
			out.Direction, out.Server, _ = q.o.Live()
		}
	case StateWaiting:
		at := now.Add(q.runningLeftLocked(now))
		for i, w := range q.waiting {
			if w == e {
				out.Position = i + 1
				break
			}
			at = at.Add(time.Duration(max(w.Steps, 1)) * q.estJob(w))
		}
		out.EstStart = at
		out.EstFinish = at.Add(time.Duration(max(e.Steps, 1)) * q.estJob(e))
	}
	return out
}

// estJob is one of e's tests with the pause before it.
func (q *Queue) estJob(e *qentry) time.Duration {
	if e.Kind == KindSweep {
		return EstJob + q.o.EstSweepPause
	}
	return EstJob
}
