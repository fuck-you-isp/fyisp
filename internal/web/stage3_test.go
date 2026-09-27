package web

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

// ---------- fakes ----------

type fakeNotes struct {
	mu     sync.Mutex
	next   int64
	m      map[int64]model.Annotation
	lastPO bool // publicOnly of the last query
	liar   bool // ignore publicOnly (the handler must filter anyway)
}

func newFakeNotes() *fakeNotes { return &fakeNotes{m: map[int64]model.Annotation{}} }

func (f *fakeNotes) AddAnnotation(_ context.Context, a *model.Annotation) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	a.ID, a.Created, a.Updated = f.next, time.Now(), time.Now()
	f.m[a.ID] = *a
	return nil
}
func (f *fakeNotes) UpdateAnnotation(_ context.Context, a *model.Annotation) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	old, ok := f.m[a.ID]
	if !ok {
		return fmt.Errorf("annotation %d: %w", a.ID, ErrNotFound)
	}
	a.Created, a.Updated = old.Created, time.Now()
	f.m[a.ID] = *a
	return nil
}
func (f *fakeNotes) DeleteAnnotation(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.m[id]; !ok {
		return ErrNotFound
	}
	delete(f.m, id)
	return nil
}
func (f *fakeNotes) Annotations(_ context.Context, from, to time.Time, publicOnly bool) ([]model.Annotation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastPO = publicOnly
	var out []model.Annotation
	for _, a := range f.m {
		end := a.End
		if end.IsZero() {
			end = a.At
		}
		if a.At.After(to) || end.Before(from) || (publicOnly && !a.Public && !f.liar) {
			continue
		}
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out, nil
}

type fakeReports struct {
	mu     sync.Mutex
	m      map[string]model.ReportMeta
	html   map[string][]byte
	builds []ReportOptions
}

func newFakeReports() *fakeReports {
	return &fakeReports{m: map[string]model.ReportMeta{}, html: map[string][]byte{}}
}

func (f *fakeReports) Build(_ context.Context, o ReportOptions) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.builds = append(f.builds, o)
	if o.Redact {
		return []byte("<!doctype html><title>r</title><style>body{color:red}</style><p>redacted report</p>"), nil
	}
	return []byte("<!doctype html><title>f</title><p>full report: gateway 192.168.1.1</p>"), nil
}
func (f *fakeReports) SaveReport(_ context.Context, m model.ReportMeta, b []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.m[m.ID], f.html[m.ID] = m, b
	return nil
}
func (f *fakeReports) Report(_ context.Context, id string) (model.ReportMeta, []byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.m[id]
	if !ok {
		return m, nil, fmt.Errorf("report %s: not found", id)
	}
	return m, f.html[id], nil
}
func (f *fakeReports) Reports(context.Context) ([]model.ReportMeta, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []model.ReportMeta
	for _, m := range f.m {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out, nil
}
func (f *fakeReports) SetReportPublic(_ context.Context, id string, public bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.m[id]
	if !ok {
		return ErrNotFound
	}
	m.Public = public
	f.m[id] = m
	return nil
}
func (f *fakeReports) DeleteReport(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.m[id]; !ok {
		return ErrNotFound
	}
	delete(f.m, id)
	delete(f.html, id)
	return nil
}

// put stores a report directly (bypassing the handler's rules).
func (f *fakeReports) put(id string, public bool, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.m[id] = model.ReportMeta{ID: id, Title: "t", Public: public, Created: time.Now(), Bytes: len(body)}
	f.html[id] = []byte(body)
}

type fakeBaselines struct{ median float64 }

func (f fakeBaselines) Get(k model.SeriesKey, at time.Time) (model.Baseline, bool) {
	if k.Target == "NAS" {
		return model.Baseline{}, false
	}
	return model.Baseline{Key: k, MedianMs: f.median, P95Ms: f.median * 1.5, Loss: 0.001, Samples: 1000, HourOfDay: true}, true
}

func stage3Fixture(t *testing.T) (Deps, *fakeNotes, *fakeReports) {
	t.Helper()
	_, d, _ := fixture(t, time.Now())
	fn, fr := newFakeNotes(), newFakeReports()
	d.Annotations, d.Reports, d.Baselines = fn, fr, fakeBaselines{median: 10}
	d.Slow = func() []SlowTarget {
		return []SlowTarget{
			{Target: "Alpha", Kind: model.KindHTTPS, Ratio: 2.346, NowMs: 23.456, NormalMs: 10},
			{Target: "evil.example.com", Kind: model.KindHTTPS, Ratio: 3, NowMs: 30, NormalMs: 10},
			{Target: "Beta", Kind: model.KindHTTPS, Ratio: math.NaN(), NowMs: 1, NormalMs: 1},
		}
	}
	return d, fn, fr
}

var goodHdr = map[string]string{"Host": "127.0.0.1:3000", "Origin": "http://127.0.0.1:3000", "Sec-Fetch-Site": "same-origin", HeaderCSRF: "tok123", "Content-Type": "application/json"}

func hdrWith(over map[string]string, drop ...string) map[string]string {
	m := map[string]string{}
	for k, v := range goodHdr {
		m[k] = v
	}
	for k, v := range over {
		m[k] = v
	}
	for _, k := range drop {
		delete(m, k)
	}
	return m
}

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

func noteBody(at time.Time, text string, public bool) string {
	b, _ := json.Marshal(map[string]any{"at": at, "text": text, "public": public})
	return string(b)
}

func pubHdr(i int) map[string]string {
	return map[string]string{"Cf-Connecting-Ip": fmt.Sprintf("198.51.100.%d", i%250)}
}

// ---------- nil deps ----------

func TestStage3Nil(t *testing.T) {
	_, d, _ := fixture(t, time.Now())
	h := Local(d, LocalOptions{Addr: "127.0.0.1:3000", CSRFToken: "tok123"})
	for _, c := range []struct{ method, path string }{
		{"GET", "/api/annotations"}, {"POST", "/api/annotations"}, {"PUT", "/api/annotations/1"}, {"DELETE", "/api/annotations/1"},
		{"GET", "/api/reports"}, {"POST", "/api/reports"}, {"GET", "/api/reports/r" + strings.Repeat("a", 25)},
		{"GET", "/api/baselines?group=common"},
	} {
		if w := doBody(h, c.method, c.path, "{}", goodHdr); w.Code != 404 {
			t.Errorf("nil local %s %s = %d", c.method, c.path, w.Code)
		}
	}
	p := Public(d, testSecret)
	base := "/s/" + testSecret + "/"
	for i, path := range []string{"api/annotations", "api/baselines?group=common", "r/r" + strings.Repeat("a", 25), "api/reports"} {
		if w := do(p, "GET", base+path, pubHdr(i)); w.Code != 404 {
			t.Errorf("nil public %s = %d", path, w.Code)
		}
	}
	var pj profileJSON
	_ = json.Unmarshal(do(h, "GET", "/api/profile", goodHdr).Body.Bytes(), &pj)
	if pj.Features.Notes || pj.Features.Reports || pj.Features.Baselines {
		t.Errorf("features with nil deps: %+v", pj.Features)
	}
	var v map[string]any
	_ = json.Unmarshal(do(h, "GET", "/api/verdict", goodHdr).Body.Bytes(), &v)
	if _, ok := v["slow"]; ok {
		t.Error("slow served without Deps.Slow")
	}
}

// ---------- annotations ----------

func TestNotesCRUD(t *testing.T) {
	d, fn, _ := stage3Fixture(t)
	h := Local(d, LocalOptions{Addr: "127.0.0.1:3000", CSRFToken: "tok123"})
	now := time.Now()
	at := now.Add(-10 * time.Minute)

	w := doBody(h, "POST", "/api/annotations", noteBody(at, "  call dropped  ", false), goodHdr)
	if w.Code != 201 {
		t.Fatalf("add = %d %s", w.Code, w.Body.String())
	}
	var n noteJSON
	_ = json.Unmarshal(w.Body.Bytes(), &n)
	if n.ID != 1 || n.Text != "call dropped" || n.Public || !n.At.Equal(at.UTC().Truncate(time.Nanosecond)) || n.End != nil || n.Created == nil {
		t.Errorf("added = %+v", n)
	}
	// A range note.
	rb, _ := json.Marshal(map[string]any{"at": at.Add(-time.Hour), "end": at, "text": "ISP ticket #123 opened", "public": true})
	if w := doBody(h, "POST", "/api/annotations", string(rb), goodHdr); w.Code != 201 {
		t.Fatalf("add range = %d %s", w.Code, w.Body.String())
	}

	for name, c := range map[string]struct {
		body string
		hdr  map[string]string
		code int
	}{
		"empty text":       {noteBody(at, "   ", false), goodHdr, 400},
		"long text":        {noteBody(at, strings.Repeat("x", MaxAnnotationText+1), false), goodHdr, 400},
		"max text":         {noteBody(at, strings.Repeat("é", MaxAnnotationText), false), goodHdr, 201},
		"control char":     {noteBody(at, "a\x1b[31mb", false), goodHdr, 400},
		"no at":            {`{"text":"x"}`, goodHdr, 400},
		"future":           {noteBody(now.Add(48*time.Hour), "x", false), goodHdr, 400},
		"end before at":    {fmt.Sprintf(`{"at":%q,"end":%q,"text":"x"}`, at.Format(time.RFC3339), at.Add(-time.Minute).Format(time.RFC3339)), goodHdr, 400},
		"unknown field":    {`{"at":"2026-01-01T00:00:00Z","text":"x","id":5}`, goodHdr, 400},
		"trailing data":    {noteBody(at, "x", false) + `{}`, goodHdr, 400},
		"not json":         {`at=1&text=x`, goodHdr, 400},
		"form content":     {noteBody(at, "x", false), hdrWith(map[string]string{"Content-Type": "application/x-www-form-urlencoded"}), 415},
		"no content type":  {noteBody(at, "x", false), hdrWith(nil, "Content-Type"), 415},
		"huge body":        {`{"text":"` + strings.Repeat("x", maxJSONBody) + `"}`, goodHdr, 413},
		"cross-origin":     {noteBody(at, "x", false), hdrWith(map[string]string{"Origin": "http://evil.com"}, "Sec-Fetch-Site"), 403},
		"cross-site fetch": {noteBody(at, "x", false), hdrWith(map[string]string{"Sec-Fetch-Site": "cross-site"}), 403},
		"no origin":        {noteBody(at, "x", false), hdrWith(nil, "Origin", "Sec-Fetch-Site"), 403},
		"no token":         {noteBody(at, "x", false), hdrWith(nil, HeaderCSRF), 403},
		"bad token":        {noteBody(at, "x", false), hdrWith(map[string]string{HeaderCSRF: "nope"}), 403},
		"foreign host":     {noteBody(at, "x", false), hdrWith(map[string]string{"Host": "evil.com", "Origin": "http://evil.com"}), 421},
	} {
		if w := doBody(h, "POST", "/api/annotations", c.body, c.hdr); w.Code != c.code {
			t.Errorf("%s: %d, want %d (%s)", name, w.Code, c.code, w.Body.String())
		}
	}
	if len(fn.m) != 3 {
		t.Errorf("store has %d notes, want 3 (refused requests must not write)", len(fn.m))
	}

	// List (local: private and public, oldest first).
	w = do(h, "GET", "/api/annotations?from=now-3h", goodHdr)
	var list []noteJSON
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || w.Code != 200 {
		t.Fatalf("list = %d %s", w.Code, w.Body.String())
	}
	if len(list) != 3 || list[0].Text != "ISP ticket #123 opened" || list[0].End == nil || !list[0].Public || fn.lastPO {
		t.Errorf("list = %+v (publicOnly %v)", list, fn.lastPO)
	}
	for p, code := range map[string]int{"/api/annotations?x=1": 400, "/api/annotations?from=now&to=now-1h": 400, "/api/annotations/": 404} {
		if w := do(h, "GET", p, goodHdr); w.Code != code {
			t.Errorf("GET %s = %d, want %d", p, w.Code, code)
		}
	}

	// Update.
	w = doBody(h, "PUT", "/api/annotations/1", noteBody(at, "call dropped (Zoom)", true), goodHdr)
	_ = json.Unmarshal(w.Body.Bytes(), &n)
	if w.Code != 200 || n.Text != "call dropped (Zoom)" || !n.Public || fn.m[1].Text != "call dropped (Zoom)" {
		t.Errorf("update = %d %s", w.Code, w.Body.String())
	}
	for p, code := range map[string]int{"/api/annotations/99": 404, "/api/annotations/0": 404, "/api/annotations/01": 404, "/api/annotations/x": 404, "/api/annotations/1?x=1": 400} {
		if w := doBody(h, "PUT", p, noteBody(at, "x", false), goodHdr); w.Code != code {
			t.Errorf("PUT %s = %d, want %d", p, w.Code, code)
		}
	}
	if w := doBody(h, "PUT", "/api/annotations/1", noteBody(at, "x", false), hdrWith(nil, HeaderCSRF)); w.Code != 403 {
		t.Errorf("PUT without token = %d", w.Code)
	}
	// Methods.
	for _, c := range []struct{ m, p string }{{"PATCH", "/api/annotations"}, {"PUT", "/api/annotations"}, {"POST", "/api/annotations/1"}, {"GET", "/api/annotations/1"}} {
		if w := doBody(h, c.m, c.p, "{}", goodHdr); w.Code != 405 {
			t.Errorf("%s %s = %d, want 405", c.m, c.p, w.Code)
		}
	}
	// Delete.
	if w := doBody(h, "DELETE", "/api/annotations/1", "", hdrWith(nil, HeaderCSRF)); w.Code != 403 || len(fn.m) != 3 {
		t.Errorf("DELETE without token = %d", w.Code)
	}
	if w := doBody(h, "DELETE", "/api/annotations/1", "", goodHdr); w.Code != 200 || len(fn.m) != 2 {
		t.Errorf("DELETE = %d %s", w.Code, w.Body.String())
	}
	if w := doBody(h, "DELETE", "/api/annotations/1", "", goodHdr); w.Code != 404 {
		t.Errorf("DELETE again = %d", w.Code)
	}

	// Disabled controls (LAN bind, no admin token) refuse writes but serve reads.
	hb := Local(d, LocalOptions{Addr: "0.0.0.0:3000", CSRFToken: "tok123"})
	if w := doBody(hb, "POST", "/api/annotations", noteBody(at, "x", false), goodHdr); w.Code != 403 {
		t.Errorf("disabled controls POST = %d", w.Code)
	}
	if w := do(hb, "GET", "/api/annotations", goodHdr); w.Code != 200 {
		t.Errorf("disabled controls GET = %d", w.Code)
	}
	// Admin token required when set.
	ha := Local(d, LocalOptions{Addr: "0.0.0.0:3000", CSRFToken: "tok123", AdminToken: "adm"})
	if w := doBody(ha, "POST", "/api/annotations", noteBody(at, "x", false), goodHdr); w.Code != 403 {
		t.Errorf("admin missing = %d", w.Code)
	}
	if w := doBody(ha, "POST", "/api/annotations", noteBody(at, "x", false), hdrWith(map[string]string{HeaderAdmin: "adm"})); w.Code != 201 {
		t.Errorf("admin ok = %d %s", w.Code, w.Body.String())
	}
}

func TestNotesPublic(t *testing.T) {
	d, fn, _ := stage3Fixture(t)
	now := time.Now()
	ctx := context.Background()
	_ = fn.AddAnnotation(ctx, &model.Annotation{At: now.Add(-5 * time.Minute), Text: "router 192.168.1.1 rebooted, see /home/venkat/notes secret-path", Public: false})
	_ = fn.AddAnnotation(ctx, &model.Annotation{At: now.Add(-4 * time.Minute), Text: "ISP ticket opened", Public: true})
	fn.liar = true // a store that ignores publicOnly must not leak either
	h := Public(d, testSecret)
	base := "/s/" + testSecret + "/"
	w := do(h, "GET", base+"api/annotations?from=now-1h", pubHdr(1))
	if w.Code != 200 {
		t.Fatalf("public notes = %d %s", w.Code, w.Body.String())
	}
	if !fn.lastPO {
		t.Error("public query did not ask for public notes only")
	}
	var list []map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &list)
	if len(list) != 1 || list[0]["text"] != "ISP ticket opened" {
		t.Errorf("public notes = %s", w.Body.String())
	}
	if _, ok := list[0]["created"]; ok {
		t.Error("public note has created")
	}
	if m := leakRE.FindString(w.Body.String()); m != "" || strings.Contains(w.Body.String(), "rebooted") {
		t.Errorf("public notes leak %q: %s", m, w.Body.String())
	}
	for i, c := range []struct {
		m, p string
		code int
	}{
		{"POST", "api/annotations", 405}, {"PUT", "api/annotations", 405}, {"DELETE", "api/annotations", 405},
		{"GET", "api/annotations/1", 404}, {"PUT", "api/annotations/1", 404}, {"DELETE", "api/annotations/2", 404},
		{"GET", "api/annotations?x=1", 400}, {"GET", "api/reports", 404}, {"POST", "api/reports", 404},
	} {
		if w := doBody(h, c.m, base+c.p, noteBody(now, "x", true), hdrWith(pubHdr(10+i))); w.Code != c.code {
			t.Errorf("public %s %s = %d, want %d", c.m, c.p, w.Code, c.code)
		}
	}
	if len(fn.m) != 2 {
		t.Error("public request wrote a note")
	}
	var pj profileJSON
	_ = json.Unmarshal(do(h, "GET", base+"api/profile", pubHdr(30)).Body.Bytes(), &pj)
	if !pj.Features.Notes || pj.Features.Reports || !pj.Features.Baselines {
		t.Errorf("public features = %+v", pj.Features)
	}
}

// ---------- reports ----------

func reportBody(from, to time.Time, title string, redact, public bool) string {
	b, _ := json.Marshal(map[string]any{"from": from, "to": to, "title": title, "redact": redact, "public": public})
	return string(b)
}

func TestReports(t *testing.T) {
	d, _, fr := stage3Fixture(t)
	h := Local(d, LocalOptions{Addr: "127.0.0.1:3000", CSRFToken: "tok123"})
	now := time.Now()
	from, to := now.Add(-2*time.Hour), now.Add(-time.Hour)

	for name, c := range map[string]struct {
		body string
		hdr  map[string]string
		code int
	}{
		"public needs redact": {reportBody(from, to, "x", false, true), goodHdr, 400},
		"reversed":            {reportBody(to, from, "x", true, false), goodHdr, 400},
		"too long":            {reportBody(now.Add(-100*24*time.Hour), now, "x", true, false), goodHdr, 400},
		"no range":            {`{"title":"x"}`, goodHdr, 400},
		"long title":          {reportBody(from, to, strings.Repeat("t", MaxReportTitle+1), true, false), goodHdr, 400},
		"control in title":    {reportBody(from, to, "a\nb", true, false), goodHdr, 400},
		"unknown field":       {`{"from":"2026-01-01T00:00:00Z","to":"2026-01-02T00:00:00Z","x":1}`, goodHdr, 400},
		"no token":            {reportBody(from, to, "x", true, false), hdrWith(nil, HeaderCSRF), 403},
		"cross-origin":        {reportBody(from, to, "x", true, false), hdrWith(map[string]string{"Origin": "http://evil.com"}, "Sec-Fetch-Site"), 403},
		"form":                {reportBody(from, to, "x", true, false), hdrWith(map[string]string{"Content-Type": "text/plain"}), 415},
	} {
		if w := doBody(h, "POST", "/api/reports", c.body, c.hdr); w.Code != c.code {
			t.Errorf("%s: %d, want %d (%s)", name, w.Code, c.code, w.Body.String())
		}
	}
	if len(fr.builds) != 0 {
		t.Fatalf("refused requests built reports: %v", fr.builds)
	}

	w := doBody(h, "POST", "/api/reports", reportBody(from, now.Add(time.Hour), " Outage evidence ", true, true), goodHdr)
	var red reportJSON
	_ = json.Unmarshal(w.Body.Bytes(), &red)
	if w.Code != 201 || !red.Redacted || !red.Public || red.ID[0] != 'r' || !reportIDRE.MatchString(red.ID) || red.Title != "Outage evidence" || red.Bytes == 0 {
		t.Fatalf("create redacted = %d %s", w.Code, w.Body.String())
	}
	if b := fr.builds[0]; !b.Redact || b.To.After(now.Add(time.Second)) || !b.From.Equal(from.UTC().Truncate(time.Nanosecond)) {
		t.Errorf("build options = %+v", b)
	}
	w = doBody(h, "POST", "/api/reports", reportBody(from, to, "", false, false), goodHdr)
	var full reportJSON
	_ = json.Unmarshal(w.Body.Bytes(), &full)
	if w.Code != 201 || full.Redacted || full.Public || full.ID[0] != 'f' || full.Title != "Connection report" {
		t.Fatalf("create full = %d %s", w.Code, w.Body.String())
	}

	// List.
	var list []reportJSON
	_ = json.Unmarshal(do(h, "GET", "/api/reports", goodHdr).Body.Bytes(), &list)
	if len(list) != 2 {
		t.Errorf("list = %+v", list)
	}
	if w := do(h, "GET", "/api/reports?x=1", goodHdr); w.Code != 400 {
		t.Errorf("list with params = %d", w.Code)
	}

	// View and download: the report CSP replaces the dashboard's.
	w = do(h, "GET", "/api/reports/"+full.ID, goodHdr)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "full report") || w.Header().Get("Content-Security-Policy") != ReportCSP ||
		!strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") || w.Header().Get("Content-Disposition") != "" {
		t.Errorf("view = %d %v", w.Code, w.Header())
	}
	w = do(h, "GET", "/api/reports/"+full.ID+"?download=1", goodHdr)
	if cd := w.Header().Get("Content-Disposition"); w.Code != 200 || !strings.HasPrefix(cd, `attachment; filename="fyisp-report-`) || !strings.HasSuffix(cd, `.html"`) {
		t.Errorf("download = %d %q", w.Code, cd)
	}
	for p, code := range map[string]int{
		"/api/reports/" + full.ID + "?download=2": 400, "/api/reports/" + full.ID + "?x=1": 400,
		"/api/reports/r" + strings.Repeat("a", 25): 404, "/api/reports/../status": 404, "/api/reports/R" + strings.Repeat("a", 25): 404,
		"/api/reports/": 404,
	} {
		if w := do(h, "GET", p, goodHdr); w.Code != code {
			t.Errorf("GET %s = %d, want %d", p, w.Code, code)
		}
	}

	// Publishing: only redacted reports.
	if w := doBody(h, "PUT", "/api/reports/"+full.ID, `{"public":true}`, goodHdr); w.Code != 409 || fr.m[full.ID].Public {
		t.Errorf("publish full = %d", w.Code)
	}
	if w := doBody(h, "PUT", "/api/reports/"+red.ID, `{"public":false}`, hdrWith(nil, HeaderCSRF)); w.Code != 403 {
		t.Errorf("PUT without token = %d", w.Code)
	}
	w = doBody(h, "PUT", "/api/reports/"+red.ID, `{"public":false}`, goodHdr)
	var upd reportJSON
	_ = json.Unmarshal(w.Body.Bytes(), &upd)
	if w.Code != 200 || upd.Public || fr.m[red.ID].Public {
		t.Errorf("unpublish = %d %s", w.Code, w.Body.String())
	}
	for body, code := range map[string]int{`{}`: 400, `{"public":1}`: 400, `{"public":true,"x":1}`: 400} {
		if w := doBody(h, "PUT", "/api/reports/"+red.ID, body, goodHdr); w.Code != code {
			t.Errorf("PUT %s = %d, want %d", body, w.Code, code)
		}
	}
	if w := doBody(h, "PUT", "/api/reports/r"+strings.Repeat("b", 25), `{"public":true}`, goodHdr); w.Code != 404 {
		t.Errorf("PUT missing = %d", w.Code)
	}
	// Delete.
	if w := doBody(h, "DELETE", "/api/reports/"+full.ID, "", hdrWith(nil, "Origin", "Sec-Fetch-Site")); w.Code != 403 {
		t.Errorf("DELETE no origin = %d", w.Code)
	}
	if w := doBody(h, "DELETE", "/api/reports/"+full.ID, "", goodHdr); w.Code != 200 || len(fr.m) != 1 {
		t.Errorf("DELETE = %d", w.Code)
	}
	if w := doBody(h, "DELETE", "/api/reports/"+full.ID, "", goodHdr); w.Code != 404 {
		t.Errorf("DELETE again = %d", w.Code)
	}
	if w := doBody(h, "PATCH", "/api/reports/"+red.ID, "", goodHdr); w.Code != 405 {
		t.Errorf("PATCH = %d", w.Code)
	}
	// Disabled controls.
	hb := Local(d, LocalOptions{Addr: "0.0.0.0:3000", CSRFToken: "tok123"})
	if w := doBody(hb, "POST", "/api/reports", reportBody(from, to, "x", true, false), goodHdr); w.Code != 403 {
		t.Errorf("disabled POST = %d", w.Code)
	}
}

func TestPublicReportSnapshot(t *testing.T) {
	d, _, fr := stage3Fixture(t)
	idPub := "r" + strings.Repeat("a", 25)  // redacted, public: served
	idPriv := "r" + strings.Repeat("b", 25) // redacted, not public
	idFull := "f" + strings.Repeat("c", 25) // full details, flag forced public in the store
	idOdd := "x" + strings.Repeat("d", 25)  // not an ID we issue
	fr.put(idPub, true, "<!doctype html><style>p{color:red}</style><p>snapshot</p>")
	fr.put(idPriv, false, "<p>private snapshot</p>")
	fr.put(idFull, true, "<p>full 192.168.1.1</p>")
	fr.put(idOdd, true, "<p>odd</p>")
	h := Public(d, testSecret)
	base := "/s/" + testSecret + "/"

	w := do(h, "GET", base+"r/"+idPub, pubHdr(1))
	csp := w.Header().Get("Content-Security-Policy")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "snapshot") || csp != ReportCSP {
		t.Fatalf("public snapshot = %d %q %s", w.Code, csp, w.Body.String())
	}
	if !strings.HasPrefix(csp, "default-src 'none'; style-src 'unsafe-inline'; img-src data:;") || strings.Contains(csp, "script") || strings.Contains(csp, "'self'") {
		t.Errorf("snapshot CSP not strict: %q", csp)
	}
	if w.Header().Get("X-Robots-Tag") == "" || w.Header().Get("X-Content-Type-Options") != "nosniff" || w.Header().Get("Cache-Control") != "private, no-store" {
		t.Errorf("snapshot headers = %v", w.Header())
	}
	if w := do(h, "HEAD", base+"r/"+idPub, pubHdr(2)); w.Code != 200 || w.Body.Len() != 0 {
		t.Errorf("HEAD = %d", w.Code)
	}
	for i, c := range []struct {
		m, p string
		code int
	}{
		{"GET", "r/" + idPriv, 404}, {"GET", "r/" + idFull, 404}, {"GET", "r/" + idOdd, 404},
		{"GET", "r/r" + strings.Repeat("z", 25), 404}, {"GET", "r/", 404}, {"GET", "r/" + idPub + "/x", 404},
		{"GET", "r/" + idPub + "?download=1", 400}, {"POST", "r/" + idPub, 405}, {"DELETE", "r/" + idPub, 405},
		{"GET", "r/../api/status", 404},
	} {
		w := do(h, c.m, base+c.p, pubHdr(10+i))
		if w.Code != c.code {
			t.Errorf("public %s %s = %d, want %d", c.m, c.p, w.Code, c.code)
		}
		if strings.Contains(w.Body.String(), "snapshot") || strings.Contains(w.Body.String(), "192.168") {
			t.Errorf("public %s %s leaks a report: %s", c.m, c.p, w.Body.String())
		}
	}
	// Wrong secret.
	if w := do(h, "GET", "/s/wrong-secret-wrong-secret-xx/r/"+idPub, pubHdr(40)); w.Code != 404 {
		t.Errorf("wrong secret = %d", w.Code)
	}
	// Local listing never shows a full report as published.
	hl := Local(d, LocalOptions{Addr: "127.0.0.1:3000", CSRFToken: "tok123"})
	var list []reportJSON
	_ = json.Unmarshal(do(hl, "GET", "/api/reports", goodHdr).Body.Bytes(), &list)
	for _, r := range list {
		if r.ID == idFull && (r.Public || r.Redacted) {
			t.Errorf("full report listed as %+v", r)
		}
	}
	// The dashboard's other routes keep the dashboard CSP.
	if csp := do(h, "GET", base, pubHdr(41)).Header().Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self'") {
		t.Errorf("index CSP = %q", csp)
	}
}

// ---------- baselines ----------

func TestBaselines(t *testing.T) {
	d, _, _ := stage3Fixture(t)
	for _, tc := range []struct {
		name, prefix string
		h            http.Handler
	}{
		{"local", "/", Local(d, LocalOptions{Addr: "127.0.0.1:3000"})},
		{"public", "/s/" + testSecret + "/", Public(d, testSecret)},
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
	d, _, _ := stage3Fixture(t)
	for _, tc := range []struct {
		name, path string
		h          http.Handler
	}{
		{"local", "/api/verdict", Local(d, LocalOptions{Addr: "127.0.0.1:3000"})},
		{"public", "/s/" + testSecret + "/api/verdict", Public(d, testSecret)},
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

// TestStage3PublicLeaks runs the public leak check over every stage 3
// route with private notes and full reports planted.
func TestStage3PublicLeaks(t *testing.T) {
	d, fn, fr := stage3Fixture(t)
	now := time.Now()
	ctx := context.Background()
	for i, txt := range []string{"gateway 192.168.1.1 down", "NAS 10.1.2.3 at /home/venkat", "see alpha.example.com:8443/secret-path", "localhost trycloudflare"} {
		_ = fn.AddAnnotation(ctx, &model.Annotation{At: now.Add(-time.Duration(i+1) * time.Minute), Text: txt})
		_ = fn.AddAnnotation(ctx, &model.Annotation{At: now.Add(-time.Duration(i+1) * time.Hour), End: now.Add(-time.Duration(i) * time.Hour), Text: txt})
	}
	_ = fn.AddAnnotation(ctx, &model.Annotation{At: now.Add(-time.Minute), Text: "Zoom call dropped", Public: true})
	fr.put("f"+strings.Repeat("a", 25), true, "full 192.168.1.1 /home/venkat")
	fn.liar = true
	h := Public(d, testSecret)
	base := "/s/" + testSecret + "/"
	for i, p := range []string{"api/annotations", "api/annotations?from=now-90d", "api/annotations?from=now-6h&to=now-1h",
		"api/baselines?group=lan", "api/baselines?group=common&at=now-1h", "api/verdict", "api/profile",
		"api/overview", "api/overview?kind=tcp&from=now-6h",
		"r/f" + strings.Repeat("a", 25), "r/r" + strings.Repeat("a", 25)} {
		w := do(h, "GET", base+p, pubHdr(100+i))
		body := w.Body.String()
		for k, v := range w.Header() {
			body += "\n" + k + ": " + strings.Join(v, ",")
		}
		if m := leakRE.FindString(body); m != "" {
			t.Errorf("public %q leaks %q", p, m)
		}
		if strings.Contains(body, "gateway 192") || strings.Contains(body, "NAS ") {
			t.Errorf("public %q serves a private note", p)
		}
	}
}
