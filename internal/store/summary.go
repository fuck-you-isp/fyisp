package store

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/store/blob"
)

const hourMs = int64(time.Hour / time.Millisecond)

// summary is one series' closed hour. RTTs are in blob units (10 µs).
type summary struct {
	n, lost       int64                   // successful / lost samples (not-measured slots excluded)
	by            [blob.MaxCode + 1]int64 // lost per reason; by[0] unused
	min, max, p95 int64                   // valid when n > 0
	sum           int64                   // sum of values: mean = sum/n, exact
	// p50 is the hour's median (valid when n > 0), or -1 when unknown: hours
	// written in day format v1 (fyisp v0.1/v0.2) have none. Use median().
	p50 int64
}

// median returns the hour's median RTT, falling back to the mean (rounded)
// for hours stored without one (day format v1). Only valid when n > 0.
func (s *summary) median() int64 {
	if s.p50 >= 0 {
		return s.p50
	}
	return (s.sum + s.n/2) / s.n
}

func summarize(slots []blob.Slot) *summary {
	s := &summary{}
	var vals []int64
	for _, x := range slots {
		switch {
		case x.Valid:
			vals = append(vals, x.Value)
			s.sum += x.Value
		case x.Code != 0:
			s.by[x.Code]++
			s.lost++
		}
	}
	s.n = int64(len(vals))
	if s.n > 0 {
		slices.Sort(vals)
		s.min, s.max = vals[0], vals[len(vals)-1]
		s.p95 = vals[(len(vals)*95+99)/100-1]
		s.p50 = vals[(len(vals)*50+99)/100-1] // the lower median, as p95
	} else {
		s.p50 = -1
	}
	return s
}

// hourSummary is a summary with its absolute hour (unix hours).
type hourSummary struct {
	hour int64
	s    summary
}

// Day blobs: summary_1h keeps one row per (UTC day, series) whose data packs
// that day's hourly summaries. A row per hour would be 194k rows for a
// 90-day, 90-series panel, and iterating rows through database/sql and
// modernc costs ~0.25-1 µs each: 200+ ms. Packed per day it is 8k rows.
//
// Format v1 (uncompressed; written by fyisp v0.1 and v0.2):
//
//	byte  version (1)
//	then per hour, in increasing hour order:
//	  byte     hour of day (0..23)
//	  uvarint  n, lost
//	  if lost > 0: 15 uvarints, lost per reason 1..15 (they sum to lost)
//	  if n > 0:    uvarint min, max-min, p95-min, sum
//
// Format v2 (fyisp v0.3, schema v4) is v1 with the version byte 2 and one
// more uvarint per hour with n > 0, after sum: p50-min+1, or 0 when the
// median is unknown (an hour first written in v1 and carried over when its
// day row is rewritten). Both formats are read; v2 is always written, so v1
// day rows turn into v2 as their days are rewritten (never for closed days,
// which is fine: readers fall back to the mean, see summary.median).
const (
	summaryV1 = 1
	summaryV2 = 2
)

var errSummary = errors.New("store: corrupt summary")

func appendDay(dst []byte, day int64, hs []hourSummary) []byte {
	dst = append(dst, summaryV2)
	for _, h := range hs {
		dst = append(dst, byte(h.hour-day*24))
		dst = binary.AppendUvarint(dst, uint64(h.s.n))
		dst = binary.AppendUvarint(dst, uint64(h.s.lost))
		if h.s.lost > 0 {
			for c := 1; c <= blob.MaxCode; c++ {
				dst = binary.AppendUvarint(dst, uint64(h.s.by[c]))
			}
		}
		if h.s.n > 0 {
			dst = binary.AppendUvarint(dst, uint64(h.s.min))
			dst = binary.AppendUvarint(dst, uint64(h.s.max-h.s.min))
			dst = binary.AppendUvarint(dst, uint64(h.s.p95-h.s.min))
			dst = binary.AppendUvarint(dst, uint64(h.s.sum))
			var p50 uint64 // unknown
			if h.s.p50 >= h.s.min && h.s.p50 <= h.s.max {
				p50 = uint64(h.s.p50-h.s.min) + 1
			}
			dst = binary.AppendUvarint(dst, p50)
		}
	}
	return dst
}

// decodeDay appends the hours in data to dst. It never panics.
func decodeDay(dst []hourSummary, day int64, data []byte) ([]hourSummary, error) {
	if len(data) == 0 || (data[0] != summaryV1 && data[0] != summaryV2) {
		return dst, fmt.Errorf("%w: version", errSummary)
	}
	v2 := data[0] == summaryV2
	p := data[1:]
	next := func() (int64, bool) {
		v, n := binary.Uvarint(p)
		if n <= 0 || v > 1<<50 {
			return 0, false
		}
		p = p[n:]
		return int64(v), true
	}
	prev := -1
	for len(p) > 0 {
		hod := int(p[0])
		p = p[1:]
		if hod > 23 || hod <= prev {
			return dst, fmt.Errorf("%w: hour %d after %d", errSummary, hod, prev)
		}
		prev = hod
		h := hourSummary{hour: day*24 + int64(hod), s: summary{p50: -1}}
		var ok1, ok2 bool
		h.s.n, ok1 = next()
		h.s.lost, ok2 = next()
		if !ok1 || !ok2 {
			return dst, fmt.Errorf("%w: counts", errSummary)
		}
		if h.s.lost > 0 {
			var total int64
			for c := 1; c <= blob.MaxCode; c++ {
				v, ok := next()
				if !ok {
					return dst, fmt.Errorf("%w: lost_by", errSummary)
				}
				h.s.by[c] = v
				total += v
			}
			if total != h.s.lost {
				return dst, fmt.Errorf("%w: lost_by sums to %d, not %d", errSummary, total, h.s.lost)
			}
		}
		if h.s.n > 0 {
			mn, ok1 := next()
			dmax, ok2 := next()
			dp95, ok3 := next()
			sum, ok4 := next()
			if !ok1 || !ok2 || !ok3 || !ok4 || dp95 > dmax {
				return dst, fmt.Errorf("%w: values", errSummary)
			}
			h.s.min, h.s.max, h.s.p95, h.s.sum = mn, mn+dmax, mn+dp95, sum
			if v2 {
				dp50, ok := next()
				if !ok || dp50 > dmax+1 {
					return dst, fmt.Errorf("%w: p50", errSummary)
				}
				if dp50 > 0 {
					h.s.p50 = mn + dp50 - 1
				}
			}
		}
		dst = append(dst, h)
	}
	return dst, nil
}

// mergeDay replaces or inserts the hours of add into hs (sorted by hour).
func mergeDay(hs []hourSummary, add ...hourSummary) []hourSummary {
	for _, a := range add {
		i, found := slices.BinarySearchFunc(hs, a.hour, func(h hourSummary, t int64) int { return int(h.hour - t) })
		if found {
			hs[i] = a
		} else {
			hs = slices.Insert(hs, i, a)
		}
	}
	return hs
}

// toSlot converts a sample to its stored form.
func toSlot(s model.Sample) blob.Slot {
	if s.Lost {
		return blob.Lost(uint8(min(s.Reason, model.MaxReason)))
	}
	v := (s.RTT + blob.Unit/2) / blob.Unit
	return blob.Val(max(0, min(int64(v), blob.MaxValue)))
}

// unitMs converts blob units to milliseconds (exact for RTTs that are
// multiples of 10 µs).
func unitMs(v int64) float64 { return float64(v*blob.Unit) / 1e6 }

var nan32 = float32(math.NaN())

func floorDiv(a, b int64) int64 {
	q := a / b
	if a%b != 0 && (a < 0) != (b < 0) {
		q--
	}
	return q
}
