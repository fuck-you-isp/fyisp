package probe

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/profile"
)

// TestLiveDefaultProfile probes the real 87 targets for FYISP_LIVE_DEFAULT
// (a duration, e.g. 40s) and prints a per-kind summary.
func TestLiveDefaultProfile(t *testing.T) {
	d, err := time.ParseDuration(os.Getenv("FYISP_LIVE_DEFAULT"))
	if err != nil {
		t.Skip("set FYISP_LIVE_DEFAULT=40s to probe the default profile")
	}
	var logs bytes.Buffer
	old := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(old)
	p, err := profile.Default()
	if err != nil {
		t.Fatal(err)
	}
	r := New(Options{Log: quietLog()})
	t.Logf("caps: %+v", r.Caps())
	type agg struct {
		n, lost, reused int
		reasons         map[model.Reason]int
		rtt             time.Duration
	}
	var mu sync.Mutex
	by := map[model.ProbeKind]*agg{}
	lostBy := map[string]int{}
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	start := time.Now()
	_ = r.Run(ctx, p, model.SinkFunc(func(s model.Sample) {
		mu.Lock()
		defer mu.Unlock()
		a := by[s.Key.Kind]
		if a == nil {
			a = &agg{reasons: map[model.Reason]int{}}
			by[s.Key.Kind] = a
		}
		a.n++
		if s.Lost {
			a.lost++
			a.reasons[s.Reason]++
			lostBy[s.Key.Target+"/"+s.Key.Kind.String()+": "+s.Err]++
		} else {
			a.rtt += s.RTT
		}
		if s.Reused {
			a.reused++
		}
	}))
	t.Logf("ran %s", time.Since(start).Round(time.Second))
	for _, k := range []model.ProbeKind{model.KindHTTPS, model.KindTCP, model.KindICMP} {
		a := by[k]
		if a == nil {
			t.Logf("%s: no samples", k)
			continue
		}
		mean := time.Duration(0)
		if a.n > a.lost {
			mean = a.rtt / time.Duration(a.n-a.lost)
		}
		t.Logf("%-5s samples=%d lost=%d reasons=%v reused=%d mean_rtt=%s", k, a.n, a.lost, a.reasons, a.reused, mean.Round(time.Microsecond))
	}
	var ls []string
	for k, n := range lostBy {
		ls = append(ls, fmt.Sprintf("%s x%d", k, n))
	}
	sort.Strings(ls)
	for _, l := range ls {
		t.Log("lost: " + l)
	}
	if bytes.Contains(logs.Bytes(), []byte("unhandled response frame")) {
		t.Errorf("http2 noise leaked: %s", logs.String())
	}
	t.Logf("std log output during run: %q", logs.String())
}
