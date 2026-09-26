package web

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

// VerdictSource is what the verdict banner and the outage log read. The
// verdict engine (internal/verdict.Engine) implements it: wire it as
// Deps.Verdict = engine. Nil hides the banner: /api/verdict answers
// {"kind":"unknown"} and /api/incidents [].
//
// Contract: Verdict.Summary and Incident.Summary are plain-English sentences
// free of IP addresses and hostnames (the engine guarantees this; they are
// served on the public link as is). The public handler additionally drops
// target names that are not in the profile and evidence keys that are not
// plain identifiers, but it does not rewrite summaries.
type VerdictSource interface {
	Current() model.Verdict
	Incidents(ctx context.Context, from, to time.Time) ([]model.Incident, error)
}

// VerdictUnknown is the kind reported when there is no verdict source.
const VerdictUnknown model.VerdictKind = "unknown"

// MaxIncidents caps /api/incidents (newest first).
const MaxIncidents = 500

// incidentsTimeout bounds one incidents query.
const incidentsTimeout = 10 * time.Second

// evidenceKeyRE is what a public evidence key may look like: a lower-case
// identifier such as "gateway_loss". Anything else (an address, a hostname,
// a target name) is dropped on the public link.
var evidenceKeyRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// verdictJSON is /api/verdict. Since is omitted when unknown.
type verdictJSON struct {
	Kind     model.VerdictKind  `json:"kind"`
	Since    *time.Time         `json:"since,omitempty"`
	Summary  string             `json:"summary,omitempty"`
	Targets  []string           `json:"targets,omitempty"`
	Evidence map[string]float64 `json:"evidence,omitempty"`
	// Slow lists targets slower than their normal (Deps.Slow), worst first.
	Slow []slowJSON `json:"slow,omitempty"`
}

// incidentJSON is one /api/incidents item. End is omitted while ongoing.
type incidentJSON struct {
	ID       int64             `json:"id"`
	Start    time.Time         `json:"start"`
	End      *time.Time        `json:"end,omitempty"`
	Kind     model.VerdictKind `json:"kind"`
	Summary  string            `json:"summary"`
	Targets  []string          `json:"targets,omitempty"`
	PeakLoss float64           `json:"peak_loss"`
}

// targetFilter keeps names that exist in the profile (public only).
func targetFilter(p *model.Profile) func([]string) []string {
	known := map[string]bool{}
	if p != nil {
		for _, t := range p.Targets {
			known[t.Name] = true
		}
	}
	return func(in []string) []string {
		var out []string
		for _, n := range in {
			if known[n] {
				out = append(out, n)
			}
		}
		return out
	}
}

func (s *server) verdict() verdictJSON {
	if s.d.Verdict == nil {
		return verdictJSON{Kind: VerdictUnknown}
	}
	v := s.d.Verdict.Current()
	out := verdictJSON{Kind: v.Kind, Summary: v.Summary, Targets: v.Targets, Slow: s.slow()}
	if out.Kind == "" {
		out.Kind = VerdictUnknown
	}
	if !v.Since.IsZero() {
		t := v.Since.UTC()
		out.Since = &t
	}
	if s.public {
		out.Targets = targetFilter(s.d.Profile())(v.Targets)
	}
	// Copy the evidence: NaN and ±Inf do not marshal, and the public link
	// only carries keys that are plain identifiers.
	for k, x := range v.Evidence {
		if math.IsNaN(x) || math.IsInf(x, 0) || (s.public && !evidenceKeyRE.MatchString(k)) {
			continue
		}
		if out.Evidence == nil {
			out.Evidence = map[string]float64{}
		}
		out.Evidence[k] = x
	}
	return out
}

// incidentParams parses /api/incidents with /api/panel's time rules.
type incidentParams struct {
	from, to time.Time
	key      string
}

func parseIncidentParams(q url.Values, now time.Time) (incidentParams, error) {
	if err := checkParams(q, "from", "to"); err != nil {
		return incidentParams{}, err
	}
	return parseRange(q, now)
}

// parseRange parses from and to (already whitelisted) with /api/panel's
// time rules: defaults now-30m..now, to clamped to now, span to MaxRange.
func parseRange(q url.Values, now time.Time) (incidentParams, error) {
	var p incidentParams
	fs, ts := q.Get("from"), q.Get("to")
	if fs == "" {
		fs = "now-30m"
	}
	if ts == "" {
		ts = "now"
	}
	var fk, tk string
	var err error
	if p.from, fk, err = parseTime(fs, now); err != nil {
		return p, err
	}
	if p.to, tk, err = parseTime(ts, now); err != nil {
		return p, err
	}
	if p.to.After(now) {
		p.to, tk = now, "now"
	}
	if !p.from.Before(p.to) {
		return p, badReq("from must be before to")
	}
	if p.to.Sub(p.from) > MaxRange {
		p.from, fk = p.to.Add(-MaxRange), tk+"-90d"
	}
	p.key = fk + "\x00" + tk
	return p, nil
}

func (s *server) incidents(ctx context.Context, p incidentParams) ([]incidentJSON, error) {
	out := []incidentJSON{}
	if s.d.Verdict == nil {
		return out, nil
	}
	list, err := s.d.Verdict.Incidents(ctx, p.from, p.to)
	if err != nil {
		return nil, err
	}
	filter := targetFilter(s.d.Profile())
	for _, in := range list {
		if len(out) >= MaxIncidents {
			break
		}
		j := incidentJSON{ID: in.ID, Start: in.Start.UTC(), Kind: in.Kind, Summary: in.Summary, Targets: in.Targets, PeakLoss: in.PeakLoss}
		if !in.End.IsZero() {
			e := in.End.UTC()
			j.End = &e
		}
		if math.IsNaN(j.PeakLoss) || j.PeakLoss < 0 {
			j.PeakLoss = 0
		} else if j.PeakLoss > 1 {
			j.PeakLoss = 1
		}
		if s.public {
			j.Targets = filter(in.Targets)
		}
		out = append(out, j)
	}
	return out, nil
}

// serveVerdict answers /api/verdict. It is a cheap in-memory read (the UI
// polls it every 5s), so it is never cached.
func (s *server) serveVerdict(w http.ResponseWriter, r *http.Request) {
	if err := checkParams(r.URL.Query()); err != nil {
		writeErr(w, r, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, r, http.StatusOK, s.verdict())
}

// serveIncidents answers /api/incidents?from=&to=, newest first, at most
// MaxIncidents items.
func (s *server) serveIncidents(w http.ResponseWriter, r *http.Request) {
	p, err := parseIncidentParams(r.URL.Query(), s.now())
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, err.Error())
		return
	}
	render := func(ctx context.Context) (*cached, error) {
		if s.sem != nil {
			select {
			case s.sem <- struct{}{}:
				defer func() { <-s.sem }()
			case <-ctx.Done():
				return nil, errBusy
			}
		}
		list, err := s.incidents(ctx, p)
		if err != nil {
			return nil, err
		}
		b, err := json.Marshal(list)
		if err != nil {
			return nil, err
		}
		return &cached{code: http.StatusOK, ctype: "application/json; charset=utf-8", body: append(b, '\n')}, nil
	}
	ctx, cancel := context.WithTimeout(r.Context(), incidentsTimeout)
	defer cancel()
	var c *cached
	if s.cache != nil {
		c, err = s.cache.get(ctx, "inc\x00"+p.key, render)
	} else {
		c, err = render(ctx)
	}
	if err != nil {
		if errors.Is(err, errBusy) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			w.Header().Set("Retry-After", "2")
			writeErr(w, r, http.StatusServiceUnavailable, "busy, retry shortly")
			return
		}
		s.d.log().Error("incidents query", "err", err)
		msg := "query failed"
		if !s.public {
			msg += ": " + strings.TrimSpace(err.Error())
		}
		writeErr(w, r, http.StatusInternalServerError, msg)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeBody(w, r, c.code, c.ctype, c.body)
}
