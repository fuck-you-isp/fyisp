package web

import (
	"bytes"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/export"
	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/s3test"
	"github.com/fuck-you-isp/fyisp/internal/storage"
	"github.com/fuck-you-isp/fyisp/internal/store"
)

// storageReportFailures is how many failures the Markdown report lists.
const storageReportFailures = 50

// serveStorageReport answers GET api/storage/report?campaign=&format=md|csv
// [&hosts=1]: a campaign as a Markdown report (the finding, a table per
// target, the line, the failures) or its attempts as CSV (fyisp export
// --storage's columns). Target names only; hosts=1 adds each target's host
// and bucket to the Markdown. Never this machine's or its public address.
func (s *server) serveStorageReport(w http.ResponseWriter, r *http.Request) {
	if s.d.Storage == nil || s.d.StorageData == nil || s.d.Campaigns == nil {
		s.noCampaigns(w, r)
		return
	}
	q := r.URL.Query()
	if err := checkParams(q, "campaign", "format", "hosts"); err != nil {
		writeErr(w, r, http.StatusBadRequest, err.Error())
		return
	}
	id, err := reqInt(q, "campaign")
	format := q.Get("format")
	if err != nil || (format != "md" && format != "csv") || (q.Has("hosts") && q.Get("hosts") != "1" && q.Get("hosts") != "0") {
		writeErr(w, r, http.StatusBadRequest, "want campaign=<id>&format=md|csv[&hosts=1]")
		return
	}
	ctx := r.Context()
	c, ok, err := s.d.StorageData.StorageCampaign(ctx, id)
	if err != nil {
		s.storeFailed(w, r, err)
		return
	}
	if !ok {
		notFound(w, r)
		return
	}
	var buf bytes.Buffer
	name := fmt.Sprintf("fyisp-storage-%d", id)
	if format == "csv" { // streamed: a long schedule has many attempts
		w.Header().Set("Content-Disposition", `attachment; filename="`+name+`.csv"`)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		if err := export.WriteStorage(ctx, s.d.StorageData, w, id, time.Time{}, time.Time{}); err != nil {
			s.d.log().Warn("storage tests: writing a report", "err", err) // headers are gone: the CSV ends short
		}
		return
	}
	hosts := map[string]string{}
	if q.Get("hosts") == "1" {
		ts, _ := s.d.Storage.Targets()
		for _, t := range ts {
			hosts[t.ID] = strings.TrimSuffix(t.Host()+"/"+t.Bucket, "/")
		}
	}
	if err := s.storageMarkdown(r, &buf, c, hosts); err != nil {
		s.storeFailed(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeBody(w, r, http.StatusOK, "text/markdown; charset=utf-8", buf.Bytes())
}

// mdCell escapes text for a Markdown table cell or line: backslashes and
// the characters Markdown reads (a name could hold them).
func mdCell(v string) string {
	return strings.NewReplacer(`\`, `\\`, "|", `\|`, "\n", " ", "*", `\*`, "_", `\_`, "`", "\\`", "[", `\[`, "<", "&lt;").Replace(v)
}

func (s *server) storageMarkdown(r *http.Request, b *bytes.Buffer, c model.StorageCampaign, hosts map[string]string) error {
	ctx := r.Context()
	rounds, err := s.d.StorageData.StorageRounds(ctx, c.ID, time.Time{})
	if err != nil {
		return err
	}
	rows, err := s.d.StorageData.StorageCells(ctx, c.ID)
	if err != nil {
		return err
	}
	timings, err := s.d.StorageData.StorageTimings(ctx, c.ID)
	if err != nil {
		return err
	}
	traces, err := s.d.StorageData.StorageTraces(ctx, c.ID)
	if err != nil {
		return err
	}
	fails, total, err := s.d.StorageData.StorageAttempts(ctx, store.StorageAttemptFilter{Campaign: c.ID, Failed: true, Limit: storageReportFailures})
	if err != nil {
		return err
	}
	counts := make([]storage.AttemptCount, len(rows))
	for i, row := range rows {
		counts[i] = storage.AttemptCount{Round: row.Round, Target: row.Target, Phase: row.Phase, Class: row.Class, EdgePrefix: row.EdgePrefix, N: row.N}
	}
	f := storage.Explain(c, rounds, counts, c.Live())
	name := map[string]string{}
	for _, t := range c.Targets {
		name[t.ID] = t.Name
	}
	utc := func(t time.Time) string { return t.UTC().Format("2006-01-02 15:04 UTC") }
	start := c.Start
	if start.IsZero() {
		start = c.Created
	}
	kind := "On demand"
	if c.Shape == model.StorageRecurring {
		var days []string
		for d, n := range []string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"} {
			if c.Days&(1<<d) != 0 {
				days = append(days, n)
			}
		}
		kind = fmt.Sprintf("Recurring, %s %s–%s %s", strings.Join(days, ", "), hhmm(c.FromMin), hhmm(c.ToMin), c.TZ)
	}
	fmt.Fprintf(b, "# fyisp storage test\n\n%s · from %s", kind, utc(start))
	if !c.End.IsZero() && !c.Live() {
		fmt.Fprintf(b, " to %s", utc(c.End))
	}
	fmt.Fprintf(b, " · %d rounds every %s · %s down, %s up\n\n", c.RoundsDone, reportEvery(c.Interval), reportBytes(c.BytesDown), reportBytes(c.BytesUp))
	kicker := map[string]string{"path": "Points to the path", "line": "Points to your line", "provider": "Points to the provider",
		"config": "Configuration problem", "none": "No problems found", "early": "Too early to tell"}[f.Kicker]
	fmt.Fprintf(b, "## Finding\n\n**%s.** %s\n\n", kicker, mdCell(f.Text))
	for _, e := range f.Evidence {
		fmt.Fprintf(b, "- %s\n", mdCell(e))
	}
	if len(f.Evidence) > 0 {
		b.WriteString("\n")
	}
	b.WriteString("## Targets\n\n")
	head := "| Target | Fresh connections failed | Connect p50 / p95 | TLS p50 / p95 | First byte p50 / p95 | Download median | Upload median |\n|---|---|---|---|---|---|---|\n"
	if len(hosts) > 0 {
		head = "| Target | Endpoint | Fresh connections failed | Connect p50 / p95 | TLS p50 / p95 | First byte p50 / p95 | Download median | Upload median |\n|---|---|---|---|---|---|---|---|\n"
	}
	b.WriteString(head)
	ms := func(p storagePctJSON) string {
		if p.P50 == nil {
			return "—"
		}
		return fmt.Sprintf("%s / %s ms", reportMs(*p.P50), reportMs(*p.P95))
	}
	rate := func(x *storageRateJSON) string {
		if x == nil || x.MedianBps == nil {
			return "—"
		}
		return fmt.Sprintf("%s (%d of %d)", reportMbps(*x.MedianBps), x.OK, x.N)
	}
	for _, card := range storageCards(c, timings) {
		cols := []string{mdCell(name[card.Target])}
		if len(hosts) > 0 {
			cols = append(cols, mdCell(hosts[card.Target]))
		}
		cols = append(cols, fmt.Sprintf("%d of %d", card.ColdFailed, card.ColdN), ms(card.Connectms), ms(card.TLSms), ms(card.TTFBms), rate(card.Down), rate(card.Up))
		b.WriteString("| " + strings.Join(cols, " | ") + " |\n")
	}
	var gw, edge, lossy int
	for _, rr := range rounds {
		if rr.GwLoss != nil {
			gw++
		}
		if rr.EdgeLoss != nil {
			edge++
		}
		if (rr.GwLoss != nil && *rr.GwLoss >= storage.LineLoss) || (rr.EdgeLoss != nil && *rr.EdgeLoss >= storage.LineLoss) {
			lossy++
		}
	}
	if gw+edge > 0 {
		fmt.Fprintf(b, "\n## Your line\n\nPacket loss of %d%% or more at your router or your provider's first router during %d of the %d rounds measured.\n", int(storage.LineLoss*100), lossy, max(gw, edge))
	}
	if len(traces) > 0 {
		b.WriteString("\n## Traceroutes\n\n")
		for _, t := range traces {
			var hops []string
			for _, h := range t.Hops {
				switch {
				case h.IP == "" && h.Owner == "":
					hops = append(hops, "*")
				case h.IP == "":
					hops = append(hops, h.Owner) // your network, your provider: no address
				case h.ASN != 0:
					hops = append(hops, fmt.Sprintf("%s (AS%d)", h.IP, h.ASN))
				default:
					hops = append(hops, h.IP)
				}
			}
			changed := ""
			if t.RouteChanged {
				changed = ", the route changed"
			}
			fmt.Fprintf(b, "- %s, %s (%s%s) to %s: %s\n", mdCell(name[t.Target]), utc(t.At), t.Trigger, changed, strings.Replace(t.EdgePrefix, ".0/24", ".x", 1), strings.Join(hops, " → ")+" → the provider")
		}
	}
	fmt.Fprintf(b, "\n## Failures\n\n")
	if total == 0 {
		b.WriteString("None.\n")
	} else {
		if total > len(fails) {
			fmt.Fprintf(b, "The newest %d of %d.\n\n", len(fails), total)
		}
		b.WriteString("| When | Target | Step | What | Provider address | Request ID |\n|---|---|---|---|---|---|\n")
		for _, a := range fails {
			what := a.Class
			if a.HTTPStatus != 0 {
				what += " (" + strings.TrimSpace(strconv.Itoa(a.HTTPStatus)+" "+a.S3Code) + ")"
			}
			if s3test.Class(a.Class).Group() == s3test.GroupConfig {
				what += ", configuration"
			}
			fmt.Fprintf(b, "| %s | %s | %s %d | %s | %s | %s |\n", a.At.UTC().Format("2006-01-02 15:04:05"), mdCell(name[a.Target]), a.Phase, a.N+1,
				mdCell(what), strings.Replace(a.EdgePrefix, ".0/24", ".x", 1), mdCell(a.RequestID))
		}
	}
	b.WriteString("\nTarget names are yours. The provider's addresses are /24s; routers on your network and your provider's are named, not addressed; routers past your provider are shown in full. This machine's and your public address are never included.\n")
	return nil
}

// reportBytes is "64 MiB", "1.5 GiB".
func reportBytes(b int64) string {
	switch {
	case b >= 1<<30:
		return strconv.FormatFloat(float64(b)/(1<<30), 'f', 1, 64) + " GiB"
	case b >= 1<<20:
		return strconv.FormatInt(b>>20, 10) + " MiB"
	case b > 0:
		return strconv.FormatInt(max(1, b>>10), 10) + " KiB"
	}
	return "0 bytes"
}

// reportMs is a duration in ms, with a decimal under 100.
func reportMs(v float64) string {
	if v < 100 {
		return strconv.FormatFloat(v, 'f', 1, 64)
	}
	return strconv.FormatFloat(v, 'f', 0, 64)
}

// reportMbps is a rate in Mbit/s.
func reportMbps(bps float64) string { return mbps(bps/1e6) + " Mbit/s" }

// reportEvery is "5 min", "1 h 30 min".
func reportEvery(d time.Duration) string {
	m := int(d / time.Minute)
	if m < 60 || m%60 != 0 && m < 120 {
		return fmt.Sprintf("%d min", m)
	}
	if m%60 == 0 {
		return fmt.Sprintf("%d h", m/60)
	}
	return fmt.Sprintf("%d h %d min", m/60, m%60)
}
