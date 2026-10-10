package main

import (
	"context"
	"math/rand/v2"
	"slices"
	"sync"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/speedtest"
)

// fakeSpeed is a web.SpeedSource with synthetic history in mode regions:
// the 9 catalog regions every 15 minutes (100 s apart) for the backfill
// range (2 to 30 days). Speed falls with distance (Europe fastest), every
// region is ~40% slower 19-23 local server time, first choices are busy
// now and then (fallback, or busy when the whole chain is), and loaded
// latency is stored. Queued tests (Test now, sweeps) run 18 s fake tests
// through the real queue.
type fakeSpeed struct {
	mu    sync.Mutex
	cat   *speedtest.Catalog
	mode  speedtest.Mode
	next  time.Time
	run   *speedtest.RunState
	tests []model.SpeedTest
	r     *rand.Rand
	q     *speedtest.Queue
}

// regionBase is each region's typical download from the fake vantage
// point (Europe), in Mbit/s.
var regionBase = map[string]float64{"uk": 420, "eu-west": 400, "eu-central": 390, "us-east": 180, "us-central": 140,
	"us-west": 110, "asia-east": 60, "asia-southeast": 55, "oceania": 40}

func newFakeSpeed(now time.Time, bf time.Duration) *fakeSpeed {
	cat, err := speedtest.Builtin()
	if err != nil {
		panic(err)
	}
	f := &fakeSpeed{cat: cat, mode: speedtest.ModeRegions, next: now.Add(37 * time.Second), r: rand.New(rand.NewPCG(3, 4))}
	on := true // the dev server's share link may queue tests
	f.q = speedtest.NewQueue(speedtest.QueueOptions{Resolve: f.resolve, Exec: f.exec, Live: f.live, Public: &on})
	span := min(max(bf, 2*24*time.Hour), 30*24*time.Hour)
	spacing := 15 * time.Minute / time.Duration(len(cat.Regions))
	for cycle := now.Add(-span).Truncate(15 * time.Minute); cycle.Before(now.Add(-15 * time.Minute)); cycle = cycle.Add(15 * time.Minute) {
		for i, reg := range cat.Regions {
			f.tests = append(f.tests, f.result(cycle.Add(time.Duration(i)*spacing), reg, "schedule"))
		}
	}
	return f
}

// result is one synthetic test of region reg at at.
func (f *fakeSpeed) result(at time.Time, reg speedtest.Region, trigger string) model.SpeedTest {
	pf := func(v float64) *float64 { return &v }
	pi := func(v int64) *int64 { return &v }
	t := model.SpeedTest{ID: int64(len(f.tests) + 1), Start: at, End: at.Add(time.Duration(12+f.r.IntN(8)) * time.Second), Trigger: trigger,
		Outcome: model.SpeedOK, Region: reg.ID, Server: reg.Chain[0], Port: 5201 + f.r.IntN(10), Attempts: 2}
	switch x := f.r.Float64(); {
	case x < 0.01:
		t.Outcome, t.Attempts, t.Server, t.Fallback = model.SpeedBusy, 6, reg.Chain[len(reg.Chain)-1], true
		return t
	case x < 0.08:
		t.Server, t.Fallback, t.Attempts = reg.Chain[1], true, 5
	case x < 0.085:
		t.Outcome, t.Attempts = model.SpeedUnreachable, 1
		return t
	}
	down := regionBase[reg.ID] * 1e6 * (1 + 0.06*f.r.NormFloat64())
	if h := at.Local().Hour(); h >= 19 && h <= 22 {
		down *= 0.55 + 0.1*f.r.Float64()
	}
	up := min(down, 38e6*(1+0.04*f.r.NormFloat64()))
	t.DownBps, t.UpBps = pf(down), pf(up)
	t.DownRetrans, t.UpRetrans = pi(int64(f.r.IntN(400))), pi(int64(f.r.IntN(60)))
	t.IdleGatewayMs, t.LoadedGatewayMs = pf(1+f.r.Float64()), pf(8+20*f.r.Float64())
	t.IdleCallMs, t.LoadedCallMs = pf(12+3*f.r.Float64()), pf(40+60*f.r.Float64())
	return t
}

func (f *fakeSpeed) State() speedtest.State {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := speedtest.State{Mode: f.mode, Modes: []speedtest.Mode{speedtest.ModeOff, speedtest.ModeRegions, speedtest.ModeNearest},
		Interval: 15 * time.Minute, Regions: f.cat.Regions}
	if f.mode == speedtest.ModeRegions {
		st.Spacing = 100 * time.Second
		st.PlannedShare = 9 * 15.0 / 900
	} else if f.mode == speedtest.ModeNearest {
		st.Spacing, st.PlannedShare = 15*time.Minute, 15.0/900
	}
	if f.mode != speedtest.ModeOff && f.run == nil {
		st.Next, st.NextRegion, st.NextServer = f.next, "uk", "Clouvider-London"
	}
	if f.run != nil {
		c := *f.run
		st.Running = &c
	}
	near, mid := 4.8, 9.6
	for _, sv := range f.cat.Servers {
		ss := speedtest.ServerState{Server: sv}
		switch sv.ID {
		case "Clouvider-London":
			ss.RTTms, ss.Picked = &near, true
		case "Leaseweb-London":
			ss.RTTms, ss.Picked = &mid, true
		}
		st.Servers = append(st.Servers, ss)
	}
	return st
}

func (f *fakeSpeed) SpeedTests(_ context.Context, from, to time.Time) ([]model.SpeedTest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []model.SpeedTest
	for _, t := range f.tests {
		if !t.Start.Before(from) && !t.Start.After(to) {
			out = append(out, t)
		}
	}
	return out, nil
}

// Queue is the real speed-test queue over a fake test: each job walks
// through download (9 s) and upload (9 s) and stores a synthetic result.
func (f *fakeSpeed) Queue() *speedtest.Queue { return f.q }

// resolve turns queue requests into jobs: a region (or the first), a
// server, or every server for a sweep.
func (f *fakeSpeed) resolve(kind speedtest.Kind, id string) ([]speedtest.Job, error) {
	if kind == speedtest.KindSweep {
		var out []speedtest.Job
		for _, sv := range f.cat.Servers {
			out = append(out, speedtest.Job{Chain: []speedtest.Server{sv}})
		}
		return out, nil
	}
	if id == "" {
		reg := f.cat.Regions[0]
		sv, _ := f.cat.Server(reg.Chain[0])
		return []speedtest.Job{{Region: reg.ID, Chain: []speedtest.Server{sv}}}, nil
	}
	for _, r := range f.cat.Regions {
		if r.ID == id {
			sv, _ := f.cat.Server(r.Chain[0])
			return []speedtest.Job{{Region: r.ID, Chain: []speedtest.Server{sv}}}, nil
		}
	}
	if sv, ok := f.cat.Server(id); ok {
		return []speedtest.Job{{Chain: []speedtest.Server{sv}}}, nil
	}
	return nil, speedtest.ErrUnknownID
}

// exec fakes one test (18 s), or stops early when cancelled.
func (f *fakeSpeed) exec(ctx context.Context, j speedtest.Job, trigger string) (int64, bool, int64) {
	reg := speedtest.Region{ID: j.Region, Chain: []string{j.Chain[0].ID}}
	if reg.ID == "" {
		reg.ID = "uk" // speeds of a near server
		for _, r := range f.cat.Regions {
			if slices.Contains(r.Chain, j.Chain[0].ID) {
				reg.ID = r.ID
			}
		}
	}
	now := time.Now()
	f.mu.Lock()
	f.run = &speedtest.RunState{Started: now, ExpectedEnd: now.Add(20 * time.Second), Direction: "download", Server: j.Chain[0].ID, Region: j.Region}
	f.mu.Unlock()
	defer func() { f.mu.Lock(); f.run = nil; f.mu.Unlock() }()
	for _, step := range []string{"download", "upload"} {
		f.mu.Lock()
		f.run.Direction = step
		f.mu.Unlock()
		select {
		case <-ctx.Done():
			return 0, false, 0
		case <-time.After(9 * time.Second):
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	t := f.result(now, reg, trigger)
	if j.Region == "" {
		t.Region = ""
	}
	f.tests = append(f.tests, t)
	var bytes int64
	if t.DownBps != nil && t.UpBps != nil {
		bytes = int64((*t.DownBps + *t.UpBps) * 8 / 8) // 8 s measured each way
	}
	return t.ID, true, bytes
}

func (f *fakeSpeed) live() (string, string, time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.run == nil {
		return "", "", time.Time{}
	}
	return f.run.Direction, f.run.Server, f.run.ExpectedEnd
}

func (f *fakeSpeed) SetMode(_ context.Context, m speedtest.Mode) (time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch m {
	case speedtest.ModeOff, speedtest.ModeRegions, speedtest.ModeNearest:
	default:
		return time.Time{}, speedtest.ErrMode
	}
	f.mode, f.next = m, time.Time{}
	if m != speedtest.ModeOff {
		f.next = time.Now().Add(2 * time.Minute)
	}
	return f.next, nil
}

func (f *fakeSpeed) MaskedShare(time.Duration) float64 { return 0.141 }
