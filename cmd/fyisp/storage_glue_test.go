package main

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/asn"
	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/netinfo"
	"github.com/fuck-you-isp/fyisp/internal/trace"
)

// fakeTracer answers Investigate with a session that has ended and Route
// with route.
type fakeTracer struct {
	route model.Route
	asked string
}

type endedSession struct{}

func (endedSession) Stop()                      {}
func (endedSession) Progress() (int, time.Time) { return 24, time.Now() }

func (f *fakeTracer) Run(context.Context, *model.Profile, trace.Sink) error { return nil }
func (f *fakeTracer) Investigate(target string, _ model.TraceMode, _ int) (trace.Investigation, error) {
	f.asked = target
	return endedSession{}, nil
}
func (f *fakeTracer) Routes() []model.Route                             { return nil }
func (f *fakeTracer) Route(string, model.TraceMode) (model.Route, bool) { return f.route, true }
func (f *fakeTracer) Interval(string, model.ProbeKind) time.Duration    { return time.Second }
func (f *fakeTracer) Running() bool                                     { return true }

// TestStorageTraceHops: a storage traceroute keeps no address of a router
// on this network or in this connection's provider's network (one could
// be the user's public address), keeps the routers past it, and leaves
// out the traced address itself.
func TestStorageTraceHops(t *testing.T) {
	isp, transit, target := netip.MustParseAddr("73.0.0.1"), netip.MustParseAddr("4.2.2.1"), netip.MustParseAddr("52.216.10.10")
	if asn.Lookup(isp).ASN == 0 || asn.Lookup(transit).ASN == 0 || asn.Lookup(isp).ASN == asn.Lookup(transit).ASN {
		t.Skip("the built-in table doesn't know these addresses")
	}
	ft := &fakeTracer{route: model.Route{Hops: []netip.Addr{netip.MustParseAddr("192.168.1.1"), netip.MustParseAddr("100.64.0.1"),
		netip.MustParseAddr("73.0.0.2"), {}, transit, target}}}
	hops, handoff, ok := storageTrace(ft, func() netinfo.Path { return netinfo.Path{Edge: isp} })(context.Background(), target)
	if !ok || ft.asked != model.HostTarget(target.String()) {
		t.Fatalf("ok %v, asked %q", ok, ft.asked)
	}
	want := []model.StorageHop{{Owner: model.HopYourNetwork}, {Owner: model.HopYourNetwork}, {ASN: asn.Lookup(isp).ASN, Owner: model.HopYourProvider},
		{}, {IP: transit.String(), ASN: asn.Lookup(transit).ASN, Owner: asn.Lookup(transit).Owner}}
	if len(hops) != len(want) {
		t.Fatalf("hops %+v", hops)
	}
	for i := range want {
		if hops[i] != want[i] {
			t.Errorf("hop %d: %+v, want %+v", i, hops[i], want[i])
		}
	}
	if handoff != asn.Lookup(transit).ASN {
		t.Errorf("handoff %d", handoff)
	}
	// With the provider unknown, every router could be its.
	hops, _, _ = storageTrace(ft, func() netinfo.Path { return netinfo.Path{} })(context.Background(), target)
	if hops[4].IP != "" {
		t.Errorf("an unknown provider's router kept its address: %+v", hops[4])
	}
	if _, _, ok := storageTrace(ft, func() netinfo.Path { return netinfo.Path{} })(context.Background(), netip.MustParseAddr("10.0.0.5")); ok {
		t.Error("traced a private address")
	}
}
