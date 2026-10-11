package web

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/speedtest"
)

// The speed-test queue's API (docs/SPEEDTEST-API.md, "Queue"): its state,
// one entry, the last sweep, starting entries and cancelling them
// (admin-gated).

// speedEntryJSON is one queue entry.
type speedEntryJSON struct {
	ID         uint64    `json:"id"`
	Kind       string    `json:"kind"`   // single | sweep | scheduled
	Who        string    `json:"who"`    // you | scheduled
	Origin     string    `json:"origin"` // owner | scheduled
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
	// StoppedReason: finished before its last test: "cancelled" or
	// "unreachable" (a sweep's servers stopped answering); else null.
	StoppedReason *string `json:"stopped_reason"`
	CancelledBy   *string `json:"cancelled_by"` // cancelled: "owner"; null when not, or unknown (shutdown)
	// Results: a sweep's servers in order, with each one's status (part=entry
	// and part=sweep only).
	Results []speedJobJSON `json:"results,omitempty"`
}

// speedJobJSON is one server of a sweep.
type speedJobJSON struct {
	Server   speedRef `json:"server"`
	Display  string   `json:"display"`  // "Region · Provider, City"
	Region   *string  `json:"region"`   // the test region the server belongs to
	Provider string   `json:"provider"` //
	Status   string   `json:"status"`   // waiting | testing | ok | busy | unreachable | failed | cancelled | skipped
	Test     *int64   `json:"test"`     // the stored test's id
	// DownBps and UpBps: the result (ok only); DownShort and UpShort: the
	// direction hit the byte cap (show "≥ X").
	DownBps        *float64 `json:"down_bps"`
	UpBps          *float64 `json:"up_bps"`
	DownShort      bool     `json:"down_short"`
	UpShort        bool     `json:"up_short"`
	CappedByBudget bool     `json:"capped_by_budget"`
}

// jobStatus is a sweep's job i before its stored test is looked up:
// "testing", "waiting", "cancelled" (the test a cancel stopped mid-run), or
// "skipped" (run without a stored result, or never reached by a finished
// sweep); a run job's stored outcome replaces "skipped".
func jobStatus(e speedtest.Entry, i int) string {
	switch {
	case i == e.Cut-1:
		return "cancelled" // stopped mid-test; the servers after it are never reached
	case i < e.Ran:
		return "skipped"
	case e.State == speedtest.StateRunning && i == e.Step-1:
		return "testing"
	case e.State == speedtest.StateWaiting || e.State == speedtest.StateRunning:
		return "waiting"
	}
	return "skipped" // finished early: never reached
}

// speedResults fills a sweep entry's results from the stored tests.
func (s *server) speedResults(ctx context.Context, j *speedEntryJSON, e speedtest.Entry, names speedNames, now time.Time) error {
	if e.Kind != speedtest.KindSweep {
		return nil
	}
	var ts []model.SpeedTest
	if e.Ran > 0 {
		to := now
		if !e.Finished.IsZero() {
			to = e.Finished
		}
		var err error
		if ts, err = s.d.Speed.SpeedTests(ctx, e.Started.Add(-time.Second), to.Add(time.Second)); err != nil {
			return err
		}
	}
	j.Results = make([]speedJobJSON, len(e.Servers))
	for i, id := range e.Servers {
		ref, display, provider, _ := names.server(id)
		r := speedJobJSON{Server: ref, Display: display, Provider: provider, Status: jobStatus(e, i)}
		if reg := names.inReg[id]; reg != "" {
			r.Region = &reg
		}
		k := slices.IndexFunc(ts, func(t model.SpeedTest) bool { return t.ID == e.JobTests[i] })
		if r.Status == "skipped" && i < e.Ran && e.JobTests[i] != 0 && k >= 0 {
			t := ts[k]
			r.Status, r.Test = t.Outcome, &t.ID
			if t.Outcome == model.SpeedOK {
				r.DownBps, r.UpBps, r.DownShort, r.UpShort, r.CappedByBudget = t.DownBps, t.UpBps, t.DownShort, t.UpShort, t.DownShort || t.UpShort
			}
		}
		j.Results[i] = r
	}
	return nil
}

// speedQueueJSON is api/speed?part=queue.
type speedQueueJSON struct {
	At        int64            `json:"at"`
	Mine      *uint64          `json:"mine"` // the owner's latest entry
	Running   *speedEntryJSON  `json:"running"`
	Waiting   []speedEntryJSON `json:"waiting"`
	LastSweep *speedEntryJSON  `json:"last_sweep"`
}

// who is an entry's "who": the schedule's or the owner's.
func who(e speedtest.Entry) string {
	if e.Who == speedtest.WhoSchedule {
		return "scheduled"
	}
	return "you"
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

func (s *server) speedEntry(e speedtest.Entry, st speedtest.State, names speedNames) speedEntryJSON {
	j := speedEntryJSON{ID: e.ID, Kind: string(e.Kind), Who: who(e), State: e.State, Target: names.target(e.Target, st.Regions),
		Position: e.Position, EnqueuedAt: e.Enqueued.UnixMilli(), StartedAt: msPtr(e.Started), FinishedAt: msPtr(e.Finished),
		EstStart: e.EstStart.UnixMilli(), EstFinish: e.EstFinish.UnixMilli(), Step: e.Step, Steps: e.Steps, Tests: e.Tests}
	if j.Tests == nil {
		j.Tests = []int64{}
	}
	j.Origin = "owner"
	if e.Who == speedtest.WhoSchedule {
		j.Origin = "scheduled"
	}
	j.StoppedReason = strPtr(e.Stopped)
	j.CancelledBy = strPtr(e.CancelledBy)
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
// part=sweep. In memory: never cached.
func (s *server) serveSpeedQueue(w http.ResponseWriter, r *http.Request, p speedParams, now time.Time) {
	w.Header().Set("Cache-Control", "no-store")
	q := s.d.Speed.Queue()
	st := s.d.Speed.State()
	names := newSpeedNames(st, s.d.Speed.Catalog())
	switch p.part {
	case "entry":
		e, ok := q.Get(p.entry)
		if !ok {
			writeErr(w, r, http.StatusNotFound, "no such queue entry")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), notesTimeout)
		defer cancel()
		j := s.speedEntry(e, st, names)
		if err := s.speedResults(ctx, &j, e, names, now); err != nil {
			writeErr(w, r, http.StatusServiceUnavailable, "speed tests unavailable")
			return
		}
		writeJSON(w, r, http.StatusOK, j)
	case "sweep":
		qs := q.State()
		ctx, cancel := context.WithTimeout(r.Context(), notesTimeout)
		defer cancel()
		// running: a sweep under way (its results so far); entry and tests:
		// the last finished one.
		out := map[string]any{"at": now.UnixMilli(), "running": nil, "entry": nil, "tests": []speedTestJSON{}}
		if e := qs.Running; e != nil && e.Kind == speedtest.KindSweep {
			j := s.speedEntry(*e, st, names)
			if err := s.speedResults(ctx, &j, *e, names, now); err != nil {
				writeErr(w, r, http.StatusServiceUnavailable, "speed tests unavailable")
				return
			}
			out["running"] = j
		}
		if e := qs.LastSweep; e != nil {
			j := s.speedEntry(*e, st, names)
			if err := s.speedResults(ctx, &j, *e, names, now); err != nil {
				writeErr(w, r, http.StatusServiceUnavailable, "speed tests unavailable")
				return
			}
			out["entry"] = j
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
		out := speedQueueJSON{At: now.UnixMilli(), Waiting: []speedEntryJSON{}}
		var mine *uint64
		note := func(e speedtest.Entry) {
			if e.Who == speedtest.WhoOwner && (mine == nil || e.ID > *mine) {
				id := e.ID
				mine = &id
			}
		}
		if qs.Running != nil {
			j := s.speedEntry(*qs.Running, st, names)
			out.Running = &j
			note(*qs.Running)
		}
		for _, e := range qs.Waiting {
			out.Waiting = append(out.Waiting, s.speedEntry(e, st, names))
			note(e)
		}
		if qs.LastSweep != nil {
			j := s.speedEntry(*qs.LastSweep, st, names)
			out.LastSweep = &j
		}
		out.Mine = mine
		writeJSON(w, r, http.StatusOK, out)
	}
}

// speedRunRequest reads POST api/speed/run's kind and id: kind single
// (default; id a server or region, none for the nearest) or sweep (no id).
func speedRunRequest(r *http.Request) (speedtest.Kind, string, error) {
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
	return kind, id, nil
}

// enqueueSpeed adds the request and writes the answer: 202 {"entry",
// "duplicate"}, 404 not_found, 409 skipped.
func (s *server) enqueueSpeed(w http.ResponseWriter, r *http.Request, req speedtest.Request) {
	st := s.d.Speed.State()
	names := newSpeedNames(st, s.d.Speed.Catalog())
	e, dup, err := s.d.Speed.Queue().Enqueue(req)
	switch {
	case errors.Is(err, speedtest.ErrUnknownID):
		writeErr(w, r, http.StatusNotFound, "no such server or region")
		return
	case errors.Is(err, speedtest.ErrSkipped):
		writeErrCode(w, r, http.StatusConflict, "skipped", err.Error())
		return
	case err != nil:
		writeErr(w, r, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, r, http.StatusAccepted, map[string]any{"entry": s.speedEntry(e, st, names), "duplicate": dup})
}

// serveSpeedRun answers POST api/speed/run?kind=single[&id=]|sweep on the
// local listener: the same protections as notes.
func (h *localHandler) serveSpeedRun(w http.ResponseWriter, r *http.Request) {
	if !h.speedPOST(w, r, "kind", "id") {
		return
	}
	kind, id, err := speedRunRequest(r)
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, err.Error())
		return
	}
	h.enqueueSpeed(w, r, speedtest.Request{Kind: kind, Target: id, Who: speedtest.WhoOwner})
}

// serveSpeedCancel answers POST api/speed/cancel?id=<entry> (waiting or
// running): admin-gated like notes.
func (h *localHandler) serveSpeedCancel(w http.ResponseWriter, r *http.Request) {
	if !h.speedPOST(w, r, "id") {
		return
	}
	q := r.URL.Query()
	switch {
	case q.Has("id"):
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
		writeJSON(w, r, http.StatusOK, map[string]any{"entry": h.speedEntry(e, st, newSpeedNames(st, h.d.Speed.Catalog()))})
	default:
		writeErr(w, r, http.StatusBadRequest, "want id=<entry>")
	}
}
