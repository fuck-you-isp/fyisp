package tiles

import (
	"math"
	"slices"
	"time"
)

// The tile panel's chart: the last 24 hours in 5-minute buckets.
const (
	chartStep    = barStep
	chartBuckets = int(24 * time.Hour / chartStep)
)

// Chart is a tile's last 24 hours.
type Chart struct {
	ID   string
	From time.Time // the first bucket; the last is the current, partial one
	Step time.Duration
	// Ms is each bucket's latency: the median of its charted minutes
	// (app and stream tiles: the median of the endpoints' TCP connect
	// medians; call tiles: their UDP delay, TCP without UDP). NaN: no
	// data.
	Ms      [chartBuckets]float64
	UsualMs *float64 // nil while learning or unknown
	Bands   []Band   // oldest first
}

// Band kinds.
const (
	BandProblem = "problem" // one of the tile's problems (or the connection's)
	BandOff     = "off"     // fyisp was not running: barNoneMins or more minutes without any check
)

// Band is a shaded span of a chart.
type Band struct {
	From, To time.Time // To zero: ongoing
	Kind     string
	// Problem bands: the episode.
	Episode string
	Sev     State
	Cause   Cause
}

// Chart implements Engine.
func (e *engine) Chart(id string) (*Chart, bool) {
	i := slices.IndexFunc(e.tiles[:], func(t *tile) bool { return t.def.ID == id })
	if i < 0 || e.tiles[i].off {
		return nil, false
	}
	t := e.tiles[i]
	s := e.snap.Load()
	now := s.At
	c := &Chart{ID: id, Step: chartStep, From: now.Truncate(chartStep).Add(-time.Duration(chartBuckets-1) * chartStep)}
	if u := s.Tiles[i].UsualMs; u != nil {
		v := *u
		c.UsualMs = &v
	}
	e.mu.Lock()
	var vb [int(chartStep / time.Minute)]float64
	for b := range c.Ms {
		v := vb[:0]
		from := c.From.Add(time.Duration(b) * chartStep)
		for m := from; m.Before(from.Add(chartStep)) && m.Before(e.nextMin); m = m.Add(time.Minute) {
			if x := t.plot[minIdx(m)]; x > 0 {
				v = append(v, float64(x))
			}
		}
		c.Ms[b] = math.NaN()
		if len(v) > 0 {
			c.Ms[b] = median(v)
		}
	}
	var off []Band
	var run time.Time
	n := 0
	for m := c.From; m.Before(e.nextMin) && m.Before(now); m = m.Add(time.Minute) {
		if !e.anyData[minIdx(m)] {
			if n == 0 {
				run = m
			}
			n++
			continue
		}
		if n >= barNoneMins {
			off = append(off, Band{From: run, To: m, Kind: BandOff})
		}
		n = 0
	}
	if n >= barNoneMins {
		off = append(off, Band{From: run, To: e.nextMin, Kind: BandOff})
	}
	e.mu.Unlock()
	for _, x := range tileEpisodes(s.Eps, id) {
		if !x.Ongoing() && x.End.Before(c.From) {
			continue
		}
		c.Bands = append(c.Bands, Band{From: x.Start, To: x.End, Kind: BandProblem, Episode: x.ID, Sev: x.Sev, Cause: x.Cause})
	}
	c.Bands = append(c.Bands, off...)
	slices.SortStableFunc(c.Bands, func(a, b Band) int { return a.From.Compare(b.From) })
	return c, true
}
