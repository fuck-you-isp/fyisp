package store

import (
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

// fixtureSamples is the content of testdata/v1.db: 3 series, two closed
// hours and part of a third.
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

// TestWriteFixture regenerates testdata/v1.db (FYISP_WRITE_FIXTURE=dir).
// Never regenerate a released fixture: it pins the v1 on-disk format.
func TestWriteFixture(t *testing.T) {
	dir := os.Getenv("FYISP_WRITE_FIXTURE")
	if dir == "" {
		t.Skip("set FYISP_WRITE_FIXTURE to regenerate testdata/v1.db")
	}
	s := openT(t, dir, Options{Version: "v0.1.0-fixture", Now: func() time.Time { return t0.Add(2*time.Hour + 20*time.Minute) }})
	for _, x := range fixtureSamples() {
		s.Observe(x)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(dir, LockName))
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		t.Fatal(err)
	}
	out.Close()
}

func fixtureDir(t *testing.T) string {
	dir := t.TempDir()
	copyFile(t, filepath.Join("testdata", "v1.db"), filepath.Join(dir, DBName))
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

func TestFixtureV1(t *testing.T) {
	dir := fixtureDir(t)
	s := openT(t, dir, Options{Now: func() time.Time { return t0.Add(2*time.Hour + 30*time.Minute) }})
	defer s.Close()
	checkFixture(t, s)
	var v string
	s.db.r.QueryRow(`SELECT value FROM meta WHERE key='created_by_version'`).Scan(&v)
	if v != "v0.1.0-fixture" {
		t.Fatalf("meta %q", v)
	}
	// The open hour continues.
	k := model.SeriesKey{Target: "github", Kind: model.KindTCP}
	s.Observe(model.Sample{Key: k, Slot: t0.Add(2*time.Hour + 20*time.Minute), RTT: time.Millisecond})
	pts := rawAll(t, s, []model.SeriesKey{k}, t0.Add(2*time.Hour), t0.Add(3*time.Hour))
	if len(pts) != 81 || pts[80].RTTms != 1 {
		t.Fatalf("continued fixture hour: %d points", len(pts))
	}
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

func TestNewerSchema(t *testing.T) {
	dir := fixtureDir(t)
	setUserVersion(t, filepath.Join(dir, DBName), 99)
	if _, err := Open(dir, Options{}); !errors.Is(err, ErrNewerSchema) {
		t.Fatalf("Open: %v", err)
	}
	if _, err := OpenReadOnly(dir); !errors.Is(err, ErrNewerSchema) {
		t.Fatalf("OpenReadOnly: %v", err)
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

// TestMigration runs the real migrations plus a (test-only) extra one over
// the v1 fixture: a VACUUM INTO backup is written first and the data
// survives.
func TestMigration(t *testing.T) {
	saved := migrations
	migrations = append(migrations[:len(migrations):len(migrations)], `CREATE TABLE test_vx(x); INSERT INTO test_vx VALUES(42);`)
	defer func() { migrations = saved }()
	dir := fixtureDir(t)
	s := openT(t, dir, Options{Now: func() time.Time { return t0.Add(2*time.Hour + 30*time.Minute) }})
	defer s.Close()
	if v := pragmaInt(t, s.db.w, "user_version"); v != int64(len(migrations)) {
		t.Fatalf("user_version %d", v)
	}
	var x int
	if err := s.db.r.QueryRow(`SELECT x FROM test_vx`).Scan(&x); err != nil || x != 42 {
		t.Fatalf("migration not applied: %v", err)
	}
	checkFixture(t, s)
	bak := filepath.Join(dir, DBName+".bak-v1")
	fi, err := os.Stat(bak)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Fatalf("backup mode %v", fi.Mode())
	}
	b, err := sql.Open("sqlite", bak)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	var uv, n int
	b.QueryRow(`PRAGMA user_version`).Scan(&uv)
	b.QueryRow(`SELECT count(*) FROM samples`).Scan(&n)
	if uv != 1 || n == 0 {
		t.Fatalf("backup user_version %d samples %d", uv, n)
	}
}

// fixtureIncidents are the incidents of testdata/v2.db.
func fixtureIncidents() []model.Incident {
	return []model.Incident{
		{Start: t0.Add(10 * time.Minute), End: t0.Add(25 * time.Minute), Kind: model.VerdictISP, Summary: "closed", Targets: []string{"github"}, PeakLoss: 0.5},
		{Start: t0.Add(2 * time.Hour), Kind: model.VerdictUpstream, Summary: "ongoing", PeakLoss: 0.25},
	}
}

// TestWriteFixtureV2 regenerates testdata/v2.db (FYISP_WRITE_FIXTURE=dir):
// the v1 fixture's samples plus fixtureIncidents, written by a v2 build.
// Never regenerate a released fixture: it pins the v2 on-disk format (a
// newer build cannot write it, hence the skip).
func TestWriteFixtureV2(t *testing.T) {
	dir := os.Getenv("FYISP_WRITE_FIXTURE")
	if dir == "" || SchemaVersion() != 2 {
		t.Skip("set FYISP_WRITE_FIXTURE in a schema v2 build to regenerate testdata/v2.db")
	}
	s := openT(t, dir, Options{Version: "v0.2.0-fixture", Now: func() time.Time { return t0.Add(2*time.Hour + 20*time.Minute) }})
	for _, x := range fixtureSamples() {
		s.Observe(x)
	}
	for _, in := range fixtureIncidents() {
		if err := s.SaveIncident(ctx, &in); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(dir, LockName))
	os.RemoveAll(filepath.Join(dir, "tmp"))
}

// fixtureTrace is the trace data of testdata/v3.db (besides fixtureSamples
// and fixtureIncidents): one route, one change and one hop address.
func fixtureTrace() (model.Route, model.RouteChange, model.HopInfo) {
	a, b, c := netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("198.51.100.1"), netip.MustParseAddr("203.0.113.9")
	r := model.Route{Target: "github", Hops: []netip.Addr{a, c}, Since: t0.Add(time.Hour)}
	ch := model.RouteChange{Target: "github", At: t0.Add(time.Hour), From: []netip.Addr{a, b}, To: []netip.Addr{a, c}, FirstDiff: 2}
	h := model.HopInfo{IP: c, RDNS: "edge.example.net", ASN: 64500, Owner: "EXAMPLE-AS", FirstSeen: t0, LastSeen: t0.Add(2 * time.Hour)}
	return r, ch, h
}

// TestWriteFixtureV3 regenerates testdata/v3.db (FYISP_WRITE_FIXTURE=dir):
// fixtureSamples, fixtureIncidents and fixtureTrace written by a v3 build
// (summary_1h in day format v1: no hourly medians). Never regenerate a
// released fixture: it pins the v3 on-disk format.
func TestWriteFixtureV3(t *testing.T) {
	dir := os.Getenv("FYISP_WRITE_FIXTURE")
	if dir == "" || SchemaVersion() != 3 {
		t.Skip("set FYISP_WRITE_FIXTURE in a schema v3 build to regenerate testdata/v3.db")
	}
	s := openT(t, dir, Options{Version: "v0.2.0-fixture-v3", Now: func() time.Time { return t0.Add(2*time.Hour + 20*time.Minute) }})
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
	os.Remove(filepath.Join(dir, LockName))
	os.RemoveAll(filepath.Join(dir, "tmp"))
}
