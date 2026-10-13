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

	"github.com/fuck-you-isp/fyisp/internal/netinfo"
)

//go:embed static
var staticFS embed.FS

type asset struct {
	body, gz []byte
	etag     string
	ctype    string
}

// assets maps "static/<path>" to its content. index.html is rendered per
// handler instead (it carries the CSRF token).
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

// server holds the read-only API (the local handler adds the write
// routes).
type server struct {
	d     Deps
	index []byte
	now   func() time.Time
	inv   *Investigations // on-demand traces
	// canControlSpeed reports whether r may use the speed-test controls
	// (local only; api/speed's "configurable", api/home's "can_control").
	canControlSpeed func(r *http.Request) bool
	// sameMachine reports whether r comes from the machine fyisp runs on
	// (viewer.go; the Home's Wi-Fi wording). nil: never.
	sameMachine  func(r *http.Request) bool
	inContainer  func() bool     // netinfo.InContainer
	home         *homeCache      // /api/home
	homeSpeedSrc homeSpeedSource // nil: s.d.Speed
}

func newServer(d Deps, token string) *server {
	d.firsts = &firstCache{}
	html := strings.ReplaceAll(indexTmpl, "{{token}}", token)
	return &server{d: d, index: []byte(html), now: time.Now, inv: NewInvestigations(),
		inContainer: netinfo.InContainer, home: newHomeCache()}
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
	routeTrace
	routeTracePanel
	routeRouteChanges
	routeBaselines
	routeOverview
	routeCalls
	routeHome
	routeSpeed
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
	case "api/trace":
		return routeTrace
	case "api/trace/panel":
		return routeTracePanel
	case "api/routes/changes":
		return routeRouteChanges
	case "api/baselines":
		return routeBaselines
	case "api/overview":
		return routeOverview
	case "api/calls":
		return routeCalls
	case "api/home":
		return routeHome
	case "api/speed":
		return routeSpeed
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
	case routeStatic, routeStatus, routeProfile: // the index ignores params (e.g. ?fbclid=)
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
		writeJSON(w, r, http.StatusOK, s.rawStatus(r.Context()))
	case routeProfile:
		pj := buildProfile(s.d.Profile())
		pj.Features.Baselines = s.d.Baselines != nil
		pj.Features.Speed = s.d.Speed != nil
		if s.d.Trace != nil {
			pj.Features.Trace = true
			tr := s.traced()
			for i := range pj.Targets {
				pj.Targets[i].Trace = tr[pj.Targets[i].Name]
			}
		}
		writeJSON(w, r, http.StatusOK, pj)
	case routePanel, routePanelCSV:
		s.servePanel(w, r, rt == routePanelCSV)
	case routeVerdict:
		s.serveVerdict(w, r)
	case routeIncidents:
		s.serveIncidents(w, r)
	case routeBaselines:
		s.serveBaselines(w, r)
	case routeOverview:
		s.serveOverview(w, r)
	case routeCalls:
		s.serveCalls(w, r)
	case routeHome:
		s.serveHome(w, r)
	case routeSpeed:
		s.serveSpeed(w, r)
	case routeTrace, routeTracePanel, routeRouteChanges:
		switch {
		case s.d.Trace == nil:
			writeErr(w, r, http.StatusNotFound, "not found")
		case rt == routeTrace:
			s.serveTrace(w, r)
		case rt == routeTracePanel:
			s.serveTracePanel(w, r)
		default:
			s.serveRouteChanges(w, r)
		}
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

// localStatus is /api/status.
type localStatus struct {
	Status
	Controls string `json:"controls"` // speed-test controls: "enabled", "admin" (need the admin token) or "disabled"
	Admin    bool   `json:"admin"`    // "admin" controls and this request carries the token (header or cookie)
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
	if s.d.Tiles != nil {
		n := s.d.Tiles.LateSamples()
		st.TilesLate = &n
	}
	if st.Targets == 0 {
		if p := s.d.Profile(); p != nil {
			st.Targets = len(p.Targets)
		}
	}
	return st
}

// servePanel answers /api/panel and /api/panel.csv.
func (s *server) servePanel(w http.ResponseWriter, r *http.Request, asCSV bool) {
	now := s.now()
	p, err := parsePanelParams(r.URL.Query(), now)
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, err.Error())
		return
	}
	render := func(ctx context.Context) (*cached, error) {
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
	c, err := render(ctx)
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
		writeErr(w, r, http.StatusInternalServerError, "query failed: "+err.Error())
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

func notFound(w http.ResponseWriter, r *http.Request) {
	writeErr(w, r, http.StatusNotFound, "not found")
}
