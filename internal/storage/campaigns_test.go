package storage

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/s3test"
	"github.com/fuck-you-isp/fyisp/internal/speedtest"
)

// memStore is the campaigns' store in memory.
type memStore struct {
	mu       sync.Mutex
	camps    map[int64]model.StorageCampaign
	rounds   []model.StorageRound
	attempts map[int64][]model.StorageAttempt
	settings map[string]string
	traces   []model.StorageTrace
}

func newMemStore() *memStore {
	return &memStore{camps: map[int64]model.StorageCampaign{}, attempts: map[int64][]model.StorageAttempt{}, settings: map[string]string{}}
}

func (m *memStore) SaveStorageCampaign(_ context.Context, c *model.StorageCampaign) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c.ID == 0 {
		c.ID = int64(len(m.camps) + 1)
	}
	m.camps[c.ID] = *c
	return nil
}

func (m *memStore) StorageCampaign(_ context.Context, id int64) (model.StorageCampaign, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.camps[id]
	return c, ok, nil
}

func (m *memStore) StorageCampaigns(_ context.Context, before int64, limit int, live bool) ([]model.StorageCampaign, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []model.StorageCampaign
	for _, id := range slices.Sorted(maps.Keys(m.camps)) {
		if c := m.camps[id]; (before == 0 || id < before) && (!live || c.Live()) {
			out = append(out, c)
		}
	}
	slices.Reverse(out)
	return out[:min(limit, len(out))], nil
}

func (m *memStore) SaveStorageRound(_ context.Context, r *model.StorageRound, as []model.StorageAttempt) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r.ID = int64(len(m.rounds) + 1)
	m.rounds = append(m.rounds, *r)
	m.attempts[r.ID] = as
	return nil
}

func (m *memStore) StorageRounds(_ context.Context, campaign int64, after time.Time) ([]model.StorageRound, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []model.StorageRound
	for _, r := range m.rounds {
		if r.Campaign == campaign && r.Due.After(after) {
			out = append(out, r)
		}
	}
	return out, nil
}

func (m *memStore) StorageRoundsTail(_ context.Context, campaign int64) (int, time.Time, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	seq, last := -1, time.Time{}
	for _, r := range m.rounds {
		if r.Campaign != campaign {
			continue
		}
		if r.Seq != nil && *r.Seq > seq {
			seq = *r.Seq
		}
		for _, t := range []time.Time{r.Due, r.End} {
			if t.After(last) {
				last = t
			}
		}
	}
	return seq, last, nil
}

func (m *memStore) SaveStorageTrace(_ context.Context, t *model.StorageTrace) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.traces = append(m.traces, *t)
	t.ID = int64(len(m.traces))
	return nil
}

func (m *memStore) StorageTraces(_ context.Context, campaign int64) ([]model.StorageTrace, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []model.StorageTrace
	for _, t := range m.traces {
		if t.Campaign == campaign {
			out = append(out, t)
		}
	}
	return out, nil
}

func (m *memStore) Setting(_ context.Context, k string) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.settings[k]
	return v, ok, nil
}

func (m *memStore) SetSetting(_ context.Context, k, v string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.settings[k] = v
	return nil
}

// roundsOf is campaign id's rows: "seq:outcome" for runs, "seq:skip:why"
// for skips, "gap" for gaps.
func (m *memStore) roundsOf(id int64) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, r := range m.rounds {
		if r.Campaign != id {
			continue
		}
		switch {
		case r.Outcome == model.RoundGap:
			out = append(out, "gap")
		case r.Outcome == model.RoundSkipped:
			out = append(out, fmt.Sprintf("%d:skip:%s", *r.Seq, r.SkipReason))
		default:
			out = append(out, fmt.Sprintf("%d:%s", *r.Seq, r.Outcome))
		}
	}
	return out
}

// camLab is campaigns over a real queue and targets service, the engine
// faked (no network): a host with "auth" in it refuses keys, one with
// "down" doesn't answer. It runs in a synctest bubble.
type camLab struct {
	t      *testing.T
	st     *memStore
	svc    *Service
	q      *speedtest.Queue
	c      *Campaigns
	dir    string
	stop   context.CancelFunc
	done   chan struct{}
	block  chan struct{} // a manual speed test runs until it is closed
	signed sync.Map      // target id -> asked signed
}

func newCamLab(t *testing.T, st *memStore, dir string) *camLab {
	l := &camLab{t: t, st: st, dir: dir, block: make(chan struct{})}
	lookup := func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("52.216.10.10")}, nil
	}
	e := s3test.New(s3test.Options{Lookup: lookup, IsSelf: func(netip.Addr) bool { return false },
		Fake: func(_ context.Context, tg s3test.Target, phase, op string, signed bool) model.StorageAttempt {
			if signed {
				l.signed.Store(tg.ID, true)
			}
			host := tg.Endpoint.Hostname()
			edge := netip.MustParseAddr("52.216.10.10")
			switch {
			case strings.Contains(host, "auth") && signed:
				return model.StorageAttempt{At: time.Now(), Class: string(s3test.ClassHTTPAuth), HTTPStatus: 403}
			case strings.Contains(host, "down"):
				return model.StorageAttempt{At: time.Now(), Class: string(s3test.ClassConnectTimeout), EdgeIP: netip.MustParseAddr("52.216.20.20")}
			}
			return model.StorageAttempt{At: time.Now(), Class: string(s3test.ClassOK), HTTPStatus: 200, EdgeIP: edge}
		}})
	l.svc = Open(Options{Dir: dir, Engine: e, Lookup: lookup})
	l.q = speedtest.NewQueue(speedtest.QueueOptions{
		Resolve: func(speedtest.Kind, string) ([]speedtest.Job, error) { return []speedtest.Job{{}}, nil },
		Exec: func(ctx context.Context, _ speedtest.Job, _ string) (int64, bool) {
			select {
			case <-ctx.Done():
			case <-l.block:
			}
			return 0, false
		},
	})
	l.c = NewCampaigns(CampaignOptions{Store: st, Queue: l.q, Engine: e, Targets: l.svc})
	ctx, cancel := context.WithCancel(context.Background())
	l.stop, l.done = cancel, make(chan struct{})
	go l.q.Run(ctx)
	go func() { defer close(l.done); l.c.Run(ctx) }()
	synctest.Wait()
	return l
}

func (l *camLab) close() {
	l.stop()
	<-l.done
	synctest.Wait()
}

func (l *camLab) target(name, endpoint, bucket string) string {
	l.t.Helper()
	ts, err := l.svc.Add(context.Background(), Spec{Name: name, Endpoint: endpoint, Bucket: bucket})
	if err != nil {
		l.t.Fatal(err)
	}
	return ts.ID
}

func keysFor(ids ...string) map[string]*s3test.Creds {
	m := map[string]*s3test.Creds{}
	for _, id := range ids {
		m[id] = s3test.NewCreds("AKIAEXAMPLE"+id[2:6], "secret", "")
	}
	return m
}

func (l *camLab) start(s CampaignSpec, keys map[string]*s3test.Creds) model.StorageCampaign {
	l.t.Helper()
	c, err := l.c.Start(context.Background(), s, keys)
	if err != nil {
		l.t.Fatalf("Start %+v: %v", s, err)
	}
	return c
}

func (l *camLab) campaign(id int64) model.StorageCampaign {
	c, _, _ := l.st.StorageCampaign(context.Background(), id)
	return c
}

// TestCampaignCount: a campaign of 3 rounds every 5 minutes runs them, a
// fresh connection per request and transfers with keys, then finishes and
// drops its keys from memory.
func TestCampaignCount(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newCamLab(t, newMemStore(), t.TempDir())
		defer l.close()
		a := l.target("A", "https://a.example", "bkt")
		b := l.target("B", "https://b.example", "")
		c := l.start(CampaignSpec{Targets: []string{a, b}, Shape: model.StorageOnDemand, Interval: 5 * time.Minute, Count: 3,
			Cold: 2, Warm: 1, Down: 1 << 20, Up: 1 << 20}, keysFor(a))
		if ts, _ := l.svc.Target(a); ts.Keys.State != "memory" || ts.Keys.Campaign != c.ID || l.c.InUse(a) != c.ID {
			t.Fatalf("keys %+v, in use by %d", ts.Keys, l.c.InUse(a))
		}
		if err := l.svc.Delete(b); !errors.Is(err, ErrInUse) {
			t.Errorf("delete a target in use: %v", err)
		}
		time.Sleep(11 * time.Minute)
		synctest.Wait()
		if got := l.st.roundsOf(c.ID); !slices.Equal(got, []string{"0:ok", "1:ok", "2:ok"}) {
			t.Fatalf("rounds %v", got)
		}
		got := l.campaign(c.ID)
		if got.State != model.StorageDone || got.Stopped != model.StopFinished || got.End.IsZero() {
			t.Errorf("campaign %+v", got)
		}
		if ts, _ := l.svc.Target(a); ts.Keys.State != "none" || l.c.InUse(a) != 0 {
			t.Errorf("after: keys %+v, in use by %d", ts.Keys, l.c.InUse(a))
		}
		r := l.st.rounds[0]
		var phases []string
		for _, at := range l.st.attempts[r.ID] {
			phases = append(phases, at.Target[:2]+at.Phase+"/"+at.Op)
		}
		want := []string{"t_cold/get", "t_cold/head", "t_cold/get", "t_cold/head", "t_warm/get", "t_warm/head", "t_down/get", "t_up/put", "t_up/delete"}
		if !slices.Equal(phases, want) || r.BytesDown != 1<<20 || r.BytesUp != 1<<20 || !r.Start.Equal(r.Due) {
			t.Errorf("round %+v\nphases %v", r, phases)
		}
		if _, ok := l.signed.Load(b); ok {
			t.Error("the target without keys was asked signed")
		}
		if v, ok, _ := l.st.Setting(context.Background(), settingInstall); !ok || len(v) != 16 {
			t.Errorf("install id %q", v)
		}
	})
}

// TestCampaignDurationPauseStop: by duration; paused rounds are stored as
// skipped; Stop ends it at once.
func TestCampaignDurationPauseStop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newCamLab(t, newMemStore(), t.TempDir())
		defer l.close()
		a := l.target("A", "https://a.example", "")
		c := l.start(CampaignSpec{Targets: []string{a}, Shape: model.StorageOnDemand, Interval: 5 * time.Minute, Duration: time.Hour, Cold: 1}, nil)
		time.Sleep(6 * time.Minute)
		if _, err := l.c.Pause(context.Background(), c.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := l.c.Pause(context.Background(), c.ID); err == nil {
			t.Error("paused twice")
		}
		time.Sleep(10 * time.Minute)
		if _, err := l.c.Resume(context.Background(), c.ID); err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Minute)
		synctest.Wait()
		if _, err := l.c.Stop(context.Background(), c.ID); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Hour)
		synctest.Wait()
		if got := l.st.roundsOf(c.ID); !slices.Equal(got, []string{"0:ok", "1:ok", "2:skip:paused", "3:skip:paused", "4:ok"}) {
			t.Errorf("rounds %v", got)
		}
		if got := l.campaign(c.ID); got.State != model.StorageStopped || got.Stopped != model.StopOwner {
			t.Errorf("campaign %+v", got)
		}
		if _, err := l.c.Stop(context.Background(), c.ID); !errors.Is(err, ErrNoCampaign) {
			t.Errorf("stop twice: %v", err)
		}
	})
}

// TestCampaignBusy: a round waits behind a long speed test; the ones due
// meanwhile are skipped (busy), and it is dropped (stale) when the test
// ends, having waited over ScheduledStale (the queue drops stale entries
// between tests). Then rounds run again.
func TestCampaignBusy(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newCamLab(t, newMemStore(), t.TempDir())
		defer l.close()
		a := l.target("A", "https://a.example", "")
		if _, _, err := l.q.Enqueue(speedtest.Request{Kind: speedtest.KindSingle}); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		c := l.start(CampaignSpec{Targets: []string{a}, Shape: model.StorageOnDemand, Interval: 4 * time.Minute, Count: 5, Cold: 1}, nil)
		time.Sleep(13 * time.Minute)
		close(l.block)
		time.Sleep(10 * time.Minute)
		synctest.Wait()
		if got := l.st.roundsOf(c.ID); !slices.Equal(got, []string{"1:skip:busy", "2:skip:busy", "3:skip:busy", "0:skip:stale", "4:ok"}) {
			t.Errorf("rounds %v", got)
		}
	})
}

// TestCampaignPausedWhileWaiting: a round paused while it waits behind a
// speed test is skipped (paused) when its turn comes, and the campaign runs
// on after Resume, then finishes.
func TestCampaignPausedWhileWaiting(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newCamLab(t, newMemStore(), t.TempDir())
		defer l.close()
		a := l.target("A", "https://a.example", "")
		if _, _, err := l.q.Enqueue(speedtest.Request{Kind: speedtest.KindSingle}); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		c := l.start(CampaignSpec{Targets: []string{a}, Shape: model.StorageOnDemand, Interval: 4 * time.Minute, Count: 5, Cold: 1}, nil)
		time.Sleep(time.Minute)
		if _, err := l.c.Pause(context.Background(), c.ID); err != nil {
			t.Fatal(err)
		}
		close(l.block)
		time.Sleep(time.Minute)
		if _, err := l.c.Resume(context.Background(), c.ID); err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Minute)
		synctest.Wait()
		if got := l.st.roundsOf(c.ID); !slices.Equal(got, []string{"0:skip:paused", "1:ok", "2:ok", "3:ok", "4:ok"}) {
			t.Errorf("rounds %v", got)
		}
		if got := l.campaign(c.ID); got.State != model.StorageDone {
			t.Errorf("campaign %+v", got)
		}
		if len(l.c.State()) != 0 {
			t.Errorf("still live: %+v", l.c.State())
		}
	})
}

// TestCampaignKeys: keys refused are refused at Start; a second on-demand
// campaign or schedule is refused; an interval shorter than a round is.
func TestCampaignKeys(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newCamLab(t, newMemStore(), t.TempDir())
		defer l.close()
		a := l.target("A", "https://auth.example", "bkt")
		b := l.target("B", "https://b.example", "bkt")
		s := CampaignSpec{Targets: []string{a, b}, Shape: model.StorageOnDemand, Interval: 5 * time.Minute, Count: 2, Cold: 1, Down: 1 << 20}
		var ce *CampaignError
		if _, err := l.c.Start(context.Background(), s, keysFor(a, b)); !errors.As(err, &ce) || ce.Code != "keys_refused" || !slices.Equal(ce.Targets, []string{a}) {
			t.Fatalf("refused keys: %v", err)
		}
		if cs, _ := l.st.StorageCampaigns(context.Background(), 0, 10, false); len(cs) != 0 {
			t.Errorf("a refused campaign was stored: %+v", cs)
		}
		short := s
		short.Interval, short.Down, short.Cold = time.Minute, 1<<30, 20
		if _, err := l.c.Start(context.Background(), short, keysFor(b)); !errors.As(err, &ce) || ce.Code != "interval_too_short" || ce.Min <= time.Minute {
			t.Errorf("short interval: %v", err)
		}
		l.start(s, keysFor(b))
		if _, err := l.c.Start(context.Background(), s, nil); !errors.As(err, &ce) || ce.Code != "active_campaign" {
			t.Errorf("second campaign: %v", err)
		}
		rec := CampaignSpec{Targets: []string{b}, Shape: model.StorageRecurring, Interval: 10 * time.Minute, Cold: 1, Days: 0b1111111,
			FromMin: 19 * 60, ToMin: 23 * 60, TZ: "America/New_York"}
		l.start(rec, nil)
		if _, err := l.c.Start(context.Background(), rec, nil); !errors.As(err, &ce) || ce.Code != "schedule_exists" {
			t.Errorf("second schedule: %v", err)
		}
		for name, bad := range map[string]CampaignSpec{
			"no targets": {Shape: model.StorageOnDemand, Interval: 5 * time.Minute, Count: 1, Cold: 1},
			"no rounds":  {Targets: []string{b}, Shape: model.StorageOnDemand, Interval: 5 * time.Minute, Cold: 1},
			"both":       {Targets: []string{b}, Shape: model.StorageOnDemand, Interval: 5 * time.Minute, Count: 1, Duration: time.Hour, Cold: 1},
			"no request": {Targets: []string{b}, Shape: model.StorageOnDemand, Interval: 5 * time.Minute, Count: 1},
			"tz":         {Targets: []string{b}, Shape: model.StorageRecurring, Interval: 5 * time.Minute, Cold: 1, Days: 1, FromMin: 0, ToMin: 60, TZ: "Local"},
			"no days":    {Targets: []string{b}, Shape: model.StorageRecurring, Interval: 5 * time.Minute, Cold: 1, FromMin: 0, ToMin: 60, TZ: "UTC"},
		} {
			if _, _, err := l.c.validate(bad, nil, time.Now()); err == nil {
				t.Errorf("%s: accepted", name)
			}
		}
	})
}

// TestCampaignWindows: a recurring window's rounds across DST changes (New
// York and Berlin, both ways: the window keeps its wall-clock hours, and
// is an hour shorter or longer) and across midnight, on its weekdays only.
func TestCampaignWindows(t *testing.T) {
	slots := func(tz string, days uint8, from, to int, iv time.Duration, start, end string) []string {
		loc, _ := time.LoadLocation(tz)
		r := &run{c: model.StorageCampaign{Shape: model.StorageRecurring, Days: days, FromMin: from, ToMin: to, TZ: tz, Interval: iv}, loc: loc}
		a, _ := time.ParseInLocation("2006-01-02 15:04", start, loc)
		b, _ := time.ParseInLocation("2006-01-02 15:04", end, loc)
		var out []string
		for t, ok := r.nextSlot(a); ok && t.Before(b); t, ok = r.nextSlot(t.Add(time.Nanosecond)) {
			out = append(out, t.In(loc).Format("Mon 15:04 MST"))
		}
		return out
	}
	for _, c := range []struct {
		name string
		got  []string
		want []string
	}{
		{"New York spring forward", slots("America/New_York", 0b1111111, 60, 4*60, time.Hour, "2026-03-08 00:00", "2026-03-08 12:00"),
			[]string{"Sun 01:00 EST", "Sun 03:00 EDT"}},
		{"New York fall back", slots("America/New_York", 0b1111111, 60, 3*60, time.Hour, "2026-11-01 00:00", "2026-11-01 12:00"),
			[]string{"Sun 01:00 EDT", "Sun 01:00 EST", "Sun 02:00 EST"}},
		{"Berlin spring forward", slots("Europe/Berlin", 0b1111111, 19*60, 21*60, time.Hour, "2026-03-28 12:00", "2026-03-30 00:00"),
			[]string{"Sat 19:00 CET", "Sat 20:00 CET", "Sun 19:00 CEST", "Sun 20:00 CEST"}},
		{"Berlin fall back", slots("Europe/Berlin", 0b1111111, 60, 4*60, 90*time.Minute, "2026-10-25 00:00", "2026-10-25 12:00"),
			[]string{"Sun 01:00 CEST", "Sun 02:30 CEST", "Sun 03:00 CET"}},
		{"across midnight, weekdays", slots("UTC", 0b0011111, 23*60, 60, 30*time.Minute, "2026-10-23 22:00", "2026-10-26 02:00"),
			[]string{"Fri 23:00 UTC", "Fri 23:30 UTC", "Sat 00:00 UTC", "Sat 00:30 UTC"}},
	} {
		if !slices.Equal(c.got, c.want) {
			t.Errorf("%s: %q, want %q", c.name, c.got, c.want)
		}
	}
}

// TestCampaignUntil: a schedule's keys are saved in the targets file and
// deleted when its end passes, live; the schedule ends with a notice.
func TestCampaignUntil(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newCamLab(t, newMemStore(), t.TempDir())
		defer l.close()
		a := l.target("A", "https://a.example", "bkt")
		now := time.Now()
		c := l.start(CampaignSpec{Targets: []string{a}, Shape: model.StorageRecurring, Interval: 10 * time.Minute, Cold: 1, Down: 1 << 20,
			Days: 0b1111111, FromMin: 0, ToMin: 23*60 + 59, TZ: "UTC", Until: now.Add(35 * time.Minute)}, keysFor(a))
		if ts, _ := l.svc.Target(a); ts.Keys.State != "saved" || !c.KeysSaved {
			t.Fatalf("keys %+v", ts.Keys)
		}
		raw, _ := os.ReadFile(l.svc.path)
		if !strings.Contains(string(raw), `"secret": "secret"`) {
			t.Fatalf("not saved:\n%s", raw)
		}
		time.Sleep(time.Hour)
		synctest.Wait()
		got := l.campaign(c.ID)
		if got.State != model.StorageDone || got.Stopped != model.StopUntil || got.KeysSaved {
			t.Errorf("campaign %+v", got)
		}
		if rs := l.st.roundsOf(c.ID); len(rs) != 4 || rs[3] != "3:ok" {
			t.Errorf("rounds %v", rs)
		}
		raw, _ = os.ReadFile(l.svc.path)
		if strings.Contains(string(raw), "secret") {
			t.Errorf("keys left:\n%s", raw)
		}
		if ns := l.c.Notices(); len(ns) != 1 || ns[0].Code != "schedule_ended" {
			t.Errorf("notices %+v", ns)
		}
	})
}

// TestCampaignRestart: after fyisp was down, a live campaign gets a gap
// row and goes on; an on-demand one's keys are gone (its bucket targets
// wait for keys, with a notice) and ProvideKeys brings them back; a
// schedule whose end passed meanwhile is ended and its keys deleted.
func TestCampaignRestart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		st, dir := newMemStore(), t.TempDir()
		l := newCamLab(t, st, dir)
		a := l.target("A", "https://a.example", "bkt")
		b := l.target("B", "https://b.example", "bkt")
		od := l.start(CampaignSpec{Targets: []string{a}, Shape: model.StorageOnDemand, Interval: 5 * time.Minute, Count: 50, Cold: 1, Down: 1 << 20}, keysFor(a))
		rec := l.start(CampaignSpec{Targets: []string{b}, Shape: model.StorageRecurring, Interval: 30 * time.Minute, Cold: 1, Days: 0b1111111,
			FromMin: 0, ToMin: 23*60 + 59, TZ: "UTC", Until: time.Now().Add(time.Hour)}, keysFor(b))
		time.Sleep(6 * time.Minute)
		synctest.Wait()
		l.close()

		time.Sleep(2 * time.Hour) // fyisp is down
		l = newCamLab(t, st, dir)
		defer l.close()
		if got := l.campaign(rec.ID); got.State != model.StorageDone || got.Stopped != model.StopUntil {
			t.Errorf("schedule %+v", got)
		}
		if ts, _ := l.svc.Target(b); ts.Keys.State != "none" {
			t.Errorf("schedule keys %+v", ts.Keys)
		}
		live := l.c.State()
		if len(live) != 1 || live[0].ID != od.ID || !slices.Equal(live[0].WaitingKeys, []string{a}) {
			t.Fatalf("live %+v", live)
		}
		if ns := l.c.Notices(); len(ns) != 2 || !slices.ContainsFunc(ns, func(n Notice) bool { return n.Code == "keys_lost" && n.Campaign == od.ID }) {
			t.Errorf("notices %+v", ns)
		}
		time.Sleep(5 * time.Minute)
		synctest.Wait()
		if err := l.c.ProvideKeys(context.Background(), od.ID, keysFor(a)); err != nil {
			t.Fatal(err)
		}
		if err := l.c.ProvideKeys(context.Background(), od.ID, keysFor(a)); err == nil {
			t.Error("keys for a target that isn't waiting")
		}
		time.Sleep(5 * time.Minute)
		synctest.Wait()
		// Down from 0:06 to 2:06: the slots from 0:10 to 2:05 are a gap;
		// 2:10 runs without keys (connection checks), 2:15 with them.
		if rs := st.roundsOf(od.ID); !slices.Equal(rs, []string{"0:ok", "1:ok", "gap", "26:ok", "27:ok"}) {
			t.Errorf("rounds %v", rs)
		}
		st.mu.Lock()
		after := st.rounds[len(st.rounds)-2:]
		for _, r := range st.rounds {
			if r.Campaign == od.ID && r.Outcome == model.RoundGap && !r.Due.Equal(od.Start.Add(6*time.Minute)) {
				t.Errorf("the gap starts at %v, not when fyisp stopped", r.Due) // not at the last round (0:05)
			}
		}
		st.mu.Unlock()
		if after[0].Skips[a] != model.SkipNoKeys || after[0].BytesDown != 0 || after[1].BytesDown != 1<<20 || len(after[1].Skips) != 0 {
			t.Errorf("the rounds after the restart: %+v", after)
		}
	})
}

// TestCampaignQueueCancel: a round the owner cancels in the queue is
// stored as cancelled and the campaign goes on; one waiting when the
// campaign is paused is skipped paused when its turn comes.
func TestCampaignQueueCancel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newCamLab(t, newMemStore(), t.TempDir())
		defer l.close()
		a := l.target("A", "https://a.example", "")
		if _, _, err := l.q.Enqueue(speedtest.Request{Kind: speedtest.KindSingle}); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		c := l.start(CampaignSpec{Targets: []string{a}, Shape: model.StorageOnDemand, Interval: 3 * time.Minute, Count: 4, Cold: 1}, nil)
		synctest.Wait()
		e := l.c.State()[0].Entry
		if e == 0 {
			t.Fatal("no round queued")
		}
		if _, err := l.q.Cancel(e); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if l.c.State()[0].Entry != 0 {
			t.Fatal("the cancelled round is still the campaign's")
		}
		time.Sleep(4 * time.Minute) // round 1 queues behind the speed test
		synctest.Wait()
		if _, err := l.c.Pause(context.Background(), c.ID); err != nil {
			t.Fatal(err)
		}
		close(l.block)
		time.Sleep(10 * time.Minute)
		synctest.Wait()
		if got := l.st.roundsOf(c.ID); !slices.Equal(got, []string{"0:cancelled", "1:skip:paused", "2:skip:paused", "3:skip:paused"}) {
			t.Errorf("rounds %v", got)
		}
	})
}

// TestCampaignClockJump: a clock that jumps ahead (a suspended machine)
// gives one gap, not a round or a skip per missed slot.
func TestCampaignClockJump(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newCamLab(t, newMemStore(), t.TempDir())
		defer l.close()
		a := l.target("A", "https://a.example", "")
		now := time.Now()
		var mu sync.Mutex
		jump := time.Duration(0)
		l.c.o.Now = func() time.Time { mu.Lock(); defer mu.Unlock(); return time.Now().Add(jump) }
		c := l.start(CampaignSpec{Targets: []string{a}, Shape: model.StorageRecurring, Interval: time.Minute, Cold: 1,
			Days: 0b1111111, FromMin: 0, ToMin: 23*60 + 59, TZ: "UTC"}, nil)
		time.Sleep(90 * time.Second)
		synctest.Wait()
		mu.Lock()
		jump = 3 * time.Hour
		mu.Unlock()
		l.c.poke()
		time.Sleep(90 * time.Second)
		synctest.Wait()
		got := l.st.roundsOf(c.ID)
		if len(got) > 6 || !slices.Contains(got, "gap") {
			t.Errorf("rounds after a 3 h jump (%v from %v): %v", jump, now, got)
		}
	})
}

// TestCampaignLineAndTraces: a round stores the line's loss and RTT; the
// first round traces each public target's edge ("start"), a failure
// traces it again ("failure") once the /24's gap has passed, one trace at
// a time; a route that changed is flagged.
func TestCampaignLineAndTraces(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newCamLab(t, newMemStore(), t.TempDir())
		defer l.close()
		var traced []string
		var mu sync.Mutex
		hop := "62.115.1.1"
		l.c.o.Line = func(from, to time.Time) (*model.LoadStats, *model.LoadStats) {
			return &model.LoadStats{N: 10, Loss: 0.1, P50Ms: 2}, nil
		}
		l.c.o.Trace = func(ctx context.Context, ip netip.Addr) ([]model.StorageHop, uint32, bool) {
			if ctx.Err() != nil {
				return nil, 0, false // the round's context must not end the trace
			}
			mu.Lock()
			traced = append(traced, ip.String())
			mu.Unlock()
			time.Sleep(time.Minute) // a trace takes a while
			mu.Lock()
			defer mu.Unlock()
			return []model.StorageHop{{IP: "192.168.1.1"}, {IP: hop, ASN: 1299}}, 1299, true
		}
		a := l.target("A", "https://a.example", "")
		d := l.target("D", "https://down.example", "")
		c := l.start(CampaignSpec{Targets: []string{a, d}, Shape: model.StorageOnDemand, Interval: 4 * time.Minute, Count: 5, Cold: 1}, nil)
		time.Sleep(7 * time.Minute) // D's first trace (round 1, at 4 min) is done
		synctest.Wait()
		mu.Lock()
		hop = "62.115.9.9"
		mu.Unlock()
		time.Sleep(10 * time.Minute)
		synctest.Wait()
		if r := l.st.rounds[0]; r.GwLoss == nil || *r.GwLoss != 0.1 || *r.GwRTTms != 2 || r.EdgeLoss != nil {
			t.Errorf("line context %+v", r)
		}
		var got []string
		mu.Lock()
		defer mu.Unlock()
		for _, tr := range l.st.traces {
			got = append(got, fmt.Sprintf("%s:%s:%s:%v", l.name(tr.Target), tr.Trigger, tr.EdgePrefix, tr.RouteChanged))
		}
		// Round 0 traces A at start (one at a time: D waits); round 1, D
		// fails (start); round 3, D fails again 10 min after: failure, the route changed.
		want := []string{"A:start:52.216.10.0/24:false", "D:failure:52.216.20.0/24:false", "D:failure:52.216.20.0/24:true"}
		if !slices.Equal(got, want) || c.ID == 0 {
			t.Errorf("traces %v (asked %v), want %v", got, traced, want)
		}
	})
}

func (l *camLab) name(id string) string {
	ts, _ := l.svc.Target(id)
	return ts.Name
}
