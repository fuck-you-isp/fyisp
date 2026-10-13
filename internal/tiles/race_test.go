package tiles

import (
	"context"
	"encoding/json"
	"net/netip"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/netinfo"
	"github.com/fuck-you-isp/fyisp/internal/profile"
	"github.com/fuck-you-isp/fyisp/internal/store"
)

// testClock is a clock other goroutines can move forward.
type testClock struct{ ns atomic.Int64 }

func (c *testClock) now() time.Time { return time.Unix(0, c.ns.Load()).UTC() }

// set moves the clock to t unless it is already later.
func (c *testClock) set(t time.Time) {
	for {
		o := c.ns.Load()
		if t.UnixNano() <= o || c.ns.CompareAndSwap(o, t.UnixNano()) {
			return
		}
	}
}

// slowStore moves the clock on at every Raw call: a replay that takes
// longer than the samples' hold.
type slowStore struct {
	*store.Fake
	clk  *testClock
	step time.Duration
}

func (s *slowStore) Raw(ctx context.Context, keys []model.SeriesKey, from, to time.Time, fn func(store.RawPoint) error) error {
	s.clk.set(s.clk.now().Add(s.step))
	return s.Fake.Raw(ctx, keys, from, to, fn)
}

// TestEngineConcurrent (meant for -race): Run with a slow replay, then
// live on a 1 ms timer, while 4 goroutines Observe, 3 read Snapshot, Chart,
// Problems and LateSamples, and the network path changes. A published
// snapshot never changes, and once Ready no reader gets the checking one.
func TestEngineConcurrent(t *testing.T) {
	s := newScene(t)
	arr := eGen(eSeries(s.engine(nil)), s.start, s.end, s.evs, 7)
	t0 := s.drop.from.Add(-2 * time.Minute).Truncate(tickEvery)
	clk := &testClock{}
	clk.set(t0)
	pc := make(chan netinfo.Path, 4)
	src := s.sources(&slowStore{Fake: stored(arr, t0), clk: clk, step: 15 * time.Second})
	src.PathChanges = func() (<-chan netinfo.Path, func()) { return pc, func() {} }
	e := newEngine(s.p, profile.ResolveTiles(s.p), src, Options{Now: clk.now, Interval: time.Millisecond, Replay: 28 * time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- e.Run(ctx) }()

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for r := range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				ready := e.Ready()
				sn := e.Snapshot()
				if ready && sn.Conn.Kind == ConnChecking {
					t.Errorf("reader %d: Ready, but the snapshot is still checking", r)
					return
				}
				b1, _ := json.Marshal(sn)
				ps := e.Snapshot().Problems(time.Time{})
				p1, _ := json.Marshal(ps)
				for _, d := range profile.Tiles {
					if c, ok := e.Chart(d.ID); ok {
						_, _ = json.Marshal(c.Bands)
					}
				}
				_ = e.LateSamples()
				runtime.Gosched()
				b2, _ := json.Marshal(sn)
				p2, _ := json.Marshal(ps)
				if string(b1) != string(b2) || string(p1) != string(p2) {
					t.Errorf("reader %d: a published snapshot changed", r)
					return
				}
			}
		}()
	}
	const nf = 4
	chs := make([]chan model.Sample, nf)
	var fw sync.WaitGroup
	for i := range chs {
		chs[i] = make(chan model.Sample, 64)
		fw.Add(1)
		go func() {
			defer fw.Done()
			for x := range chs[i] {
				e.Observe(x)
			}
		}()
	}
	var rest []eArr
	for _, a := range arr {
		if a.arrive.After(t0) {
			rest = append(rest, a)
		}
	}
	for i, a := range rest {
		clk.set(a.arrive)
		chs[int(e.index[a.s.Key])%nf] <- a.s
		if i%5000 == 0 {
			pc <- netinfo.Path{Gateway: netip.AddrFrom4([4]byte{10, 0, byte(i >> 16), byte(i >> 8)})}
		}
		if i%200 == 0 {
			runtime.Gosched()
		}
	}
	for _, c := range chs {
		close(c)
	}
	fw.Wait()
	for !e.Ready() {
		runtime.Gosched()
	}
	close(stop)
	wg.Wait()
	cancel()
	if err := <-runDone; err != nil {
		t.Fatal(err)
	}
	t.Logf("late %d, problems %d, card %s", e.LateSamples(), len(e.Snapshot().Eps), e.Snapshot().Conn.Kind)
}
