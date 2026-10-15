// Command mtrprobe is a throwaway proof of concept: can an unprivileged
// process trace a path with ICMP, UDP and TCP probes on this OS?
package main

import (
	"flag"
	"fmt"
	"net/netip"
	"os"
	"runtime"
	"time"
)

// Hop is one probe's answer. A zero Addr means no answer.
type Hop struct {
	Addr    netip.Addr
	ICMP    string // "11/0" etc., or "" when the destination answered itself
	Reached bool
	RTT     time.Duration
	Note    string
}

type prober func(dst netip.AddrPort, ttl int, timeout time.Duration) (Hop, error)

func main() {
	dst := flag.String("dst", "1.1.1.1", "destination IPv4")
	port := flag.Int("port", 443, "TCP/UDP port (UDP increments per TTL like traceroute)")
	maxTTL := flag.Int("max", 12, "max TTL")
	only := flag.String("only", "", "run only this method")
	flag.Parse()
	a := netip.MustParseAddr(*dst)
	fmt.Printf("os=%s/%s uid-ish=%s dst=%s\n", runtime.GOOS, runtime.GOARCH, whoami(), a)
	for _, m := range methods() {
		if *only != "" && m.name != *only {
			continue
		}
		fmt.Printf("\n== %s\n", m.name)
		answered, reached := 0, false
		for ttl := 1; ttl <= *maxTTL; ttl++ {
			p := *port
			if m.udp {
				p = 33434 + ttl
			}
			h, err := m.fn(netip.AddrPortFrom(a, uint16(p)), ttl, 1500*time.Millisecond)
			switch {
			case err != nil:
				fmt.Printf("  ttl %2d  ERROR %v\n", ttl, err)
			case !h.Addr.IsValid():
				fmt.Printf("  ttl %2d  *   %s\n", ttl, h.Note)
			default:
				answered++
				fmt.Printf("  ttl %2d  %-16s icmp=%-5s reached=%-5v rtt=%6.1fms %s\n", ttl, h.Addr, h.ICMP, h.Reached, float64(h.RTT.Microseconds())/1000, h.Note)
			}
			if h.Reached {
				reached = true
				break
			}
			if err != nil && ttl == 1 {
				break
			}
		}
		fmt.Printf("RESULT %s %s answered=%d reached=%v\n", runtime.GOOS, m.name, answered, reached)
	}
	os.Exit(0)
}

type method struct {
	name string
	udp  bool
	fn   prober
}
