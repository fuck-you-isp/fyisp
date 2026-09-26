package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/store/blob"
)

// File names inside the data directory.
const (
	DBName   = "fyisp.db"
	LockName = "fyisp.lock"
)

// Options configure Open.
type Options struct {
	// Version is recorded in meta.created_by_version when the file is created.
	Version string
	// URL is written into the lock file so a second instance can print it.
	// It can be changed later with SetURL.
	URL string
	// Interval returns the slot interval of a series. It is read for every
	// sample (keep it cheap): a new hour uses it, and an hour whose interval
	// differs (restored after a profile change) is regridded to fit both;
	// nil means 15s (ICMP 5s). Values are clamped to [100ms, 24h].
	Interval func(model.SeriesKey) time.Duration
	// Now is the clock used to close hours that stopped receiving samples
	// (Flush) and to stamp meta.created_at. Default time.Now.
	Now func() time.Time
	// MaxBuffer bounds the memory held by closed hours that could not be
	// written (disk full). Default 10 MB; the oldest hours are dropped first.
	MaxBuffer int
	// Log receives rare operational messages. Default: discard.
	Log *slog.Logger
}

// closeGrace is how long after its end an hour without new samples stays
// open; late samples for it are still accepted until then.
const closeGrace = 2 * time.Minute

// clockSlack is how far a series' last accepted slot may be ahead of
// Options.Now before the store concludes the clock was wrong (it stepped back,
// or a sample was stamped in the future), and how far ahead of Options.Now an
// hour may start and still count as "now" when the store is opened.
const clockSlack = time.Minute

// SQLite is the real Store. Create it with Open.
//
// Writes: Observe only touches memory. The caller runs Flush every 60s and on
// shutdown, and Prune about hourly; SQLite starts no goroutines of its own.
type SQLite struct {
	dir  string
	db   *conns
	lock *dirLock // nil when read-only
	o    Options

	flushMu  sync.Mutex   // one Flush/Prune at a time
	commitMu sync.RWMutex // write: commit + memory update; read: memory snapshot + first query step

	mu        sync.Mutex // guards everything below
	cur       map[model.SeriesKey]*block
	pending   []*block // closed hours not yet written, oldest first
	pendBytes int
	floor     map[model.SeriesKey]int64 // last accepted slot (unix ms); later samples only
	ids       map[model.SeriesKey]int64
	keys      map[int64]model.SeriesKey
	ivs       map[model.SeriesKey]int64 // series.interval_ms as stored
	dropped   int64                     // samples discarded because the buffer was full
	rejected  int64                     // duplicate or backward samples
	clockLog  time.Time                 // last "clock stepped back" warning
	pruneLog  bool                      // "not pruning" warned once
	lastFlush time.Time
	lastErr   string
	closed    bool
}

// block is one series' hour in memory.
type block struct {
	key     model.SeriesKey
	hour    int64 // unix hour
	slot0   int64 // unix ms of slot 0
	iv      int64 // ms
	slots   []blob.Slot
	flushed int // len(slots) as last written to the database; -1: rewrite
	// fresh: never written. Its first write merges with an hour already
	// stored for the same series (after a clock correction), never
	// replacing stored samples.
	fresh bool
	gen   int // bumped whenever slots, slot0 or iv are replaced (not appended)

	// Set on closed blocks once encoded (then slots is released).
	data []byte
	sum  *summary
}

func (b *block) memSize() int {
	if b.data != nil {
		return len(b.data) + 256
	}
	return 16*cap(b.slots) + 256
}

var _ Store = (*SQLite)(nil)

// Open opens or creates the store in dir: <dir>/fyisp.db (0600; the
// directory is created 0700) guarded by the single-instance lock
// <dir>/fyisp.lock. If another process holds the lock, the error is a
// *LockedError (errors.Is(err, ErrLocked)) carrying its PID and URL.
func Open(dir string, o Options) (*SQLite, error) {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.MaxBuffer <= 0 {
		o.MaxBuffer = 10 << 20
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	lk, err := acquireLock(filepath.Join(dir, LockName))
	if err != nil {
		return nil, err
	}
	if err := lk.write(o.URL); err != nil {
		lk.release()
		return nil, err
	}
	ctx := context.Background()
	db, err := openConns(ctx, filepath.Join(dir, DBName), o.Version, o.Now())
	if err != nil {
		lk.release()
		return nil, err
	}
	s := newSQLite(dir, db, o)
	s.lock = lk
	if err := s.load(ctx); err != nil {
		db.Close()
		lk.release()
		return nil, err
	}
	return s, nil
}

// OpenReadOnly opens an existing store for reading without taking the lock,
// e.g. for `fyisp export` while fyisp is running. It sees what the running
// instance has flushed (at most ~60s behind).
func OpenReadOnly(dir string) (*SQLite, error) {
	path := filepath.Join(dir, DBName)
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	ctx := context.Background()
	r, err := openReader(ctx, path)
	if err != nil {
		return nil, err
	}
	var ver int
	if err := r.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&ver); err != nil {
		r.Close()
		return nil, err
	}
	switch {
	case ver > SchemaVersion():
		r.Close()
		return nil, fmt.Errorf("%w (%d > %d)", ErrNewerSchema, ver, SchemaVersion())
	case ver < SchemaVersion():
		r.Close()
		return nil, fmt.Errorf("store: %s has schema v%d; run fyisp once to upgrade it to v%d", path, ver, SchemaVersion())
	}
	s := newSQLite(dir, &conns{path: path, r: r}, Options{Now: time.Now, Log: slog.New(slog.DiscardHandler)})
	if err := s.loadSeries(ctx); err != nil {
		r.Close()
		return nil, err
	}
	return s, nil
}

func newSQLite(dir string, db *conns, o Options) *SQLite {
	return &SQLite{
		dir: dir, db: db, o: o,
		cur:   map[model.SeriesKey]*block{},
		floor: map[model.SeriesKey]int64{},
		ids:   map[model.SeriesKey]int64{},
		keys:  map[int64]model.SeriesKey{},
		ivs:   map[model.SeriesKey]int64{},
	}
}

func (s *SQLite) loadSeries(ctx context.Context) error {
	rows, err := s.db.r.QueryContext(ctx, `SELECT id, target, kind, interval_ms FROM series`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, iv int64
		var k model.SeriesKey
		if err := rows.Scan(&id, &k.Target, &k.Kind, &iv); err != nil {
			return err
		}
		s.ids[k], s.keys[id], s.ivs[k] = id, k, iv
	}
	return rows.Err()
}

// load restores the writer state after a restart or crash: the newest hour
// of each series, if it has no summary yet, becomes the in-memory current
// hour again (so the rest of the hour is appended to it, not written over
// it); older hours left without a summary (a crash, or hours dropped from a
// full buffer) are finalized now. Samples at or before the newest stored
// hour's data are rejected afterwards.
func (s *SQLite) load(ctx context.Context) error {
	if err := s.loadSeries(ctx); err != nil {
		return err
	}
	// Hours that start after now (+clockSlack) were stamped by a clock that
	// was ahead (or the clock is behind now). They are kept, but ignored
	// for the floor and never restored as the current hour, so a wrong
	// clock in the past does not stop recording; a later block for such an
	// hour merges with it instead of replacing it.
	limit := (s.o.Now().UnixMilli() + clockSlack.Milliseconds()) / hourMs
	var future int64
	if err := s.db.w.QueryRowContext(ctx, `SELECT count(*) FROM samples WHERE hour > ?`, limit).Scan(&future); err != nil {
		return err
	}
	if future > 0 {
		s.o.Log.Warn("store: ignoring hours stamped after the current time (the clock was or is wrong)",
			"hours", future, "now", s.o.Now().UTC())
	}
	maxHour := map[int64]int64{}
	rows, err := s.db.w.QueryContext(ctx, `SELECT series, max(hour) FROM samples WHERE hour <= ? GROUP BY series`, limit)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id, h int64
		if err := rows.Scan(&id, &h); err != nil {
			rows.Close()
			return err
		}
		maxHour[id] = h
		if k, ok := s.keys[id]; ok {
			s.floor[k] = (h+1)*hourMs - 1
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	// Hours without a summary: only recent ones are looked at (a crash
	// leaves the newest hour open; a full write buffer drops at most a few
	// days of closed hours).
	var newest int64
	for _, h := range maxHour {
		newest = max(newest, h)
	}
	since := newest - 7*24
	have, err := summarized(ctx, s.db.w, since/24)
	if err != nil {
		return err
	}
	var missing [][2]int64
	rows, err = s.db.w.QueryContext(ctx, `SELECT hour, series FROM samples WHERE hour >= ?`, since)
	if err != nil {
		return err
	}
	for rows.Next() {
		var hk [2]int64
		if err := rows.Scan(&hk[0], &hk[1]); err != nil {
			rows.Close()
			return err
		}
		if !have[hk] {
			missing = append(missing, hk)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	finalize := map[dayKey][]hourSummary{}
	for _, hk := range missing {
		h, id := hk[0], hk[1]
		var data []byte
		if err := s.db.w.QueryRowContext(ctx, `SELECT data FROM samples WHERE hour = ? AND series = ?`, h, id).Scan(&data); err != nil {
			return err
		}
		b, err := blob.Decode(data)
		k, known := s.keys[id]
		if err != nil || !known {
			s.o.Log.Warn("store: skipping unreadable hour", "hour", h, "series", id, "err", err)
			continue
		}
		if mh, ok := maxHour[id]; ok && h == mh {
			s.cur[k] = &block{key: k, hour: h, slot0: b.Slot0, iv: b.Interval, slots: b.Slots, flushed: len(b.Slots)}
			if len(b.Slots) > 0 {
				s.floor[k] = b.Slot0 + int64(len(b.Slots)-1)*b.Interval
			} else {
				s.floor[k] = h*hourMs - 1
			}
			continue
		}
		dk := dayKey{h / 24, id}
		finalize[dk] = append(finalize[dk], hourSummary{h, *summarize(b.Slots)})
	}
	if len(finalize) == 0 {
		return nil
	}
	tx, err := s.db.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := writeSummaries(ctx, tx, finalize, -1); err != nil {
		return err
	}
	return tx.Commit()
}

type dayKey struct{ day, series int64 }

type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// summarized returns the (hour, series) pairs that have a summary, for days
// from fromDay on.
func summarized(ctx context.Context, q queryer, fromDay int64) (map[[2]int64]bool, error) {
	rows, err := q.QueryContext(ctx, `SELECT day, series, data FROM summary_1h WHERE day >= ?`, fromDay)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[[2]int64]bool{}
	var hs []hourSummary
	for rows.Next() {
		var day, id int64
		var data []byte
		if err := rows.Scan(&day, &id, &data); err != nil {
			return nil, err
		}
		hs, err = decodeDay(hs[:0], day, data)
		if err != nil {
			return nil, fmt.Errorf("day %d series %d: %w", day, id, err)
		}
		for _, h := range hs {
			out[[2]int64{h.hour, id}] = true
		}
	}
	return out, rows.Err()
}

// writeSummaries merges hours into their day rows. With dropBefore >= 0,
// hours before it are removed from the rows instead (prune; add is then the
// set of day rows to rewrite, with no hours of its own).
func writeSummaries(ctx context.Context, tx *sql.Tx, add map[dayKey][]hourSummary, dropBefore int64) error {
	for dk, hs := range add {
		var old []byte
		err := tx.QueryRowContext(ctx, `SELECT data FROM summary_1h WHERE day = ? AND series = ?`, dk.day, dk.series).Scan(&old)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var cur []hourSummary
		if old != nil {
			if cur, err = decodeDay(nil, dk.day, old); err != nil {
				return fmt.Errorf("day %d series %d: %w", dk.day, dk.series, err)
			}
		}
		slices.SortFunc(hs, func(a, b hourSummary) int { return int(a.hour - b.hour) })
		cur = mergeDay(cur, hs...)
		if dropBefore >= 0 {
			cur = slices.DeleteFunc(cur, func(h hourSummary) bool { return h.hour < dropBefore })
		}
		if len(cur) == 0 {
			_, err = tx.ExecContext(ctx, `DELETE FROM summary_1h WHERE day = ? AND series = ?`, dk.day, dk.series)
		} else {
			_, err = tx.ExecContext(ctx, `INSERT OR REPLACE INTO summary_1h(day, series, data) VALUES(?,?,?)`,
				dk.day, dk.series, appendDay(nil, dk.day, cur))
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// SetURL records the URL of this instance in the lock file, for the error a
// second instance prints.
func (s *SQLite) SetURL(url string) error {
	if s.lock == nil {
		return errors.New("store: read-only")
	}
	return s.lock.write(url)
}

func (s *SQLite) interval(k model.SeriesKey) int64 {
	var d time.Duration
	if s.o.Interval != nil {
		d = s.o.Interval(k)
	}
	if d <= 0 {
		d = 15 * time.Second
		if k.Kind == model.KindICMP {
			d = 5 * time.Second
		}
	}
	return max(100, min(d.Milliseconds(), blob.MaxInterval))
}

// Observe stores a sample in memory. It never blocks on I/O.
//
// Sample.Slot is authoritative. A slot at or before the series' last
// accepted slot (duplicate, small clock step back) is dropped and counted in
// Stats.Rejected. When that last slot is more than a minute ahead of
// Options.Now (the clock was ahead, or stepped back) and the sample is not,
// the clock is taken as corrected: the series' current hour is closed and
// samples are accepted again; a block for an hour that is already stored is
// merged into it on Flush, stored samples win. Slots skipped over (sleep,
// restart, clock jumped forward) stay "not measured" (code 0) and are never
// counted as loss. A sample in a later hour closes the series' current hour.
// A slot that is not a multiple of the interval from the hour's first slot
// is rounded down to one. When the series' interval changes mid-hour, the
// hour's slots are regridded to an interval that fits both.
func (s *SQLite) Observe(x model.Sample) {
	ms := x.Slot.UnixMilli()
	if ms < 0 {
		return
	}
	hour := ms / hourMs
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.lock == nil {
		return
	}
	if last, ok := s.floor[x.Key]; ok && ms <= last && !s.clockBackLocked(x.Key, ms, last) {
		s.rejected++
		return
	}
	b := s.cur[x.Key]
	if b != nil && hour > b.hour {
		s.closeLocked(b)
		b = nil
	}
	if b == nil {
		iv := s.interval(x.Key)
		b = &block{key: x.Key, hour: hour, iv: iv, slot0: hour*hourMs + (ms-hour*hourMs)%iv, fresh: true}
		s.cur[x.Key] = b
	} else if iv := s.interval(x.Key); iv != b.iv {
		b.regrid(iv)
	}
	idx := int((ms - b.slot0) / b.iv)
	if ms < b.slot0 || idx < len(b.slots) || idx >= blob.MaxSlots {
		s.rejected++
		return
	}
	for len(b.slots) < idx {
		b.slots = append(b.slots, blob.Gap())
	}
	b.slots = append(b.slots, toSlot(x))
	s.floor[x.Key] = ms
}

// clockBackLocked handles a sample at or before the series' last accepted
// slot. If that slot is ahead of the wall clock and the sample is not, the
// clock was wrong (stamped in the future, or stepped back): the current hour
// is closed and the floor dropped so the sample is accepted. It reports
// whether the sample may be stored.
func (s *SQLite) clockBackLocked(k model.SeriesKey, ms, last int64) bool {
	now := s.o.Now()
	slack := clockSlack.Milliseconds()
	if last <= now.UnixMilli()+slack || ms > now.UnixMilli()+slack {
		return false
	}
	if b := s.cur[k]; b != nil {
		s.closeLocked(b)
	}
	delete(s.floor, k)
	if now.Sub(s.clockLog) >= time.Minute || now.Before(s.clockLog) {
		s.clockLog = now
		s.o.Log.Warn("store: clock stepped back; recording from the current time again",
			"series", k, "last_slot", time.UnixMilli(last).UTC(), "slot", time.UnixMilli(ms).UTC())
	}
	return true
}

// regrid switches b to a slot interval that fits both its own and iv (their
// greatest common divisor), so a series whose interval changed mid-hour (a
// new profile after a restart) keeps its earlier slots and places the new
// ones exactly. Slots in between are "not measured". If the finer grid would
// not fit in a blob, b is left as is (samples round down to its slots).
func (b *block) regrid(iv int64) {
	g := gcd(b.iv, iv)
	if g == b.iv || hourMs/g > blob.MaxSlots {
		return
	}
	b.slots = place(nil, b.slot0, g, b.slots, b.slot0, b.iv)
	b.iv = g
	b.gen++
	b.flushed = -1
}

func gcd(a, b int64) int64 {
	a, b = max(a, -a), max(b, -b)
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

func isGap(x blob.Slot) bool { return !x.Valid && x.Code == 0 }

// place copies the measured slots of src (grid s0, siv) into dst (grid d0,
// div), extending dst with gaps as needed. Slots already measured in dst
// win. Positions before d0 or past blob.MaxSlots are dropped.
func place(dst []blob.Slot, d0, div int64, src []blob.Slot, s0, siv int64) []blob.Slot {
	for i, x := range src {
		if isGap(x) {
			continue
		}
		off := s0 + int64(i)*siv - d0
		if off < 0 || off/div >= blob.MaxSlots {
			continue
		}
		pos := int(off / div)
		for len(dst) <= pos {
			dst = append(dst, blob.Gap())
		}
		if isGap(dst[pos]) {
			dst[pos] = x
		}
	}
	return dst
}

// mergeBlocks combines a stored hour with a new block for the same hour, on
// a grid that fits both. Stored samples win. If no grid fits one blob, the
// stored block is returned unchanged.
func mergeBlocks(stored, mine blob.Block) blob.Block {
	g := gcd(gcd(stored.Interval, mine.Interval), stored.Slot0-mine.Slot0)
	s0 := min(stored.Slot0, mine.Slot0)
	end := max(stored.Slot0+int64(len(stored.Slots))*stored.Interval, mine.Slot0+int64(len(mine.Slots))*mine.Interval)
	if g <= 0 || (end-s0)/g > blob.MaxSlots {
		return stored
	}
	out := place(nil, s0, g, stored.Slots, stored.Slot0, stored.Interval)
	out = place(out, s0, g, mine.Slots, mine.Slot0, mine.Interval)
	return blob.Block{Slot0: s0, Interval: g, Slots: out}
}

// closeLocked moves b from cur to pending.
func (s *SQLite) closeLocked(b *block) {
	delete(s.cur, b.key)
	if end := (b.hour+1)*hourMs - 1; s.floor[b.key] < end {
		s.floor[b.key] = end
	}
	s.pending = append(s.pending, b)
	s.pendBytes += b.memSize()
	// Not trimmed here: unencoded closed hours live until the next Flush,
	// which encodes them (~2 B/sample) and trims only if it fails.
}

// trimLocked drops the oldest closed hours while the buffer is over budget.
func (s *SQLite) trimLocked() {
	for s.pendBytes > s.o.MaxBuffer && len(s.pending) > 0 {
		b := s.pending[0]
		s.pending = s.pending[1:]
		s.pendBytes -= b.memSize()
		if b.sum != nil {
			s.dropped += b.sum.n + b.sum.lost
		} else {
			for _, x := range b.slots {
				if x.Valid || x.Code != 0 {
					s.dropped++
				}
			}
		}
		s.o.Log.Warn("store: write buffer full, dropping an hour", "series", b.key, "hour", time.Unix(b.hour*3600, 0).UTC())
	}
}

// flushItem is one blob to write.
type flushItem struct {
	b         *block
	n         int  // slots written (current hours)
	final     bool // closed hour: write the summary too
	fresh     bool // first write: merge with a stored hour
	gen       int
	slot0, iv int64
	slots     []blob.Slot
	data      []byte
	sum       *summary
	merged    *blob.Block // what was written, when merged with a stored hour
}

// Flush writes every changed current hour and every closed hour in one short
// transaction; blobs are encoded before it begins. Hours that ended more than
// two minutes ago (by Options.Now) without a newer sample are closed first.
// After closing an hour it runs wal_checkpoint(TRUNCATE).
//
// On error (disk full, I/O) nothing is lost from memory: closed hours stay
// buffered (up to Options.MaxBuffer), the error is returned and shown in
// Stats.LastFlushErr, and the next Flush retries.
func (s *SQLite) Flush(ctx context.Context) error {
	s.flushMu.Lock()
	defer s.flushMu.Unlock()
	if s.lock == nil {
		return errors.New("store: read-only")
	}

	now := s.o.Now().UnixMilli()
	var items []*flushItem
	var ups []seriesUpsert
	upSeen := map[model.SeriesKey]bool{}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errors.New("store: closed")
	}
	for _, b := range s.cur {
		if (b.hour+1)*hourMs+closeGrace.Milliseconds() <= now {
			s.closeLocked(b)
		}
	}
	for _, b := range s.pending {
		items = append(items, &flushItem{b: b, final: true, fresh: b.fresh, gen: b.gen, slot0: b.slot0, iv: b.iv,
			slots: b.slots, data: b.data, sum: b.sum})
	}
	for _, b := range s.cur {
		if len(b.slots) != b.flushed {
			n := len(b.slots)
			items = append(items, &flushItem{b: b, n: n, fresh: b.fresh, gen: b.gen, slot0: b.slot0, iv: b.iv,
				slots: b.slots[:n:n]})
		}
	}
	for _, it := range items {
		k := it.b.key
		if iv, ok := s.ivs[k]; (!ok || iv != it.iv) && !upSeen[k] {
			upSeen[k] = true
			ups = append(ups, seriesUpsert{k, it.iv})
		}
	}
	anyClosed := len(s.pending) > 0
	s.mu.Unlock()
	if len(items) == 0 {
		s.mu.Lock()
		s.lastFlush, s.lastErr = s.o.Now(), ""
		s.mu.Unlock()
		return nil
	}

	// Encode outside the transaction (and outside the lock).
	for _, it := range items {
		if it.data != nil {
			continue
		}
		var err error
		it.data, err = blob.Encode(&blob.Block{Slot0: it.slot0, Interval: it.iv, Slots: it.slots})
		if err != nil {
			return s.flushFailed(items, fmt.Errorf("store: encode %v: %w", it.b.key, err))
		}
		if it.final {
			it.sum = summarize(it.slots)
		}
	}

	if _, err := s.writeTx(ctx, items, ups); err != nil {
		return s.flushFailed(items, err)
	}
	if anyClosed {
		if _, err := checkpoint(ctx, s.db.w); err != nil {
			s.o.Log.Warn("store: checkpoint", "err", err)
		}
	}
	return nil
}

type seriesUpsert struct {
	k  model.SeriesKey
	iv int64
}

func (s *SQLite) writeTx(ctx context.Context, items []*flushItem, ups []seriesUpsert) (map[model.SeriesKey]int64, error) {
	tx, err := s.db.w.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	s.mu.Lock()
	ids := make(map[model.SeriesKey]int64, len(s.ids))
	for k, v := range s.ids {
		ids[k] = v
	}
	s.mu.Unlock()
	newIDs := map[model.SeriesKey]int64{}
	for _, u := range ups {
		var id int64
		if err := tx.QueryRowContext(ctx, `INSERT INTO series(target, kind, interval_ms) VALUES(?,?,?)
			ON CONFLICT(target, kind) DO UPDATE SET interval_ms = excluded.interval_ms RETURNING id`,
			u.k.Target, int(u.k.Kind), u.iv).Scan(&id); err != nil {
			return nil, err
		}
		ids[u.k], newIDs[u.k] = id, id
	}
	insSample, err := tx.PrepareContext(ctx, `INSERT OR REPLACE INTO samples(hour, series, data) VALUES(?,?,?)`)
	if err != nil {
		return nil, err
	}
	defer insSample.Close()
	sums := map[dayKey][]hourSummary{}
	for _, it := range items {
		id := ids[it.b.key]
		if it.fresh {
			if err := s.mergeStored(ctx, tx, id, it); err != nil {
				return nil, err
			}
		}
		if _, err := insSample.ExecContext(ctx, it.b.hour, id, it.data); err != nil {
			return nil, err
		}
		if it.final {
			dk := dayKey{it.b.hour / 24, id}
			sums[dk] = append(sums[dk], hourSummary{it.b.hour, *it.sum})
		}
	}
	if err := writeSummaries(ctx, tx, sums, -1); err != nil {
		return nil, err
	}

	// Commit and update memory atomically for readers (see snapshot).
	s.commitMu.Lock()
	defer s.commitMu.Unlock()
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, id := range newIDs {
		s.ids[k], s.keys[id] = id, k
	}
	for _, u := range ups {
		s.ivs[u.k] = u.iv
	}
	done := map[*block]bool{}
	for _, it := range items {
		b := it.b
		switch {
		case it.final:
			done[b] = true
		case it.gen != b.gen:
			// Regridded since the snapshot: written again next time
			// (merging again if it was fresh; stored samples win).
		case it.merged != nil:
			// Continue from what was written: the stored samples plus
			// ours, then what arrived since the snapshot.
			m := it.merged
			extra := b.slots[it.n:]
			ns := place(slices.Clone(m.Slots), m.Slot0, m.Interval, extra, b.slot0+int64(it.n)*b.iv, b.iv)
			b.slot0, b.iv, b.slots = m.Slot0, m.Interval, ns
			b.gen++
			b.fresh = false
			b.flushed = -1
			if len(extra) == 0 && len(ns) == len(m.Slots) {
				b.flushed = len(ns)
			}
		default:
			b.fresh = false
			if it.n > b.flushed {
				b.flushed = it.n
			}
		}
	}
	kept := s.pending[:0]
	for _, b := range s.pending {
		if done[b] {
			s.pendBytes -= b.memSize()
		} else {
			kept = append(kept, b)
		}
	}
	clear(s.pending[len(kept):])
	s.pending = kept
	s.lastFlush, s.lastErr = s.o.Now(), ""
	return newIDs, nil
}

// mergeStored makes the first write of a block merge with the hour already
// stored for its series (possible only after the clock was corrected), so
// stored samples are never replaced.
func (s *SQLite) mergeStored(ctx context.Context, tx *sql.Tx, id int64, it *flushItem) error {
	var old []byte
	err := tx.QueryRowContext(ctx, `SELECT data FROM samples WHERE hour = ? AND series = ?`, it.b.hour, id).Scan(&old)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	stored, err := blob.Decode(old)
	if err != nil {
		s.o.Log.Warn("store: replacing unreadable hour", "series", it.b.key, "hour", it.b.hour, "err", err)
		return nil
	}
	mine := blob.Block{Slot0: it.slot0, Interval: it.iv, Slots: it.slots}
	if it.slots == nil && it.data != nil {
		if mine, err = blob.Decode(it.data); err != nil {
			return err
		}
	}
	m := mergeBlocks(stored, mine)
	if it.data, err = blob.Encode(&m); err != nil {
		return err
	}
	if it.final {
		it.sum = summarize(m.Slots)
	}
	it.merged = &m
	return nil
}

// flushFailed keeps the encoded closed hours (smaller than their slots) and
// records the error.
func (s *SQLite) flushFailed(items []*flushItem, err error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	inPending := map[*block]bool{}
	for _, b := range s.pending {
		inPending[b] = true
	}
	for _, it := range items {
		if it.final && it.data != nil && it.sum != nil && it.b.data == nil && inPending[it.b] {
			s.pendBytes -= it.b.memSize()
			it.b.data, it.b.sum, it.b.slots = it.data, it.sum, nil
			s.pendBytes += it.b.memSize()
		}
	}
	s.trimLocked()
	s.lastErr = err.Error()
	s.o.Log.Warn("store: flush failed; keeping data in memory", "err", err)
	return err
}

// Prune deletes every hour that ends at or before `before` (and incidents
// that ended before the first kept hour), returns the free pages to the OS
// (incremental_vacuum) and truncates the WAL.
//
// The retention it implies (Options.Now - before) is counted back from the
// newest stored hour, not only from the clock: a clock far ahead (e.g. at
// boot, before time sync) never deletes the history that the retention would
// keep. Hours starting more than an hour after Options.Now are ignored for
// this; if every stored hour is such a future hour, nothing is pruned.
func (s *SQLite) Prune(ctx context.Context, before time.Time) error {
	s.flushMu.Lock()
	defer s.flushMu.Unlock()
	if s.lock == nil {
		return errors.New("store: read-only")
	}
	hour, ok, err := s.pruneHour(ctx, before)
	if err != nil || !ok {
		return err
	}
	if err := s.prune(ctx, hour); err != nil {
		return err
	}
	_, err = checkpoint(ctx, s.db.w)
	return err
}

// pruneHour returns the first hour to keep, or ok=false to prune nothing.
func (s *SQLite) pruneHour(ctx context.Context, before time.Time) (int64, bool, error) {
	now := s.o.Now()
	limit := floorHour(now.Add(time.Hour).UnixMilli())
	var newest, future sql.NullInt64
	if err := s.db.w.QueryRowContext(ctx, `SELECT (SELECT max(hour) FROM samples WHERE hour <= ?), (SELECT count(*) FROM samples WHERE hour > ?)`,
		limit, limit).Scan(&newest, &future); err != nil {
		return 0, false, err
	}
	s.mu.Lock()
	for _, b := range append(slices.Clone(s.pending), mapValues(s.cur)...) {
		if b.hour <= limit && (!newest.Valid || b.hour > newest.Int64) {
			newest = sql.NullInt64{Int64: b.hour, Valid: true}
		}
	}
	warned := s.pruneLog
	s.pruneLog = s.pruneLog || (!newest.Valid && future.Int64 > 0)
	s.mu.Unlock()
	cut := before.UnixMilli()
	if newest.Valid {
		cut = min(cut, (newest.Int64+1)*hourMs-now.Sub(before).Milliseconds())
	} else if future.Int64 > 0 {
		if !warned {
			s.o.Log.Warn("store: every stored hour is after the current time; not pruning until the clock is right",
				"now", now.UTC(), "hours", future.Int64)
		}
		return 0, false, nil
	}
	return floorHour(cut), true, nil
}

// prune deletes hours before `hour` from samples and summary_1h (rewriting
// the day row that straddles it), then returns the freed pages with
// incremental_vacuum.
func (s *SQLite) prune(ctx context.Context, hour int64) error {
	tx, err := s.db.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM samples WHERE hour < ?`, hour); err != nil {
		return err
	}
	// Incidents that ended before the first kept hour go with it; ongoing
	// ones are always kept.
	if _, err := tx.ExecContext(ctx, `DELETE FROM incidents WHERE end_ms IS NOT NULL AND end_ms < ?`, hour*hourMs); err != nil {
		return err
	}
	day := hour / 24
	if _, err := tx.ExecContext(ctx, `DELETE FROM summary_1h WHERE day < ?`, day); err != nil {
		return err
	}
	if hour%24 != 0 {
		rows, err := tx.QueryContext(ctx, `SELECT series FROM summary_1h WHERE day = ?`, day)
		if err != nil {
			return err
		}
		edge := map[dayKey][]hourSummary{}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			edge[dayKey{day, id}] = nil
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if err := writeSummaries(ctx, tx, edge, hour); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return incrementalVacuum(ctx, s.db.w)
}

// Close flushes, checkpoints, closes the database and releases the lock.
// Call it last during shutdown (after the HTTP servers stopped reading).
func (s *SQLite) Close() error {
	if s.lock == nil {
		return s.db.Close()
	}
	errF := s.Flush(context.Background())
	s.flushMu.Lock()
	defer s.flushMu.Unlock()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()
	_, errC := checkpoint(context.Background(), s.db.w)
	return errors.Join(errF, errC, s.db.Close(), s.lock.release())
}
