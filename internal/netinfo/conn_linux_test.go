//go:build linux

package netinfo

import (
	"os"
	"path/filepath"
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
		if c := connType("/sys", n); c != model.ConnUnknown {
			t.Errorf("%s: %s", n, c)
		}
	}
	for _, n := range []string{"eth0", "enp3s0", "wlan0"} {
		if virtualName(n) {
			t.Errorf("%s: virtual", n)
		}
	}
	if c := connType("/sys", "lo"); c != model.ConnUnknown { // no device link
		t.Errorf("lo: %s", c)
	}
}

// fakeSys builds a sysfs tree under a temp dir. Each file is a path
// relative to the root: a trailing "/" makes a directory, "->" a symlink
// to a directory, otherwise "path=content" a file.
func fakeSys(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for p, content := range files {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		var err error
		switch content {
		case "/":
			err = os.MkdirAll(full, 0o755)
		case "->":
			target := filepath.Join(root, "devices", filepath.Base(p))
			if err = os.MkdirAll(target, 0o755); err == nil {
				err = os.Symlink(target, full)
			}
		default:
			err = os.WriteFile(full, []byte(content), 0o644)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestConnTypeSysfs(t *testing.T) {
	sys := fakeSys(t, map[string]string{
		// cfg80211 Wi-Fi: wireless directory.
		"class/net/wlan0/wireless": "/",
		"class/net/wlan0/device":   "->",
		"class/net/wlan0/type":     "1\n",
		// Wi-Fi without wireless/ (no wext): phy80211 link.
		"class/net/wlp2s0/phy80211": "->",
		"class/net/wlp2s0/device":   "->",
		"class/net/wlp2s0/type":     "1\n",
		// Only uevent says it is a WLAN.
		"class/net/wlx00c0/uevent": "INTERFACE=wlx00c0\nIFINDEX=4\nDEVTYPE=wlan\n",
		"class/net/wlx00c0/type":   "1\n",
		// A physical Ethernet NIC.
		"class/net/enp3s0/device": "->",
		"class/net/enp3s0/type":   "1\n",
		"class/net/enp3s0/uevent": "INTERFACE=enp3s0\nIFINDEX=2\n",
		// Ethernet-type without a device: a container's eth0 (a veth peer).
		"class/net/eth0/type":   "1\n",
		"class/net/eth0/uevent": "INTERFACE=eth0\nIFINDEX=7\n",
		// A veth with a device link still is virtual.
		"class/net/veth9f2/device": "->",
		"class/net/veth9f2/type":   "1\n",
		// A mobile broadband (raw IP) modem: type 519, not Ethernet.
		"class/net/wwan0/device": "->",
		"class/net/wwan0/type":   "519\n",
		"class/net/wwan0/uevent": "DEVTYPE=wwan\n",
		// Unreadable type: unknown.
		"class/net/enp4s0/device": "->",
		// Outside class/net: must never be reached through a crafted name.
		"class/x/wireless": "/",
	})
	for iface, want := range map[string]string{
		"wlan0":   model.ConnWiFi,
		"wlp2s0":  model.ConnWiFi,
		"wlx00c0": model.ConnWiFi,
		"enp3s0":  model.ConnWired,
		"eth0":    model.ConnUnknown,
		"veth9f2": model.ConnUnknown,
		"wwan0":   model.ConnUnknown,
		"enp4s0":  model.ConnUnknown,
		"missing": model.ConnUnknown,
		"":        model.ConnUnknown,
		".":       model.ConnUnknown,
		"..":      model.ConnUnknown,
		"../x":    model.ConnUnknown,
		"../../x": model.ConnUnknown,
		"x/..":    model.ConnUnknown,
	} {
		if got := connType(sys, iface); got != want {
			t.Errorf("connType(%q) = %s, want %s", iface, got, want)
		}
	}
}

func TestContainerized(t *testing.T) {
	none := func(string) string { return "" }
	cases := []struct {
		name  string
		files map[string]string
		env   map[string]string
		want  bool
	}{
		{"host", map[string]string{"etc/hostname": "pi\n", "run/": "/"}, nil, false},
		{"docker", map[string]string{".dockerenv": ""}, nil, true},
		{"podman", map[string]string{"run/.containerenv": "engine=\"podman-5\"\n"}, nil, true},
		{"container env", nil, map[string]string{"container": "oci"}, true},
		{"empty container env", nil, map[string]string{"container": ""}, false},
	}
	for _, c := range cases {
		root := fakeSys(t, c.files)
		getenv := none
		if c.env != nil {
			getenv = func(k string) string { return c.env[k] }
		}
		if got := containerized(root, getenv); got != c.want {
			t.Errorf("%s: containerized = %v, want %v", c.name, got, c.want)
		}
	}
}
