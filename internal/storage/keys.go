package storage

import (
	"errors"
	"slices"

	"github.com/fuck-you-isp/fyisp/internal/s3test"
)

// Keys belong to a campaign, never to a target: an on-demand campaign's
// are held in memory (HoldKeys) until it ends, a recurring schedule's are
// saved in the targets file (SaveKeys) until it ends. Either way they are
// bound to the target's settings, and a change to those drops them.

// ErrInUse is a target a live campaign uses (it can't be deleted).
var ErrInUse = errors.New("a campaign uses this target")

// memKey is an on-demand campaign's keys for one target.
type memKey struct {
	campaign int64
	binding  string
	creds    *s3test.Creds
}

// KeyState is where a target's keys are, never the keys.
type KeyState struct {
	State    string // "none", "memory" or "saved"
	Last4    string // the access key's last 4 characters ("" when 4 or fewer)
	Token    bool
	Campaign int64 // the campaign holding them (0: none)
}

func last4(ak string) string {
	if len(ak) <= 4 {
		return ""
	}
	return ak[len(ak)-4:]
}

// keyStateLocked says where target t's keys are.
func (s *Service) keyStateLocked(t Target) KeyState {
	if m, ok := s.mem[t.ID]; ok && m.binding == t.binding() {
		return KeyState{State: "memory", Last4: m.creds.AccessKeyLast4(), Token: m.creds.HasToken(), Campaign: m.campaign}
	}
	if k, ok := s.savedLocked(t); ok {
		return KeyState{State: "saved", Last4: last4(k.AccessKey), Token: k.SessionToken != "", Campaign: k.Campaign}
	}
	return KeyState{State: "none"}
}

func (s *Service) savedLocked(t Target) (SavedKey, bool) {
	i := slices.IndexFunc(s.f.Keys, func(k SavedKey) bool { return k.Target == t.ID && k.Binding == t.binding() })
	if i < 0 {
		return SavedKey{}, false
	}
	return s.f.Keys[i], true
}

// SetInUse tells Delete which targets a live campaign uses (the campaign's
// id, 0: none).
func (s *Service) SetInUse(f func(target string) int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inUse = f
}

// HoldKeys holds an on-demand campaign's keys in memory, replacing what
// any campaign held for those targets. The service keeps its own copies.
func (s *Service) HoldKeys(campaign int64, keys map[string]*s3test.Creds) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	for id := range keys {
		if s.index(id) < 0 {
			return ErrNotFound
		}
	}
	for id, c := range keys {
		if old, ok := s.mem[id]; ok {
			old.creds.Zero()
		}
		s.mem[id] = memKey{campaign: campaign, binding: s.f.Targets[s.index(id)].binding(), creds: c.Clone()}
	}
	s.rev++
	return nil
}

// SaveKeys saves a recurring campaign's keys in the targets file,
// replacing what any campaign saved for those targets.
func (s *Service) SaveKeys(campaign int64, keys map[string]*s3test.Creds) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	saved := slices.DeleteFunc(slices.Clone(s.f.Keys), func(k SavedKey) bool { _, ok := keys[k.Target]; return ok })
	now := s.o.Now().UnixMilli()
	for id, c := range keys {
		i := s.index(id)
		if i < 0 {
			return ErrNotFound
		}
		ak, sec, tok := c.Material()
		saved = append(saved, SavedKey{Target: id, Campaign: campaign, Binding: s.f.Targets[i].binding(),
			AccessKey: ak, Secret: sec, SessionToken: tok, SavedMs: now})
	}
	return s.commitLocked(s.f.Targets, saved)
}

// DropKeys deletes a campaign's keys: those held in memory (zeroed) and
// those saved in the targets file.
func (s *Service) DropKeys(campaign int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	for id, m := range s.mem {
		if m.campaign == campaign {
			m.creds.Zero()
			delete(s.mem, id)
			s.rev++
		}
	}
	if !slices.ContainsFunc(s.f.Keys, func(k SavedKey) bool { return k.Campaign == campaign }) {
		return nil
	}
	return s.commitLocked(s.f.Targets, slices.DeleteFunc(slices.Clone(s.f.Keys), func(k SavedKey) bool { return k.Campaign == campaign }))
}

// Keys is a copy of the keys campaign holds for target (0: any
// campaign's, memory first); the caller zeroes it when done.
func (s *Service) Keys(target string, campaign int64) (*s3test.Creds, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return nil, false
	}
	i := s.index(target)
	if i < 0 {
		return nil, false
	}
	t := s.f.Targets[i]
	if m, ok := s.mem[target]; ok && m.binding == t.binding() && (campaign == 0 || m.campaign == campaign) {
		return m.creds.Clone(), true
	}
	if k, ok := s.savedLocked(t); ok && (campaign == 0 || k.Campaign == campaign) {
		return s3test.NewCreds(k.AccessKey, k.Secret, k.SessionToken), true
	}
	return nil, false
}

// HasKeys reports keys campaign holds for target (copying nothing).
func (s *Service) HasKeys(target string, campaign int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return false
	}
	i := s.index(target)
	if i < 0 {
		return false
	}
	t := s.f.Targets[i]
	if m, ok := s.mem[target]; ok && m.binding == t.binding() && m.campaign == campaign {
		return true
	}
	k, ok := s.savedLocked(t)
	return ok && k.Campaign == campaign
}

// KeyCampaigns lists the campaigns that hold keys (held or saved).
func (s *Service) KeyCampaigns() []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []int64
	for _, m := range s.mem {
		out = append(out, m.campaign)
	}
	if s.f != nil {
		for _, k := range s.f.Keys {
			out = append(out, k.Campaign)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// ForgetKeys deletes the keys any campaign holds for a target.
func (s *Service) ForgetKeys(id string) (TargetState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return TargetState{}, s.err
	}
	i := s.index(id)
	if i < 0 {
		return TargetState{}, ErrNotFound
	}
	if err := s.dropTargetKeysLocked(id); err != nil {
		return TargetState{}, err
	}
	return s.stateLocked(s.f.Targets[i]), nil
}

// dropTargetKeysLocked deletes a target's keys, held and saved.
func (s *Service) dropTargetKeysLocked(id string) error {
	if m, ok := s.mem[id]; ok {
		m.creds.Zero()
		delete(s.mem, id)
		s.rev++
	}
	if !slices.ContainsFunc(s.f.Keys, func(k SavedKey) bool { return k.Target == id }) {
		return nil
	}
	return s.commitLocked(s.f.Targets, slices.DeleteFunc(slices.Clone(s.f.Keys), func(k SavedKey) bool { return k.Target == id }))
}
