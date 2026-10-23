// Probe results in SQLite (modernc.org/sqlite): this file holds the
// connection setup and the schema.

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
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

// schema is the whole database, created in one go in a new file. Its
// version (PRAGMA user_version) is SchemaVersion. A change is additive only
// (new tables, columns and indexes; nothing dropped, renamed or retyped): it
// goes into schema, bumps schemaVersion, and adds the step from the previous
// version to migrations, which upgrades the files of earlier releases in
// place (see prepare).
const schema = `
CREATE TABLE series(
	id          INTEGER PRIMARY KEY,
	target      TEXT    NOT NULL,
	kind        INTEGER NOT NULL,
	hop         INTEGER NOT NULL DEFAULT 0, -- the TTL of a KindTrace series, 0 otherwise
	interval_ms INTEGER NOT NULL,
	UNIQUE(target, kind, hop)
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
-- in summary.go: n, lost, lost per reason, min, max, p95, sum and median per
-- hour). Long-range panels read these; a row per hour would be too many rows.
CREATE TABLE summary_1h(
	day    INTEGER NOT NULL, -- unix seconds / 86400
	series INTEGER NOT NULL,
	data   BLOB    NOT NULL,
	PRIMARY KEY(day, series)
) WITHOUT ROWID;
-- created_by_version and created_at, and the settings changed in the
-- dashboard (key 'setting:<name>').
CREATE TABLE meta(
	key   TEXT PRIMARY KEY,
	value ANY
) WITHOUT ROWID;
-- The outage log (see incidents.go).
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
-- Traceroutes (see trace.go). One row per router address seen on a traced
-- path. Unknown rdns, asn and owner are NULL.
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
-- Speed test results (see speedtests.go).
CREATE TABLE speedtests(
	id              INTEGER PRIMARY KEY,
	start_ms        INT  NOT NULL,
	end_ms          INT  NOT NULL,
	trigger         TEXT NOT NULL,
	outcome         TEXT NOT NULL,
	region          TEXT NOT NULL DEFAULT '',
	server          TEXT NOT NULL,
	fallback        INT  NOT NULL DEFAULT 0,
	fallback_level  TEXT NOT NULL DEFAULT 'none',
	capped_server   INT  NOT NULL DEFAULT 0,
	down_s          REAL NOT NULL DEFAULT 0,
	up_s            REAL NOT NULL DEFAULT 0,
	bytes           INT  NOT NULL DEFAULT 0,
	score_down      REAL,
	score_up        REAL,
	extra           TEXT,
	port            INT  NOT NULL DEFAULT 0,
	attempts        INT  NOT NULL DEFAULT 0,
	down_bps        REAL,
	up_bps          REAL,
	down_retrans    INT,
	up_retrans      INT,
	latency_json    TEXT,
	conn            TEXT NOT NULL DEFAULT '',
	handoff_asn     INT,
	handoff_name    TEXT,
	isp_asn         INT,
	handoff_ms      INT
);
CREATE INDEX speedtests_start ON speedtests(start_ms);
` + storageDDL

// storageDDL is the storage tests' tables (schema v9). They never hold an
// endpoint's host, a bucket, a CA, a key, the install id (meta has it), the
// local address, the public address, a full edge address (only its /24),
// a raw body or header, or an error's text.
const storageDDL = `
-- A campaign: its shape and settings, and a snapshot of its targets (JSON
-- [{id, name, mode}], mode 'conn' or 'bucket').
CREATE TABLE storage_campaigns(
	id           INTEGER PRIMARY KEY,
	created_ms   INT  NOT NULL,
	updated_ms   INT  NOT NULL,
	shape        TEXT NOT NULL, -- 'on_demand' | 'recurring'
	state        TEXT NOT NULL, -- 'scheduled' | 'running' | 'paused' | 'done' | 'stopped'
	stop_reason  TEXT,          -- 'owner' | 'finished' | 'until' | 'removed'
	targets      TEXT NOT NULL,
	cold         INT  NOT NULL,
	warm         INT  NOT NULL,
	down_bytes   INT  NOT NULL,
	up_bytes     INT  NOT NULL,
	interval_s   INT  NOT NULL,
	start_ms     INT,
	count        INT,
	end_ms       INT, -- set when any campaign ends
	days         INT, -- recurring: a bitmask of weekdays, Monday = 1
	win_from_min INT,
	win_to_min   INT,
	tz           TEXT,
	until_ms     INT,
	keys_saved   INT  NOT NULL DEFAULT 0,
	rounds_done  INT  NOT NULL DEFAULT 0,
	bytes_down   INT  NOT NULL DEFAULT 0,
	bytes_up     INT  NOT NULL DEFAULT 0
);
CREATE INDEX storage_campaigns_end ON storage_campaigns(end_ms);
-- A campaign's rounds, skipped rounds and gaps (seq NULL: fyisp wasn't
-- running). skips is JSON {target: why} for targets the round left out.
CREATE TABLE storage_rounds(
	id            INTEGER PRIMARY KEY,
	campaign      INT  NOT NULL,
	seq           INT,
	due_ms        INT  NOT NULL,
	start_ms      INT,
	end_ms        INT,
	queue_wait_ms INT,
	outcome       TEXT NOT NULL, -- 'ok' | 'slow' | 'failed' | 'config' | 'cancelled' | 'skipped' | 'gap'
	skip_reason   TEXT,          -- 'busy' | 'stale' | 'paused' | 'no_keys' | 'other_campaign'
	skips         TEXT,
	gw_loss       REAL,
	gw_rtt_ms     REAL,
	edge_loss     REAL,
	edge_rtt_ms   REAL,
	bytes_down    INT  NOT NULL DEFAULT 0,
	bytes_up      INT  NOT NULL DEFAULT 0,
	wire_bytes    INT  NOT NULL DEFAULT 0,
	UNIQUE(campaign, seq)
);
CREATE INDEX storage_rounds_due ON storage_rounds(due_ms);
-- One request of a round (model.StorageAttempt).
CREATE TABLE storage_attempts(
	id                INTEGER PRIMARY KEY,
	round             INT  NOT NULL,
	target            TEXT NOT NULL,
	phase             TEXT NOT NULL,
	op                TEXT NOT NULL,
	n                 INT  NOT NULL,
	at_ms             INT  NOT NULL,
	class             TEXT NOT NULL,
	http_status       INT,
	s3_code           TEXT,
	request_id        TEXT,
	host_id           TEXT,
	request_id_header TEXT,
	dns_ms            REAL,
	connect_ms        REAL,
	tls_ms            REAL,
	ttfb_ms           REAL,
	total_ms          REAL,
	bytes             INT,
	goodput_bps       REAL,
	stall_ms          REAL,
	edge_prefix       TEXT, -- the provider address's /24
	tls_version       TEXT,
	resumed           INT,
	tcp_rtt_ms        REAL,
	retrans           INT,
	mss               INT,
	skew_s            INT,
	trace             INT -- storage_traces.id
);
CREATE INDEX storage_attempts_round ON storage_attempts(round);
CREATE INDEX storage_attempts_at ON storage_attempts(at_ms);
CREATE INDEX storage_attempts_fail ON storage_attempts(at_ms) WHERE class <> 'ok';
-- Objects fyisp put and could not delete yet: the key under
-- fyisp-test/<install>/ only (no host, no bucket).
CREATE TABLE storage_leftovers(
	target TEXT NOT NULL,
	object TEXT NOT NULL,
	at_ms  INT  NOT NULL,
	PRIMARY KEY(target, object)
) WITHOUT ROWID;
-- Connection checks per minute and target; classes is JSON {class: n}.
CREATE TABLE storage_connects(
	campaign    INT  NOT NULL,
	target      TEXT NOT NULL,
	minute_ms   INT  NOT NULL,
	n           INT  NOT NULL,
	failed      INT  NOT NULL,
	classes     TEXT,
	p50_ms      REAL,
	during_test INT  NOT NULL DEFAULT 0,
	PRIMARY KEY(campaign, target, minute_ms)
) WITHOUT ROWID;
CREATE INDEX storage_connects_at ON storage_connects(minute_ms);
-- Traceroutes to a target's edge: hops is a JSON array as in routes.
CREATE TABLE storage_traces(
	id             INTEGER PRIMARY KEY,
	campaign       INT  NOT NULL,
	target         TEXT NOT NULL,
	at_ms          INT  NOT NULL,
	trigger        TEXT NOT NULL, -- 'start' | 'failure'
	edge_prefix    TEXT,
	hops           TEXT NOT NULL,
	handoff_asn    INT,
	route_changed  INT  NOT NULL DEFAULT 0
);
CREATE INDEX storage_traces_at ON storage_traces(at_ms);
CREATE INDEX storage_traces_campaign ON storage_traces(campaign);
`

// SchemaVersion is the schema version this build writes.
func SchemaVersion() int { return schemaVersion }

// schemaVersion is SchemaVersion (a variable for the migration tests).
var schemaVersion = 9

// oldestUpgradable is the oldest schema upgraded in place: fyisp v0.11.0's.
// A file with an older schema is replaced with a new empty one.
const oldestUpgradable = 8

// A migration upgrades schema from to from+1, with additive DDL only.
type migration struct {
	from int
	up   func(ctx context.Context, tx *sql.Tx) error
}

// migrations upgrade every schema from oldestUpgradable to SchemaVersion,
// one step per version, oldest first.
var migrations = []migration{
	{from: 8, up: execDDL(storageDDL)}, // storage tests
}

// execDDL is a step that runs additive DDL.
func execDDL(ddl string) func(ctx context.Context, tx *sql.Tx) error {
	return func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, ddl)
		return err
	}
}

// migrationsFrom returns the steps from schema ver to SchemaVersion, or an
// error naming the first missing one.
func migrationsFrom(ver int) ([]migration, error) {
	var out []migration
	for v := ver; v < SchemaVersion(); v++ {
		i := slices.IndexFunc(migrations, func(m migration) bool { return m.from == v })
		if i < 0 {
			return nil, fmt.Errorf("store: this build has no upgrade from schema v%d to v%d; the database is left as it is", v, v+1)
		}
		out = append(out, migrations[i])
	}
	return out, nil
}

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

// openConns opens the database at path, creating, upgrading or replacing it
// first (see prepare).
//
// Sequence:
//  1. prepare: a complete file of the current schema at path, 0600 (SQLite
//     gives -wal/-shm the same mode as the main file);
//  2. writer pool, MaxOpenConns=1, DSN: busy_timeout, synchronous=NORMAL,
//     BEGIN IMMEDIATE;
//  3. PRAGMA journal_mode=WAL (persistent);
//  4. reader pool, DSN: busy_timeout, query_only.
//
// The caller holds the instance lock, so nothing else uses the file.
func openConns(ctx context.Context, path, version string, now time.Time, log *slog.Logger) (_ *conns, err error) {
	if err := prepare(ctx, path, version, now, log); err != nil {
		return nil, err
	}

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

	var mode string
	if err := w.QueryRowContext(ctx, `PRAGMA journal_mode=WAL`).Scan(&mode); err != nil {
		return nil, err
	}
	if !strings.EqualFold(mode, "wal") {
		return nil, fmt.Errorf("store: journal_mode is %q, want wal", mode)
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

// errSimulatedCrash is returned at crashAt (tests only): the replacement or
// upgrade stops there as if the process had been killed, leaving everything
// as is.
var errSimulatedCrash = errors.New("store: simulated crash")

// crashAt names the step of prepare at which tests stop it ("" never).
var crashAt string

func crashPoint(step string) error {
	if crashAt != "" && crashAt == step {
		return errSimulatedCrash
	}
	return nil
}

// prepare leaves a complete database of the current schema at path (see
// classify). No database yet (no file, 0 bytes, or no tables): a new one. A
// database with an older schema from oldestUpgradable on: upgraded in place
// (migrate) with all of its data, and one info line says so; if this build
// lacks a step, prepare fails and leaves the file as it is. A database with
// a schema older than oldestUpgradable: replaced with a new empty one,
// together with its -wal, -shm and -journal files; its history is not
// carried over, and one info line says so. Anything classify refuses is
// left exactly as it is.
//
// A new file is built as <path>.new, synced, and renamed over path, so a
// kill at any point leaves either the old file (replaced again on the next
// start) or a complete new database, never a half-made one; a stale
// <path>.new is removed first.
func prepare(ctx context.Context, path, version string, now time.Time, log *slog.Logger) error {
	state, ver, err := classify(ctx, path)
	if err != nil {
		return err
	}
	switch state {
	case dbCurrent:
		return nil
	case dbUpgrade:
		if err := migrate(ctx, path, ver); err != nil {
			return err
		}
		log.Info(fmt.Sprintf("store: database upgraded from schema v%d to v%d", ver, SchemaVersion()))
		return nil
	}
	tmp := path + ".new"
	for _, p := range []string{tmp, tmp + "-journal"} {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("store: remove a half-made database: %w", err)
		}
	}
	if err := replace(ctx, path, tmp, version, now); err != nil {
		return err
	}
	if state == dbOld {
		log.Info(fmt.Sprintf("store: database from an older fyisp (schema v%d) replaced with a new empty one; its history isn't carried over", ver))
	}
	return nil
}

// What classify found at a database path.
type dbState int

const (
	dbNone    dbState = iota // no database yet: no file, 0 bytes, or no tables
	dbCurrent                // a complete database of the current schema
	dbUpgrade                // an older schema from oldestUpgradable on: upgraded in place
	dbOld                    // a schema older than oldestUpgradable: replaced
)

// classify tells what is at path, for the daemon (prepare) and the
// read-only commands (OpenReadOnly) alike, without writing to it: the
// connection is read-only, so closing it never checkpoints a WAL into the
// file or removes its -wal and -shm. It refuses a symbolic link, a newer
// schema (ErrNewerSchema: a downgrade never wipes data), and a file that is
// not a fyisp database, or one of the current schema without all of its
// tables (ErrNotDatabase).
func classify(ctx context.Context, path string) (dbState, int, error) {
	fi, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return dbNone, 0, nil
	case err != nil:
		return 0, 0, err
	case fi.Mode()&fs.ModeSymlink != 0:
		return 0, 0, fmt.Errorf("store: %s is a symbolic link; fyisp needs the database file itself there (point --data-dir at the directory instead)", path)
	case !fi.Mode().IsRegular():
		return 0, 0, fmt.Errorf("%w: %s is not a regular file", ErrNotDatabase, path)
	case fi.Size() == 0:
		return dbNone, 0, nil
	}
	ver, objs, err := inspect(ctx, path)
	if err != nil {
		return 0, 0, err
	}
	switch {
	case ver > SchemaVersion():
		return 0, 0, fmt.Errorf("%w (%d > %d)", ErrNewerSchema, ver, SchemaVersion())
	case ver == 0 && len(objs) == 0:
		return dbNone, 0, nil // SQLite's header and nothing else: no history to lose
	case ver == SchemaVersion():
		for _, t := range tables {
			if !objs[t] {
				return 0, 0, fmt.Errorf("%w: %s has schema v%d but no %s table", ErrNotDatabase, path, ver, t)
			}
		}
		return dbCurrent, ver, nil
	case ver == 0 || !objs["meta"] || !objs["series"]:
		return 0, 0, fmt.Errorf("%w: %s has tables but not fyisp's schema", ErrNotDatabase, path)
	case ver >= oldestUpgradable:
		return dbUpgrade, ver, nil
	}
	return dbOld, ver, nil
}

// tables are the schema's tables, which a current file must all have.
var tables = []string{"series", "samples", "summary_1h", "meta", "incidents", "hop_info", "route_changes", "routes", "speedtests",
	"storage_campaigns", "storage_rounds", "storage_attempts", "storage_leftovers", "storage_connects", "storage_traces"}

// inspect reads the schema version and the table names of the database at
// path over a read-only connection (mode=ro): it writes nothing, and its
// close checkpoints nothing. Only a file SQLite reads as no database at all
// is ErrNotDatabase; an error opening or reading it is returned as it is.
func inspect(ctx context.Context, path string) (ver int, objs map[string]bool, err error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return 0, nil, err
	}
	db, err := sql.Open("sqlite", readOnlyURI(filepath.ToSlash(abs)))
	if err != nil {
		return 0, nil, err
	}
	defer db.Close()
	notDB := func(err error) error {
		var se *sqlite.Error
		if errors.As(err, &se) && se.Code()&0xff == sqlite3.SQLITE_NOTADB {
			return fmt.Errorf("%w: %s: %v", ErrNotDatabase, path, err)
		}
		return fmt.Errorf("store: read %s: %w", path, err)
	}
	rows, err := db.QueryContext(ctx, `SELECT name FROM sqlite_schema WHERE type = 'table'`)
	if err != nil {
		return 0, nil, notDB(err)
	}
	defer rows.Close()
	objs = map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return 0, nil, err
		}
		objs[n] = true
	}
	if err := rows.Err(); err != nil {
		return 0, nil, notDB(err)
	}
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&ver); err != nil {
		return 0, nil, notDB(err)
	}
	return ver, objs, nil
}

// readOnlyURI is the absolute, slash-separated path as an SQLite URI
// filename opened read-only: file:///home/me/fyisp.db?mode=ro, or
// file:///C:/Users/me/fyisp.db?mode=ro for a Windows path (a leading slash
// before the drive, which would otherwise read as the URI's authority).
// The path is percent-escaped: a space, '?', '#' or '%' in it stays part of
// the name.
func readOnlyURI(slashPath string) string {
	if !strings.HasPrefix(slashPath, "/") {
		slashPath = "/" + slashPath
	}
	return "file://" + (&url.URL{Path: slashPath}).EscapedPath() + "?mode=ro"
}

// replace builds a new database at tmp, removes the old one's -wal, -shm
// and -journal files, and renames tmp over path (the commit point).
func replace(ctx context.Context, path, tmp, version string, now time.Time) error {
	if err := build(ctx, tmp, version, now); err != nil {
		return fmt.Errorf("store: create the database: %w", err)
	}
	for _, p := range []string{path + "-wal", path + "-shm", path + "-journal"} {
		if err := crashPoint("remove"); err != nil {
			return err
		}
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("store: replace the database: %w", err)
		}
	}
	if err := crashPoint("rename"); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("store: create the database: %w", err)
	}
	syncDir(filepath.Dir(path))
	return nil
}

// build writes a complete database of the current schema at path (0600): page size and
// incremental auto-vacuum, then the schema, the creation stamp and the
// version in one transaction, synced to disk (rollback journal, synchronous
// FULL) before build returns.
func build(ctx context.Context, path, version string, now time.Time) error {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	f.Close()
	db, err := sql.Open("sqlite", path+"?_pragma=synchronous(FULL)"+tempDirParam(path))
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for _, p := range []string{fmt.Sprintf("PRAGMA page_size=%d", PageSize), "PRAGMA auto_vacuum=INCREMENTAL"} {
		if _, err := db.ExecContext(ctx, p); err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
	}
	if err := crashPoint("build"); err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("schema: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO meta(key, value) VALUES('created_by_version', ?), ('created_at', ?)`,
		version, now.UTC().Format(time.RFC3339)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version=%d", SchemaVersion())); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return db.Close()
}

// migrate upgrades the database at path from schema ver to SchemaVersion in
// place: every step and the new user_version in one transaction, synced to
// disk (synchronous FULL) before migrate returns. A kill at any point leaves
// the file at schema ver (upgraded again on the next start) or fully
// upgraded. A missing step is an error before anything is written.
func migrate(ctx context.Context, path string, ver int) error {
	steps, err := migrationsFrom(ver)
	if err != nil {
		return err
	}
	db, err := sql.Open("sqlite", path+"?_pragma=synchronous(FULL)"+tempDirParam(path))
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: upgrade the database: %w", err)
	}
	defer tx.Rollback()
	for _, m := range steps {
		if err := m.up(ctx, tx); err != nil {
			return fmt.Errorf("store: upgrade schema v%d to v%d: %w", m.from, m.from+1, err)
		}
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version=%d", SchemaVersion())); err != nil {
		return fmt.Errorf("store: upgrade the database: %w", err)
	}
	if err := crashPoint("migrate"); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: upgrade the database: %w", err)
	}
	return db.Close()
}

// syncDir makes a rename in dir durable where the OS allows it.
func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
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

// msTime is unix milliseconds as a UTC time.
func msTime(ms int64) time.Time { return time.UnixMilli(ms).UTC() }

// inList returns "?,?,?" for n placeholders.
func inList(n int) string { return strings.TrimSuffix(strings.Repeat("?,", n), ",") }

// tempDirParam points SQLite's temporary files (large sorts, temporary
// indexes) at a private directory next to the database. Minimal systems
// such as the scratch Docker image have no /tmp, /var/tmp or TMPDIR, and
// SQLite then fails with SQLITE_IOERR_GETTEMPPATH ("disk I/O error
// (6410)"). The environment cannot be used instead: the transpiled libc
// snapshots it at process start. Windows uses GetTempPath.
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
