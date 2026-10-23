package s3test

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

// EstimateRound is about how long a round of n targets (keyed of them with
// a bucket and keys) takes: the cold checks a second apart, the warm ones,
// and the transfers at the recent rates (50 Mbit/s when unknown).
func EstimateRound(n, keyed int, p Phases, recent [2]float64) time.Duration {
	if n == 0 {
		return 0
	}
	const perCheck = 300 * time.Millisecond
	d := time.Duration(p.Cold) * max(time.Second, time.Duration(n)*perCheck)
	d += time.Duration(n) * time.Duration(p.Warm) * time.Second
	rate := func(bps float64) float64 {
		if bps <= 0 {
			return 50e6
		}
		return bps
	}
	for range keyed {
		if p.Down > 0 {
			d += 2*time.Second + time.Duration(float64(p.Down*8)/rate(recent[0])*float64(time.Second))
		}
		if p.Up > 0 {
			d += 3*time.Second + time.Duration(float64(p.Up*8)/rate(recent[1])*float64(time.Second))
		}
	}
	return d
}

// DataPerRound is the payload a round moves down and up (the checks'
// small objects included; seeding, once per campaign, is not).
func DataPerRound(keyed int, p Phases) (down, up int64) {
	k := int64(keyed)
	return k * (p.Down + int64(p.Cold+p.Warm)*seedSmall), k * p.Up
}

// CheckResult is a target's check: one fresh connection with a small
// request, and, with keys and a bucket, whether the keys work.
type CheckResult struct {
	Attempt model.StorageAttempt
	Keys    string // "work", "refused" or "none"
	// Hint names a likely fix of a config failure: "region" (the region
	// is part of the signature), "path_style" (a bucket name with dots
	// breaks the TLS certificate of virtual-host addressing), "endpoint"
	// (a redirect: another endpoint or region), "clock" (this machine's
	// clock), "ca" (an unknown certificate authority).
	Hint string
}

// Check tests one target once: signed HEAD of the bucket with keys,
// unsigned otherwise.
func (e *Engine) Check(ctx context.Context, rt RoundTarget) CheckResult {
	var out CheckResult
	a := &out.Attempt
	a.Target, a.Phase = rt.ID, model.StoragePhaseCold
	if rt.Validate(rt.Creds != nil && rt.Creds.HasToken()) != nil {
		a.At, a.Class, out.Keys = e.o.Now(), string(ClassHTTP4xx), "none"
		return out // the API refuses such targets: this is a backstop
	}
	a.Op = "head"
	out.Keys = "none"
	if e.o.Fake != nil {
		if rt.keyed() {
			a.Op = "list"
		}
		*a = e.o.Fake(ctx, rt.Target, model.StoragePhaseCold, a.Op, rt.keyed())
		a.Target, a.Phase = rt.ID, model.StoragePhaseCold
	} else if rt.keyed() {
		// A listing of fyisp's own prefix: its error body names a refusal
		// (a HEAD's can't), and it needs no object.
		_, *a = e.list(ctx, &rt, "fyisp-test/")
		a.Target, a.Phase = rt.ID, model.StoragePhaseCold
	} else {
		e.do(ctx, &rt.Target, request{method: "HEAD", root: rt.Bucket == ""}, nil, false, a)
	}
	if rt.keyed() {
		switch Class(a.Class) {
		case ClassOK:
			out.Keys = "work"
		case ClassHTTPAuth, ClassClockSkew:
			out.Keys = "refused"
		}
	}
	switch Class(a.Class) {
	case ClassHTTPAuth:
		if a.S3Code == "AuthorizationHeaderMalformed" {
			out.Hint = "region"
		}
	case ClassClockSkew:
		out.Hint = "clock"
	case ClassHTTPRedirect:
		out.Hint = "endpoint"
	case ClassTLSCert:
		out.Hint = "ca"
		if !rt.PathStyle && strings.Contains(rt.Bucket, ".") {
			out.Hint = "path_style"
		}
	}
	return out
}

// Sweep deletes what fyisp left under the install's prefix (uploads a
// crash or a failed delete left behind), keeping the seeds when
// keepSeeds. One sweep lists one page (up to 1000 objects); class is the
// first failure ("ok" when none).
func (e *Engine) Sweep(ctx context.Context, rt RoundTarget, install string, keepSeeds bool) (deleted int, class Class) {
	class = ClassOK
	if !rt.keyed() || !installRe.MatchString(install) || rt.Validate(rt.Creds.HasToken()) != nil {
		return 0, class
	}
	prefix := installPrefix(install)
	have, a := e.list(ctx, &rt, prefix)
	if a.Class != string(ClassOK) {
		return 0, Class(a.Class)
	}
	keys := make([]string, 0, len(have))
	for k := range have {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if ctx.Err() != nil {
			break
		}
		if !strings.HasPrefix(k, prefix) || strings.Contains(k[len(prefix):], "..") {
			continue // never anything outside fyisp's own prefix
		}
		if keepSeeds && strings.HasPrefix(k, prefix+"seed/") {
			continue
		}
		var d model.StorageAttempt
		e.do(ctx, &rt.Target, request{method: "DELETE", key: k, creds: rt.Creds}, nil, false, &d)
		if d.Class == string(ClassOK) {
			deleted++
		} else if class == ClassOK {
			class = Class(d.Class)
		}
	}
	return deleted, class
}
