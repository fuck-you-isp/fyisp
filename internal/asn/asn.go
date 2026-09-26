// Package asn maps IPv4 addresses to their origin AS number and name using a
// table built into the binary (from iptoasn.com, public domain / PDDL). No
// network lookups.
package asn

import "net/netip"

// Info is the result of a lookup.
type Info struct {
	ASN   uint32 // 0 = not routed / unknown
	Owner string // AS name as published, e.g. "COMCAST-7922"
}

// Interface summary (implemented by the asn agent):
//
//	func Lookup(ip netip.Addr) Info   // IPv4 only; private/unknown -> zero Info
//	func Version() string             // table build date, e.g. "2026-09-26"
var _ = netip.Addr{}
