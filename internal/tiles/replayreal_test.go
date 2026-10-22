package tiles

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/profile"
	"github.com/fuck-you-isp/fyisp/internal/store"
)

// TestReplayReal (FYISP_REPLAY_DB=<a copy of fyisp.db>) replays a real
// database with the live instance's profile (--profile all,calls; set
// FYISP_REPLAY_PROFILE for another) up to its newest sample and prints a
// summary: per tile the minutes in each state, the word changes per day
// and the drops; the connection drops; the problems with their causes;
// what happened during the speed tests. It runs the replay twice: the two
// must be identical. FYISP_REPLAY_INCIDENTS=off leaves the stored verdict
// incidents out; FYISP_REPLAY_AT=<RFC 3339> adds the raw checks around a
// time. Never point it at a running instance's files.
func TestReplayReal(t *testing.T) {
	path := os.Getenv("FYISP_REPLAY_DB")
	if path == "" {
		t.Skip("set FYISP_REPLAY_DB")
	}
	ctx := context.Background()
	st, err := store.OpenReadOnly(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	names := []string{"all", "calls"}
	if s := os.Getenv("FYISP_REPLAY_PROFILE"); s != "" {
		names = strings.Split(s, ",")
	}
	r, err := profile.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	p, err := r.Resolve(names, profile.ResolveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	rts := profile.ResolveTiles(p)
	probe := newEngine(p, rts, Sources{}, Options{})
	infos, err := st.Series(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var end time.Time
	for _, in := range infos {
		if _, ok := probe.index[in.Key]; ok && in.Last.After(end) {
			end = in.Last
		}
	}
	end = end.Add(time.Minute).Truncate(tickEvery)
	run := func() (*engine, time.Duration) {
		src := Sources{Store: st, SpeedWindows: st.SpeedWindows, Incidents: st.Incidents,
			Verdict: func() model.Verdict { return model.Verdict{Kind: model.VerdictOK} }}
		if os.Getenv("FYISP_REPLAY_INCIDENTS") == "off" { // the tiles' own attribution only
			src.Incidents = nil
		}
		e := newEngine(p, rts, src, Options{})
		a := time.Now()
		if err := e.replay(ctx, end); err != nil {
			t.Fatal(err)
		}
		e.goLive(ctx, end)
		return e, time.Since(a)
	}
	e, took := run()
	e2, took2 := run()
	if !reflect.DeepEqual(e.Snapshot(), e2.Snapshot()) {
		t.Error("two replays differ")
	}
	tz := time.UTC
	f := func(x time.Time) string { return x.In(tz).Format("01-02 15:04:05") }
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }
	snap := e.Snapshot()
	from := end.Add(-problemsFor)
	w("replay up to %s UTC: %v and %v; %d tracked series; late samples %d; learning %v; card %s",
		f(end), took.Round(time.Millisecond), took2.Round(time.Millisecond), len(e.ss), e.LateSamples(), snap.Learning, snap.Conn.Kind)
	sp := e.speed()
	a := time.Now()
	for i := 1; i <= 20; i++ {
		e2.advance(ctx, end.Add(time.Duration(i)*tickEvery))
	}
	w("live tick with snapshot: %v", (time.Since(a) / 20).Round(time.Microsecond))

	w("\n== tiles: minutes per state (last 7 days); word changes per UTC day; drops")
	v := e.view(end)
	for i, tl := range e.tiles {
		if tl.off {
			w("%-9s off", tl.def.ID)
			continue
		}
		count := map[State]int{}
		speedBad, speedMins := 0, 0
		for m := from.Truncate(time.Minute); m.Before(e.nextMin); m = m.Add(time.Minute) {
			s := tl.minute(m)
			count[s]++
			if sp != nil && sp.Overlaps(m, m.Add(time.Minute-time.Nanosecond)) {
				speedMins++
				if s.Bad() {
					speedBad++
				}
			}
		}
		days := map[string]int{}
		runs := slices.Clone(tl.runs)
		if r, ok := tl.w.open(); ok {
			runs = append(runs, r)
		}
		for _, r := range runs {
			if r.Start.Before(from) {
				continue
			}
			days[r.Start.In(tz).Format("01-02")]++
			if !r.End.IsZero() {
				days[r.End.In(tz).Format("01-02")]++
			}
		}
		var ds []string
		for _, d := range slices.Sorted(maps.Keys(days)) {
			ds = append(ds, fmt.Sprintf("%s:%d", d, days[d]))
		}
		var drops []string
		for _, d := range v.tileDrops[i] {
			drops = append(drops, fmt.Sprintf("%s %v", f(d.Start), d.Dur))
		}
		w("%-9s good %5d slow %4d down %4d none %5d | speed-test minutes %d, bad %d | changes %s | drops %d %v",
			tl.def.ID, count[Good], count[Slow], count[Down], count[None], speedMins, speedBad, strings.Join(ds, " "), len(drops), drops)
	}

	w("\n== connection drops")
	for _, d := range v.connDrops {
		w("%s..%s %v", f(d.Start), f(d.End), d.Dur)
	}
	if c := v.connCur; c != nil {
		w("in progress since %s", f(c.Start))
	}

	w("\n== problems (%d), newest first", len(snap.Eps))
	perDay := map[string]int{}
	for _, x := range snap.Eps {
		end := "ongoing"
		dur := "-"
		if !x.Ongoing() {
			end, dur = f(x.End), x.End.Sub(x.Start).Round(time.Second).String()
			perDay[x.End.In(tz).Format("01-02")]++
		}
		st := ""
		if sp != nil && sp.Overlaps(x.Start, x.End) {
			st = " [speed test]"
		}
		w("%s..%s %8s %-4s %-6s conn=%-5v %-28s drop=%-5v verdict=%-10s cause=%s/%s/%s%s",
			f(x.Start), end, dur, x.Sev, x.Kind, x.Connection, strings.Join(x.Tiles, ","), x.DropDur, x.Verdict, x.Cause.Who, x.Cause.Vendor, x.Cause.Evidence, st)
	}
	var pd []string
	for _, d := range slices.Sorted(maps.Keys(perDay)) {
		pd = append(pd, fmt.Sprintf("%s:%d", d, perDay[d]))
	}
	w("problems ended per UTC day: %s", strings.Join(pd, " "))
	w("last 24 h: %+v", snap.Day)

	w("\n== speed tests in the last 7 days")
	tests, err := st.SpeedTests(ctx, from, end)
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range tests {
		var hit []string
		for _, ep := range snap.Eps {
			if ep.window().overlaps(window{x.Start, x.End.Add(2 * time.Second)}, end) {
				hit = append(hit, fmt.Sprintf("%s(%s,%s)", ep.ID, ep.Kind, ep.Cause.Who))
			}
		}
		w("%s..%s %-9s %s overlapping problems %v", f(x.Start), x.End.In(tz).Format("15:04:05"), x.Outcome, x.Trigger, hit)
	}
	if at := os.Getenv("FYISP_REPLAY_AT"); at != "" { // the raw checks around a time, per minute
		x, err := time.Parse(time.RFC3339, at)
		if err != nil {
			t.Fatal(err)
		}
		w("\n== checks per minute around %s (ok/lost)", f(x))
		lo, hi := x.Add(-5*time.Minute).Truncate(time.Minute), x.Add(5*time.Minute)
		var keys []model.SeriesKey
		for _, s := range e.ss {
			if s.key.Kind != model.KindUDP {
				keys = append(keys, s.key)
			} else {
				keys = append(keys, s.key, model.SeriesKey{Target: s.key.Target, Kind: model.KindUDPLoss})
			}
		}
		type cnt struct{ ok, lost int }
		rows := map[model.SeriesKey][]cnt{}
		n := int(hi.Sub(lo) / time.Minute)
		err = st.Raw(ctx, keys, lo, hi, func(p store.RawPoint) error {
			if rows[p.Key] == nil {
				rows[p.Key] = make([]cnt, n+1)
			}
			c := &rows[p.Key][int(p.TS.Sub(lo)/time.Minute)]
			switch {
			case p.Key.Kind == model.KindUDPLoss && !p.Lost && p.RTTms > 0:
				c.lost++ // a partly lost burst
			case p.Key.Kind == model.KindUDPLoss:
			case p.Lost && p.Reason != model.ReasonGap:
				c.lost++
			case !p.Lost:
				c.ok++
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, k := range keys {
			var cs []string
			for _, c := range rows[k] {
				cs = append(cs, fmt.Sprintf("%2d/%-2d", c.ok, c.lost))
			}
			w("%-28s %-10s %s", k.Target, k.Kind, strings.Join(cs, " "))
		}
	}
	t.Log("\n" + b.String())
}
