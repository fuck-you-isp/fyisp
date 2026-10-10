package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestResolveAdminToken(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, adminTokenFile)
	for _, c := range []struct{ flag, addr, want string }{
		{"off", "0.0.0.0:3000", ""},
		{"off", "127.0.0.1:3000", ""},
		{"mine", "0.0.0.0:3000", "mine"},
		{"mine", "127.0.0.1:3000", "mine"},
		{"", "127.0.0.1:3000", ""},
		{"", "[::1]:3000", ""},
		{"", "localhost:3000", ""},
	} {
		tok, stored, err := resolveAdminToken(c.flag, c.addr, dir)
		if err != nil || tok != c.want || stored {
			t.Errorf("%q on %s = %q, %v, %v; want %q", c.flag, c.addr, tok, stored, err, c.want)
		}
	}
	if _, err := os.Stat(file); err == nil {
		t.Fatal("token file written without need")
	}

	// A network listener without --admin-token: generated once, then reused.
	tok, stored, err := resolveAdminToken("", "0.0.0.0:3000", dir)
	if err != nil || !stored || len(tok) != 32 {
		t.Fatalf("generated = %q, %v, %v", tok, stored, err)
	}
	fi, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Errorf("token file mode %v, want 0600", fi.Mode().Perm())
	}
	for _, addr := range []string{"0.0.0.0:3000", "192.168.1.10:3000", "[::]:3000"} {
		again, stored, err := resolveAdminToken("", addr, dir)
		if err != nil || !stored || again != tok {
			t.Errorf("%s: reread = %q, %v, %v; want %q", addr, again, stored, err, tok)
		}
	}
	if _, err := os.Stat(file + ".tmp"); err == nil {
		t.Error("temporary file left behind")
	}

	// Loose permissions are tightened; a damaged file is an error, not a
	// silently new token.
	if runtime.GOOS != "windows" {
		_ = os.Chmod(file, 0o644)
		if _, _, err := resolveAdminToken("", "0.0.0.0:3000", dir); err != nil {
			t.Fatal(err)
		}
		if fi, _ := os.Stat(file); fi.Mode().Perm() != 0o600 {
			t.Errorf("mode not tightened: %v", fi.Mode().Perm())
		}
	}
	for _, bad := range []string{"", "short", strings.Repeat("a", 31), strings.Repeat("a", 20) + " " + strings.Repeat("b", 20)} {
		_ = os.WriteFile(file, []byte(bad), 0o600)
		if tok, _, err := resolveAdminToken("", "0.0.0.0:3000", dir); err == nil {
			t.Errorf("file %q accepted as %q", bad, tok)
		}
	}
}

func TestAdminLink(t *testing.T) {
	if got := adminLink("http://127.0.0.1:3000/", "abc"); got != "http://127.0.0.1:3000/#admin=abc" {
		t.Errorf("adminLink = %q", got)
	}
}
