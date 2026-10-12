package tiles

import (
	"time"
)

// t0 is the tests' clock origin (no wall clock anywhere).
var t0 = time.Date(2026, 10, 9, 14, 0, 0, 0, time.UTC)

func at(d time.Duration) time.Time { return t0.Add(d) }

const ms10 = 10 * time.Millisecond

// pattern builds checks from a string, one character per slot of every
// starting at start: '.' an answered check (RTT rtt, jitter 1 ms), 'x' a
// lost one, '_' a slot that was not measured.
func pattern(every time.Duration, start time.Time, p string, rtt time.Duration) []check {
	var out []check
	for i, c := range p {
		ms := start.Add(time.Duration(i) * every).UnixMilli()
		switch c {
		case '.':
			out = append(out, check{ms: ms, rttUs: uint32(rtt.Microseconds()), jitUs: 1000})
		case 'x':
			out = append(out, check{ms: ms, lost: true, lossPct: 100})
		case '_':
			out = append(out, check{ms: ms, gap: true})
		}
	}
	return out
}

// feed runs checks through a new tracker and flushes it once all are held
// long enough. It returns the tracker and the series' runs (closed, then
// the open one).
func feed(every time.Duration, cs []check) (*tracker, runSet) {
	t := &tracker{every: every}
	var rs []failRun
	var last int64
	for _, c := range cs {
		t.add(c)
		last = max(last, c.ms)
	}
	if last > 0 {
		rs = append(rs, t.flush(time.UnixMilli(last).Add(holdFor(every)))...)
	}
	if r, ok := t.open(); ok {
		rs = append(rs, r)
	}
	s := runSet{runs: rs, every: every}
	if t.last != 0 {
		s.last = time.UnixMilli(t.last).UTC()
	}
	return t, s
}

// series is feed(pattern(…)).
func series(every time.Duration, start time.Time, p string) (*tracker, runSet) {
	return feed(every, pattern(every, start, p, ms10))
}

// rep repeats s n times.
func rep(s string, n int) string {
	out := ""
	for range n {
		out += s
	}
	return out
}

// callMin is a UDP minute of n answered bursts with a delay, jitter and a
// sum of burst losses (%), spread over bursts that lost 1 of 5 requests
// (20%) each.
func callMin(n int, delayMs, jitterMs float64, lossPctSum uint32) minute {
	return minute{ok: uint16(n), medUs: uint32(delayMs * 1000), jitUs: uint32(jitterMs * 1000), lossPctSum: lossPctSum,
		lossy: uint16(min(n, int(lossPctSum+19)/20))}
}
