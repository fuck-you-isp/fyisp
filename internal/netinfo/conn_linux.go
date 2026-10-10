//go:build linux

package netinfo

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

// ConnType reports the connection type of iface: model.ConnWiFi for a
// wireless interface (cfg80211 exposes /sys/class/net/<if>/wireless or
// phy80211, and DEVTYPE=wlan in uevent), model.ConnWired for any other
// Ethernet-type interface, model.ConnUnknown otherwise (tunnels, VPNs, an
// unknown interface).
func ConnType(iface string) string {
	if iface == "" || iface == "." || iface == ".." || strings.Contains(iface, "/") {
		return model.ConnUnknown
	}
	dir := filepath.Join("/sys/class/net", iface)
	for _, f := range []string{"wireless", "phy80211"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
			return model.ConnWiFi
		}
	}
	if b, err := os.ReadFile(filepath.Join(dir, "uevent")); err == nil && strings.Contains(string(b), "DEVTYPE=wlan") {
		return model.ConnWiFi
	}
	if b, err := os.ReadFile(filepath.Join(dir, "type")); err == nil && strings.TrimSpace(string(b)) == "1" { // ARPHRD_ETHER
		return model.ConnWired
	}
	return model.ConnUnknown
}
