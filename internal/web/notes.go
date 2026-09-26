package web

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

// AnnotationStore keeps timeline notes. It is store.AnnotationStore
// (implemented by *store.SQLite and *store.Fake); wire it as
// Deps.Annotations. Nil hides notes: every annotation route answers 404.
type AnnotationStore interface {
	AddAnnotation(ctx context.Context, a *model.Annotation) error // sets ID, Created, Updated
	UpdateAnnotation(ctx context.Context, a *model.Annotation) error
	DeleteAnnotation(ctx context.Context, id int64) error
	// Annotations overlapping [from, to], oldest first. publicOnly limits
	// the result to notes marked Public.
	Annotations(ctx context.Context, from, to time.Time, publicOnly bool) ([]model.Annotation, error)
}

// Annotation limits.
const (
	MaxAnnotationText = 500  // characters
	MaxAnnotations    = 1000 // per response, oldest first
	maxJSONBody       = 8 << 10
	notesTimeout      = 10 * time.Second
)

// ErrNotFound is what Deps' stores return (wrapped or not) for a missing
// annotation or report; errors.Is(err, store.ErrNotFound) style sentinels
// named "not found" and sql.ErrNoRows are recognised too.
var ErrNotFound = errors.New("not found")

func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrNotFound) || errors.Is(err, sql.ErrNoRows) {
		return true
	}
	// store.ErrNotFound (and anything wrapping it) without importing a
	// sentinel that may not exist yet: match on the chain's messages.
	for e := err; e != nil; e = errors.Unwrap(e) {
		if strings.HasSuffix(e.Error(), "not found") {
			return true
		}
	}
	return false
}

// noteJSON is one annotation as served. Created/Updated are local only.
type noteJSON struct {
	ID      int64      `json:"id"`
	At      time.Time  `json:"at"`
	End     *time.Time `json:"end,omitempty"`
	Text    string     `json:"text"`
	Public  bool       `json:"public"`
	Created *time.Time `json:"created,omitempty"`
	Updated *time.Time `json:"updated,omitempty"`
}

func toNoteJSON(a model.Annotation, public bool) noteJSON {
	j := noteJSON{ID: a.ID, At: a.At.UTC(), Text: a.Text, Public: a.Public}
	if !a.End.IsZero() {
		e := a.End.UTC()
		j.End = &e
	}
	if !public {
		if !a.Created.IsZero() {
			c := a.Created.UTC()
			j.Created = &c
		}
		if !a.Updated.IsZero() {
			u := a.Updated.UTC()
			j.Updated = &u
		}
	}
	return j
}

// noteInput is the body of POST /api/annotations and PUT /api/annotations/{id}.
type noteInput struct {
	At     time.Time `json:"at"`
	End    time.Time `json:"end"`
	Text   string    `json:"text"`
	Public bool      `json:"public"`
}

// validate normalizes and checks a note. now bounds how far in the future
// a note may be.
func (in *noteInput) validate(now time.Time) error {
	in.Text = strings.TrimSpace(in.Text)
	if in.Text == "" {
		return badReq("text is required")
	}
	if !utf8.ValidString(in.Text) {
		return badReq("text is not valid UTF-8")
	}
	if n := utf8.RuneCountInString(in.Text); n > MaxAnnotationText {
		return badReq("text is too long (%d characters, at most %d)", n, MaxAnnotationText)
	}
	for _, r := range in.Text {
		if r != '\n' && r != '\t' && (unicode.IsControl(r) || r == ' ' || r == ' ') {
			return badReq("text contains control characters")
		}
	}
	if in.At.IsZero() || in.At.Year() < 2000 {
		return badReq("at is required")
	}
	if in.At.After(now.Add(24 * time.Hour)) {
		return badReq("at is too far in the future")
	}
	if !in.End.IsZero() {
		if !in.End.After(in.At) {
			return badReq("end must be after at")
		}
		if in.End.Sub(in.At) > MaxRange {
			return badReq("a note may span at most 90 days")
		}
		if in.End.After(now.Add(24 * time.Hour)) {
			return badReq("end is too far in the future")
		}
	}
	in.At = in.At.UTC()
	if !in.End.IsZero() {
		in.End = in.End.UTC()
	}
	return nil
}

// readJSON decodes a small JSON object body: Content-Type must be JSON,
// unknown fields and trailing data are refused. It writes the error.
func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "application/json" {
		writeErr(w, r, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
		return false
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeErr(w, r, http.StatusRequestEntityTooLarge, "body too large")
			return false
		}
		writeErr(w, r, http.StatusBadRequest, "bad JSON body: "+err.Error())
		return false
	}
	if _, err := dec.Token(); err != io.EOF {
		writeErr(w, r, http.StatusBadRequest, "bad JSON body: trailing data")
		return false
	}
	return true
}

// parseID parses a positive decimal ID.
func parseID(s string) (int64, bool) {
	if s == "" || len(s) > 19 || s[0] == '0' {
		return 0, false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	return n, err == nil && n > 0
}

// listNotes answers GET /api/annotations?from=&to= (public: public notes only).
func (s *server) listNotes(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if err := checkParams(q, "from", "to"); err != nil {
		writeErr(w, r, http.StatusBadRequest, err.Error())
		return
	}
	p, err := parseRange(q, s.now())
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, err.Error())
		return
	}
	render := func(ctx context.Context) (*cached, error) {
		list, err := s.d.Annotations.Annotations(ctx, p.from, p.to, s.public)
		if err != nil {
			return nil, err
		}
		out := make([]noteJSON, 0, min(len(list), MaxAnnotations))
		for _, a := range list {
			if s.public && !a.Public {
				continue // never trust the store's filter alone
			}
			if len(out) >= MaxAnnotations {
				break
			}
			out = append(out, toNoteJSON(a, s.public))
		}
		return jsonCached(out)
	}
	if s.public {
		s.runCached(w, r, "notes\x00"+p.key, render)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), notesTimeout)
	defer cancel()
	c, err := render(ctx)
	if err != nil {
		s.d.log().Error("annotations query", "err", err)
		writeErr(w, r, http.StatusInternalServerError, "query failed: "+err.Error())
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeBody(w, r, c.code, c.ctype, c.body)
}

// serveNotes answers /api/annotations and /api/annotations/{id} on the
// local listener: GET lists, POST adds, PUT replaces, DELETE removes.
// Writes need the share controls' protections (see controlAllowed).
func (h *localHandler) serveNotes(w http.ResponseWriter, r *http.Request, rest string) {
	if h.d.Annotations == nil {
		writeErr(w, r, http.StatusNotFound, "notes are not available")
		return
	}
	if rest == "" {
		switch r.Method {
		case http.MethodGet, http.MethodHead:
			h.listNotes(w, r)
		case http.MethodPost:
			h.writeNote(w, r, 0)
		default:
			w.Header().Set("Allow", "GET, HEAD, POST")
			writeErr(w, r, http.StatusMethodNotAllowed, "method not allowed")
		}
		return
	}
	id, ok := parseID(rest)
	if !ok {
		writeErr(w, r, http.StatusNotFound, "not found")
		return
	}
	switch r.Method {
	case http.MethodPut:
		h.writeNote(w, r, id)
	case http.MethodDelete:
		if len(r.URL.RawQuery) > 0 {
			writeErr(w, r, http.StatusBadRequest, "no parameters allowed")
			return
		}
		if !h.controlAllowed(w, r, "notes") {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), notesTimeout)
		defer cancel()
		if err := h.d.Annotations.DeleteAnnotation(ctx, id); err != nil {
			h.storeErr(w, r, "delete annotation", err)
			return
		}
		writeJSON(w, r, http.StatusOK, map[string]any{"deleted": id})
	default:
		w.Header().Set("Allow", "PUT, DELETE")
		writeErr(w, r, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// writeNote adds (id 0) or replaces a note.
func (h *localHandler) writeNote(w http.ResponseWriter, r *http.Request, id int64) {
	if len(r.URL.RawQuery) > 0 {
		writeErr(w, r, http.StatusBadRequest, "no parameters allowed")
		return
	}
	if !h.controlAllowed(w, r, "notes") {
		return
	}
	var in noteInput
	if !readJSON(w, r, &in) {
		return
	}
	if err := in.validate(h.now()); err != nil {
		writeErr(w, r, http.StatusBadRequest, err.Error())
		return
	}
	a := model.Annotation{ID: id, At: in.At, End: in.End, Text: in.Text, Public: in.Public}
	ctx, cancel := context.WithTimeout(r.Context(), notesTimeout)
	defer cancel()
	var err error
	code := http.StatusOK
	if id == 0 {
		err, code = h.d.Annotations.AddAnnotation(ctx, &a), http.StatusCreated
	} else {
		err = h.d.Annotations.UpdateAnnotation(ctx, &a)
	}
	if err != nil {
		h.storeErr(w, r, "save annotation", err)
		return
	}
	if a.Updated.IsZero() {
		a.Updated = h.now()
	}
	writeJSON(w, r, code, toNoteJSON(a, false))
}

// storeErr writes a store failure: 404 for a missing item, else 500.
func (h *localHandler) storeErr(w http.ResponseWriter, r *http.Request, what string, err error) {
	if isNotFound(err) {
		writeErr(w, r, http.StatusNotFound, "not found")
		return
	}
	h.d.log().Warn(what, "err", err)
	writeErr(w, r, http.StatusInternalServerError, what+" failed: "+err.Error())
}
