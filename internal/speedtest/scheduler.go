package speedtest

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"net"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

// Schedule defaults (see the brief): a test every 15 minutes at most often,
// ±60 s of jitter so installs don't line up on the quarter-hour, the two
// nearest servers by RTT re-picked daily, up to 3 attempts 2 s apart on a
// busy server.
const (
	MinInterval  = 15 * time.Minute
	Jitter       = time.Minute
	RepickEvery  = 24 * time.Hour
	MaxAttempts  = 3
	AttemptGap   = 2 * time.Second
	// relisten is the pause before the upload: a stock iperf3 server
	// closes and reopens its listener after every test, and refuses
	// connections in between.
	relisten = time.Second
	DirTimeout   = 15 * time.Second // one direction, connect to results
	MaxTestTime  = 60 * time.Second // the whole test, retries included
	firstDelay   = 2 * time.Minute  // after start or enabling: idle RTTs first
	pickTimeout  = 2 * time.Second  // one RTT check connect
	pickConnects = 3
)

// ErrRunning: one test at a time, process-wide.
var ErrRunning = errors.New("a speed test is already running")

// running is process-wide: two schedulers (tests) never run tests at once.
var running atomic.Bool

// Store keeps results and the schedule setting (*store.SQLite).
type Store interface {
	SaveSpeedTest(ctx context.Context, t *model.SpeedTest) error
	SpeedTests(ctx context.Context, from, to time.Time) ([]model.SpeedTest, error)
	Setting(ctx context.Context, key string) (string, bool, error)
	SetSetting(ctx context.Context, key, value string) error
}

// SettingKey is the store setting that remembers the Details toggle.
const SettingKey = "speedtest_enabled"

// Options configure a Scheduler.
type Options struct {
	Servers []Server // the catalog list; ignored when Custom is set
	Custom  *Server  // --speedtest-server
	// Interval between scheduled tests (default and minimum MinInterval;
	// any positive value with Custom).
	Interval time.Duration
	// Enabled switches the schedule on at start (--speedtest). Otherwise the
	// saved Details toggle decides; without one it is off.
	Enabled bool
	Store   Store
	Windows *Windows // marked for every test (masking); required
	Load    *LoadTap // nil: no latency under load
	// Ping returns the ICMP RTT to a host (probe.PingOnce); nil or failing:
	// TCP connect time to port 80, 443, then the server's first iperf3 port.
	Ping func(ctx context.Context, host string) (time.Duration, error)
	// OnResult is called after every saved test (metrics).
	OnResult func(model.SpeedTest)
	Log      *slog.Logger
	Now      func() time.Time

	// Tests: the iperf3 run, the RTT check, the sleep and the random source.
	run   func(ctx context.Context, c Config) (Result, error)
	rtt   func(ctx context.Context, host string, port int) (time.Duration, error)
	sleep func(ctx context.Context, d time.Duration) bool
	rand  *rand.Rand
}

// Scheduler runs speed tests on a schedule and on demand.
type Scheduler struct {
	o    Options
	wake chan struct{}

	mu       sync.Mutex
	enabled  bool
	next     time.Time
	manual   bool // a "Test now" is queued
	cur      *RunState
	picked   []string // the nearest servers, by name
	pickedAt time.Time
	rtts     map[string]time.Duration // last RTT check; absent: unreachable
	turn     int
}

// RunState is a running test.
type RunState struct {
	Started   time.Time `json:"started"`
	Direction string    `json:"direction"` // picking, download, upload
	Server    string    `json:"server,omitempty"`
}

// ServerState is one server with its last RTT check.
type ServerState struct {
	Name    string   `json:"name"`
	City    string   `json:"city"`
	Country string   `json:"country"`
	RTTms   *float64 `json:"rtt_ms"`
	Picked  bool     `json:"picked"`
}

// State is the scheduler's state for the API.
type State struct {
	Enabled  bool          `json:"enabled"`
	Interval time.Duration `json:"-"`
	Next     time.Time     `json:"-"`
	Running  *RunState     `json:"running"`
	Custom   string        `json:"custom_server,omitempty"`
	Servers  []ServerState `json:"servers"`
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
	if o.rtt == nil {
		o.rtt = func(ctx context.Context, host string, port int) (time.Duration, error) {
			return serverRTT(ctx, o.Ping, host, port)
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
	if o.Custom != nil {
		o.Servers = []Server{*o.Custom}
		if o.Interval <= 0 {
			o.Interval = MinInterval
		}
	} else {
		o.Interval = max(o.Interval, MinInterval)
	}
	return &Scheduler{o: o, wake: make(chan struct{}, 1), rtts: map[string]time.Duration{}}
}

// Interval is the schedule's interval.
func (s *Scheduler) Interval() time.Duration { return s.o.Interval }

// jittered is iv ± Jitter (never below a minute).
func (s *Scheduler) jittered(iv time.Duration) time.Duration {
	s.mu.Lock()
	j := time.Duration(s.o.rand.Int64N(int64(2*Jitter)+1)) - Jitter
	s.mu.Unlock()
	return max(iv+j, time.Minute)
}

func (s *Scheduler) poke() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Run loads the saved toggle and the past windows, then runs tests until
// ctx ends.
func (s *Scheduler) Run(ctx context.Context) error {
	enabled := s.o.Enabled
	if !enabled && s.o.Store != nil {
		if v, ok, err := s.o.Store.Setting(ctx, SettingKey); err == nil && ok {
			enabled = v == "on"
		}
	}
	if s.o.Store != nil {
		now := s.o.Now()
		if ts, err := s.o.Store.SpeedTests(ctx, now.Add(-time.Hour), now); err == nil {
			for _, t := range ts {
				s.o.Windows.Add(Window{Start: t.Start, End: t.End})
			}
		}
	}
	s.mu.Lock()
	s.enabled = enabled
	if enabled {
		s.next = s.o.Now().Add(firstDelay + s.jitterUp())
	}
	s.mu.Unlock()
	for {
		s.mu.Lock()
		var wait time.Duration = time.Hour
		due, manual := false, s.manual
		if s.enabled && !s.next.IsZero() {
			wait = s.next.Sub(s.o.Now())
			due = wait <= 0
		}
		s.mu.Unlock()
		if manual || due {
			trigger := "schedule"
			if manual {
				trigger = "manual"
			}
			s.runOne(ctx, trigger)
			s.mu.Lock()
			if manual {
				s.manual = false
			}
			if due && s.enabled {
				s.next = s.o.Now().Add(s.jitteredLocked(s.o.Interval))
			}
			s.mu.Unlock()
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

func (s *Scheduler) jitterUp() time.Duration {
	return time.Duration(s.o.rand.Int64N(int64(Jitter) + 1))
}

func (s *Scheduler) jitteredLocked(iv time.Duration) time.Duration {
	j := time.Duration(s.o.rand.Int64N(int64(2*Jitter)+1)) - Jitter
	return max(iv+j, time.Minute)
}

// TestNow queues a test to run immediately. It fails with ErrRunning while
// one runs or is queued.
func (s *Scheduler) TestNow() (time.Time, error) {
	s.mu.Lock()
	if s.manual || s.cur != nil || running.Load() {
		s.mu.Unlock()
		return time.Time{}, ErrRunning
	}
	s.manual = true
	s.mu.Unlock()
	s.poke()
	return s.o.Now(), nil
}

// SetEnabled switches the schedule and saves the choice. It returns the
// next scheduled test (zero when off).
func (s *Scheduler) SetEnabled(ctx context.Context, on bool) (time.Time, error) {
	if s.o.Store != nil {
		v := "off"
		if on {
			v = "on"
		}
		if err := s.o.Store.SetSetting(ctx, SettingKey, v); err != nil {
			return time.Time{}, err
		}
	}
	s.mu.Lock()
	switch {
	case on && !s.enabled:
		s.next = s.o.Now().Add(firstDelay + s.jitterUp())
	case !on:
		s.next = time.Time{}
	}
	s.enabled = on
	next := s.next
	s.mu.Unlock()
	s.poke()
	return next, nil
}

// State reports the schedule, the running test and the servers.
func (s *Scheduler) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := State{Enabled: s.enabled, Interval: s.o.Interval}
	if s.enabled && s.cur == nil {
		st.Next = s.next
	}
	if s.cur != nil {
		c := *s.cur
		st.Running = &c
	}
	if s.o.Custom != nil {
		st.Custom = s.o.Custom.Name
	}
	for _, sv := range s.o.Servers {
		ss := ServerState{Name: sv.Name, City: sv.City, Country: sv.Country, Picked: slices.Contains(s.picked, sv.Name)}
		if d, ok := s.rtts[sv.Name]; ok {
			ms := float64(d) / float64(time.Millisecond)
			ss.RTTms = &ms
		}
		st.Servers = append(st.Servers, ss)
	}
	return st
}

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

// runOne runs one test (picking servers first when due), saves it and
// reports it.
func (s *Scheduler) runOne(ctx context.Context, trigger string) {
	if !running.CompareAndSwap(false, true) {
		return
	}
	defer running.Store(false)
	s.mu.Lock()
	s.cur = &RunState{Started: s.o.Now(), Direction: "picking"}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.cur = nil
		s.mu.Unlock()
	}()
	sv, ok := s.server(ctx)
	if ctx.Err() != nil {
		return
	}
	t := model.SpeedTest{Trigger: trigger, Outcome: model.SpeedUnreachable}
	if !ok {
		now := s.o.Now()
		t.Start, t.End = now, now
		s.save(ctx, &t)
		return
	}
	t.Server = sv.Name
	tctx, cancel := context.WithTimeout(ctx, MaxTestTime)
	defer cancel()
	t.Start = s.o.Now()
	s.o.Windows.Begin(t.Start)
	s.measure(tctx, sv, &t)
	t.End = s.o.Now()
	win := s.o.Windows.End(t.End)
	if ctx.Err() != nil {
		return // shutting down: not a result
	}
	if l := s.o.Load; l != nil {
		set := func(layer string, from, to time.Time, into **float64) {
			if v, ok := l.Median(layer, from, to); ok {
				*into = &v
			}
		}
		set(LayerGateway, win.Start.Add(-idleWindow), win.Start, &t.IdleGatewayMs)
		set(LayerGateway, win.Start, win.End, &t.LoadedGatewayMs)
		set(LayerCall, win.Start.Add(-idleWindow), win.Start, &t.IdleCallMs)
		set(LayerCall, win.Start, win.End, &t.LoadedCallMs)
	}
	s.save(ctx, &t)
}

func (s *Scheduler) save(ctx context.Context, t *model.SpeedTest) {
	if s.o.Store != nil {
		if err := s.o.Store.SaveSpeedTest(ctx, t); err != nil {
			s.o.Log.Warn("speed test: saving the result failed", "err", err)
		}
	}
	args := []any{"server", t.Server, "outcome", t.Outcome, "attempts", t.Attempts, "trigger", t.Trigger}
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

// measure runs download then upload, with busy retries.
func (s *Scheduler) measure(ctx context.Context, sv Server, t *model.SpeedTest) {
	port := 0
	for _, dir := range []Direction{Download, Upload} {
		s.setRun(dir.String(), sv.Name)
		if port != 0 && !s.o.sleep(ctx, relisten) {
			t.Outcome = model.SpeedFailed
			return
		}
		res, p, attempts, outcome := s.direction(ctx, sv, dir, port)
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

// direction runs one direction: first on port (0: a random one), then on
// other random ports while the server is busy, up to MaxAttempts. After a
// download on port (the server answered moments ago), a refused connection
// is the server re-listening, so it is retried like a busy one.
func (s *Scheduler) direction(ctx context.Context, sv Server, dir Direction, port int) (Result, int, int, string) {
	reachable := port != 0
	refused := false
	tried := map[int]bool{}
	for attempt := 1; attempt <= MaxAttempts; attempt++ {
		if attempt > 1 || port == 0 {
			port = s.randomPort(sv, tried)
		}
		tried[port] = true
		res, err := s.o.run(ctx, Config{
			Addr: net.JoinHostPort(sv.Host, strconv.Itoa(port)), Direction: dir, Timeout: DirTimeout,
		})
		switch {
		case err == nil:
			return res, port, attempt, model.SpeedOK
		case ctx.Err() != nil:
			return res, port, attempt, model.SpeedFailed
		case errors.Is(err, ErrUnreachable) && !reachable:
			return res, port, attempt, model.SpeedUnreachable // retrying other ports does not help
		case !errors.Is(err, ErrBusy) && !errors.Is(err, ErrUnreachable):
			s.o.Log.Debug("speed test failed", "server", sv.Name, "port", port, "direction", dir.String(), "err", err)
			return res, port, attempt, model.SpeedFailed
		}
		refused = errors.Is(err, ErrUnreachable)
		if attempt < MaxAttempts && !s.o.sleep(ctx, AttemptGap) {
			return res, port, attempt, model.SpeedFailed
		}
	}
	if refused {
		return Result{}, port, MaxAttempts, model.SpeedUnreachable
	}
	return Result{}, port, MaxAttempts, model.SpeedBusy
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

// server picks the server for this test: the custom one, else the nearer
// two of the catalog by RTT (re-checked every RepickEvery), alternating.
// ok is false when none answered.
func (s *Scheduler) server(ctx context.Context) (Server, bool) {
	if s.o.Custom != nil {
		return *s.o.Custom, true
	}
	s.mu.Lock()
	due := len(s.picked) == 0 || s.o.Now().Sub(s.pickedAt) >= RepickEvery
	s.mu.Unlock()
	if due {
		s.pick(ctx)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.picked) == 0 {
		return Server{}, false
	}
	name := s.picked[s.turn%len(s.picked)]
	s.turn++
	for _, sv := range s.o.Servers {
		if sv.Name == name {
			return sv, true
		}
	}
	return Server{}, false
}

// pick measures the RTT to every server (the minimum of pickConnects
// checks) and keeps the two nearest. A failed check keeps
// the previous pick and retries at the next test.
func (s *Scheduler) pick(ctx context.Context) {
	type r struct {
		name string
		d    time.Duration
	}
	var mu sync.Mutex
	var got []r
	var wg sync.WaitGroup
	for _, sv := range s.o.Servers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			best := time.Duration(-1)
			for range pickConnects {
				d, err := s.o.rtt(ctx, sv.Host, sv.PortMin)
				if err == nil && (best < 0 || d < best) {
					best = d
				}
			}
			if best >= 0 {
				mu.Lock()
				got = append(got, r{sv.Name, best})
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
		s.rtts[x.name] = x.d
	}
	if len(got) == 0 {
		return
	}
	s.picked = nil
	for _, x := range got[:min(2, len(got))] {
		s.picked = append(s.picked, x.name)
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
