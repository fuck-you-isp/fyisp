package probe

import (
	"context"
	"math"
	"math/rand/v2"
	"slices"
	"sync/atomic"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

// Fake produces deterministic synthetic samples on the real schedule
// (Interval for HTTPS/TCP/UDP, Interval/3 for ICMP). For UI and store work.
// UDP targets get the three sibling series of a burst (see FakeBurst), and
// UDPState reports "swedencentral" for targets tagged app:teams.
type Fake struct {
	Seed     uint64
	LossRate float64 // e.g. 0.01
	Now      func() time.Time

	udp atomic.Pointer[[]UDPState]
}

// UDPState implements UDPStateSource.
func (f *Fake) UDPState() []UDPState {
	if p := f.udp.Load(); p != nil {
		return slices.Clone(*p)
	}
	return nil
}

// FakeBurst returns synthetic samples of one burst of target at slot: base
// is its typical RTT in ms. Mostly clean, with occasional lossy, jittery
// bursts; lossP is the chance that a request is lost.
func FakeBurst(r *rand.Rand, target string, slot time.Time, base, lossP float64) []model.Sample {
	res := burstResult{sent: BurstSize}
	jit := 0.3 + r.ExpFloat64()*0.4
	if r.Float64() < 0.03 {
		jit *= 15 // a rough patch
	}
	for range BurstSize {
		if r.Float64() < lossP {
			continue
		}
		ms := base*(1+0.05*math.Sin(float64(slot.Unix())/600)) + r.NormFloat64()*jit
		res.rtts = append(res.rtts, time.Duration(max(ms, 0.2)*float64(time.Millisecond)))
	}
	out := burstSamples(target, res, nil)
	for i := range out {
		out[i].Slot = slot
	}
	return out
}

func (f *Fake) Caps() Caps { return Caps{ICMP: "udp", TCP: true, HTTPS: true} }

func (f *Fake) Run(ctx context.Context, p *model.Profile, sink model.Sink) error {
	now := f.Now
	if now == nil {
		now = time.Now
	}
	r := rand.New(rand.NewPCG(f.Seed, 7))
	var udp []UDPState
	for _, tg := range p.Targets {
		if tg.IsUDP() {
			st := UDPState{Target: tg.Name}
			if slices.Contains(tg.Tags, "app:teams") {
				st.Region = "swedencentral"
			}
			udp = append(udp, st)
		}
	}
	f.udp.Store(&udp)
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
				if k == model.KindUDP {
					for _, s := range FakeBurst(r, tg.Name, slot, base, f.LossRate) {
						sink.Observe(s)
					}
					continue
				}
				if k.IsUDP() {
					continue // written by KindUDP's bursts
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
