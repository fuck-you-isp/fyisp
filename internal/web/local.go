package web

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"net"
	"net/http"
	"strings"
	"time"
)

// LocalOptions configures the local (owner) listener.
type LocalOptions struct {
	// Addr is the address actually listened on, e.g. "127.0.0.1:3000"
	// (after any port fallback). Its port is required in the Host header
	// for loopback names unless the listener binds all interfaces.
	Addr string
	// ExtraHosts are additional allowed Host header values, e.g.
	// "fyisp.lan:3000" or "fyisp.lan" (any port). Compared case-insensitively.
	ExtraHosts []string
	// PublicBind reports that the listener accepts non-loopback clients
	// (e.g. --listen 0.0.0.0:3000). It is also inferred from an Addr whose
	// host is unspecified or a non-loopback IP (--listen 192.168.1.10:3000).
	// Then any IP-literal Host is allowed (DNS rebinding needs a hostname),
	// and the POST routes are disabled unless AdminToken is set (fyisp
	// generates one unless --admin-token=off).
	PublicBind bool
	// AdminToken, if set, must be sent as X-FYISP-Admin on POST routes, or
	// as the session cookie that POST /api/admin/session sets in exchange
	// for it (the "open this link" URL in the logs).
	AdminToken string
	// CSRFToken overrides the random per-process token (tests only).
	CSRFToken string
}

// Header names used by the local POST routes.
const (
	HeaderCSRF  = "X-FYISP-Token"
	HeaderAdmin = "X-FYISP-Admin"
)

// adminCookieMaxAge is the session cookie's lifetime (browsers cap it at
// 400 days). Changing the admin token revokes every session at once.
const adminCookieMaxAge = 400 * 24 * 3600

type localHandler struct {
	*server
	opts     LocalOptions
	port     string
	self     string // bound IP literal (normalized), "" for a wildcard bind
	extra    map[string]bool
	csrf     string
	controls string
}

// Local returns the owner's handler: the UI at /, the API under /api/,
// /metrics, and the write routes (POST /api/share/start|stop, POST
// /api/speed/run|config, POST/PUT/DELETE /api/annotations[/{id}] and
// /api/reports[/{id}]), all behind controlAllowed; POST /api/investigate,
// behind the same-origin and CSRF checks only (no admin token); and
// POST/DELETE /api/admin/session, which exchanges the admin token for a
// cookie.
func Local(d Deps, o LocalOptions) http.Handler {
	tok := o.CSRFToken
	if tok == "" {
		var b [16]byte
		_, _ = rand.Read(b[:])
		tok = hex.EncodeToString(b[:])
	}
	h := &localHandler{server: newServer(d, false, tok), opts: o, csrf: tok, extra: map[string]bool{}}
	if host, port, err := net.SplitHostPort(o.Addr); err == nil {
		h.port = port
		ip := net.ParseIP(host)
		switch {
		case host == "" || (ip != nil && ip.IsUnspecified()):
			h.opts.PublicBind = true
		case ip != nil:
			h.self = ip.String()
			if !ip.IsLoopback() {
				h.opts.PublicBind = true // a LAN address: same rules as 0.0.0.0
			}
		}
	}
	for _, e := range o.ExtraHosts {
		h.extra[strings.ToLower(e)] = true
	}
	switch {
	case o.AdminToken != "":
		h.controls = "admin"
	case h.opts.PublicBind:
		h.controls = "disabled"
	default:
		h.controls = "enabled"
	}
	h.canControl = func(r *http.Request) bool {
		return h.controls != "disabled" && (h.opts.AdminToken == "" || h.adminOK(r))
	}
	return h
}

func (h *localHandler) hostAllowed(hostport string) bool {
	hostport = strings.ToLower(hostport)
	if hostport == "" {
		return false
	}
	if h.extra[hostport] {
		return true
	}
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		host, port = hostport, "80"
		if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
			host = host[1 : len(host)-1]
		}
	}
	if h.extra[host] {
		return true
	}
	portOK := h.opts.PublicBind || h.port == "" || h.port == "0" || port == h.port
	switch host {
	case "127.0.0.1", "localhost", "::1":
		return portOK
	}
	if ip := net.ParseIP(host); ip != nil && h.self != "" && ip.String() == h.self {
		return portOK // the address we are bound to
	}
	// IP literals cannot be DNS-rebound; allow them when LAN access is on.
	return h.opts.PublicBind && net.ParseIP(host) != nil
}

func (h *localHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w.Header())
	if !h.hostAllowed(r.Host) {
		writeErr(w, r, http.StatusMisdirectedRequest, "unknown host: open "+h.localURL()+
			" or allow this name with --allow-host (e.g. --allow-host nas.local)")
		return
	}
	p := r.URL.Path
	switch p {
	case "/metrics":
		if h.d.Metrics == nil {
			http.NotFound(w, r)
			return
		}
		if err := checkParams(r.URL.Query()); err != nil {
			writeErr(w, r, http.StatusBadRequest, err.Error())
			return
		}
		if allowGET(w, r) {
			h.d.Metrics.ServeHTTP(w, r)
		}
		return
	case "/api/share/start", "/api/share/stop":
		h.serveShare(w, r, p == "/api/share/start")
		return
	case "/api/investigate":
		h.serveInvestigate(w, r)
		return
	case "/api/admin/session":
		h.serveAdminSession(w, r)
		return
	case "/api/speed/run":
		h.serveSpeedRun(w, r)
		return
	case "/api/speed/config":
		h.serveSpeedConfig(w, r)
		return
	case "/api/speed/cancel":
		h.serveSpeedCancel(w, r)
		return
	}
	if rest, ok := strings.CutPrefix(p, "/api/annotations"); ok && (rest == "" || (rest[0] == '/' && len(rest) > 1)) {
		h.serveNotes(w, r, strings.TrimPrefix(rest, "/"))
		return
	}
	if rest, ok := strings.CutPrefix(p, "/api/reports"); ok && (rest == "" || (rest[0] == '/' && len(rest) > 1)) {
		h.serveReports(w, r, strings.TrimPrefix(rest, "/"))
		return
	}
	if !strings.HasPrefix(p, "/") {
		http.NotFound(w, r)
		return
	}
	rel := p[1:]
	rt := lookup(rel)
	if rt == routeNone {
		writeErr(w, r, http.StatusNotFound, "not found")
		return
	}
	if !allowGET(w, r) {
		return
	}
	if rt == routeStatus {
		if err := checkParams(r.URL.Query()); err != nil {
			writeErr(w, r, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, r, http.StatusOK, localStatus{Status: h.rawStatus(r.Context()), Mode: "local", Controls: h.controls,
			Admin: h.controls == "admin" && h.adminOK(r)})
		return
	}
	h.serveGET(w, r, rt, rel)
}

// localURL is the address to suggest when the Host is refused.
func (h *localHandler) localURL() string {
	host := "127.0.0.1"
	if h.self != "" {
		host = h.self
		if strings.Contains(host, ":") {
			host = "[" + host + "]"
		}
	}
	return "http://" + host + portSuffix(h.port) + "/"
}

func portSuffix(p string) string {
	if p == "" || p == "80" || p == "0" {
		return ""
	}
	return ":" + p
}

// sameOrigin requires browser evidence that the request comes from our own
// page: Sec-Fetch-Site: same-origin and/or an Origin matching the Host.
func sameOrigin(r *http.Request) bool {
	sfs := r.Header.Get("Sec-Fetch-Site")
	origin := r.Header.Get("Origin")
	if sfs == "" && origin == "" {
		return false
	}
	if sfs != "" && sfs != "same-origin" {
		return false
	}
	if origin != "" {
		o := strings.ToLower(origin)
		host := strings.ToLower(r.Host)
		if o != "http://"+host && o != "https://"+host {
			return false
		}
	}
	return true
}

func tokenEqual(got, want string) bool {
	return want != "" && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// controlAllowed applies the POST routes' protections: controls enabled,
// same origin, CSRF token and (when set) the admin token. It writes the
// refusal and returns false.
func (h *localHandler) controlAllowed(w http.ResponseWriter, r *http.Request, what string) bool {
	if h.controls == "disabled" {
		writeErr(w, r, http.StatusForbidden, what+" are disabled: the dashboard is reachable from the network (--listen) and fyisp was started with --admin-token=off")
		return false
	}
	if !sameOrigin(r) {
		writeErr(w, r, http.StatusForbidden, "cross-origin request refused")
		return false
	}
	if !tokenEqual(r.Header.Get(HeaderCSRF), h.csrf) {
		writeErr(w, r, http.StatusForbidden, "missing or bad "+HeaderCSRF)
		return false
	}
	if h.opts.AdminToken != "" && !h.adminOK(r) {
		writeErr(w, r, http.StatusForbidden, "admin token required")
		return false
	}
	return true
}

// adminCookie is the session cookie's name. Cookies are shared by every
// port of a host, so the port keeps two dashboards on one host apart.
func (h *localHandler) adminCookie() string {
	if h.port == "" || h.port == "0" {
		return "fyisp_admin"
	}
	return "fyisp_admin_" + h.port
}

// adminOK reports whether r carries the admin token, as X-FYISP-Admin or
// as the session cookie.
func (h *localHandler) adminOK(r *http.Request) bool {
	if tokenEqual(r.Header.Get(HeaderAdmin), h.opts.AdminToken) {
		return true
	}
	c, err := r.Cookie(h.adminCookie())
	return err == nil && tokenEqual(c.Value, h.opts.AdminToken)
}

// serveAdminSession: POST exchanges the admin token (X-FYISP-Admin, taken
// by the page from the link's #admin= fragment, which never reaches a
// server log or a Referer) for an HttpOnly, SameSite=Strict cookie; DELETE
// clears it. Both need the usual same-origin and CSRF checks.
func (h *localHandler) serveAdminSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodDelete {
		w.Header().Set("Allow", "POST, DELETE")
		writeErr(w, r, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if len(r.URL.RawQuery) > 0 {
		writeErr(w, r, http.StatusBadRequest, "no parameters allowed")
		return
	}
	if h.opts.AdminToken == "" {
		writeErr(w, r, http.StatusNotFound, "no admin token is set")
		return
	}
	if !sameOrigin(r) {
		writeErr(w, r, http.StatusForbidden, "cross-origin request refused")
		return
	}
	if !tokenEqual(r.Header.Get(HeaderCSRF), h.csrf) {
		writeErr(w, r, http.StatusForbidden, "missing or bad "+HeaderCSRF)
		return
	}
	c := &http.Cookie{Name: h.adminCookie(), Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode}
	if r.Method == http.MethodDelete {
		c.MaxAge = -1
		http.SetCookie(w, c)
		writeJSON(w, r, http.StatusOK, map[string]any{"admin": false})
		return
	}
	if !tokenEqual(r.Header.Get(HeaderAdmin), h.opts.AdminToken) {
		writeErr(w, r, http.StatusForbidden, "this admin link is not valid (any more)")
		return
	}
	c.Value, c.MaxAge = h.opts.AdminToken, adminCookieMaxAge
	http.SetCookie(w, c)
	writeJSON(w, r, http.StatusOK, map[string]any{"admin": true})
}

func (h *localHandler) serveShare(w http.ResponseWriter, r *http.Request, start bool) {
	if h.d.Share == nil {
		writeErr(w, r, http.StatusNotFound, "sharing is not available")
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeErr(w, r, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if len(r.URL.RawQuery) > 0 {
		writeErr(w, r, http.StatusBadRequest, "no parameters allowed")
		return
	}
	if !h.controlAllowed(w, r, "share controls") {
		return
	}
	var err error
	if start {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		err = h.d.Share.Start(ctx)
		cancel()
	} else {
		err = h.d.Share.Stop()
	}
	if err != nil {
		h.d.log().Warn("share control", "start", start, "err", err)
		writeJSON(w, r, http.StatusInternalServerError, map[string]any{"error": err.Error(), "share": h.d.Share.State()})
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"share": h.d.Share.State()})
}
