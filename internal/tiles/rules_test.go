package tiles

import (
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/profile"
)

func TestLimits(t *testing.T) {
	cases := []struct {
		name         string
		usual        float64
		ok, learning bool
		slowMs       float64
	}{
		{"short path: +30 decides", 5, true, false, 35},
		{"long path: 2x decides", 100, true, false, 200},
		{"crossover", 30, true, false, 60},
		{"learning: fixed", 5, true, true, learnSlowMs},
		{"no usual yet: fixed", 0, false, false, learnSlowMs},
	}
	for _, c := range cases {
		l := limitsFor(c.usual, c.ok, c.learning)
		if l.slowMs != c.slowMs {
			t.Errorf("%s: got %+v, want slow %v", c.name, l, c.slowMs)
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
		name string
		m    minute
		l    limits
		want State
	}{
		{"usual 5: 35 ms is not slow", m(4, 0, 35), u5, Good},
		{"usual 5: needs more than 35", m(4, 0, 35.001), u5, Slow},
		{"usual 5: 105 is slow", m(4, 0, 105), u5, Slow},
		{"latency alone is never down", m(4, 0, 900), u5, Slow},
		// 10-07 16:23 UTC: dev-GitHub answered every check in 101–195 ms
		// (usual 24 ms); that is slow, not "GitHub is down"
		{"GitHub 10-07: 148 ms against 24 ms", m(4, 0, 148), limitsFor(24, true, false), Slow},
		{"3 of 4 lost: down", m(1, 3, 5), u5, Down},
		{"all lost: down", m(0, 4, 0), u5, Down},
		{"half lost is not most: slow", m(2, 2, 5), u5, Slow},
		{"1 of 4 lost: slow", m(3, 1, 5), u5, Slow},
		{"1 of 5 lost: good", m(4, 1, 5), u5, Good},
		{"one check: not enough data", m(1, 0, 5), u5, None},
		{"no checks", m(0, 0, 0), u5, None},
		{"learning: 300 ms is fine", m(4, 0, 300), learn, Good},
		{"learning: slow above 300", m(4, 0, 301), learn, Slow},
		{"learning: 1001 ms is slow, not down", m(4, 0, 1001), learn, Slow},
	}
	for _, c := range cases {
		if got := judgeWeb(c.m, c.l); got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
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
		{"loss 1.9%", []minute{callMin(12, 20, 5, 23), callMin(12, 20, 5, 23), callMin(12, 20, 5, 22)}, every, Good},
		{"loss 2%: choppy", three(callMin(12, 20, 5, 24)), every, Slow},
		{"400 ms is choppy, not down", three(callMin(12, 400, 5, 0)), every, Slow},
		{"401 ms: down", three(callMin(12, 401, 5, 0)), every, Down},
		{"10% loss in the last minute only: choppy",
			[]minute{callMin(12, 20, 5, 0), callMin(12, 20, 5, 0), callMin(12, 20, 5, 120)}, every, Slow},
		{"10% loss in the last 2 minutes: down",
			[]minute{callMin(12, 20, 5, 0), callMin(12, 20, 5, 120), callMin(12, 20, 5, 120)}, every, Down},
		{"9.9% in one of them: choppy",
			[]minute{callMin(12, 20, 5, 0), callMin(12, 20, 5, 119), callMin(12, 20, 5, 120)}, every, Slow},
		{"10% loss two minutes apart: choppy",
			[]minute{callMin(12, 20, 5, 120), callMin(12, 20, 5, 0), callMin(12, 20, 5, 120)}, every, Slow},
		{"median of the minutes: one slow minute is not enough",
			[]minute{callMin(12, 20, 5, 0), callMin(12, 300, 5, 0), callMin(12, 20, 5, 0)}, every, Good},
		{"17 of 36 bursts: not enough data", []minute{callMin(6, 20, 5, 0), callMin(6, 20, 5, 0), callMin(5, 20, 5, 0)}, every, None},
		{"18 of 36 bursts: judged", three(callMin(6, 20, 5, 0)), every, Good},
		{"UDP 53 at 15 s: 6 of 12 is enough", three(callMin(2, 20, 5, 0)), 15 * time.Second, Good},
		{"UDP 53 at 15 s: 5 is not", []minute{callMin(2, 20, 5, 0), callMin(2, 20, 5, 0), callMin(1, 20, 5, 0)}, 15 * time.Second, None},
		{"no minutes", nil, every, None},
		// one fully lost burst is 100/36 = 2.8%, a ~100 ms stall: not Choppy
		{"one lost burst: good", []minute{callMin(12, 20, 5, 0), {ok: 11, lost: 1, medUs: 20000, jitUs: 5000, lossPctSum: 100, lossy: 1}, callMin(12, 20, 5, 0)}, every, Good},
		{"one burst losing 3 of 5 (60%, 1.7%): good", three(minute{ok: 12, medUs: 20000, jitUs: 5000, lossPctSum: 20, lossy: 1}), every, Good},
		{"two lossy bursts reaching 2%: choppy", []minute{callMin(12, 20, 5, 0), {ok: 12, medUs: 20000, jitUs: 5000, lossPctSum: 80, lossy: 2}, callMin(12, 20, 5, 0)}, every, Slow},
		{"a lost burst and a stray partial one: choppy", []minute{{ok: 11, lost: 1, medUs: 20000, jitUs: 5000, lossPctSum: 100, lossy: 1}, {ok: 12, medUs: 20000, jitUs: 5000, lossPctSum: 20, lossy: 1}, callMin(12, 20, 5, 0)}, every, Slow},
		{"two lossy bursts under 2%: good", three(minute{ok: 12, medUs: 20000, jitUs: 5000, lossPctSum: 23, lossy: 2}), every, Good},
	}
	for _, c := range cases {
		if got, _ := judgeCall(c.ms, c.every); got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}

	// one lost burst on a single-member tile (Teams' relay): the tile stays
	// Good through summarize, judgeCall and tileMinute
	cs := pattern(every, t0, rep(".", 17)+"x"+rep(".", 18), ms10)
	_, rs := series(every, t0, rep(".", 17)+"x"+rep(".", 18))
	var ms []minute
	for i := range 3 {
		from := t0.Add(time.Duration(i) * time.Minute)
		ms = append(ms, summarize(cs, rs.runs, ruleUDP, from, from.Add(80*time.Second)))
	}
	if ms[1].lossy != 1 || ms[1].lost != 1 {
		t.Fatalf("the lost burst's minute: %+v", ms[1])
	}
	if st, _ := judgeCall(ms, every); st != Good {
		t.Errorf("one lost burst: %s, want good", st)
	} else if tm := tileMinute(profile.TileCall, []State{Good}, []State{st}); tm != Good {
		t.Errorf("one lost burst: tile-minute %s", tm)
	}

	// every burst timed out: down; the stats say so
	lost := minute{lost: 12, lossPctSum: 1200, lossy: 12, jitUs: noJitter}
	got, st := judgeCall([]minute{lost, lost, lost}, every)
	if got != Down || st.lossPct != 100 || st.hasJitter {
		t.Errorf("all lost: got %s %+v", got, st)
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
		if got := majority(c.in); got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
}

func TestCallVotes(t *testing.T) {
	// the shared path: Cloudflare (STUN + UDP 53, one vote) and Twilio
	if got := majority([]State{vote(Good, Good), Down}); got != Good {
		t.Errorf("one provider dead: got %s, want good", got)
	}
	if got := vote(Good, Slow); got != Slow {
		t.Errorf("a provider is as bad as its worst endpoint: got %s", got)
	}
	if got := vote(None, Good); got != Good {
		t.Errorf("endpoints without data are left out: got %s", got)
	}
	if got := vote(None, None); got != None {
		t.Errorf("no data: got %s", got)
	}
	if got := majority([]State{vote(Slow), vote(Slow, Good)}); got != Slow {
		t.Errorf("both providers choppy: got %s", got)
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
		if got := tileMinute(c.kind, c.web, c.udp); got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
}

func TestImmediate(t *testing.T) {
	const every = 15 * time.Second
	tr := func(p string) *tracker { x, _ := series(every, t0, p); return x }
	one := func(p string) [][]*tracker { return [][]*tracker{{tr(p)}} }
	many := func(ps ...string) [][]*tracker {
		var out [][]*tracker
		for _, p := range ps {
			out = append(out, []*tracker{tr(p)})
		}
		return out
	}
	none := time.Time{}
	cases := []struct {
		name    string
		members [][]*tracker
		want    signal
		at      time.Time
	}{
		{"single endpoint: 2 failures are not enough", one("....xx"), hold, none},
		{"single endpoint: 3 failures", one("...xxx"), failing, at(3 * every)},
		{"single endpoint: clears after 2 OK, at the second", one("..xxx.."), cleared, at(6 * every)},
		{"single endpoint: 1 OK holds", one("..xxx."), hold, none},
		{"multi: 2 failures on most members", many("....xx", "...xxx", "......"), failing, at(4 * every)},
		{"multi: since is when most were failing", many("..xxxx", "....xx", "......"), failing, at(4 * every)},
		{"multi: 1 of 3 failing", many("....xx", "......", "......"), cleared, at(1 * every)},
		{"multi: 1 failure each is not enough", many(".....x", ".....x", "......"), cleared, at(1 * every)},
		{"multi: 2 of the latest 3 answered is OK", many("..x.x.", "..x.x.", "......"), cleared, at(5 * every)},
		{"multi: 1 of the latest 3 is not", many("....xx", "...x.x", "....x."), hold, none},
		// a tie is no majority for Down, so it clears (the Twilio-dead case)
		{"multi: a tie clears", many("....xx", "......"), cleared, at(1 * every)},
		{"multi: a dead member and a recovered one clear", many(rep("x", 6), "..xx.."), cleared, at(5 * every)},
		{"multi: members without checks don't count", many("....xx", "....xx", ""), failing, at(4 * every)},
		{"no checks", many("", ""), noData, none},
	}
	for _, c := range cases {
		s, a := immediate(c.members)
		if s != c.want || !a.Equal(c.at) {
			t.Errorf("%s: got %v %v, want %v %v", c.name, s, a, c.want, c.at)
		}
	}

	// a provider with two series fails when either does (k = 2: two members)
	cf := []*tracker{tr("......"), tr("..xx")}
	if s, _ := immediate([][]*tracker{cf, {tr("....xx")}}); s != failing {
		t.Errorf("providers: got %v, want failing", s)
	}
}

func TestTileSignal(t *testing.T) {
	a, b := at(time.Minute), at(2*time.Minute)
	cases := []struct {
		name string
		sigs []signal
		ats  []time.Time
		want signal
		at   time.Time
	}{
		{"any group failing", []signal{cleared, failing}, []time.Time{a, b}, failing, b},
		{"the earliest failing", []signal{failing, failing}, []time.Time{b, a}, failing, a},
		{"one group holding", []signal{cleared, hold}, []time.Time{a, {}}, hold, time.Time{}},
		{"all cleared: at the latest", []signal{cleared, cleared}, []time.Time{b, a}, cleared, b},
		{"web cleared, UDP without checks", []signal{cleared, noData}, []time.Time{a, {}}, cleared, a},
		{"nothing", []signal{noData, noData}, []time.Time{{}, {}}, noData, time.Time{}},
	}
	for _, c := range cases {
		if s, got := tileSignal(c.sigs, c.ats); s != c.want || !got.Equal(c.at) {
			t.Errorf("%s: got %v %v, want %v %v", c.name, s, got, c.want, c.at)
		}
	}
}

// steps feeds a word one minute per character from start: g good, s slow,
// d down, n none. It returns the runs that ended.
func steps(w *word, start time.Time, p string) []wordRun {
	var out []wordRun
	for i, c := range p {
		s := map[rune]State{'g': Good, 's': Slow, 'd': Down, 'n': None}[c]
		if r := w.step(start.Add(time.Duration(i)*time.Minute), s); r != nil {
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
		{"slow escalates to down, since resets", "sssd", Down, 3 * time.Minute},
		{"down steps back to slow without a down minute in 5", "gggd" + rep("s", 30), Slow, 4 * time.Minute},
		{"down with slow minutes: still down within 5", "gggdssss", Down, 3 * time.Minute},
		{"down, then a lone slow minute: good once no down is left", "dgggsgggg", Good, -1},
		{"one down minute doesn't change the rest", "d" + rep("gggs", 30), Good, -1},
		{"the same minutes alone", rep("gggs", 30), Good, -1},
		{"down, then slow minutes count toward 3 of 5", "dgggggsss", Slow, 6 * time.Minute},
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
	if r := w.outageSignal(failing, at(3*time.Minute+10*time.Second)); r != nil {
		t.Errorf("run ended on failing: %+v", r)
	}
	if s, since := w.state(); s != Down || !since.Equal(at(3*time.Minute+10*time.Second)) {
		t.Errorf("outage: got %s %v", s, since)
	}
	// hold, no data and a minute without data keep it
	w.outageSignal(hold, time.Time{})
	w.outageSignal(noData, time.Time{})
	steps(&w, at(4*time.Minute), "n")
	if s, _ := w.state(); s != Down {
		t.Errorf("hold: got %s", s)
	}
	// cleared: back to Good at once; the run ends at the clearing slot
	r := w.outageSignal(cleared, at(4*time.Minute+15*time.Second))
	if s, _ := w.state(); s != Good {
		t.Errorf("cleared: got %s", s)
	}
	if r == nil || !r.Start.Equal(at(3*time.Minute+10*time.Second)) || !r.End.Equal(at(4*time.Minute+15*time.Second)) || r.Sev != Down {
		t.Errorf("outage run: got %+v", r)
	}

	// an outage over a slow word goes back to slow
	var v word
	steps(&v, t0, "sss")
	v.outageSignal(failing, at(3*time.Minute))
	v.outageSignal(cleared, at(4*time.Minute))
	if s, since := v.state(); s != Slow || !since.Equal(t0) {
		t.Errorf("outage over slow: got %s %v", s, since)
	}
	if r, _ := v.open(); r.Sev != Down {
		t.Errorf("run severity: got %s", r.Sev)
	}
}

// TestTieClears: Twilio's STUN is dead and Cloudflare's drops for a minute
// and recovers. Zoom (shared path, two provider votes) is Down during the
// drop and Good again once Cloudflare answers: the tie must not hold it
// Down forever.
func TestTieClears(t *testing.T) {
	const every = 5 * time.Second
	var w word
	steps(&w, at(-5*time.Minute), "ggggg")
	tw, _ := series(every, t0, rep("x", 20))
	cf, _ := series(every, t0, rep(".", 10)+rep("x", 10))
	sig, a := immediate([][]*tracker{{tw}, {cf}})
	w.outageSignal(tileSignal([]signal{sig}, []time.Time{a}))
	if s, since := w.state(); s != Down || !since.Equal(at(50*time.Second)) {
		t.Fatalf("during the drop: got %s %v", s, since)
	}
	tw, _ = series(every, t0, rep("x", 60))
	cf, _ = series(every, t0, rep(".", 10)+rep("x", 12)+rep(".", 38))
	sig, a = immediate([][]*tracker{{tw}, {cf}})
	r := w.outageSignal(tileSignal([]signal{sig}, []time.Time{a}))
	if s, _ := w.state(); s != Good || r == nil || !r.End.Equal(at(115*time.Second)) {
		t.Errorf("after the drop: got %s %+v, want good at 1:55", s, r)
	}
}

func TestSummarize(t *testing.T) {
	const every = 5 * time.Second
	closeOf := func(from time.Time) time.Time { return from.Add(80 * time.Second) }
	sum := func(p string, rule runRule, from time.Time) minute {
		cs := pattern(every, t0, p, 20*time.Millisecond)
		_, s := feed(every, cs)
		return summarize(cs, s.runs, rule, from, closeOf(from))
	}
	// minute 0 clean, a 40 s drop from 1:10 to 1:50, then clean
	drop := rep(".", 14) + rep("x", 8) + rep(".", 14)
	m := sum(drop, ruleUDP, at(time.Minute))
	if m.lost != 0 || m.ok != 4 || m.lossPctSum != 0 || m.medMs() != 20 {
		t.Errorf("drop slots should be left out: got %+v", m)
	}
	// so the calls are not Choppy after the drop
	if got, _ := judgeCall([]minute{sum(drop, ruleUDP, t0), m, sum(drop, ruleUDP, at(2*time.Minute))}, every); got != Good {
		t.Errorf("after a drop: got %s, want good", got)
	}
	// the path layers count every lost check
	if m := sum(drop, rulePath, at(time.Minute)); m.lost != 8 {
		t.Errorf("path: got %+v", m)
	}

	// a 10 s failure is loss, not a drop
	short := rep(".", 14) + "xx" + rep(".", 20)
	if m := sum(short, ruleUDP, at(time.Minute)); m.lost != 2 || m.ok != 10 {
		t.Errorf("short failure: got %+v", m)
	}
	if got, _ := judgeCall([]minute{sum(short, ruleUDP, t0), sum(short, ruleUDP, at(time.Minute)), sum(short, ruleUDP, at(2*time.Minute))}, every); got != Slow {
		t.Errorf("short failure: got %s, want choppy (2 of 36 lost)", got)
	}

	// a run still open when the minute closes is left out (it may become a
	// drop), and judged later (replay) the same minute is the same
	cs := pattern(every, t0, rep(".", 22)+"xx"+rep(".", 20), 20*time.Millisecond)
	_, s := feed(every, cs)
	from := at(time.Minute)
	if m := summarize(cs, s.runs, ruleUDP, from, at(115*time.Second)); m.lost != 0 || m.ok != 10 {
		t.Errorf("open at close: got %+v", m)
	}
	if m := summarize(cs, s.runs, ruleUDP, from, closeOf(from)); m.lost != 2 {
		t.Errorf("closed short run: got %+v", m)
	}

	// gaps are counted apart from data
	if m := sum(rep(".", 12)+rep("_", 6)+rep(".", 6), ruleUDP, at(time.Minute)); m.gap != 6 || m.ok != 6 {
		t.Errorf("gaps: got %+v", m)
	}

	// jitter: the median of the bursts that measured one
	cs = nil
	for i, jit := range []uint32{noJitter, 2000, 4000, 30000} {
		cs = append(cs, check{ms: at(time.Duration(i) * every).UnixMilli(), rttUs: 20000, jitUs: jit})
	}
	if m := summarize(cs, nil, ruleUDP, t0, closeOf(t0)); !m.hasJitter() || m.jitterMs() != 4 {
		t.Errorf("jitter: got %+v", m)
	}
	if m := summarize(cs[:1], nil, ruleUDP, t0, closeOf(t0)); m.hasJitter() {
		t.Errorf("jitter not measured: got %+v", m)
	}
}

// TestLossIsCounted: runs too short for a drop are loss. Two lost checks
// at 15 s (a 30 s estimate) are no tile drop (that needs 3), so they count
// in their minutes.
func TestLossIsCounted(t *testing.T) {
	const every = 15 * time.Second
	// the review's pattern: half lost, counted, Slow (half is not "most")
	cs := pattern(every, t0, "."+rep("xx..", 30), ms10)
	_, s := feed(every, cs)
	m := summarize(cs, s.runs, ruleWeb, at(time.Minute), at(time.Minute+80*time.Second))
	if m.lost != 2 || m.ok != 2 {
		t.Errorf("xx..: got %+v, want 2 lost of 4", m)
	}
	if st := judgeWeb(m, limitsFor(10, true, false)); st != Slow {
		t.Errorf("xx..: got %s, want slow", st)
	}
	// one lost check of four: Slow; three (never in a drop-like run): Down
	for p, want := range map[string]State{"....x...": Slow, "....xx.x": Down} {
		cs := pattern(every, t0, p, ms10)
		_, s := feed(every, cs)
		if st := judgeWeb(summarize(cs, s.runs, ruleWeb, at(time.Minute), at(time.Minute+80*time.Second)), limitsFor(10, true, false)); st != want {
			t.Errorf("%s: got %s, want %s", p, st, want)
		}
	}
	// 60% lost, never 3 in a row: Down minutes, and no drop
	p := rep("xx.x.", 24)
	cs = pattern(every, t0, p, ms10)
	_, s = feed(every, cs)
	var w word
	downs := 0
	for i := range 25 {
		from := at(time.Duration(i) * time.Minute)
		st := judgeWeb(summarize(cs, s.runs, ruleWeb, from, from.Add(80*time.Second)), limitsFor(10, true, false))
		if st == Down {
			downs++
		}
		w.step(from, st)
	}
	if downs < 5 {
		t.Errorf("60%% loss: %d down minutes of 25", downs)
	}
	if st, _ := w.state(); st == Good {
		t.Error("60% loss: the word stayed good")
	}
	if ds := tileDrops([]runSet{s}, nil, at(time.Hour)); ds != nil {
		t.Errorf("60%% loss: drops %+v", ds)
	}
}

// TestWordOutageInsideGoodRun: an outage that clears inside the good
// minutes that end a Slow run: the run ends when the outage cleared, not
// before it.
func TestWordOutageInsideGoodRun(t *testing.T) {
	var w word
	steps(&w, t0, "sss")
	steps(&w, at(3*time.Minute), "gg")
	w.outageSignal(failing, at(5*time.Minute+10*time.Second))
	w.outageSignal(cleared, at(5*time.Minute+55*time.Second))
	rs := steps(&w, at(5*time.Minute), "ggg")
	if len(rs) != 1 || !rs[0].Start.Equal(t0) || !rs[0].End.Equal(at(5*time.Minute+55*time.Second)) || rs[0].Sev != Down {
		t.Errorf("runs %+v", rs)
	}
}

// TestLiveVsReplayClose: a UDP run 3:00–3:10 lost, OK from 3:15, after
// lost bursts at 1:40 and 2:55. Live, results arrive up to 6 s after their slot and
// the engine summarizes minute 2 at its first grid tick at or after
// closeAt + holdFor; replay sees all checks at once. Both judge alike.
func TestLiveVsReplayClose(t *testing.T) {
	const every = 5 * time.Second
	cs := pattern(every, t0, rep(".", 20)+"x"+rep(".", 14)+"x"+"xxx"+rep(".", 20), ms10)
	judge := func(runs []failRun) State {
		var ms []minute
		for i := range 3 {
			f := at(time.Duration(i) * time.Minute)
			ms = append(ms, summarize(cs, runs, ruleUDP, f, f.Add(80*time.Second)))
		}
		st, _ := judgeCall(ms, every)
		return st
	}
	_, s := feed(every, cs)
	replay := judge(s.runs)
	now := at(3*time.Minute + 20*time.Second).Add(holdFor(every)).Truncate(5 * time.Second)
	if now.Before(at(3*time.Minute + 20*time.Second).Add(holdFor(every))) {
		now = now.Add(5 * time.Second)
	}
	tr := &tracker{every: every}
	for _, c := range cs {
		if !c.at().Add(6 * time.Second).After(now) { // arrived by now
			tr.add(c)
		}
	}
	runs := tr.flush(now)
	if r, ok := tr.open(); ok {
		runs = append(runs, r)
	}
	if live := judge(runs); live != replay || live != Slow {
		t.Errorf("live %s, replay %s, want slow (2 of 36 lost)", live, replay)
	}
}

// TestImmediateAllocs: the tick path allocates nothing.
func TestImmediateAllocs(t *testing.T) {
	var ms [][]*tracker
	for range 3 {
		tr, _ := series(15*time.Second, t0, "....xx")
		ms = append(ms, []*tracker{tr})
	}
	if n := testing.AllocsPerRun(100, func() { immediate(ms) }); n != 0 {
		t.Errorf("immediate: %v allocs", n)
	}
	cs := pattern(time.Second, t0, rep(".", 50)+rep("x", 10), ms10)
	if n := testing.AllocsPerRun(100, func() { summarize(cs, nil, rulePath, t0, at(80*time.Second)) }); n != 0 {
		t.Errorf("summarize: %v allocs", n)
	}
}
