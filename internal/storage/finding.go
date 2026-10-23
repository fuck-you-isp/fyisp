package storage

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/s3test"
)

// Finding is what a campaign's rounds point to, in words: a kicker
// ("path", "line", "provider", "config", "none", "early"), a sentence and
// the evidence for it.
type Finding struct {
	Kicker   string
	Text     string
	Evidence []string
}

// AttemptCount is how many attempts of a round, target, phase and class
// went to an edge /24 (store.StorageCells' rows).
type AttemptCount struct {
	Round                int64
	Target, Phase, Class string
	EdgePrefix           string
	N                    int
}

// Finding thresholds.
const (
	findingRounds = 5 // rounds before anything but "too early" (refusals excepted)
	// LineLoss is the loss on the line (the router, the provider's first
	// router) that counts in a round: 3% (a lost probe or two of fewer
	// than 20 says nothing; the glue leaves such rounds unmeasured).
	LineLoss = 0.03
)

// Explain reads a campaign's rounds and attempt counts (live: it still
// runs, so the words say "so far").
func Explain(c model.StorageCampaign, rounds []model.StorageRound, counts []AttemptCount, live bool) Finding {
	f := explain(c, rounds, counts, live)
	if f.Text != "" {
		f.Text = strings.ToUpper(f.Text[:1]) + f.Text[1:]
	}
	return f
}

func explain(c model.StorageCampaign, rounds []model.StorageRound, counts []AttemptCount, live bool) Finding {
	name := map[string]string{}
	for _, t := range c.Targets {
		name[t.ID] = t.Name
	}
	ran := map[int64]model.StorageRound{}
	for _, r := range rounds {
		if r.Seq != nil && r.Outcome != model.RoundSkipped && r.Outcome != model.RoundCancelled {
			ran[r.ID] = r
		}
	}
	type tally struct{ n, failed, provider, config int }
	per := map[string]*tally{}
	classes := map[string]int{}
	prefixes := map[string]int{}
	failRounds := map[int64]map[string]bool{} // round -> targets that failed in it
	total := 0
	for _, a := range counts {
		if _, ok := ran[a.Round]; !ok {
			continue
		}
		t := per[a.Target]
		if t == nil {
			t = &tally{}
			per[a.Target] = t
		}
		t.n += a.N
		total += a.N
		switch s3test.Class(a.Class).Group() {
		case s3test.GroupNetwork:
			t.failed += a.N
		case s3test.GroupProvider:
			t.failed += a.N
			t.provider += a.N
		case s3test.GroupConfig:
			t.config += a.N
			continue
		default:
			continue
		}
		classes[a.Class] += a.N
		if a.EdgePrefix != "" {
			prefixes[a.EdgePrefix] += a.N
		}
		if failRounds[a.Round] == nil {
			failRounds[a.Round] = map[string]bool{}
		}
		failRounds[a.Round][a.Target] = true
	}
	so := ""
	if live {
		so = "So far: "
	}
	failed, provider, config := 0, 0, 0
	for _, t := range per {
		failed, provider, config = failed+t.failed, provider+t.provider, config+t.config
	}
	ids := slices.Sorted(func(yield func(string) bool) {
		for id := range per {
			if !yield(id) {
				return
			}
		}
	})
	var ev []string
	for _, id := range ids {
		t := per[id]
		if t.failed > 0 || t.config > 0 {
			ev = append(ev, fmt.Sprintf("%s: %d of %d attempts failed%s.", cmp.Or(name[id], id), t.failed+t.config, t.n, configNote(t.config)))
		}
	}
	if top := topKeys(classes, 2); len(top) > 0 {
		var parts []string
		for _, k := range top {
			parts = append(parts, fmt.Sprintf("%d %s", classes[k], strings.ReplaceAll(k, "_", " ")))
		}
		ev = append(ev, "Most often: "+strings.Join(parts, ", ")+".")
	}
	switch {
	case len(ran) == 0:
		return Finding{Kicker: "early", Text: "no round has run yet."}
	case failed == 0 && config > 0:
		return Finding{Kicker: "config", Text: so + "the only failures were refusals of the keys, the clock or the settings: fix them and test again; they aren't network problems.", Evidence: ev}
	case failed == 0 && len(ran) < findingRounds:
		return Finding{Kicker: "early", Text: fmt.Sprintf("%sno failures in %d round%s. A few more rounds tell more.", so, len(ran), plural(len(ran))), Evidence: ev}
	case failed == 0:
		return Finding{Kicker: "none", Text: fmt.Sprintf("%sno failures: every one of %d attempts in %d rounds worked.", so, total, len(ran)), Evidence: ev}
	case len(ran) < findingRounds:
		return Finding{Kicker: "early", Text: fmt.Sprintf("%s%d failure%s in %d round%s. A few more rounds tell where they come from.", so, failed, plural(failed), len(ran), plural(len(ran))), Evidence: ev}
	}
	// Where the failures are: with loss on the line, everywhere at once, at
	// the provider's answers, or on one path.
	lossy, both, measured := 0, 0, 0
	for rid, ts := range failRounds {
		r := ran[rid]
		if r.GwLoss != nil || r.EdgeLoss != nil {
			measured++
		}
		if (r.GwLoss != nil && *r.GwLoss >= LineLoss) || (r.EdgeLoss != nil && *r.EdgeLoss >= LineLoss) {
			lossy++
		}
		if len(ts) >= 2 && len(ts) == len(c.Targets) {
			both++
		}
	}
	nf := len(failRounds)
	if p := topKeys(prefixes, 1); len(p) == 1 && prefixes[p[0]]*2 > failed && failed >= 3 {
		ev = append(ev, fmt.Sprintf("%d of the %d failures went to %s.", prefixes[p[0]], failed, strings.Replace(p[0], ".0/24", ".x", 1)))
	}
	switch {
	case lossy*2 >= nf:
		ev = append(ev, fmt.Sprintf("Your router or your provider's first router lost packets in %d of the %d rounds with failures.", lossy, nf))
		return Finding{Kicker: "line", Text: so + "the failures came with packet loss on your own line in the same rounds: they point to your connection, not the storage provider.", Evidence: ev}
	case len(c.Targets) >= 2 && both*2 >= nf:
		ev = append(ev, fmt.Sprintf("Every target failed together in %d of the %d rounds with failures.", both, nf))
		return Finding{Kicker: "line", Text: so + "all the targets failed in the same rounds: the problem is nearer to you than any one provider.", Evidence: ev}
	case provider*2 >= failed:
		return Finding{Kicker: "provider", Text: so + "most failures were the provider's own answers (server errors or slow-downs): they point to the provider.", Evidence: ev}
	}
	var worst []string
	for _, id := range ids {
		if per[id].failed > 0 {
			worst = append(worst, cmp.Or(name[id], id))
		}
	}
	clean := lossy == 0 && measured == nf
	if clean {
		ev = append(ev, "Your line showed no packet loss in the rounds with failures.")
	}
	while := " while your line was clean"
	if !clean {
		while = " and not every target failed at once"
	}
	return Finding{Kicker: "path", Text: fmt.Sprintf("%sthe failures were on %s%s: they point to the path to %s or the endpoint itself.",
		so, strings.Join(worst, " and "), while, map[bool]string{true: "that endpoint", false: "those endpoints"}[len(worst) == 1]), Evidence: ev}
}

func configNote(n int) string {
	if n == 0 {
		return ""
	}
	return fmt.Sprintf(" (%d refused: keys, clock or settings)", n)
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// topKeys is m's n biggest keys, biggest first (ties by key).
func topKeys(m map[string]int, n int) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	slices.SortFunc(ks, func(a, b string) int { return cmp.Or(m[b]-m[a], strings.Compare(a, b)) })
	return ks[:min(n, len(ks))]
}
