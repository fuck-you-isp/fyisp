package web

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

// ReportOptions mirrors report.Options. The web package does not import
// internal/report (reports read web types such as HopStat), so wire an
// adapter:
//
//	type reports struct {
//		b report.Builder
//		store.ReportStore
//	}
//	func (r reports) Build(ctx context.Context, o web.ReportOptions) ([]byte, error) {
//		return r.b.Build(ctx, report.Options{From: o.From, To: o.To, Title: o.Title, Redact: o.Redact})
//	}
type ReportOptions struct {
	From, To time.Time
	Title    string
	Redact   bool
}

// ReportSource builds evidence reports and keeps their snapshots
// (report.Builder + store.ReportStore). Wire it as Deps.Reports; nil hides
// reports: every report route answers 404.
//
// The web layer chooses report IDs and the store must keep them: IDs of
// reports built with Redact start with "r", others with "f". Only an "r"
// report that is also marked Public is served on the public link, so a
// report with private details can never be published, whatever its flag.
type ReportSource interface {
	Build(ctx context.Context, o ReportOptions) (html []byte, err error)
	SaveReport(ctx context.Context, meta model.ReportMeta, html []byte) error
	Report(ctx context.Context, id string) (model.ReportMeta, []byte, error) // not found: see isNotFound
	Reports(ctx context.Context) ([]model.ReportMeta, error)                 // newest first
	SetReportPublic(ctx context.Context, id string, public bool) error
	DeleteReport(ctx context.Context, id string) error
}

// Report limits.
const (
	MaxReportTitle = 200
	MaxReportBytes = 16 << 20
	MaxReports     = 500 // listed, newest first
	reportTimeout  = 25 * time.Second
	reportIDLen    = 26 // prefix + 25 base32 characters (125 random bits)
)

// ReportCSP is the Content-Security-Policy of a served report: the report
// is one self-contained HTML document with inline CSS and SVG, no scripts.
const ReportCSP = "default-src 'none'; style-src 'unsafe-inline'; img-src data:; frame-ancestors 'none'; base-uri 'none'; form-action 'none'; sandbox allow-popups allow-popups-to-escape-sandbox"

var (
	reportIDRE       = regexp.MustCompile(`^[rf][a-z2-7]{25}$`)
	publicReportIDRE = regexp.MustCompile(`^r[a-z2-7]{25}$`)
	b32              = base32.StdEncoding.WithPadding(base32.NoPadding)
)

func newReportID(redacted bool) string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	p := "f"
	if redacted {
		p = "r"
	}
	return p + strings.ToLower(b32.EncodeToString(b[:]))[:reportIDLen-1]
}

// reportRedacted reports whether an ID was issued for a redacted build.
func reportRedacted(id string) bool { return publicReportIDRE.MatchString(id) }

// reportJSON is one report as listed locally.
type reportJSON struct {
	model.ReportMeta
	Redacted bool `json:"redacted"` // built without private details (may be published)
}

func toReportJSON(m model.ReportMeta) reportJSON {
	m.From, m.To, m.Created = m.From.UTC(), m.To.UTC(), m.Created.UTC()
	red := reportRedacted(m.ID)
	if !red {
		m.Public = false // never shown as published: the public route refuses it
	}
	return reportJSON{ReportMeta: m, Redacted: red}
}

// reportInput is the body of POST /api/reports.
type reportInput struct {
	From   time.Time `json:"from"`
	To     time.Time `json:"to"`
	Title  string    `json:"title"`
	Redact bool      `json:"redact"`
	Public bool      `json:"public"`
}

func (in *reportInput) validate(now time.Time) error {
	if in.From.IsZero() || in.To.IsZero() || in.From.Year() < 2000 {
		return badReq("from and to are required")
	}
	if in.To.After(now) {
		in.To = now
	}
	if !in.From.Before(in.To) {
		return badReq("from must be before to")
	}
	if in.To.Sub(in.From) > MaxRange {
		return badReq("a report may span at most 90 days")
	}
	if in.Public && !in.Redact {
		return badReq("only redacted reports can be published: a report with private details stays on this machine")
	}
	in.Title = strings.TrimSpace(in.Title)
	if !utf8.ValidString(in.Title) {
		return badReq("title is not valid UTF-8")
	}
	if n := utf8.RuneCountInString(in.Title); n > MaxReportTitle {
		return badReq("title is too long (%d characters, at most %d)", n, MaxReportTitle)
	}
	for _, r := range in.Title {
		if unicode.IsControl(r) {
			return badReq("title contains control characters")
		}
	}
	if in.Title == "" {
		in.Title = "Connection report"
	}
	in.From, in.To = in.From.UTC(), in.To.UTC()
	return nil
}

// serveReports answers /api/reports and /api/reports/{id} on the local
// listener: GET lists or shows (?download=1 as an attachment), POST builds
// and saves, PUT {"public":bool} publishes or withdraws, DELETE removes.
func (h *localHandler) serveReports(w http.ResponseWriter, r *http.Request, rest string) {
	if h.d.Reports == nil {
		writeErr(w, r, http.StatusNotFound, "reports are not available")
		return
	}
	if rest == "" {
		switch r.Method {
		case http.MethodGet, http.MethodHead:
			h.listReports(w, r)
		case http.MethodPost:
			h.createReport(w, r)
		default:
			w.Header().Set("Allow", "GET, HEAD, POST")
			writeErr(w, r, http.StatusMethodNotAllowed, "method not allowed")
		}
		return
	}
	if !reportIDRE.MatchString(rest) {
		writeErr(w, r, http.StatusNotFound, "not found")
		return
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		q := r.URL.Query()
		if err := checkParams(q, "download"); err != nil {
			writeErr(w, r, http.StatusBadRequest, err.Error())
			return
		}
		dl := q.Get("download")
		if dl != "" && dl != "1" {
			writeErr(w, r, http.StatusBadRequest, "download must be 1")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), notesTimeout)
		defer cancel()
		meta, body, err := h.d.Reports.Report(ctx, rest)
		if err != nil {
			h.storeErr(w, r, "load report", err)
			return
		}
		serveReportHTML(w, r, meta, body, dl == "1")
	case http.MethodPut:
		if len(r.URL.RawQuery) > 0 {
			writeErr(w, r, http.StatusBadRequest, "no parameters allowed")
			return
		}
		if !h.controlAllowed(w, r, "reports") {
			return
		}
		var in struct {
			Public *bool `json:"public"`
		}
		if !readJSON(w, r, &in) {
			return
		}
		if in.Public == nil {
			writeErr(w, r, http.StatusBadRequest, "public is required")
			return
		}
		if *in.Public && !reportRedacted(rest) {
			writeErr(w, r, http.StatusConflict, "this report includes private details and cannot be published; build a redacted one")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), notesTimeout)
		defer cancel()
		if err := h.d.Reports.SetReportPublic(ctx, rest, *in.Public); err != nil {
			h.storeErr(w, r, "update report", err)
			return
		}
		meta, _, err := h.d.Reports.Report(ctx, rest)
		if err != nil {
			h.storeErr(w, r, "load report", err)
			return
		}
		writeJSON(w, r, http.StatusOK, toReportJSON(meta))
	case http.MethodDelete:
		if len(r.URL.RawQuery) > 0 {
			writeErr(w, r, http.StatusBadRequest, "no parameters allowed")
			return
		}
		if !h.controlAllowed(w, r, "reports") {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), notesTimeout)
		defer cancel()
		if err := h.d.Reports.DeleteReport(ctx, rest); err != nil {
			h.storeErr(w, r, "delete report", err)
			return
		}
		writeJSON(w, r, http.StatusOK, map[string]any{"deleted": rest})
	default:
		w.Header().Set("Allow", "GET, HEAD, PUT, DELETE")
		writeErr(w, r, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (h *localHandler) listReports(w http.ResponseWriter, r *http.Request) {
	if err := checkParams(r.URL.Query()); err != nil {
		writeErr(w, r, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), notesTimeout)
	defer cancel()
	list, err := h.d.Reports.Reports(ctx)
	if err != nil {
		h.storeErr(w, r, "list reports", err)
		return
	}
	out := make([]reportJSON, 0, min(len(list), MaxReports))
	for _, m := range list {
		if len(out) >= MaxReports {
			break
		}
		out = append(out, toReportJSON(m))
	}
	writeJSON(w, r, http.StatusOK, out)
}

func (h *localHandler) createReport(w http.ResponseWriter, r *http.Request) {
	if len(r.URL.RawQuery) > 0 {
		writeErr(w, r, http.StatusBadRequest, "no parameters allowed")
		return
	}
	if !h.controlAllowed(w, r, "reports") {
		return
	}
	var in reportInput
	if !readJSON(w, r, &in) {
		return
	}
	now := h.now()
	if err := in.validate(now); err != nil {
		writeErr(w, r, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), reportTimeout)
	defer cancel()
	body, err := h.d.Reports.Build(ctx, ReportOptions{From: in.From, To: in.To, Title: in.Title, Redact: in.Redact})
	if err != nil {
		h.d.log().Warn("build report", "err", err)
		writeErr(w, r, http.StatusInternalServerError, "could not build the report: "+err.Error())
		return
	}
	if len(body) > MaxReportBytes {
		writeErr(w, r, http.StatusInternalServerError, "the report is too large; choose a shorter range")
		return
	}
	meta := model.ReportMeta{ID: newReportID(in.Redact), Title: in.Title, From: in.From, To: in.To,
		Created: now.UTC(), Public: in.Public && in.Redact, Bytes: len(body)}
	if err := h.d.Reports.SaveReport(ctx, meta, body); err != nil {
		h.storeErr(w, r, "save report", err)
		return
	}
	writeJSON(w, r, http.StatusCreated, toReportJSON(meta))
}

// serveReportHTML writes a stored report with the report CSP (it replaces
// the dashboard's).
func serveReportHTML(w http.ResponseWriter, r *http.Request, meta model.ReportMeta, body []byte, download bool) {
	hd := w.Header()
	hd.Set("Content-Security-Policy", ReportCSP)
	hd.Set("Cache-Control", "private, no-store")
	if download {
		hd.Set("Content-Disposition", `attachment; filename="`+reportFileName(meta)+`"`)
	}
	writeBody(w, r, http.StatusOK, "text/html; charset=utf-8", body)
}

func reportFileName(m model.ReportMeta) string {
	return "fyisp-report-" + m.From.UTC().Format("20060102T1504Z") + "-" + m.To.UTC().Format("20060102T1504Z") + ".html"
}

// servePublicReport answers /s/<secret>/r/<id>: a report snapshot that is
// marked public and was built redacted. Anything else is the same 404.
func (s *server) servePublicReport(w http.ResponseWriter, r *http.Request, id string) {
	if s.d.Reports == nil || !publicReportIDRE.MatchString(id) {
		notFound(w, r)
		return
	}
	if err := checkParams(r.URL.Query()); err != nil {
		writeErr(w, r, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), notesTimeout)
	defer cancel()
	if s.sem != nil {
		select {
		case s.sem <- struct{}{}:
			defer func() { <-s.sem }()
		case <-ctx.Done():
			w.Header().Set("Retry-After", "2")
			writeErr(w, r, http.StatusServiceUnavailable, "busy, retry shortly")
			return
		}
	}
	meta, body, err := s.d.Reports.Report(ctx, id)
	if err != nil {
		if !isNotFound(err) {
			s.d.log().Warn("public report", "err", err)
		}
		notFound(w, r)
		return
	}
	if !meta.Public || meta.ID != id || !reportRedacted(meta.ID) {
		notFound(w, r)
		return
	}
	serveReportHTML(w, r, meta, body, false)
}
