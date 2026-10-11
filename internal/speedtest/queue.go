package speedtest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"
)

// The speed-test queue: every test (Test now, a sweep of every server,
// scheduled slots) waits in one process-wide FIFO and exactly one runs at a
// time. The owner's requests go first, then schedule slots, then the share
// link's (never pre-empting the running test); the share link's are capped. See docs/SPEEDTEST-API.md.

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
	WhoOwner    Who = "owner"    // the local dashboard
	WhoPublic   Who = "public"   // the share link
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
	// MaxPublicWaiting bounds the share link's waiting entries in all;
	// a visitor may have one waiting, and at most MaxPublicSweeps of them
	// may be sweeps.
	MaxPublicWaiting = 12
	MaxPublicSweeps  = 3
	// ScheduledStale: a scheduled slot still waiting this long is dropped
	// (the schedule never stacks; the next slot comes anyway).
	ScheduledStale = 10 * time.Minute
	// KeepFinished is how long a finished entry can be polled by id.
	KeepFinished = time.Hour
	// EstJob is a typical test, for the queue's estimates.
	EstJob = EstTestTime + 5*time.Second
	// DefaultPublicBudget is the share link's data budget per rolling 24
	// hours: bytes its tests actually transferred.
	DefaultPublicBudget int64 = 10e9
	budgetSpan                = 24 * time.Hour
	// The share link's tests mask probes (they would measure the test),
	// so they may keep the line under test at most PublicTimePerHour in
	// any hour and PublicTimePerDay in any 24 hours, and a public sweep
	// can start PublicSweepGap after the last one ended at the earliest:
	// visitors can't blank out the monitoring.
	PublicTimePerHour = 10 * time.Minute
	PublicTimePerDay  = 60 * time.Minute
	PublicSweepGap    = 30 * time.Minute
	// Settings keys (Store.Setting): the owner's opt-in and the budget's
	// ledger, so both survive restarts.
	SettingPublic       = "speedtest_public"
	SettingPublicLedger = "speedtest_public_bytes"
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
// KindSingle: the nearest servers). Visitor is the share link's per-client
// key (never shown).
type Request struct {
	Kind    Kind
	Target  string
	Who     Who
	Visitor string
}

// Entry is a snapshot of one queue entry.
type Entry struct {
	ID        uint64
	Kind      Kind
	Who       Who
	Visitor   string // the share link's client key; never shown
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
	// Held: a waiting share-link sweep that may not start before EstStart
	// (the gap after the last one).
	Held bool
	// Stopped: why a finished entry ended before its last job (StopCancelled,
	// StopUnreachable, StopLimit); "" when it ran them all.
	Stopped string
}

// Why an entry stopped early (Entry.Stopped).
const (
	StopCancelled   = "cancelled"   // the owner (or shutdown)
	StopUnreachable = "unreachable" // a sweep's servers stopped answering (Pause)
	StopLimit       = "limit"       // the share link's budget or time ran out, or it was turned off
)

// Refusals.
var (
	ErrQueueFull      = errors.New("the queue is full")
	ErrUnknownID      = errors.New("no such server or region")
	ErrNoEntry        = errors.New("no such queue entry")
	ErrPublicDisabled = errors.New("tests from the share link are turned off")
)

// BudgetError: the share link's budget (data, or test time) is spent
// until ResetsAt.
type BudgetError struct {
	ResetsAt time.Time
	Time     bool // the test-time cap rather than the data budget
}

func (e *BudgetError) Error() string {
	if e.Time {
		return "tests from the share link have used their time for now"
	}
	return "the share link's daily test budget is used up"
}

// SweepWaitError: a public sweep ran recently; the next may start at
// Until.
type SweepWaitError struct{ Until time.Time }

func (e *SweepWaitError) Error() string { return "a full sweep ran recently; try again later" }

// Settings persists the owner's choices (Store).
type Settings interface {
	Setting(ctx context.Context, key string) (string, bool, error)
	SetSetting(ctx context.Context, key, value string) error
}

// QueueFullError says why the share link may not add an entry.
type QueueFullError struct{ Reason string }

func (e *QueueFullError) Error() string { return e.Reason }
func (e *QueueFullError) Unwrap() error { return ErrQueueFull }

// QueueOptions configure a Queue.
type QueueOptions struct {
	// Resolve turns a request into its tests: single (a server, a region's
	// chain, "" for the nearest), sweep (every server), scheduled (filled
	// in by EnqueueScheduled). An unknown ID is ErrUnknownID.
	Resolve func(kind Kind, id string) ([]Job, error)
	// Exec runs one test with trigger ("manual", "sweep", "public",
	// "public_sweep" or "schedule")
	// and returns the stored test's ID, if one was saved, and the bytes it
	// transferred. It must return promptly once ctx ends.
	Exec func(ctx context.Context, j Job, trigger string) (id int64, saved bool, bytes int64)
	// Settings keeps the share link's opt-in and budget ledger (optional).
	Settings Settings
	// Public, when set (--speedtest-public), is the opt-in at start;
	// otherwise the saved choice, else off.
	Public *bool
	// PublicBudget is the share link's bytes per rolling 24 hours
	// (default DefaultPublicBudget).
	PublicBudget int64
	// Pause, when set, runs before each job and may wait (e.g. to leave
	// the probes unmasked time between a sweep's tests); that time is not
	// charged to the share link. A non-empty result stops the entry with
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
	next   int // the next job: a public sweep that yielded resumes here
	cancel context.CancelFunc
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
	public    bool      // the owner lets the share link queue tests
	ledger    []usage   // the share link's tests, last 24 h, oldest first
	sweepEnd  time.Time // when the last public sweep ended (saved with the ledger)
	pubFrom   time.Time // a share-link test runs: its time from here is not in the ledger yet
}

// savedLedger is the ledger's saved form (SettingPublicLedger).
type savedLedger struct {
	Uses     []usage
	SweepEnd time.Time `json:",omitzero"`
}

// chargeEvery is how often a running share-link test's time is charged
// (and saved) while it runs.
const chargeEvery = 15 * time.Second

// usage is the bytes one share-link test transferred and how long it kept
// the line under test.
type usage struct {
	At    time.Time
	Bytes int64
	Dur   time.Duration `json:",omitempty"`
}

// NewQueue returns a Queue; Run drives it.
func NewQueue(o QueueOptions) *Queue {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.PublicBudget <= 0 {
		o.PublicBudget = DefaultPublicBudget
	}
	q := &Queue{o: o, wake: make(chan struct{}, 1), finished: map[uint64]*qentry{}}
	if o.Public != nil {
		q.public = *o.Public
	}
	return q
}

// load restores the opt-in (unless the flag set it) and the ledger.
func (q *Queue) load(ctx context.Context) {
	if q.o.Settings == nil {
		return
	}
	pub, okPub, _ := q.o.Settings.Setting(ctx, SettingPublic)
	led, okLed, _ := q.o.Settings.Setting(ctx, SettingPublicLedger)
	q.mu.Lock()
	defer q.mu.Unlock()
	if okPub && q.o.Public == nil {
		q.public = pub == "1"
	}
	if okLed {
		var l savedLedger
		if json.Unmarshal([]byte(led), &l) == nil {
			q.ledger, q.sweepEnd = l.Uses, l.SweepEnd
		}
	}
	q.pruneLedgerLocked(q.o.Now())
}

// PublicEnabled reports whether the share link may queue tests.
func (q *Queue) PublicEnabled() bool { q.mu.Lock(); defer q.mu.Unlock(); return q.public }

// SetPublic turns the share link's tests on or off (the Details toggle) and
// saves the choice. Turning it off drops the share link's waiting entries.
func (q *Queue) SetPublic(ctx context.Context, on bool) error {
	if q.o.Settings != nil {
		v := "0"
		if on {
			v = "1"
		}
		if err := q.o.Settings.SetSetting(ctx, SettingPublic, v); err != nil {
			return err
		}
	}
	q.mu.Lock()
	q.public = on
	q.mu.Unlock()
	if !on {
		q.ClearPublic()
	}
	return nil
}

func (q *Queue) pruneLedgerLocked(now time.Time) {
	q.ledger = slices.DeleteFunc(q.ledger, func(u usage) bool { return now.Sub(u.At) >= budgetSpan })
}

// Budget reports the share link's budget: its limit, the bytes used in the
// last 24 hours, and when it is spent, when enough of that use ages out.
func (q *Queue) Budget() (limit, used int64, resetsAt time.Time) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.budgetLocked(q.o.Now())
}

func (q *Queue) budgetLocked(now time.Time) (limit, used int64, resetsAt time.Time) {
	q.pruneLedgerLocked(now)
	limit = q.o.PublicBudget
	for _, u := range q.ledger {
		used += u.Bytes
	}
	if used >= limit {
		left := used
		for _, u := range q.ledger { // oldest first: the first age-out that brings it under
			left -= u.Bytes
			if left < limit {
				resetsAt = u.At.Add(budgetSpan)
				break
			}
		}
	}
	return limit, used, resetsAt
}

// timeLocked is the share link's test time in the last hour and day, and,
// when either cap is reached, when enough of it ages out.
func (q *Queue) timeLocked(now time.Time) (hour, day time.Duration, resetsAt time.Time) {
	q.pruneLedgerLocked(now)
	for _, u := range q.ledger {
		day += u.Dur
		if now.Sub(u.At) < time.Hour {
			hour += u.Dur
		}
	}
	if !q.pubFrom.IsZero() && now.After(q.pubFrom) { // the running test's time so far
		hour += now.Sub(q.pubFrom)
		day += now.Sub(q.pubFrom)
	}
	reset := func(span, limit, used time.Duration) time.Time {
		left := used
		for _, u := range q.ledger {
			if now.Sub(u.At) >= span {
				continue
			}
			left -= u.Dur
			if left < limit {
				return u.At.Add(span)
			}
		}
		return now.Add(span)
	}
	if day >= PublicTimePerDay {
		resetsAt = reset(budgetSpan, PublicTimePerDay, day)
	}
	if hour >= PublicTimePerHour {
		if r := reset(time.Hour, PublicTimePerHour, hour); r.After(resetsAt) {
			resetsAt = r
		}
	}
	return hour, day, resetsAt
}

// spend adds a share-link test's bytes and the time since pubFrom (when
// set) to the ledger and saves it; final ends the test's charging.
func (q *Queue) spend(bytes int64, final bool) {
	q.mu.Lock()
	now := q.o.Now()
	var dur time.Duration
	if !q.pubFrom.IsZero() {
		dur = max(now.Sub(q.pubFrom), 0)
		q.pubFrom = now
	}
	if final {
		q.pubFrom = time.Time{}
	}
	if bytes <= 0 && dur <= 0 {
		q.mu.Unlock()
		return
	}
	q.pruneLedgerLocked(now)
	q.ledger = append(q.ledger, usage{At: now, Bytes: bytes, Dur: dur})
	q.mu.Unlock()
	q.save()
}

// save writes the ledger and the last public sweep's end.
func (q *Queue) save() {
	q.mu.Lock()
	b, _ := json.Marshal(savedLedger{Uses: q.ledger, SweepEnd: q.sweepEnd})
	q.mu.Unlock()
	if q.o.Settings != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := q.o.Settings.SetSetting(ctx, SettingPublicLedger, string(b)); err != nil {
			q.o.Log.Warn("speed test: saving the share link's budget failed", "err", err)
		}
	}
}

// publicLocked refuses a share-link request: off, or the budget spent.
func (q *Queue) publicLocked(now time.Time) error {
	if !q.public {
		return ErrPublicDisabled
	}
	if _, _, resets := q.budgetLocked(now); !resets.IsZero() {
		return &BudgetError{ResetsAt: resets}
	}
	if _, _, resets := q.timeLocked(now); !resets.IsZero() {
		return &BudgetError{ResetsAt: resets, Time: true}
	}
	return nil
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
	case who == WhoPublic && k == KindSweep:
		return "public_sweep"
	case who == WhoPublic:
		return "public"
	case k == KindSweep:
		return "sweep"
	}
	return "manual"
}

// same reports an equivalent request from the same requester (the owner,
// the schedule, or one share-link visitor): asking again returns the entry.
// Different visitors get entries of their own (the caps bound them).
func (e *qentry) same(r Request) bool {
	if e.Kind != r.Kind || e.Who != r.Who || (r.Who == WhoPublic && e.Visitor != r.Visitor) {
		return false
	}
	return e.Kind != KindSingle || e.Target == r.Target
}

// Enqueue adds a request, or returns the same requester's equivalent entry
// already queued or running (duplicate). The owner's entries go after the
// owner's waiting ones and ahead of everyone else's. The share link's are
// refused with a *QueueFullError when the visitor already has one
// waiting, when MaxPublicSweeps sweeps or MaxPublicWaiting entries are
// waiting.
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
	if r.Who == WhoPublic && !q.public {
		return Entry{}, false, ErrPublicDisabled
	}
	if e := q.running; e != nil && e.same(r) {
		return q.snapshotLocked(e, now), true, nil
	}
	for _, e := range q.waiting {
		if e.same(r) {
			return q.snapshotLocked(e, now), true, nil
		}
	}
	if err := q.capsLocked(r); err != nil {
		return Entry{}, false, err
	}
	q.nextID++
	e := &qentry{Entry: Entry{ID: q.nextID, Kind: r.Kind, Who: r.Who, Visitor: r.Visitor, Target: r.Target,
		State: StateWaiting, Enqueued: now, Steps: len(jobs), Servers: jobServers(jobs), JobTests: make([]int64, len(jobs))}, jobs: jobs}
	if r.Who != WhoPublic {
		e.Visitor = ""
	}
	q.insertLocked(e)
	q.poke()
	return q.snapshotLocked(e, now), false, nil
}

// capsLocked applies the share link's caps to a new entry. q.mu is held.
func (q *Queue) capsLocked(r Request) error {
	if r.Who != WhoPublic {
		return nil
	}
	if err := q.publicLocked(q.o.Now()); err != nil {
		return err
	}
	pub, sweeps := 0, 0
	for _, e := range q.waiting {
		if e.Who != WhoPublic {
			continue
		}
		pub++
		if e.Kind == KindSweep {
			sweeps++
		}
		if e.Visitor == r.Visitor {
			return &QueueFullError{"you already have a test waiting"}
		}
	}
	switch {
	case pub >= MaxPublicWaiting:
		return &QueueFullError{"the queue is full"}
	case r.Kind == KindSweep && sweeps >= MaxPublicSweeps:
		return &QueueFullError{"enough full sweeps are already waiting"}
	case r.Kind == KindSweep && !q.sweepEnd.IsZero() && q.o.Now().Before(q.sweepEnd.Add(PublicSweepGap)):
		return &SweepWaitError{Until: q.sweepEnd.Add(PublicSweepGap)}
	}
	return nil
}

// Admit says what Enqueue would do with r right now: return an existing
// entry (dup), refuse it (a *QueueFullError), or add it (neither). The
// share link asks first, so a refusal or a duplicate never counts as a
// new start against its per-client limits.
func (q *Queue) Admit(r Request) (dup bool, err error) {
	if r.Kind == KindSweep {
		r.Target = ""
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if r.Who == WhoPublic && !q.public {
		return false, ErrPublicDisabled
	}
	if (q.running != nil && q.running.same(r)) || slices.ContainsFunc(q.waiting, func(e *qentry) bool { return e.same(r) }) {
		return true, nil
	}
	return false, q.capsLocked(r)
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

// insertLocked places e by class: the owner's, then schedule slots, then
// the share link's; FIFO within a class. A schedule slot thus never
// starves behind public sweeps, and the owner always goes first. Nothing
// pre-empts the running entry.
func (q *Queue) insertLocked(e *qentry) {
	rank := func(w Who) int {
		switch w {
		case WhoOwner:
			return 0
		case WhoSchedule:
			return 1
		}
		return 2
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
		e.State, e.Stopped = StateCancelled, StopCancelled // the runner finishes it once the test returns
		if e.cancel != nil {
			e.cancel()
		}
		return q.snapshotLocked(e, now), nil
	}
	for i, e := range q.waiting {
		if e.ID == id {
			q.waiting = slices.Delete(q.waiting, i, i+1)
			q.finishLocked(e, StateCancelled, now)
			return q.snapshotLocked(e, now), nil
		}
	}
	if e, ok := q.finished[id]; ok {
		return q.snapshotLocked(e, now), nil
	}
	return Entry{}, ErrNoEntry
}

// ClearPublic cancels every waiting share-link entry and returns how many.
func (q *Queue) ClearPublic() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	now := q.o.Now()
	n := 0
	q.waiting = slices.DeleteFunc(q.waiting, func(e *qentry) bool {
		if e.Who != WhoPublic {
			return false
		}
		q.finishLocked(e, StateCancelled, now)
		n++
		return true
	})
	return n
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
	q.load(ctx)
	for {
		q.mu.Lock()
		now := q.o.Now()
		q.dropStaleLocked(now)
		var e *qentry
		for i := 0; i < len(q.waiting) && e == nil; {
			e = q.waiting[i]
			if q.gapLocked(e, now) {
				e = nil // a share-link sweep waits out the gap after the last one; others go by
				i++
				continue
			}
			q.waiting = slices.Delete(q.waiting, i, i+1)
			if e.Who == WhoPublic && q.publicLocked(now) != nil {
				e.Stopped = StopLimit
				q.finishLocked(e, StateDropped, now) // turned off, or the budget ran out while it waited
				e = nil
				continue
			}
			e.State = StateRunning
			if e.Started.IsZero() {
				e.Started = now
			}
			q.running = e
		}
		var ectx context.Context
		if e != nil {
			ectx, e.cancel = context.WithCancel(ctx)
		}
		q.mu.Unlock()
		if e != nil {
			q.execute(ectx, e)
			continue
		}
		wait := time.Minute // stale slots are dropped even when idle
		q.mu.Lock()
		if gap := q.sweepEnd.Add(PublicSweepGap).Sub(q.o.Now()); gap > 0 && slices.ContainsFunc(q.waiting, func(w *qentry) bool { return q.gapLocked(w, q.o.Now()) }) {
			wait = min(wait, gap)
		}
		q.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-q.wake:
		case <-time.After(wait):
		}
	}
}

// beforeNextJob, in tests, runs before execute takes the lock for job i
// (to race a Cancel against a sweep's yield).
var beforeNextJob func(id uint64, i int)

// gapLocked reports a share-link sweep that may not start yet: the last
// one ended less than PublicSweepGap ago. A paused sweep resumes anyway.
func (q *Queue) gapLocked(e *qentry, now time.Time) bool {
	return e.Who == WhoPublic && e.Kind == KindSweep && e.next == 0 && !q.sweepEnd.IsZero() && now.Before(q.sweepEnd.Add(PublicSweepGap))
}

func (q *Queue) execute(ctx context.Context, e *qentry) {
	for i := e.next; i < len(e.jobs); i++ {
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
		if e.State == StateCancelled { // cancelled since the check: never re-queue it
			q.mu.Unlock()
			break
		}
		// A public sweep yields between its tests to the owner's entries
		// and schedule slots: it waits again at the head of the share
		// link's entries and resumes here.
		if i > e.next && e.Who == WhoPublic && slices.ContainsFunc(q.waiting, func(w *qentry) bool { return w.Who != WhoPublic }) {
			e.next, e.State = i, StateWaiting
			e.cancel()
			e.cancel = nil
			q.running = nil
			first := slices.IndexFunc(q.waiting, func(w *qentry) bool { return w.Who == WhoPublic })
			if first < 0 {
				first = len(q.waiting)
			}
			q.waiting = slices.Insert(q.waiting, first, e)
			q.mu.Unlock()
			return
		}
		e.Step = i + 1
		q.mu.Unlock()
		var charging chan struct{}
		var charged sync.WaitGroup
		if e.Who == WhoPublic {
			// The line is under test (and probes masked) whether or not the
			// test finishes: charge its time as it runs.
			q.mu.Lock()
			q.pubFrom = q.o.Now()
			q.mu.Unlock()
			charging = make(chan struct{})
			charged.Go(func() {
				t := time.NewTicker(chargeEvery)
				defer t.Stop()
				for {
					select {
					case <-charging:
						return
					case <-t.C:
						q.spend(0, false)
					}
				}
			})
		}
		id, saved, bytes := q.o.Exec(ctx, e.jobs[i], trigger(e.Kind, e.Who))
		q.mu.Lock()
		e.Ran = i + 1
		if saved {
			e.Tests = append(e.Tests, id)
			e.JobTests[i] = id
		}
		q.mu.Unlock()
		if e.Who == WhoPublic {
			close(charging)
			charged.Wait()
			q.spend(bytes, true)
			q.mu.Lock()
			stop := q.publicLocked(q.o.Now()) != nil && i+1 < len(e.jobs)
			if stop {
				e.Stopped = StopLimit
			}
			q.mu.Unlock()
			if stop {
				break // a sweep stops when the budget runs out (or the owner turned it off)
			}
		}
	}
	if e.Kind == KindSweep && e.Who == WhoPublic {
		defer q.save() // after the unlock: sweepEnd survives a restart
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
	if e.Kind == KindSweep && e.Who == WhoPublic {
		q.sweepEnd = now
	}
	if cancel != nil {
		cancel()
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
	// PublicWaiting and PublicSweeps count the share link's waiting
	// entries; Full and SweepsFull say whether it may add more.
	PublicWaiting, PublicSweeps int
	Full, SweepsFull            bool
	// Public: the owner lets the share link queue tests; the budget's
	// limit, use in the last 24 hours, and (when spent) when it resets.
	Public                  bool
	BudgetLimit, BudgetUsed int64
	BudgetResets            time.Time
	// The share link's test time in the last hour and day (caps
	// PublicTimePerHour, PublicTimePerDay), and when a capped one frees up;
	// when its next sweep may start (zero: now).
	TimeHour, TimeDay time.Duration
	TimeResets        time.Time
	SweepAt           time.Time
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
		if e.Who == WhoPublic {
			st.PublicWaiting++
			if e.Kind == KindSweep {
				st.PublicSweeps++
			}
		}
	}
	st.Full = st.PublicWaiting >= MaxPublicWaiting
	st.SweepsFull = st.PublicSweeps >= MaxPublicSweeps
	st.Public = q.public
	st.BudgetLimit, st.BudgetUsed, st.BudgetResets = q.budgetLocked(now)
	st.TimeHour, st.TimeDay, st.TimeResets = q.timeLocked(now)
	if !q.sweepEnd.IsZero() && now.Before(q.sweepEnd.Add(PublicSweepGap)) {
		st.SweepAt = q.sweepEnd.Add(PublicSweepGap)
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
		if q.gapLocked(e, now) {
			out.Held = true
			if g := q.sweepEnd.Add(PublicSweepGap); g.After(at) {
				at = g
			}
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
