//go:build netns && linux

package netns

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/speedtest"
	"github.com/fuck-you-isp/fyisp/internal/store"
)

// fastRate is the lab line's rate: a veth shaped with htb holds 2 Gbit/s
// in both directions (the lab's iperf3 server and fyisp's client are both
// Go, on loopback-speed veths).
const (
	fastRate = 2e9
	fastCut  = 0.3 // the evening cut
)

// fastEveningCut: on a 2 Gbit/s line every direction reaches the 100 MB
// cap within half a second. The steady-rate estimator (A2) must measure
// such a direction from where its rate levels off (rate method peak90) and
// store a lower bound exactly when it was still speeding up at the cap;
// the API's flags must be the stored ones. Measured in the lab: the near
// server's download levels off at about 2 Gbit/s; its upload is often a
// lower bound (100 MB is about four 100 ms ticks, and a first tick still
// ramping up leaves fewer than 3 at 90% of the peak); the far server (150 ms
// RTT) levels off at about 950 Mbit/s, window-limited, so not a lower
// bound. Under load the near download can be a lower bound too, so at
// 2 Gbit/s only the method, the rate and the stored/API agreement are
// asserted. A 30% cut of the line's download (to 1.4 Gbit/s, about six
// ticks) must read as about 30% less, from the plateau, not as a lower
// bound; and at 1 Gbit/s neither direction is a lower bound.
//
// The finding needs days of scheduled tests, so the store is seeded with
// 9 days of hourly tests against the built-in catalog's regions before
// fyisp starts: near regions at 2 Gbit/s with the same 30% cut from 19 to
// 22 h UTC, far regions (East and Southeast Asia, Oceania) mostly lower
// bounds that read low. The finding must say isp, never nothing.
func fastEveningCut(t *testing.T) {
	rate := func(r float64) string { return fmt.Sprintf("%.0fmbit", r/1e6) }
	l := newSpeedLabData(t, func(h *harness) {
		for _, x := range []struct{ ns, dev, peer string }{{h.isp, "i0", "src"}, {h.gw, "g1", "dst"}} {
			tc := func(args ...string) { h.nsRun(t, x.ns, append([]string{"tc"}, args...)...) }
			tc("qdisc", "add", "dev", x.dev, "root", "handle", "1:", "htb", "default", "10")
			for _, c := range []string{"1:10", "1:20"} {
				tc("class", "add", "dev", x.dev, "parent", "1:", "classid", c, "htb", "rate", rate(fastRate), "burst", "4mb", "cburst", "4mb")
			}
			tc("qdisc", "add", "dev", x.dev, "parent", "1:10", "fq_codel")
			// Server B is far: 75 ms each way.
			tc("qdisc", "add", "dev", x.dev, "parent", "1:20", "netem", "delay", "75ms", "limit", "100000")
			tc("filter", "add", "dev", x.dev, "parent", "1:", "protocol", "ip", "prio", "1", "u32",
				"match", "ip", x.peer, iperfB.String()+"/32", "flowid", "1:20")
		}
	}, func(dir string) { seedFastHistory(t, dir, 9) })

	type result struct {
		api    speedTestAPI
		method [2]string
		lower  [2]bool
	}
	run := func(srv string, a netip.Addr) result {
		t.Helper()
		r := l.speedTest(t, a)
		if r.Outcome != "ok" || r.DownBps == nil || r.UpBps == nil {
			t.Fatalf("%s: %+v", srv, r)
		}
		x := l.storedExtra(t, r.ID)
		t.Logf("%s: down %.0f Mbit/s up %.0f Mbit/s, short %v/%v, lower bound %v/%v, rate method %q",
			srv, *r.DownBps/1e6, *r.UpBps/1e6, r.DownShort, r.UpShort, r.DownLowerBound, r.UpLowerBound, x.RateMethod)
		return result{r, x.RateMethod, x.LowerBound}
	}

	// Every capped direction has a rate method, and the API's lower-bound
	// flags are the stored ones.
	consistent := func(name string, r result) {
		t.Helper()
		for i, dir := range []string{"down", "up"} {
			short := []bool{r.api.DownShort, r.api.UpShort}[i]
			lb := []bool{r.api.DownLowerBound, r.api.UpLowerBound}[i]
			if short != (r.method[i] != "") || lb != r.lower[i] {
				t.Errorf("%s %s: short %v, rate method %q, lower bound stored %v, API %v", name, dir, short, r.method[i], r.lower[i], lb)
			}
		}
	}

	// Near server, full rate: capped both ways, measured from the plateau.
	// At 2 Gbit/s 100 MB lasts about four 100 ms ticks, so a direction whose
	// first tick was still ramping up is a lower bound by design (fewer than
	// 3 ticks at 90% of the peak); the download, received here and fastest
	// to ramp, levels off.
	near := run("near", iperfA)
	consistent("near", near)
	for i, dir := range []string{"down", "up"} {
		bps := []*float64{near.api.DownBps, near.api.UpBps}[i]
		if near.method[i] != speedtest.MethodPeak90 || *bps < 0.8*fastRate || *bps > 1.05*fastRate {
			t.Errorf("near %s: %.0f Mbit/s by %q, want about %.0f from the plateau", dir, *bps/1e6, near.method[i], fastRate/1e6)
		}
	}
	if near.lower[0] || near.lower[1] {
		t.Logf("near at 2 Gbit/s: lower bound %v/%v (allowed: about four ticks to the cap)", near.lower[0], near.lower[1])
	}

	// Far server (150 ms): its rate and flags as measured, consistently.
	far := run("far", iperfB)
	consistent("far", far)

	// The evening cut: the line's download drops 30%; the near server reads
	// about 30% less, measured from the plateau, not a lower bound.
	l.nsRun(t, l.isp, "tc", "class", "change", "dev", "i0", "parent", "1:", "classid", "1:10", "htb",
		"rate", rate(fastRate*(1-fastCut)), "burst", "4mb", "cburst", "4mb")
	cut := run("near, cut", iperfA)
	consistent("near, cut", cut)
	ratio := *cut.api.DownBps / *near.api.DownBps
	if ratio < 1-fastCut-0.1 || ratio > 1-fastCut+0.1 || cut.method[0] != speedtest.MethodPeak90 || cut.lower[0] || cut.api.DownLowerBound {
		t.Errorf("near after a %.0f%% cut: x%.2f (%.0f -> %.0f Mbit/s), rate method %q, lower bound %v/%v; want about x%.1f from the plateau",
			100*fastCut, ratio, *near.api.DownBps/1e6, *cut.api.DownBps/1e6, cut.method[0], cut.lower[0], cut.api.DownLowerBound, 1-fastCut)
	}

	// At 1 Gbit/s the cap leaves about eight ticks: a near server's
	// directions level off and are never lower bounds.
	for _, x := range []struct{ ns, dev string }{{l.isp, "i0"}, {l.gw, "g1"}} {
		l.nsRun(t, x.ns, "tc", "class", "change", "dev", x.dev, "parent", "1:", "classid", "1:10", "htb",
			"rate", rate(fastRate/2), "burst", "4mb", "cburst", "4mb")
	}
	slow := run("near, 1 Gbit/s", iperfA)
	consistent("near, 1 Gbit/s", slow)
	for i, dir := range []string{"down", "up"} {
		bps := []*float64{slow.api.DownBps, slow.api.UpBps}[i]
		if slow.method[i] != speedtest.MethodPeak90 || slow.lower[i] || *bps < 0.4*fastRate || *bps > 0.53*fastRate {
			t.Errorf("near at 1 Gbit/s %s: %.0f Mbit/s by %q, lower bound %v; want about 1000 from the plateau, not a lower bound", dir, *bps/1e6, slow.method[i], slow.lower[i])
		}
	}

	// The finding over the seeded days: the ISP, at the cut's hours.
	var s struct {
		Finding struct {
			Case  string `json:"case"`
			Hours []int  `json:"hours"`
			Text  string `json:"text"`
			Days  int    `json:"days"`
			Tests int    `json:"tests"`
		} `json:"finding"`
		Rows []struct {
			ID          string `json:"id"`
			NLowerBound int    `json:"n_lower_bound"`
			TooFast     bool   `json:"too_fast"`
		} `json:"rows"`
	}
	b := l.get(t, "/api/speed?days=30&tz=0")
	l.save(t, "speed-summary.json", b)
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	f := s.Finding
	t.Logf("finding: %s, hours %v, %d days, %d tests: %s", f.Case, f.Hours, f.Days, f.Tests, f.Text)
	if f.Case != "isp" || !slices.Contains(f.Hours, 20) {
		t.Errorf("finding %s at %v, want isp at 19-22 h: %s", f.Case, f.Hours, f.Text)
	}
	for _, r := range s.Rows {
		if fastFar[r.ID] && r.NLowerBound == 0 {
			t.Errorf("row %s: n_lower_bound 0, want the far region's lower bounds counted", r.ID)
		}
	}
	l.checkMasking(t)
}

// fastFar are the built-in regions the seeded history treats as far from
// the lab (their tests mostly lower bounds).
var fastFar = map[string]bool{"asia-east": true, "asia-southeast": true, "oceania": true}

// seedFastHistory stores days of hourly scheduled tests against the
// built-in catalog's default regions, ending an hour ago: near regions at
// fastRate with fastCut from 19 to 22 h UTC, far ones a lower bound
// reading 0.6 two hours in three (else 1.05 of a 400 Mbit/s normal). Every
// download is capped and measured from its plateau, as v0.7.1 stores them.
func seedFastHistory(t *testing.T, dir string, days int) {
	cat, err := speedtest.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(dir, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	end := time.Now().UTC().Truncate(time.Hour).Add(-time.Hour)
	n := 0
	for at := end.Add(-time.Duration(days) * 24 * time.Hour); at.Before(end); at = at.Add(time.Hour) {
		for i, r := range cat.Regions {
			if !r.Default {
				continue
			}
			ts := at.Add(time.Duration(i) * 400 * time.Second)
			chain := r.Chain(cat, ts, 0)
			if len(chain) == 0 {
				continue
			}
			h := ts.Hour()
			sc, base, lower := 1.0, fastRate, false
			switch {
			case fastFar[r.ID] && h%3 != 0:
				sc, base, lower = 0.6, 400e6, true
			case fastFar[r.ID]:
				sc, base = 1.05, 400e6
			case h >= 19 && h <= 22:
				sc = 1 - fastCut
			}
			down, up := sc*base, math.Min(base, 1e9)
			x := speedtest.Extra{RateMethod: [2]string{speedtest.MethodPeak90, ""}, LowerBound: [2]bool{lower, false}}
			b, err := json.Marshal(x)
			if err != nil {
				t.Fatal(err)
			}
			test := model.SpeedTest{Start: ts, End: ts.Add(12 * time.Second), Trigger: "schedule", Outcome: model.SpeedOK, Region: r.ID,
				Server: chain[0].Server.ID, FallbackLevel: speedtest.FallbackNone, DownBps: &down, UpBps: &up, ScoreDown: &sc,
				Conn: model.ConnWired, DownShort: true, DownSecs: 0.4, UpSecs: 4, Bytes: 100e6 + int64(4*up/8), Extra: string(b)}
			if err := st.SaveSpeedTest(context.Background(), &test); err != nil {
				t.Fatal(err)
			}
			n++
		}
	}
	t.Logf("seeded %d tests over %d days", n, days)
}

// storedExtra reads test id's Extra from the store (the API doesn't carry
// the rate method).
func (l *speedLab) storedExtra(t *testing.T, id int64) speedtest.Extra {
	t.Helper()
	st, err := store.OpenReadOnly(l.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ts, err := st.SpeedTests(context.Background(), time.Now().Add(-time.Hour), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range ts {
		if x.ID == id {
			return speedtest.ParseExtra(x.Extra)
		}
	}
	t.Fatalf("test %d not in the store", id)
	return speedtest.Extra{}
}
