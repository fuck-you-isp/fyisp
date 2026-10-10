package speedtest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

// Mode is what the schedule tests.
type Mode string

const (
	ModeOff Mode = "off" // the default: no scheduled tests (Test now still works)
	// ModeRegions tests every region of the catalog once per interval, one
	// test at a time, spread evenly; each region falls back along its chain.
	ModeRegions Mode = "regions"
	// ModeNearest tests the two servers nearest by RTT, alternating, one
	// test per interval.
	ModeNearest Mode = "nearest"
	// ModeCustom tests the --speedtest-server list, each once per interval.
	ModeCustom Mode = "custom"
)

// Schedule and test limits (see docs/SPEEDTEST-API.md).
const (
	MinInterval = 15 * time.Minute // per region or server; none when every server is the user's own
	Jitter      = time.Minute      // on each cycle's start only: the spacing stays regular
	RepickEvery = 24 * time.Hour   // nearest servers by RTT
	MaxAttempts = 3                // per server: busy or refused tries another port
	AttemptGap  = 2 * time.Second
	DirTimeout  = 15 * time.Second // one direction, connect to results
	// SlotBudget: no new attempt or fallback server after this much of a
	// slot; SlotCap is the hard limit of one slot.
	SlotBudget = 25 * time.Second
	SlotCap    = 45 * time.Second
	// MinMeasure..MaxMeasure is the adaptive measured time per direction,
	// after a 1 s ramp-up.
	MinMeasure = 3 * time.Second
	MaxMeasure = 8 * time.Second
	// EstTestTime is a typical test (both directions, adaptive): the
	// planned share of time under test is slots × EstTestTime / interval.
	EstTestTime = 15 * time.Second
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
)

// ErrRunning: one test at a time, process-wide.
var ErrRunning = errors.New("a speed test is already running")

// ErrMode is a mode this install does not offer.
var ErrMode = errors.New("unknown speed test mode")

// running is process-wide: two schedulers (tests) never run tests at once.
var running atomic.Bool

// Store keeps results and the schedule setting (*store.SQLite).
type Store interface {
	SaveSpeedTest(ctx context.Context, t *model.SpeedTest) error
	SpeedTests(ctx context.Context, from, to time.Time) ([]model.SpeedTest, error)
	Setting(ctx context.Context, key string) (string, bool, error)
	SetSetting(ctx context.Context, key, value string) error
}

// SettingKey is the store setting that remembers the Details choice.
const SettingKey = "speedtest_mode"

// Options configure a Scheduler.
type Options struct {
	Catalog *Catalog
	Custom  []Server // --speedtest-server
	// Mode at start (--speedtest); "" uses the saved Details choice, else off.
	Mode Mode
	// Interval per region or server (default and minimum MinInterval, any
	// positive value when every Custom server is the user's own).
	Interval time.Duration
	Store    Store
	Windows  *Windows // marked for every test (masking)
	Load     *LoadTap // idle latency before each test; nil: none
	// Path returns the gateway and the ISP edge (netinfo); OpenPinger the
	// ICMP pinger (probe.OpenPinger). With both, each direction of a test
	// pings them every LoadEvery (latency under load, see load.go).
	Path       func() (gateway, edge netip.Addr)
	OpenPinger func() (LoadPinger, error)
	// Conn returns the connection type (model.ConnWired, ...) of the
	// interface tests use; nil: unknown.
	Conn func() string
	// Handoff returns where the path to host leaves the user's ISP
	// (HandoffFinder.Handoff); nil: not recorded.
	Handoff func(ctx context.Context, host string) (*model.Handoff, error)
	// Ping returns the ICMP RTT to a host (probe.PingOnce); nil or failing:
	// TCP connect time to port 80, 443, then the server's first iperf3 port.
	Ping func(ctx context.Context, host string) (time.Duration, error)
	// OnResult is called after every saved test (metrics).
	OnResult func(model.SpeedTest)
	Log      *slog.Logger
	Now      func() time.Time

	// Tests: the iperf3 run, the RTT check, the sleep and the random source.
	run       func(ctx context.Context, c Config) (Result, error)
	loadEvery time.Duration // default LoadEvery
	rtt       func(ctx context.Context, host string, port int) (time.Duration, error)
	sleep     func(ctx context.Context, d time.Duration) bool
	rand      *rand.Rand
}

// target is one slot's test: a region (or none) and the servers to try.
type target struct {
	region string
	chain  []Server
}

// Scheduler runs speed tests on a schedule and on demand.
type Scheduler struct {
	o    Options
	wake chan struct{}

	mu         sync.Mutex
	mode       Mode
	cycleBase  time.Time // the cycle's start without jitter
	cycleStart time.Time
	slot       int
	next       time.Time // the next slot; zero when off
	manual     *target   // a queued Test now
	cur        *RunState
	picked     []string // the nearest servers, by ID
	pickedAt   time.Time
	rtts       map[string]time.Duration
	turn       int
}

// RunState is a running test.
type RunState struct {
	Started     time.Time
	ExpectedEnd time.Time
	Direction   string // picking, download, upload
	Server      string // ID
	Region      string
}

// ServerState is one server for the API.
type ServerState struct {
	Server
	RTTms  *float64
	Picked bool
}

// State is the scheduler's state for the API.
type State struct {
	Mode         Mode
	Modes        []Mode // the modes this install offers
	Interval     time.Duration
	Spacing      time.Duration // between slots
	PlannedShare float64       // of time under test, planned
	Warning      string        // when PlannedShare is over ShareWarn
	Next         time.Time
	NextRegion   string
	NextServer   string
	Running      *RunState
	Servers      []ServerState // the catalog, then the custom servers
	Regions      []Region
	Custom       []string // custom server IDs
}

// New returns a Scheduler; Run drives it.
func New(o Options) *Scheduler {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
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
	if o.Interval <= 0 {
		o.Interval = MinInterval
	}
	if !OwnOnly(o.Custom) {
		o.Interval = max(o.Interval, MinInterval)
	}
	return &Scheduler{o: o, wake: make(chan struct{}, 1), rtts: map[string]time.Duration{}, mode: ModeOff}
}

// OwnOnly reports a non-empty custom list of the user's own servers only:
// the 15-minute floor does not apply to it.
func OwnOnly(ss []Server) bool {
	return len(ss) > 0 && !slices.ContainsFunc(ss, func(s Server) bool { return !s.Custom })
}

// Interval is the schedule's interval per region or server.
func (s *Scheduler) Interval() time.Duration { return s.o.Interval }

// Modes are the modes this install offers: custom only with servers.
func (s *Scheduler) Modes() []Mode {
	m := []Mode{ModeOff, ModeRegions, ModeNearest}
	if len(s.o.Custom) > 0 {
		m = append(m, ModeCustom)
	}
	return m
}

func (s *Scheduler) valid(m Mode) bool { return slices.Contains(s.Modes(), m) }

// planLocked is a cycle's targets for mode m (nearest: one, filled in at
// run time).
func (s *Scheduler) planLocked(m Mode) []target {
	switch m {
	case ModeRegions:
		var out []target
		for _, r := range s.o.Catalog.Regions {
			t := target{region: r.ID}
			for _, id := range r.Chain {
				if sv, ok := s.o.Catalog.Server(id); ok {
					t.chain = append(t.chain, sv)
				}
			}
			out = append(out, t)
		}
		return out
	case ModeCustom:
		var out []target
		for _, sv := range s.o.Custom {
			out = append(out, target{chain: []Server{sv}})
		}
		return out
	case ModeNearest:
		return []target{{}}
	}
	return nil
}

// spacingLocked is the time between slots of mode m.
func (s *Scheduler) spacingLocked(m Mode) time.Duration {
	n := len(s.planLocked(m))
	if n == 0 {
		return 0
	}
	return s.o.Interval / time.Duration(n)
}

// plannedShareLocked is the share of time mode m keeps the line under test.
func (s *Scheduler) plannedShareLocked(m Mode) float64 {
	return float64(len(s.planLocked(m))) * float64(EstTestTime) / float64(s.o.Interval)
}

// PlannedShare is the share of time mode m would keep the line under test.
func (s *Scheduler) PlannedShare(m Mode) float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.plannedShareLocked(m)
}

func (s *Scheduler) jitter() time.Duration {
	return time.Duration(s.o.rand.Int64N(int64(2*Jitter)+1)) - Jitter
}

// restartLocked schedules the mode from now: the first slot after
// firstDelay (+0..Jitter), so idle RTTs exist before the first test.
func (s *Scheduler) restartLocked(now time.Time) {
	s.slot = 0
	s.next = time.Time{}
	if len(s.planLocked(s.mode)) == 0 {
		return
	}
	s.cycleBase = now.Add(firstDelay)
	s.cycleStart = s.cycleBase.Add(time.Duration(s.o.rand.Int64N(int64(Jitter) + 1)))
	s.next = s.cycleStart
}

// advanceLocked moves to the next slot. A new cycle starts at its base plus
// fresh jitter (the spacing within a cycle stays regular), never sooner
// than cycleGap after the previous cycle's last slot.
func (s *Scheduler) advanceLocked() {
	n := len(s.planLocked(s.mode))
	if n == 0 {
		s.next = time.Time{}
		return
	}
	sp := s.o.Interval / time.Duration(n)
	s.slot++
	if s.slot >= n {
		last := s.cycleStart.Add(time.Duration(n-1) * sp)
		s.cycleBase = s.cycleBase.Add(s.o.Interval)
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
			"planned_share", round1(share*100)/100, "interval", s.o.Interval.String())
	}
}

// Run loads the saved mode and the past windows, then runs tests until ctx
// ends.
func (s *Scheduler) Run(ctx context.Context) error {
	mode := s.o.Mode
	if mode == "" && s.o.Store != nil {
		if v, ok, err := s.o.Store.Setting(ctx, SettingKey); err == nil && ok {
			mode = Mode(v)
		}
	}
	if !s.valid(mode) {
		mode = ModeOff
	}
	if s.o.Store != nil {
		now := s.o.Now()
		if ts, err := s.o.Store.SpeedTests(ctx, now.Add(-keepWindows), now); err == nil {
			for _, t := range ts {
				s.o.Windows.Add(Window{Start: t.Start, End: t.End})
			}
		}
	}
	s.mu.Lock()
	s.mode = mode
	s.restartLocked(s.o.Now())
	s.mu.Unlock()
	s.warn(mode)
	for {
		s.mu.Lock()
		now := s.o.Now()
		m := s.manual
		s.manual = nil
		var tg *target
		wait := time.Hour
		if m == nil && !s.next.IsZero() {
			if wait = s.next.Sub(now); wait <= 0 {
				plan := s.planLocked(s.mode)
				late := now.Sub(s.next) > s.spacingLocked(s.mode)/2
				if s.slot < len(plan) && !late {
					t := plan[s.slot]
					tg = &t
				}
				s.advanceLocked() // a late slot is skipped: later ones never move
			}
		}
		s.mu.Unlock()
		switch {
		case m != nil:
			s.runTarget(ctx, *m, "manual")
			continue
		case tg != nil:
			s.runTarget(ctx, *tg, "schedule")
			continue
		case wait <= 0:
			continue
		}
		t := time.NewTimer(min(wait, time.Hour))
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

// TestNow queues a test to run immediately: of the server or region with
// this ID, else of the nearest servers. It fails with ErrRunning while one
// runs or is queued, and with an error for an unknown ID.
func (s *Scheduler) TestNow(id string) (time.Time, error) {
	tg := target{}
	if id != "" {
		if sv, ok := s.server(id); ok {
			tg.chain = []Server{sv}
		} else if r, ok := s.region(id); ok {
			tg.region = r.ID
			for _, x := range r.Chain {
				if sv, ok := s.o.Catalog.Server(x); ok {
					tg.chain = append(tg.chain, sv)
				}
			}
		} else {
			return time.Time{}, fmt.Errorf("no server or region %q", id)
		}
	}
	s.mu.Lock()
	if s.manual != nil || s.cur != nil || running.Load() {
		s.mu.Unlock()
		return time.Time{}, ErrRunning
	}
	s.manual = &tg
	s.mu.Unlock()
	s.poke()
	return s.o.Now(), nil
}

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

// SetMode switches the schedule and saves the choice. It returns the next
// scheduled test (zero when off).
func (s *Scheduler) SetMode(ctx context.Context, m Mode) (time.Time, error) {
	if !s.valid(m) {
		return time.Time{}, ErrMode
	}
	if s.o.Store != nil {
		if err := s.o.Store.SetSetting(ctx, SettingKey, string(m)); err != nil {
			return time.Time{}, err
		}
	}
	s.mu.Lock()
	changed := s.mode != m
	s.mode = m
	if changed {
		s.restartLocked(s.o.Now())
	}
	next := s.next
	s.mu.Unlock()
	if changed {
		s.warn(m)
	}
	s.poke()
	return next, nil
}

// State reports the schedule, the running test and the servers.
func (s *Scheduler) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := State{Mode: s.mode, Modes: s.Modes(), Interval: s.o.Interval, Spacing: s.spacingLocked(s.mode),
		PlannedShare: s.plannedShareLocked(s.mode), Regions: s.o.Catalog.Regions}
	if st.PlannedShare > ShareWarn {
		st.Warning = fmt.Sprintf("About %d%% of the time is spent testing, which slows down everything else on this line while it runs. Consider a longer interval.", int(st.PlannedShare*100+0.5))
	}
	if s.cur == nil && !s.next.IsZero() {
		st.Next = s.next
		if plan := s.planLocked(s.mode); s.slot < len(plan) {
			st.NextRegion = plan[s.slot].region
			if len(plan[s.slot].chain) > 0 {
				st.NextServer = plan[s.slot].chain[0].ID
			}
		}
	}
	if s.cur != nil {
		c := *s.cur
		st.Running = &c
	}
	add := func(sv Server) {
		ss := ServerState{Server: sv, Picked: slices.Contains(s.picked, sv.ID)}
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

// MaskedShare is the share of the last d spent under test (masked).
func (s *Scheduler) MaskedShare(d time.Duration) float64 { return s.o.Windows.Share(s.o.Now(), d) }

// SpeedTests reads stored tests (oldest first).
func (s *Scheduler) SpeedTests(ctx context.Context, from, to time.Time) ([]model.SpeedTest, error) {
	if s.o.Store == nil {
		return nil, nil
	}
	return s.o.Store.SpeedTests(ctx, from, to)
}

func (s *Scheduler) setRun(dir, server string) {
	s.mu.Lock()
	if s.cur != nil {
		s.cur.Direction, s.cur.Server = dir, server
	}
	s.mu.Unlock()
}

// runTarget runs one slot: the chain's servers in order until one
// measures, within SlotBudget; then saves and reports the result.
func (s *Scheduler) runTarget(ctx context.Context, tg target, trigger string) {
	if !running.CompareAndSwap(false, true) {
		return
	}
	defer running.Store(false)
	now := s.o.Now()
	s.mu.Lock()
	s.cur = &RunState{Started: now, ExpectedEnd: now.Add(EstTestTime + 5*time.Second), Direction: "picking", Region: tg.region}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.cur = nil
		s.mu.Unlock()
	}()
	if len(tg.chain) == 0 { // nearest
		tg.chain = s.nearest(ctx)
	}
	if ctx.Err() != nil {
		return
	}
	t := model.SpeedTest{Trigger: trigger, Region: tg.region, Outcome: model.SpeedUnreachable}
	if len(tg.chain) == 0 {
		now := s.o.Now()
		t.Start, t.End = now, now
		s.save(ctx, &t)
		return
	}
	sctx, cancel := context.WithTimeout(ctx, SlotCap)
	defer cancel()
	lr := s.loadRun()
	if lr != nil {
		defer lr.p.Close()
	}
	t.Conn = model.ConnUnknown
	if s.o.Conn != nil {
		t.Conn = s.o.Conn()
	}
	t.Start = s.o.Now()
	s.o.Windows.Begin(t.Start)
	var used Server
	for i, sv := range tg.chain {
		if i > 0 && s.o.Now().Sub(t.Start) >= SlotBudget {
			break
		}
		t.Server, t.Fallback, t.Port, used = sv.ID, i > 0, 0, sv
		s.testServer(sctx, sv, &t, lr)
		if t.Outcome == model.SpeedOK || t.DownBps != nil || sctx.Err() != nil {
			break // measured (a failed upload keeps the download), or out of time
		}
	}
	t.End = s.o.Now()
	win := s.o.Windows.End(t.End)
	if ctx.Err() != nil {
		return // shutting down: not a result
	}
	if t.Latency = latency(s.o.Load, s.o.Windows, lr, win.Start); t.Latency != nil {
		g := t.Latency.Gateway
		if g.Idle != nil {
			t.IdleGatewayMs = &g.Idle.P50Ms
		}
		for _, x := range []*model.LoadStats{g.Down, g.Up} {
			if x != nil && (t.LoadedGatewayMs == nil || x.P50Ms > *t.LoadedGatewayMs) {
				t.LoadedGatewayMs = &x.P50Ms
			}
		}
	}
	if s.o.Handoff != nil && t.Outcome != model.SpeedUnreachable && used.Host != "" {
		hctx, cancel := context.WithTimeout(ctx, handoffBudget)
		h, err := s.o.Handoff(hctx, used.Host)
		cancel()
		if err != nil {
			s.o.Log.Debug("speed test: handoff traceroute failed", "server", used.ID, "err", err)
		}
		t.Handoff = h
	}
	s.save(ctx, &t)
}

// handoffBudget bounds one handoff traceroute (after the test's window).
const handoffBudget = 10 * time.Second

// loadRun opens the pinger for one test's latency under load; nil without
// Path or OpenPinger, or when neither address is known.
func (s *Scheduler) loadRun() *loadRun {
	if s.o.Path == nil || s.o.OpenPinger == nil {
		return nil
	}
	gw, edge := s.o.Path()
	if !gw.IsValid() && !edge.IsValid() {
		return nil
	}
	p, err := s.o.OpenPinger()
	if err != nil {
		s.o.Log.Debug("speed test: no ICMP for latency under load", "err", err)
		return nil
	}
	return &loadRun{p: p, gw: gw, edge: edge, every: s.o.loadEvery}
}

func (s *Scheduler) save(ctx context.Context, t *model.SpeedTest) {
	if s.o.Store != nil {
		if err := s.o.Store.SaveSpeedTest(ctx, t); err != nil {
			s.o.Log.Warn("speed test: saving the result failed", "err", err)
		}
	}
	args := []any{"region", t.Region, "server", t.Server, "fallback", t.Fallback, "outcome", t.Outcome, "attempts", t.Attempts, "trigger", t.Trigger}
	if t.DownBps != nil {
		args = append(args, "down_mbps", round1(*t.DownBps/1e6))
	}
	if t.UpBps != nil {
		args = append(args, "up_mbps", round1(*t.UpBps/1e6))
	}
	s.o.Log.Info("speed test", args...)
	if s.o.OnResult != nil {
		s.o.OnResult(*t)
	}
}

func round1(v float64) float64 { return float64(int64(v*10+0.5)) / 10 }

// testServer runs download then upload against sv, probing latency under
// load during each direction (lr nil: none).
func (s *Scheduler) testServer(ctx context.Context, sv Server, t *model.SpeedTest, lr *loadRun) {
	port := 0
	for _, dir := range []Direction{Download, Upload} {
		s.setRun(dir.String(), sv.ID)
		if port != 0 && !s.o.sleep(ctx, relisten) {
			t.Outcome = model.SpeedFailed
			return
		}
		var stop func() [2][]loadSample
		if lr != nil {
			stop = lr.probe(ctx)
		}
		res, p, attempts, outcome := s.direction(ctx, sv, dir, port, t.Start)
		if stop != nil {
			if smp := stop(); dir == Download {
				lr.down = smp
			} else {
				lr.up = smp
			}
		}
		t.Attempts += attempts
		if p != 0 {
			t.Port, port = p, p
		}
		t.Outcome = outcome
		if outcome != model.SpeedOK {
			return
		}
		bps := res.BitsPerSecond()
		var re *int64
		if res.Retransmits >= 0 {
			r := res.Retransmits
			re = &r
		}
		if dir == Download {
			t.DownBps, t.DownRetrans = &bps, re
		} else {
			t.UpBps, t.UpRetrans = &bps, re
		}
	}
}

// direction runs one direction: first on port (0: a random one), then,
// while the server is busy or refuses (a stock iperf3 server re-listens
// for a moment after every test), on other random ports 2 s apart, up to
// MaxAttempts and within the slot's budget. A timeout or DNS failure
// (ErrUnreachable) and protocol errors end it at once.
func (s *Scheduler) direction(ctx context.Context, sv Server, dir Direction, port int, slotStart time.Time) (Result, int, int, string) {
	tried := map[int]bool{}
	for attempt := 1; ; attempt++ {
		if attempt > 1 || port == 0 {
			port = s.randomPort(sv, tried)
		}
		tried[port] = true
		res, err := s.o.run(ctx, Config{
			Addr: net.JoinHostPort(sv.Host, strconv.Itoa(port)), Direction: dir, Timeout: DirTimeout,
			Duration: MaxMeasure, MinDuration: MinMeasure,
		})
		switch {
		case err == nil:
			return res, port, attempt, model.SpeedOK
		case ctx.Err() != nil:
			return res, port, attempt, model.SpeedFailed
		case errors.Is(err, ErrUnreachable):
			return res, port, attempt, model.SpeedUnreachable
		case !errors.Is(err, ErrBusy) && !errors.Is(err, ErrRefused):
			s.o.Log.Debug("speed test failed", "server", sv.ID, "port", port, "direction", dir.String(), "err", err)
			return res, port, attempt, model.SpeedFailed
		}
		if attempt == MaxAttempts || s.o.Now().Sub(slotStart)+AttemptGap >= SlotBudget || !s.o.sleep(ctx, AttemptGap) {
			if errors.Is(err, ErrRefused) {
				return Result{}, port, attempt, model.SpeedUnreachable
			}
			return Result{}, port, attempt, model.SpeedBusy
		}
	}
}

// randomPort picks an untried port of sv (any, once all were tried).
func (s *Scheduler) randomPort(sv Server, tried map[int]bool) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := sv.ports()
	for range 4 * n {
		p := sv.PortMin + s.o.rand.IntN(n)
		if !tried[p] {
			return p
		}
	}
	return sv.PortMin + s.o.rand.IntN(n)
}

// nearest returns the two catalog servers nearest by RTT (re-checked every
// RepickEvery), in alternating order, for mode nearest and Test now.
func (s *Scheduler) nearest(ctx context.Context) []Server {
	s.mu.Lock()
	due := len(s.picked) == 0 || s.o.Now().Sub(s.pickedAt) >= RepickEvery
	s.mu.Unlock()
	if due {
		s.pick(ctx)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Server
	for i := range s.picked {
		if sv, ok := s.o.Catalog.Server(s.picked[(s.turn+i)%len(s.picked)]); ok {
			out = append(out, sv)
		}
	}
	s.turn++
	return out
}

// pick measures the RTT to every catalog server (the minimum of
// pickConnects checks, pickParallel servers at a time) and keeps the two
// nearest. A failed check keeps the previous pick.
func (s *Scheduler) pick(ctx context.Context) {
	type r struct {
		id string
		d  time.Duration
	}
	var mu sync.Mutex
	var got []r
	var wg sync.WaitGroup
	sem := make(chan struct{}, pickParallel)
	for _, sv := range s.o.Catalog.Servers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			best := time.Duration(-1)
			for range pickConnects {
				d, err := s.o.rtt(ctx, sv.Host, sv.PortMin)
				if err == nil && (best < 0 || d < best) {
					best = d
				}
			}
			if best >= 0 {
				mu.Lock()
				got = append(got, r{sv.ID, best})
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	slices.SortFunc(got, func(a, b r) int { return int(a.d - b.d) })
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rtts = map[string]time.Duration{}
	for _, x := range got {
		s.rtts[x.id] = x.d
	}
	if len(got) == 0 {
		return
	}
	s.picked = nil
	for _, x := range got[:min(2, len(got))] {
		s.picked = append(s.picked, x.id)
	}
	s.pickedAt = s.o.Now()
}

// serverRTT measures the RTT to an iperf3 server without disturbing it: an
// ICMP echo, else a TCP connect to port 80 or 443. A connect to the iperf3
// port itself is the last resort: the server takes it for a client and logs
// an error when it closes without a cookie.
func serverRTT(ctx context.Context, ping func(context.Context, string) (time.Duration, error), host string, iperfPort int) (time.Duration, error) {
	if ping != nil {
		if d, err := ping(ctx, host); err == nil {
			return d, nil
		}
	}
	var err error
	for _, p := range []int{80, 443, iperfPort} {
		var d time.Duration
		if d, err = connectRTT(ctx, host, p); err == nil {
			return d, nil
		}
	}
	return 0, err
}

// connectRTT is the time to open (and close) a TCP connection.
func connectRTT(ctx context.Context, host string, port int) (time.Duration, error) {
	d := net.Dialer{Timeout: pickTimeout}
	t0 := time.Now()
	c, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return 0, err
	}
	rtt := time.Since(t0)
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.SetLinger(0)
	}
	_ = c.Close()
	return rtt, nil
}

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
