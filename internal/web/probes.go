package web

import (
	"cmp"
	"container/heap"
	"context"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/store"
)

// Probe list limits.
const (
	MaxProbes = 5000 // probes and lost probes per response
	maxOverMs = 60000
)

// A TCP connect of 1000 to 1400 ms is most likely a lost SYN or SYN-ACK
// that the OS sent again after its 1 s initial retransmission timeout, plus
// the path's RTT (up to 400 ms). A second resend would land at 3 s, which is
// the probe timeout: that probe is lost.
const (
	likelyDropMinMs = 1000
	likelyDropMaxMs = 1400
)

type probesParams struct {
	target string
	kind   model.ProbeKind
	from   time.Time
	to     time.Time
	overMs float64
	limit  int
}

type probeJSON struct {
	TS int64   `json:"ts"`
	Ms float64 `json:"ms"`
}

type lostJSON struct {
	TS     int64  `json:"ts"`
	Reason string `json:"reason"`
}

// probesJSON answers /api/probes: one series' probes slower than over_ms.
type probesJSON struct {
	Target string  `json:"target"`
	Kind   string  `json:"kind"`
	From   int64   `json:"from"`
	To     int64   `json:"to"`
	OverMs float64 `json:"over_ms"`
	N      int     `json:"n"`    // answered probes
	Over   int     `json:"over"` // of them, slower than over_ms
	// CompleteAboveMs is set when probes holds fewer than over: every probe
	// slower than it is listed.
	CompleteAboveMs *float64       `json:"complete_above_ms"`
	Probes          []probeJSON    `json:"probes"` // the slowest, in time order
	LikelyDrop      *int           `json:"likely_drop,omitempty"`
	LostTotal       int            `json:"lost_total"`
	LostBy          map[string]int `json:"lost_by"`
	Lost            []lostJSON     `json:"lost"` // the first MaxProbes
	LostCapped      bool           `json:"lost_capped"`
}

func parseProbesParams(q url.Values, now time.Time, prof *model.Profile) (probesParams, error) {
	var p probesParams
	if err := checkParams(q, "target", "kind", "from", "to", "over_ms", "limit"); err != nil {
		return p, err
	}
	p.target = q.Get("target")
	if p.target == "" {
		return p, badReq("target is required")
	}
	ks := q.Get("kind")
	if ks == "" {
		return p, badReq("kind is required")
	}
	rg, err := parseRange(q, now)
	if err != nil {
		return p, err
	}
	p.from, p.to = rg.from, rg.to
	ov := q.Get("over_ms")
	if ov == "" {
		return p, badReq("over_ms is required")
	}
	p.overMs, err = strconv.ParseFloat(ov, 64)
	if err != nil || math.IsNaN(p.overMs) || p.overMs < 0 || p.overMs > maxOverMs {
		return p, badReq("bad over_ms %q: want 0 to %d", ov, maxOverMs)
	}
	p.limit = MaxProbes
	if s := q.Get("limit"); s != "" {
		if p.limit, err = strconv.Atoi(s); err != nil || p.limit < 0 || p.limit > MaxProbes {
			return p, badReq("bad limit %q: want 0 to %d", s, MaxProbes)
		}
	}
	// Unknown targets and kinds are 404, after the 400s.
	if prof != nil {
		for _, t := range prof.Targets {
			if t.Name != p.target {
				continue
			}
			for _, k := range t.ProbeKinds() {
				if k.String() == ks {
					p.kind = k
					return p, nil
				}
			}
		}
	}
	return p, errUnknownTarget
}

func (s *server) serveProbes(w http.ResponseWriter, r *http.Request) {
	p, err := parseProbesParams(r.URL.Query(), s.now(), s.d.Profile())
	switch {
	case err == errUnknownTarget:
		writeErr(w, r, http.StatusNotFound, "unknown target or kind")
		return
	case err != nil:
		writeErr(w, r, http.StatusBadRequest, err.Error())
		return
	}
	s.runCached(w, r, "probes", func(ctx context.Context) (*cached, error) {
		pj, err := queryProbes(ctx, s.d.Store, p)
		if err != nil {
			return nil, err
		}
		return jsonCached(pj)
	})
}

// queryProbes reads the series' raw probes once: counts, the limit slowest
// over over_ms and the first MaxProbes lost ones.
func queryProbes(ctx context.Context, st store.Reader, p probesParams) (*probesJSON, error) {
	pj := &probesJSON{Target: p.target, Kind: p.kind.String(), From: p.from.UnixMilli(), To: p.to.UnixMilli(),
		OverMs: p.overMs, LostBy: map[string]int{}, Probes: []probeJSON{}, Lost: []lostJSON{}}
	var drops int
	var top slowest
	maxOver := 0.0
	key := model.SeriesKey{Target: p.target, Kind: p.kind}
	err := st.Raw(ctx, []model.SeriesKey{key}, p.from, p.to, func(rp store.RawPoint) error {
		switch {
		case rp.Lost && rp.Reason == model.ReasonGap:
		case rp.Lost:
			pj.LostTotal++
			slug := reasonSlug(rp.Reason)
			pj.LostBy[slug]++
			if len(pj.Lost) < MaxProbes {
				pj.Lost = append(pj.Lost, lostJSON{rp.TS.UnixMilli(), slug})
			} else {
				pj.LostCapped = true
			}
		default:
			pj.N++
			if rp.RTTms >= likelyDropMinMs && rp.RTTms < likelyDropMaxMs {
				drops++
			}
			if rp.RTTms <= p.overMs {
				return nil
			}
			pj.Over++
			maxOver = max(maxOver, rp.RTTms)
			pr := probeJSON{rp.TS.UnixMilli(), rp.RTTms}
			switch {
			case len(top) < p.limit:
				heap.Push(&top, pr)
			case p.limit > 0 && pr.Ms > top[0].Ms:
				top[0] = pr
				heap.Fix(&top, 0)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if pj.Over > len(top) {
		c := maxOver
		if len(top) > 0 {
			c = top[0].Ms
		}
		pj.CompleteAboveMs = &c
	}
	pj.Probes = append(pj.Probes, top...)
	slices.SortFunc(pj.Probes, func(a, b probeJSON) int { return cmp.Compare(a.TS, b.TS) })
	if p.kind == model.KindTCP {
		pj.LikelyDrop = &drops
	}
	return pj, nil
}

// slowest is a min-heap on ms; on equal ms the later probe is evicted first.
type slowest []probeJSON

func (h slowest) Len() int { return len(h) }
func (h slowest) Less(i, j int) bool {
	if h[i].Ms != h[j].Ms {
		return h[i].Ms < h[j].Ms
	}
	return h[i].TS > h[j].TS
}
func (h slowest) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *slowest) Push(x any)   { *h = append(*h, x.(probeJSON)) }
func (h *slowest) Pop() any {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}
