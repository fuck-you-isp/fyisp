package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

// The annotations table (migration v4) holds the user's timeline notes.
// *SQLite and *Fake implement AnnotationStore. Times are unix milliseconds;
// end_ms is NULL for a note on a point in time. Notes are pruned with the
// samples: a note whose end (or time, without an end) is before the first
// kept hour goes (see Prune).

// ErrNotFound means the annotation or report does not exist.
var ErrNotFound = errors.New("store: not found")

// ErrInvalid is wrapped by every validation error of annotations and
// reports (the web API answers 400).
var ErrInvalid = errors.New("store: invalid")

var errReadOnly = errors.New("store: read-only")

// MaxAnnotationText is the longest annotation text, in characters (runes),
// after trimming surrounding white space.
const MaxAnnotationText = 500

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrInvalid}, a...)...)
}

// normAnnotation validates a and normalizes it in place: the text is
// trimmed, times are UTC with millisecond precision.
func normAnnotation(a *model.Annotation) error {
	a.Text = strings.TrimSpace(a.Text)
	switch {
	case a.Text == "":
		return invalid("annotation text is empty")
	case !utf8.ValidString(a.Text):
		return invalid("annotation text is not UTF-8")
	case utf8.RuneCountInString(a.Text) > MaxAnnotationText:
		return invalid("annotation text is longer than %d characters", MaxAnnotationText)
	case a.At.IsZero():
		return invalid("annotation has no time")
	case !a.End.IsZero() && a.End.Before(a.At):
		return invalid("annotation ends before it starts")
	}
	a.At = msTime(a.At.UnixMilli())
	if !a.End.IsZero() {
		a.End = msTime(a.End.UnixMilli())
	}
	return nil
}

func msTime(ms int64) time.Time { return time.UnixMilli(ms).UTC() }

// nullMs is t in unix ms, or NULL for the zero time.
func nullMs(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UnixMilli()
}

// AddAnnotation validates and inserts a, setting a.ID, a.Created and
// a.Updated (Options.Now); a.Text is trimmed.
func (s *SQLite) AddAnnotation(ctx context.Context, a *model.Annotation) error {
	if s.lock == nil {
		return errReadOnly
	}
	if err := normAnnotation(a); err != nil {
		return err
	}
	now := s.o.Now().UnixMilli()
	var id int64
	if err := s.db.w.QueryRowContext(ctx, `INSERT INTO annotations(at_ms, end_ms, text, public, created_ms, updated_ms)
		VALUES(?, ?, ?, ?, ?, ?) RETURNING id`,
		a.At.UnixMilli(), nullMs(a.End), a.Text, a.Public, now, now).Scan(&id); err != nil {
		return fmt.Errorf("store: add annotation: %w", err)
	}
	a.ID, a.Created, a.Updated = id, msTime(now), msTime(now)
	return nil
}

// UpdateAnnotation validates a and replaces the note with a.ID (time, end,
// text, public), setting a.Updated and reading back a.Created. ErrNotFound
// if there is no such note.
func (s *SQLite) UpdateAnnotation(ctx context.Context, a *model.Annotation) error {
	if s.lock == nil {
		return errReadOnly
	}
	if err := normAnnotation(a); err != nil {
		return err
	}
	now := s.o.Now().UnixMilli()
	var created sql.NullInt64
	err := s.db.w.QueryRowContext(ctx, `UPDATE annotations SET at_ms = ?, end_ms = ?, text = ?, public = ?, updated_ms = ?
		WHERE id = ? RETURNING created_ms`,
		a.At.UnixMilli(), nullMs(a.End), a.Text, a.Public, now, a.ID).Scan(&created)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: annotation %d", ErrNotFound, a.ID)
	}
	if err != nil {
		return fmt.Errorf("store: update annotation: %w", err)
	}
	a.Updated = msTime(now)
	if created.Valid {
		a.Created = msTime(created.Int64)
	}
	return nil
}

// DeleteAnnotation deletes the note id; ErrNotFound if there is none.
func (s *SQLite) DeleteAnnotation(ctx context.Context, id int64) error {
	if s.lock == nil {
		return errReadOnly
	}
	res, err := s.db.w.ExecContext(ctx, `DELETE FROM annotations WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: delete annotation: %w", err)
	}
	return notFoundIfNone(res, "annotation", id)
}

func notFoundIfNone(res sql.Result, what string, id any) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: %s %v", ErrNotFound, what, id)
	}
	return nil
}

// Annotations returns the notes overlapping [from, to] (a note without an
// end overlaps if its time is in the range), oldest first; publicOnly keeps
// only public notes.
func (s *SQLite) Annotations(ctx context.Context, from, to time.Time, publicOnly bool) ([]model.Annotation, error) {
	q := `SELECT id, at_ms, end_ms, text, public, created_ms, updated_ms FROM annotations
		WHERE at_ms <= ? AND coalesce(end_ms, at_ms) >= ?`
	if publicOnly {
		q += ` AND public = 1`
	}
	fromMs, toMs := msRange(from, to)
	rows, err := s.db.r.QueryContext(ctx, q+` ORDER BY at_ms, id`, toMs, fromMs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Annotation{}
	for rows.Next() {
		var (
			a                     model.Annotation
			at                    int64
			end, created, updated sql.NullInt64
		)
		if err := rows.Scan(&a.ID, &at, &end, &a.Text, &a.Public, &created, &updated); err != nil {
			return nil, err
		}
		a.At = msTime(at)
		if end.Valid {
			a.End = msTime(end.Int64)
		}
		if created.Valid {
			a.Created = msTime(created.Int64)
		}
		if updated.Valid {
			a.Updated = msTime(updated.Int64)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

var _ AnnotationStore = (*SQLite)(nil)
