package storage

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/s3test"
	"github.com/fuck-you-isp/fyisp/internal/speedtest"
)

// Campaigns: rounds every Interval against a campaign's targets, on demand
// (from a time, a number of rounds or for a while) or recurring (inside a
// weekly window, in the owner's zone, until a date). Each round waits its
// turn in the speed-test queue (never pre-empting a test) and is stored
// with every attempt. A round that can't run is stored as skipped, and
// the time fyisp wasn't running as a gap, so the grid shows them.

// Campaign limits.
const (
	MinInterval     = time.Minute
	MaxInterval     = 24 * time.Hour
	MaxRounds       = 1000               // an on-demand campaign by count
	MaxDuration     = 7 * 24 * time.Hour // ... or by duration
	MaxCampaignTgts = s3test.MaxTargets
	// One round now (RunOnce): about 80 MiB per target with keys.
	DefaultCold, DefaultWarm = 10, 5
	DefaultDown, DefaultUp   = 64 << 20, 16 << 20
	slowStall                = 2 * time.Second // a transfer that stalled this long makes its round slow
	storeTimeout             = 10 * time.Second
	settingInstall           = "storage_install"
)

// Store is what campaigns keep in the database (store.SQLite).
type Store interface {
	SaveStorageCampaign(ctx context.Context, c *model.StorageCampaign) error
	StorageCampaign(ctx context.Context, id int64) (model.StorageCampaign, bool, error)
	StorageCampaigns(ctx context.Context, before int64, limit int, live bool) ([]model.StorageCampaign, error)
	SaveStorageRound(ctx context.Context, r *model.StorageRound, attempts []model.StorageAttempt) error
	StorageRoundsTail(ctx context.Context, campaign int64) (maxSeq int, last time.Time, err error)
	AddStorageLeftovers(ctx context.Context, target string, objects []string, at time.Time) error
	ClearStorageLeftovers(ctx context.Context, target string) error
	Setting(ctx context.Context, key string) (string, bool, error)
	SetSetting(ctx context.Context, key, value string) error
}

// CampaignOptions configure Campaigns.
type CampaignOptions struct {
	Store   Store
	Queue   *speedtest.Queue
	Engine  *s3test.Engine
	Targets *Service
	Now     func() time.Time
	Log     *slog.Logger
}

// CampaignSpec is a new campaign as asked for.
type CampaignSpec struct {
	Targets    []string
	Shape      string // model.StorageOnDemand or model.StorageRecurring
	Interval   time.Duration
	Cold, Warm int
	Down, Up   int64
	// On demand: the first round (zero: now), then Count rounds or for
	// Duration.
	Start    time.Time
	Count    int
	Duration time.Duration
	// Recurring: weekdays (bit 0 Monday ... bit 6 Sunday), the window in
	// minutes after midnight (an end at or before the start crosses
	// midnight), its IANA zone, and the last moment (zero: until removed).
	Days    uint8
	FromMin int
	ToMin   int
	TZ      string
	Until   time.Time
}

// CampaignError is a refused campaign: the API's 400 or 409.
type CampaignError struct {
	Code    string // bad_request, interval_too_short, keys_refused, active_campaign, schedule_exists, not_waiting, bad_state
	Field   string
	Msg     string
	Targets []string      // keys_refused: whose keys
	Min     time.Duration // interval_too_short: the shortest interval allowed
}

func (e *CampaignError) Error() string { return e.Msg }

func cerr(code, field, format string, a ...any) error {
	return &CampaignError{Code: code, Field: field, Msg: fmt.Sprintf(format, a...)}
}

// ErrNoCampaign is a campaign that doesn't exist (or has ended, for the
// controls).
var ErrNoCampaign = errors.New("no such campaign")

// run is a live campaign's state.
type run struct {
	c       model.StorageCampaign
	loc     *time.Location  // recurring
	seq     int             // the next round's number
	next    time.Time       // the next round due (zero: none left)
	entry   uint64          // its round waiting or running in the queue (0: none)
	owner   bool            // its next round goes first (RunOnce)
	swept   map[string]bool // targets swept since fyisp started
	lastDue time.Time
}

// Campaigns runs the campaigns. Its methods are safe for concurrent use.
type Campaigns struct {
	o     CampaignOptions
	wake  chan struct{}
	inUse atomic.Pointer[map[string]int64]
	// cur is what the running round does now (one runs at a time). The
	// queue reads it under its own lock, so never through mu.
	cur atomic.Pointer[livePhase]

	checking sync.Mutex // one key check at a time (ErrBusy)

	mu      sync.Mutex
	live    map[int64]*run
	install string
	rev     int64
	notices []Notice
}

type livePhase struct {
	campaign      int64
	phase, target string
}

// Notice is something the owner should hear about once: a schedule that
// ended, or a restart that lost an on-demand campaign's keys.
type Notice struct {
	Code     string // "schedule_ended", "keys_lost"
	At       time.Time
	Campaign int64
	Targets  []string
}

// NewCampaigns returns Campaigns; Run drives them.
func NewCampaigns(o CampaignOptions) *Campaigns {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	c := &Campaigns{o: o, wake: make(chan struct{}, 1), live: map[int64]*run{}, rev: o.Now().UnixMilli()}
	empty := map[string]int64{}
	c.inUse.Store(&empty)
	o.Targets.SetInUse(c.InUse)
	return c
}

func (c *Campaigns) poke() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// InUse is the live campaign that uses target (0: none). It never takes
// the campaigns' lock (the targets service calls it under its own).
func (c *Campaigns) InUse(target string) int64 { return (*c.inUse.Load())[target] }

// changedLocked publishes the targets in use and bumps rev.
func (c *Campaigns) changedLocked() {
	c.publishLocked(nil)
	c.rev++
	c.poke()
}

// publishLocked publishes the live campaigns' targets in use, and extra
// ones (a campaign starting: id -1).
func (c *Campaigns) publishLocked(extra []model.StorageCampaignTarget) {
	m := map[string]int64{}
	for _, t := range extra {
		m[t.ID] = -1
	}
	for id, r := range c.live {
		for _, t := range r.c.Targets {
			m[t.ID] = id
		}
	}
	c.inUse.Store(&m)
}

// Rev increases with every change the page should redraw for.
func (c *Campaigns) Rev() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rev
}

func storeCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), storeTimeout)
}

// ---------- the schedule ----------

// weekday is d's bit in Days (Monday 0 ... Sunday 6).
func weekday(d time.Time) uint8 { return 1 << ((int(d.Weekday()) + 6) % 7) }

// window is the recurring window that starts on day (any time that day,
// in loc): from FromMin on it to ToMin, the next day when it crosses
// midnight. Wall-clock times: a window keeps its hours across DST.
func window(c *model.StorageCampaign, loc *time.Location, day time.Time) (from, to time.Time) {
	y, m, d := day.In(loc).Date()
	from = time.Date(y, m, d, c.FromMin/60, c.FromMin%60, 0, 0, loc)
	if c.ToMin <= c.FromMin {
		d++
	}
	return from, time.Date(y, m, d, c.ToMin/60, c.ToMin%60, 0, 0, loc)
}

// nextSlot is the first round due at or after t (false: none left).
func (r *run) nextSlot(t time.Time) (time.Time, bool) {
	c := &r.c
	if c.Shape == model.StorageOnDemand {
		k := 0
		if t.After(c.Start) {
			k = int((t.Sub(c.Start) + c.Interval - 1) / c.Interval)
		}
		at := c.Start.Add(time.Duration(k) * c.Interval)
		if (c.Count > 0 && k >= c.Count) || (!c.End.IsZero() && !at.Before(c.End)) {
			return time.Time{}, false
		}
		return at, true
	}
	for i := -1; i <= 8; i++ {
		day := t.In(r.loc).AddDate(0, 0, i)
		if c.Days&weekday(day) == 0 {
			continue
		}
		from, to := window(c, r.loc, day)
		if !to.After(t) {
			continue
		}
		at := from
		if t.After(from) {
			at = from.Add((t.Sub(from) + c.Interval - 1) / c.Interval * c.Interval)
		}
		if !at.Before(to) {
			continue
		}
		if !c.Until.IsZero() && !at.Before(c.Until) {
			return time.Time{}, false
		}
		return at, true
	}
	return time.Time{}, false
}

// inWindow reports a recurring campaign inside one of its windows at t.
func (r *run) inWindow(t time.Time) bool {
	if r.c.Shape != model.StorageRecurring {
		return false
	}
	for i := -1; i <= 0; i++ {
		day := t.In(r.loc).AddDate(0, 0, i)
		if r.c.Days&weekday(day) == 0 {
			continue
		}
		if from, to := window(&r.c, r.loc, day); !t.Before(from) && t.Before(to) {
			return true
		}
	}
	return false
}

// ---------- starting ----------

// validate checks a spec and returns the campaign it makes, with its
// targets snapshotted (mode bucket for the ones given keys).
func (c *Campaigns) validate(s CampaignSpec, keys map[string]*s3test.Creds, now time.Time) (model.StorageCampaign, *time.Location, error) {
	mc := model.StorageCampaign{Shape: s.Shape, Cold: s.Cold, Warm: s.Warm, Down: s.Down, Up: s.Up, Interval: s.Interval,
		Created: now, Updated: now}
	switch {
	case len(s.Targets) == 0 || len(s.Targets) > MaxCampaignTgts:
		return mc, nil, cerr("bad_request", "targets", "a campaign has 1 to %d targets", MaxCampaignTgts)
	case s.Interval < MinInterval || s.Interval > MaxInterval || s.Interval%time.Second != 0:
		return mc, nil, cerr("bad_request", "interval_s", "the interval is 60 s to 24 h")
	case s.Cold < 0 || s.Cold > s3test.MaxCold:
		return mc, nil, cerr("bad_request", "cold", "0 to %d fresh connections", s3test.MaxCold)
	case s.Warm < 0 || s.Warm > s3test.MaxWarm:
		return mc, nil, cerr("bad_request", "warm", "0 to %d reused requests", s3test.MaxWarm)
	case s.Down < 0 || s.Down > s3test.MaxTransfer:
		return mc, nil, cerr("bad_request", "down_bytes", "a download of 0 to 1 GiB")
	case s.Up < 0 || s.Up > s3test.MaxTransfer:
		return mc, nil, cerr("bad_request", "up_bytes", "an upload of 0 to 1 GiB")
	}
	for id := range keys {
		if !slices.Contains(s.Targets, id) {
			return mc, nil, cerr("bad_request", "keys", "keys for a target that isn't in the campaign")
		}
	}
	keyed := 0
	for _, id := range s.Targets {
		if slices.ContainsFunc(mc.Targets, func(t model.StorageCampaignTarget) bool { return t.ID == id }) {
			return mc, nil, cerr("bad_request", "targets", "a target is listed twice")
		}
		ts, err := c.o.Targets.Target(id)
		if err != nil {
			return mc, nil, cerr("bad_request", "targets", "no such target")
		}
		mode := "conn"
		if k := keys[id]; k != nil {
			if ts.Bucket == "" {
				return mc, nil, cerr("bad_request", "keys", "%s has no bucket: it takes no keys", ts.Name)
			}
			if k.HasToken() && ts.Scheme() != "https" {
				return mc, nil, cerr("bad_request", "keys", "a session token is only sent over https")
			}
			mode = "bucket"
			keyed++
		}
		mc.Targets = append(mc.Targets, model.StorageCampaignTarget{ID: id, Name: ts.Name, Mode: mode})
	}
	if s.Cold+s.Warm == 0 && (keyed == 0 || s.Down+s.Up == 0) {
		return mc, nil, cerr("bad_request", "cold", "a round needs at least one request")
	}
	if min := c.MinInterval(len(s.Targets), keyed, s3test.Phases{Cold: s.Cold, Warm: s.Warm, Down: s.Down, Up: s.Up}); s.Interval < min {
		return mc, nil, &CampaignError{Code: "interval_too_short", Field: "interval_s", Min: min,
			Msg: fmt.Sprintf("a round of these takes up to %s: the interval must be at least that", min)}
	}
	var loc *time.Location
	switch s.Shape {
	case model.StorageOnDemand:
		mc.Start = s.Start
		if mc.Start.IsZero() || mc.Start.Before(now) {
			mc.Start = now
		}
		switch {
		case mc.Start.After(now.Add(30 * 24 * time.Hour)):
			return mc, nil, cerr("bad_request", "start_at", "start within 30 days")
		case (s.Count > 0) == (s.Duration > 0):
			return mc, nil, cerr("bad_request", "count", "give a number of rounds or a duration")
		case s.Count > MaxRounds:
			return mc, nil, cerr("bad_request", "count", "at most %d rounds", MaxRounds)
		case s.Duration > MaxDuration:
			return mc, nil, cerr("bad_request", "duration_s", "at most 7 days")
		case s.Duration > 0 && s.Duration < s.Interval:
			return mc, nil, cerr("bad_request", "duration_s", "the duration is at least one interval")
		}
		mc.Count = s.Count
		if s.Duration > 0 {
			mc.End = mc.Start.Add(s.Duration)
		}
		mc.State = model.StorageRunning
		if mc.Start.After(now) {
			mc.State = model.StorageScheduled
		}
	case model.StorageRecurring:
		var err error
		switch {
		case s.Days == 0 || s.Days >= 1<<7:
			return mc, nil, cerr("bad_request", "days", "pick at least one weekday")
		case s.FromMin < 0 || s.FromMin >= 1440 || s.ToMin < 0 || s.ToMin >= 1440 || s.FromMin == s.ToMin:
			return mc, nil, cerr("bad_request", "from", "a window from one time of day to another")
		case s.TZ == "" || s.TZ == "Local":
			return mc, nil, cerr("bad_request", "tz", "the time zone is an IANA name, e.g. America/New_York")
		case !s.Until.IsZero() && !s.Until.After(now):
			return mc, nil, cerr("bad_request", "until", "the end is in the future")
		}
		if loc, err = time.LoadLocation(s.TZ); err != nil {
			return mc, nil, cerr("bad_request", "tz", "unknown time zone")
		}
		if span := (s.ToMin - s.FromMin + 1440) % 1440; time.Duration(span)*time.Minute < s.Interval {
			return mc, nil, cerr("bad_request", "interval_s", "the window is shorter than one interval")
		}
		mc.Days, mc.FromMin, mc.ToMin, mc.TZ, mc.Until = s.Days, s.FromMin, s.ToMin, s.TZ, s.Until
		mc.State, mc.KeysSaved = model.StorageRunning, keyed > 0
	default:
		return mc, nil, cerr("bad_request", "shape", `"on_demand" or "recurring"`)
	}
	return mc, loc, nil
}

// MinInterval is the shortest interval for rounds of n targets (keyed with
// keys) and phases: at least a minute, and at least the round's estimate,
// rounded up to a minute (a round that runs long makes the next one
// skipped, busy).
func (c *Campaigns) MinInterval(n, keyed int, p s3test.Phases) time.Duration {
	est := s3test.EstimateRound(n, keyed, p, c.recent())
	return max(MinInterval, (est+time.Minute-1)/time.Minute*time.Minute)
}

// recent is the line's recent speeds for the transfers' time bounds:
// unknown (the engine's defaults).
func (c *Campaigns) recent() [2]float64 { return [2]float64{} }

// checkKeys asks each target once with its keys (at the same time) and
// returns the targets whose keys were refused; another key check running
// is ErrBusy. Only a refusal fails: a target that doesn't answer gets its
// keys, and its rounds will say what happens.
func (c *Campaigns) checkKeys(ctx context.Context, keys map[string]*s3test.Creds) ([]string, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	if !c.checking.TryLock() {
		return nil, ErrBusy
	}
	defer c.checking.Unlock()
	var mu sync.Mutex
	var refused []string
	var wg sync.WaitGroup
	for id, k := range keys {
		ts, err := c.o.Targets.Target(id)
		if err != nil {
			continue
		}
		et, err := ts.Engine()
		if err != nil {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, CheckTimeout)
			defer cancel()
			if r := c.o.Engine.Check(cctx, s3test.RoundTarget{Target: et, Creds: k}); r.Keys == "refused" {
				mu.Lock()
				refused = append(refused, id)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	slices.Sort(refused)
	return refused, nil
}

// Start starts a campaign with keys for its bucket targets (targets
// without keys get connection checks only), after checking the keys. One
// on-demand campaign and one recurring schedule may be live at a time.
func (c *Campaigns) Start(ctx context.Context, s CampaignSpec, keys map[string]*s3test.Creds) (model.StorageCampaign, error) {
	return c.start(ctx, s, keys, false)
}

// RunOnce runs one round now (first in the queue): an on-demand campaign
// of one round with the default phases.
func (c *Campaigns) RunOnce(ctx context.Context, targets []string, keys map[string]*s3test.Creds) (model.StorageCampaign, error) {
	s := CampaignSpec{Targets: targets, Shape: model.StorageOnDemand, Count: 1, Cold: DefaultCold, Warm: DefaultWarm}
	if len(keys) > 0 {
		s.Down, s.Up = DefaultDown, DefaultUp
	}
	keyed := 0
	for range keys {
		keyed++
	}
	s.Interval = c.MinInterval(len(targets), keyed, s3test.Phases{Cold: s.Cold, Warm: s.Warm, Down: s.Down, Up: s.Up})
	return c.start(ctx, s, keys, true)
}

func (c *Campaigns) start(ctx context.Context, s CampaignSpec, keys map[string]*s3test.Creds, owner bool) (model.StorageCampaign, error) {
	now := c.o.Now()
	mc, loc, err := c.validate(s, keys, now)
	if err != nil {
		return mc, err
	}
	if err := c.conflict(mc, now); err != nil {
		return mc, err
	}
	if refused, err := c.checkKeys(ctx, keys); err != nil {
		return mc, err
	} else if len(refused) > 0 {
		return mc, &CampaignError{Code: "keys_refused", Field: "keys", Targets: refused, Msg: "the keys were refused"}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.conflictLocked(mc, now); err != nil { // checked again: the keys took a while
		return mc, err
	}
	// The targets are in use from now (Delete refuses them), then checked
	// again: one deleted meanwhile fails the start.
	c.publishLocked(mc.Targets)
	for _, t := range mc.Targets {
		if _, err := c.o.Targets.Target(t.ID); err != nil {
			c.changedLocked()
			return mc, cerr("bad_request", "targets", "a target was deleted meanwhile")
		}
	}
	sctx, cancel := storeCtx(ctx)
	defer cancel()
	if err := c.o.Store.SaveStorageCampaign(sctx, &mc); err != nil {
		return mc, err
	}
	if len(keys) > 0 {
		if mc.Shape == model.StorageRecurring {
			err = c.o.Targets.SaveKeys(mc.ID, keys)
		} else {
			err = c.o.Targets.HoldKeys(mc.ID, keys)
		}
		if err != nil {
			mc.State, mc.Stopped, mc.End, mc.KeysSaved = model.StorageStopped, model.StopOwner, now, false
			_ = c.o.Store.SaveStorageCampaign(sctx, &mc)
			return mc, err
		}
	}
	r := &run{c: mc, loc: loc, owner: owner, swept: map[string]bool{}}
	r.next, _ = r.nextSlot(now)
	c.live[mc.ID] = r
	c.changedLocked()
	return mc, nil
}

func (c *Campaigns) conflict(mc model.StorageCampaign, now time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conflictLocked(mc, now)
}

// conflictLocked refuses a second on-demand campaign, a second recurring
// schedule, and an on-demand campaign starting inside the schedule's
// window.
func (c *Campaigns) conflictLocked(mc model.StorageCampaign, now time.Time) error {
	for _, r := range c.live {
		switch {
		case r.c.Shape == mc.Shape && mc.Shape == model.StorageRecurring:
			return &CampaignError{Code: "schedule_exists", Msg: "a recurring schedule exists: remove it first"}
		case r.c.Shape == mc.Shape:
			return &CampaignError{Code: "active_campaign", Msg: "another campaign is live: stop it first"}
		case r.c.Shape == model.StorageRecurring && r.inWindow(mc.Start):
			return &CampaignError{Code: "active_campaign", Msg: "the recurring schedule runs then"}
		}
	}
	return nil
}

// ---------- the run loop ----------

// Run handles a restart (ended schedules, gaps, lost keys), then fires
// each campaign's rounds as they fall due until ctx ends.
func (c *Campaigns) Run(ctx context.Context) {
	c.restart(ctx)
	for {
		now := c.o.Now()
		var due []*run
		var next time.Time
		c.mu.Lock()
		for _, r := range c.live {
			if !r.c.Until.IsZero() && !now.Before(r.c.Until) && r.entry == 0 {
				c.endLocked(ctx, r, model.StorageDone, model.StopUntil, now)
				continue
			}
			if r.next.IsZero() {
				if r.entry == 0 && r.c.Shape == model.StorageOnDemand {
					c.endLocked(ctx, r, model.StorageDone, model.StopFinished, now)
				}
				continue
			}
			if !r.next.After(now) {
				due = append(due, r)
			} else if next.IsZero() || r.next.Before(next) {
				next = r.next
			}
			if !r.c.Until.IsZero() && (next.IsZero() || r.c.Until.Before(next)) {
				next = r.c.Until
			}
		}
		for _, r := range due {
			c.fireLocked(ctx, r, now)
			if !r.next.IsZero() && (next.IsZero() || r.next.Before(next)) {
				next = r.next
			}
		}
		c.mu.Unlock()
		wait := time.Minute // a clock step is noticed within a minute
		if !next.IsZero() {
			wait = min(wait, max(next.Sub(now), 0))
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			c.shutdown()
			return
		case <-c.wake:
		case <-t.C:
		}
		t.Stop()
	}
}

// fireLocked handles r's due round: skipped (paused, another campaign's
// round, its own still waiting) or queued.
func (c *Campaigns) fireLocked(ctx context.Context, r *run, now time.Time) {
	due, seq := r.next, r.seq
	if r.c.Shape == model.StorageOnDemand {
		seq = int(due.Sub(r.c.Start) / r.c.Interval) // its slot: missed ones leave their numbers out
	}
	r.seq = seq + 1
	r.lastDue = due
	r.next, _ = r.nextSlot(due.Add(time.Nanosecond))
	if !r.next.IsZero() && r.next.Before(now) {
		// The clock jumped (or the machine slept): one gap, no burst.
		gap := &model.StorageRound{Campaign: r.c.ID, Due: due, End: now, Outcome: model.RoundGap}
		sctx, cancel := storeCtx(ctx)
		if err := c.o.Store.SaveStorageRound(sctx, gap, nil); err != nil {
			c.o.Log.Warn("storage tests: saving a gap", "err", err)
		}
		cancel()
		r.next, _ = r.nextSlot(now)
		if r.c.Shape == model.StorageRecurring && !r.next.IsZero() {
			r.seq++ // the gap's numbers are left out
		}
	}
	if r.c.State == model.StorageScheduled {
		r.c.State = model.StorageRunning
		r.c.Updated = now
		c.saveCampaignLocked(ctx, r)
	}
	skip := ""
	switch {
	case r.c.State == model.StoragePaused:
		skip = model.SkipPaused
	case r.entry != 0:
		skip = model.SkipBusy
	case c.otherRoundLocked(r):
		skip = model.SkipOtherCampaign
	}
	if skip != "" {
		c.saveSkipLocked(ctx, r.c.ID, seq, due, skip)
		return
	}
	id := r.c.ID
	est := s3test.EstimateRound(len(r.c.Targets), r.keyedTargets(), r.phases(), c.recent())
	var entry atomic.Uint64
	j := speedtest.RoundJob(strconv.FormatInt(id, 10), c.label(r), est,
		func(ctx context.Context) (int64, bool) { return c.runRound(ctx, id, seq, due) },
		func() (string, string) { return c.live1(id) },
		// Dropped (stale) or cancelled in the queue before it ran: the
		// queue may call this while Stop holds mu, so it waits apart.
		func(stale bool) { go c.unrun(ctx, id, entry.Load(), seq, due, stale) })
	e, added := c.o.Queue.EnqueueRound(j, r.owner)
	entry.Store(e.ID)
	r.owner = false
	if !added {
		c.saveSkipLocked(ctx, id, seq, due, model.SkipBusy)
		return
	}
	r.entry = e.ID
	c.changedLocked()
}

// unrun records a round that never ran: skipped stale, or cancelled.
func (c *Campaigns) unrun(ctx context.Context, id int64, entry uint64, seq int, due time.Time, stale bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r := c.live[id]
	if r == nil || r.entry != entry {
		return // stopped, or long since replaced
	}
	r.entry = 0
	if stale {
		c.saveSkipLocked(ctx, id, seq, due, model.SkipStale)
		return
	}
	sctx, cancel := storeCtx(ctx)
	defer cancel()
	rr := &model.StorageRound{Campaign: id, Seq: &seq, Due: due, Outcome: model.RoundCancelled}
	if err := c.o.Store.SaveStorageRound(sctx, rr, nil); err != nil {
		c.o.Log.Warn("storage tests: saving a cancelled round", "err", err)
	}
	c.changedLocked()
}

// otherRoundLocked reports another campaign's round waiting or running.
func (c *Campaigns) otherRoundLocked(r *run) bool {
	for _, o := range c.live {
		if o != r && o.entry != 0 {
			return true
		}
	}
	return false
}

func (c *Campaigns) label(r *run) string {
	names := ""
	for i, t := range r.c.Targets {
		if i > 0 {
			names += ", "
		}
		names += t.Name
	}
	return names
}

// live1 is what campaign id's running round does now (lock-free: the
// queue asks under its own lock).
func (c *Campaigns) live1(id int64) (phase, target string) {
	if p := c.cur.Load(); p != nil && p.campaign == id {
		return p.phase, p.target
	}
	return "", ""
}

// Live is what the running round does now: its campaign (0: none), phase
// and target.
func (c *Campaigns) Live() (campaign int64, phase, target string) {
	if p := c.cur.Load(); p != nil {
		return p.campaign, p.phase, p.target
	}
	return 0, "", ""
}

func (r *run) phases() s3test.Phases {
	return s3test.Phases{Cold: r.c.Cold, Warm: r.c.Warm, Down: r.c.Down, Up: r.c.Up}
}

func (r *run) keyedTargets() int {
	n := 0
	for _, t := range r.c.Targets {
		if t.Mode == "bucket" {
			n++
		}
	}
	return n
}

func (c *Campaigns) saveSkipLocked(ctx context.Context, id int64, seq int, due time.Time, why string) {
	sctx, cancel := storeCtx(ctx)
	defer cancel()
	rr := &model.StorageRound{Campaign: id, Seq: &seq, Due: due, Outcome: model.RoundSkipped, SkipReason: why}
	if err := c.o.Store.SaveStorageRound(sctx, rr, nil); err != nil {
		c.o.Log.Warn("storage tests: saving a skipped round", "err", err)
	}
	c.changedLocked()
}

func (c *Campaigns) saveCampaignLocked(ctx context.Context, r *run) {
	sctx, cancel := storeCtx(ctx)
	defer cancel()
	if err := c.o.Store.SaveStorageCampaign(sctx, &r.c); err != nil {
		c.o.Log.Warn("storage tests: saving a campaign", "err", err)
	}
}

// runRound runs campaign id's round seq (the queue's turn): its targets
// with the keys held for them now (keys may arrive while it waits), each
// keyed target's leftovers swept first, then stored.
func (c *Campaigns) runRound(ctx context.Context, id int64, seq int, due time.Time) (int64, bool) {
	c.mu.Lock()
	r := c.live[id]
	if r == nil {
		c.mu.Unlock()
		return 0, false
	}
	mc := r.c
	install := c.install
	if mc.State == model.StoragePaused { // paused while it waited
		c.saveSkipLocked(ctx, id, seq, due, model.SkipPaused)
		c.mu.Unlock()
		return 0, false
	}
	c.mu.Unlock()
	c.cur.Store(&livePhase{campaign: id})
	defer func() {
		c.cur.Store(nil)
		c.mu.Lock()
		if r := c.live[id]; r != nil {
			r.entry = 0
			c.changedLocked()
		}
		c.mu.Unlock()
	}()

	spec := s3test.RoundSpec{Install: install, Campaign: id, Round: seq, Phases: r.phases(), RecentBps: c.recent(),
		OnPhase: func(phase, target string) {
			c.cur.Store(&livePhase{campaign: id, phase: phase, target: target})
			c.mu.Lock()
			c.rev++
			c.mu.Unlock()
		}}
	skips := map[string]string{}
	var creds []*s3test.Creds
	defer func() {
		for _, k := range creds {
			k.Zero()
		}
	}()
	for _, t := range mc.Targets {
		ts, err := c.o.Targets.Target(t.ID)
		if err != nil {
			skips[t.ID] = "config"
			continue
		}
		et, err := ts.Engine()
		if err != nil {
			skips[t.ID] = "config"
			continue
		}
		rt := s3test.RoundTarget{Target: et}
		if t.Mode == "bucket" {
			if k, ok := c.o.Targets.Keys(t.ID, id); ok {
				rt.Creds = k
				creds = append(creds, k)
			} else {
				skips[t.ID] = model.SkipNoKeys // connection checks only, this round
			}
		}
		spec.Targets = append(spec.Targets, rt)
	}
	if len(spec.Targets) == 0 || (spec.Phases.Cold+spec.Phases.Warm == 0 && len(creds) == 0) {
		c.mu.Lock()
		c.saveSkipLocked(ctx, id, seq, due, model.SkipNoKeys)
		c.mu.Unlock()
		return 0, false
	}
	for _, rt := range spec.Targets {
		if rt.Creds == nil {
			continue
		}
		c.mu.Lock()
		swept := r.swept[rt.ID]
		c.mu.Unlock()
		if !swept && c.sweep(ctx, rt, install) {
			c.mu.Lock()
			r.swept[rt.ID] = true
			c.mu.Unlock()
		}
	}
	res := c.o.Engine.RunRound(ctx, spec)
	for k, v := range res.Skips {
		skips[k] = v
	}
	rr := &model.StorageRound{Campaign: id, Seq: &seq, Due: due, Start: res.Start, End: res.End, Skips: skips,
		Outcome: outcome(res), BytesDown: res.Down, BytesUp: res.Up, Wire: res.Down + res.Up}
	if res.Start.After(due) {
		rr.QueueWait = res.Start.Sub(due)
	}
	sctx, cancel := storeCtx(ctx)
	defer cancel()
	if err := c.o.Store.SaveStorageRound(sctx, rr, res.Attempts); err != nil {
		c.o.Log.Warn("storage tests: saving a round", "err", err)
		return 0, false
	}
	byTarget := map[string][]string{}
	for _, l := range res.Leftovers { // "<campaign>/<round>-<target>"
		for _, t := range mc.Targets {
			if strings.HasSuffix(l, "-"+t.ID) {
				byTarget[t.ID] = append(byTarget[t.ID], l)
			}
		}
	}
	for t, objs := range byTarget {
		if err := c.o.Store.AddStorageLeftovers(sctx, t, objs, res.End); err != nil {
			c.o.Log.Warn("storage tests: saving leftovers", "err", err)
		}
	}
	c.mu.Lock()
	if r := c.live[id]; r != nil {
		r.c.RoundsDone++
		r.c.BytesDown += res.Down
		r.c.BytesUp += res.Up
	}
	c.mu.Unlock()
	return rr.ID, true
}

// sweep deletes what fyisp left under the install's prefix in a target's
// bucket (a crash's uploads), keeping the seeds, and reports whether it
// worked; then the target's leftovers are forgotten. It runs before the
// round, bounded on its own (one listing of at most 1000 objects).
func (c *Campaigns) sweep(ctx context.Context, rt s3test.RoundTarget, install string) bool {
	sctx, cancel := context.WithTimeout(ctx, s3test.RoundCap)
	defer cancel()
	if _, class := c.o.Engine.Sweep(sctx, rt, install, true); class != s3test.ClassOK {
		return false
	}
	stx, cancel2 := storeCtx(ctx)
	defer cancel2()
	if err := c.o.Store.ClearStorageLeftovers(stx, rt.ID); err != nil {
		c.o.Log.Warn("storage tests: clearing leftovers", "err", err)
	}
	return true
}

// outcome is a round's result: cancelled, failed (a network or provider
// failure), config (only settings, keys or clock), slow (a transfer
// stalled), or ok.
func outcome(res s3test.RoundResult) string {
	if res.Cancelled {
		return model.RoundCancelled
	}
	out := model.RoundOK
	for _, a := range res.Attempts {
		switch s3test.Class(a.Class).Group() {
		case s3test.GroupNetwork, s3test.GroupProvider:
			return model.RoundFailed
		case s3test.GroupConfig:
			out = model.RoundConfig
		}
		if out == model.RoundOK && a.Stallms != nil && *a.Stallms >= float64(slowStall/time.Millisecond) {
			out = model.RoundSlow
		}
	}
	for _, why := range res.Skips {
		if why != "round_cap" && out != model.RoundFailed {
			out = model.RoundConfig
		}
	}
	return out
}

// ---------- controls ----------

func (c *Campaigns) get(id int64) (*run, error) {
	r := c.live[id]
	if r == nil {
		return nil, ErrNoCampaign
	}
	return r, nil
}

// Stop ends a campaign now: its waiting or running round is cancelled
// and its keys deleted. A recurring schedule is removed.
func (c *Campaigns) Stop(ctx context.Context, id int64) (model.StorageCampaign, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, err := c.get(id)
	if err != nil {
		return model.StorageCampaign{}, err
	}
	why := model.StopOwner
	if r.c.Shape == model.StorageRecurring {
		why = model.StopRemoved
	}
	c.endLocked(ctx, r, model.StorageStopped, why, c.o.Now())
	return r.c, nil
}

// Pause skips a campaign's rounds (stored as skipped) until Resume; a
// running round finishes.
func (c *Campaigns) Pause(ctx context.Context, id int64) (model.StorageCampaign, error) {
	return c.setPaused(ctx, id, true)
}

// Resume undoes Pause.
func (c *Campaigns) Resume(ctx context.Context, id int64) (model.StorageCampaign, error) {
	return c.setPaused(ctx, id, false)
}

func (c *Campaigns) setPaused(ctx context.Context, id int64, on bool) (model.StorageCampaign, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, err := c.get(id)
	if err != nil {
		return model.StorageCampaign{}, err
	}
	switch {
	case on && r.c.State != model.StorageRunning && r.c.State != model.StorageScheduled:
		return r.c, &CampaignError{Code: "bad_state", Msg: "the campaign isn't running"}
	case !on && r.c.State != model.StoragePaused:
		return r.c, &CampaignError{Code: "bad_state", Msg: "the campaign isn't paused"}
	}
	r.c.State = model.StoragePaused
	if !on {
		r.c.State = model.StorageRunning
		if r.c.Start.After(c.o.Now()) {
			r.c.State = model.StorageScheduled
		}
	}
	r.c.Updated = c.o.Now()
	c.saveCampaignLocked(ctx, r)
	c.changedLocked()
	return r.c, nil
}

// ProvideKeys gives a live campaign keys for bucket targets waiting for
// them (after a restart, or a change to a target's settings), after
// checking them.
func (c *Campaigns) ProvideKeys(ctx context.Context, id int64, keys map[string]*s3test.Creds) error {
	c.mu.Lock()
	r, err := c.get(id)
	if err != nil {
		c.mu.Unlock()
		return err
	}
	waiting := c.waitingLocked(r)
	shape := r.c.Shape
	c.mu.Unlock()
	if len(keys) == 0 {
		return cerr("bad_request", "keys", "no keys")
	}
	for t := range keys {
		if !slices.Contains(waiting, t) {
			return &CampaignError{Code: "not_waiting", Msg: "that target isn't waiting for keys"}
		}
	}
	if refused, err := c.checkKeys(ctx, keys); err != nil {
		return err
	} else if len(refused) > 0 {
		return &CampaignError{Code: "keys_refused", Field: "keys", Targets: refused, Msg: "the keys were refused"}
	}
	if shape == model.StorageRecurring {
		err = c.o.Targets.SaveKeys(id, keys)
	} else {
		err = c.o.Targets.HoldKeys(id, keys)
	}
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if r := c.live[id]; r != nil && shape == model.StorageRecurring && !r.c.KeysSaved {
		r.c.KeysSaved = true
		c.saveCampaignLocked(ctx, r)
	}
	c.changedLocked()
	return nil
}

// waitingLocked is r's bucket targets with no keys held.
func (c *Campaigns) waitingLocked(r *run) []string {
	var out []string
	for _, t := range r.c.Targets {
		if t.Mode != "bucket" {
			continue
		}
		if !c.o.Targets.HasKeys(t.ID, r.c.ID) {
			out = append(out, t.ID)
		}
	}
	return out
}

// endLocked ends r: stored with state and why, its round cancelled, its
// keys deleted.
func (c *Campaigns) endLocked(ctx context.Context, r *run, state, why string, now time.Time) {
	if r.entry != 0 {
		_, _ = c.o.Queue.Cancel(r.entry)
	}
	r.c.State, r.c.Stopped, r.c.Updated, r.c.End = state, why, now, now
	r.c.KeysSaved = false
	if err := c.o.Targets.DropKeys(r.c.ID); err != nil {
		c.o.Log.Warn("storage tests: deleting a campaign's keys", "err", err)
	}
	c.saveCampaignLocked(ctx, r)
	if why == model.StopUntil {
		c.notices = append(c.notices, Notice{Code: "schedule_ended", At: now, Campaign: r.c.ID})
	}
	delete(c.live, r.c.ID)
	c.changedLocked()
}

// shutdown drops the on-demand campaigns' keys (memory only).
func (c *Campaigns) shutdown() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, r := range c.live {
		if r.c.Shape == model.StorageOnDemand {
			_ = c.o.Targets.DropKeys(id)
		}
	}
}

// ---------- restart ----------

// restart, before the first round: the install id; schedules whose end
// passed while fyisp was down are ended and their keys deleted; on-demand
// campaigns that ran out are finished; a gap row per campaign for the time
// fyisp wasn't running (when a round fell due in it); saved keys of no
// campaign deleted. A campaign that can't be read is left for the next
// start, keys and all.
func (c *Campaigns) restart(ctx context.Context) {
	now := c.o.Now()
	sctx, cancel := storeCtx(ctx)
	defer cancel()
	c.mu.Lock()
	defer c.mu.Unlock()
	if v, ok, err := c.o.Store.Setting(sctx, settingInstall); err == nil && ok && len(v) == 16 {
		c.install = v
	} else {
		var b [8]byte
		_, _ = rand.Read(b[:])
		c.install = hex.EncodeToString(b[:])
		if err := c.o.Store.SetSetting(sctx, settingInstall, c.install); err != nil {
			c.o.Log.Warn("storage tests: saving the install id", "err", err)
		}
	}
	cs, err := c.o.Store.StorageCampaigns(sctx, 0, 100, true)
	if err != nil {
		c.o.Log.Warn("storage tests: loading the campaigns; none run until fyisp restarts", "err", err)
		return
	}
	keep := map[int64]bool{} // the campaigns whose keys stay
	for _, mc := range cs {
		keep[mc.ID] = true
		r := &run{c: mc, swept: map[string]bool{}}
		if mc.Shape == model.StorageRecurring {
			if r.loc, err = time.LoadLocation(mc.TZ); err != nil {
				r.loc = time.UTC
			}
		}
		maxSeq, last, err := c.o.Store.StorageRoundsTail(sctx, mc.ID)
		if err != nil {
			c.o.Log.Warn("storage tests: loading a campaign's rounds; it runs after the next restart", "campaign", mc.ID, "err", err)
			continue
		}
		r.seq = maxSeq + 1
		for _, t := range []time.Time{mc.Start, mc.Created} {
			if last.IsZero() || t.After(last) && t.Before(now) {
				last = t
			}
		}
		c.live[mc.ID] = r
		// A round fell due while fyisp was down: say so (no back-fill).
		if at, ok := r.nextSlot(last.Add(time.Nanosecond)); ok && at.Before(now) {
			gap := &model.StorageRound{Campaign: mc.ID, Due: last, End: now, Outcome: model.RoundGap}
			if err := c.o.Store.SaveStorageRound(sctx, gap, nil); err != nil {
				c.o.Log.Warn("storage tests: saving a gap", "err", err)
			}
		}
		if _, ok := r.nextSlot(now); (!mc.Until.IsZero() && !now.Before(mc.Until)) || (!ok && mc.Shape == model.StorageOnDemand) {
			why := model.StopFinished
			if !mc.Until.IsZero() && !now.Before(mc.Until) {
				why = model.StopUntil
			}
			c.endLocked(ctx, r, model.StorageDone, why, now)
			delete(keep, mc.ID)
			continue
		}
		r.next, _ = r.nextSlot(now)
		if mc.State == model.StorageScheduled && !mc.Start.After(now) {
			r.c.State = model.StorageRunning
			c.saveCampaignLocked(ctx, r)
		}
		if mc.Shape == model.StorageOnDemand && r.keyedTargets() > 0 {
			c.notices = append(c.notices, Notice{Code: "keys_lost", At: now, Campaign: mc.ID, Targets: c.waitingLocked(r)})
		}
	}
	for _, id := range c.o.Targets.KeyCampaigns() {
		if !keep[id] {
			_ = c.o.Targets.DropKeys(id)
		}
	}
	c.changedLocked()
}

// ---------- for the API ----------

// LiveCampaign is a live campaign as the page shows it.
type LiveCampaign struct {
	model.StorageCampaign
	InWindow    bool      // recurring: inside a window now
	Next        time.Time // the next round due (zero: none)
	Entry       uint64    // its round in the queue (0: none)
	WaitingKeys []string  // bucket targets with no keys held
}

// State lists the live campaigns, oldest first.
func (c *Campaigns) State() []LiveCampaign {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.o.Now()
	var out []LiveCampaign
	for _, r := range c.live {
		out = append(out, LiveCampaign{StorageCampaign: r.c, InWindow: r.inWindow(now), Next: r.next, Entry: r.entry, WaitingKeys: c.waitingLocked(r)})
	}
	slices.SortFunc(out, func(a, b LiveCampaign) int { return int(a.ID - b.ID) })
	return out
}

// Notices are the last day's notices, newest first.
func (c *Campaigns) Notices() []Notice {
	c.mu.Lock()
	defer c.mu.Unlock()
	cut := c.o.Now().Add(-24 * time.Hour)
	c.notices = slices.DeleteFunc(c.notices, func(n Notice) bool { return n.At.Before(cut) })
	out := slices.Clone(c.notices)
	slices.Reverse(out)
	return out
}

// Estimate is what a campaign would move: per round (download, upload
// bytes), the rounds in all (on demand, by count or duration) or per
// window (recurring), how long a round takes and the shortest interval.
type Estimate struct {
	PerRound     [2]int64
	Rounds       int // on demand; per window when recurring
	RoundTime    time.Duration
	MinInterval  time.Duration
	WindowsPer30 float64 // recurring: windows in 30 days
}

// Estimate estimates spec with keyed of its targets given keys.
func (c *Campaigns) Estimate(s CampaignSpec, keyed int) Estimate {
	p := s3test.Phases{Cold: s.Cold, Warm: s.Warm, Down: s.Down, Up: s.Up}
	var e Estimate
	e.PerRound[0], e.PerRound[1] = s3test.DataPerRound(keyed, p)
	e.RoundTime = s3test.EstimateRound(len(s.Targets), keyed, p, c.recent())
	e.MinInterval = c.MinInterval(len(s.Targets), keyed, p)
	if s.Interval <= 0 {
		return e
	}
	switch s.Shape {
	case model.StorageOnDemand:
		e.Rounds = s.Count
		if s.Duration > 0 {
			e.Rounds = int((s.Duration + s.Interval - 1) / s.Interval)
		}
	case model.StorageRecurring:
		span := time.Duration((s.ToMin-s.FromMin+1440)%1440) * time.Minute
		e.Rounds = int((span + s.Interval - 1) / s.Interval)
		e.WindowsPer30 = float64(bitsSet(s.Days)) * 30 / 7
	}
	return e
}

func bitsSet(b uint8) int {
	n := 0
	for ; b != 0; b &= b - 1 {
		n++
	}
	return n
}
