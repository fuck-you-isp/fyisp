// Package synth generates deterministic synthetic RTT series for storage
// tests and benchmarks: a log-uniform 5–150 ms baseline, log-normal jitter,
// rare spikes, 0.5% loss with a reason code, and occasional unmeasured gaps.
package synth

import (
	"math"
	"math/rand/v2"

	"github.com/fuck-you-isp/fyisp/internal/store/blob"
)

// Series produces one series' slots.
type Series struct {
	r        *rand.Rand
	base, sd float64 // seconds
	gapLeft  int
}

// New returns series number id of the stream seeded by seed.
func New(seed uint64, id int) *Series {
	r := rand.New(rand.NewPCG(seed, uint64(id)*7919+1))
	base := 0.005 * math.Exp(r.Float64()*math.Log(30))
	return &Series{r: r, base: base, sd: math.Max(0.0005, math.Min(0.003, 0.03*base))}
}

// Next returns the next slot.
func (s *Series) Next() blob.Slot {
	r := s.r
	if s.gapLeft > 0 {
		s.gapLeft--
		return blob.Gap()
	}
	if r.Float64() < 0.0005 { // restart / sleep
		s.gapLeft = 1 + r.IntN(20)
		return blob.Gap()
	}
	if r.Float64() < 0.005 {
		if r.Float64() < 0.8 {
			return blob.Lost(1) // timeout
		}
		return blob.Lost(uint8(2 + r.IntN(blob.MaxCode-1)))
	}
	v := s.base + s.sd*math.Exp(0.7*r.NormFloat64())
	if r.Float64() < 0.002 {
		v *= 3 + 17*r.Float64()
	}
	return blob.Val(int64(math.Round(v * 1e9 / blob.Unit)))
}

// Fill appends n slots to dst.
func (s *Series) Fill(dst []blob.Slot, n int) []blob.Slot {
	for range n {
		dst = append(dst, s.Next())
	}
	return dst
}
