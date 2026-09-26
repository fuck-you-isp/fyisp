//go:build unix && !linux

package hops

import "fmt"

// open uses the unprivileged "udp4" ICMP socket where the OS delivers ICMP
// errors to it (macOS), else a raw socket.
func open() (Prober, error) {
	t, err := openPacket("udp4")
	if err == nil {
		return t, nil
	}
	t, errRaw := openPacket("ip4:icmp")
	if errRaw != nil {
		return nil, fmt.Errorf("udp4 ICMP: %w; raw: %w", err, errRaw)
	}
	return t, nil
}
