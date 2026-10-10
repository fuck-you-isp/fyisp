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
func (h *publicHandler) servePublicInvestigate(w http.ResponseWriter, r *http.Request) {
	if h.d.Trace == nil {
		notFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeErr(w, r, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !sameOrigin(r) {
		writeErr(w, r, http.StatusForbidden, "cross-origin request refused")
		return
	}
	key := clientKey(r)
	h.investigate(w, r, func() (bool, time.Duration) { return h.starts.allow(key, h.now()) })
}
