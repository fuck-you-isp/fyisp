package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"net/netip"
	"slices"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

// Traceroutes (migration v3). *SQLite implements trace.Sink:
//
//   - Observe stores per-hop samples like any other series, keyed
//     {Target, KindTrace, Hop}; Options.Interval must return the tracer's
//     round interval for them (default 5s).
//   - ObserveHop, ObserveRoute and ObserveRouteChange only touch memory, like
//     Observe; Flush writes them in its transaction.
//
// The hop_info table holds one row per router address. Repeated sightings of
// an address are coalesced: last_seen is rewritten at most every
// HopSeenInterval per address, or sooner when its rdns/asn/owner change.

// HopSeenInterval is how stale hop_info.last_seen may get before a sighting
// is written again.
const HopSeenInterval = 10 * time.Minute

// Loss classification thresholds (see HopStat).
const (
	// HopLossMin is the loss fraction from which a hop's loss is
	// considered at all (5 percentage points).
	HopLossMin = 0.05
	// HopLossContinueRatio: a hop's loss is real when the destination
	// shows at least this share of it.
	HopLossContinueRatio = 0.5
)

// Memory bounds for the trace queues. Past them the oldest route changes are
// dropped (counted in Stats.Dropped) and the hop coalescing memory restarts.
const (
	maxQueuedChanges = 10000
	maxHopMarks      = 4096
)

// HopStat describes one hop (TTL) of a traced target over a time range.
type HopStat struct {
	Hop int `json:"hop"` // TTL, 1 = first hop
	// IP is the address at this TTL in the current route (invalid: the hop
	// does not answer, or the current route is shorter or unknown).
	IP netip.Addr `json:"ip"`
	// MostCommonIP is the address seen at this TTL for the longest time
	// within the range (from the current route and the route changes).
	MostCommonIP netip.Addr `json:"most_common_ip"`
	// Info describes IP (or MostCommonIP when IP is invalid), if known.
	Info *model.HopInfo `json:"info,omitempty"`

	N    int64   `json:"n"`    // answered probes
	Lost int64   `json:"lost"` // unanswered probes (not-measured slots excluded)
	Loss float64 `json:"loss"` // Lost / (N + Lost), 0 without probes
	// RTTs in milliseconds, 0 when N == 0. In the hourly tier (ranges over
	// RawMaxRange) P95Ms is the n-weighted 95th percentile of the hourly
	// p95s and JitterMs is 0 (not available).
	MinMs    float64 `json:"min_ms"`
	MeanMs   float64 `json:"mean_ms"`
	MaxMs    float64 `json:"max_ms"`
	P95Ms    float64 `json:"p95_ms"`
	JitterMs float64 `json:"jitter_ms"` // mean |difference| of consecutive answered RTTs

	// LossContinues: this hop's loss is at least HopLossMin and the
	// destination (the highest TTL with probes in the range) loses at least
	// HopLossContinueRatio of it: real loss that starts at or before this
	// hop.
	LossContinues bool `json:"loss_continues"`
	// RateLimited: this hop loses at least HopLossMin but the loss does not
	// continue to the destination, so it is the router rate-limiting or
	// deprioritizing ICMP replies, not loss of forwarded traffic.
	RateLimited bool `json:"rate_limited"`
}

// TraceReader serves the Investigate view. *SQLite and *Fake implement it.
// Per-hop timelines use Reader.Panel with keys {target, KindTrace, hop}.
type TraceReader interface {
	// Hops returns per-hop statistics of target over [from, to], by TTL.
	Hops(ctx context.Context, target string, from, to time.Time) ([]HopStat, error)
	// Route returns target's current route.
	Route(ctx context.Context, target string) (model.Route, bool)
	// RouteChanges returns the route changes of every target in [from, to],
	// newest first. Changes not yet flushed have ID 0.
	RouteChanges(ctx context.Context, from, to time.Time) ([]model.RouteChange, error)
	// HopInfo returns what is known about a router address.
	HopInfo(ctx context.Context, ip netip.Addr) (model.HopInfo, bool, error)
}

var (
	_ TraceReader = (*SQLite)(nil)
	_ TraceReader = (*Fake)(nil)
)

// traceState is the SQLite's trace memory, guarded by SQLite.mu.
type traceState struct {
	seq     int64                        // bumped for every queued item
	hops    map[netip.Addr]pendingHop    // hop_info upserts to write
	routes  map[string]pendingRoute      // routes rows to write
	changes []pendingChange              // route_changes rows to write, oldest first
	marks   map[netip.Addr]model.HopInfo // last queued state per address (coalescing)
	cur     map[string]model.Route       // current route per target (writer only)
}

type pendingHop struct {
	info model.HopInfo
	seq  int64
}

type pendingRoute struct {
	r   model.Route
	seq int64
}

type pendingChange struct {
	c   model.RouteChange
	seq int64
}

func newTraceState() traceState {
	return traceState{
		hops:   map[netip.Addr]pendingHop{},
		routes: map[string]pendingRoute{},
		marks:  map[netip.Addr]model.HopInfo{},
		cur:    map[string]model.Route{},
	}
}

// traceSnap is what one Flush writes.
type traceSnap struct {
	hops    []pendingHop
	routes  []pendingRoute
	changes []pendingChange
}

func (t *traceSnap) empty() bool {
	return t == nil || len(t.hops)+len(t.routes)+len(t.changes) == 0
}

func (t *traceState) snapshotLocked() *traceSnap {
	sn := &traceSnap{changes: slices.Clone(t.changes)}
	for _, h := range t.hops {
		sn.hops = append(sn.hops, h)
	}
	for _, r := range t.routes {
		sn.routes = append(sn.routes, r)
	}
	return sn
}

// committedLocked drops the written items that were not replaced since.
func (t *traceState) committedLocked(sn *traceSnap) {
	if sn == nil {
		return
	}
	for _, h := range sn.hops {
		if p, ok := t.hops[h.info.IP]; ok && p.seq == h.seq {
			delete(t.hops, h.info.IP)
		}
	}
	for _, r := range sn.routes {
		if p, ok := t.routes[r.r.Target]; ok && p.seq == r.seq {
			delete(t.routes, r.r.Target)
		}
	}
	if n := len(sn.changes); n > 0 {
		last := sn.changes[n-1].seq
		t.changes = slices.DeleteFunc(t.changes, func(c pendingChange) bool { return c.seq <= last })
	}
}

// mergeHop combines two records of the same address: the widest seen range,
// newer non-empty metadata wins.
func mergeHop(old, nu model.HopInfo) model.HopInfo {
	out := old
	if !nu.FirstSeen.IsZero() && (out.FirstSeen.IsZero() || nu.FirstSeen.Before(out.FirstSeen)) {
		out.FirstSeen = nu.FirstSeen
	}
	if nu.LastSeen.After(out.LastSeen) {
		out.LastSeen = nu.LastSeen
	}
	if nu.RDNS != "" {
		out.RDNS = nu.RDNS
	}
	if nu.ASN != 0 {
		out.ASN = nu.ASN
	}
	if nu.Owner != "" {
		out.Owner = nu.Owner
	}
	return out
}

// newMeta reports whether nu carries metadata that m does not have.
func newMeta(m, nu model.HopInfo) bool {
	return (nu.RDNS != "" && nu.RDNS != m.RDNS) || (nu.ASN != 0 && nu.ASN != m.ASN) || (nu.Owner != "" && nu.Owner != m.Owner)
}

// ObserveHop records that a router address was seen. It never blocks on I/O.
// A zero LastSeen means Options.Now; a zero FirstSeen means LastSeen.
func (s *SQLite) ObserveHop(info model.HopInfo) {
	if !info.IP.IsValid() {
		return
	}
	info.IP = info.IP.Unmap()
	if info.LastSeen.IsZero() {
		info.LastSeen = s.o.Now()
	}
	if info.FirstSeen.IsZero() || info.FirstSeen.After(info.LastSeen) {
		info.FirstSeen = info.LastSeen
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.lock == nil {
		return
	}
	t := &s.tr
	m, ok := t.marks[info.IP]
	if ok && !newMeta(m, info) && info.LastSeen.Sub(m.LastSeen) < HopSeenInterval {
		return
	}
	if len(t.marks) >= maxHopMarks && !ok {
		clear(t.marks) // costs only extra writes
	}
	if ok {
		t.marks[info.IP] = mergeHop(m, info)
	} else {
		t.marks[info.IP] = info
	}
	p := t.hops[info.IP]
	if p.seq != 0 {
		info = mergeHop(p.info, info)
	}
	t.seq++
	t.hops[info.IP] = pendingHop{info, t.seq}
}

// ObserveRoute records target's current route; it is written only when it
// differs from the last one. A zero Since means Options.Now.
func (s *SQLite) ObserveRoute(r model.Route) {
	if r.Since.IsZero() {
		r.Since = s.o.Now()
	}
	r.Hops = slices.Clone(r.Hops)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.lock == nil {
		return
	}
	t := &s.tr
	if cur, ok := t.cur[r.Target]; ok && slices.Equal(cur.Hops, r.Hops) {
		return
	}
	t.cur[r.Target] = r
	t.seq++
	t.routes[r.Target] = pendingRoute{r, t.seq}
}

// ObserveRouteChange records a route change. A zero At means Options.Now.
func (s *SQLite) ObserveRouteChange(c model.RouteChange) {
	if c.At.IsZero() {
		c.At = s.o.Now()
	}
	c.ID = 0
	c.From, c.To = slices.Clone(c.From), slices.Clone(c.To)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.lock == nil {
		return
	}
	t := &s.tr
	if len(t.changes) >= maxQueuedChanges {
		t.changes = t.changes[1:]
		s.dropped++
	}
	t.seq++
	t.changes = append(t.changes, pendingChange{c, t.seq})
}

// loadTrace restores the current routes and the coalescing memory.
func (s *SQLite) loadTrace(ctx context.Context) error {
	rows, err := s.db.w.QueryContext(ctx, `SELECT target, hops, since_ms FROM routes`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var r model.Route
		var hops string
		var since int64
		if err := rows.Scan(&r.Target, &hops, &since); err != nil {
			rows.Close()
			return err
		}
		if r.Hops, err = decodeHops(hops); err != nil {
			rows.Close()
			return fmt.Errorf("store: route of %q: %w", r.Target, err)
		}
		r.Since = time.UnixMilli(since).UTC()
		s.tr.cur[r.Target] = r
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	return scanHopInfo(ctx, s.db.w, `ORDER BY last_seen_ms DESC LIMIT ?`, func(h model.HopInfo) {
		s.tr.marks[h.IP] = h
	}, maxHopMarks)
}

func encodeHops(hops []netip.Addr) string {
	ss := make([]string, len(hops))
	for i, h := range hops {
		if h.IsValid() {
			ss[i] = h.String()
		}
	}
	b, _ := json.Marshal(ss)
	return string(b)
}

func decodeHops(s string) ([]netip.Addr, error) {
	var ss []string
	if err := json.Unmarshal([]byte(s), &ss); err != nil {
		return nil, err
	}
	out := make([]netip.Addr, len(ss))
	for i, x := range ss {
		if x == "" {
			continue
		}
		a, err := netip.ParseAddr(x)
		if err != nil {
			return nil, err
		}
		out[i] = a
	}
	return out, nil
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// writeTrace writes a trace snapshot inside Flush's transaction.
func writeTrace(ctx context.Context, tx *sql.Tx, sn *traceSnap) error {
	if sn.empty() {
		return nil
	}
	for _, h := range sn.hops {
		i := h.info
		var asn any
		if i.ASN != 0 {
			asn = int64(i.ASN)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO hop_info(ip, rdns, asn, owner, first_seen_ms, last_seen_ms) VALUES(?,?,?,?,?,?)
			ON CONFLICT(ip) DO UPDATE SET
				rdns = coalesce(excluded.rdns, rdns),
				asn = coalesce(excluded.asn, asn),
				owner = coalesce(excluded.owner, owner),
				first_seen_ms = min(first_seen_ms, excluded.first_seen_ms),
				last_seen_ms = max(last_seen_ms, excluded.last_seen_ms)`,
			i.IP.String(), nullStr(i.RDNS), asn, nullStr(i.Owner), i.FirstSeen.UnixMilli(), i.LastSeen.UnixMilli()); err != nil {
			return fmt.Errorf("store: hop_info: %w", err)
		}
	}
	for _, r := range sn.routes {
		if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO routes(target, hops, since_ms) VALUES(?,?,?)`,
			r.r.Target, encodeHops(r.r.Hops), r.r.Since.UnixMilli()); err != nil {
			return fmt.Errorf("store: routes: %w", err)
		}
	}
	for _, c := range sn.changes {
		if _, err := tx.ExecContext(ctx, `INSERT INTO route_changes(target, at_ms, from_hops, to_hops, first_diff) VALUES(?,?,?,?,?)`,
			c.c.Target, c.c.At.UnixMilli(), encodeHops(c.c.From), encodeHops(c.c.To), c.c.FirstDiff); err != nil {
			return fmt.Errorf("store: route_changes: %w", err)
		}
	}
	return nil
}

func scanHopInfo(ctx context.Context, q queryer, where string, fn func(model.HopInfo), args ...any) error {
	rows, err := q.QueryContext(ctx, `SELECT ip, rdns, asn, owner, first_seen_ms, last_seen_ms FROM hop_info `+where, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			ip          string
			rdns, owner sql.NullString
			asn         sql.NullInt64
			first, last int64
		)
		if err := rows.Scan(&ip, &rdns, &asn, &owner, &first, &last); err != nil {
			return err
		}
		a, err := netip.ParseAddr(ip)
		if err != nil {
			continue
		}
		fn(model.HopInfo{IP: a, RDNS: rdns.String, ASN: uint32(asn.Int64), Owner: owner.String,
			FirstSeen: time.UnixMilli(first).UTC(), LastSeen: time.UnixMilli(last).UTC()})
	}
	return rows.Err()
}

// readTx opens a read transaction whose snapshot agrees with the memory
// read by mem (called with SQLite.mu held): see snapshot in reader.go.
func (s *SQLite) readTx(ctx context.Context, mem func()) (*sql.Tx, error) {
	tx, err := s.db.r.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	s.commitMu.RLock()
	s.mu.Lock()
	mem()
	s.mu.Unlock()
	var n int64
	err = tx.QueryRowContext(ctx, `SELECT count(*) FROM routes`).Scan(&n) // pins the read snapshot
	s.commitMu.RUnlock()
	if err != nil {
		tx.Rollback()
		return nil, err
	}
	return tx, nil
}

// Route returns target's current route: from memory in the running
// instance, from the database when read-only.
func (s *SQLite) Route(ctx context.Context, target string) (model.Route, bool) {
	if s.lock != nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		r, ok := s.tr.cur[target]
		r.Hops = slices.Clone(r.Hops)
		return r, ok
	}
	var hops string
	var since int64
	if err := s.db.r.QueryRowContext(ctx, `SELECT hops, since_ms FROM routes WHERE target = ?`, target).Scan(&hops, &since); err != nil {
		return model.Route{}, false
	}
	h, err := decodeHops(hops)
	if err != nil {
		return model.Route{}, false
	}
	return model.Route{Target: target, Hops: h, Since: time.UnixMilli(since).UTC()}, true
}

// RouteChanges returns the route changes in [from, to], newest first,
// including those not flushed yet (ID 0).
func (s *SQLite) RouteChanges(ctx context.Context, from, to time.Time) ([]model.RouteChange, error) {
	return s.routeChanges(ctx, "", from, to)
}

// routeChanges is RouteChanges limited to one target (all when "").
func (s *SQLite) routeChanges(ctx context.Context, target string, from, to time.Time) ([]model.RouteChange, error) {
	fromMs, toMs := msRange(from, to)
	var out []model.RouteChange
	tx, err := s.readTx(ctx, func() {
		for _, p := range s.tr.changes {
			c := p.c
			if at := c.At.UnixMilli(); at >= fromMs && at <= toMs && (target == "" || c.Target == target) {
				c.At = time.UnixMilli(at).UTC()
				out = append(out, c)
			}
		}
	})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	q := `SELECT id, target, at_ms, from_hops, to_hops, first_diff FROM route_changes WHERE at_ms BETWEEN ? AND ?`
	args := []any{fromMs, toMs}
	if target != "" {
		q += ` AND target = ?`
		args = append(args, target)
	}
	rows, err := tx.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var c model.RouteChange
		var at int64
		var fh, th string
		if err := rows.Scan(&c.ID, &c.Target, &at, &fh, &th, &c.FirstDiff); err != nil {
			return nil, err
		}
		c.At = time.UnixMilli(at).UTC()
		if c.From, err = decodeHops(fh); err != nil {
			return nil, fmt.Errorf("store: route change %d: %w", c.ID, err)
		}
		if c.To, err = decodeHops(th); err != nil {
			return nil, fmt.Errorf("store: route change %d: %w", c.ID, err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sortChanges(out)
	return out, nil
}

// sortChanges orders newest first; unflushed changes (ID 0) are newer than
// flushed ones at the same time.
func sortChanges(cs []model.RouteChange) {
	slices.SortStableFunc(cs, func(a, b model.RouteChange) int {
		if c := b.At.Compare(a.At); c != 0 {
			return c
		}
		switch {
		case a.ID == 0 && b.ID != 0:
			return -1
		case b.ID == 0 && a.ID != 0:
			return 1
		}
		return int(b.ID - a.ID)
	})
}

// HopInfo returns what is known about ip, including sightings not flushed
// yet.
func (s *SQLite) HopInfo(ctx context.Context, ip netip.Addr) (model.HopInfo, bool, error) {
	m, err := s.hopInfos(ctx, []netip.Addr{ip})
	if err != nil {
		return model.HopInfo{}, false, err
	}
	h, ok := m[ip.Unmap()]
	return h, ok, nil
}

func (s *SQLite) hopInfos(ctx context.Context, ips []netip.Addr) (map[netip.Addr]model.HopInfo, error) {
	out := map[netip.Addr]model.HopInfo{}
	pend := map[netip.Addr]model.HopInfo{}
	var args []any
	for _, ip := range ips {
		if ip.IsValid() {
			args = append(args, ip.Unmap().String())
		}
	}
	if len(args) == 0 {
		return out, nil
	}
	tx, err := s.readTx(ctx, func() {
		for _, ip := range ips {
			if p, ok := s.tr.hops[ip.Unmap()]; ok {
				pend[p.info.IP] = p.info
			}
		}
	})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := scanHopInfo(ctx, tx, `WHERE ip IN (`+inList(len(args))+`)`, func(h model.HopInfo) { out[h.IP] = h }, args...); err != nil {
		return nil, err
	}
	for ip, p := range pend {
		if h, ok := out[ip]; ok {
			out[ip] = mergeHop(h, p)
		} else {
			out[ip] = p
		}
	}
	return out, nil
}

// traceKeys returns the KindTrace series of target known to the store.
func (s *SQLite) traceKeys(target string) []model.SeriesKey {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := map[model.SeriesKey]bool{}
	add := func(k model.SeriesKey) {
		if k.Target == target && k.Kind == model.KindTrace {
			seen[k] = true
		}
	}
	for k := range s.ids {
		add(k)
	}
	for k := range s.cur {
		add(k)
	}
	for _, b := range s.pending {
		add(b.key)
	}
	return sortedKeys(seen)
}

func sortedKeys(m map[model.SeriesKey]bool) []model.SeriesKey {
	out := make([]model.SeriesKey, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.SortFunc(out, func(a, b model.SeriesKey) int { return int(a.Hop) - int(b.Hop) })
	return out
}

// Hops returns per-hop statistics of target over [from, to]: raw samples
// for ranges up to RawMaxRange, else the hourly summaries (see HopStat).
func (s *SQLite) Hops(ctx context.Context, target string, from, to time.Time) ([]HopStat, error) {
	if to.Before(from) {
		return nil, fmt.Errorf("store: range ends before it starts")
	}
	keys := s.traceKeys(target)
	accs := map[uint8]*hopAcc{}
	if to.Sub(from) <= RawMaxRange {
		err := s.Raw(ctx, keys, from, to, func(p RawPoint) error {
			a := accs[p.Key.Hop]
			if a == nil {
				a = &hopAcc{}
				accs[p.Key.Hop] = a
			}
			a.addRaw(p)
			return nil
		})
		if err != nil {
			return nil, err
		}
	} else if err := s.hourlySummaries(ctx, keys, from, to, func(k model.SeriesKey, u *summary) {
		a := accs[k.Hop]
		if a == nil {
			a = &hopAcc{}
			accs[k.Hop] = a
		}
		a.addSummary(u)
	}); err != nil {
		return nil, err
	}
	route, haveRoute := s.Route(ctx, target)
	changes, err := s.routeChanges(ctx, target, from, time.UnixMilli(math.MaxInt64/2))
	if err != nil {
		return nil, err
	}
	return buildHopStats(ctx, accs, route, haveRoute, changes, from, to, s.hopInfos)
}

// hourlySummaries calls fn with every hourly summary of keys whose hour
// starts in [from, to] (in memory or stored), with one SQL query.
func (s *SQLite) hourlySummaries(ctx context.Context, keys []model.SeriesKey, from, to time.Time, fn func(model.SeriesKey, *summary)) error {
	if len(keys) == 0 {
		return nil
	}
	fromMs, toMs := msRange(from, to)
	hFrom, hTo := floorHour(fromMs), floorHour(toMs)
	if hFrom*hourMs < fromMs {
		hFrom++ // an hour belongs to the range if its start does
	}
	s.commitMu.RLock()
	mem, ids := s.snapshot(keys, hFrom, hTo)
	var rows *sql.Rows
	var more bool
	var err error
	keyOf := map[int64]model.SeriesKey{}
	if len(ids) > 0 {
		args := []any{floorDiv(hFrom, 24), floorDiv(hTo, 24)}
		for k, id := range ids {
			args = append(args, id)
			keyOf[id] = k
		}
		rows, err = s.db.r.QueryContext(ctx, `SELECT day, series, data FROM summary_1h WHERE day BETWEEN ? AND ? AND series IN (`+inList(len(ids))+`)`, args...)
		if err == nil {
			more = rows.Next()
		}
	}
	s.commitMu.RUnlock()
	if err != nil {
		return err
	}
	if rows != nil {
		defer rows.Close()
	}
	var hs []hourSummary
	for ; more; more = rows.Next() {
		var day, id int64
		var data []byte
		if err := rows.Scan(&day, &id, &data); err != nil {
			return err
		}
		if hs, err = decodeDay(hs[:0], day, data); err != nil {
			return fmt.Errorf("store: day %d series %d: %w", day, id, err)
		}
		k := keyOf[id]
		for i := range hs {
			h := &hs[i]
			if h.hour < hFrom || h.hour > hTo {
				continue
			}
			if _, ok := mem[hourKey{k, h.hour}]; ok {
				continue
			}
			fn(k, &h.s)
		}
	}
	if rows != nil {
		if err := rows.Err(); err != nil {
			return err
		}
	}
	for _, m := range mem {
		if m.hour >= hFrom {
			fn(m.key, m.summary())
		}
	}
	return nil
}

// hopAcc accumulates one hop's samples.
type hopAcc struct {
	n, lost  int64
	sum      float64 // ms
	min, max float64
	vals     []float64 // raw tier: every RTT, for p95
	p95s     []weightedP95
	jitSum   float64
	jitN     int64
	prev     float64
	hasPrev  bool
}

type weightedP95 struct {
	p95 float64
	n   int64
}

func (a *hopAcc) addRTT(ms float64) {
	if a.n == 0 || ms < a.min {
		a.min = ms
	}
	if a.n == 0 || ms > a.max {
		a.max = ms
	}
	a.n++
	a.sum += ms
}

func (a *hopAcc) addRaw(p RawPoint) {
	switch {
	case !p.Lost:
		a.addRTT(p.RTTms)
		a.vals = append(a.vals, p.RTTms)
		if a.hasPrev {
			a.jitSum += math.Abs(p.RTTms - a.prev)
			a.jitN++
		}
		a.prev, a.hasPrev = p.RTTms, true
	case p.Reason == model.ReasonGap:
		a.hasPrev = false // not measured: the next RTT starts a new run
	default:
		a.lost++
	}
}

func (a *hopAcc) addSummary(u *summary) {
	a.lost += u.lost
	if u.n == 0 {
		return
	}
	lo, hi := unitMs(u.min), unitMs(u.max)
	if a.n == 0 || lo < a.min {
		a.min = lo
	}
	if a.n == 0 || hi > a.max {
		a.max = hi
	}
	a.n += u.n
	a.sum += unitMs(u.sum)
	a.p95s = append(a.p95s, weightedP95{unitMs(u.p95), u.n})
}

func (a *hopAcc) stat(hop int) HopStat {
	st := HopStat{Hop: hop, N: a.n, Lost: a.lost}
	if t := a.n + a.lost; t > 0 {
		st.Loss = float64(a.lost) / float64(t)
	}
	if a.n == 0 {
		return st
	}
	st.MinMs, st.MaxMs, st.MeanMs = a.min, a.max, a.sum/float64(a.n)
	if len(a.vals) > 0 {
		v := slices.Clone(a.vals)
		slices.Sort(v)
		st.P95Ms = v[(len(v)*95+99)/100-1] // as summarize
	} else if len(a.p95s) > 0 {
		ps := slices.Clone(a.p95s)
		slices.SortFunc(ps, func(x, y weightedP95) int { return cmpF(x.p95, y.p95) })
		need := (a.n*95 + 99) / 100
		var c int64
		for _, p := range ps {
			c += p.n
			st.P95Ms = p.p95
			if c >= need {
				break
			}
		}
	}
	if a.jitN > 0 {
		st.JitterMs = a.jitSum / float64(a.jitN)
	}
	return st
}

func cmpF(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// buildHopStats turns per-hop accumulators into HopStats: the TTLs with
// samples plus those of the current route, their addresses (current and most
// common over the range, from route and changes, newest first) and the loss
// classification.
func buildHopStats(ctx context.Context, accs map[uint8]*hopAcc, route model.Route, haveRoute bool, changes []model.RouteChange,
	from, to time.Time, infos func(context.Context, []netip.Addr) (map[netip.Addr]model.HopInfo, error)) ([]HopStat, error) {
	maxTTL := 0
	for h := range accs {
		maxTTL = max(maxTTL, int(h))
	}
	if haveRoute {
		maxTTL = max(maxTTL, len(route.Hops))
	}
	common := mostCommonHops(route, haveRoute, changes, from, to)
	var out []HopStat
	var lookup []netip.Addr
	for ttl := 1; ttl <= maxTTL; ttl++ {
		a, ok := accs[uint8(ttl)]
		inRoute := haveRoute && ttl <= len(route.Hops)
		if !ok && !inRoute {
			continue
		}
		if !ok {
			a = &hopAcc{}
		}
		st := a.stat(ttl)
		if inRoute {
			st.IP = route.Hops[ttl-1]
		}
		if ttl <= len(common) {
			st.MostCommonIP = common[ttl-1]
		}
		for _, ip := range []netip.Addr{st.IP, st.MostCommonIP} {
			if ip.IsValid() {
				lookup = append(lookup, ip)
			}
		}
		out = append(out, st)
	}
	classifyLoss(out)
	if len(lookup) > 0 && infos != nil {
		m, err := infos(ctx, lookup)
		if err != nil {
			return nil, err
		}
		for i := range out {
			ip := out[i].IP
			if !ip.IsValid() {
				ip = out[i].MostCommonIP
			}
			if h, ok := m[ip.Unmap()]; ok {
				out[i].Info = &h
			}
		}
	}
	return out, nil
}

// classifyLoss sets LossContinues and RateLimited. The destination is the
// highest TTL with probes in the range.
func classifyLoss(hs []HopStat) {
	dest := -1
	for i := range hs {
		if hs[i].N+hs[i].Lost > 0 {
			dest = i
		}
	}
	if dest < 0 {
		return
	}
	dl := hs[dest].Loss
	for i := range hs {
		h := &hs[i]
		if h.Loss < HopLossMin || h.N+h.Lost == 0 {
			continue
		}
		h.LossContinues = dl >= HopLossContinueRatio*h.Loss
		h.RateLimited = !h.LossContinues
	}
}

// mostCommonHops returns, per TTL, the address that held it longest within
// [from, to]. The route in effect is reconstructed backwards from the
// current route through the changes (newest first): before a change, the
// route was its From.
func mostCommonHops(route model.Route, haveRoute bool, changes []model.RouteChange, from, to time.Time) []netip.Addr {
	var hops []netip.Addr
	if haveRoute {
		hops = route.Hops
	} else if len(changes) > 0 {
		hops = changes[0].To
	}
	type seg struct {
		hops []netip.Addr
		d    time.Duration
	}
	var segs []seg
	end := to
	for _, c := range changes {
		if c.At.After(to) {
			hops = c.From
			continue
		}
		if !c.At.After(from) {
			break
		}
		segs = append(segs, seg{hops, end.Sub(c.At)})
		end, hops = c.At, c.From
	}
	segs = append(segs, seg{hops, max(end.Sub(from), 0)})
	n := 0
	for _, s := range segs {
		n = max(n, len(s.hops))
	}
	out := make([]netip.Addr, n)
	for ttl := range n {
		w := map[netip.Addr]time.Duration{}
		for _, s := range segs {
			if ttl < len(s.hops) && s.hops[ttl].IsValid() {
				w[s.hops[ttl]] += s.d + 1 // +1: a zero-length range still counts
			}
		}
		var best time.Duration
		for ip, d := range w {
			if d > best || (d == best && ip.Less(out[ttl])) {
				out[ttl], best = ip, d
			}
		}
	}
	return out
}
