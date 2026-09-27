package profile

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

// Named profiles (`--profile a,b`) are selections over the catalog, defined
// in profiles.yml:
//
//	profiles:
//	  - name: europe                       # [a-z0-9-]; not "default" or a provider ID
//	    description: Every target in Europe
//	    extends: [default]                 # optional: other named profiles to include
//	    select:                            # a rule, or a list of rules (OR)
//	      providers: [aws, services-*]     # provider IDs (globs allowed)
//	      kinds: [hyperscaler, cloud]      # provider kinds
//	      tags: [voice]                    # target tags or provider categories
//	      geos: [eu]                       # target geo
//	      anycast: false                   # only (non-)anycast targets
//	      targets: [AWS-eu-west-1]         # explicit names, added to the rule's matches
//	      exclude: [AWS-eu-south-2]        # names (globs allowed) dropped from the profile
//	      exclude_providers: [services-dev] # providers (globs allowed) this rule skips
//	    groups: provider                   # provider | provider-geo | geo | custom
//	    custom_groups:                     # with groups: custom; first match wins,
//	      - id: voice                      # unmatched targets are grouped by provider
//	        title: Voice & video
//	        match: {tags: [voice]}
//
// Within a rule every set field must match (lists match any of their
// values); a rule with only `targets` selects just those. Unknown providers,
// tags and names select nothing, so profiles can name catalogs that do not
// exist yet.
//
// Every catalog provider is also a profile of its own, named by its ID,
// unless profiles.yml defines that name: grouped by provider, or by
// provider and geo when it spans several geos and has more than
// providerSplitAt targets. "default" is the embedded default.yml.
//
// Combining profiles (Resolve) is a union: targets keep the group of the
// first profile that has them, groups with the same ID merge, and empty
// groups are dropped. The network path group is added once, first.

//go:embed profiles.yml
var profilesYAML []byte

// providerSplitAt is the size above which a provider profile spanning
// several geos gets one group per geo.
const providerSplitAt = 20

// DefaultMaxTargets is the default target limit of a run; MaxTargetsCap is
// the most --max-targets may allow.
const (
	DefaultMaxTargets = 300
	MaxTargetsCap     = 1000
)

// Grouping strategies.
const (
	GroupByProvider    = "provider"
	GroupByProviderGeo = "provider-geo"
	GroupByGeo         = "geo"
	GroupCustom        = "custom"
)

// Rule selects catalog targets.
type Rule struct {
	Providers []string `yaml:"providers"`
	Kinds     []string `yaml:"kinds"`
	Tags      []string `yaml:"tags"`
	Geos      []string `yaml:"geos"`
	Anycast   *bool    `yaml:"anycast"`
	Targets   []string `yaml:"targets"`
	Exclude   []string `yaml:"exclude"`
	// ExcludeProviders drops providers (globs allowed) from this rule.
	ExcludeProviders []string `yaml:"exclude_providers"`
}

// Rules is a rule or a list of rules.
type Rules []Rule

// UnmarshalYAML accepts a single mapping or a sequence of them.
func (rs *Rules) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.MappingNode {
		var r Rule
		if err := decodeStrict(n, &r); err != nil {
			return err
		}
		*rs = Rules{r}
		return nil
	}
	if n.Kind != yaml.SequenceNode {
		return fmt.Errorf("line %d: select must be a rule or a list of rules", n.Line)
	}
	var list []Rule
	for _, c := range n.Content {
		var r Rule
		if err := decodeStrict(c, &r); err != nil {
			return err
		}
		list = append(list, r)
	}
	*rs = list
	return nil
}

// decodeStrict decodes a node rejecting unknown fields (Node.Decode does
// not honour Decoder.KnownFields).
func decodeStrict(n *yaml.Node, v any) error {
	b, err := yaml.Marshal(n)
	if err != nil {
		return err
	}
	d := yaml.NewDecoder(bytes.NewReader(b))
	d.KnownFields(true)
	if err := d.Decode(v); err != nil {
		return fmt.Errorf("line %d: %w", n.Line, err)
	}
	return nil
}

// CustomGroup is one group of a `groups: custom` profile.
type CustomGroup struct {
	ID    string `yaml:"id"`
	Title string `yaml:"title"`
	Match Rules  `yaml:"match"`
}

// Definition is one named profile.
type Definition struct {
	Name         string        `yaml:"name"`
	Description  string        `yaml:"description"`
	Extends      []string      `yaml:"extends"`
	Select       Rules         `yaml:"select"`
	Groups       string        `yaml:"groups"`
	CustomGroups []CustomGroup `yaml:"custom_groups"`
	// Provider is set for the automatic per-provider profiles.
	Provider string `yaml:"-"`
}

type profilesFile struct {
	Profiles []Definition `yaml:"profiles"`
}

// Registry is the catalog plus the named profiles over it.
type Registry struct {
	Catalog *Catalog
	defs    []*Definition // profiles.yml order, then provider profiles
	byName  map[string]*Definition
}

// DefaultDescription describes the built-in default profile.
const DefaultDescription = "The original 87 targets: common services, DNS, dev tunnels, dev services, AWS, Hetzner, GCP"

var (
	profileNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	groupIDRe     = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
)

// NewRegistry parses profiles.yml against a catalog.
func NewRegistry(c *Catalog, profilesYML []byte) (*Registry, error) {
	d := yaml.NewDecoder(bytes.NewReader(profilesYML))
	d.KnownFields(true)
	var pf profilesFile
	if err := d.Decode(&pf); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	r := &Registry{Catalog: c, byName: map[string]*Definition{}}
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }
	for i := range pf.Profiles {
		def := &pf.Profiles[i]
		switch {
		case !profileNameRe.MatchString(def.Name):
			bad("profile %q: name must be lowercase [a-z0-9-]", def.Name)
		case def.Name == "default":
			bad("profile %q: reserved for default.yml", def.Name)
		case r.byName[def.Name] != nil:
			bad("profile %q: defined twice", def.Name)
		}
		if strings.TrimSpace(def.Description) == "" {
			bad("profile %q: description is required", def.Name)
		}
		if def.Groups == "" {
			def.Groups = GroupByProvider
		}
		switch def.Groups {
		case GroupByProvider, GroupByProviderGeo, GroupByGeo:
			if len(def.CustomGroups) > 0 {
				bad("profile %q: custom_groups need groups: custom", def.Name)
			}
		case GroupCustom:
			if len(def.CustomGroups) == 0 {
				bad("profile %q: groups: custom needs custom_groups", def.Name)
			}
			for _, g := range def.CustomGroups {
				if !groupIDRe.MatchString(g.ID) || g.ID == PathGroup {
					bad("profile %q: custom group id %q must be lowercase [a-z0-9-] and not %q", def.Name, g.ID, PathGroup)
				}
				if g.Title == "" || len(g.Match) == 0 {
					bad("profile %q: custom group %q needs a title and match", def.Name, g.ID)
				}
			}
		default:
			bad("profile %q: groups %q, want provider, provider-geo, geo or custom", def.Name, def.Groups)
		}
		if len(def.Select) == 0 && len(def.Extends) == 0 {
			bad("profile %q: needs select or extends", def.Name)
		}
		for _, rule := range def.Select {
			for _, k := range rule.Kinds {
				if !slices.Contains(ProviderKinds, k) {
					bad("profile %q: kind %q, want one of %s", def.Name, k, strings.Join(ProviderKinds, ", "))
				}
			}
			for _, g := range rule.Geos {
				if !slices.Contains(Geos, g) {
					bad("profile %q: geo %q, want one of %s", def.Name, g, strings.Join(Geos, ", "))
				}
			}
		}
		r.byName[def.Name] = def
		r.defs = append(r.defs, def)
	}
	for _, p := range c.Providers {
		if r.byName[p.ID] != nil || p.ID == "default" {
			continue // profiles.yml wins
		}
		def := &Definition{
			Name: p.ID, Description: p.Display, Provider: p.ID,
			Select: Rules{{Providers: []string{p.ID}}}, Groups: GroupByProvider,
		}
		geos := map[string]bool{}
		for _, t := range p.Targets {
			geos[t.GeoKey()] = true
		}
		if len(p.Targets) > providerSplitAt && len(geos) > 1 {
			def.Groups = GroupByProviderGeo
		}
		r.byName[def.Name] = def
		r.defs = append(r.defs, def)
	}
	for _, def := range r.defs {
		for _, e := range def.Extends {
			if e != "default" && r.byName[e] == nil {
				bad("profile %q: extends unknown profile %q", def.Name, e)
			}
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	for _, def := range r.defs {
		if _, err := r.base([]string{def.Name}, nil); err != nil {
			return nil, err // cycles
		}
	}
	return r, nil
}

// Definitions returns the named profiles: "default" first (nil Select),
// then profiles.yml order, then the provider profiles.
func (r *Registry) Definitions() []*Definition {
	out := []*Definition{{Name: "default", Description: DefaultDescription}}
	return append(out, r.defs...)
}

// Definition returns a named profile, or nil.
func (r *Registry) Definition(name string) *Definition {
	if name == "default" {
		return r.Definitions()[0]
	}
	return r.byName[name]
}

// ResolveOptions change what Resolve builds.
type ResolveOptions struct {
	// NoPath leaves out the built-in network path group.
	NoPath bool
	// Geos, if set, keeps only targets whose catalog entry is located in
	// one of these geos: an intersection with the selection (e.g. clouds
	// in Europe). Targets that are not in the catalog, and anycast
	// targets without a geo, are dropped.
	Geos []string
}

// Resolve builds the union of named profiles, with the network path group
// unless o.NoPath. It does not validate limits (see Validate).
func (r *Registry) Resolve(names []string, o ResolveOptions) (*model.Profile, error) {
	if len(names) == 0 {
		return nil, errors.New("no profile named")
	}
	for _, g := range o.Geos {
		if !slices.Contains(Geos, g) {
			return nil, fmt.Errorf("unknown geo %q, want one of %s", g, strings.Join(Geos, ", "))
		}
	}
	b, err := r.base(names, nil)
	if err != nil {
		return nil, err
	}
	p := b.Profile
	p.Name = strings.Join(names, ",")
	if len(o.Geos) > 0 {
		p.Name += "@" + strings.Join(o.Geos, ",")
		p.Targets = slices.DeleteFunc(p.Targets, func(t model.Target) bool {
			ct, ok := r.Catalog.Target(t.Name)
			return !ok || !slices.Contains(o.Geos, ct.Geo)
		})
		dropEmptyGroups(p)
	}
	if !o.NoPath {
		addPath(p)
	}
	return p, nil
}

// GeoNames maps the regional profile names to geo codes.
var GeoNames = map[string]string{
	"north-america": "na", "south-america": "sa", "europe": "eu", "middle-east": "me",
	"africa": "af", "asia": "as", "oceania": "oc",
}

// ParseGeos parses a comma-separated list of geo codes (eu) or region
// names (europe).
func ParseGeos(s string) ([]string, error) {
	var out []string
	for _, g := range strings.Split(s, ",") {
		g = strings.ToLower(strings.TrimSpace(g))
		if g == "" {
			continue
		}
		if c, ok := GeoNames[g]; ok {
			g = c
		}
		if !slices.Contains(Geos, g) {
			return nil, fmt.Errorf("unknown geo %q: want one of %s (or north-america, europe, ...)", g, strings.Join(Geos, ", "))
		}
		if !slices.Contains(out, g) {
			out = append(out, g)
		}
	}
	return out, nil
}

// dropEmptyGroups removes groups without targets and renumbers the rest.
func dropEmptyGroups(p *model.Profile) {
	inUse := map[string]bool{}
	for _, t := range p.Targets {
		inUse[t.Group] = true
	}
	p.Groups = slices.DeleteFunc(p.Groups, func(g model.Group) bool { return !inUse[g.ID] })
	slices.SortStableFunc(p.Groups, func(a, b model.Group) int { return a.Order - b.Order })
	for i := range p.Groups {
		p.Groups[i].Order = i + 1
	}
}

// sel is a profile being built, with the names of the targets that come
// from default.yml (legacy): a catalog target of the same name replaces
// them when merged.
type sel struct {
	*model.Profile
	legacy map[string]bool
}

func newSel(name string) *sel {
	return &sel{Profile: &model.Profile{Name: name, Version: "1"}, legacy: map[string]bool{}}
}

// base is the union of the named profiles without the path group.
func (r *Registry) base(names []string, stack []string) (*sel, error) {
	out := newSel(strings.Join(names, ","))
	for _, n := range names {
		n = strings.TrimSpace(n)
		if slices.Contains(stack, n) {
			return nil, fmt.Errorf("profile %q extends itself (%s)", n, strings.Join(append(stack, n), " -> "))
		}
		var part *sel
		if n == "default" {
			p, err := defaultBase()
			if err != nil {
				return nil, err
			}
			part = &sel{Profile: p, legacy: map[string]bool{}}
			for _, t := range p.Targets {
				part.legacy[t.Name] = true
			}
		} else {
			def := r.byName[n]
			if def == nil {
				return nil, fmt.Errorf("unknown profile %q (see `fyisp profiles`)", n)
			}
			var err error
			if part, err = r.build(def, append(stack, n)); err != nil {
				return nil, err
			}
		}
		merge(out, part)
	}
	return out, nil
}

// build resolves one definition: its extends, then its own selection.
func (r *Registry) build(def *Definition, stack []string) (*sel, error) {
	out := newSel(def.Name)
	if len(def.Extends) > 0 {
		ext, err := r.base(def.Extends, stack)
		if err != nil {
			return nil, err
		}
		merge(out, ext)
	}
	// Groups are ordered by their grouping key: custom groups in definition
	// order, then providers in catalog order, then geos in Geos order.
	type key struct{ custom, prov, geo int }
	provRank := map[string]int{}
	for i, p := range r.Catalog.Providers {
		provRank[p.ID] = i
	}
	own := newSel(def.Name)
	keys := map[string]key{}
	for _, ct := range r.Select(def) {
		prov := r.Catalog.Provider(ct.Provider)
		id, title, custom := r.groupOf(def, prov, ct)
		if _, ok := keys[id]; !ok {
			k := key{custom: custom, prov: provRank[prov.ID], geo: slices.Index(Geos, ct.GeoKey())}
			if k.geo < 0 {
				k.geo = len(Geos) // global last
			}
			switch {
			case custom < len(def.CustomGroups):
				k.prov, k.geo = 0, 0
			case def.Groups == GroupByGeo:
				k.prov = 0
			case def.Groups != GroupByProviderGeo:
				k.geo = 0
			}
			keys[id] = k
			own.Groups = append(own.Groups, model.Group{ID: id, Title: title})
		}
		own.Targets = append(own.Targets, ct.toTarget(id))
	}
	slices.SortStableFunc(own.Groups, func(a, b model.Group) int {
		ka, kb := keys[a.ID], keys[b.ID]
		if ka.custom != kb.custom {
			return ka.custom - kb.custom
		}
		if ka.prov != kb.prov {
			return ka.prov - kb.prov
		}
		return ka.geo - kb.geo
	})
	for i := range own.Groups {
		own.Groups[i].Order = i + 1
	}
	merge(out, own)
	return out, nil
}

// Select returns the catalog targets a definition selects itself (without
// extends), in catalog order. Targets without kinds are left out (see
// Skipped).
func (r *Registry) Select(def *Definition) []CatalogTarget {
	return r.selectTargets(def, true)
}

// Skipped returns the catalog targets a definition matches but leaves out
// because they have no kinds (unreachable when last checked).
func (r *Registry) Skipped(def *Definition) []CatalogTarget {
	return r.selectTargets(def, false)
}

// SkippedIn returns the targets Skipped for each named profile and the
// profiles it extends, without repeats.
func (r *Registry) SkippedIn(names []string) []CatalogTarget {
	var out []CatalogTarget
	seen := map[string]bool{}
	var walk func(ns []string)
	walk = func(ns []string) {
		for _, n := range ns {
			def := r.byName[n]
			if def == nil || seen["\x00"+n] {
				continue
			}
			seen["\x00"+n] = true
			walk(def.Extends)
			for _, t := range r.Skipped(def) {
				if !seen[t.Name] {
					seen[t.Name] = true
					out = append(out, t)
				}
			}
		}
	}
	walk(names)
	return out
}

func (r *Registry) selectTargets(def *Definition, probeable bool) []CatalogTarget {
	var excl []string
	for _, rule := range def.Select {
		excl = append(excl, rule.Exclude...)
	}
	var out []CatalogTarget
	for _, t := range r.Catalog.Targets {
		if t.Probeable() != probeable || matchAny(excl, t.Name) {
			continue
		}
		prov := r.Catalog.Provider(t.Provider)
		for _, rule := range def.Select {
			if rule.matches(prov, t) {
				out = append(out, t)
				break
			}
		}
	}
	return out
}

func (rule Rule) matches(p *Provider, t CatalogTarget) bool {
	if matchAny(rule.Targets, t.Name) {
		return true
	}
	if len(rule.Providers)+len(rule.Kinds)+len(rule.Tags)+len(rule.Geos) == 0 && rule.Anycast == nil {
		return false // only explicit targets
	}
	if matchAny(rule.ExcludeProviders, p.ID) {
		return false
	}
	if len(rule.Providers) > 0 && !matchAny(rule.Providers, p.ID) {
		return false
	}
	if len(rule.Kinds) > 0 && !slices.Contains(rule.Kinds, p.Kind) {
		return false
	}
	if len(rule.Tags) > 0 && !slices.ContainsFunc(rule.Tags, func(tag string) bool {
		return slices.Contains(t.Tags, tag) || slices.Contains(p.Category, tag)
	}) {
		return false
	}
	if len(rule.Geos) > 0 && !slices.Contains(rule.Geos, t.Geo) {
		return false
	}
	if rule.Anycast != nil && *rule.Anycast != t.Anycast {
		return false
	}
	return true
}

// matchAny reports whether s matches any of the patterns (path.Match globs).
func matchAny(patterns []string, s string) bool {
	for _, p := range patterns {
		if p == s {
			return true
		}
		if ok, _ := path.Match(p, s); ok {
			return true
		}
	}
	return false
}

// groupOf returns the group ID and title of a target in a definition, and
// the index of the custom group it matched (len(CustomGroups) if none).
func (r *Registry) groupOf(def *Definition, p *Provider, t CatalogTarget) (id, title string, custom int) {
	strategy := def.Groups
	if strategy == GroupCustom {
		for i, g := range def.CustomGroups {
			for _, rule := range g.Match {
				if rule.matches(p, t) {
					return g.ID, g.Title, i
				}
			}
		}
		strategy = GroupByProvider
	}
	custom = len(def.CustomGroups)
	geo := t.GeoKey()
	switch strategy {
	case GroupByProviderGeo:
		return p.ID + "-" + geo, p.Display + " · " + GeoTitles[geo], custom
	case GroupByGeo:
		return "geo-" + geo, GeoTitles[geo], custom
	}
	return p.ID, p.Display, custom
}

// merge adds src's groups and targets to dst. A target already in dst (by
// name) keeps its place and group, except that a catalog target replaces
// a default.yml target of the same name (the catalog definition wins; an
// always-on trace carries over). Groups with the same ID merge (dst's
// title wins), new groups are ordered after dst's, and groups left empty
// are dropped.
func merge(dst, src *sel) {
	have := map[string]int{}
	for i, t := range dst.Targets {
		have[t.Name] = i
	}
	take := map[string]bool{}
	replaced := map[string]model.Target{}
	for _, t := range src.Targets {
		i, ok := have[t.Name]
		switch {
		case !ok:
			take[t.Name] = true
		case dst.legacy[t.Name] && !src.legacy[t.Name]:
			take[t.Name] = true
			replaced[t.Name] = dst.Targets[i]
		}
	}
	if len(replaced) > 0 {
		dst.Targets = slices.DeleteFunc(dst.Targets, func(t model.Target) bool {
			_, ok := replaced[t.Name]
			return ok
		})
		for n := range replaced {
			delete(dst.legacy, n)
		}
	}
	maxOrder := 0
	for _, g := range dst.Groups {
		maxOrder = max(maxOrder, g.Order)
	}
	groups := slices.Clone(src.Groups)
	slices.SortStableFunc(groups, func(a, b model.Group) int { return a.Order - b.Order })
	used := map[string]bool{}
	for _, t := range src.Targets {
		if take[t.Name] {
			used[t.Group] = true
		}
	}
	for _, g := range groups {
		if !used[g.ID] || indexGroup(dst.Groups, g.ID) >= 0 {
			continue
		}
		maxOrder++
		g.Order = maxOrder
		dst.Groups = append(dst.Groups, g)
	}
	for _, t := range src.Targets {
		if !take[t.Name] {
			continue
		}
		take[t.Name] = false // first occurrence only
		if old, ok := replaced[t.Name]; ok {
			t.Trace = t.Trace || old.Trace
		}
		if src.legacy[t.Name] {
			dst.legacy[t.Name] = true
		}
		dst.Targets = append(dst.Targets, t)
	}
	if len(replaced) > 0 {
		dropEmptyGroups(dst.Profile)
	}
}
