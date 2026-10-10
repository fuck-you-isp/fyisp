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
		IdleGatewayMs: f(1.2), LoadedGatewayMs: f(9.5), Extra: `{"latency":{"edge":{"idle_ms":8,"down":{"p50_ms":21,"p90_ms":34,"sent":40,"lost":2},"up":{"p50_ms":55,"p90_ms":80,"sent":30,"lost":0}},` +
			`"gateway":{"idle_ms":1.2,"down":{"p50_ms":9.5,"p90_ms":12,"sent":40,"lost":0}}}}`})
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
