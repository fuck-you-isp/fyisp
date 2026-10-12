package tiles

import (
	"time"

	"github.com/fuck-you-isp/fyisp/internal/speedtest"
)

// Bar is one of a tile's 5-minute bars.
type Bar struct {
	From  time.Time
	State State // Good, Slow, Down or None
	// Episode is the ID of the episode that coloured a Slow or Down bar
	// ("" when none matches), Drop the drop that made it Down (rounded;
	// 0 for none).
	Episode string
	Drop    time.Duration
	// Speedtest: the bar overlaps a fyisp speed test. It keeps its
	// measured colour; its note says so.
	Speedtest bool
}

// bars builds a tile's nBars bars ending with the one now is in (the
// current, partial bucket). mins are the tile's closed minutes from from on
// (the tile-minute states, before hysteresis), drops its drops, eps its
// episodes. A bar is
//
//   - Down with any Down minute or an overlapping drop;
//   - else Slow with barSlowMins or more Slow minutes;
//   - else None with barNoneMins or more closed minutes without data
//     (fyisp was not running);
//   - else Good.
func bars(now, from time.Time, mins []State, drops []Drop, eps []Episode, wins []speedtest.Window) [nBars]Bar {
	var out [nBars]Bar
	first := now.Truncate(barStep).Add(-(nBars - 1) * barStep)
	for i := range out {
		b := &out[i]
		b.From = first.Add(time.Duration(i) * barStep)
		w := window{b.From, b.From.Add(barStep)}
		var slow, none int
		down := false
		for t := b.From; t.Before(w.to); t = t.Add(time.Minute) {
			s := None // before from: no data
			if !t.Before(from) {
				k := int(t.Sub(from) / time.Minute)
				if k >= len(mins) {
					continue // not closed yet
				}
				s = mins[k]
			}
			switch s {
			case Down:
				down = true
			case Slow:
				slow++
			case None:
				none++
			}
		}
		for _, d := range drops {
			if (window{d.Start, d.End}).overlaps(w, now) {
				down = true
				b.Drop = max(b.Drop, d.Dur)
			}
		}
		switch {
		case down:
			b.State = Down
		case slow >= barSlowMins:
			b.State = Slow
		case none >= barNoneMins:
			b.State = None
		default:
			b.State = Good
		}
		if b.State.Bad() {
			for _, e := range eps {
				if rank(e.Sev) >= rank(b.State) && e.window().overlaps(w, now) {
					b.Episode = e.ID
					break
				}
			}
		}
		b.Speedtest = speedOverlap(w.from, w.to.Add(-time.Nanosecond), wins, now)
	}
	return out
}
