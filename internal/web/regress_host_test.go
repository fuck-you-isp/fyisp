package web

import (
	"encoding/json"
	"strings"
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
	w := do(h, "GET", "/api/status", map[string]string{"Host": "192.168.1.10:3000"})
	var st localStatus
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if st.Controls != "disabled" {
		t.Errorf("LAN bind without admin token: controls = %q, want disabled", st.Controls)
	}
	w = do(h, "POST", "/api/share/start", map[string]string{"Host": "192.168.1.10:3000", "Origin": "http://192.168.1.10:3000", HeaderCSRF: "tok123"})
	if w.Code != 403 {
		t.Errorf("LAN bind POST without admin token = %d, want 403", w.Code)
	}
	// Unknown names are refused with a hint to --allow-host.
	w = do(h, "GET", "/", map[string]string{"Host": "nas.local:3000"})
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
