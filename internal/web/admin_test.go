package web

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

const testAdmin = "0123456789abcdef0123456789abcdef"

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
	statusAdmin := func(hdr map[string]string) bool {
		var st localStatus
		_ = json.Unmarshal(do(h, "GET", "/api/status", hdr).Body.Bytes(), &st)
		if st.Controls != "admin" {
			t.Errorf("controls = %q", st.Controls)
		}
		return st.Admin
	}

	// The exchange keeps every POST protection and needs the right token.
	for _, c := range []struct {
		name string
		hdr  map[string]string
		code int
	}{
		{"no token", good, 403},
		{"bad token", with(map[string]string{HeaderAdmin: testAdmin[:31] + "0"}), 403},
		{"cross-origin", with(map[string]string{HeaderAdmin: testAdmin, "Origin": "http://evil.com"}, "Sec-Fetch-Site"), 403},
		{"cross-site", with(map[string]string{HeaderAdmin: testAdmin, "Sec-Fetch-Site": "cross-site"}), 403},
		{"no CSRF", with(map[string]string{HeaderAdmin: testAdmin}, HeaderCSRF), 403},
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

	w := do(h, "POST", "/api/admin/session", with(map[string]string{HeaderAdmin: testAdmin}))
	if w.Code != 200 {
		t.Fatalf("exchange = %d %s", w.Code, w.Body.String())
	}
	cs := w.Result().Cookies()
	if len(cs) != 1 {
		t.Fatalf("cookies = %v", cs)
	}
	c := cs[0]
	if c.Name != "fyisp_admin_3000" || c.Value != testAdmin || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode ||
		c.Path != "/" || c.Domain != "" || c.MaxAge != adminCookieMaxAge {
		t.Errorf("cookie = %+v", c)
	}
	cookie := c.Name + "=" + c.Value

	// The cookie stands in for X-FYISP-Admin, but not for CSRF or origin.
	if statusAdmin(good) || !statusAdmin(with(map[string]string{"Cookie": cookie})) {
		t.Error("status admin flag wrong")
	}
	if w := do(h, "DELETE", "/api/annotations/1", good); w.Code != 403 || sh.n != 0 {
		t.Errorf("no cookie = %d", w.Code)
	}
	for name, hdr := range map[string]map[string]string{
		"other port":    with(map[string]string{"Cookie": "fyisp_admin_3001=" + testAdmin}),
		"bad value":     with(map[string]string{"Cookie": "fyisp_admin_3000=" + strings.Repeat("0", 32)}),
		"no CSRF":       with(map[string]string{"Cookie": cookie}, HeaderCSRF),
		"cross-site":    with(map[string]string{"Cookie": cookie, "Sec-Fetch-Site": "cross-site"}),
		"wrong origin":  with(map[string]string{"Cookie": cookie, "Origin": "http://evil.com"}, "Sec-Fetch-Site"),
		"no origin set": with(map[string]string{"Cookie": cookie}, "Origin", "Sec-Fetch-Site"),
	} {
		if w := do(h, "DELETE", "/api/annotations/1", hdr); w.Code != 403 {
			t.Errorf("%s = %d, want 403", name, w.Code)
		}
	}
	if w := do(h, "DELETE", "/api/annotations/1", with(map[string]string{"Cookie": cookie})); w.Code != 200 || sh.n != 1 {
		t.Errorf("cookie = %d (controls %d)", w.Code, sh.n)
	}

	// A new token (rotation) revokes the cookie.
	h2, sh2 := localHandlerFor(t, LocalOptions{Addr: "0.0.0.0:3000", AdminToken: strings.Repeat("f", 32)})
	if w := do(h2, "DELETE", "/api/annotations/1", with(map[string]string{"Cookie": cookie})); w.Code != 403 || sh2.n != 0 {
		t.Errorf("old cookie after rotation = %d", w.Code)
	}

	// DELETE signs out.
	w = do(h, "DELETE", "/api/admin/session", with(map[string]string{"Cookie": cookie}))
	if cs := w.Result().Cookies(); w.Code != 200 || len(cs) != 1 || cs[0].MaxAge >= 0 || cs[0].Value != "" {
		t.Errorf("delete = %d %v", w.Code, cs)
	}
	if w := do(h, "DELETE", "/api/admin/session", with(nil, HeaderCSRF)); w.Code != 403 {
		t.Errorf("delete without CSRF = %d", w.Code)
	}

	// No admin token (loopback, or --admin-token=off): nothing to exchange.
	for _, o := range []LocalOptions{{}, {Addr: "0.0.0.0:3000"}} {
		hn, _ := localHandlerFor(t, o)
		hdr := with(map[string]string{HeaderAdmin: testAdmin, "Host": "127.0.0.1:3000", "Origin": "http://127.0.0.1:3000"})
		if w := do(hn, "POST", "/api/admin/session", hdr); w.Code != 404 || w.Header().Get("Set-Cookie") != "" {
			t.Errorf("%+v: exchange = %d", o, w.Code)
		}
	}
	// A loopback dashboard ignores cookies and stays enabled.
	hl, shl := localHandlerFor(t, LocalOptions{})
	if w := do(hl, "DELETE", "/api/annotations/1", with(map[string]string{"Host": "127.0.0.1:3000", "Origin": "http://127.0.0.1:3000"})); w.Code != 200 || shl.n != 1 {
		t.Errorf("loopback = %d", w.Code)
	}
}
