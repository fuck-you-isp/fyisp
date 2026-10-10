// Package profile loads target profiles: the embedded default (a selection
// of catalog targets, see default.yml), the named profiles over the catalog
// and local --config files (which may extend them).
//
// File format (YAML):
//
//	name: my-profile
//	version: "1"
//	extends: [default]          # optional: named profiles (see profiles.yml), e.g. [aws, europe]
//	groups:                     # new groups, or a changed title/order by id
//	  - {id: home, title: Home, order: 8}
//	add:                        # with extends: new targets
//	  - {name: router, host: 192.168.1.1, group: home, kinds: [icmp]}
//	remove: [Discord]           # with extends: drop targets by name
//	override:                   # with extends: change fields of a target by name
//	  - {name: Zoom, interval: 30s}
//	targets: [...]              # without extends: the full target list
//	path: false                 # optional: leave out the built-in "Network path" group
//
// Target fields: name, host, group, port (default 443), path (HTTPS only,
// default "/"), kinds (tcp|https|icmp|udp, default [tcp]: a TCP connect to
// port), interval (duration, default 15s; ICMP runs at interval/3),
// host_overrides (kind -> host), trace (true: an always-on traceroute to the
// target, every 5s; at most MaxTraced targets), tags (labels such as
// "app:teams", shown in /api/profile). Shipped profiles (the default and the
// catalog) use TCP, plus UDP for their STUN targets; https and icmp remain
// for local files that ask for them (e.g. pinging a NAS).
//
// UDP targets (kinds: [udp]) send STUN Binding Requests to a public STUN
// server (see package probe): they need a port, take no other kind, and
// their interval only has to divide an hour. Shipped STUN targets probe
// every 5s (some every 15s or 30s); targets of local files need at least
// MinUDPInterval, and a profile may have at most MaxCustomUDP of them.
//
// An interval must be a whole number of seconds, divisible by 3 (so the ICMP
// interval is whole seconds too) and divide an hour evenly, so that slots
// line up with hour boundaries: 6s, 9s, 12s, 15s, 18s, 24s, 30s, 36s, 45s,
// 60s, ... up to 3600s. It must also be at least Limits.MinIntervalSecs,
// except for targets on the special hosts below, which may use 3s.
//
// Special hosts: "@gateway" (the default gateway) and "@isp-edge" (the first
// public hop towards the internet) are resolved at run time from the local
// network path. Targets using them must have kinds [icmp].
//
// Network path group: Default and every loaded file (standalone or
// extending a profile) get a built-in group "path" ("Network path", shown
// first) that measures the layers of the path: the gateway and the ISP edge
// (ICMP every second), and three anycast resolvers (ICMP and TCP). A file
// opts out with `path: false` at the top level. Its target names are
// reserved while it is present.
package profile

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"gopkg.in/yaml.v3"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

// Limits bound what a profile may ask for.
type Limits struct {
	MaxTargets      int  // 0: no limit
	MinIntervalSecs int  // default 5
	AllowPrivateIPs bool // true only for local files
}

// MaxTraced is the most targets a profile may trace always-on.
const MaxTraced = 20

// UDP (STUN) target limits for targets that are not shipped (see
// isShippedUDP): an interval of at least MinUDPInterval (no opt-in for
// faster), and at most MaxCustomUDP of them.
const (
	MinUDPInterval = 6 * time.Second
	MaxCustomUDP   = 32
)

// UDPInterval is the interval of a catalog STUN target that sets none.
const UDPInterval = 5 * time.Second

// DefaultInterval is the TCP/HTTPS interval of a target that sets none.
const DefaultInterval = 15 * time.Second

//go:embed default.yml
var defaultYAML []byte

type fileTarget struct {
	Name          string            `yaml:"name"`
	Host          string            `yaml:"host"`
	Group         string            `yaml:"group"`
	Port          int               `yaml:"port"`
	Path          string            `yaml:"path"`
	Kinds         []string          `yaml:"kinds"`
	Interval      string            `yaml:"interval"`
	HostOverrides map[string]string `yaml:"host_overrides"`
	Trace         *bool             `yaml:"trace"`
	Tags          []string          `yaml:"tags"`
}

type fileProfile struct {
	Name     string        `yaml:"name"`
	Version  string        `yaml:"version"`
	Extends  []string      `yaml:"extends"`
	Groups   []model.Group `yaml:"groups"`
	Targets  []fileTarget  `yaml:"targets"`
	Add      []fileTarget  `yaml:"add"`
	Remove   []string      `yaml:"remove"`
	Override []fileTarget  `yaml:"override"`
	// Path adds the built-in network path group unless false.
	Path *bool `yaml:"path"`
}

// PathGroup is the ID of the built-in network path group.
const PathGroup = "path"

// PathInterval is the interval of the gateway and ISP edge targets: ICMP
// every second.
const PathInterval = 3 * time.Second

// pathTargets are the built-in network path targets. Names are identities
// (renaming one starts a new series) and differ from the DNS group's names
// for the same resolvers, which measure them as services.
func pathTargets() []model.Target {
	icmp := []model.ProbeKind{model.KindICMP}
	icmpTCP := []model.ProbeKind{model.KindICMP, model.KindTCP}
	return []model.Target{
		{Name: "Gateway", Host: model.HostGateway, Group: PathGroup, Layer: model.LayerGateway, Kinds: icmp, Interval: PathInterval},
		{Name: "ISP edge", Host: model.HostEdge, Group: PathGroup, Layer: model.LayerEdge, Kinds: icmp, Interval: PathInterval},
		{Name: "Cloudflare DNS 1.1.1.1", Host: "1.1.1.1", Group: PathGroup, Layer: model.LayerAnycast, Kinds: icmpTCP, Interval: DefaultInterval},
		{Name: "Google DNS 8.8.8.8", Host: "8.8.8.8", Group: PathGroup, Layer: model.LayerAnycast, Kinds: icmpTCP, Interval: DefaultInterval},
		{Name: "Quad9 DNS 9.9.9.9", Host: "9.9.9.9", Group: PathGroup, Layer: model.LayerAnycast, Kinds: icmpTCP, Interval: DefaultInterval},
	}
}

// addPath puts the network path group first: its Order is below every
// other group's, and its targets come before the others.
func addPath(p *model.Profile) {
	order := 0
	for _, g := range p.Groups {
		order = min(order, g.Order-1)
	}
	p.Groups = append([]model.Group{{ID: PathGroup, Title: "Network path", Order: order}}, p.Groups...)
	p.Targets = append(pathTargets(), p.Targets...)
}

// WithoutPath removes the network path group and its targets.
func WithoutPath(p *model.Profile) {
	p.Targets = slices.DeleteFunc(p.Targets, func(t model.Target) bool { return t.Group == PathGroup })
	p.Groups = slices.DeleteFunc(p.Groups, func(g model.Group) bool { return g.ID == PathGroup })
}

// Default returns a fresh copy of the embedded default profile, with the
// network path group.
func Default() (*model.Profile, error) {
	r, err := Builtin()
	if err != nil {
		return nil, err
	}
	p, err := defaultBase(r.Catalog)
	if err != nil {
		return nil, err
	}
	addPath(p)
	return p, nil
}

// defaultBase is the embedded default profile without the path group. Its
// targets name catalog targets: each is the catalog entry in the group
// default.yml gives it, plus its always-on trace; default.yml sets nothing
// else.
func defaultBase(c *Catalog) (*model.Profile, error) {
	fp, err := decode(defaultYAML)
	if err != nil {
		return nil, fmt.Errorf("default profile: %w", err)
	}
	p := &model.Profile{Name: fp.Name, Version: fp.Version, Groups: slices.Clone(fp.Groups)}
	var errs []error
	for _, ft := range fp.Targets {
		ct, ok := c.Target(ft.Name)
		switch {
		case !ok || !ct.Probeable():
			errs = append(errs, fmt.Errorf("target %q: not a probeable catalog target", ft.Name))
			continue
		case !reflect.DeepEqual(ft, fileTarget{Name: ft.Name, Group: ft.Group, Trace: ft.Trace}):
			errs = append(errs, fmt.Errorf("target %q: only name, group and trace may be set (the rest comes from the catalog)", ft.Name))
			continue
		}
		t := ct.toTarget(ft.Group, c.Provider(ct.Provider))
		t.Trace = ft.Trace != nil && *ft.Trace
		p.Targets = append(p.Targets, t)
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("default profile: %w", err)
	}
	return p, nil
}

// Load reads a local profile file, which may extend named profiles, and
// validates it with local-file limits (private IPs allowed).
func Load(path string) (*model.Profile, error) {
	return LoadLimits(path, Limits{})
}

// LoadLimits is Load with limits; private IPs are always allowed.
func LoadLimits(path string, l Limits) (*model.Profile, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	p, err := Parse(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	l.AllowPrivateIPs = true
	if err := Validate(p, l); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return p, nil
}

// Parse builds a profile from YAML without validating it. The network path
// group is added unless the file sets `path: false`.
func Parse(b []byte) (*model.Profile, error) {
	fp, err := decode(b)
	if err != nil {
		return nil, err
	}
	p, err := parse(fp)
	if err != nil {
		return nil, err
	}
	if fp.Path == nil || *fp.Path {
		if indexGroup(p.Groups, PathGroup) >= 0 {
			return nil, fmt.Errorf("group id %q is reserved for the network path group (set `path: false` to use it)", PathGroup)
		}
		for _, t := range pathTargets() {
			if indexTarget(p.Targets, t.Name) >= 0 {
				return nil, fmt.Errorf("target name %q is reserved for the network path group (set `path: false` to use it)", t.Name)
			}
		}
		addPath(p)
	}
	return p, nil
}

func parse(fp *fileProfile) (*model.Profile, error) {
	if len(fp.Extends) == 0 {
		if len(fp.Add)+len(fp.Remove)+len(fp.Override) > 0 {
			return nil, errors.New("add/remove/override need `extends: [default]`")
		}
		return standalone(fp)
	}
	if len(fp.Targets) > 0 {
		return nil, errors.New("targets: not allowed with extends; use add/remove/override")
	}
	r, err := Builtin()
	if err != nil {
		return nil, fmt.Errorf("extends: %w", err)
	}
	base, err := r.base(fp.Extends, nil)
	if err != nil {
		return nil, fmt.Errorf("extends: %w", err)
	}
	return extend(base, fp)
}

func decode(b []byte) (*fileProfile, error) {
	d := yaml.NewDecoder(bytes.NewReader(b))
	d.KnownFields(true)
	var fp fileProfile
	if err := d.Decode(&fp); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("empty profile")
		}
		return nil, err
	}
	return &fp, nil
}

func standalone(fp *fileProfile) (*model.Profile, error) {
	p := &model.Profile{Name: fp.Name, Version: fp.Version}
	if p.Name == "" {
		p.Name = "custom"
	}
	for i, g := range fp.Groups {
		if g.Order == 0 {
			g.Order = i + 1
		}
		p.Groups = append(p.Groups, g)
	}
	var errs []error
	for _, ft := range fp.Targets {
		t, err := ft.toTarget()
		if err != nil {
			errs = append(errs, err)
			continue
		}
		p.Targets = append(p.Targets, t)
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return p, nil
}

func extend(p *model.Profile, fp *fileProfile) (*model.Profile, error) {
	if fp.Name != "" {
		p.Name = fp.Name
	}
	if fp.Version != "" {
		p.Version = fp.Version
	}
	maxOrder := 0
	for _, g := range p.Groups {
		maxOrder = max(maxOrder, g.Order)
	}
	for _, g := range fp.Groups {
		if i := indexGroup(p.Groups, g.ID); i >= 0 {
			if g.Title != "" {
				p.Groups[i].Title = g.Title
			}
			if g.Order != 0 {
				p.Groups[i].Order = g.Order
			}
			continue
		}
		if g.Order == 0 {
			maxOrder++
			g.Order = maxOrder
		}
		p.Groups = append(p.Groups, g)
	}
	var errs []error
	for _, name := range fp.Remove {
		i := indexTarget(p.Targets, name)
		if i < 0 {
			errs = append(errs, fmt.Errorf("remove: no target named %q", name))
			continue
		}
		p.Targets = append(p.Targets[:i], p.Targets[i+1:]...)
	}
	for _, ft := range fp.Override {
		i := indexTarget(p.Targets, ft.Name)
		if i < 0 {
			errs = append(errs, fmt.Errorf("override: no target named %q", ft.Name))
			continue
		}
		if err := ft.apply(&p.Targets[i]); err != nil {
			errs = append(errs, err)
		}
	}
	for _, ft := range fp.Add {
		if indexTarget(p.Targets, ft.Name) >= 0 {
			errs = append(errs, fmt.Errorf("add: target %q already exists; use override", ft.Name))
			continue
		}
		t, err := ft.toTarget()
		if err != nil {
			errs = append(errs, err)
			continue
		}
		p.Targets = append(p.Targets, t)
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return p, nil
}

func (ft fileTarget) toTarget() (model.Target, error) {
	t := model.Target{Name: ft.Name, Interval: DefaultInterval}
	t.Kinds = append([]model.ProbeKind(nil), model.DefaultKinds...)
	err := ft.apply(&t)
	return t, err
}

// apply copies the fields set in ft onto t.
func (ft fileTarget) apply(t *model.Target) error {
	if ft.Host != "" {
		t.Host = ft.Host
	}
	if ft.Group != "" {
		t.Group = ft.Group
	}
	if ft.Port != 0 {
		t.Port = ft.Port
	}
	if ft.Path != "" {
		t.Path = ft.Path
	}
	if ft.Kinds != nil {
		ks, err := parseKinds(ft.Kinds)
		if err != nil {
			return fmt.Errorf("target %q: %w", ft.Name, err)
		}
		t.Kinds = ks
	}
	if ft.Interval != "" {
		d, err := time.ParseDuration(ft.Interval)
		if err != nil {
			return fmt.Errorf("target %q: interval: %w", ft.Name, err)
		}
		t.Interval = d
	}
	if ft.Trace != nil {
		t.Trace = *ft.Trace
	}
	if ft.Tags != nil {
		t.Tags = append([]string(nil), ft.Tags...)
	}
	if ft.HostOverrides != nil {
		m := map[model.ProbeKind]string{}
		for k, h := range ft.HostOverrides {
			kind, err := parseKind(k)
			if err != nil {
				return fmt.Errorf("target %q: host_overrides: %w", ft.Name, err)
			}
			m[kind] = h
		}
		t.HostOverrides = m
	}
	return nil
}

func parseKind(s string) (model.ProbeKind, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "https":
		return model.KindHTTPS, nil
	case "tcp":
		return model.KindTCP, nil
	case "icmp":
		return model.KindICMP, nil
	case "udp":
		return model.KindUDP, nil
	}
	return 0, fmt.Errorf("unknown kind %q (want https, tcp, icmp or udp)", s)
}

func parseKinds(ss []string) ([]model.ProbeKind, error) {
	ks := make([]model.ProbeKind, 0, len(ss))
	for _, s := range ss {
		k, err := parseKind(s)
		if err != nil {
			return nil, err
		}
		ks = append(ks, k)
	}
	return ks, nil
}

func indexTarget(ts []model.Target, name string) int {
	for i := range ts {
		if ts[i].Name == name {
			return i
		}
	}
	return -1
}

func indexGroup(gs []model.Group, id string) int {
	for i := range gs {
		if gs[i].ID == id {
			return i
		}
	}
	return -1
}

// Validate checks a profile against limits (zero limits use the defaults)
// and reports every problem at once.
func Validate(p *model.Profile, l Limits) error {
	if p == nil {
		return errors.New("nil profile")
	}
	if l.MinIntervalSecs <= 0 {
		l.MinIntervalSecs = 5
	}
	minIv := time.Duration(l.MinIntervalSecs) * time.Second
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	groups := map[string]bool{}
	for _, g := range p.Groups {
		switch {
		case g.ID == "":
			bad("group with empty id (title %q)", g.Title)
		case groups[g.ID]:
			bad("duplicate group id %q", g.ID)
		}
		groups[g.ID] = true
	}
	if len(p.Targets) == 0 {
		bad("no targets")
	}
	if l.MaxTargets > 0 && len(p.Targets) > l.MaxTargets {
		bad("%d targets, at most %d allowed", len(p.Targets), l.MaxTargets)
	}
	traced := 0
	for _, t := range p.Targets {
		if t.Trace {
			traced++
		}
	}
	if traced > MaxTraced {
		bad("%d targets have trace: true, at most %d allowed", traced, MaxTraced)
	}
	customUDP := 0
	for _, t := range p.Targets {
		if t.IsUDP() && !isShippedUDP(t) {
			customUDP++
		}
	}
	if customUDP > MaxCustomUDP {
		bad("%d UDP targets, at most %d allowed (besides the shipped STUN targets)", customUDP, MaxCustomUDP)
	}
	names := map[string]bool{}
	for _, t := range p.Targets {
		if err := checkName(t.Name); err != nil {
			bad("target %q: %v", t.Name, err)
		} else if names[t.Name] {
			bad("duplicate target name %q", t.Name)
		}
		names[t.Name] = true
		if !groups[t.Group] {
			bad("target %q: unknown group %q", t.Name, t.Group)
		}
		if err := checkHost(t.Host, l.AllowPrivateIPs); err != nil {
			bad("target %q: host: %v", t.Name, err)
		}
		if err := checkHomeTags(t.Tags); err != nil {
			bad("target %q: %v", t.Name, err)
		}
		special := isSpecial(t.Host)
		for k, h := range t.HostOverrides {
			if !validKind(k) || k == model.KindUDP {
				bad("target %q: host_overrides: unknown kind %d", t.Name, k)
			}
			if err := checkHost(h, l.AllowPrivateIPs); err != nil {
				bad("target %q: host_overrides[%s]: %v", t.Name, k, err)
			}
			special = special || isSpecial(h)
		}
		if special && (len(t.Kinds) != 1 || t.Kinds[0] != model.KindICMP) {
			bad("target %q: %s and %s are probed with ICMP only: set kinds: [icmp]", t.Name, model.HostGateway, model.HostEdge)
		}
		if t.Port < 0 || t.Port > 65535 {
			bad("target %q: port %d out of range", t.Name, t.Port)
		}
		if t.Path != "" && !strings.HasPrefix(t.Path, "/") {
			bad("target %q: path %q must start with /", t.Name, t.Path)
		}
		seen := map[model.ProbeKind]bool{}
		for _, k := range t.Kinds {
			if !validKind(k) {
				bad("target %q: unknown kind %d", t.Name, k)
			} else if seen[k] {
				bad("target %q: kind %s listed twice", t.Name, k)
			}
			seen[k] = true
		}
		if t.IsUDP() {
			validateUDP(t, minIv, bad)
			continue
		}
		tMin := minIv
		if special {
			tMin = min(minIv, PathInterval) // pinging the own router/edge every second is fine
		}
		if t.Interval < 0 || (t.Interval != 0 && t.Interval < tMin) {
			bad("target %q: interval %s is below the minimum %s", t.Name, t.Interval, tMin)
		} else if t.Interval != 0 && !validInterval(t.Interval) {
			bad("target %q: interval %s must be whole seconds, divisible by 3 and divide 1h evenly (e.g. 6s, 9s, 12s, 15s, 30s, 60s)", t.Name, t.Interval)
		}
	}
	return errors.Join(errs...)
}

// validKind reports a kind a target may list (the UDP siblings are written
// by KindUDP's bursts, never listed).
func validKind(k model.ProbeKind) bool {
	return (k >= model.KindHTTPS && k <= model.KindICMP) || k == model.KindUDP
}

// validateUDP checks a UDP (STUN) target: kinds [udp] alone, a port, and an
// interval that divides an hour, at least MinUDPInterval (and minIv) unless
// the target is shipped as is.
func validateUDP(t model.Target, minIv time.Duration, bad func(string, ...any)) {
	if len(t.Kinds) != 1 {
		bad("target %q: kind udp cannot be combined with other kinds (use a separate target)", t.Name)
	}
	if t.Port <= 0 || t.Port > 65535 {
		bad("target %q: kind udp needs a port (e.g. 3478 for STUN)", t.Name)
	}
	if t.Interval == 0 {
		return // the default, 15s
	}
	if t.Interval < 0 || t.Interval%time.Second != 0 || 3600%int64(t.Interval/time.Second) != 0 {
		bad("target %q: interval %s must be whole seconds and divide 1h evenly (e.g. 6s, 10s, 15s, 30s, 60s)", t.Name, t.Interval)
		return
	}
	if tMin := max(minIv, MinUDPInterval); !isShippedUDP(t) && t.Interval < tMin {
		bad("target %q: interval %s is below the minimum %s for UDP targets", t.Name, t.Interval, tMin)
	}
}

// validInterval reports whether iv is whole seconds, divisible by 3 (ICMP
// runs at iv/3, which must be whole seconds as well) and divides an hour, so
// slots of both line up with hour boundaries.
func validInterval(iv time.Duration) bool {
	if iv <= 0 || iv%time.Second != 0 {
		return false
	}
	secs := int64(iv / time.Second)
	return secs%3 == 0 && 3600%secs == 0
}

// checkName allows single spaces between words (e.g. "ISP edge"), but no
// other whitespace, control characters, or leading/trailing/double spaces.
func checkName(n string) error {
	if n == "" {
		return errors.New("empty name")
	}
	if len(n) > 64 {
		return errors.New("name longer than 64 bytes")
	}
	for _, r := range n {
		if unicode.IsControl(r) || (unicode.IsSpace(r) && r != ' ') {
			return errors.New("name contains whitespace or control characters")
		}
	}
	if strings.TrimSpace(n) != n || strings.Contains(n, "  ") {
		return errors.New("name contains whitespace at the start or end, or double spaces")
	}
	return nil
}

// WithoutUDP removes every UDP (STUN) target (--no-udp), and groups left
// empty.
func WithoutUDP(p *model.Profile) {
	n := len(p.Targets)
	p.Targets = slices.DeleteFunc(p.Targets, func(t model.Target) bool { return t.IsUDP() })
	if len(p.Targets) == n {
		return
	}
	inUse := map[string]bool{}
	for _, t := range p.Targets {
		inUse[t.Group] = true
	}
	p.Groups = slices.DeleteFunc(p.Groups, func(g model.Group) bool { return !inUse[g.ID] })
}

// shippedUDP holds the STUN targets fyisp ships (the catalog's, which the
// default selects from) by name: they may probe faster than
// MinUDPInterval, and do not count against MaxCustomUDP. A target only
// counts as shipped while its host, port and interval are unchanged.
var shippedUDP = sync.OnceValue(func() map[string]model.Target {
	out := map[string]model.Target{}
	if r, err := Builtin(); err == nil {
		for _, ct := range r.Catalog.Targets {
			if t := ct.toTarget("", nil); t.IsUDP() {
				out[t.Name] = t
			}
		}
	}
	return out
})

func isShippedUDP(t model.Target) bool {
	s, ok := shippedUDP()[t.Name]
	return ok && s.Host == t.Host && s.Port == t.Port && s.Interval == t.Interval && len(t.HostOverrides) == 0
}

// isSpecial reports whether h is resolved from the network path.
func isSpecial(h string) bool { return h == model.HostGateway || h == model.HostEdge }

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

func checkHost(h string, allowPrivate bool) error {
	if h == "" {
		return errors.New("empty")
	}
	if isSpecial(h) {
		return nil
	}
	// IPv6 literals are not accepted in v0.1: probes are IPv4-only.
	if len(h) > 253 || strings.ContainsAny(h, "/:@ \t\r\n?#[]") {
		return fmt.Errorf("%q is not a hostname or IPv4 address", h)
	}
	if a, err := netip.ParseAddr(h); err == nil && !allowPrivate {
		if a.IsPrivate() || a.IsLoopback() || a.IsLinkLocalUnicast() || a.IsUnspecified() ||
			a.IsMulticast() || cgnat.Contains(a) {
			return fmt.Errorf("%s is a private address (only allowed in local files)", h)
		}
	}
	return nil
}
