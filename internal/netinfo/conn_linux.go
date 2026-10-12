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
// Ethernet-type interface backed by a device, model.ConnUnknown otherwise
// (veth and bridges inside containers, VM and VPN interfaces, an unknown
// interface). In a container with the host's network it sees the host's
// interfaces; see InContainer.
func ConnType(iface string) string { return connType("/sys", iface) }

// connType is ConnType with sysfs mounted at sysRoot.
func connType(sysRoot, iface string) string {
	if iface == "" || iface == "." || iface == ".." || strings.Contains(iface, "/") {
		return model.ConnUnknown
	}
	dir := filepath.Join(sysRoot, "class/net", iface)
	for _, f := range []string{"wireless", "phy80211"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
			return model.ConnWiFi
		}
	}
	if b, err := os.ReadFile(filepath.Join(dir, "uevent")); err == nil && strings.Contains(string(b), "DEVTYPE=wlan") {
		return model.ConnWiFi
	}
	// Virtual interfaces (a container's veth, a bridge, a VM's or VPN's
	// tun/tap) say nothing about the machine's own link: unknown. A
	// physical NIC has a device link.
	if virtualName(iface) {
		return model.ConnUnknown
	}
	if _, err := os.Stat(filepath.Join(dir, "device")); err != nil {
		return model.ConnUnknown
	}
	if b, err := os.ReadFile(filepath.Join(dir, "type")); err == nil && strings.TrimSpace(string(b)) == "1" { // ARPHRD_ETHER
		return model.ConnWired
	}
	return model.ConnUnknown
}

// virtualName reports interface names of virtual links.
func virtualName(iface string) bool {
	for _, p := range []string{"veth", "docker", "br-", "virbr", "tun", "tap", "vnet", "cni", "flannel", "cali", "wg", "tailscale", "zt"} {
		if strings.HasPrefix(iface, p) {
			return true
		}
	}
	return false
}
