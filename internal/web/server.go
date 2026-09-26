package web

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"
)

//go:embed static
var staticFS embed.FS

type asset struct {
	body, gz []byte
	etag     string
	ctype    string
}

// assets maps "static/<path>" to its content. index.html is rendered per
// handler instead (it carries the mode and the CSRF token).
var (
	assets    map[string]*asset
	indexTmpl string
)

func init() {
	assets = map[string]*asset{}
	err := fs.WalkDir(staticFS, "static", func(p string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return err
		}
		b, err := staticFS.ReadFile(p)
		if err != nil {
			return err
		}
		if p == "static/index.html" {
			indexTmpl = string(b)
			return nil
		}
		sum := sha256.Sum256(b)
		a := &asset{body: b, etag: `"` + hex.EncodeToString(sum[:12]) + `"`, ctype: contentType(p)}
		var buf bytes.Buffer
		zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
		_, _ = zw.Write(b)
		_ = zw.Close()
		if buf.Len() < len(b) {
			a.gz = buf.Bytes()
		}
		assets[p] = a
		return nil
	})
	if err != nil || indexTmpl == "" {
		panic("web: embedded static files missing")
	}
}

func contentType(p string) string {
	switch path.Ext(p) {
	case ".js":
		return "text/javascript; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	case "", ".LICENSE":
		return "text/plain; charset=utf-8"
	}
	if t := mime.TypeByExtension(path.Ext(p)); t != "" {
		return t
	}
	return "application/octet-stream"
}

// server holds what the local and public handlers share.
type server struct {
	d      Deps
	public bool
	index  []byte
	now    func() time.Time
	sem    chan struct{} // bounds concurrent panel queries (public only)
	cache  *respCache    // public only
}

func newServer(d Deps, public bool, token string) *server {
	mode := "local"
	if public {
		mode = "public"
	}
	d.firsts = &firstCache{}
	html := strings.NewReplacer("{{mode}}", mode, "{{token}}", token).Replace(indexTmpl)
	return &server{d: d, public: public, index: []byte(html), now: time.Now}
}

// route is one GET route under the handler's base path.
type route int

const (
	routeNone route = iota
	routeIndex
	routeStatic
	routeStatus
	routeProfile
	routePanel
	routePanelCSV
	routeVerdict
	routeIncidents
)

func lookup(rel string) route {
	switch rel {
	case "":
		return routeIndex
	case "api/status":
		return routeStatus
	case "api/profile":
		return routeProfile
	case "api/panel":
		return routePanel
	case "api/panel.csv":
		return routePanelCSV
	case "api/verdict":
		return routeVerdict
	case "api/incidents":
		return routeIncidents
	}
	if _, ok := assets[rel]; ok && strings.HasPrefix(rel, "static/") {
		return routeStatic
	}
	return routeNone
}

// serveGET serves a known read-only route. The method has been checked.
func (s *server) serveGET(w http.ResponseWriter, r *http.Request, rt route, rel string) {
	q := r.URL.Query()
	switch rt {
	case routeStatic, routeStatus, routeProfile: // the index ignores params (e.g. ?fbclid= on shared links)
		if err := checkParams(q); err != nil {
			writeErr(w, r, http.StatusBadRequest, err.Error())
			return
		}
	}
	switch rt {
	case routeIndex:
		w.Header().Set("Cache-Control", "no-store")
		writeBody(w, r, http.StatusOK, "text/html; charset=utf-8", s.index)
	case routeStatic:
		serveAsset(w, r, assets[rel])
	case routeStatus:
		writeJSON(w, r, http.StatusOK, s.status(r.Context()))
	case routeProfile:
		writeJSON(w, r, http.StatusOK, buildProfile(s.d.Profile()))
	case routePanel, routePanelCSV:
		s.servePanel(w, r, rt == routePanelCSV)
	case routeVerdict:
		s.serveVerdict(w, r)
	case routeIncidents:
		s.serveIncidents(w, r)
	default:
		http.NotFound(w, r)
	}
}

func serveAsset(w http.ResponseWriter, r *http.Request, a *asset) {
	h := w.Header()
	h.Set("ETag", a.etag)
	h.Set("Cache-Control", "no-cache")
	h.Add("Vary", "Accept-Encoding")
	if inm := r.Header.Get("If-None-Match"); inm != "" && strings.Contains(inm, a.etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	b := a.body
	if a.gz != nil && acceptsGzip(r) {
		b = a.gz
		h.Set("Content-Encoding", "gzip")
	}
	h.Set("Content-Type", a.ctype)
	h.Set("Content-Length", strconv.Itoa(len(b)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(b)
	}
}

// localStatus is /api/status on the local listener.
type localStatus struct {
	Status
	Mode     string `json:"mode"`
	Controls string `json:"controls"` // "enabled", "admin" (needs admin token) or "disabled"
}

// publicStatus is the redacted /api/status: no version, no hints, no error
// strings, no paths, no share URL.
type publicStatus struct {
	Mode    string    `json:"mode"`
	Started time.Time `json:"started"`
	Caps    struct {
		ICMP  string `json:"icmp"`
		TCP   bool   `json:"tcp"`
		HTTPS bool   `json:"https"`
	} `json:"caps"`
	Targets int `json:"targets"`
	Ready   int `json:"ready"`
	Share   struct {
		Phase string `json:"phase"`
	} `json:"share"`
	Store struct {
		Oldest  time.Time `json:"oldest"`
		Healthy bool      `json:"healthy"`
	} `json:"store"`
}

func (s *server) rawStatus(ctx context.Context) Status {
	var st Status
	if s.d.Status != nil {
		st = s.d.Status()
	}
	if s.d.Store != nil {
		if stats, err := s.d.Store.Stats(ctx); err == nil {
			st.Store = stats
		} else {
			s.d.log().Warn("store stats", "err", err)
			st.Store.LastFlushErr = err.Error()
		}
	}
	if s.d.Share != nil {
		st.Share = s.d.Share.State()
	}
	if st.Share.Phase == "" {
		st.Share.Phase = ShareOff
	}
	if st.Targets == 0 {
		if p := s.d.Profile(); p != nil {
			st.Targets = len(p.Targets)
		}
	}
	return st
}

func redact(st Status) publicStatus {
	var p publicStatus
	p.Mode = "public"
	p.Started = st.Started
	p.Caps.ICMP, p.Caps.TCP, p.Caps.HTTPS = st.Caps.ICMP, st.Caps.TCP, st.Caps.HTTPS
	p.Targets, p.Ready = st.Targets, st.Ready
	p.Share.Phase = st.Share.Phase
	p.Store.Oldest = st.Store.Oldest
	p.Store.Healthy = st.Store.LastFlushErr == ""
	return p
}

func (s *server) status(ctx context.Context) any {
	st := s.rawStatus(ctx)
	if s.public {
		return redact(st)
	}
	return st // wrapped by the local handler
}

// servePanel answers /api/panel and /api/panel.csv.
func (s *server) servePanel(w http.ResponseWriter, r *http.Request, asCSV bool) {
	now := s.now()
	p, err := parsePanelParams(r.URL.Query(), now)
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, err.Error())
		return
	}
	if s.public {
		if asCSV {
			p.points = min(p.points, PublicCSVMaxPoints)
		}
		p.quantize(now)
	}
	render := func(ctx context.Context) (*cached, error) {
		if s.sem != nil {
			select {
			case s.sem <- struct{}{}:
				defer func() { <-s.sem }()
			case <-ctx.Done():
				return nil, errBusy
			}
		}
		pd, err := s.d.queryPanel(ctx, p, now)
		if err != nil {
			return nil, err
		}
		if asCSV {
			return &cached{code: http.StatusOK, ctype: "text/csv; charset=utf-8", body: pd.CSV(),
				disposition: `attachment; filename="` + pd.csvName() + `"`}, nil
		}
		return &cached{code: http.StatusOK, ctype: "application/json; charset=utf-8", body: pd.JSON()}, nil
	}
	ctx, cancel := context.WithTimeout(r.Context(), panelTimeout)
	defer cancel()
	var c *cached
	if s.cache != nil {
		key := "json\x00"
		if asCSV {
			key = "csv\x00"
		}
		c, err = s.cache.get(ctx, key+p.key, render)
	} else {
		c, err = render(ctx)
	}
	switch {
	case err == nil:
	case errors.Is(err, errUnknownGroup):
		writeErr(w, r, http.StatusNotFound, "unknown group")
		return
	case errors.Is(err, errBusy), errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		switch {
		case r.Context().Err() != nil:
			s.d.log().Debug("panel query: client went away", "group", p.group)
		case errors.Is(err, errBusy):
			s.d.log().Debug("panel query: busy", "group", p.group)
		default:
			s.d.log().Warn("panel query timed out", "group", p.group, "err", err)
		}
		w.Header().Set("Retry-After", "2")
		writeErr(w, r, http.StatusServiceUnavailable, "busy, retry shortly")
		return
	default:
		s.d.log().Error("panel query", "group", p.group, "err", err)
		msg := "query failed"
		if !s.public {
			msg += ": " + err.Error()
		}
		writeErr(w, r, http.StatusInternalServerError, msg)
		return
	}
	if c.disposition != "" {
		w.Header().Set("Content-Disposition", c.disposition)
	}
	w.Header().Set("Cache-Control", "no-store")
	writeBody(w, r, c.code, c.ctype, c.body)
}

var errBusy = errors.New("busy")

// panelTimeout bounds one panel query.
const panelTimeout = 20 * time.Second

func allowGET(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return true
	}
	w.Header().Set("Allow", "GET, HEAD")
	writeErr(w, r, http.StatusMethodNotAllowed, "method not allowed")
	return false
}
