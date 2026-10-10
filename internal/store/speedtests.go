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

// The speedtests table (migrations v5 and v6) keeps one row per speed test;
// retention (Prune) drops them with the hours they ended in. *SQLite
// implements speedtest.Store.

// SaveSpeedTest inserts t and sets t.ID.
func (s *SQLite) SaveSpeedTest(ctx context.Context, t *model.SpeedTest) error {
	if s.lock == nil {
		return errors.New("store: read-only")
	}
	level := t.FallbackLevel
	if level == "" {
		level = "none"
	}
	var extra any
	if t.Extra != "" {
		extra = t.Extra
	}
	var lat sql.NullString
	if t.Latency != nil {
		b, err := json.Marshal(t.Latency)
		if err != nil {
			return err
		}
		lat = sql.NullString{String: string(b), Valid: true}
	}
	var hASN, ispASN, hMs sql.NullInt64
	var hName sql.NullString
	if h := t.Handoff; h != nil {
		hASN, ispASN, hMs = sql.NullInt64{Int64: int64(h.ASN), Valid: true}, sql.NullInt64{Int64: int64(h.ISPASN), Valid: true}, sql.NullInt64{Int64: h.At.UnixMilli(), Valid: true}
		hName = sql.NullString{String: h.Name, Valid: true}
	}
	err := s.db.w.QueryRowContext(ctx, `INSERT INTO speedtests(start_ms, end_ms, trigger, outcome, region, server, fallback, port, attempts,
		down_bps, up_bps, down_retrans, up_retrans, idle_gw_ms, loaded_gw_ms, latency_json, conn, handoff_asn, handoff_name, isp_asn, handoff_ms,
		fallback_level, capped_server, down_short, up_short, down_s, up_s, bytes, score_down, score_up, extra)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id`,
		t.Start.UnixMilli(), t.End.UnixMilli(), t.Trigger, t.Outcome, t.Region, t.Server, t.Fallback, t.Port, t.Attempts,
		nullF(t.DownBps), nullF(t.UpBps), nullI(t.DownRetrans), nullI(t.UpRetrans),
		nullF(t.IdleGatewayMs), nullF(t.LoadedGatewayMs), lat, t.Conn, hASN, hName, ispASN, hMs,
		level, t.CappedByServer, t.DownShort, t.UpShort, t.DownSecs, t.UpSecs, t.Bytes, nullF(t.ScoreDown), nullF(t.ScoreUp), extra).Scan(&t.ID)
	if err != nil {
		return fmt.Errorf("store: save speed test: %w", err)
	}
	return nil
}

// SpeedTests returns the tests that started in [from, to], oldest first.
func (s *SQLite) SpeedTests(ctx context.Context, from, to time.Time) ([]model.SpeedTest, error) {
	rows, err := s.db.r.QueryContext(ctx, `SELECT id, start_ms, end_ms, trigger, outcome, region, server, fallback, port, attempts,
		down_bps, up_bps, down_retrans, up_retrans, idle_gw_ms, loaded_gw_ms, latency_json, conn, handoff_asn, handoff_name, isp_asn, handoff_ms,
		fallback_level, capped_server, down_short, up_short, down_s, up_s, bytes, score_down, score_up, coalesce(extra, '')
		FROM speedtests WHERE start_ms >= ? AND start_ms <= ? ORDER BY start_ms, id`, from.UnixMilli(), to.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.SpeedTest
	for rows.Next() {
		var t model.SpeedTest
		var start, end int64
		var down, up, igw, lgw, sd, su sql.NullFloat64
		var dr, ur, hASN, ispASN, hMs sql.NullInt64
		var lat, hName sql.NullString
		if err := rows.Scan(&t.ID, &start, &end, &t.Trigger, &t.Outcome, &t.Region, &t.Server, &t.Fallback, &t.Port, &t.Attempts,
			&down, &up, &dr, &ur, &igw, &lgw, &lat, &t.Conn, &hASN, &hName, &ispASN, &hMs,
			&t.FallbackLevel, &t.CappedByServer, &t.DownShort, &t.UpShort, &t.DownSecs, &t.UpSecs, &t.Bytes, &sd, &su, &t.Extra); err != nil {
			return nil, err
		}
		if lat.Valid {
			var l model.SpeedLatency
			if err := json.Unmarshal([]byte(lat.String), &l); err == nil {
				t.Latency = &l
			}
		}
		if hASN.Valid {
			t.Handoff = &model.Handoff{ASN: uint32(hASN.Int64), Name: hName.String, ISPASN: uint32(ispASN.Int64), At: msTime(hMs.Int64)}
		}
		t.Start, t.End = msTime(start), msTime(end)
		t.DownBps, t.UpBps = ptrF(down), ptrF(up)
		t.DownRetrans, t.UpRetrans = ptrI(dr), ptrI(ur)
		t.IdleGatewayMs, t.LoadedGatewayMs = ptrF(igw), ptrF(lgw)
		t.ScoreDown, t.ScoreUp = ptrF(sd), ptrF(su)
		out = append(out, t)
	}
	return out, rows.Err()
}

// BytesSince is the data the speed tests used since t.
func (s *SQLite) BytesSince(ctx context.Context, t time.Time) (int64, error) {
	var n sql.NullInt64
	err := s.db.r.QueryRowContext(ctx, `SELECT sum(bytes) FROM speedtests WHERE start_ms >= ?`, t.UnixMilli()).Scan(&n)
	return n.Int64, err
}

// Setting returns a dashboard setting saved with SetSetting.
func (s *SQLite) Setting(ctx context.Context, key string) (string, bool, error) {
	var v string
	err := s.db.r.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, "setting:"+key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return v, err == nil, err
}

// SetSetting saves a dashboard setting (e.g. the speed test toggle).
func (s *SQLite) SetSetting(ctx context.Context, key, value string) error {
	if s.lock == nil {
		return errors.New("store: read-only")
	}
	_, err := s.db.w.ExecContext(ctx, `INSERT INTO meta(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		"setting:"+key, value)
	return err
}

func nullF(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}

func nullI(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

func ptrF(v sql.NullFloat64) *float64 {
	if !v.Valid {
		return nil
	}
	return &v.Float64
}

func ptrI(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	return &v.Int64
}
