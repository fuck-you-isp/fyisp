package store

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// oldMigrations is the migration chain of fyisp v0.1 to v0.9 (schema v1 to
// v6), as released, for TestSchemaMatchesChain only: v0.10.0 replaced it
// with one schema.
var oldMigrations = []string{
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
	// v5 (fyisp v0.7): speed test results (see speedtests.go). Settings
	// changed in the dashboard live in meta (key 'setting:<name>').
	`
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
	down_short      INT  NOT NULL DEFAULT 0,
	up_short        INT  NOT NULL DEFAULT 0,
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
	idle_gw_ms      REAL,
	loaded_gw_ms    REAL,
	latency_json    TEXT,
	conn            TEXT NOT NULL DEFAULT '',
	handoff_asn     INT,
	handoff_name    TEXT,
	isp_asn         INT,
	handoff_ms      INT
);
CREATE INDEX speedtests_start ON speedtests(start_ms);
`,
	// v6 (fyisp v0.8): the share link is gone. Reports are no longer
	// published, and the share link's speed-test opt-in and time ledger
	// settings go. Speed tests it started stay, with their triggers
	// (public, public_sweep).
	`
ALTER TABLE reports DROP COLUMN public;
DELETE FROM meta WHERE key IN ('setting:speedtest_public', 'setting:speedtest_public_bytes');
`,
}

// droppedFromChain is what schema v7 leaves out of the old chain's v6:
// notes and reports (removed in v0.10.0), with their indexes.
var droppedFromChain = []string{"table annotations", "index annotations_at", "table reports", "index reports_created"}

// schemaDump describes every table (columns with type, NOT NULL, default
// and key position; WITHOUT ROWID) and index (table, uniqueness, columns)
// of db, one line each, sorted, keyed by "table name" / "index name".
func schemaDump(t *testing.T, db *sql.DB) map[string]string {
	t.Helper()
	rows, err := db.Query(`SELECT type, name, tbl_name, coalesce(sql, '') FROM sqlite_schema WHERE name NOT LIKE 'sqlite_%'`)
	if err != nil {
		t.Fatal(err)
	}
	type obj struct{ typ, name, tbl, sql string }
	var objs []obj
	for rows.Next() {
		var o obj
		if err := rows.Scan(&o.typ, &o.name, &o.tbl, &o.sql); err != nil {
			t.Fatal(err)
		}
		objs = append(objs, o)
	}
	rows.Close()
	out := map[string]string{}
	for _, o := range objs {
		var cols []string
		switch o.typ {
		case "table":
			r, err := db.Query(`SELECT name, type, "notnull", coalesce(dflt_value, 'NULL'), pk FROM pragma_table_info(?)`, o.name)
			if err != nil {
				t.Fatal(err)
			}
			for r.Next() {
				var n, ty, d string
				var nn, pk int
				r.Scan(&n, &ty, &nn, &d, &pk)
				cols = append(cols, fmt.Sprintf("%s %s notnull=%d default=%s pk=%d", n, ty, nn, d, pk))
			}
			r.Close()
			cols = append(cols, fmt.Sprintf("without_rowid=%v", strings.Contains(strings.ToUpper(o.sql), "WITHOUT ROWID")))
		case "index":
			var unique int
			db.QueryRow(`SELECT "unique" FROM pragma_index_list(?) WHERE name = ?`, o.tbl, o.name).Scan(&unique)
			r, err := db.Query(`SELECT coalesce(name, '') FROM pragma_index_info(?) ORDER BY seqno`, o.name)
			if err != nil {
				t.Fatal(err)
			}
			for r.Next() {
				var n string
				r.Scan(&n)
				cols = append(cols, n)
			}
			r.Close()
			cols = append(cols, fmt.Sprintf("on=%s unique=%d", o.tbl, unique))
		}
		out[o.typ+" "+o.name] = strings.Join(cols, "; ")
	}
	return out
}

// TestSchemaMatchesChain: schema v7, created in one go, is exactly what
// the old chain built by v6 (the last released schema), minus
// droppedFromChain: the same tables, columns (type, NOT NULL, default,
// key), WITHOUT ROWID and indexes.
func TestSchemaMatchesChain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chain.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for i, m := range oldMigrations {
		if _, err := db.Exec(m); err != nil {
			t.Fatalf("v%d: %v", i+1, err)
		}
	}
	chain := schemaDump(t, db)
	for _, d := range droppedFromChain {
		if _, ok := chain[d]; !ok {
			t.Errorf("the chain has no %s to drop", d)
		}
		delete(chain, d)
	}
	s := openT(t, t.TempDir(), Options{})
	defer s.Close()
	fresh := schemaDump(t, s.db.r)
	var keys []string
	for k := range chain {
		keys = append(keys, k)
	}
	for k := range fresh {
		if _, ok := chain[k]; !ok {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	for _, k := range keys {
		if chain[k] != fresh[k] {
			t.Errorf("%s:\n  chain: %s\n  v7:    %s", k, chain[k], fresh[k])
		}
	}
}
