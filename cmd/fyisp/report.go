package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/export"
	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/report"
	"github.com/fuck-you-isp/fyisp/internal/store"
)

// cmdReport implements `fyisp report`: an evidence report (one
// self-contained HTML file, no scripts) from the saved data. Like export it
// opens the store read-only, so it works while fyisp runs.
//
//	fyisp report [--from T] [--to T] [--title S] [--redact] [--config FILE | --profile A,B [--geo eu]] [-o report.html]
func cmdReport(args []string) int {
	fs := flag.NewFlagSet("fyisp report", flag.ContinueOnError)
	dataDir := fs.String("data-dir", "", "data directory (default: per-OS state directory)")
	configPath := fs.String("config", "", "profile file the data was recorded with (default: the built-in profile)")
	var profiles profileNames
	var geos geoList
	fs.Var(&profiles, "profile", "profiles the data was recorded with (as given to fyisp --profile)")
	fs.Var(&geos, "geo", "regions the data was recorded with (as given to fyisp --geo)")
	noPath := fs.Bool("no-path", false, "the data was recorded with --no-path")
	from := fs.String("from", "7d", "start time: RFC 3339, YYYY-MM-DD, or an age like 24h or 7d")
	to := fs.String("to", "now", "end time, same forms as --from")
	title := fs.String("title", "", "report title")
	redactFlag := fs.Bool("redact", false, "apply the public-link rules: no private addresses, ISP edge masked, public notes only")
	out := fs.String("output", "", "output file, created with mode 0600 (default stdout)")
	fs.StringVar(out, "o", "", "shorthand for --output")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	fail := func(code int, err error) int {
		fmt.Fprintln(os.Stderr, "fyisp report:", err)
		return code
	}
	if fs.NArg() > 0 {
		return fail(2, fmt.Errorf("unexpected argument %q", fs.Arg(0)))
	}
	now := time.Now().UTC()
	f, err := export.ParseTime(*from, now)
	if err != nil {
		return fail(2, fmt.Errorf("--from: %w", err))
	}
	t, err := export.ParseTime(*to, now)
	if err != nil {
		return fail(2, fmt.Errorf("--to: %w", err))
	}
	if t.IsZero() {
		t = now
	}
	if f.IsZero() {
		f = t.Add(-report.DefaultRange)
	}
	if !t.After(f) {
		return fail(2, fmt.Errorf("--to must be after --from"))
	}
	if *configPath != "" && (len(profiles) > 0 || len(geos) > 0) {
		return fail(2, fmt.Errorf("--config and --profile/--geo are mutually exclusive"))
	}
	// No target limit: the report covers whatever the run was allowed.
	prof, err := loadProfile(config{configPath: *configPath, profiles: profiles, geos: geos, noPath: *noPath})
	if err != nil {
		return fail(2, err)
	}
	d := *dataDir
	if d == "" {
		if d, err = defaultDataDir(); err != nil {
			return fail(1, err)
		}
	}
	ro, err := store.OpenReadOnly(d)
	if err != nil {
		return fail(1, err)
	}
	defer ro.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	html, err := report.New(reportDeps(ctx, ro, prof)).Build(ctx, report.Options{From: f, To: t, Title: *title, Redact: *redactFlag})
	if err != nil {
		return fail(1, err)
	}
	if err := writeOutput(*out, html); err != nil {
		return fail(1, err)
	}
	if *out != "" && *out != "-" {
		fmt.Fprintf(os.Stderr, "fyisp report: wrote %s (%d KB)\n", *out, (len(html)+1023)/1024)
	}
	return 0
}

// reportDeps wires a report builder to a store: incidents, traces, and,
// when the store implements them, notes and baselines. The web endpoint
// can use the same function with the running store.
func reportDeps(ctx context.Context, st store.Reader, prof *model.Profile) report.Deps {
	d := report.Deps{
		Profile: func() *model.Profile { return prof },
		Store:   st,
		Version: version,
	}
	if tr, ok := st.(store.TraceReader); ok {
		d.Trace = tr
	}
	if is, ok := st.(interface {
		Incidents(ctx context.Context, from, to time.Time) ([]model.Incident, error)
	}); ok {
		d.Incidents = is.Incidents
	}
	if as, ok := st.(store.AnnotationStore); ok {
		d.Annotations = as
	}
	if br, ok := st.(store.BaselineReader); ok {
		var mu sync.Mutex
		cache := map[model.SeriesKey]model.Baseline{}
		missing := map[model.SeriesKey]bool{}
		d.Baselines = func(k model.SeriesKey, at time.Time) (model.Baseline, bool) {
			mu.Lock()
			defer mu.Unlock()
			if b, ok := cache[k]; ok {
				return b, true
			}
			if missing[k] {
				return model.Baseline{}, false
			}
			m, err := br.Baselines(ctx, []model.SeriesKey{k}, at, 7*24*time.Hour, false)
			b, ok := m[k]
			if err != nil || !ok || b.Samples == 0 {
				missing[k] = true
				return model.Baseline{}, false
			}
			cache[k] = b
			return b, true
		}
	}
	return d
}

// writeOutput writes to path (mode 0600) or stdout for "" and "-".
func writeOutput(path string, b []byte) error {
	var w io.Writer = os.Stdout
	if path != "" && path != "-" {
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
		if err != nil {
			return err
		}
		if _, err := f.Write(b); err != nil {
			f.Close()
			return err
		}
		return f.Close()
	}
	_, err := w.Write(b)
	return err
}
