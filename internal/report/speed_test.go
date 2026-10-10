package report

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

func TestSpeedSection(t *testing.T) {
	f := newFixture(t, 7, 5*time.Minute)
	d := f.deps()
	var tests []model.SpeedTest
	start := time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC)
	for i := range 7 * 96 {
		at := start.Add(time.Duration(i) * 15 * time.Minute)
		x := model.SpeedTest{Start: at, End: at.Add(23 * time.Second), Outcome: model.SpeedOK, Server: "Clouvider-London"}
		if i%40 == 3 {
			x.Outcome = model.SpeedBusy
		} else {
			down, up := 400e6, 40e6
			if h := at.Hour(); h >= 19 && h <= 22 {
				down /= 2
			}
			x.DownBps, x.UpBps = &down, &up
		}
		tests = append(tests, x)
	}
	d.SpeedTests = func(_ context.Context, from, to time.Time) ([]model.SpeedTest, error) {
		var out []model.SpeedTest
		for _, x := range tests {
			if !x.Start.Before(from) && !x.Start.After(to) {
				out = append(out, x)
			}
		}
		return out, nil
	}
	html := f.build(t, d, Options{Redact: true})
	checkStatic(t, html)
	for _, want := range []string{
		`id="speed"`, "<h2>Speed tests</h2>",
		"672 speed tests ran in this range: 655 measured, 17 found the test server busy (not counted as slow).",
		"Typical download 400 Mbit/s, upload 40.0 Mbit/s",
		"Evenings are slower: about 50% below your daytime speed (19:00–23:00 UTC).",
		"<td>Evening</td><td class=\"t\">18–24</td>",
		"left out everywhere in this report",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("report lacks %q", want)
		}
	}
	if strings.Contains(html, "Clouvider") {
		t.Error("the report names a speed test server")
	}
	if html := f.build(t, f.deps(), Options{}); strings.Contains(html, `id="speed"`) {
		t.Error("speed section without speed tests")
	}
}
