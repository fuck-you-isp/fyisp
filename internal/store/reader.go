package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"os"
	"runtime"
	"slices"
	"sync"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/store/blob"
)

// memBlock is a reader's view of an hour still held in memory: the current
// hour of a series, or a closed hour not yet written. slots is a prefix of
// the writer's slice; the writer only appends, so it never changes.
type memBlock struct {
	key             model.SeriesKey
	hour, slot0, iv int64
	slots           []blob.Slot
	data            []byte // closed hour already encoded (slots is nil)
	sum             *summary
}

func (m *memBlock) block() (blob.Block, error) {
	if m.data != nil {
		return blob.Decode(m.data)
	}
	return blob.Block{Slot0: m.slot0, Interval: m.iv, Slots: m.slots}, nil
}

func (m *memBlock) summary() *summary {
	if m.sum != nil {
		return m.sum
	}
	return summarize(m.slots)
}

type hourKey struct {
	key  model.SeriesKey
	hour int64
}

// snapshot returns the in-memory hours of keys in [hFrom, hTo] and the
// series ids of keys. The caller holds commitMu for reading, so the memory
// view and the database snapshot it opens next agree: a flush either
// committed and dropped its closed hours from memory, or did neither.
func (s *SQLite) snapshot(keys []model.SeriesKey, hFrom, hTo int64) (map[hourKey]*memBlock, map[model.SeriesKey]int64) {
	want := make(map[model.SeriesKey]bool, len(keys))
	for _, k := range keys {
		want[k] = true
	}
	mem := map[hourKey]*memBlock{}
	ids := map[model.SeriesKey]int64{}
	s.mu.Lock()
	defer s.mu.Unlock()
	add := func(b *block) {
		if !want[b.key] || b.hour < hFrom || b.hour > hTo {
			return
		}
		mem[hourKey{b.key, b.hour}] = &memBlock{key: b.key, hour: b.hour, slot0: b.slot0, iv: b.iv,
			slots: b.slots[:len(b.slots):len(b.slots)], data: b.data, sum: b.sum}
	}
	for _, b := range s.pending {
		add(b)
	}
	for _, b := range s.cur {
		add(b)
	}
	for k := range want {
		if id, ok := s.ids[k]; ok {
			ids[k] = id
		}
	}
	return mem, ids
}

// msRange converts an inclusive time range to inclusive unix ms bounds.
func msRange(from, to time.Time) (int64, int64) {
	f := from.UnixMilli()
	if time.UnixMilli(f).Before(from) {
		f++ // a slot is a whole millisecond: round the start up
	}
	return f, to.UnixMilli()
}

func floorHour(ms int64) int64 {
	h := ms / hourMs
	if ms < 0 && ms%hourMs != 0 {
		h--
	}
	return h
}

// panelAcc accumulates one series' buckets.
type panelAcc struct {
	c   *SeriesCols
	sum []float64
}

func (a *panelAcc) lost(b int, code uint8, cnt uint32, n int) {
	a.c.Lost[b] += cnt
	if a.c.LostBy == nil {
		a.c.LostBy = map[model.Reason][]uint32{}
	}
	r := model.Reason(code)
	if a.c.LostBy[r] == nil {
		a.c.LostBy[r] = make([]uint32, n)
	}
	a.c.LostBy[r][b] += cnt
}

// Panel buckets several series over one range with ONE SQL query: raw
// blobs for ranges up to RawMaxRange, else the hourly summaries. Hours still
// in memory (the current hour, closed hours not yet flushed) are included.
// Semantics match Fake; in the hourly tier whole hours are counted (an hour
// belongs to the range if its start does).
func (s *SQLite) Panel(ctx context.Context, q PanelQuery) (*PanelResult, error) {
	if q.To.Before(q.From) {
		return nil, fmt.Errorf("store: panel range ends before it starts")
	}
	tier := TierRaw
	if q.To.Sub(q.From) > RawMaxRange {
		tier = TierHourly
	}
	step := BucketStep(q.From, q.To, q.MaxPoints, tier)
	start := q.From.Truncate(step)
	n := int(q.To.Sub(start)/step) + 1
	res := &PanelResult{Tier: tier, Start: start, Step: step, Series: make([]SeriesCols, len(q.Keys))}
	pc := &panelCtx{n: n, accs: map[model.SeriesKey][]*panelAcc{}, worker: map[model.SeriesKey]int{}}
	accList := make([]*panelAcc, len(q.Keys))
	for i, k := range q.Keys {
		c := &res.Series[i]
		*c = SeriesCols{Key: k, Mean: make([]float32, n), Min: make([]float32, n), Max: make([]float32, n),
			N: make([]uint32, n), Lost: make([]uint32, n)}
		for j := range c.Min {
			c.Min[j], c.Max[j] = float32(math.Inf(1)), float32(math.Inf(-1))
		}
		accList[i] = &panelAcc{c: c, sum: make([]float64, n)}
		if _, ok := pc.accs[k]; !ok {
			pc.worker[k] = len(pc.worker)
		}
		pc.accs[k] = append(pc.accs[k], accList[i])
	}
	pc.fromMs, pc.toMs = msRange(q.From, q.To)
	pc.startMs, pc.stepMs = start.UnixMilli(), step.Milliseconds()
	hFrom, hTo := floorHour(pc.fromMs), floorHour(pc.toMs)

	var query string
	if tier == TierRaw {
		query = `SELECT hour, series, data FROM samples WHERE hour BETWEEN ? AND ? AND series IN (`
	} else {
		query = `SELECT day, series, data FROM summary_1h WHERE day BETWEEN ? AND ? AND series IN (`
		hFrom, hTo = floorDiv(hFrom, 24), floorDiv(hTo, 24) // as days
	}

	s.commitMu.RLock()
	mem, ids := s.snapshot(q.Keys, floorHour(pc.fromMs), floorHour(pc.toMs))
	var rows *sql.Rows
	var more bool
	var err error
	pc.keyOf = make(map[int64]model.SeriesKey, len(ids))
	if len(ids) > 0 {
		args := []any{hFrom, hTo}
		for k, id := range ids {
			args = append(args, id)
			pc.keyOf[id] = k
		}
		rows, err = s.db.r.QueryContext(ctx, query+inList(len(ids))+`) ORDER BY 1, 2`, args...)
		if err == nil {
			more = rows.Next() // pins the read snapshot
		}
	}
	s.commitMu.RUnlock()
	if err != nil {
		return nil, err
	}
	if rows != nil {
		defer rows.Close()
	}
	if tier == TierRaw {
		err = pc.raw(rows, more, mem)
	} else {
		err = pc.hourly(rows, more, mem, floorHour(pc.fromMs), floorHour(pc.toMs))
	}
	if err != nil {
		return nil, err
	}

	for i := range res.Series {
		c, sum := &res.Series[i], accList[i].sum
		for j := range c.Mean {
			switch {
			case c.N[j] == 0:
				c.Mean[j], c.Min[j], c.Max[j] = nan32, nan32, nan32
			case tier == TierRaw:
				c.Mean[j] = float32(sum[j] / float64(c.N[j]))
			default: // sum of 10 µs units
				c.Mean[j] = float32(sum[j] / float64(c.N[j]) * blob.Unit / 1e6)
			}
		}
	}
	return res, nil
}

// panelCtx holds one Panel call's bucketing state.
type panelCtx struct {
	n                             int
	fromMs, toMs, startMs, stepMs int64
	accs                          map[model.SeriesKey][]*panelAcc
	worker                        map[model.SeriesKey]int // distinct key index
	keyOf                         map[int64]model.SeriesKey
}

// addRaw adds one block's slots in [fromMs, toMs].
func (pc *panelCtx) addRaw(k model.SeriesKey, b *blob.Block) {
	for _, a := range pc.accs[k] {
		t := b.Slot0
		for _, x := range b.Slots {
			if t > pc.toMs {
				break
			}
			if t >= pc.fromMs {
				i := int((t - pc.startMs) / pc.stepMs)
				switch {
				case x.Valid:
					ms := float32(unitMs(x.Value))
					a.c.N[i]++
					a.sum[i] += float64(ms)
					a.c.Min[i] = min(a.c.Min[i], ms)
					a.c.Max[i] = max(a.c.Max[i], ms)
				case x.Code != 0:
					a.lost(i, x.Code, 1, pc.n)
				}
			}
			t += b.Interval
		}
	}
}

// addSum adds one hourly summary (sum in 10 µs units).
func (pc *panelCtx) addSum(k model.SeriesKey, hour int64, u *summary) {
	i := int((hour*hourMs - pc.startMs) / pc.stepMs)
	for _, a := range pc.accs[k] {
		if u.n > 0 {
			a.c.N[i] += uint32(u.n)
			a.sum[i] += float64(u.sum)
			a.c.Min[i] = min(a.c.Min[i], float32(unitMs(u.min)))
			a.c.Max[i] = max(a.c.Max[i], float32(unitMs(u.max)))
		}
		for c := 1; c <= blob.MaxCode; c++ {
			if u.by[c] > 0 {
				a.lost(i, uint8(c), uint32(u.by[c]), pc.n)
			}
		}
	}
}

type rawJob struct {
	key  model.SeriesKey
	data []byte
	mem  *memBlock
}

// raw decodes blobs on several goroutines. Each series goes to one worker,
// in hour order, so its buckets are summed in time order (as in Fake) and
// no two workers touch the same accumulator.
func (pc *panelCtx) raw(rows *sql.Rows, more bool, mem map[hourKey]*memBlock) error {
	nw := min(runtime.GOMAXPROCS(0), 8, max(1, len(pc.worker)))
	jobs := make([]chan rawJob, nw)
	errs := make([]error, nw)
	var wg sync.WaitGroup
	for w := range jobs {
		jobs[w] = make(chan rawJob, 64)
		wg.Go(func() {
			var d blob.Decoder
			for j := range jobs[w] {
				if errs[w] != nil {
					continue
				}
				var b blob.Block
				var err error
				if j.mem != nil {
					b, err = j.mem.block()
				} else {
					b, err = d.Decode(j.data)
				}
				if err != nil {
					errs[w] = fmt.Errorf("store: %v: %w", j.key, err)
					continue
				}
				pc.addRaw(j.key, &b)
			}
		})
	}
	send := func(j rawJob) { jobs[pc.worker[j.key]%nw] <- j }
	memHours := sortedMem(mem, nil)
	mi := 0
	flushMem := func(before int64) {
		for ; mi < len(memHours) && memHours[mi].hour < before; mi++ {
			send(rawJob{key: memHours[mi].key, mem: memHours[mi]})
		}
	}
	var err error
	for ; more && rows != nil; more = rows.Next() {
		var hour, id int64
		var data []byte
		if err = rows.Scan(&hour, &id, &data); err != nil {
			break
		}
		flushMem(hour)
		k := pc.keyOf[id]
		if _, ok := mem[hourKey{k, hour}]; ok {
			continue
		}
		send(rawJob{key: k, data: data})
	}
	if err == nil && rows != nil {
		err = rows.Err()
	}
	flushMem(math.MaxInt64)
	for _, c := range jobs {
		close(c)
	}
	wg.Wait()
	return errors.Join(append(errs, err)...)
}

func (pc *panelCtx) hourly(rows *sql.Rows, more bool, mem map[hourKey]*memBlock, hFrom, hTo int64) error {
	var hs []hourSummary
	for ; more && rows != nil; more = rows.Next() {
		var day, id int64
		var data []byte
		if err := rows.Scan(&day, &id, &data); err != nil {
			return err
		}
		var err error
		if hs, err = decodeDay(hs[:0], day, data); err != nil {
			return fmt.Errorf("store: day %d series %d: %w", day, id, err)
		}
		k := pc.keyOf[id]
		for i := range hs {
			h := &hs[i]
			if h.hour < hFrom || h.hour > hTo {
				continue
			}
			if _, ok := mem[hourKey{k, h.hour}]; ok {
				continue
			}
			pc.addSum(k, h.hour, &h.s)
		}
	}
	if rows != nil {
		if err := rows.Err(); err != nil {
			return err
		}
	}
	for _, m := range mem {
		pc.addSum(m.key, m.hour, m.summary())
	}
	return nil
}

// sortedMem returns the memory blocks (of key k, or all when k is nil) in
// hour order.
func sortedMem(mem map[hourKey]*memBlock, k *model.SeriesKey) []*memBlock {
	var out []*memBlock
	for hk, m := range mem {
		if k == nil || hk.key == *k {
			out = append(out, m)
		}
	}
	slices.SortFunc(out, func(a, b *memBlock) int { return int(a.hour - b.hour) })
	return out
}

// Raw calls fn for every stored slot of keys in [from, to], key by key in
// the given order, in time order: measured values, losses, and not-measured
// slots (Lost true, Reason ReasonGap). It includes hours still in memory.
// It reads one consistent snapshot.
func (s *SQLite) Raw(ctx context.Context, keys []model.SeriesKey, from, to time.Time, fn func(RawPoint) error) error {
	fromMs, toMs := msRange(from, to)
	if toMs < fromMs {
		return nil
	}
	hFrom, hTo := floorHour(fromMs), floorHour(toMs)

	tx, err := s.db.r.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	s.commitMu.RLock()
	mem, ids := s.snapshot(keys, hFrom, hTo)
	var nseries int64
	err = tx.QueryRowContext(ctx, `SELECT count(*) FROM series`).Scan(&nseries) // pins the read snapshot
	s.commitMu.RUnlock()
	if err != nil {
		return err
	}

	emit := func(k model.SeriesKey, b *blob.Block) error {
		t := b.Slot0
		for _, x := range b.Slots {
			if t > toMs {
				break
			}
			if t >= fromMs {
				p := RawPoint{Key: k, TS: time.UnixMilli(t).UTC()}
				if x.Valid {
					p.RTTms = unitMs(x.Value)
				} else {
					p.Lost, p.Reason = true, model.Reason(x.Code)
				}
				if err := fn(p); err != nil {
					return err
				}
			}
			t += b.Interval
		}
		return nil
	}

	var dec blob.Decoder
	for _, k := range keys {
		memHours := sortedMem(mem, &k)
		mi := 0
		flushMem := func(before int64) error {
			for ; mi < len(memHours) && memHours[mi].hour < before; mi++ {
				b, err := memHours[mi].block()
				if err != nil {
					return err
				}
				if err := emit(k, &b); err != nil {
					return err
				}
			}
			return nil
		}
		if id, ok := ids[k]; ok {
			if err := func() error {
				rows, err := tx.QueryContext(ctx, `SELECT hour, data FROM samples WHERE hour BETWEEN ? AND ? AND series = ? ORDER BY hour`, hFrom, hTo, id)
				if err != nil {
					return err
				}
				defer rows.Close()
				for rows.Next() {
					var hour int64
					var data []byte
					if err := rows.Scan(&hour, &data); err != nil {
						return err
					}
					if err := flushMem(hour); err != nil {
						return err
					}
					if _, ok := mem[hourKey{k, hour}]; ok {
						continue
					}
					b, err := dec.Decode(data)
					if err != nil {
						return fmt.Errorf("store: hour %d series %d: %w", hour, id, err)
					}
					if err := emit(k, &b); err != nil {
						return err
					}
				}
				return rows.Err()
			}(); err != nil {
				return err
			}
		}
		if err := flushMem(math.MaxInt64); err != nil {
			return err
		}
	}
	return nil
}

// Series lists every series with stored data. First and Last are the first
// and last points Raw returns for it.
func (s *SQLite) Series(ctx context.Context) ([]SeriesInfo, error) {
	type span struct {
		id               int64
		first, last      time.Time
		minHour, maxHour int64
		hasDB            bool
	}
	spans := map[model.SeriesKey]*span{}
	s.commitMu.RLock()
	s.mu.Lock()
	keys := make(map[int64]model.SeriesKey, len(s.keys))
	for id, k := range s.keys {
		keys[id] = k
	}
	ivs := make(map[model.SeriesKey]int64, len(s.ivs))
	for k, iv := range s.ivs {
		ivs[k] = iv
	}
	var mem []*memBlock
	for _, b := range append(slices.Clone(s.pending), mapValues(s.cur)...) {
		mem = append(mem, &memBlock{key: b.key, hour: b.hour, slot0: b.slot0, iv: b.iv,
			slots: b.slots[:len(b.slots):len(b.slots)], data: b.data, sum: b.sum})
		if _, ok := ivs[b.key]; !ok {
			ivs[b.key] = b.iv
		}
	}
	s.mu.Unlock()
	tx, err := s.db.r.BeginTx(ctx, nil)
	if err != nil {
		s.commitMu.RUnlock()
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT series, min(hour), max(hour) FROM samples GROUP BY series`)
	s.commitMu.RUnlock()
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var sp span
		if err := rows.Scan(&sp.id, &sp.minHour, &sp.maxHour); err != nil {
			rows.Close()
			return nil, err
		}
		if k, ok := keys[sp.id]; ok {
			sp.hasDB = true
			spans[k] = &sp
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	stored := func(id, hour int64) (blob.Block, error) {
		var data []byte
		if err := tx.QueryRowContext(ctx, `SELECT data FROM samples WHERE hour = ? AND series = ?`, hour, id).Scan(&data); err != nil {
			return blob.Block{}, err
		}
		return blob.Decode(data)
	}
	for k, sp := range spans {
		b, err := stored(sp.id, sp.minHour)
		if err != nil {
			return nil, fmt.Errorf("store: series %v: %w", k, err)
		}
		sp.first = time.UnixMilli(b.Slot0).UTC()
		if sp.maxHour != sp.minHour {
			if b, err = stored(sp.id, sp.maxHour); err != nil {
				return nil, fmt.Errorf("store: series %v: %w", k, err)
			}
		}
		sp.last = time.UnixMilli(b.Slot0 + int64(max(0, len(b.Slots)-1))*b.Interval).UTC()
	}
	for _, m := range mem {
		b, err := m.block()
		if err != nil || len(b.Slots) == 0 {
			continue
		}
		f := time.UnixMilli(b.Slot0).UTC()
		l := time.UnixMilli(b.Slot0 + int64(len(b.Slots)-1)*b.Interval).UTC()
		sp := spans[m.key]
		if sp == nil {
			sp = &span{first: f, last: l, minHour: m.hour, maxHour: m.hour}
			spans[m.key] = sp
			continue
		}
		if m.hour <= sp.minHour {
			sp.first, sp.minHour = f, m.hour
		}
		if m.hour >= sp.maxHour {
			sp.last, sp.maxHour = l, m.hour
		}
	}
	out := make([]SeriesInfo, 0, len(spans))
	for k, sp := range spans {
		out = append(out, SeriesInfo{ID: sp.id, Key: k, Interval: time.Duration(ivs[k]) * time.Millisecond, First: sp.first, Last: sp.last})
	}
	slices.SortFunc(out, func(a, b SeriesInfo) int {
		if a.Key.Target != b.Key.Target {
			if a.Key.Target < b.Key.Target {
				return -1
			}
			return 1
		}
		return int(a.Key.Kind) - int(b.Key.Kind)
	})
	return out, nil
}

func mapValues[K comparable, V any](m map[K]V) []V {
	out := make([]V, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

// Stats reports file sizes (database + WAL), the oldest stored hour and the
// flush state.
func (s *SQLite) Stats(ctx context.Context) (Stats, error) {
	st := Stats{Dir: s.dir}
	for _, suf := range []string{"", "-wal"} {
		if fi, err := os.Stat(s.db.path + suf); err == nil {
			st.FileBytes += fi.Size()
		}
	}
	var oldest sql.NullInt64
	if err := s.db.r.QueryRowContext(ctx, `SELECT min(hour) FROM samples`).Scan(&oldest); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return st, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, b := range append(slices.Clone(s.pending), mapValues(s.cur)...) {
		if !oldest.Valid || b.hour < oldest.Int64 {
			oldest = sql.NullInt64{Int64: b.hour, Valid: true}
		}
		st.BufferedBytes += int64(b.memSize())
	}
	if oldest.Valid {
		st.Oldest = time.Unix(oldest.Int64*3600, 0).UTC()
	}
	st.LastFlush, st.LastFlushErr = s.lastFlush, s.lastErr
	st.Dropped, st.Rejected = s.dropped, s.rejected
	return st, nil
}
