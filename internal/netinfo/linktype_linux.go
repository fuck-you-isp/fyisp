package netinfo

import (
	"os"
	"path/filepath"
	"strings"
)

// LinkType says how an interface connects: "wifi" (it has a wireless
// directory), "wired" (a physical device), or "unknown" (virtual
// interfaces, e.g. a Docker bridge's veth, VPNs: the real link is not
// visible from here).
func LinkType(iface string) string {
	if iface == "" || strings.ContainsAny(iface, "/.") {
		return "unknown"
	}
	base := filepath.Join("/sys/class/net", iface)
	if _, err := os.Stat(filepath.Join(base, "wireless")); err == nil {
		return "wifi"
	}
	if _, err := os.Stat(filepath.Join(base, "phy80211")); err == nil {
		return "wifi"
	}
	if _, err := os.Stat(filepath.Join(base, "device")); err == nil {
		return "wired"
	}
	return "unknown"
}
