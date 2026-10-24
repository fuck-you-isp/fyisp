package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

// Storage tests (schema v9). The tables hold no host, bucket, key, CA, full
// edge address or error text: targets are referred to by their stable id,
// and their settings live in the targets file (internal/storage).

const campaignCols = `id, created_ms, updated_ms, shape, state, coalesce(stop_reason, ''), targets, cold, warm, down_bytes, up_bytes,
	interval_s, start_ms, count, end_ms, days, win_from_min, win_to_min, coalesce(tz, ''), until_ms, keys_saved, rounds_done, bytes_down, bytes_up`

func scanCampaign(sc interface{ Scan(...any) error }) (model.StorageCampaign, error) {
	var c model.StorageCampaign
	var created, updated, interval int64
	var targets string
	var start, count, end, days, from, to, until sql.NullInt64
	err := sc.Scan(&c.ID, &created, &updated, &c.Shape, &c.State, &c.Stopped, &targets, &c.Cold, &c.Warm, &c.Down, &c.Up,
		&interval, &start, &count, &end, &days, &from, &to, &c.TZ, &until, &c.KeysSaved, &c.RoundsDone, &c.BytesDown, &c.BytesUp)
	if err != nil {
		return c, err
	}
	c.Created, c.Updated, c.Interval = msTime(created), msTime(updated), time.Duration(interval)*time.Second
	_ = json.Unmarshal([]byte(targets), &c.Targets)
	if start.Valid {
		c.Start = msTime(start.Int64)
	}
	if end.Valid {
		c.End = msTime(end.Int64)
	}
	if until.Valid {
		c.Until = msTime(until.Int64)
	}
	c.Count, c.Days, c.FromMin, c.ToMin = int(count.Int64), uint8(days.Int64), int(from.Int64), int(to.Int64)
	return c, nil
}

// nullMs is t in unix ms, or NULL for the zero time.
func nullMs(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UnixMilli()
}

func nullInt(v int, ok bool) any {
	if !ok {
		return nil
	}
	return v
}

// SaveStorageCampaign stores a new campaign (c.ID is set), or an existing
// one's fields but its counters (rounds_done, bytes_down, bytes_up), which
// SaveStorageRound adds to.
func (s *SQLite) SaveStorageCampaign(ctx context.Context, c *model.StorageCampaign) error {
	if s.lock == nil {
		return errors.New("store: read-only")
	}
	targets, _ := json.Marshal(c.Targets)
	rec := c.Shape == model.StorageRecurring
	args := []any{c.Created.UnixMilli(), c.Updated.UnixMilli(), c.Shape, c.State, nullStr(c.Stopped), string(targets), c.Cold, c.Warm, c.Down, c.Up,
		int64(c.Interval / time.Second), nullMs(c.Start), nullInt(c.Count, c.Count > 0), nullMs(c.End),
		nullInt(int(c.Days), rec), nullInt(c.FromMin, rec), nullInt(c.ToMin, rec), nullStr(c.TZ), nullMs(c.Until),
		c.KeysSaved, c.RoundsDone, c.BytesDown, c.BytesUp}
	if c.ID == 0 {
		r, err := s.db.w.ExecContext(ctx, `INSERT INTO storage_campaigns(created_ms, updated_ms, shape, state, stop_reason, targets, cold, warm,
			down_bytes, up_bytes, interval_s, start_ms, count, end_ms, days, win_from_min, win_to_min, tz, until_ms, keys_saved, rounds_done,
			bytes_down, bytes_up) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, args...)
		if err != nil {
			return err
		}
		c.ID, err = r.LastInsertId()
		return err
	}
	_, err := s.db.w.ExecContext(ctx, `UPDATE storage_campaigns SET created_ms = ?, updated_ms = ?, shape = ?, state = ?, stop_reason = ?,
		targets = ?, cold = ?, warm = ?, down_bytes = ?, up_bytes = ?, interval_s = ?, start_ms = ?, count = ?, end_ms = ?, days = ?,
		win_from_min = ?, win_to_min = ?, tz = ?, until_ms = ?, keys_saved = ? WHERE id = ?`,
		append(args[:20:20], c.ID)...)
	return err
}

// StorageCampaign reads one campaign (ok false: none).
func (s *SQLite) StorageCampaign(ctx context.Context, id int64) (model.StorageCampaign, bool, error) {
	c, err := scanCampaign(s.db.r.QueryRowContext(ctx, `SELECT `+campaignCols+` FROM storage_campaigns WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return c, false, nil
	}
	return c, err == nil, err
}

// StorageCampaigns lists campaigns newest first: those with an id below
// before (0: all), at most limit; live ones only with live.
func (s *SQLite) StorageCampaigns(ctx context.Context, before int64, limit int, live bool) ([]model.StorageCampaign, error) {
	q := `SELECT ` + campaignCols + ` FROM storage_campaigns WHERE (? = 0 OR id < ?)`
	if live {
		q += ` AND state IN ('scheduled', 'running', 'paused')`
	}
	rows, err := s.db.r.QueryContext(ctx, q+` ORDER BY id DESC LIMIT ?`, before, before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.StorageCampaign
	for rows.Next() {
		c, err := scanCampaign(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SaveStorageRound stores a round (a run, a skip or a gap) with its
// attempts in one transaction, and adds a run's bytes and count to its
// campaign. r.ID is set.
func (s *SQLite) SaveStorageRound(ctx context.Context, r *model.StorageRound, attempts []model.StorageAttempt) error {
	if s.lock == nil {
		return errors.New("store: read-only")
	}
	tx, err := s.db.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var seq, skips any
	if r.Seq != nil {
		seq = *r.Seq
	}
	if len(r.Skips) > 0 {
		b, _ := json.Marshal(r.Skips)
		skips = string(b)
	}
	var wait any
	if !r.Start.IsZero() {
		wait = r.QueueWait.Milliseconds()
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO storage_rounds(campaign, seq, due_ms, start_ms, end_ms, queue_wait_ms, outcome, skip_reason, skips,
		gw_loss, gw_rtt_ms, edge_loss, edge_rtt_ms, bytes_down, bytes_up) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.Campaign, seq, r.Due.UnixMilli(), nullMs(r.Start), nullMs(r.End), wait, r.Outcome, nullStr(r.SkipReason), skips,
		r.GwLoss, r.GwRTTms, r.EdgeLoss, r.EdgeRTTms, r.BytesDown, r.BytesUp)
	if err != nil {
		return err
	}
	if r.ID, err = res.LastInsertId(); err != nil {
		return err
	}
	if len(attempts) > 0 {
		st, err := tx.PrepareContext(ctx, `INSERT INTO storage_attempts(round, target, phase, op, n, at_ms, class, http_status, s3_code,
			request_id, host_id, request_id_header, dns_ms, connect_ms, tls_ms, ttfb_ms, total_ms, bytes, goodput_bps, stall_ms,
			edge_prefix, tls_version, resumed, tcp_rtt_ms, retrans, mss, skew_s) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
		if err != nil {
			return err
		}
		defer st.Close()
		for _, a := range attempts {
			var status any
			if a.HTTPStatus != 0 {
				status = a.HTTPStatus
			}
			if _, err := st.ExecContext(ctx, r.ID, a.Target, a.Phase, a.Op, a.N, a.At.UnixMilli(), a.Class, status, nullStr(a.S3Code),
				nullStr(a.RequestID), nullStr(a.HostID), nullStr(a.RequestIDHeader), a.DNSms, a.Connectms, a.TLSms, a.TTFBms, a.Totalms,
				a.Bytes, a.GoodputBps, a.Stallms, nullStr(a.EdgePrefix), nullStr(a.TLSVersion), a.Resumed, a.TCPRTTms, a.Retrans, a.MSS, a.SkewS); err != nil {
				return err
			}
		}
	}
	if r.Seq != nil && !r.Start.IsZero() { // a round that ran (not skipped, or cancelled in the queue)
		if _, err := tx.ExecContext(ctx, `UPDATE storage_campaigns SET rounds_done = rounds_done + 1, bytes_down = bytes_down + ?,
			bytes_up = bytes_up + ? WHERE id = ?`, r.BytesDown, r.BytesUp, r.Campaign); err != nil {
			return err
		}
	}
	return tx.Commit()
}

const roundCols = `id, campaign, seq, due_ms, start_ms, end_ms, queue_wait_ms, outcome, coalesce(skip_reason, ''), coalesce(skips, ''),
	gw_loss, gw_rtt_ms, edge_loss, edge_rtt_ms, bytes_down, bytes_up`

func scanRound(sc interface{ Scan(...any) error }) (model.StorageRound, error) {
	var r model.StorageRound
	var seq, start, end, wait sql.NullInt64
	var due int64
	var skips string
	var gl, gr, el, er sql.NullFloat64
	if err := sc.Scan(&r.ID, &r.Campaign, &seq, &due, &start, &end, &wait, &r.Outcome, &r.SkipReason, &skips,
		&gl, &gr, &el, &er, &r.BytesDown, &r.BytesUp); err != nil {
		return r, err
	}
	if seq.Valid {
		n := int(seq.Int64)
		r.Seq = &n
	}
	r.Due = msTime(due)
	if start.Valid {
		r.Start = msTime(start.Int64)
	}
	if end.Valid {
		r.End = msTime(end.Int64)
	}
	r.QueueWait = time.Duration(wait.Int64) * time.Millisecond
	if skips != "" {
		_ = json.Unmarshal([]byte(skips), &r.Skips)
	}
	r.GwLoss, r.GwRTTms, r.EdgeLoss, r.EdgeRTTms = ptrF(gl), ptrF(gr), ptrF(el), ptrF(er)
	return r, nil
}

// StorageRounds lists a campaign's rounds, skips and gaps due after
// afterDue (zero: all), oldest first.
func (s *SQLite) StorageRounds(ctx context.Context, campaign int64, afterDue time.Time) ([]model.StorageRound, error) {
	rows, err := s.db.r.QueryContext(ctx, `SELECT `+roundCols+` FROM storage_rounds WHERE campaign = ? AND due_ms > ? ORDER BY due_ms, id`,
		campaign, afterDue.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.StorageRound
	for rows.Next() {
		r, err := scanRound(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// StorageRoundsTail is a campaign's highest round number (-1: none) and
// the latest time any of its rows reaches (a round's due or end; zero:
// none).
func (s *SQLite) StorageRoundsTail(ctx context.Context, campaign int64) (int, time.Time, error) {
	var seq, last sql.NullInt64
	err := s.db.r.QueryRowContext(ctx, `SELECT max(seq), max(max(due_ms, coalesce(end_ms, 0))) FROM storage_rounds WHERE campaign = ?`, campaign).Scan(&seq, &last)
	if err != nil {
		return -1, time.Time{}, err
	}
	n := -1
	if seq.Valid {
		n = int(seq.Int64)
	}
	var t time.Time
	if last.Valid {
		t = msTime(last.Int64)
	}
	return n, t, nil
}

// StorageRound reads one round.
func (s *SQLite) StorageRound(ctx context.Context, id int64) (model.StorageRound, bool, error) {
	r, err := scanRound(s.db.r.QueryRowContext(ctx, `SELECT `+roundCols+` FROM storage_rounds WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return r, false, nil
	}
	return r, err == nil, err
}

// StorageAttemptFilter selects attempts: a round's, or a campaign's with
// the other fields narrowing them. Before pages (attempt ids below it).
type StorageAttemptFilter struct {
	Round, Campaign int64
	Target, Class   string
	Phase           string
	Failed          bool // class <> 'ok'
	Before          int64
	// Oldest reads oldest first, from the attempt ids above After.
	Oldest bool
	After  int64
	Limit  int
}

// StorageAttempt is a stored attempt with its id and round.
type StorageAttempt struct {
	ID, Round int64
	model.StorageAttempt
}

// StorageAttempts lists the attempts f selects, newest first (a round's in
// the order they ran), and how many match without the paging.
func (s *SQLite) StorageAttempts(ctx context.Context, f StorageAttemptFilter) ([]StorageAttempt, int, error) {
	var where []string
	var args []any
	if f.Round != 0 {
		where, args = append(where, "a.round = ?"), append(args, f.Round)
	}
	if f.Campaign != 0 {
		where, args = append(where, "a.round IN (SELECT id FROM storage_rounds WHERE campaign = ?)"), append(args, f.Campaign)
	}
	for _, c := range []struct{ col, v string }{{"a.target", f.Target}, {"a.class", f.Class}, {"a.phase", f.Phase}} {
		if c.v != "" {
			where, args = append(where, c.col+" = ?"), append(args, c.v)
		}
	}
	if f.Failed {
		where = append(where, "a.class <> 'ok'")
	}
	if len(where) == 0 {
		return nil, 0, errors.New("store: attempts need a round or a campaign")
	}
	cond := strings.Join(where, " AND ")
	var total int
	if err := s.db.r.QueryRowContext(ctx, `SELECT count(*) FROM storage_attempts a WHERE `+cond, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	order := "a.id DESC"
	if (f.Round != 0 && f.Campaign == 0) || f.Oldest {
		order = "a.id"
	}
	page := cond
	pargs := args
	switch {
	case f.Before > 0:
		page, pargs = cond+" AND a.id < ?", append(append([]any{}, args...), f.Before)
	case f.Oldest && f.After > 0:
		page, pargs = cond+" AND a.id > ?", append(append([]any{}, args...), f.After)
	}
	limit := f.Limit
	if limit <= 0 {
		limit = -1
	}
	rows, err := s.db.r.QueryContext(ctx, `SELECT a.id, a.round, a.target, a.phase, a.op, a.n, a.at_ms, a.class, a.http_status,
		coalesce(a.s3_code, ''), coalesce(a.request_id, ''), coalesce(a.host_id, ''), coalesce(a.request_id_header, ''), a.dns_ms, a.connect_ms,
		a.tls_ms, a.ttfb_ms, a.total_ms, coalesce(a.bytes, 0), a.goodput_bps, a.stall_ms, coalesce(a.edge_prefix, ''), coalesce(a.tls_version, ''),
		coalesce(a.resumed, 0), a.tcp_rtt_ms, a.retrans, a.mss, a.skew_s FROM storage_attempts a WHERE `+page+` ORDER BY `+order+` LIMIT ?`,
		append(pargs, limit)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []StorageAttempt
	for rows.Next() {
		var a StorageAttempt
		var at int64
		var status, retrans, mss, skew sql.NullInt64
		var dns, conn, tlsMs, ttfb, total, goodput, stall, rtt sql.NullFloat64
		if err := rows.Scan(&a.ID, &a.Round, &a.Target, &a.Phase, &a.Op, &a.N, &at, &a.Class, &status, &a.S3Code, &a.RequestID, &a.HostID,
			&a.RequestIDHeader, &dns, &conn, &tlsMs, &ttfb, &total, &a.Bytes, &goodput, &stall, &a.EdgePrefix, &a.TLSVersion, &a.Resumed,
			&rtt, &retrans, &mss, &skew); err != nil {
			return nil, 0, err
		}
		a.At, a.HTTPStatus = msTime(at), int(status.Int64)
		a.DNSms, a.Connectms, a.TLSms, a.TTFBms, a.Totalms = ptrF(dns), ptrF(conn), ptrF(tlsMs), ptrF(ttfb), ptrF(total)
		a.GoodputBps, a.Stallms, a.TCPRTTms = ptrF(goodput), ptrF(stall), ptrF(rtt)
		a.Retrans, a.MSS, a.SkewS = intPtr(retrans), intPtr(mss), intPtr(skew)
		out = append(out, a)
	}
	return out, total, rows.Err()
}

// s3MaxTransfer bounds how long before from a transfer that reaches into
// [from, to] can have started (s3test's longest).
const s3MaxTransfer = 10 * time.Minute

// storageWindows are the storage rounds' transfer windows (each phase of a
// target in a round) reaching into [from, to] (seeding, downloads and uploads: the probes during them
// measure a loaded link), as bare speed-test windows with trigger
// "storage".
func (s *SQLite) storageWindows(ctx context.Context, from, to time.Time) ([]model.SpeedTest, error) {
	rows, err := s.db.r.QueryContext(ctx, `SELECT min(at_ms), max(at_ms + coalesce(total_ms, 0)) FROM storage_attempts
		WHERE at_ms >= ? AND at_ms <= ? AND phase IN ('seed', 'down', 'up') GROUP BY round, target, phase ORDER BY 1`,
		from.Add(-s3MaxTransfer).UnixMilli(), to.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.SpeedTest
	for rows.Next() {
		var a int64
		var b float64
		if err := rows.Scan(&a, &b); err != nil {
			return nil, err
		}
		out = append(out, model.SpeedTest{Start: msTime(a), End: msTime(int64(b)), Trigger: model.TriggerStorage, Outcome: model.SpeedRemoved})
	}
	return out, rows.Err()
}

// pruneStorage deletes the storage tests' rows before cut (unix ms): rounds
// due before it with their attempts, traces, and the campaigns that ended
// before it with no round left.
func pruneStorage(ctx context.Context, tx *sql.Tx, cut int64) error {
	for _, q := range []string{
		`DELETE FROM storage_attempts WHERE round IN (SELECT id FROM storage_rounds WHERE due_ms < ?1) OR at_ms < ?1`,
		`DELETE FROM storage_rounds WHERE due_ms < ?1`,
		`DELETE FROM storage_traces WHERE at_ms < ?1`,
		`DELETE FROM storage_campaigns WHERE state IN ('done', 'stopped') AND end_ms IS NOT NULL AND end_ms < ?1
			AND NOT EXISTS (SELECT 1 FROM storage_rounds WHERE campaign = storage_campaigns.id)`,
	} {
		if _, err := tx.ExecContext(ctx, q, cut); err != nil {
			return err
		}
	}
	return nil
}

func intPtr(v sql.NullInt64) *int {
	if !v.Valid {
		return nil
	}
	n := int(v.Int64)
	return &n
}

// StorageCellRow is one campaign round's attempts of one target, phase and
// class: how many, the longest stall and the mean goodput (transfers).
type StorageCellRow struct {
	Round              int64
	Target, Phase      string
	Class              string
	EdgePrefix         string // "" unknown
	N                  int
	MaxStallMs         float64
	GoodputBps         *float64
	HTTPStatus, S3Code string // the first failure's (status as text)
}

// cellCache keeps the last campaign's StorageCells rows: a round's are
// written once with it, so a later call reads only the newer rounds (a
// schedule kept for 90 days has millions of attempts).
type cellCache struct {
	mu       sync.Mutex
	campaign int64
	created  int64 // its created_ms: ids are reused once pruning empties a table
	last     int64 // the newest round read
	rows     []StorageCellRow
}

// StorageCells groups a campaign's attempts by round, target, phase and
// class, oldest round first.
func (s *SQLite) StorageCells(ctx context.Context, campaign int64) ([]StorageCellRow, error) {
	c := &s.cells
	c.mu.Lock()
	defer c.mu.Unlock()
	var created sql.NullInt64
	var first, newest sql.NullInt64 // the oldest round kept (pruning drops older ones), the newest
	if err := s.db.r.QueryRowContext(ctx, `SELECT (SELECT created_ms FROM storage_campaigns WHERE id = ?), min(id), max(id) FROM storage_rounds WHERE campaign = ?`,
		campaign, campaign).Scan(&created, &first, &newest); err != nil {
		return nil, err
	}
	if c.campaign != campaign || c.created != created.Int64 || !newest.Valid || newest.Int64 < c.last {
		// Another campaign, one with a reused id, or rounds gone: read afresh.
		c.campaign, c.created, c.last, c.rows = campaign, created.Int64, 0, nil
	}
	if i := slices.IndexFunc(c.rows, func(r StorageCellRow) bool { return r.Round >= first.Int64 }); i > 0 {
		c.rows = slices.Delete(c.rows, 0, i)
	} else if i < 0 {
		c.rows = c.rows[:0]
	}
	rows, err := s.db.r.QueryContext(ctx, `SELECT a.round, a.target, a.phase, a.class, coalesce(a.edge_prefix, ''), count(*), max(coalesce(a.stall_ms, 0)),
		avg(a.goodput_bps), coalesce(min(CAST(a.http_status AS TEXT)), ''), coalesce(min(a.s3_code), '')
		FROM storage_attempts a JOIN storage_rounds r ON r.id = a.round WHERE r.campaign = ? AND r.id > ?
		GROUP BY a.round, a.target, a.phase, a.class, a.edge_prefix ORDER BY a.round, a.target, a.phase, a.class`, campaign, c.last)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var add []StorageCellRow
	for rows.Next() {
		var r StorageCellRow
		var g sql.NullFloat64
		if err := rows.Scan(&r.Round, &r.Target, &r.Phase, &r.Class, &r.EdgePrefix, &r.N, &r.MaxStallMs, &g, &r.HTTPStatus, &r.S3Code); err != nil {
			return nil, err
		}
		r.GoodputBps = ptrF(g)
		add = append(add, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	c.rows = append(c.rows, add...)
	if len(c.rows) > 0 {
		c.last = c.rows[len(c.rows)-1].Round
	}
	return slices.Clone(c.rows), nil
}

// SaveStorageTrace stores a traceroute snapshot (t.ID is set).
func (s *SQLite) SaveStorageTrace(ctx context.Context, t *model.StorageTrace) error {
	if s.lock == nil {
		return errors.New("store: read-only")
	}
	hops, _ := json.Marshal(t.Hops)
	var asn any
	if t.HandoffASN != 0 {
		asn = int64(t.HandoffASN)
	}
	r, err := s.db.w.ExecContext(ctx, `INSERT INTO storage_traces(campaign, target, at_ms, trigger, edge_prefix, hops, handoff_asn, route_changed)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, t.Campaign, t.Target, t.At.UnixMilli(), t.Trigger, nullStr(t.EdgePrefix), string(hops), asn, t.RouteChanged)
	if err != nil {
		return err
	}
	t.ID, err = r.LastInsertId()
	return err
}

// StorageTraces lists a campaign's traceroutes, oldest first.
func (s *SQLite) StorageTraces(ctx context.Context, campaign int64) ([]model.StorageTrace, error) {
	rows, err := s.db.r.QueryContext(ctx, `SELECT id, campaign, target, at_ms, trigger, coalesce(edge_prefix, ''), hops, coalesce(handoff_asn, 0), route_changed
		FROM storage_traces WHERE campaign = ? ORDER BY at_ms, id`, campaign)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.StorageTrace
	for rows.Next() {
		var t model.StorageTrace
		var at int64
		var hops string
		if err := rows.Scan(&t.ID, &t.Campaign, &t.Target, &at, &t.Trigger, &t.EdgePrefix, &hops, &t.HandoffASN, &t.RouteChanged); err != nil {
			return nil, err
		}
		t.At = msTime(at)
		_ = json.Unmarshal([]byte(hops), &t.Hops)
		out = append(out, t)
	}
	return out, rows.Err()
}

// StorageTiming is one attempt's timings, for a campaign's percentiles.
type StorageTiming struct {
	Target, Phase, Op, Class, EdgePrefix string
	Connectms, TLSms, TTFBms             *float64
	GoodputBps                           *float64
}

// StorageTimingsMax bounds StorageTimings: the newest attempts only.
const StorageTimingsMax = 50000

// StorageTimings lists the timings (no other columns) of a campaign's
// newest StorageTimingsMax attempts.
func (s *SQLite) StorageTimings(ctx context.Context, campaign int64) ([]StorageTiming, error) {
	rows, err := s.db.r.QueryContext(ctx, `SELECT a.target, a.phase, a.op, a.class, coalesce(a.edge_prefix, ''), a.connect_ms, a.tls_ms, a.ttfb_ms, a.goodput_bps
		FROM storage_attempts a WHERE a.round IN (SELECT id FROM storage_rounds WHERE campaign = ?) ORDER BY a.id DESC LIMIT ?`, campaign, StorageTimingsMax)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StorageTiming
	for rows.Next() {
		var t StorageTiming
		var c, tl, tt, g sql.NullFloat64
		if err := rows.Scan(&t.Target, &t.Phase, &t.Op, &t.Class, &t.EdgePrefix, &c, &tl, &tt, &g); err != nil {
			return nil, err
		}
		t.Connectms, t.TLSms, t.TTFBms, t.GoodputBps = ptrF(c), ptrF(tl), ptrF(tt), ptrF(g)
		out = append(out, t)
	}
	return out, rows.Err()
}
