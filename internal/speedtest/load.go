package speedtest

import (
	"context"
	"math"
	"net/netip"
	"slices"
	"sync"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/callq"
	"github.com/fuck-you-isp/fyisp/internal/model"
)

// Latency under load. Bufferbloat lives in the queue of the link's
// bottleneck: the modem's upstream buffer, or the ISP's CMTS, OLT or BNG
// downstream. The gateway's LAN address is in front of that queue, the
// ISP edge (the first hop past the router) behind it. During each direction
// of a test the scheduler pings both every LoadEvery on one ICMP socket;
// the 2 minutes before the test come from the continuous probes (LoadTap).
// Probes that get no answer within LoadTimeout are lost: they count as
// loss and, in the percentiles, as an RTT of LoadTimeout, so a bloated
// queue that times out most probes still reads as bloated.
const (
	LoadEvery   = 150 * time.Millisecond
	LoadTimeout = time.Second
	minLoad     = 3 // probes a phase needs for its stats
)

// LoadPinger pings one address (probe.Pinger). Safe for concurrent use.
type LoadPinger interface {
	Ping(ctx context.Context, dst netip.Addr, timeout time.Duration) (time.Duration, error)
	Close() error
}

// loadSample is one probe: its RTT in ms, or lost.
type loadSample struct {
	ms   float64
	lost bool
}

// statsOf summarises probes; nil with fewer than minLoad. A lost probe is
// an RTT of LoadTimeout in the percentiles.
func statsOf(v []loadSample) *model.LoadStats {
	if len(v) < minLoad {
		return nil
	}
	st := &model.LoadStats{N: len(v)}
	ms := make([]float64, len(v))
	for i, x := range v {
		ms[i] = x.ms
		if x.lost {
			st.Lost++
			ms[i] = float64(LoadTimeout / time.Millisecond)
		}
	}
	st.Loss = round3(float64(st.Lost) / float64(st.N))
	st.P50Ms = round1(callq.Median(ms))
	st.P95Ms = round1(callq.Quantile(ms, 0.95))
	return st
}

func round3(v float64) float64 { return math.Round(v*1000) / 1000 }

// loadRun probes the gateway and the ISP edge during each direction of one
// test.
type loadRun struct {
	p        LoadPinger
	gw, edge netip.Addr
	every    time.Duration
	down, up [2][]loadSample // [gateway, edge]
}

// probe pings the run's addresses every lr.every from now until stop is
// called; stop waits for the probes in flight and returns the samples,
// [gateway, edge].
func (lr *loadRun) probe(ctx context.Context) (stop func() [2][]loadSample) {
	ctx, cancel := context.WithCancel(ctx)
	var mu sync.Mutex
	var wg sync.WaitGroup
	var out [2][]loadSample
	ping := func(i int, dst netip.Addr) {
		defer wg.Done()
		rtt, err := lr.p.Ping(context.WithoutCancel(ctx), dst, LoadTimeout)
		s := loadSample{ms: float64(rtt) / float64(time.Millisecond), lost: err != nil}
		mu.Lock()
		out[i] = append(out[i], s)
		mu.Unlock()
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		tk := time.NewTicker(lr.every)
		defer tk.Stop()
		for {
			for i, dst := range []netip.Addr{lr.gw, lr.edge} {
				if dst.IsValid() {
					wg.Add(1)
					go ping(i, dst)
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-tk.C:
			}
		}
	}()
	return func() [2][]loadSample {
		cancel()
		<-done
		wg.Wait()
		mu.Lock()
		defer mu.Unlock()
		return [2][]loadSample{slices.Clone(out[0]), slices.Clone(out[1])}
	}
}

// latency builds a test's latency under load: idle from the tap over the
// idleWindow before start, loaded from the run (nil: none); nil when no
// phase has stats.
func latency(tap *LoadTap, lr *loadRun, start time.Time) *model.SpeedLatency {
	var l model.SpeedLatency
	for i, ph := range []*model.LoadPhases{&l.Gateway, &l.Edge} {
		if tap != nil {
			ph.Idle = tap.Stats([]string{LayerGateway, LayerEdge}[i], start.Add(-idleWindow), start)
		}
		if lr != nil {
			ph.Down, ph.Up = statsOf(lr.down[i]), statsOf(lr.up[i])
		}
	}
	if l == (model.SpeedLatency{}) {
		return nil
	}
	return &l
}
