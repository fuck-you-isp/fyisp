// Package export writes stored samples as CSV or as a plain SQLite file
// (`fyisp export`).
//
// Raw tier (default), one row per stored slot, exactly what store.Reader.Raw
// returns:
//
//	target,kind,ts,rtt_ms,lost,reason,hop
//	github,https,2026-09-26T12:00:00.000Z,23.41,0,,
//	github,tcp,2026-09-26T12:00:15.000Z,,1,timeout,
//	github,icmp,2026-09-26T12:00:05.000Z,,,not measured,
//	github,trace,2026-09-26T12:00:00.000Z,8.2,0,,3
//
// rtt_ms is set only for measured slots; lost is 0 or 1, and empty for
// not-measured slots (fyisp was stopped, the machine slept), which are not
// losses. ts is the slot start in UTC with millisecond precision. hop is the
// TTL of a traceroute hop (kind trace) and empty for every other kind; it is
// the last column so that v0.1 readers of the first six keep working.
//
// Hourly tier ("1h"), one row per series and hour with data:
//
//	target,kind,ts,n,lost,rtt_mean_ms,rtt_min_ms,rtt_max_ms,hop
//
// The SQLite format holds the same columns in a table named samples (raw;
// ts is the column ts_utc, lost is NULL when not measured) or summary_1h;
// hop is NULL for kinds other than trace.
package export

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"slices"
	"strconv"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/store"
	_ "modernc.org/sqlite"
)

// Formats and tiers.
const (
	FormatCSV    = "csv"
	FormatSQLite = "sqlite"
	TierRaw      = "raw"
	TierHourly   = "1h"
)

// TimeFormat is the timestamp format of both output formats.
const TimeFormat = "2006-01-02T15:04:05.000Z"

// Options select what to export.
type Options struct {
	Format   string // FormatCSV (default) or FormatSQLite
	Tier     string // TierRaw (default) or TierHourly
	From, To time.Time
	Targets  []string          // empty: all
	Kinds    []model.ProbeKind // empty: all
	Output   string            // file path; "" or "-" means the io.Writer (CSV only)
}

// RawHeader and HourlyHeader are the CSV headers.
var (
	RawHeader    = []string{"target", "kind", "ts", "rtt_ms", "lost", "reason", "hop"}
	HourlyHeader = []string{"target", "kind", "ts", "n", "lost", "rtt_mean_ms", "rtt_min_ms", "rtt_max_ms", "hop"}
)

// Run exports to o.Output, or as CSV to stdout when o.Output is "" or "-".
func Run(ctx context.Context, r store.Reader, o Options, stdout io.Writer) error {
	if o.Output == "" || o.Output == "-" {
		return Write(ctx, r, stdout, o)
	}
	return WriteFile(ctx, r, o.Output, o)
}

// Write writes CSV to w.
func Write(ctx context.Context, r store.Reader, w io.Writer, o Options) error {
	if o.Format != "" && o.Format != FormatCSV {
		return fmt.Errorf("export: format %q needs an output file", o.Format)
	}
	keys, o, err := selectKeys(ctx, r, o)
	if err != nil {
		return err
	}
	cw := csv.NewWriter(w)
	switch o.Tier {
	case "", TierRaw:
		if err := cw.Write(RawHeader); err != nil {
			return err
		}
		rec := make([]string, 7)
		err = r.Raw(ctx, keys, o.From, o.To, func(p store.RawPoint) error {
			rec[0], rec[1], rec[2] = p.Key.Target, p.Key.Kind.String(), p.TS.UTC().Format(TimeFormat)
			rec[6] = hopText(p.Key)
			switch {
			case !p.Lost:
				rec[3], rec[4], rec[5] = fmtMs(p.RTTms), "0", ""
			case p.Reason == model.ReasonGap:
				rec[3], rec[4], rec[5] = "", "", ReasonName(p.Reason)
			default:
				rec[3], rec[4], rec[5] = "", "1", ReasonName(p.Reason)
			}
			return cw.Write(rec)
		})
	case TierHourly:
		if err := cw.Write(HourlyHeader); err != nil {
			return err
		}
		err = hourly(ctx, r, keys, o, func(h hourRow) error {
			return cw.Write([]string{h.key.Target, h.key.Kind.String(), h.ts.Format(TimeFormat),
				strconv.FormatUint(uint64(h.n), 10), strconv.FormatUint(uint64(h.lost), 10),
				fmtMs32(h.mean), fmtMs32(h.min), fmtMs32(h.max), hopText(h.key)})
		})
	default:
		return fmt.Errorf("export: unknown tier %q", o.Tier)
	}
	if err != nil {
		return err
	}
	cw.Flush()
	return cw.Error()
}

// WriteFile creates path (mode 0600; it must not exist) and exports into it
// in o.Format. On error the partial file is removed.
func WriteFile(ctx context.Context, r store.Reader, path string, o Options) (err error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("export: %w", err)
	}
	defer func() {
		if err != nil {
			os.Remove(path)
			os.Remove(path + "-journal")
		}
	}()
	switch o.Format {
	case "", FormatCSV:
		bw := bufio.NewWriterSize(f, 1<<16)
		err = Write(ctx, r, bw, o)
		if err == nil {
			err = bw.Flush()
		}
		return errors.Join(err, f.Close())
	case FormatSQLite:
		f.Close()
		return writeSQLite(ctx, r, path, o)
	}
	f.Close()
	return fmt.Errorf("export: unknown format %q", o.Format)
}

func writeSQLite(ctx context.Context, r store.Reader, path string, o Options) error {
	keys, o, err := selectKeys(ctx, r, o)
	if err != nil {
		return err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	switch o.Tier {
	case "", TierRaw:
		if _, err := tx.ExecContext(ctx, `CREATE TABLE samples(target TEXT NOT NULL, kind TEXT NOT NULL, ts_utc TEXT NOT NULL,
			rtt_ms REAL, lost INTEGER, reason TEXT, hop INTEGER)`); err != nil {
			return err
		}
		ins, err := tx.PrepareContext(ctx, `INSERT INTO samples VALUES(?,?,?,?,?,?,?)`)
		if err != nil {
			return err
		}
		defer ins.Close()
		err = r.Raw(ctx, keys, o.From, o.To, func(p store.RawPoint) error {
			var rtt, lost, reason any
			switch {
			case !p.Lost:
				rtt, lost = p.RTTms, 0
			case p.Reason == model.ReasonGap:
				reason = ReasonName(p.Reason)
			default:
				lost, reason = 1, ReasonName(p.Reason)
			}
			_, err := ins.ExecContext(ctx, p.Key.Target, p.Key.Kind.String(), p.TS.UTC().Format(TimeFormat), rtt, lost, reason, hopValue(p.Key))
			return err
		})
		if err != nil {
			return err
		}
	case TierHourly:
		if _, err := tx.ExecContext(ctx, `CREATE TABLE summary_1h(target TEXT NOT NULL, kind TEXT NOT NULL, ts_utc TEXT NOT NULL,
			n INTEGER NOT NULL, lost INTEGER NOT NULL, rtt_mean_ms REAL, rtt_min_ms REAL, rtt_max_ms REAL, hop INTEGER)`); err != nil {
			return err
		}
		ins, err := tx.PrepareContext(ctx, `INSERT INTO summary_1h VALUES(?,?,?,?,?,?,?,?,?)`)
		if err != nil {
			return err
		}
		defer ins.Close()
		err = hourly(ctx, r, keys, o, func(h hourRow) error {
			_, err := ins.ExecContext(ctx, h.key.Target, h.key.Kind.String(), h.ts.Format(TimeFormat), h.n, h.lost,
				nullF(h.mean), nullF(h.min), nullF(h.max), hopValue(h.key))
			return err
		})
		if err != nil {
			return err
		}
	default:
		return fmt.Errorf("export: unknown tier %q", o.Tier)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return db.Close()
}

// selectKeys resolves the target/kind filters against the stored series and
// fills in a zero From/To with the stored range.
func selectKeys(ctx context.Context, r store.Reader, o Options) ([]model.SeriesKey, Options, error) {
	ss, err := r.Series(ctx)
	if err != nil {
		return nil, o, err
	}
	var keys []model.SeriesKey
	var first, last time.Time
	for _, s := range ss {
		if len(o.Targets) > 0 && !slices.Contains(o.Targets, s.Key.Target) {
			continue
		}
		if len(o.Kinds) > 0 && !slices.Contains(o.Kinds, s.Key.Kind) {
			continue
		}
		keys = append(keys, s.Key)
		if first.IsZero() || s.First.Before(first) {
			first = s.First
		}
		if s.Last.After(last) {
			last = s.Last
		}
	}
	for _, t := range o.Targets {
		if !slices.ContainsFunc(ss, func(s store.SeriesInfo) bool { return s.Key.Target == t }) {
			return nil, o, fmt.Errorf("export: no data for target %q", t)
		}
	}
	if o.From.IsZero() {
		o.From = first
	}
	if o.To.IsZero() {
		o.To = last
	}
	return keys, o, nil
}

type hourRow struct {
	key            model.SeriesKey
	ts             time.Time
	n, lost        uint32
	mean, min, max float32
}

// hourly reads whole hours through Panel in the hourly tier with one-hour
// buckets: the query range is widened past RawMaxRange if needed, and rows
// outside [From truncated to the hour, To] are skipped.
func hourly(ctx context.Context, r store.Reader, keys []model.SeriesKey, o Options, fn func(hourRow) error) error {
	if len(keys) == 0 || o.To.Before(o.From) {
		return nil
	}
	from := o.From.UTC().Truncate(time.Hour)
	qFrom := from
	if o.To.Sub(qFrom) <= store.RawMaxRange {
		qFrom = o.To.Add(-store.RawMaxRange - time.Hour).Truncate(time.Hour)
	}
	p, err := r.Panel(ctx, store.PanelQuery{Keys: keys, From: qFrom, To: o.To, MaxPoints: int(o.To.Sub(qFrom)/time.Hour) + 2})
	if err != nil {
		return err
	}
	if p.Tier != store.TierHourly || p.Step != time.Hour {
		return fmt.Errorf("export: store returned tier %s step %v, want 1h buckets", p.Tier, p.Step)
	}
	for _, c := range p.Series {
		for i := range c.N {
			ts := p.Start.Add(time.Duration(i) * p.Step).UTC()
			if ts.Before(from) || c.N[i]+c.Lost[i] == 0 {
				continue
			}
			if err := fn(hourRow{key: c.Key, ts: ts, n: c.N[i], lost: c.Lost[i], mean: c.Mean[i], min: c.Min[i], max: c.Max[i]}); err != nil {
				return err
			}
		}
	}
	return nil
}

// hopText is the hop column: the TTL of a trace hop, else empty.
func hopText(k model.SeriesKey) string {
	if k.Kind != model.KindTrace {
		return ""
	}
	return strconv.Itoa(int(k.Hop))
}

// hopValue is the SQLite hop column: the TTL of a trace hop, else NULL.
func hopValue(k model.SeriesKey) any {
	if k.Kind != model.KindTrace {
		return nil
	}
	return int64(k.Hop)
}

// ReasonName is the reason column value: model.Reason.String(), except that
// codes without a name of their own (9..14) are written "code-N" so the
// export stays lossless.
func ReasonName(r model.Reason) string {
	if r.String() == model.ReasonOther.String() && r != model.ReasonOther {
		return "code-" + strconv.Itoa(int(r))
	}
	return r.String()
}

// fmtMs formats milliseconds with the fewest digits that parse back to the
// same float64.
func fmtMs(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

func fmtMs32(v float32) string {
	if math.IsNaN(float64(v)) {
		return ""
	}
	return strconv.FormatFloat(float64(v), 'f', -1, 32)
}

func nullF(v float32) any {
	if math.IsNaN(float64(v)) {
		return nil
	}
	f, _ := strconv.ParseFloat(fmtMs32(v), 64) // 12.34, not 12.340000152587890625
	return f
}
