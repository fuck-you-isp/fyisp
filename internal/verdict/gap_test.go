package verdict

import (
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

// masked returns every probe in [t0+from, t0+to) as "not measured", as the
// speed test mask does during a sweep; stray, if set, is a real sample
// every so often inside the span.
func masked(from, to time.Duration, stray func(time.Time) (outcome, bool)) behavior {
	return func(_ model.Target, _ model.ProbeKind, at time.Time) (outcome, bool) {
		if at.Before(t0.Add(from)) || !at.Before(t0.Add(to)) {
			return outcome{}, false
		}
		if stray != nil {
			if o, ok := stray(at); ok {
				return o, true
			}
		}
		return outcome{lost: true, reason: model.ReasonGap}, true
	}
}

// TestMaskedSpanHoldsVerdict: a speed test sweep masks every probe for
// minutes. Not measured is neither loss nor "no network": no incident
// starts, and the verdict (ok, or a problem in progress) holds.
func TestMaskedSpanHoldsVerdict(t *testing.T) {
	lossyEdge := []behavior{
		layer(model.LayerEdge, every(2, model.ReasonTimeout)),
		internet(every(2, model.ReasonTimeout)),
	}
	strayNoNet := func(at time.Time) (outcome, bool) {
		return outcome{lost: true, reason: model.ReasonNoNetwork}, at.Unix()%40 == 0
	}
	for _, c := range []struct {
		name   string
		before []behavior
		stray  func(time.Time) (outcome, bool)
		kind   model.VerdictKind
	}{
		{name: "ok holds", kind: model.VerdictOK},
		{name: "ok holds with stray lost samples", stray: strayNoNet, kind: model.VerdictOK},
		{name: "isp problem holds", before: lossyEdge, kind: model.VerdictISP},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			h.fx = c.before
			h.run(4 * time.Minute)
			v := h.e.Current()
			if v.Kind != c.kind {
				t.Fatalf("before the mask: %s", v.Kind)
			}
			incidents := len(h.st.all())
			h.seen = map[model.VerdictKind]bool{}
			h.with(masked(4*time.Minute, 8*time.Minute, c.stray))
			h.run(4 * time.Minute)
			for k := range h.seen {
				if k != c.kind {
					t.Errorf("verdict %s seen during the mask", k)
				}
			}
			if w := h.e.Current(); w.Kind != c.kind || !w.Since.Equal(v.Since) {
				t.Errorf("after the mask %+v, want %s since %v", w, c.kind, v.Since)
			}
			in := h.st.all()
			if len(in) != incidents {
				t.Fatalf("incidents %+v", in)
			}
			for _, x := range in {
				if x.Kind == model.VerdictNoNetwork || (c.kind != model.VerdictOK && !x.End.IsZero()) {
					t.Errorf("incident %+v", x)
				}
			}
		})
	}
}

// TestMaskedThenRealLoss: after a masked span, real loss is judged as
// usual.
func TestMaskedThenRealLoss(t *testing.T) {
	for _, c := range []struct {
		name  string
		after []behavior
		kind  model.VerdictKind
	}{
		{"lan", []behavior{
			layer(model.LayerGateway, every(2, model.ReasonTimeout)),
			layer(model.LayerEdge, every(2, model.ReasonTimeout)),
			internet(every(2, model.ReasonTimeout)),
		}, model.VerdictLAN},
		{"no network", []behavior{
			layer(model.LayerGateway, always(model.ReasonNoNetwork)),
			layer(model.LayerEdge, always(model.ReasonNoNetwork)),
			layer(model.LayerAnycast, always(model.ReasonNoNetwork)),
			internet(always(model.ReasonNoNetwork)),
		}, model.VerdictNoNetwork},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			h.run(2 * time.Minute)
			h.with(masked(2*time.Minute, 5*time.Minute+30*time.Second, nil))
			h.run(3*time.Minute + 30*time.Second)
			if len(h.st.all()) != 0 {
				t.Fatalf("incident from the mask: %+v", h.st.all())
			}
			h.fx = c.after
			h.run(2 * time.Minute)
			if v := h.e.Current(); v.Kind != c.kind {
				t.Fatalf("verdict %+v, want %s", v, c.kind)
			}
			// The first measured minute may pass through another kind
			// (the last healthy slot is still in the window); none starts
			// before the mask ends.
			in := h.st.all()
			if len(in) == 0 || in[len(in)-1].Kind != c.kind {
				t.Fatalf("incidents %+v", in)
			}
			for _, x := range in {
				if x.Start.Before(t0.Add(5*time.Minute + 30*time.Second)) {
					t.Fatalf("incident during the mask: %+v", x)
				}
			}
		})
	}
}

// TestOutageDuringSweep: a real outage while a sweep runs. Its tests fail
// fast, so every 6 s 2 s are masked and the 4 s between lose every probe
// with "no network": each window is about a third masked, yet the outage is
// no_network, not held at an early transitional kind. (Found by ux-eng.)
func TestOutageDuringSweep(t *testing.T) {
	h := newHarness(t)
	h.run(3 * time.Minute)
	h.fx = []behavior{func(_ model.Target, _ model.ProbeKind, at time.Time) (outcome, bool) {
		if at.Sub(t0)%(6*time.Second) < 2*time.Second {
			return outcome{lost: true, reason: model.ReasonGap}, true
		}
		return outcome{lost: true, reason: model.ReasonNoNetwork}, true
	}}
	h.run(2 * time.Minute)
	if v := h.e.Current(); v.Kind != model.VerdictNoNetwork {
		t.Fatalf("verdict %+v, want no_network", v)
	}
	if in := h.st.all(); len(in) == 0 || in[len(in)-1].Kind != model.VerdictNoNetwork || !in[len(in)-1].End.IsZero() {
		t.Fatalf("incidents %+v", in)
	}
}
