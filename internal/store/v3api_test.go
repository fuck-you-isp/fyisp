package store

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/store/blob"
)

type v3Store interface {
	AnnotationStore
	ReportStore
	BaselineReader
}

// v3Impls returns a fresh *SQLite and *Fake using the clock *now.
func v3Impls(t *testing.T, now *time.Time) map[string]v3Store {
	clock := func() time.Time { return *now }
	s := openT(t, t.TempDir(), Options{Now: clock})
	t.Cleanup(func() { s.Close() })
	f := NewFake()
	f.Now = clock
	return map[string]v3Store{"sqlite": s, "fake": f}
}

func checkV4Schema(t *testing.T, s *SQLite) {
	t.Helper()
	if SchemaVersion() < 4 {
		t.Fatalf("SchemaVersion %d", SchemaVersion())
	}
	for _, tbl := range []string{"annotations", "reports"} {
		var n int
		if err := s.db.r.QueryRow(`SELECT count(*) FROM ` + tbl).Scan(&n); err != nil {
			t.Fatalf("%s: %v", tbl, err)
		}
	}
	var idx int
	s.db.r.QueryRow(`SELECT count(*) FROM sqlite_schema WHERE type = 'index' AND name IN ('annotations_at', 'reports_created')`).Scan(&idx)
	if idx != 2 {
		t.Fatal("v4 indexes missing")
	}
}

func fixtureDirV3(t *testing.T) string {
	dir := t.TempDir()
	copyFile(t, filepath.Join("testdata", "v3.db"), filepath.Join(dir, DBName))
	return dir
}

// TestFixtureV3ToV4 upgrades the v3 fixture: samples, incidents and trace
// data survive, a v3 backup is written, and its v1-format summaries (no
// medians) give baselines from the hourly means.
func TestFixtureV3ToV4(t *testing.T) {
	dir := fixtureDirV3(t)
	now := t0.Add(2*time.Hour + 30*time.Minute)
	s := openT(t, dir, Options{Now: func() time.Time { return now }})
	defer s.Close()
	checkV3Schema(t, s, 3)
	checkFixture(t, s)
	if got, err := s.Incidents(ctx, t0, t0.Add(3*time.Hour)); err != nil || len(got) != len(fixtureIncidents()) {
		t.Fatalf("incidents %+v %v", got, err)
	}
	r, ch, h := fixtureTrace()
	if got, ok := s.Route(ctx, "github"); !ok || !slices.Equal(got.Hops, r.Hops) {
		t.Fatalf("route %+v", got)
	}
	if got, err := s.RouteChanges(ctx, t0, now); err != nil || len(got) != 1 || got[0].FirstDiff != ch.FirstDiff {
		t.Fatalf("route changes %+v %v", got, err)
	}
	if got, ok, err := s.HopInfo(ctx, h.IP); err != nil || !ok || got.Owner != h.Owner || got.ASN != h.ASN {
		t.Fatalf("hop info %+v %v %v", got, ok, err)
	}

	// Stored summaries are day format v1: no medians.
	for hk, u := range summaries(t, s) {
		if u.n > 0 && u.p50 != -1 {
			t.Fatalf("v1 summary %v has p50 %d", hk, u.p50)
		}
	}
	// Baselines over hours 0 and 1 use each hour's mean as its median.
	keys := []model.SeriesKey{{Target: "github", Kind: model.KindHTTPS}, {Target: "github", Kind: model.KindTCP}, {Target: "github", Kind: model.KindICMP}}
	got, err := s.Baselines(ctx, keys, now, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range keys {
		var p50s, p95s []weightedValue
		var n, lost int64
		for hour := range 2 {
			var slots []blob.Slot
			for _, x := range fixtureSamples() {
				if x.Key == k && floorHour(x.Slot.UnixMilli()) == t0.Unix()/3600+int64(hour) {
					slots = append(slots, toSlot(x))
				}
			}
			u := summarize(slots)
			n, lost = n+u.n, lost+u.lost
			p50s = append(p50s, weightedValue{unitMs((u.sum + u.n/2) / u.n), u.n})
			p95s = append(p95s, weightedValue{unitMs(u.p95), u.n})
		}
		b, ok := got[k]
		want := model.Baseline{Key: k, MedianMs: weightedQuantile(p50s, n, 50), P95Ms: weightedQuantile(p95s, n, 95),
			Loss: float64(lost) / float64(n+lost), Samples: n, From: t0, To: t0.Add(2 * time.Hour)}
		if !ok || b != want {
			t.Fatalf("%v: baseline\n%+v, want\n%+v", k, b, want)
		}
	}

	// A new closed hour is written in format v2, with its median; the v1
	// day row it joins keeps the old hours without one.
	k := keys[0]
	for i := range 240 {
		s.Observe(model.Sample{Key: k, Slot: t0.Add(3*time.Hour + time.Duration(i)*15*time.Second), RTT: time.Duration(10+i%3) * time.Millisecond})
	}
	now = t0.Add(4*time.Hour + 5*time.Minute)
	if err := s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	sums := summaries(t, s)
	if u := sums[[2]int64{t0.Unix()/3600 + 3, s.ids[k]}]; u.n != 240 || u.p50 != 1100 {
		t.Fatalf("new hour summary %+v", u)
	}
	if u := sums[[2]int64{t0.Unix() / 3600, s.ids[k]}]; u.n == 0 || u.p50 != -1 {
		t.Fatalf("old hour summary %+v", u)
	}
}

func TestSummaryV1Decode(t *testing.T) {
	// A day row as fyisp v0.2 wrote it (format v1): hour 5, n=3, lost=0,
	// min 100, max-min 50, p95-min 40, sum 390.
	v1 := []byte{summaryV1, 5, 3, 0, 100, 50, 40, 0x86, 0x03}
	hs, err := decodeDay(nil, 10, v1)
	if err != nil || len(hs) != 1 {
		t.Fatalf("%v %+v", err, hs)
	}
	u := hs[0].s
	if u.p50 != -1 || u.median() != 130 || u.min != 100 || u.max != 150 || u.p95 != 140 || u.sum != 390 {
		t.Fatalf("%+v median %d", u, u.median())
	}
	// Re-encoded as v2 the median stays unknown.
	again, err := decodeDay(nil, 10, appendDay(nil, 10, hs))
	if err != nil || !slices.Equal(again, hs) {
		t.Fatalf("v1 -> v2: %v %+v", err, again)
	}
	// v2 with a median.
	hs[0].s.p50 = 120
	enc := appendDay(nil, 10, hs)
	if enc[0] != summaryV2 {
		t.Fatal("not v2")
	}
	again, err = decodeDay(nil, 10, enc)
	if err != nil || again[0].s.p50 != 120 || again[0].s.median() != 120 {
		t.Fatalf("v2: %v %+v", err, again)
	}
	// A median above max is corrupt.
	bad := slices.Clone(enc)
	bad[len(bad)-1] = 52 // p50-min+1 = 52 > max-min+1
	if _, err := decodeDay(nil, 10, bad); err == nil {
		t.Fatal("p50 > max accepted")
	}
	if s := summarize([]blob.Slot{blob.Val(5), blob.Val(1), blob.Val(9), blob.Val(3)}); s.p50 != 3 || s.p95 != 9 {
		t.Fatalf("summarize p50 %+v", s)
	}
	if s := summarize([]blob.Slot{blob.Lost(1)}); s.p50 != -1 {
		t.Fatalf("summarize all lost %+v", s)
	}
}

func TestAnnotations(t *testing.T) {
	now := t0.Add(10 * time.Hour)
	for name, st := range v3Impls(t, &now) {
		t.Run(name, func(t *testing.T) {
			bad := []model.Annotation{
				{At: t0, Text: ""},
				{At: t0, Text: " \n\t "},
				{At: t0, Text: strings.Repeat("x", MaxAnnotationText+1)},
				{At: t0, Text: "bad \xff utf8"},
				{Text: "no time"},
				{At: t0, End: t0.Add(-time.Second), Text: "ends before"},
			}
			for _, a := range bad {
				if err := st.AddAnnotation(ctx, &a); !errors.Is(err, ErrInvalid) {
					t.Fatalf("Add %q: %v", a.Text, err)
				}
			}
			a1 := model.Annotation{At: t0.Add(time.Hour), End: t0.Add(2 * time.Hour), Text: "  outage  ", Public: true}
			a2 := model.Annotation{At: t0, Text: strings.Repeat("é", MaxAnnotationText)} // 500 characters, 1000 bytes
			a3 := model.Annotation{At: t0.Add(3 * time.Hour), End: t0.Add(3 * time.Hour), Text: "ticket"}
			for _, a := range []*model.Annotation{&a1, &a2, &a3} {
				if err := st.AddAnnotation(ctx, a); err != nil {
					t.Fatal(err)
				}
				if a.ID == 0 || !a.Created.Equal(now) || !a.Updated.Equal(now) {
					t.Fatalf("Add set %+v", a)
				}
			}
			if a1.Text != "outage" {
				t.Fatalf("not trimmed: %q", a1.Text)
			}
			all, err := st.Annotations(ctx, t0, t0.Add(24*time.Hour), false)
			if err != nil || len(all) != 3 || all[0].ID != a2.ID || all[1] != a1 || all[2].ID != a3.ID {
				t.Fatalf("all %+v %v", all, err)
			}
			pub, _ := st.Annotations(ctx, t0, t0.Add(24*time.Hour), true)
			if len(pub) != 1 || pub[0].ID != a1.ID {
				t.Fatalf("public %+v", pub)
			}
			// Overlap: a range inside a1 finds it; a point note only at its time.
			for _, c := range []struct {
				from, to time.Time
				want     []int64
			}{
				{t0.Add(90 * time.Minute), t0.Add(100 * time.Minute), []int64{a1.ID}},
				{t0.Add(2 * time.Hour), t0.Add(3 * time.Hour), []int64{a1.ID, a3.ID}},
				{t0.Add(time.Millisecond), t0.Add(59 * time.Minute), nil},
				{t0, t0, []int64{a2.ID}},
				{t0.Add(-time.Hour), t0.Add(-time.Millisecond), nil},
			} {
				got, err := st.Annotations(ctx, c.from, c.to, false)
				var ids []int64
				for _, a := range got {
					ids = append(ids, a.ID)
				}
				if err != nil || !slices.Equal(ids, c.want) {
					t.Fatalf("[%v, %v]: %v %v, want %v", c.from, c.to, ids, err, c.want)
				}
			}

			now = now.Add(time.Minute)
			up := a1
			up.Text, up.Public, up.End = "outage (ISP confirmed)", false, time.Time{}
			up.Created = time.Time{}
			if err := st.UpdateAnnotation(ctx, &up); err != nil {
				t.Fatal(err)
			}
			if !up.Created.Equal(a1.Created) || !up.Updated.Equal(now) {
				t.Fatalf("update stamps %+v", up)
			}
			got, _ := st.Annotations(ctx, a1.At, a1.At, false)
			if len(got) != 1 || got[0] != up {
				t.Fatalf("after update %+v, want %+v", got, up)
			}
			if pub, _ := st.Annotations(ctx, t0, t0.Add(24*time.Hour), true); len(pub) != 0 {
				t.Fatalf("public after update %+v", pub)
			}
			bad1 := up
			bad1.Text = ""
			if err := st.UpdateAnnotation(ctx, &bad1); !errors.Is(err, ErrInvalid) {
				t.Fatalf("invalid update: %v", err)
			}
			missing := model.Annotation{ID: 999, At: t0, Text: "x"}
			if err := st.UpdateAnnotation(ctx, &missing); !errors.Is(err, ErrNotFound) {
				t.Fatalf("update missing: %v", err)
			}
			if err := st.DeleteAnnotation(ctx, a2.ID); err != nil {
				t.Fatal(err)
			}
			if err := st.DeleteAnnotation(ctx, a2.ID); !errors.Is(err, ErrNotFound) {
				t.Fatalf("delete twice: %v", err)
			}
			if all, _ := st.Annotations(ctx, t0, t0.Add(24*time.Hour), false); len(all) != 2 {
				t.Fatalf("after delete %+v", all)
			}
		})
	}
}

func testHTML(n int) []byte {
	var b bytes.Buffer
	b.WriteString("<!doctype html><title>r</title>")
	for i := 0; b.Len() < n; i++ {
		fmt.Fprintf(&b, "<tr><td>github</td><td>%d.%02d ms</td></tr>\n", 10+i%7, i%100)
	}
	return b.Bytes()[:n]
}

func TestReports(t *testing.T) {
	now := t0.Add(48 * time.Hour)
	for name, st := range v3Impls(t, &now) {
		t.Run(name, func(t *testing.T) {
			html := testHTML(200 << 10)
			for _, c := range []struct {
				id   string
				html []byte
			}{
				{"short", html},
				{strings.Repeat("a", 65), html},
				{"has space x", html},
				{"../../etc/pw", html},
				{"abcdefgh", nil},
				{"abcdefgh", make([]byte, MaxReportBytes+1)},
			} {
				if err := st.SaveReport(ctx, model.ReportMeta{ID: c.id}, c.html); !errors.Is(err, ErrInvalid) {
					t.Fatalf("SaveReport %q (%d bytes): %v", c.id, len(c.html), err)
				}
			}
			if err := st.SaveReport(ctx, model.ReportMeta{ID: "rangeback", From: t0, To: t0.Add(-time.Hour)}, html); !errors.Is(err, ErrInvalid) {
				t.Fatalf("backwards range: %v", err)
			}
			m1 := model.ReportMeta{ID: "Rep_0001-a", Title: " Week 36 ", From: t0, To: t0.Add(24 * time.Hour), Created: t0.Add(25 * time.Hour), Bytes: 1}
			if err := st.SaveReport(ctx, m1, html); err != nil {
				t.Fatal(err)
			}
			m2 := model.ReportMeta{ID: strings.Repeat("Z", 64), Title: "big", Public: true}
			big := testHTML(MaxReportBytes)
			if err := st.SaveReport(ctx, m2, big); err != nil {
				t.Fatal(err)
			}
			list, err := st.Reports(ctx)
			if err != nil || len(list) != 2 || list[0].ID != m2.ID || list[1].ID != m1.ID {
				t.Fatalf("Reports %+v %v", list, err)
			}
			want1 := model.ReportMeta{ID: m1.ID, Title: "Week 36", From: t0, To: t0.Add(24 * time.Hour), Created: t0.Add(25 * time.Hour), Bytes: len(html)}
			if list[1] != want1 || !list[0].Created.Equal(now) || list[0].Bytes != MaxReportBytes || !list[0].Public {
				t.Fatalf("metas %+v", list)
			}
			got, gotHTML, err := st.Report(ctx, m1.ID)
			if err != nil || got != want1 || !bytes.Equal(gotHTML, html) {
				t.Fatalf("Report: %+v %v (html equal %v)", got, err, bytes.Equal(gotHTML, html))
			}
			if _, gotHTML, err := st.Report(ctx, m2.ID); err != nil || !bytes.Equal(gotHTML, big) {
				t.Fatalf("big report: %v", err)
			}
			for _, id := range []string{"missing1", "x", "a/b/c/d/e/f"} {
				if _, _, err := st.Report(ctx, id); !errors.Is(err, ErrNotFound) {
					t.Fatalf("Report(%q): %v", id, err)
				}
			}
			if err := st.SetReportPublic(ctx, m1.ID, true); err != nil {
				t.Fatal(err)
			}
			if got, _, _ := st.Report(ctx, m1.ID); !got.Public {
				t.Fatal("not public")
			}
			if err := st.SetReportPublic(ctx, "missing1", true); !errors.Is(err, ErrNotFound) {
				t.Fatalf("public missing: %v", err)
			}
			// Saving the same ID replaces it.
			m1.Title = "Week 36 v2"
			if err := st.SaveReport(ctx, m1, html[:1000]); err != nil {
				t.Fatal(err)
			}
			if got, h, _ := st.Report(ctx, m1.ID); got.Title != "Week 36 v2" || got.Public || len(h) != 1000 {
				t.Fatalf("replace: %+v %d", got, len(h))
			}
			if err := st.DeleteReport(ctx, m2.ID); err != nil {
				t.Fatal(err)
			}
			if err := st.DeleteReport(ctx, m2.ID); !errors.Is(err, ErrNotFound) {
				t.Fatalf("delete twice: %v", err)
			}
			if list, _ := st.Reports(ctx); len(list) != 1 {
				t.Fatalf("after delete %+v", list)
			}
		})
	}
}

// TestReportCompression: the stored blob is zstd and much smaller.
func TestReportCompression(t *testing.T) {
	s := openT(t, t.TempDir(), Options{Now: func() time.Time { return t0 }})
	defer s.Close()
	html := testHTML(1 << 20)
	if err := s.SaveReport(ctx, model.ReportMeta{ID: "compress1"}, html); err != nil {
		t.Fatal(err)
	}
	var stored []byte
	var size int
	if err := s.db.r.QueryRow(`SELECT html, bytes FROM reports WHERE id = 'compress1'`).Scan(&stored, &size); err != nil {
		t.Fatal(err)
	}
	if size != len(html) || len(stored)*4 > len(html) || !bytes.HasPrefix(stored, []byte{0x28, 0xb5, 0x2f, 0xfd}) {
		t.Fatalf("stored %d bytes (zstd magic %x) for %d (bytes column %d)", len(stored), stored[:4], len(html), size)
	}
	back, err := decompressReport(stored, size)
	if err != nil || !bytes.Equal(back, html) {
		t.Fatalf("round trip: %v", err)
	}
	// A corrupt blob is an error, not garbage.
	if _, err := s.db.w.Exec(`UPDATE reports SET html = x'28b52ffd00' WHERE id = 'compress1'`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Report(ctx, "compress1"); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("corrupt report: %v", err)
	}
}

// TestReportCap keeps the newest MaxReports (by Created).
func TestReportCap(t *testing.T) {
	now := t0
	for name, st := range v3Impls(t, &now) {
		t.Run(name, func(t *testing.T) {
			html := []byte("<p>r</p>")
			for i := range MaxReports + 5 {
				// Created out of order: minutes 7i mod 205 (a permutation of 0..204).
				m := model.ReportMeta{ID: fmt.Sprintf("report-%04d", i), Created: t0.Add(time.Duration(i*7%(MaxReports+5)) * time.Minute)}
				if err := st.SaveReport(ctx, m, html); err != nil {
					t.Fatal(err)
				}
			}
			list, err := st.Reports(ctx)
			if err != nil || len(list) != MaxReports {
				t.Fatalf("%d reports %v", len(list), err)
			}
			for i, m := range list {
				if want := t0.Add(time.Duration(MaxReports+4-i) * time.Minute); !m.Created.Equal(want) {
					t.Fatalf("list[%d] created %v, want %v", i, m.Created, want)
				}
			}
			// Minutes 0-4 went: reports 0, 88, 176, 59 and 147.
			for _, id := range []string{"report-0000", "report-0088", "report-0176", "report-0059", "report-0147"} {
				if _, _, err := st.Report(ctx, id); !errors.Is(err, ErrNotFound) {
					t.Fatalf("%s kept: %v", id, err)
				}
			}
			if _, _, err := st.Report(ctx, "report-0001"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestPruneV3: retention deletes old annotations (by end, else time) and
// never reports.
func TestPruneV3(t *testing.T) {
	now := t0.Add(100 * 24 * time.Hour)
	for name, st := range v3Impls(t, &now) {
		t.Run(name, func(t *testing.T) {
			before := t0.Add(10 * 24 * time.Hour)
			notes := []model.Annotation{
				{At: t0, Text: "old point"},
				{At: t0, End: before.Add(-2 * time.Hour), Text: "old range"},
				{At: t0, End: before.Add(time.Hour), Text: "long range ending after"},
				{At: before.Add(time.Hour), Text: "new point"},
			}
			for i := range notes {
				if err := st.AddAnnotation(ctx, &notes[i]); err != nil {
					t.Fatal(err)
				}
			}
			if err := st.SaveReport(ctx, model.ReportMeta{ID: "oldreport", From: t0, To: t0.Add(time.Hour), Created: t0}, []byte("x")); err != nil {
				t.Fatal(err)
			}
			if err := st.(Writer).Prune(ctx, before); err != nil {
				t.Fatal(err)
			}
			got, _ := st.Annotations(ctx, t0.Add(-time.Hour), now, false)
			var texts []string
			for _, a := range got {
				texts = append(texts, a.Text)
			}
			if !slices.Equal(texts, []string{"long range ending after", "new point"}) {
				t.Fatalf("after prune %q", texts)
			}
			if _, _, err := st.Report(ctx, "oldreport"); err != nil {
				t.Fatalf("report pruned: %v", err)
			}
		})
	}
}

// Baseline math, over synthetic hourly summaries written straight into
// summary_1h.

// synthHour is a synthetic hour: offset from the hour of `at` (-1 is the last
// closed hour), n successful samples with median p50 and p95 (ms), lost.
type synthHour struct {
	off      int
	n, lost  int64
	p50, p95 float64
	noP50    bool // stored without a median (format v1); mean = p50+1
}

func (h synthHour) summary() summary {
	u := summary{n: h.n, lost: h.lost, p50: -1}
	u.by[1] = h.lost
	if h.n > 0 {
		p50, p95 := int64(h.p50*100), int64(h.p95*100) // ms -> 10 µs
		u.min, u.max, u.p95 = p50/2, p95*2, p95
		mean := p50
		if h.noP50 {
			mean = p50 + 100
		} else {
			u.p50 = p50
		}
		u.sum = mean * h.n
	}
	return u
}

// putHours stores the synthetic hours of k relative to the hour of at.
func putHours(t *testing.T, s *SQLite, k model.SeriesKey, at time.Time, hours []synthHour) {
	t.Helper()
	// Register the series with one sample far outside every window.
	s.Observe(model.Sample{Key: k, Slot: at.Add(-300 * 24 * time.Hour), RTT: time.Millisecond})
	if err := s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	id := s.ids[k]
	base := at.Unix() / 3600
	days := map[int64][]hourSummary{}
	for _, h := range hours {
		hour := base + int64(h.off)
		days[floorDiv(hour, 24)] = mergeDay(days[floorDiv(hour, 24)], hourSummary{hour, h.summary()})
	}
	for day, list := range days {
		var n int
		s.db.w.QueryRow(`SELECT count(*) FROM summary_1h WHERE day = ? AND series = ?`, day, id).Scan(&n)
		if n != 0 {
			t.Fatalf("day %d already stored", day)
		}
		data := appendDay(nil, day, list)
		if slices.ContainsFunc(list, func(x hourSummary) bool { return x.s.n > 0 && x.s.p50 < 0 }) {
			data = appendDayV1(day, list) // as fyisp v0.2 wrote it
		}
		if _, err := s.db.w.Exec(`INSERT INTO summary_1h(day, series, data) VALUES(?, ?, ?)`, day, id, data); err != nil {
			t.Fatal(err)
		}
	}
}

// appendDayV1 encodes hours without medians in day format v1 (fyisp
// v0.1/v0.2): format v2 minus the version and each hour's p50 field.
func appendDayV1(day int64, list []hourSummary) []byte {
	out := []byte{summaryV1}
	for _, h := range list {
		h.s.p50 = -1
		one := appendDay(nil, day, []hourSummary{h})[1:]
		if h.s.n > 0 {
			one = one[:len(one)-1] // an unknown p50 is the single byte 0
		}
		out = append(out, one...)
	}
	return out
}

func TestBaselines(t *testing.T) {
	at := time.Date(2026, 9, 20, 14, 30, 0, 0, time.UTC) // hour of day 14
	to := at.Truncate(time.Hour)
	k := model.SeriesKey{Target: "github", Kind: model.KindHTTPS}
	sameHour := func(days ...int) []synthHour {
		var out []synthHour
		for _, d := range days {
			out = append(out, synthHour{off: -24 * d, n: 100, p50: float64(10 * d), p95: float64(20 * d)})
			out = append(out, synthHour{off: -24*d + 1, n: 100, p50: 500, p95: 900}) // hour 15: other hours of day
		}
		return out
	}
	cases := []struct {
		name      string
		hours     []synthHour
		window    time.Duration
		hourOfDay bool
		want      *model.Baseline // nil: no baseline
		span      [2]int          // offsets of the first and last contributing hour
	}{
		{name: "no data"},
		{name: "weighted medians", hours: []synthHour{
			{off: -1, n: 100, p50: 10, p95: 20},
			{off: -2, n: 300, lost: 100, p50: 30, p95: 50},
			{off: -3, n: 100, p50: 20, p95: 40},
		}, want: &model.Baseline{MedianMs: 30, P95Ms: 50, Loss: 100.0 / 600, Samples: 500}, span: [2]int{-3, -1}},
		{name: "median not the mean of medians", hours: []synthHour{
			{off: -1, n: 1000, p50: 10, p95: 12},
			{off: -2, n: 10, p50: 900, p95: 1000},
		}, want: &model.Baseline{MedianMs: 10, P95Ms: 12, Samples: 1010}, span: [2]int{-2, -1}},
		{name: "current hour excluded", hours: []synthHour{
			{off: 0, n: 100, p50: 99, p95: 99},
		}},
		{name: "window bounds", window: 3 * time.Hour, hours: []synthHour{
			{off: 0, n: 1000, p50: 99, p95: 99},  // current hour: out
			{off: -1, n: 100, p50: 10, p95: 20},  // in
			{off: -3, n: 100, p50: 12, p95: 22},  // first hour of the window: in
			{off: -4, n: 1000, p50: 99, p95: 99}, // before the window: out
		}, want: &model.Baseline{MedianMs: 10, P95Ms: 22, Samples: 200}, span: [2]int{-3, -1}},
		{name: "default window is 7 days", hours: []synthHour{
			{off: -7 * 24, n: 100, p50: 10, p95: 20},
			{off: -7*24 - 1, n: 1000, p50: 99, p95: 99},
		}, want: &model.Baseline{MedianMs: 10, P95Ms: 20, Samples: 100}, span: [2]int{-168, -168}},
		{name: "loss only", hours: []synthHour{
			{off: -1, lost: 40},
			{off: -2}, // no samples at all (not measured): no data
		}, want: &model.Baseline{Loss: 1}, span: [2]int{-1, -1}},
		{name: "only unmeasured hours", hours: []synthHour{{off: -1}, {off: -2}}},
		{name: "old summaries use the mean", hours: []synthHour{
			{off: -1, n: 100, p50: 10, p95: 20, noP50: true},
			{off: -2, n: 101, p50: 30, p95: 40, noP50: true},
		}, want: &model.Baseline{MedianMs: 31, P95Ms: 40, Samples: 201}, span: [2]int{-2, -1}},
		{name: "same hour", hourOfDay: true, hours: sameHour(1, 2, 3, 4, 5, 6, 7),
			want: &model.Baseline{MedianMs: 40, P95Ms: 140, Samples: 700}, span: [2]int{-168, -24}},
		{name: "same hour, 6 days", hourOfDay: true, hours: sameHour(1, 2, 3, 4, 5, 6),
			want: &model.Baseline{MedianMs: 30, P95Ms: 120, Samples: 600}, span: [2]int{-144, -24}},
		{name: "same hour, 5 days: too few", hourOfDay: true, hours: sameHour(1, 2, 3, 4, 5)},
		{name: "same hour, day 8 outside the window", hourOfDay: true, hours: sameHour(1, 2, 3, 4, 5, 8)},
		{name: "all hours", hours: sameHour(1, 2, 3, 4, 5, 6, 7),
			want: &model.Baseline{MedianMs: 70, P95Ms: 900, Samples: 1400}, span: [2]int{-168, -23}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := openT(t, t.TempDir(), Options{Now: func() time.Time { return at }})
			defer s.Close()
			putHours(t, s, k, at, c.hours)
			other := model.SeriesKey{Target: "never", Kind: model.KindTCP}
			got, err := s.Baselines(ctx, []model.SeriesKey{k, other, k}, at, c.window, c.hourOfDay)
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := got[other]; ok || len(got) > 1 {
				t.Fatalf("unexpected keys %+v", got)
			}
			b, ok := got[k]
			if c.want == nil {
				if ok {
					t.Fatalf("baseline %+v, want none", b)
				}
				return
			}
			w := *c.want
			w.Key, w.HourOfDay = k, c.hourOfDay
			w.From, w.To = to.Add(time.Duration(c.span[0])*time.Hour), to.Add(time.Duration(c.span[1]+1)*time.Hour)
			if !ok || math.Abs(b.Loss-w.Loss) > 1e-12 {
				t.Fatalf("baseline %+v (%v), want %+v", b, ok, w)
			}
			b.Loss = w.Loss
			if b != w {
				t.Fatalf("baseline\n%+v, want\n%+v", b, w)
			}
		})
	}
}

// TestBaselinesFake: the fake and the real store agree on the same
// samples, including closed hours still in memory.
func TestBaselinesFake(t *testing.T) {
	at := t0.Add(9*24*time.Hour + 14*time.Hour + 20*time.Minute)
	now := at
	s := openT(t, t.TempDir(), Options{Now: func() time.Time { return now }})
	defer s.Close()
	f := NewFake()
	keys := []model.SeriesKey{{Target: "a", Kind: model.KindTCP}, {Target: "b", Kind: model.KindTCP}}
	i := 0
	for ts := at.Add(-9 * 24 * time.Hour); ts.Before(at); ts = ts.Add(15 * time.Second) {
		for j, k := range keys {
			x := model.Sample{Key: k, Slot: ts, RTT: time.Duration(5+(i*7+j*3)%40+ts.Hour()) * time.Millisecond}
			switch {
			case i%23 == 3:
				x = model.Sample{Key: k, Slot: ts, Lost: true, Reason: model.ReasonTimeout}
			case i%101 == 5:
				x = model.Sample{Key: k, Slot: ts, Lost: true, Reason: model.ReasonGap}
			}
			s.Observe(x)
			f.Observe(x)
		}
		i++
		if ts.Minute() == 0 && ts.Second() == 0 && ts.Before(at.Add(-3*time.Hour)) {
			now = ts
			if err := s.Flush(ctx); err != nil {
				t.Fatal(err)
			}
		}
	}
	now = at
	for _, hod := range []bool{false, true} {
		for _, w := range []time.Duration{0, 5 * time.Hour, 36*time.Hour + 30*time.Minute} {
			got, err := s.Baselines(ctx, keys, at, w, hod)
			if err != nil {
				t.Fatal(err)
			}
			want, _ := f.Baselines(ctx, keys, at, w, hod)
			if len(want) != 2 && (!hod || w == 0) {
				t.Fatalf("fake: %+v", want)
			}
			if !mapsEqual(got, want) {
				t.Fatalf("hourOfDay %v window %v:\n%+v\n%+v", hod, w, got, want)
			}
		}
	}
}
