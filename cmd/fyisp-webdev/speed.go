package main

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/speedtest"
)

// newFakeSpeed is the real speed test scheduler over a fake network and an
// in-memory store with synthetic history in mode regions: the 9 default
// regions once an hour (400 s apart, providers rotated) for the backfill
// range (2 to 30 days). Speed falls with distance (Europe fastest), every
// region is ~40% slower 19-23 local time with every provider, first
// choices are busy now and then (a same-metro fallback, or busy), and the
// latency under load and the connection are stored. Queued tests (Test
// now, sweeps, the schedule) take about 4 s a direction.
func newFakeSpeed(ctx context.Context, now time.Time, bf time.Duration, log *slog.Logger) *speedtest.Scheduler {
	cat, err := speedtest.Builtin()
	if err != nil {
		panic(err)
	}
	const seed = 7
	st := &speedtest.MemStore{}
	_ = st.SetSetting(ctx, speedtest.SettingKey, string(speedtest.ModeRegions))
	_ = st.SetSetting(ctx, "speedtest_seed", "7")
	n := &fakeNet{cat: cat, r: rand.New(rand.NewPCG(3, 4))}
	span := min(max(bf, 2*24*time.Hour), 30*24*time.Hour)
	for hour := now.Add(-span).Truncate(time.Hour); hour.Before(now.Add(-time.Hour)); hour = hour.Add(time.Hour) {
		i := 0
		for _, reg := range cat.Regions {
			if !reg.Default {
				continue
			}
			at := hour.Add(time.Duration(i) * 400 * time.Second)
			i++
			x := n.result(at, reg, reg.Chain(cat, at, seed))
			_ = st.SaveSpeedTest(ctx, &x)
		}
	}
	s := speedtest.New(speedtest.Options{Catalog: cat, Store: st, Log: log, Fake: &speedtest.Fake{Run: n.run, RTT: n.rtt}})
	go func() { _ = s.Run(ctx) }()
	return s
}

// fakeNet fakes the network: speeds and RTTs by the server's area.
type fakeNet struct {
	cat *speedtest.Catalog
	mu  sync.Mutex
	r   *rand.Rand
}

// areaBase is the typical download from the fake vantage point (Europe)
// to each area, in Mbit/s, and the RTT in ms.
var areaBase = map[string][2]float64{"eu": {410, 18}, "na": {160, 95}, "sa": {50, 210}, "as": {60, 240}, "oc": {40, 290}, "af": {50, 160}, "me": {90, 110}}

func (n *fakeNet) base(sv speedtest.Server) [2]float64 {
	b, ok := areaBase[sv.Geo]
	if !ok {
		b = [2]float64{100, 150}
	}
	if strings.HasPrefix(sv.ID, "OVH") { // one provider a bit slower
		b[0] *= 0.9
	}
	return b
}

func (n *fakeNet) float() float64 {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.r.Float64()
}

func (n *fakeNet) norm() float64 {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.r.NormFloat64()
}

// result is one synthetic scheduled test of region reg at at.
func (n *fakeNet) result(at time.Time, reg speedtest.Region, chain []speedtest.Step) model.SpeedTest {
	pf := func(v float64) *float64 { return &v }
	pi := func(v int64) *int64 { return &v }
	t := model.SpeedTest{Start: at, End: at.Add(time.Duration(10+n.float()*6) * time.Second), Trigger: "schedule", Outcome: model.SpeedOK,
		Region: reg.ID, Server: chain[0].Server.ID, FallbackLevel: speedtest.FallbackNone, Port: 5201 + int(n.float()*9), Attempts: 1}
	switch x := n.float(); {
	case x < 0.01:
		t.Outcome, t.Attempts = model.SpeedBusy, 4
		return t
	case x < 0.08 && len(chain) > 1:
		t.Server, t.Fallback, t.FallbackLevel, t.Attempts = chain[1].Server.ID, true, chain[1].Level, 3
	case x < 0.085:
		t.Outcome = model.SpeedUnreachable
		return t
	}
	sv, _ := n.cat.Server(t.Server)
	b := n.base(sv)
	sc := 1 + 0.06*n.norm()
	if h := at.Local().Hour(); h >= 19 && h <= 22 {
		sc *= 0.55 + 0.1*n.float()
	}
	down := b[0] * 1e6 * sc
	up := min(down, 38e6*(1+0.04*n.norm()))
	t.DownBps, t.UpBps = pf(down), pf(up)
	t.ScoreDown, t.ScoreUp = pf(min(sc, 1)), pf(min(up/38e6, 1))
	t.DownRetrans, t.UpRetrans = pi(int64(n.float()*400)), pi(int64(n.float()*60))
	t.DownSecs, t.UpSecs = 3+n.float()*2, 3+n.float()*2
	t.Bytes = int64((down*t.DownSecs + up*t.UpSecs) / 8 * 1.2)
	t.DownShort = down*t.DownSecs/8 > speedtest.MaxBytes
	t.CappedByServer = sv.CapacityMbps > 0 && down >= 0.9*sv.CapacityMbps*1e6
	t.IdleGatewayMs, t.LoadedGatewayMs = pf(1+n.float()), pf(1.5+n.float())
	edgeDown, edgeUp := 10+15*n.float(), 25+50*n.float()
	lost := 0
	if n.float() < 0.2 {
		lost = 1 + int(n.float()*3)
	}
	t.Extra = `{"connection":"wired","latency":{"edge":{"idle_ms":8,"down":{"p50_ms":` + f1(edgeDown) + `,"p90_ms":` + f1(edgeDown*1.6) +
		`,"sent":30,"lost":` + f1(float64(lost)) + `},"up":{"p50_ms":` + f1(edgeUp) + `,"p90_ms":` + f1(edgeUp*1.5) + `,"sent":27,"lost":0}},` +
		`"gateway":{"idle_ms":1.2,"down":{"p50_ms":1.6,"p90_ms":2.5,"sent":30,"lost":0},"up":{"p50_ms":1.8,"p90_ms":3,"sent":27,"lost":0}}},"ramp_ms":[700,500]}`
	return t
}

func f1(v float64) string { return strconv.FormatFloat(v, 'f', 1, 64) }

// run fakes one direction: about 4 s at the area's speed.
func (n *fakeNet) run(ctx context.Context, c speedtest.Config) (speedtest.Result, error) {
	host, _, _ := strings.Cut(c.Addr, ":")
	var sv speedtest.Server
	for _, x := range n.cat.Servers {
		if x.Host == host {
			sv = x
		}
	}
	el := time.Duration(3500+n.float()*1500) * time.Millisecond
	t := time.NewTimer(el + 700*time.Millisecond)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return speedtest.Result{}, ctx.Err()
	case <-t.C:
	}
	bps := n.base(sv)[0] * 1e6 * (1 + 0.06*n.norm())
	if c.Direction == speedtest.Upload {
		bps = min(bps, 38e6)
	}
	b := int64(bps * el.Seconds() / 8)
	return speedtest.Result{Direction: c.Direction, Bytes: b, Elapsed: el, Streams: 8, Ramp: 700 * time.Millisecond, TotalBytes: b * 6 / 5}, nil
}

func (n *fakeNet) rtt(_ context.Context, host string, _ int) (time.Duration, error) {
	for _, x := range n.cat.Servers {
		if x.Host == host {
			return time.Duration(n.base(x)[1]*(1+0.1*n.float())) * time.Millisecond, nil
		}
	}
	return 150 * time.Millisecond, nil
}
