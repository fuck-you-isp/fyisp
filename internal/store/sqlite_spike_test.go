//go:build unix

// Spike S3 gates for the SQLite storage setup. Every gate runs in a short
// form under plain `go test`; set FYISP_SPIKE=1 for the full-length runs
// (G2 20s of concurrent load, G3 20+ SIGKILLs, G4 96 hours).
package store

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/store/blob"
	"github.com/fuck-you-isp/fyisp/internal/store/synth"
)

func full() bool { return os.Getenv("FYISP_SPIKE") == "1" }

// headerPageSize reads the page size from the database file header.
func headerPageSize(t *testing.T, path string) int {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var h [100]byte
	if _, err := io.ReadFull(f, h[:]); err != nil {
		t.Fatalf("header: %v", err)
	}
	if string(h[:16]) != "SQLite format 3\x00" {
		t.Fatalf("bad magic %q", h[:16])
	}
	ps := int(binary.BigEndian.Uint16(h[16:18]))
	if ps == 1 {
		ps = 65536
	}
	return ps
}

func pragmaInt(t *testing.T, db *sql.DB, p string) int64 {
	t.Helper()
	var v int64
	if err := db.QueryRow("PRAGMA " + p).Scan(&v); err != nil {
		t.Fatalf("PRAGMA %s: %v", p, err)
	}
	return v
}

func pragmaStr(t *testing.T, db *sql.DB, p string) string {
	t.Helper()
	var v string
	if err := db.QueryRow("PRAGMA " + p).Scan(&v); err != nil {
		t.Fatalf("PRAGMA %s: %v", p, err)
	}
	return v
}

func mode(path string) string {
	fi, err := os.Stat(path)
	if err != nil {
		return "missing"
	}
	return fmt.Sprintf("%04o", fi.Mode().Perm())
}

func mustOpen(t testing.TB, path string) *DB {
	t.Helper()
	d, err := openDB(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// ---------------------------------------------------------------- G1

func TestG1Pragmas(t *testing.T) {
	old := syscall.Umask(0o022)
	defer syscall.Umask(old)
	dir := t.TempDir()

	// Alternatives, for the record: which DSN spellings actually take effect.
	for _, c := range []struct{ name, params string }{
		{"dsn-_pragma-list", "?_pragma=page_size(16384)&_pragma=auto_vacuum(INCREMENTAL)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"},
		{"dsn-shorthand", "?_busy_timeout=5000&_auto_vacuum=INCREMENTAL&_pragma=page_size(16384)&_journal_mode=WAL&_synchronous=NORMAL"},
		{"dsn-ps+jm", "?_pragma=page_size(16384)&_journal_mode=WAL"},
		{"dsn-ps-only", "?_pragma=page_size(16384)"},
		{"dsn-ps+av", "?_pragma=page_size(16384)&_auto_vacuum=INCREMENTAL"},
		{"dsn-av+ps-list", "?_pragma=auto_vacuum(2)&_pragma=page_size(16384)"},
	} {
		p := filepath.Join(dir, c.name+".db")
		db, err := sql.Open("sqlite", p+c.params)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`CREATE TABLE x(a)`); err != nil {
			t.Fatal(err)
		}
		t.Logf("G1 alt %-17s header page_size=%d auto_vacuum=%d journal_mode=%s  files: db=%s wal=%s shm=%s",
			c.name, headerPageSize(t, p), pragmaInt(t, db, "auto_vacuum"), pragmaStr(t, db, "journal_mode"),
			mode(p), mode(p+"-wal"), mode(p+"-shm"))
		db.Close()
	}

	p := filepath.Join(dir, "fyisp.db")
	d := mustOpen(t, p)
	ps, av, jm := headerPageSize(t, p), pragmaInt(t, d.W, "auto_vacuum"), pragmaStr(t, d.W, "journal_mode")
	syn, uv := pragmaInt(t, d.W, "synchronous"), pragmaInt(t, d.W, "user_version")
	qo, bt := pragmaInt(t, d.R, "query_only"), pragmaInt(t, d.R, "busy_timeout")
	t.Logf("G1 openDB: header page_size=%d auto_vacuum=%d journal_mode=%s synchronous=%d user_version=%d reader query_only=%d busy_timeout=%d",
		ps, av, jm, syn, uv, qo, bt)
	t.Logf("G1 modes (umask 022): db=%s wal=%s shm=%s", mode(p), mode(p+"-wal"), mode(p+"-shm"))
	if ps != PageSize || av != 2 || jm != "wal" || syn != 1 || uv != SchemaVersion || qo != 1 || bt != 5000 {
		t.Fatal("G1 FAIL")
	}
	for _, s := range []string{"", "-wal", "-shm"} {
		if m := mode(p + s); m != "0600" {
			t.Errorf("G1 FAIL: %s mode %s", p+s, m)
		}
	}
	if _, err := d.R.Exec(`INSERT INTO meta VALUES('x', 1)`); err == nil {
		t.Error("reader pool accepted a write")
	} else {
		t.Logf("G1 reader write rejected: %v", err)
	}
	d.Close()

	// Reopen: settings persist and nothing is re-applied.
	d = mustOpen(t, p)
	if headerPageSize(t, p) != PageSize || pragmaInt(t, d.W, "auto_vacuum") != 2 || pragmaStr(t, d.W, "journal_mode") != "wal" {
		t.Fatal("G1 FAIL after reopen")
	}
	// A newer schema is refused.
	if _, err := d.W.Exec(`PRAGMA user_version=99`); err != nil {
		t.Fatal(err)
	}
	d.Close()
	if _, err := openDB(context.Background(), p); !errors.Is(err, ErrNewerSchema) {
		t.Fatalf("newer schema: %v", err)
	}
	t.Log("G1 PASS")
}

// ---------------------------------------------------------------- helpers

// hourOf returns the hour number of unix ms.
func hourOf(ms int64) int64 { return ms / 3_600_000 }

func summarize(slots []blob.Slot) (n, lost int, lostBy []byte, mn, mean, mx, p95 sql.NullInt64) {
	var vals []int64
	var by [blob.MaxCode + 1]int
	var sum int64
	for _, s := range slots {
		switch {
		case s.Valid:
			vals = append(vals, s.Value)
			sum += s.Value
			n++
		case s.Code != 0:
			by[s.Code]++
			lost++
			n++
		}
	}
	for c := 1; c <= blob.MaxCode; c++ {
		lostBy = binary.AppendUvarint(lostBy, uint64(by[c]))
	}
	if len(vals) > 0 {
		slices.Sort(vals)
		mn = sql.NullInt64{Int64: vals[0], Valid: true}
		mx = sql.NullInt64{Int64: vals[len(vals)-1], Valid: true}
		mean = sql.NullInt64{Int64: sum / int64(len(vals)), Valid: true}
		p95 = sql.NullInt64{Int64: vals[(len(vals)*95+99)/100-1], Valid: true}
	}
	return
}

func putHour(ctx context.Context, tx *sql.Tx, hour, series int64, b *blob.Block, final bool) (int, error) {
	data, err := blob.Encode(b)
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO samples(hour, series, data) VALUES(?,?,?)`, hour, series, data); err != nil {
		return 0, err
	}
	if final {
		n, lost, by, mn, mean, mx, p95 := summarize(b.Slots)
		if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO summary_1h VALUES(?,?,?,?,?,?,?,?,?)`,
			hour, series, n, lost, by, mn, mean, mx, p95); err != nil {
			return 0, err
		}
	}
	return len(data), nil
}

func addSeries(t testing.TB, d *DB, n int, interval func(i int) int64) {
	t.Helper()
	for i := range n {
		if _, err := d.W.Exec(`INSERT OR IGNORE INTO series(id, target, kind, interval_ms) VALUES(?,?,?,?)`,
			i+1, fmt.Sprintf("target%03d", i), 1+i%3, interval(i)); err != nil {
			t.Fatal(err)
		}
	}
}

func pct(d []time.Duration, p float64) time.Duration {
	if len(d) == 0 {
		return 0
	}
	slices.Sort(d)
	return d[min(len(d)-1, int(float64(len(d))*p))]
}

func fsize(p string) int64 {
	fi, err := os.Stat(p)
	if err != nil {
		return 0
	}
	return fi.Size()
}

// ---------------------------------------------------------------- G2

func TestG2Concurrency(t *testing.T) {
	const nSeries = 90
	dur := 2 * time.Second
	if full() {
		dur = 20 * time.Second
	}
	ctx := context.Background()
	p := filepath.Join(t.TempDir(), "g2.db")
	d := mustOpen(t, p)
	defer d.Close()
	addSeries(t, d, nSeries, func(int) int64 { return 1000 })

	// 48 hours of history: 90 series x 3600 slots at 1s.
	const t0 = int64(1767225600000)
	h0 := hourOf(t0)
	hist := 48
	if !full() {
		hist = 6
	}
	gens := make([]*synth.Series, nSeries)
	for i := range gens {
		gens[i] = synth.New(7, i)
	}
	tx, _ := d.W.Begin()
	for h := range hist {
		for s := range nSeries {
			b := blob.Block{Slot0: t0 + int64(h)*3_600_000, Interval: 1000, Slots: gens[s].Fill(nil, 3600)}
			if _, err := putHour(ctx, tx, h0+int64(h), int64(s+1), &b, true); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := d.W.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	errs := map[string]int{}
	lat := map[string][]time.Duration{}
	var rows, pts int64
	record := func(kind string, dt time.Duration, err error) {
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			errs[err.Error()]++
			return
		}
		lat[kind] = append(lat[kind], dt)
	}

	// Writer: the per-minute rewrite, compressed to every ~100ms, 10 new slots
	// per series per rewrite; closes the hour (final blob + summary) at 3600.
	var cur atomic.Int64
	cur.Store(h0 + int64(hist))
	curSlots := make([][]blob.Slot, nSeries)
	var walMax int64
	var flushes int
	wg.Go(func() {
		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
			}
			closing := len(curSlots[0])+10 > 3600
			start := time.Now()
			tx, err := d.W.BeginTx(ctx, nil)
			if err != nil {
				record("writer", 0, err)
				continue
			}
			for s := range nSeries {
				curSlots[s] = gens[s].Fill(curSlots[s], min(10, 3600-len(curSlots[s])))
				b := blob.Block{Slot0: cur.Load() * 3_600_000, Interval: 1000, Slots: curSlots[s]}
				if _, err = putHour(ctx, tx, cur.Load(), int64(s+1), &b, closing); err != nil {
					break
				}
			}
			if err == nil {
				_, err = tx.ExecContext(ctx, `INSERT OR REPLACE INTO meta VALUES('flush', ?)`, time.Now().UnixMilli())
			}
			if err == nil {
				err = tx.Commit()
			} else {
				tx.Rollback()
			}
			record("writer", time.Since(start), err)
			flushes++
			if closing {
				cur.Add(1)
				for s := range curSlots {
					curSlots[s] = curSlots[s][:0:0]
				}
			}
			if w := fsize(p + "-wal"); w > walMax {
				walMax = w
			}
		}
	})

	// Readers: one query per panel, blobs decoded.
	panel := func(kind string, from, to int64, ids []any) {
		start := time.Now()
		args := append([]any{from, to}, ids...)
		rs, err := d.R.QueryContext(ctx, panelSQL(len(ids)), args...)
		if err != nil {
			record(kind, 0, err)
			return
		}
		var nr, np int64
		for rs.Next() {
			var h, s int64
			var data []byte
			if err = rs.Scan(&h, &s, &data); err != nil {
				break
			}
			b, derr := blob.Decode(data)
			if derr != nil {
				err = derr
				break
			}
			nr++
			np += int64(len(b.Slots))
		}
		if err == nil {
			err = rs.Err()
		}
		rs.Close()
		record(kind, time.Since(start), err)
		mu.Lock()
		rows += nr
		pts += np
		mu.Unlock()
	}
	nReaders := 4
	if v, err := strconv.Atoi(os.Getenv("FYISP_G2_READERS")); err == nil {
		nReaders = v
	}
	for r := range nReaders {
		wg.Go(func() {
			rnd := rand.New(rand.NewPCG(uint64(r), 9))
			for {
				select {
				case <-stop:
					return
				default:
				}
				c := cur.Load()
				switch rnd.IntN(3) {
				case 0: // 12 series, last 48h
					ids := make([]any, 12)
					for i := range ids {
						ids[i] = 1 + rnd.IntN(nSeries)
					}
					panel("48h x12", c-47, c, ids)
				case 1: // all series, last 2h
					ids := make([]any, nSeries)
					for i := range ids {
						ids[i] = i + 1
					}
					panel("2h x90", c-1, c, ids)
				case 2: // 6 series, current hour (live view)
					ids := make([]any, 6)
					for i := range ids {
						ids[i] = 1 + rnd.IntN(nSeries)
					}
					panel("1h x6", c, c, ids)
				}
			}
		})
	}
	time.Sleep(dur)
	close(stop)
	wg.Wait()
	var busy, walPages, ckpt int
	if err := d.W.QueryRow(`PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &walPages, &ckpt); err != nil {
		t.Fatal(err)
	}
	t.Logf("G2 after load: wal_checkpoint(TRUNCATE) busy=%d log=%d checkpointed=%d, wal now %d B", busy, walPages, ckpt, fsize(p+"-wal"))

	t.Logf("G2 %s, %d readers: writer flushes=%d (90 blobs each), hours closed=%d, WAL max %.1f MB, decoded rows=%d points=%d",
		dur, nReaders, flushes, cur.Load()-h0-int64(hist), float64(walMax)/1e6, rows, pts)
	for _, k := range []string{"writer", "1h x6", "2h x90", "48h x12"} {
		l := lat[k]
		t.Logf("G2 %-8s n=%5d p50=%7.2fms p99=%7.2fms max=%7.2fms", k, len(l),
			ms(pct(l, .5)), ms(pct(l, .99)), ms(pct(l, 1)))
	}
	if len(errs) > 0 {
		t.Fatalf("G2 FAIL errors: %v", errs)
	}
	t.Log("G2 PASS: no errors")
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

// ---------------------------------------------------------------- G3

// The crash child: flushes forever until killed. Generation g writes, in one
// transaction, the current hour (g/hourGens) for every series with
// (g%hourGens+1)*slotsPerGen slots of deterministic content, closes the
// previous hour on a boundary, prunes, and records meta.gen = g. After each
// commit it prints the generation on stdout.
const (
	crashSeries = 90
	hourGens    = 50
	slotsPerGen = 20
	keepHours   = 4
)

func crashSlot(series int64, i int) blob.Slot {
	if i%97 == 0 {
		return blob.Lost(uint8(i/97) % 16) // cycles through gap and every reason
	}
	return blob.Val((series*1000 + int64(i)*7919) % 50_000)
}

func crashBlock(series, hour int64, n int) blob.Block {
	b := blob.Block{Slot0: hour * 3_600_000, Interval: 1000, Slots: make([]blob.Slot, n)}
	for i := range b.Slots {
		b.Slots[i] = crashSlot(series, i)
	}
	return b
}

func TestG3CrashChild(t *testing.T) {
	path := os.Getenv("FYISP_CRASH_CHILD")
	if path == "" {
		t.Skip("child process only")
	}
	ctx := context.Background()
	d := mustOpen(t, path)
	addSeries(t, d, crashSeries, func(int) int64 { return 1000 })
	var g int64
	d.W.QueryRow(`SELECT value FROM meta WHERE key='gen'`).Scan(&g)
	for {
		g++
		hour := g / hourGens
		tx, err := d.W.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		for s := int64(1); s <= crashSeries; s++ {
			if g%hourGens == 0 && hour > 0 {
				b := crashBlock(s, hour-1, hourGens*slotsPerGen)
				if _, err := putHour(ctx, tx, hour-1, s, &b, true); err != nil {
					t.Fatal(err)
				}
			}
			b := crashBlock(s, hour, int(g%hourGens+1)*slotsPerGen)
			if _, err := putHour(ctx, tx, hour, s, &b, false); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := tx.Exec(`INSERT OR REPLACE INTO meta VALUES('gen', ?)`, g); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		if g%hourGens == 0 {
			if err := prune(ctx, d.W, hour-keepHours); err != nil {
				t.Fatal(err)
			}
		}
		fmt.Fprintf(os.Stdout, "committed %d\n", g)
	}
}

func TestG3Crash(t *testing.T) {
	iters := 3
	if full() {
		iters = 25
	}
	old := syscall.Umask(0o022)
	defer syscall.Umask(old)
	path := filepath.Join(t.TempDir(), "crash.db")
	rnd := rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), 1))
	var lastGen int64
	var totalFlushes int64
	for it := range iters {
		cmd := exec.Command(os.Args[0], "-test.run=^TestG3CrashChild$", "-test.v=false")
		cmd.Env = append(os.Environ(), "FYISP_CRASH_CHILD="+path)
		out, _ := cmd.StdoutPipe()
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		var reported atomicInt
		done := make(chan struct{})
		go func() {
			defer close(done)
			sc := bufio.NewScanner(out)
			for sc.Scan() {
				if g, ok := strings.CutPrefix(sc.Text(), "committed "); ok {
					v, _ := strconv.ParseInt(g, 10, 64)
					reported.set(v)
				}
			}
		}()
		// Wait for the first commit of this run, then kill at a random point.
		for deadline := time.Now().Add(30 * time.Second); reported.get() <= lastGen; {
			if time.Now().After(deadline) {
				cmd.Process.Kill()
				t.Fatal("child made no progress")
			}
			time.Sleep(time.Millisecond)
		}
		time.Sleep(time.Duration(rnd.IntN(1500)) * time.Millisecond)
		cmd.Process.Signal(syscall.SIGKILL)
		cmd.Wait()
		<-done
		rep := reported.get()
		walSize := fsize(path + "-wal")

		// Reopen and verify.
		d := mustOpen(t, path)
		ic := pragmaStr(t, d.W, "integrity_check")
		var g int64
		if err := d.R.QueryRow(`SELECT value FROM meta WHERE key='gen'`).Scan(&g); err != nil {
			t.Fatal(err)
		}
		if ic != "ok" {
			t.Fatalf("G3 FAIL iter %d: integrity_check = %s", it, ic)
		}
		if g < rep || g > rep+1 {
			t.Fatalf("G3 FAIL iter %d: db gen %d, child reported %d", it, g, rep)
		}
		nrows, nsum := verifyCrashDB(t, d, g)
		t.Logf("G3 iter %2d: killed after %4d flushes; reported gen %6d, db gen %6d (+%d uncommitted-to-stdout), wal at kill %.1f MB, %d sample rows / %d summary rows verified, integrity ok",
			it, rep-lastGen, rep, g, g-rep, float64(walSize)/1e6, nrows, nsum)
		totalFlushes += rep - lastGen
		lastGen = g
		d.Close()
	}
	t.Logf("G3 modes after crashes (umask 022): db=%s wal=%s shm=%s", mode(path), mode(path+"-wal"), mode(path+"-shm"))
	t.Logf("G3 PASS: %d kills, %d flushes, no corruption or lost commits", iters, totalFlushes)
}

func verifyCrashDB(t *testing.T, d *DB, g int64) (nrows, nsum int) {
	t.Helper()
	hour := g / hourGens
	rs, err := d.R.Query(`SELECT hour, series, data FROM samples ORDER BY hour, series`)
	if err != nil {
		t.Fatal(err)
	}
	defer rs.Close()
	seen := map[int64]int{}
	for rs.Next() {
		var h, s int64
		var data []byte
		if err := rs.Scan(&h, &s, &data); err != nil {
			t.Fatal(err)
		}
		b, err := blob.Decode(data)
		if err != nil {
			t.Fatalf("G3 FAIL: hour %d series %d undecodable: %v", h, s, err)
		}
		want := hourGens * slotsPerGen
		if h == hour {
			want = int(g%hourGens+1) * slotsPerGen
		}
		if h > hour || h < hour-keepHours-1 {
			t.Fatalf("G3 FAIL: unexpected hour %d (current %d)", h, hour)
		}
		exp := crashBlock(s, h, want)
		if len(b.Slots) != want || b.Slot0 != exp.Slot0 || !slices.Equal(b.Slots, exp.Slots) {
			t.Fatalf("G3 FAIL: hour %d series %d: %d slots, want %d (gen %d)", h, s, len(b.Slots), want, g)
		}
		seen[h]++
		nrows++
	}
	if err := rs.Err(); err != nil {
		t.Fatal(err)
	}
	if seen[hour] != crashSeries {
		t.Fatalf("G3 FAIL: current hour has %d series", seen[hour])
	}
	if err := d.R.QueryRow(`SELECT count(*) FROM summary_1h`).Scan(&nsum); err != nil {
		t.Fatal(err)
	}
	var open int
	d.R.QueryRow(`SELECT count(*) FROM summary_1h WHERE hour >= ?`, hour).Scan(&open)
	if open != 0 || nsum != nrows-crashSeries {
		t.Fatalf("G3 FAIL: %d summary rows for %d closed-hour sample rows (%d for open hour)", nsum, nrows-crashSeries, open)
	}
	return
}

type atomicInt struct {
	mu sync.Mutex
	v  int64
}

func (a *atomicInt) set(v int64) { a.mu.Lock(); a.v = v; a.mu.Unlock() }
func (a *atomicInt) get() int64  { a.mu.Lock(); defer a.mu.Unlock(); return a.v }

// ---------------------------------------------------------------- G4 (+ G5 size)

// g4Interval is a v0.1-like mix: 60 series @15s, 20 @5s, 10 @1s = 18 samples/s.
func g4Interval(i int) int64 {
	switch {
	case i < 60:
		return 15000
	case i < 80:
		return 5000
	}
	return 1000
}

// fillG4 creates a database at p (replacing the samples table with ddl when
// set) and writes hours of closed hours for 90 series.
func fillG4(t *testing.T, p string, hours int, ddl string) (d *DB, samples, payload int64) {
	ctx := context.Background()
	d = mustOpen(t, p)
	if ddl != "" {
		if _, err := d.W.Exec(`DROP TABLE samples; ` + ddl); err != nil {
			t.Fatal(err)
		}
		if err := incrementalVacuum(ctx, d.W); err != nil {
			t.Fatal(err)
		}
	}
	addSeries(t, d, 90, g4Interval)
	const t0 = int64(1767225600000)
	h0 := hourOf(t0)
	gens := make([]*synth.Series, 90)
	for i := range gens {
		gens[i] = synth.New(42, i)
	}
	for h := range hours {
		tx, _ := d.W.Begin()
		for s := range 90 {
			b := blob.Block{Slot0: t0 + int64(h)*3_600_000, Interval: g4Interval(s)}
			b.Slots = gens[s].Fill(nil, int(3_600_000/b.Interval))
			n, err := putHour(ctx, tx, h0+int64(h), int64(s+1), &b, true)
			if err != nil {
				t.Fatal(err)
			}
			samples += int64(len(b.Slots))
			payload += int64(n)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.W.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
	return d, samples, payload
}

// TestG5Layout compares samples table layouts for bytes/sample on disk.
func TestG5Layout(t *testing.T) {
	hours := 48
	if !full() {
		hours = 4
	}
	for _, c := range []struct{ name, ddl string }{
		{"schema (default)", ""},
		{"WITHOUT ROWID", `CREATE TABLE samples(hour INTEGER NOT NULL, series INTEGER NOT NULL, data BLOB NOT NULL, PRIMARY KEY(hour, series)) WITHOUT ROWID`},
		{"rowid + PK index", `CREATE TABLE samples(hour INTEGER NOT NULL, series INTEGER NOT NULL, data BLOB NOT NULL, PRIMARY KEY(hour, series))`},
	} {
		p := filepath.Join(t.TempDir(), "g5.db")
		d, samples, payload := fillG4(t, p, hours, c.ddl)
		sz := fsize(p)
		var tables []string
		if rs, err := d.W.Query(`SELECT name, sum(pgsize), sum(payload), sum(unused), count(*) FROM dbstat GROUP BY name ORDER BY 2 DESC`); err == nil {
			for rs.Next() {
				var name string
				var pg, pl, un, n int64
				rs.Scan(&name, &pg, &pl, &un, &n)
				tables = append(tables, fmt.Sprintf("%s=%dp/%.0f%%used", name, n, 100*float64(pl)/float64(pg)))
			}
			rs.Close()
		} else {
			tables = []string{"dbstat unavailable: " + err.Error()}
		}
		t.Logf("G5 %-17s %dh %d samples: payload %.3f B/sample, file %.3f B/sample  [%s]",
			c.name, hours, samples, float64(payload)/float64(samples), float64(sz)/float64(samples), strings.Join(tables, " "))
		d.Close()
	}
}

func TestG4Retention(t *testing.T) {
	hours := 96
	if !full() {
		hours = 8
	}
	ctx := context.Background()
	p := filepath.Join(t.TempDir(), "g4.db")
	d, samples, payload := fillG4(t, p, hours, "")
	defer d.Close()
	h0 := hourOf(1767225600000)
	checkpoint := func() {
		if _, err := d.W.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
			t.Fatal(err)
		}
	}
	stat := func(label string) (int64, int64) {
		checkpoint()
		sz, fl, pc := fsize(p), pragmaInt(t, d.W, "freelist_count"), pragmaInt(t, d.W, "page_count")
		t.Logf("G4 %-26s file %9d B  page_count %6d  freelist_count %6d  wal %d B", label, sz, pc, fl, fsize(p+"-wal"))
		return sz, fl
	}
	s1, _ := stat("filled:")
	var sumBytes int64
	d.W.QueryRow(`SELECT sum(pgsize) FROM dbstat WHERE name IN ('summary_1h')`).Scan(&sumBytes)
	t.Logf("G5 %d hours, %d samples: blob payload %.3f B/sample, db file %.3f B/sample (summary_1h pages %.3f B/sample)",
		hours, samples, float64(payload)/float64(samples), float64(s1)/float64(samples), float64(sumBytes)/float64(samples))

	// Prune half.
	cut := h0 + int64(hours/2)
	tx, _ := d.W.Begin()
	tx.Exec(`DELETE FROM samples WHERE hour < ?`, cut)
	tx.Exec(`DELETE FROM summary_1h WHERE hour < ?`, cut)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	s2, f2 := stat("after DELETE:")

	// Exec vs drained Query: check whether a plain Exec frees everything.
	if _, err := d.W.Exec(`PRAGMA incremental_vacuum(1)`); err != nil {
		t.Fatal(err)
	}
	_, f3 := stat("after Exec(iv(1)):")
	if _, err := d.W.Exec(`PRAGMA incremental_vacuum`); err != nil {
		t.Fatal(err)
	}
	s4, f4 := stat("after Exec(iv):")
	if err := incrementalVacuum(ctx, d.W); err != nil {
		t.Fatal(err)
	}
	s5, f5 := stat("after drained Query(iv):")
	t.Logf("G4 shrink %d -> %d B (%.0f%%), freelist %d -> %d -> %d -> %d",
		s2, s5, 100*float64(s5)/float64(s1), f2, f3, f4, f5)
	if !(s5 < s1*6/10 && f5 < f2 && f5 == 0) {
		t.Fatal("G4 FAIL")
	}
	_ = s4
	if ic := pragmaStr(t, d.W, "integrity_check"); ic != "ok" {
		t.Fatalf("integrity %s", ic)
	}
	t.Log("G4 PASS")
}
