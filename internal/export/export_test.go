package export

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/csv"
	"flag"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
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

// fill stores 5 hours of 6 series, 2 of them trace hops (every reason code,
// gaps) in a real store.
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
	// In Series order (target, kind, hop), which is the export order.
	keys := []model.SeriesKey{{Target: "alpha", Kind: model.KindHTTPS}, {Target: "alpha", Kind: model.KindICMP},
		{Target: "alpha", Kind: model.KindTrace, Hop: 1}, {Target: "alpha", Kind: model.KindTrace, Hop: 2},
		{Target: "beta,\"quoted\"", Kind: model.KindTCP}, {Target: "gamma", Kind: model.KindHTTPS}}
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
func point(t *testing.T, target, kind, ts string, rtt sql.NullFloat64, lost sql.NullInt64, reason string, hop sql.NullInt64) store.RawPoint {
	tt, err := time.Parse(TimeFormat, ts)
	if err != nil {
		t.Fatal(err)
	}
	p := store.RawPoint{Key: model.SeriesKey{Target: target, Kind: parseKind(t, kind), Hop: uint8(hop.Int64)}, TS: tt}
	if hop.Valid != (p.Key.Kind == model.KindTrace) || (hop.Valid && hop.Int64 < 1) {
		t.Fatalf("bad hop %v for kind %s", hop, kind)
	}
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
		var hop sql.NullInt64
		if r[6] != "" {
			v, err := strconv.ParseInt(r[6], 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			hop = sql.NullInt64{Int64: v, Valid: true}
		}
		out = append(out, point(t, r[0], r[1], r[2], rtt, lost, r[5], hop))
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

func TestFlags(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	build := Flags(fs)
	if err := fs.Parse([]string{"-o", "x.csv", "--from", "7d", "--to", "2026-09-26", "--target", "a,b", "--target", "c", "--kind", "icmp,TCP"}); err != nil {
		t.Fatal(err)
	}
	o, err := build(now)
	if err != nil {
		t.Fatal(err)
	}
	if o.Output != "x.csv" || !o.From.Equal(now.Add(-7*24*time.Hour)) || !o.To.Equal(time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC)) ||
		!slices.Equal(o.Targets, []string{"a", "b", "c"}) || !slices.Equal(o.Kinds, []model.ProbeKind{model.KindICMP, model.KindTCP}) {
		t.Fatalf("%+v", o)
	}
	fs = flag.NewFlagSet("export", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	Flags(fs)
	for _, f := range []string{"--format", "--tier"} {
		if err := fs.Parse([]string{f, "x"}); err == nil {
			t.Errorf("%s accepted", f)
		}
	}
}

// TestTraceHops: --kind trace exports only the hop series, each row with
// its hop.
func TestTraceHops(t *testing.T) {
	s, keys := fill(t)
	from, to := storedRange(t, s)
	var buf bytes.Buffer
	o := Options{Kinds: []model.ProbeKind{parseKind(t, "trace")}}
	if err := Write(ctx, s, &buf, o); err != nil {
		t.Fatal(err)
	}
	got := readCSV(t, buf.Bytes())
	equalPoints(t, got, raw(t, s, keys[2:4], from, to))
	hops := map[uint8]int{}
	for _, p := range got {
		hops[p.Key.Hop]++
	}
	if len(hops) != 2 || hops[1] == 0 || hops[2] == 0 {
		t.Fatalf("hops %v", hops)
	}
	if !bytes.Contains(buf.Bytes(), []byte(",trace,")) {
		t.Fatal("no trace rows")
	}
}

func TestSpeedTests(t *testing.T) {
	s, err := store.Open(t.TempDir(), store.Options{Now: func() time.Time { return t0 }})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	down, up := 412345678.5, 40e6
	re := int64(12)
	for _, x := range []model.SpeedTest{
		{Start: t0, End: t0.Add(23 * time.Second), Trigger: "schedule", Outcome: model.SpeedOK, Region: "uk", Server: "Clouvider-London", Port: 5203, Attempts: 2,
			DownBps: &down, UpBps: &up, DownRetrans: &re, Conn: model.ConnWiFi,
			FallbackLevel: "none", DownSecs: 4.2, UpSecs: 3, Bytes: 180 << 20,
			Latency: &model.SpeedLatency{Edge: model.LoadPhases{Idle: &model.LoadStats{N: 40, P50Ms: 8.2, P95Ms: 9.9},
				Down: &model.LoadStats{N: 60, P50Ms: 24, P95Ms: 61.5}, Up: &model.LoadStats{N: 58, Lost: 3, Loss: 0.052, P50Ms: 140, P95Ms: 1000}},
				Gateway: model.LoadPhases{Down: &model.LoadStats{N: 60, P50Ms: 1.4, P95Ms: 3}}},
			Handoff: &model.Handoff{ASN: 3356, Name: "LEVEL3", ISPASN: 3320, At: t0}},
		{Start: t0.Add(15 * time.Minute), End: t0.Add(15*time.Minute + 6*time.Second), Trigger: "schedule", Outcome: model.SpeedBusy, Region: "eu-central", Server: "Eranium-Amsterdam", Fallback: true, FallbackLevel: "same_metro", Port: 5209, Attempts: 3},
	} {
		if err := s.SaveSpeedTest(ctx, &x); err != nil {
			t.Fatal(err)
		}
	}
	fs := flag.NewFlagSet("x", flag.ContinueOnError)
	parse := Flags(fs)
	fs.Parse([]string{"--speedtests"})
	o, err := parse(t0)
	if err != nil || !o.SpeedTests {
		t.Fatalf("flags: %+v %v", o, err)
	}
	var buf bytes.Buffer
	if err := Run(ctx, s, o, &buf); err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		"id,start,end,trigger,outcome,region,server,fallback,fallback_level,port,attempts,down_bps,up_bps,down_retrans,up_retrans," +
			"down_s,up_s,capped_by_server,bytes,score_down,score_up," +
			"conn,handoff_asn,isp_asn,edge_idle_p50_ms,edge_idle_loss,edge_down_p50_ms,edge_down_p95_ms,edge_down_loss,edge_up_p50_ms,edge_up_p95_ms,edge_up_loss," +
			"gw_down_p50_ms,gw_down_p95_ms,gw_up_p50_ms,gw_up_p95_ms",
		"1,2026-09-01T00:00:00.000Z,2026-09-01T00:00:23.000Z,schedule,ok,uk,Clouvider-London,0,none,5203,2,412345678.5,40000000,12,," +
			"4.2,3,0,188743680,,," +
			"wifi,3356,3320,8.2,0,24,61.5,0,140,1000,0.052,1.4,3,,",
		"2,2026-09-01T00:15:00.000Z,2026-09-01T00:15:06.000Z,schedule,busy,eu-central,Eranium-Amsterdam,1,same_metro,5209,3,,,,," +
			",,0,0,,," +
			",,,,,,,,,,,,,,",
	}, "\n") + "\n"
	if buf.String() != want {
		t.Errorf("got\n%s\nwant\n%s", buf.String(), want)
	}
}
