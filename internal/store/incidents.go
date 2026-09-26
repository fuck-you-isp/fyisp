package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

// The incidents table (migration v2) is the outage log written by the
// verdict engine. *SQLite implements verdict.IncidentStore and
// verdict.IncidentRecovery. Times are unix milliseconds; end_ms is NULL while
// an incident is ongoing; updated_ms is stamped (Options.Now) by every
// SaveIncident so that an incident left open by a crash can be closed at its
// last update.

// SaveIncident inserts in (in.ID == 0; in.ID is set) or updates the row with
// in.ID.
func (s *SQLite) SaveIncident(ctx context.Context, in *model.Incident) error {
	if s.lock == nil {
		return errors.New("store: read-only")
	}
	var targets any
	if len(in.Targets) > 0 {
		b, err := json.Marshal(in.Targets)
		if err != nil {
			return err
		}
		targets = string(b)
	}
	var end any
	if !in.End.IsZero() {
		end = in.End.UnixMilli()
	}
	now := s.o.Now().UnixMilli()
	if in.ID == 0 {
		var id int64
		if err := s.db.w.QueryRowContext(ctx, `INSERT INTO incidents(start_ms, end_ms, kind, summary, targets, peak_loss, updated_ms)
			VALUES(?, ?, ?, ?, ?, ?, ?) RETURNING id`,
			in.Start.UnixMilli(), end, string(in.Kind), in.Summary, targets, in.PeakLoss, now).Scan(&id); err != nil {
			return fmt.Errorf("store: save incident: %w", err)
		}
		in.ID = id
		return nil
	}
	res, err := s.db.w.ExecContext(ctx, `UPDATE incidents SET start_ms = ?, end_ms = ?, kind = ?, summary = ?, targets = ?, peak_loss = ?, updated_ms = ?
		WHERE id = ?`,
		in.Start.UnixMilli(), end, string(in.Kind), in.Summary, targets, in.PeakLoss, now, in.ID)
	if err != nil {
		return fmt.Errorf("store: save incident: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return fmt.Errorf("store: save incident: no incident %d", in.ID)
	}
	return nil
}

// Incidents returns the incidents overlapping [from, to] (ongoing ones
// overlap every range after their start), newest first.
func (s *SQLite) Incidents(ctx context.Context, from, to time.Time) ([]model.Incident, error) {
	var out []model.Incident
	err := s.scanIncidents(ctx, `WHERE start_ms <= ? AND (end_ms IS NULL OR end_ms >= ?) ORDER BY start_ms DESC, id DESC`,
		func(in model.Incident, _ time.Time) { out = append(out, in) }, to.UnixMilli(), from.UnixMilli())
	return out, err
}

// OpenIncidents calls fn for every incident without an end, with the time
// it was last saved.
func (s *SQLite) OpenIncidents(ctx context.Context, fn func(in model.Incident, updated time.Time)) error {
	return s.scanIncidents(ctx, `WHERE end_ms IS NULL ORDER BY start_ms, id`, fn)
}

func (s *SQLite) scanIncidents(ctx context.Context, where string, fn func(model.Incident, time.Time), args ...any) error {
	rows, err := s.db.r.QueryContext(ctx, `SELECT id, start_ms, end_ms, kind, summary, targets, peak_loss, updated_ms FROM incidents `+where, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			in           model.Incident
			start        int64
			end, updated sql.NullInt64
			targets      sql.NullString
			peak         sql.NullFloat64
			kind         string
		)
		if err := rows.Scan(&in.ID, &start, &end, &kind, &in.Summary, &targets, &peak, &updated); err != nil {
			return err
		}
		in.Start = time.UnixMilli(start).UTC()
		if end.Valid {
			in.End = time.UnixMilli(end.Int64).UTC()
		}
		in.Kind = model.VerdictKind(kind)
		if targets.Valid && targets.String != "" {
			if err := json.Unmarshal([]byte(targets.String), &in.Targets); err != nil {
				return fmt.Errorf("store: incident %d targets: %w", in.ID, err)
			}
		}
		in.PeakLoss = peak.Float64
		up := in.Start
		if updated.Valid {
			up = time.UnixMilli(updated.Int64).UTC()
		}
		fn(in, up)
	}
	return rows.Err()
}
