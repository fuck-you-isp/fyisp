package web

// Adversarial tests of the on-demand trace start budget and its admin
// bypass (from the PR #39 security review).

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

// instantTrace ends every session as soon as it starts, so slots never
// bound anything and only the budget does.
type instantTrace struct {
	*fakeTrace
	mu sync.Mutex
	n  int
}

func (f *instantTrace) Investigate(target string, rounds int) (Investigation, error) {
	if f.fakeTrace.err != nil {
		return nil, f.fakeTrace.err
	}
	f.mu.Lock()
	f.n++
	f.mu.Unlock()
	return &fakeInv{target: target, max: rounds, rounds: rounds, ended: time.Now()}, nil
}

func budgetFixture(t *testing.T, o LocalOptions, ntargets int) (*localHandler, *instantTrace, *time.Time) {
	d, ft, _ := traceFixture(t)
	it := &instantTrace{fakeTrace: ft}
	d.Trace = it
	p := d.Profile()
	for i := range ntargets {
		p.Targets = append(p.Targets, model.Target{Name: fmt.Sprintf("R%d", i), Group: "common"})
	}
	o.CSRFToken = "tok123"
	h := Local(d, o).(*localHandler)
	now := time.Now()
	h.inv.now = func() time.Time { return now }
	return h, it, &now
}

func sameOriginHdr(host string) map[string]string {
	return map[string]string{"Host": host, "Origin": "http://" + host, "Sec-Fetch-Site": "same-origin", HeaderCSRF: "tok123"}
}

// Concurrent token-less starts against distinct targets that end at once:
// exactly InvestigateStartsPerHour succeed, the rest are 429 rate_limited
// with a positive Retry-After equal to retry_after.
func TestInvestigateBudgetRace(t *testing.T) {
	for _, o := range []LocalOptions{
		{Addr: "127.0.0.1:3000"},
		{Addr: "0.0.0.0:3000"},
		{Addr: "0.0.0.0:3000", AdminToken: "adm"},
		{Addr: "127.0.0.1:3000", ExtraHosts: []string{"t.example"}},
	} {
		host := "127.0.0.1:3000"
		if o.Addr == "0.0.0.0:3000" {
			host = "192.168.1.5:3000"
		}
		h, it, _ := budgetFixture(t, o, 200)
		var wg sync.WaitGroup
		var mu sync.Mutex
		codes := map[string]int{}
		for i := range 200 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				hdr := sameOriginHdr(host)
				hdr["X-Forwarded-For"] = fmt.Sprintf("198.51.100.%d", i%250) // varying client addresses change nothing
				hdr[HeaderAdmin] = "wrong"
				w := do(h, "POST", fmt.Sprintf("/api/investigate?target=R%d", i), hdr)
				var e struct {
					Code  string `json:"code"`
					Retry int    `json:"retry_after"`
				}
				_ = json.Unmarshal(w.Body.Bytes(), &e)
				k := fmt.Sprintf("%d %s", w.Code, e.Code)
				if w.Code == 429 {
					ra, _ := strconv.Atoi(w.Header().Get("Retry-After"))
					if ra <= 0 || ra != e.Retry || ra > 3600 {
						t.Errorf("Retry-After %q retry_after %d", w.Header().Get("Retry-After"), e.Retry)
					}
				}
				mu.Lock()
				codes[k]++
				mu.Unlock()
			}()
		}
		wg.Wait()
		if codes["200 "] != InvestigateStartsPerHour || codes["429 rate_limited"] != 200-InvestigateStartsPerHour || it.n != InvestigateStartsPerHour {
			t.Errorf("%+v: %v, tracer starts %d", o, codes, it.n)
		}
	}
}

// Refused requests never touch the budget: bad CSRF, cross origin, GET,
// a body, extra params, unknown targets, tracer errors, busy slots.
func TestInvestigateRefusedDontCount(t *testing.T) {
	h, it, _ := budgetFixture(t, LocalOptions{Addr: "127.0.0.1:3000"}, 40)
	good := sameOriginHdr("127.0.0.1:3000")
	bad := []func() *httptest.ResponseRecorder{
		func() *httptest.ResponseRecorder { return do(h, "GET", "/api/investigate?target=R0", good) },
		func() *httptest.ResponseRecorder { return do(h, "HEAD", "/api/investigate?target=R0", good) },
		func() *httptest.ResponseRecorder {
			return do(h, "POST", "/api/investigate?target=R0", withHdr(good, HeaderCSRF, "x"))
		},
		func() *httptest.ResponseRecorder {
			return do(h, "POST", "/api/investigate?target=R0", withHdr(good, "Origin", "http://evil.example"))
		},
		func() *httptest.ResponseRecorder {
			return do(h, "POST", "/api/investigate?target=R0&target=R1", good)
		},
		func() *httptest.ResponseRecorder { return do(h, "POST", "/api/investigate?target=R0&x=1", good) },
		func() *httptest.ResponseRecorder { return do(h, "POST", "/api/investigate?target=192.0.2.1", good) },
		func() *httptest.ResponseRecorder { return do(h, "POST", "/api/investigate?target=", good) },
		func() *httptest.ResponseRecorder { return doBody(h, "POST", "/api/investigate?target=R0", "x", good) },
	}
	for round := range 5 {
		for i, f := range bad {
			if w := f(); w.Code == 200 {
				t.Fatalf("round %d case %d: 200", round, i)
			}
		}
	}
	it.fakeTrace.err = fmt.Errorf("boom")
	for range 30 {
		if w := do(h, "POST", "/api/investigate?target=R0", good); w.Code != 500 {
			t.Fatalf("tracer error = %d", w.Code)
		}
	}
	it.fakeTrace.err = nil
	for i := range InvestigateStartsPerHour {
		if w := do(h, "POST", fmt.Sprintf("/api/investigate?target=R%d", i), good); w.Code != 200 {
			t.Fatalf("start %d = %d %s", i, w.Code, w.Body.String())
		}
	}
	if w := do(h, "POST", "/api/investigate?target=R30", good); w.Code != 429 {
		t.Fatalf("13th = %d", w.Code)
	}
}

// The budget's edge: exactly one hour after a start it frees; one ns
// before, Retry-After is 1 (never 0).
func TestInvestigateBudgetEdge(t *testing.T) {
	h, _, now := budgetFixture(t, LocalOptions{Addr: "127.0.0.1:3000"}, 40)
	good := sameOriginHdr("127.0.0.1:3000")
	t0 := *now
	for i := range InvestigateStartsPerHour {
		if w := do(h, "POST", fmt.Sprintf("/api/investigate?target=R%d", i), good); w.Code != 200 {
			t.Fatal(w.Code)
		}
	}
	*now = t0.Add(time.Hour - time.Nanosecond)
	w := do(h, "POST", "/api/investigate?target=R20", good)
	if w.Code != 429 || w.Header().Get("Retry-After") != "1" {
		t.Fatalf("1ns before = %d %q", w.Code, w.Header().Get("Retry-After"))
	}
	*now = t0.Add(time.Hour)
	for i := range InvestigateStartsPerHour {
		if w := do(h, "POST", fmt.Sprintf("/api/investigate?target=R%d", 20+i), good); w.Code != 200 {
			t.Fatalf("after an hour start %d = %d", i, w.Code)
		}
	}
	if w := do(h, "POST", "/api/investigate?target=R39", good); w.Code != 429 || w.Header().Get("Retry-After") != "3600" {
		t.Fatalf("then = %d %q", w.Code, w.Header().Get("Retry-After"))
	}
	// A clock that goes back (tests only: time.Now is monotonic) keeps the budget.
	*now = t0
	if w := do(h, "POST", "/api/investigate?target=R39", good); w.Code != 429 {
		t.Fatalf("clock back = %d", w.Code)
	}
}

// The admin token's bypass: only the right token, by header or by the
// port's cookie; nothing on a dashboard without a token (empty header,
// empty cookie). The slot cap still applies to the owner.
func TestInvestigateAdminBypass(t *testing.T) {
	// Token-less dashboards: no way around the budget.
	for _, o := range []LocalOptions{{Addr: "127.0.0.1:3000"}, {Addr: "0.0.0.0:3000"}, {Addr: "127.0.0.1:3000", ExtraHosts: []string{"t.example"}}} {
		host := "127.0.0.1:3000"
		if o.Addr == "0.0.0.0:3000" {
			host = "192.168.1.5:3000"
		}
		h, _, _ := budgetFixture(t, o, 40)
		for i := range 30 {
			hdr := sameOriginHdr(host)
			hdr[HeaderAdmin] = ""
			r := httptest.NewRequest("POST", fmt.Sprintf("/api/investigate?target=R%d", i), nil)
			r.Host = host
			for k, v := range hdr {
				if k != "Host" {
					r.Header.Set(k, v)
				}
			}
			r.Header["X-Fyisp-Admin"] = []string{""}
			r.AddCookie(&http.Cookie{Name: "fyisp_admin_3000", Value: ""})
			r.AddCookie(&http.Cookie{Name: "fyisp_admin", Value: ""})
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if want := 200; i >= InvestigateStartsPerHour {
				want = 429
				if w.Code != want {
					t.Errorf("%+v start %d = %d", o, i, w.Code)
				}
			} else if w.Code != want {
				t.Errorf("%+v start %d = %d", o, i, w.Code)
			}
		}
	}
	// With a token: wrong values, other ports' cookies are budgeted; the
	// right header or cookie is not.
	o := LocalOptions{Addr: "0.0.0.0:3000", AdminToken: "adm"}
	h, _, _ := budgetFixture(t, o, 80)
	good := sameOriginHdr("192.168.1.5:3000")
	n := 0
	post := func(mod func(r *http.Request)) int {
		r := httptest.NewRequest("POST", fmt.Sprintf("/api/investigate?target=R%d", n), nil)
		n++
		r.Host = "192.168.1.5:3000"
		for k, v := range good {
			if k != "Host" {
				r.Header.Set(k, v)
			}
		}
		mod(r)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	for i := range InvestigateStartsPerHour {
		var c int
		switch i % 4 {
		case 0:
			c = post(func(r *http.Request) { r.Header.Set(HeaderAdmin, "ADM") })
		case 1:
			c = post(func(r *http.Request) { r.AddCookie(&http.Cookie{Name: "fyisp_admin_3001", Value: "adm"}) })
		case 2:
			c = post(func(r *http.Request) { r.AddCookie(&http.Cookie{Name: "fyisp_admin", Value: "adm"}) })
		case 3:
			c = post(func(r *http.Request) { r.Header.Set(HeaderAdmin, "adm ") })
		}
		if c != 200 {
			t.Fatalf("budgeted %d = %d", i, c)
		}
	}
	if c := post(func(r *http.Request) { r.Header.Set(HeaderAdmin, "admx") }); c != 429 {
		t.Fatalf("over = %d", c)
	}
	for range 20 {
		if c := post(func(r *http.Request) { r.AddCookie(&http.Cookie{Name: "fyisp_admin_3000", Value: "adm"}) }); c != 200 {
			t.Fatalf("cookie = %d", c)
		}
		if c := post(func(r *http.Request) { r.Header.Set(HeaderAdmin, "adm") }); c != 200 {
			t.Fatalf("header = %d", c)
		}
	}
	// The admin token never skips the origin and CSRF checks.
	if c := post(func(r *http.Request) { r.Header.Set(HeaderAdmin, "adm"); r.Header.Del(HeaderCSRF) }); c != 403 {
		t.Fatalf("admin without CSRF = %d", c)
	}
	if c := post(func(r *http.Request) { r.Header.Set(HeaderAdmin, "adm"); r.Header.Set("Origin", "http://evil.example") }); c != 403 {
		t.Fatalf("admin cross-origin = %d", c)
	}
}

// Slots bind the owner too: with sessions that never end, 4 start, the
// 5th is busy, token or not; a token-less viewer's 429 is rate_limited
// (budget first) only once the budget is spent.
func TestInvestigateAdminSlots(t *testing.T) {
	d, ft, _ := traceFixture(t)
	p := d.Profile()
	for i := range 10 {
		p.Targets = append(p.Targets, model.Target{Name: fmt.Sprintf("S%d", i), Group: "common"})
	}
	h := Local(d, LocalOptions{Addr: "0.0.0.0:3000", AdminToken: "adm", CSRFToken: "tok123"})
	good := sameOriginHdr("192.168.1.5:3000")
	admin := withHdr(good, HeaderAdmin, "adm")
	for i := range MaxInvestigations {
		if w := do(h, "POST", fmt.Sprintf("/api/investigate?target=S%d", i), admin); w.Code != 200 {
			t.Fatal(w.Code)
		}
	}
	for i := MaxInvestigations; i < 10; i++ {
		if w := do(h, "POST", fmt.Sprintf("/api/investigate?target=S%d", i), admin); w.Code != 429 || !json.Valid(w.Body.Bytes()) {
			t.Fatalf("admin 5th = %d", w.Code)
		}
	}
	if len(ft.started) != MaxInvestigations {
		t.Fatalf("started %d", len(ft.started))
	}
}

func withHdr(m map[string]string, k, v string) map[string]string {
	o := map[string]string{}
	for a, b := range m {
		o[a] = b
	}
	o[k] = v
	return o
}
