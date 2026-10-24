package store

import (
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

// TestPruneStorage: rounds due before the cut go with their attempts (even
// one after the cut), as do old traces; a campaign goes once it ended
// before the cut and has no round left.
func TestPruneStorage(t *testing.T) {
	now := t0.Add(100 * 24 * time.Hour)
	s := openT(t, t.TempDir(), Options{Now: func() time.Time { return now }})
	defer s.Close()
	k := model.SeriesKey{Target: "a", Kind: model.KindTCP}
	s.Observe(model.Sample{Key: k, Slot: now.Add(-time.Minute), RTT: time.Millisecond})
	if err := s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	cut := now.Add(-90 * 24 * time.Hour)
	old, recent := cut.Add(-2*time.Hour).UnixMilli(), cut.Add(2*time.Hour).UnixMilli()
	ended := cut.Add(-time.Hour).UnixMilli()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		// 1: ended, all rounds old; 2: ended before the cut, a recent round;
		// 3: ongoing, all rounds old.
		{`INSERT INTO storage_campaigns(id, created_ms, updated_ms, shape, state, targets, cold, warm, down_bytes, up_bytes, interval_s, end_ms)
			VALUES (1, 0, 0, 'on_demand', 'done', '[]', 1, 1, 0, 0, 60, ?), (2, 0, 0, 'on_demand', 'done', '[]', 1, 1, 0, 0, 60, ?),
			(3, 0, 0, 'recurring', 'running', '[]', 1, 1, 0, 0, 60, NULL)`, []any{ended, recent}},
		{`INSERT INTO storage_rounds(id, campaign, seq, due_ms, outcome) VALUES (10, 1, 0, ?1, 'ok'), (11, 2, 0, ?2, 'ok'), (12, 3, 0, ?1, 'ok'), (13, 3, NULL, ?2, 'gap')`,
			[]any{old, recent}},
		{`INSERT INTO storage_attempts(round, target, phase, op, n, at_ms, class) VALUES (10, 't', 'cold', 'head', 0, ?1, 'ok'),
			(10, 't', 'up', 'put', 0, ?2, 'ok'), (11, 't', 'cold', 'head', 0, ?2, 'ok')`, []any{old, recent}},
		{`INSERT INTO storage_traces(campaign, target, at_ms, trigger, hops) VALUES (1, 't', ?1, 'start', '[]'), (2, 't', ?2, 'start', '[]')`, []any{old, recent}},
	} {
		if _, err := s.db.w.Exec(q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Prune(ctx, cut); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		q    string
		want string
	}{
		{`SELECT group_concat(id) FROM (SELECT id FROM storage_campaigns ORDER BY id)`, "2,3"},
		{`SELECT group_concat(id) FROM (SELECT id FROM storage_rounds ORDER BY id)`, "11,13"},
		{`SELECT group_concat(round) FROM storage_attempts`, "11"},
		{`SELECT group_concat(campaign) FROM storage_traces`, "2"},
	} {
		var got string
		if err := s.db.r.QueryRow(c.q).Scan(&got); err != nil {
			t.Fatal(c.q, err)
		}
		if got != c.want {
			t.Errorf("%s: %q, want %q", c.q, got, c.want)
		}
	}
}

// TestStorageRoundTrip: a campaign, a round with its attempts, a skip and
// a gap read back as saved; the round counts toward its campaign; attempts
// page and filter; the transfers are speed-test windows.
func TestStorageRoundTrip(t *testing.T) {
	s := openT(t, t.TempDir(), Options{Now: func() time.Time { return t0 }})
	defer s.Close()
	c := &model.StorageCampaign{Created: t0, Updated: t0, Shape: model.StorageRecurring, State: model.StorageRunning,
		Targets: []model.StorageCampaignTarget{{ID: "t_00000001", Name: "A", Mode: "bucket"}}, Cold: 3, Warm: 2, Down: 1 << 20, Up: 1 << 19,
		Interval: 5 * time.Minute, Days: 0b0011111, FromMin: 19 * 60, ToMin: 23 * 60, TZ: "America/New_York", Until: t0.Add(30 * 24 * time.Hour), KeysSaved: true}
	if err := s.SaveStorageCampaign(ctx, c); err != nil || c.ID == 0 {
		t.Fatal(c.ID, err)
	}
	f := func(v float64) *float64 { return &v }
	n := func(v int) *int { return &v }
	seq := 0
	r := &model.StorageRound{Campaign: c.ID, Seq: &seq, Due: t0, Start: t0.Add(time.Second), End: t0.Add(40 * time.Second), QueueWait: time.Second,
		Outcome: model.RoundFailed, Skips: map[string]string{"t_00000002": model.SkipNoKeys}, GwLoss: f(0.01), BytesDown: 1 << 20, BytesUp: 1 << 19}
	at := t0.Add(2 * time.Second)
	atts := []model.StorageAttempt{
		{Target: "t_00000001", Phase: model.StoragePhaseCold, Op: "head", At: at, Class: "ok", HTTPStatus: 200, DNSms: f(3), Totalms: f(40), EdgePrefix: "52.216.1.0/24"},
		{Target: "t_00000001", Phase: model.StoragePhaseCold, Op: "head", N: 1, At: at.Add(time.Second), Class: "connect_timeout", Totalms: f(5000)},
		{Target: "t_00000001", Phase: model.StoragePhaseDown, Op: "get", At: at.Add(10 * time.Second), Class: "ok", HTTPStatus: 206, Bytes: 1 << 20,
			GoodputBps: f(8e7), Totalms: f(900), Retrans: n(2), RequestID: "ABC", RequestIDHeader: "x-amz-request-id"},
	}
	if err := s.SaveStorageRound(ctx, r, atts); err != nil || r.ID == 0 {
		t.Fatal(r.ID, err)
	}
	skip := &model.StorageRound{Campaign: c.ID, Due: t0.Add(5 * time.Minute), Outcome: model.RoundSkipped, SkipReason: model.SkipBusy}
	gap := &model.StorageRound{Campaign: c.ID, Due: t0.Add(10 * time.Minute), End: t0.Add(20 * time.Minute), Outcome: model.RoundGap}
	for _, x := range []*model.StorageRound{skip, gap} {
		if err := s.SaveStorageRound(ctx, x, nil); err != nil {
			t.Fatal(err)
		}
	}
	got, ok, err := s.StorageCampaign(ctx, c.ID)
	want := *c
	want.RoundsDone, want.BytesDown, want.BytesUp = 1, 1<<20, 1<<19
	if err != nil || !ok || !reflect.DeepEqual(got, want) {
		t.Errorf("campaign\n%+v\nwant\n%+v (%v)", got, want, err)
	}
	rs, err := s.StorageRounds(ctx, c.ID, time.Time{})
	if err != nil || len(rs) != 3 || !reflect.DeepEqual(rs[0], *r) || rs[1].SkipReason != model.SkipBusy || rs[1].Seq != nil || rs[2].Outcome != model.RoundGap || !rs[2].End.Equal(gap.End) {
		t.Errorf("rounds %+v %v", rs, err)
	}
	if rs, _ := s.StorageRounds(ctx, c.ID, t0); len(rs) != 2 {
		t.Errorf("after the first: %d", len(rs))
	}
	as, total, err := s.StorageAttempts(ctx, StorageAttemptFilter{Round: r.ID})
	if err != nil || total != 3 || len(as) != 3 || !reflect.DeepEqual(as[2].StorageAttempt, atts[2]) || as[0].Round != r.ID {
		t.Errorf("round attempts %+v %d %v", as, total, err)
	}
	as, total, _ = s.StorageAttempts(ctx, StorageAttemptFilter{Campaign: c.ID, Failed: true})
	if total != 1 || len(as) != 1 || as[0].Class != "connect_timeout" {
		t.Errorf("failed %+v %d", as, total)
	}
	as, total, _ = s.StorageAttempts(ctx, StorageAttemptFilter{Campaign: c.ID, Limit: 2})
	if total != 3 || len(as) != 2 || as[0].Phase != model.StoragePhaseDown {
		t.Errorf("newest first, a page of 2: %+v %d", as, total)
	}
	if more, _, _ := s.StorageAttempts(ctx, StorageAttemptFilter{Campaign: c.ID, Limit: 2, Before: as[1].ID}); len(more) != 1 || more[0].N != 0 {
		t.Errorf("next page %+v", more)
	}
	ws, err := s.SpeedWindows(ctx, t0, t0.Add(time.Hour))
	if err != nil || len(ws) != 1 || ws[0].Trigger != model.TriggerStorage || !ws[0].Start.Equal(at.Add(10*time.Second)) || !ws[0].End.Equal(at.Add(10*time.Second+900*time.Millisecond)) {
		t.Errorf("windows %+v %v", ws, err)
	}
	if ts, _ := s.SpeedTests(ctx, t0, t0.Add(time.Hour)); len(ts) != 0 {
		t.Errorf("rounds aren't speed tests: %+v", ts)
	}
	cs, err := s.StorageCampaigns(ctx, 0, 10, true)
	if err != nil || len(cs) != 1 {
		t.Errorf("live %v %v", cs, err)
	}
	got.State, got.Stopped, got.End = model.StorageStopped, model.StopOwner, t0.Add(time.Hour)
	if err := s.SaveStorageCampaign(ctx, &got); err != nil {
		t.Fatal(err)
	}
	if cs, _ := s.StorageCampaigns(ctx, 0, 10, true); len(cs) != 0 {
		t.Errorf("live after stop %v", cs)
	}
	if cs, _ := s.StorageCampaigns(ctx, c.ID, 10, false); len(cs) != 0 {
		t.Errorf("before its own id %v", cs)
	}
}

// TestStorageCellsCache: StorageCells reads only newer rounds into its
// cache, and starts afresh when pruning took every round, also for a new
// campaign that got a pruned one's id.
func TestStorageCellsCache(t *testing.T) {
	s := openT(t, t.TempDir(), Options{})
	defer s.Close()
	ex := func(q string) {
		t.Helper()
		if _, err := s.db.w.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	targets := func() []string {
		t.Helper()
		rs, err := s.StorageCells(ctx, 1)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, r := range rs {
			out = append(out, r.Target)
		}
		return out
	}
	ex(`INSERT INTO storage_campaigns(id, created_ms, updated_ms, shape, state, targets, cold, warm, down_bytes, up_bytes, interval_s) VALUES (1,0,0,'on_demand','done','[]',1,1,0,0,60)`)
	ex(`INSERT INTO storage_rounds(id, campaign, seq, due_ms, outcome) VALUES (100, 1, 0, 1, 'ok')`)
	ex(`INSERT INTO storage_attempts(round, target, phase, op, n, at_ms, class) VALUES (100,'old','cold','head',0,1,'ok')`)
	if got := targets(); !slices.Equal(got, []string{"old"}) {
		t.Fatalf("first read %v", got)
	}
	ex(`INSERT INTO storage_rounds(id, campaign, seq, due_ms, outcome) VALUES (101, 1, 1, 2, 'ok')`)
	ex(`INSERT INTO storage_attempts(round, target, phase, op, n, at_ms, class) VALUES (101,'later','cold','head',0,2,'ok')`)
	if got := targets(); !slices.Equal(got, []string{"old", "later"}) {
		t.Fatalf("second read %v", got)
	}
	ex(`DELETE FROM storage_attempts`)
	ex(`DELETE FROM storage_rounds`)
	ex(`DELETE FROM storage_campaigns`)
	if got := targets(); len(got) != 0 {
		t.Errorf("all pruned: %v", got)
	}
	// The tables are empty, so SQLite hands out id 1 again.
	ex(`INSERT INTO storage_campaigns(created_ms, updated_ms, shape, state, targets, cold, warm, down_bytes, up_bytes, interval_s) VALUES (0,0,'on_demand','done','[]',1,1,0,0,60)`)
	ex(`INSERT INTO storage_rounds(campaign, seq, due_ms, outcome) VALUES (1, 0, 3, 'ok')`)
	ex(`INSERT INTO storage_attempts(round, target, phase, op, n, at_ms, class) SELECT id,'new','cold','head',0,3,'ok' FROM storage_rounds`)
	if got := targets(); !slices.Equal(got, []string{"new"}) {
		t.Errorf("reused id: %v", got)
	}
}
