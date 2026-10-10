package metrics

import (
	"math"
	"sort"
	"sync"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/callq"
	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/prometheus/client_golang/prometheus"
)

// CallMOSWindow is the window fyisp_call_mos covers.
const CallMOSWindow = 5 * time.Minute

var callMOSDesc = prometheus.NewDesc("fyisp_call_mos",
	"Call quality (mean opinion score, 1 to 4.5) of the worst minute over the last 5 minutes, as the dashboard's Calls view scores it: app=\"path\" is your call path (the STUN servers tagged udp-path), the others the apps measured over their own STUN servers (meet, teams). 4.0 and up is good, 3.6 fair, 3.1 poor, below unusable. Absent with fewer than 10 bursts.",
	[]string{"app"}, nil)

// CallMOS exports fyisp_call_mos{app}. It is a model.Sink (feed it every
// sample; only the UDP burst kinds are kept, for CallMOSWindow) and a
// prometheus.Collector.
type CallMOS struct {
	profile func() *model.Profile
	now     func() time.Time

	mu     sync.Mutex
	bursts map[string]map[int64]*callBurst // target -> slot (unix ms)
	latest time.Time                       // the newest slot seen
	pruned time.Time
}

// callBurst is what the three sibling samples of one burst said.
type callBurst struct {
	rtt, jitter, loss float64 // ms, ms, percent; NaN while not (yet) seen
	answered, timeout bool
	unknown           bool // a socket or DNS failure: not loss
}

// NewCallMOS returns a CallMOS over the current profile (p may return nil).
func NewCallMOS(p func() *model.Profile) *CallMOS {
	return &CallMOS{profile: p, now: time.Now, bursts: map[string]map[int64]*callBurst{}}
}

// Observe implements model.Sink.
func (c *CallMOS) Observe(s model.Sample) {
	if !s.Key.Kind.IsUDP() || (s.Lost && s.Reason == model.ReasonGap) {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	// Prune by the samples' own clock: the newest slot seen.
	if s.Slot.After(c.latest) {
		c.latest = s.Slot
		if c.latest.Sub(c.pruned) > 30*time.Second {
			c.prune(c.latest)
		}
	}
	m := c.bursts[s.Key.Target]
	if m == nil {
		m = map[int64]*callBurst{}
		c.bursts[s.Key.Target] = m
	}
	slot := s.Slot.UnixMilli()
	b := m[slot]
	if b == nil {
		b = &callBurst{rtt: math.NaN(), jitter: math.NaN(), loss: math.NaN()}
		m[slot] = b
	}
	ms := float64(s.RTT) / float64(time.Millisecond)
	switch {
	case s.Lost && s.Reason != model.ReasonTimeout:
		b.unknown = true
	case s.Key.Kind == model.KindUDP && s.Lost:
		b.timeout = true
	case s.Key.Kind == model.KindUDP:
		b.answered, b.rtt = true, ms
	case s.Key.Kind == model.KindUDPJitter:
		b.jitter = ms
	case s.Key.Kind == model.KindUDPLoss:
		b.loss = ms
	}
}

// prune drops bursts older than the window (and a minute of slack) before
// now.
func (c *CallMOS) prune(now time.Time) {
	c.pruned = now
	cut := now.Add(-CallMOSWindow - time.Minute).UnixMilli()
	for t, m := range c.bursts {
		for slot := range m {
			if slot < cut {
				delete(m, slot)
			}
		}
		if len(m) == 0 {
			delete(c.bursts, t)
		}
	}
}

// minutes buckets a target's bursts in [from, from+n minutes).
func minutes(m map[int64]*callBurst, from time.Time, n int) []callq.Minute {
	out := make([]callq.Minute, n)
	type acc struct{ rtt, jit, loss, nr, nj, nl float64 }
	accs := make([]acc, n)
	for i := range out {
		out[i] = callq.Empty
	}
	f := from.UnixMilli()
	for slot, b := range m {
		i := int((slot - f) / time.Minute.Milliseconds())
		if slot < f || i >= n {
			continue
		}
		mi, a := &out[i], &accs[i]
		switch {
		case b.unknown:
			mi.Unknown++
			continue
		case b.answered:
			mi.Bursts++
			mi.Answered++
			a.rtt += b.rtt
			a.nr++
		case b.timeout:
			mi.Bursts++
		default:
			continue // only a sibling seen so far
		}
		if !math.IsNaN(b.jitter) {
			a.jit += b.jitter
			a.nj++
		}
		if !math.IsNaN(b.loss) {
			a.loss += b.loss
			a.nl++
		}
	}
	for i, a := range accs {
		if a.nr > 0 {
			out[i].RTT = a.rtt / a.nr
		}
		if a.nj > 0 {
			out[i].Jitter = a.jit / a.nj
		}
		if a.nl > 0 {
			out[i].Loss = a.loss / a.nl
		}
	}
	return out
}

// Describe implements prometheus.Collector.
func (c *CallMOS) Describe(ch chan<- *prometheus.Desc) { ch <- callMOSDesc }

// Collect implements prometheus.Collector.
func (c *CallMOS) Collect(ch chan<- prometheus.Metric) {
	var p *model.Profile
	if c.profile != nil {
		p = c.profile()
	}
	if p == nil {
		return
	}
	now := c.now()
	from := now.Add(-CallMOSWindow).Truncate(time.Minute)
	n := int(now.Sub(from)/time.Minute) + 1
	var udp []model.Target
	var all [][]callq.Minute
	c.mu.Lock()
	for _, t := range p.Targets {
		if t.IsUDP() {
			udp = append(udp, t)
			all = append(all, minutes(c.bursts[t.Name], from, n))
		}
	}
	c.mu.Unlock()
	dead := callq.NotAnswering(all)
	group := func(keep func(model.Target) bool) (float64, bool) {
		var tm [][]callq.Minute
		var td []bool
		for i, t := range udp {
			if keep(t) {
				tm, td = append(tm, all[i]), append(td, dead[i])
			}
		}
		if len(tm) == 0 {
			return 0, false
		}
		ms, ok := callq.Group(tm, td, n)
		s := callq.Summarize(ms)
		return s.MOS, ok && s.Worst >= 0 && s.Bursts >= 10
	}
	type row struct {
		app string
		v   float64
	}
	var rows []row
	if v, ok := group(callq.IsCorePath); ok {
		rows = append(rows, row{"path", v})
	}
	for _, a := range callq.Apps {
		if !a.Measurable {
			continue
		}
		if v, ok := group(func(t model.Target) bool { return callq.HasApp(t, a.ID) }); ok {
			rows = append(rows, row{a.ID, v})
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].app < rows[j].app })
	for _, r := range rows {
		ch <- prometheus.MustNewConstMetric(callMOSDesc, prometheus.GaugeValue, r.v, r.app)
	}
}
