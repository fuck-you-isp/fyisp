//go:build unix && !linux

package netinfo

import "fmt"

// openTracer uses the unprivileged "udp4" ICMP socket where the OS delivers
// ICMP errors to it (macOS), else a raw socket. If neither works the edge
// stays unknown.
func openTracer() (tracer, error) {
	t, err := openPacketTracer("udp4")
	if err == nil {
		return t, nil
	}
	t, errRaw := openPacketTracer("ip4:icmp")
	if errRaw != nil {
		return nil, fmt.Errorf("udp4 ICMP: %w; raw: %w", err, errRaw)
	}
	return t, nil
}
