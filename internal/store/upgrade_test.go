package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

// lastRelease is the release whose database testdata/<lastRelease>.db is:
// written by that release's own store (the dbfixture Dockerfile stage).
// After tagging a release, set it and the stage's RELEASE to the new tag
// and regenerate the file.
const lastRelease = "v0.13.0"

// The generator's clock and values (testdata/genfixture/main.go): keep
// them in step.
var (
	relT0  = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	relEnd = relT0.Add(3*time.Hour + 20*time.Minute)
)

func relInterval(k model.SeriesKey) time.Duration {
	if k.Kind == model.KindHTTPS || k.Kind == model.KindTCP {
		return 15 * time.Second
	}
	return 5 * time.Second
}

var relKeys = []model.SeriesKey{
	{Target: "github", Kind: model.KindHTTPS}, {Target: "github", Kind: model.KindTCP}, {Target: "github", Kind: model.KindICMP},
	{Target: "github", Kind: model.KindTrace, Hop: 1}, {Target: "github", Kind: model.KindTrace, Hop: 2}, {Target: "github", Kind: model.KindTrace, Hop: 3},
	{Target: "Meet-STUN", Kind: model.KindUDP}, {Target: "Meet-STUN", Kind: model.KindUDPJitter}, {Target: "Meet-STUN", Kind: model.KindUDPLoss},
}

func relSamples() []model.Sample {
	var out []model.Sample
	for i, k := range relKeys {
		n := 0
		for ts := relT0; ts.Before(relEnd); ts = ts.Add(relInterval(k)) {
			x := model.Sample{Key: k, Slot: ts, RTT: time.Duration(1000+n%100*37+i*100) * 10 * time.Microsecond}
			switch {
			case k.Kind == model.KindUDPLoss:
				x.RTT = time.Duration(n%5*20) * time.Millisecond
			case n%200 == 13:
				x = model.Sample{Key: k, Slot: ts, Lost: true, Reason: model.ReasonGap}
			case n%50 == 7:
				x = model.Sample{Key: k, Slot: ts, Lost: true, Reason: model.Reason(1 + n%15)}
			}
			out = append(out, x)
			n++
		}
	}
	return out
}

func relIncidents() []model.Incident {
	return []model.Incident{
		{Start: relT0.Add(10 * time.Minute), End: relT0.Add(25 * time.Minute), Kind: model.VerdictISP, Summary: "closed", Targets: []string{"github"}, PeakLoss: 0.5},
		{Start: relT0.Add(3 * time.Hour), Kind: model.VerdictUpstream, Summary: "ongoing", PeakLoss: 0.25},
	}
}

func relTrace() (model.Route, model.RouteChange, model.HopInfo) {
	a, b, c := netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("198.51.100.1"), netip.MustParseAddr("203.0.113.9")
	r := model.Route{Target: "github", Hops: []netip.Addr{a, c, b}, Since: relT0.Add(time.Hour)}
	ch := model.RouteChange{Target: "github", At: relT0.Add(time.Hour), From: []netip.Addr{a, b, b}, To: []netip.Addr{a, c, b}, FirstDiff: 2}
	h := model.HopInfo{IP: c, RDNS: "edge.example.net", ASN: 64500, Owner: "EXAMPLE-AS", FirstSeen: relT0, LastSeen: relT0.Add(3 * time.Hour)}
	return r, ch, h
}

func relSpeedTest() model.SpeedTest {
	down, up, sc, re := 4.12e8, 3.5e7, 0.93, int64(112)
	return model.SpeedTest{Start: relT0.Add(2 * time.Hour), End: relT0.Add(2*time.Hour + 33*time.Second), Trigger: "schedule", Outcome: model.SpeedOK,
		Region: "uk", Server: "Clouvider-London", Port: 5203, Attempts: 2, DownBps: &down, UpBps: &up, DownRetrans: &re,
		Latency: &model.SpeedLatency{Edge: model.LoadPhases{Idle: &model.LoadStats{N: 40, P50Ms: 8.2, P95Ms: 9.9},
			Down: &model.LoadStats{N: 58, Lost: 3, Loss: 0.052, P50Ms: 140, P95Ms: 1000}}},
		Conn: model.ConnWired, Handoff: &model.Handoff{ASN: 3356, Name: "LEVEL3", ISPASN: 3320, At: relT0.Add(time.Hour)},
		FallbackLevel: "none", DownSecs: 0.8, UpSecs: 3.1, Bytes: 140 << 20, ScoreDown: &sc, Extra: `{"ramp_ms":[400,300]}`}
}

// releaseDir copies the last release's database into a new data directory.
func releaseDir(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", lastRelease+".db"))
	if err != nil {
		t.Fatalf("the fixture is missing (docker buildx build --target dbfixture --output type=local,dest=. .): %v", err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, DBName), b, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func openRO(t *testing.T, path string) *sql.DB {
	t.Helper()
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", readOnlyURI(filepath.ToSlash(abs)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func queryStrings(t *testing.T, db *sql.DB, q string, args ...any) []string {
	t.Helper()
	rows, err := db.Query(q, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// tableCounts is the row count of every table of db.
func tableCounts(t *testing.T, db *sql.DB) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, name := range queryStrings(t, db, `SELECT name FROM sqlite_schema WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`) {
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM "` + name + `"`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		out[name] = n
	}
	return out
}

// schemaShape is every table and index of db, and every column of each
// table with its declared type: "table x", "index y on x", "column x.c" ->
// type.
func schemaShape(t *testing.T, db *sql.DB) map[string]string {
	t.Helper()
	out := map[string]string{}
	rows, err := db.Query(`SELECT type, name, tbl_name FROM sqlite_schema WHERE type IN ('table', 'index') AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var typ, name, tbl string
		if err := rows.Scan(&typ, &name, &tbl); err != nil {
			t.Fatal(err)
		}
		out[typ+" "+name] = tbl
		if typ == "table" {
			tables = append(tables, name)
		}
	}
	rows.Close()
	for _, tbl := range tables {
		rows, err := db.Query(`SELECT name, type FROM pragma_table_info(?)`, tbl)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var col, typ string
			if err := rows.Scan(&col, &typ); err != nil {
				t.Fatal(err)
			}
			out["column "+tbl+"."+col] = typ
		}
		rows.Close()
	}
	return out
}

// TestUpgradeFromLastRelease: the database the last release wrote opens
// with this build and keeps everything: no table loses or gains a row, the
// tables the upgrade added are empty, the schema is a new file's, the
// creation stamp is the release's, and the Reader API returns the values
// the generator wrote (raw and hourly tiers, incidents, the route, the
// route change, hop info, the speed test and a setting).
func TestUpgradeFromLastRelease(t *testing.T) {
	dir := releaseDir(t)
	path := filepath.Join(dir, DBName)
	before := tableCounts(t, openRO(t, path))
	for _, tbl := range []string{"series", "samples", "summary_1h", "incidents", "hop_info", "route_changes", "routes", "speedtests"} {
		if before[tbl] == 0 {
			t.Errorf("the fixture has no %s rows", tbl)
		}
	}

	log, logs := testLog()
	s := openT(t, dir, Options{Interval: relInterval, Now: func() time.Time { return relEnd }, Log: log})
	defer s.Close()
	if l := logs.String(); strings.Contains(l, "replaced") {
		t.Fatalf("the %s database was replaced:\n%s", lastRelease, l)
	}
	// Every row is kept; the tables the upgrade added are empty.
	after := tableCounts(t, s.db.r)
	for tbl, n := range after {
		if _, old := before[tbl]; !old && n == 0 {
			delete(after, tbl)
		}
	}
	if !maps.Equal(after, before) {
		t.Errorf("row counts\n%v\nwant\n%v", after, before)
	}
	if got := schemaObjects(t, s.db.r); !slices.Equal(got, wantSchema) {
		t.Errorf("schema objects after the upgrade\n%q\nwant\n%q", got, wantSchema)
	}
	if v := pragmaInt(t, s.db.w, "user_version"); v != int64(SchemaVersion()) {
		t.Errorf("user_version %d, want %d", v, SchemaVersion())
	}
	var by, at string
	s.db.r.QueryRow(`SELECT value FROM meta WHERE key = 'created_by_version'`).Scan(&by)
	s.db.r.QueryRow(`SELECT value FROM meta WHERE key = 'created_at'`).Scan(&at)
	if by != lastRelease || at != relEnd.Format(time.RFC3339) {
		t.Errorf("created_by_version %q, created_at %q: not the %s file", by, at, lastRelease)
	}

	f := NewFake()
	for _, x := range relSamples() {
		f.Observe(x)
	}
	equalRaw(t, "raw", rawAll(t, s, relKeys, relT0, relEnd), rawAll(t, f, relKeys, relT0, relEnd))
	q := PanelQuery{Keys: relKeys, From: relT0, To: relEnd, MaxPoints: 20}
	if p := panel(t, s, q); p.Tier != TierRaw {
		t.Errorf("tier %s, want %s", p.Tier, TierRaw)
	} else {
		equalPanel(t, "raw tier", p, panel(t, f, q), 1e-6)
	}
	q = PanelQuery{Keys: relKeys, From: relT0.Add(-46 * time.Hour), To: relT0.Add(3*time.Hour - time.Millisecond), MaxPoints: 60}
	if p := panel(t, s, q); p.Tier != TierHourly {
		t.Errorf("tier %s, want %s", p.Tier, TierHourly)
	} else {
		equalPanel(t, "hourly tier", p, panel(t, f, q), 1e-4)
	}

	ins, err := s.Incidents(ctx, relT0, relEnd)
	if err != nil {
		t.Fatal(err)
	}
	for i := range ins {
		ins[i].ID = 0
	}
	slices.Reverse(ins) // newest first
	if want := relIncidents(); !reflect.DeepEqual(ins, want) {
		t.Errorf("incidents\n%+v\nwant\n%+v", ins, want)
	}
	r, ch, h := relTrace()
	if got, ok := s.Route(ctx, r.Target); !ok || !reflect.DeepEqual(got, r) {
		t.Errorf("route %+v %v, want %+v", got, ok, r)
	}
	chs, err := s.RouteChanges(ctx, relT0, relEnd)
	if err != nil || len(chs) != 1 || chs[0].ID == 0 {
		t.Fatalf("route changes %+v %v", chs, err)
	}
	chs[0].ID = 0
	if !reflect.DeepEqual(chs[0], ch) {
		t.Errorf("route change %+v, want %+v", chs[0], ch)
	}
	if got, ok, err := s.HopInfo(ctx, h.IP); err != nil || !ok || !reflect.DeepEqual(got, h) {
		t.Errorf("hop info %+v %v %v, want %+v", got, ok, err, h)
	}
	sts, err := s.SpeedTests(ctx, relT0, relEnd)
	if err != nil || len(sts) != 1 || sts[0].ID == 0 {
		t.Fatalf("speed tests %+v %v", sts, err)
	}
	sts[0].ID = 0
	if want := relSpeedTest(); !reflect.DeepEqual(sts[0], want) {
		t.Errorf("speed test\n%+v\nwant\n%+v", sts[0], want)
	}
	if v, ok, err := s.Setting(ctx, "owner_tz"); v != "Europe/Berlin" || !ok || err != nil {
		t.Errorf("setting owner_tz %q %v %v", v, ok, err)
	}
}

// TestUpgradeAdditiveOnly: every table, index and column of the last
// release's database is still there after this build opens it, with the
// same declared type. Schema changes are additive only (new tables, columns
// and indexes), so the data users already have keeps its meaning: never
// drop, rename or retype anything.
func TestUpgradeAdditiveOnly(t *testing.T) {
	dir := releaseDir(t)
	old := schemaShape(t, openRO(t, filepath.Join(dir, DBName)))
	s := openT(t, dir, Options{Now: func() time.Time { return relEnd }})
	defer s.Close()
	now := schemaShape(t, s.db.r)
	for _, k := range slices.Sorted(maps.Keys(old)) {
		got, ok := now[k]
		switch {
		case !ok:
			t.Errorf("%s of the %s database is gone after the upgrade: schema changes are additive only (new tables, columns and indexes); never drop or rename anything", k, lastRelease)
		case got != old[k]:
			t.Errorf("%s is %q after the upgrade, %q in the %s database: schema changes are additive only; never retype anything", k, got, old[k], lastRelease)
		}
	}
}

// TestUpgradeCoversLastRelease: this build upgrades the last release's
// schema to its own, one migration step per version. A bump of
// SchemaVersion needs its step in migrations (sqlite.go), or users of the
// last release could not start this build.
func TestUpgradeCoversLastRelease(t *testing.T) {
	ver, _, err := inspect(ctx, filepath.Join("testdata", lastRelease+".db"))
	if err != nil {
		t.Fatal(err)
	}
	if ver < oldestUpgradable || ver > SchemaVersion() {
		t.Fatalf("the %s database has schema v%d, outside v%d..v%d", lastRelease, ver, oldestUpgradable, SchemaVersion())
	}
	if _, err := migrationsFrom(ver); err != nil {
		t.Errorf("SchemaVersion is %d but the migrations do not cover every step from the %s schema (v%d): add the missing step to migrations in sqlite.go: %v",
			SchemaVersion(), lastRelease, ver, err)
	}
	for v := oldestUpgradable; v < SchemaVersion(); v++ {
		n := 0
		for _, m := range migrations {
			if m.from == v {
				n++
			}
		}
		if n != 1 {
			t.Errorf("%d migrations from schema v%d, want 1", n, v)
		}
	}
}

// withMigrations sets schemaVersion and migrations for one test.
func withMigrations(t *testing.T, ver int, ms []migration) {
	t.Helper()
	oldVer, oldMs := schemaVersion, migrations
	schemaVersion, migrations = ver, ms
	t.Cleanup(func() { schemaVersion, migrations = oldVer, oldMs })
}

// TestMigrate: with a step from the release's schema to a next one, the
// release's database is upgraded in place: the step ran, user_version is
// the new one, one line is logged, and every row is kept. A kill before
// the commit leaves the old schema, upgraded on the next start. Without
// the step, Open fails and leaves the file exactly as it is.
func TestMigrate(t *testing.T) {
	ver, _, err := inspect(ctx, filepath.Join("testdata", lastRelease+".db"))
	if err != nil {
		t.Fatal(err)
	}
	next := ver + 1

	t.Run("missing step", func(t *testing.T) {
		withMigrations(t, next, nil)
		dir := releaseDir(t)
		path := filepath.Join(dir, DBName)
		before := fileSum(t, path)
		_, err := Open(dir, Options{})
		if want := fmt.Sprintf("no upgrade from schema v%d to v%d", ver, next); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Open: %v, want %q", err, want)
		}
		if fileSum(t, path) != before {
			t.Error("the database changed")
		}
		if _, err := OpenReadOnly(dir); err == nil || !strings.Contains(err.Error(), "upgrades it") {
			t.Errorf("OpenReadOnly: %v", err)
		}
	})

	t.Run("in place", func(t *testing.T) {
		defer func() { crashAt = "" }()
		withMigrations(t, next, []migration{{from: ver, up: func(ctx context.Context, tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `ALTER TABLE speedtests ADD COLUMN added TEXT; CREATE TABLE added(x)`)
			return err
		}}})
		dir := releaseDir(t)
		path := filepath.Join(dir, DBName)
		before := tableCounts(t, openRO(t, path))

		crashAt = "migrate"
		if _, err := Open(dir, Options{}); !errors.Is(err, errSimulatedCrash) {
			t.Fatalf("Open: %v", err)
		}
		crashAt = ""
		if v, objs, err := inspect(ctx, path); err != nil || v != ver || objs["added"] {
			t.Fatalf("after a crash: schema v%d, added table %v (%v), want v%d unchanged", v, objs["added"], err, ver)
		}

		log, logs := testLog()
		s := openT(t, dir, Options{Now: func() time.Time { return relEnd }, Log: log})
		defer s.Close()
		want := fmt.Sprintf("msg=\"store: database upgraded from schema v%d to v%d\"\n", ver, next)
		if l := logs.String(); !strings.HasSuffix(l, " "+want) || strings.Count(l, "\n") != 1 {
			t.Errorf("log\n%s\nwant\n%s", l, want)
		}
		if v := pragmaInt(t, s.db.w, "user_version"); v != int64(next) {
			t.Errorf("user_version %d, want %d", v, next)
		}
		after := tableCounts(t, s.db.r)
		if after["added"] != 0 {
			t.Errorf("added table: %v", after)
		}
		delete(after, "added")
		if !maps.Equal(after, before) {
			t.Errorf("row counts\n%v\nwant\n%v", after, before)
		}
		if typ := schemaShape(t, s.db.r)["column speedtests.added"]; typ != "TEXT" {
			t.Errorf("added column %q", typ)
		}
		var by string
		s.db.r.QueryRow(`SELECT value FROM meta WHERE key = 'created_by_version'`).Scan(&by)
		if by != lastRelease {
			t.Errorf("created_by_version %q", by)
		}
		equalRaw(t, "raw", rawAll(t, s, relKeys, relT0, relEnd), func() []RawPoint {
			f := NewFake()
			for _, x := range relSamples() {
				f.Observe(x)
			}
			return rawAll(t, f, relKeys, relT0, relEnd)
		}())
	})
}
