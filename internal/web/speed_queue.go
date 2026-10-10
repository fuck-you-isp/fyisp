package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/speedtest"
)

// The speed-test queue's API (docs/SPEEDTEST-API.md, "Queue"): its state,
// one entry, the last sweep, starting entries (local and share link) and
// cancelling them (local, admin-gated).

// speedEntryJSON is one queue entry. "who" is relative to the viewer and
// never carries an address.
type speedEntryJSON struct {
	ID         uint64    `json:"id"`
	Kind       string    `json:"kind"`             // single | sweep | scheduled
	Who        string    `json:"who"`              // you | someone | scheduled
	Origin     string    `json:"origin,omitempty"` // local only: owner | public | scheduled
	State      string    `json:"state"`
	Target     *speedRef `json:"target"`   // the server or region; null: the nearest, a sweep or a slot
	Position   int       `json:"position"` // waiting: 1 is next; else 0
	EnqueuedAt int64     `json:"enqueued_at"`
	StartedAt  *int64    `json:"started_at"`
	FinishedAt *int64    `json:"finished_at"`
	EstStart   int64     `json:"est_start"`
	EstFinish  int64     `json:"est_finish"`
	Step       int       `json:"step"`
	Steps      int       `json:"steps"`
	Server     *speedRef `json:"server"`    // running: the server under test
	Direction  *string   `json:"direction"` // running: picking, download, upload
	Tests      []int64   `json:"tests"`
}

// speedQueueJSON is api/speed?part=queue.
type speedQueueJSON struct {
	At         int64            `json:"at"`
	MaxWaiting int              `json:"max_waiting"`
	Full       bool             `json:"full"`
	SweepsFull bool             `json:"sweeps_full"`
	Mine       *uint64          `json:"mine"`
	Running    *speedEntryJSON  `json:"running"`
	Waiting    []speedEntryJSON `json:"waiting"`
	LastSweep  *speedEntryJSON  `json:"last_sweep"`
	// PublicEnabled: the owner lets the share link queue tests (the
	// Details toggle); PublicStart: the share link may start one now (on,
	// budget left, room in the queue).
	PublicEnabled bool            `json:"public_enabled"`
	PublicStart   bool            `json:"public_start"`
	Budget        speedBudgetJSON `json:"budget"`
}

// speedBudgetJSON is the share link's rolling 24-hour data budget.
type speedBudgetJSON struct {
	LimitBytes     int64  `json:"limit_bytes"`
	UsedBytes      int64  `json:"used_bytes"`
	RemainingBytes int64  `json:"remaining_bytes"`
	ResetsAt       *int64 `json:"resets_at"` // when spent: when enough use ages out
}

// speedViewer is who looks at the queue: the owner (local) or a share-link
// visitor (key).
type speedViewer struct {
	public bool
	key    string
}

func (s *server) speedViewer(r *http.Request) speedViewer {
	if !s.public {
		return speedViewer{}
	}
	return speedViewer{public: true, key: visitorKey(r)}
}

// visitorKey is the queue's per-visitor key: a hash of the client's
// address group (limitKey of Cf-Connecting-Ip), so entries never hold an
// address.
func visitorKey(r *http.Request) string {
	sum := sha256.Sum256([]byte("fyisp-speed-visitor\x00" + limitKey(clientKey(r))))
	return hex.EncodeToString(sum[:12])
}

func (v speedViewer) mine(e speedtest.Entry) bool {
	if v.public {
		return e.Who == speedtest.WhoPublic && e.Visitor == v.key
	}
	return e.Who == speedtest.WhoOwner
}

func (v speedViewer) who(e speedtest.Entry) string {
	switch {
	case e.Who == speedtest.WhoSchedule:
		return "scheduled"
	case v.mine(e):
		return "you"
	}
	return "someone"
}

func msPtr(t time.Time) *int64 {
	if t.IsZero() {
		return nil
	}
	ms := t.UnixMilli()
	return &ms
}

// target is a server's or region's reference for this viewer.
func (n speedNames) target(id string, regions []speedtest.Region) *speedRef {
	if id == "" {
		return nil
	}
	if i := slices.IndexFunc(regions, func(r speedtest.Region) bool { return r.ID == id }); i >= 0 {
		return &speedRef{ID: id, Label: regions[i].Title}
	}
	ref := n.ref(id)
	return &ref
}

func (s *server) speedEntry(e speedtest.Entry, v speedViewer, st speedtest.State, names speedNames) speedEntryJSON {
	j := speedEntryJSON{ID: e.ID, Kind: string(e.Kind), Who: v.who(e), State: e.State, Target: names.target(e.Target, st.Regions),
		Position: e.Position, EnqueuedAt: e.Enqueued.UnixMilli(), StartedAt: msPtr(e.Started), FinishedAt: msPtr(e.Finished),
		EstStart: e.EstStart.UnixMilli(), EstFinish: e.EstFinish.UnixMilli(), Step: e.Step, Steps: e.Steps, Tests: e.Tests}
	if j.Tests == nil {
		j.Tests = []int64{}
	}
	if !s.public {
		j.Origin = map[speedtest.Who]string{speedtest.WhoOwner: "owner", speedtest.WhoPublic: "public", speedtest.WhoSchedule: "scheduled"}[e.Who]
	}
	if e.State == speedtest.StateRunning {
		if e.Server != "" {
			ref := names.ref(e.Server)
			j.Server = &ref
		}
		j.Direction = strPtr(e.Direction)
	}
	if e.State != speedtest.StateWaiting && e.State != speedtest.StateRunning {
		j.EstStart, j.EstFinish = j.EnqueuedAt, j.EnqueuedAt
		if j.StartedAt != nil {
			j.EstStart = *j.StartedAt
		}
		if j.FinishedAt != nil {
			j.EstFinish = *j.FinishedAt
		}
	}
	return j
}

// serveSpeedQueue answers api/speed?part=queue, part=entry&id= and
// part=sweep. In memory and per viewer ("you"): never cached.
func (s *server) serveSpeedQueue(w http.ResponseWriter, r *http.Request, p speedParams, now time.Time) {
	w.Header().Set("Cache-Control", "no-store")
	q := s.d.Speed.Queue()
	st := s.d.Speed.State()
	names := newSpeedNames(st, s.public)
	v := s.speedViewer(r)
	switch p.part {
	case "entry":
		e, ok := q.Get(p.entry)
		if !ok {
			writeErr(w, r, http.StatusNotFound, "no such queue entry")
			return
		}
		writeJSON(w, r, http.StatusOK, s.speedEntry(e, v, st, names))
	case "sweep":
		qs := q.State()
		out := map[string]any{"at": now.UnixMilli(), "entry": nil, "tests": []speedTestJSON{}}
		if e := qs.LastSweep; e != nil {
			out["entry"] = s.speedEntry(*e, v, st, names)
			ctx, cancel := context.WithTimeout(r.Context(), notesTimeout)
			defer cancel()
			ts, err := s.d.Speed.SpeedTests(ctx, e.Started.Add(-time.Second), e.Finished.Add(time.Second))
			if err != nil {
				writeErr(w, r, http.StatusServiceUnavailable, "speed tests unavailable")
				return
			}
			var tests []speedTestJSON
			for _, t := range ts {
				if slices.Contains(e.Tests, t.ID) {
					tests = append(tests, names.test(t))
				}
			}
			if tests != nil {
				out["tests"] = tests
			}
		}
		writeJSON(w, r, http.StatusOK, out)
	default:
		qs := q.State()
		out := speedQueueJSON{At: now.UnixMilli(), MaxWaiting: speedtest.MaxPublicWaiting, Full: qs.Full, SweepsFull: qs.SweepsFull,
			Waiting: []speedEntryJSON{}}
		var mine *uint64
		note := func(e speedtest.Entry) {
			if v.mine(e) && (mine == nil || e.ID > *mine) {
				id := e.ID
				mine = &id
			}
		}
		if qs.Running != nil {
			j := s.speedEntry(*qs.Running, v, st, names)
			out.Running = &j
			note(*qs.Running)
		}
		for _, e := range qs.Waiting {
			out.Waiting = append(out.Waiting, s.speedEntry(e, v, st, names))
			note(e)
		}
		if qs.LastSweep != nil {
			j := s.speedEntry(*qs.LastSweep, v, st, names)
			out.LastSweep = &j
		}
		out.Mine = mine
		out.PublicEnabled = qs.Public
		out.Budget = speedBudgetJSON{LimitBytes: qs.BudgetLimit, UsedBytes: qs.BudgetUsed,
			RemainingBytes: max(qs.BudgetLimit-qs.BudgetUsed, 0), ResetsAt: msPtr(qs.BudgetResets)}
		out.PublicStart = qs.Public && qs.BudgetResets.IsZero() && !qs.Full
		writeJSON(w, r, http.StatusOK, out)
	}
}

// speedRunRequest reads POST api/speed/run's kind and id: kind single
// (default; id a server or region, none for the nearest) or sweep (no id).
func speedRunRequest(r *http.Request, names speedNames) (speedtest.Kind, string, error) {
	q := r.URL.Query()
	if len(q["kind"]) > 1 || len(q["id"]) > 1 {
		return "", "", badReq("one kind and at most one id")
	}
	kind := speedtest.Kind(q.Get("kind"))
	switch kind {
	case "", speedtest.KindSingle:
		kind = speedtest.KindSingle
	case speedtest.KindSweep:
		if q.Has("id") {
			return "", "", badReq("a sweep takes no id")
		}
	default:
		return "", "", badReq("bad kind %q: want single or sweep", kind)
	}
	id := q.Get("id")
	if len(id) > 64 {
		return "", "", badReq("id too long")
	}
	if id != "" {
		local := id
		if id = names.resolve(id); names.public && id == local && names.custom[id] > 0 {
			return "", "", errNoSpeedID // the user's own servers only by public ID there
		}
	}
	return kind, id, nil
}

// enqueueSpeed adds the request and writes the answer: 202 {"entry",
// "duplicate"}, 404 not_found, 429 queue_full. started (share link only)
// is called when a new entry was added, not for a duplicate.
func (s *server) enqueueSpeed(w http.ResponseWriter, r *http.Request, req speedtest.Request, started func()) {
	st := s.d.Speed.State()
	names := newSpeedNames(st, s.public)
	e, dup, err := s.d.Speed.Queue().Enqueue(req)
	if queueRefused(w, r, err, s.now()) {
		return
	}
	switch {
	case errors.Is(err, speedtest.ErrUnknownID):
		writeErr(w, r, http.StatusNotFound, "no such server or region")
		return
	case err != nil:
		writeErr(w, r, http.StatusBadRequest, err.Error())
		return
	}
	if !dup && started != nil {
		started()
	}
	writeJSON(w, r, http.StatusAccepted, map[string]any{"entry": s.speedEntry(e, s.speedViewer(r), st, names), "duplicate": dup})
}

// serveSpeedRun answers POST api/speed/run?kind=single[&id=]|sweep on the
// local listener: the same protections as notes. The owner's entries go
// ahead of the share link's.
func (h *localHandler) serveSpeedRun(w http.ResponseWriter, r *http.Request) {
	if !h.speedPOST(w, r, "kind", "id") {
		return
	}
	kind, id, err := speedRunRequest(r, newSpeedNames(h.d.Speed.State(), false))
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, err.Error())
		return
	}
	h.enqueueSpeed(w, r, speedtest.Request{Kind: kind, Target: id, Who: speedtest.WhoOwner}, nil)
}

// serveSpeedCancel answers POST api/speed/cancel?id=<entry> (waiting or
// running) or ?public=1 (every waiting share-link entry): admin-gated like
// notes.
func (h *localHandler) serveSpeedCancel(w http.ResponseWriter, r *http.Request) {
	if !h.speedPOST(w, r, "id", "public") {
		return
	}
	q := r.URL.Query()
	switch {
	case q.Get("public") == "1" && !q.Has("id"):
		writeJSON(w, r, http.StatusOK, map[string]any{"cancelled": h.d.Speed.Queue().ClearPublic()})
	case q.Has("id") && !q.Has("public"):
		id, err := strconv.ParseUint(q.Get("id"), 10, 64)
		if err != nil {
			writeErr(w, r, http.StatusBadRequest, "bad id")
			return
		}
		e, err := h.d.Speed.Queue().Cancel(id)
		if err != nil {
			writeErr(w, r, http.StatusNotFound, "no such queue entry")
			return
		}
		st := h.d.Speed.State()
		writeJSON(w, r, http.StatusOK, map[string]any{"entry": h.speedEntry(e, speedViewer{}, st, newSpeedNames(st, false))})
	default:
		writeErr(w, r, http.StatusBadRequest, "want id=<entry> or public=1")
	}
}

// servePublicSpeedRun answers POST /s/<secret>/api/speed/run: anyone with
// the link may queue a test or a sweep. As POST api/investigate: no body,
// only kind and id, same origin, never cached, per-client start limits
// (Cf-Connecting-Ip, IPv6 per /64); and the queue's own caps (one waiting
// entry per visitor, MaxPublicSweeps sweeps, MaxPublicWaiting in all).
func (h *publicHandler) servePublicSpeedRun(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if h.d.Speed == nil {
		notFound(w, r)
		return
	}
	if !strictPOST(w, r, "kind", "id") {
		return
	}
	if !sameOrigin(r) {
		writeInvErr(w, r, http.StatusForbidden, "forbidden", "cross-origin request refused", 0)
		return
	}
	kind, id, err := speedRunRequest(r, newSpeedNames(h.d.Speed.State(), true))
	if errors.Is(err, errNoSpeedID) {
		writeErr(w, r, http.StatusNotFound, "no such server or region")
		return
	}
	if err != nil {
		writeInvErr(w, r, http.StatusBadRequest, "bad_request", err.Error(), 0)
		return
	}
	key := limitKey(clientKey(r)) // the start limiter's (in memory only)
	req := speedtest.Request{Kind: kind, Target: id, Who: speedtest.WhoPublic, Visitor: visitorKey(r)}
	// The queue's answer first: a duplicate or a refusal is not a start.
	dup, err := h.d.Speed.Queue().Admit(req)
	if queueRefused(w, r, err, h.now()) {
		return
	}
	if ok, wait := h.speedStarts.check(key, h.now()); !ok && !dup {
		writeInvErr(w, r, http.StatusTooManyRequests, "rate_limited", "too many tests started from here; try again later", wait)
		return
	}
	h.enqueueSpeed(w, r, req, func() { h.speedStarts.record(key, h.now()) })
}

// queueRefused writes the queue's refusals of a share-link request: 403
// disabled (the owner hasn't opted in), 429 budget_exhausted (with
// resets_at), 429 queue_full. It reports whether it wrote one.
func queueRefused(w http.ResponseWriter, r *http.Request, err error, now time.Time) bool {
	var full *speedtest.QueueFullError
	var budget *speedtest.BudgetError
	switch {
	case errors.Is(err, speedtest.ErrPublicDisabled):
		writeInvErr(w, r, http.StatusForbidden, "disabled", "the owner hasn't turned on tests from the share link", 0)
	case errors.As(err, &budget):
		secs := int((budget.ResetsAt.Sub(now) + time.Second - 1) / time.Second)
		w.Header().Set("Retry-After", strconv.Itoa(max(secs, 1)))
		writeJSON(w, r, http.StatusTooManyRequests, map[string]any{"error": budget.Error(), "code": "budget_exhausted",
			"resets_at": budget.ResetsAt.UnixMilli(), "retry_after": max(secs, 1)})
	case errors.As(err, &full):
		writeInvErr(w, r, http.StatusTooManyRequests, "queue_full", full.Reason, 0)
	default:
		return false
	}
	return true
}

// strictPOST checks a public write's request: POST, no body, only these
// parameters (each at most once). It writes the refusal.
func strictPOST(w http.ResponseWriter, r *http.Request, params ...string) bool {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeInvErr(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed", 0)
		return false
	}
	if r.ContentLength != 0 || len(r.TransferEncoding) > 0 {
		writeInvErr(w, r, http.StatusBadRequest, "bad_request", "this request takes no body", 0)
		return false
	}
	if err := checkParams(r.URL.Query(), params...); err != nil {
		writeInvErr(w, r, http.StatusBadRequest, "bad_request", err.Error(), 0)
		return false
	}
	return true
}
