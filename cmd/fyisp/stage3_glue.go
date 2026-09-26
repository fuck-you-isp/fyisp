package main

import (
	"context"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/report"
	"github.com/fuck-you-isp/fyisp/internal/store"
	"github.com/fuck-you-isp/fyisp/internal/web"
)

// webReports adapts report.Builder + store.ReportStore to web.ReportSource.
type webReports struct {
	b report.Builder
	store.ReportStore
}

func (w webReports) Build(ctx context.Context, o web.ReportOptions) ([]byte, error) {
	return w.b.Build(ctx, report.Options{From: o.From, To: o.To, Title: o.Title, Redact: o.Redact})
}

var _ web.ReportSource = webReports{}

// probeKeys lists the HTTPS/TCP/ICMP series of the profile (not trace
// hops), for baselines.
func probeKeys(p *model.Profile) []model.SeriesKey {
	var out []model.SeriesKey
	for _, t := range p.Targets {
		kinds := t.Kinds
		if len(kinds) == 0 {
			kinds = []model.ProbeKind{model.KindHTTPS, model.KindTCP, model.KindICMP}
		}
		for _, k := range kinds {
			out = append(out, model.SeriesKey{Target: t.Name, Kind: k})
		}
	}
	return out
}
