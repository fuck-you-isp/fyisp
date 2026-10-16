package trace

import (
	"context"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

func (tr *tracer) has(name string, mode model.TraceMode) bool {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return tr.targets[tkey{name, mode}] != nil
}

// A host outside the profile is a temporary target: traced under its
// reserved name while it has sessions, dropped when the last one ends, its
// route kept in memory (never reported to the sink) and still known after.
func TestInvestigateHost(t *testing.T) {
	f := newFakeNet("192.168.1.1", "62.1.1.1")
	inv := 20 * time.Millisecond
	var lookups atomic.Int32
	tr, sink := runTracer(t, f, Options{Interval: time.Hour, Investigate: inv,
		Resolve: func(_ context.Context, host string) ([]netip.Addr, error) {
			lookups.Add(1)
			if host != "example.com" {
				t.Errorf("resolved %q", host)
			}
			return addrs("5.5.5.5"), nil
		}}, labProfile())
	name := model.HostTarget("example.com")
	s, err := tr.Investigate(name, model.TraceICMP, 3)
	if err != nil {
		t.Fatal(err)
	}
	if !tr.has(name, model.TraceICMP) {
		t.Fatal("no temporary target")
	}
	waitFor(t, "rounds", func() bool { _, ended := s.Progress(); return !ended.IsZero() })
	waitFor(t, "dropped", func() bool { return !tr.has(name, model.TraceICMP) })
	smp, routes, changes := sink.take()
	n := 0
	for _, x := range smp {
		if x.Key.Target == name {
			n++
			if x.Key.Kind != model.KindTrace {
				t.Errorf("sample %+v", x.Key)
			}
		}
	}
	if n == 0 || lookups.Load() == 0 || len(f.callsTo(a("5.5.5.5"))) == 0 {
		t.Errorf("samples %d, lookups %d, probes %v", n, lookups.Load(), f.callsTo(a("5.5.5.5")))
	}
	for _, r := range routes {
		if r.Target == name {
			t.Errorf("host route reported to the sink: %+v", r)
		}
	}
	if len(changes) != 0 {
		t.Errorf("changes %+v", changes)
	}
	if r, ok := tr.Route(name, model.TraceICMP); !ok || len(r.Hops) != 3 || r.Hops[2] != a("5.5.5.5") {
		t.Errorf("route after the drop: %+v %v", r, ok)
	}
	for _, r := range tr.Routes() {
		if r.Target == name {
			t.Errorf("Routes lists the host: %+v", r)
		}
	}
	if got := tr.Interval(name, model.KindTrace); got != inv {
		t.Errorf("interval %v, want %v", got, inv)
	}
	// Traced again: a new temporary target.
	s2, err := tr.Investigate(name, model.TraceTCP, 2)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "tcp rounds", func() bool { _, ended := s2.Progress(); return !ended.IsZero() })
	waitFor(t, "tcp dropped", func() bool { return !tr.has(name, model.TraceTCP) })

	// The route is forgotten hostKeep after the drop.
	tr.mu.Lock()
	k := tkey{name, model.TraceICMP}
	g := tr.gone[k]
	g.at = g.at.Add(-hostKeep)
	tr.gone[k] = g
	tr.mu.Unlock()
	if _, ok := tr.Route(name, model.TraceICMP); ok {
		t.Error("route still known after hostKeep")
	}
}

// Every resolution of a host outside the profile is checked: when the name
// later resolves to a private address (DNS rebinding), its sessions end and
// nothing is sent there. Private address literals never get a probe.
func TestInvestigateHostRebinding(t *testing.T) {
	f := newFakeNet("192.168.1.1")
	var n atomic.Int32
	tr, _ := runTracer(t, f, Options{Interval: time.Hour, Investigate: 20 * time.Millisecond, ResolveEvery: time.Nanosecond,
		Resolve: func(context.Context, string) ([]netip.Addr, error) {
			if n.Add(1) <= 3 {
				return addrs("5.5.5.5"), nil
			}
			return addrs("5.5.5.5", "192.168.1.50"), nil // one bad answer is enough
		}}, labProfile())
	name := model.HostTarget("rebind.example")
	s, err := tr.Investigate(name, model.TraceICMP, 1000)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "stopped", func() bool { _, ended := s.Progress(); return !ended.IsZero() })
	if done, _ := s.Progress(); done >= 1000 || n.Load() <= 3 {
		t.Errorf("rounds %d, lookups %d", done, n.Load())
	}
	waitFor(t, "dropped", func() bool { return !tr.has(name, model.TraceICMP) })
	if c := f.callsTo(a("192.168.1.50")); len(c) != 0 {
		t.Errorf("probes to the private address: %v", c)
	}

	for _, lit := range []string{"10.0.0.1", "127.0.0.1", "100.64.0.1", "169.254.1.1", "224.0.0.1", "192.0.2.1"} {
		s, err := tr.Investigate(model.HostTarget(lit), model.TraceICMP, 1000)
		if err != nil {
			t.Fatal(err)
		}
		waitFor(t, lit+" stopped", func() bool { _, ended := s.Progress(); return !ended.IsZero() })
		if c := f.callsTo(a(lit)); len(c) != 0 {
			t.Errorf("%s: probes %v", lit, c)
		}
	}
}
