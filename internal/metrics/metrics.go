// Package metrics serves fyisp's samples in the Prometheus text format on the
// local listener's /metrics.
//
// ICMP samples are exposed with the same metric names, help strings, types and
// label names as network_exporter (github.com/syepes/network_exporter), which
// the legacy docker-compose stack scraped, so existing dashboards and alerts
// keep working. network_exporter was configured with one packet per probe, so
// every ping_* value describes the most recent probe only. Differences:
//
//   - label target is the hostname fyisp pings (the legacy stack substituted
//     the IP resolved once at startup);
//   - label target_ip is always "" (model.Sample does not carry the address
//     the probe used);
//   - only targets of the current profile that have produced a sample are
//     exported; ping_targets counts every ICMP target in the profile;
//   - ping_rtt_snt_seconds is the running sum of successful RTTs (upstream
//     reports 0 with one packet per probe), and ping_rtt_seconds type="csd"
//     is 0 (upstream reports garbage for a single packet).
//
// HTTPS and TCP results are exported as fyisp_https_rtt_seconds and
// fyisp_tcp_rtt_seconds, with loss and sample counters for every kind.
//
// The Collector uses a private registry: cloudflared registers collectors
// into (and fyisp's tunnel replaces) prometheus.DefaultRegisterer, so the
// global registry is never used here.
package metrics

import (
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// network_exporter compatible descriptors (collector_ping.go upstream).
var (
	icmpLabels = []string{"name", "target", "target_ip"}

	pingUpDesc      = prometheus.NewDesc("ping_up", "Exporter state", nil, nil)
	pingTargetsDesc = prometheus.NewDesc("ping_targets", "Number of active targets", nil, nil)
	pingStatusDesc  = prometheus.NewDesc("ping_status", "Ping Status", icmpLabels, nil)
	pingRTTDesc     = prometheus.NewDesc("ping_rtt_seconds", "Round Trip Time in seconds", append(append([]string{}, icmpLabels...), "type"), nil)
	pingSntDesc     = prometheus.NewDesc("ping_rtt_snt_count", "Packet sent count", icmpLabels, nil)
	pingSntFailDesc = prometheus.NewDesc("ping_rtt_snt_fail_count", "Packet sent fail count", icmpLabels, nil)
	pingSntTimeDesc = prometheus.NewDesc("ping_rtt_snt_seconds", "Packet sent time total", icmpLabels, nil)
	pingLossDesc    = prometheus.NewDesc("ping_loss_percent", "Packet loss in percent", icmpLabels, nil)
)

// fyisp's own series.
var (
	httpsRTTDesc = prometheus.NewDesc("fyisp_https_rtt_seconds",
		"Latest HTTPS round trip (request written to first response byte) in seconds; absent while the latest probe failed.",
		[]string{"name", "target"}, nil)
	tcpRTTDesc = prometheus.NewDesc("fyisp_tcp_rtt_seconds",
		"Latest TCP connect time in seconds; absent while the latest probe failed.",
		[]string{"name", "target"}, nil)
	samplesDesc = prometheus.NewDesc("fyisp_probe_samples_total",
		"Probes completed (successful or lost).",
		[]string{"name", "kind"}, nil)
	lostDesc = prometheus.NewDesc("fyisp_probe_lost_total",
		"Probes lost, by failure reason.",
		[]string{"name", "kind", "reason"}, nil)
)

// Verdict series (set with SetVerdictSource).
var (
	verdictDesc = prometheus.NewDesc("fyisp_verdict",
		"Current verdict: 1 for the current kind, 0 for the others.",
		[]string{"kind"}, nil)
	layerDesc = prometheus.NewDesc("fyisp_layer_healthy",
		"Whether a path layer (gateway, isp-edge, anycast) looks healthy over the last minute; absent while unknown.",
		[]string{"layer"}, nil)
)

// verdictKinds are every fyisp_verdict kind label, in a stable order.
var verdictKinds = []model.VerdictKind{
	model.VerdictOK, model.VerdictWarmingUp, model.VerdictLAN, model.VerdictISP,
	model.VerdictUpstream, model.VerdictDNS, model.VerdictService, model.VerdictNoNetwork,
}

// VerdictSource returns the current verdict and the health of each path
// layer (nil: unknown). See verdict.MetricsSource.
type VerdictSource func() (model.Verdict, map[string]*bool)

// rttTypes are the ping_rtt_seconds "type" label values, in upstream order.
var rttTypes = []string{"best", "worst", "mean", "sum", "sd", "usd", "csd", "range"}

type series struct {
	last     model.Sample
	sent     uint64
	failed   uint64
	rttTotal time.Duration // sum of successful RTTs
	lost     map[model.Reason]uint64
}

// Collector keeps the latest sample and running counters per series. It
// implements model.Sink; Observe never blocks on I/O.
type Collector struct {
	profile func() *model.Profile

	mu      sync.Mutex
	series  map[model.SeriesKey]*series
	verdict VerdictSource

	handler http.Handler
}

var _ model.Sink = (*Collector)(nil)

// New returns a Collector. p returns the current profile (it may change on
// reload); it is called on every scrape and may return nil.
func New(p func() *model.Profile) *Collector {
	c := &Collector{profile: p, series: map[model.SeriesKey]*series{}}
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		(*promCollector)(c),
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	c.handler = promhttp.HandlerFor(reg, promhttp.HandlerOpts{
		ErrorHandling:       promhttp.ContinueOnError,
		MaxRequestsInFlight: 4,
		Timeout:             10 * time.Second,
	})
	return c
}

// Handler serves the metrics in the Prometheus exposition format.
func (c *Collector) Handler() http.Handler { return c.handler }

// SetVerdictSource makes scrapes export fyisp_verdict and
// fyisp_layer_healthy from f (called once per scrape; nil disables them).
func (c *Collector) SetVerdictSource(f VerdictSource) {
	c.mu.Lock()
	c.verdict = f
	c.mu.Unlock()
}

// Observe records a sample. "Not measured" gaps are ignored.
func (c *Collector) Observe(s model.Sample) {
	if s.Lost && s.Reason == model.ReasonGap {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	st := c.series[s.Key]
	if st == nil {
		st = &series{lost: map[model.Reason]uint64{}}
		c.series[s.Key] = st
	}
	st.last = s
	st.sent++
	if s.Lost {
		st.failed++
		st.lost[s.Reason]++
	} else {
		st.rttTotal += s.RTT
	}
}

// promCollector is the prometheus.Collector view of a Collector (kept off the
// exported API).
type promCollector Collector

func (pc *promCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{
		pingUpDesc, pingTargetsDesc, pingStatusDesc, pingRTTDesc, pingSntDesc,
		pingSntFailDesc, pingSntTimeDesc, pingLossDesc,
		httpsRTTDesc, tcpRTTDesc, samplesDesc, lostDesc, verdictDesc, layerDesc,
	} {
		ch <- d
	}
}

type snap struct {
	key      model.SeriesKey
	host     string
	last     model.Sample
	sent     uint64
	failed   uint64
	rttTotal time.Duration
	lost     map[model.Reason]uint64
}

func (pc *promCollector) Collect(ch chan<- prometheus.Metric) {
	c := (*Collector)(pc)
	var p *model.Profile
	if c.profile != nil {
		p = c.profile()
	}
	targets := map[string]model.Target{}
	icmpTargets := 0
	if p != nil {
		for _, t := range p.Targets {
			targets[t.Name] = t
			if hasKind(t, model.KindICMP) {
				icmpTargets++
			}
		}
	}

	// Copy under the lock, emit without it.
	c.mu.Lock()
	snaps := make([]snap, 0, len(c.series))
	for k, st := range c.series {
		t, ok := targets[k.Target]
		if !ok {
			continue // not in the current profile (removed on reload)
		}
		lost := make(map[model.Reason]uint64, len(st.lost))
		for r, n := range st.lost {
			lost[r] = n
		}
		snaps = append(snaps, snap{k, t.HostFor(k.Kind), st.last, st.sent, st.failed, st.rttTotal, lost})
	}
	vsrc := c.verdict
	c.mu.Unlock()
	sort.Slice(snaps, func(i, j int) bool {
		if snaps[i].key.Target != snaps[j].key.Target {
			return snaps[i].key.Target < snaps[j].key.Target
		}
		return snaps[i].key.Kind < snaps[j].key.Kind
	})

	gauge := func(d *prometheus.Desc, v float64, labels ...string) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.GaugeValue, v, labels...)
	}
	counter := func(d *prometheus.Desc, v float64, labels ...string) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.CounterValue, v, labels...)
	}

	gauge(pingUpDesc, 1)
	gauge(pingTargetsDesc, float64(icmpTargets))

	if vsrc != nil {
		v, layers := vsrc()
		for _, k := range verdictKinds {
			gauge(verdictDesc, b2f(v.Kind == k), string(k))
		}
		names := make([]string, 0, len(layers))
		for l := range layers {
			names = append(names, l)
		}
		sort.Strings(names)
		for _, l := range names {
			if h := layers[l]; h != nil {
				gauge(layerDesc, b2f(*h), l)
			}
		}
	}

	for _, s := range snaps {
		name, kind := s.key.Target, s.key.Kind.String()
		counter(samplesDesc, float64(s.sent), name, kind)
		reasons := make([]model.Reason, 0, len(s.lost))
		for r := range s.lost {
			reasons = append(reasons, r)
		}
		sort.Slice(reasons, func(i, j int) bool { return reasons[i] < reasons[j] })
		for _, r := range reasons {
			counter(lostDesc, float64(s.lost[r]), name, kind, reasonLabel(r))
		}

		switch s.key.Kind {
		case model.KindHTTPS:
			if !s.last.Lost {
				gauge(httpsRTTDesc, s.last.RTT.Seconds(), name, s.host)
			}
		case model.KindTCP:
			if !s.last.Lost {
				gauge(tcpRTTDesc, s.last.RTT.Seconds(), name, s.host)
			}
		case model.KindICMP:
			ls := []string{name, s.host, ""} // target_ip unknown, see package doc
			status, loss, rtt := 1.0, 0.0, s.last.RTT.Seconds()
			if s.last.Lost {
				status, loss, rtt = 0, 1, 0
			}
			gauge(pingStatusDesc, status, ls...)
			gauge(pingLossDesc, loss, ls...)
			// One packet per probe: best = worst = mean = sum; no spread.
			for _, typ := range rttTypes {
				v := rtt
				switch typ {
				case "sd", "usd", "csd", "range":
					v = 0
				}
				gauge(pingRTTDesc, v, append(ls, typ)...)
			}
			gauge(pingSntDesc, float64(s.sent), ls...)
			gauge(pingSntFailDesc, float64(s.failed), ls...)
			gauge(pingSntTimeDesc, s.rttTotal.Seconds(), ls...)
		}
	}
}

func hasKind(t model.Target, k model.ProbeKind) bool {
	if len(t.Kinds) == 0 {
		return true // default: all kinds
	}
	for _, x := range t.Kinds {
		if x == k {
			return true
		}
	}
	return false
}

// reasonLabel is a Prometheus-friendly spelling of r ("no network" -> "no_network").
func reasonLabel(r model.Reason) string {
	return strings.ReplaceAll(r.String(), " ", "_")
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
