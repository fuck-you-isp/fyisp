package netinfo

import (
	"os"
	"path/filepath"
	"sync"
)

// InContainer reports whether fyisp runs in a container. It is checked
// once. Even with the host's network (where ConnType sees the host's
// interfaces), a container is not the machine its user sits at.
func InContainer() bool { return inContainer() }

var inContainer = sync.OnceValue(func() bool { return containerized("/", os.Getenv) })

// containerized reports whether the filesystem at root belongs to a
// container:
//   - Docker creates /.dockerenv and Podman /run/.containerenv;
//   - systemd writes /run/systemd/container in any container it detects,
//     including nspawn and LXC, whose $container its services don't inherit;
//   - container managers set $container for the container's first process
//     (systemd-nspawn, LXC, Podman), and the fyisp image sets container=oci
//     for runtimes that create no marker file (containerd, Kubernetes).
func containerized(root string, getenv func(string) string) bool {
	if getenv("container") != "" {
		return true
	}
	for _, f := range []string{".dockerenv", "run/.containerenv", "run/systemd/container"} {
		if _, err := os.Stat(filepath.Join(root, f)); err == nil {
			return true
		}
	}
	return false
}
