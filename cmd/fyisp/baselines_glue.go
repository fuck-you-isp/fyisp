package main

import "github.com/fuck-you-isp/fyisp/internal/model"

// probeKeys lists the TCP/HTTPS/ICMP series of the profile (not trace
// hops), for baselines.
func probeKeys(p *model.Profile) []model.SeriesKey {
	var out []model.SeriesKey
	for _, t := range p.Targets {
		for _, k := range t.ProbeKinds() {
			out = append(out, model.SeriesKey{Target: t.Name, Kind: k})
		}
	}
	return out
}
