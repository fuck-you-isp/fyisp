package s3test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoErrorTextOrLocalAddr: the engine never keeps an error's text (a
// net.OpError names the local address, the public one on a host without
// NAT) or reads the local address: attempts carry classes only.
func TestNoErrorTextOrLocalAddr(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, bad := range []string{"LocalAddr(", ".Error()", "%v\", err", "\"err\", err"} {
			if strings.Contains(string(b), bad) {
				t.Errorf("%s uses %s", f, bad)
			}
		}
	}
}
