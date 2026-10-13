package web

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// Who is looking: the Home says "Your Wi-Fi and router" only when fyisp's
// own uplink is Wi-Fi and the page is viewed on that machine, since fyisp
// can't see the Wi-Fi of another device. Only the wording depends on it:
// the API never returns the viewer's address or the decision, and neither
// is logged or cached. A forwarder that adds no proxy header (ssh -L,
// socat) reads as this machine (README "Wi-Fi or router?").

// ownAddrsTTL is how long the host's interface addresses are kept.
const ownAddrsTTL = 30 * time.Second

// ownAddrs are the host's interface addresses, re-read every ownAddrsTTL.
type ownAddrs struct {
	list func() ([]netip.Addr, error) // default: net.InterfaceAddrs
	now  func() time.Time

	mu  sync.Mutex
	at  time.Time
	set map[netip.Addr]bool
}

func newOwnAddrs() *ownAddrs { return &ownAddrs{list: interfaceAddrs, now: time.Now} }

func interfaceAddrs() ([]netip.Addr, error) {
	as, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	var out []netip.Addr
	for _, a := range as {
		if n, ok := a.(*net.IPNet); ok {
			if ip, ok := netip.AddrFromSlice(n.IP); ok {
				out = append(out, ip.Unmap())
			}
		}
	}
	return out, nil
}

// has reports an address of this host.
func (o *ownAddrs) has(a netip.Addr) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	now := o.now()
	if o.set == nil || now.Sub(o.at) >= ownAddrsTTL || now.Before(o.at) {
		o.at, o.set = now, map[netip.Addr]bool{}
		if as, err := o.list(); err == nil {
			for _, x := range as {
				o.set[x.Unmap().WithZone("")] = true
			}
		}
	}
	return o.set[a]
}

// sameMachine reports whether r comes from the machine fyisp runs on: from
// a loopback address or one of the host's own, not through a proxy or
// tunnel (proxied), and not by an --allow-host name (which means the
// dashboard is reached from elsewhere).
func (h *localHandler) sameMachine(r *http.Request) bool {
	ap, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	a := ap.Addr().Unmap().WithZone("")
	if !a.IsLoopback() && !h.own.has(a) {
		return false
	}
	return !proxied(r) && !h.extraHost(r.Host)
}

// extraHost reports a Host that is one of the --allow-host names.
func (h *localHandler) extraHost(hostport string) bool {
	hostport = strings.ToLower(hostport)
	if h.extra[hostport] {
		return true
	}
	host, _, err := net.SplitHostPort(hostport)
	return err == nil && h.extra[host]
}
