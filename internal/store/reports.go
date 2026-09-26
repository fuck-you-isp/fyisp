package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/klauspost/compress/zstd"
)

// The reports table (migration v4) keeps rendered evidence report
// snapshots, zstd-compressed (HTML shrinks ~5-10x). *SQLite and *Fake
// implement ReportStore.
//
// Reports are user-created, so retention (Prune) never deletes them; the
// store keeps at most MaxReports and deletes the oldest (by Created) when a
// save goes over.

// Report limits.
const (
	MaxReportBytes = 8 << 20 // uncompressed HTML
	MaxReports     = 200
	MaxReportTitle = 200 // characters, after trimming
)

var reportID = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)

// ValidReportID reports whether id has the stored form: 8-64 characters of
// [A-Za-z0-9_-].
func ValidReportID(id string) bool { return reportID.MatchString(id) }

// normReport validates meta and html and normalizes meta in place (title
// trimmed, times UTC milliseconds, Created defaulting to now, Bytes set).
func normReport(meta *model.ReportMeta, html []byte, now time.Time) error {
	meta.Title = strings.TrimSpace(meta.Title)
	switch {
	case !ValidReportID(meta.ID):
		return invalid("report id %q (want 8-64 of A-Z a-z 0-9 _ -)", meta.ID)
	case len(html) == 0:
		return invalid("report is empty")
	case len(html) > MaxReportBytes:
		return invalid("report is %d bytes, more than %d", len(html), MaxReportBytes)
	case !utf8.ValidString(meta.Title):
		return invalid("report title is not UTF-8")
	case utf8.RuneCountInString(meta.Title) > MaxReportTitle:
		return invalid("report title is longer than %d characters", MaxReportTitle)
	case !meta.From.IsZero() && !meta.To.IsZero() && meta.To.Before(meta.From):
		return invalid("report range ends before it starts")
	}
	if meta.Created.IsZero() {
		meta.Created = now
	}
	for _, t := range []*time.Time{&meta.From, &meta.To, &meta.Created} {
		if !t.IsZero() {
			*t = msTime(t.UnixMilli())
		}
	}
	meta.Bytes = len(html)
	return nil
}

var reportCodec = sync.OnceValues(func() (*zstd.Encoder, *zstd.Decoder) {
	enc, err := zstd.NewWriter(nil,
		zstd.WithEncoderLevel(zstd.SpeedDefault),
		zstd.WithEncoderCRC(true),
		zstd.WithEncoderConcurrency(1),
		zstd.WithLowerEncoderMem(true))
	if err != nil {
		panic(err)
	}
	dec, err := zstd.NewReader(nil,
		zstd.WithDecoderConcurrency(1),
		zstd.WithDecoderLowmem(true),
		zstd.WithDecoderMaxMemory(MaxReportBytes))
	if err != nil {
		panic(err)
	}
	return enc, dec
})

func compressReport(html []byte) []byte {
	enc, _ := reportCodec()
	return enc.EncodeAll(html, make([]byte, 0, len(html)/4))
}

func decompressReport(data []byte, size int) ([]byte, error) {
	_, dec := reportCodec()
	return dec.DecodeAll(data, make([]byte, 0, size))
}

// SaveReport validates and stores a report snapshot (replacing one with the
// same ID), then deletes the oldest reports beyond MaxReports. A zero
// meta.Created means Options.Now; meta.Bytes is ignored (it is len(html)).
func (s *SQLite) SaveReport(ctx context.Context, meta model.ReportMeta, html []byte) error {
	if s.lock == nil {
		return errReadOnly
	}
	if err := normReport(&meta, html, s.o.Now()); err != nil {
		return err
	}
	data := compressReport(html) // before BEGIN: keep the write short
	tx, err := s.db.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO reports(id, title, from_ms, to_ms, created_ms, public, bytes, html)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET title = excluded.title, from_ms = excluded.from_ms, to_ms = excluded.to_ms,
			created_ms = excluded.created_ms, public = excluded.public, bytes = excluded.bytes, html = excluded.html`,
		meta.ID, meta.Title, nullMs(meta.From), nullMs(meta.To), meta.Created.UnixMilli(), meta.Public, meta.Bytes, data); err != nil {
		return fmt.Errorf("store: save report: %w", err)
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM reports WHERE rowid IN
		(SELECT rowid FROM reports ORDER BY created_ms DESC, rowid DESC LIMIT -1 OFFSET ?)`, MaxReports)
	if err != nil {
		return fmt.Errorf("store: save report: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return incrementalVacuum(ctx, s.db.w)
	}
	return nil
}

const reportCols = `id, title, from_ms, to_ms, created_ms, public, bytes`

func scanReport(sc interface{ Scan(...any) error }, extra ...any) (model.ReportMeta, error) {
	var (
		m                 model.ReportMeta
		title             sql.NullString
		from, to, created sql.NullInt64
	)
	if err := sc.Scan(append([]any{&m.ID, &title, &from, &to, &created, &m.Public, &m.Bytes}, extra...)...); err != nil {
		return m, err
	}
	m.Title = title.String
	for _, x := range []struct {
		v sql.NullInt64
		t *time.Time
	}{{from, &m.From}, {to, &m.To}, {created, &m.Created}} {
		if x.v.Valid {
			*x.t = msTime(x.v.Int64)
		}
	}
	return m, nil
}

// Report returns a stored report and its HTML; ErrNotFound if there is none
// (or id is not a valid report id).
func (s *SQLite) Report(ctx context.Context, id string) (model.ReportMeta, []byte, error) {
	if !ValidReportID(id) {
		return model.ReportMeta{}, nil, fmt.Errorf("%w: report %q", ErrNotFound, id)
	}
	var data []byte
	m, err := scanReport(s.db.r.QueryRowContext(ctx, `SELECT `+reportCols+`, html FROM reports WHERE id = ?`, id), &data)
	if errors.Is(err, sql.ErrNoRows) {
		return model.ReportMeta{}, nil, fmt.Errorf("%w: report %q", ErrNotFound, id)
	}
	if err != nil {
		return model.ReportMeta{}, nil, err
	}
	html, err := decompressReport(data, m.Bytes)
	if err != nil {
		return model.ReportMeta{}, nil, fmt.Errorf("store: report %q: %w", id, err)
	}
	m.Bytes = len(html)
	return m, html, nil
}

// Reports lists the stored reports (without their HTML), newest first.
func (s *SQLite) Reports(ctx context.Context) ([]model.ReportMeta, error) {
	rows, err := s.db.r.QueryContext(ctx, `SELECT `+reportCols+` FROM reports ORDER BY created_ms DESC, rowid DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.ReportMeta{}
	for rows.Next() {
		m, err := scanReport(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// SetReportPublic marks a report public or private; ErrNotFound if there is
// none.
func (s *SQLite) SetReportPublic(ctx context.Context, id string, public bool) error {
	if s.lock == nil {
		return errReadOnly
	}
	res, err := s.db.w.ExecContext(ctx, `UPDATE reports SET public = ? WHERE id = ?`, public, id)
	if err != nil {
		return fmt.Errorf("store: report public: %w", err)
	}
	return notFoundIfNone(res, "report", id)
}

// DeleteReport deletes a report and returns its pages to the OS;
// ErrNotFound if there is none.
func (s *SQLite) DeleteReport(ctx context.Context, id string) error {
	if s.lock == nil {
		return errReadOnly
	}
	res, err := s.db.w.ExecContext(ctx, `DELETE FROM reports WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: delete report: %w", err)
	}
	if err := notFoundIfNone(res, "report", id); err != nil {
		return err
	}
	return incrementalVacuum(ctx, s.db.w)
}

var _ ReportStore = (*SQLite)(nil)
