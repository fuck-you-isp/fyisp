package web

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/store"
)

// probesHandler serves a Fake with Alpha's tcp and https probed every 15 s for
// the last 30 minutes at 20 ms, with the given samples on top.
func probesHandler(t *testing.T, now time.Time, extra ...model.Sample) func(q string) (int, probesJSON) {
	t.Helper()
	f := store.NewFake()
	taken := map[model.SeriesKey]map[int64]bool{}
	for _, s := range extra {
		if taken[s.Key] == nil {
			taken[s.Key] = map[int64]bool{}
		}
		taken[s.Key][s.Slot.UnixMilli()] = true
	}
	for _, k := range []model.ProbeKind{model.KindTCP, model.KindHTTPS} {
		key := model.SeriesKey{Target: "Alpha", Kind: k}
		for ts := now.Add(-30 * time.Minute).Truncate(15 * time.Second); ts.Before(now); ts = ts.Add(15 * time.Second) {
			if !taken[key][ts.UnixMilli()] {
				f.Observe(model.Sample{Key: key, Slot: ts, RTT: 20 * time.Millisecond})
			}
		}
	}
	for _, s := range extra {
		f.Observe(s)
	}
	return probesGet(t, f)
}

// probesGet answers /api/probes over st with testProfile.
func probesGet(t *testing.T, st store.Reader) func(q string) (int, probesJSON) {
	t.Helper()
	p := testProfile()
	h := Local(Deps{Profile: func() *model.Profile { return p }, Store: st}, LocalOptions{Addr: "127.0.0.1:3000", CSRFToken: "tok"})
	return func(q string) (int, probesJSON) {
		w := do(h, "GET", "/api/probes?"+q, map[string]string{"Host": "127.0.0.1:3000"})
		var pj probesJSON
		if w.Code == 200 {
			if err := json.Unmarshal(w.Body.Bytes(), &pj); err != nil {
				t.Fatalf("%s: %v", q, err)
			}
		}
		return w.Code, pj
	}
}

func TestProbesParams(t *testing.T) {
	get := probesHandler(t, time.Now())
	for q, code := range map[string]int{
		"target=Alpha&kind=tcp&over_ms=5":                    200,
		"target=Alpha&kind=https&over_ms=0&limit=0":          200,
		"kind=tcp&over_ms=5":                                 400,
		"target=Alpha&over_ms=5":                             400,
		"target=Alpha&kind=tcp":                              400,
		"target=Alpha&kind=tcp&over_ms=-1":                   400,
		"target=Alpha&kind=tcp&over_ms=NaN":                  400,
		"target=Alpha&kind=tcp&over_ms=60001":                400,
		"target=Alpha&kind=tcp&over_ms=abc":                  400,
		"target=Alpha&kind=tcp&over_ms=5&limit=5001":         400,
		"target=Alpha&kind=tcp&over_ms=5&limit=-1":           400,
		"target=Alpha&kind=tcp&over_ms=5&nope=1":             400,
		"target=Alpha&kind=tcp&over_ms=5&over_ms=6":          400,
		"target=Alpha&kind=tcp&over_ms=5&from=now&to=now-1h": 400,
		"target=Nope&kind=tcp&over_ms=5":                     404,
		"target=Beta&kind=tcp&over_ms=5":                     404,
		"target=Alpha&kind=trace&over_ms=5":                  404,
		"target=Alpha&kind=tcp&over_ms=5&from=now-400d":      200,
	} {
		if c, _ := get(q); c != code {
			t.Errorf("%s = %d, want %d", q, c, code)
		}
	}
}

func TestProbes(t *testing.T) {
	now := time.Now().Truncate(time.Minute)
	key := model.SeriesKey{Target: "Alpha", Kind: model.KindTCP}
	at := func(min int) time.Time { return now.Add(-time.Duration(min) * time.Minute) }
	slow := func(min int, ms float64) model.Sample {
		return model.Sample{Key: key, Slot: at(min), RTT: time.Duration(ms * float64(time.Millisecond))}
	}
	get := probesHandler(t, now,
		slow(25, 1040), slow(20, 50), slow(15, 1050), slow(10, 30),
		slow(9, 999.99), slow(8, 1399.99), slow(7, 1400),
		model.Sample{Key: key, Slot: at(5), Lost: true, Reason: model.ReasonTimeout},
		model.Sample{Key: key, Slot: at(4), Lost: true, Reason: model.ReasonRefused},
		model.Sample{Key: key, Slot: at(3), Lost: true, Reason: model.ReasonGap})
	ms := func(pj probesJSON) []float64 {
		var v []float64
		for _, p := range pj.Probes {
			v = append(v, p.Ms)
		}
		return v
	}

	_, pj := get("target=Alpha&kind=tcp&over_ms=25&from=now-31m")
	if pj.Over != 7 || pj.CompleteAboveMs != nil || fmt.Sprint(ms(pj)) != "[1040 50 1050 30 999.99 1399.99 1400]" {
		t.Errorf("all: over %d, complete %v, probes %v", pj.Over, pj.CompleteAboveMs, ms(pj))
	}
	if pj.N != 117 || pj.LostTotal != 2 || pj.LostBy["timeout"] != 1 || pj.LostBy["refused"] != 1 || len(pj.Lost) != 2 || pj.LostCapped {
		t.Errorf("counts: n %d, lost %d %v %v", pj.N, pj.LostTotal, pj.LostBy, pj.Lost)
	}
	if pj.Lost[0].TS != at(5).UnixMilli() || pj.Lost[0].Reason != "timeout" {
		t.Errorf("lost[0] = %+v", pj.Lost[0])
	}
	// 1040, 1050 and 1399.99: 999.99 is under the band and 1400 past it.
	if pj.LikelyDrop == nil || *pj.LikelyDrop != 3 {
		t.Errorf("likely_drop = %v, want 3", pj.LikelyDrop)
	}

	// Capped: the slowest 3, in time order; everything over 1050 is listed.
	_, pj = get("target=Alpha&kind=tcp&over_ms=25&limit=3&from=now-31m")
	if pj.Over != 7 || fmt.Sprint(ms(pj)) != "[1050 1399.99 1400]" || pj.CompleteAboveMs == nil || *pj.CompleteAboveMs != 1050 {
		t.Errorf("limit 3: over %d, probes %v, complete %v", pj.Over, ms(pj), pj.CompleteAboveMs)
	}
	if pj.Probes[0].TS != at(15).UnixMilli() {
		t.Errorf("limit 3: first probe at %d, want %d", pj.Probes[0].TS, at(15).UnixMilli())
	}
	_, pj = get("target=Alpha&kind=tcp&over_ms=25&limit=0&from=now-31m")
	if pj.Over != 7 || len(pj.Probes) != 0 || pj.CompleteAboveMs == nil || *pj.CompleteAboveMs != 1400 {
		t.Errorf("limit 0: over %d, probes %v, complete %v", pj.Over, ms(pj), pj.CompleteAboveMs)
	}
	_, pj = get("target=Alpha&kind=tcp&over_ms=5000&from=now-31m")
	if pj.Over != 0 || len(pj.Probes) != 0 || pj.CompleteAboveMs != nil {
		t.Errorf("none over: over %d, probes %v, complete %v", pj.Over, ms(pj), pj.CompleteAboveMs)
	}
	_, pj = get("target=Alpha&kind=https&over_ms=5&from=now-31m")
	if pj.LikelyDrop != nil || pj.Over != 120 {
		t.Errorf("https: likely_drop %v, over %d", pj.LikelyDrop, pj.Over)
	}
}

// Fake.Raw returns samples in the order they were observed: oldest first here.
func TestProbesLostCapped(t *testing.T) {
	now := time.Now().Truncate(time.Minute)
	var lost []model.Sample
	for i := range MaxProbes + 1 {
		lost = append(lost, model.Sample{Key: model.SeriesKey{Target: "Alpha", Kind: model.KindHTTPS},
			Slot: now.Add(-time.Duration(MaxProbes+1-i) * 5 * time.Second), Lost: true, Reason: model.ReasonTimeout})
	}
	get := probesHandler(t, now, lost...)
	_, pj := get("target=Alpha&kind=https&over_ms=5&from=now-8h")
	if pj.LostTotal != MaxProbes+1 || len(pj.Lost) != MaxProbes || !pj.LostCapped || pj.Lost[0].TS > pj.Lost[1].TS {
		t.Errorf("lost %d, listed %d, capped %v", pj.LostTotal, len(pj.Lost), pj.LostCapped)
	}
}

// The real store returns a flushed hour and the hour still in memory in time
// order, and an unmeasured slot inside a stored hour counts nowhere.
func TestProbesSQLite(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 30, 0, 0, time.UTC)
	st, err := store.Open(t.TempDir(), store.Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	key := model.SeriesKey{Target: "Alpha", Kind: model.KindTCP}
	from := time.Date(2026, 9, 26, 11, 0, 0, 0, time.UTC)
	special := map[time.Time]model.Sample{
		from.Add(10 * time.Minute): {Lost: true, Reason: model.ReasonTimeout},
		from.Add(20 * time.Minute): {RTT: 1040 * time.Millisecond},
		from.Add(70 * time.Minute): {RTT: 1050 * time.Millisecond},
		from.Add(80 * time.Minute): {Lost: true, Reason: model.ReasonRefused},
	}
	for ts := from; ts.Before(now); ts = ts.Add(15 * time.Second) {
		if ts.Equal(from.Add(50 * time.Minute)) {
			continue // not measured
		}
		s, ok := special[ts]
		if !ok {
			s.RTT = 20 * time.Millisecond
		}
		s.Key, s.Slot = key, ts
		st.Observe(s)
	}
	if err := st.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	get := probesGet(t, st)
	code, pj := get(fmt.Sprintf("target=Alpha&kind=tcp&over_ms=25&from=%d&to=%d", from.UnixMilli(), now.UnixMilli()))
	if code != 200 {
		t.Fatalf("code %d", code)
	}
	if pj.N != 357 || pj.Over != 2 || pj.LostTotal != 2 || pj.LikelyDrop == nil || *pj.LikelyDrop != 2 {
		t.Errorf("n %d, over %d, lost %d, likely_drop %v", pj.N, pj.Over, pj.LostTotal, pj.LikelyDrop)
	}
	if fmt.Sprint(pj.Probes) != fmt.Sprintf("[{%d 1040} {%d 1050}]", from.Add(20*time.Minute).UnixMilli(), from.Add(70*time.Minute).UnixMilli()) {
		t.Errorf("probes %v", pj.Probes)
	}
	if fmt.Sprint(pj.Lost) != fmt.Sprintf("[{%d timeout} {%d refused}]", from.Add(10*time.Minute).UnixMilli(), from.Add(80*time.Minute).UnixMilli()) {
		t.Errorf("lost %v", pj.Lost)
	}
}
