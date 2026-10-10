package main

import (
	"context"
	"math/rand/v2"
	"sort"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/web"
)

// Call-path scenarios for the Calls view (-calls):
//
//   - good: the call path is clean now; a 7-minute poor stretch 35 minutes
//     ago (8-12% loss, jittery), during an ISP incident, so "What happened
//     at …?" and the working-hours strip have something to show;
//   - poor: the same, and the call path has been lossy and jittery for the
//     last 10 minutes and stays so live (the top card says Poor);
//   - blocked: the same, and for the last 10 minutes and live every STUN
//     request times out while the verdict is udp_blocked.
const (
	callsGood    = "good"
	callsPoor    = "poor"
	callsBlocked = "blocked"
)

// callsPast is the scripted poor stretch (relative to start).
func callsPast(start time.Time) window {
	return window{start.Add(-35 * time.Minute), start.Add(-28 * time.Minute)}
}

// callsSink rewrites the UDP sibling samples for the scenario: in the past
// poor stretch, and from 10 minutes before start on for poor and blocked.
func callsSink(inner model.Sink, mode string, start time.Time) model.Sink {
	past := callsPast(start)
	r := rand.New(rand.NewPCG(3, 4))
	return model.SinkFunc(func(s model.Sample) {
		k := s.Key.Kind
		if !k.IsUDP() {
			inner.Observe(s)
			return
		}
		recent := !s.Slot.Before(start.Add(-10 * time.Minute))
		switch {
		case mode == callsBlocked && recent:
			switch k {
			case model.KindUDP:
				s.Lost, s.Reason, s.RTT = true, model.ReasonTimeout, 0
			case model.KindUDPJitter:
				s.Lost, s.Reason, s.RTT = true, model.ReasonGap, 0
			case model.KindUDPLoss:
				s.Lost, s.RTT = false, 100*time.Millisecond
			}
		case past.has(s.Slot) || (mode == callsPoor && recent):
			if s.Lost {
				break
			}
			switch k {
			case model.KindUDP:
				s.RTT += time.Duration(r.IntN(25)) * time.Millisecond
			case model.KindUDPJitter:
				s.RTT = s.RTT*4 + time.Duration(8+r.IntN(20))*time.Millisecond
			case model.KindUDPLoss:
				// One of five requests lost in most bursts: ~11%.
				if r.Float64() < 0.55 {
					s.RTT = 20 * time.Millisecond
				}
			}
		}
		inner.Observe(s)
	})
}

// callsVerdict adds the scenario to a verdict source: an ISP incident over
// the past poor stretch and a current udp_degraded (poor) or udp_blocked
// (blocked) verdict, as the engine would give.
type callsVerdict struct {
	web.VerdictSource
	mode  string
	start time.Time
}

func (c callsVerdict) Current() model.Verdict {
	if c.mode == callsPoor {
		return model.Verdict{Kind: model.VerdictUDPDegraded, Since: c.start.Add(-10 * time.Minute),
			Summary:  "Calls may sound choppy: the UDP path to 3 call services shows up to 20% packet loss and up to 45 ms of jitter, while the rest of your connection looks fine.",
			Targets:  []string{"Meet-STUN", "Cloudflare-STUN", "Twilio-STUN-global"},
			Evidence: map[string]float64{"udp_providers": 3, "udp_poor_providers": 3, "udp_loss": 0.2, "udp_jitter_ms": 45, "loss": 0.2}}
	}
	if c.mode == callsBlocked {
		return model.Verdict{Kind: model.VerdictUDPBlocked, Since: c.start.Add(-10 * time.Minute),
			Summary:  "UDP is blocked or failing on this network (or a firewall on this computer is blocking fyisp): calls will fall back to slower TCP relays.",
			Targets:  []string{"Meet-STUN", "Cloudflare-STUN", "Twilio-STUN-global"},
			Evidence: map[string]float64{"udp_providers": 3, "udp_blocked_providers": 3, "udp53_ok": 0}}
	}
	return c.VerdictSource.Current()
}

func (c callsVerdict) Incidents(ctx context.Context, from, to time.Time) ([]model.Incident, error) {
	list, err := c.VerdictSource.Incidents(ctx, from, to)
	if err != nil {
		return nil, err
	}
	past := callsPast(c.start)
	if past.from.Before(to) && past.to.After(from) {
		list = append(list, model.Incident{ID: 900, Start: past.from.Add(-time.Minute), End: past.to, Kind: model.VerdictISP, PeakLoss: 0.11,
			Summary: "Your ISP dropped 11% of packets while your router was fine."})
	}
	switch c.mode {
	case callsBlocked:
		list = append(list, model.Incident{ID: 901, Start: c.start.Add(-10 * time.Minute), Kind: model.VerdictUDPBlocked, PeakLoss: 1,
			Summary: "UDP was blocked on this network: calls fell back to slower relays."})
	case callsPoor:
		list = append(list, model.Incident{ID: 902, Start: c.start.Add(-10 * time.Minute), Kind: model.VerdictUDPDegraded, PeakLoss: 0.2,
			Summary: "The UDP path to call services was losing packets and jittery."})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Start.After(list[j].Start) })
	return list, nil
}
