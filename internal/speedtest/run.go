package speedtest

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// Extra is a test's further measurements, stored as JSON.
type Extra struct {
	RampMs [2]float64 `json:"ramp_ms"` // down, up: the ramp-up left out
	// Bridged: the "ISP edge" had a private address, so fyisp most likely
	// runs behind another router (a container's bridge network, a VM, a
	// second router): the edge may be the home router, and the link type
	// is not this machine's.
	Bridged bool `json:"bridged,omitempty"`
}

// ParseExtra reads a test's Extra ("" gives the zero value).
func ParseExtra(s string) Extra {
	var x Extra
	_ = json.Unmarshal([]byte(s), &x)
	return x
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

// resolve turns a queue request into its tests (QueueOptions.Resolve): a
// sweep is every server once (the catalog, then own servers); single is
// one server, a region's rotated chain, or ("") the nearest servers.
func (s *Scheduler) resolve(kind Kind, id string) ([]Job, error) {
	switch kind {
	case KindSweep:
		var out []Job
		for _, sv := range s.o.Catalog.Servers {
			if !s.skipped(sv.ID) {
				out = append(out, s.slotJob(target{server: sv.ID, nearby: -1}))
			}
		}
		for _, sv := range s.o.Custom {
			if _, ok := s.o.Catalog.Server(sv.ID); !ok && !s.skipped(sv.ID) {
				out = append(out, s.slotJob(target{server: sv.ID, nearby: -1}))
			}
		}
		return out, nil
	case KindSingle:
		if id != "" {
			if _, ok := s.server(id); !ok {
				if _, ok := s.region(id); !ok {
					return nil, ErrUnknownID
				}
			} else if s.skipped(id) {
				return nil, ErrSkipped
			}
		}
		j := Job{steps: func(ctx context.Context) (string, []Step) { return s.resolveTest(ctx, id) }}
		if sv, ok := s.server(id); ok {
			j.Chain = []Server{sv}
		} else if r, ok := s.region(id); ok {
			j.Region = r.ID
			for _, st := range r.Chain(s.o.Catalog, s.o.Now(), s.seedNow()) {
				j.Chain = append(j.Chain, st.Server)
			}
		}
		return []Job{j}, nil
	}
	return nil, fmt.Errorf("speed test queue: kind %q", kind)
}

// slotJob is a scheduled slot's (or one server's) job: its servers are
// resolved when it runs, by the slot's nominal hour (t.at; zero: now).
func (s *Scheduler) slotJob(t target) Job {
	j := Job{Region: t.region, steps: func(ctx context.Context) (string, []Step) { return s.resolveSlot(ctx, t) }}
	if sv, ok := s.server(t.server); ok {
		j.Chain = []Server{sv}
	} else if r, ok := s.region(t.region); ok {
		for _, st := range r.Chain(s.o.Catalog, cmp.Or(t.at, s.o.Now()), s.seedNow()) {
			j.Chain = append(j.Chain, st.Server)
		}
	}
	return j
}

// pause runs before each queued job (QueueOptions.Pause, never charged to
// the share link's time cap: the line is neither tested nor masked then).
// A sweep's tests would blind the monitoring for minutes back to back: it
// leaves the probes SweepGap of unmasked time after the last window's
// tail. StopCancelled: ctx ended while waiting.
//
// A sweep also stops (StopUnreachable) after SweepUnreachable unreachable
// servers in a row: during an outage it would only mask the monitoring.
func (s *Scheduler) pause(ctx context.Context, _ Job, trigger string) string {
	if trigger != "sweep" && trigger != "public_sweep" {
		return ""
	}
	s.mu.Lock()
	stop := s.sweepUnreach >= SweepUnreachable
	if stop {
		s.sweepUnreach = 0
	}
	s.mu.Unlock()
	if stop {
		s.o.Log.Info("speed test: sweep stopped: the servers stopped answering")
		return StopUnreachable
	}
	if last := s.o.Windows.LastEnd(); !last.IsZero() {
		if d := last.Add(MaskTail + SweepGap).Sub(s.o.Now()); d > 0 && !s.o.sleep(ctx, d) {
			return StopCancelled
		}
	}
	return ""
}

// execJob runs one queued test (QueueOptions.Exec): the stored test's ID,
// whether it was saved, and the bytes on the wire, every attempt's.
func (s *Scheduler) execJob(ctx context.Context, j Job, trigger string) (int64, bool, int64) {
	region, steps := j.Region, []Step(nil)
	if j.steps != nil {
		region, steps = j.steps(ctx)
	} else {
		for i, sv := range j.Chain {
			lvl := FallbackNone
			if i > 0 {
				lvl = FallbackSameMetro
			}
			steps = append(steps, Step{Server: sv, Level: lvl})
		}
	}
	if n := len(steps); n > 0 {
		if steps = s.filter(steps, trigger == "schedule"); len(steps) == 0 {
			return 0, false, 0 // every server skipped or resting: no test, not a failure
		}
	}
	t, saved, bytes := s.runSteps(ctx, region, steps, trigger)
	s.mu.Lock()
	switch {
	case trigger != "sweep" && trigger != "public_sweep":
		s.sweepUnreach = 0
	case t != nil && t.Outcome == model.SpeedUnreachable:
		s.sweepUnreach++
	case t != nil:
		s.sweepUnreach = 0
	}
	s.mu.Unlock()
	if t == nil {
		return 0, false, bytes
	}
	return t.ID, saved, bytes
}

// live is the running test's direction and server and its expected end
// (QueueOptions.Live).
type live struct {
	dir, server string
	end         time.Time
}

func (s *Scheduler) live() (string, string, time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur == nil {
		return "", "", time.Time{}
	}
	return s.cur.dir, s.cur.server, s.cur.end
}

func (s *Scheduler) setLive(dir, server string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur == nil {
		s.cur = &live{end: s.o.Now().Add(EstJob)}
	}
	s.cur.dir, s.cur.server = dir, server
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
			return r.ID, r.Chain(s.o.Catalog, cmp.Or(t.at, s.o.Now()), s.seedNow())
		}
		return "", nil
	case t.region != "":
		if r, ok := s.region(t.region); ok {
			return r.ID, r.Chain(s.o.Catalog, cmp.Or(t.at, s.o.Now()), s.seedNow())
		}
	case t.server != "":
		if sv, ok := s.server(t.server); ok {
			return "", []Step{{Server: sv, Level: FallbackNone}}
		}
	}
	return "", nil
}

// runSteps runs one test over a chain: the servers in order until one
// measures, within SlotBudget. It saves and returns the result (and
// whether it was stored) and the bytes on the wire; nil when cancelled
// (the owner, or shutdown): not a result.
func (s *Scheduler) runSteps(ctx context.Context, region string, steps []Step, trigger string) (*model.SpeedTest, bool, int64) {
	defer func() {
		s.mu.Lock()
		s.cur = nil
		s.mu.Unlock()
	}()
	s.setLive("picking", "")
	t := model.SpeedTest{Trigger: trigger, Region: region, Outcome: model.SpeedUnreachable, FallbackLevel: FallbackNone}
	if ctx.Err() != nil {
		return nil, false, 0
	}
	if len(steps) == 0 {
		now := s.o.Now()
		t.Start, t.End = now, now
		return &t, s.save(ctx, &t, Extra{}), 0
	}
	sctx, cancel := context.WithTimeout(ctx, SlotCap)
	defer cancel()
	var x Extra
	t.Conn = model.ConnUnknown
	if s.o.Conn != nil {
		t.Conn = s.o.Conn()
	}
	lr := s.loadRun()
	if lr != nil {
		defer lr.p.Close()
	}
	if s.o.Path != nil {
		if _, edge := s.o.Path(); edge.IsValid() && edge.Unmap().IsPrivate() { // RFC 1918 (not CGNAT: some ISPs number their edge there)
			x.Bridged = true
		}
	}
	t.Start = s.o.Now()
	s.o.Windows.Begin(t.Start)
	var used Server
	var ip netip.Addr // the address the measuring server was dialled at (checked)
	left := int64(SlotMaxBytes)
	for i, st := range steps {
		if i > 0 && s.o.Now().Sub(t.Start) >= SlotBudget {
			break
		}
		t.Server, t.FallbackLevel, t.Fallback, t.Port, used = st.Server.ID, st.Level, st.Level != FallbackNone, 0, st.Server
		ip = netip.Addr{}
		s.testServer(sctx, st.Server, &t, &x, lr, &ip, &left)
		if trigger == "schedule" && sctx.Err() == nil {
			s.noteBusy(st.Server.ID, t.Outcome == model.SpeedBusy)
		}
		if t.Outcome == model.SpeedOK || t.DownBps != nil || sctx.Err() != nil {
			break // measured (a failed upload keeps the download), or out of time
		}
	}
	t.End = s.o.Now()
	// The window ends when the test's traffic has: every direction's Run
	// returned (its streams closed, SO_LINGER 0 on an abort, and their
	// goroutines exited), plus, after a cut-short test, the data still in
	// flight (AbortTail). MaskTail covers the queues draining: it is at
	// least 2 × the loaded RTT the probes can see (lost at LoadTimeout).
	end := t.End
	if sctx.Err() != nil || t.Outcome == model.SpeedFailed {
		end = end.Add(AbortTail)
	}
	win := s.o.Windows.End(end)
	t.End = end // the stored window covers what was masked
	if ctx.Err() != nil {
		// Cancelled or shutting down: not a result, but a row without
		// speeds keeps its masked window explained. Never linked to a
		// sweep (saved false).
		s.saveCancelled(ctx, t, used)
		return nil, false, t.Bytes
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
	if s.o.Handoff != nil && t.Outcome != model.SpeedUnreachable && ip.IsValid() {
		// The traceroute goes to the address the test used, already
		// resolved and checked (never a fresh lookup of the name).
		hctx, cancel := context.WithTimeout(ctx, handoffBudget)
		h, err := s.o.Handoff(hctx, ip.String())
		cancel()
		if err != nil {
			s.o.Log.Debug("speed test: handoff traceroute failed", "server", used.ID, "err", err)
		}
		t.Handoff = h
	}
	saved := s.save(ctx, &t, x)
	return &t, saved, t.Bytes
}

// saveCancelled stores a cancelled test: its window (Start, End), server,
// trigger and the data it used, no speeds.
func (s *Scheduler) saveCancelled(ctx context.Context, t model.SpeedTest, used Server) {
	ctx, cancel := storeCtx(ctx)
	defer cancel()
	c := model.SpeedTest{Start: t.Start, End: t.End, Trigger: t.Trigger, Outcome: model.SpeedCancelled, Region: t.Region,
		Server: used.ID, FallbackLevel: t.FallbackLevel, Fallback: t.Fallback, Port: t.Port, Attempts: t.Attempts,
		Bytes: t.Bytes, Conn: t.Conn}
	if s.o.Store != nil {
		if err := s.o.Store.SaveSpeedTest(ctx, &c); err != nil {
			s.o.Log.Warn("speed test: saving a cancelled test failed", "err", err)
		}
	}
	s.o.Log.Info("speed test cancelled", "server", c.Server, "trigger", c.Trigger)
	if s.o.OnResult != nil {
		s.o.OnResult(c)
	}
}

// saveTimeout bounds storing a test.
const saveTimeout = 10 * time.Second

// storeCtx is the context a test is stored with: never cancelled with the
// job (a row and its window must not be lost to a cancel), bounded on its
// own.
func storeCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), saveTimeout)
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

// save stores t (it reports whether it did), logs and reports it.
//
// A test that finished is stored as its result even when the entry was
// cancelled meanwhile (the cancel landed after the test's own last check):
// the store gets its own context, never the job's.
func (s *Scheduler) save(ctx context.Context, t *model.SpeedTest, x Extra) bool {
	ctx, cancel := storeCtx(ctx)
	defer cancel()
	if b, err := json.Marshal(x); err == nil && x != (Extra{}) {
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
	saved := false
	if s.o.Store != nil {
		if err := s.o.Store.SaveSpeedTest(ctx, t); err != nil {
			s.o.Log.Warn("speed test: saving the result failed", "err", err)
		} else {
			saved = true
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
	return saved
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
// It probes latency under load during each direction (lr nil: none).
// left is the slot's bytes still allowed (SlotMaxBytes over every server
// and retry).
func (s *Scheduler) testServer(ctx context.Context, sv Server, t *model.SpeedTest, x *Extra, lr *loadRun, ip *netip.Addr, left *int64) {
	port := 0
	for i, dir := range []Direction{Download, Upload} {
		s.setLive(dir.String(), sv.ID)
		if port != 0 && !s.o.sleep(ctx, relisten) {
			t.Outcome = model.SpeedFailed
			return
		}
		var stop func() [2][]loadSample
		if lr != nil {
			stop = lr.probe(ctx)
		}
		res, p, attempts, outcome, wire := s.direction(ctx, sv, dir, port, t.Start, left)
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
		t.Bytes += wire
		if res.IP.IsValid() {
			*ip = res.IP
		}
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
// wire is every attempt's bytes on the wire.
func (s *Scheduler) direction(ctx context.Context, sv Server, dir Direction, port int, slotStart time.Time, left *int64) (res Result, used, attempts int, outcome string, wire int64) {
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
		if *left <= 0 {
			s.o.Log.Info("speed test: the slot's data cap is used up", "server", sv.ID)
			return Result{}, port, attempt - 1, model.SpeedFailed, wire
		}
		var err error
		res, err = s.o.run(ctx, Config{
			Addr: net.JoinHostPort(sv.Host, strconv.Itoa(port)), Direction: dir, Timeout: DirTimeout,
			Duration: MaxMeasure, MinDuration: MinMeasure, MaxBytes: min(MaxBytes, *left), Check: sv.check,
		})
		w := max(res.WireBytes, res.TotalBytes, res.Bytes)
		wire += w
		*left -= w
		switch {
		case err == nil:
			return res, port, attempt, model.SpeedOK, wire
		case ctx.Err() != nil:
			return res, port, attempt, model.SpeedFailed, wire
		case errors.Is(err, ErrUnreachable):
			return res, port, attempt, model.SpeedUnreachable, wire
		case !errors.Is(err, ErrBusy) && !errors.Is(err, ErrRefused):
			s.o.Log.Debug("speed test failed", "server", sv.ID, "port", port, "direction", dir.String(), "err", err)
			return res, port, attempt, model.SpeedFailed, wire
		}
		if attempt >= tries || s.o.Now().Sub(slotStart)+AttemptGap >= SlotBudget || !s.o.sleep(ctx, AttemptGap) {
			if errors.Is(err, ErrRefused) {
				return res, port, attempt, model.SpeedUnreachable, wire
			}
			return res, port, attempt, model.SpeedBusy, wire
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
	s.mu.Lock()
	var servers []Server
	for _, sv := range s.o.Catalog.Servers {
		if s.usableLocked(sv.ID) { // not skipped, not resting
			servers = append(servers, sv)
		}
	}
	s.mu.Unlock()
	for _, sv := range servers {
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
//
// The host is resolved once and must be public (only catalog servers are
// picked by RTT).
func serverRTT(ctx context.Context, ping func(context.Context, string) (time.Duration, error), host string, iperfPort int) (time.Duration, error) {
	ips, err := lookupIPv4(ctx, host)
	if err != nil {
		return 0, err
	}
	if len(ips) == 0 || !PublicIPv4(ips[0]) {
		return 0, fmt.Errorf("%s: no public IPv4 address", host)
	}
	ip := ips[0].Unmap().String()
	if ping != nil {
		if d, err := ping(ctx, ip); err == nil {
			return d, nil
		}
	}
	for _, p := range []int{80, 443, iperfPort} {
		var d time.Duration
		if d, err = connectRTT(ctx, ip, p); err == nil {
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
