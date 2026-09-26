// Package store persists probe results in SQLite (modernc.org/sqlite).
//
// This file holds the connection setup and the schema migrations. The open
// sequence was proven by spike S3 (see docs/PLAN.md "Storage").
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// PageSize is the database page size, fixed when the file is created.
const PageSize = 16384

// Per-connection settings go in the DSN so they survive the pool reopening a
// connection. Persistent settings (page_size, auto_vacuum, journal_mode) are
// executed explicitly in openConns: modernc runs _pragma DSN values in
// *lexicographic* order (busy_timeout first), so "_pragma=page_size(..)" lands
// after "_pragma=journal_mode(WAL)", which has already written page 1 with the
// default 4096 page size, and the page_size is silently ignored.
//
// The path is passed without a "file:" prefix: modernc then strips the query
// and opens the plain path, so Windows paths and '%' need no URI escaping.
// (A path containing '?' is not supported.)
//
// wal_autocheckpoint(256) checkpoints every 4 MB of WAL instead of the default
// 1000 pages (16 MB at 16 KB pages); journal_size_limit truncates the WAL
// back to 16 MB after a reset. Neither helps while readers overlap
// continuously (checkpoint starvation): the WAL then grows until a gap
// between reads, which is why Flush runs wal_checkpoint(TRUNCATE) after each
// hour close and prune.
const (
	writerParams = "?_busy_timeout=5000&_synchronous=NORMAL&_txlock=immediate&_pragma=wal_autocheckpoint(256)&_pragma=journal_size_limit(16777216)"
	readerParams = "?_busy_timeout=5000&_query_only=1&_pragma=cache_size(-1024)"
)

// Reader pool memory. Each SQLite connection keeps its own page cache
// (default 2000 KiB), outside the Go heap, for as long as it stays open, and
// the pool opens up to max(4, GOMAXPROCS) of them. Panel queries scan far
// more blob pages than any cache holds (the OS page cache serves those), so
// readers get 1 MiB each, and a connection idle for readerIdleTime is
// closed: a burst of dashboard panels opens several, a dashboard left open
// keeps them busy, and a process nobody looks at holds none.
const readerIdleTime = time.Minute

// migrations[i] brings the schema from version i to i+1. Append only: never
// edit a released migration. The schema version (PRAGMA user_version) is
// len(migrations).
var migrations = []string{
	// v1 (fyisp v0.1)
	`
CREATE TABLE series(
	id          INTEGER PRIMARY KEY,
	target      TEXT    NOT NULL,
	kind        INTEGER NOT NULL,
	interval_ms INTEGER NOT NULL,
	UNIQUE(target, kind)
);
-- A rowid table on purpose: in a WITHOUT ROWID table (an index b-tree) a
-- row larger than ~4 KB on a 16 KB page spills into an overflow page that is
-- mostly empty, and hourly blobs of 1 s series are ~5 KB. Measured (spike
-- S3): file 3.75 B/sample (39% page use) WITHOUT ROWID vs 1.70 B/sample here.
CREATE TABLE samples(
	hour   INTEGER NOT NULL, -- unix seconds / 3600
	series INTEGER NOT NULL,
	data   BLOB    NOT NULL, -- blob v1
	PRIMARY KEY(hour, series)
);
-- Hourly summaries of closed hours, packed per UTC day and series (format
-- in summary.go: n, lost, lost per reason, min, max, p95, sum per hour).
-- Long-range panels read these; a row per hour would be too many rows.
CREATE TABLE summary_1h(
	day    INTEGER NOT NULL, -- unix seconds / 86400
	series INTEGER NOT NULL,
	data   BLOB    NOT NULL,
	PRIMARY KEY(day, series)
) WITHOUT ROWID;
CREATE TABLE meta(
	key   TEXT PRIMARY KEY,
	value ANY
) WITHOUT ROWID;
`,
	// v2 (fyisp v0.2): the outage log (see incidents.go).
	`
CREATE TABLE incidents(
	id         INTEGER PRIMARY KEY,
	start_ms   INT  NOT NULL,
	end_ms     INT,  -- NULL while ongoing
	kind       TEXT NOT NULL,
	summary    TEXT NOT NULL,
	targets    TEXT, -- JSON array of target names, NULL if none
	peak_loss  REAL,
	updated_ms INT   -- last SaveIncident (store clock)
);
CREATE INDEX incidents_start ON incidents(start_ms);
`,
	// v3 (fyisp v0.2): traceroutes (see trace.go). A series gains the hop
	// (TTL) of KindTrace series, 0 otherwise; the table is rebuilt because
	// the UNIQUE constraint changes. Ids are copied, so samples and
	// summary_1h rows keep pointing at the same series.
	`
CREATE TABLE series_v3(
	id          INTEGER PRIMARY KEY,
	target      TEXT    NOT NULL,
	kind        INTEGER NOT NULL,
	hop         INTEGER NOT NULL DEFAULT 0,
	interval_ms INTEGER NOT NULL,
	UNIQUE(target, kind, hop)
);
INSERT INTO series_v3(id, target, kind, hop, interval_ms) SELECT id, target, kind, 0, interval_ms FROM series;
DROP TABLE series;
ALTER TABLE series_v3 RENAME TO series;
-- One row per router address seen on a traced path. Unknown rdns, asn and
-- owner are NULL.
CREATE TABLE hop_info(
	ip            TEXT PRIMARY KEY,
	rdns          TEXT,
	asn           INTEGER,
	owner         TEXT,
	first_seen_ms INT NOT NULL,
	last_seen_ms  INT NOT NULL
);
-- The hops columns are JSON arrays of addresses, "" for a hop that did not
-- answer; element i is TTL i+1.
CREATE TABLE route_changes(
	id         INTEGER PRIMARY KEY,
	target     TEXT NOT NULL,
	at_ms      INT  NOT NULL,
	from_hops  TEXT NOT NULL,
	to_hops    TEXT NOT NULL,
	first_diff INT  NOT NULL
);
CREATE INDEX route_changes_at ON route_changes(at_ms);
-- The current route of every traced target.
CREATE TABLE routes(
	target   TEXT PRIMARY KEY,
	hops     TEXT NOT NULL,
	since_ms INT  NOT NULL
);
`,
	// v4 (fyisp v0.3): annotations and evidence report snapshots (see
	// annotations.go, reports.go). summary_1h day rows gain hourly medians
	// (day format v2, summary.go) without a schema change: both formats are
	// read.
	`
CREATE TABLE annotations(
	id         INTEGER PRIMARY KEY,
	at_ms      INT  NOT NULL,
	end_ms     INT,  -- NULL for a point in time
	text       TEXT NOT NULL,
	public     INT  NOT NULL DEFAULT 0,
	created_ms INT,
	updated_ms INT
);
CREATE INDEX annotations_at ON annotations(at_ms);
-- html is the rendered report, zstd-compressed; bytes is its uncompressed
-- size (listed without reading the blob).
CREATE TABLE reports(
	id         TEXT PRIMARY KEY,
	title      TEXT,
	from_ms    INT,
	to_ms      INT,
	created_ms INT,
	public     INT  NOT NULL DEFAULT 0,
	bytes      INT  NOT NULL DEFAULT 0,
	html       BLOB NOT NULL
);
CREATE INDEX reports_created ON reports(created_ms);
`,
}

// SchemaVersion is the schema version this build writes.
func SchemaVersion() int { return len(migrations) }

// conns is the writer/reader handle pair.
type conns struct {
	path string
	w    *sql.DB // exactly one connection; all writes go here (nil when read-only)
	r    *sql.DB // query_only pool for UI/API reads
}

// ErrNewerSchema means the file was written by a newer fyisp.
var ErrNewerSchema = errors.New("store: database schema is newer than this build")

// ErrNotDatabase means the file exists but is not a fyisp database.
var ErrNotDatabase = errors.New("store: not a fyisp database")

// openConns opens (creating if needed) the database at path.
//
// Sequence:
//  1. create the file 0600 if missing: SQLite gives -wal/-shm the same mode
//     as the main file, so they come out 0600 too;
//  2. writer pool, MaxOpenConns=1, DSN: busy_timeout, synchronous=NORMAL,
//     BEGIN IMMEDIATE;
//  3. refuse newer schema versions;
//  4. on an empty file: PRAGMA page_size=16384; PRAGMA auto_vacuum=INCREMENTAL;
//  5. PRAGMA journal_mode=WAL (persistent; writes page 1, fixing page_size);
//  6. migrations (with a VACUUM INTO backup when upgrading an existing file);
//  7. reader pool, DSN: busy_timeout, query_only.
func openConns(ctx context.Context, path, version string, now time.Time) (_ *conns, err error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	f.Close()

	w, err := sql.Open("sqlite", path+writerParams+tempDirParam(path))
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			w.Close()
		}
	}()
	w.SetMaxOpenConns(1)
	w.SetMaxIdleConns(1)
	w.SetConnMaxLifetime(0)
	w.SetConnMaxIdleTime(0)

	var nobj, ver int
	if err := w.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_schema`).Scan(&nobj); err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrNotDatabase, path, err)
	}
	if err := w.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&ver); err != nil {
		return nil, err
	}
	if ver > SchemaVersion() {
		return nil, fmt.Errorf("%w (%d > %d)", ErrNewerSchema, ver, SchemaVersion())
	}
	if ver == 0 && nobj != 0 {
		return nil, fmt.Errorf("%w: %s has tables but no schema version", ErrNotDatabase, path)
	}
	if nobj == 0 {
		for _, p := range []string{
			fmt.Sprintf("PRAGMA page_size=%d", PageSize),
			"PRAGMA auto_vacuum=INCREMENTAL",
		} {
			if _, err := w.ExecContext(ctx, p); err != nil {
				return nil, fmt.Errorf("store: %s: %w", p, err)
			}
		}
	}
	var mode string
	if err := w.QueryRowContext(ctx, `PRAGMA journal_mode=WAL`).Scan(&mode); err != nil {
		return nil, err
	}
	if !strings.EqualFold(mode, "wal") {
		return nil, fmt.Errorf("store: journal_mode is %q, want wal", mode)
	}
	if ver < SchemaVersion() {
		if err := migrate(ctx, w, path, ver, version, now); err != nil {
			return nil, err
		}
	}

	r, err := openReader(ctx, path)
	if err != nil {
		return nil, err
	}
	return &conns{path: path, w: w, r: r}, nil
}

func openReader(ctx context.Context, path string) (*sql.DB, error) {
	r, err := sql.Open("sqlite", path+readerParams+tempDirParam(path))
	if err != nil {
		return nil, err
	}
	n := max(4, runtime.GOMAXPROCS(0))
	r.SetMaxOpenConns(n)
	r.SetMaxIdleConns(n)
	r.SetConnMaxIdleTime(readerIdleTime)
	if err := r.PingContext(ctx); err != nil {
		r.Close()
		return nil, err
	}
	return r, nil
}

// migrate applies migrations[from:] in one transaction. An existing database
// (from > 0) is first copied to <path>.bak-v<from> with VACUUM INTO.
func migrate(ctx context.Context, w *sql.DB, path string, from int, version string, now time.Time) error {
	if from > 0 {
		bak := fmt.Sprintf("%s.bak-v%d", path, from)
		// VACUUM INTO needs a missing or empty file; create it empty so it
		// gets mode 0600 instead of the umask default.
		f, err := os.OpenFile(bak, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
		if err != nil {
			return fmt.Errorf("store: backup before migration: %w", err)
		}
		f.Close()
		if _, err := w.ExecContext(ctx, `VACUUM INTO ?`, bak); err != nil {
			return fmt.Errorf("store: backup before migration: %w", err)
		}
	}
	tx, err := w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for v := from; v < len(migrations); v++ {
		if _, err := tx.ExecContext(ctx, migrations[v]); err != nil {
			return fmt.Errorf("store: migrate to v%d: %w", v+1, err)
		}
	}
	if from == 0 {
		if _, err := tx.ExecContext(ctx, `INSERT INTO meta(key, value) VALUES('created_by_version', ?), ('created_at', ?)`,
			version, now.UTC().Format(time.RFC3339)); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version=%d", len(migrations))); err != nil {
		return err
	}
	return tx.Commit()
}

// Close closes the reader pool, then the writer (whose close checkpoints and
// removes the WAL when it is the last connection).
func (d *conns) Close() error {
	var errW error
	if d.w != nil {
		errW = d.w.Close()
	}
	return errors.Join(d.r.Close(), errW)
}

// incrementalVacuum returns all free pages to the OS. The pragma yields no
// rows, but it is run through Query and drained so every step executes
// regardless of how the driver treats Exec (spike S3 G4).
func incrementalVacuum(ctx context.Context, w *sql.DB) error {
	rows, err := w.QueryContext(ctx, `PRAGMA incremental_vacuum`)
	if err != nil {
		return err
	}
	for rows.Next() {
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	return rows.Close()
}

// checkpoint runs wal_checkpoint(TRUNCATE). Readers holding a snapshot make
// it return busy (or fail with SQLITE_BUSY after busy_timeout); both are
// tolerated: the next checkpoint catches up.
func checkpoint(ctx context.Context, w *sql.DB) (busy bool, err error) {
	var b, logPages, done int
	if err := w.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&b, &logPages, &done); err != nil {
		if strings.Contains(err.Error(), "busy") || strings.Contains(err.Error(), "locked") {
			return true, nil
		}
		return false, err
	}
	return b != 0, nil
}

// inList returns "?,?,?" for n placeholders.
func inList(n int) string { return strings.TrimSuffix(strings.Repeat("?,", n), ",") }

// tempDirParam points SQLite's temporary files (used by VACUUM INTO backups
// before migrations, and large sorts) at a private directory next to the
// database. Minimal systems such as the scratch Docker image have no /tmp,
// /var/tmp or TMPDIR, and SQLite then fails with SQLITE_IOERR_GETTEMPPATH
// ("disk I/O error (6410)"). The environment cannot be used instead: the
// transpiled libc snapshots it at process start. Windows uses GetTempPath.
func tempDirParam(dbPath string) string {
	if runtime.GOOS == "windows" {
		return ""
	}
	dir := filepath.Join(filepath.Dir(dbPath), "tmp")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ""
	}
	return "&_pragma=" + url.QueryEscape("temp_store_directory('"+strings.ReplaceAll(dir, "'", "''")+"')")
}
