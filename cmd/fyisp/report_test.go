package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/store"
)

// TestCmdReport writes a report from a real (read-only) store, with an
// incident from the outage log.
func TestCmdReport(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir, store.Options{Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	end := time.Now().UTC().Truncate(time.Hour).Add(-time.Hour)
	start := end.Add(-3 * time.Hour)
	for ts := start; ts.Before(end); ts = ts.Add(15 * time.Second) {
		k := model.SeriesKey{Target: "Discord", Kind: model.KindHTTPS}
		if ts.Sub(start) > time.Hour && ts.Sub(start) < 70*time.Minute {
			st.Observe(model.Sample{Key: k, Slot: ts, Lost: true, Reason: model.ReasonTimeout})
		} else {
			st.Observe(model.Sample{Key: k, Slot: ts, RTT: 40 * time.Millisecond})
		}
	}
	ctx := context.Background()
	if err := st.SaveIncident(ctx, &model.Incident{Start: start.Add(time.Hour), End: start.Add(70 * time.Minute),
		Kind: model.VerdictService, Summary: "Discord is not answering.", Targets: []string{"Discord"}, PeakLoss: 1}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "r.html")
	if code := cmdReport([]string{"--data-dir", dir, "--from", start.Format(time.RFC3339), "--to", end.Format(time.RFC3339),
		"--title", "CLI test", "--redact", "-o", out}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	html := string(b)
	for _, want := range []string{"<title>CLI test</title>", "Discord is not answering.", "Some services down", "redacted for sharing"} {
		if !strings.Contains(html, want) {
			t.Errorf("report lacks %q", want)
		}
	}
	if strings.Contains(html, dir) || strings.Contains(strings.ToLower(html), "<script") {
		t.Error("report leaks the data directory or has a script")
	}
	if fi, _ := os.Stat(out); fi.Mode().Perm() != 0o600 && os.PathSeparator == '/' {
		t.Errorf("mode %v", fi.Mode().Perm())
	}
	if code := cmdReport([]string{"--data-dir", dir, "--from", "1h", "--to", "2h"}); code != 2 {
		t.Errorf("reversed range: exit %d, want 2", code)
	}
}
