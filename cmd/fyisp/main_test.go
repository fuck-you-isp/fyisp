package main

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

func TestAllowHostFlag(t *testing.T) {
	c, err := parseFlags([]string{"--allow-host", "NAS.local, fyisp.lan:3000", "--allow-host=box.tail1234.ts.net"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"nas.local", "fyisp.lan:3000", "box.tail1234.ts.net"}
	if !slices.Equal(c.allowHosts, want) {
		t.Errorf("allowHosts = %q, want %q", c.allowHosts, want)
	}
	if _, err := parseFlags([]string{"--allow-host", "http://nas.local/"}); err == nil {
		t.Error("URL accepted as --allow-host")
	}
}

// TestEnvDefaults: FYISP_DATA_DIR and FYISP_LISTEN are defaults (the
// Docker image sets them), flags win, and an unknown word still errors.
func TestEnvDefaults(t *testing.T) {
	t.Setenv("FYISP_DATA_DIR", "/data")
	t.Setenv("FYISP_LISTEN", "0.0.0.0:3000")
	c, err := parseFlags(nil)
	if err != nil || c.listen != "0.0.0.0:3000" {
		t.Fatalf("listen %q %v", c.listen, err)
	}
	if d, err := defaultDataDir(); err != nil || d != "/data" {
		t.Errorf("data dir %q %v", d, err)
	}
	if c, err := parseFlags([]string{"--listen=127.0.0.1:4000"}); err != nil || c.listen != "127.0.0.1:4000" {
		t.Errorf("the flag lost: %q %v", c.listen, err)
	}
	if _, err := parseFlags([]string{"--ephemeral"}); err != nil {
		t.Errorf("--ephemeral with FYISP_DATA_DIR set: %v", err)
	}
	if _, err := parseFlags([]string{"repport"}); err == nil {
		t.Error("an unknown word ran the server")
	}
}

// TestTunnelEnv: FYISP_TUNNEL turns --tunnel on (container templates); the
// flag wins.
func TestTunnelEnv(t *testing.T) {
	if c, err := parseFlags(nil); err != nil || c.tunnel {
		t.Errorf("default: tunnel %v, %v", c.tunnel, err)
	}
	t.Setenv("FYISP_TUNNEL", "1")
	if c, err := parseFlags(nil); err != nil || !c.tunnel {
		t.Errorf("FYISP_TUNNEL=1: tunnel %v, %v", c.tunnel, err)
	}
	if c, err := parseFlags([]string{"--tunnel=false"}); err != nil || c.tunnel {
		t.Errorf("--tunnel=false over FYISP_TUNNEL=1: tunnel %v, %v", c.tunnel, err)
	}
	t.Setenv("FYISP_TUNNEL", "yes")
	if _, err := parseFlags(nil); err == nil {
		t.Error("FYISP_TUNNEL=yes accepted")
	}
}

func TestLoopbackHostOnly(t *testing.T) {
	h := loopbackHostOnly(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	for host, code := range map[string]int{
		"127.0.0.1:6060": 200, "localhost:6060": 200, "LOCALHOST": 200, "[::1]:6060": 200,
		"evil.example:6060": 421, "rebind.evil.example": 421, "192.168.1.5:6060": 421, "": 421,
	} {
		r := httptest.NewRequest("GET", "/debug/pprof/", nil)
		r.Host = host
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != code {
			t.Errorf("Host %q = %d, want %d", host, w.Code, code)
		}
	}
}
