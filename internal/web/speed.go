package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/profile"
	"github.com/fuck-you-isp/fyisp/internal/speedtest"
)

// SpeedSource runs and keeps the speed tests (speedtest.Scheduler). See
// docs/SPEEDTEST-API.md; keep the JSON types below in sync with it.
type SpeedSource interface {
	State() speedtest.State
	SpeedTests(ctx context.Context, from, to time.Time) ([]model.SpeedTest, error)
	// Queue is the speed-test queue (Test now, sweeps, schedule slots).
	Queue() *speedtest.Queue
	SetMode(ctx context.Context, m speedtest.Mode) (time.Time, error)
	MaskedShare(d time.Duration) float64
}

// SpeedTTL is how long the share link caches api/speed.
const SpeedTTL = 30 * time.Second

// fallbackNote is the share of a region's tests on a fallback server (last
// 24 h) above which the region gets a note.
const fallbackNote = 0.5

// speedJSON is api/speed?part=summary.
type speedJSON struct {
	At              int64              `json:"at"`
	TZ              int                `json:"tz"`
	Days            int                `json:"days"`
	ExcludeFallback bool               `json:"exclude_fallback"`
	Mode            speedtest.Mode     `json:"mode"`
	Modes           []speedtest.Mode   `json:"modes"`
	Enabled         bool               `json:"enabled"`
	Configurable    bool               `json:"configurable"`
	IntervalS       int64              `json:"interval_s"`
	SpacingS        int64              `json:"spacing_s"`
	PlannedShare    float64            `json:"planned_share"`
	Warning         *string            `json:"warning"`
	MaskedShare24h  float64            `json:"masked_share_24h"`
	NextAt          *int64             `json:"next_at"`
	Next            *speedNext         `json:"next"`
	Running         *speedRunning      `json:"running"`
	Servers         []speedServerJSON  `json:"servers"`
	Regions         []speedRegionJSON  `json:"regions"`
	PerRegion       []speedRowJSON     `json:"per_region"`
	PerServer       []speedRowJSON     `json:"per_server"`
	Latest          *speedTestJSON     `json:"latest"`
	Pattern         *speedtest.Pattern `json:"pattern"`
}

type speedNext struct {
	Region *string `json:"region"`
	Server *string `json:"server"`
}

type speedRef struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

type speedRunning struct {
	Started     int64     `json:"started"`
	ExpectedEnd int64     `json:"expected_end"`
	Direction   string    `json:"direction"`
	Server      *speedRef `json:"server"`
	Region      *string   `json:"region"`
}

type speedServerJSON struct {
	ID       string   `json:"id"`
	Label    string   `json:"label"`
	Provider string   `json:"provider"`
	City     string   `json:"city"`
	Country  string   `json:"country"`
	Geo      string   `json:"geo"`
	Region   string   `json:"region"` // the geo's title, e.g. "Europe"
	RTTms    *float64 `json:"rtt_ms"`
	Picked   bool     `json:"picked"`
	Custom   bool     `json:"custom"`
	Host     string   `json:"host,omitempty"`  // local only
	Ports    string   `json:"ports,omitempty"` // local only
}

type speedRegionJSON struct {
	ID    string   `json:"id"`
	Title string   `json:"title"`
	Chain []string `json:"chain"`
}

// speedRowJSON is one heatmap or table row: a region or a server.
type speedRowJSON struct {
	ID       string `json:"id"`
	Label    string `json:"label"` // region title or server label
	LatestAt *int64 `json:"latest_at"`
	speedtest.Stats
	// Regions only: the share of the last 24 h's tests on a fallback
	// server, and a note when it is over half.
	FallbackShare24h *float64 `json:"fallback_share_24h,omitempty"`
	FallbackNote     *string  `json:"fallback_note,omitempty"`
}

type speedWindow struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"`
}

// speedTestJSON is one test. The port (and every host) is left out on the
// share link; the user's own servers are "custom-N" there.
type speedTestJSON struct {
	ID              int64    `json:"id"`
	Start           int64    `json:"start"`
	End             int64    `json:"end"`
	Trigger         string   `json:"trigger"`
	OnDemand        bool     `json:"on_demand"` // Test now or a sweep: left out of patterns and rows
	Outcome         string   `json:"outcome"`
	Region          *string  `json:"region"`
	Server          string   `json:"server"`
	Label           string   `json:"label"`
	Fallback        bool     `json:"fallback"`
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

// speedNames maps server IDs to their public ID and label.
type speedNames struct {
	public bool
	label  map[string]string
	custom map[string]int // own servers (host:port): their number
}

func newSpeedNames(st speedtest.State, public bool) speedNames {
	n := speedNames{public: public, label: map[string]string{}, custom: map[string]int{}}
	for _, sv := range st.Servers {
		n.label[sv.ID] = sv.Label
		if sv.Custom {
			n.custom[sv.ID] = len(n.custom) + 1
		}
	}
	return n
}

// ref is a server's ID and label as this viewer may see them.
func (n speedNames) ref(id string) speedRef {
	if k, ok := n.custom[id]; ok {
		label := "Your server"
		if len(n.custom) > 1 {
			label += " " + strconv.Itoa(k)
		}
		if n.public {
			return speedRef{ID: "custom-" + strconv.Itoa(k), Label: label}
		}
		return speedRef{ID: id, Label: label}
	}
	if l, ok := n.label[id]; ok {
		return speedRef{ID: id, Label: l}
	}
	if n.public && id != "" {
		return speedRef{ID: "custom", Label: "Your server"} // a removed own server
	}
	return speedRef{ID: id, Label: id}
}

// resolve maps a requested public or local ID back to the stored one.
func (n speedNames) resolve(id string) string {
	for local, k := range n.custom {
		if id == "custom-"+strconv.Itoa(k) {
			return local
		}
	}
	return id
}

func roundP(p *float64, digits float64) *float64 {
	if p == nil {
		return nil
	}
	v := math.Round(*p*digits) / digits
	return &v
}

func (n speedNames) test(t model.SpeedTest) speedTestJSON {
	ref := n.ref(t.Server)
	j := speedTestJSON{ID: t.ID, Start: t.Start.UnixMilli(), End: t.End.UnixMilli(), Trigger: t.Trigger, OnDemand: t.OnDemand(), Outcome: t.Outcome,
		Server: ref.ID, Label: ref.Label, Fallback: t.Fallback, Attempts: t.Attempts,
		DownBps: roundP(t.DownBps, 1), UpBps: roundP(t.UpBps, 1), DownRetrans: t.DownRetrans, UpRetrans: t.UpRetrans,
		IdleGatewayMs: roundP(t.IdleGatewayMs, 10), LoadedGatewayMs: roundP(t.LoadedGatewayMs, 10),
		IdleCallMs: roundP(t.IdleCallMs, 10), LoadedCallMs: roundP(t.LoadedCallMs, 10)}
	if t.Region != "" {
		r := t.Region
		j.Region = &r
	}
	if !n.public {
		j.Port = t.Port
	}
	return j
}

type speedParams struct {
	part            string
	days, tz        int
	excludeFallback bool
	server, region  string
	from, to        time.Time
	entry           uint64 // part=entry
	key             string
}

func parseSpeed(q url.Values, now time.Time) (speedParams, error) {
	p := speedParams{part: q.Get("part"), days: 7}
	if err := checkParams(q, "part", "days", "tz", "from", "to", "exclude_fallback", "server", "region", "id"); err != nil {
		return p, err
	}
	if q.Has("id") && p.part != "entry" {
		return p, badReq("id is for part=entry")
	}
	only := func(part string, names ...string) error {
		for _, n := range names {
			if q.Has(n) {
				return badReq("%s is for part=%s", n, part)
			}
		}
		return nil
	}
	switch p.part {
	case "", "summary":
		p.part = "summary"
		if err := only("windows", "from", "to"); err != nil {
			return p, err
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
		switch x := q.Get("exclude_fallback"); x {
		case "", "0":
		case "1":
			p.excludeFallback = true
		default:
			return p, badReq("bad exclude_fallback %q: want 0 or 1", x)
		}
		p.server, p.region = q.Get("server"), q.Get("region")
		if p.server != "" && p.region != "" {
			return p, badReq("server or region, not both")
		}
		if len(p.server) > 64 || len(p.region) > 64 {
			return p, badReq("id too long")
		}
		p.key = fmt.Sprintf("speed\x00%d\x00%d\x00%v\x00%s\x00%s", p.days, p.tz, p.excludeFallback, p.server, p.region)
	case "windows":
		if err := only("summary", "days", "tz", "exclude_fallback", "server", "region"); err != nil {
			return p, err
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
	case "running":
		if err := only("summary", "days", "tz", "exclude_fallback", "server", "region", "from", "to"); err != nil {
			return p, err
		}
		p.key = "speedr"
	case "queue", "sweep", "entry":
		if err := only("summary or windows", "days", "tz", "exclude_fallback", "server", "region", "from", "to"); err != nil {
			return p, err
		}
		if p.part == "entry" {
			id, err := strconv.ParseUint(q.Get("id"), 10, 64)
			if err != nil || len(q["id"]) != 1 {
				return p, badReq("part=entry needs one id")
			}
			p.entry = id
		}
	default:
		return p, badReq("bad part %q: want summary, windows, running, queue, entry or sweep", p.part)
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
	switch p.part {
	case "queue", "entry", "sweep":
		s.serveSpeedQueue(w, r, p, now)
		return
	}
	configurable := !s.public && s.canControl != nil && s.canControl(r)
	if p.part == "summary" && (p.server != "" || p.region != "") {
		out, err := s.speedOne(r.Context(), p, now)
		if errors.Is(err, errNoSpeedID) {
			writeErr(w, r, http.StatusNotFound, err.Error())
			return
		}
		if err != nil {
			writeErr(w, r, http.StatusServiceUnavailable, "speed tests unavailable")
			return
		}
		s.runCached(w, r, p.key, func(context.Context) (*cached, error) { return jsonCached(out) })
		return
	}
	s.runCached(w, r, p.key, func(ctx context.Context) (*cached, error) {
		st := s.d.Speed.State()
		names := newSpeedNames(st, s.public)
		switch p.part {
		case "windows":
			ts, err := s.d.Speed.SpeedTests(ctx, p.from.Add(-speedtest.SlotCap), p.to)
			if err != nil {
				return nil, err
			}
			return jsonCached(map[string]any{"at": now.UnixMilli(), "windows": speedWindows(ts, p.from)})
		case "running":
			return jsonCached(map[string]any{"at": now.UnixMilli(), "running": speedRunningOf(st, names)})
		}
		out, err := s.speedSummary(ctx, p, st, names, now)
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

func speedRunningOf(st speedtest.State, names speedNames) *speedRunning {
	if st.Running == nil {
		return nil
	}
	r := &speedRunning{Started: st.Running.Started.UnixMilli(), ExpectedEnd: st.Running.ExpectedEnd.UnixMilli(), Direction: st.Running.Direction}
	if st.Running.Server != "" {
		ref := names.ref(st.Running.Server)
		r.Server = &ref
	}
	if st.Running.Region != "" {
		x := st.Running.Region
		r.Region = &x
	}
	return r
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func (s *server) speedSummary(ctx context.Context, p speedParams, st speedtest.State, names speedNames, now time.Time) (*speedJSON, error) {
	out := &speedJSON{At: now.UnixMilli(), TZ: p.tz, Days: p.days, ExcludeFallback: p.excludeFallback, Mode: st.Mode, Modes: st.Modes,
		Enabled: st.Mode != speedtest.ModeOff, IntervalS: int64(st.Interval / time.Second), SpacingS: int64(st.Spacing / time.Second),
		PlannedShare: math.Round(st.PlannedShare*1000) / 1000, Warning: strPtr(st.Warning),
		MaskedShare24h: math.Round(s.d.Speed.MaskedShare(24*time.Hour)*1000) / 1000,
		Running:        speedRunningOf(st, names), Servers: []speedServerJSON{}, Regions: []speedRegionJSON{},
		PerRegion: []speedRowJSON{}, PerServer: []speedRowJSON{}}
	if !st.Next.IsZero() {
		n := st.Next.UnixMilli()
		out.NextAt = &n
		out.Next = &speedNext{Region: strPtr(st.NextRegion)}
		if st.NextServer != "" {
			id := names.ref(st.NextServer).ID
			out.Next.Server = &id
		}
	}
	for _, sv := range st.Servers {
		ref := names.ref(sv.ID)
		j := speedServerJSON{ID: ref.ID, Label: ref.Label, Provider: sv.Provider, City: sv.City, Country: sv.Country, Geo: sv.Geo,
			Region: profile.GeoTitles[sv.Geo], RTTms: roundP(sv.RTTms, 10), Picked: sv.Picked, Custom: sv.Custom}
		if sv.Custom {
			j.Provider = "Your server"
		}
		if !s.public {
			j.Host = sv.Host
			j.Ports = strconv.Itoa(sv.PortMin)
			if sv.PortMax != sv.PortMin {
				j.Ports += "-" + strconv.Itoa(sv.PortMax)
			}
		}
		out.Servers = append(out.Servers, j)
	}
	titles := map[string]string{}
	for _, r := range st.Regions {
		out.Regions = append(out.Regions, speedRegionJSON{ID: r.ID, Title: r.Title, Chain: r.Chain})
		titles[r.ID] = r.Title
	}
	from := now.Add(-time.Duration(p.days) * 24 * time.Hour)
	all, err := s.d.Speed.SpeedTests(ctx, now.Add(-speedtest.PatternSpan), now)
	if err != nil {
		return nil, err
	}
	tz := time.Duration(p.tz) * time.Minute
	byRegion, byServer := map[string][]model.SpeedTest{}, map[string][]model.SpeedTest{}
	var last24 []model.SpeedTest
	for _, t := range all {
		if t.OnDemand() {
			continue // rows and patterns are the schedule's (latest still shows every test)
		}
		if !t.Start.Before(now.Add(-24 * time.Hour)) {
			last24 = append(last24, t)
		}
		if t.Start.Before(from) {
			continue
		}
		byServer[t.Server] = append(byServer[t.Server], t)
		if t.Region != "" && !(p.excludeFallback && t.Fallback) {
			byRegion[t.Region] = append(byRegion[t.Region], t)
		}
	}
	for _, r := range st.Regions {
		row := speedRow(r.ID, r.Title, byRegion[r.ID], tz)
		var n, fb int
		for _, t := range last24 {
			if t.Region == r.ID && t.Outcome == model.SpeedOK {
				n++
				if t.Fallback {
					fb++
				}
			}
		}
		if n > 0 {
			share := math.Round(float64(fb)/float64(n)*100) / 100
			row.FallbackShare24h = &share
			if share > fallbackNote {
				note := fmt.Sprintf("%s mostly used a fallback server in the last 24 hours: its first choice was busy or not answering.", r.Title)
				row.FallbackNote = &note
			}
		}
		out.PerRegion = append(out.PerRegion, row)
	}
	// Servers with tests in the span, in catalog order.
	for _, sv := range st.Servers {
		if ts := byServer[sv.ID]; len(ts) > 0 {
			ref := names.ref(sv.ID)
			out.PerServer = append(out.PerServer, speedRow(ref.ID, ref.Label, ts, tz))
		}
	}
	latest := all
	if len(latest) == 0 { // an install that tested long ago
		if latest, err = s.d.Speed.SpeedTests(ctx, now.Add(-MaxRange), now); err != nil {
			return nil, err
		}
	}
	if len(latest) > 0 {
		l := names.test(latest[len(latest)-1])
		out.Latest = &l
	}
	out.Pattern = speedtest.PooledPattern(all, tz)
	return out, nil
}

func speedRow(id, label string, ts []model.SpeedTest, tz time.Duration) speedRowJSON {
	row := speedRowJSON{ID: id, Label: label, Stats: speedtest.Summarize(ts, tz)}
	if row.Stats.LatestAt != nil {
		ms := row.Stats.LatestAt.UnixMilli()
		row.LatestAt = &ms
	}
	return row
}

var errNoSpeedID = errors.New("no such server or region")

// speedOneJSON is api/speed?server=<id> or ?region=<id>: one row's stats,
// every test in the span (oldest first) and its windows.
type speedOneJSON struct {
	At              int64           `json:"at"`
	TZ              int             `json:"tz"`
	Days            int             `json:"days"`
	ExcludeFallback bool            `json:"exclude_fallback"`
	Kind            string          `json:"kind"` // "server" or "region"
	Row             speedRowJSON    `json:"row"`
	Tests           []speedTestJSON `json:"tests"`
	Windows         []speedWindow   `json:"windows"`
}

func (s *server) speedOne(ctx context.Context, p speedParams, now time.Time) (*speedOneJSON, error) {
	st := s.d.Speed.State()
	names := newSpeedNames(st, s.public)
	from := now.Add(-time.Duration(p.days) * 24 * time.Hour)
	out := &speedOneJSON{At: now.UnixMilli(), TZ: p.tz, Days: p.days, ExcludeFallback: p.excludeFallback, Tests: []speedTestJSON{}}
	keep := func(model.SpeedTest) bool { return false }
	var label string
	if p.region != "" {
		i := slices.IndexFunc(st.Regions, func(r speedtest.Region) bool { return r.ID == p.region })
		if i < 0 {
			return nil, errNoSpeedID
		}
		out.Kind, label = "region", st.Regions[i].Title
		keep = func(t model.SpeedTest) bool { return t.Region == p.region && !(p.excludeFallback && t.Fallback) }
	} else {
		id := names.resolve(p.server)
		i := slices.IndexFunc(st.Servers, func(sv speedtest.ServerState) bool { return sv.ID == id })
		if i < 0 || (s.public && id == p.server && names.custom[id] > 0) { // own servers only by public ID there
			return nil, errNoSpeedID
		}
		out.Kind, label = "server", names.ref(id).Label
		keep = func(t model.SpeedTest) bool { return t.Server == id }
	}
	all, err := s.d.Speed.SpeedTests(ctx, from, now)
	if err != nil {
		return nil, err
	}
	var ts []model.SpeedTest
	for _, t := range all {
		if keep(t) {
			ts = append(ts, t)
			out.Tests = append(out.Tests, names.test(t))
		}
	}
	id := p.region
	if id == "" {
		id = p.server
	}
	out.Row = speedRow(id, label, speedtest.Scheduled(ts), time.Duration(p.tz)*time.Minute) // tests lists every test
	out.Windows = speedWindows(ts, from)
	return out, nil
}

// speedPattern is Home's line: the pooled 30-day pattern in tz's hours.
func (s *server) speedPattern(ctx context.Context, now time.Time, tz time.Duration) *speedtest.Pattern {
	if s.d.Speed == nil {
		return nil
	}
	ts, err := s.d.Speed.SpeedTests(ctx, now.Add(-speedtest.PatternSpan), now)
	if err != nil {
		return nil
	}
	return speedtest.PooledPattern(ts, tz)
}

// serveSpeedConfig answers POST api/speed/config {"mode": ...}.
func (h *localHandler) serveSpeedConfig(w http.ResponseWriter, r *http.Request) {
	if !h.speedPOST(w, r) {
		return
	}
	var body struct {
		Mode   *string `json:"mode"`
		Public *bool   `json:"public"` // the share link may queue tests
	}
	d := json.NewDecoder(io.LimitReader(r.Body, 1<<10))
	d.DisallowUnknownFields()
	if err := d.Decode(&body); err != nil || (body.Mode == nil && body.Public == nil) {
		writeErr(w, r, http.StatusBadRequest, `want {"mode": "off", "regions", "nearest" or "custom"} and/or {"public": true or false}`)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), notesTimeout)
	defer cancel()
	out := map[string]any{}
	if body.Mode != nil {
		next, err := h.d.Speed.SetMode(ctx, speedtest.Mode(*body.Mode))
		if errors.Is(err, speedtest.ErrMode) {
			writeErr(w, r, http.StatusBadRequest, fmt.Sprintf("mode %q is not offered here", *body.Mode))
			return
		}
		if err != nil {
			h.storeErr(w, r, "save the speed test setting", err)
			return
		}
		out["mode"], out["next_at"] = *body.Mode, nil
		if !next.IsZero() {
			out["next_at"] = next.UnixMilli()
		}
	}
	if body.Public != nil {
		if err := h.d.Speed.Queue().SetPublic(ctx, *body.Public); err != nil {
			h.storeErr(w, r, "save the speed test setting", err)
			return
		}
		out["public"] = *body.Public
	}
	writeJSON(w, r, http.StatusOK, out)
}

// speedPOST checks a speed-test write: a source, POST, only the allowed
// parameters, and the same protections as notes.
func (h *localHandler) speedPOST(w http.ResponseWriter, r *http.Request, params ...string) bool {
	if h.d.Speed == nil {
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
	return h.controlAllowed(w, r, "speed tests")
}
