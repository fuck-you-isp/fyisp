package storage

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/s3test"
)

// CheckTimeout bounds one check.
const CheckTimeout = 20 * time.Second

// Options configure a Service.
type Options struct {
	Dir    string // the data directory, which holds FileName
	Engine *s3test.Engine
	// Lookup resolves an endpoint's name when it is saved; nil: the
	// system resolver.
	Lookup func(ctx context.Context, network, host string) ([]netip.Addr, error)
	Now    func() time.Time
	// Busy reports a queued test (a speed test) running now: a check
	// still runs, and says its timings may be high.
	Busy func() bool
}

// Service keeps the targets file and runs checks. Its methods are safe
// for concurrent use.
type Service struct {
	o    Options
	path string

	mu     sync.Mutex
	f      *file
	err    error // the file can't be used: storage is off
	rev    int64
	checks map[string]Check
	mem    map[string]memKey         // target -> an on-demand campaign's keys
	inUse  func(target string) int64 // SetInUse

	checking sync.Mutex // one check at a time
}

// Check is a target's last check (kept in memory only).
type Check struct {
	At                              time.Time
	Class                           string
	Keys                            string // "work", "refused" or "none"
	Hint                            string
	HTTPStatus                      int
	S3Code                          string
	DNSms, Connectms, TLSms, TTFBms *float64
	DuringTest                      bool
}

// TargetState is a target with where its keys are and its last check.
type TargetState struct {
	Target
	Keys  KeyState
	Check *Check
}

// Errors.
var (
	ErrNotFound  = errors.New("no such target")
	ErrNameTaken = errors.New("another target has that name")
	ErrTooMany   = fmt.Errorf("at most %d targets", MaxTargets)
	ErrBusy      = errors.New("a check is running")
	ErrChanged   = errors.New("the target changed meanwhile: try again")
)

// Open reads the targets file in o.Dir. A file it can't use turns storage
// off (Err) and is left as it is. Saved keys whose target is gone or
// changed (a file edited by hand) are deleted.

func Open(o Options) *Service {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Lookup == nil {
		o.Lookup = net.DefaultResolver.LookupNetIP
	}
	// rev starts at the clock, so it doesn't repeat across restarts.
	s := &Service{o: o, path: filepath.Join(o.Dir, FileName), checks: map[string]Check{}, mem: map[string]memKey{}, rev: o.Now().UnixMilli()}
	s.f, s.err = loadFile(s.path)
	if s.err == nil {
		keys := slices.DeleteFunc(slices.Clone(s.f.Keys), func(k SavedKey) bool {
			i := s.index(k.Target)
			return i < 0 || s.f.Targets[i].binding() != k.Binding
		})
		if len(keys) != len(s.f.Keys) {
			_ = s.commitLocked(s.f.Targets, keys) // unused anyway (bound to other settings); the next save retries
		}
	}
	return s
}

// Err is why storage is off (an ErrFile), or nil.
func (s *Service) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// Rev increases with every change to the targets or their checks.
func (s *Service) Rev() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rev
}

func (s *Service) index(id string) int {
	return slices.IndexFunc(s.f.Targets, func(t Target) bool { return t.ID == id })
}

// Targets lists the targets in the order they were added.
func (s *Service) Targets() ([]TargetState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return nil, s.err
	}
	out := make([]TargetState, len(s.f.Targets))
	for i, t := range s.f.Targets {
		out[i] = s.stateLocked(t)
	}
	return out, nil
}

// Target is one target.
func (s *Service) Target(id string) (TargetState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return TargetState{}, s.err
	}
	i := s.index(id)
	if i < 0 {
		return TargetState{}, ErrNotFound
	}
	return s.stateLocked(s.f.Targets[i]), nil
}

func (s *Service) stateLocked(t Target) TargetState {
	ts := TargetState{Target: t, Keys: s.keyStateLocked(t)}
	if c, ok := s.checks[t.ID]; ok {
		ts.Check = &c
	}
	return ts
}

// commitLocked saves targets and keys as the file; on failure nothing
// changes.
func (s *Service) commitLocked(targets []Target, keys []SavedKey) error {
	next := &file{Version: fileVersion, Targets: targets, Keys: keys}
	if err := saveFile(s.path, next); err != nil {
		return err
	}
	s.f = next
	s.rev++
	return nil
}

func (s *Service) nameTakenLocked(name, except string) bool {
	return slices.ContainsFunc(s.f.Targets, func(t Target) bool { return t.ID != except && strings.EqualFold(t.Name, name) })
}

// Add saves a new target.
func (s *Service) Add(ctx context.Context, sp Spec) (TargetState, error) {
	if err := s.Err(); err != nil {
		return TargetState{}, err
	}
	if err := sp.normalize(); err != nil {
		return TargetState{}, err
	}
	nw, err := network(ctx, s.o.Lookup, sp.Endpoint)
	if err != nil {
		return TargetState{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case len(s.f.Targets) >= MaxTargets:
		return TargetState{}, ErrTooMany
	case s.nameTakenLocked(sp.Name, ""):
		return TargetState{}, ErrNameTaken
	}
	now := s.o.Now().UnixMilli()
	t := Target{ID: newID(), Name: sp.Name, Endpoint: sp.Endpoint, Region: sp.Region, Bucket: sp.Bucket, Addressing: sp.Addressing,
		CAPEM: sp.CAPEM, Network: nw, CreatedMs: now, UpdatedMs: now}
	for s.index(t.ID) >= 0 {
		t.ID = newID()
	}
	if err := s.commitLocked(append(slices.Clone(s.f.Targets), t), s.f.Keys); err != nil {
		return TargetState{}, err
	}
	return s.stateLocked(t), nil
}

// Update changes a target's settings. A change to its endpoint, region,
// bucket, addressing or CA deletes the keys campaigns hold for it
// (keysCleared) and drops its last check. A target another request changed
// while the new endpoint resolved is ErrChanged.
func (s *Service) Update(ctx context.Context, id string, p Patch) (ts TargetState, keysCleared bool, err error) {
	old, err := s.Target(id)
	if err != nil {
		return TargetState{}, false, err
	}
	sp := Spec{Name: old.Name, Endpoint: old.Endpoint, Region: old.Region, Bucket: old.Bucket, Addressing: old.Addressing, CAPEM: old.CAPEM}
	for _, f := range []struct{ dst, src *string }{{&sp.Name, p.Name}, {&sp.Endpoint, p.Endpoint}, {&sp.Region, p.Region},
		{&sp.Bucket, p.Bucket}, {&sp.Addressing, p.Addressing}, {&sp.CAPEM, p.CAPEM}} {
		if f.src != nil {
			*f.dst = *f.src
		}
	}
	if err := sp.normalize(); err != nil {
		return TargetState{}, false, err
	}
	nw := old.Network
	if sp.Endpoint != old.Endpoint {
		if nw, err = network(ctx, s.o.Lookup, sp.Endpoint); err != nil {
			return TargetState{}, false, err
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.index(id)
	switch {
	case i < 0 || s.f.Targets[i] != old.Target:
		return TargetState{}, false, ErrChanged
	case s.nameTakenLocked(sp.Name, id):
		return TargetState{}, false, ErrNameTaken
	}
	t := old.Target
	t.Name, t.Endpoint, t.Region, t.Bucket, t.Addressing, t.CAPEM, t.Network = sp.Name, sp.Endpoint, sp.Region, sp.Bucket, sp.Addressing, sp.CAPEM, nw
	if t == old.Target {
		return s.stateLocked(t), false, nil
	}
	t.UpdatedMs = s.o.Now().UnixMilli()
	targets := slices.Clone(s.f.Targets)
	targets[i] = t
	keys := s.f.Keys
	rebound := t.binding() != old.binding()
	if rebound {
		keys = slices.DeleteFunc(slices.Clone(keys), func(k SavedKey) bool { return k.Target == id })
		_, held := s.mem[id]
		keysCleared = held || len(keys) != len(s.f.Keys)
	}
	if err := s.commitLocked(targets, keys); err != nil {
		return TargetState{}, false, err
	}
	if rebound {
		if m, ok := s.mem[id]; ok {
			m.creds.Zero()
			delete(s.mem, id)
		}
		delete(s.checks, id)
	}
	return s.stateLocked(t), keysCleared, nil
}

// Delete removes a target, its keys and its last check; a live campaign's
// target is ErrInUse.
func (s *Service) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	i := s.index(id)
	if i < 0 {
		return ErrNotFound
	}
	if s.inUse != nil && s.inUse(id) != 0 {
		return ErrInUse
	}
	keys := slices.DeleteFunc(slices.Clone(s.f.Keys), func(k SavedKey) bool { return k.Target == id })
	if err := s.commitLocked(slices.Delete(slices.Clone(s.f.Targets), i, i+1), keys); err != nil {
		return err
	}
	if m, ok := s.mem[id]; ok {
		m.creds.Zero()
		delete(s.mem, id)
	}
	delete(s.checks, id)
	return nil
}

// Check tests a target once, signed with creds (nil: the keys a campaign
// holds for it, else unsigned), and keeps the result as its last check. One check runs at a time (ErrBusy). A
// check whose ctx ends first returns ctx's error and keeps nothing; a
// session token for an http:// endpoint is a FieldError of "keys".
func (s *Service) Check(ctx context.Context, id string, creds *s3test.Creds) (Check, error) {
	if !s.checking.TryLock() {
		return Check{}, ErrBusy
	}
	defer s.checking.Unlock()
	ts, err := s.Target(id)
	if err != nil {
		return Check{}, err
	}
	et, err := ts.Engine()
	if err != nil {
		return Check{}, err
	}
	if creds == nil {
		if held, ok := s.Keys(id, 0); ok {
			creds = held
			defer held.Zero()
		}
	}
	if creds != nil && creds.HasToken() && et.Endpoint.Scheme != "https" {
		return Check{}, &FieldError{Field: "keys", Msg: "a session token is only sent over https", Code: "bad_request"}
	}
	busy := s.o.Busy != nil && s.o.Busy()
	cctx, cancel := context.WithTimeout(ctx, CheckTimeout)
	defer cancel()
	r := s.o.Engine.Check(cctx, s3test.RoundTarget{Target: et, Creds: creds})
	if err := ctx.Err(); err != nil {
		return Check{}, err
	}
	if r.Attempt.Class == "" { // cut off by CheckTimeout
		r.Attempt.Class = string(s3test.ClassTimeoutTotal)
	}
	c := checkOf(r, busy || (s.o.Busy != nil && s.o.Busy()))

	s.mu.Lock()
	defer s.mu.Unlock()
	if j := s.index(id); j >= 0 && s.f.Targets[j].binding() == ts.binding() {
		s.checks[id] = c
		s.rev++
	}
	return c, nil
}

func checkOf(r s3test.CheckResult, busy bool) Check {
	a := r.Attempt
	return Check{At: a.At, Class: a.Class, Keys: r.Keys, Hint: r.Hint, HTTPStatus: a.HTTPStatus, S3Code: a.S3Code,
		DNSms: a.DNSms, Connectms: a.Connectms, TLSms: a.TLSms, TTFBms: a.TTFBms, DuringTest: busy}
}
