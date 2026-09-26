// Package store persists probe results in SQLite (modernc.org/sqlite).
//
// This file holds the open sequence and schema proven by spike S3; see
// sqlite_spike_test.go for the gates.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"

	_ "modernc.org/sqlite"
)

// SchemaVersion is stored in PRAGMA user_version.
const SchemaVersion = 1

// PageSize is the database page size, fixed when the file is created.
const PageSize = 16384

// Per-connection settings go in the DSN so they survive the pool reopening a
// connection. Persistent settings (page_size, auto_vacuum, journal_mode) are
// executed explicitly in openDB: modernc runs _pragma DSN values in
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
// continuously (checkpoint starvation, see G2): the WAL then grows until a
// gap between reads.
const (
	writerParams = "?_busy_timeout=5000&_synchronous=NORMAL&_txlock=immediate&_pragma=wal_autocheckpoint(256)&_pragma=journal_size_limit(16777216)"
	readerParams = "?_busy_timeout=5000&_query_only=1"
)

const schemaSQL = `
CREATE TABLE series(
	id          INTEGER PRIMARY KEY,
	target      TEXT    NOT NULL,
	kind        INTEGER NOT NULL,
	interval_ms INTEGER NOT NULL,
	UNIQUE(target, kind)
);
-- A rowid table on purpose: in a WITHOUT ROWID table (an index b-tree) a
-- row larger than ~4 KB on a 16 KB page spills into an overflow page that is
-- mostly empty, and hourly blobs of 1 s series are ~5 KB. Measured: file
-- 3.75 B/sample (39% page use) WITHOUT ROWID vs 1.70 B/sample (90%) here.
CREATE TABLE samples(
	hour   INTEGER NOT NULL, -- unix seconds / 3600
	series INTEGER NOT NULL,
	data   BLOB    NOT NULL, -- blob v1
	PRIMARY KEY(hour, series)
);
CREATE TABLE summary_1h(
	hour    INTEGER NOT NULL,
	series  INTEGER NOT NULL,
	n       INTEGER NOT NULL,
	lost    INTEGER NOT NULL,
	lost_by BLOB,
	min     INTEGER, -- 10 µs units; NULL when no values
	mean    INTEGER,
	max     INTEGER,
	p95     INTEGER,
	PRIMARY KEY(hour, series)
) WITHOUT ROWID;
CREATE TABLE meta(
	key   TEXT PRIMARY KEY,
	value ANY
) WITHOUT ROWID;
`

// DB is the writer/reader handle pair.
type DB struct {
	Path string
	W    *sql.DB // exactly one connection; all writes go here
	R    *sql.DB // query_only pool for UI/API reads
}

// ErrNewerSchema means the file was written by a newer fyisp.
var ErrNewerSchema = errors.New("store: database schema is newer than this build")

// openDB opens (creating if needed) the database at path.
//
// Sequence:
//  1. create the file 0600 if missing: SQLite gives -wal/-shm the same mode
//     as the main file, so they come out 0600 too;
//  2. writer pool, MaxOpenConns=1, DSN: busy_timeout, synchronous=NORMAL,
//     BEGIN IMMEDIATE;
//  3. on an empty file: PRAGMA page_size=16384; PRAGMA auto_vacuum=INCREMENTAL;
//  4. PRAGMA journal_mode=WAL (persistent; writes page 1, fixing page_size);
//  5. schema + user_version in one transaction, refuse newer versions;
//  6. reader pool, DSN: busy_timeout, query_only.
func openDB(ctx context.Context, path string) (_ *DB, err error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	f.Close()

	w, err := sql.Open("sqlite", path+writerParams)
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
		return nil, fmt.Errorf("store: %s is not a database: %w", path, err)
	}
	if err := w.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&ver); err != nil {
		return nil, err
	}
	if ver > SchemaVersion {
		return nil, fmt.Errorf("%w (%d > %d)", ErrNewerSchema, ver, SchemaVersion)
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
	if ver == 0 {
		if err := migrate0(ctx, w); err != nil {
			return nil, err
		}
	}

	r, err := sql.Open("sqlite", path+readerParams)
	if err != nil {
		return nil, err
	}
	r.SetMaxOpenConns(max(4, runtime.GOMAXPROCS(0)))
	r.SetMaxIdleConns(max(4, runtime.GOMAXPROCS(0)))
	if err := r.PingContext(ctx); err != nil {
		r.Close()
		return nil, err
	}
	return &DB{Path: path, W: w, R: r}, nil
}

func migrate0(ctx context.Context, w *sql.DB) error {
	tx, err := w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, schemaSQL); err != nil {
		return fmt.Errorf("store: create schema: %w", err)
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version=%d", SchemaVersion)); err != nil {
		return err
	}
	return tx.Commit()
}

// Close closes the reader pool, then the writer (whose close checkpoints and
// removes the WAL when it is the last connection).
func (d *DB) Close() error {
	return errors.Join(d.R.Close(), d.W.Close())
}

// incrementalVacuum returns all free pages to the OS. The pragma yields no
// rows, but it is run through Query and drained so every step executes
// regardless of how the driver treats Exec.
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

// prune deletes hours before `before` from samples and summary_1h, then
// returns the freed pages with incremental_vacuum.
func prune(ctx context.Context, w *sql.DB, before int64) error {
	tx, err := w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		`DELETE FROM samples WHERE hour < ?`,
		`DELETE FROM summary_1h WHERE hour < ?`,
	} {
		if _, err := tx.ExecContext(ctx, q, before); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return incrementalVacuum(ctx, w)
}

// panelSQL builds the one-query-per-panel statement for n series.
func panelSQL(n int) string {
	return `SELECT hour, series, data FROM samples WHERE hour BETWEEN ? AND ? AND series IN (` +
		strings.TrimSuffix(strings.Repeat("?,", n), ",") + `) ORDER BY hour, series`
}
