// Package profile loads target profiles: the embedded default and local
// --config files (which may extend the default).
//
// File format (YAML):
//
//	name: my-profile
//	version: "1"
//	extends: [default]          # optional; only "default" is known
//	groups:                     # new groups, or a changed title/order by id
//	  - {id: home, title: Home, order: 8}
//	add:                        # with extends: new targets
//	  - {name: router, host: 192.168.1.1, group: home, kinds: [icmp]}
//	remove: [Discord]           # with extends: drop targets by name
//	override:                   # with extends: change fields of a target by name
//	  - {name: dev-GitHub, interval: 30s}
//	targets: [...]              # without extends: the full target list
//
// Target fields: name, host, group, port (default 443), path (default "/"),
// kinds (https|tcp|icmp, default all), interval (duration, default 15s; ICMP
// runs at interval/3), host_overrides (kind -> host).
package profile

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"strings"
	"time"
	"unicode"

	"gopkg.in/yaml.v3"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

// Limits bound what a profile may ask for.
type Limits struct {
	MaxTargets      int  // default 300
	MinIntervalSecs int  // default 5
	AllowPrivateIPs bool // true only for local files
}

// DefaultInterval is the HTTPS/TCP interval of a target that sets none.
const DefaultInterval = 15 * time.Second

//go:embed default.yml
var defaultYAML []byte

// AllKinds is the kind list of a target that sets none.
var AllKinds = []model.ProbeKind{model.KindHTTPS, model.KindTCP, model.KindICMP}

type fileTarget struct {
	Name          string            `yaml:"name"`
	Host          string            `yaml:"host"`
	Group         string            `yaml:"group"`
	Port          int               `yaml:"port"`
	Path          string            `yaml:"path"`
	Kinds         []string          `yaml:"kinds"`
	Interval      string            `yaml:"interval"`
	HostOverrides map[string]string `yaml:"host_overrides"`
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
}

// Default returns a fresh copy of the embedded default profile.
func Default() (*model.Profile, error) {
	fp, err := decode(defaultYAML)
	if err != nil {
		return nil, fmt.Errorf("default profile: %w", err)
	}
	p, err := standalone(fp)
	if err != nil {
		return nil, fmt.Errorf("default profile: %w", err)
	}
	return p, nil
}

// Load reads a local profile file, which may extend the default profile, and
// validates it with local-file limits (private IPs allowed).
func Load(path string) (*model.Profile, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	p, err := Parse(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := Validate(p, Limits{AllowPrivateIPs: true}); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return p, nil
}

// Parse builds a profile from YAML without validating it.
func Parse(b []byte) (*model.Profile, error) {
	fp, err := decode(b)
	if err != nil {
		return nil, err
	}
	if len(fp.Extends) == 0 {
		if len(fp.Add)+len(fp.Remove)+len(fp.Override) > 0 {
			return nil, errors.New("add/remove/override need `extends: [default]`")
		}
		return standalone(fp)
	}
	if len(fp.Extends) != 1 || fp.Extends[0] != "default" {
		return nil, fmt.Errorf("extends: only [default] is supported, got %v", fp.Extends)
	}
	if len(fp.Targets) > 0 {
		return nil, errors.New("targets: not allowed with extends; use add/remove/override")
	}
	base, err := Default()
	if err != nil {
		return nil, err
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
	t.Kinds = append([]model.ProbeKind(nil), AllKinds...)
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
	}
	return 0, fmt.Errorf("unknown kind %q (want https, tcp or icmp)", s)
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
	if l.MaxTargets <= 0 {
		l.MaxTargets = 300
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
	if len(p.Targets) > l.MaxTargets {
		bad("%d targets, at most %d allowed", len(p.Targets), l.MaxTargets)
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
		for k, h := range t.HostOverrides {
			if k < model.KindHTTPS || k > model.KindICMP {
				bad("target %q: host_overrides: unknown kind %d", t.Name, k)
			}
			if err := checkHost(h, l.AllowPrivateIPs); err != nil {
				bad("target %q: host_overrides[%s]: %v", t.Name, k, err)
			}
		}
		if t.Port < 0 || t.Port > 65535 {
			bad("target %q: port %d out of range", t.Name, t.Port)
		}
		if t.Path != "" && !strings.HasPrefix(t.Path, "/") {
			bad("target %q: path %q must start with /", t.Name, t.Path)
		}
		seen := map[model.ProbeKind]bool{}
		for _, k := range t.Kinds {
			if k < model.KindHTTPS || k > model.KindICMP {
				bad("target %q: unknown kind %d", t.Name, k)
			} else if seen[k] {
				bad("target %q: kind %s listed twice", t.Name, k)
			}
			seen[k] = true
		}
		if t.Interval < 0 || (t.Interval != 0 && t.Interval < minIv) {
			bad("target %q: interval %s is below the minimum %s", t.Name, t.Interval, minIv)
		}
	}
	return errors.Join(errs...)
}

func checkName(n string) error {
	if n == "" {
		return errors.New("empty name")
	}
	if len(n) > 64 {
		return errors.New("name longer than 64 bytes")
	}
	for _, r := range n {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return errors.New("name contains whitespace or control characters")
		}
	}
	return nil
}

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

func checkHost(h string, allowPrivate bool) error {
	if h == "" {
		return errors.New("empty")
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
