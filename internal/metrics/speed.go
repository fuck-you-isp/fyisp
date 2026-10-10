package metrics

import (
	"sort"
	"sync"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/prometheus/client_golang/prometheus"
)

var (
	speedDownDesc = prometheus.NewDesc("fyisp_speed_download_bits_per_second",
		"Download speed of the latest speed test against this server that measured it (iperf3, 8 streams, the first second left out).",
		[]string{"server"}, nil)
	speedUpDesc = prometheus.NewDesc("fyisp_speed_upload_bits_per_second",
		"Upload speed of the latest speed test against this server that measured it.",
		[]string{"server"}, nil)
	speedRTTDesc = prometheus.NewDesc("fyisp_speed_loaded_rtt_seconds",
		"Latency under load of the latest speed test against this server: the median RTT to the gateway or the call path in the 2 minutes before the test (load=\"idle\") and during it (load=\"loaded\").",
		[]string{"server", "layer", "load"}, nil)
	speedTestsDesc = prometheus.NewDesc("fyisp_speed_tests_total",
		"Speed tests since start by server and outcome (ok, busy: the server was running another test, unreachable, failed).",
		[]string{"server", "outcome"}, nil)
)

// Speed exports the speed tests' metrics. Feed it every finished test
// (Observe); register it with Collector.Register.
type Speed struct {
	mu     sync.Mutex
	down   map[string]float64
	up     map[string]float64
	rtt    map[[3]string]float64
	counts map[[2]string]uint64
}

// NewSpeed returns an empty Speed.
func NewSpeed() *Speed {
	return &Speed{down: map[string]float64{}, up: map[string]float64{}, rtt: map[[3]string]float64{}, counts: map[[2]string]uint64{}}
}

// Observe records a finished test.
func (s *Speed) Observe(t model.SpeedTest) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.counts[[2]string{t.Server, t.Outcome}]++
	if t.Server == "" {
		return
	}
	if t.DownBps != nil {
		s.down[t.Server] = *t.DownBps
	}
	if t.UpBps != nil {
		s.up[t.Server] = *t.UpBps
	}
	for _, x := range []struct {
		layer, load string
		ms          *float64
	}{
		{"gateway", "idle", t.IdleGatewayMs}, {"gateway", "loaded", t.LoadedGatewayMs},
		{"call", "idle", t.IdleCallMs}, {"call", "loaded", t.LoadedCallMs},
	} {
		if x.ms != nil {
			s.rtt[[3]string{t.Server, x.layer, x.load}] = *x.ms / 1000
		}
	}
}

// Describe implements prometheus.Collector.
func (s *Speed) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{speedDownDesc, speedUpDesc, speedRTTDesc, speedTestsDesc} {
		ch <- d
	}
}

// Collect implements prometheus.Collector.
func (s *Speed) Collect(ch chan<- prometheus.Metric) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, srv := range sortedKeys(s.down) {
		ch <- prometheus.MustNewConstMetric(speedDownDesc, prometheus.GaugeValue, s.down[srv], srv)
	}
	for _, srv := range sortedKeys(s.up) {
		ch <- prometheus.MustNewConstMetric(speedUpDesc, prometheus.GaugeValue, s.up[srv], srv)
	}
	for k, v := range s.rtt {
		ch <- prometheus.MustNewConstMetric(speedRTTDesc, prometheus.GaugeValue, v, k[0], k[1], k[2])
	}
	for k, n := range s.counts {
		ch <- prometheus.MustNewConstMetric(speedTestsDesc, prometheus.CounterValue, float64(n), k[0], k[1])
	}
}

func sortedKeys(m map[string]float64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
