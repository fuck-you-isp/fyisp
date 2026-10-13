package metrics

import (
	"os"
	"testing"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

// TestSpeedGolden pins the speed metrics (testdata/speed.golden): the
// latest measured values per server, busy tests counted but never a speed.
func TestSpeedGolden(t *testing.T) {
	c := New(func() *model.Profile { return &model.Profile{} })
	s := NewSpeed()
	c.Register(s)
	f := func(v float64) *float64 { return &v }
	s.Observe(model.SpeedTest{Region: "uk", Server: "Clouvider-London", Outcome: model.SpeedOK, DownBps: f(4.12e8), UpBps: f(4.1e7),
		Latency: &model.SpeedLatency{
			Edge:    model.LoadPhases{Idle: &model.LoadStats{N: 40, P50Ms: 8.2, P95Ms: 9.9}, Down: &model.LoadStats{N: 60, P50Ms: 24, P95Ms: 61.5}, Up: &model.LoadStats{N: 58, Lost: 3, Loss: 0.052, P50Ms: 140, P95Ms: 1000}},
			Gateway: model.LoadPhases{Idle: &model.LoadStats{N: 40, P50Ms: 1.2, P95Ms: 1.9}, Down: &model.LoadStats{N: 60, P50Ms: 1.4, P95Ms: 3}}}})
	s.Observe(model.SpeedTest{Region: "uk", Server: "Clouvider-London", Outcome: model.SpeedBusy})
	s.Observe(model.SpeedTest{Region: "eu-central", Server: "Eranium-Amsterdam", Fallback: true, Outcome: model.SpeedFailed, DownBps: f(3.9e8)})
	s.Observe(model.SpeedTest{Server: "Leaseweb-Tokyo", Outcome: model.SpeedOK, DownBps: f(1.2e8), UpBps: f(1.1e8)}) // Test now
	s.Observe(model.SpeedTest{Outcome: model.SpeedUnreachable})
	got := familyLines(rawScrape(t, c), "fyisp_speed_")
	want, err := os.ReadFile("testdata/speed.golden")
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("speed metrics differ from testdata/speed.golden; got:\n%s", got)
	}
}
