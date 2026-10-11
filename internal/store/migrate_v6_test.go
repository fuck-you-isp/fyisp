package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

// writeV5 builds a v5 database (fyisp v0.7) in dir with what the share
// link left behind: a published report, the speed-test opt-in and its time
// ledger, and a test a visitor started.
func writeV5(t *testing.T, dir string) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(dir, DBName))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, m := range migrations[:5] {
		if _, err := db.Exec(m); err != nil {
			t.Fatal(err)
		}
	}
	html := []byte("<!doctype html><title>r</title><p>evidence</p>")
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO meta(key, value) VALUES('created_by_version', 'v0.7.1-fixture'), ('created_at', '2026-10-01T00:00:00Z'),
			('setting:speedtest_public', '1'), ('setting:speedtest_public_bytes', '{"Uses":[]}'), ('setting:speedtest_mode', 'nearby')`, nil},
		{`INSERT INTO reports(id, title, from_ms, to_ms, created_ms, public, bytes, html) VALUES(?, 'ISP outage', 1, 2, 3, 1, ?, ?)`,
			[]any{"rabcdefghijklmnopqrstuvwxy", len(html), compressReport(html)}},
		{`INSERT INTO speedtests(start_ms, end_ms, trigger, outcome, server) VALUES(?, ?, 'public', 'ok', 'Clouvider-London')`,
			[]any{t0.UnixMilli(), t0.Add(20 * time.Second).UnixMilli()}},
		{`PRAGMA user_version=5`, nil},
	} {
		if _, err := db.Exec(q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
}

func columns(t *testing.T, db *sql.DB, table string) []string {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		out = append(out, n)
	}
	return out
}

// TestMigrateV5ToV6: v0.8.0 drops what only the share link used. A v5
// database is backed up, then reports lose their public column and the
// share link's settings go; reports, other settings and the speed tests a
// visitor started stay. Until fyisp runs once, `fyisp report` and
// `fyisp export` (OpenReadOnly) refuse it with a hint.
func TestMigrateV5ToV6(t *testing.T) {
	dir := t.TempDir()
	writeV5(t, dir)
	if _, err := OpenReadOnly(dir); err == nil || !strings.Contains(err.Error(), "run fyisp once") {
		t.Fatalf("OpenReadOnly on v5: %v", err)
	}
	s := openT(t, dir, Options{Now: func() time.Time { return t0.Add(time.Hour) }})
	defer s.Close()
	if v := pragmaInt(t, s.db.w, "user_version"); v != 6 || SchemaVersion() != 6 {
		t.Fatalf("user_version %d, schema %d", v, SchemaVersion())
	}
	if cols := columns(t, s.db.r, "reports"); slices.Contains(cols, "public") || !slices.Contains(cols, "html") {
		t.Errorf("reports columns %v", cols)
	}
	if cols := columns(t, s.db.r, "annotations"); !slices.Contains(cols, "public") {
		t.Errorf("annotations lost public (redacted reports use it): %v", cols)
	}
	ctx := context.Background()
	m, html, err := s.Report(ctx, "rabcdefghijklmnopqrstuvwxy")
	if err != nil || m.Title != "ISP outage" || !strings.Contains(string(html), "evidence") {
		t.Errorf("report after migration: %+v %q %v", m, html, err)
	}
	for k, want := range map[string]bool{"speedtest_public": false, "speedtest_public_bytes": false, "speedtest_mode": true} {
		if _, ok, err := s.Setting(ctx, k); ok != want || err != nil {
			t.Errorf("setting %s kept %v, want %v (%v)", k, ok, want, err)
		}
	}
	if ts, err := s.SpeedTests(ctx, t0.Add(-time.Hour), t0.Add(time.Hour)); err != nil || len(ts) != 1 || ts[0].Trigger != "public" || !ts[0].OnDemand() {
		t.Errorf("speed tests after migration: %+v %v", ts, err)
	}
	// The backup is the v5 file as it was.
	b, err := sql.Open("sqlite", filepath.Join(dir, DBName+".bak-v5"))
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	var uv int
	b.QueryRow(`PRAGMA user_version`).Scan(&uv)
	if cols := columns(t, b, "reports"); uv != 5 || !slices.Contains(cols, "public") {
		t.Errorf("backup: user_version %d, reports %v", uv, cols)
	}
	// Saving a report works on the new schema.
	if err := s.SaveReport(ctx, model.ReportMeta{ID: "rzzzzzzzzzzzzzzzzzzzzzzzzz", Title: "new"}, []byte("<p>x</p>")); err != nil {
		t.Fatal(err)
	}
}

// TestFreshSchemaV6: a new database (v1 to v6 in one go) has no share-link
// column either, and OpenReadOnly opens it.
func TestFreshSchemaV6(t *testing.T) {
	dir := t.TempDir()
	s := openT(t, dir, Options{})
	if cols := columns(t, s.db.r, "reports"); slices.Contains(cols, "public") {
		t.Errorf("fresh reports columns %v", cols)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, DBName+".bak-v0")); err == nil {
		t.Error("a new database was backed up")
	}
	ro, err := OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	ro.Close()
}
