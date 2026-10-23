package export

import (
	"context"
	"encoding/csv"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/s3test"
	"github.com/fuck-you-isp/fyisp/internal/store"
)

// StorageReader reads the storage tests (*store.SQLite).
type StorageReader interface {
	StorageCampaign(ctx context.Context, id int64) (model.StorageCampaign, bool, error)
	StorageCampaigns(ctx context.Context, before int64, limit int, live bool) ([]model.StorageCampaign, error)
	StorageAttempts(ctx context.Context, f store.StorageAttemptFilter) ([]store.StorageAttempt, int, error)
}

// StorageHeader is the storage attempts' CSV header: one row per attempt.
// target is the target's id and target_name its name when the campaign
// ran (never its host or bucket); edge_prefix the provider address's /24.
var StorageHeader = []string{"campaign", "round", "target", "target_name", "phase", "op", "n", "at", "class", "group", "http_status", "s3_code",
	"request_id", "host_id", "dns_ms", "connect_ms", "tls_ms", "ttfb_ms", "total_ms", "bytes", "goodput_bps", "stall_ms",
	"edge_prefix", "tls_version", "tcp_rtt_ms", "retrans", "mss", "skew_s"}

// WriteStorage writes as CSV (StorageHeader) the attempts of campaign (0:
// every campaign) made in [from, to] (a zero end is open), oldest first,
// reading them a page at a time.
func WriteStorage(ctx context.Context, sr StorageReader, w io.Writer, campaign int64, from, to time.Time) error {
	var cs []model.StorageCampaign
	if campaign != 0 {
		c, ok, err := sr.StorageCampaign(ctx, campaign)
		if err != nil {
			return err
		}
		if ok {
			cs = []model.StorageCampaign{c}
		}
	} else {
		var err error
		if cs, err = sr.StorageCampaigns(ctx, 0, 1<<30, false); err != nil {
			return err
		}
	}
	cw := csv.NewWriter(w)
	if err := cw.Write(StorageHeader); err != nil {
		return err
	}
	for k := len(cs) - 1; k >= 0; k-- { // oldest campaign first
		c := cs[k]
		names := map[string]string{}
		for _, t := range c.Targets {
			names[t.ID] = t.Name
		}
		for after := int64(0); ; {
			as, _, err := sr.StorageAttempts(ctx, store.StorageAttemptFilter{Campaign: c.ID, After: after, Oldest: true, Limit: storagePage})
			if err != nil {
				return err
			}
			for _, a := range as {
				if (!from.IsZero() && a.At.Before(from)) || (!to.IsZero() && a.At.After(to)) {
					continue
				}
				if err := cw.Write(attemptRecord(c.ID, names, a)); err != nil {
					return err
				}
			}
			if len(as) < storagePage {
				break
			}
			after = as[len(as)-1].ID
			if err := ctx.Err(); err != nil {
				return err
			}
		}
	}
	cw.Flush()
	return cw.Error()
}

// storagePage is the attempts WriteStorage reads at a time.
const storagePage = 5000

// attemptRecord is an attempt's CSV row (StorageHeader).
func attemptRecord(campaign int64, names map[string]string, a store.StorageAttempt) []string {
	f := func(p *float64) string {
		if p == nil {
			return ""
		}
		return strconv.FormatFloat(*p, 'f', -1, 64)
	}
	i := func(p *int) string {
		if p == nil {
			return ""
		}
		return strconv.Itoa(*p)
	}
	status := ""
	if a.HTTPStatus != 0 {
		status = strconv.Itoa(a.HTTPStatus)
	}
	return []string{strconv.FormatInt(campaign, 10), strconv.FormatInt(a.Round, 10), a.Target, textCell(names[a.Target]), a.Phase, a.Op, strconv.Itoa(a.N),
		a.At.UTC().Format(TimeFormat), a.Class, s3test.Class(a.Class).Group(), status, a.S3Code, textCell(a.RequestID), textCell(a.HostID),
		f(a.DNSms), f(a.Connectms), f(a.TLSms), f(a.TTFBms), f(a.Totalms), strconv.FormatInt(a.Bytes, 10), f(a.GoodputBps), f(a.Stallms),
		a.EdgePrefix, a.TLSVersion, f(a.TCPRTTms), i(a.Retrans), i(a.MSS), i(a.SkewS)}
}

// textCell keeps text a spreadsheet would read as a formula (a provider's
// request ID, a target's name) as text: a leading ' before = + - @.
func textCell(v string) string {
	if v != "" && strings.ContainsRune("=+-@\t\r", rune(v[0])) {
		return "'" + v
	}
	return v
}
