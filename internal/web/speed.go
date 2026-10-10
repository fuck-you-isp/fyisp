package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/speedtest"
)

// SpeedSource runs and keeps the speed tests (speedtest.Scheduler). See
// docs/SPEEDTEST-API.md; keep the JSON types below in sync with it.
type SpeedSource interface {
	State() speedtest.State
	SpeedTests(ctx context.Context, from, to time.Time) ([]model.SpeedTest, error)
	TestNow() (time.Time, error)
	SetEnabled(ctx context.Context, on bool) (time.Time, error)
}

// SpeedTTL is how long the share link caches api/speed.
const SpeedTTL = 30 * time.Second

// speedJSON is api/speed?part=summary.
type speedJSON struct {
	At           int64                   `json:"at"`
	TZ           int                     `json:"tz"`
	Days         int                     `json:"days"`
	Enabled      bool                    `json:"enabled"`
	Configurable bool                    `json:"configurable"`
	IntervalS    int64                   `json:"interval_s"`
	NextAt       *int64                  `json:"next_at"`
	Running      *speedRunning           `json:"running"`
	Custom       string                  `json:"custom_server,omitempty"`
	Servers      []speedtest.ServerState `json:"servers,omitempty"`
	Latest       *speedTestJSON          `json:"latest"`
	Tests        []speedTestJSON         `json:"tests"`
	ByHour       speedByHour             `json:"by_hour"`
	Pattern      *speedtest.Pattern      `json:"pattern"`
	Windows      []speedWindow           `json:"windows"`
}

type speedRunning struct {
	Started   int64  `json:"started"`
	Direction string `json:"direction"`
	Server    string `json:"server,omitempty"`
}

type speedByHour struct {
	Hours    []speedtest.HourStat `json:"hours"`
	DipHours []int                `json:"dip_hours"`
}

type speedWindow struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"`
}

// speedTestJSON is one test; server, city and port are left out on the
// share link (the nearest servers tell roughly where the user is).
type speedTestJSON struct {
	ID              int64    `json:"id"`
	Start           int64    `json:"start"`
	End             int64    `json:"end"`
	Trigger         string   `json:"trigger"`
	Outcome         string   `json:"outcome"`
	Server          string   `json:"server,omitempty"`
	City            string   `json:"city,omitempty"`
	Port            int      `json:"port,omitempty"`
	Attempts        int      `json:"attempts"`
	DownBps         *float64 `json:"down_bps"`
	UpBps           *float64 `json:"up_bps"`
	DownRetrans     *int64   `json:"down_retrans"`
	UpRetrans       *int64   `json:"up_retrans"`
	IdleGatewayMs   *float64 `json:"idle_gateway_ms"`
	LoadedGatewayMs *float64 `json:"loaded_gateway_ms"`
	IdleCallMs      *float64 `json:"idle_call_ms"`
	LoadedCallMs    *float64 `json:"loaded_call_ms"`
}

func speedTestOf(t model.SpeedTest, cities map[string]string, public bool) speedTestJSON {
	r := func(p *float64, digits float64) *float64 {
		if p == nil {
			return nil
		}
		v := math.Round(*p*digits) / digits
		return &v
	}
	j := speedTestJSON{ID: t.ID, Start: t.Start.UnixMilli(), End: t.End.UnixMilli(), Trigger: t.Trigger, Outcome: t.Outcome,
		Attempts: t.Attempts, DownBps: r(t.DownBps, 1), UpBps: r(t.UpBps, 1), DownRetrans: t.DownRetrans, UpRetrans: t.UpRetrans,
		IdleGatewayMs: r(t.IdleGatewayMs, 10), LoadedGatewayMs: r(t.LoadedGatewayMs, 10), IdleCallMs: r(t.IdleCallMs, 10), LoadedCallMs: r(t.LoadedCallMs, 10)}
	if !public {
		j.Server, j.City, j.Port = t.Server, cities[t.Server], t.Port
	}
	return j
}

type speedParams struct {
	part     string
	days, tz int
	from, to time.Time
	key      string
}

func parseSpeed(q url.Values, now time.Time) (speedParams, error) {
	p := speedParams{part: q.Get("part"), days: 7}
	if err := checkParams(q, "part", "days", "tz", "from", "to"); err != nil {
		return p, err
	}
	switch p.part {
	case "", "summary":
		p.part = "summary"
		if q.Has("from") || q.Has("to") {
			return p, badReq("from and to are for part=windows")
		}
		switch d := q.Get("days"); d {
		case "", "7":
		case "30":
			p.days = 30
		default:
			return p, badReq("bad days %q: want 7 or 30", d)
		}
		if s := q.Get("tz"); s != "" {
			tz, err := strconv.Atoi(s)
			if err != nil || tz < -840 || tz > 840 {
				return p, badReq("bad tz %q: want minutes east of UTC, -840 to 840", s)
			}
			p.tz = int(math.Round(float64(tz)/15)) * 15
		}
		p.key = "speed\x00" + strconv.Itoa(p.days) + "\x00" + strconv.Itoa(p.tz)
	case "windows":
		if q.Has("days") || q.Has("tz") {
			return p, badReq("days and tz are for part=summary")
		}
		fs, ts := q.Get("from"), q.Get("to")
		if fs == "" {
			fs = "now-30m"
		}
		if ts == "" {
			ts = "now"
		}
		var fk, tk string
		var err error
		if p.from, fk, err = parseTime(fs, now); err != nil {
			return p, err
		}
		if p.to, tk, err = parseTime(ts, now); err != nil {
			return p, err
		}
		if !p.from.Before(p.to) {
			return p, badReq("from must be before to")
		}
		if p.to.Sub(p.from) > MaxRange {
			return p, badReq("range longer than %d days", int(MaxRange/(24*time.Hour)))
		}
		p.key = "speedw\x00" + fk + "\x00" + tk
	default:
		return p, badReq("bad part %q: want summary or windows", p.part)
	}
	return p, nil
}

// serveSpeed answers GET api/speed.
func (s *server) serveSpeed(w http.ResponseWriter, r *http.Request) {
	if s.d.Speed == nil {
		notFound(w, r)
		return
	}
	now := s.now()
	p, err := parseSpeed(r.URL.Query(), now)
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, err.Error())
		return
	}
	configurable := !s.public && s.canControl != nil && s.canControl(r)
	s.runCached(w, r, p.key, func(ctx context.Context) (*cached, error) {
		if p.part == "windows" {
			ts, err := s.d.Speed.SpeedTests(ctx, p.from.Add(-speedtest.MaxTestTime), p.to)
			if err != nil {
				return nil, err
			}
			return jsonCached(map[string]any{"at": now.UnixMilli(), "windows": speedWindows(ts, p.from)})
		}
		out, err := s.speedSummary(ctx, p, now)
		if err != nil {
			return nil, err
		}
		out.Configurable = configurable
		return jsonCached(out)
	})
}

func speedWindows(ts []model.SpeedTest, from time.Time) []speedWindow {
	out := []speedWindow{}
	for _, t := range ts {
		if !t.End.Before(from) && t.End.After(t.Start) {
			out = append(out, speedWindow{Start: t.Start.UnixMilli(), End: t.End.UnixMilli()})
		}
	}
	return out
}

func (s *server) speedSummary(ctx context.Context, p speedParams, now time.Time) (*speedJSON, error) {
	st := s.d.Speed.State()
	out := &speedJSON{At: now.UnixMilli(), TZ: p.tz, Days: p.days, Enabled: st.Enabled, IntervalS: int64(st.Interval / time.Second),
		Tests: []speedTestJSON{}}
	if !st.Next.IsZero() {
		n := st.Next.UnixMilli()
		out.NextAt = &n
	}
	if st.Running != nil {
		out.Running = &speedRunning{Started: st.Running.Started.UnixMilli(), Direction: st.Running.Direction}
		if !s.public {
			out.Running.Server = st.Running.Server
		}
	}
	cities := map[string]string{}
	for _, sv := range st.Servers {
		cities[sv.Name] = sv.City
	}
	if !s.public {
		out.Custom, out.Servers = st.Custom, st.Servers
	}
	from := now.Add(-time.Duration(p.days) * 24 * time.Hour)
	all, err := s.d.Speed.SpeedTests(ctx, now.Add(-speedtest.PatternSpan), now)
	if err != nil {
		return nil, err
	}
	var span []model.SpeedTest
	for _, t := range all {
		if !t.Start.Before(from) {
			span = append(span, t)
			out.Tests = append(out.Tests, speedTestOf(t, cities, s.public))
		}
	}
	latest := all
	if len(latest) == 0 { // an install that tested long ago
		if latest, err = s.d.Speed.SpeedTests(ctx, now.Add(-MaxRange), now); err != nil {
			return nil, err
		}
	}
	if len(latest) > 0 {
		l := speedTestOf(latest[len(latest)-1], cities, s.public)
		out.Latest = &l
	}
	tz := time.Duration(p.tz) * time.Minute
	out.ByHour = speedByHour{Hours: speedtest.ByHour(span, tz), DipHours: []int{}}
	if pt := speedtest.FindPattern(speedtest.ByHour(all, tz)); pt != nil {
		out.Pattern = pt
		// Highlight the dip in the asked span's chart too.
		out.ByHour.DipHours = pt.Hours
	}
	out.Windows = speedWindows(span, from)
	return out, nil
}

// speedPattern is Home's line: the 30-day pattern of the local hours of tz.
func (s *server) speedPattern(ctx context.Context, now time.Time, tz time.Duration) *speedtest.Pattern {
	if s.d.Speed == nil {
		return nil
	}
	ts, err := s.d.Speed.SpeedTests(ctx, now.Add(-speedtest.PatternSpan), now)
	if err != nil {
		return nil
	}
	return speedtest.FindPattern(speedtest.ByHour(ts, tz))
}

// serveSpeedRun answers POST api/speed/run ("Test now").
func (h *localHandler) serveSpeedRun(w http.ResponseWriter, r *http.Request) {
	if !h.speedPOST(w, r) {
		return
	}
	at, err := h.d.Speed.TestNow()
	if errors.Is(err, speedtest.ErrRunning) {
		writeErr(w, r, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		writeErr(w, r, http.StatusInternalServerError, "could not start the test")
		return
	}
	writeJSON(w, r, http.StatusAccepted, map[string]any{"started": at.UnixMilli()})
}

// serveSpeedConfig answers POST api/speed/config {"enabled": bool}.
func (h *localHandler) serveSpeedConfig(w http.ResponseWriter, r *http.Request) {
	if !h.speedPOST(w, r) {
		return
	}
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	d := json.NewDecoder(io.LimitReader(r.Body, 1<<10))
	d.DisallowUnknownFields()
	if err := d.Decode(&body); err != nil || body.Enabled == nil {
		writeErr(w, r, http.StatusBadRequest, `want {"enabled": true} or {"enabled": false}`)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), notesTimeout)
	defer cancel()
	next, err := h.d.Speed.SetEnabled(ctx, *body.Enabled)
	if err != nil {
		h.storeErr(w, r, "save the speed test setting", err)
		return
	}
	out := map[string]any{"enabled": *body.Enabled, "next_at": nil}
	if !next.IsZero() {
		out["next_at"] = next.UnixMilli()
	}
	writeJSON(w, r, http.StatusOK, out)
}

// speedPOST checks a speed-test write: a source, POST, no parameters, and
// the same protections as notes.
func (h *localHandler) speedPOST(w http.ResponseWriter, r *http.Request) bool {
	if h.d.Speed == nil {
		notFound(w, r)
		return false
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeErr(w, r, http.StatusMethodNotAllowed, "method not allowed")
		return false
	}
	if len(r.URL.RawQuery) > 0 {
		writeErr(w, r, http.StatusBadRequest, "no parameters allowed")
		return false
	}
	return h.controlAllowed(w, r, "speed tests")
}
