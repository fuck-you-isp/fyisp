package web

import (
	"cmp"
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
	// Queue is the speed-test queue (Test now, sweeps, schedule slots).
	Queue() *speedtest.Queue
	SetSkip(ctx context.Context, ids []string) error
	TestShare(d time.Duration) float64
	Catalog() *speedtest.Catalog
	BytesSince(ctx context.Context, t time.Time) (int64, error)
}

// Speed API limits.
const (
	// fallbackNote is the share of a region's tests on a fallback server
	// (last 24 h) above which the row gets a note.
	fallbackNote     = 0.5
	speedTestsCap    = 5000 // tests in one row view
	defaultTestBytes = 150 << 20
)

// ---------- JSON ----------

type speedRef struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

type speedServerJSON struct {
	ID       string   `json:"id"`
	Label    string   `json:"label"`
	Display  string   `json:"display"` // "UK · Clouvider, London"
	Provider string   `json:"provider"`
	City     string   `json:"city"`
	Country  string   `json:"country"`
	Geo      string   `json:"geo"`
	Area     string   `json:"area"`   // the geo's title, e.g. "Europe"
	Region   *string  `json:"region"` // the test region it belongs to, if any
	RTTms    *float64 `json:"rtt_ms"`
	Picked   bool     `json:"picked"`
	Custom   bool     `json:"custom"`
	Capacity *float64 `json:"capacity_mbps"`
	// Skipped: never tested (SkipLocked: by --speedtest-skip, the toggle
	// can't undo it); RestUntil: resting after being busy several times.
	Skipped    bool   `json:"skipped"`
	SkipLocked bool   `json:"skip_locked"`
	RestUntil  *int64 `json:"rest_until"`
	Host       string `json:"host,omitempty"`
	Ports      string `json:"ports,omitempty"`
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
	// Learning: tests but no scores yet (a server needs BaselineMin
	// first-choice tests over 14 days for its baseline).
	Learning bool `json:"learning"`
}

type speedWindow struct {
	Start  int64  `json:"start"`
	End    int64  `json:"end"`
	Origin string `json:"origin"` // scheduled | owner
}

// speedTestJSON is one test.
type speedTestJSON struct {
	ID            int64    `json:"id"`
	Start         int64    `json:"start"`
	End           int64    `json:"end"`
	Trigger       string   `json:"trigger"`   // schedule, manual or sweep
	OnDemand      bool     `json:"on_demand"` // Test now or a sweep: left out of patterns and rows
	Outcome       string   `json:"outcome"`
	Region        *string  `json:"region"`
	Server        string   `json:"server"`
	Label         string   `json:"label"`
	Display       string   `json:"display"`
	Provider      string   `json:"provider"`
	City          string   `json:"city"`
	Fallback      bool     `json:"fallback"`
	FallbackLevel string   `json:"fallback_level"`
	Port          int      `json:"port,omitempty"`
	Attempts      int      `json:"attempts"`
	DownBps       *float64 `json:"down_bps"`
	UpBps         *float64 `json:"up_bps"`
	// DownLowerBound and UpLowerBound: still speeding up at the time cap,
	// so the speed was at least this. It counts as at least normal in the
	// patterns, never slow.
	DownLowerBound bool     `json:"down_lower_bound,omitempty"`
	UpLowerBound   bool     `json:"up_lower_bound,omitempty"`
	CancelledBy    string   `json:"cancelled_by,omitempty"`    // a cancelled test: "owner" when the owner stopped it
	DownLBCounted  bool     `json:"down_lb_counted,omitempty"` // a lower bound that reached normal: counted as normal
	UpLBCounted    bool     `json:"up_lb_counted,omitempty"`
	DownSecs       float64  `json:"down_s"`
	UpSecs         float64  `json:"up_s"`
	CappedByServer bool     `json:"capped_by_server"`
	ScoreDown      *float64 `json:"score"`
	ScoreUp        *float64 `json:"up_score"`
	Bytes          int64    `json:"bytes"`
	DownRetrans    *int64   `json:"down_retrans"`
	UpRetrans      *int64   `json:"up_retrans"`
	// Latency under load at the ISP edge and the gateway; Conn the
	// connection type; Handoff (local only) the first network past the ISP.
	Latency *model.SpeedLatency `json:"latency"`
	Conn    string              `json:"conn"`
	Handoff *model.Handoff      `json:"handoff,omitempty"`
}

// speedRunning is the test running now (summary and part=running): the
// queue's running entry in short.
type speedRunning struct {
	Entry     uint64    `json:"entry"`
	Kind      string    `json:"kind"` // single | sweep | scheduled
	Started   int64     `json:"started"`
	Direction string    `json:"direction"` // picking, download, upload
	Server    *speedRef `json:"server"`
	Step      int       `json:"step"` // a sweep: the server under test, 1-based, of steps
	Steps     int       `json:"steps"`
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

// speedLoad is one layer's medians of the tests' p50s.
type speedLoad struct {
	IdleMs, DownMs, UpMs *float64
}

type speedUnderLoad struct {
	Text []string `json:"text"`
}

// speedJSON is api/speed?part=summary.
type speedJSON struct {
	At              int64              `json:"at"`
	TZ              int                `json:"tz"`            // minutes east of UTC now
	TZName          string             `json:"tz_name"`       // the IANA zone used ("" for a fixed offset; the server's when tz is not sent)
	SweepBytes      int64              `json:"sweep_bytes"`   // "Test every server": its estimated data (see sweepBytes)
	SweepServers    int                `json:"sweep_servers"` // the servers it tests (skipped ones left out)
	Days            int                `json:"days"`
	ExcludeFallback bool               `json:"exclude_fallback"`
	Mode            speedtest.Mode     `json:"mode"`
	Modes           []speedtest.Mode   `json:"modes"`
	Enabled         bool               `json:"enabled"`
	SetByFlag       bool               `json:"set_by_flag"`
	Configurable    bool               `json:"configurable"`
	IntervalS       int64              `json:"interval_s"`
	SpacingS        int64              `json:"spacing_s"`
	PlannedShare    float64            `json:"planned_share"`
	Warning         *string            `json:"warning"`
	Plans           []speedPlan        `json:"plans"`
	TestShare24h    float64            `json:"test_share_24h"`
	BytesMonth      int64              `json:"bytes_month"`
	Bytes24h        int64              `json:"bytes_24h"`
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

// speedEdgeHour is one local hour of day of the ISP edge's idle RTT: the
// median over days of that hour's mean RTT, and the loss over them.
type speedEdgeHour struct {
	Hour  int
	N     int  // days with samples in that hour
	RTTms *num // nil when N is 0
	Loss  *num // nil when N is 0
}

type speedConnection struct {
	Type   string `json:"type"`   // wired, wifi, unknown
	Source string `json:"source"` // detected
}

// ---------- names ----------

// speedNames maps server IDs to labels.
type speedNames struct {
	cat     *speedtest.Catalog
	custom  map[string]int // own servers (host:port): their number
	regions map[string]string
	inReg   map[string]string // server -> its (first) region
}

func newSpeedNames(st speedtest.State, cat *speedtest.Catalog) speedNames {
	n := speedNames{cat: cat, custom: map[string]int{}, regions: map[string]string{}, inReg: map[string]string{}}
	for _, sv := range st.Servers {
		if sv.Custom {
			n.custom[sv.ID] = len(n.custom) + 1
		}
	}
	for _, r := range st.Regions {
		n.regions[r.ID] = r.Title
	}
	if cat != nil { // every catalog server's one region (catalog.RegionOf)
		for _, sv := range cat.Servers {
			if r := cat.RegionOf(sv.ID); r != "" {
				n.inReg[sv.ID] = r
			}
		}
	}
	return n
}

// server is a server's identity: id, label, display, provider and city.
func (n speedNames) server(id string) (ref speedRef, display, provider, city string) {
	if k, ok := n.custom[id]; ok {
		label := "Your server"
		if len(n.custom) > 1 {
			label += " " + strconv.Itoa(k)
		}
		return speedRef{ID: id, Label: label}, label, "Your server", ""
	}
	if sv, ok := n.cat.Server(id); ok {
		display = sv.Provider + ", " + sv.City
		if r := n.inReg[id]; r != "" {
			display = n.regions[r] + " · " + display
		}
		return speedRef{ID: id, Label: sv.Label}, display, sv.Provider, sv.City
	}
	return speedRef{ID: id, Label: id}, id, "", ""
}

func (n speedNames) ref(id string) speedRef { r, _, _, _ := n.server(id); return r }

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
	j := speedTestJSON{ID: t.ID, Start: t.Start.UnixMilli(), End: t.End.UnixMilli(), Trigger: t.Trigger, OnDemand: t.OnDemand(),
		Outcome: t.Outcome, Server: ref.ID, Label: ref.Label, Display: display, Provider: provider, City: city,
		Fallback: t.Fallback, FallbackLevel: t.FallbackLevel, Attempts: t.Attempts,
		DownBps: roundP(t.DownBps, 1), UpBps: roundP(t.UpBps, 1),
		DownSecs: t.DownSecs, UpSecs: t.UpSecs, CappedByServer: t.CappedByServer, ScoreDown: t.ScoreDown, ScoreUp: t.ScoreUp,
		Bytes: t.Bytes, DownRetrans: t.DownRetrans, UpRetrans: t.UpRetrans}
	j.DownLowerBound, j.UpLowerBound = speedtest.LowerBounds(&t)
	if t.Outcome == model.SpeedCancelled {
		j.CancelledBy = speedtest.ParseExtra(t.Extra).CancelledBy
	}
	j.DownLBCounted = speedtest.LowerBoundCounted(t.ScoreDown, j.DownLowerBound)
	j.UpLBCounted = speedtest.LowerBoundCounted(t.ScoreUp, j.UpLowerBound)
	if j.FallbackLevel == "" {
		j.FallbackLevel = speedtest.FallbackNone
	}
	if t.Region != "" {
		r := t.Region
		j.Region = &r
	}
	j.Latency, j.Conn = t.Latency, cmp.Or(t.Conn, model.ConnUnknown)
	j.Port, j.Handoff = t.Port, t.Handoff
	return j
}

// ---------- params ----------

type speedParams struct {
	part            string
	days, tz        int
	zone            zoneParam
	excludeFallback bool
	row             string
	from, to        time.Time
	entry           uint64 // part=entry
}

func parseSpeed(q url.Values, now time.Time) (speedParams, error) {
	p := speedParams{part: q.Get("part"), days: 7}
	if err := checkParams(q, "part", "days", "tz", "from", "to", "exclude_fallback", "server", "region", "row", "id"); err != nil {
		return p, err
	}
	if q.Has("id") && p.part != "entry" {
		return p, badReq("id is for part=entry")
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
		z, err := parseZone(q.Get("tz"), now)
		if err != nil {
			return err
		}
		p.zone, p.tz = z, z.min
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
		var err error
		if p.from, err = parseTime(fs, now); err != nil {
			return p, err
		}
		if p.to, err = parseTime(ts, now); err != nil {
			return p, err
		}
		if !p.from.Before(p.to) {
			return p, badReq("from must be before to")
		}
		if p.to.Sub(p.from) > MaxRange {
			return p, badReq("range longer than %d days", int(MaxRange/(24*time.Hour)))
		}
	case "running":
		if err := only(p.part, "days", "tz", "exclude_fallback", "server", "region", "row", "from", "to", "id"); err != nil {
			return p, err
		}
	case "queue", "sweep", "entry":
		if err := only("summary or windows", "days", "tz", "exclude_fallback", "server", "region", "row", "from", "to"); err != nil {
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
	switch p.part {
	case "queue", "entry", "sweep":
		s.serveSpeedQueue(w, r, p, now)
		return
	}
	st := s.d.Speed.State()
	names := newSpeedNames(st, s.d.Speed.Catalog())
	if p.part == "running" {
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, r, http.StatusOK, map[string]any{"at": now.UnixMilli(), "running": s.speedRunning(names)})
		return
	}
	if p.part == "summary" && p.row != "" {
		if !rowKnown(p.row, st) {
			writeErr(w, r, http.StatusNotFound, errNoSpeedID.Error())
			return
		}
		s.runCached(w, r, "speed", func(ctx context.Context) (*cached, error) {
			out, err := s.speedOne(ctx, p, st, names, now)
			if err != nil {
				return nil, err
			}
			return jsonCached(out)
		})
		return
	}
	configurable := s.canControlSpeed != nil && s.canControlSpeed(r)
	s.runCached(w, r, "speed", func(ctx context.Context) (*cached, error) {
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

// speedWindows are the tests' windows from from.
func speedWindows(ts []model.SpeedTest, from time.Time) []speedWindow {
	out := []speedWindow{}
	for _, t := range ts {
		if !t.End.Before(from) && t.End.After(t.Start) {
			out = append(out, speedWindow{Start: t.Start.UnixMilli(), End: t.End.UnixMilli(), Origin: t.Origin()})
		}
	}
	return out
}

func (s *server) speedRunning(names speedNames) *speedRunning {
	e := s.d.Speed.Queue().State().Running
	if e == nil {
		return nil
	}
	r := &speedRunning{Entry: e.ID, Kind: string(e.Kind), Started: e.Started.UnixMilli(),
		Direction: cmp.Or(e.Direction, "picking"), Step: e.Step, Steps: e.Steps}
	if e.Server != "" {
		ref := names.ref(e.Server)
		r.Server = &ref
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
	cat := s.d.Speed.Catalog()
	out := &speedJSON{At: now.UnixMilli(), TZ: p.tz, TZName: zoneLabel(p.zone), Days: p.days, ExcludeFallback: p.excludeFallback, Mode: st.Mode, Modes: st.Modes,
		Enabled: st.Mode != speedtest.ModeOff, SetByFlag: st.SetByFlag,
		IntervalS: int64(st.Interval / time.Second), SpacingS: int64(st.Spacing / time.Second),
		PlannedShare: math.Round(st.PlannedShare*1000) / 1000, Warning: strPtr(st.Warning),
		TestShare24h: math.Round(s.d.Speed.TestShare(24*time.Hour)*1000) / 1000,
		NextAt:       ms(st.Next), NextRow: strPtr(st.NextRow), Running: s.speedRunning(names),
		Rows: []speedRow{}, PerRegion: []speedRow{}, PerServer: []speedRow{}, Servers: []speedServerJSON{}, Regions: []speedRegionJSON{},
		Connection: speedConnection{Type: "unknown", Source: "detected"}}
	month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	out.BytesMonth, _ = s.d.Speed.BytesSince(ctx, month)
	out.Bytes24h, _ = s.d.Speed.BytesSince(ctx, now.Add(-24*time.Hour))
	for _, sv := range st.Servers {
		ref, display, _, _ := names.server(sv.ID)
		j := speedServerJSON{ID: ref.ID, Label: ref.Label, Display: display, Provider: sv.Provider, City: sv.City, Country: sv.Country,
			Geo: sv.Geo, Area: profile.GeoTitles[sv.Geo], RTTms: roundP(sv.RTTms, 10), Picked: sv.Picked, Custom: sv.Custom,
			Skipped: sv.Skipped, SkipLocked: sv.SkipLocked, RestUntil: ms(sv.RestUntil)}
		if reg := names.inReg[sv.ID]; reg != "" {
			j.Region = &reg
		}
		if sv.CapacityMbps > 0 {
			c := sv.CapacityMbps
			j.Capacity = &c
		}
		if sv.Custom {
			j.Provider = "Your server"
		}
		j.Host = sv.Host
		j.Ports = strconv.Itoa(sv.PortMin)
		if sv.PortMax != sv.PortMin {
			j.Ports += "-" + strconv.Itoa(sv.PortMax)
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
	all = withVia(all, st)
	tz := p.zone.loc
	byRegion, byServer := map[string][]model.SpeedTest{}, map[string][]model.SpeedTest{}
	var span, last24 []model.SpeedTest
	var avgBytes []float64
	for _, t := range all {
		if t.OnDemand() {
			continue // rows and patterns are the schedule's (latest still shows every test)
		}
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
	out.SweepBytes, out.SweepServers = sweepBytes(st, all)
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
			if t.Region == row.ID && t.Outcome == model.SpeedOK && !t.OnDemand() {
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
	for i := len(latest) - 1; i >= 0; i-- { // the newest result (a cancelled test is none)
		if latest[i].Outcome != model.SpeedCancelled {
			l := names.test(latest[i])
			out.Latest = &l
			break
		}
	}
	for i := len(latest) - 1; i >= 0; i-- { // the newest test that knows
		if c := latest[i].Conn; c != "" && c != model.ConnUnknown {
			out.Connection.Type = c
			break
		}
	}
	out.Pattern = speedtest.PooledPattern(all, tz)
	eh, err := s.edgeHours(ctx, now, tz)
	if err != nil {
		return nil, err
	}
	out.Finding = speedtest.FindFinding(speedtest.FindingInput{Tests: all, Loc: tz, Catalog: cat, EdgeIdle: edgeIdle(eh), From: from, To: now})
	for i, x := range out.Finding.Slowest {
		var ref speedRef
		ref, out.Finding.Slowest[i].Display, _, _ = names.server(x.Server)
		out.Finding.Slowest[i].Ref = ref.ID
	}
	out.Coverage = speedCoverage(span, tz, cat)
	out.UnderLoad = speedUnderLoadOf(span)
	return out, nil
}

// edgeIdle is the finding's access-network control: the ISP edge's idle
// RTT per local hour (NaN with fewer than 3 days).
func edgeIdle(eh []speedEdgeHour) [24]float64 {
	var out [24]float64
	for h := range out {
		out[h] = math.NaN()
	}
	for _, x := range eh {
		if x.N >= 3 && x.RTTms != nil {
			out[x.Hour] = float64(*x.RTTms)
		}
	}
	return out
}

// edgeHours is the access network's control: it reads the ISP edge's
// hourly RTT over speedtest.PatternSpan and folds it into 24 local hours of
// day (see speedEdgeHour); nil without an ISP edge target.
func (s *server) edgeHours(ctx context.Context, now time.Time, tz *time.Location) ([]speedEdgeHour, error) {
	var key model.SeriesKey
	if p := s.d.Profile(); p != nil {
		for _, t := range p.Targets {
			if t.Layer == model.LayerEdge {
				key = model.SeriesKey{Target: t.Name, Kind: model.KindICMP}
			}
		}
	}
	if key.Target == "" || s.d.Store == nil {
		return nil, nil
	}
	to := now.Truncate(time.Hour)
	from := to.Add(-speedtest.PatternSpan)
	res, err := s.d.Store.Panel(ctx, store.PanelQuery{Keys: []model.SeriesKey{key}, From: from, To: to, MaxPoints: int(speedtest.PatternSpan / time.Hour)})
	if err != nil {
		return nil, err
	}
	rtts := make([][]float64, 24)
	var ok, lost [24]float64
	for _, c := range res.Series {
		for i := range c.N {
			n, l := float64(c.N[i]), float64(at(c.Lost, i))
			if n+l == 0 {
				continue
			}
			h := res.Start.Add(time.Duration(i) * res.Step).In(tz).Hour()
			ok[h] += n
			lost[h] += l
			if n > 0 && i < len(c.Mean) && !math.IsNaN(float64(c.Mean[i])) {
				rtts[h] = append(rtts[h], float64(c.Mean[i]))
			}
		}
	}
	out := make([]speedEdgeHour, 24)
	for h := range out {
		out[h] = speedEdgeHour{Hour: h, N: len(rtts[h])}
		if ok[h]+lost[h] > 0 {
			l := num(lost[h] / (ok[h] + lost[h]))
			out[h].Loss = &l
		}
		if len(rtts[h]) > 0 {
			r := num(callq.Median(rtts[h]))
			out[h].RTTms = &r
		}
	}
	return out, nil
}

func speedRowOf(id, kind, label string, ts []model.SpeedTest, tz *time.Location) speedRow {
	row := speedRow{ID: id, Kind: kind, Label: label, Stats: speedtest.Summarize(ts, tz)}
	if row.Stats.LatestAt != nil {
		row.LatestAt = ms(*row.Stats.LatestAt)
	}
	row.Learning = row.N > 0 && row.Score == nil
	return row
}

// speedCoverage is per local hour: scheduled ok tests and the providers
// that answered (a pattern's hour needs ≥ 2 providers).
func speedCoverage(ts []model.SpeedTest, tz *time.Location, cat *speedtest.Catalog) []speedHour {
	out := make([]speedHour, 24)
	provs := make([]map[string]bool, 24)
	for h := range 24 {
		out[h].Hour, provs[h] = h, map[string]bool{}
	}
	for _, t := range ts {
		if t.OnDemand() || t.Outcome != model.SpeedOK {
			continue
		}
		h := t.Start.In(tz).Hour()
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
// the median of the tests' idle and per-direction p50, in one sentence per
// layer.
func speedUnderLoadOf(ts []model.SpeedTest) speedUnderLoad {
	type acc struct {
		idle, down, up []float64
	}
	by := map[string]*acc{}
	for _, t := range ts {
		if t.Outcome != model.SpeedOK {
			continue
		}
		if t.Latency == nil {
			continue
		}
		for layer, l := range map[string]model.LoadPhases{speedtest.LayerEdge: t.Latency.Edge, speedtest.LayerGateway: t.Latency.Gateway} {
			a := by[layer]
			if a == nil {
				a = &acc{}
				by[layer] = a
			}
			if l.Idle != nil {
				a.idle = append(a.idle, l.Idle.P50Ms)
			}
			if l.Down != nil {
				a.down = append(a.down, l.Down.P50Ms)
			}
			if l.Up != nil {
				a.up = append(a.up, l.Up.P50Ms)
			}
		}
	}
	out := speedUnderLoad{Text: []string{}}
	med := func(v []float64) *float64 {
		if len(v) == 0 {
			return nil
		}
		x := math.Round(callq.Median(v)*10) / 10
		return &x
	}
	var edge, gw *speedLoad
	for _, layer := range []string{speedtest.LayerEdge, speedtest.LayerGateway} {
		a := by[layer]
		if a == nil {
			continue
		}
		l := &speedLoad{IdleMs: med(a.idle), DownMs: med(a.down), UpMs: med(a.up)}
		if layer == speedtest.LayerEdge {
			edge = l
		} else {
			gw = l
		}
	}
	effect := func(rise float64) string {
		switch {
		case rise < 5:
			return "no real change"
		case rise < 50:
			return "a small rise, fine for calls"
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
				line := fmt.Sprintf("Delay to your provider's first router rises from %.0f to %.0f ms while %s (%+.0f ms): %s.",
					*edge.IdleMs, *d.v, d.what, rise, effect(rise))
				if rise >= 50 {
					line += " This usually builds up in the queue on your line (your modem, or the provider's first router), not further out."
				}
				out.Text = append(out.Text, line)
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

var errNoSpeedID = errors.New("no such server or region")

// rowKnown reports a region or server id.
func rowKnown(id string, st speedtest.State) bool {
	return slices.ContainsFunc(st.Regions, func(r speedtest.Region) bool { return r.ID == id }) ||
		slices.ContainsFunc(st.Servers, func(sv speedtest.ServerState) bool { return sv.ID == id })
}

// speedOneJSON is api/speed?row=<id> (or server=, region=): one row's
// stats, every test in the span (oldest first, at most speedTestsCap) and
// its windows.
type speedOneJSON struct {
	At              int64           `json:"at"`
	TZ              int             `json:"tz"`
	TZName          string          `json:"tz_name"`
	Days            int             `json:"days"`
	ExcludeFallback bool            `json:"exclude_fallback"`
	Kind            string          `json:"kind"`
	Row             speedRow        `json:"row"`
	Tests           []speedTestJSON `json:"tests"`
	Windows         []speedWindow   `json:"windows"`
}

func (s *server) speedOne(ctx context.Context, p speedParams, st speedtest.State, names speedNames, now time.Time) (*speedOneJSON, error) {
	from := now.Add(-time.Duration(p.days) * 24 * time.Hour)
	out := &speedOneJSON{At: now.UnixMilli(), TZ: p.tz, TZName: zoneLabel(p.zone), Days: p.days, ExcludeFallback: p.excludeFallback, Tests: []speedTestJSON{}}
	var keep func(model.SpeedTest) bool
	var label, id string
	var servers []string
	if i := slices.IndexFunc(st.Regions, func(r speedtest.Region) bool { return r.ID == p.row }); i >= 0 {
		r := st.Regions[i]
		out.Kind, label, id = "region", r.Title, r.ID
		servers = append(slices.Clone(r.Servers), r.Nearby...)
		keep = func(t model.SpeedTest) bool { return t.Region == r.ID && !(p.excludeFallback && t.Fallback) }
	} else {
		local := p.row
		if !slices.ContainsFunc(st.Servers, func(sv speedtest.ServerState) bool { return sv.ID == local }) {
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
	all = withVia(all, s.d.Speed.State())
	var ts []model.SpeedTest
	for _, t := range all {
		if keep(t) {
			ts = append(ts, t)
		}
	}
	for _, t := range ts[max(0, len(ts)-speedTestsCap):] {
		out.Tests = append(out.Tests, names.test(t))
	}
	out.Row = speedRowOf(id, out.Kind, label, ts, p.zone.loc)
	out.Row.Servers = servers
	for _, r := range st.Rows {
		if r.ID == id || names.ref(r.ID).ID == id {
			out.Row.Scheduled, out.Row.Why = true, r.Why
		}
	}
	out.Windows = speedWindows(ts, from)
	return out, nil
}

// withVia adds the internet exchange each test's handoff crossed
// (State.Vias: kept in memory, never stored, re-traced daily) when the
// server's latest handoff is the test's.
func withVia(ts []model.SpeedTest, st speedtest.State) []model.SpeedTest {
	if len(st.Vias) == 0 {
		return ts
	}
	out := slices.Clone(ts)
	for i, t := range out {
		if v, ok := st.Vias[t.Server]; ok && t.Handoff != nil && t.Handoff.ASN == v.ASN && t.Handoff.ISPASN == v.ISPASN {
			h := *t.Handoff
			h.Via = v.Via
			out[i].Handoff = &h
		}
	}
	return out
}

// storeTimeout bounds a write route's store call.
const storeTimeout = 10 * time.Second

// storeErr logs a store failure and answers 500.
func (h *localHandler) storeErr(w http.ResponseWriter, r *http.Request, what string, err error) {
	h.d.log().Warn(what, "err", err)
	writeErr(w, r, http.StatusInternalServerError, what+" failed: "+err.Error())
}

// speedPOST checks a speed-test write: a source, POST, only the allowed
// parameters, and the speed-test control checks (speedControlAllowed).
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
	return h.speedControlAllowed(w, r)
}

// serveSpeedConfig answers POST api/speed/config {"mode", "extra",
// "skip"}.
func (h *localHandler) serveSpeedConfig(w http.ResponseWriter, r *http.Request) {
	if !h.speedPOST(w, r) {
		return
	}
	var body struct {
		Mode  *string  `json:"mode"`
		Extra []string `json:"extra"` // opt-in (regions) or far (nearby) regions
		Skip  []string `json:"skip"`  // servers never to test (replaces the list)
	}
	d := json.NewDecoder(io.LimitReader(r.Body, 4<<10))
	d.DisallowUnknownFields()
	if err := d.Decode(&body); err != nil || (body.Mode == nil && body.Skip == nil) || (body.Mode == nil && body.Extra != nil) {
		writeErr(w, r, http.StatusBadRequest, `want {"mode": "off", "regions", "nearby" or "custom", "extra": [region ids]} and/or {"skip": [server ids]}`)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), storeTimeout)
	defer cancel()
	out := map[string]any{}
	if body.Mode != nil {
		next, err := h.d.Speed.SetMode(ctx, speedtest.Mode(*body.Mode), body.Extra)
		if errors.Is(err, speedtest.ErrMode) {
			writeErr(w, r, http.StatusBadRequest, err.Error())
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
	if body.Skip != nil {
		if err := h.d.Speed.SetSkip(ctx, body.Skip); errors.Is(err, speedtest.ErrMode) {
			writeErr(w, r, http.StatusBadRequest, err.Error())
			return
		} else if err != nil {
			h.storeErr(w, r, "save the speed test setting", err)
			return
		}
		out["skip"] = body.Skip
	}
	writeJSON(w, r, http.StatusOK, out)
}

// zoneLabel is the IANA name the hours are in: the parameter's, or the
// server's own for no tz (its TZ, else "Local"), "" for a fixed offset.
// sweepBytes estimates "Test every server": the sum, over the servers it
// tests, of each one's median data over its recent ok tests, else this
// line's median rate both ways for MaxMeasure, else defaultTestBytes.
func sweepBytes(st speedtest.State, ts []model.SpeedTest) (total int64, n int) {
	per := map[string][]float64{}
	var down, up []float64
	for _, t := range ts {
		if t.Outcome != model.SpeedOK {
			continue
		}
		if t.Bytes > 0 {
			per[t.Server] = append(per[t.Server], float64(t.Bytes))
		}
		if t.DownBps != nil {
			down = append(down, *t.DownBps)
		}
		if t.UpBps != nil {
			up = append(up, *t.UpBps)
		}
	}
	fallback := float64(defaultTestBytes)
	if len(down) > 0 {
		rate := callq.Median(down)
		if len(up) > 0 {
			rate += callq.Median(up)
		} else {
			rate *= 2
		}
		fallback = rate / 8 * speedtest.MaxMeasure.Seconds()
	}
	for _, sv := range st.Servers {
		if sv.Skipped {
			continue
		}
		n++
		if v := per[sv.ID]; len(v) > 0 {
			total += int64(callq.Median(v))
		} else {
			total += int64(fallback)
		}
	}
	return total, n
}

func zoneLabel(z zoneParam) string {
	if z.name != "" || z.loc != time.Local {
		return z.name
	}
	return time.Local.String()
}
