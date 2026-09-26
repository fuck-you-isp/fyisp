//go:build unix

package store

import (
	"bufio"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

// Crash test: a child process observes samples on a simulated clock running
// 1800x real time and flushes every real second, printing the simulated time
// covered by each successful flush; the parent SIGKILLs it at random
// moments, reopens the store, and checks integrity and that everything up
// to the last reported flush is there, exactly. The child resumes each
// series after its last stored slot. FYISP_CRASH=1 runs 25 kills (the
// default is 3).
const (
	crashSeries = 20
	crashSpeed  = 1800
	crashKeep   = 6 * time.Hour
)

func crashKey(i int) model.SeriesKey {
	return model.SeriesKey{Target: fmt.Sprintf("t%02d", i/3), Kind: model.ProbeKind(1 + i%3)}
}

// crashSample is the deterministic content of series i at t.
func crashSample(i int, t time.Time) model.Sample {
	v := (int64(i)*7919 + t.Unix()*104729) % 100_000
	s := model.Sample{Key: crashKey(i), Slot: t}
	if v%97 == 0 {
		s.Lost, s.Reason = true, model.Reason(v/97%16)
	} else {
		s.RTT = time.Duration(v) * 10 * time.Microsecond
	}
	return s
}

func TestCrashChild(t *testing.T) {
	dir := os.Getenv("FYISP_CRASH_CHILD")
	if dir == "" {
		t.Skip("child process only")
	}
	var simMs atomicInt
	s, err := Open(dir, Options{Interval: func(model.SeriesKey) time.Duration { return time.Second },
		Now: func() time.Time { return time.UnixMilli(simMs.get()) }})
	if err != nil {
		fmt.Println("error", err)
		os.Exit(1)
	}
	next := make([]time.Time, crashSeries)
	for i := range next {
		next[i] = t0
	}
	ss, err := s.Series(ctx)
	if err != nil {
		fmt.Println("error", err)
		os.Exit(1)
	}
	sim := t0
	for _, si := range ss {
		for i := range next {
			if crashKey(i) == si.Key {
				next[i] = si.Last.Add(time.Second)
				if si.Last.After(sim) {
					sim = si.Last
				}
			}
		}
	}
	start, lastFlush, lastPrune := time.Now(), time.Now(), sim
	simStart := sim
	for {
		want := simStart.Add(time.Since(start) * crashSpeed)
		for ; sim.Before(want); sim = sim.Add(time.Second) {
			for i := range next {
				if !next[i].After(sim) {
					s.Observe(crashSample(i, sim))
					next[i] = sim.Add(time.Second)
				}
			}
		}
		simMs.set(sim.UnixMilli())
		if time.Since(lastFlush) >= time.Second {
			covered := sim.Add(-time.Second) // every series observed up to here
			if err := s.Flush(ctx); err != nil {
				fmt.Println("error", err)
				os.Exit(1)
			}
			lastFlush = time.Now()
			fmt.Printf("flushed %d\n", covered.UnixMilli())
			if sim.Sub(lastPrune) >= time.Hour {
				if err := s.Prune(ctx, sim.Add(-crashKeep)); err != nil {
					fmt.Println("error", err)
					os.Exit(1)
				}
				lastPrune = sim
			}
		}
		time.Sleep(time.Millisecond)
	}
}

func TestCrash(t *testing.T) {
	kills := 3
	if os.Getenv("FYISP_CRASH") == "1" {
		kills = 25
	}
	dir := t.TempDir()
	rnd := rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), 1))
	var maxLostWall time.Duration
	var flushes int
	for it := range kills {
		cmd := exec.Command(os.Args[0], "-test.run=^TestCrashChild$")
		cmd.Env = append(os.Environ(), "FYISP_CRASH_CHILD="+dir)
		out, _ := cmd.StdoutPipe()
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		type rep struct {
			ms   int64
			wall time.Time
		}
		reports := make(chan rep, 1024)
		go func() {
			defer close(reports)
			sc := bufio.NewScanner(out)
			for sc.Scan() {
				if v, ok := strings.CutPrefix(sc.Text(), "flushed "); ok {
					ms, _ := strconv.ParseInt(v, 10, 64)
					reports <- rep{ms, time.Now()}
				} else if strings.HasPrefix(sc.Text(), "error") {
					t.Error(sc.Text())
				}
			}
		}()
		// Wait for the first flush, then kill at a random moment; every
		// other kill aims at the next flush (the child flushes 1s after the
		// previous one) to land during the write transaction.
		var last rep
		select {
		case last = <-reports:
			flushes++
		case <-time.After(60 * time.Second):
			cmd.Process.Kill()
			t.Fatal("child made no progress")
		}
		collect := func(until time.Time) {
			for {
				select {
				case r := <-reports:
					last = r
					flushes++
				case <-time.After(time.Until(until)):
					return
				}
			}
		}
		collect(time.Now().Add(time.Duration(300+rnd.IntN(2700)) * time.Millisecond))
		if it%2 == 1 {
			collect(last.wall.Add(time.Second + time.Duration(rnd.IntN(15000))*time.Microsecond))
		}
		cmd.Process.Signal(syscall.SIGKILL)
		killed := time.Now()
		cmd.Wait()
		for r := range reports {
			last = r
			flushes++
		}
		lostWall := killed.Sub(last.wall)
		maxLostWall = max(maxLostWall, lostWall)

		s := openT(t, dir, Options{Interval: func(model.SeriesKey) time.Duration { return time.Second },
			Now: func() time.Time { return time.UnixMilli(last.ms) }})
		if ic := pragmaStr(t, s.db.w, "integrity_check"); ic != "ok" {
			t.Fatalf("kill %d: integrity_check %s", it, ic)
		}
		if open := unsummarized(t, s, true); open != 0 {
			t.Fatalf("kill %d: %d closed hours without summary after reopen", it, open)
		}
		flushedTo := time.UnixMilli(last.ms).UTC()
		from := flushedTo.Add(-crashKeep + time.Hour).Truncate(time.Hour)
		if from.Before(t0) {
			from = t0
		}
		var checked, beyond int
		for i := range crashSeries {
			k := crashKey(i)
			want := from
			err := s.Raw(ctx, []model.SeriesKey{k}, from, flushedTo.Add(time.Hour), func(p RawPoint) error {
				if p.TS.After(flushedTo) {
					beyond++
					return nil
				}
				x := crashSample(i, p.TS)
				if !p.TS.Equal(want) || p.Lost != x.Lost || p.Reason != x.Reason || (!x.Lost && p.RTTms != float64(x.RTT)/1e6) {
					return fmt.Errorf("series %v at %v: got %+v, want slot %v %+v", k, p.TS, p, want, x)
				}
				want = want.Add(time.Second)
				checked++
				return nil
			})
			if err != nil {
				t.Fatalf("kill %d: %v", it, err)
			}
			if want.Before(flushedTo.Add(time.Second)) {
				t.Fatalf("kill %d: series %v stored up to %v, but a flush up to %v was reported", it, k, want.Add(-time.Second), flushedTo)
			}
		}
		st, _ := s.Stats(ctx)
		t.Logf("kill %2d: flushed to sim %s, %6d samples verified (+%d written after the last report), wall time since last flush %4dms, db+wal %d KB",
			it, flushedTo.Format("01-02 15:04:05"), checked, beyond, lostWall.Milliseconds(), st.FileBytes>>10)
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("%d kills, %d flushes: integrity ok every time; at most %v of wall time after the last successful flush (<= 60s required)", kills, flushes, maxLostWall)
	if maxLostWall > 60*time.Second {
		t.Fatal("lost more than 60s")
	}
}
