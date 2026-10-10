package verdict

import (
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/profile"
)

// TestDefaultProfileServiceVsUpstream pins how the v0.6 default (18
// internet targets in three groups: call apps, work apps, websites) splits
// "some services" from "beyond your ISP": upstream needs at least half of
// the internet targets, in more than one group.
func TestDefaultProfileServiceVsUpstream(t *testing.T) {
	def, err := profile.Default()
	if err != nil {
		t.Fatal(err)
	}
	byGroup := map[string][]string{}
	inet := 0
	for _, tg := range def.Targets {
		if tg.Layer == "" && !tg.IsUDP() {
			byGroup[tg.Group] = append(byGroup[tg.Group], tg.Name)
			inet++
		}
	}
	if inet < 12 || len(byGroup) < 3 {
		t.Fatalf("default: %d internet targets in %d groups", inet, len(byGroup))
	}
	work, web := byGroup["work"], byGroup["web"]
	if len(work) < 4 || len(web) < 3 {
		t.Fatalf("groups %v", byGroup)
	}
	half := (inet + 1) / 2
	cross := append(append([]string{}, work...), web[:half-len(work)]...)
	for _, tc := range []struct {
		name string
		down []string
		want model.VerdictKind
	}{
		{"one website", web[:1], model.VerdictService},
		{"most of one group", work[:4], model.VerdictService},
		{"a whole group", work, model.VerdictService},
		{"just under half, two groups", cross[:len(cross)-1], model.VerdictService},
		{"half, two groups", cross, model.VerdictUpstream},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.p = def
			h.with(targets(always(model.ReasonTimeout), tc.down...))
			h.run(3 * time.Minute)
			if v := h.e.Current(); v.Kind != tc.want {
				t.Errorf("%d of %d down: %s (%s)", len(tc.down), inet, v.Kind, v.Summary)
			}
		})
	}
}
