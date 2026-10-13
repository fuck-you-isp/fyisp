package store

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// logBuf collects a test's log output.
type logBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func testLog() (*slog.Logger, *logBuf) {
	var l logBuf
	return slog.New(slog.NewTextHandler(&l, nil)), &l
}

// schemaObjects lists the tables and indexes of db (SQLite's own
// autoindexes left out).
func schemaObjects(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`SELECT type || ' ' || name FROM sqlite_schema WHERE name NOT LIKE 'sqlite_%' ORDER BY type, name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

func countRows(t *testing.T, path, table string) int {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// wantSchema is every table and index of schema v7: only what the code
// reads and writes.
var wantSchema = []string{
	"index incidents_start", "index route_changes_at", "index speedtests_start",
	"table hop_info", "table incidents", "table meta", "table route_changes", "table routes",
	"table samples", "table series", "table speedtests", "table summary_1h",
}

// TestFreshSchema: a new file gets schema v7 in one go: exactly the
// objects in wantSchema, no free pages, the creation stamp, no backup.
func TestFreshSchema(t *testing.T) {
	dir := t.TempDir()
	log, logs := testLog()
	s := openT(t, dir, Options{Version: "v0.10.0-test", Now: func() time.Time { return t0 }, Log: log})
	if got := schemaObjects(t, s.db.r); !slices.Equal(got, wantSchema) {
		t.Errorf("schema objects\n%q\nwant\n%q", got, wantSchema)
	}
	if v := pragmaInt(t, s.db.w, "user_version"); v != 7 || SchemaVersion() != 7 {
		t.Errorf("user_version %d, SchemaVersion %d", v, SchemaVersion())
	}
	if free := pragmaInt(t, s.db.w, "freelist_count"); free != 0 {
		t.Errorf("a new database has %d free pages", free)
	}
	var by, at string
	s.db.r.QueryRow(`SELECT value FROM meta WHERE key = 'created_by_version'`).Scan(&by)
	s.db.r.QueryRow(`SELECT value FROM meta WHERE key = 'created_at'`).Scan(&at)
	if by != "v0.10.0-test" || at != t0.Format(time.RFC3339) {
		t.Errorf("created_by_version %q, created_at %q", by, at)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logs.String(), "replaced") {
		t.Errorf("a new database logged:\n%s", logs)
	}
	if m, _ := filepath.Glob(filepath.Join(dir, DBName+".bak*")); len(m) != 0 {
		t.Errorf("backed up: %v", m)
	}
	ro, err := OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	ro.Close()
}

// writeOld writes a database as an older fyisp left it (schema v<ver>):
// some of its tables with rows, a WAL, shared-memory and journal file
// beside it, and two backups of earlier upgrades.
func writeOld(t *testing.T, dir string, ver int) {
	t.Helper()
	path := filepath.Join(dir, DBName)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE series(id INTEGER PRIMARY KEY, target TEXT NOT NULL, kind INTEGER NOT NULL, interval_ms INTEGER NOT NULL, UNIQUE(target, kind))`,
		`CREATE TABLE samples(hour INTEGER NOT NULL, series INTEGER NOT NULL, data BLOB NOT NULL, PRIMARY KEY(hour, series))`,
		`CREATE TABLE meta(key TEXT PRIMARY KEY, value ANY) WITHOUT ROWID`,
		`CREATE TABLE annotations(id INTEGER PRIMARY KEY, at_ms INT NOT NULL, text TEXT NOT NULL)`,
		`CREATE TABLE reports(id TEXT PRIMARY KEY, html BLOB NOT NULL)`,
		`INSERT INTO series(target, kind, interval_ms) VALUES('github', 1, 15000)`,
		`INSERT INTO samples VALUES(493000, 1, x'01')`,
		`INSERT INTO meta VALUES('created_by_version', 'v0.9.0'), ('setting:owner_tz', 'Europe/Berlin'), ('setting:speedtest_mode', 'nearby')`,
		`INSERT INTO annotations(at_ms, text) VALUES(1, 'call dropped')`,
		`INSERT INTO reports VALUES('r1', x'00')`,
		fmt.Sprintf(`PRAGMA user_version=%d`, ver),
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	for name, size := range map[string]int{DBName + "-wal": 30, DBName + "-shm": 30, DBName + "-journal": 30, DBName + ".bak-v4": 1000, DBName + ".bak-v5": 2000} {
		if err := os.WriteFile(filepath.Join(dir, name), make([]byte, size), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// TestWriteOldFixture writes writeOld's v6 database, sidecars and backups
// into $FYISP_WRITE_OLD, for CI's check that the runtime image replaces an
// old database (Dockerfile target oldfixture).
func TestWriteOldFixture(t *testing.T) {
	dir := os.Getenv("FYISP_WRITE_OLD")
	if dir == "" {
		t.Skip("set FYISP_WRITE_OLD to write an old database")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeOld(t, dir, 6)
}

// strays are files beside the database that fyisp never touches: not
// exactly fyisp.db.bak-v<N>.
var strays = []string{DBName + ".bak-v6.old", DBName + ".bak-vx", DBName + ".bak-v", "fyisp.db.backup", "notes.txt"}

func writeStrays(t *testing.T, dir string) {
	t.Helper()
	for _, n := range strays {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("mine"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func checkStrays(t *testing.T, dir, what string) {
	t.Helper()
	for _, n := range strays {
		if b, err := os.ReadFile(filepath.Join(dir, n)); err != nil || string(b) != "mine" {
			t.Errorf("%s: %s touched: %v", what, n, err)
		}
	}
}

func fileSum(t *testing.T, path string) [32]byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(b)
}

// TestOldSchemaReplaced: v0.10.0 starts over. A file from an older fyisp
// (schema v1 to v6) is refused by the read-only commands (fyisp export),
// which leave it as it is, and is not touched while another instance holds
// the lock. The daemon's Open replaces it, its -wal, -shm and -journal
// files and the fyisp.db.bak-v<N> backups with a new empty v7 database,
// logging one line with the old version and the backups removed. Nothing
// of the old file is kept, settings included, and stray files stay.
func TestOldSchemaReplaced(t *testing.T) {
	for _, ver := range []int{1, 3, 6} {
		dir := t.TempDir()
		writeOld(t, dir, ver)
		writeStrays(t, dir)
		path := filepath.Join(dir, DBName)
		before := fileSum(t, path)
		if _, err := OpenReadOnly(dir); err == nil || !strings.Contains(err.Error(), "older fyisp") {
			t.Errorf("v%d: OpenReadOnly: %v", ver, err)
		}
		lk, err := acquireLock(filepath.Join(dir, LockName))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Open(dir, Options{}); !errors.Is(err, ErrLocked) {
			t.Errorf("v%d: Open while locked: %v", ver, err)
		}
		lk.release()
		if fileSum(t, path) != before {
			t.Errorf("v%d: touched by OpenReadOnly or while locked", ver)
		}
		for _, n := range []string{DBName + ".bak-v4", DBName + "-journal"} {
			if _, err := os.Stat(filepath.Join(dir, n)); err != nil {
				t.Errorf("v%d: %s removed early: %v", ver, n, err)
			}
		}

		log, logs := testLog()
		s := openT(t, dir, Options{Version: "v0.10.0-test", Log: log})
		want := fmt.Sprintf("level=INFO msg=\"store: database from an older fyisp (schema v%d) replaced with a new empty one; its history isn't carried over\" backups_removed=2 backup_bytes_freed=3000\n", ver)
		if l := logs.String(); !strings.HasSuffix(l, " "+want) || strings.Count(l, "\n") != 1 {
			t.Errorf("v%d: log\n%s\nwant\n%s", ver, logs, want)
		}
		if got := schemaObjects(t, s.db.r); !slices.Equal(got, wantSchema) {
			t.Errorf("v%d: schema objects %q", ver, got)
		}
		if v := pragmaInt(t, s.db.w, "user_version"); v != 7 {
			t.Errorf("v%d: user_version %d", ver, v)
		}
		var n int
		s.db.r.QueryRow(`SELECT (SELECT count(*) FROM series) + (SELECT count(*) FROM samples) + (SELECT count(*) FROM meta WHERE key LIKE 'setting:%')`).Scan(&n)
		if n != 0 {
			t.Errorf("v%d: %d old rows kept", ver, n)
		}
		if st, err := s.Stats(ctx); err != nil || !st.Oldest.IsZero() {
			t.Errorf("v%d: stats %+v %v", ver, st, err)
		}
		for _, n := range []string{DBName + ".bak-v4", DBName + ".bak-v5", DBName + "-journal", DBName + ".new"} {
			if _, err := os.Stat(filepath.Join(dir, n)); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("v%d: %s left: %v", ver, n, err)
			}
		}
		checkStrays(t, dir, fmt.Sprintf("v%d", ver))
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		// The next start keeps it.
		log2, logs2 := testLog()
		s = openT(t, dir, Options{Log: log2})
		s.Close()
		if logs2.String() != "" {
			t.Errorf("v%d: second start logged:\n%s", ver, logs2)
		}
	}
}

// TestFreshStartKeepsStrays: without a database, Open creates one and
// leaves everything else in the directory alone, old backups included.
func TestFreshStartKeepsStrays(t *testing.T) {
	dir := t.TempDir()
	writeStrays(t, dir)
	bak := filepath.Join(dir, DBName+".bak-v5")
	if err := os.WriteFile(bak, []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	log, logs := testLog()
	s := openT(t, dir, Options{Log: log})
	s.Close()
	checkStrays(t, dir, "fresh")
	if b, err := os.ReadFile(bak); err != nil || string(b) != "mine" {
		t.Errorf("backup touched on a fresh start: %v", err)
	}
	if logs.String() != "" {
		t.Errorf("fresh start logged:\n%s", logs)
	}
}

// writePending writes a database at dir/fyisp.db whose last writes are
// still in its WAL, as a killed process leaves it: sqls run in WAL mode
// with automatic checkpoints off, and the files are copied while the
// connection is open.
func writePending(t *testing.T, dir string, sqls ...string) {
	t.Helper()
	src := filepath.Join(t.TempDir(), DBName)
	db, err := sql.Open("sqlite", src)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for _, q := range append([]string{`PRAGMA journal_mode=WAL`, `PRAGMA wal_autocheckpoint=0`}, sqls...) {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	for _, ext := range []string{"", "-wal", "-shm"} {
		b, err := os.ReadFile(src + ext)
		if err != nil {
			t.Fatal(err)
		}
		if ext == "-wal" && len(b) == 0 {
			t.Fatal("no pending WAL")
		}
		if err := os.WriteFile(filepath.Join(dir, DBName+ext), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// sums hashes fyisp.db and its -wal and -journal files, and notes that its
// -shm file exists: a reader, even a read-only one, updates the read marks
// in that shared-memory index (SQLite's own bookkeeping, rebuilt from the
// WAL whenever needed), so only its presence is compared.
func sums(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, ext := range []string{"", "-wal", "-shm", "-journal"} {
		b, err := os.ReadFile(filepath.Join(dir, DBName+ext))
		switch {
		case err != nil:
		case ext == "-shm":
			out[ext] = "present"
		default:
			out[ext] = fmt.Sprintf("%x", sha256.Sum256(b))
		}
	}
	return out
}

// TestRefusedUntouched: what the daemon refuses, a newer schema, a file
// that is not a fyisp database, or a v7 file without fyisp's tables, is
// refused by `fyisp export` (OpenReadOnly) the same way, and both leave
// it exactly as it is: the main file, its pending WAL and shared memory,
// the backups and everything else beside it.
func TestRefusedUntouched(t *testing.T) {
	meta := `CREATE TABLE meta(key TEXT PRIMARY KEY, value ANY) WITHOUT ROWID`
	series := `CREATE TABLE series(id INTEGER PRIMARY KEY)`
	for name, c := range map[string]struct {
		sqls []string
		want error
	}{
		"newer schema":                {[]string{meta, series, `INSERT INTO meta VALUES('k', 1)`, `PRAGMA user_version=8`}, ErrNewerSchema},
		"old version, foreign tables": {[]string{`CREATE TABLE other(x)`, `INSERT INTO other VALUES(1)`, `PRAGMA user_version=3`}, ErrNotDatabase},
		"old version, no series":      {[]string{meta, `INSERT INTO meta VALUES('k', 1)`, `PRAGMA user_version=6`}, ErrNotDatabase},
		"no version, tables":          {[]string{`CREATE TABLE other(x)`, `INSERT INTO other VALUES(1)`}, ErrNotDatabase},
		"v7 without fyisp's tables":   {[]string{meta, series, `INSERT INTO series VALUES(1)`, `PRAGMA user_version=7`}, ErrNotDatabase},
	} {
		dir := t.TempDir()
		writePending(t, dir, c.sqls...)
		writeStrays(t, dir)
		if err := os.WriteFile(filepath.Join(dir, DBName+".bak-v5"), []byte("mine"), 0o600); err != nil {
			t.Fatal(err)
		}
		before := sums(t, dir)
		if _, err := Open(dir, Options{}); !errors.Is(err, c.want) {
			t.Errorf("%s: Open: %v, want %v", name, err, c.want)
		}
		if _, err := OpenReadOnly(dir); !errors.Is(err, c.want) {
			t.Errorf("%s: OpenReadOnly: %v, want %v", name, err, c.want)
		}
		if after := sums(t, dir); !maps.Equal(after, before) {
			t.Errorf("%s: files changed\n%v\n%v", name, before, after)
		}
		checkStrays(t, dir, name)
		if _, err := os.Stat(filepath.Join(dir, DBName+".bak-v5")); err != nil {
			t.Errorf("%s: backup removed: %v", name, err)
		}
	}
	// Not SQLite at all.
	dir := t.TempDir()
	path := filepath.Join(dir, DBName)
	junk := []byte("hello, this is not sqlite at all, not even close......................................................")
	if err := os.WriteFile(path, junk, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir, Options{}); !errors.Is(err, ErrNotDatabase) {
		t.Fatalf("garbage: Open: %v", err)
	}
	if _, err := OpenReadOnly(dir); !errors.Is(err, ErrNotDatabase) {
		t.Fatalf("garbage: OpenReadOnly: %v", err)
	}
	if b, _ := os.ReadFile(path); string(b) != string(junk) {
		t.Error("garbage file changed")
	}
}

// TestOldPendingWALUntouchedReadOnly: `fyisp export` on an older database
// with a pending WAL says fyisp replaces it, and leaves every file as is.
func TestOldPendingWALUntouchedReadOnly(t *testing.T) {
	dir := t.TempDir()
	writePending(t, dir, `CREATE TABLE meta(key TEXT PRIMARY KEY, value ANY) WITHOUT ROWID`, `CREATE TABLE series(id INTEGER PRIMARY KEY)`,
		`INSERT INTO series VALUES(1)`, `PRAGMA user_version=6`)
	before := sums(t, dir)
	if _, err := OpenReadOnly(dir); err == nil || !strings.Contains(err.Error(), "older fyisp") {
		t.Errorf("OpenReadOnly: %v", err)
	}
	if after := sums(t, dir); !maps.Equal(after, before) {
		t.Errorf("files changed\n%v\n%v", before, after)
	}
}

// TestNoDatabaseYetReadOnly: `fyisp export` before fyisp ever ran, or on
// a file without tables, says so and creates nothing.
func TestNoDatabaseYetReadOnly(t *testing.T) {
	dir := t.TempDir()
	if _, err := OpenReadOnly(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("no file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, DBName)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("created a file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, DBName), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenReadOnly(dir); err == nil || !strings.Contains(err.Error(), "no fyisp database yet") {
		t.Errorf("0-byte file: %v", err)
	}
}

// TestSymlinkRefused: a symbolic link at fyisp.db is refused by the daemon
// and by `fyisp export`, and neither the link nor its target is touched.
func TestSymlinkRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symbolic links need privileges on Windows")
	}
	target := t.TempDir()
	writeOld(t, target, 6)
	dir := t.TempDir()
	link := filepath.Join(dir, DBName)
	if err := os.Symlink(filepath.Join(target, DBName), link); err != nil {
		t.Fatal(err)
	}
	before := sums(t, target)
	for _, err := range []error{func() error { _, err := Open(dir, Options{}); return err }(), func() error { _, err := OpenReadOnly(dir); return err }()} {
		if err == nil || !strings.Contains(err.Error(), "symbolic link") {
			t.Errorf("err %v", err)
		}
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&fs.ModeSymlink == 0 {
		t.Errorf("link replaced: %v", err)
	}
	if after := sums(t, target); !maps.Equal(after, before) {
		t.Errorf("target changed")
	}
	if _, err := os.Stat(filepath.Join(target, DBName+".bak-v4")); err != nil {
		t.Errorf("target's backup removed: %v", err)
	}
}

// TestReplaceCrash: a kill at any step of creating or replacing the
// database (simulated with crashAt) never leaves a 0-byte or half-made
// database that a later start accepts: before the rename fyisp.db is the
// old file or missing, after it a complete v7, and the next start ends
// with a complete empty v7 database and no leftover <path>.new. A kill
// while the backups are removed (after the rename) leaves the rest.
func TestReplaceCrash(t *testing.T) {
	defer func() { crashAt = "" }()
	for _, old := range []bool{true, false} {
		for _, step := range []string{"build", "remove", "rename", "backups"} {
			if !old && step == "backups" {
				continue // a new database removes no backups
			}
			what := fmt.Sprintf("old=%v crash at %s", old, step)
			dir := t.TempDir()
			path := filepath.Join(dir, DBName)
			if old {
				writeOld(t, dir, 6)
			}
			crashAt = step
			log, logs := testLog()
			if _, err := Open(dir, Options{Log: log}); !errors.Is(err, errSimulatedCrash) {
				t.Fatalf("%s: Open: %v", what, err)
			}
			if logs.String() != "" {
				t.Errorf("%s: logged before the replacement was done:\n%s", what, logs)
			}
			crashAt = ""
			fi, err := os.Stat(path)
			switch {
			case step == "backups":
				// After the rename: a complete v7 database, which the next
				// start keeps; the backups not yet removed stay.
				if st, v, err := classify(ctx, path); err != nil || st != dbCurrent || v != 7 {
					t.Errorf("%s: fyisp.db is %v v%d (%v), want complete v7", what, st, v, err)
				}
				if m, _ := filepath.Glob(path + ".bak-v*"); len(m) == 0 {
					t.Errorf("%s: no backup left", what)
				}
			case !old && !errors.Is(err, fs.ErrNotExist):
				t.Errorf("%s: a database exists after the crash: %v", what, err)
			case old && (err != nil || fi.Size() == 0):
				t.Errorf("%s: old file gone or empty: %v", what, err)
			case old:
				if v, _, err := inspect(ctx, path); err != nil || v != 6 {
					t.Errorf("%s: fyisp.db is v%d (%v), want the old v6", what, v, err)
				}
			}
			s := openT(t, dir, Options{})
			if got := schemaObjects(t, s.db.r); !slices.Equal(got, wantSchema) {
				t.Errorf("%s: schema objects after restart %q", what, got)
			}
			var n int
			s.db.r.QueryRow(`SELECT count(*) FROM series`).Scan(&n)
			if n != 0 {
				t.Errorf("%s: %d old series", what, n)
			}
			s.Close()
			if _, err := os.Stat(path + ".new"); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("%s: %s.new left: %v", what, DBName, err)
			}
		}
	}
	// A 0-byte fyisp.db (an older fyisp killed right after creating it) is
	// no database: it gets a complete new one.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, DBName), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	s := openT(t, dir, Options{})
	if got := schemaObjects(t, s.db.r); !slices.Equal(got, wantSchema) {
		t.Errorf("0-byte file: schema objects %q", got)
	}
	s.Close()
}

// TestReadOnlyURI: the read-only URI keeps every path whole on every OS:
// a Windows drive is not the URI's authority (file:///C:/...), a UNC path
// keeps an empty authority, and a space, '#', '?' or '%' is escaped.
func TestReadOnlyURI(t *testing.T) {
	for in, want := range map[string]string{
		"/home/me/.local/state/fyisp/fyisp.db":           "file:///home/me/.local/state/fyisp/fyisp.db?mode=ro",
		"C:/Users/Me/AppData/Local/fyisp/fyisp.db":       "file:///C:/Users/Me/AppData/Local/fyisp/fyisp.db?mode=ro",
		"C:/Users/Jane Doe/AppData/Local/fyisp/fyisp.db": "file:///C:/Users/Jane%20Doe/AppData/Local/fyisp/fyisp.db?mode=ro",
		"//server/share/fyisp/fyisp.db":                  "file:////server/share/fyisp/fyisp.db?mode=ro",
		"/data/my data #2/50% ?/fyisp.db":                "file:///data/my%20data%20%232/50%25%20%3F/fyisp.db?mode=ro",
	} {
		if got := readOnlyURI(in); got != want {
			t.Errorf("%s:\n got %s\nwant %s", in, got, want)
		}
	}
}

// TestOddPathReplaced: a data directory whose name has spaces, '#' and '%'
// works end to end: an old database there is classified and replaced, the
// new one reopens, and `fyisp export` opens it.
func TestOddPathReplaced(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "my data #2 50%")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeOld(t, dir, 6)
	log, logs := testLog()
	s := openT(t, dir, Options{Log: log})
	if got := schemaObjects(t, s.db.r); !slices.Equal(got, wantSchema) {
		t.Errorf("schema objects %q", got)
	}
	s.Close()
	if !strings.Contains(logs.String(), "replaced with a new empty one") {
		t.Errorf("log:\n%s", logs)
	}
	s = openT(t, dir, Options{})
	s.Close()
	ro, err := OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	ro.Close()
}
