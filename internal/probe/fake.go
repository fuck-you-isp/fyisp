package probe

import (
	"context"
	"math"
	"math/rand/v2"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

// Fake produces deterministic synthetic samples on the real schedule
// (Interval for HTTPS/TCP, Interval/3 for ICMP). For UI and store work.
type Fake struct {
	Seed     uint64
	LossRate float64 // e.g. 0.01
	Now      func() time.Time
}

func (f *Fake) Caps() Caps { return Caps{ICMP: "udp", TCP: true, HTTPS: true} }

func (f *Fake) Run(ctx context.Context, p *model.Profile, sink model.Sink) error {
	now := f.Now
	if now == nil {
		now = time.Now
	}
	r := rand.New(rand.NewPCG(f.Seed, 7))
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
		n := now().UTC()
		for i, tg := range p.Targets {
			iv := tg.Interval
			if iv == 0 {
				iv = 15 * time.Second
			}
			base := 5 + float64(i%30)*5 // ms
			for _, k := range tg.ProbeKinds() {
				kiv := iv
				if k == model.KindICMP {
					kiv = iv / 3
				}
				slot := n.Truncate(kiv)
				if !n.Equal(slot) && n.Sub(slot) >= time.Second {
					continue
				}
				s := model.Sample{Key: model.SeriesKey{Target: tg.Name, Kind: k}, Slot: slot}
				if r.Float64() < f.LossRate {
					s.Lost, s.Reason = true, model.Reason(1+r.IntN(8))
				} else {
					ms := base*(1+0.05*math.Sin(float64(n.Unix())/600)) + r.ExpFloat64()*0.8
					s.RTT = time.Duration(ms * float64(time.Millisecond))
				}
				sink.Observe(s)
			}
		}
	}
}
