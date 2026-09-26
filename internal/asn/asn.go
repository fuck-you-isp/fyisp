// Package asn maps IPv4 addresses to their origin AS number and name using a
// table built into the binary (from iptoasn.com, public domain / PDDL). No
// network lookups.
//
// The table (data/ip2asn-v4.bin, format in table.go) is regenerated from
// https://iptoasn.com/data/ip2asn-v4.tsv.gz by the Dockerfile's asndb target:
//
//	docker buildx build --target asndb --output type=local,dest=. .
//
// It is decoded lazily on the first Lookup or Version call.
package asn

import (
	_ "embed"
	"log/slog"
	"net/netip"
	"sync"
)

// Info is the result of a lookup.
type Info struct {
	ASN   uint32 // 0 = not routed / unknown
	Owner string // AS name as published, e.g. "COMCAST-7922"
}

//go:embed data/ip2asn-v4.bin
var embedded []byte

var std = newDB(embedded, func(err error) {
	slog.Warn("asn: built-in IP-to-ASN table is unusable; hop owners will be empty", "err", err)
})

// Lookup returns the origin AS of an IPv4 address (IPv4-mapped IPv6 addresses
// are unmapped first). Private, reserved, IPv6 and unrouted addresses, and any
// address when the built-in table cannot be decoded, return the zero Info.
// Safe for concurrent use.
func Lookup(ip netip.Addr) Info { return std.lookup(ip) }

// Version is the build date of the built-in table, e.g. "2026-09-26", or ""
// if the table cannot be decoded.
func Version() string {
	if t := std.get(); t != nil {
		return t.version
	}
	return ""
}

// db decodes a table on first use and remembers the result (or the failure,
// which is reported once).
type db struct {
	once   sync.Once
	data   []byte
	onFail func(error)
	t      *table
}

func newDB(data []byte, onFail func(error)) *db { return &db{data: data, onFail: onFail} }

func (d *db) get() *table {
	d.once.Do(func() {
		t, err := decode(d.data)
		d.data = nil
		if err != nil {
			if d.onFail != nil {
				d.onFail(err)
			}
			return
		}
		d.t = t
	})
	return d.t
}

func (d *db) lookup(ip netip.Addr) Info {
	ip = ip.Unmap()
	if !ip.Is4() || !public(ip) {
		return Info{}
	}
	t := d.get()
	if t == nil {
		return Info{}
	}
	return t.lookup(ip.As4())
}

var (
	cgnat    = netip.MustParsePrefix("100.64.0.0/10")
	thisNet  = netip.MustParsePrefix("0.0.0.0/8")
	benchNet = netip.MustParsePrefix("198.18.0.0/15")
	reserved = netip.MustParsePrefix("240.0.0.0/4") // includes broadcast
)

// public reports whether ip may be globally routed. The table has no entries
// for the other ranges anyway; this keeps the answer right whatever it holds.
func public(ip netip.Addr) bool {
	return !(ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() ||
		ip.IsMulticast() || ip.IsUnspecified() || cgnat.Contains(ip) ||
		thisNet.Contains(ip) || benchNet.Contains(ip) || reserved.Contains(ip))
}
