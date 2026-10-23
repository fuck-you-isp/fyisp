package s3test

import (
	"context"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

// fakeRound is RunRound with Options.Fake answering each attempt: the
// same phases and order, a second between requests, no seeding.
func (e *Engine) fakeRound(ctx context.Context, s RoundSpec) RoundResult {
	res := RoundResult{Start: e.o.Now(), Skips: map[string]string{}}
	ask := func(rt *RoundTarget, phase, op string, n int) model.StorageAttempt {
		a := e.o.Fake(ctx, rt.Target, phase, op, rt.keyed())
		if ctx.Err() != nil {
			return model.StorageAttempt{}
		}
		a.Target, a.Phase, a.Op, a.N = rt.ID, phase, op, n
		if a.At.IsZero() {
			a.At = e.o.Now()
		}
		res.Attempts = append(res.Attempts, a)
		return a
	}
	pause := func() bool {
		if e.o.Sleep != nil {
			return e.o.Sleep(ctx, time.Second)
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(time.Second):
			return true
		}
	}
	op := func(rt *RoundTarget) string {
		if rt.keyed() {
			return "get"
		}
		return "head"
	}
	if s.Phases.Cold > 0 {
		s.phase(model.StoragePhaseCold, "")
	}
	for i := 0; i < s.Phases.Cold && ctx.Err() == nil; i++ {
		for j := range s.Targets {
			ask(&s.Targets[j], model.StoragePhaseCold, op(&s.Targets[j]), i)
		}
		if i < s.Phases.Cold-1 && !pause() {
			break
		}
	}
	for j := range s.Targets {
		rt := &s.Targets[j]
		if s.Phases.Warm > 0 && ctx.Err() == nil {
			s.phase(model.StoragePhaseWarm, rt.ID)
		}
		for i := 0; i < s.Phases.Warm && ctx.Err() == nil; i++ {
			if i > 0 && !pause() {
				break
			}
			ask(rt, model.StoragePhaseWarm, op(rt), i)
		}
	}
	for j := range s.Targets {
		rt := &s.Targets[j]
		if !rt.keyed() || ctx.Err() != nil {
			continue
		}
		if s.Phases.Down > 0 {
			s.phase(model.StoragePhaseDown, rt.ID)
			if a := ask(rt, model.StoragePhaseDown, "get", 0); a.Class == string(ClassOK) {
				res.Down += s.Phases.Down
				res.Attempts[len(res.Attempts)-1].Bytes = s.Phases.Down
			}
		}
		if s.Phases.Up > 0 && ctx.Err() == nil {
			s.phase(model.StoragePhaseUp, rt.ID)
			if a := ask(rt, model.StoragePhaseUp, "put", 0); a.Class == string(ClassOK) {
				res.Up += s.Phases.Up
				res.Attempts[len(res.Attempts)-1].Bytes = s.Phases.Up
			}
			ask(rt, model.StoragePhaseUp, "delete", 1)
		}
	}
	res.Cancelled = ctx.Err() != nil
	res.End = e.o.Now()
	return res
}
