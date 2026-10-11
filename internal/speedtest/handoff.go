package speedtest

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/asn"
	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/netinfo/hops"
)

// Handoff: where the path to a speed test server leaves the user's ISP.
// Same-metro hosting providers often buy the same transit, so the network
// that matters for "your ISP's link to X slows in the evening" is the first
// network past the ISP, not the server's provider. A traceroute per server
// at most once per HandoffEvery finds it; between traceroutes the last
// answer is reused.
const (
	HandoffEvery   = 24 * time.Hour
	handoffMaxHops = 20
	handoffTimeout = time.Second
	handoffPar     = 4
)

// HandoffFinder runs the traceroutes (TTL-limited ICMP, as package trace
// does) and maps hops to ASNs with the built-in table.
type HandoffFinder struct {
	Open    func() (hops.Prober, error)                                  // default hops.Open
	Lookup  func(netip.Addr) asn.Info                                    // default asn.Lookup
	Edge    func() netip.Addr                                            // the ISP edge (netinfo); its ASN is the ISP's when known
	Now     func() time.Time                                             // default time.Now
	Resolve func(ctx context.Context, host string) ([]netip.Addr, error) // IPv4 addresses of host; default the system resolver

	mu    sync.Mutex
	cache map[string]handoffEntry // host -> last answer
}

type handoffEntry struct {
	h  *model.Handoff
	at time.Time
}

// Handoff returns the handoff on the path to host: the cached answer when
// it is younger than HandoffEvery, else a new traceroute. nil when it
// cannot tell (no public hop of another network, or the path stays in the
// ISP's network).
func (f *HandoffFinder) Handoff(ctx context.Context, host string) (*model.Handoff, error) {
	now := time.Now
	if f.Now != nil {
		now = f.Now
	}
	f.mu.Lock()
	if e, ok := f.cache[host]; ok && now().Sub(e.at) < HandoffEvery {
		f.mu.Unlock()
		return e.h, nil
	}
	f.mu.Unlock()
	dst, err := f.resolve(ctx, host)
	if err != nil {
		return nil, err
	}
	open := hops.Open
	if f.Open != nil {
		open = f.Open
	}
	p, err := open()
	if err != nil {
		return nil, err
	}
	defer p.Close()
	path := traceTo(ctx, p, dst)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	lookup := asn.Lookup
	if f.Lookup != nil {
		lookup = f.Lookup
	}
	var edge netip.Addr
	if f.Edge != nil {
		edge = f.Edge()
	}
	h := handoffOf(path, edge, lookup)
	if h != nil {
		h.At = now()
	}
	f.mu.Lock()
	if f.cache == nil {
		f.cache = map[string]handoffEntry{}
	}
	f.cache[host] = handoffEntry{h: h, at: now()}
	f.mu.Unlock()
	return h, nil
}

// Cached is the answer for host without a traceroute, when one younger
// than HandoffEvery is cached.
func (f *HandoffFinder) Cached(host string) (*model.Handoff, bool) {
	now := time.Now
	if f.Now != nil {
		now = f.Now
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if e, ok := f.cache[host]; ok && now().Sub(e.at) < HandoffEvery {
		return e.h, true
	}
	return nil, false
}

func (f *HandoffFinder) resolve(ctx context.Context, host string) (netip.Addr, error) {
	if a, err := netip.ParseAddr(host); err == nil {
		return a.Unmap(), nil
	}
	look := f.Resolve
	if look == nil {
		look = func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip4", host)
		}
	}
	ips, err := look(ctx, host)
	if err != nil {
		return netip.Addr{}, err
	}
	if len(ips) == 0 {
		return netip.Addr{}, errors.New("no IPv4 address")
	}
	return ips[0].Unmap(), nil
}

// traceTo probes TTL 1..handoffMaxHops towards dst, handoffPar at a time,
// and returns the answering address per TTL (zero for no answer), up to the
// destination or an unreachable.
func traceTo(ctx context.Context, p hops.Prober, dst netip.Addr) []netip.Addr {
	var out []netip.Addr
	for base := 1; base <= handoffMaxHops; base += handoffPar {
		res := make([]hops.Hop, handoffPar)
		var wg sync.WaitGroup
		for i := range handoffPar {
			wg.Add(1)
			go func() {
				defer wg.Done()
				res[i], _ = p.Probe(ctx, dst, base+i, handoffTimeout)
			}()
		}
		wg.Wait()
		for _, h := range res {
			out = append(out, h.Addr)
			if h.Reached || h.Unreach {
				return out
			}
		}
		if ctx.Err() != nil {
			break
		}
	}
	return out
}

// handoffOf finds the handoff on path: the ISP is the edge's network when
// the edge has a public address with an ASN, else the first hop that has
// one; the handoff is the first later hop with another ASN. Hops of an
// internet exchange (ixASNs) are looked through like hops without an ASN,
// and the exchange is kept as Via; a path that ends there gives the
// exchange with no peer (ASN 0). nil when there is none.
func handoffOf(path []netip.Addr, edge netip.Addr, lookup func(netip.Addr) asn.Info) *model.Handoff {
	isp := uint32(0)
	if edge.IsValid() {
		isp = lookup(edge).ASN
	}
	via := ""
	for _, a := range path {
		if !a.IsValid() {
			continue
		}
		in := lookup(a)
		if ix, ok := ixASNs[in.ASN]; ok {
			if isp != 0 && via == "" {
				via = ix
			}
			continue
		}
		switch {
		case in.ASN == 0:
		case isp == 0:
			isp = in.ASN
		case in.ASN != isp:
			return &model.Handoff{ASN: in.ASN, Name: in.Owner, ISPASN: isp, Via: via}
		}
	}
	if via != "" {
		return &model.Handoff{ISPASN: isp, Via: via}
	}
	return nil
}
