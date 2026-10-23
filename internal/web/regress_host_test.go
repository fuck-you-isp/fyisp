package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// Finding 3: --listen on a specific LAN IP works (Host = that IP) and is
// treated as LAN mode.
func TestLocalSpecificBindIP(t *testing.T) {
	h, _ := localHandlerFor(t, LocalOptions{Addr: "192.168.1.10:3000"})
	for host, code := range map[string]int{
		"192.168.1.10:3000": 200, "127.0.0.1:3000": 200, "evil.com:3000": 421, "nas.local:3000": 421,
	} {
		if w := do(h, "GET", "/api/status", map[string]string{"Host": host}); w.Code != code {
			t.Errorf("--listen 192.168.1.10:3000, Host %q = %d, want %d", host, w.Code, code)
		}
	}
	// Unknown names are refused with a hint to --allow-host.
	w := do(h, "GET", "/", map[string]string{"Host": "nas.local:3000"})
	if w.Code != 421 || !strings.Contains(w.Body.String(), "--allow-host") || !strings.Contains(w.Body.String(), "192.168.1.10:3000") {
		t.Errorf("421 body = %s", w.Body.String())
	}
	// ExtraHosts (--allow-host) admits a name.
	h2, _ := localHandlerFor(t, LocalOptions{Addr: "0.0.0.0:3000", ExtraHosts: []string{"nas.local"}})
	if w := do(h2, "GET", "/api/status", map[string]string{"Host": "NAS.local:3000"}); w.Code != 200 {
		t.Errorf("--allow-host nas.local: %d", w.Code)
	}
	// A non-default loopback address accepts its own IP literal.
	h3, _ := localHandlerFor(t, LocalOptions{Addr: "127.0.0.2:3000"})
	for host, code := range map[string]int{"127.0.0.2:3000": 200, "127.0.0.2:3001": 421, "192.168.1.5:3000": 421} {
		if w := do(h3, "GET", "/api/status", map[string]string{"Host": host}); w.Code != code {
			t.Errorf("--listen 127.0.0.2:3000, Host %q = %d, want %d", host, w.Code, code)
		}
	}
}

// TestLocalTunnelHost: the tunnel's current name is allowed (a new one
// after a restart, none while it is down) over HTTPS only, writes through
// it need the admin token, and its visitors never count as this machine.
func TestLocalTunnelHost(t *testing.T) {
	var name atomic.Value
	name.Store("")
	h, _ := localHandlerFor(t, LocalOptions{Addr: "127.0.0.1:3000", TunnelHost: func() string { return name.Load().(string) }})
	get := func(host string) int { return do(h, "GET", "/api/status", map[string]string{"Host": host}).Code }
	if c := get("calm-river.trycloudflare.com"); c != 421 {
		t.Errorf("tunnel down: %d, want 421", c)
	}
	name.Store("calm-river.trycloudflare.com")
	for host, code := range map[string]int{
		"calm-river.trycloudflare.com": 200, "Calm-River.trycloudflare.com:443": 200, "calm-river.trycloudflare.com:3000": 421,
		"calm-river.trycloudflare.com:80": 421, // plain HTTP: never the tunnel's
		"other.trycloudflare.com":         421, "127.0.0.1:3000": 200,
	} {
		if c := get(host); c != code {
			t.Errorf("Host %q = %d, want %d", host, c, code)
		}
	}
	name.Store("new-name.trycloudflare.com")
	if get("calm-river.trycloudflare.com") != 421 || get("new-name.trycloudflare.com") != 200 {
		t.Error("the old name still works, or the new one doesn't")
	}
	// With the token, a write from the tunnel's HTTPS page passes the
	// same-origin check.
	h2, _ := localHandlerFor(t, LocalOptions{Addr: "127.0.0.1:3000", AdminToken: "adm", TunnelHost: func() string { return "calm-river.trycloudflare.com" }})
	tunnelHdr := map[string]string{"Host": "calm-river.trycloudflare.com", "Origin": "https://calm-river.trycloudflare.com",
		"Sec-Fetch-Site": "same-origin", "Cf-Connecting-Ip": "203.0.113.9", HeaderCSRF: "tok123"}
	local := doControl(h2, map[string]string{"Host": "127.0.0.1:3000", "Origin": "http://127.0.0.1:3000", HeaderCSRF: "tok123", HeaderAdmin: "adm"})
	if local.Code != 200 {
		t.Fatalf("local POST with the admin token = %d: %s", local.Code, local.Body.String())
	}
	if w := doControl(h2, tunnelHdr); w.Code != 401 || !strings.Contains(w.Body.String(), "admin token") {
		t.Errorf("tunnel POST without the admin token = %d: %s", w.Code, w.Body.String())
	}
	tunnelHdr[HeaderAdmin] = "adm"
	if w := doControl(h2, tunnelHdr); w.Code != 200 {
		t.Errorf("tunnel POST with the admin token = %d: %s", w.Code, w.Body.String())
	}
	// The session cookie set through the tunnel is Secure; locally it isn't.
	for host, secure := range map[string]bool{"calm-river.trycloudflare.com": true, "127.0.0.1:3000": false} {
		origin := "http://" + host
		if secure {
			origin = "https://" + host
		}
		w := do(h2, "POST", "/api/admin/session", map[string]string{"Host": host, "Origin": origin, HeaderCSRF: "tok123", HeaderAdmin: "adm"})
		if w.Code != 200 || len(w.Result().Cookies()) != 1 || w.Result().Cookies()[0].Secure != secure {
			t.Errorf("%s: session %d, cookies %v, want Secure %v", host, w.Code, w.Result().Cookies(), secure)
		}
	}
	// The in-process tunnel connects from 127.0.0.1: its name alone says the
	// viewer is elsewhere (Home's Wi-Fi wording), proxy headers or not.
	r := httptest.NewRequest(http.MethodGet, "/api/home?part=now", nil)
	r.RemoteAddr, r.Host = "127.0.0.1:5000", "calm-river.trycloudflare.com"
	if lh := h2.(*localHandler); lh.sameMachine(r) {
		t.Error("a tunnel request counts as this machine")
	}
}
