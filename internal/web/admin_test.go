package web

import (
	"net/http"
	"strings"
	"testing"
)

const testAdmin = "0123456789abcdef0123456789abcdef"

// Most tests build a Local without an AdminToken, which then asks for none
// (testNoAdmin); the ones here set one.
func init() { testNoAdmin = true }

// TestAdminRequired: with the admin token set, only the page, its static
// files and the session exchange are open; everything else needs the
// token, as X-FYISP-Admin or "Authorization: Bearer".
func TestAdminRequired(t *testing.T) {
	h, sh := localHandlerFor(t, LocalOptions{AdminToken: testAdmin})
	host := map[string]string{"Host": "127.0.0.1:3000"}
	open := []string{"/"}
	for rel := range assets {
		if strings.HasPrefix(rel, "static/") {
			open = append(open, "/"+rel)
		}
	}
	if len(open) < 10 {
		t.Fatalf("only %d open paths: %v", len(open), open)
	}
	for _, p := range open {
		if w := do(h, "GET", p, host); w.Code != 200 {
			t.Errorf("GET %s = %d, want 200 without the token", p, w.Code)
		}
	}
	for _, c := range []struct{ method, path string }{{"HEAD", "/api/status"}, {"OPTIONS", "/api/status"}, {"GET", "/static/nope.js"},
		{"GET", "/static/"}, {"GET", "//api/status"}, {"GET", "/api/status/"}} {
		if w := do(h, c.method, c.path, host); w.Code != 401 {
			t.Errorf("%s %s = %d, want 401", c.method, c.path, w.Code)
		}
	}
	for _, p := range []string{"/api/status", "/api/profile", "/api/home?part=now", "/api/speed", "/api/trace?target=Alpha", "/api/probes?target=Alpha&kind=tcp&over_ms=5", "/metrics", "/api/nope"} {
		w := do(h, "GET", p, host)
		if w.Code != 401 || w.Header().Get("WWW-Authenticate") != `Bearer realm="fyisp"` || strings.Contains(w.Body.String(), testAdmin) {
			t.Errorf("GET %s = %d %q, want 401 with WWW-Authenticate", p, w.Code, w.Header().Get("WWW-Authenticate"))
		}
	}
	write := map[string]string{"Host": "127.0.0.1:3000", "Origin": "http://127.0.0.1:3000", "Sec-Fetch-Site": "same-origin", HeaderCSRF: "tok123"}
	if w := doControl(h, write); w.Code != 401 || sh.n != 0 {
		t.Errorf("control without the token = %d", w.Code)
	}
	if w := do(h, "POST", "/api/investigate?target=Alpha", write); w.Code != 401 {
		t.Errorf("investigate without the token = %d", w.Code)
	}
	for name, c := range map[string]struct {
		k, v string
		code int
	}{
		"header":       {HeaderAdmin, testAdmin, 200},
		"bearer":       {"Authorization", "Bearer " + testAdmin, 200},
		"bad bearer":   {"Authorization", "Bearer " + strings.Repeat("0", 32), 401},
		"basic":        {"Authorization", "Basic " + testAdmin, 401},
		"lower bearer": {"Authorization", "bearer " + testAdmin, 200},
		"no space":     {"Authorization", "Bearer" + testAdmin, 401},
		"bad header":   {HeaderAdmin, testAdmin + "x", 401},
	} {
		if w := do(h, "GET", "/api/status", map[string]string{"Host": "127.0.0.1:3000", c.k: c.v}); w.Code != c.code {
			t.Errorf("%s = %d, want %d", name, w.Code, c.code)
		}
	}
	// The token doesn't stand in for the write checks.
	if w := doControl(h, map[string]string{"Host": "127.0.0.1:3000", HeaderAdmin: testAdmin}); w.Code != 403 || sh.n != 0 {
		t.Errorf("control with the token but no CSRF or origin = %d", w.Code)
	}
	ok := map[string]string{HeaderAdmin: testAdmin}
	for k, v := range write {
		ok[k] = v
	}
	if w := doControl(h, ok); w.Code != 200 || sh.n != 1 {
		t.Errorf("control with the token = %d", w.Code)
	}
}

// The CSRF token comes from the admin token: the same after a restart with
// the same token (an open page's writes keep working), new with a new one.
func TestCSRFFromAdminToken(t *testing.T) {
	csrf := func(tok string) string {
		h := Local(Deps{}, LocalOptions{Addr: "127.0.0.1:3000", AdminToken: tok}).(*localHandler)
		return h.csrf
	}
	a, b := csrf(testAdmin), csrf(testAdmin)
	if a == "" || a != b || a == csrf(strings.Repeat("f", 32)) || strings.Contains(a, testAdmin) ||
		a == derive(testAdmin, "fyisp session cookie") {
		t.Errorf("csrf %q %q", a, b)
	}
}

func TestLocalNeedsAdminToken(t *testing.T) {
	testNoAdmin = false
	defer func() { testNoAdmin = true }()
	defer func() {
		if recover() == nil {
			t.Error("Local without an AdminToken did not panic")
		}
	}()
	Local(Deps{}, LocalOptions{Addr: "127.0.0.1:3000"})
}

func TestAdminSession(t *testing.T) {
	h, sh := localHandlerFor(t, LocalOptions{Addr: "0.0.0.0:3000", AdminToken: testAdmin})
	good := map[string]string{"Host": "192.168.1.10:3000", "Origin": "http://192.168.1.10:3000", "Sec-Fetch-Site": "same-origin", HeaderCSRF: "tok123"}
	with := func(over map[string]string, drop ...string) map[string]string {
		m := map[string]string{}
		for k, v := range good {
			m[k] = v
		}
		for k, v := range over {
			m[k] = v
		}
		for _, k := range drop {
			delete(m, k)
		}
		return m
	}
	signedIn := func(hdr map[string]string) bool {
		return do(h, "GET", "/api/status", hdr).Code == 200
	}

	// The exchange needs the right token, the Host allowlist and, from a
	// browser, Sec-Fetch-Site: same-origin.
	for _, c := range []struct {
		name string
		hdr  map[string]string
		code int
	}{
		{"no token", good, 403},
		{"bad token", with(map[string]string{HeaderAdmin: testAdmin[:31] + "0"}), 403},
		{"cross-site", with(map[string]string{HeaderAdmin: testAdmin, "Sec-Fetch-Site": "cross-site"}), 403},
		{"same-site", with(map[string]string{HeaderAdmin: testAdmin, "Sec-Fetch-Site": "same-site"}), 403},
		{"foreign host", with(map[string]string{HeaderAdmin: testAdmin, "Host": "evil.com:3000", "Origin": "http://evil.com:3000"}), 421},
	} {
		w := do(h, "POST", "/api/admin/session", c.hdr)
		if w.Code != c.code || w.Header().Get("Set-Cookie") != "" {
			t.Errorf("%s: %d %q, want %d and no cookie", c.name, w.Code, w.Header().Get("Set-Cookie"), c.code)
		}
	}
	for _, m := range []string{"GET", "PUT"} {
		if w := do(h, m, "/api/admin/session", with(map[string]string{HeaderAdmin: testAdmin})); w.Code != 405 {
			t.Errorf("%s = %d, want 405", m, w.Code)
		}
	}
	if w := do(h, "POST", "/api/admin/session?x=1", with(map[string]string{HeaderAdmin: testAdmin})); w.Code != 400 {
		t.Errorf("query = %d, want 400", w.Code)
	}

	// No CSRF token or Origin needed: a page elsewhere can't send the token
	// header (CORS preflight), and a proxy that rewrites Host to the
	// listener's address makes the owner's own Origin look foreign.
	for name, hdr := range map[string]map[string]string{
		"no CSRF":        with(map[string]string{HeaderAdmin: testAdmin}, HeaderCSRF),
		"Host rewritten": with(map[string]string{HeaderAdmin: testAdmin, "Host": "127.0.0.1:3000", "Origin": "https://nas.example"}),
	} {
		if w := do(h, "POST", "/api/admin/session", hdr); w.Code != 200 {
			t.Errorf("%s: %d %s, want 200", name, w.Code, w.Body.String())
		}
	}

	w := do(h, "POST", "/api/admin/session", with(map[string]string{HeaderAdmin: testAdmin}))
	if w.Code != 200 {
		t.Fatalf("exchange = %d %s", w.Code, w.Body.String())
	}
	cs := w.Result().Cookies()
	if len(cs) != 1 {
		t.Fatalf("cookies = %v", cs)
	}
	c := cs[0]
	if c.Name != "fyisp_admin_3000" || c.Value != derive(testAdmin, "fyisp session cookie") || strings.Contains(c.Value, testAdmin) || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode ||
		c.Path != "/" || c.Domain != "" || c.MaxAge != adminCookieMaxAge {
		t.Errorf("cookie = %+v", c)
	}
	cookie := c.Name + "=" + c.Value

	// The cookie stands in for X-FYISP-Admin, but not for CSRF or origin.
	if signedIn(good) || !signedIn(with(map[string]string{"Cookie": cookie})) {
		t.Error("the cookie does not sign in")
	}
	for name, hdr := range map[string]map[string]string{
		"no cookie":      good,
		"other port":     with(map[string]string{"Cookie": "fyisp_admin_3001=" + derive(testAdmin, "fyisp session cookie")}),
		"bad value":      with(map[string]string{"Cookie": "fyisp_admin_3000=" + strings.Repeat("0", 64)}),
		"token as value": with(map[string]string{"Cookie": "fyisp_admin_3000=" + testAdmin}),
	} {
		if w := doControl(h, hdr); w.Code != 401 || sh.n != 0 {
			t.Errorf("%s = %d, want 401", name, w.Code)
		}
	}
	for name, hdr := range map[string]map[string]string{
		"no CSRF":       with(map[string]string{"Cookie": cookie}, HeaderCSRF),
		"cross-site":    with(map[string]string{"Cookie": cookie, "Sec-Fetch-Site": "cross-site"}),
		"wrong origin":  with(map[string]string{"Cookie": cookie, "Origin": "http://evil.com"}, "Sec-Fetch-Site"),
		"no origin set": with(map[string]string{"Cookie": cookie}, "Origin", "Sec-Fetch-Site"),
	} {
		if w := doControl(h, hdr); w.Code != 403 {
			t.Errorf("%s = %d, want 403", name, w.Code)
		}
	}
	if w := doControl(h, with(map[string]string{"Cookie": cookie})); w.Code != 200 || sh.n != 1 {
		t.Errorf("cookie = %d (controls %d)", w.Code, sh.n)
	}

	// A new token (rotation) revokes the cookie.
	h2, sh2 := localHandlerFor(t, LocalOptions{Addr: "0.0.0.0:3000", AdminToken: strings.Repeat("f", 32)})
	if w := doControl(h2, with(map[string]string{"Cookie": cookie})); w.Code != 401 || sh2.n != 0 {
		t.Errorf("old cookie after rotation = %d", w.Code)
	}

	// Through an HTTPS page (the owner's own HTTPS proxy), the cookie is Secure.
	hs, _ := localHandlerFor(t, LocalOptions{Addr: "127.0.0.1:3000", AdminToken: testAdmin, ExtraHosts: []string{"fyisp.example.com"}})
	for name, hdr := range map[string]map[string]string{
		"https Origin":      with(map[string]string{HeaderAdmin: testAdmin, "Host": "fyisp.example.com", "Origin": "https://fyisp.example.com"}),
		"X-Forwarded-Proto": with(map[string]string{HeaderAdmin: testAdmin, "Host": "fyisp.example.com", "X-Forwarded-Proto": "https"}, "Origin"),
	} {
		w := do(hs, "POST", "/api/admin/session", hdr)
		if cs := w.Result().Cookies(); w.Code != 200 || len(cs) != 1 || !cs[0].Secure {
			t.Errorf("%s: %d %v, want a Secure cookie", name, w.Code, cs)
		}
	}
	if cs := w.Result().Cookies(); cs[0].Secure {
		t.Error("cookie over plain HTTP is Secure")
	}

	// DELETE signs out.
	w = do(h, "DELETE", "/api/admin/session", with(map[string]string{"Cookie": cookie}))
	if cs := w.Result().Cookies(); w.Code != 200 || len(cs) != 1 || cs[0].MaxAge >= 0 || cs[0].Value != "" {
		t.Errorf("delete = %d %v", w.Code, cs)
	}
	if w := do(h, "DELETE", "/api/admin/session", with(map[string]string{"Sec-Fetch-Site": "cross-site"})); w.Code != 403 {
		t.Errorf("cross-site delete = %d", w.Code)
	}
}
