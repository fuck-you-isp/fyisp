// Package export writes stored samples as CSV (`fyisp export`).
//
// One row per stored slot, exactly what store.Reader.Raw
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
// TTL of a traceroute hop (kind trace) and empty for every other kind.
//
// Speed tests (--speedtests), one row per test, oldest first; bits per
// second, seconds and milliseconds, empty when not measured or unknown;
// the edge columns are the ISP edge's latency before (idle) and during
// each direction (lost: probes without an answer):
//
//	id,start,end,trigger,outcome,region,server,fallback,fallback_level,port,attempts,down_bps,up_bps,down_retrans,up_retrans,
//	down_s,up_s,down_capped_by_bytes,up_capped_by_bytes,capped_by_server,bytes,score_down,score_up,idle_gateway_ms,loaded_gateway_ms,
//	conn,handoff_asn,isp_asn,edge_idle_p50_ms,edge_idle_loss,edge_down_p50_ms,edge_down_p95_ms,edge_down_loss,edge_up_p50_ms,edge_up_p95_ms,edge_up_loss,
//	gw_down_p50_ms,gw_down_p95_ms,gw_up_p50_ms,gw_up_p95_ms
//
// (one line). Latency under load: ISP edge and gateway RTT percentiles in
// ms, lost probes counted as 1 s; loss is a fraction.
package export

import (
	"bufio"
	"context"
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
)

// TimeFormat is the timestamp format of the ts, start and end columns.
const TimeFormat = "2006-01-02T15:04:05.000Z"

// Options select what to export.
type Options struct {
	From, To time.Time
	Targets  []string          // empty: all
	Kinds    []model.ProbeKind // empty: all
	Output   string            // file path; "" or "-" means the io.Writer
	// SpeedTests exports the speed tests that started in [From, To] as CSV
	// (SpeedTestHeader) instead of samples. The reader must also be a
	// SpeedTestReader.
	SpeedTests bool
}

// SpeedTestReader reads stored speed tests (*store.SQLite).
type SpeedTestReader interface {
	SpeedTests(ctx context.Context, from, to time.Time) ([]model.SpeedTest, error)
}

// RawHeader and SpeedTestHeader are the CSV headers.
var (
	RawHeader = []string{"target", "kind", "ts", "rtt_ms", "lost", "reason", "hop"}
	// SpeedTestHeader: start and end in TimeFormat; outcome ok, busy (the
	// server ran another test: not a slow result), unreachable or failed.
	SpeedTestHeader = []string{"id", "start", "end", "trigger", "outcome", "region", "server", "fallback", "fallback_level", "port", "attempts", "down_bps", "up_bps",
		"down_retrans", "up_retrans", "down_s", "up_s", "down_capped_by_bytes", "up_capped_by_bytes", "capped_by_server", "bytes", "score_down", "score_up",
		"idle_gateway_ms", "loaded_gateway_ms",
		"conn", "handoff_asn", "isp_asn", "edge_idle_p50_ms", "edge_idle_loss", "edge_down_p50_ms", "edge_down_p95_ms", "edge_down_loss",
		"edge_up_p50_ms", "edge_up_p95_ms", "edge_up_loss", "gw_down_p50_ms", "gw_down_p95_ms", "gw_up_p50_ms", "gw_up_p95_ms"}
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
	if o.SpeedTests {
		sr, ok := r.(SpeedTestReader)
		if !ok {
			return errors.New("export: this store has no speed tests")
		}
		return WriteSpeedTests(ctx, sr, w, o.From, o.To)
	}
	keys, o, err := selectKeys(ctx, r, o)
	if err != nil {
		return err
	}
	cw := csv.NewWriter(w)
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
	if err != nil {
		return err
	}
	cw.Flush()
	return cw.Error()
}

// WriteSpeedTests writes the tests that started in [from, to] as CSV
// (SpeedTestHeader); a zero from or to leaves that side open.
func WriteSpeedTests(ctx context.Context, sr SpeedTestReader, w io.Writer, from, to time.Time) error {
	if from.IsZero() {
		from = time.UnixMilli(0)
	}
	if to.IsZero() {
		to = time.UnixMilli(math.MaxInt64 / 2)
	}
	ts, err := sr.SpeedTests(ctx, from, to)
	if err != nil {
		return err
	}
	f := func(p *float64) string {
		if p == nil {
			return ""
		}
		return strconv.FormatFloat(*p, 'f', -1, 64)
	}
	i := func(p *int64) string {
		if p == nil {
			return ""
		}
		return strconv.FormatInt(*p, 10)
	}
	fb := func(b bool) string {
		if b {
			return "1"
		}
		return "0"
	}
	cw := csv.NewWriter(w)
	if err := cw.Write(SpeedTestHeader); err != nil {
		return err
	}
	secs := func(v float64) string {
		if v == 0 {
			return ""
		}
		return strconv.FormatFloat(v, 'f', -1, 64)
	}
	for _, t := range ts {
		row := []string{strconv.FormatInt(t.ID, 10), t.Start.UTC().Format(TimeFormat), t.End.UTC().Format(TimeFormat),
			t.Trigger, t.Outcome, t.Region, t.Server, fb(t.Fallback), t.FallbackLevel, strconv.Itoa(t.Port), strconv.Itoa(t.Attempts), f(t.DownBps), f(t.UpBps),
			i(t.DownRetrans), i(t.UpRetrans), secs(t.DownSecs), secs(t.UpSecs), fb(t.DownShort), fb(t.UpShort), fb(t.CappedByServer),
			strconv.FormatInt(t.Bytes, 10), f(t.ScoreDown), f(t.ScoreUp), f(t.IdleGatewayMs), f(t.LoadedGatewayMs)}
		var lat model.SpeedLatency
		if t.Latency != nil {
			lat = *t.Latency
		}
		st := func(x *model.LoadStats, part string) string {
			if x == nil {
				return ""
			}
			v := map[string]float64{"p50": x.P50Ms, "p95": x.P95Ms, "loss": x.Loss}[part]
			return f(&v)
		}
		as := func(v uint32) string {
			if v == 0 {
				return ""
			}
			return strconv.FormatUint(uint64(v), 10)
		}
		var h model.Handoff
		if t.Handoff != nil {
			h = *t.Handoff
		}
		row = append(row, t.Conn, as(h.ASN), as(h.ISPASN),
			st(lat.Edge.Idle, "p50"), st(lat.Edge.Idle, "loss"),
			st(lat.Edge.Down, "p50"), st(lat.Edge.Down, "p95"), st(lat.Edge.Down, "loss"),
			st(lat.Edge.Up, "p50"), st(lat.Edge.Up, "p95"), st(lat.Edge.Up, "loss"),
			st(lat.Gateway.Down, "p50"), st(lat.Gateway.Down, "p95"), st(lat.Gateway.Up, "p50"), st(lat.Gateway.Up, "p95"))
		if err := cw.Write(row); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

// WriteFile creates path (mode 0600; it must not exist) and exports into it.
// On error the partial file is removed.
func WriteFile(ctx context.Context, r store.Reader, path string, o Options) (err error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("export: %w", err)
	}
	defer func() {
		if err != nil {
			os.Remove(path)
		}
	}()
	bw := bufio.NewWriterSize(f, 1<<16)
	err = Write(ctx, r, bw, o)
	if err == nil {
		err = bw.Flush()
	}
	return errors.Join(err, f.Close())
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

// hopText is the hop column: the TTL of a trace hop, else empty.
func hopText(k model.SeriesKey) string {
	if k.Kind != model.KindTrace {
		return ""
	}
	return strconv.Itoa(int(k.Hop))
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
