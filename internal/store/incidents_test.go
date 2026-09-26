package store

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

func TestIncidentsCRUD(t *testing.T) {
	now := t0.Add(10 * time.Hour)
	s := openT(t, t.TempDir(), Options{Now: func() time.Time { return now }})
	defer s.Close()

	a := &model.Incident{Start: t0.Add(time.Hour), End: t0.Add(time.Hour + 5*time.Minute), Kind: model.VerdictLAN,
		Summary: "Your Wi-Fi or router is dropping 34% of packets.", PeakLoss: 0.34}
	b := &model.Incident{Start: t0.Add(3 * time.Hour), Kind: model.VerdictService,
		Summary: "Only GitHub looks affected.", Targets: []string{"github"}, PeakLoss: 0.5}
	for _, in := range []*model.Incident{a, b} {
		if err := s.SaveIncident(ctx, in); err != nil {
			t.Fatal(err)
		}
		if in.ID == 0 {
			t.Fatal("no id")
		}
	}
	if a.ID == b.ID {
		t.Fatal("same id")
	}

	got, err := s.Incidents(ctx, t0, t0.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != b.ID || got[1].ID != a.ID {
		t.Fatalf("newest first: %+v", got)
	}
	if !got[1].Start.Equal(a.Start) || !got[1].End.Equal(a.End) || got[1].Kind != model.VerdictLAN || got[1].PeakLoss != 0.34 || got[1].Targets != nil {
		t.Fatalf("round trip a: %+v", got[1])
	}
	if !got[0].End.IsZero() || !slices.Equal(got[0].Targets, []string{"github"}) {
		t.Fatalf("round trip b: %+v", got[0])
	}

	// Overlap: a ended before the range; b is ongoing so it overlaps anything after its start.
	for _, c := range []struct {
		from, to time.Time
		want     []int64
	}{
		{t0.Add(2 * time.Hour), t0.Add(48 * time.Hour), []int64{b.ID}},
		{t0.Add(time.Hour + 5*time.Minute), t0.Add(2 * time.Hour), []int64{a.ID}}, // touches a's end
		{t0, t0.Add(time.Hour - time.Second), nil},
		{t0.Add(100 * time.Hour), t0.Add(101 * time.Hour), []int64{b.ID}},
	} {
		got, err := s.Incidents(ctx, c.from, c.to)
		if err != nil {
			t.Fatal(err)
		}
		var ids []int64
		for _, in := range got {
			ids = append(ids, in.ID)
		}
		if !slices.Equal(ids, c.want) {
			t.Errorf("[%v, %v]: %v, want %v", c.from, c.to, ids, c.want)
		}
	}

	// Update: close b.
	now = now.Add(time.Minute)
	b.End = t0.Add(3*time.Hour + 10*time.Minute)
	b.PeakLoss = 0.9
	b.Targets = []string{"github", "gitlab"}
	if err := s.SaveIncident(ctx, b); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Incidents(ctx, b.Start, b.Start)
	if len(got) != 1 || !got[0].End.Equal(b.End) || got[0].PeakLoss != 0.9 || len(got[0].Targets) != 2 {
		t.Fatalf("updated: %+v", got)
	}
	if err := s.SaveIncident(ctx, &model.Incident{ID: 999, Start: t0, Kind: model.VerdictLAN}); err == nil {
		t.Fatal("updating a missing incident succeeded")
	}
}

func TestOpenIncidents(t *testing.T) {
	now := t0
	s := openT(t, t.TempDir(), Options{Now: func() time.Time { return now }})
	defer s.Close()
	open := &model.Incident{Start: t0.Add(-time.Minute), Kind: model.VerdictISP, Summary: "x"}
	closed := &model.Incident{Start: t0.Add(-time.Hour), End: t0.Add(-30 * time.Minute), Kind: model.VerdictLAN, Summary: "y"}
	for _, in := range []*model.Incident{open, closed} {
		if err := s.SaveIncident(ctx, in); err != nil {
			t.Fatal(err)
		}
	}
	now = t0.Add(30 * time.Second)
	if err := s.SaveIncident(ctx, open); err != nil {
		t.Fatal(err)
	}
	var ids []int64
	var ups []time.Time
	if err := s.OpenIncidents(ctx, func(in model.Incident, up time.Time) { ids = append(ids, in.ID); ups = append(ups, up) }); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ids, []int64{open.ID}) || !ups[0].Equal(t0.Add(30*time.Second)) {
		t.Fatalf("open %v updated %v", ids, ups)
	}
}

func TestPruneIncidents(t *testing.T) {
	now := t0.Add(100 * 24 * time.Hour)
	s := openT(t, t.TempDir(), Options{Now: func() time.Time { return now }})
	defer s.Close()
	k := model.SeriesKey{Target: "a", Kind: model.KindTCP}
	s.Observe(model.Sample{Key: k, Slot: now.Add(-time.Minute), RTT: time.Millisecond})
	if err := s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	cut := now.Add(-90 * 24 * time.Hour)
	old := &model.Incident{Start: cut.Add(-2 * time.Hour), End: cut.Add(-time.Hour), Kind: model.VerdictLAN, Summary: "old"}
	straddle := &model.Incident{Start: cut.Add(-2 * time.Hour), End: cut.Add(time.Hour), Kind: model.VerdictLAN, Summary: "straddle"}
	ongoing := &model.Incident{Start: cut.Add(-5 * time.Hour), Kind: model.VerdictLAN, Summary: "ongoing"}
	recent := &model.Incident{Start: now.Add(-time.Hour), End: now.Add(-50 * time.Minute), Kind: model.VerdictDNS, Summary: "recent"}
	for _, in := range []*model.Incident{old, straddle, ongoing, recent} {
		if err := s.SaveIncident(ctx, in); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Prune(ctx, cut); err != nil {
		t.Fatal(err)
	}
	got, err := s.Incidents(ctx, time.Time{}, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, in := range got {
		names = append(names, in.Summary)
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"ongoing", "recent", "straddle"}) {
		t.Fatalf("after prune: %v", names)
	}
}

// TestFixtureV1ToV2 upgrades the released v1 fixture: the incidents table
// appears, the samples survive and a v1 backup is written.
func TestFixtureV1ToV2(t *testing.T) {
	dir := fixtureDir(t)
	s := openT(t, dir, Options{Now: func() time.Time { return t0.Add(2*time.Hour + 30*time.Minute) }})
	defer s.Close()
	if v := pragmaInt(t, s.db.w, "user_version"); v != 2 || SchemaVersion() != 2 {
		t.Fatalf("user_version %d, SchemaVersion %d", v, SchemaVersion())
	}
	checkFixture(t, s)
	in := &model.Incident{Start: t0, Kind: model.VerdictUpstream, Summary: "z"}
	if err := s.SaveIncident(ctx, in); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Incidents(ctx, t0, t0); err != nil || len(got) != 1 {
		t.Fatalf("%v %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(dir, DBName+".bak-v1")); err != nil {
		t.Fatal(err)
	}
}

func TestIncidentsReadOnly(t *testing.T) {
	dir := t.TempDir()
	s := openT(t, dir, Options{})
	in := &model.Incident{Start: t0, Kind: model.VerdictDNS, Summary: "d"}
	if err := s.SaveIncident(ctx, in); err != nil {
		t.Fatal(err)
	}
	s.Close()
	r, err := OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if got, err := r.Incidents(ctx, t0, t0.Add(time.Hour)); err != nil || len(got) != 1 {
		t.Fatalf("%v %v", got, err)
	}
	if err := r.SaveIncident(ctx, in); err == nil {
		t.Fatal("read-only save succeeded")
	}
}
