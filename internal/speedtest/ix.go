package speedtest

// Internet exchanges whose route servers or peering LANs show up under
// their own ASN on a traceroute: the handoff looks through them to the
// network on the other side (the peer) and names the exchange as "via".
// Each ASN was checked against PeeringDB (peeringdb.com/api/net) on
// 2026-10-11; the names are the exchanges' usual short names. No runtime
// lookup, no reverse DNS.
var ixASNs = map[uint32]string{
	1200:  "AMS-IX",     // Amsterdam Internet Exchange BV
	6777:  "AMS-IX",     // AMS-IX Route Servers
	6695:  "DE-CIX",     // DE-CIX Frankfurt Route Servers
	63034: "DE-CIX",     // DE-CIX New York Route Servers
	5459:  "LINX",       // LINX Route Collectors
	8714:  "LINX",       // LINX Route Servers
	51706: "France-IX",  // France-IX Paris Route Servers
	34307: "NL-ix",      // NL-ix Route Servers
	8674:  "Netnod",     // Netnod
	33108: "SIX",        // SIX Seattle Route Servers
	7527:  "JPIX",       // JPIX
	4635:  "HKIX",       // HKIX Route Servers
	26162: "IX.br",      // NIC.br - IX.br (PTT.br) MLPA Route Servers
	24115: "Equinix IX", // Equinix Internet Exchange - MLPE Route Servers
	8631:  "MSK-IX",     // MSK-IX Moscow Route Servers
}
