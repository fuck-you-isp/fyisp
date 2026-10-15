package metrics

import (
	"math"
	"sync"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/prometheus/client_golang/prometheus"
)

var (
	speedDownDesc = prometheus.NewDesc("fyisp_speed_download_bits_per_second",
		"Download speed of the latest speed test against this server that measured it (iperf3 over IPv4, 8 streams, the ramp-up of up to 1 s left out, 3 to 10 s measured). region is the catalog region the test was for (empty outside mode regions).",
		[]string{"region", "server"}, nil)
	speedUpDesc = prometheus.NewDesc("fyisp_speed_upload_bits_per_second",
		"Upload speed of the latest speed test against this server that measured it.",
		[]string{"region", "server"}, nil)
	speedRTTDesc = prometheus.NewDesc("fyisp_speed_loaded_rtt_seconds",
		"Latency under load of the latest speed test against this server: the median RTT to the ISP edge (layer=\"edge\", behind the line's queue) or the gateway (layer=\"gateway\", the home network) in the 2 minutes before the test (load=\"idle\") and during its download and upload (load=\"download\", \"upload\"). Lost probes count as 1 s.",
		[]string{"region", "server", "layer", "load"}, nil)
	speedTestsDesc = prometheus.NewDesc("fyisp_speed_tests_total",
		"Speed tests since start by server and outcome (ok, busy: the server was running another test, unreachable, failed, cancelled).",
		[]string{"region", "server", "outcome"}, nil)
)

// Speed exports the speed tests' metrics. Feed it every finished test
// (Observe); register it with Collector.Register.
type Speed struct {
	mu     sync.Mutex
	down   map[[2]string]float64 // region, server
	up     map[[2]string]float64
	rtt    map[[4]string]float64 // region, server, layer, load
	counts map[[3]string]uint64  // region, server, outcome
}

// NewSpeed returns an empty Speed.
func NewSpeed() *Speed {
	return &Speed{down: map[[2]string]float64{}, up: map[[2]string]float64{}, rtt: map[[4]string]float64{}, counts: map[[3]string]uint64{}}
}

// Observe records a finished test.
func (s *Speed) Observe(t model.SpeedTest) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.counts[[3]string{t.Region, t.Server, t.Outcome}]++
	if t.Server == "" {
		return
	}
	k := [2]string{t.Region, t.Server}
	if t.DownBps != nil {
		s.down[k] = *t.DownBps
	}
	if t.UpBps != nil {
		s.up[k] = *t.UpBps
	}
	if l := t.Latency; l != nil {
		for layer, ph := range map[string]model.LoadPhases{"edge": l.Edge, "gateway": l.Gateway} {
			for load, x := range map[string]*model.LoadStats{"idle": ph.Idle, "download": ph.Down, "upload": ph.Up} {
				if x != nil {
					s.rtt[[4]string{t.Region, t.Server, layer, load}] = math.Round(x.P50Ms*10) / 1e4 // ms to s, 0.1 ms
				}
			}
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
	for k, v := range s.down {
		ch <- prometheus.MustNewConstMetric(speedDownDesc, prometheus.GaugeValue, v, k[0], k[1])
	}
	for k, v := range s.up {
		ch <- prometheus.MustNewConstMetric(speedUpDesc, prometheus.GaugeValue, v, k[0], k[1])
	}
	for k, v := range s.rtt {
		ch <- prometheus.MustNewConstMetric(speedRTTDesc, prometheus.GaugeValue, v, k[0], k[1], k[2], k[3])
	}
	for k, n := range s.counts {
		ch <- prometheus.MustNewConstMetric(speedTestsDesc, prometheus.CounterValue, float64(n), k[0], k[1], k[2])
	}
}
