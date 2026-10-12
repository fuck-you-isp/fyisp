package netinfo

import (
	"os"
	"path/filepath"
	"testing"
)

// fakeSys builds a file tree (a fake sysfs or root) under a temp dir. Keys
// are paths relative to it; the value "/" makes a directory, "->" a
// symlink to a directory, anything else a file with that content.
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

func TestContainerized(t *testing.T) {
	none := func(string) string { return "" }
	cases := []struct {
		name  string
		files map[string]string
		env   map[string]string
		want  bool
	}{
		{"host", map[string]string{"etc/hostname": "pi\n", "run/systemd/system": "/"}, nil, false},
		{"docker", map[string]string{".dockerenv": ""}, nil, true},
		{"podman", map[string]string{"run/.containerenv": "engine=\"podman-5\"\n"}, nil, true},
		{"systemd in a container", map[string]string{"run/systemd/container": "lxc\n"}, nil, true},
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
