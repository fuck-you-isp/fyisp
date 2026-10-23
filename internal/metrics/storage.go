package metrics

import (
	"sync"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/prometheus/client_golang/prometheus"
)

var (
	storageAttemptsDesc = prometheus.NewDesc("fyisp_storage_attempts_total",
		"Storage test attempts since start by target (its name), phase (cold: a fresh connection, warm: a reused one, down, up, seed) and class (ok, or how it failed; see docs/STORAGE-API.md).",
		[]string{"target", "phase", "class"}, nil)
	storageRoundsDesc = prometheus.NewDesc("fyisp_storage_rounds_total",
		"Storage test rounds since start by outcome (ok, slow, failed, config, cancelled, skipped, gap).",
		[]string{"outcome"}, nil)
	storageBytesDesc = prometheus.NewDesc("fyisp_storage_bytes_total",
		"Payload bytes storage test rounds moved since start, by direction (down, up).",
		[]string{"direction"}, nil)
	storageGoodputDesc = prometheus.NewDesc("fyisp_storage_goodput_bits_per_second",
		"Payload speed of the latest storage transfer that worked, by target and direction.",
		[]string{"target", "direction"}, nil)
)

// Storage exports the storage tests' metrics. Feed it every stored round
// (Observe); register it with Collector.Register.
type Storage struct {
	mu       sync.Mutex
	attempts map[[3]string]uint64
	rounds   map[string]uint64
	bytes    map[string]uint64
	goodput  map[[2]string]float64
}

// NewStorage returns an empty Storage.
func NewStorage() *Storage {
	return &Storage{attempts: map[[3]string]uint64{}, rounds: map[string]uint64{}, bytes: map[string]uint64{}, goodput: map[[2]string]float64{}}
}

// Observe records a round (a run, a skip, a gap) of campaign c with its
// attempts.
func (s *Storage) Observe(c model.StorageCampaign, r model.StorageRound, as []model.StorageAttempt) {
	name := map[string]string{}
	for _, t := range c.Targets {
		name[t.ID] = t.Name
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rounds[r.Outcome]++
	s.bytes["down"] += uint64(r.BytesDown)
	s.bytes["up"] += uint64(r.BytesUp)
	for _, a := range as {
		n := name[a.Target]
		s.attempts[[3]string{n, a.Phase, a.Class}]++
		if a.Class == "ok" && a.GoodputBps != nil && (a.Phase == model.StoragePhaseDown || a.Phase == model.StoragePhaseUp) {
			s.goodput[[2]string{n, a.Phase}] = *a.GoodputBps
		}
	}
}

// Describe implements prometheus.Collector.
func (s *Storage) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{storageAttemptsDesc, storageRoundsDesc, storageBytesDesc, storageGoodputDesc} {
		ch <- d
	}
}

// Collect implements prometheus.Collector.
func (s *Storage) Collect(ch chan<- prometheus.Metric) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range s.attempts {
		ch <- prometheus.MustNewConstMetric(storageAttemptsDesc, prometheus.CounterValue, float64(v), k[0], k[1], k[2])
	}
	for k, v := range s.rounds {
		ch <- prometheus.MustNewConstMetric(storageRoundsDesc, prometheus.CounterValue, float64(v), k)
	}
	for k, v := range s.bytes {
		ch <- prometheus.MustNewConstMetric(storageBytesDesc, prometheus.CounterValue, float64(v), k)
	}
	for k, v := range s.goodput {
		ch <- prometheus.MustNewConstMetric(storageGoodputDesc, prometheus.GaugeValue, v, k[0], k[1])
	}
}
