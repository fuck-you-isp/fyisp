package verdict

import (
	"testing"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

// TestInnermostLossyLayer: loss on an inner link shows on every layer
// beyond it; the verdict blames the innermost lossy layer even when an
// outer layer crossed a threshold first by chance, and never blames a
// layer on a handful of lost samples.
//
// Counts are per one-minute window as in fyisp: the gateway and the ISP
// edge are pinged every second (60 samples), each anycast target gets 12
// ICMP and 4 TCP probes (16), each internet target 50. The "lab" cases are
// the evidence observed in the netns lab (test/netns TestVerdict) with
// netem 40% loss between client and gateway (lan) and between gateway and
// ISP (isp), and with the anycast resolvers dropped (upstream).
func TestInnermostLossyLayer(t *testing.T) {
	const (
		unknown = -1
		layerN  = 60
		acN     = 16
		inetN   = 50
	)
	cases := []struct {
		name     string
		gw, edge int    // lost of layerN; unknown = no samples
		ac       [3]int // lost of acN per anycast target
		inet     int    // lost of inetN per internet target
		kind     model.VerdictKind
	}{
		// lab, lan entered: gateway 0.237, edge 0.169, anycast 0.244
		{"lan: lab evidence at entry", 14, 10, [3]int{4, 4, 4}, 10, model.VerdictLAN},
		// lab run 1, the evaluation before: anycast 0.238 crossed 20% before
		// the gateway (the first rules said upstream here)
		{"lan: anycast crossed 20% before the gateway", 10, 9, [3]int{4, 4, 3}, 9, model.VerdictLAN},
		// lab run 2, 20s into the lan fault: gateway 0.067 (4 lost), edge
		// 0.169 (10 lost), anycast 0.104. The edge is not significantly
		// worse than the gateway (z = 1.7): not isp (the percentage rule
		// said isp here); the verdict waits, and says lan once the gateway
		// has lost enough.
		{"lab run 2: edge ahead of the gateway by chance", 4, 10, [3]int{2, 2, 1}, 5, model.VerdictOK},
		{"lan once the gateway lost 6", 6, 10, [3]int{2, 2, 1}, 5, model.VerdictLAN},
		{"lan: edge unknown", 7, unknown, [3]int{5, 5, 4}, 5, model.VerdictLAN},
		// lab, isp entered: gateway 0, edge 0.267, anycast 0.238
		{"isp: lab evidence at entry", 0, 16, [3]int{4, 4, 3}, 12, model.VerdictISP},
		// lab run 2, the evaluation before: anycast 0.289, edge 0.15
		{"isp: anycast crossed 20% before the edge", 0, 9, [3]int{5, 5, 4}, 10, model.VerdictISP},
		{"isp: early, edge 7 lost", 0, 7, [3]int{2, 1, 1}, 5, model.VerdictISP},
		{"isp with background loss on the gateway", 2, 30, [3]int{8, 8, 7}, 25, model.VerdictISP},
		{"not isp: edge only 4 lost", 0, 4, [3]int{1, 1, 1}, 2, model.VerdictOK},
		{"not isp: edge not worse than the gateway", 4, 9, [3]int{2, 2, 2}, 5, model.VerdictOK},
		// lab, upstream: gateway 0, edge 0, anycast 0.65
		{"upstream: lab evidence", 0, 0, [3]int{10, 11, 10}, 0, model.VerdictUpstream},
		{"upstream: edge with a little loss", 0, 2, [3]int{5, 5, 4}, 0, model.VerdictUpstream},
		{"upstream: edge unknown, gateway fine", 0, unknown, [3]int{8, 8, 8}, 0, model.VerdictUpstream},
		{"upstream: no inner layer known", unknown, unknown, [3]int{8, 8, 8}, 0, model.VerdictUpstream},
		// gateway 4/60 against anycast 11/48: not significant (z = 2.4)
		{"not upstream while the gateway is lossy", 4, 4, [3]int{4, 4, 3}, 5, model.VerdictOK},
		// an ICMP-rate-limiting router: the loss does not propagate
		{"gateway loss that stays there is not lan", 9, 0, [3]int{0, 0, 0}, 0, model.VerdictOK},
		{"edge loss that stays there is not isp", 0, 9, [3]int{0, 1, 0}, 0, model.VerdictOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := &model.Profile{Groups: []model.Group{{ID: "path"}, {ID: "a"}, {ID: "b"}}}
			var ts []*tstat
			add := func(name, layer, group string, n, lost int) {
				tg := model.Target{Name: name, Group: group, Layer: layer, Host: "h-" + name}
				p.Targets = append(p.Targets, tg)
				st := &tstat{t: tg}
				if lost != unknown {
					st.n, st.lost = n, lost
				}
				ts = append(ts, st)
			}
			add("Gateway", model.LayerGateway, "path", layerN, c.gw)
			add("Edge", model.LayerEdge, "path", layerN, c.edge)
			for i, n := range []string{"ac1", "ac2", "ac3"} {
				add(n, model.LayerAnycast, "path", acN, c.ac[i])
			}
			for i, n := range []string{"s1", "s2", "s3", "s4"} {
				add(n, "", []string{"a", "b"}[i%2], inetN, c.inet)
			}
			j := judge(p, ts)
			if j.v.Kind != c.kind {
				t.Errorf("kind %s (%q), want %s; evidence %v", j.v.Kind, j.v.Summary, c.kind, j.v.Evidence)
			}
		})
	}
}
