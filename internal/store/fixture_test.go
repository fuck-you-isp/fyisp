package store

import (
	"database/sql"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

// fixtureSamples are 3 series: two closed hours and part of a third.
func fixtureSamples() []model.Sample {
	var out []model.Sample
	for i, k := range []model.SeriesKey{{Target: "github", Kind: model.KindHTTPS}, {Target: "github", Kind: model.KindTCP}, {Target: "github", Kind: model.KindICMP}} {
		iv := 15 * time.Second
		if k.Kind == model.KindICMP {
			iv = 5 * time.Second
		}
		n := 0
		for tt := t0; tt.Before(t0.Add(2*time.Hour + 20*time.Minute)); tt = tt.Add(iv) {
			s := model.Sample{Key: k, Slot: tt, RTT: time.Duration(1000+n*37+i*100) * 10 * time.Microsecond}
			if n%50 == 7 {
				s = model.Sample{Key: k, Slot: tt, Lost: true, Reason: model.Reason(1 + n%15)}
			}
			if n%200 == 13 {
				s = model.Sample{Key: k, Slot: tt, Lost: true, Reason: model.ReasonGap}
			}
			out = append(out, s)
			n++
		}
	}
	return out
}

// fixtureIncidents are the fixture's incidents.
func fixtureIncidents() []model.Incident {
	return []model.Incident{
		{Start: t0.Add(10 * time.Minute), End: t0.Add(25 * time.Minute), Kind: model.VerdictISP, Summary: "closed", Targets: []string{"github"}, PeakLoss: 0.5},
		{Start: t0.Add(2 * time.Hour), Kind: model.VerdictUpstream, Summary: "ongoing", PeakLoss: 0.25},
	}
}

// fixtureTrace is the fixture's trace data: one route, one change and one
// hop address.
func fixtureTrace() (model.Route, model.RouteChange, model.HopInfo) {
	a, b, c := netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("198.51.100.1"), netip.MustParseAddr("203.0.113.9")
	r := model.Route{Target: "github", Hops: []netip.Addr{a, c}, Since: t0.Add(time.Hour)}
	ch := model.RouteChange{Target: "github", At: t0.Add(time.Hour), From: []netip.Addr{a, b}, To: []netip.Addr{a, c}, FirstDiff: 2}
	h := model.HopInfo{IP: c, RDNS: "edge.example.net", ASN: 64500, Owner: "EXAMPLE-AS", FirstSeen: t0, LastSeen: t0.Add(2 * time.Hour)}
	return r, ch, h
}

// fixtureDir writes fixtureSamples, fixtureIncidents and fixtureTrace into
// a new store and returns its directory, closed, as fyisp leaves it.
func fixtureDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	s := openT(t, dir, Options{Now: func() time.Time { return t0.Add(2*time.Hour + 20*time.Minute) }})
	for _, x := range fixtureSamples() {
		s.Observe(x)
	}
	for _, in := range fixtureIncidents() {
		if err := s.SaveIncident(ctx, &in); err != nil {
			t.Fatal(err)
		}
	}
	r, ch, h := fixtureTrace()
	s.ObserveRoute(r)
	s.ObserveRouteChange(ch)
	s.ObserveHop(h)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	return dir
}

func checkFixture(t *testing.T, s *SQLite) {
	t.Helper()
	f := NewFake()
	for _, x := range fixtureSamples() {
		f.Observe(x)
	}
	keys := []model.SeriesKey{{Target: "github", Kind: model.KindHTTPS}, {Target: "github", Kind: model.KindTCP}, {Target: "github", Kind: model.KindICMP}}
	end := t0.Add(3 * time.Hour)
	equalRaw(t, "fixture", rawAll(t, s, keys, t0, end), rawAll(t, f, keys, t0, end))
	q := PanelQuery{Keys: keys, From: t0, To: end, MaxPoints: 20}
	equalPanel(t, "fixture", panel(t, s, q), panel(t, f, q), 1e-6)
}

func setUserVersion(t *testing.T, path string, v int) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(fmt.Sprintf(`PRAGMA user_version=%d`, v)); err != nil {
		t.Fatal(err)
	}
}

// TestNewerSchema: a file from a newer fyisp is refused, never replaced.
func TestNewerSchema(t *testing.T) {
	dir := fixtureDir(t)
	setUserVersion(t, filepath.Join(dir, DBName), 99)
	if _, err := Open(dir, Options{}); !errors.Is(err, ErrNewerSchema) {
		t.Fatalf("Open: %v", err)
	}
	if _, err := OpenReadOnly(dir); !errors.Is(err, ErrNewerSchema) {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	// The file is left as it was.
	if n := countRows(t, filepath.Join(dir, DBName), "samples"); n == 0 {
		t.Error("a newer database was emptied")
	}
	// The failed Open released the lock.
	if lk, err := acquireLock(filepath.Join(dir, LockName)); err != nil {
		t.Fatal(err)
	} else {
		lk.release()
	}
}

func TestNotDatabase(t *testing.T) {
	dir := t.TempDir()
	db, _ := sql.Open("sqlite", filepath.Join(dir, DBName))
	db.Exec(`CREATE TABLE other(x)`)
	db.Close()
	if _, err := Open(dir, Options{}); !errors.Is(err, ErrNotDatabase) {
		t.Fatalf("Open: %v", err)
	}
	os.WriteFile(filepath.Join(dir, DBName), []byte("hello, this is not sqlite at all, not even close......................................................"), 0o600)
	if _, err := Open(dir, Options{}); err == nil {
		t.Fatal("opened garbage")
	}
}
