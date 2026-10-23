package web

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/s3test"
	"github.com/fuck-you-isp/fyisp/internal/storage"
	"github.com/fuck-you-isp/fyisp/internal/store"
)

// Storage campaigns: the summary's live campaigns and history, the parts
// (api/storage?part=...) and the controls (docs/STORAGE-API.md).

// StorageReader reads what campaigns stored (store.SQLite).
type StorageReader interface {
	StorageCampaign(ctx context.Context, id int64) (model.StorageCampaign, bool, error)
	StorageCampaigns(ctx context.Context, before int64, limit int, live bool) ([]model.StorageCampaign, error)
	StorageRounds(ctx context.Context, campaign int64, afterDue time.Time) ([]model.StorageRound, error)
	StorageRound(ctx context.Context, id int64) (model.StorageRound, bool, error)
	StorageAttempts(ctx context.Context, f store.StorageAttemptFilter) ([]store.StorageAttempt, int, error)
	StorageCells(ctx context.Context, campaign int64) ([]store.StorageCellRow, error)
	StorageTraces(ctx context.Context, campaign int64) ([]model.StorageTrace, error)
	StorageTimings(ctx context.Context, campaign int64) ([]store.StorageTiming, error)
}

// Limits of the campaign reads.
const (
	storageHistory       = 10       // campaigns in the summary
	storageCampaignsPage = 50       // part=campaigns
	storageAttemptsPage  = 50       // part=attempts default
	storageAttemptsMax   = 500      // ... and most
	storageCampaignBody  = 64 << 10 // 8 targets' keys with session tokens
)

// ---------- JSON ----------

type storageCampaignTargetJSON struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Mode string `json:"mode"`
}

type storageScheduleJSON struct {
	Days      []int  `json:"days"` // 1 Monday ... 7 Sunday
	From      string `json:"from"` // "19:00"
	To        string `json:"to"`
	TZ        string `json:"tz"`
	Until     *int64 `json:"until"`
	KeysSaved bool   `json:"keys_saved"`
}

// storageCampaignJSON is a campaign: what every view shows of one.
type storageCampaignJSON struct {
	ID         int64                       `json:"id"`
	Shape      string                      `json:"shape"`
	State      string                      `json:"state"`
	StopReason *string                     `json:"stop_reason"`
	Created    int64                       `json:"created"`
	StartAt    *int64                      `json:"start_at"`
	EndsAt     *int64                      `json:"ends_at"` // planned (duration), or when it ended
	Count      *int                        `json:"count"`
	IntervalS  int64                       `json:"interval_s"`
	Cold       int                         `json:"cold"`
	Warm       int                         `json:"warm"`
	DownBytes  int64                       `json:"down_bytes"`
	UpBytes    int64                       `json:"up_bytes"`
	Targets    []storageCampaignTargetJSON `json:"targets"`
	Schedule   *storageScheduleJSON        `json:"schedule"`
	RoundsDone int                         `json:"rounds_done"`
	BytesDown  int64                       `json:"bytes_down"`
	BytesUp    int64                       `json:"bytes_up"`
}

// storageLiveJSON is a live campaign: the campaign and what it does now.
type storageLiveJSON struct {
	storageCampaignJSON
	InWindow      *bool    `json:"in_window"` // recurring only
	NextAt        *int64   `json:"next_at"`
	Running       bool     `json:"running"` // a round runs now
	Phase         *string  `json:"phase"`
	Target        *string  `json:"target"`
	QueuePosition *int     `json:"queue_position"` // its round waits in the queue
	WaitingKeys   []string `json:"waiting_keys"`
}

func hhmm(m int) string { return fmt.Sprintf("%02d:%02d", m/60, m%60) }

func storageCampaign(c model.StorageCampaign) storageCampaignJSON {
	j := storageCampaignJSON{ID: c.ID, Shape: c.Shape, State: c.State, StopReason: strOrNil(c.Stopped), Created: c.Created.UnixMilli(),
		StartAt: msPtr(c.Start), EndsAt: msPtr(c.End), IntervalS: int64(c.Interval / time.Second), Cold: c.Cold, Warm: c.Warm,
		DownBytes: c.Down, UpBytes: c.Up, RoundsDone: c.RoundsDone, BytesDown: c.BytesDown, BytesUp: c.BytesUp, Targets: []storageCampaignTargetJSON{}}
	if c.Count > 0 {
		j.Count = &c.Count
	}
	for _, t := range c.Targets {
		j.Targets = append(j.Targets, storageCampaignTargetJSON(t))
	}
	if c.Shape == model.StorageRecurring {
		sc := &storageScheduleJSON{Days: []int{}, From: hhmm(c.FromMin), To: hhmm(c.ToMin), TZ: c.TZ, Until: msPtr(c.Until), KeysSaved: c.KeysSaved}
		for d := 0; d < 7; d++ {
			if c.Days&(1<<d) != 0 {
				sc.Days = append(sc.Days, d+1)
			}
		}
		j.Schedule = sc
	}
	return j
}

func (s *server) storageLive(l storage.LiveCampaign) storageLiveJSON {
	j := storageLiveJSON{storageCampaignJSON: storageCampaign(l.StorageCampaign), NextAt: msPtr(l.Next), WaitingKeys: l.WaitingKeys}
	if j.WaitingKeys == nil {
		j.WaitingKeys = []string{}
	}
	if l.Shape == model.StorageRecurring {
		j.InWindow = &l.InWindow
	}
	if id, phase, target := s.d.Campaigns.Live(); id == l.ID {
		j.Running, j.Phase, j.Target = true, strOrNil(phase), strOrNil(target)
	} else if l.Entry != 0 && s.d.Speed != nil {
		if e, ok := s.d.Speed.Queue().Get(l.Entry); ok && e.State == "waiting" {
			j.QueuePosition = &e.Position
		}
	}
	return j
}

// storageCampaignsSummary adds the live campaigns, the last ones that
// ended and the notices to the summary.
func (s *server) storageCampaignsSummary(ctx context.Context, out map[string]any) error {
	live := []storageLiveJSON{}
	for _, l := range s.d.Campaigns.State() {
		live = append(live, s.storageLive(l))
	}
	out["live"] = live
	hist := []storageCampaignJSON{}
	if s.d.StorageData != nil {
		cs, err := s.d.StorageData.StorageCampaigns(ctx, 0, storageHistory+len(live), false)
		if err != nil {
			return err
		}
		for _, c := range cs {
			if !c.Live() && len(hist) < storageHistory {
				hist = append(hist, storageCampaign(c))
			}
		}
	}
	out["history"] = hist
	notices := []map[string]any{}
	for _, n := range s.d.Campaigns.Notices() {
		notices = append(notices, map[string]any{"code": n.Code, "at": n.At.UnixMilli(), "campaign": n.Campaign, "targets": orEmpty(n.Targets)})
	}
	out["notices"] = notices
	return nil
}

func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// storeFailed logs a failed store read and answers 500.
func (s *server) storeFailed(w http.ResponseWriter, r *http.Request, err error) {
	s.d.log().Warn("storage tests: reading the store", "err", err)
	writeErr(w, r, http.StatusInternalServerError, "reading the storage tests failed")
}

// ---------- parts ----------

func (s *server) serveStoragePart(w http.ResponseWriter, r *http.Request, part string) {
	q := r.URL.Query()
	var allowed []string
	switch part {
	case "live":
		allowed = []string{"part"}
	case "campaigns":
		allowed = []string{"part", "before", "limit"}
	case "campaign", "round":
		allowed = []string{"part", "id"}
	case "attempts":
		allowed = []string{"part", "campaign", "target", "class", "phase", "failed", "before", "limit"}
	case "estimate":
		allowed = []string{"part", "targets", "keyed", "shape", "interval_s", "cold", "warm", "down_bytes", "up_bytes", "count", "duration_s", "days", "from", "to"}
	default:
		writeErr(w, r, http.StatusBadRequest, "unknown part")
		return
	}
	if err := checkParams(q, allowed...); err != nil {
		writeErr(w, r, http.StatusBadRequest, err.Error())
		return
	}
	if s.d.Campaigns == nil || s.d.StorageData == nil {
		notFound(w, r)
		return
	}
	ctx := r.Context()
	switch part {
	case "live":
		live := []storageLiveJSON{}
		for _, l := range s.d.Campaigns.State() {
			live = append(live, s.storageLive(l))
		}
		writeJSON(w, r, http.StatusOK, map[string]any{"at": s.now().UnixMilli(), "rev": s.storageRev(), "live": live})
	case "campaigns":
		before, err1 := optInt(q, "before", 0, 0, 1<<62)
		limit, err2 := optInt(q, "limit", storageCampaignsPage, 1, storageCampaignsPage)
		if err := firstErr(err1, err2); err != nil {
			writeErr(w, r, http.StatusBadRequest, err.Error())
			return
		}
		cs, err := s.d.StorageData.StorageCampaigns(ctx, before, int(limit)+1, false)
		if err != nil {
			s.storeFailed(w, r, err)
			return
		}
		var next *int64
		if len(cs) > int(limit) {
			cs = cs[:limit]
			next = &cs[len(cs)-1].ID
		}
		out := []storageCampaignJSON{}
		for _, c := range cs {
			out = append(out, storageCampaign(c))
		}
		writeJSON(w, r, http.StatusOK, map[string]any{"campaigns": out, "next_before": next})
	case "campaign":
		id, err := reqInt(q, "id")
		if err != nil {
			writeErr(w, r, http.StatusBadRequest, err.Error())
			return
		}
		s.serveCampaign(w, r, id)
	case "round":
		id, err := reqInt(q, "id")
		if err != nil {
			writeErr(w, r, http.StatusBadRequest, err.Error())
			return
		}
		rr, ok, err := s.d.StorageData.StorageRound(ctx, id)
		if err != nil {
			s.storeFailed(w, r, err)
			return
		}
		if !ok {
			notFound(w, r)
			return
		}
		as, _, err := s.d.StorageData.StorageAttempts(ctx, store.StorageAttemptFilter{Round: id})
		if err != nil {
			s.storeFailed(w, r, err)
			return
		}
		by := map[string][]storageAttemptJSON{}
		for _, a := range as {
			by[a.Target] = append(by[a.Target], storageAttempt(a))
		}
		writeJSON(w, r, http.StatusOK, map[string]any{"round": storageRound(rr), "attempts": by})
	case "attempts":
		s.serveAttempts(w, r)
	case "estimate":
		s.serveEstimate(w, r)
	}
}

func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

// optInt is q's integer key in [lo, hi], or def without it.
func optInt(q url.Values, key string, def, lo, hi int64) (int64, error) {
	v := q.Get(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < lo || n > hi {
		return 0, badReq("%s: an integer from %d to %d", key, lo, hi)
	}
	return n, nil
}

func reqInt(q url.Values, key string) (int64, error) {
	if q.Get(key) == "" {
		return 0, badReq("%s is required", key)
	}
	return optInt(q, key, 0, 1, 1<<62)
}

type storageRoundJSON struct {
	ID          int64             `json:"id"`
	Seq         *int              `json:"seq"`
	Due         int64             `json:"due"`
	Start       *int64            `json:"start"`
	End         *int64            `json:"end"`
	QueueWaitMs *int64            `json:"queue_wait_ms"`
	Outcome     string            `json:"outcome"`
	SkipReason  *string           `json:"skip_reason"`
	Skips       map[string]string `json:"skips"`
	GwLoss      *float64          `json:"gw_loss"`
	EdgeLoss    *float64          `json:"edge_loss"`
	BytesDown   int64             `json:"bytes_down"`
	BytesUp     int64             `json:"bytes_up"`
}

func storageRound(r model.StorageRound) storageRoundJSON {
	j := storageRoundJSON{ID: r.ID, Seq: r.Seq, Due: r.Due.UnixMilli(), Start: msPtr(r.Start), End: msPtr(r.End), Outcome: r.Outcome,
		SkipReason: strOrNil(r.SkipReason), Skips: r.Skips, GwLoss: r.GwLoss, EdgeLoss: r.EdgeLoss, BytesDown: r.BytesDown, BytesUp: r.BytesUp}
	if j.Skips == nil {
		j.Skips = map[string]string{}
	}
	if !r.Start.IsZero() {
		ms := r.QueueWait.Milliseconds()
		j.QueueWaitMs = &ms
	}
	return j
}

type storageAttemptJSON struct {
	ID              int64    `json:"id"`
	Round           int64    `json:"round"`
	Target          string   `json:"target"`
	Phase           string   `json:"phase"`
	Op              string   `json:"op"`
	N               int      `json:"n"`
	At              int64    `json:"at"`
	Class           string   `json:"class"`
	Group           string   `json:"group"`
	HTTPStatus      *int     `json:"http_status"`
	S3Code          *string  `json:"s3_code"`
	RequestID       *string  `json:"request_id"`
	HostID          *string  `json:"host_id"`
	RequestIDHeader *string  `json:"request_id_header"`
	DNSms           *float64 `json:"dns_ms"`
	Connectms       *float64 `json:"connect_ms"`
	TLSms           *float64 `json:"tls_ms"`
	TTFBms          *float64 `json:"ttfb_ms"`
	Totalms         *float64 `json:"total_ms"`
	Bytes           int64    `json:"bytes"`
	GoodputBps      *float64 `json:"goodput_bps"`
	Stallms         *float64 `json:"stall_ms"`
	EdgePrefix      *string  `json:"edge_prefix"`
	TLSVersion      *string  `json:"tls_version"`
	TCPRTTms        *float64 `json:"tcp_rtt_ms"`
	Retrans         *int     `json:"retrans"`
	MSS             *int     `json:"mss"`
	SkewS           *int     `json:"skew_s"`
}

func storageAttempt(a store.StorageAttempt) storageAttemptJSON {
	j := storageAttemptJSON{ID: a.ID, Round: a.Round, Target: a.Target, Phase: a.Phase, Op: a.Op, N: a.N, At: a.At.UnixMilli(), Class: a.Class,
		Group: s3test.Class(a.Class).Group(), S3Code: strOrNil(a.S3Code), RequestID: strOrNil(a.RequestID), HostID: strOrNil(a.HostID),
		RequestIDHeader: strOrNil(a.RequestIDHeader), DNSms: a.DNSms, Connectms: a.Connectms, TLSms: a.TLSms, TTFBms: a.TTFBms, Totalms: a.Totalms,
		Bytes: a.Bytes, GoodputBps: a.GoodputBps, Stallms: a.Stallms, EdgePrefix: strOrNil(a.EdgePrefix), TLSVersion: strOrNil(a.TLSVersion),
		TCPRTTms: a.TCPRTTms, Retrans: a.Retrans, MSS: a.MSS, SkewS: a.SkewS}
	if a.HTTPStatus != 0 {
		j.HTTPStatus = &a.HTTPStatus
	}
	return j
}

// Cell letters: one per round and target.
const (
	cellOK        = 'o'
	cellSlow      = 's' // a transfer stalled
	cellFailed    = 'f' // a network or provider failure
	cellConfig    = 'a' // only settings, keys or the clock refused
	cellNoKeys    = 'k' // transfers didn't run: no keys
	cellSkipped   = 'b' // the round didn't run (busy, paused, another campaign)
	cellGap       = 'g' // fyisp wasn't running
	cellCancelled = 'c'
)

type storageNoteJSON struct {
	Class  string `json:"class"` // the most frequent failure's
	Failed int    `json:"failed"`
	Of     int    `json:"of"`
	Status string `json:"status"` // its HTTP status and S3 code, "" without
}

type storageSeriesJSON struct {
	At      []int64    `json:"at"`
	DownBps []*float64 `json:"down_bps"`
	UpBps   []*float64 `json:"up_bps"`
}

// serveCampaign answers part=campaign: the campaign, its rounds (runs,
// skips and gaps, oldest first), a cell per round and target with notes
// on the ones not ok, and each keyed target's transfer speeds.
func (s *server) serveCampaign(w http.ResponseWriter, r *http.Request, id int64) {
	ctx := r.Context()
	c, ok, err := s.d.StorageData.StorageCampaign(ctx, id)
	if err != nil {
		s.storeFailed(w, r, err)
		return
	}
	if !ok {
		notFound(w, r)
		return
	}
	rounds, err := s.d.StorageData.StorageRounds(ctx, id, time.Time{})
	if err != nil {
		s.storeFailed(w, r, err)
		return
	}
	rows, err := s.d.StorageData.StorageCells(ctx, id)
	if err != nil {
		s.storeFailed(w, r, err)
		return
	}
	type agg struct {
		n, failed, config int
		stall             bool
		classes           map[string]int
		status            map[string]string
		down, up          *float64
	}
	cells := map[int64]map[string]*agg{}
	for _, row := range rows {
		m := cells[row.Round]
		if m == nil {
			m = map[string]*agg{}
			cells[row.Round] = m
		}
		a := m[row.Target]
		if a == nil {
			a = &agg{classes: map[string]int{}, status: map[string]string{}}
			m[row.Target] = a
		}
		a.n += row.N
		switch s3test.Class(row.Class).Group() {
		case s3test.GroupNetwork, s3test.GroupProvider:
			a.failed += row.N
			a.classes[row.Class] += row.N
		case s3test.GroupConfig:
			a.config += row.N
			a.classes[row.Class] += row.N
		}
		if row.Class != string(s3test.ClassOK) && a.status[row.Class] == "" {
			a.status[row.Class] = strings.TrimSpace(row.HTTPStatus + " " + row.S3Code)
		}
		if row.MaxStallMs >= 2000 {
			a.stall = true
		}
		if row.Class == string(s3test.ClassOK) && row.GoodputBps != nil {
			switch row.Phase {
			case model.StoragePhaseDown:
				a.down = row.GoodputBps
			case model.StoragePhaseUp:
				a.up = row.GoodputBps
			}
		}
	}
	out := map[string]any{"campaign": storageCampaign(c)}
	rj := make([]storageRoundJSON, len(rounds))
	strs := map[string][]byte{}
	notes := map[string]map[string]storageNoteJSON{}
	series := map[string]*storageSeriesJSON{}
	for _, t := range c.Targets {
		strs[t.ID] = make([]byte, 0, len(rounds))
		notes[t.ID] = map[string]storageNoteJSON{}
		if t.Mode == "bucket" {
			series[t.ID] = &storageSeriesJSON{At: []int64{}, DownBps: []*float64{}, UpBps: []*float64{}}
		}
	}
	for i, rr := range rounds {
		rj[i] = storageRound(rr)
		for _, t := range c.Targets {
			a := cells[rr.ID][t.ID]
			ch := byte(cellOK)
			switch {
			case rr.Outcome == model.RoundGap:
				ch = cellGap
			case rr.Outcome == model.RoundSkipped && rr.SkipReason == model.SkipNoKeys:
				ch = cellNoKeys
			case rr.Outcome == model.RoundSkipped:
				ch = cellSkipped
			case rr.Outcome == model.RoundCancelled:
				ch = cellCancelled
			case a != nil && a.failed > 0:
				ch = cellFailed
			case a != nil && a.config > 0, rr.Skips[t.ID] == "auth", rr.Skips[t.ID] == "clock_skew", rr.Skips[t.ID] == "config":
				ch = cellConfig
			case rr.Skips[t.ID] == model.SkipNoKeys:
				ch = cellNoKeys
			case a == nil:
				ch = cellSkipped
			case a.stall:
				ch = cellSlow
			}
			strs[t.ID] = append(strs[t.ID], ch)
			if a != nil && (ch == cellFailed || ch == cellConfig) {
				top := ""
				for cl, n := range a.classes {
					if top == "" || n > a.classes[top] || (n == a.classes[top] && cl < top) {
						top = cl
					}
				}
				notes[t.ID][strconv.Itoa(i)] = storageNoteJSON{Class: top, Failed: a.failed + a.config, Of: a.n, Status: a.status[top]}
			}
			if sr := series[t.ID]; sr != nil && a != nil && (a.down != nil || a.up != nil) {
				at := rr.Start
				if at.IsZero() {
					at = rr.Due
				}
				sr.At, sr.DownBps, sr.UpBps = append(sr.At, at.UnixMilli()), append(sr.DownBps, a.down), append(sr.UpBps, a.up)
			}
		}
	}
	cj := map[string]string{}
	for id, b := range strs {
		cj[id] = string(b)
	}
	out["rounds"], out["cells"], out["notes"], out["throughput"] = rj, cj, notes, series
	counts := make([]storage.AttemptCount, len(rows))
	for i, row := range rows {
		counts[i] = storage.AttemptCount{Round: row.Round, Target: row.Target, Phase: row.Phase, Class: row.Class, EdgePrefix: row.EdgePrefix, N: row.N}
	}
	fd := storage.Explain(c, rounds, counts, c.Live())
	out["finding"] = map[string]any{"kicker": fd.Kicker, "text": fd.Text, "evidence": orEmpty(fd.Evidence)}
	traces, err := s.d.StorageData.StorageTraces(ctx, id)
	if err != nil {
		s.storeFailed(w, r, err)
		return
	}
	tj := []map[string]any{}
	for _, t := range traces {
		hops := t.Hops
		if hops == nil {
			hops = []model.StorageHop{}
		}
		var handoff *uint32
		if t.HandoffASN != 0 {
			handoff = &t.HandoffASN
		}
		tj = append(tj, map[string]any{"id": t.ID, "target": t.Target, "at": t.At.UnixMilli(), "trigger": t.Trigger, "edge_prefix": strOrNil(t.EdgePrefix),
			"hops": hops, "handoff_asn": handoff, "route_changed": t.RouteChanged})
	}
	out["traces"] = tj
	timings, err := s.d.StorageData.StorageTimings(ctx, id)
	if err != nil {
		s.storeFailed(w, r, err)
		return
	}
	out["cards"] = storageCards(c, timings)
	writeJSON(w, r, http.StatusOK, out)
}

type storagePctJSON struct {
	P50 *float64 `json:"p50"`
	P95 *float64 `json:"p95"`
}

type storageRateJSON struct {
	MedianBps *float64 `json:"median_bps"`
	P10Bps    *float64 `json:"p10_bps"`
	OK        int      `json:"ok"`
	N         int      `json:"n"`
}

type storagePrefixJSON struct {
	Prefix string `json:"prefix"`
	N      int    `json:"n"`
	Failed int    `json:"failed"`
}

// storageCardJSON is one target's summary over a campaign.
type storageCardJSON struct {
	Target     string              `json:"target"`
	ColdN      int                 `json:"cold_n"`
	ColdFailed int                 `json:"cold_failed"`
	Failed     int                 `json:"failed"` // every phase, network and provider
	Config     int                 `json:"config"`
	Classes    map[string]int      `json:"classes"`
	Connectms  storagePctJSON      `json:"connect_ms"`
	TLSms      storagePctJSON      `json:"tls_ms"`
	TTFBms     storagePctJSON      `json:"ttfb_ms"`
	Down       *storageRateJSON    `json:"down"`
	Up         *storageRateJSON    `json:"up"`
	Prefixes   []storagePrefixJSON `json:"prefixes"`     // the 5 with the most failures, then attempts
	PrefixN    int                 `json:"prefix_count"` // all of them
}

// pct is the q-quantile of vs (nearest rank; nil for none).
func pct(vs []float64, q float64) *float64 {
	if len(vs) == 0 {
		return nil
	}
	s := slices.Clone(vs)
	slices.Sort(s)
	i := int(q*float64(len(s)-1) + 0.5)
	v := s[i]
	return &v
}

// storageCards summarises each target's attempts: fresh connections and
// their failures by class, the timings of the ones that worked, the
// transfers' speeds, and the provider address ranges with their failures.
func storageCards(c model.StorageCampaign, ts []store.StorageTiming) []storageCardJSON {
	type acc struct {
		card            storageCardJSON
		conn, tls, ttfb []float64
		down, up        []float64
		downN, upN      int
		prefixes        map[string]*storagePrefixJSON
	}
	per := map[string]*acc{}
	for _, t := range c.Targets {
		per[t.ID] = &acc{card: storageCardJSON{Target: t.ID, Classes: map[string]int{}, Prefixes: []storagePrefixJSON{}}, prefixes: map[string]*storagePrefixJSON{}}
	}
	for _, t := range ts {
		a := per[t.Target]
		if a == nil {
			continue
		}
		g := s3test.Class(t.Class).Group()
		bad := g == s3test.GroupNetwork || g == s3test.GroupProvider
		if t.Phase == model.StoragePhaseCold {
			a.card.ColdN++
			if bad {
				a.card.ColdFailed++
			}
		}
		if bad {
			a.card.Failed++
			a.card.Classes[t.Class]++
		} else if g == s3test.GroupConfig {
			a.card.Config++
			a.card.Classes[t.Class]++
		}
		if t.Class == string(s3test.ClassOK) && (t.Phase == model.StoragePhaseCold || t.Phase == model.StoragePhaseWarm) {
			for _, x := range []struct {
				v   *float64
				dst *[]float64
			}{{t.Connectms, &a.conn}, {t.TLSms, &a.tls}, {t.TTFBms, &a.ttfb}} {
				if x.v != nil && (x.dst != &a.conn || t.Phase == model.StoragePhaseCold) {
					*x.dst = append(*x.dst, *x.v)
				}
			}
		}
		switch {
		case t.Phase == model.StoragePhaseDown:
			a.downN++
			if t.Class == string(s3test.ClassOK) && t.GoodputBps != nil {
				a.down = append(a.down, *t.GoodputBps)
			}
		case t.Phase == model.StoragePhaseUp && t.Op == "put":
			a.upN++
			if t.Class == string(s3test.ClassOK) && t.GoodputBps != nil {
				a.up = append(a.up, *t.GoodputBps)
			}
		}
		if t.EdgePrefix != "" {
			p := a.prefixes[t.EdgePrefix]
			if p == nil {
				p = &storagePrefixJSON{Prefix: t.EdgePrefix}
				a.prefixes[t.EdgePrefix] = p
			}
			p.N++
			if bad {
				p.Failed++
			}
		}
	}
	out := []storageCardJSON{}
	for _, t := range c.Targets {
		a := per[t.ID]
		a.card.Connectms = storagePctJSON{pct(a.conn, 0.5), pct(a.conn, 0.95)}
		a.card.TLSms = storagePctJSON{pct(a.tls, 0.5), pct(a.tls, 0.95)}
		a.card.TTFBms = storagePctJSON{pct(a.ttfb, 0.5), pct(a.ttfb, 0.95)}
		if a.downN > 0 {
			a.card.Down = &storageRateJSON{MedianBps: pct(a.down, 0.5), P10Bps: pct(a.down, 0.1), OK: len(a.down), N: a.downN}
		}
		if a.upN > 0 {
			a.card.Up = &storageRateJSON{MedianBps: pct(a.up, 0.5), P10Bps: pct(a.up, 0.1), OK: len(a.up), N: a.upN}
		}
		for _, p := range a.prefixes {
			a.card.Prefixes = append(a.card.Prefixes, *p)
		}
		slices.SortFunc(a.card.Prefixes, func(x, y storagePrefixJSON) int {
			if x.Failed != y.Failed {
				return y.Failed - x.Failed
			}
			if x.N != y.N {
				return y.N - x.N
			}
			return strings.Compare(x.Prefix, y.Prefix)
		})
		a.card.PrefixN = len(a.card.Prefixes)
		a.card.Prefixes = a.card.Prefixes[:min(len(a.card.Prefixes), 5)]
		out = append(out, a.card)
	}
	return out
}

var (
	storageClassRe  = regexp.MustCompile(`^[a-z_0-9]{1,32}$`)
	storageTargetRe = regexp.MustCompile(`^t_[0-9a-f]{8}$`)
)

// serveAttempts answers part=attempts: a campaign's attempts, newest first,
// filtered and paged.
func (s *server) serveAttempts(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	camp, err := reqInt(q, "campaign")
	before, err2 := optInt(q, "before", 0, 0, 1<<62)
	limit, err3 := optInt(q, "limit", storageAttemptsPage, 1, storageAttemptsMax)
	if err := firstErr(err, err2, err3); err != nil {
		writeErr(w, r, http.StatusBadRequest, err.Error())
		return
	}
	f := store.StorageAttemptFilter{Campaign: camp, Target: q.Get("target"), Class: q.Get("class"), Phase: q.Get("phase"),
		Failed: q.Get("failed") == "1", Before: before, Limit: int(limit) + 1}
	switch {
	case f.Class != "" && !storageClassRe.MatchString(f.Class), f.Target != "" && !storageTargetRe.MatchString(f.Target),
		f.Phase != "" && !slices.Contains([]string{"seed", "cold", "warm", "down", "up"}, f.Phase), q.Has("failed") && q.Get("failed") != "1":
		writeErr(w, r, http.StatusBadRequest, "bad target, class, phase or failed")
		return
	}
	if _, ok, err := s.d.StorageData.StorageCampaign(r.Context(), camp); err != nil {
		s.storeFailed(w, r, err)
		return
	} else if !ok {
		notFound(w, r)
		return
	}
	as, total, err := s.d.StorageData.StorageAttempts(r.Context(), f)
	if err != nil {
		s.storeFailed(w, r, err)
		return
	}
	var next *int64
	if len(as) > int(limit) {
		as = as[:limit]
		next = &as[len(as)-1].ID
	}
	items := make([]storageAttemptJSON, len(as))
	for i, a := range as {
		items[i] = storageAttempt(a)
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"items": items, "next_before": next, "total": total})
}

// serveEstimate answers part=estimate: what a campaign would move and
// take, from the same code Start checks with.
func (s *server) serveEstimate(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	sp, keyed, err := estimateSpec(q)
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, err.Error())
		return
	}
	e := s.d.Campaigns.Estimate(sp, keyed)
	per := map[string]int64{"down": e.PerRound[0], "up": e.PerRound[1]}
	out := map[string]any{"per_round": per, "round_s_est": int64(e.RoundTime / time.Second), "min_interval_s": int64(e.MinInterval / time.Second),
		"total": nil, "per_window": nil, "per_30d": nil, "rounds": nil}
	if e.Rounds > 0 {
		out["rounds"] = e.Rounds
		tot := map[string]int64{"down": e.PerRound[0] * int64(e.Rounds), "up": e.PerRound[1] * int64(e.Rounds)}
		if sp.Shape == model.StorageRecurring {
			out["per_window"] = tot
			out["per_30d"] = map[string]int64{"down": int64(float64(tot["down"]) * e.WindowsPer30), "up": int64(float64(tot["up"]) * e.WindowsPer30)}
		} else {
			out["total"] = tot
		}
	}
	writeJSON(w, r, http.StatusOK, out)
}

// estimateSpec reads an estimate's parameters.
func estimateSpec(q url.Values) (storage.CampaignSpec, int, error) {
	var sp storage.CampaignSpec
	sp.Targets = strings.Split(q.Get("targets"), ",")
	if q.Get("targets") == "" || len(sp.Targets) > storage.MaxCampaignTgts {
		return sp, 0, badReq("targets: 1 to %d target ids", storage.MaxCampaignTgts)
	}
	for _, id := range sp.Targets {
		if !storageTargetRe.MatchString(id) {
			return sp, 0, badReq("targets: target ids")
		}
	}
	keyed := 0
	if k := q.Get("keyed"); k != "" {
		for _, id := range strings.Split(k, ",") {
			if !slices.Contains(sp.Targets, id) {
				return sp, 0, badReq("keyed: targets of the campaign")
			}
			keyed++
		}
	}
	sp.Shape = q.Get("shape")
	iv, e1 := optInt(q, "interval_s", 0, 0, int64(storage.MaxInterval/time.Second))
	cold, e2 := optInt(q, "cold", 0, 0, s3test.MaxCold)
	warm, e3 := optInt(q, "warm", 0, 0, s3test.MaxWarm)
	down, e4 := optInt(q, "down_bytes", 0, 0, s3test.MaxTransfer)
	up, e5 := optInt(q, "up_bytes", 0, 0, s3test.MaxTransfer)
	count, e6 := optInt(q, "count", 0, 0, storage.MaxRounds)
	dur, e7 := optInt(q, "duration_s", 0, 0, int64(storage.MaxDuration/time.Second))
	if err := firstErr(e1, e2, e3, e4, e5, e6, e7); err != nil {
		return sp, 0, err
	}
	sp.Interval, sp.Cold, sp.Warm, sp.Down, sp.Up = time.Duration(iv)*time.Second, int(cold), int(warm), down, up
	sp.Count, sp.Duration = int(count), time.Duration(dur)*time.Second
	switch sp.Shape {
	case model.StorageOnDemand:
	case model.StorageRecurring:
		days, err := parseDays(q.Get("days"))
		if err != nil {
			return sp, 0, err
		}
		from, err1 := parseHHMM(q.Get("from"))
		to, err2 := parseHHMM(q.Get("to"))
		if err := firstErr(err1, err2); err != nil {
			return sp, 0, err
		}
		sp.Days, sp.FromMin, sp.ToMin = days, from, to
	default:
		return sp, 0, badReq(`shape: "on_demand" or "recurring"`)
	}
	return sp, min(keyed, len(sp.Targets)), nil
}

// parseDays reads "1,2,3" (1 Monday ... 7 Sunday) as a Days bitmask.
func parseDays(v string) (uint8, error) {
	var b uint8
	for _, d := range strings.Split(v, ",") {
		n, err := strconv.Atoi(d)
		if err != nil || n < 1 || n > 7 {
			return 0, badReq("days: weekdays 1 (Monday) to 7 (Sunday)")
		}
		b |= 1 << (n - 1)
	}
	return b, nil
}

var hhmmRe = regexp.MustCompile(`^([01]\d|2[0-3]):([0-5]\d)$`)

// parseHHMM reads "19:00" as minutes after midnight.
func parseHHMM(v string) (int, error) {
	m := hhmmRe.FindStringSubmatch(v)
	if m == nil {
		return 0, badReq("from and to: times as HH:MM")
	}
	h, _ := strconv.Atoi(m[1])
	mm, _ := strconv.Atoi(m[2])
	return h*60 + mm, nil
}

// ---------- controls ----------

// storageKeysMap is keys per target in a request.
type storageKeysMap map[string]storageKeysIn

// creds checks every target's keys; the caller zeroes them.
func (m storageKeysMap) creds() (map[string]*s3test.Creds, error) {
	out := map[string]*s3test.Creds{}
	for id, k := range m {
		c, err := k.creds()
		if err != nil {
			for _, x := range out {
				x.Zero()
			}
			return nil, err
		}
		out[id] = c
	}
	return out, nil
}

func zeroAll(m map[string]*s3test.Creds) {
	for _, c := range m {
		c.Zero()
	}
}

// serveCampaignWrite answers the campaign controls: run, campaign/start,
// stop, pause, resume and keys.
func (h *localHandler) serveCampaignWrite(w http.ResponseWriter, r *http.Request, route string) {
	params := []string{"id"}
	if route == "run" || route == "campaign/start" {
		params = nil
	}
	if !h.storagePOST(w, r, params...) {
		return
	}
	cs := h.d.Campaigns
	if cs == nil {
		notFound(w, r)
		return
	}
	ctx := r.Context()
	var id int64
	if params != nil {
		var err error
		if id, err = reqInt(r.URL.Query(), "id"); err != nil {
			writeErr(w, r, http.StatusBadRequest, err.Error())
			return
		}
	}
	keysErr := func(err error) {
		writeJSON(w, r, http.StatusBadRequest, map[string]string{"error": err.Error(), "code": "bad_request", "field": "keys"})
	}
	switch route {
	case "run":
		var body struct {
			Targets []string       `json:"targets"`
			Keys    storageKeysMap `json:"keys"`
		}
		if !decodeStorage(w, r, storageCampaignBody, &body, true) {
			return
		}
		keys, err := body.Keys.creds()
		if err != nil {
			keysErr(err)
			return
		}
		defer zeroAll(keys)
		c, err := cs.RunOnce(ctx, body.Targets, keys)
		if err != nil {
			storageErr(w, r, err)
			return
		}
		writeJSON(w, r, http.StatusAccepted, map[string]any{"campaign": storageCampaign(c)})
	case "campaign/start":
		var body struct {
			Targets   []string       `json:"targets"`
			Shape     string         `json:"shape"`
			IntervalS int64          `json:"interval_s"`
			Cold      int            `json:"cold"`
			Warm      int            `json:"warm"`
			DownBytes int64          `json:"down_bytes"`
			UpBytes   int64          `json:"up_bytes"`
			StartAt   *int64         `json:"start_at"`
			Count     int            `json:"count"`
			DurationS int64          `json:"duration_s"`
			Days      []int          `json:"days"`
			From      string         `json:"from"`
			To        string         `json:"to"`
			TZ        string         `json:"tz"`
			Until     *int64         `json:"until"`
			Keys      storageKeysMap `json:"keys"`
		}
		if !decodeStorage(w, r, storageCampaignBody, &body, true) {
			return
		}
		sp := storage.CampaignSpec{Targets: body.Targets, Shape: body.Shape, Interval: time.Duration(body.IntervalS) * time.Second,
			Cold: body.Cold, Warm: body.Warm, Down: body.DownBytes, Up: body.UpBytes, Count: body.Count,
			Duration: time.Duration(body.DurationS) * time.Second, TZ: body.TZ}
		if body.StartAt != nil {
			sp.Start = time.UnixMilli(*body.StartAt)
		}
		if body.Until != nil {
			sp.Until = time.UnixMilli(*body.Until)
		}
		if body.Shape == model.StorageRecurring {
			var err error
			days := make([]string, len(body.Days))
			for i, d := range body.Days {
				days[i] = strconv.Itoa(d)
			}
			if sp.Days, err = parseDays(strings.Join(days, ",")); err != nil {
				writeJSON(w, r, http.StatusBadRequest, map[string]string{"error": err.Error(), "code": "bad_request", "field": "days"})
				return
			}
			from, err1 := parseHHMM(body.From)
			to, err2 := parseHHMM(body.To)
			if err := firstErr(err1, err2); err != nil {
				writeJSON(w, r, http.StatusBadRequest, map[string]string{"error": err.Error(), "code": "bad_request", "field": "from"})
				return
			}
			sp.FromMin, sp.ToMin = from, to
		}
		keys, err := body.Keys.creds()
		if err != nil {
			keysErr(err)
			return
		}
		defer zeroAll(keys)
		c, err := cs.Start(ctx, sp, keys)
		if err != nil {
			storageErr(w, r, err)
			return
		}
		writeJSON(w, r, http.StatusCreated, map[string]any{"campaign": storageCampaign(c)})
	case "campaign/stop", "campaign/pause", "campaign/resume":
		if !decodeStorage(w, r, storageBody, nil, false) {
			return
		}
		f := map[string]func(context.Context, int64) (model.StorageCampaign, error){
			"campaign/stop": cs.Stop, "campaign/pause": cs.Pause, "campaign/resume": cs.Resume}[route]
		c, err := f(ctx, id)
		if err != nil {
			storageErr(w, r, err)
			return
		}
		writeJSON(w, r, http.StatusOK, map[string]any{"campaign": storageCampaign(c)})
	case "campaign/keys":
		var body struct {
			Keys storageKeysMap `json:"keys"`
		}
		if !decodeStorage(w, r, storageCampaignBody, &body, true) {
			return
		}
		keys, err := body.Keys.creds()
		if err != nil {
			keysErr(err)
			return
		}
		defer zeroAll(keys)
		if err := cs.ProvideKeys(ctx, id, keys); err != nil {
			storageErr(w, r, err)
			return
		}
		writeJSON(w, r, http.StatusOK, map[string]any{})
	}
}
