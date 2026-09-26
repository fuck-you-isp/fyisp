package export

import (
	"flag"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

// Flags registers the `fyisp export` flags on fs (main adds its own, such as
// --data-dir) and returns a function that builds the Options after
// fs.Parse. Relative times are resolved against now.
//
//	--format csv|sqlite  --tier raw|1h  --from T  --to T
//	--target NAME[,NAME]  --kind https,tcp,icmp,trace  --output FILE (-o)
//	--annotations        (the timeline notes instead of samples; CSV only)
//
// T is RFC 3339 ("2026-09-26T12:00:00Z"), a date ("2026-09-26", UTC
// midnight), "now", or an age such as "24h", "90m", "7d" (that long ago).
// The defaults export everything stored.
func Flags(fs *flag.FlagSet) func(now time.Time) (Options, error) {
	format := fs.String("format", FormatCSV, "output format: csv or sqlite")
	tier := fs.String("tier", TierRaw, "raw (every sample) or 1h (hourly summaries)")
	from := fs.String("from", "", "start time: RFC 3339, YYYY-MM-DD, or an age like 24h or 7d (default: oldest data)")
	to := fs.String("to", "", "end time, same forms as --from (default: newest data)")
	var targets, kinds multi
	fs.Var(&targets, "target", "target name to export (repeatable or comma-separated; default all)")
	fs.Var(&kinds, "kind", "probe kind: https, tcp, icmp, trace (traceroute hops; repeatable or comma-separated; default all)")
	notes := fs.Bool("annotations", false, "export the timeline annotations (CSV: id,at,end,public,text) instead of samples")
	out := fs.String("output", "", "output file, created with mode 0600 (default stdout; required for sqlite)")
	fs.StringVar(out, "o", "", "shorthand for --output")
	return func(now time.Time) (Options, error) {
		o := Options{Format: *format, Tier: *tier, Targets: targets, Output: *out, Annotations: *notes}
		if o.Annotations && o.Format != FormatCSV {
			return o, fmt.Errorf("--annotations is exported as CSV only")
		}
		switch o.Format {
		case FormatCSV:
		case FormatSQLite:
			if o.Output == "" || o.Output == "-" {
				return o, fmt.Errorf("--format sqlite needs --output FILE")
			}
		default:
			return o, fmt.Errorf("unknown --format %q (csv or sqlite)", o.Format)
		}
		if o.Tier != TierRaw && o.Tier != TierHourly {
			return o, fmt.Errorf("unknown --tier %q (raw or 1h)", o.Tier)
		}
		for _, k := range kinds {
			pk, err := ParseKind(k)
			if err != nil {
				return o, err
			}
			o.Kinds = append(o.Kinds, pk)
		}
		var err error
		if o.From, err = ParseTime(*from, now); err != nil {
			return o, fmt.Errorf("--from: %w", err)
		}
		if o.To, err = ParseTime(*to, now); err != nil {
			return o, fmt.Errorf("--to: %w", err)
		}
		if !o.From.IsZero() && !o.To.IsZero() && o.To.Before(o.From) {
			return o, fmt.Errorf("--to is before --from")
		}
		return o, nil
	}
}

type multi []string

func (m *multi) String() string { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error {
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			*m = append(*m, s)
		}
	}
	return nil
}

// ParseKind parses "https", "tcp", "icmp" or "trace".
func ParseKind(s string) (model.ProbeKind, error) {
	for _, k := range []model.ProbeKind{model.KindHTTPS, model.KindTCP, model.KindICMP, model.KindTrace} {
		if strings.EqualFold(s, k.String()) {
			return k, nil
		}
	}
	return 0, fmt.Errorf("unknown kind %q (https, tcp, icmp or trace)", s)
}

// ParseTime parses the --from/--to forms; "" gives the zero time.
func ParseTime(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	switch {
	case s == "":
		return time.Time{}, nil
	case s == "now":
		return now.UTC(), nil
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
		}
	}
	age := strings.TrimPrefix(s, "-")
	if d, ok := strings.CutSuffix(age, "d"); ok {
		if n, err := strconv.ParseFloat(d, 64); err == nil && n >= 0 {
			return now.Add(-time.Duration(n * float64(24*time.Hour))).UTC(), nil
		}
	}
	if d, err := time.ParseDuration(age); err == nil && d >= 0 {
		return now.Add(-d).UTC(), nil
	}
	return time.Time{}, fmt.Errorf("cannot parse time %q", s)
}
