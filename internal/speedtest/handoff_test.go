package speedtest

import (
	"context"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/asn"
	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/netinfo/hops"
)

// fakeHops answers TTL k with path[k-1]; the last one is the destination.
type fakeHops struct {
	path  []string
	calls atomic.Int32
}

func (f *fakeHops) Probe(_ context.Context, dst netip.Addr, ttl int, _ time.Duration) (hops.Hop, error) {
	f.calls.Add(1)
	if ttl > len(f.path) {
		return hops.Hop{Addr: dst, Reached: true}, nil
	}
	a := f.path[ttl-1]
	if a == "" {
		return hops.Hop{}, nil // silent
	}
	return hops.Hop{Addr: netip.MustParseAddr(a), Reached: ttl == len(f.path)}, nil
}

func (f *fakeHops) Close() error { return nil }

var testASN = map[string]asn.Info{
	"62.1.1.1": {ASN: 3320, Owner: "DTAG"}, "62.1.2.2": {ASN: 3320, Owner: "DTAG"},
	"4.69.1.1": {ASN: 3356, Owner: "LEVEL3"}, "5.79.1.1": {ASN: 60781, Owner: "LEASEWEB-NL-AMS-01"},
	"80.81.192.10": {ASN: 6695, Owner: "DECIX-RS-AS"}, // a DE-CIX Frankfurt LAN address
}

func testLookup(a netip.Addr) asn.Info { return testASN[a.String()] }

func TestHandoff(t *testing.T) {
	now := time.Date(2026, 10, 10, 21, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name string
		edge string
		path []string
		want uint32
		isp  uint32
	}{
		{"transit", "62.1.1.1", []string{"192.168.1.1", "62.1.1.1", "", "62.1.2.2", "4.69.1.1", "5.79.1.1"}, 3356, 3320},
		{"direct peering", "62.1.1.1", []string{"192.168.1.1", "62.1.1.1", "62.1.2.2", "5.79.1.1"}, 60781, 3320},
		{"edge private: ISP from the first public hop", "100.64.0.1", []string{"192.168.1.1", "100.64.0.1", "62.1.2.2", "4.69.1.1", "5.79.1.1"}, 3356, 3320},
		{"on-net: no handoff", "62.1.1.1", []string{"192.168.1.1", "62.1.1.1", "62.1.2.2"}, 0, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			fh := &fakeHops{path: c.path}
			f := &HandoffFinder{Open: func() (hops.Prober, error) { return fh, nil }, Lookup: testLookup,
				Edge: func() netip.Addr { return netip.MustParseAddr(c.edge) }, Now: func() time.Time { return now },
				Resolve: func(context.Context, string) ([]netip.Addr, error) {
					return []netip.Addr{netip.MustParseAddr(c.path[len(c.path)-1])}, nil
				}}
			h, err := f.Handoff(context.Background(), "speedtest.example")
			if err != nil {
				t.Fatal(err)
			}
			switch {
			case c.want == 0 && h != nil:
				t.Errorf("handoff %+v, want none", *h)
			case c.want != 0 && (h == nil || h.ASN != c.want || h.ISPASN != c.isp || !h.At.Equal(now)):
				t.Errorf("handoff %+v, want AS%d after AS%d", h, c.want, c.isp)
			}
			// Cached for a day: no new traceroute.
			n := fh.calls.Load()
			now = now.Add(HandoffEvery - time.Minute)
			if _, err := f.Handoff(context.Background(), "speedtest.example"); err != nil || fh.calls.Load() != n {
				t.Errorf("second call within a day probed again (%d -> %d), err %v", n, fh.calls.Load(), err)
			}
			now = now.Add(2 * time.Minute)
			if _, _ = f.Handoff(context.Background(), "speedtest.example"); fh.calls.Load() == n {
				t.Error("no new traceroute after a day")
			}
		})
	}
}

// TestHandoffIX: an internet exchange's hop is looked through to the peer
// behind it, and kept as Via; a path that ends at it has no peer.
func TestHandoffIX(t *testing.T) {
	edge := netip.MustParseAddr("62.1.1.1")
	path := func(hops ...string) []netip.Addr {
		var out []netip.Addr
		for _, h := range hops {
			a, _ := netip.ParseAddr(h)
			out = append(out, a)
		}
		return out
	}
	h := handoffOf(path("192.168.1.1", "62.1.1.1", "62.1.2.2", "80.81.192.10", "5.79.1.1"), edge, testLookup)
	if h == nil || h.ASN != 60781 || h.Via != "DE-CIX" || h.ISPASN != 3320 {
		t.Errorf("through DE-CIX: %+v", h)
	}
	h = handoffOf(path("192.168.1.1", "62.1.1.1", "62.1.2.2", "80.81.192.10", "", ""), edge, testLookup)
	if h == nil || h.ASN != 0 || h.Via != "DE-CIX" {
		t.Errorf("ends at DE-CIX: %+v", h)
	}
	if h = handoffOf(path("192.168.1.1", "62.1.1.1", "4.69.1.1"), edge, testLookup); h == nil || h.Via != "" || h.ASN != 3356 {
		t.Errorf("no exchange: %+v", h)
	}
	for asn, name := range ixASNs {
		if asn == 0 || name == "" {
			t.Errorf("ixASNs %d %q", asn, name)
		}
	}
}

// TestNetKey: one peer met at two exchanges is two links.
func TestNetKey(t *testing.T) {
	a := model.SpeedTest{Handoff: &model.Handoff{ASN: 3356, Name: "LEVEL3", Via: "DE-CIX"}}
	b := model.SpeedTest{Handoff: &model.Handoff{ASN: 3356, Name: "LEVEL3", Via: "AMS-IX"}}
	c := model.SpeedTest{Handoff: &model.Handoff{ASN: 3356, Name: "LEVEL3"}}
	if netKey(a) == netKey(b) || netKey(a) == netKey(c) || netKey(c) != "LEVEL3 (AS3356)" || netKey(model.SpeedTest{}) != "" {
		t.Errorf("%q %q %q", netKey(a), netKey(b), netKey(c))
	}
}
