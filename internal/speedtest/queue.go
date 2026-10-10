package speedtest

import (
	"errors"
	"slices"
	"sync"
	"time"
)

// The test queue: every test, whether "Test now", a sweep of every server
// or a scheduled slot, from the local dashboard, the share link or the
// schedule, goes into one process-wide FIFO, and exactly one test runs at
// a time, so results never skew each other.
//
//   - The owner's entries (local) and scheduled slots go ahead of waiting
//     visitors' (share link) entries; nothing pre-empts the running entry.
//   - An equivalent waiting entry (same kind and target) is returned
//     instead of a new one.
//   - Visitors may have one waiting entry each (by client key: the
//     public handler's Cf-Connecting-Ip, IPv6 grouped by /64), and at most
//     MaxPublicSweeps public sweeps may wait in all.
//   - A scheduled slot still waiting after ScheduledTTL is dropped; missed
//     slots never stack.
//   - The owner can cancel the running entry or any waiting one.

// Entry kinds.
const (
	KindTest      = "test"      // Test now: one server or region
	KindSweep     = "sweep"     // every catalog server, one at a time
	KindScheduled = "scheduled" // a slot of the schedule
)

// Who queued an entry.
const (
	WhoOwner     = "owner"
	WhoVisitor   = "visitor"
	WhoScheduled = "scheduled"
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
	MaxPublicSweeps = 3
	ScheduledTTL    = 10 * time.Minute
	maxFinished     = 32 // finished entries kept for polling
	// EstSweepStep is the estimate per server of a sweep (and per test).
	EstSweepStep = 20 * time.Second
)

// ErrQueueFull: a visitor's limit is reached.
var ErrQueueFull = errors.New("the speed test queue is full for now")

// ErrNoEntry: no such entry (or it is no longer cancellable).
var ErrNoEntry = errors.New("no such queue entry")

// SweepResult is one server of a sweep.
type SweepResult struct {
	Server    string
	Status    string // waiting, testing, done (ok), busy, unreachable, failed
	Direction string // while testing
	DownBps   *float64
	UpBps     *float64
	DownShort bool // the direction hit the byte cap
	UpShort   bool
	At        time.Time
}

// Progress is a running entry's progress.
type Progress struct {
	Step   string // picking, download, upload
	Server string // ID
	Index  int    // 1-based, of Total (sweeps; 1 of 1 otherwise)
	Total  int
}

// Entry is one queued request.
type Entry struct {
	ID          int64
	Kind        string
	Who         string
	Client      string // visitor's client key; never shown
	Target      string // server or region ID ("" = nearest); a slot: its region or server
	RequestedAt time.Time
	StartedAt   time.Time
	FinishedAt  time.Time
	State       string
	Progress    Progress
	Results     []SweepResult // sweeps
	TestIDs     []int64       // stored tests

	slot   *target // scheduled: the slot's target
	cancel func()
}

func (e *Entry) clone() Entry {
	c := *e
	c.Results = slices.Clone(e.Results)
	c.TestIDs = slices.Clone(e.TestIDs)
	c.slot, c.cancel = nil, nil
	return c
}

// QueueState is a snapshot of the queue.
type QueueState struct {
	Running   *Entry
	Waiting   []Entry // in order
	LastSweep *Entry  // the last finished (or cancelled) sweep
}

type queue struct {
	mu        sync.Mutex
	next      int64
	running   *Entry
	waiting   []*Entry
	finished  []*Entry
	lastSweep *Entry
	notify    func()
}

func (q *queue) snapshot() QueueState {
	q.mu.Lock()
	defer q.mu.Unlock()
	var st QueueState
	if q.running != nil {
		c := q.running.clone()
		st.Running = &c
	}
	for _, e := range q.waiting {
		st.Waiting = append(st.Waiting, e.clone())
	}
	if q.lastSweep != nil {
		c := q.lastSweep.clone()
		st.LastSweep = &c
	}
	return st
}

// entry returns any entry by ID (waiting, running or recently finished).
func (q *queue) entry(id int64) (Entry, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, e := range q.all() {
		if e.ID == id {
			return e.clone(), true
		}
	}
	return Entry{}, false
}

func (q *queue) all() []*Entry {
	out := slices.Clone(q.waiting)
	if q.running != nil {
		out = append(out, q.running)
	}
	return append(out, q.finished...)
}

// add queues e (Kind, Who, Client, Target set). It returns the queued
// entry, or an equivalent one already waiting (dup), or ErrQueueFull.
func (q *queue) add(e *Entry, now time.Time) (Entry, bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, w := range q.waiting {
		if w.Kind == e.Kind && w.Target == e.Target {
			return w.clone(), true, nil
		}
	}
	if e.Who == WhoVisitor {
		sweeps := 0
		for _, w := range q.waiting {
			if w.Who == WhoVisitor && w.Client == e.Client {
				return Entry{}, false, ErrQueueFull
			}
			if w.Who == WhoVisitor && w.Kind == KindSweep {
				sweeps++
			}
		}
		if e.Kind == KindSweep && sweeps >= MaxPublicSweeps {
			return Entry{}, false, ErrQueueFull
		}
	}
	q.next++
	e.ID, e.RequestedAt, e.State = q.next, now, StateWaiting
	// The owner's entries and scheduled slots go after the other owner
	// and scheduled entries, ahead of every waiting visitor.
	at := len(q.waiting)
	if e.Who != WhoVisitor {
		at = slices.IndexFunc(q.waiting, func(w *Entry) bool { return w.Who == WhoVisitor })
		if at < 0 {
			at = len(q.waiting)
		}
	}
	q.waiting = slices.Insert(q.waiting, at, e)
	c := e.clone()
	if q.notify != nil {
		q.notify()
	}
	return c, false, nil
}

// pop takes the next entry to run, dropping scheduled slots that waited
// longer than ScheduledTTL. It returns nil while one runs or none waits.
func (q *queue) pop(now time.Time) *Entry {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.running != nil {
		return nil
	}
	for len(q.waiting) > 0 {
		e := q.waiting[0]
		q.waiting = q.waiting[1:]
		if e.Kind == KindScheduled && now.Sub(e.RequestedAt) > ScheduledTTL {
			e.State, e.FinishedAt = StateDropped, now
			q.finishLocked(e)
			continue
		}
		e.State, e.StartedAt = StateRunning, now
		q.running = e
		return e
	}
	return nil
}

// update changes the running entry under the lock.
func (q *queue) update(e *Entry, f func(*Entry)) {
	q.mu.Lock()
	f(e)
	q.mu.Unlock()
}

// done finishes the running entry.
func (q *queue) done(e *Entry, now time.Time) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if e.State == StateRunning {
		e.State = StateDone
	}
	e.FinishedAt = now
	if q.running == e {
		q.running = nil
	}
	q.finishLocked(e)
	if q.notify != nil {
		q.notify()
	}
}

func (q *queue) finishLocked(e *Entry) {
	q.finished = append(q.finished, e)
	if len(q.finished) > maxFinished {
		q.finished = q.finished[len(q.finished)-maxFinished:]
	}
	if e.Kind == KindSweep && e.State != StateDropped {
		q.lastSweep = e
	}
}

// cancel removes a waiting entry or stops the running one.
func (q *queue) cancel(id int64, now time.Time) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.running != nil && q.running.ID == id {
		q.running.State = StateCancelled
		if q.running.cancel != nil {
			q.running.cancel()
		}
		return nil
	}
	for i, e := range q.waiting {
		if e.ID == id {
			q.waiting = slices.Delete(q.waiting, i, i+1)
			e.State, e.FinishedAt = StateCancelled, now
			q.finishLocked(e)
			return nil
		}
	}
	return ErrNoEntry
}

// clear cancels every waiting entry of who ("" = all).
func (q *queue) clear(who string, now time.Time) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	var keep []*Entry
	n := 0
	for _, e := range q.waiting {
		if who == "" || e.Who == who {
			e.State, e.FinishedAt = StateCancelled, now
			q.finishLocked(e)
			n++
			continue
		}
		keep = append(keep, e)
	}
	q.waiting = keep
	return n
}

// estimate is how long an entry will run.
func (e *Entry) estimate(sweepSize int) time.Duration {
	if e.Kind == KindSweep {
		return time.Duration(sweepSize) * EstSweepStep
	}
	return EstSweepStep
}
