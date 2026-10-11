package speedtest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math/rand/v2"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

// Mode is what the schedule tests.
type Mode string

const (
	ModeOff Mode = "off" // no scheduled tests (Test now and sweeps still work)
	// ModeRegions tests every default region of the catalog (plus opt-in
	// extras) once per interval, one test at a time, spread evenly; each
	// slot rotates the region's providers by day and hour and falls back
	// within the metro, then to a nearby city.
	ModeRegions Mode = "regions"
	// ModeNearby tests the closest server of 3 different providers and 1
	// far region (another continent on a major route), each per interval.
	ModeNearby Mode = "nearby"
	// ModeCustom tests the --speedtest-server list, each per interval.
	ModeCustom Mode = "custom"
)

// DefaultMode is what --speedtest alone switches on. The owner's choice;
// see docs/SPEEDTEST-API.md.
const DefaultMode = ModeRegions

// DefaultInterval is each mode's interval per region or server.
func DefaultInterval(m Mode) time.Duration {
	if m == ModeRegions {
		return time.Hour
	}
	return 15 * time.Minute
}

// Schedule and test limits (see docs/SPEEDTEST-API.md).
const (
	MinInterval = 15 * time.Minute // per region or server; MinOwnInterval when every server is the user's own (LAN)
	// MinOwnInterval is the floor for the user's LAN servers: a test takes
	// about 15 s.
	MinOwnInterval = time.Minute
	Jitter         = time.Minute    // on each cycle's start only: the spacing stays regular
	RepickEvery    = 24 * time.Hour // nearby servers by RTT
	MaxAttempts    = 3              // per server: busy or refused tries another port (not on a single-port server)
	AttemptGap     = 2 * time.Second
	// MinMeasure..MaxMeasure is the adaptive measured time per direction,
	// after an adaptive ramp-up of at most MaxOmit: the time cap (the
	// owner's tests and the schedule have no byte cap).
	MinMeasure = 3 * time.Second
	MaxMeasure = 10 * time.Second
	MaxOmit    = time.Second
	// TimeBackstop: from a direction's data phase start, its streams are
	// cut with an RST whatever the server does.
	TimeBackstop = MaxOmit + MaxMeasure + 2*time.Second
	// DirTimeout bounds one direction, connect to results.
	DirTimeout = TimeBackstop + 5*time.Second
	// SlotBudget: no new attempt or fallback server after this much of a
	// slot; SlotCap is the hard limit of one slot: an attempt started at
	// SlotBudget still has both directions and the pause between them.
	SlotBudget = 25 * time.Second
	SlotCap    = SlotBudget + 2*DirTimeout + relisten
	// SweepUnreachable unreachable servers in a row end a sweep.
	SweepUnreachable = 3
	// EstTestTime is a typical test (both directions, adaptive): the
	// planned share of time under test is slots × EstTestTime / interval.
	EstTestTime = 20 * time.Second
	ShareWarn   = 0.25
	// relisten is the pause before the upload: a stock iperf3 server
	// closes and reopens its listener after every test, and refuses
	// connections in between.
	relisten     = time.Second
	cycleGap     = 40 * time.Second // between a cycle's last slot and the next cycle
	firstDelay   = 2 * time.Minute  // after start or a mode change: idle RTTs first
	pickTimeout  = 2 * time.Second  // one RTT check
	pickConnects = 3
	pickParallel = 8
	nearbyCount  = 3 // providers in mode nearby
)

// ErrMode is a mode this install does not offer, or an unknown region.
var ErrMode = errors.New("unknown speed test mode")

// Store keeps results and settings (*store.SQLite).
type Store interface {
	SaveSpeedTest(ctx context.Context, t *model.SpeedTest) error
	SpeedTests(ctx context.Context, from, to time.Time) ([]model.SpeedTest, error)
	Setting(ctx context.Context, key string) (string, bool, error)
	SetSetting(ctx context.Context, key, value string) error
}

// Settings keys.
const (
	SettingKey   = "speedtest_mode"
	settingExtra = "speedtest_extra"
	settingSeed  = "speedtest_seed"
	// SettingZone is the owner's IANA time zone, as their browser last
	// sent it on a local page: the report's zone.
	SettingZone = "owner_tz"
)

// Options configure a Scheduler.
type Options struct {
	Catalog *Catalog
	Custom  []Server // --speedtest-server
	// Mode at start (--speedtest); "" uses the saved Details choice, else off.
	Mode Mode
	// Interval per region or server (0: the mode's default; at least
	// MinInterval unless every Custom server is the user's own).
	Interval time.Duration
	// Extra regions: opt-in regions in mode regions, far regions in mode
	// nearby (--speedtest=...,add=canada or far=us-west).
	Extra []string
	// Skip is servers never to test (--speedtest-skip), on top of the
	// saved Details choice.
	Skip    []string
	Store   Store
	Windows *Windows // marked for every test (masking)
	Load    *LoadTap // idle latency before each test; nil: none
	// Path returns the gateway and the ISP edge (netinfo); OpenPinger the
	// ICMP pinger (probe.OpenPinger). With both, each direction of a test
	// pings them every LoadEvery (latency under load, see load.go).
	Path       func() (gateway, edge netip.Addr)
	OpenPinger func() (LoadPinger, error)
	// Conn returns the connection type (model.ConnWired, ...) of the
	// interface tests use; nil: unknown.
	Conn func() string
	// Handoff returns where the path to host leaves the user's ISP
	// (HandoffFinder.Handoff); nil: not recorded. HandoffCached is its
	// answer without a traceroute, when it has a recent one. When neither
	// is cached, the traceroute runs before the test's window, bounded
	// (see handoffFor).
	Handoff       func(ctx context.Context, host string) (*model.Handoff, error)
	HandoffCached func(host string) (*model.Handoff, bool)
	// Ping returns the ICMP RTT to a host (probe.PingOnce); nil or failing:
	// TCP connect time to port 80, 443, then the server's first iperf3 port.
	Ping func(ctx context.Context, host string) (time.Duration, error)
	// OnResult is called after every saved test (metrics).
	OnResult func(model.SpeedTest)
	Log    *slog.Logger
	Now    func() time.Time
	// Fake replaces the network (cmd/fyisp-webdev and the web tests).
	Fake *Fake

	// Tests: the iperf3 run, the RTT check, the sleep and the random source.
	run       func(ctx context.Context, c Config) (Result, error)
	loadEvery time.Duration // default LoadEvery
	rtt       func(ctx context.Context, host string, port int) (time.Duration, error)
	sleep     func(ctx context.Context, d time.Duration) bool
	rand      *rand.Rand
	lookup    func(ctx context.Context, host string) ([]netip.Addr, error) // the handoff's resolution (nil: the system resolver)
	handoffIn time.Duration                                                // the handoff traceroute's bound (default handoffBudget)
}

// Fake stands in for the network: Run for the iperf3 run, RTT for the
// server RTT check (nil: the real ones); a nonzero Seed fixes the random
// choices (the install's seed, the jitter).
type Fake struct {
	Run   func(ctx context.Context, c Config) (Result, error)
	RTT   func(ctx context.Context, host string, port int) (time.Duration, error)
	Sleep func(ctx context.Context, d time.Duration) bool // nil: real pauses
	Seed  uint64
}

// target is one slot's or request's test.
type target struct {
	region string // a region: its rotated chain
	server string // one server
	nearby int    // mode nearby: the i-th pick (-1: none)
	why    string // why this row is scheduled
	// at is a scheduled slot's nominal time (on the install's grid, without
	// jitter): its rotation is that hour's, even when it waited in the
	// queue past the hour. Zero: now.
	at time.Time
}

func (t target) row() string {
	if t.region != "" {
		return t.region
	}
	return t.server
}

// Scheduler runs speed tests on a schedule and on demand.
type Scheduler struct {
	o    Options
	wake chan struct{}
	q    *Queue

	mu           sync.Mutex
	mode         Mode
	setByFlag    bool
	extra        []string
	seed         uint32
	cycleBase    time.Time // the cycle's start on the install's grid, without jitter
	cycleStart   time.Time
	skipBefore   time.Time // after an early test: grid slots before this are skipped
	slot         int
	next         time.Time            // the next slot; zero when off
	cur          *live                // the test running: its direction and server
	skip         map[string]bool      // the user's saved skip list
	busyRun      map[string]int       // scheduled tests in a row that found the server busy
	sweepUnreach int                  // a sweep's unreachable servers in a row (see pause)
	rest         map[string]time.Time // the busy backoff: resting until
	picked       []string             // mode nearby: the picked servers, by ID
	far          string               // mode nearby: the automatic far region
	pickedAt     time.Time
	rtts         map[string]time.Duration
	vias         map[string]model.Handoff // server -> latest handoff with a Via
	zone         string                   // the owner's IANA zone ("": unknown)
}

// Row is a scheduled row of the current mode (a region or a server).
type Row struct {
	ID      string
	Kind    string // region or server
	Label   string
	Why     string   // e.g. "nearest Clouvider", "far: US East", "added"
	Servers []string // regions: the metro's servers, then the nearby ones
}

// ServerState is one server for the API.
type ServerState struct {
	Server
	RTTms  *float64
	Picked bool
	// Skipped: the user skipped it (SkipLocked: by --speedtest-skip, so the
	// toggle can't undo it); RestUntil: the busy backoff (zero: none).
	Skipped, SkipLocked bool
	RestUntil           time.Time
}

// Plan is one mode's load: tests per hour and the share of time under test.
type Plan struct {
	Mode         Mode
	TestsPerHour float64
	Share        float64
}

// State is the scheduler's state for the API.
type State struct {
	Mode         Mode
	Modes        []Mode // the modes this install offers
	SetByFlag    bool   // a --speedtest flag chose the mode at start
	Extra        []string
	Interval     time.Duration
	Spacing      time.Duration // between slots
	PlannedShare float64       // of time under test, planned
	Warning      string        // when PlannedShare is over ShareWarn
	Plans        []Plan        // every offered mode, for estimates
	Next         time.Time
	NextRow      string
	Rows         []Row // the current mode's scheduled rows
	Servers      []ServerState
	Regions      []Region
	Custom       []string // custom server IDs
	// Vias is each server's latest handoff that crossed an internet
	// exchange (Handoff.Via; in memory only, local only).
	Vias map[string]model.Handoff
	// OwnerZone is the owner's IANA zone ("" unknown: see OwnerZone).
	OwnerZone string
}

// New returns a Scheduler; Run drives it.
func New(o Options) *Scheduler {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if f := o.Fake; f != nil {
		o.run, o.rtt, o.sleep = f.Run, f.RTT, f.Sleep
		if f.Seed != 0 {
			o.rand = rand.New(rand.NewPCG(f.Seed, f.Seed))
		}
	}
	if o.run == nil {
		o.run = Run
	}
	if o.loadEvery <= 0 {
		o.loadEvery = LoadEvery
	}
	if o.rtt == nil {
		ping := o.Ping
		o.rtt = func(ctx context.Context, host string, port int) (time.Duration, error) {
			return serverRTT(ctx, ping, host, port)
		}
	}
	if o.sleep == nil {
		o.sleep = sleepCtx
	}
	if o.rand == nil {
		o.rand = rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64()))
	}
	if o.Windows == nil {
		o.Windows = &Windows{}
	}
	if o.Catalog == nil {
		o.Catalog = &Catalog{byID: map[string]int{}}
	}
	s := &Scheduler{o: o, wake: make(chan struct{}, 1), rtts: map[string]time.Duration{}, mode: ModeOff,
		extra: slices.Clone(o.Extra), seed: o.rand.Uint32(), skip: map[string]bool{}, busyRun: map[string]int{}, rest: map[string]time.Time{}}
	s.q = NewQueue(QueueOptions{Resolve: s.resolve, Exec: s.execJob, Pause: s.pause, EstSweepPause: MaskTail + SweepGap, Live: s.live, Log: o.Log, Now: o.Now})
	return s
}

// OwnOnly reports a non-empty custom list of the user's own servers only:
// the 15-minute floor does not apply to it.
func OwnOnly(ss []Server) bool {
	return len(ss) > 0 && !slices.ContainsFunc(ss, func(s Server) bool { return !s.LAN })
}

// IntervalFor is mode m's interval per region or server.
func (s *Scheduler) IntervalFor(m Mode) time.Duration {
	iv := s.o.Interval
	if iv <= 0 {
		return DefaultInterval(m)
	}
	if m == ModeCustom && OwnOnly(s.o.Custom) {
		return max(iv, MinOwnInterval)
	}
	return max(iv, MinInterval)
}

// Modes are the modes this install offers: custom only with servers.
func (s *Scheduler) Modes() []Mode {
	m := []Mode{ModeOff, ModeRegions, ModeNearby}
	if len(s.o.Custom) > 0 {
		m = append(m, ModeCustom)
	}
	return m
}

func (s *Scheduler) valid(m Mode) bool { return slices.Contains(s.Modes(), m) }

// planLocked is a cycle's targets for mode m. Regions come in an order
// permuted by the install's seed, so installs don't all test the same
// region at the same minute.
func (s *Scheduler) planLocked(m Mode) []target {
	switch m {
	case ModeRegions:
		var out []target
		for _, r := range s.o.Catalog.Regions {
			switch {
			case r.Default:
				out = append(out, target{region: r.ID, nearby: -1})
			case slices.Contains(s.extra, r.ID):
				out = append(out, target{region: r.ID, nearby: -1, why: "added"})
			}
		}
		pr := rand.New(rand.NewPCG(uint64(s.seed), 0x5eed))
		pr.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
		return out
	case ModeNearby:
		var out []target
		for i := range nearbyCount {
			out = append(out, target{nearby: i})
		}
		out = append(out, target{region: "far", nearby: -1, why: "far"})
		for _, r := range s.extra {
			out = append(out, target{region: r, nearby: -1, why: "far"})
		}
		return out
	case ModeCustom:
		var out []target
		for _, sv := range s.o.Custom {
			out = append(out, target{server: sv.ID, nearby: -1})
		}
		return out
	}
	return nil
}

func (s *Scheduler) spacingLocked(m Mode) time.Duration {
	n := len(s.planLocked(m))
	if n == 0 {
		return 0
	}
	return s.IntervalFor(m) / time.Duration(n)
}

func (s *Scheduler) planLoadLocked(m Mode) Plan {
	n := float64(len(s.planLocked(m)))
	iv := s.IntervalFor(m)
	return Plan{Mode: m, TestsPerHour: n * float64(time.Hour) / float64(iv), Share: n * float64(EstTestTime) / float64(iv)}
}

// PlannedShare is the share of time mode m would keep the line under test.
func (s *Scheduler) PlannedShare(m Mode) float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.planLoadLocked(m).Share
}

func (s *Scheduler) jitter() time.Duration {
	return time.Duration(s.o.rand.Int64N(int64(2*Jitter)+1)) - Jitter
}

// phase is the install's offset within an interval: cycles of different
// installs start at different minutes of the hour.
func (s *Scheduler) phase(iv time.Duration) time.Duration {
	return time.Duration(uint64(s.seed) * uint64(time.Second) % uint64(iv))
}

// restartLocked schedules the mode from now. Cycles sit on the install's
// phase; the first test is the first slot at least firstDelay away, so
// idle RTTs exist before it.
func (s *Scheduler) restartLocked(now time.Time) {
	s.slot, s.next, s.skipBefore = 0, time.Time{}, time.Time{}
	n := len(s.planLocked(s.mode))
	if n == 0 {
		return
	}
	iv := s.IntervalFor(s.mode)
	sp := iv / time.Duration(n)
	base := now.Truncate(iv).Add(s.phase(iv))
	if base.After(now) {
		base = base.Add(-iv)
	}
	for {
		for i := range n {
			if at := base.Add(time.Duration(i) * sp); !at.Before(now.Add(firstDelay)) {
				s.cycleBase, s.cycleStart, s.slot, s.next = base, base, i, at
				return
			}
		}
		base = base.Add(iv)
	}
}

// dueLocked takes the slot due at now (nil: none) and moves on; wait is
// the time to the next one (0: look again now). Every due slot goes into
// the queue, late ones too: the queue drops a slot that waits
// ScheduledStale, and never stacks two. The slot times themselves never
// move.
func (s *Scheduler) dueLocked(now time.Time) (*target, time.Duration) {
	if s.next.IsZero() {
		return nil, time.Hour
	}
	if wait := s.next.Sub(now); wait > 0 {
		return nil, wait
	}
	var tg *target
	if plan := s.planLocked(s.mode); s.slot < len(plan) {
		t := plan[s.slot]
		t.at = s.cycleBase.Add(time.Duration(s.slot) * s.IntervalFor(s.mode) / time.Duration(len(plan)))
		tg = &t
	}
	s.advanceLocked()
	for !s.next.IsZero() && s.next.Before(s.skipBefore) {
		s.advanceLocked() // too close to an early test
	}
	return tg, 0
}

// earlyLocked, after an explicit turn-on or mode change (never a
// restart), brings the first test to firstDelay from now: an extra run of
// the next grid slot (its rotation stays its nominal time's). The grid
// then goes on from the slot after it, skipping any slot less than half a
// spacing after the early test, so no two tests are closer than that.
func (s *Scheduler) earlyLocked(now time.Time) {
	early := now.Add(firstDelay)
	if s.next.IsZero() || !s.next.After(early) {
		return
	}
	s.next = early
	s.skipBefore = early.Add(s.spacingLocked(s.mode) / 2)
}

// advanceLocked moves to the next slot. A new cycle starts at the next
// interval plus fresh jitter (the spacing within a cycle stays regular),
// never sooner than cycleGap after the previous cycle's last slot.
func (s *Scheduler) advanceLocked() {
	n := len(s.planLocked(s.mode))
	if n == 0 {
		s.next = time.Time{}
		return
	}
	iv := s.IntervalFor(s.mode)
	sp := iv / time.Duration(n)
	s.slot++
	if s.slot >= n {
		last := s.cycleStart.Add(time.Duration(n-1) * sp)
		s.cycleBase = s.cycleBase.Add(iv)
		start := s.cycleBase.Add(s.jitter())
		if earliest := last.Add(min(cycleGap, sp/2)); start.Before(earliest) {
			start = earliest
		}
		s.cycleStart, s.slot = start, 0
	}
	s.next = s.cycleStart.Add(time.Duration(s.slot) * sp)
}

func (s *Scheduler) poke() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Scheduler) warn(m Mode) {
	if share := s.PlannedShare(m); share > ShareWarn {
		s.o.Log.Warn("speed tests: the line will be under test a large share of the time", "mode", m,
			"planned_share", round1(share*100)/100, "interval", s.IntervalFor(m).String())
	}
}

// load reads the saved seed, mode and extra regions.
func (s *Scheduler) load(ctx context.Context) {
	st := s.o.Store
	if st == nil {
		return
	}
	if v, ok, err := st.Setting(ctx, settingSeed); err == nil && ok {
		if n, err := strconv.ParseUint(v, 10, 32); err == nil {
			s.seed = uint32(n)
		}
	} else if err == nil {
		_ = st.SetSetting(ctx, settingSeed, strconv.FormatUint(uint64(s.seed), 10))
	}
	if s.o.Mode == "" {
		if v, ok, err := st.Setting(ctx, SettingKey); err == nil && ok {
			s.mode = Mode(v)
		}
		if v, ok, err := st.Setting(ctx, settingExtra); err == nil && ok && v != "" && len(s.o.Extra) == 0 {
			s.extra = splitIDs(v)
		}
	}
	if v, ok, err := st.Setting(ctx, SettingZone); err == nil && ok {
		s.zone = v
	}
	if v, ok, err := st.Setting(ctx, settingSkip); err == nil && ok {
		for _, id := range splitIDs(v) {
			s.skip[id] = true
		}
	}
}

func splitIDs(v string) []string {
	var out []string
	for _, x := range strings.Split(v, ",") {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return out
}

// Run loads the saved settings and past windows, then schedules and runs
// tests until ctx ends.
func (s *Scheduler) Run(ctx context.Context) error {
	s.mu.Lock()
	s.load(ctx)
	if s.o.Mode != "" {
		s.mode, s.setByFlag = s.o.Mode, true
	}
	if !s.valid(s.mode) {
		s.mode = ModeOff
	}
	s.restartLocked(s.o.Now())
	mode := s.mode
	s.mu.Unlock()
	if s.o.Store != nil {
		now := s.o.Now()
		if ts, err := s.o.Store.SpeedTests(ctx, now.Add(-keepWindows), now); err == nil {
			for _, t := range ts {
				s.o.Windows.Add(Window{Start: t.Start, End: t.End})
			}
		}
	}
	s.warn(mode)
	qdone := make(chan struct{})
	go func() {
		defer close(qdone)
		s.q.Run(ctx)
	}()
	defer func() { <-qdone }()
	for {
		s.mu.Lock()
		tg, wait := s.dueLocked(s.o.Now())
		s.mu.Unlock()
		if tg != nil {
			s.q.EnqueueScheduled(s.slotJob(*tg))
		}
		if wait <= 0 {
			continue
		}
		t := time.NewTimer(min(wait, time.Minute)) // re-check: dropped slots, clock steps
		select {
		case <-ctx.Done():
			t.Stop()
			return nil
		case <-s.wake:
		case <-t.C:
		}
		t.Stop()
	}
}

// Queue is the speed-test queue: every test, scheduled or on demand, runs
// through it, one at a time.
func (s *Scheduler) Queue() *Queue { return s.q }

func (s *Scheduler) server(id string) (Server, bool) {
	if sv, ok := s.o.Catalog.Server(id); ok {
		return sv, true
	}
	for _, sv := range s.o.Custom {
		if sv.ID == id {
			return sv, true
		}
	}
	return Server{}, false
}

func (s *Scheduler) region(id string) (Region, bool) {
	for _, r := range s.o.Catalog.Regions {
		if r.ID == id {
			return r, true
		}
	}
	return Region{}, false
}

// SetMode switches the schedule (and the extra regions, when extra is not
// nil) and saves the choice. It returns the next scheduled test (zero when
// off).
func (s *Scheduler) SetMode(ctx context.Context, m Mode, extra []string) (time.Time, error) {
	if !s.valid(m) {
		return time.Time{}, ErrMode
	}
	for _, r := range extra {
		if _, ok := s.region(r); !ok {
			return time.Time{}, fmt.Errorf("%w: region %q", ErrMode, r)
		}
	}
	if st := s.o.Store; st != nil {
		if err := st.SetSetting(ctx, SettingKey, string(m)); err != nil {
			return time.Time{}, err
		}
		if extra != nil {
			if err := st.SetSetting(ctx, settingExtra, strings.Join(extra, ",")); err != nil {
				return time.Time{}, err
			}
		}
	}
	s.mu.Lock()
	changed := s.mode != m || (extra != nil && !slices.Equal(extra, s.extra))
	s.mode, s.setByFlag = m, false
	if extra != nil {
		s.extra = slices.Clone(extra)
	}
	if changed {
		s.restartLocked(s.o.Now())
		s.earlyLocked(s.o.Now())
	}
	next := s.next
	s.mu.Unlock()
	if changed {
		s.warn(m)
	}
	s.poke()
	return next, nil
}

// NoteZone keeps the owner's IANA zone (from a local page's tz) when it
// changed; anything time.LoadLocation can't load is ignored.
func (s *Scheduler) NoteZone(name string) {
	if name == "" || name == "Local" {
		return
	}
	s.mu.Lock()
	same := s.zone == name
	s.mu.Unlock()
	if same {
		return
	}
	if _, err := time.LoadLocation(name); err != nil {
		return
	}
	s.mu.Lock()
	s.zone = name
	s.mu.Unlock()
	if st := s.o.Store; st != nil {
		ctx, cancel := storeCtx(context.Background())
		defer cancel()
		if err := st.SetSetting(ctx, SettingZone, name); err != nil {
			s.o.Log.Debug("speed test: saving the owner's time zone failed", "err", err)
		}
	}
}

// OwnerZone is where the owner's times of day are: their IANA zone,
// else the server's (TZ, time.Local), which is UTC when unset.
func OwnerZone(name string) *time.Location {
	if name != "" && name != "Local" {
		if loc, err := time.LoadLocation(name); err == nil {
			return loc
		}
	}
	return time.Local
}

// ZoneLabel names loc at t: "EDT (UTC−4)", "UTC+5:30" when it has no
// abbreviation, "UTC".
func ZoneLabel(loc *time.Location, t time.Time) string {
	name, off := t.In(loc).Zone()
	if off == 0 && (name == "UTC" || name == "GMT" || name == "") {
		return "UTC"
	}
	sign := "+"
	if off < 0 {
		sign, off = "−", -off
	}
	utc := fmt.Sprintf("UTC%s%d", sign, off/3600)
	if m := off % 3600 / 60; m != 0 {
		utc += fmt.Sprintf(":%02d", m)
	}
	if name == "" || name[0] == '+' || name[0] == '-' {
		return utc
	}
	return name + " (" + utc + ")"
}

// StoredZone is OwnerZone of the zone saved in st (the report's zone).
func StoredZone(ctx context.Context, st interface {
	Setting(ctx context.Context, key string) (string, bool, error)
}) *time.Location {
	if st == nil {
		return time.Local
	}
	v, _, _ := st.Setting(ctx, SettingZone)
	return OwnerZone(v)
}

// State reports the schedule, the rows and the servers.
func (s *Scheduler) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	load := s.planLoadLocked(s.mode)
	st := State{Mode: s.mode, Modes: s.Modes(), SetByFlag: s.setByFlag, Extra: slices.Clone(s.extra),
		Interval: s.IntervalFor(s.mode), Spacing: s.spacingLocked(s.mode), PlannedShare: load.Share,
		Regions: s.o.Catalog.Regions, Next: s.next, Vias: maps.Clone(s.vias), OwnerZone: s.zone}
	if s.mode == ModeOff {
		st.PlannedShare = 0
	}
	for _, m := range s.Modes()[1:] {
		st.Plans = append(st.Plans, s.planLoadLocked(m))
	}
	if st.PlannedShare > ShareWarn {
		st.Warning = fmt.Sprintf("About %d%% of the time is spent testing, which slows down everything else on this line while it runs. Consider a longer interval.", int(st.PlannedShare*100+0.5))
	}
	plan := s.planLocked(s.mode)
	if !s.next.IsZero() && s.slot < len(plan) {
		st.NextRow = s.rowOfLocked(plan[s.slot]).ID
	}
	for _, t := range plan {
		if r := s.rowOfLocked(t); r.ID != "" {
			st.Rows = append(st.Rows, r)
		}
	}
	add := func(sv Server) {
		ss := ServerState{Server: sv, Picked: slices.Contains(s.picked, sv.ID), Skipped: s.skippedLocked(sv.ID), SkipLocked: slices.Contains(s.o.Skip, sv.ID)}
		if r := s.rest[sv.ID]; s.o.Now().Before(r) {
			ss.RestUntil = r
		}
		if d, ok := s.rtts[sv.ID]; ok {
			ms := float64(d) / float64(time.Millisecond)
			ss.RTTms = &ms
		}
		st.Servers = append(st.Servers, ss)
	}
	for _, sv := range s.o.Catalog.Servers {
		add(sv)
	}
	for _, sv := range s.o.Custom {
		if _, ok := s.o.Catalog.Server(sv.ID); !ok {
			add(sv)
		}
		st.Custom = append(st.Custom, sv.ID)
	}
	return st
}

// rowOfLocked describes a plan target as a row (nearby picks and the far
// region are resolved once picked; empty before).
func (s *Scheduler) rowOfLocked(t target) Row {
	switch {
	case t.nearby >= 0:
		if t.nearby >= len(s.picked) {
			return Row{}
		}
		sv, _ := s.o.Catalog.Server(s.picked[t.nearby])
		return Row{ID: sv.ID, Kind: "server", Label: sv.Label, Why: "nearest " + sv.Provider, Servers: []string{sv.ID}}
	case t.region != "":
		id := t.region
		if id == "far" {
			if id = s.far; id == "" {
				return Row{}
			}
		}
		r, ok := s.region(id)
		if !ok {
			return Row{}
		}
		why := t.why
		if why == "far" {
			why = "far: " + r.Title
		}
		return Row{ID: r.ID, Kind: "region", Label: r.Title, Why: why, Servers: append(slices.Clone(r.Servers), r.Nearby...)}
	case t.server != "":
		sv, _ := s.server(t.server)
		return Row{ID: sv.ID, Kind: "server", Label: sv.Label, Servers: []string{sv.ID}}
	}
	return Row{}
}

// MaskedShare is the share of the last d spent under test (masked).
func (s *Scheduler) MaskedShare(d time.Duration) float64 { return s.o.Windows.Share(s.o.Now(), d) }

// SpeedTests reads stored tests (oldest first).
func (s *Scheduler) SpeedTests(ctx context.Context, from, to time.Time) ([]model.SpeedTest, error) {
	if s.o.Store == nil {
		return nil, nil
	}
	return s.o.Store.SpeedTests(ctx, from, to)
}

func round1(v float64) float64 { return float64(int64(v*10+0.5)) / 10 }

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// Catalog is the server catalog.
func (s *Scheduler) Catalog() *Catalog { return s.o.Catalog }

// BytesSince is the data the speed tests used since t (0 when the store
// cannot tell).
func (s *Scheduler) BytesSince(ctx context.Context, t time.Time) (int64, error) {
	if b, ok := s.o.Store.(interface {
		BytesSince(ctx context.Context, t time.Time) (int64, error)
	}); ok {
		return b.BytesSince(ctx, t)
	}
	return 0, nil
}
