package profile

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

// The target catalog: one YAML file per provider under catalog/ (format in
// catalog/SCHEMA.md). The whole directory is embedded because a pattern
// like catalog/*.yml fails to compile while no file matches; only the
// top-level *.yml files are read (catalog/testdata holds test samples).
//
//go:embed catalog
var catalogFS embed.FS

// ProviderKinds are the valid values of a catalog file's `kind`.
var ProviderKinds = []string{"hyperscaler", "cloud", "cdn", "dns", "service", "game", "gaming-platform"}

// Geos are the valid values of a catalog target's `geo`, in display order.
var Geos = []string{"na", "sa", "eu", "me", "af", "as", "oc"}

// GeoTitles names each geo (and "global": anycast targets) in panel titles.
var GeoTitles = map[string]string{
	"na": "North America", "sa": "South America", "eu": "Europe", "me": "Middle East",
	"af": "Africa", "as": "Asia", "oc": "Oceania", "global": "Global",
}

// Provider is one catalog file.
type Provider struct {
	ID       string          `yaml:"provider"`
	Display  string          `yaml:"display"`
	Kind     string          `yaml:"kind"`
	Category []string        `yaml:"category"`
	Sources  []string        `yaml:"sources"`
	Notes    string          `yaml:"notes"`
	Targets  []CatalogTarget `yaml:"targets"`
	// File is the catalog file name (set by the loader).
	File string `yaml:"-"`
}

// CatalogTarget is one endpoint of a provider.
type CatalogTarget struct {
	Name    string   `yaml:"name"`
	Host    string   `yaml:"host"`
	Kinds   []string `yaml:"kinds"`
	Port    int      `yaml:"port"`
	Path    string   `yaml:"path"`
	Region  string   `yaml:"region"`
	City    string   `yaml:"city"`
	Country string   `yaml:"country"`
	Geo     string   `yaml:"geo"`
	Anycast bool     `yaml:"anycast"`
	Tags    []string `yaml:"tags"`
	// HostOverrides probes some kinds on another host (kind -> host).
	HostOverrides map[string]string `yaml:"host_overrides"`
	// Verified and Notes are research metadata: accepted in any shape,
	// never used for probing.
	Verified map[string]any `yaml:"verified"`
	Notes    string         `yaml:"notes"`
	// Provider is the owning provider's ID (set by the loader).
	Provider string `yaml:"-"`
}

// Probeable reports whether the target has any kind to probe; targets with
// `kinds: []` (unreachable when last checked) are skipped by profiles.
func (t CatalogTarget) Probeable() bool { return len(t.Kinds) > 0 }

// GeoKey is the target's geo for grouping: "global" for anycast targets
// and targets without a geo.
func (t CatalogTarget) GeoKey() string {
	if t.Anycast || t.Geo == "" {
		return "global"
	}
	return t.Geo
}

// Catalog is every provider file, sorted by provider ID.
type Catalog struct {
	Providers []*Provider
	// Targets is every target, in provider order then file order.
	Targets []CatalogTarget
	byName  map[string]int
	byProv  map[string]*Provider
}

// Provider returns the provider with this ID, or nil.
func (c *Catalog) Provider(id string) *Provider { return c.byProv[id] }

// Target returns the target with this name.
func (c *Catalog) Target(name string) (CatalogTarget, bool) {
	i, ok := c.byName[name]
	if !ok {
		return CatalogTarget{}, false
	}
	return c.Targets[i], true
}

var (
	providerIDRe  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	catalogNameRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,40}$`)
	countryRe     = regexp.MustCompile(`^[A-Z]{2}$`)
)

// LoadCatalog reads and validates every *.yml file at the top level of
// fsys. It reports every problem at once.
func LoadCatalog(fsys fs.FS) (*Catalog, error) {
	files, err := fs.Glob(fsys, "*.yml")
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	c := &Catalog{byName: map[string]int{}, byProv: map[string]*Provider{}}
	var errs []error
	owner := map[string]string{} // target name -> file
	for _, f := range files {
		b, err := fs.ReadFile(fsys, f)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		p, err := decodeProvider(b)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", f, err))
			continue
		}
		p.File = f
		if err := p.validate(); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", f, err))
		}
		if prev, ok := c.byProv[p.ID]; ok && p.ID != "" {
			errs = append(errs, fmt.Errorf("%s: provider %q already defined in %s", f, p.ID, prev.File))
			continue
		}
		c.byProv[p.ID] = p
		c.Providers = append(c.Providers, p)
		for i := range p.Targets {
			n := p.Targets[i].Name
			if prev, ok := owner[n]; ok {
				errs = append(errs, fmt.Errorf("%s: target name %q already used in %s", f, n, prev))
				continue
			}
			owner[n] = f
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	sort.SliceStable(c.Providers, func(i, j int) bool { return c.Providers[i].ID < c.Providers[j].ID })
	for _, p := range c.Providers {
		for _, t := range p.Targets {
			t.Provider = p.ID
			c.byName[t.Name] = len(c.Targets)
			c.Targets = append(c.Targets, t)
		}
	}
	return c, nil
}

func decodeProvider(b []byte) (*Provider, error) {
	d := yaml.NewDecoder(bytes.NewReader(b))
	d.KnownFields(true)
	var p Provider
	if err := d.Decode(&p); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("empty file")
		}
		return nil, err
	}
	return &p, nil
}

func (p *Provider) validate() error {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }
	if !providerIDRe.MatchString(p.ID) {
		bad("provider %q: want lowercase [a-z0-9-]", p.ID)
	}
	if strings.TrimSpace(p.Display) == "" {
		bad("provider %q: display is required", p.ID)
	}
	if !slices.Contains(ProviderKinds, p.Kind) {
		bad("provider %q: kind %q, want one of %s", p.ID, p.Kind, strings.Join(ProviderKinds, ", "))
	}
	if len(p.Targets) == 0 {
		bad("provider %q: no targets", p.ID)
	}
	seen := map[string]bool{}
	for _, t := range p.Targets {
		if !catalogNameRe.MatchString(t.Name) {
			bad("target %q: name must be 1-40 characters of [A-Za-z0-9._-]", t.Name)
		} else if seen[t.Name] {
			bad("target %q: duplicate name", t.Name)
		}
		seen[t.Name] = true
		if t.Host == "" {
			bad("target %q: host is required", t.Name)
		} else if isSpecial(t.Host) {
			bad("target %q: host %s is not allowed in the catalog", t.Name, t.Host)
		} else if err := checkHost(t.Host, false); err != nil {
			bad("target %q: host: %v", t.Name, err)
		}
		// Empty kinds mark a known but currently unreachable endpoint: kept
		// in the catalog, skipped by every profile.
		ks := map[model.ProbeKind]bool{}
		for _, k := range t.Kinds {
			kind, err := parseKind(k)
			if err != nil {
				bad("target %q: %v", t.Name, err)
			} else if ks[kind] {
				bad("target %q: kind %s listed twice", t.Name, kind)
			}
			ks[kind] = true
		}
		for k, h := range t.HostOverrides {
			if _, err := parseKind(k); err != nil {
				bad("target %q: host_overrides: %v", t.Name, err)
			}
			if isSpecial(h) {
				bad("target %q: host_overrides: host %s is not allowed in the catalog", t.Name, h)
			} else if err := checkHost(h, false); err != nil {
				bad("target %q: host_overrides[%s]: %v", t.Name, k, err)
			}
		}
		if t.Port < 0 || t.Port > 65535 {
			bad("target %q: port %d out of range", t.Name, t.Port)
		}
		if t.Path != "" && !strings.HasPrefix(t.Path, "/") {
			bad("target %q: path %q must start with /", t.Name, t.Path)
		} else if u, err := url.Parse("https://x" + t.Path); err != nil || strings.ContainsAny(t.Path, " \t#") || u.Host != "x" {
			bad("target %q: path %q is not a valid URL path (query strings are fine)", t.Name, t.Path)
		}
		switch {
		case t.Geo == "" && !t.Anycast:
			bad("target %q: geo is required (one of %s) unless anycast: true", t.Name, strings.Join(Geos, ", "))
		case t.Geo != "" && !slices.Contains(Geos, t.Geo):
			bad("target %q: geo %q, want one of %s", t.Name, t.Geo, strings.Join(Geos, ", "))
		}
		if t.Country != "" && !countryRe.MatchString(t.Country) {
			bad("target %q: country %q, want an ISO 3166-1 alpha-2 code like US", t.Name, t.Country)
		}
	}
	return errors.Join(errs...)
}

// toTarget converts a catalog target of provider p to a probe target in
// group g, with the provider and location metadata.
func (t CatalogTarget) toTarget(group string, p *Provider) model.Target {
	ks, _ := parseKinds(t.Kinds) // validated by LoadCatalog
	mt := model.Target{Name: t.Name, Host: t.Host, Group: group, Port: t.Port, Path: t.Path, Kinds: ks, Interval: DefaultInterval,
		City: t.City, Country: t.Country, Geo: t.GeoKey()}
	if p != nil {
		mt.Provider, mt.ProviderTitle, mt.ProviderKind = p.ID, p.Display, p.Kind
	}
	if len(t.HostOverrides) > 0 {
		mt.HostOverrides = map[model.ProbeKind]string{}
		for k, h := range t.HostOverrides {
			kind, _ := parseKind(k)
			mt.HostOverrides[kind] = h
		}
	}
	return mt
}

var (
	builtinOnce sync.Once
	builtinReg  *Registry
	builtinErr  error
)

// Builtin returns the registry of the embedded catalog and profiles.yml.
func Builtin() (*Registry, error) {
	builtinOnce.Do(func() {
		sub, err := fs.Sub(catalogFS, "catalog")
		if err != nil {
			builtinErr = err
			return
		}
		c, err := LoadCatalog(sub)
		if err != nil {
			builtinErr = fmt.Errorf("embedded catalog: %w", err)
			return
		}
		builtinReg, builtinErr = NewRegistry(c, profilesYAML)
		if builtinErr != nil {
			builtinErr = fmt.Errorf("embedded profiles.yml: %w", builtinErr)
		}
	})
	return builtinReg, builtinErr
}
