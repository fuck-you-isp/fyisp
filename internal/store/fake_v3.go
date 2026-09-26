package store

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/store/blob"
)

// Fake's annotations, reports and baselines: the same validation, ordering
// and statistics as *SQLite, kept in memory (reports uncompressed; notes
// pruned like the real store, reports never).

type fakeReport struct {
	meta model.ReportMeta
	html []byte
	seq  int64 // insertion order, breaks Created ties as rowid does
}

func (f *Fake) now() time.Time {
	if f.Now != nil {
		return f.Now()
	}
	return time.Now()
}

// noteEnd is when a note ends: End, or At for a point in time.
func noteEnd(a model.Annotation) time.Time {
	if a.End.IsZero() {
		return a.At
	}
	return a.End
}

func (f *Fake) AddAnnotation(_ context.Context, a *model.Annotation) error {
	if err := normAnnotation(a); err != nil {
		return err
	}
	now := msTime(f.now().UnixMilli())
	f.mu.Lock()
	defer f.mu.Unlock()
	f.noteSeq++
	a.ID, a.Created, a.Updated = f.noteSeq, now, now
	f.notes = append(f.notes, *a)
	return nil
}

func (f *Fake) UpdateAnnotation(_ context.Context, a *model.Annotation) error {
	if err := normAnnotation(a); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	i := slices.IndexFunc(f.notes, func(x model.Annotation) bool { return x.ID == a.ID })
	if i < 0 {
		return fmt.Errorf("%w: annotation %d", ErrNotFound, a.ID)
	}
	a.Created, a.Updated = f.notes[i].Created, msTime(f.now().UnixMilli())
	f.notes[i] = *a
	return nil
}

func (f *Fake) DeleteAnnotation(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := len(f.notes)
	f.notes = slices.DeleteFunc(f.notes, func(x model.Annotation) bool { return x.ID == id })
	if len(f.notes) == n {
		return fmt.Errorf("%w: annotation %d", ErrNotFound, id)
	}
	return nil
}

func (f *Fake) Annotations(_ context.Context, from, to time.Time, publicOnly bool) ([]model.Annotation, error) {
	fromMs, toMs := msRange(from, to)
	f.mu.RLock()
	defer f.mu.RUnlock()
	out := []model.Annotation{}
	for _, a := range f.notes {
		if a.At.UnixMilli() <= toMs && noteEnd(a).UnixMilli() >= fromMs && (!publicOnly || a.Public) {
			out = append(out, a)
		}
	}
	slices.SortStableFunc(out, func(x, y model.Annotation) int {
		if c := x.At.Compare(y.At); c != 0 {
			return c
		}
		return int(x.ID - y.ID)
	})
	return out, nil
}

func (f *Fake) SaveReport(_ context.Context, meta model.ReportMeta, html []byte) error {
	if err := normReport(&meta, html, f.now()); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.reports == nil {
		f.reports = map[string]fakeReport{}
	}
	seq := int64(len(f.reports)) + 1
	for _, r := range f.reports {
		seq = max(seq, r.seq+1)
	}
	if old, ok := f.reports[meta.ID]; ok {
		seq = old.seq
	}
	f.reports[meta.ID] = fakeReport{meta, slices.Clone(html), seq}
	for _, m := range f.reportsLocked()[min(len(f.reports), MaxReports):] {
		delete(f.reports, m.meta.ID)
	}
	return nil
}

// reportsLocked returns the reports newest first.
func (f *Fake) reportsLocked() []fakeReport {
	out := make([]fakeReport, 0, len(f.reports))
	for _, r := range f.reports {
		out = append(out, r)
	}
	slices.SortFunc(out, func(x, y fakeReport) int {
		if c := y.meta.Created.Compare(x.meta.Created); c != 0 {
			return c
		}
		return int(y.seq - x.seq)
	})
	return out
}

func (f *Fake) Report(_ context.Context, id string) (model.ReportMeta, []byte, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	r, ok := f.reports[id]
	if !ok {
		return model.ReportMeta{}, nil, fmt.Errorf("%w: report %q", ErrNotFound, id)
	}
	return r.meta, slices.Clone(r.html), nil
}

func (f *Fake) Reports(context.Context) ([]model.ReportMeta, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	out := []model.ReportMeta{}
	for _, r := range f.reportsLocked() {
		out = append(out, r.meta)
	}
	return out, nil
}

func (f *Fake) SetReportPublic(_ context.Context, id string, public bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.reports[id]
	if !ok {
		return fmt.Errorf("%w: report %q", ErrNotFound, id)
	}
	r.meta.Public = public
	f.reports[id] = r
	return nil
}

func (f *Fake) DeleteReport(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.reports[id]; !ok {
		return fmt.Errorf("%w: report %q", ErrNotFound, id)
	}
	delete(f.reports, id)
	return nil
}

// Baselines summarizes the kept samples per hour, as the real store's
// hourly summaries, and combines them the same way.
func (f *Fake) Baselines(_ context.Context, keys []model.SeriesKey, at time.Time, window time.Duration, hourOfDay bool) (map[model.SeriesKey]model.Baseline, error) {
	b := newBaselineSet(keys, at, window, hourOfDay)
	f.mu.RLock()
	defer f.mu.RUnlock()
	for k := range b.accs {
		hours := map[int64][]model.Sample{}
		for _, x := range f.data[k] {
			if !x.Slot.Before(b.from) && x.Slot.Before(b.to) {
				h := floorHour(x.Slot.UnixMilli())
				hours[h] = append(hours[h], x)
			}
		}
		for h, ss := range hours {
			slots := make([]blob.Slot, 0, len(ss))
			for _, x := range ss {
				slots = append(slots, toSlot(x))
			}
			b.add(k, h, summarize(slots))
		}
	}
	return b.result(), nil
}

var (
	_ AnnotationStore = (*Fake)(nil)
	_ ReportStore     = (*Fake)(nil)
	_ BaselineReader  = (*Fake)(nil)
)
