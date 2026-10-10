package speedtest

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

// memStore is an in-memory Store.
type memStore struct {
	mu       sync.Mutex
	tests    []model.SpeedTest
	settings map[string]string
}

func (m *memStore) SaveSpeedTest(_ context.Context, t *model.SpeedTest) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	t.ID = int64(len(m.tests) + 1)
	m.tests = append(m.tests, *t)
	return nil
}

func (m *memStore) SpeedTests(_ context.Context, from, to time.Time) ([]model.SpeedTest, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []model.SpeedTest
	for _, t := range m.tests {
		if !t.Start.Before(from) && !t.Start.After(to) {
			out = append(out, t)
		}
	}
	return out, nil
}

func (m *memStore) Setting(_ context.Context, k string) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.settings[k]
	return v, ok, nil
}

func (m *memStore) SetSetting(_ context.Context, k, v string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.settings == nil {
		m.settings = map[string]string{}
	}
	m.settings[k] = v
	return nil
}

var testServers = []Server{
	{Name: "Far", Host: "far.example", PortMin: 5200, PortMax: 5209},
	{Name: "Near", Host: "near.example", PortMin: 5201, PortMax: 5210},
	{Name: "Mid", Host: "mid.example", PortMin: 9204, PortMax: 9240},
	{Name: "Dead", Host: "dead.example", PortMin: 5200, PortMax: 5200},
}

// harness wires a Scheduler to stubs: rtts per host, a scripted run.
type harness struct {
	s     *Scheduler
	st    *memStore
	now   time.Time
	mu    sync.Mutex
	rtt   map[string]time.Duration
	calls []Config
	slept []time.Duration
	run   func(c Config, n int) (Result, error)
}

func newHarness(t *testing.T, o Options) *harness {
	h := &harness{st: &memStore{}, now: time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC),
		rtt: map[string]time.Duration{"far.example": 90 * time.Millisecond, "near.example": 10 * time.Millisecond, "mid.example": 30 * time.Millisecond}}
	h.run = func(c Config, n int) (Result, error) {
		return Result{Direction: c.Direction, Bytes: 125_000_000, Elapsed: 9 * time.Second, Retransmits: 3}, nil
	}
	if o.Servers == nil && o.Custom == nil {
		o.Servers = testServers
	}
	o.Store = h.st
	o.Now = func() time.Time { h.mu.Lock(); defer h.mu.Unlock(); return h.now }
	o.rand = rand.New(rand.NewPCG(1, 2))
	o.rtt = func(_ context.Context, host string, port int) (time.Duration, error) {
		h.mu.Lock()
		defer h.mu.Unlock()
		if d, ok := h.rtt[host]; ok {
			return d, nil
		}
		return 0, errors.New("no route")
	}
	o.sleep = func(_ context.Context, d time.Duration) bool {
		h.mu.Lock()
		h.slept = append(h.slept, d)
		h.now = h.now.Add(d)
		h.mu.Unlock()
		return true
	}
	o.run = func(_ context.Context, c Config) (Result, error) {
		h.mu.Lock()
		h.calls = append(h.calls, c)
		n := len(h.calls)
		h.now = h.now.Add(10 * time.Second)
		h.mu.Unlock()
		return h.run(c, n)
	}
	h.s = New(o)
	return h
}

func (h *harness) test(t *testing.T) model.SpeedTest {
	t.Helper()
	n := len(h.st.tests)
	h.s.runOne(context.Background(), "schedule")
	if len(h.st.tests) != n+1 {
		t.Fatalf("%d tests saved", len(h.st.tests)-n)
	}
	return h.st.tests[n]
}

func hostOf(c Config) (string, int) {
	h, p, _ := net.SplitHostPort(c.Addr)
	n, _ := strconv.Atoi(p)
	return h, n
}

func TestIntervalFloor(t *testing.T) {
	if iv := New(Options{Interval: 5 * time.Minute}).Interval(); iv != MinInterval {
		t.Errorf("catalog servers: %v, want the 15m floor", iv)
	}
	if iv := New(Options{Interval: time.Hour}).Interval(); iv != time.Hour {
		t.Errorf("1h: %v", iv)
	}
	c, _ := ParseCustom("10.0.0.5:5201")
	if iv := New(Options{Interval: 5 * time.Minute, Custom: &c}).Interval(); iv != 5*time.Minute {
		t.Errorf("custom server: %v, want no floor", iv)
	}
}

func TestJitter(t *testing.T) {
	s := New(Options{})
	lo, hi := time.Hour, time.Duration(0)
	for range 2000 {
		d := s.jittered(MinInterval)
		lo, hi = min(lo, d), max(hi, d)
		if d < MinInterval-Jitter || d > MinInterval+Jitter {
			t.Fatalf("%v outside ±%v", d, Jitter)
		}
	}
	if hi-lo < 100*time.Second {
		t.Errorf("jitter spread %v..%v", lo, hi)
	}
}

// TestAlternation: the two nearest servers by RTT, alternating; re-picked
// after 24 hours.
func TestAlternation(t *testing.T) {
	h := newHarness(t, Options{})
	var got []string
	for range 4 {
		got = append(got, h.test(t).Server)
	}
	if fmt.Sprint(got) != "[Near Mid Near Mid]" {
		t.Errorf("servers %v", got)
	}
	st := h.s.State()
	if len(st.Servers) != 4 || !st.Servers[1].Picked || !st.Servers[2].Picked || st.Servers[0].Picked || st.Servers[3].RTTms != nil || *st.Servers[1].RTTms != 10 {
		t.Errorf("state %+v", st.Servers)
	}
	// A day later Far is nearest: the pick changes.
	h.mu.Lock()
	h.now = h.now.Add(RepickEvery)
	h.rtt["far.example"] = time.Millisecond
	h.mu.Unlock()
	if s := h.test(t).Server; s != "Far" && s != "Near" {
		t.Errorf("after the re-pick: %s", s)
	}
	if st := h.s.State(); !st.Servers[0].Picked || st.Servers[2].Picked {
		t.Errorf("re-pick %+v", st.Servers)
	}
}

func TestResult(t *testing.T) {
	h := newHarness(t, Options{})
	r := h.test(t)
	if r.Outcome != model.SpeedOK || r.Attempts != 2 || r.DownBps == nil || r.UpBps == nil {
		t.Errorf("%+v", r)
	}
	if *r.DownBps < 111e6 || *r.DownBps > 112e6 || *r.UpBps < 111e6 || *r.DownRetrans != 3 {
		t.Errorf("down %v up %v retrans %v", *r.DownBps, *r.UpBps, *r.DownRetrans)
	}
	// Download then upload, on the same port, with the hard timeout.
	if len(h.calls) != 2 || h.calls[0].Direction != Download || h.calls[1].Direction != Upload || h.calls[0].Addr != h.calls[1].Addr ||
		h.calls[0].Timeout != DirTimeout {
		t.Errorf("calls %+v", h.calls)
	}
	if host, p := hostOf(h.calls[0]); host != "near.example" || p < 5201 || p > 5210 || p != r.Port {
		t.Errorf("addr %s, port %d", h.calls[0].Addr, r.Port)
	}
	if r.End.Sub(r.Start) != 20*time.Second || r.Trigger != "schedule" {
		t.Errorf("window %v..%v", r.Start, r.End)
	}
}

// TestBusy: busy ports are retried on other random ports 2 s apart, up to 3
// attempts; all busy is "busy", never a slow result; unreachable stops.
func TestBusy(t *testing.T) {
	h := newHarness(t, Options{})
	h.run = func(c Config, n int) (Result, error) {
		if n <= 2 {
			return Result{}, ErrBusy
		}
		return Result{Bytes: 1e6, Elapsed: time.Second}, nil
	}
	r := h.test(t)
	if r.Outcome != model.SpeedOK || r.Attempts != 4 || len(h.slept) != 2 || h.slept[0] != AttemptGap {
		t.Fatalf("%+v slept %v", r, h.slept)
	}
	_, p1 := hostOf(h.calls[0])
	_, p2 := hostOf(h.calls[1])
	_, p3 := hostOf(h.calls[2])
	if p1 == p2 || p2 == p3 || p1 == p3 {
		t.Errorf("ports %d %d %d not distinct", p1, p2, p3)
	}

	h.calls, h.slept = nil, nil
	h.run = func(Config, int) (Result, error) { return Result{}, ErrBusy }
	r = h.test(t)
	if r.Outcome != model.SpeedBusy || r.Attempts != MaxAttempts || r.DownBps != nil || r.UpBps != nil || len(h.calls) != MaxAttempts || len(h.slept) != 2 {
		t.Errorf("all busy: %+v, %d calls", r, len(h.calls))
	}

	h.calls = nil
	h.run = func(Config, int) (Result, error) { return Result{}, fmt.Errorf("%w: refused", ErrUnreachable) }
	if r = h.test(t); r.Outcome != model.SpeedUnreachable || len(h.calls) != 1 {
		t.Errorf("unreachable: %+v, %d calls", r, len(h.calls))
	}

	// Download fine, upload fails: the download is kept, the outcome is the upload's.
	h.calls = nil
	h.run = func(c Config, n int) (Result, error) {
		if c.Direction == Upload {
			return Result{}, errors.New("protocol error")
		}
		return Result{Bytes: 1e6, Elapsed: time.Second}, nil
	}
	if r = h.test(t); r.Outcome != model.SpeedFailed || r.DownBps == nil || r.UpBps != nil {
		t.Errorf("upload failed: %+v", r)
	}
}

func TestNoServer(t *testing.T) {
	h := newHarness(t, Options{})
	h.rtt = map[string]time.Duration{}
	if r := h.test(t); r.Outcome != model.SpeedUnreachable || len(h.calls) != 0 {
		t.Errorf("%+v", r)
	}
	c, _ := ParseCustom("iperf.example:5201")
	h = newHarness(t, Options{Custom: &c})
	if r := h.test(t); r.Server != "iperf.example:5201" || h.calls[0].Addr != "iperf.example:5201" {
		t.Errorf("custom: %+v %v", r, h.calls)
	}
}

// TestWindowsAndLoad: the test window masks probes that overlapped it and
// idle vs loaded RTT come from the tap.
func TestWindowsAndLoad(t *testing.T) {
	p := &model.Profile{Targets: []model.Target{
		{Name: "Gateway", Layer: model.LayerGateway, Kinds: []model.ProbeKind{model.KindICMP}},
		{Name: "Meet-STUN", Kinds: []model.ProbeKind{model.KindUDP}, Port: 19302, Tags: []string{"udp-path"}},
		{Name: "Twilio-jp1", Kinds: []model.ProbeKind{model.KindUDP}, Port: 3478, Tags: []string{"udp-path"}, Geo: "as"},
	}}
	w := &Windows{}
	var h *harness
	var tap *LoadTap
	gw := model.SeriesKey{Target: "Gateway", Kind: model.KindICMP}
	call := model.SeriesKey{Target: "Meet-STUN", Kind: model.KindUDP}
	feed := func(k model.SeriesKey, ms float64, n int) {
		for range n {
			tap.Observe(model.Sample{Key: k, RTT: time.Duration(ms * float64(time.Millisecond))})
		}
	}
	h = newHarness(t, Options{Windows: w})
	tap = NewLoadTap(p, h.s.o.Now)
	h.s.o.Load = tap
	h.mu.Lock()
	h.now = h.now.Add(-30 * time.Second)
	h.mu.Unlock()
	feed(gw, 2, 10)
	feed(call, 15, 5)
	feed(model.SeriesKey{Target: "Twilio-jp1", Kind: model.KindUDP}, 300, 5) // not the core path
	h.mu.Lock()
	h.now = h.now.Add(30 * time.Second)
	h.mu.Unlock()
	h.run = func(c Config, n int) (Result, error) {
		feed(gw, 40, 5)
		feed(call, 80, 3)
		return Result{Bytes: 1e6, Elapsed: time.Second}, nil
	}
	r := h.test(t)
	if *r.IdleGatewayMs != 2 || *r.LoadedGatewayMs != 40 || *r.IdleCallMs != 15 || *r.LoadedCallMs != 80 {
		t.Errorf("load %v %v %v %v", *r.IdleGatewayMs, *r.LoadedGatewayMs, *r.IdleCallMs, *r.LoadedCallMs)
	}

	var got []model.Sample
	m := Mask{Inner: model.SinkFunc(func(s model.Sample) { got = append(got, s) }), Windows: w,
		Interval: func(model.SeriesKey) time.Duration { return 5 * time.Second }}
	for _, at := range []time.Time{
		r.Start.Add(-time.Minute),         // before: kept
		r.Start.Add(-8 * time.Second),     // fired before, may have ended inside (5s + 6s margin): masked
		r.Start.Add(10 * time.Second),     // inside: masked
		r.End.Add(time.Second),            // just after the end: kept
		r.End.Add(maskMargin + time.Hour), // long after: kept
	} {
		m.Observe(model.Sample{Key: gw, Slot: at, RTT: 30 * time.Millisecond})
	}
	want := []bool{false, true, true, false, false}
	for i, s := range got {
		masked := s.Lost && s.Reason == model.ReasonGap && s.RTT == 0
		if masked != want[i] {
			t.Errorf("sample %d at %v: masked %v, want %v", i, s.Slot.Sub(r.Start), masked, want[i])
		}
	}
	// A running test masks everything from its start on.
	w.Begin(r.End.Add(time.Hour))
	if !w.Overlaps(r.End.Add(2*time.Hour), r.End.Add(2*time.Hour)) || w.Overlaps(r.End.Add(10*time.Minute), r.End.Add(20*time.Minute)) {
		t.Error("running window")
	}
}

func TestTestNowAndToggle(t *testing.T) {
	h := newHarness(t, Options{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { h.s.Run(ctx); close(done) }()
	if st := h.s.State(); st.Enabled || !st.Next.IsZero() {
		t.Fatalf("off by default: %+v", st)
	}
	release := make(chan struct{})
	h.run = func(c Config, n int) (Result, error) {
		<-release
		return Result{Bytes: 1e6, Elapsed: time.Second}, nil
	}
	if _, err := h.s.TestNow(); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.TestNow(); !errors.Is(err, ErrRunning) {
		t.Errorf("second: %v", err)
	}
	for h.s.State().Running == nil || h.s.State().Running.Direction == "picking" {
		time.Sleep(5 * time.Millisecond)
	}
	if st := h.s.State(); st.Running.Server != "Near" || st.Running.Direction != "download" {
		t.Errorf("running %+v", st.Running)
	}
	close(release)
	for h.s.State().Running != nil {
		time.Sleep(5 * time.Millisecond)
	}
	if len(h.st.tests) != 1 || h.st.tests[0].Trigger != "manual" {
		t.Fatalf("tests %+v", h.st.tests)
	}
	next, err := h.s.SetEnabled(ctx, true)
	if err != nil || next.Sub(h.s.o.Now()) < firstDelay || next.Sub(h.s.o.Now()) > firstDelay+Jitter || h.st.settings[SettingKey] != "on" {
		t.Errorf("enable: %v %v %v", next, err, h.st.settings)
	}
	if st := h.s.State(); !st.Enabled || !st.Next.Equal(next) {
		t.Errorf("state %+v", st)
	}
	cancel()
	<-done
	// The saved toggle is picked up at the next start.
	h2 := newHarness(t, Options{})
	h2.st.settings = map[string]string{SettingKey: "on"}
	ctx2, cancel2 := context.WithCancel(context.Background())
	go h2.s.Run(ctx2)
	for !h2.s.State().Enabled {
		time.Sleep(5 * time.Millisecond)
	}
	cancel2()
}

func TestParseServers(t *testing.T) {
	ss, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	if len(ss) != 7 || ss[0].Name != "Clouvider-London" || ss[0].PortMin != 5200 || ss[0].PortMax != 5209 || ss[6].PortMax != 9240 {
		t.Errorf("%+v", ss)
	}
	for want, y := range map[string]string{
		"want first-last":   "sources: [x]\nservers: [{name: a, host: a.example, ports: 9-3, country: GB}]",
		"private address":   "sources: [x]\nservers: [{name: a, host: 10.0.0.1, ports: 1-2, country: GB}]",
		"duplicate name":    "sources: [x]\nservers: [{name: a, host: a.example, ports: 1, country: GB}, {name: a, host: b.example, ports: 1, country: GB}]",
		"sources":           "servers: [{name: a, host: a.example, ports: 1, country: GB}]",
		"field bogus":       "sources: [x]\nbogus: 1\nservers: []",
		"country \"Britain": "sources: [x]\nservers: [{name: a, host: a.example, ports: 1, country: Britain}]",
	} {
		if _, err := ParseServers([]byte(y)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v", want, err)
		}
	}
	for _, bad := range []string{"host", "host:0", ":5201", "a b:1"} {
		if _, err := ParseCustom(bad); err == nil {
			t.Errorf("custom %q accepted", bad)
		}
	}
	if c, err := ParseCustom("192.168.1.10:5201"); err != nil || !c.Custom || c.PortMin != 5201 {
		t.Errorf("LAN server: %+v %v", c, err)
	}
}
