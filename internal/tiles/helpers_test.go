package tiles

import (
	"time"
)

// t0 is the tests' clock origin (no wall clock anywhere).
var t0 = time.Date(2026, 10, 9, 14, 0, 0, 0, time.UTC)

func at(d time.Duration) time.Time { return t0.Add(d) }

// pattern builds a tail from a string, one character per slot of every
// starting at start: '.' an answered check (RTT rtt), 'x' a lost one, '_'
// not measured (a gap).
func pattern(every time.Duration, start time.Time, p string, rtt time.Duration) tail {
	t := tail{every: every}
	for i, c := range p {
		a := start.Add(time.Duration(i) * every)
		switch c {
		case '.':
			t.checks = append(t.checks, check{at: a, rtt: rtt, jitter: time.Millisecond})
		case 'x':
			t.checks = append(t.checks, check{at: a, lost: true, loss: 1})
		}
	}
	return t
}

// rep repeats s n times.
func rep(s string, n int) string {
	out := ""
	for range n {
		out += s
	}
	return out
}

// callMin is a UDP minute of n answered bursts with a delay, jitter and loss.
func callMin(n int, delayMs, jitterMs, loss float64) minute {
	return minute{ok: uint16(n), medUs: uint32(delayMs * 1000), jitUs: uint32(jitterMs * 1000), loss: loss}
}

func j(s State) judged { return judged{s: s} }

func js(ss ...State) []judged {
	out := make([]judged, len(ss))
	for i, s := range ss {
		out[i] = j(s)
	}
	return out
}
