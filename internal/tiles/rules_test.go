package tiles

import (
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/profile"
)

func TestLimits(t *testing.T) {
	cases := []struct {
		name           string
		usual          float64
		ok, learning   bool
		slowMs, downMs float64
	}{
		{"short path: +30 and +100 decide", 5, true, false, 35, 105},
		{"long path: 2x and 3x decide", 100, true, false, 200, 300},
		{"crossover", 30, true, false, 60, 130},
		{"learning: fixed", 5, true, true, learnSlowMs, learnDownMs},
		{"no usual yet: fixed", 0, false, false, learnSlowMs, learnDownMs},
	}
	for _, c := range cases {
		l := limitsFor(c.usual, c.ok, c.learning)
		if l.slowMs != c.slowMs || l.downMs != c.downMs {
			t.Errorf("%s: got %+v, want slow %v down %v", c.name, l, c.slowMs, c.downMs)
		}
	}
}

func TestJudgeWeb(t *testing.T) {
	u5 := limitsFor(5, true, false)
	learn := limitsFor(0, false, true)
	m := func(ok, lost int, medMs float64) minute {
		return minute{ok: uint16(ok), lost: uint16(lost), medUs: uint32(medMs * 1000)}
	}
	cases := []struct {
		name    string
		m       minute
		l       limits
		stream  bool
		want    State
		allLost bool
	}{
		{"usual 5: 35 ms is not slow", m(4, 0, 35), u5, false, Good, false},
		{"usual 5: needs more than 35", m(4, 0, 35.001), u5, false, Slow, false},
		{"usual 5: 105 is slow, not down", m(4, 0, 105), u5, false, Slow, false},
		{"usual 5: down above 105", m(4, 0, 105.001), u5, false, Down, false},
		{"stream: never down on latency", m(4, 0, 900), u5, true, Slow, false},
		{"stream: down when checks fail", m(1, 3, 5), u5, true, Down, false},
		{"3 of 4 lost: down", m(1, 3, 5), u5, false, Down, false},
		{"all lost: down, all lost", m(0, 4, 0), u5, false, Down, true},
		{"half lost is not most", m(2, 2, 5), u5, false, Good, false},
		{"1 of 4 lost: judged on latency", m(3, 1, 50), u5, false, Slow, false},
		{"one check: not enough data", m(1, 0, 5), u5, false, None, false},
		{"no checks", m(0, 0, 0), u5, false, None, false},
		{"learning: 300 ms is fine", m(4, 0, 300), learn, false, Good, false},
		{"learning: slow above 300", m(4, 0, 301), learn, false, Slow, false},
		{"learning: down above 1000", m(4, 0, 1001), learn, false, Down, false},
	}
	for _, c := range cases {
		got := judgeWeb(c.m, c.l, c.stream)
		if got.s != c.want || got.allLost != c.allLost {
			t.Errorf("%s: got %+v, want %s allLost=%v", c.name, got, c.want, c.allLost)
		}
	}
}

func TestJudgeCall(t *testing.T) {
	const every = 5 * time.Second // 12 bursts a minute, 36 in the window
	three := func(m minute) []minute { return []minute{m, m, m} }
	cases := []struct {
		name  string
		ms    []minute
		every time.Duration
		want  State
	}{
		{"149 ms", three(callMin(12, 149, 5, 0)), every, Good},
		{"150 ms is not over 150", three(callMin(12, 150, 5, 0)), every, Good},
		{"151 ms: choppy", three(callMin(12, 151, 5, 0)), every, Slow},
		{"jitter 29.9", three(callMin(12, 20, 29.9, 0)), every, Good},
		{"jitter 30.1: choppy", three(callMin(12, 20, 30.1, 0)), every, Slow},
		{"loss 1.9%", three(callMin(12, 20, 5, 0.019)), every, Good},
		{"loss 2%: choppy", three(callMin(12, 20, 5, 0.02)), every, Slow},
		{"400 ms is choppy, not down", three(callMin(12, 400, 5, 0)), every, Slow},
		{"401 ms: down", three(callMin(12, 401, 5, 0)), every, Down},
		{"10% loss in the last minute only: choppy",
			[]minute{callMin(12, 20, 5, 0), callMin(12, 20, 5, 0), callMin(12, 20, 5, 0.10)}, every, Slow},
		{"10% loss in the last 2 minutes: down",
			[]minute{callMin(12, 20, 5, 0), callMin(12, 20, 5, 0.10), callMin(12, 20, 5, 0.10)}, every, Down},
		{"10% loss two minutes apart: choppy",
			[]minute{callMin(12, 20, 5, 0.10), callMin(12, 20, 5, 0), callMin(12, 20, 5, 0.10)}, every, Slow},
		{"median of the minutes: one slow minute is not enough",
			[]minute{callMin(12, 20, 5, 0), callMin(12, 300, 5, 0), callMin(12, 20, 5, 0)}, every, Good},
		{"17 of 36 bursts: not enough data", []minute{callMin(6, 20, 5, 0), callMin(6, 20, 5, 0), callMin(5, 20, 5, 0)}, every, None},
		{"18 of 36 bursts: judged", three(callMin(6, 20, 5, 0)), every, Good},
		{"UDP 53 at 15 s: 6 of 12 is enough", three(callMin(2, 20, 5, 0)), 15 * time.Second, Good},
		{"UDP 53 at 15 s: 5 is not", []minute{callMin(2, 20, 5, 0), callMin(2, 20, 5, 0), callMin(1, 20, 5, 0)}, 15 * time.Second, None},
		{"no minutes", nil, every, None},
	}
	for _, c := range cases {
		got, _ := judgeCall(c.ms, c.every)
		if got.s != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got.s, c.want)
		}
	}

	// every burst timed out: down, all lost; the stats say so
	lost := minute{lost: 12, loss: 1, jitUs: noJitter}
	got, st := judgeCall([]minute{lost, lost, lost}, every)
	if got.s != Down || !got.allLost || st.loss != 1 || st.hasJitter {
		t.Errorf("all lost: got %+v %+v", got, st)
	}
}

func TestMajority(t *testing.T) {
	cases := []struct {
		name string
		in   []State
		want State
	}{
		{"1 endpoint down", []State{Down}, Down},
		{"1 endpoint slow", []State{Slow}, Slow},
		{"2 endpoints, 1 down: a tie is not a majority", []State{Down, Good}, Good},
		{"2 endpoints, both down", []State{Down, Down}, Down},
		{"2 endpoints, slow and down: slow", []State{Slow, Down}, Slow},
		{"3 endpoints, 1 down", []State{Down, Good, Good}, Good},
		{"3 endpoints, 2 down", []State{Down, Down, Good}, Down},
		{"3 endpoints, 1 down 1 slow: slow", []State{Down, Slow, Good}, Slow},
		{"only endpoints with data count", []State{Down, None, None}, Down},
		{"2 with data, tie", []State{Down, Good, None}, Good},
		{"no data", []State{None, None}, None},
		{"empty", nil, None},
	}
	for _, c := range cases {
		if got := majority(js(c.in...)); got.s != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got.s, c.want)
		}
	}
}

func TestCallVotes(t *testing.T) {
	// the shared path: Cloudflare (STUN + UDP 53, one vote) and Twilio
	cloudflare := vote(j(Good), j(Good))
	if got := majority([]judged{cloudflare, j(Down)}); got.s != Good {
		t.Errorf("one provider dead: got %s, want good", got.s)
	}
	if got := vote(j(Good), j(Slow)); got.s != Slow {
		t.Errorf("a provider is as bad as its worst endpoint: got %s", got.s)
	}
	if got := vote(j(None), j(Good)); got.s != Good {
		t.Errorf("endpoints without data are left out: got %s", got.s)
	}
	if got := vote(judged{Down, true}, judged{Down, true}); !got.allLost {
		t.Errorf("all lost: got %+v", got)
	}
	if got := majority([]judged{vote(j(Slow)), vote(j(Slow), j(Good))}); got.s != Slow {
		t.Errorf("both providers choppy: got %s", got.s)
	}
}

func TestTileMinute(t *testing.T) {
	call, app := profile.TileCall, profile.TileApp
	cases := []struct {
		name     string
		kind     profile.TileKind
		web, udp []State
		want     State
	}{
		{"call: quality from UDP", call, []State{Good}, []State{Slow}, Slow},
		{"call: web down is not connecting", call, []State{Down}, []State{Good, Good}, Down},
		{"call: web slow does not make it choppy", call, []State{Slow}, []State{Good}, Good},
		{"call: tcp only, web slow: choppy impossible", call, []State{Slow}, nil, Good},
		{"call: tcp only, web down", call, []State{Down}, nil, Down},
		{"call: no UDP data: judged on TCP", call, []State{Good}, []State{None}, Good},
		{"call: no data at all", call, []State{None}, []State{None}, None},
		{"call: shared path, one provider dead", call, []State{Good}, []State{Good, Down}, Good},
		{"app: majority of 3", app, []State{Down, Down, Good}, nil, Down},
		{"app: 1 of 3 down", app, []State{Down, Good, Good}, nil, Good},
		{"app: single endpoint", app, []State{Slow}, nil, Slow},
	}
	for _, c := range cases {
		if got := tileMinute(c.kind, js(c.web...), js(c.udp...)); got.s != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got.s, c.want)
		}
	}
	got := tileMinute(call, []judged{{Down, true}}, []judged{{Down, true}})
	if got.s != Down || !got.allLost {
		t.Errorf("call, everything lost: got %+v", got)
	}
	got = tileMinute(call, []judged{{Down, true}}, js(Good))
	if got.s != Down || got.allLost {
		t.Errorf("call, web lost but UDP answers: got %+v (not every check failed)", got)
	}
}

func TestImmediate(t *testing.T) {
	const every = 15 * time.Second
	one := func(p string) [][]tail { return [][]tail{{pattern(every, t0, p, 10*time.Millisecond)}} }
	many := func(ps ...string) [][]tail {
		var out [][]tail
		for _, p := range ps {
			out = append(out, []tail{pattern(every, t0, p, 10*time.Millisecond)})
		}
		return out
	}
	cases := []struct {
		name    string
		members [][]tail
		want    signal
		since   time.Time
	}{
		{"single endpoint: 2 failures are not enough", one("....xx"), hold, time.Time{}},
		{"single endpoint: 3 failures", one("...xxx"), failing, at(3 * every)},
		{"single endpoint: clears after 2 OK", one("..xxx.."), cleared, time.Time{}},
		{"single endpoint: 1 OK holds", one("..xxx."), hold, time.Time{}},
		{"multi: 2 failures on most members", many("....xx", "...xxx", "......"), failing, at(4 * every)},
		{"multi: since is when most were failing", many("..xxxx", "....xx", "......"), failing, at(4 * every)},
		{"multi: 1 of 3 failing", many("....xx", "......", "......"), cleared, time.Time{}},
		{"multi: 1 failure each is not enough", many(".....x", ".....x", "......"), hold, time.Time{}},
		{"multi: a tie is not a majority", many("....xx", "......"), hold, time.Time{}},
		{"multi: members without checks don't count", many("....xx", "....xx", ""), failing, at(4 * every)},
		{"no checks", many("", ""), hold, time.Time{}},
	}
	for _, c := range cases {
		s, since := immediate(c.members)
		if s != c.want || !since.Equal(c.since) {
			t.Errorf("%s: got %v %v, want %v %v", c.name, s, since, c.want, c.since)
		}
	}

	// a provider with two series fails when either does (k = 2: two members)
	cf := []tail{pattern(5*time.Second, t0, "......", 0), pattern(15*time.Second, t0, "..xx", 0)}
	tw := []tail{pattern(5*time.Second, t0, "....xx", 0)}
	if s, _ := immediate([][]tail{cf, tw}); s != failing {
		t.Errorf("providers: got %v, want failing", s)
	}

	// the tile: any group failing; cleared only when every group is
	if s, since := tileSignal([]signal{cleared, failing}, []time.Time{{}, at(time.Minute)}); s != failing || !since.Equal(at(time.Minute)) {
		t.Errorf("tile: got %v %v", s, since)
	}
	if s, _ := tileSignal([]signal{cleared, hold}, []time.Time{{}, {}}); s != hold {
		t.Errorf("tile: got %v, want hold", s)
	}
	if s, _ := tileSignal([]signal{cleared, cleared}, []time.Time{{}, {}}); s != cleared {
		t.Errorf("tile: got %v, want cleared", s)
	}
}

// steps feeds a word one minute per character from start: g good, s slow,
// d down, o all lost (down), n none. It returns the runs that ended.
func steps(w *word, start time.Time, p string) []wordRun {
	var out []wordRun
	for i, c := range p {
		m := map[rune]judged{'g': j(Good), 's': j(Slow), 'd': j(Down), 'o': {Down, true}, 'n': j(None)}[c]
		if r := w.step(start.Add(time.Duration(i)*time.Minute), m); r != nil {
			out = append(out, *r)
		}
	}
	return out
}

func TestWordHysteresis(t *testing.T) {
	cases := []struct {
		name  string
		p     string
		want  State
		since time.Duration // from t0; -1 for zero
	}{
		{"no data yet", "nnn", None, -1},
		{"2 of 5 bad stays good", "gsgsg", Good, -1},
		{"3 of 5 bad flips", "gsgsgs", Slow, 1 * time.Minute},
		{"3 in a row", "ggsss", Slow, 2 * time.Minute},
		{"bad minutes older than 5 judged ones don't count", "sgsggggs", Good, -1},
		{"4 good minutes stay slow", "sssgggg", Slow, 0},
		{"5 good minutes flip back", "sssggggg", Good, -1},
		{"none holds the word", "sssnnnnnnn", Slow, 0},
		{"none doesn't count toward 5 good", "sssggnngg", Slow, 0},
		{"none doesn't break a good run", "sssggnnggg", Good, -1},
		{"latency down shows at once", "gggd", Down, 3 * time.Minute},
		{"latency down needs 5 good minutes", "gggdgggg", Down, 3 * time.Minute},
		{"latency down: 5 good minutes", "gggdggggg", Good, -1},
		{"slow escalates to down", "sssd", Down, 3 * time.Minute},
		{"a slow minute doesn't leave down", "dgggsgggg", Down, 0},
		{"down, then slow minutes count toward 3 of 5", "dgggggsss", Slow, 6 * time.Minute},
		{"a minute where every check failed: down at once", "ggo", Down, 2 * time.Minute},
		{"all-lost down waits for the immediate signal", "ggoggggggg", Down, 2 * time.Minute},
	}
	for _, c := range cases {
		var w word
		steps(&w, t0, c.p)
		s, since := w.state()
		want := time.Time{}
		if c.since >= 0 {
			want = at(c.since)
		}
		if s != c.want || !since.Equal(want) {
			t.Errorf("%s (%s): got %s since %v, want %s since %v", c.name, c.p, s, since, c.want, want)
		}
	}
}

func TestWordRuns(t *testing.T) {
	var w word
	rs := steps(&w, t0, "gsssdggggg")
	if len(rs) != 1 {
		t.Fatalf("runs: got %v", rs)
	}
	// the run ends where the good minutes began, not when the word flipped
	if r := rs[0]; !r.Start.Equal(at(time.Minute)) || !r.End.Equal(at(5*time.Minute)) || r.Sev != Down {
		t.Errorf("run: got %+v", r)
	}
	if _, open := w.open(); open {
		t.Error("run still open")
	}
	steps(&w, at(10*time.Minute), "sss")
	if r, open := w.open(); !open || !r.Start.Equal(at(10*time.Minute)) || r.Sev != Slow {
		t.Errorf("open run: got %+v %v", r, open)
	}
}

func TestWordOutage(t *testing.T) {
	var w word
	steps(&w, t0, "ggg")
	// Down now: at once, since the first failed slot
	if r := w.outageSignal(failing, at(3*time.Minute+10*time.Second), at(3*time.Minute+45*time.Second)); r != nil {
		t.Errorf("run ended on failing: %+v", r)
	}
	if s, since := w.state(); s != Down || !since.Equal(at(3*time.Minute+10*time.Second)) {
		t.Errorf("outage: got %s %v", s, since)
	}
	// hold keeps it, a minute without data keeps it
	w.outageSignal(hold, time.Time{}, at(4*time.Minute))
	steps(&w, at(4*time.Minute), "n")
	if s, _ := w.state(); s != Down {
		t.Errorf("hold: got %s", s)
	}
	// cleared: back to Good at once, no 5-minute wait
	r := w.outageSignal(cleared, time.Time{}, at(4*time.Minute+30*time.Second))
	if s, _ := w.state(); s != Good {
		t.Errorf("cleared: got %s", s)
	}
	if r == nil || !r.Start.Equal(at(3*time.Minute+10*time.Second)) || !r.End.Equal(at(4*time.Minute+30*time.Second)) || r.Sev != Down {
		t.Errorf("outage run: got %+v", r)
	}

	// an outage over a slow word goes back to slow
	var v word
	steps(&v, t0, "sss")
	v.outageSignal(failing, at(3*time.Minute), at(3*time.Minute))
	v.outageSignal(cleared, time.Time{}, at(4*time.Minute))
	if s, since := v.state(); s != Slow || !since.Equal(t0) {
		t.Errorf("outage over slow: got %s %v", s, since)
	}
	if r, _ := v.open(); r.Sev != Down {
		t.Errorf("run severity: got %s", r.Sev)
	}
}

func TestSummarize(t *testing.T) {
	const every = 5 * time.Second
	ms := 20 * time.Millisecond
	// minute 0 clean, a 40 s drop from 1:10 to 1:50, then clean
	tl := pattern(every, t0, rep(".", 14)+rep("x", 8)+rep(".", 14), ms)
	m := summarize(tl, at(time.Minute))
	if m.lost != 0 || m.ok != 4 || m.loss != 0 || m.medMs() != 20 {
		t.Errorf("drop slots should be left out: got %+v", m)
	}
	// so the calls are not Choppy after the drop
	m0 := summarize(tl, t0)
	m2 := summarize(tl, at(2*time.Minute))
	if got, _ := judgeCall([]minute{m0, m, m2}, every); got.s != Good {
		t.Errorf("after a drop: got %s, want good", got.s)
	}

	// a 10 s failure is loss, not a drop
	tl = pattern(every, t0, rep(".", 14)+"xx"+rep(".", 20), ms)
	m = summarize(tl, at(time.Minute))
	if m.lost != 2 || m.ok != 10 {
		t.Errorf("short failure: got %+v", m)
	}
	if got, _ := judgeCall([]minute{summarize(tl, t0), m, summarize(tl, at(2*time.Minute))}, every); got.s != Slow {
		t.Errorf("short failure: got %s, want choppy (2 of 36 lost)", got.s)
	}

	// a run still open at the tail's end is left out: it may become a drop
	tl = pattern(every, t0, rep(".", 22)+"xx", ms)
	if m := summarize(tl, at(time.Minute)); m.lost != 0 || m.ok != 10 {
		t.Errorf("open run: got %+v", m)
	}

	// jitter: the median of the bursts that measured one
	tl = tail{every: every}
	for i, jit := range []time.Duration{-1, 2, 4, 30} {
		tl.checks = append(tl.checks, check{at: at(time.Duration(i) * every), rtt: ms, jitter: jit * time.Millisecond})
	}
	if m := summarize(tl, t0); !m.hasJitter() || m.jitterMs() != 4 {
		t.Errorf("jitter: got %+v", m)
	}
	tl.checks = tl.checks[:1]
	if m := summarize(tl, t0); m.hasJitter() {
		t.Errorf("jitter not measured: got %+v", m)
	}
}
