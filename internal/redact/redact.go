// Package redact holds the public-link redaction rules shared by the web
// API and evidence reports: which addresses identify the user's own network,
// how the ISP edge is masked, and how much of each traceroute hop may be
// shown.
//
// Rules (public view):
//   - private addresses (RFC 1918, CGNAT, link-local, loopback, ULA, ...)
//     are never shown;
//   - the first public hop (the ISP edge) is masked to its /24 (IPv6: /48)
//     and has no reverse DNS;
//   - the 2nd and 3rd public hops are shown without reverse DNS;
//   - later hops are shown in full.
//
// A hop's level is the strictest it gets in any path of one document, so an
// address is never masked in one place and shown elsewhere.
package redact

import (
	"net/netip"
	"regexp"
	"strconv"
	"strings"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// IsPrivate reports addresses that identify the user's own network or the
// ISP's internal plant: RFC 1918, CGNAT, link-local, loopback, ULA,
// unspecified, multicast and 0.0.0.0/8.
func IsPrivate(a netip.Addr) bool {
	a = a.Unmap()
	return a.IsPrivate() || a.IsLoopback() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() ||
		a.IsUnspecified() || a.IsMulticast() || a.IsInterfaceLocalMulticast() || cgnat.Contains(a) ||
		(a.Is4() && a.As4()[0] == 0)
}

// MaskEdge hides the host part: "a.b.c.x" for IPv4, the /48 plus "::x" for
// IPv6.
func MaskEdge(a netip.Addr) string {
	a = a.Unmap()
	if a.Is4() {
		b := a.As4()
		return strconv.Itoa(int(b[0])) + "." + strconv.Itoa(int(b[1])) + "." + strconv.Itoa(int(b[2])) + ".x"
	}
	p, _ := a.Prefix(48)
	return strings.TrimSuffix(p.Addr().String(), "::") + "::x"
}

// Level is how much of a hop address may be shown publicly, from least to
// most strict.
type Level int

const (
	Full    Level = iota // later public hops: everything shown
	NoRDNS               // 2nd and 3rd public hops: rDNS hidden
	Masked               // 1st public hop (the ISP edge): IP masked, no rDNS
	Private              // private hop: no address at all
)

// Classifier assigns levels to the addresses of every path in one document.
type Classifier struct {
	lvl map[netip.Addr]Level
}

// NewClassifier returns an empty classifier.
func NewClassifier() *Classifier { return &Classifier{lvl: map[netip.Addr]Level{}} }

// Seq classifies a path: seq[i] are the addresses seen at TTL i+1 (several
// with ECMP; invalid addresses are hops that did not answer).
func (c *Classifier) Seq(seq [][]netip.Addr) {
	pub := 0
	for _, addrs := range seq {
		any := false
		for _, a := range addrs {
			if !a.IsValid() {
				continue
			}
			a = a.Unmap()
			l := Full
			switch {
			case IsPrivate(a):
				l = Private
			case pub == 0:
				l, any = Masked, true
			case pub < 3:
				l, any = NoRDNS, true
			default:
				any = true
			}
			if old, ok := c.lvl[a]; !ok || l > old {
				c.lvl[a] = l
			}
		}
		if any {
			pub++
		}
	}
}

// Route classifies a single-address-per-TTL path.
func (c *Classifier) Route(hops []netip.Addr) {
	seq := make([][]netip.Addr, len(hops))
	for i, a := range hops {
		seq[i] = []netip.Addr{a}
	}
	c.Seq(seq)
}

// Level returns a's level; addresses never classified are Masked (strict),
// private ones always Private.
func (c *Classifier) Level(a netip.Addr) Level {
	a = a.Unmap()
	if IsPrivate(a) {
		return Private
	}
	if l, ok := c.lvl[a]; ok {
		return l
	}
	return Masked
}

// Hop is one hop address as it may be shown.
type Hop struct {
	IP      string // "" when NoReply, or Private in a public view
	NoReply bool
	Private bool
	Masked  bool
	RDNS    string
	ASN     uint32
	Owner   string
}

// Hop describes address a (with what is known about it) for a public
// (redacted) or local view.
func (c *Classifier) Hop(a netip.Addr, info model.HopInfo, public bool) Hop {
	var h Hop
	if !a.IsValid() {
		h.NoReply = true
		return h
	}
	a = a.Unmap()
	if IsPrivate(a) {
		h.Private = true
		if !public {
			h.IP, h.RDNS = a.String(), info.RDNS
		}
		return h
	}
	h.ASN, h.Owner = info.ASN, info.Owner
	if !public {
		h.IP, h.RDNS = a.String(), info.RDNS
		return h
	}
	switch c.Level(a) {
	case Masked:
		h.IP, h.Masked = MaskEdge(a), true
	case NoRDNS:
		h.IP = a.String()
	default:
		h.IP, h.RDNS = a.String(), info.RDNS
	}
	return h
}

// Text replaces every private address literal in free text (user notes,
// summaries) with "[private address]". Public addresses are kept.
func Text(s string) string {
	s = ipv4Re.ReplaceAllStringFunc(s, func(m string) string {
		if a, err := netip.ParseAddr(m); err == nil && IsPrivate(a) {
			return privateText
		}
		return m
	})
	// IPv6: runs of hex digits and colons containing "::" or several colons.
	return ipv6Re.ReplaceAllStringFunc(s, func(m string) string {
		core := strings.TrimRight(m, ":")
		if a, err := netip.ParseAddr(core); err == nil && IsPrivate(a) {
			return privateText + m[len(core):]
		}
		return m
	})
}

const privateText = "[private address]"

var (
	// Digits not glued to other digits, so "110.0.0.1" is not "10.0.0.1".
	ipv4Re = regexp.MustCompile(`\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}`)
	ipv6Re = regexp.MustCompile(`[0-9A-Fa-f:]*:[0-9A-Fa-f:]*:[0-9A-Fa-f:.]*`)
)
