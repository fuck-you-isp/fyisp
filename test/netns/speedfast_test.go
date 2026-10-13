//go:build netns && linux

package netns

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
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

// fastEveningCut: a 2 Gbit/s line under the 10 s time cap. Every
// direction is measured from where its rate levels off (rate method
// peak90); a lower bound only when it was
// still speeding up at the cap; the API's flags are the stored ones. On
// this line (htb, whose bursts make the bytes per 100 ms tick swing by a
// third at 2 Gbit/s while the ticks themselves are on time) the near
// server's directions, the 30% download cut (1.4 Gbit/s) and a 1 Gbit/s
// line level off and stop early (about 3-4 s): never a lower bound, within
// 12% of the shaped rate. The far server (150 ms RTT) runs longer while its
// windows grow, then levels off, socket-buffer limited at about 1.6 Gbit/s:
// not a lower bound either.
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
		h.env = append(h.env, "FYISP_TICKS=1") // per-tick log of capped directions (fyisp-1.log)
	}, func(dir string) { seedFastHistory(t, dir, 9) })

	type result struct {
		api    speedTestAPI
		method [2]string
		lower  [2]bool
		runS   [2]float64 // each direction's data phase, from fyisp's tick log
	}
	run := func(srv string, a netip.Addr) result {
		t.Helper()
		r := l.speedTest(t, a)
		if r.Outcome != "ok" || r.DownBps == nil || r.UpBps == nil {
			t.Fatalf("%s: %+v", srv, r)
		}
		x := l.storedExtra(t, r.ID)
		runS := l.lastTickRuns(t)
		t.Logf("%s: down %.0f Mbit/s up %.0f Mbit/s, ran %.1f/%.1f s, measured %.1f/%.1f s, lower bound %v/%v, rate method %q",
			srv, *r.DownBps/1e6, *r.UpBps/1e6, runS[0], runS[1], r.DownS, r.UpS, r.DownLowerBound, r.UpLowerBound, x.RateMethod)
		return result{r, x.RateMethod, x.LowerBound, runS}
	}

	// Every test runs under the 10 s time cap: every direction has a rate
	// method, and the API's lower-bound flags are the stored ones.
	consistent := func(name string, r result) {
		t.Helper()
		for i, dir := range []string{"down", "up"} {
			lb := []bool{r.api.DownLowerBound, r.api.UpLowerBound}[i]
			if r.method[i] == "" || lb != r.lower[i] {
				t.Errorf("%s %s: rate method %q, lower bound stored %v, API %v; want a method, the same flags",
					name, dir, r.method[i], r.lower[i], lb)
			}
		}
	}

	// quick: a near server's direction stops early once it levels off.
	quick := func(name string, r result) {
		t.Helper()
		for i, dir := range []string{"down", "up"} {
			if r.runS[i] < 2.5 || r.runS[i] > 6 {
				t.Errorf("%s %s: ran %.1f s, want an early stop at about 3-5.5 s", name, dir, r.runS[i])
			}
		}
	}

	// levelled checks a capped direction measured from its plateau: rate
	// method peak90, not a lower bound, within 12% of the shaped rate (of 80
	// measured, the lowest was 10.05% below, a 2 Gbit/s upload, and the
	// highest 7.8% above, a 1 Gbit/s upload over an htb burst).
	levelled := func(name string, r result, i int, want float64) {
		t.Helper()
		bps := []*float64{r.api.DownBps, r.api.UpBps}[i]
		dir := []string{"down", "up"}[i]
		if r.method[i] != speedtest.MethodPeak90 || r.lower[i] || *bps < 0.88*want || *bps > 1.12*want {
			t.Errorf("%s %s: %.0f Mbit/s by %q, lower bound %v; want about %.0f, from the plateau, not a lower bound",
				name, dir, *bps/1e6, r.method[i], r.lower[i], want/1e6)
		}
	}

	// Near server, full rate: capped both ways, levelled off.
	near := run("near", iperfA)
	consistent("near", near)
	levelled("near", near, 0, fastRate)
	levelled("near", near, 1, fastRate)
	quick("near", near)

	// Far server (150 ms): its window-limited download is still speeding
	// up at the cap over its RTT-scaled smoothing, so a lower bound.
	far := run("far", iperfB)
	consistent("far", far)
	// Over up to 10 s the far path's windows grow and
	// it levels off, socket-buffer limited (1595-1675 Mbit/s in 9 runs on
	// release/v0.7.2 0f4370a): never a lower bound.
	for i, dir := range []string{"down", "up"} {
		bps := []*float64{far.api.DownBps, far.api.UpBps}[i]
		if far.method[i] != speedtest.MethodPeak90 || far.lower[i] || *bps < 1.3e9 || *bps > 2e9 {
			t.Errorf("far %s: %.0f Mbit/s by %q, lower bound %v; want a plateau of about 1600 (1300-2000), not a lower bound",
				dir, *bps/1e6, far.method[i], far.lower[i])
		}
		// It runs longer than the near server, within the 10 s cap (and the
		// ramp-up before it).
		if far.runS[i] <= near.runS[i] || far.runS[i] > 12 {
			t.Errorf("far %s: ran %.1f s (near %.1f s); want longer than near, at most the 10 s cap", dir, far.runS[i], near.runS[i])
		}
	}

	// The evening cut: the line's download drops 30%; the near server reads
	// about 30% less, measured from the plateau, not a lower bound.
	l.nsRun(t, l.isp, "tc", "class", "change", "dev", "i0", "parent", "1:", "classid", "1:10", "htb",
		"rate", rate(fastRate*(1-fastCut)), "burst", "4mb", "cburst", "4mb")
	cut := run("near, cut", iperfA)
	consistent("near, cut", cut)
	levelled("near, cut", cut, 0, fastRate*(1-fastCut))
	levelled("near, cut", cut, 1, fastRate)
	quick("near, cut", cut)

	// At 1 Gbit/s: the same.
	for _, x := range []struct{ ns, dev string }{{l.isp, "i0"}, {l.gw, "g1"}} {
		l.nsRun(t, x.ns, "tc", "class", "change", "dev", x.dev, "parent", "1:", "classid", "1:10", "htb",
			"rate", rate(fastRate/2), "burst", "4mb", "cburst", "4mb")
	}
	slow := run("near, 1 Gbit/s", iperfA)
	consistent("near, 1 Gbit/s", slow)
	levelled("near, 1 Gbit/s", slow, 0, fastRate/2)
	levelled("near, 1 Gbit/s", slow, 1, fastRate/2)
	quick("near, 1 Gbit/s", slow)

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
// test ran under the 10 s time cap with its download measured from its
// plateau.
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
				Conn: model.ConnWired, DownSecs: 4, UpSecs: 4, Bytes: int64(4 * (down + up) / 8), Extra: string(b)}
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

// lastTickRuns reads fyisp's FYISP_TICKS log ("speed test ticks", one line
// per direction) and returns the last test's data phase per direction, in
// seconds: the time of its last tick.
func (l *speedLab) lastTickRuns(t *testing.T) [2]float64 {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(l.dir, fmt.Sprintf("fyisp-%d.log", l.runs)))
	if err != nil {
		t.Fatal(err)
	}
	var out [2]float64
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.Contains(line, `"speed test ticks"`) {
			continue
		}
		var x struct {
			Direction string    `json:"direction"`
			AtMs      []float64 `json:"at_ms"`
		}
		if json.Unmarshal([]byte(line), &x) != nil || len(x.AtMs) == 0 {
			continue
		}
		i := 0
		if x.Direction == "upload" {
			i = 1
		}
		out[i] = x.AtMs[len(x.AtMs)-1] / 1000
	}
	return out
}
