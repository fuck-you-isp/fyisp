package web

import (
	"crypto/subtle"
	"net"
	"net/http"
	"strings"
	"time"
)

// Public limits (see docs/PLAN.md, "Public listener").
const (
	PublicRate        = 20 // requests/s, all clients
	PublicBurst       = 40
	PublicClientRate  = 5  // requests/s per Cf-Connecting-Ip
	PublicClientBurst = 20 // one page load is ~15 requests
	PublicMaxQueries  = 8  // concurrent panel queries
	PublicCacheTTL    = 10 * time.Second
	// The response cache keeps at most PublicCacheEntries responses and
	// PublicCacheBytes in total; single responses over PublicCacheMaxEntry
	// are served but not kept.
	PublicCacheEntries  = 512
	PublicCacheBytes    = 32 << 20
	PublicCacheMaxEntry = 4 << 20
	// PublicCSVMaxPoints caps buckets per series in the public CSV export
	// (the local listener allows MaxPoints).
	PublicCSVMaxPoints = 500
	MinSecretLen       = 16
)

type publicHandler struct {
	*server
	prefix string // "/s/<secret>/"
	secret string
	lim    *limiter
	starts startLimiter // new on-demand traces per client
}

// Public returns the tunnel origin's handler. The UI and read-only API live
// under /s/<secret>/; every other path is 404. secret must be at least 16
// URL-safe characters (use 128 random bits).
func Public(d Deps, secret string) http.Handler {
	if len(secret) < MinSecretLen || strings.ContainsAny(secret, "/?#%") {
		panic("web: public secret too short or not URL-safe")
	}
	s := newServer(d, true, "")
	s.sem = make(chan struct{}, PublicMaxQueries)
	s.cache = newRespCache(PublicCacheTTL, PublicCacheEntries, PublicCacheBytes, PublicCacheMaxEntry)
	return &publicHandler{
		server: s,
		prefix: "/s/" + secret + "/",
		secret: secret,
		lim:    newLimiter(PublicRate, PublicBurst, PublicClientRate, PublicClientBurst),
	}
}

// clientKey identifies a public client by Cf-Connecting-Ip, which
// Cloudflare's edge sets on every tunnelled request (overwriting any value
// the client sent). That is trustworthy only because the public listener
// binds 127.0.0.1 and only the in-process tunnel connects to it: keep it
// loopback-only. Never log the key.
func clientKey(r *http.Request) string {
	if ip := r.Header.Get("Cf-Connecting-Ip"); ip != "" {
		return ip
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (h *publicHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	hd := w.Header()
	setSecurityHeaders(hd)
	hd.Set("X-Robots-Tag", "noindex, nofollow")
	if !h.lim.allow(clientKey(r), h.now()) {
		hd.Set("Cache-Control", "no-store")
		hd.Set("Retry-After", "1")
		writeErr(w, r, http.StatusTooManyRequests, "too many requests")
		return
	}
	// Match the secret segment in constant time, then route the rest.
	p := r.URL.Path
	rest, ok := strings.CutPrefix(p, "/s/")
	if !ok {
		notFound(w, r)
		return
	}
	seg, rel, hasSlash := strings.Cut(rest, "/")
	if subtle.ConstantTimeCompare([]byte(seg), []byte(h.secret)) != 1 {
		notFound(w, r)
		return
	}
	if !hasSlash {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			allowGET(w, r)
			return
		}
		http.Redirect(w, r, h.prefix, http.StatusMovedPermanently)
		return
	}
	if rel == "api/investigate" {
		h.servePublicInvestigate(w, r)
		return
	}
	if id, ok := strings.CutPrefix(rel, "r/"); ok {
		if !allowGET(w, r) {
			return
		}
		hd.Set("Cache-Control", "private, no-store")
		h.servePublicReport(w, r, id)
		return
	}
	rt := lookup(rel)
	if rt == routeNone {
		notFound(w, r)
		return
	}
	if !allowGET(w, r) {
		return
	}
	hd.Set("Cache-Control", "private, no-store")
	h.serveGET(w, r, rt, rel)
}

func notFound(w http.ResponseWriter, r *http.Request) {
	writeErr(w, r, http.StatusNotFound, "not found")
}

// servePublicInvestigate answers POST /s/<secret>/api/investigate?target=,
// the public listener's only write route: anyone with the link may trace a
// profile target. Same origin only; new starts are limited per client; the
// global MaxInvestigations is shared with the local listener.
//
// sameOrigin works behind the tunnel: cloudflared passes the visitor's Host
// (the trycloudflare hostname) to the origin unchanged, so a same-origin
// browser POST carries Origin https://<that host> (TestLiveSameOrigin in
// internal/tunnel checks this on a real tunnel).
func (h *publicHandler) servePublicInvestigate(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store") // every response, errors included
	target := h.investigateRequest(w, r)
	if target == "" {
		return
	}
	if !sameOrigin(r) {
		writeInvErr(w, r, http.StatusForbidden, "forbidden", "cross-origin request refused", 0)
		return
	}
	key := clientKey(r)
	h.investigate(w, r, target, func() (bool, time.Duration) { return h.starts.check(key, h.now()) },
		func() { h.starts.record(key, h.now()) })
}
