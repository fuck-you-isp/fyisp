package speedtest

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

// TestSchedulerQueue: Test now and sweeps run through the queue with the
// scheduler's servers (a region's rotated chain, every catalog server once)
// and triggers; a sweep's tests are on demand, never in patterns.
func TestSchedulerQueue(t *testing.T) {
	h := newHarness(t, Options{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.s.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	q := h.s.Queue()
	if _, _, err := q.Enqueue(Request{Kind: KindSingle, Target: "Nope", Who: WhoOwner}); !errors.Is(err, ErrUnknownID) {
		t.Errorf("unknown id: %v", err)
	}
	e, _, err := q.Enqueue(Request{Kind: KindSingle, Target: "home", Who: WhoOwner})
	if err != nil || e.Steps != 1 || len(e.Servers) != 1 || e.Servers[0] == "" {
		t.Fatalf("%+v %v", e, err)
	}
	waitFor(t, func() bool { x, _ := q.Get(e.ID); return x.State == StateDone })
	sw, _, err := q.Enqueue(Request{Kind: KindSweep, Who: WhoOwner})
	if err != nil || sw.Steps != len(h.s.o.Catalog.Servers) {
		t.Fatalf("sweep %+v %v", sw, err)
	}
	waitFor(t, func() bool { x, _ := q.Get(sw.ID); return x.State == StateDone })
	x, _ := q.Get(sw.ID)
	h.st.mu.Lock()
	defer h.st.mu.Unlock()
	if len(h.st.tests) != 1+len(h.s.o.Catalog.Servers) || len(x.Tests) != len(h.s.o.Catalog.Servers) {
		t.Fatalf("%d tests, sweep %+v", len(h.st.tests), x)
	}
	if first := h.st.tests[0]; first.Trigger != "manual" || first.Region != "home" || !first.OnDemand() {
		t.Errorf("Test now %+v", first)
	}
	for _, ts := range h.st.tests[1:] {
		if ts.Trigger != "sweep" || ts.Region != "" || ts.FallbackLevel != FallbackNone {
			t.Errorf("sweep test %+v", ts)
		}
	}
}

// TestWireBytes: a test's bytes count every attempt's data phase on the
// wire (ramp-up included), not only the measured window: the share link's
// budget charges what was sent.
func TestWireBytes(t *testing.T) {
	h := newHarness(t, Options{})
	h.setRun(func(c Config, n int) (Result, error) {
		if n == 1 { // the first download attempt fails after its data phase started
			return Result{WireBytes: 7e6}, ErrBusy
		}
		return Result{Direction: c.Direction, Bytes: 10e6, WireBytes: 12e6, Elapsed: time.Second}, nil
	})
	_, _, bytes := h.s.execJob(context.Background(), h.s.slotJob(target{server: "Near", nearby: -1}), "manual")
	if bytes != 7e6+12e6+12e6 {
		t.Errorf("bytes %d, want %d", bytes, int64(7e6+12e6+12e6))
	}
}

// TestModes: off by default; a saved mode, a flag over it; the opt-in
// regions are validated and saved.
func TestModes(t *testing.T) {
	h := newHarness(t, Options{})
	ctx, cancel := context.WithCancel(context.Background())
	go h.s.Run(ctx)
	set := func(k string) string { v, _, _ := h.st.Setting(ctx, k); return v }
	waitFor(t, func() bool { return set(settingSeed) != "" })
	if st := h.s.State(); st.Mode != ModeOff || !st.Next.IsZero() || len(st.Modes) != 3 || st.SetByFlag || len(st.Plans) != 2 {
		t.Fatalf("off by default: %+v", st)
	}
	if _, err := h.s.SetMode(ctx, ModeCustom, nil); !errors.Is(err, ErrMode) {
		t.Errorf("custom without servers: %v", err)
	}
	if _, err := h.s.SetMode(ctx, ModeRegions, []string{"nowhere"}); !errors.Is(err, ErrMode) {
		t.Errorf("unknown region: %v", err)
	}
	next, err := h.s.SetMode(ctx, ModeRegions, []string{"us-east"})
	if err != nil || next.IsZero() || set(SettingKey) != "regions" || set(settingExtra) != "us-east" {
		t.Errorf("regions: %v %v", next, err)
	}
	if st := h.s.State(); st.Mode != ModeRegions || len(st.Rows) != 3 || st.NextRow == "" || st.Rows[2].Why != "added" && st.Rows[0].Why != "added" && st.Rows[1].Why != "added" {
		t.Errorf("state %+v", st)
	}
	cancel()
	h2 := newHarness(t, Options{Mode: ModeNearby})
	h2.st.settings = map[string]string{SettingKey: "off", settingSeed: "42"}
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	go h2.s.Run(ctx2)
	waitFor(t, func() bool { return h2.s.State().Mode == ModeNearby })
	if st := h2.s.State(); !st.SetByFlag || h2.s.seedNow() != 42 {
		t.Errorf("flag over saved: %+v, seed %d", st.SetByFlag, h2.s.seedNow())
	}
}

// TestMask: exactly the probes that ran during a test (from Sample.At to
// their arrival) or within MaskTail after it are blanked.
func TestMask(t *testing.T) {
	w := &Windows{}
	t0 := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	w.Add(Window{Start: t0, End: t0.Add(20 * time.Second)})
	now := t0
	var got []model.Sample
	m := Mask{Inner: model.SinkFunc(func(s model.Sample) { got = append(got, s) }), Windows: w, Now: func() time.Time { return now }}
	gw := model.SeriesKey{Target: "Gateway", Kind: model.KindICMP}
	for _, c := range []struct {
		at, arrive time.Duration // from t0
		masked     bool
	}{
		{-10 * time.Second, -9 * time.Second, false},
		{-2 * time.Second, 100 * time.Millisecond, true},
		{10 * time.Second, 11 * time.Second, true},
		{21 * time.Second, 21500 * time.Millisecond, true}, // the 2 s tail
		{22500 * time.Millisecond, 23 * time.Second, false},
	} {
		now = t0.Add(c.arrive)
		m.Observe(model.Sample{Key: gw, Slot: t0.Add(c.at).Truncate(time.Second), At: t0.Add(c.at), RTT: 30 * time.Millisecond})
		s := got[len(got)-1]
		if masked := s.Lost && s.Reason == model.ReasonGap && s.RTT == 0; masked != c.masked {
			t.Errorf("probe at %v: masked %v, want %v", c.at, masked, c.masked)
		}
	}
	if sh := w.Share(t0.Add(time.Minute), 5*time.Minute); sh < 21.9/300 || sh > 22.1/300 {
		t.Errorf("share %v", sh)
	}
}

// TestAbortTail: a test cut short masks (and stores) AbortTail more than
// one that ended normally.
func TestAbortTail(t *testing.T) {
	var spans [2]time.Duration
	for i, fail := range []bool{false, true} {
		h := newHarness(t, Options{})
		h.setRun(func(c Config, n int) (Result, error) {
			if fail {
				return Result{}, errors.New("protocol error")
			}
			return Result{Direction: c.Direction, Bytes: 1e6, Elapsed: time.Second}, nil
		})
		ts := h.job(t, h.s.slotJob(target{server: "Near", nearby: -1}), "manual")
		spans[i] = ts[0].End.Sub(ts[0].Start)
		if end := h.s.o.Windows.LastEnd(); !end.Equal(ts[0].End) {
			t.Errorf("failed %v: window ends %v, stored %v", fail, end, ts[0].End)
		}
	}
	// The harness clock moves 6 s per run call (and 1 s for the relisten
	// pause): ok makes 2 calls, failed 1.
	if spans[0] != 13*time.Second || spans[1] != 6*time.Second+AbortTail {
		t.Errorf("windows %v (ok) and %v (failed): want 13s and 6s + AbortTail", spans[0], spans[1])
	}
}

// TestSkipAndRest: a skipped server is never tested (chains, sweeps, Test
// now); one busy BusyRun scheduled tests in a row rests for BusyRest.
func TestSkipAndRest(t *testing.T) {
	h := newHarness(t, Options{})
	ctx := context.Background()
	if err := h.s.SetSkip(ctx, []string{"Nope"}); !errors.Is(err, ErrMode) {
		t.Errorf("unknown: %v", err)
	}
	if err := h.s.SetSkip(ctx, []string{"Mid"}); err != nil || h.st.settings[settingSkip] != "Mid" {
		t.Fatalf("%v %v", err, h.st.settings)
	}
	if _, err := h.s.resolve(KindSingle, "Mid"); !errors.Is(err, ErrSkipped) {
		t.Errorf("Test now of a skipped server: %v", err)
	}
	jobs, _ := h.s.resolve(KindSweep, "")
	for _, j := range jobs {
		if j.Chain[0].ID == "Mid" {
			t.Error("a sweep tests a skipped server")
		}
	}
	for range 24 {
		ts := h.region(t, "home")
		if ts.Server == "Mid" {
			t.Fatal("a slot used a skipped server")
		}
		h.mu.Lock()
		h.now = h.now.Add(time.Hour)
		h.mu.Unlock()
	}
	if st := h.s.State(); !st.Servers[slices.IndexFunc(st.Servers, func(s ServerState) bool { return s.ID == "Mid" })].Skipped {
		t.Error("state")
	}
	// Busy three scheduled tests in a row: rests.
	h.setRun(func(c Config, n int) (Result, error) {
		if host, _ := hostPort(c); host == "near.example" {
			return Result{}, ErrBusy
		}
		return Result{Direction: c.Direction, Bytes: 1e6, Elapsed: time.Second}, nil
	})
	for range BusyRun {
		h.slot(t, target{server: "Near"})
	}
	st := h.s.State()
	near := st.Servers[slices.IndexFunc(st.Servers, func(s ServerState) bool { return s.ID == "Near" })]
	if near.RestUntil.Sub(h.clock()) < BusyRest-time.Minute {
		t.Fatalf("rest %v", near.RestUntil)
	}
	if ts := h.job(t, h.s.slotJob(target{server: "Near", nearby: -1}), "schedule"); len(ts) != 0 {
		t.Errorf("a resting server was tested: %+v", ts)
	}
	if ts := h.job(t, h.s.slotJob(target{server: "Near", nearby: -1}), "manual"); len(ts) != 1 {
		t.Error("Test now of a resting server")
	}
}

// TestScheduleThroughQueue (ux-eng's, ported): a late slot is queued, not
// skipped (it is only dropped after waiting ScheduledStale in the queue);
// and a Test now asked for while a test runs is never lost: it runs next.
func TestScheduleThroughQueue(t *testing.T) {
	h := newHarness(t, Options{Mode: ModeRegions})
	release := make(chan struct{})
	h.setRun(func(c Config, n int) (Result, error) {
		<-release
		return Result{Direction: c.Direction, Bytes: 1e6, Elapsed: time.Second}, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.s.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	waitFor(t, func() bool { return !h.s.State().Next.IsZero() })
	st := h.s.State()
	// Late by more than half the spacing: the old scheduler skipped it.
	h.mu.Lock()
	h.now = st.Next.Add(st.Spacing/2 + time.Minute)
	h.mu.Unlock()
	h.s.poke()
	waitFor(t, func() bool { return h.s.Queue().State().Running != nil })
	e, _, err := h.s.Queue().Enqueue(Request{Kind: KindSingle, Target: "away", Who: WhoOwner})
	if err != nil || e.State != StateWaiting {
		t.Fatalf("test now: %+v %v", e, err)
	}
	close(release)
	waitFor(t, func() bool { h.st.mu.Lock(); defer h.st.mu.Unlock(); return len(h.st.tests) >= 2 })
	h.st.mu.Lock()
	defer h.st.mu.Unlock()
	if h.st.tests[0].Trigger != "schedule" || h.st.tests[1].Trigger != "manual" || h.st.tests[1].Region != "away" {
		t.Errorf("tests %+v", h.st.tests)
	}
}

// TestSlotCap: SlotMaxBytes holds over a slot's retries and servers: each
// attempt gets what is left, and none starts once it is spent.
func TestSlotCap(t *testing.T) {
	h := newHarness(t, Options{})
	h.setRun(func(c Config, n int) (Result, error) {
		return Result{WireBytes: 150 << 20}, ErrBusy // busy, after moving data
	})
	h.slot(t, target{server: "Near"})
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.calls) != 2 || h.calls[0].MaxBytes != MaxBytes || h.calls[1].MaxBytes != SlotMaxBytes-150<<20 {
		var caps []int64
		for _, c := range h.calls {
			caps = append(caps, c.MaxBytes)
		}
		t.Errorf("%d attempts, caps %v", len(h.calls), caps)
	}
}

// TestHandoffAddress: the handoff traceroute goes to the address the test
// used (resolved and checked), never a fresh lookup of the name.
func TestHandoffAddress(t *testing.T) {
	h := newHarness(t, Options{})
	ip := netip.MustParseAddr("198.51.100.7")
	h.setRun(func(c Config, n int) (Result, error) {
		return Result{Direction: c.Direction, Bytes: 1e6, Elapsed: time.Second, IP: ip}, nil
	})
	var asked []string
	h.s.o.Handoff = func(_ context.Context, host string) (*model.Handoff, error) {
		asked = append(asked, host)
		return &model.Handoff{ASN: 174}, nil
	}
	if ts := h.slot(t, target{server: "Near"}); ts.Handoff == nil || len(asked) != 1 || asked[0] != "198.51.100.7" {
		t.Errorf("asked %v, handoff %+v", asked, ts.Handoff)
	}
}

// TestSlotTimeRotation: a slot that waited past its hour still uses its
// own hour's provider.
func TestSlotTimeRotation(t *testing.T) {
	h := newHarness(t, Options{})
	r, _ := h.s.region("home")
	at := h.clock().Truncate(time.Hour).Add(50 * time.Minute)
	want := r.Chain(h.s.o.Catalog, at, h.s.seedNow())[0].Server.ID
	later := r.Chain(h.s.o.Catalog, at.Add(time.Hour), h.s.seedNow())[0].Server.ID
	if want == later {
		t.Fatal("pick an hour where the rotation changes")
	}
	h.mu.Lock()
	h.now = at.Add(15 * time.Minute) // the next hour
	h.mu.Unlock()
	if ts := h.slot(t, target{region: "home", at: at}); ts.Server != want {
		t.Errorf("server %s, want %s (the slot's hour), not %s", ts.Server, want, later)
	}
}

// TestSweepGap: a sweep's tests leave SweepGap of unmasked time between
// them (after the mask's tail), so monitoring is never blind for minutes.
// The wait is the queue's Pause, outside the charged Exec; MaskTail covers
// 2 × the slowest RTT the probes under load can see.
func TestSweepGap(t *testing.T) {
	h := newHarness(t, Options{})
	job := h.s.slotJob(target{server: "Near", nearby: -1})
	h.job(t, job, "sweep")
	h.mu.Lock()
	h.slept = nil
	h.mu.Unlock()
	if h.s.pause(context.Background(), job, "manual") != "" || h.s.pause(context.Background(), job, "sweep") != "" {
		t.Fatal("pause")
	}
	h.mu.Lock()
	if len(h.slept) != 1 || h.slept[0] != MaskTail+SweepGap {
		t.Errorf("slept %v before the next sweep test (none before a single test), want %v", h.slept, MaskTail+SweepGap)
	}
	h.mu.Unlock()
	h.slept = nil
	h.job(t, job, "sweep") // Exec itself never waits
	if slices.Contains(h.slept, MaskTail+SweepGap) {
		t.Error("execJob waited the gap: it would be charged to the share link")
	}
	if 2*LoadTimeout > MaskTail {
		t.Errorf("MaskTail %v is under 2 × LoadTimeout", MaskTail)
	}
}

// TestCancelMasksUntilStreamsClose: cancelling a test mid-download (a real
// run against the fake iperf3 server) stores a "cancelled" row without
// speeds (never a result, never counted), and the mask window
// lasts until the run's streams are closed and its goroutines gone (plus
// AbortTail): a probe sent while the streams were still open is masked.
func TestCancelMasksUntilStreamsClose(t *testing.T) {
	f := newFakeServer(t, "stall")
	host, port, _ := net.SplitHostPort(f.addr())
	p, _ := strconv.Atoi(port)
	sv := Server{ID: "Local", Host: host, PortMin: p, PortMax: p, Custom: true, LAN: true}
	st := &MemStore{}
	var returned time.Time
	var mu sync.Mutex
	started := make(chan struct{}, 4)
	s := New(Options{Catalog: &Catalog{byID: map[string]int{}}, Custom: []Server{sv}, Store: st,
		Fake: &Fake{Run: func(ctx context.Context, c Config) (Result, error) {
			started <- struct{}{}
			r, err := Run(ctx, c) // returns once its streams are closed and their goroutines exited
			mu.Lock()
			returned = time.Now()
			mu.Unlock()
			return r, err
		}}})
	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-started; time.Sleep(300 * time.Millisecond); cancel() }()
	id, saved, _ := s.execJob(ctx, s.slotJob(target{server: sv.ID, nearby: -1}), "manual")
	if saved || id != 0 {
		t.Fatalf("a cancelled test was saved (%d)", id)
	}
	mu.Lock()
	ret := returned
	mu.Unlock()
	// A row without speeds keeps the masked window explained.
	ts, _ := st.SpeedTests(context.Background(), time.Time{}, time.Now().Add(time.Hour))
	if len(ts) != 1 || ts[0].Outcome != model.SpeedCancelled || ts[0].DownBps != nil || ts[0].Server != sv.ID || ts[0].End.Before(ret.Add(AbortTail)) {
		t.Fatalf("stored %+v", ts)
	}
	if ts[0].Bytes <= 0 {
		t.Errorf("the cancelled row's bytes: %d", ts[0].Bytes)
	}
	if st := Summarize(ts, nil); st.N != 0 || st.Failed != 0 || st.Busy != 0 {
		t.Errorf("a cancelled test counted: %+v", st)
	}
	end := s.o.Windows.LastEnd()
	if end.Before(ret.Add(AbortTail)) {
		t.Errorf("window ends %v, before the streams closed (%v) plus AbortTail", end, ret)
	}
	if !s.o.Windows.Overlaps(ret.Add(-time.Millisecond), ret.Add(-time.Millisecond)) || !s.o.Windows.Overlaps(ret.Add(AbortTail), ret.Add(AbortTail)) {
		t.Error("probes while the streams were open, or in the abort tail, are not masked")
	}
}

// TestSweepStopsWhenUnreachable: during an outage a sweep stops after
// SweepUnreachable unreachable servers in a row instead of masking the
// monitoring for every server.
func TestSweepStopsWhenUnreachable(t *testing.T) {
	h := newHarness(t, Options{})
	h.setRun(func(c Config, n int) (Result, error) { return Result{}, fmt.Errorf("%w: no route", ErrUnreachable) })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.s.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	e, _, err := h.s.Queue().Enqueue(Request{Kind: KindSweep, Who: WhoOwner})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { x, _ := h.s.Queue().Get(e.ID); return x.State == StateDone })
	x, _ := h.s.Queue().Get(e.ID)
	if x.Ran != SweepUnreachable || x.Steps <= SweepUnreachable || x.Stopped != StopUnreachable {
		t.Errorf("ran %d of %d, stopped %q", x.Ran, x.Steps, x.Stopped)
	}
}

// ctxStore fails a save whose context ended, as SQLite does.
type ctxStore struct{ *MemStore }

func (c ctxStore) SaveSpeedTest(ctx context.Context, t *model.SpeedTest) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return c.MemStore.SaveSpeedTest(ctx, t)
}

// TestCancelAtCompletion: a cancel that lands just as a test completes
// (after its last check, before it is stored) never loses the row or its
// window: the finished test is stored as its result.
func TestCancelAtCompletion(t *testing.T) {
	h := newHarness(t, Options{})
	st := ctxStore{h.st}
	h.s.o.Store = st
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.s.o.Handoff = func(context.Context, string) (*model.Handoff, error) { return nil, nil }
	h.s.o.HandoffCached = func(string) (*model.Handoff, bool) { // runs after the test, before the save
		cancel()
		return nil, false
	}
	h.setRun(func(c Config, n int) (Result, error) {
		return Result{Direction: c.Direction, Bytes: 1e6, Elapsed: time.Second, IP: netip.MustParseAddr("198.51.100.7")}, nil
	})
	id, saved, _ := h.s.execJob(ctx, h.s.slotJob(target{server: "Near", nearby: -1}), "sweep")
	ts, _ := st.SpeedTests(context.Background(), time.Time{}, h.clock().Add(time.Hour))
	if !saved || id == 0 || len(ts) != 1 || ts[0].Outcome != model.SpeedOK || ts[0].DownBps == nil {
		t.Fatalf("saved %v id %d: %+v", saved, id, ts)
	}
	if end := h.s.o.Windows.LastEnd(); !end.Equal(ts[0].End) {
		t.Errorf("window %v, stored %v", end, ts[0].End)
	}
	// A cancel during the test still stores its cancelled row, with a
	// store that refuses cancelled contexts.
	ctx2, cancel2 := context.WithCancel(context.Background())
	h.s.bg.Wait()
	h.s.o.Handoff, h.s.o.HandoffCached = nil, nil
	h.setRun(func(c Config, n int) (Result, error) {
		cancel2()
		return Result{}, context.Canceled
	})
	h.s.execJob(ctx2, h.s.slotJob(target{server: "Near", nearby: -1}), "sweep")
	ts, _ = st.SpeedTests(context.Background(), time.Time{}, h.clock().Add(time.Hour))
	if len(ts) != 2 || ts[1].Outcome != model.SpeedCancelled {
		t.Errorf("cancelled: %+v", ts)
	}
}

// TestAtMostNineConnections: one test never holds more than Streams+1
// connections to a server, across download, the relisten pause, a busy
// retry and upload (counted by the client's dialler and by the server).
func TestAtMostNineConnections(t *testing.T) {
	f := newFakeServer(t, "")
	host, port, _ := net.SplitHostPort(f.addr())
	p, _ := strconv.Atoi(port)
	sv := Server{ID: "Local", Host: host, PortMin: p, PortMax: p + 5, Custom: true, LAN: true}
	var open, peak atomic.Int32
	dial := func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		c, err := d.DialContext(ctx, network, f.addr()) // every port is the fake
		if err != nil {
			return nil, err
		}
		n := open.Add(1)
		for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
		}
		return &dialedConn{Conn: c, open: &open}, nil
	}
	var runs []Direction
	s := New(Options{Catalog: &Catalog{byID: map[string]int{}}, Custom: []Server{sv}, Store: &MemStore{},
		Fake: &Fake{
			Run: func(ctx context.Context, c Config) (Result, error) {
				if c.Direction == Upload && !slices.Contains(runs, Upload) {
					f.busyNext.Store(1) // the upload's first port is busy
				}
				runs = append(runs, c.Direction)
				c.Dial, c.Streams = dial, 8
				c.Duration, c.MinDuration, c.Omit = 300*time.Millisecond, 0, 100*time.Millisecond
				return Run(ctx, c)
			},
			Sleep: func(ctx context.Context, _ time.Duration) bool { return ctx.Err() == nil },
		}})
	id, saved, _ := s.execJob(context.Background(), s.slotJob(target{server: sv.ID, nearby: -1}), "manual")
	if !saved || id == 0 || !slices.Equal(runs, []Direction{Download, Upload, Upload}) {
		t.Fatalf("saved %v, runs %v", saved, runs)
	}
	if peak.Load() > 9 || f.peak.Load() > 9 || peak.Load() < 9 {
		t.Errorf("at most %d open (client), %d (server); want 9", peak.Load(), f.peak.Load())
	}
	if open.Load() != 0 {
		t.Errorf("%d connections left open", open.Load())
	}
}

type dialedConn struct {
	net.Conn
	open *atomic.Int32
	once sync.Once
}

func (c *dialedConn) Close() error {
	c.once.Do(func() { c.open.Add(-1) })
	return c.Conn.Close()
}

// TestEarlyFirstTest: turning tests on (or changing the mode) runs the
// first test 2 minutes later, an extra run of the next grid slot; the grid
// then goes on, never two tests closer than half a spacing. A restart, or
// turning on what is already on, stays on the grid.
func TestEarlyFirstTest(t *testing.T) {
	for _, phase := range []time.Duration{0, 37 * time.Second, 3*time.Minute + 11*time.Second, 6 * time.Minute, 29 * time.Minute} {
		h := newHarness(t, Options{})
		h.now = h.now.Add(phase)
		ctx := context.Background()
		on := h.clock()
		next, err := h.s.SetMode(ctx, ModeRegions, nil)
		if err != nil || next.Sub(on) != firstDelay || !h.s.State().Next.Equal(next) {
			t.Fatalf("phase %v: next %v after turning on at %v (%v)", phase, next, on, err)
		}
		h.s.mu.Lock()
		sp := h.s.spacingLocked(ModeRegions)
		grid := func(at time.Time) bool { // on the install's grid, up to the cycle's jitter
			off := at.Sub(h.s.cycleStart) % sp
			return off == 0
		}
		var runs []time.Time
		var slots []int
		now := on
		for len(runs) < 25 {
			tg, wait := h.s.dueLocked(now)
			if tg != nil {
				runs = append(runs, now)
				slots = append(slots, h.s.slot)
			}
			if wait > 0 {
				now = now.Add(wait)
			}
		}
		if !h.s.next.IsZero() && !grid(h.s.next) {
			t.Errorf("phase %v: off the grid afterwards: %v", phase, h.s.next)
		}
		h.s.mu.Unlock()
		for i := 1; i < len(runs); i++ {
			if d := runs[i].Sub(runs[i-1]); d < sp/2 {
				t.Errorf("phase %v: tests %d and %d %v apart (spacing %v)", phase, i-1, i, d, sp)
			}
		}
		// Turning on what is already on changes nothing.
		h.s.mu.Lock()
		before := h.s.next
		h.s.mu.Unlock()
		if n, _ := h.s.SetMode(ctx, ModeRegions, nil); !n.Equal(before) {
			t.Errorf("phase %v: on again moved the next test: %v -> %v", phase, before, n)
		}
	}
	// A restart: the first test is the grid's, at least firstDelay away.
	h := newHarness(t, Options{})
	h.now = h.now.Add(3 * time.Minute)
	h.s.mu.Lock()
	h.s.mode = ModeRegions
	h.s.restartLocked(h.clock())
	next, base := h.s.next, h.s.cycleBase
	sp := h.s.spacingLocked(ModeRegions)
	h.s.mu.Unlock()
	if next.Sub(h.clock()) < firstDelay || next.Sub(base)%sp != 0 {
		t.Errorf("restart: next %v, cycle %v", next, base)
	}
}

// TestDeferredHandoff: the daily handoff traceroute runs after the test is
// saved, never in its way or the queue's; it fills the row in later, one
// at a time, to the address the test dialled, and ends on shutdown.
func TestDeferredHandoff(t *testing.T) {
	h := newHarness(t, Options{})
	h.setRun(func(c Config, n int) (Result, error) {
		return Result{Direction: c.Direction, Bytes: 1e6, Elapsed: time.Second, IP: netip.MustParseAddr("198.51.100.7")}, nil
	})
	release := make(chan struct{})
	var running, peak atomic.Int32
	var hosts []string
	var mu sync.Mutex
	h.s.o.Handoff = func(ctx context.Context, host string) (*model.Handoff, error) {
		n := running.Add(1)
		defer running.Add(-1)
		if n > peak.Load() {
			peak.Store(n)
		}
		mu.Lock()
		hosts = append(hosts, host)
		mu.Unlock()
		select { // a slow traceroute
		case <-release:
			return &model.Handoff{ASN: 6939, Name: "HURRICANE", ISPASN: 64500}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(5 * time.Second):
			return nil, errors.New("too slow")
		}
	}
	t0 := time.Now()
	id, saved, _ := h.s.execJob(context.Background(), h.s.slotJob(target{server: "Near", nearby: -1}), "manual")
	id2, _, _ := h.s.execJob(context.Background(), h.s.slotJob(target{server: "Near", nearby: -1}), "manual")
	if !saved || id == 0 || id2 == 0 || time.Since(t0) > time.Second {
		t.Fatalf("saved %v %d %d after %v", saved, id, id2, time.Since(t0))
	}
	ts, _ := h.st.SpeedTests(context.Background(), time.Time{}, h.clock().Add(time.Hour))
	if len(ts) != 2 || ts[0].Handoff != nil {
		t.Fatalf("saved before the traceroute: %+v", ts)
	}
	close(release)
	waitFor(t, func() bool {
		ts, _ := h.st.SpeedTests(context.Background(), time.Time{}, h.clock().Add(time.Hour))
		return ts[0].Handoff != nil && ts[0].Handoff.ASN == 6939
	})
	h.s.bg.Wait()
	mu.Lock()
	defer mu.Unlock()
	if peak.Load() != 1 || len(hosts) != 1 || hosts[0] != "198.51.100.7" {
		t.Errorf("traceroutes: at most %d at once, to %v", peak.Load(), hosts)
	}
	// A cached answer is taken at once, without a traceroute.
	h.s.o.HandoffCached = func(host string) (*model.Handoff, bool) { return &model.Handoff{ASN: 1299, Name: "TWELVE99"}, true }
	if r := h.job(t, h.s.slotJob(target{server: "Near", nearby: -1}), "manual"); len(r) != 1 || r[0].Handoff == nil || r[0].Handoff.ASN != 1299 {
		t.Errorf("cached: %+v", r)
	}
	// Shutdown ends a traceroute that's running.
	h2 := newHarness(t, Options{})
	h2.setRun(func(c Config, n int) (Result, error) {
		return Result{Direction: c.Direction, Bytes: 1e6, Elapsed: time.Second, IP: netip.MustParseAddr("198.51.100.7")}, nil
	})
	h2.s.o.Handoff = func(ctx context.Context, _ string) (*model.Handoff, error) { <-ctx.Done(); return nil, ctx.Err() }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h2.s.Run(ctx); close(done) }()
	h2.s.execJob(context.Background(), h2.s.slotJob(target{server: "Near", nearby: -1}), "manual")
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Error("shutdown waited for the traceroute")
	}
}
