package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/fuck-you-isp/fyisp/internal/s3test"
	"github.com/fuck-you-isp/fyisp/internal/storage"
)

// The storage tests' API (docs/STORAGE-API.md; keep the JSON types below
// in sync with it). Keys are write-only: no response, log line or error
// carries them.

// Body limits.
const (
	storageBody       = 16 << 10
	storageTargetBody = 96 << 10 // a CA bundle
)

type storageTargetJSON struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	Endpoint   string            `json:"endpoint"`
	Scheme     string            `json:"scheme"`
	Host       string            `json:"host"`
	Port       int               `json:"port"`
	Region     string            `json:"region"`
	Bucket     string            `json:"bucket"`
	Addressing string            `json:"addressing"`
	HasCA      bool              `json:"has_ca"`
	Network    string            `json:"network"`
	LastCheck  *storageCheckJSON `json:"last_check"`
	CreatedMs  int64             `json:"created"`
	UpdatedMs  int64             `json:"updated"`
}

type storageCheckJSON struct {
	At         int64    `json:"at"`
	Class      string   `json:"class"`
	Group      string   `json:"group"`
	Keys       string   `json:"keys"` // "work" | "refused" | "none"
	Hint       *string  `json:"hint"`
	HTTPStatus *int     `json:"http_status"`
	S3Code     *string  `json:"s3_code"`
	DNSms      *float64 `json:"dns_ms"`
	Connectms  *float64 `json:"connect_ms"`
	TLSms      *float64 `json:"tls_ms"`
	TTFBms     *float64 `json:"ttfb_ms"`
	DuringTest bool     `json:"during_test"`
}

func strOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func storageTarget(ts storage.TargetState) storageTargetJSON {
	t := ts.Target
	port, _ := strconv.Atoi(t.Port())
	out := storageTargetJSON{ID: t.ID, Name: t.Name, Endpoint: t.Endpoint, Scheme: t.Scheme(), Host: t.Host(), Port: port,
		Region: t.Region, Bucket: t.Bucket, Addressing: t.Addressing, HasCA: t.CAPEM != "", Network: t.Network,
		CreatedMs: t.CreatedMs, UpdatedMs: t.UpdatedMs}
	if c := ts.Check; c != nil {
		out.LastCheck = storageCheck(*c)
	}
	return out
}

func storageCheck(c storage.Check) *storageCheckJSON {
	out := &storageCheckJSON{At: c.At.UnixMilli(), Class: c.Class, Group: s3test.Class(c.Class).Group(), Keys: c.Keys, Hint: strOrNil(c.Hint),
		S3Code: strOrNil(c.S3Code), DNSms: c.DNSms, Connectms: c.Connectms, TLSms: c.TLSms, TTFBms: c.TTFBms, DuringTest: c.DuringTest}
	if c.HTTPStatus != 0 {
		out.HTTPStatus = &c.HTTPStatus
	}
	return out
}

// serveStorage answers GET api/storage: the targets.
func (s *server) serveStorage(w http.ResponseWriter, r *http.Request) {
	if s.d.Storage == nil {
		notFound(w, r)
		return
	}
	if err := checkParams(r.URL.Query()); err != nil {
		writeErr(w, r, http.StatusBadRequest, err.Error())
		return
	}
	rev := s.d.Storage.Rev()
	ts, err := s.d.Storage.Targets()
	if err != nil {
		storageErr(w, r, err)
		return
	}
	out := make([]storageTargetJSON, len(ts))
	for i, t := range ts {
		out[i] = storageTarget(t)
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"at": s.now().UnixMilli(), "rev": rev, "targets": out})
}

// storageErr answers a storage error with its status and code.
func storageErr(w http.ResponseWriter, r *http.Request, err error) {
	var fe *storage.FieldError
	switch {
	case errors.As(err, &fe):
		writeJSON(w, r, http.StatusBadRequest, map[string]string{"error": fe.Error(), "code": fe.Code, "field": fe.Field})
	case errors.Is(err, storage.ErrNotFound):
		writeErrCode(w, r, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, storage.ErrNameTaken):
		writeErrCode(w, r, http.StatusConflict, "name_taken", err.Error())
	case errors.Is(err, storage.ErrTooMany):
		writeErrCode(w, r, http.StatusConflict, "too_many", err.Error())
	case errors.Is(err, storage.ErrChanged):
		writeErrCode(w, r, http.StatusConflict, "changed", err.Error())
	case errors.Is(err, storage.ErrBusy):
		writeErrCode(w, r, http.StatusTooManyRequests, "busy", err.Error())
	case errors.Is(err, storage.ErrFile):
		writeErrCode(w, r, http.StatusServiceUnavailable, "storage_file",
			err.Error()+"; fix or move it and restart fyisp (it is never overwritten)")
	default:
		writeErrCode(w, r, http.StatusInternalServerError, "error", "the storage request failed: "+err.Error())
	}
}

// serveStorageWrite answers the POST routes under api/storage/.
func (h *localHandler) serveStorageWrite(w http.ResponseWriter, r *http.Request) {
	route := strings.TrimPrefix(r.URL.Path, "/api/storage/")
	switch route {
	case "target/add", "target/update", "target/delete", "check":
	default:
		notFound(w, r)
		return
	}
	params := []string{"id"}
	if route == "target/add" {
		params = nil
	}
	if !h.storagePOST(w, r, params...) {
		return
	}
	id := r.URL.Query().Get("id")
	if params != nil && id == "" {
		writeErr(w, r, http.StatusBadRequest, "id is required")
		return
	}
	st := h.d.Storage
	ctx := r.Context()
	switch route {
	case "target/add":
		var body storageSpecJSON
		if !decodeStorage(w, r, storageTargetBody, &body, true) {
			return
		}
		if body.Name == nil || body.Endpoint == nil {
			field := "endpoint"
			if body.Name == nil {
				field = "name"
			}
			writeJSON(w, r, http.StatusBadRequest, map[string]string{"error": "name and endpoint are required", "code": "bad_request", "field": field})
			return
		}
		sp := storage.Spec{Name: *body.Name, Endpoint: *body.Endpoint, Region: deref(body.Region), Bucket: deref(body.Bucket),
			Addressing: deref(body.Addressing), CAPEM: deref(body.CAPEM)}
		ts, err := st.Add(ctx, sp)
		if err != nil {
			storageErr(w, r, err)
			return
		}
		writeJSON(w, r, http.StatusCreated, map[string]any{"target": storageTarget(ts)})
	case "target/update":
		var body storageSpecJSON
		if !decodeStorage(w, r, storageTargetBody, &body, true) {
			return
		}
		ts, err := st.Update(ctx, id, storage.Patch(body))
		if err != nil {
			storageErr(w, r, err)
			return
		}
		writeJSON(w, r, http.StatusOK, map[string]any{"target": storageTarget(ts)})
	case "target/delete":
		if !decodeStorage(w, r, storageBody, nil, false) {
			return
		}
		if err := st.Delete(id); err != nil {
			storageErr(w, r, err)
			return
		}
		writeJSON(w, r, http.StatusOK, map[string]any{})
	case "check":
		var body struct {
			Keys *storageKeysIn `json:"keys"`
		}
		if !decodeStorage(w, r, storageBody, &body, false) {
			return
		}
		var creds *s3test.Creds
		if body.Keys != nil {
			var err error
			if creds, err = body.Keys.creds(); err != nil {
				writeJSON(w, r, http.StatusBadRequest, map[string]string{"error": err.Error(), "code": "bad_request", "field": "keys"})
				return
			}
			defer creds.Zero()
		}
		c, err := st.Check(ctx, id, creds) // bounded by storage.CheckTimeout
		if ctx.Err() != nil {
			return // the client is gone
		}
		if err != nil {
			storageErr(w, r, err)
			return
		}
		writeJSON(w, r, http.StatusOK, map[string]any{"result": storageCheck(c)})
	}
}

// storageSpecJSON is a target's settings in a request (nil: not given).
type storageSpecJSON struct {
	Name       *string `json:"name"`
	Endpoint   *string `json:"endpoint"`
	Region     *string `json:"region"`
	Bucket     *string `json:"bucket"`
	Addressing *string `json:"addressing"`
	CAPEM      *string `json:"ca_pem"`
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// storageKeysIn is keys in a request.
type storageKeysIn struct {
	AccessKey    string `json:"access_key"`
	Secret       string `json:"secret"`
	SessionToken string `json:"session_token"`
}

// printable reports 1 to max printable ASCII characters (with spaces
// when space).
func printable(v string, max int, space bool) bool {
	if v == "" || len(v) > max {
		return false
	}
	for i := 0; i < len(v); i++ {
		if c := v[i]; c > 0x7e || c < 0x20 || (c == ' ' && !space) {
			return false
		}
	}
	return true
}

// creds checks the keys (never echoing them).
func (k *storageKeysIn) creds() (*s3test.Creds, error) {
	switch {
	case !printable(k.AccessKey, 128, false):
		return nil, errors.New("the access key is 1 to 128 printable characters, no spaces")
	case !printable(k.Secret, 256, true):
		return nil, errors.New("the secret is 1 to 256 printable characters")
	case k.SessionToken != "" && !printable(k.SessionToken, 4096, false):
		return nil, errors.New("the session token is up to 4096 printable characters, no spaces")
	}
	return s3test.NewCreds(k.AccessKey, k.Secret, k.SessionToken), nil
}

// storagePOST checks a storage write: a storage source, POST, only the
// allowed parameters, and the write checks (writeAllowed).
func (h *localHandler) storagePOST(w http.ResponseWriter, r *http.Request, params ...string) bool {
	if h.d.Storage == nil {
		notFound(w, r)
		return false
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeErr(w, r, http.StatusMethodNotAllowed, "method not allowed")
		return false
	}
	if err := checkParams(r.URL.Query(), params...); err != nil {
		writeErr(w, r, http.StatusBadRequest, err.Error())
		return false
	}
	return h.writeAllowed(w, r)
}

var fieldNameRe = regexp.MustCompile(`^"[a-z_]{1,32}"$`)

// decodeStorage reads a JSON body of at most limit bytes into v, with no
// unknown fields; a body must be application/json. need: a body is
// required; v nil: none is allowed. The error never quotes the body (it
// may hold keys).
func decodeStorage(w http.ResponseWriter, r *http.Request, limit int64, v any, need bool) bool {
	b, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	switch {
	case err != nil:
		writeErr(w, r, http.StatusBadRequest, "reading the body failed")
		return false
	case int64(len(b)) > limit:
		writeErrCode(w, r, http.StatusRequestEntityTooLarge, "too_large", "the body is too large")
		return false
	case len(strings.TrimSpace(string(b))) == 0:
		if need {
			writeErr(w, r, http.StatusBadRequest, "a JSON body is required")
			return false
		}
		return true
	case v == nil:
		writeErr(w, r, http.StatusBadRequest, "this route takes no body")
		return false
	}
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		writeErrCode(w, r, http.StatusUnsupportedMediaType, "unsupported_media_type", "the body must be application/json")
		return false
	}
	d := json.NewDecoder(strings.NewReader(string(b)))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil || d.More() {
		var ute *json.UnmarshalTypeError
		msg := "the body is not the expected JSON object"
		if errors.As(err, &ute) && ute.Field != "" {
			msg = "wrong type for " + ute.Field
		} else if f, ok := strings.CutPrefix(fmt.Sprint(err), "json: unknown field "); ok && fieldNameRe.MatchString(f) {
			msg = "unknown field " + f // not a key pasted as a name
		}
		writeErr(w, r, http.StatusBadRequest, msg)
		return false
	}
	return true
}
