package web

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/store"
)

// API limits.
const (
	MaxRange      = 90 * 24 * time.Hour
	MaxPoints     = 2000
	DefaultPoints = 1000
	minPoints     = 10
	defaultRange  = 30 * time.Minute
	defaultIv     = 15 * time.Second
)

// errBadRequest marks client errors whose message is safe to show publicly.
type errBadRequest struct{ msg string }

func (e errBadRequest) Error() string { return e.msg }

func badReq(format string, a ...any) error { return errBadRequest{fmt.Sprintf(format, a...)} }

var errUnknownGroup = errors.New("unknown group")

// checkParams enforces the parameter whitelist: no unknown or repeated keys.
func checkParams(q url.Values, allowed ...string) error {
	for k, v := range q {
		ok := false
		for _, a := range allowed {
			if k == a {
				ok = true
				break
			}
		}
		if !ok {
			return badReq("unknown parameter %q", k)
		}
		if len(v) > 1 {
			return badReq("repeated parameter %q", k)
		}
	}
	return nil
}

// panelParams is a parsed and clamped /api/panel query.
type panelParams struct {
	group    string
	from, to time.Time
	points   int
	key      string // normalized query, for caching
}

// parseTime accepts "now", "now-<n><s|m|h|d>" and unix milliseconds. It
// returns the normalized form for the cache key.
func parseTime(s string, now time.Time) (time.Time, string, error) {
	if s == "now" {
		return now, s, nil
	}
	if rest, ok := strings.CutPrefix(s, "now-"); ok {
		d, err := parseDur(rest)
		if err != nil {
			return time.Time{}, "", err
		}
		return now.Add(-d), "now-" + strconv.FormatInt(int64(d/time.Second), 10) + "s", nil
	}
	ms, err := strconv.ParseInt(s, 10, 64)
	if err != nil || ms < 0 || ms > 1<<45 {
		return time.Time{}, "", badReq("bad time %q: want now, now-<duration> or unix milliseconds", s)
	}
	return time.UnixMilli(ms), strconv.FormatInt(ms, 10), nil
}

func parseDur(s string) (time.Duration, error) {
	if len(s) < 2 || len(s) > 8 {
		return 0, badReq("bad duration %q", s)
	}
	n, err := strconv.Atoi(s[:len(s)-1])
	if err != nil || n <= 0 {
		return 0, badReq("bad duration %q", s)
	}
	var unit time.Duration
	switch s[len(s)-1] {
	case 's':
		unit = time.Second
	case 'm':
		unit = time.Minute
	case 'h':
		unit = time.Hour
	case 'd':
		unit = 24 * time.Hour
	default:
		return 0, badReq("bad duration %q", s)
	}
	d := time.Duration(n) * unit
	if d > MaxRange {
		d = MaxRange
	}
	return d, nil
}

func parsePanelParams(q url.Values, now time.Time) (panelParams, error) {
	var p panelParams
	if err := checkParams(q, "group", "from", "to", "points"); err != nil {
		return p, err
	}
	p.group = q.Get("group")
	if p.group == "" || len(p.group) > 64 {
		return p, badReq("group is required")
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
	if p.to.After(now) {
		p.to, tk = now, "now"
	}
	if !p.from.Before(p.to) {
		return p, badReq("from must be before to")
	}
	if p.to.Sub(p.from) > MaxRange {
		p.from = p.to.Add(-MaxRange)
		fk = tk + "-90d"
	}
	p.points = DefaultPoints
	if s := q.Get("points"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil {
			return p, badReq("bad points %q", s)
		}
		p.points = min(max(n, minPoints), MaxPoints)
	}
	p.key = p.group + "\x00" + fk + "\x00" + tk + "\x00" + strconv.Itoa(p.points)
	return p, nil
}

// panelSeries is one series of a resolved group.
type panelSeries struct {
	key      model.SeriesKey
	interval time.Duration
}

func kindsOf(t model.Target) []model.ProbeKind {
	if len(t.Kinds) > 0 {
		return t.Kinds
	}
	return []model.ProbeKind{model.KindHTTPS, model.KindTCP, model.KindICMP}
}

func intervalOf(t model.Target, k model.ProbeKind) time.Duration {
	iv := t.Interval
	if iv <= 0 {
		iv = defaultIv
	}
	if k == model.KindICMP {
		iv /= 3
	}
	return iv
}

// resolveGroup returns the group and every series of its targets, in profile
// order, plus the longest probe interval (the finest useful bucket).
func resolveGroup(p *model.Profile, id string) (model.Group, []panelSeries, time.Duration, error) {
	if p == nil {
		return model.Group{}, nil, 0, errUnknownGroup
	}
	var g model.Group
	found := false
	for _, x := range p.Groups {
		if x.ID == id {
			g, found = x, true
			break
		}
	}
	if !found {
		return g, nil, 0, errUnknownGroup
	}
	var out []panelSeries
	var maxIv time.Duration
	for _, t := range p.Targets {
		if t.Group != id {
			continue
		}
		for _, k := range kindsOf(t) {
			iv := intervalOf(t, k)
			maxIv = max(maxIv, iv)
			out = append(out, panelSeries{key: model.SeriesKey{Target: t.Name, Kind: k}, interval: iv})
		}
	}
	return g, out, maxIv, nil
}

// panelData is a query result ready to render.
type panelData struct {
	group  model.Group
	params panelParams
	now    time.Time
	res    *store.PanelResult
	series []panelSeries
	cols   []*store.SeriesCols // parallel to series; nil if the store returned nothing
	gap    [][]uint32          // not-measured sample counts, parallel to series
	nb     int                 // buckets
}

// queryPanel resolves the group and runs exactly one store.Panel call.
func (d *Deps) queryPanel(ctx context.Context, p panelParams, now time.Time) (*panelData, error) {
	g, series, maxIv, err := resolveGroup(d.Profile(), p.group)
	if err != nil {
		return nil, err
	}
	points := p.points
	if maxIv > 0 {
		// Buckets finer than the probe interval would be mostly empty and
		// break every line: never ask for more buckets than samples.
		points = min(points, max(1, int(p.to.Sub(p.from)/maxIv)))
	}
	keys := make([]model.SeriesKey, len(series))
	for i, s := range series {
		keys[i] = s.key
	}
	res, err := d.Store.Panel(ctx, store.PanelQuery{Keys: keys, From: p.from, To: p.to, MaxPoints: points})
	if err != nil {
		return nil, err
	}
	// Before the store's oldest data fyisp never ran: that is not a gap.
	floor := p.from
	if st, err := d.Store.Stats(ctx); err == nil && st.Oldest.After(floor) {
		floor = st.Oldest
	}
	pd := &panelData{group: g, params: p, now: now, res: res, series: series}
	byKey := make(map[model.SeriesKey]*store.SeriesCols, len(res.Series))
	for i := range res.Series {
		byKey[res.Series[i].Key] = &res.Series[i]
		pd.nb = max(pd.nb, len(res.Series[i].Mean))
	}
	pd.cols = make([]*store.SeriesCols, len(series))
	pd.gap = make([][]uint32, len(series))
	for i, s := range series {
		c := byKey[s.key]
		pd.cols[i] = c
		pd.gap[i] = notMeasured(res, c, s.interval, floor, p.to, now, pd.nb)
	}
	return pd, nil
}

// notMeasured estimates, per bucket, how many scheduled samples are missing
// entirely (neither a result nor a loss): fyisp was not running, the machine
// slept, or the clock jumped. It counts only whole slots inside [from, to),
// older than one interval, so it never over-reports.
func notMeasured(res *store.PanelResult, c *store.SeriesCols, iv time.Duration, from, to, now time.Time, nb int) []uint32 {
	gap := make([]uint32, nb)
	if res.Step <= 0 || iv <= 0 {
		return gap
	}
	settled := now.Add(-iv)
	for i := range nb {
		bs := res.Start.Add(time.Duration(i) * res.Step)
		be := bs.Add(res.Step)
		lo := later(bs, from)
		hi := earlier(earlier(be, to), settled)
		if !hi.After(lo) {
			continue
		}
		expected := int64(hi.Sub(lo) / iv)
		var got int64
		if c != nil {
			got = int64(at(c.N, i)) + int64(at(c.Lost, i))
		}
		miss := expected - got
		if miss == 1 && expected >= 4 {
			miss = 0 // slot phase jitter at bucket edges
		}
		if miss > 0 {
			gap[i] = uint32(miss)
		}
	}
	return gap
}

func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func earlier(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func at[T any](s []T, i int) T {
	var z T
	if i < len(s) {
		return s[i]
	}
	return z
}

// reasonSlugs names loss reasons in the API. Codes without a name (9-14)
// are reported as "other".
var reasonSlugs = []struct {
	r    model.Reason
	slug string
}{
	{model.ReasonTimeout, "timeout"},
	{model.ReasonRefused, "refused"},
	{model.ReasonReset, "reset"},
	{model.ReasonUnreachable, "unreachable"},
	{model.ReasonDNS, "dns"},
	{model.ReasonTLS, "tls"},
	{model.ReasonHTTP, "http"},
	{model.ReasonNoNetwork, "no_network"},
	{model.ReasonOther, "other"},
}

// lostBy folds a series' per-reason counts into the API's reason slugs.
func lostBy(c *store.SeriesCols, nb int) map[string][]uint32 {
	out := map[string][]uint32{}
	if c == nil {
		return out
	}
	for r, v := range c.LostBy {
		if r == model.ReasonGap {
			continue
		}
		slug := "other"
		for _, x := range reasonSlugs {
			if x.r == r {
				slug = x.slug
			}
		}
		dst := out[slug]
		if dst == nil {
			dst = make([]uint32, nb)
			out[slug] = dst
		}
		for i := range min(nb, len(v)) {
			dst[i] += v[i]
		}
	}
	for k, v := range out {
		if allZero(v) {
			delete(out, k)
		}
	}
	return out
}

func allZero(v []uint32) bool {
	for _, x := range v {
		if x != 0 {
			return false
		}
	}
	return true
}

// appendF appends a float as JSON: NaN and ±Inf become null, values are
// rounded to 1 µs (storage resolution is 10 µs).
func appendF(b []byte, v float32) []byte {
	f := float64(v)
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return append(b, "null"...)
	}
	return strconv.AppendFloat(b, math.Round(f*1000)/1000, 'f', -1, 64)
}

func appendFloats(b []byte, v []float32, n int) []byte {
	b = append(b, '[')
	for i := range n {
		if i > 0 {
			b = append(b, ',')
		}
		if i < len(v) {
			b = appendF(b, v[i])
		} else {
			b = append(b, "null"...)
		}
	}
	return append(b, ']')
}

func appendUints(b []byte, v []uint32, n int) []byte {
	b = append(b, '[')
	for i := range n {
		if i > 0 {
			b = append(b, ',')
		}
		b = strconv.AppendUint(b, uint64(at(v, i)), 10)
	}
	return append(b, ']')
}

func appendStr(b []byte, s string) []byte {
	j, _ := json.Marshal(s)
	return append(b, j...)
}

// panelJSON renders the columnar panel response. Bucket i starts at
// start + i*step (milliseconds since the epoch).
func (pd *panelData) JSON() []byte {
	b := make([]byte, 0, 4096)
	b = append(b, `{"group":{"id":`...)
	b = appendStr(b, pd.group.ID)
	b = append(b, `,"title":`...)
	b = appendStr(b, pd.group.Title)
	b = append(b, `},"tier":`...)
	b = appendStr(b, pd.res.Tier)
	b = fmt.Appendf(b, `,"from":%d,"to":%d,"now":%d,"start":%d,"step":%d,"len":%d,"series":[`,
		pd.params.from.UnixMilli(), pd.params.to.UnixMilli(), pd.now.UnixMilli(),
		pd.res.Start.UnixMilli(), pd.res.Step.Milliseconds(), pd.nb)
	for i, s := range pd.series {
		c := pd.cols[i]
		if c == nil {
			c = &store.SeriesCols{}
		}
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, `{"target":`...)
		b = appendStr(b, s.key.Target)
		b = append(b, `,"kind":`...)
		b = appendStr(b, s.key.Kind.String())
		b = fmt.Appendf(b, `,"interval":%d,"mean":`, s.interval.Milliseconds())
		b = appendFloats(b, c.Mean, pd.nb)
		b = append(b, `,"min":`...)
		b = appendFloats(b, c.Min, pd.nb)
		b = append(b, `,"max":`...)
		b = appendFloats(b, c.Max, pd.nb)
		b = append(b, `,"n":`...)
		b = appendUints(b, c.N, pd.nb)
		b = append(b, `,"lost":`...)
		b = appendUints(b, c.Lost, pd.nb)
		b = append(b, `,"gap":`...)
		b = appendUints(b, pd.gap[i], pd.nb)
		b = append(b, `,"lost_by":{`...)
		lb := lostBy(pd.cols[i], pd.nb)
		first := true
		for _, x := range reasonSlugs {
			v, ok := lb[x.slug]
			if !ok {
				continue
			}
			if !first {
				b = append(b, ',')
			}
			first = false
			b = appendStr(b, x.slug)
			b = append(b, ':')
			b = appendUints(b, v, pd.nb)
		}
		b = append(b, "}}"...)
	}
	return append(b, "]}\n"...)
}

// csvSafe defuses spreadsheet formula injection.
func csvSafe(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}

func fmtCSV(v float32) string {
	f := float64(v)
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return ""
	}
	return strconv.FormatFloat(math.Round(f*1000)/1000, 'f', -1, 64)
}

// CSV renders one row per series and bucket.
func (pd *panelData) CSV() []byte {
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	head := []string{"bucket_start", "target", "kind", "mean_ms", "min_ms", "max_ms", "samples", "lost", "not_measured"}
	for _, x := range reasonSlugs {
		head = append(head, "lost_"+x.slug)
	}
	_ = w.Write(head)
	row := make([]string, len(head))
	for i, s := range pd.series {
		c := pd.cols[i]
		if c == nil {
			c = &store.SeriesCols{}
		}
		lb := lostBy(pd.cols[i], pd.nb)
		for j := range pd.nb {
			row[0] = pd.res.Start.Add(time.Duration(j) * pd.res.Step).UTC().Format(time.RFC3339)
			row[1] = csvSafe(s.key.Target)
			row[2] = s.key.Kind.String()
			row[3] = fmtCSV(at(c.Mean, j))
			row[4] = fmtCSV(at(c.Min, j))
			row[5] = fmtCSV(at(c.Max, j))
			if j >= len(c.Mean) {
				row[3], row[4], row[5] = "", "", ""
			}
			row[6] = strconv.FormatUint(uint64(at(c.N, j)), 10)
			row[7] = strconv.FormatUint(uint64(at(c.Lost, j)), 10)
			row[8] = strconv.FormatUint(uint64(pd.gap[i][j]), 10)
			for k, x := range reasonSlugs {
				row[9+k] = strconv.FormatUint(uint64(at(lb[x.slug], j)), 10)
			}
			_ = w.Write(row)
		}
	}
	w.Flush()
	return buf.Bytes()
}

func (pd *panelData) csvName() string {
	safe := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, pd.group.ID)
	return fmt.Sprintf("fyisp-%s-%s-%s.csv", safe,
		pd.params.from.UTC().Format("20060102T150405Z"), pd.params.to.UTC().Format("20060102T150405Z"))
}

// profileJSON is /api/profile: names, groups and kinds only (never hosts,
// ports or paths).
type profileJSON struct {
	Name    string          `json:"name"`
	Groups  []groupJSON     `json:"groups"`
	Targets []targetSummary `json:"targets"`
}

type groupJSON struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

type targetSummary struct {
	Name     string   `json:"name"`
	Group    string   `json:"group"`
	Kinds    []string `json:"kinds"`
	Interval int64    `json:"interval_ms"`
}

func buildProfile(p *model.Profile) profileJSON {
	out := profileJSON{Groups: []groupJSON{}, Targets: []targetSummary{}}
	if p == nil {
		return out
	}
	out.Name = p.Name
	gs := append([]model.Group(nil), p.Groups...)
	// Stable sort by Order, keeping file order for ties.
	for i := 1; i < len(gs); i++ {
		for j := i; j > 0 && gs[j].Order < gs[j-1].Order; j-- {
			gs[j], gs[j-1] = gs[j-1], gs[j]
		}
	}
	for _, g := range gs {
		out.Groups = append(out.Groups, groupJSON{ID: g.ID, Title: g.Title})
	}
	for _, t := range p.Targets {
		ts := targetSummary{Name: t.Name, Group: t.Group, Interval: intervalOf(t, model.KindHTTPS).Milliseconds()}
		for _, k := range kindsOf(t) {
			ts.Kinds = append(ts.Kinds, k.String())
		}
		out.Targets = append(out.Targets, ts)
	}
	return out
}

// ---- response helpers ----

func setSecurityHeaders(h http.Header) {
	h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Cross-Origin-Opener-Policy", "same-origin")
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
}

// writeBody writes b, gzipped when the client accepts it and it is worth it.
func writeBody(w http.ResponseWriter, r *http.Request, code int, ctype string, b []byte) {
	h := w.Header()
	h.Set("Content-Type", ctype)
	h.Add("Vary", "Accept-Encoding")
	if len(b) > 1024 && acceptsGzip(r) {
		var buf bytes.Buffer
		zw, _ := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
		_, _ = zw.Write(b)
		_ = zw.Close()
		b = buf.Bytes()
		h.Set("Content-Encoding", "gzip")
	}
	h.Set("Content-Length", strconv.Itoa(len(b)))
	w.WriteHeader(code)
	if r.Method != http.MethodHead {
		_, _ = w.Write(b)
	}
}

func acceptsGzip(r *http.Request) bool {
	for _, part := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		if strings.TrimSpace(strings.SplitN(part, ";", 2)[0]) == "gzip" {
			return true
		}
	}
	return false
}

func writeJSON(w http.ResponseWriter, r *http.Request, code int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		code, b = http.StatusInternalServerError, []byte(`{"error":"internal error"}`)
	}
	w.Header().Set("Cache-Control", "no-store")
	writeBody(w, r, code, "application/json; charset=utf-8", append(b, '\n'))
}

func writeErr(w http.ResponseWriter, r *http.Request, code int, msg string) {
	writeJSON(w, r, code, map[string]string{"error": msg})
}
