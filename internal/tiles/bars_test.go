package tiles

import (
	"testing"
	"time"
)

// states parses one State per character: g good, s slow, d down, n none.
func states(p string) []State {
	m := map[rune]State{'g': Good, 's': Slow, 'd': Down, 'n': None}
	var out []State
	for _, c := range p {
		out = append(out, m[c])
	}
	return out
}

func TestBars(t *testing.T) {
	// now is 2:02:30 into the window: bars from 0:05 (t0+5m) to the
	// current bucket 2:00, which has 2 closed minutes
	now := at(2*time.Hour + 2*time.Minute + 30*time.Second)
	from := at(5 * time.Minute)
	good := rep("g", 117)
	with := func(bar int, p string) []State {
		s := []byte(good)
		copy(s[bar*5:], p)
		return states(string(s))
	}
	cases := []struct {
		name  string
		mins  []State
		drops []Drop
		bar   int
		want  State
	}{
		{"all good", states(good), nil, 3, Good},
		{"1 slow minute of 5", with(3, "gsggg"), nil, 3, Good},
		{"2 slow minutes of 5", with(3, "gsgsg"), nil, 3, Slow},
		{"one down minute", with(3, "ggdgg"), nil, 3, Down},
		{"down beats slow", with(3, "ssdss"), nil, 3, Down},
		{"2 minutes without data", with(3, "gnngg"), nil, 3, Good},
		{"3 minutes without data: fyisp wasn't running", with(3, "nnngg"), nil, 3, None},
		{"slow beats none", with(3, "ssnnn"), nil, 3, Slow},
		{"a drop", states(good), []Drop{{Start: at(21 * time.Minute), End: at(21*time.Minute + 40*time.Second), Dur: 40 * time.Second}}, 3, Down},
		{"a drop in progress", states(good), []Drop{{Start: at(2 * time.Hour)}}, 23, Down},
		{"the current bucket: unclosed minutes are not none", states(good), nil, 23, Good},
	}
	for _, c := range cases {
		bs := bars(now, from, c.mins, c.drops, nil, nil)
		if bs[0].From != at(5*time.Minute) || bs[23].From != at(2*time.Hour) {
			t.Fatalf("%s: bars from %v to %v", c.name, bs[0].From, bs[23].From)
		}
		if got := bs[c.bar].State; got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
		for i, b := range bs {
			if i != c.bar && b.State != Good {
				t.Errorf("%s: bar %d is %s", c.name, i, b.State)
			}
		}
	}
	// minutes before the first one known have no data
	if bs := bars(now, at(20*time.Minute), states(good[15:]), nil, nil, nil); bs[2].State != None || bs[3].State != Good {
		t.Errorf("before the first minute: got %s %s", bs[2].State, bs[3].State)
	}

	// the colouring episode and the drop are kept for the note; a speed
	// test keeps the colour and marks the bar
	eps := []Episode{
		{ID: "t-a", Start: at(20 * time.Minute), End: at(25 * time.Minute), Sev: Slow},
		{ID: "t-b", Start: at(21 * time.Minute), End: at(22 * time.Minute), Sev: Down, DropDur: 40 * time.Second},
	}
	drops := []Drop{{Start: at(21 * time.Minute), End: at(21*time.Minute + 40*time.Second), Dur: 40 * time.Second}}
	wins := speedWindows{{Start: at(21 * time.Minute), End: at(21*time.Minute + 30*time.Second)}}
	b := bars(now, from, states(good), drops, eps, wins)[3]
	if b.State != Down || b.Episode != "t-b" || b.Drop != 40*time.Second || !b.Speedtest {
		t.Errorf("drop bar: got %+v", b)
	}
	b = bars(now, from, with(3, "ssggg"), nil, eps, nil)[3]
	if b.State != Slow || b.Episode != "t-a" || b.Speedtest {
		t.Errorf("slow bar: got %+v", b)
	}
	if b := bars(now, from, states(good), nil, eps, wins)[3]; b.State != Good || b.Episode != "" || !b.Speedtest {
		t.Errorf("good bar in a speed test: got %+v", b)
	}
	// MaskTail: a test that ended 1 s before the bar still marks it
	wins = speedWindows{{Start: at(14 * time.Minute), End: at(15*time.Minute - time.Second)}}
	if b := bars(now, from, states(good), nil, nil, wins)[2]; !b.Speedtest {
		t.Errorf("mask tail: got %+v", b)
	}
}
