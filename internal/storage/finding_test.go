package storage

import (
	"strings"
	"testing"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

// TestExplain: what a campaign points to, from its rounds and attempts.
func TestExplain(t *testing.T) {
	camp := model.StorageCampaign{Targets: []model.StorageCampaignTarget{{ID: "a", Name: "A"}, {ID: "b", Name: "B"}}}
	f := func(v float64) *float64 { return &v }
	rounds := func(n int, loss float64) []model.StorageRound {
		var out []model.StorageRound
		for i := range n {
			s := i
			out = append(out, model.StorageRound{ID: int64(i + 1), Seq: &s, Outcome: model.RoundOK, GwLoss: f(loss), EdgeLoss: f(0)})
		}
		return out
	}
	unmeasured := func(n int) []model.StorageRound {
		rs := rounds(n, 0)
		for i := range rs {
			rs[i].GwLoss, rs[i].EdgeLoss = nil, nil
		}
		return rs
	}
	ok := func(n int) []AttemptCount {
		var out []AttemptCount
		for r := 1; r <= n; r++ {
			out = append(out, AttemptCount{Round: int64(r), Target: "a", Phase: "cold", Class: "ok", N: 10},
				AttemptCount{Round: int64(r), Target: "b", Phase: "cold", Class: "ok", N: 10})
		}
		return out
	}
	fail := func(cs []AttemptCount, target, class string, rs ...int64) []AttemptCount {
		for _, r := range rs {
			cs = append(cs, AttemptCount{Round: r, Target: target, Phase: "cold", Class: class, EdgePrefix: "52.216.12.0/24", N: 2})
		}
		return cs
	}
	for _, c := range []struct {
		name, kicker, text string
		rounds             []model.StorageRound
		counts             []AttemptCount
		live               bool
	}{
		{"nothing ran", "early", "No round has run yet.", nil, nil, false},
		{"few rounds", "early", "So far: no failures in 3 rounds", rounds(3, 0), ok(3), true},
		{"clean", "none", "No failures: every one of 120 attempts in 6 rounds worked.", rounds(6, 0), ok(6), false},
		{"keys refused", "config", "The only failures were refusals", rounds(6, 0), fail(ok(6), "a", "http_auth", 1, 2), false},
		{"one path", "path", "The failures were on A while your line was clean", rounds(6, 0), fail(ok(6), "a", "tls_timeout", 1, 3, 5), false},
		{"one path, line unmeasured", "path", "The failures were on A and not every target failed at once", unmeasured(6), fail(ok(6), "a", "tls_timeout", 1, 3, 5), false},
		{"a little loss isn't the line", "path", "The failures were on A", rounds(6, 0.02), fail(ok(6), "a", "tls_timeout", 1, 3, 5), false},
		{"the line", "line", "packet loss on your own line", rounds(6, 0.05), fail(ok(6), "a", "connect_timeout", 2, 4), false},
		{"everywhere", "line", "All the targets failed in the same rounds", rounds(6, 0), fail(fail(ok(6), "a", "connect_reset", 2, 4), "b", "connect_reset", 2, 4), false},
		{"provider", "provider", "provider's own answers", rounds(6, 0), fail(ok(6), "a", "http_5xx", 1, 2, 3), false},
	} {
		got := Explain(camp, c.rounds, c.counts, c.live)
		if got.Kicker != c.kicker || !strings.Contains(got.Text, c.text) {
			t.Errorf("%s: %s %q", c.name, got.Kicker, got.Text)
		}
		if c.name == "one path" && !strings.Contains(strings.Join(got.Evidence, " "), "6 of the 6 failures went to 52.216.12.x.") {
			t.Errorf("%s: evidence %q", c.name, got.Evidence)
		}
	}
}
