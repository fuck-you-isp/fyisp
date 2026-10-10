package speedtest

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/callq"
	"github.com/fuck-you-isp/fyisp/internal/model"
)

// Latency under load: during each direction a Prober pings the ISP edge
// (the first hop on the ISP's side: its queues are where bufferbloat
// lives) and the gateway (the home network: Wi-Fi, the router) every
// 150 ms, so each direction gets dozens of samples. A probe that times
// out counts as lost and as the timeout in the percentiles: dropping it
// would understate the delay.

// Prober measures latency under load during a test.
type Prober interface {
	// Start probes until the run is stopped; Phase sets the direction the
	// following samples belong to ("" between directions).
	Start(ctx context.Context) ProbeRun
}

// ProbeRun is one test's probing.
type ProbeRun interface {
	Phase(dir string)
	Stop() map[string]Loaded // by layer (LayerEdge, LayerGateway)
}

// Loaded is one layer's latency around a test. Milliseconds.
type Loaded struct {
	IdleMs *float64   `json:"idle_ms"`
	Down   *LoadedDir `json:"down,omitempty"`
	Up     *LoadedDir `json:"up,omitempty"`
}

// LoadedDir is one direction's samples: percentiles with lost probes at
// the timeout, and the counts.
type LoadedDir struct {
	P50  float64 `json:"p50_ms"`
	P90  float64 `json:"p90_ms"`
	Sent int     `json:"sent"`
	Lost int     `json:"lost"`
}

// Extra is a test's further measurements, stored as JSON.
type Extra struct {
	Connection string            `json:"connection,omitempty"` // wired, wifi or unknown
	Latency    map[string]Loaded `json:"latency,omitempty"`    // by layer: edge, gateway
	RampMs     [2]float64        `json:"ramp_ms"`              // down, up
}

// ParseExtra reads a test's Extra ("" gives the zero value).
func ParseExtra(s string) Extra {
	var x Extra
	_ = json.Unmarshal([]byte(s), &x)
	return x
}

// LayerEdge is the ISP edge layer of latency under load.
const LayerEdge = "edge"

// FastProber pings Targets' addresses every Interval during a test.
type FastProber struct {
	Targets  func() map[string]netip.Addr // by layer; invalid addresses are skipped
	Ping     func(ctx context.Context, dst netip.Addr, timeout time.Duration) (time.Duration, error)
	Interval time.Duration // default 150 ms
	Timeout  time.Duration // default 1 s
}

type fastRun struct {
	p      *FastProber
	cancel func()
	wg     sync.WaitGroup
	mu     sync.Mutex
	dir    string
	rtts   map[string]map[string][]float64 // layer -> dir -> ms (lost = timeout)
	sent   map[string]map[string]int
	lost   map[string]map[string]int
}

// Start implements Prober.
func (p *FastProber) Start(ctx context.Context) ProbeRun {
	iv, to := p.Interval, p.Timeout
	if iv <= 0 {
		iv = 150 * time.Millisecond
	}
	if to <= 0 {
		to = time.Second
	}
	ctx, cancel := context.WithCancel(ctx)
	r := &fastRun{p: p, cancel: cancel, rtts: map[string]map[string][]float64{}, sent: map[string]map[string]int{}, lost: map[string]map[string]int{}}
	targets := p.Targets()
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		tk := time.NewTicker(iv)
		defer tk.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tk.C:
			}
			r.mu.Lock()
			dir := r.dir
			r.mu.Unlock()
			if dir == "" {
				continue
			}
			for layer, a := range targets {
				if !a.IsValid() {
					continue
				}
				r.wg.Add(1)
				go func() {
					defer r.wg.Done()
					rtt, err := p.Ping(ctx, a, to)
					if ctx.Err() != nil {
						return // stopped: neither sent nor lost
					}
					r.mu.Lock()
					defer r.mu.Unlock()
					add := func(m map[string]map[string]int) {
						if m[layer] == nil {
							m[layer] = map[string]int{}
						}
						m[layer][dir]++
					}
					add(r.sent)
					ms := float64(rtt) / float64(time.Millisecond)
					if err != nil {
						add(r.lost)
						ms = float64(to) / float64(time.Millisecond)
					}
					if r.rtts[layer] == nil {
						r.rtts[layer] = map[string][]float64{}
					}
					r.rtts[layer][dir] = append(r.rtts[layer][dir], ms)
				}()
			}
		}
	}()
	return r
}

func (r *fastRun) Phase(dir string) {
	r.mu.Lock()
	r.dir = dir
	r.mu.Unlock()
}

func (r *fastRun) Stop() map[string]Loaded {
	r.cancel()
	r.wg.Wait()
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]Loaded{}
	for layer, byDir := range r.rtts {
		var l Loaded
		for dir, v := range byDir {
			d := &LoadedDir{P50: round2(callq.Quantile(v, 0.5)), P90: round2(callq.Quantile(v, 0.9)), Sent: r.sent[layer][dir], Lost: r.lost[layer][dir]}
			if dir == Download.String() {
				l.Down = d
			} else {
				l.Up = d
			}
		}
		out[layer] = l
	}
	return out
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

// runEntry runs a queue entry: a test, a sweep or a scheduled slot.
func (s *Scheduler) runEntry(ctx context.Context, e *Entry) {
	defer s.q.done(e, s.o.Now())
	if !running.CompareAndSwap(false, true) {
		return // another scheduler in this process (tests only)
	}
	defer running.Store(false)
	ectx, cancel := context.WithCancel(ctx)
	defer cancel()
	s.q.update(e, func(e *Entry) { e.cancel = cancel })
	switch e.Kind {
	case KindSweep:
		servers := s.o.Catalog.Servers
		s.q.update(e, func(e *Entry) {
			e.Results = nil
			for _, sv := range servers {
				e.Results = append(e.Results, SweepResult{Server: sv.ID, Status: "waiting"})
			}
		})
		for i, sv := range servers {
			if ectx.Err() != nil {
				return
			}
			s.q.update(e, func(e *Entry) {
				e.Progress = Progress{Step: "download", Server: sv.ID, Index: i + 1, Total: len(servers)}
				e.Results[i].Status = "testing"
			})
			t := s.runSteps(ectx, e, "", []Step{{Server: sv, Level: FallbackNone}}, "sweep")
			s.q.update(e, func(e *Entry) {
				r := &e.Results[i]
				if t == nil {
					r.Status = "waiting"
					return
				}
				r.Status, r.DownBps, r.UpBps, r.DownShort, r.UpShort, r.At = t.Outcome, t.DownBps, t.UpBps, t.DownShort, t.UpShort, t.Start
				if t.Outcome == model.SpeedOK {
					r.Status = "done"
				}
				r.Direction = ""
			})
		}
	case KindTest:
		region, steps := s.resolveTest(ectx, e.Target)
		s.runSteps(ectx, e, region, steps, "manual")
	case KindScheduled:
		if e.slot != nil {
			region, steps := s.resolveSlot(ectx, *e.slot)
			s.runSteps(ectx, e, region, steps, "schedule")
		}
	}
}

// resolveTest is Test now's servers: one server, a region's chain, or the
// nearest servers.
func (s *Scheduler) resolveTest(ctx context.Context, id string) (string, []Step) {
	if sv, ok := s.server(id); ok {
		return "", []Step{{Server: sv, Level: FallbackNone}}
	}
	if r, ok := s.region(id); ok {
		return r.ID, r.Chain(s.o.Catalog, s.o.Now(), s.seedNow())
	}
	s.pickIfDue(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	var steps []Step
	for i, id := range s.picked {
		if sv, ok := s.o.Catalog.Server(id); ok {
			lvl := FallbackNone
			if i > 0 {
				lvl = FallbackNearby
			}
			steps = append(steps, Step{Server: sv, Level: lvl})
		}
	}
	return "", steps
}

func (s *Scheduler) seedNow() uint32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seed
}

// resolveSlot is a scheduled slot's servers.
func (s *Scheduler) resolveSlot(ctx context.Context, t target) (string, []Step) {
	switch {
	case t.nearby >= 0:
		s.pickIfDue(ctx)
		s.mu.Lock()
		defer s.mu.Unlock()
		if t.nearby >= len(s.picked) {
			return "", nil
		}
		sv, _ := s.o.Catalog.Server(s.picked[t.nearby])
		return "", []Step{{Server: sv, Level: FallbackNone}}
	case t.region == "far":
		s.pickIfDue(ctx)
		s.mu.Lock()
		far := s.far
		s.mu.Unlock()
		if r, ok := s.region(far); ok {
			return r.ID, r.Chain(s.o.Catalog, s.o.Now(), s.seedNow())
		}
		return "", nil
	case t.region != "":
		if r, ok := s.region(t.region); ok {
			return r.ID, r.Chain(s.o.Catalog, s.o.Now(), s.seedNow())
		}
	case t.server != "":
		if sv, ok := s.server(t.server); ok {
			return "", []Step{{Server: sv, Level: FallbackNone}}
		}
	}
	return "", nil
}

// runSteps runs one test over a chain: the servers in order until one
// measures, within SlotBudget. It saves and returns the result; nil when
// cancelled (the owner, or shutdown): not a result.
func (s *Scheduler) runSteps(ctx context.Context, e *Entry, region string, steps []Step, trigger string) *model.SpeedTest {
	t := model.SpeedTest{Trigger: trigger, Region: region, Outcome: model.SpeedUnreachable, FallbackLevel: FallbackNone, Entry: e.ID}
	if len(steps) == 0 {
		now := s.o.Now()
		t.Start, t.End = now, now
		s.save(ctx, &t, Extra{})
		return &t
	}
	sctx, cancel := context.WithTimeout(ctx, SlotCap)
	defer cancel()
	var x Extra
	if s.o.Conn != nil {
		x.Connection = s.o.Conn()
	}
	var probe ProbeRun
	if s.o.Prober != nil {
		probe = s.o.Prober.Start(sctx)
	}
	t.Start = s.o.Now()
	s.o.Windows.Begin(t.Start)
	for i, st := range steps {
		if i > 0 && s.o.Now().Sub(t.Start) >= SlotBudget {
			break
		}
		t.Server, t.FallbackLevel, t.Fallback, t.Port = st.Server.ID, st.Level, st.Level != FallbackNone, 0
		s.testServer(sctx, e, st.Server, &t, &x, probe)
		if t.Outcome == model.SpeedOK || t.DownBps != nil || sctx.Err() != nil {
			break // measured (a failed upload keeps the download), or out of time
		}
	}
	t.End = s.o.Now()
	win := s.o.Windows.End(t.End)
	var lat map[string]Loaded
	if probe != nil {
		lat = probe.Stop()
	}
	if ctx.Err() != nil {
		return nil // cancelled or shutting down: not a result
	}
	if l := s.o.Load; l != nil {
		for _, layer := range []string{LayerEdge, LayerGateway} {
			if v, ok := l.Median(layer, win.Start.Add(-idleWindow), win.Start); ok {
				ll := lat[layer]
				ll.IdleMs = &v
				if lat == nil {
					lat = map[string]Loaded{}
				}
				lat[layer] = ll
			}
		}
	}
	if len(lat) > 0 {
		x.Latency = lat
		if g, ok := lat[LayerGateway]; ok {
			t.IdleGatewayMs = g.IdleMs
			if g.Down != nil && g.Up != nil {
				v := max(g.Down.P50, g.Up.P50)
				t.LoadedGatewayMs = &v
			}
		}
	}
	s.save(ctx, &t, x)
	return &t
}

func (s *Scheduler) save(ctx context.Context, t *model.SpeedTest, x Extra) {
	if b, err := json.Marshal(x); err == nil && string(b) != `{"ramp_ms":[0,0]}` {
		t.Extra = string(b)
	}
	if sv, ok := s.server(t.Server); ok && sv.CapacityMbps > 0 {
		for _, bps := range []*float64{t.DownBps, t.UpBps} {
			if bps != nil && *bps >= 0.9*sv.CapacityMbps*1e6 {
				t.CappedByServer = true
			}
		}
	}
	s.score(ctx, t)
	if s.o.Store != nil {
		if err := s.o.Store.SaveSpeedTest(ctx, t); err != nil {
			s.o.Log.Warn("speed test: saving the result failed", "err", err)
		}
	}
	args := []any{"region", t.Region, "server", t.Server, "fallback", t.FallbackLevel, "outcome", t.Outcome, "attempts", t.Attempts, "trigger", t.Trigger}
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

// Baseline rules: the p90 of the server's first-choice scheduled ok tests
// of the last BaselineSpan that the server did not cap, once there are at
// least BaselineMin of them.
const (
	BaselineSpan = 14 * 24 * time.Hour
	BaselineMin  = 20
)

// Baseline is the server's baseline (down, up) as of at, false while
// learning.
func Baseline(ts []model.SpeedTest, server string, at time.Time) (down, up float64, ok bool) {
	var d, u []float64
	for _, t := range ts {
		if t.Server != server || t.Trigger != "schedule" || t.Outcome != model.SpeedOK || t.FallbackLevel != FallbackNone ||
			t.CappedByServer || t.Start.Before(at.Add(-BaselineSpan)) || !t.Start.Before(at) {
			continue
		}
		if t.DownBps != nil {
			d = append(d, *t.DownBps)
		}
		if t.UpBps != nil {
			u = append(u, *t.UpBps)
		}
	}
	if len(d) < BaselineMin {
		return 0, 0, false
	}
	down = callq.Quantile(d, 0.9)
	if len(u) >= BaselineMin {
		up = callq.Quantile(u, 0.9)
	}
	return down, up, true
}

// score sets the result relative to the server's baseline.
func (s *Scheduler) score(ctx context.Context, t *model.SpeedTest) {
	if s.o.Store == nil || t.Server == "" || (t.DownBps == nil && t.UpBps == nil) {
		return
	}
	ts, err := s.o.Store.SpeedTests(ctx, t.Start.Add(-BaselineSpan), t.Start)
	if err != nil {
		return
	}
	down, up, ok := Baseline(ts, t.Server, t.Start)
	if !ok {
		return
	}
	if t.DownBps != nil && down > 0 {
		v := math.Round(*t.DownBps/down*1000) / 1000
		t.ScoreDown = &v
	}
	if t.UpBps != nil && up > 0 {
		v := math.Round(*t.UpBps/up*1000) / 1000
		t.ScoreUp = &v
	}
}

// testServer runs download then upload against sv.
func (s *Scheduler) testServer(ctx context.Context, e *Entry, sv Server, t *model.SpeedTest, x *Extra, probe ProbeRun) {
	port := 0
	for i, dir := range []Direction{Download, Upload} {
		s.q.update(e, func(e *Entry) {
			e.Progress.Step, e.Progress.Server = dir.String(), sv.ID
			if e.Progress.Total == 0 {
				e.Progress.Index, e.Progress.Total = 1, 1
			}
			if e.Kind == KindSweep && e.Progress.Index > 0 {
				e.Results[e.Progress.Index-1].Direction = dir.String()
			}
		})
		if port != 0 && !s.o.sleep(ctx, relisten) {
			t.Outcome = model.SpeedFailed
			return
		}
		if probe != nil {
			probe.Phase(dir.String())
		}
		res, p, attempts, outcome := s.direction(ctx, sv, dir, port, t.Start)
		if probe != nil {
			probe.Phase("")
		}
		t.Attempts += attempts
		if p != 0 {
			t.Port, port = p, p
		}
		t.Outcome = outcome
		t.Bytes += res.TotalBytes
		if outcome != model.SpeedOK {
			return
		}
		bps := res.BitsPerSecond()
		var re *int64
		if res.Retransmits >= 0 {
			r := res.Retransmits
			re = &r
		}
		x.RampMs[i] = round2(float64(res.Ramp) / float64(time.Millisecond))
		secs := math.Round(res.Elapsed.Seconds()*100) / 100
		if dir == Download {
			t.DownBps, t.DownRetrans, t.DownShort, t.DownSecs = &bps, re, res.CappedByBytes, secs
		} else {
			t.UpBps, t.UpRetrans, t.UpShort, t.UpSecs = &bps, re, res.CappedByBytes, secs
		}
	}
}

// direction runs one direction: first on port (0: a random one), then,
// while the server is busy or refuses (a stock iperf3 server re-listens
// for a moment after every test), on other random ports 2 s apart, up to
// MaxAttempts and within the slot's budget. A single-port server gets one
// try. A timeout or DNS failure (ErrUnreachable) and protocol errors end
// it at once.
func (s *Scheduler) direction(ctx context.Context, sv Server, dir Direction, port int, slotStart time.Time) (Result, int, int, string) {
	tried := map[int]bool{}
	tries := MaxAttempts
	if sv.SinglePort() {
		tries = 1
	}
	for attempt := 1; ; attempt++ {
		if attempt > 1 || port == 0 {
			port = s.randomPort(sv, tried)
		}
		tried[port] = true
		res, err := s.o.run(ctx, Config{
			Addr: net.JoinHostPort(sv.Host, strconv.Itoa(port)), Direction: dir, Timeout: DirTimeout,
			Duration: MaxMeasure, MinDuration: MinMeasure, MaxBytes: MaxBytes,
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
		if attempt >= tries || s.o.Now().Sub(slotStart)+AttemptGap >= SlotBudget || !s.o.sleep(ctx, AttemptGap) {
			if errors.Is(err, ErrRefused) {
				return res, port, attempt, model.SpeedUnreachable
			}
			return res, port, attempt, model.SpeedBusy
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

// farRegion is mode nearby's automatic far region: another continent on a
// major route from where the nearest server is.
var farRegion = map[string]string{"eu": "us-east", "na": "uk", "sa": "us-east", "me": "uk", "af": "uk", "as": "us-west", "oc": "us-west"}

func (s *Scheduler) pickIfDue(ctx context.Context) {
	s.mu.Lock()
	due := len(s.picked) == 0 || s.o.Now().Sub(s.pickedAt) >= RepickEvery
	s.mu.Unlock()
	if due {
		s.pick(ctx)
	}
}

// pick measures the RTT to every catalog server (the minimum of
// pickConnects checks, pickParallel servers at a time) and picks mode
// nearby's servers: the nearest, then the closest server of other
// providers, preferring the nearest's metro (a same-metro server within
// 1.5× the nearest's RTT plus 10 ms), until nearbyCount providers; and the
// far region. A failed check keeps the previous pick.
func (s *Scheduler) pick(ctx context.Context) {
	type r struct {
		sv Server
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
				got = append(got, r{sv, best})
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
		s.rtts[x.sv.ID] = x.d
	}
	if len(got) == 0 {
		return
	}
	near := got[0]
	picked := []string{near.sv.ID}
	used := map[string]bool{near.sv.Provider: true}
	type cand struct {
		id string
		d  time.Duration
	}
	var cands []cand
	var provs []string
	for _, x := range got {
		if !slices.Contains(provs, x.sv.Provider) {
			provs = append(provs, x.sv.Provider)
		}
	}
	for _, p := range provs {
		if used[p] {
			continue
		}
		var best *r
		for i := range got {
			x := &got[i]
			if x.sv.Provider != p {
				continue
			}
			if best == nil {
				best = x
			}
			if x.sv.Metro == near.sv.Metro && float64(x.d) <= 1.5*float64(near.d)+float64(10*time.Millisecond) {
				best = x
				break
			}
		}
		if best != nil {
			cands = append(cands, cand{best.sv.ID, best.d})
		}
	}
	slices.SortFunc(cands, func(a, b cand) int { return int(a.d - b.d) })
	for _, c := range cands {
		if len(picked) >= nearbyCount {
			break
		}
		picked = append(picked, c.id)
	}
	s.picked, s.far, s.pickedAt = picked, farRegion[near.sv.Geo], s.o.Now()
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

// connectRTT is the time to open (and close) a TCP connection (IPv4).
func connectRTT(ctx context.Context, host string, port int) (time.Duration, error) {
	d := net.Dialer{Timeout: pickTimeout}
	t0 := time.Now()
	c, err := d.DialContext(ctx, "tcp4", net.JoinHostPort(host, strconv.Itoa(port)))
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
