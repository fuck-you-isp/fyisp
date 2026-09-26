package export

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/csv"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/store"
	"github.com/fuck-you-isp/fyisp/internal/store/blob"
	"github.com/fuck-you-isp/fyisp/internal/store/synth"
)

var (
	ctx = context.Background()
	t0  = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
)

// fill stores 5 hours of 4 series (every reason code, gaps) in a real store.
func fill(t *testing.T) (*store.SQLite, []model.SeriesKey) {
	t.Helper()
	now := t0
	s, err := store.Open(t.TempDir(), store.Options{
		Interval: func(k model.SeriesKey) time.Duration { return time.Duration(k.Kind) * 5 * time.Second },
		Now:      func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	keys := []model.SeriesKey{{Target: "alpha", Kind: model.KindHTTPS}, {Target: "alpha", Kind: model.KindICMP}, {Target: "beta,\"quoted\"", Kind: model.KindTCP}, {Target: "gamma", Kind: model.KindHTTPS}}
	for i, k := range keys {
		g := synth.New(7, i)
		iv := time.Duration(k.Kind) * 5 * time.Second
		for tt := t0.Add(time.Duration(i) * time.Second); tt.Before(t0.Add(5 * time.Hour)); tt = tt.Add(iv) {
			x := g.Next()
			smp := model.Sample{Key: k, Slot: tt}
			if tt.Sub(t0) < 16*iv { // every code early on
				x = blob.Lost(uint8(tt.Sub(t0) / iv))
			}
			if x.Valid {
				smp.RTT = time.Duration(x.Value) * blob.Unit
			} else {
				smp.Lost, smp.Reason = true, model.Reason(x.Code)
			}
			s.Observe(smp)
		}
	}
	now = t0.Add(4*time.Hour + 30*time.Minute) // hours 0-3 closed, hour 4 partly flushed
	if err := s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	return s, keys
}

// storedRange is what an export without From/To covers: from the first to
// the last real sample of any series (store.Reader.Series), so the
// not-measured slots before a series' first sample are not exported.
func storedRange(t *testing.T, r store.Reader) (from, to time.Time) {
	ss, err := r.Series(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range ss {
		if from.IsZero() || x.First.Before(from) {
			from = x.First
		}
		if x.Last.After(to) {
			to = x.Last
		}
	}
	return from, to
}

func raw(t *testing.T, r store.Reader, keys []model.SeriesKey, from, to time.Time) []store.RawPoint {
	var out []store.RawPoint
	if err := r.Raw(ctx, keys, from, to, func(p store.RawPoint) error { out = append(out, p); return nil }); err != nil {
		t.Fatal(err)
	}
	return out
}

func parseKind(t *testing.T, s string) model.ProbeKind {
	k, err := ParseKind(s)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func parseReason(t *testing.T, s string) model.Reason {
	for r := model.Reason(0); r <= model.MaxReason; r++ {
		if ReasonName(r) == s {
			return r
		}
	}
	t.Fatalf("reason %q", s)
	return 0
}

// point rebuilds a RawPoint from exported columns.
func point(t *testing.T, target, kind, ts string, rtt sql.NullFloat64, lost sql.NullInt64, reason string) store.RawPoint {
	tt, err := time.Parse(TimeFormat, ts)
	if err != nil {
		t.Fatal(err)
	}
	p := store.RawPoint{Key: model.SeriesKey{Target: target, Kind: parseKind(t, kind)}, TS: tt}
	switch {
	case !lost.Valid: // not measured
		if rtt.Valid || reason != "not measured" {
			t.Fatalf("bad not-measured row %v %v %q", rtt, lost, reason)
		}
		p.Lost = true
	case lost.Int64 == 1:
		p.Lost, p.Reason = true, parseReason(t, reason)
	default:
		if !rtt.Valid || reason != "" {
			t.Fatalf("bad measured row")
		}
		p.RTTms = rtt.Float64
	}
	return p
}

func equalPoints(t *testing.T, got, want []store.RawPoint) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%d points, want %d", len(got), len(want))
	}
	for i := range got {
		g, w := got[i], want[i]
		if g.Key != w.Key || !g.TS.Equal(w.TS) || g.RTTms != w.RTTms || g.Lost != w.Lost || g.Reason != w.Reason {
			t.Fatalf("point %d: %+v, want %+v", i, g, w)
		}
	}
}

func readCSV(t *testing.T, b []byte) []store.RawPoint {
	recs, err := csv.NewReader(bytes.NewReader(b)).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(recs[0], RawHeader) {
		t.Fatalf("header %v", recs[0])
	}
	var out []store.RawPoint
	for _, r := range recs[1:] {
		var rtt sql.NullFloat64
		var lost sql.NullInt64
		if r[3] != "" {
			v, err := strconv.ParseFloat(r[3], 64)
			if err != nil {
				t.Fatal(err)
			}
			rtt = sql.NullFloat64{Float64: v, Valid: true}
		}
		if r[4] != "" {
			v, _ := strconv.ParseInt(r[4], 10, 64)
			lost = sql.NullInt64{Int64: v, Valid: true}
		}
		out = append(out, point(t, r[0], r[1], r[2], rtt, lost, r[5]))
	}
	return out
}

func checkMode(t *testing.T, path string) {
	if runtime.GOOS == "windows" {
		return
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("%s mode %v", path, fi.Mode())
	}
}

func TestCSVRoundTrip(t *testing.T) {
	s, keys := fill(t)
	from, to := storedRange(t, s)
	all := raw(t, s, keys, from, to)
	var buf bytes.Buffer
	if err := Write(ctx, s, &buf, Options{}); err != nil {
		t.Fatal(err)
	}
	equalPoints(t, readCSV(t, buf.Bytes()), all)
	t.Logf("csv: %d rows, %d bytes; first rows:\n%s", len(all), buf.Len(), bytes.Join(bytes.SplitN(buf.Bytes(), []byte("\n"), 5)[:4], []byte("\n")))

	// Filters, to a file.
	from, to = t0.Add(90*time.Minute+500*time.Millisecond), t0.Add(3*time.Hour+7*time.Second)
	path := filepath.Join(t.TempDir(), "out.csv")
	o := Options{From: from, To: to, Targets: []string{"alpha"}, Kinds: []model.ProbeKind{model.KindICMP}}
	if err := WriteFile(ctx, s, path, o); err != nil {
		t.Fatal(err)
	}
	checkMode(t, path)
	b, _ := os.ReadFile(path)
	equalPoints(t, readCSV(t, b), raw(t, s, keys[1:2], from, to))
	if err := WriteFile(ctx, s, path, o); err == nil {
		t.Fatal("overwrote an existing file")
	}
}

func TestSQLiteRoundTrip(t *testing.T) {
	s, keys := fill(t)
	path := filepath.Join(t.TempDir(), "out.db")
	if err := WriteFile(ctx, s, path, Options{Format: FormatSQLite}); err != nil {
		t.Fatal(err)
	}
	checkMode(t, path)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT target, kind, ts_utc, rtt_ms, lost, coalesce(reason, '') FROM samples ORDER BY rowid`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []store.RawPoint
	for rows.Next() {
		var target, kind, ts, reason string
		var rtt sql.NullFloat64
		var lost sql.NullInt64
		if err := rows.Scan(&target, &kind, &ts, &rtt, &lost, &reason); err != nil {
			t.Fatal(err)
		}
		got = append(got, point(t, target, kind, ts, rtt, lost, reason))
	}
	from, to := storedRange(t, s)
	equalPoints(t, got, raw(t, s, keys, from, to))
}

func TestHourly(t *testing.T) {
	s, keys := fill(t)
	var buf bytes.Buffer
	if err := Write(ctx, s, &buf, Options{Tier: TierHourly}); err != nil {
		t.Fatal(err)
	}
	recs, err := csv.NewReader(&buf).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(recs[0], HourlyHeader) || len(recs)-1 != 5*len(keys) {
		t.Fatalf("%d rows: %v", len(recs)-1, recs[:2])
	}
	// Per series and hour, n and lost match the raw points.
	type hk struct {
		k string
		h string
	}
	want := map[hk][2]int{}
	for _, p := range raw(t, s, keys, t0, t0.Add(6*time.Hour)) {
		c := want[hk{p.Key.Target + "/" + p.Key.Kind.String(), p.TS.Truncate(time.Hour).Format(TimeFormat)}]
		switch {
		case !p.Lost:
			c[0]++
		case p.Reason != model.ReasonGap:
			c[1]++
		}
		want[hk{p.Key.Target + "/" + p.Key.Kind.String(), p.TS.Truncate(time.Hour).Format(TimeFormat)}] = c
	}
	for _, r := range recs[1:] {
		w := want[hk{r[0] + "/" + r[1], r[2]}]
		if fmt.Sprint(w[0]) != r[3] || fmt.Sprint(w[1]) != r[4] || r[5] == "" {
			t.Fatalf("row %v, want n=%d lost=%d", r, w[0], w[1])
		}
	}
	path := filepath.Join(t.TempDir(), "h.db")
	if err := WriteFile(ctx, s, path, Options{Format: FormatSQLite, Tier: TierHourly, From: t0.Add(2 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	db, _ := sql.Open("sqlite", path)
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM summary_1h`).Scan(&n); err != nil || n != 3*len(keys) {
		t.Fatalf("sqlite hourly rows %d %v", n, err)
	}
}

func TestFlags(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	build := Flags(fs)
	if err := fs.Parse([]string{"--format", "sqlite", "-o", "x.db", "--from", "7d", "--to", "2026-09-26", "--target", "a,b", "--target", "c", "--kind", "icmp,TCP", "--tier", "1h"}); err != nil {
		t.Fatal(err)
	}
	o, err := build(now)
	if err != nil {
		t.Fatal(err)
	}
	if o.Format != FormatSQLite || o.Output != "x.db" || !o.From.Equal(now.Add(-7*24*time.Hour)) || !o.To.Equal(time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC)) ||
		!slices.Equal(o.Targets, []string{"a", "b", "c"}) || !slices.Equal(o.Kinds, []model.ProbeKind{model.KindICMP, model.KindTCP}) || o.Tier != TierHourly {
		t.Fatalf("%+v", o)
	}
	fs = flag.NewFlagSet("export", flag.ContinueOnError)
	build = Flags(fs)
	fs.Parse([]string{"--format", "sqlite"})
	if _, err := build(now); err == nil {
		t.Fatal("sqlite without --output accepted")
	}
}
