package speedtest

import (
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

// days of tests every 15 minutes; speed(hour) gives Mbit/s by UTC hour.
func synth(days int, speed func(h int) float64) []model.SpeedTest {
	var out []model.SpeedTest
	t0 := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	for i := range days * 96 {
		at := t0.Add(time.Duration(i) * 15 * time.Minute)
		d := speed(at.Hour()) * 1e6 * (1 + 0.02*float64(i%5-2))
		u := d / 10
		out = append(out, model.SpeedTest{Start: at, Outcome: model.SpeedOK, DownBps: &d, UpBps: &u})
	}
	return out
}

func TestByHour(t *testing.T) {
	ts := synth(7, func(h int) float64 { return float64(100 + h) })
	busy := model.SpeedTest{Start: ts[0].Start, Outcome: model.SpeedBusy}
	hs := ByHour(append(ts, busy), 0)
	if len(hs) != 24 || hs[5].N != 28 || *hs[5].DownMedian != 105e6 || *hs[5].DownP10 >= *hs[5].DownMedian || *hs[5].UpMedian != 10.5e6 {
		t.Fatalf("%+v", hs[5])
	}
	// Local hours: +2h moves UTC 03:00 to hour 5.
	if hs := ByHour(ts, 2*time.Hour); *hs[5].DownMedian != 103e6 {
		t.Errorf("tz: %v", *hs[5].DownMedian)
	}
	if hs := ByHour(ts[:2], 0); hs[0].N != 2 || hs[0].DownMedian != nil {
		t.Errorf("too few: %+v", hs[0])
	}
}

func TestFindPattern(t *testing.T) {
	cases := []struct {
		name  string
		speed func(h int) float64
		days  int
		want  string
		from  int
		to    int
	}{
		{"evening", func(h int) float64 {
			if h >= 19 && h <= 22 {
				return 60
			}
			return 100
		}, 30, "Evenings are slower: about 40% below your daytime speed.", 19, 23},
		{"night wrap", func(h int) float64 {
			if h >= 23 || h <= 1 {
				return 50
			}
			return 100
		}, 30, "Nights are slower: about 50% below your daytime speed.", 23, 2},
		{"morning", func(h int) float64 {
			if h == 8 || h == 9 {
				return 70
			}
			return 100
		}, 30, "Mornings are slower: about 30% below your usual speed.", 8, 10},
		{"flat", func(int) float64 { return 100 }, 30, "", 0, 0},
		{"mild", func(h int) float64 {
			if h >= 19 {
				return 85
			}
			return 100
		}, 30, "", 0, 0},
		{"one hour", func(h int) float64 {
			if h == 20 {
				return 40
			}
			return 100
		}, 30, "", 0, 0},
	}
	for _, c := range cases {
		p := FindPattern(ByHour(synth(c.days, c.speed), 0))
		switch {
		case c.want == "" && p != nil:
			t.Errorf("%s: %+v", c.name, p)
		case c.want != "" && (p == nil || p.Text != c.want || p.DipFrom != c.from || p.DipTo != c.to || p.Level != "info"):
			t.Errorf("%s: %+v", c.name, p)
		}
	}
	// Not enough hours: two days of tests at 9..17 only.
	var few []model.SpeedTest
	for _, x := range synth(30, func(h int) float64 { return 100 }) {
		if h := x.Start.Hour(); h >= 9 && h <= 17 {
			few = append(few, x)
		}
	}
	if p := FindPattern(ByHour(few, 0)); p != nil {
		t.Errorf("9 hours: %+v", p)
	}
}
