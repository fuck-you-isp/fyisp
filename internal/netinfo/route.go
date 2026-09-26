package netinfo

import (
	"errors"
	"net/netip"
)

// ErrNoRoute is Path.Err when the system has no IPv4 default route: the
// machine is offline (as opposed to discovery being unsupported or failing).
const ErrNoRoute = "no default route"

var errNoRoute = errors.New(ErrNoRoute)

// NoRoute reports whether the last discovery found no default route.
func (p Path) NoRoute() bool { return p.Err == ErrNoRoute }

// route is the system's IPv4 default route. Gateway is invalid for a default
// route without a next hop (point-to-point links, some VPNs).
type route struct {
	Gateway netip.Addr
	Iface   string
}
