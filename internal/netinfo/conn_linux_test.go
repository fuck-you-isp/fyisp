//go:build linux

package netinfo

import (
	"testing"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

// TestConnTypeVirtual: a container's veth, bridges and tunnels are never
// "wired" (their peer may be Wi-Fi).
func TestConnTypeVirtual(t *testing.T) {
	for _, n := range []string{"veth1a2b", "docker0", "br-3f2a", "tun0", "tap1", "wg0", "tailscale0"} {
		if !virtualName(n) {
			t.Errorf("%s: not virtual", n)
		}
		if c := ConnType(n); c != model.ConnUnknown {
			t.Errorf("%s: %s", n, c)
		}
	}
	for _, n := range []string{"eth0", "enp3s0", "wlan0"} {
		if virtualName(n) {
			t.Errorf("%s: virtual", n)
		}
	}
	if c := ConnType("lo"); c != model.ConnUnknown { // no device link
		t.Errorf("lo: %s", c)
	}
}
