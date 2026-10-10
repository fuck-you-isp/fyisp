package main

import (
	"context"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/speedtest"
)

// fakeSpeed is a web.SpeedSource with synthetic history: a test every 15
// minutes for the backfill range (at most 30 days), ~380/38 Mbit/s with an
// evening dip (19-23 local server time, ~40% slower), a busy server now and
// then, and latency under load. Test now runs a 25 s fake test.
type fakeSpeed struct {
	mu      sync.Mutex
	enabled bool
	next    time.Time
	run     *speedtest.RunState
	tests   []model.SpeedTest
	r       *rand.Rand
}

var fakeServers = []speedtest.ServerState{
	{Name: "Clouvider-London", City: "London", Country: "GB", Picked: true},
	{Name: "Eranium-Amsterdam", City: "Amsterdam", Country: "NL", Picked: true},
	{Name: "Uztelecom-Tashkent", City: "Tashkent", Country: "UZ"},
	{Name: "Leaseweb-Singapore", City: "Singapore", Country: "SG"},
	{Name: "Clouvider-LosAngeles", City: "Los Angeles", Country: "US"},
	{Name: "Leaseweb-NewYork", City: "New York", Country: "US"},
	{Name: "Edgoo-SaoPaulo", City: "Sao Paulo", Country: "BR"},
}

func newFakeSpeed(now time.Time, bf time.Duration) *fakeSpeed {
	f := &fakeSpeed{enabled: true, next: now.Add(4 * time.Minute), r: rand.New(rand.NewPCG(3, 4))}
	span := min(max(bf, 2*24*time.Hour), 30*24*time.Hour)
	for at := now.Add(-span).Truncate(15 * time.Minute); at.Before(now.Add(-time.Minute)); at = at.Add(15 * time.Minute) {
		f.tests = append(f.tests, f.result(at, "schedule"))
	}
	return f
}

// result is one synthetic test at at.
func (f *fakeSpeed) result(at time.Time, trigger string) model.SpeedTest {
	pf := func(v float64) *float64 { return &v }
	pi := func(v int64) *int64 { return &v }
	i := len(f.tests)
	srv := fakeServers[i%2]
	t := model.SpeedTest{ID: int64(i + 1), Start: at, End: at.Add(time.Duration(22+f.r.IntN(4)) * time.Second), Trigger: trigger,
		Outcome: model.SpeedOK, Server: srv.Name, Port: 5200 + f.r.IntN(10), Attempts: 2}
	switch x := f.r.Float64(); {
	case x < 0.03:
		t.Outcome, t.Attempts, t.End = model.SpeedBusy, 3, at.Add(5*time.Second)
		return t
	case x < 0.035:
		t.Outcome, t.Attempts, t.End = model.SpeedUnreachable, 1, at.Add(2*time.Second)
		return t
	}
	down := 380e6 * (1 + 0.06*f.r.NormFloat64())
	if h := at.Local().Hour(); h >= 19 && h <= 22 {
		down *= 0.55 + 0.1*f.r.Float64()
	}
	up := 38e6 * (1 + 0.04*f.r.NormFloat64())
	t.DownBps, t.UpBps = pf(down), pf(up)
	t.DownRetrans, t.UpRetrans = pi(int64(f.r.IntN(400))), pi(int64(f.r.IntN(60)))
	t.IdleGatewayMs, t.LoadedGatewayMs = pf(1+f.r.Float64()), pf(8+20*f.r.Float64())
	t.IdleCallMs, t.LoadedCallMs = pf(12+3*f.r.Float64()), pf(40+60*f.r.Float64())
	return t
}

func (f *fakeSpeed) State() speedtest.State {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := speedtest.State{Enabled: f.enabled, Interval: 15 * time.Minute, Servers: fakeServers}
	if f.enabled && f.run == nil {
		st.Next = f.next
	}
	if f.run != nil {
		c := *f.run
		st.Running = &c
	}
	london, ams := 12.4, 18.9
	st.Servers = append([]speedtest.ServerState(nil), fakeServers...)
	st.Servers[0].RTTms, st.Servers[1].RTTms = &london, &ams
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

// TestNow walks through picking (2 s), download (11 s) and upload (11 s).
func (f *fakeSpeed) TestNow() (time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.run != nil {
		return time.Time{}, speedtest.ErrRunning
	}
	now := time.Now()
	f.run = &speedtest.RunState{Started: now, Direction: "picking"}
	step := func(d time.Duration, dir string) {
		time.AfterFunc(d, func() {
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.run != nil {
				f.run.Direction, f.run.Server = dir, fakeServers[len(f.tests)%2].Name
			}
		})
	}
	step(2*time.Second, "download")
	step(13*time.Second, "upload")
	time.AfterFunc(24*time.Second, func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.tests = append(f.tests, f.result(now.Add(2*time.Second), "manual"))
		f.run = nil
	})
	return now, nil
}

func (f *fakeSpeed) SetEnabled(_ context.Context, on bool) (time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enabled = on
	f.next = time.Time{}
	if on {
		f.next = time.Now().Add(2 * time.Minute)
	}
	return f.next, nil
}
