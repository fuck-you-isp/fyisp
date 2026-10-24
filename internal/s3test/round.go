package s3test

import (
	"context"
	"encoding/xml"
	"io"
	"net/url"
	"strconv"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

// Phases are a round's sizes: cold and warm checks per target, and the
// download and upload sizes (0: none) for targets with a bucket and keys.
type Phases struct {
	Cold, Warm int
	Down, Up   int64
}

// Bounds of a round's phases (the API checks campaigns against them).
// capSlack is what a round's cap allows beyond its estimate: the worst
// case of one attempt's step bounds.
const (
	capSlack    = 2 * time.Minute
	MaxCold     = 20
	MaxWarm     = 10
	MaxTransfer = 1 << 30
	MaxTargets  = 8 // per round
	RoundCap    = 10 * time.Minute
	seedSmall   = 1 << 10 // the cold and warm checks' object
)

// RoundTarget is a target with its keys for this round (nil: unsigned,
// connection checks only).
type RoundTarget struct {
	Target
	Creds *Creds
}

func (rt *RoundTarget) keyed() bool { return rt.Creds != nil && rt.Bucket != "" }

// RoundSpec is one round: where its objects go (the install's prefix, the
// campaign and round numbers), its targets and phases, and the line's
// recent iperf3 medians (bits/s, 0: unknown) for the transfers' bounds.
type RoundSpec struct {
	Install   string
	Campaign  int64
	Round     int
	Targets   []RoundTarget
	Phases    Phases
	RecentBps [2]float64 // download, upload
	// OnPhase, when set, is told each phase as it starts, with its target
	// ("" for the cold phase, which interleaves them).
	OnPhase func(phase, target string)
}

func (s *RoundSpec) phase(p, target string) {
	if s.OnPhase != nil {
		s.OnPhase(p, target)
	}
}

// RoundResult is what a round did.
type RoundResult struct {
	Start, End time.Time
	Attempts   []model.StorageAttempt
	// Skips is the targets that left the round, and why: "auth",
	// "clock_skew", "config" (the keys, the clock or the settings), or
	// "round_cap" (the round ran out of time).
	Skips     map[string]string
	Down, Up  int64    // payload bytes moved
	Leftovers []string // uploaded objects not deleted (keys under the install's prefix)
	Cancelled bool
}

// Key prefixes: everything fyisp writes is under fyisp-test/<install>/.
func installPrefix(install string) string { return "fyisp-test/" + install + "/" }
func seedKey(install string, n int64) string {
	return installPrefix(install) + "seed/" + strconv.FormatInt(n, 10)
}

// RunRound runs one round. It never retries: every attempt is recorded,
// failures included. A target whose keys, clock or settings are refused
// leaves the round; the others go on.
func (e *Engine) RunRound(ctx context.Context, s RoundSpec) RoundResult {
	if e.o.Fake != nil {
		return e.fakeRound(ctx, s)
	}
	res := RoundResult{Start: e.o.Now(), Skips: map[string]string{}}
	for i := range s.Targets {
		rt := &s.Targets[i]
		if !installRe.MatchString(s.Install) || rt.Validate(rt.Creds != nil && rt.Creds.HasToken()) != nil {
			res.Skips[rt.ID] = "config"
		}
	}
	add := func(rt *RoundTarget, a model.StorageAttempt) model.StorageAttempt {
		if a.Class == "" {
			return a // cancelled: says nothing about the endpoint
		}
		a.Target = rt.ID
		res.Attempts = append(res.Attempts, a)
		if rt.Creds != nil {
			if why := skipReason(Class(a.Class)); why != "" && res.Skips[rt.ID] == "" {
				res.Skips[rt.ID] = why
			}
		}
		return a
	}
	targets := make([]*RoundTarget, len(s.Targets))
	for i := range s.Targets {
		targets[i] = &s.Targets[i]
	}

	// Seed: the objects the checks and the download read, put once, each
	// within its own bound; the round's starts after them.
	for _, rt := range targets {
		if rt.keyed() && res.Skips[rt.ID] == "" && ctx.Err() == nil {
			s.phase(model.StoragePhaseSeed, rt.ID)
			n := len(res.Attempts)
			e.seed(ctx, rt, s.Install, s.Phases.Down, s.RecentBps[1], add)
			for _, a := range res.Attempts[n:] {
				if a.Op == "put" {
					res.Up += a.Bytes // a seed's upload costs data too
				}
			}
		}
	}
	est := EstimateRound(len(s.Targets), countKeyed(s.Targets), s.Phases, s.RecentBps)
	rctx, cancel := context.WithTimeout(ctx, min(max(2*est, est+capSlack), RoundCap))
	defer cancel()
	live := func(rt *RoundTarget) bool { return res.Skips[rt.ID] == "" && rctx.Err() == nil }
	// Cold: a fresh lookup, connection and handshake per request, the
	// targets interleaved, each at most once a second.
	if s.Phases.Cold > 0 {
		s.phase(model.StoragePhaseCold, "")
	}
	for i := 0; i < s.Phases.Cold && rctx.Err() == nil; i++ {
		start := e.o.Now()
		for _, rt := range targets {
			if !live(rt) {
				continue
			}
			var a model.StorageAttempt
			a.Phase, a.N = model.StoragePhaseCold, i
			e.check(rctx, rt, s.Install, nil, false, &a)
			add(rt, a)
		}
		if i < s.Phases.Cold-1 {
			if wait := time.Second - e.o.Now().Sub(start); wait > 0 && !e.o.Sleep(rctx, wait) {
				break
			}
		}
	}
	finished := map[string]bool{} // went through every phase it had
	// Warm: requests on one kept connection, a second apart. Once it is
	// gone the phase ends: never redial (that hides the failure).
	for _, rt := range targets {
		var c *conn
		if s.Phases.Warm > 0 && live(rt) {
			s.phase(model.StoragePhaseWarm, rt.ID)
		}
		for i := 0; i < s.Phases.Warm && live(rt); i++ {
			if i > 0 && !e.o.Sleep(rctx, time.Second) {
				break
			}
			var a model.StorageAttempt
			a.Phase, a.N = model.StoragePhaseWarm, i
			if i > 0 && c == nil { // the server closed it after an answer
				a.At, a.Op, a.Class = e.o.Now(), "head", string(ClassConnClosed)
				if rt.keyed() {
					a.Op = "get"
				}
				add(rt, a)
				break
			}
			c = e.check(rctx, rt, s.Install, c, true, &a)
			add(rt, a)
			if c == nil && a.Class != string(ClassOK) {
				break
			}
		}
		if c != nil {
			c.close(false)
		}
		if !rt.keyed() && rctx.Err() == nil {
			finished[rt.ID] = true
		}
	}
	// Transfers, one target at a time, each on a fresh connection.
	for _, rt := range targets {
		if !rt.keyed() {
			continue
		}
		if s.Phases.Down > 0 && live(rt) {
			s.phase(model.StoragePhaseDown, rt.ID)
			a := e.download(rctx, rt, s.Install, s.Phases.Down, s.RecentBps[0])
			res.Down += a.Bytes
			add(rt, a)
		}
		if s.Phases.Up > 0 && live(rt) {
			s.phase(model.StoragePhaseUp, rt.ID)
			key := installPrefix(s.Install) + strconv.FormatInt(s.Campaign, 10) + "/" + strconv.Itoa(s.Round) + "-" + rt.ID
			up, del := e.upload(rctx, rt, key, s.Phases.Up, s.RecentBps[1])
			res.Up += up.Bytes // what was sent, even when it failed
			add(rt, up)
			add(rt, del)
			if del.Class != string(ClassOK) {
				res.Leftovers = append(res.Leftovers, key[len(installPrefix(s.Install)):])
			}
		}
		if rctx.Err() == nil {
			finished[rt.ID] = true
		}
	}
	for _, rt := range targets {
		if res.Skips[rt.ID] == "" && !finished[rt.ID] && ctx.Err() == nil {
			res.Skips[rt.ID] = "round_cap"
		}
	}
	res.Cancelled = ctx.Err() != nil
	res.End = e.o.Now()
	return res
}

func countKeyed(ts []RoundTarget) int {
	n := 0
	for i := range ts {
		if ts[i].keyed() {
			n++
		}
	}
	return n
}

// skipReason is why a keyed target leaves the round after class c ("" to
// go on).
func skipReason(c Class) string {
	switch c {
	case ClassHTTPAuth:
		return "auth"
	case ClassClockSkew:
		return "clock_skew"
	case ClassTLSCert, ClassHTTPRedirect, ClassAddrRefused:
		return "config"
	}
	return ""
}

// check is one small request: a signed GET of the small seed with keys
// and a bucket, else an unsigned HEAD of the bucket (or of the service
// root without one), where any answer means the endpoint is up. It
// returns the connection when keep and it is still usable.
func (e *Engine) check(ctx context.Context, rt *RoundTarget, install string, c *conn, keep bool, a *model.StorageAttempt) *conn {
	r := request{method: "HEAD", root: rt.Bucket == ""}
	a.Op = "head"
	if rt.keyed() {
		r = request{method: "GET", key: seedKey(install, seedSmall), creds: rt.Creds, want: seedSmall}
		a.Op = "get"
	}
	c, _ = e.do(ctx, &rt.Target, r, c, keep, a)
	return c
}

// seed puts the objects the checks (and the download, of n bytes) read,
// when they're missing or the wrong length. One listing finds them: a GET
// whose error body names a refusal, which a HEAD's can't. The big one is a
// transfer: masked.
func (e *Engine) seed(ctx context.Context, rt *RoundTarget, install string, n int64, recentUp float64, add func(*RoundTarget, model.StorageAttempt) model.StorageAttempt) {
	have, list := e.list(ctx, rt, installPrefix(install)+"seed/")
	if list.Class != string(ClassOK) {
		add(rt, list) // refused keys, a network failure: recorded, and no put
		return
	}
	for _, size := range []int64{seedSmall, n} {
		key := seedKey(install, size)
		if size == 0 || have[key] == size || ctx.Err() != nil {
			continue
		}
		big := size > seedSmall
		if big && e.o.Masker != nil {
			e.o.Masker.Begin(e.o.Now())
		}
		var put model.StorageAttempt
		put.Phase, put.Op = model.StoragePhaseSeed, "put"
		r := e.putRequest(rt, key, size, recentUp)
		r.transfer = big
		if big {
			r.total = seedBound(size, recentUp)
		}
		e.do(ctx, &rt.Target, r, nil, false, &put)
		if big && e.o.Masker != nil {
			_ = e.o.Masker.End(e.o.Now())
		}
		add(rt, put)
		if put.Class != string(ClassOK) {
			return
		}
	}
}

// list lists the keys under prefix (at most 1000) and their sizes.
func (e *Engine) list(ctx context.Context, rt *RoundTarget, prefix string) (map[string]int64, model.StorageAttempt) {
	var a model.StorageAttempt
	a.Phase, a.Op = model.StoragePhaseSeed, "list"
	_, res := e.do(ctx, &rt.Target, request{method: "GET", creds: rt.Creds, keep: 1 << 20,
		query: url.Values{"list-type": {"2"}, "prefix": {prefix}, "max-keys": {"1000"}}}, nil, false, &a)
	out := map[string]int64{}
	var l struct {
		Contents []struct {
			Key  string
			Size int64
		}
	}
	if a.Class == string(ClassOK) && xml.Unmarshal(res.body, &l) == nil {
		for _, o := range l.Contents {
			out[o.Key] = o.Size
		}
	}
	return out, a
}

// putRequest uploads key's payload of n bytes: unsigned payload over TLS,
// hashed (read once before) over plain http.
func (e *Engine) putRequest(rt *RoundTarget, key string, n int64, recent float64) request {
	hash := UnsignedPayload
	if !rt.tls() {
		hash = payloadHash(key, n)
	}
	return request{method: "PUT", key: key, creds: rt.Creds, size: n, hash: hash,
		body: func() io.Reader { return newPayload(key, n) }, upload: true, total: transferBound(n, recent)}
}

// download reads the big seed's first n bytes (a ranged GET), masked.
func (e *Engine) download(ctx context.Context, rt *RoundTarget, install string, n int64, recent float64) model.StorageAttempt {
	var a model.StorageAttempt
	a.Phase, a.Op = model.StoragePhaseDown, "get"
	if e.o.Masker != nil {
		e.o.Masker.Begin(e.o.Now())
		defer func() { _ = e.o.Masker.End(e.o.Now()) }()
	}
	e.do(ctx, &rt.Target, request{method: "GET", key: seedKey(install, n), creds: rt.Creds, want: n, transfer: true,
		header: map[string]string{"range": "bytes=0-" + strconv.FormatInt(n-1, 10)}, total: transferBound(n, recent)}, nil, false, &a)
	return a
}

// upload puts key (n bytes), masked, then deletes it on a new connection.
func (e *Engine) upload(ctx context.Context, rt *RoundTarget, key string, n int64, recent float64) (up, del model.StorageAttempt) {
	up.Phase, up.Op = model.StoragePhaseUp, "put"
	if e.o.Masker != nil {
		e.o.Masker.Begin(e.o.Now())
	}
	r := e.putRequest(rt, key, n, recent)
	r.transfer = true
	e.do(ctx, &rt.Target, r, nil, false, &up)
	if e.o.Masker != nil {
		_ = e.o.Masker.End(e.o.Now())
	}
	// The delete runs even when the round was cut or cancelled: the object
	// would stay otherwise.
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), deleteTimeout)
	defer cancel()
	del.Phase, del.Op = model.StoragePhaseUp, "delete"
	e.do(dctx, &rt.Target, request{method: "DELETE", key: key, creds: rt.Creds}, nil, false, &del)
	return up, del
}

// deleteTimeout bounds an upload's delete.
const deleteTimeout = 30 * time.Second

// transferBound is a transfer's time bound: 3x its time at the recent
// rate, within 30 s to 3 min (2 min when the rate is unknown).
func transferBound(n int64, bps float64) time.Duration {
	if bps <= 0 {
		return 2 * time.Minute
	}
	d := time.Duration(3 * float64(n*8) / bps * float64(time.Second))
	return min(max(d, minTransferTime), maxTransferTime)
}

// seedBound is the download seed's: 3x its time at the recent upload
// rate, within 30 s to 10 min (10 min when the rate is unknown).
func seedBound(n int64, bps float64) time.Duration {
	if bps <= 0 {
		return maxSeedTime
	}
	d := time.Duration(3 * float64(n*8) / bps * float64(time.Second))
	return min(max(d, minTransferTime), maxSeedTime)
}

// SeedFits reports whether a download of n bytes can be seeded at the
// recent upload rate (bits/s; 0: unknown, then it may), in half the bound.
func SeedFits(n int64, upBps float64) bool {
	return upBps <= 0 || float64(n*8)/upBps*float64(time.Second) <= float64(maxSeedTime/2)
}
