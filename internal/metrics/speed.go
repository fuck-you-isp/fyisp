package metrics

import (
	"sync"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/speedtest"
	"github.com/prometheus/client_golang/prometheus"
)

var (
	speedDownDesc = prometheus.NewDesc("fyisp_speed_download_bits_per_second",
		"Download speed of the latest speed test against this server that measured it (iperf3 over IPv4, 8 streams, the ramp-up of up to 1 s left out, 3 to 8 s measured). region is the catalog region the test was for (empty outside mode regions).",
		[]string{"region", "server"}, nil)
	speedUpDesc = prometheus.NewDesc("fyisp_speed_upload_bits_per_second",
		"Upload speed of the latest speed test against this server that measured it.",
		[]string{"region", "server"}, nil)
	speedRTTDesc = prometheus.NewDesc("fyisp_speed_loaded_rtt_seconds",
		"Latency under load of the latest speed test against this server: the median RTT to the ISP edge (layer=\"edge\") or the gateway in the 2 minutes before the test (load=\"idle\") and during the download and the upload (probed every 150 ms; a lost probe counts as the 1 s timeout).",
		[]string{"region", "server", "layer", "load"}, nil)
	speedLostDesc = prometheus.NewDesc("fyisp_speed_loaded_probes_lost_ratio",
		"Share of the latency probes during the latest speed test against this server that got no answer, by layer and direction (load=\"download\" or \"upload\").",
		[]string{"region", "server", "layer", "load"}, nil)
	speedTestsDesc = prometheus.NewDesc("fyisp_speed_tests_total",
		"Speed tests since start by server and outcome (ok, busy: the server was running another test, unreachable, failed).",
		[]string{"region", "server", "outcome"}, nil)
)

// Speed exports the speed tests' metrics. Feed it every finished test
// (Observe); register it with Collector.Register.
type Speed struct {
	mu     sync.Mutex
	down   map[[2]string]float64 // region, server
	up     map[[2]string]float64
	rtt    map[[4]string]float64 // region, server, layer, load
	lost   map[[4]string]float64
	counts map[[3]string]uint64 // region, server, outcome
}

// NewSpeed returns an empty Speed.
func NewSpeed() *Speed {
	return &Speed{down: map[[2]string]float64{}, up: map[[2]string]float64{}, rtt: map[[4]string]float64{}, lost: map[[4]string]float64{}, counts: map[[3]string]uint64{}}
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
	for layer, l := range speedtest.ParseExtra(t.Extra).Latency {
		if l.IdleMs != nil {
			s.rtt[[4]string{t.Region, t.Server, layer, "idle"}] = *l.IdleMs / 1000
		}
		for load, d := range map[string]*speedtest.LoadedDir{"download": l.Down, "upload": l.Up} {
			if d != nil && d.Sent > 0 {
				k := [4]string{t.Region, t.Server, layer, load}
				s.rtt[k] = d.P50 / 1000
				s.lost[k] = float64(d.Lost) / float64(d.Sent)
			}
		}
	}
}

// Describe implements prometheus.Collector.
func (s *Speed) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{speedDownDesc, speedUpDesc, speedRTTDesc, speedLostDesc, speedTestsDesc} {
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
	for k, v := range s.lost {
		ch <- prometheus.MustNewConstMetric(speedLostDesc, prometheus.GaugeValue, v, k[0], k[1], k[2], k[3])
	}
	for k, n := range s.counts {
		ch <- prometheus.MustNewConstMetric(speedTestsDesc, prometheus.CounterValue, float64(n), k[0], k[1], k[2])
	}
}
