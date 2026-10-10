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
	"strings"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/callq"
	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/profile"
	"github.com/fuck-you-isp/fyisp/internal/speedtest"
	"github.com/fuck-you-isp/fyisp/internal/store"
)

// SpeedSource runs and keeps the speed tests (speedtest.Scheduler). See
// docs/SPEEDTEST-API.md; keep the JSON types below in sync with it.
type SpeedSource interface {
	State() speedtest.State
	SpeedTests(ctx context.Context, from, to time.Time) ([]model.SpeedTest, error)
	SetMode(ctx context.Context, m speedtest.Mode, extra []string) (time.Time, error)
	MaskedShare(d time.Duration) float64
	Enqueue(kind, id, who, client string) (speedtest.Entry, bool, error)
	Queue() speedtest.QueueState
	QueueEntry(id int64) (speedtest.Entry, bool)
	Cancel(id int64) error
	ClearQueue(who string) int
	SweepSize() int
	Catalog() *speedtest.Catalog
	BytesSince(ctx context.Context, t time.Time) (int64, error)
}

// Speed API limits.
const (
	SpeedTTL = 30 * time.Second // the share link's cache of summaries
	// fallbackNote is the share of a region's tests on a fallback server
	// (last 24 h) above which the row gets a note.
	fallbackNote = 0.5
	// Share link POSTs (Test now, sweeps): per client, on top of the
	// queue's own limits.
	speedPostRate, speedPostBurst = 1.0 / 10, 3
	speedTestsCap                 = 5000 // tests in one row view
	defaultTestBytes              = 150 << 20
)

// ---------- JSON ----------

type speedRef struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

type speedServerJSON struct {
	ID       string   `json:"id"`
	Label    string   `json:"label"`
	Display  string   `json:"display"` // "UK · Clouvider, London": public-safe
	Provider string   `json:"provider"`
	City     string   `json:"city"`
	Country  string   `json:"country"`
	Geo      string   `json:"geo"`
	Area     string   `json:"area"`   // the geo's title, e.g. "Europe"
	Region   *string  `json:"region"` // the test region it belongs to, if any
	Metro    string   `json:"metro"`
	RTTms    *float64 `json:"rtt_ms"`
	Picked   bool     `json:"picked"`
	Custom   bool     `json:"custom"`
	Single   bool     `json:"single_port"`
	Capacity *float64 `json:"capacity_mbps"`
	Host     string   `json:"host,omitempty"`  // local only
	Ports    string   `json:"ports,omitempty"` // local only
}

type speedRegionJSON struct {
	ID        string   `json:"id"`
	Title     string   `json:"title"`
	Servers   []string `json:"servers"` // the metro's servers, rotated per hour and day
	Nearby    []string `json:"nearby"`  // the nearby-city fallback
	Default   bool     `json:"default"`
	Providers []string `json:"providers"`
}

// speedRow is one heatmap or table row: a region or a server.
type speedRow struct {
	ID        string   `json:"id"`
	Kind      string   `json:"kind"` // region or server
	Label     string   `json:"label"`
	Why       string   `json:"why,omitempty"` // "nearest Clouvider", "far: US East", "added"
	Scheduled bool     `json:"scheduled"`     // a row of the current mode
	Servers   []string `json:"servers"`
	LatestAt  *int64   `json:"latest_at"`
	speedtest.Stats
	FallbackShare24h *float64 `json:"fallback_share_24h"`
	Note             *string  `json:"note"`
}

type speedWindow struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"`
}

// speedTestJSON is one test. Ports and hosts are local only; own servers
// are "custom-N" on the share link.
type speedTestJSON struct {
	ID              int64    `json:"id"`
	Start           int64    `json:"start"`
	End             int64    `json:"end"`
	Trigger         string   `json:"trigger"` // schedule, manual, sweep
	Manual          bool     `json:"manual"`  // not part of the time-of-day patterns
	Outcome         string   `json:"outcome"`
	Region          *string  `json:"region"`
	Server          string   `json:"server"`
	Label           string   `json:"label"`
	Display         string   `json:"display"`
	Provider        string   `json:"provider"`
	City            string   `json:"city"`
	Fallback        bool     `json:"fallback"`
	FallbackLevel   string   `json:"fallback_level"`
	Port            int      `json:"port,omitempty"`
	Attempts        int      `json:"attempts"`
	DownBps         *float64 `json:"down_bps"`
	UpBps           *float64 `json:"up_bps"`
	DownShort       bool     `json:"down_short"`
	UpShort         bool     `json:"up_short"`
	DownSecs        float64  `json:"down_s"`
	UpSecs          float64  `json:"up_s"`
	CappedByServer  bool     `json:"capped_by_server"`
	ScoreDown       *float64 `json:"score"`
	ScoreUp         *float64 `json:"up_score"`
	Bytes           int64    `json:"bytes"`
	DownRetrans     *int64   `json:"down_retrans"`
	UpRetrans       *int64   `json:"up_retrans"`
	IdleGatewayMs   *float64 `json:"idle_gateway_ms"`
	LoadedGatewayMs *float64 `json:"loaded_gateway_ms"`
	Connection      string   `json:"connection,omitempty"`
	Latency         any      `json:"latency,omitempty"` // by layer (edge, gateway): idle and per direction
}

type speedRunning struct {
	Entry       int64     `json:"entry"`
	Kind        string    `json:"kind"`
	Started     int64     `json:"started"`
	ExpectedEnd int64     `json:"expected_end"`
	Step        string    `json:"step"` // picking, download, upload
	Server      *speedRef `json:"server"`
	Index       int       `json:"index"`
	Total       int       `json:"total"`
}

type speedEntry struct {
	ID          int64              `json:"id"`
	Kind        string             `json:"kind"` // test, sweep, scheduled
	Who         string             `json:"who"`  // you, someone, scheduled
	State       string             `json:"state"`
	Position    int                `json:"position"` // 1 = next, 0 = running or finished
	Label       string             `json:"label"`
	Target      string             `json:"target,omitempty"`
	RequestedAt int64              `json:"requested_at"`
	StartedAt   *int64             `json:"started_at"`
	FinishedAt  *int64             `json:"finished_at"`
	EstStart    *int64             `json:"est_start"`
	EstEnd      *int64             `json:"est_end"`
	Progress    *speedRunning      `json:"progress,omitempty"`
	Results     []speedSweepResult `json:"results,omitempty"`
}

type speedSweepResult struct {
	Server    string   `json:"server"`
	Label     string   `json:"label"`
	Display   string   `json:"display"`
	Region    *string  `json:"region"`
	Provider  string   `json:"provider"`
	Status    string   `json:"status"`
	Direction string   `json:"direction,omitempty"`
	DownBps   *float64 `json:"down_bps"`
	UpBps     *float64 `json:"up_bps"`
	DownShort bool     `json:"down_short"`
	UpShort   bool     `json:"up_short"`
	At        *int64   `json:"at"`
}

type speedQueueJSON struct {
	At        int64        `json:"at"`
	Running   *speedEntry  `json:"running"`
	Waiting   []speedEntry `json:"waiting"`
	LastSweep *speedEntry  `json:"last_sweep"`
}

type speedPlan struct {
	Mode         speedtest.Mode `json:"mode"`
	TestsPerHour float64        `json:"tests_per_hour"`
	Share        float64        `json:"share"`
	EstGBMonth   float64        `json:"est_gb_month"`
}

type speedHour struct {
	Hour      int `json:"hour"`
	N         int `json:"n"`
	Providers int `json:"providers"`
}

type speedLoad struct {
	Layer    string   `json:"layer"` // edge, gateway
	IdleMs   *float64 `json:"idle_ms"`
	DownMs   *float64 `json:"down_ms"` // median p50 while downloading
	UpMs     *float64 `json:"up_ms"`
	LossDown float64  `json:"loss_down"` // share of probes lost while downloading
	LossUp   float64  `json:"loss_up"`
}

type speedUnderLoad struct {
	Layers []speedLoad `json:"layers"`
	Text   []string    `json:"text"`
}

// speedJSON is api/speed?part=summary.
type speedJSON struct {
	At              int64              `json:"at"`
	TZ              int                `json:"tz"`
	Days            int                `json:"days"`
	ExcludeFallback bool               `json:"exclude_fallback"`
	Mode            speedtest.Mode     `json:"mode"`
	Modes           []speedtest.Mode   `json:"modes"`
	Enabled         bool               `json:"enabled"`
	SetByFlag       bool               `json:"set_by_flag"`
	Extra           []string           `json:"extra"`
	Configurable    bool               `json:"configurable"`
	IntervalS       int64              `json:"interval_s"`
	SpacingS        int64              `json:"spacing_s"`
	PlannedShare    float64            `json:"planned_share"`
	Warning         *string            `json:"warning"`
	Plans           []speedPlan        `json:"plans"`
	MaskedShare24h  float64            `json:"masked_share_24h"`
	BytesMonth      int64              `json:"bytes_month"`
	Connection      speedConnection    `json:"connection"`
	NextAt          *int64             `json:"next_at"`
	NextRow         *string            `json:"next_row"`
	Running         *speedRunning      `json:"running"`
	Rows            []speedRow         `json:"rows"`
	PerRegion       []speedRow         `json:"per_region"`
	PerServer       []speedRow         `json:"per_server"`
	Servers         []speedServerJSON  `json:"servers"`
	Regions         []speedRegionJSON  `json:"regions"`
	Latest          *speedTestJSON     `json:"latest"`
	Pattern         *speedtest.Pattern `json:"pattern"`
	Finding         speedtest.Finding  `json:"finding"`
	Coverage        []speedHour        `json:"coverage"`
	UnderLoad       speedUnderLoad     `json:"under_load"`
}

type speedConnection struct {
	Type   string `json:"type"`   // wired, wifi, unknown
	Source string `json:"source"` // detected
}

// ---------- names ----------

// speedNames maps server IDs to what a viewer may see.
type speedNames struct {
	public  bool
	cat     *speedtest.Catalog
	custom  map[string]int // own servers (host:port): their number
	regions map[string]string
	inReg   map[string]string // server -> its (first) region
}

func newSpeedNames(st speedtest.State, cat *speedtest.Catalog, public bool) speedNames {
	n := speedNames{public: public, cat: cat, custom: map[string]int{}, regions: map[string]string{}, inReg: map[string]string{}}
	for _, sv := range st.Servers {
		if sv.Custom {
			n.custom[sv.ID] = len(n.custom) + 1
		}
	}
	for _, r := range st.Regions {
		n.regions[r.ID] = r.Title
		for _, id := range append(slices.Clone(r.Servers), r.Nearby...) {
			if _, ok := n.inReg[id]; !ok {
				n.inReg[id] = r.ID
			}
		}
	}
	return n
}

// server is a server's public-safe identity: id, label, display, provider
// and city.
func (n speedNames) server(id string) (ref speedRef, display, provider, city string) {
	if k, ok := n.custom[id]; ok {
		label := "Your server"
		if len(n.custom) > 1 {
			label += " " + strconv.Itoa(k)
		}
		pid := id
		if n.public {
			pid = "custom-" + strconv.Itoa(k)
		}
		return speedRef{ID: pid, Label: label}, label, "Your server", ""
	}
	if sv, ok := n.cat.Server(id); ok {
		display = sv.Provider + ", " + sv.City
		if r := n.inReg[id]; r != "" {
			display = n.regions[r] + " · " + display
		}
		return speedRef{ID: id, Label: sv.Label}, display, sv.Provider, sv.City
	}
	if n.public && id != "" {
		return speedRef{ID: "custom", Label: "Your server"}, "Your server", "Your server", ""
	}
	return speedRef{ID: id, Label: id}, id, "", ""
}

func (n speedNames) ref(id string) speedRef { r, _, _, _ := n.server(id); return r }

// resolve maps a requested (public or local) ID to the stored one.
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

func ms(t time.Time) *int64 {
	if t.IsZero() {
		return nil
	}
	v := t.UnixMilli()
	return &v
}

func (n speedNames) test(t model.SpeedTest) speedTestJSON {
	ref, display, provider, city := n.server(t.Server)
	j := speedTestJSON{ID: t.ID, Start: t.Start.UnixMilli(), End: t.End.UnixMilli(), Trigger: t.Trigger, Manual: !speedtest.Scheduled(t),
		Outcome: t.Outcome, Server: ref.ID, Label: ref.Label, Display: display, Provider: provider, City: city,
		Fallback: t.Fallback, FallbackLevel: t.FallbackLevel, Attempts: t.Attempts,
		DownBps: roundP(t.DownBps, 1), UpBps: roundP(t.UpBps, 1), DownShort: t.DownShort, UpShort: t.UpShort,
		DownSecs: t.DownSecs, UpSecs: t.UpSecs, CappedByServer: t.CappedByServer, ScoreDown: t.ScoreDown, ScoreUp: t.ScoreUp,
		Bytes: t.Bytes, DownRetrans: t.DownRetrans, UpRetrans: t.UpRetrans,
		IdleGatewayMs: roundP(t.IdleGatewayMs, 10), LoadedGatewayMs: roundP(t.LoadedGatewayMs, 10)}
	if j.FallbackLevel == "" {
		j.FallbackLevel = speedtest.FallbackNone
	}
	if t.Region != "" {
		r := t.Region
		j.Region = &r
	}
	x := speedtest.ParseExtra(t.Extra)
	j.Connection = x.Connection
	if len(x.Latency) > 0 {
		j.Latency = x.Latency
	}
	if !n.public {
		j.Port = t.Port
	}
	return j
}

// ---------- params ----------

type speedParams struct {
	part            string
	days, tz        int
	excludeFallback bool
	row             string
	entry           int64
	from, to        time.Time
	key             string
}

func parseSpeed(q url.Values, now time.Time) (speedParams, error) {
	p := speedParams{part: q.Get("part"), days: 7}
	if err := checkParams(q, "part", "days", "tz", "from", "to", "exclude_fallback", "server", "region", "row", "id"); err != nil {
		return p, err
	}
	only := func(part string, names ...string) error {
		for _, n := range names {
			if q.Has(n) {
				return badReq("%s is not for part=%s", n, part)
			}
		}
		return nil
	}
	tz := func() error {
		if s := q.Get("tz"); s != "" {
			v, err := strconv.Atoi(s)
			if err != nil || v < -840 || v > 840 {
				return badReq("bad tz %q: want minutes east of UTC, -840 to 840", s)
			}
			p.tz = int(math.Round(float64(v)/15)) * 15
		}
		return nil
	}
	switch p.part {
	case "", "summary":
		p.part = "summary"
		if err := only("summary", "from", "to", "id"); err != nil {
			return p, err
		}
		switch d := q.Get("days"); d {
		case "", "7":
		case "30":
			p.days = 30
		default:
			return p, badReq("bad days %q: want 7 or 30", d)
		}
		if err := tz(); err != nil {
			return p, err
		}
		switch x := q.Get("exclude_fallback"); x {
		case "", "0":
		case "1":
			p.excludeFallback = true
		default:
			return p, badReq("bad exclude_fallback %q: want 0 or 1", x)
		}
		ids := 0
		for _, k := range []string{"row", "server", "region"} {
			if v := q.Get(k); v != "" {
				p.row = v
				ids++
			}
		}
		if ids > 1 || len(p.row) > 64 {
			return p, badReq("one of row, server or region")
		}
		p.key = fmt.Sprintf("speed\x00%d\x00%d\x00%v\x00%s", p.days, p.tz, p.excludeFallback, p.row)
	case "windows":
		if err := only("windows", "days", "tz", "exclude_fallback", "server", "region", "row", "id"); err != nil {
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
	case "running", "queue":
		if err := only(p.part, "days", "tz", "exclude_fallback", "server", "region", "row", "from", "to", "id"); err != nil {
			return p, err
		}
	case "sweep", "entry":
		if err := only(p.part, "days", "tz", "exclude_fallback", "server", "region", "row", "from", "to"); err != nil {
			return p, err
		}
		if s := q.Get("id"); s != "" {
			id, err := strconv.ParseInt(s, 10, 64)
			if err != nil || id <= 0 {
				return p, badReq("bad id %q", s)
			}
			p.entry = id
		} else if p.part == "entry" {
			return p, badReq("id is required")
		}
	default:
		return p, badReq("bad part %q: want summary, windows, running, queue, sweep or entry", p.part)
	}
	return p, nil
}

// ---------- GET ----------

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
	st := s.d.Speed.State()
	names := newSpeedNames(st, s.d.Speed.Catalog(), s.public)
	me := s.requester(r)
	switch p.part {
	case "running":
		writeJSON(w, r, http.StatusOK, map[string]any{"at": now.UnixMilli(), "running": s.speedRunning(names)})
		return
	case "queue":
		writeJSON(w, r, http.StatusOK, s.speedQueue(names, me, now))
		return
	case "sweep", "entry":
		var e speedtest.Entry
		ok := false
		if p.entry > 0 {
			e, ok = s.d.Speed.QueueEntry(p.entry)
		} else if ls := s.d.Speed.Queue().LastSweep; ls != nil {
			e, ok = *ls, true
		}
		if !ok || (p.part == "sweep" && e.Kind != speedtest.KindSweep) {
			if p.part == "sweep" && p.entry == 0 {
				writeJSON(w, r, http.StatusOK, map[string]any{"at": now.UnixMilli(), "sweep": nil})
				return
			}
			writeErr(w, r, http.StatusNotFound, "no such entry")
			return
		}
		j := s.speedEntry(e, names, me, 0, time.Time{})
		writeJSON(w, r, http.StatusOK, map[string]any{"at": now.UnixMilli(), p.part: j})
		return
	}
	if p.part == "summary" && p.row != "" {
		out, err := s.speedOne(r.Context(), p, st, names, now)
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
	configurable := !s.public && s.canControl != nil && s.canControl(r)
	s.runCached(w, r, p.key, func(ctx context.Context) (*cached, error) {
		if p.part == "windows" {
			ts, err := s.d.Speed.SpeedTests(ctx, p.from.Add(-speedtest.SlotCap), p.to)
			if err != nil {
				return nil, err
			}
			return jsonCached(map[string]any{"at": now.UnixMilli(), "windows": speedWindows(ts, p.from)})
		}
		out, err := s.speedSummary(ctx, p, st, names, now)
		if err != nil {
			return nil, err
		}
		out.Configurable = configurable
		return jsonCached(out)
	})
}

// requester is who asks: "" for the owner (local), the client key on the
// share link.
func (s *server) requester(r *http.Request) string {
	if !s.public {
		return ""
	}
	return publicClient(r)
}

// publicClient is a share-link visitor's key: Cf-Connecting-Ip, IPv6
// grouped by /64.
func publicClient(r *http.Request) string {
	k := clientKey(r)
	if i := strings.IndexByte(k, ':'); i >= 0 { // IPv6: the first four groups
		parts := strings.SplitN(k, ":", 5)
		if len(parts) >= 4 {
			return strings.Join(parts[:4], ":") + "::/64"
		}
	}
	return k
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

func (s *server) speedRunning(names speedNames) *speedRunning {
	q := s.d.Speed.Queue()
	if q.Running == nil {
		return nil
	}
	return s.runningOf(*q.Running, names)
}

func (s *server) runningOf(e speedtest.Entry, names speedNames) *speedRunning {
	r := &speedRunning{Entry: e.ID, Kind: e.Kind, Started: e.StartedAt.UnixMilli(), Step: e.Progress.Step,
		Index: e.Progress.Index, Total: e.Progress.Total}
	left := speedtest.EstSweepStep
	if e.Kind == speedtest.KindSweep && e.Progress.Total > 0 {
		left = time.Duration(e.Progress.Total-e.Progress.Index+1) * speedtest.EstSweepStep
	}
	r.ExpectedEnd = s.now().Add(left).UnixMilli()
	if !e.StartedAt.IsZero() && e.Kind != speedtest.KindSweep {
		r.ExpectedEnd = e.StartedAt.Add(speedtest.EstSweepStep).UnixMilli()
	}
	if e.Progress.Server != "" {
		ref := names.ref(e.Progress.Server)
		r.Server = &ref
	}
	if r.Step == "" {
		r.Step = "picking"
	}
	return r
}

func (s *server) speedEntry(e speedtest.Entry, names speedNames, me string, pos int, estStart time.Time) speedEntry {
	j := speedEntry{ID: e.ID, Kind: e.Kind, State: e.State, Position: pos, RequestedAt: e.RequestedAt.UnixMilli(),
		StartedAt: ms(e.StartedAt), FinishedAt: ms(e.FinishedAt)}
	switch {
	case e.Who == speedtest.WhoScheduled:
		j.Who = "scheduled"
	case (e.Who == speedtest.WhoOwner && !s.public) || (e.Who == speedtest.WhoVisitor && s.public && e.Client == me):
		j.Who = "you"
	default:
		j.Who = "someone"
	}
	switch {
	case e.Kind == speedtest.KindSweep:
		j.Label = "All servers"
	case e.Target == "":
		j.Label = "Nearest servers"
	default:
		if t, ok := names.regions[e.Target]; ok {
			j.Label, j.Target = t, e.Target
		} else {
			ref := names.ref(e.Target)
			j.Label, j.Target = ref.Label, ref.ID
		}
	}
	if !estStart.IsZero() {
		j.EstStart = ms(estStart)
		est := speedtest.EstSweepStep
		if e.Kind == speedtest.KindSweep {
			est = time.Duration(s.d.Speed.SweepSize()) * speedtest.EstSweepStep
		}
		j.EstEnd = ms(estStart.Add(est))
	}
	if e.State == speedtest.StateRunning {
		j.Progress = s.runningOf(e, names)
		j.EstEnd = &j.Progress.ExpectedEnd
	}
	for _, x := range e.Results {
		ref, display, provider, _ := names.server(x.Server)
		res := speedSweepResult{Server: ref.ID, Label: ref.Label, Display: display, Provider: provider, Status: x.Status, Direction: x.Direction,
			DownBps: roundP(x.DownBps, 1), UpBps: roundP(x.UpBps, 1), DownShort: x.DownShort, UpShort: x.UpShort, At: ms(x.At)}
		if reg := names.inReg[x.Server]; reg != "" {
			res.Region = &reg
		}
		j.Results = append(j.Results, res)
	}
	return j
}

func (s *server) speedQueue(names speedNames, me string, now time.Time) speedQueueJSON {
	q := s.d.Speed.Queue()
	out := speedQueueJSON{At: now.UnixMilli(), Waiting: []speedEntry{}}
	at := now
	if q.Running != nil {
		e := s.speedEntry(*q.Running, names, me, 0, time.Time{})
		out.Running = &e
		if e.EstEnd != nil {
			at = time.UnixMilli(*e.EstEnd)
		}
	}
	for i, w := range q.Waiting {
		e := s.speedEntry(w, names, me, i+1, at)
		out.Waiting = append(out.Waiting, e)
		if e.EstEnd != nil {
			at = time.UnixMilli(*e.EstEnd)
		}
	}
	if q.LastSweep != nil {
		e := s.speedEntry(*q.LastSweep, names, me, 0, time.Time{})
		out.LastSweep = &e
	}
	return out
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func (s *server) speedSummary(ctx context.Context, p speedParams, st speedtest.State, names speedNames, now time.Time) (*speedJSON, error) {
	cat := s.d.Speed.Catalog()
	out := &speedJSON{At: now.UnixMilli(), TZ: p.tz, Days: p.days, ExcludeFallback: p.excludeFallback, Mode: st.Mode, Modes: st.Modes,
		Enabled: st.Mode != speedtest.ModeOff, SetByFlag: st.SetByFlag, Extra: append([]string{}, st.Extra...),
		IntervalS: int64(st.Interval / time.Second), SpacingS: int64(st.Spacing / time.Second),
		PlannedShare: math.Round(st.PlannedShare*1000) / 1000, Warning: strPtr(st.Warning),
		MaskedShare24h: math.Round(s.d.Speed.MaskedShare(24*time.Hour)*1000) / 1000,
		NextAt:         ms(st.Next), NextRow: strPtr(st.NextRow), Running: s.speedRunning(names),
		Rows: []speedRow{}, PerRegion: []speedRow{}, PerServer: []speedRow{}, Servers: []speedServerJSON{}, Regions: []speedRegionJSON{},
		Connection: speedConnection{Type: "unknown", Source: "detected"}}
	if !s.public {
		month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
		out.BytesMonth, _ = s.d.Speed.BytesSince(ctx, month)
	}
	for _, sv := range st.Servers {
		ref, display, _, _ := names.server(sv.ID)
		j := speedServerJSON{ID: ref.ID, Label: ref.Label, Display: display, Provider: sv.Provider, City: sv.City, Country: sv.Country,
			Geo: sv.Geo, Area: profile.GeoTitles[sv.Geo], Metro: sv.Metro, RTTms: roundP(sv.RTTms, 10), Picked: sv.Picked, Custom: sv.Custom,
			Single: sv.SinglePort()}
		if reg := names.inReg[sv.ID]; reg != "" {
			j.Region = &reg
		}
		if sv.CapacityMbps > 0 {
			c := sv.CapacityMbps
			j.Capacity = &c
		}
		if sv.Custom {
			j.Provider, j.Metro = "Your server", ""
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
	for _, r := range st.Regions {
		out.Regions = append(out.Regions, speedRegionJSON{ID: r.ID, Title: r.Title, Servers: append([]string{}, r.Servers...),
			Nearby: append([]string{}, r.Nearby...), Default: r.Default, Providers: r.Providers(cat)})
	}
	from := now.Add(-time.Duration(p.days) * 24 * time.Hour)
	all, err := s.d.Speed.SpeedTests(ctx, now.Add(-speedtest.PatternSpan), now)
	if err != nil {
		return nil, err
	}
	tz := time.Duration(p.tz) * time.Minute
	byRegion, byServer := map[string][]model.SpeedTest{}, map[string][]model.SpeedTest{}
	var span, last24 []model.SpeedTest
	var avgBytes []float64
	for _, t := range all {
		if !t.Start.Before(now.Add(-24 * time.Hour)) {
			last24 = append(last24, t)
		}
		if t.Bytes > 0 {
			avgBytes = append(avgBytes, float64(t.Bytes))
		}
		if t.Start.Before(from) {
			continue
		}
		span = append(span, t)
		byServer[t.Server] = append(byServer[t.Server], t)
		if t.Region != "" && !(p.excludeFallback && t.Fallback) {
			byRegion[t.Region] = append(byRegion[t.Region], t)
		}
	}
	perTest := float64(defaultTestBytes)
	if len(avgBytes) > 0 {
		perTest = callq.Median(avgBytes)
	}
	for _, pl := range st.Plans {
		out.Plans = append(out.Plans, speedPlan{Mode: pl.Mode, TestsPerHour: math.Round(pl.TestsPerHour*100) / 100,
			Share: math.Round(pl.Share*1000) / 1000, EstGBMonth: math.Round(pl.TestsPerHour*24*30*perTest/1e9*10) / 10})
	}
	scheduled := map[string]speedtest.Row{}
	for _, r := range st.Rows {
		scheduled[r.ID] = r
	}
	note := func(row *speedRow, title string) {
		var n, fb int
		for _, t := range last24 {
			if t.Region == row.ID && t.Outcome == model.SpeedOK && speedtest.Scheduled(t) {
				n++
				if t.Fallback {
					fb++
				}
			}
		}
		if n == 0 {
			return
		}
		share := math.Round(float64(fb)/float64(n)*100) / 100
		row.FallbackShare24h = &share
		if share > fallbackNote {
			x := fmt.Sprintf("%s has run on its fallback servers most of the last 24 hours: its first choice was busy or not answering.", title)
			row.Note = &x
		}
	}
	for _, r := range st.Regions {
		sr, isSched := scheduled[r.ID]
		if !r.Default && !isSched && len(byRegion[r.ID]) == 0 {
			continue
		}
		row := speedRowOf(r.ID, "region", r.Title, byRegion[r.ID], tz)
		row.Servers = append(slices.Clone(r.Servers), r.Nearby...)
		row.Scheduled, row.Why = isSched, sr.Why
		note(&row, r.Title)
		out.PerRegion = append(out.PerRegion, row)
	}
	for _, sv := range st.Servers {
		ts := byServer[sv.ID]
		sr, isSched := scheduled[sv.ID]
		if len(ts) == 0 && !isSched {
			continue
		}
		ref := names.ref(sv.ID)
		row := speedRowOf(ref.ID, "server", ref.Label, ts, tz)
		row.Servers = []string{ref.ID}
		row.Scheduled, row.Why = isSched, sr.Why
		out.PerServer = append(out.PerServer, row)
	}
	for _, r := range st.Rows {
		var src []speedRow
		if r.Kind == "region" {
			src = out.PerRegion
		} else {
			src = out.PerServer
		}
		id := r.ID
		if r.Kind == "server" {
			id = names.ref(r.ID).ID
		}
		if i := slices.IndexFunc(src, func(x speedRow) bool { return x.ID == id }); i >= 0 {
			out.Rows = append(out.Rows, src[i])
		}
	}
	latest := all
	if len(latest) == 0 {
		if latest, err = s.d.Speed.SpeedTests(ctx, now.Add(-MaxRange), now); err != nil {
			return nil, err
		}
	}
	if len(latest) > 0 {
		l := names.test(latest[len(latest)-1])
		out.Latest = &l
	}
	for i := len(latest) - 1; i >= 0; i-- { // the newest test that knows
		if x := speedtest.ParseExtra(latest[i].Extra); x.Connection != "" {
			out.Connection.Type = x.Connection
			break
		}
	}
	out.Pattern = speedtest.PooledPattern(all, tz)
	out.Finding = speedtest.FindFinding(speedtest.FindingInput{Tests: all, TZ: tz, Catalog: cat, EdgeIdle: s.edgeIdleByHour(ctx, now, tz)})
	out.Coverage = speedCoverage(span, tz, cat)
	out.UnderLoad = speedUnderLoadOf(span)
	if s.public { // the share link's wording: a visitor's line is not "yours"
		out.Finding.Text = out.Finding.TextPublic
		if out.Pattern != nil {
			out.Pattern.Text = publicWording.Replace(out.Pattern.Text)
		}
		for i, x := range out.UnderLoad.Text {
			out.UnderLoad.Text[i] = publicWording.Replace(x)
		}
		for _, rows := range [][]speedRow{out.PerRegion, out.PerServer} {
			for i := range rows {
				rows[i].publicWording()
			}
		}
	}
	return out, nil
}

// publicWording rewords a row's dips for the share link (the rows in
// Rows share the patterns with PerRegion and PerServer).
func (r *speedRow) publicWording() {
	for _, p := range []*speedtest.Pattern{r.Dip, r.UpDip} {
		if p != nil {
			p.Text = publicWording.Replace(p.Text)
		}
	}
}

var publicWording = strings.NewReplacer("your daytime", "the daytime", "your usual", "the usual", "your provider", "the internet provider",
	"Your router's", "The router's", "your home", "the home")

func speedRowOf(id, kind, label string, ts []model.SpeedTest, tz time.Duration) speedRow {
	row := speedRow{ID: id, Kind: kind, Label: label, Stats: speedtest.Summarize(ts, tz)}
	if row.Stats.LatestAt != nil {
		row.LatestAt = ms(*row.Stats.LatestAt)
	}
	return row
}

// speedCoverage is per local hour: scheduled ok tests and the providers
// that answered (a pattern's hour needs ≥ 2 providers).
func speedCoverage(ts []model.SpeedTest, tz time.Duration, cat *speedtest.Catalog) []speedHour {
	out := make([]speedHour, 24)
	provs := make([]map[string]bool, 24)
	for h := range 24 {
		out[h].Hour, provs[h] = h, map[string]bool{}
	}
	for _, t := range ts {
		if !speedtest.Scheduled(t) || t.Outcome != model.SpeedOK {
			continue
		}
		h := t.Start.Add(tz).UTC().Hour()
		out[h].N++
		if sv, ok := cat.Server(t.Server); ok {
			provs[h][sv.Provider] = true
		} else {
			provs[h]["custom"] = true
		}
	}
	for h := range 24 {
		out[h].Providers = len(provs[h])
	}
	return out
}

// speedUnderLoadOf summarizes latency under load over the span: per layer
// the median of the tests' idle and per-direction p50, and the loss; with
// one sentence per layer.
func speedUnderLoadOf(ts []model.SpeedTest) speedUnderLoad {
	type acc struct {
		idle, down, up             []float64
		sentD, lostD, sentU, lostU int
	}
	by := map[string]*acc{}
	for _, t := range ts {
		if t.Outcome != model.SpeedOK {
			continue
		}
		for layer, l := range speedtest.ParseExtra(t.Extra).Latency {
			a := by[layer]
			if a == nil {
				a = &acc{}
				by[layer] = a
			}
			if l.IdleMs != nil {
				a.idle = append(a.idle, *l.IdleMs)
			}
			if l.Down != nil {
				a.down = append(a.down, l.Down.P50)
				a.sentD, a.lostD = a.sentD+l.Down.Sent, a.lostD+l.Down.Lost
			}
			if l.Up != nil {
				a.up = append(a.up, l.Up.P50)
				a.sentU, a.lostU = a.sentU+l.Up.Sent, a.lostU+l.Up.Lost
			}
		}
	}
	out := speedUnderLoad{Layers: []speedLoad{}, Text: []string{}}
	med := func(v []float64) *float64 {
		if len(v) == 0 {
			return nil
		}
		x := math.Round(callq.Median(v)*10) / 10
		return &x
	}
	share := func(lost, sent int) float64 {
		if sent == 0 {
			return 0
		}
		return math.Round(float64(lost)/float64(sent)*1000) / 1000
	}
	var edge, gw *speedLoad
	for _, layer := range []string{speedtest.LayerEdge, speedtest.LayerGateway} {
		a := by[layer]
		if a == nil {
			continue
		}
		l := speedLoad{Layer: layer, IdleMs: med(a.idle), DownMs: med(a.down), UpMs: med(a.up), LossDown: share(a.lostD, a.sentD), LossUp: share(a.lostU, a.sentU)}
		out.Layers = append(out.Layers, l)
		if layer == speedtest.LayerEdge {
			edge = &out.Layers[len(out.Layers)-1]
		} else {
			gw = &out.Layers[len(out.Layers)-1]
		}
	}
	effect := func(rise float64) string {
		switch {
		case rise < 30:
			return "barely changes"
		case rise <= 100:
			return "noticeable on calls"
		}
		return "enough to break up calls while the line is busy"
	}
	if edge != nil && edge.IdleMs != nil {
		for _, d := range []struct {
			v    *float64
			what string
		}{{edge.DownMs, "downloading"}, {edge.UpMs, "uploading"}} {
			if d.v != nil {
				rise := *d.v - *edge.IdleMs
				out.Text = append(out.Text, fmt.Sprintf("Delay through your provider rises from %.0f ms to %.0f ms while %s (%+.0f ms): %s.",
					*edge.IdleMs, *d.v, d.what, rise, effect(rise)))
			}
		}
	}
	if gw != nil && gw.IdleMs != nil && gw.DownMs != nil {
		gr := max(*gw.DownMs, ptrOr(gw.UpMs, 0)) - *gw.IdleMs
		er := 0.0
		if edge != nil && edge.IdleMs != nil {
			er = max(ptrOr(edge.DownMs, 0), ptrOr(edge.UpMs, 0)) - *edge.IdleMs
		}
		if er > 5 && gr >= 0.7*er {
			out.Text = append(out.Text, "Most of that delay is inside your home: often Wi-Fi.")
		} else {
			out.Text = append(out.Text, fmt.Sprintf("Your router's delay rises from %.0f to %.0f ms: your home network keeps up.", *gw.IdleMs, *gw.IdleMs+gr))
		}
	}
	return out
}

func ptrOr(p *float64, d float64) float64 {
	if p == nil {
		return d
	}
	return *p
}

// edgeIdleByHour is the ISP edge's median idle RTT per local hour over the
// last 14 days (the access-network control of the finding).
func (s *server) edgeIdleByHour(ctx context.Context, now time.Time, tz time.Duration) [24]float64 {
	var out [24]float64
	for h := range out {
		out[h] = math.NaN()
	}
	p := s.d.Profile()
	if p == nil {
		return out
	}
	var key model.SeriesKey
	for _, t := range p.Targets {
		if t.Layer == model.LayerEdge {
			key = model.SeriesKey{Target: t.Name, Kind: model.KindICMP}
		}
	}
	if key.Target == "" {
		return out
	}
	from := now.Add(-14 * 24 * time.Hour).Truncate(time.Hour)
	res, err := s.d.Store.Panel(ctx, store.PanelQuery{Keys: []model.SeriesKey{key}, From: from, To: now, MaxPoints: 14*24 + 1})
	if err != nil || len(res.Series) == 0 {
		return out
	}
	var by [24][]float64
	c := res.Series[0]
	for i := range c.N {
		if c.N[i] == 0 || math.IsNaN(float64(c.Mean[i])) {
			continue
		}
		h := res.Start.Add(time.Duration(i) * res.Step).Add(tz).UTC().Hour()
		by[h] = append(by[h], float64(c.Mean[i]))
	}
	for h := range 24 {
		if len(by[h]) >= 3 {
			out[h] = callq.Median(by[h])
		}
	}
	return out
}

var errNoSpeedID = errors.New("no such server or region")

// speedOneJSON is api/speed?row=<id> (or server=, region=): one row's
// stats, every test in the span (oldest first, at most speedTestsCap) and
// its windows.
type speedOneJSON struct {
	At              int64           `json:"at"`
	TZ              int             `json:"tz"`
	Days            int             `json:"days"`
	ExcludeFallback bool            `json:"exclude_fallback"`
	Kind            string          `json:"kind"`
	Row             speedRow        `json:"row"`
	Tests           []speedTestJSON `json:"tests"`
	Windows         []speedWindow   `json:"windows"`
}

func (s *server) speedOne(ctx context.Context, p speedParams, st speedtest.State, names speedNames, now time.Time) (*speedOneJSON, error) {
	from := now.Add(-time.Duration(p.days) * 24 * time.Hour)
	out := &speedOneJSON{At: now.UnixMilli(), TZ: p.tz, Days: p.days, ExcludeFallback: p.excludeFallback, Tests: []speedTestJSON{}}
	var keep func(model.SpeedTest) bool
	var label, id string
	var servers []string
	if i := slices.IndexFunc(st.Regions, func(r speedtest.Region) bool { return r.ID == p.row }); i >= 0 {
		r := st.Regions[i]
		out.Kind, label, id = "region", r.Title, r.ID
		servers = append(slices.Clone(r.Servers), r.Nearby...)
		keep = func(t model.SpeedTest) bool { return t.Region == r.ID && !(p.excludeFallback && t.Fallback) }
	} else {
		local := names.resolve(p.row)
		i := slices.IndexFunc(st.Servers, func(sv speedtest.ServerState) bool { return sv.ID == local })
		if i < 0 || (s.public && local == p.row && names.custom[local] > 0) { // own servers only by public ID there
			return nil, errNoSpeedID
		}
		ref := names.ref(local)
		out.Kind, label, id, servers = "server", ref.Label, ref.ID, []string{ref.ID}
		keep = func(t model.SpeedTest) bool { return t.Server == local }
	}
	all, err := s.d.Speed.SpeedTests(ctx, from, now)
	if err != nil {
		return nil, err
	}
	var ts []model.SpeedTest
	for _, t := range all {
		if keep(t) {
			ts = append(ts, t)
		}
	}
	for _, t := range ts[max(0, len(ts)-speedTestsCap):] {
		out.Tests = append(out.Tests, names.test(t))
	}
	out.Row = speedRowOf(id, out.Kind, label, ts, time.Duration(p.tz)*time.Minute)
	out.Row.Servers = servers
	for _, r := range st.Rows {
		if r.ID == id || names.ref(r.ID).ID == id {
			out.Row.Scheduled, out.Row.Why = true, r.Why
		}
	}
	out.Windows = speedWindows(ts, from)
	if s.public {
		out.Row.publicWording()
	}
	return out, nil
}

// speedHome is Home's speed line (nil: nothing on Home).
func (s *server) speedHome(ctx context.Context, now time.Time, tz time.Duration, voicePublic bool) *speedtest.HomeSpeed {
	if s.d.Speed == nil || s.d.Speed.State().Mode == speedtest.ModeOff {
		return nil
	}
	ts, err := s.d.Speed.SpeedTests(ctx, now.Add(-speedtest.PatternSpan), now)
	if err != nil {
		return nil
	}
	f := speedtest.FindFinding(speedtest.FindingInput{Tests: ts, TZ: tz, Catalog: s.d.Speed.Catalog(), EdgeIdle: s.edgeIdleByHour(ctx, now, tz)})
	line := speedtest.HomeLine(f, ts, tz, now)
	if line != nil && voicePublic {
		line.Text = strings.ReplaceAll(line.Text, "your daytime", "the daytime")
	}
	return line
}

// ---------- POST ----------

type speedPostJSON struct {
	Entry     speedEntry `json:"entry"`
	Duplicate bool       `json:"duplicate,omitempty"`
}

// enqueueSpeed queues a test or sweep for who (client: the visitor key) and
// answers 202 with the entry, 200 with an equivalent waiting one, or 429
// queue_full.
func (s *server) enqueueSpeed(w http.ResponseWriter, r *http.Request, kind, id, who, client string) {
	names := newSpeedNames(s.d.Speed.State(), s.d.Speed.Catalog(), s.public)
	if id != "" {
		id = names.resolve(id)
	}
	e, dup, err := s.d.Speed.Enqueue(kind, id, who, client)
	switch {
	case errors.Is(err, speedtest.ErrQueueFull):
		w.Header().Set("Retry-After", "60")
		writeJSON(w, r, http.StatusTooManyRequests, map[string]any{"error": err.Error(), "code": "queue_full", "retry_after": 60,
			"queue": s.speedQueue(names, client, s.now())})
		return
	case errors.Is(err, speedtest.ErrUnknown):
		writeErr(w, r, http.StatusNotFound, err.Error())
		return
	case err != nil:
		writeErr(w, r, http.StatusServiceUnavailable, "could not queue the test")
		return
	}
	pos := 0
	at := time.Time{}
	q := s.speedQueue(names, client, s.now())
	for _, x := range q.Waiting {
		if x.ID == e.ID {
			pos = x.Position
			if x.EstStart != nil {
				at = time.UnixMilli(*x.EstStart)
			}
		}
	}
	code := http.StatusAccepted
	if dup {
		code = http.StatusOK
	}
	writeJSON(w, r, code, speedPostJSON{Entry: s.speedEntry(e, names, client, pos, at), Duplicate: dup})
}

// readSpeedBody decodes a small JSON body (empty is fine) into v.
func readSpeedBody(r *http.Request, v any) error {
	b, err := io.ReadAll(io.LimitReader(r.Body, 1<<10))
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return nil
	}
	d := json.NewDecoder(strings.NewReader(string(b)))
	d.DisallowUnknownFields()
	return d.Decode(v)
}

// serveSpeedPOST answers the owner's (local) speed routes:
//
//	POST   api/speed/run      {"server": "<server or region id>"} (optional)
//	POST   api/speed/sweep
//	POST   api/speed/config   {"mode": "...", "extra": [...]}
//	DELETE api/speed/queue/<id>
//	POST   api/speed/queue/clear?who=public
func (h *localHandler) serveSpeedPOST(w http.ResponseWriter, r *http.Request, rest string) {
	if h.d.Speed == nil {
		notFound(w, r)
		return
	}
	switch {
	case rest == "run" || rest == "sweep" || rest == "config" || rest == "queue/clear":
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			writeErr(w, r, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
	case strings.HasPrefix(rest, "queue/"):
		if r.Method != http.MethodDelete {
			w.Header().Set("Allow", "DELETE")
			writeErr(w, r, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
	default:
		notFound(w, r)
		return
	}
	params := []string{}
	if rest == "queue/clear" {
		params = []string{"who"}
	}
	if err := checkParams(r.URL.Query(), params...); err != nil {
		writeErr(w, r, http.StatusBadRequest, err.Error())
		return
	}
	if !h.controlAllowed(w, r, "speed tests") {
		return
	}
	switch rest {
	case "run":
		var body struct {
			Server string `json:"server"`
		}
		if err := readSpeedBody(r, &body); err != nil {
			writeErr(w, r, http.StatusBadRequest, `want {"server": "<id>"} or no body`)
			return
		}
		h.enqueueSpeed(w, r, speedtest.KindTest, body.Server, speedtest.WhoOwner, "")
	case "sweep":
		if err := readSpeedBody(r, &struct{}{}); err != nil {
			writeErr(w, r, http.StatusBadRequest, "no body expected")
			return
		}
		h.enqueueSpeed(w, r, speedtest.KindSweep, "", speedtest.WhoOwner, "")
	case "config":
		var body struct {
			Mode  *string  `json:"mode"`
			Extra []string `json:"extra"`
		}
		if err := readSpeedBody(r, &body); err != nil || body.Mode == nil {
			writeErr(w, r, http.StatusBadRequest, `want {"mode": "off", "regions", "nearby" or "custom", "extra": [region ids]}`)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), notesTimeout)
		defer cancel()
		next, err := h.d.Speed.SetMode(ctx, speedtest.Mode(*body.Mode), body.Extra)
		if errors.Is(err, speedtest.ErrMode) {
			writeErr(w, r, http.StatusBadRequest, err.Error())
			return
		}
		if err != nil {
			h.storeErr(w, r, "save the speed test setting", err)
			return
		}
		writeJSON(w, r, http.StatusOK, map[string]any{"mode": *body.Mode, "next_at": ms(next)})
	case "queue/clear":
		who := r.URL.Query().Get("who")
		switch who {
		case "public":
			who = speedtest.WhoVisitor
		case "", "all":
			who = ""
		default:
			writeErr(w, r, http.StatusBadRequest, "who: public or all")
			return
		}
		writeJSON(w, r, http.StatusOK, map[string]any{"cancelled": h.d.Speed.ClearQueue(who)})
	default:
		id, err := strconv.ParseInt(strings.TrimPrefix(rest, "queue/"), 10, 64)
		if err != nil || id <= 0 {
			writeErr(w, r, http.StatusNotFound, "not found")
			return
		}
		if err := h.d.Speed.Cancel(id); err != nil {
			writeErr(w, r, http.StatusNotFound, err.Error())
			return
		}
		writeJSON(w, r, http.StatusOK, map[string]any{"cancelled": id})
	}
}

// serveSpeedPublicPOST answers the share link's Test now and sweep: no
// admin token, but same origin, the page's token, a per-client rate and
// the queue's visitor limits.
func (h *publicHandler) serveSpeedPublicPOST(w http.ResponseWriter, r *http.Request, rest string) {
	if h.d.Speed == nil || (rest != "run" && rest != "sweep") {
		notFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeErr(w, r, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if len(r.URL.RawQuery) > 0 {
		writeErr(w, r, http.StatusBadRequest, "no parameters allowed")
		return
	}
	if !sameOrigin(r) || !tokenEqual(r.Header.Get(HeaderCSRF), h.token) {
		writeErr(w, r, http.StatusForbidden, "this request must come from the shared page")
		return
	}
	client := publicClient(r)
	if !h.speedLim.allow(client, h.now()) {
		w.Header().Set("Retry-After", "10")
		writeJSON(w, r, http.StatusTooManyRequests, map[string]any{"error": "too many requests", "code": "rate_limited", "retry_after": 10})
		return
	}
	var body struct {
		Server string `json:"server"`
	}
	if err := readSpeedBody(r, &body); err != nil || (rest == "sweep" && body.Server != "") {
		writeErr(w, r, http.StatusBadRequest, `want {"server": "<id>"} or no body`)
		return
	}
	kind := speedtest.KindTest
	if rest == "sweep" {
		kind = speedtest.KindSweep
	}
	h.enqueueSpeed(w, r, kind, body.Server, speedtest.WhoVisitor, client)
}
