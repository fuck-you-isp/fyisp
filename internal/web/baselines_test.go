package web

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

// ---------- fakes ----------

type fakeBaselines struct{ median float64 }

func (f fakeBaselines) Get(k model.SeriesKey, at time.Time) (model.Baseline, bool) {
	if k.Target == "NAS" {
		return model.Baseline{}, false
	}
	return model.Baseline{Key: k, MedianMs: f.median, P95Ms: f.median * 1.5, Loss: 0.001, Samples: 1000, HourOfDay: true}, true
}

func baselinesFixture(t *testing.T) Deps {
	t.Helper()
	_, d := fixture(t, time.Now())
	d.Baselines = fakeBaselines{median: 10}
	d.Slow = func() []SlowTarget {
		return []SlowTarget{
			{Target: "Alpha", Kind: model.KindHTTPS, Ratio: 2.346, NowMs: 23.456, NormalMs: 10},
			{Target: "evil.example.com", Kind: model.KindHTTPS, Ratio: 3, NowMs: 30, NormalMs: 10},
			{Target: "Beta", Kind: model.KindHTTPS, Ratio: math.NaN(), NowMs: 1, NormalMs: 1},
		}
	}
	return d
}

var goodHdr = map[string]string{"Host": "127.0.0.1:3000", "Origin": "http://127.0.0.1:3000", "Sec-Fetch-Site": "same-origin", HeaderCSRF: "tok123", "Content-Type": "application/json"}

func doBody(h http.Handler, method, target, body string, hdr map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	for k, v := range hdr {
		if k == "Host" {
			r.Host = v
			continue
		}
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// ---------- nil deps ----------

func TestBaselinesNil(t *testing.T) {
	_, d := fixture(t, time.Now())
	h := Local(d, LocalOptions{Addr: "127.0.0.1:3000", CSRFToken: "tok123"})
	if w := doBody(h, "GET", "/api/baselines?group=common", "", goodHdr); w.Code != 404 {
		t.Errorf("nil baselines = %d", w.Code)
	}
	var pj profileJSON
	_ = json.Unmarshal(do(h, "GET", "/api/profile", goodHdr).Body.Bytes(), &pj)
	if pj.Features.Baselines {
		t.Errorf("features with nil deps: %+v", pj.Features)
	}
	var v map[string]any
	_ = json.Unmarshal(do(h, "GET", "/api/verdict", goodHdr).Body.Bytes(), &v)
	if _, ok := v["slow"]; ok {
		t.Error("slow served without Deps.Slow")
	}
}

// TestRemovedRoutes: v0.10.0 removed notes and reports. Their routes are
// plain 404s for every method, even with good write headers or none (never
// 403 or 405, which would tell a caller about the controls), and
// /api/profile no longer has their features.
func TestRemovedRoutes(t *testing.T) {
	_, d := fixture(t, time.Now())
	for _, o := range []LocalOptions{
		{Addr: "127.0.0.1:3000", CSRFToken: "tok123"},
		{Addr: "127.0.0.1:3000", CSRFToken: "tok123", AdminToken: "adm"},
	} {
		h := Local(d, o)
		for _, p := range []string{"/api/annotations", "/api/annotations/1", "/api/reports", "/api/reports/r" + strings.Repeat("a", 25), "/api/reports/r" + strings.Repeat("a", 25) + "?download=1"} {
			for _, m := range []string{"GET", "HEAD", "POST", "PUT", "DELETE"} {
				for _, hdr := range []map[string]string{goodHdr, {"Host": "127.0.0.1:3000"}} {
					if w := doBody(h, m, p, "{}", hdr); w.Code != 404 {
						t.Errorf("admin %q: %s %s = %d, want 404", o.AdminToken, m, p, w.Code)
					}
				}
			}
		}
		w := do(h, "GET", "/api/profile", goodHdr)
		var pj struct {
			Features map[string]bool `json:"features"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &pj); err != nil || pj.Features == nil {
			t.Fatalf("profile: %v %s", err, w.Body.String())
		}
		for _, k := range []string{"notes", "reports"} {
			if _, ok := pj.Features[k]; ok {
				t.Errorf("profile still has features.%s", k)
			}
		}
	}
}

// ---------- baselines ----------

func TestBaselines(t *testing.T) {
	d := baselinesFixture(t)
	for _, tc := range []struct {
		name, prefix string
		h            http.Handler
	}{
		{"local", "/", Local(d, LocalOptions{Addr: "127.0.0.1:3000"})},
	} {
		w := do(tc.h, "GET", tc.prefix+"api/baselines?group=lan", map[string]string{"Host": "127.0.0.1:3000", "Cf-Connecting-Ip": "198.51.100.60"})
		if w.Code != 200 {
			t.Fatalf("%s baselines = %d %s", tc.name, w.Code, w.Body.String())
		}
		if m := leakRE.FindString(w.Body.String()); m != "" {
			t.Errorf("%s baselines leak %q", tc.name, m)
		}
		var b baselinesJSON
		_ = json.Unmarshal(w.Body.Bytes(), &b)
		// Router has three kinds; NAS has no baseline yet.
		if b.Group != "lan" || len(b.Series) != 3 {
			t.Fatalf("%s baselines = %s", tc.name, w.Body.String())
		}
		for _, s := range b.Series {
			if s.Target != "Router" || s.MedianMs != 10 || s.P95Ms != 15 || s.RatioNow == nil || math.Abs(*s.RatioNow-2) > 0.01 || *s.NowMs != 20 {
				t.Errorf("%s series = %+v", tc.name, s)
			}
		}
		for i, c := range []struct {
			p    string
			code int
		}{
			{"api/baselines?group=", 400}, {"api/baselines?group=nope", 404}, {"api/baselines?group=lan&at=x", 400},
			{"api/baselines?group=lan&x=1", 400}, {"api/baselines?group=lan&at=now-1h", 200},
			{fmt.Sprintf("api/baselines?group=lan&at=%d", time.Now().Add(-time.Hour).UnixMilli()), 200},
		} {
			if w := do(tc.h, "GET", tc.prefix+c.p, map[string]string{"Host": "127.0.0.1:3000", "Cf-Connecting-Ip": fmt.Sprintf("198.51.100.%d", 70+i)}); w.Code != c.code {
				t.Errorf("%s %s = %d, want %d", tc.name, c.p, w.Code, c.code)
			}
		}
		if w := do(tc.h, "POST", tc.prefix+"api/baselines?group=lan", map[string]string{"Host": "127.0.0.1:3000", "Cf-Connecting-Ip": "198.51.100.80"}); w.Code != 405 {
			t.Errorf("%s POST baselines = %d", tc.name, w.Code)
		}
	}
	// Without a group: every group's series in one response.
	h := Local(d, LocalOptions{Addr: "127.0.0.1:3000"})
	var all baselinesJSON
	_ = json.Unmarshal(do(h, "GET", "/api/baselines", map[string]string{"Host": "127.0.0.1:3000"}).Body.Bytes(), &all)
	if all.Group != "" || len(all.Series) != 3+1+3 { // Alpha 3 kinds, Beta 1, Router 3; NAS has none
		t.Errorf("all baselines = %+v", all)
	}
	// Long ago there was no data: no ratio.
	var b baselinesJSON
	_ = json.Unmarshal(do(h, "GET", "/api/baselines?group=common&at=now-2d", map[string]string{"Host": "127.0.0.1:3000"}).Body.Bytes(), &b)
	if len(b.Series) == 0 || b.Series[0].RatioNow != nil {
		t.Errorf("baselines 2 days ago = %+v", b)
	}
}

func TestVerdictSlow(t *testing.T) {
	d := baselinesFixture(t)
	for _, tc := range []struct {
		name, path string
		h          http.Handler
	}{
		{"local", "/api/verdict", Local(d, LocalOptions{Addr: "127.0.0.1:3000"})},
	} {
		w := do(tc.h, "GET", tc.path, map[string]string{"Host": "127.0.0.1:3000", "Cf-Connecting-Ip": "198.51.100.90"})
		var v struct {
			Slow []slowJSON `json:"slow"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &v)
		if len(v.Slow) != 1 || v.Slow[0].Target != "Alpha" || v.Slow[0].Ratio != 2.35 || v.Slow[0].NowMs != 23.46 || v.Slow[0].Kind != "https" {
			t.Errorf("%s slow = %s", tc.name, w.Body.String())
		}
		if m := leakRE.FindString(w.Body.String()); m != "" {
			t.Errorf("%s verdict leaks %q", tc.name, m)
		}
	}
}
