package profile

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

// sampleRegistry is the embedded profiles.yml over the test catalog.
func sampleRegistry(t *testing.T) *Registry {
	t.Helper()
	r, err := NewRegistry(sampleCatalog(t), profilesYAML)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// useRegistry makes Builtin return r for the rest of the test.
func useRegistry(t *testing.T, r *Registry) {
	t.Helper()
	if _, err := Builtin(); err != nil {
		t.Fatal(err)
	}
	old := builtinReg
	builtinReg = r
	t.Cleanup(func() { builtinReg = old })
}

func names(ts []model.Target) []string {
	var out []string
	for _, tg := range ts {
		out = append(out, tg.Name)
	}
	return out
}

func groupSummary(p *model.Profile) []string {
	var out []string
	for _, g := range p.Groups {
		n := 0
		for _, tg := range p.Targets {
			if tg.Group == g.ID {
				n++
			}
		}
		out = append(out, fmt.Sprintf("%s=%s/%d", g.ID, g.Title, n))
	}
	return out
}

func resolve(t *testing.T, r *Registry, noPath bool, ns ...string) *model.Profile {
	t.Helper()
	p, err := r.Resolve(ns, ResolveOptions{NoPath: noPath})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestResolveDefaultUnchanged(t *testing.T) {
	r := sampleRegistry(t)
	want, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	got := resolve(t, r, false, "default")
	if !reflect.DeepEqual(got, want) {
		t.Errorf("--profile default differs from Default():\n got %+v\nwant %+v", got.Groups, want.Groups)
	}
	// And through the real embedded registry.
	b, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	if got := resolve(t, b, false, "default"); !reflect.DeepEqual(got, want) {
		t.Error("builtin --profile default differs from Default()")
	}
}

func TestBuiltinDefinitionsOnSample(t *testing.T) {
	r := sampleRegistry(t)
	cases := []struct {
		name    string
		targets []string
		groups  []string
	}{
		{"hyperscalers", []string{"AWS-us-east-1", "AWS-us-west-2", "AWS-eu-central-1", "AWS-global-accelerator"},
			[]string{"aws-na=Amazon Web Services · North America/2", "aws-eu=Amazon Web Services · Europe/1", "aws-global=Amazon Web Services · Global/1"}},
		{"clouds", []string{"AWS-us-east-1", "AWS-us-west-2", "AWS-eu-central-1", "AWS-global-accelerator", "Hetzner-Falkenstein", "Hetzner-Ashburn"},
			[]string{"aws-na=Amazon Web Services · North America/2", "aws-eu=Amazon Web Services · Europe/1", "aws-global=Amazon Web Services · Global/1",
				"hetzner-na=Hetzner · North America/1", "hetzner-eu=Hetzner · Europe/1"}},
		{"europe", []string{"AWS-eu-central-1", "Hetzner-Falkenstein", "Zoom-eu"},
			[]string{"aws=Amazon Web Services/1", "hetzner=Hetzner/1", "services-voice=Voice & video/1"}},
		{"north-america", []string{"AWS-us-east-1", "AWS-us-west-2", "Hetzner-Ashburn"},
			[]string{"aws=Amazon Web Services/2", "hetzner=Hetzner/1"}},
		{"common", []string{"Discord", "Zoom-eu"}, []string{"services-voice=Voice & video/2"}},
		{"aws", []string{"AWS-us-east-1", "AWS-us-west-2", "AWS-eu-central-1", "AWS-global-accelerator"}, []string{"aws=Amazon Web Services/4"}},
		{"services-voice", []string{"Discord", "Zoom-eu"}, []string{"services-voice=Voice & video/2"}},
		{"gaming", nil, nil},
		{"cdn", nil, nil},
		{"asia", nil, nil},
		{"middle-east", nil, nil}, // AWS-me-south-1 has kinds: []
	}
	for _, c := range cases {
		p := resolve(t, r, true, c.name)
		if got := names(p.Targets); !slices.Equal(got, c.targets) {
			t.Errorf("%s: targets %v, want %v", c.name, got, c.targets)
		}
		if got := groupSummary(p); !slices.Equal(got, c.groups) {
			t.Errorf("%s: groups %v, want %v", c.name, got, c.groups)
		}
		for i, g := range p.Groups {
			if g.Order != i+1 {
				t.Errorf("%s: group %s order %d", c.name, g.ID, g.Order)
			}
		}
	}
	for _, d := range r.Definitions() {
		if d.Description == "" {
			t.Errorf("%s: no description", d.Name)
		}
	}
	if d := r.Definition("aws"); d == nil || d.Provider != "aws" || d.Description != "Amazon Web Services" {
		t.Errorf("provider profile aws: %+v", d)
	}
	if sk := r.SkippedIn([]string{"middle-east", "aws", "hyperscalers"}); len(sk) != 1 || sk[0].Name != "AWS-me-south-1" {
		t.Errorf("skipped %v", sk)
	}
	if r.Definition("nope") != nil || r.Definition("default") == nil {
		t.Error("Definition lookup")
	}
}

func TestResolveCombine(t *testing.T) {
	r := sampleRegistry(t)
	p := resolve(t, r, false, "aws", "europe")
	if p.Name != "aws,europe" {
		t.Errorf("name %q", p.Name)
	}
	want := []string{"Gateway", "ISP edge", "Cloudflare DNS 1.1.1.1", "Google DNS 8.8.8.8", "Quad9 DNS 9.9.9.9",
		"AWS-us-east-1", "AWS-us-west-2", "AWS-eu-central-1", "AWS-global-accelerator", "Hetzner-Falkenstein", "Zoom-eu"}
	if got := names(p.Targets); !slices.Equal(got, want) {
		t.Errorf("targets %v", got)
	}
	// AWS-eu-central-1 stays in the aws group; europe's aws group merges.
	wantG := []string{"path=Network path/5", "aws=Amazon Web Services/4", "hetzner=Hetzner/1", "services-voice=Voice & video/1"}
	if got := groupSummary(p); !slices.Equal(got, wantG) {
		t.Errorf("groups %v", got)
	}
	if err := Validate(p, Limits{}); err != nil {
		t.Error(err)
	}

	// default first: catalog definitions replace default.yml targets of the
	// same name (Discord, three AWS regions, two Hetzner sites) and move to
	// the catalog's groups; the rest of default is untouched.
	p = resolve(t, r, false, "default", "clouds", "common")
	def, _ := Default()
	replaced := []string{"Discord", "AWS-us-east-1", "AWS-us-west-2", "AWS-eu-central-1", "Hetzner-Falkenstein", "Hetzner-Ashburn"}
	var kept []model.Target
	for _, tg := range def.Targets {
		if !slices.Contains(replaced, tg.Name) {
			kept = append(kept, tg)
		}
	}
	if !reflect.DeepEqual(p.Targets[:len(kept)], kept) || !reflect.DeepEqual(p.Groups[:len(def.Groups)], def.Groups) {
		t.Errorf("default's part changed when combined: %v", names(p.Targets))
	}
	extra := names(p.Targets[len(kept):])
	if !slices.Equal(extra, []string{"AWS-us-east-1", "AWS-us-west-2", "AWS-eu-central-1", "AWS-global-accelerator",
		"Hetzner-Falkenstein", "Hetzner-Ashburn", "Discord", "Zoom-eu"}) {
		t.Errorf("added %v", extra)
	}
	var ids []string
	for _, g := range p.Groups[len(def.Groups):] {
		ids = append(ids, g.ID)
	}
	if !slices.Equal(ids, []string{"aws-na", "aws-eu", "aws-global", "hetzner-na", "hetzner-eu", "services-voice"}) {
		t.Errorf("new groups %v", ids)
	}
	by := map[string]model.Target{}
	for _, tg := range p.Targets {
		by[tg.Name] = tg
	}
	if a := by["AWS-us-east-1"]; a.Host != "dynamodb.us-east-1.amazonaws.com" || a.Path != "/ping" || a.Group != "aws-na" || !a.Trace {
		t.Errorf("AWS-us-east-1 not replaced by the catalog one (trace kept): %+v", a)
	}
	if err := Validate(p, Limits{}); err != nil {
		t.Error(err)
	}
	// The catalog wins in either order; default's AWS group merges into
	// the catalog's aws group.
	p = resolve(t, r, true, "aws", "default")
	by = map[string]model.Target{}
	for _, tg := range p.Targets {
		by[tg.Name] = tg
	}
	if a := by["AWS-us-east-1"]; a.Host != "dynamodb.us-east-1.amazonaws.com" || a.Group != "aws" {
		t.Errorf("aws,default: %+v", a)
	}
	if len(p.Targets) != 4+84 || p.Groups[0].ID != "aws" || len(p.Groups) != 7 {
		t.Errorf("aws,default: %d targets, groups %v", len(p.Targets), groupSummary(p))
	}
	// A group emptied by replacements is dropped and orders renumbered.
	p = resolve(t, r, true, "default", "services-voice")
	if i := indexGroup(p.Groups, "common"); i < 0 {
		t.Error("common dropped though Google-Meet is left")
	}
	// Repeating a profile changes nothing.
	if q := resolve(t, r, false, "aws", "aws", "hyperscalers"); len(q.Targets) != 5+4 || len(q.Groups) != 2 {
		t.Errorf("aws,aws,hyperscalers: %v", groupSummary(q))
	}

	if _, err := r.Resolve([]string{"aws", "nope"}, ResolveOptions{}); err == nil || !strings.Contains(err.Error(), `unknown profile "nope"`) {
		t.Errorf("unknown: %v", err)
	}
	if _, err := r.Resolve(nil, ResolveOptions{}); err == nil {
		t.Error("empty list accepted")
	}
	if p := resolve(t, r, true, "aws"); indexGroup(p.Groups, PathGroup) >= 0 || len(p.Targets) != 4 {
		t.Error("noPath kept the path group")
	}
}

func TestGeoFilter(t *testing.T) {
	r := sampleRegistry(t)
	p, err := r.Resolve([]string{"clouds", "common"}, ResolveOptions{Geos: []string{"eu"}, NoPath: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := names(p.Targets); !slices.Equal(got, []string{"AWS-eu-central-1", "Hetzner-Falkenstein", "Zoom-eu"}) || p.Name != "clouds,common@eu" {
		t.Errorf("%s: %v", p.Name, got)
	}
	if got := groupSummary(p); !slices.Equal(got, []string{
		"aws-eu=Amazon Web Services · Europe/1", "hetzner-eu=Hetzner · Europe/1", "services-voice=Voice & video/1"}) {
		t.Errorf("groups %v", got)
	}
	// default targets count by their catalog entry's geo; the others go.
	p, err = r.Resolve([]string{"default"}, ResolveOptions{Geos: []string{"na"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := names(p.Targets[5:]); !slices.Equal(got, []string{"AWS-us-east-1", "AWS-us-west-2", "Hetzner-Ashburn"}) || p.Groups[0].ID != PathGroup {
		t.Errorf("default@na: %v", got)
	}
	if _, err := r.Resolve([]string{"aws"}, ResolveOptions{Geos: []string{"europe"}}); err == nil {
		t.Error("bad geo accepted")
	}
	g, err := ParseGeos(" Europe,eu, na ")
	if err != nil || !slices.Equal(g, []string{"eu", "na"}) {
		t.Errorf("ParseGeos: %v %v", g, err)
	}
	if _, err := ParseGeos("mars"); err == nil {
		t.Error("ParseGeos mars")
	}
}

func TestLimits(t *testing.T) {
	r := sampleRegistry(t)
	p := resolve(t, r, false, "default", "clouds") // 92 + 4 new
	err := Validate(p, Limits{MaxTargets: 50})
	if err == nil || !strings.Contains(err.Error(), "at most 50 allowed") || !strings.Contains(err.Error(), "--max-targets") {
		t.Errorf("limit: %v", err)
	}
	if err := Validate(p, Limits{MaxTargets: 96}); err != nil {
		t.Error(err)
	}
}

func TestSelectionSemantics(t *testing.T) {
	c := sampleCatalog(t)
	yml := `
profiles:
  - name: or-rules
    description: two rules
    select:
      - {providers: [hetzner], geos: [na]}
      - {tags: [voice], anycast: true}
      - {targets: [AWS-us-west-2, Nope-x]}
  - name: glob-exclude
    description: globs
    select: {providers: ["*"], exclude: ["AWS-us-*", Zoom-eu]}
  - name: tags
    description: target tags and provider categories
    select: {tags: [storage, chat]}
  - name: by-geo
    description: geo groups
    select: {kinds: [hyperscaler, cloud]}
    groups: geo
  - name: custom
    description: custom groups
    select: {geos: [eu, na]}
    groups: custom
    custom_groups:
      - {id: de, title: Germany-ish, match: [{targets: [Hetzner-Falkenstein]}, {tags: [storage]}]}
      - {id: us, title: US, match: {geos: [na]}}
  - name: ext
    description: extends
    extends: [tags, hetzner]
    select: {targets: [AWS-us-east-1]}
  - name: no-services
    description: exclude_providers
    select: {kinds: [service, cloud], exclude_providers: ["services-*"]}
  - name: nothing
    description: unknown provider and tag
    select: {providers: [akamai], tags: [none]}
  - name: aws
    description: overrides the provider profile
    select: {targets: [AWS-us-east-1]}
`
	r, err := NewRegistry(c, []byte(yml))
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string][]string{
		"or-rules":     {"AWS-us-west-2", "Hetzner-Ashburn", "Discord"},
		"glob-exclude": {"AWS-eu-central-1", "AWS-global-accelerator", "Hetzner-Falkenstein", "Hetzner-Ashburn", "Discord"},
		"tags":         {"AWS-eu-central-1", "Discord"},
		"nothing":      nil,
		"no-services":  {"Hetzner-Falkenstein", "Hetzner-Ashburn"},
		"aws":          {"AWS-us-east-1"},
		"ext":          {"AWS-eu-central-1", "Discord", "Hetzner-Falkenstein", "Hetzner-Ashburn", "AWS-us-east-1"},
	}
	for n, want := range cases {
		if got := names(resolve(t, r, true, n).Targets); !slices.Equal(got, want) {
			t.Errorf("%s: %v, want %v", n, got, want)
		}
	}
	if got := groupSummary(resolve(t, r, true, "by-geo")); !slices.Equal(got, []string{
		"geo-na=North America/3", "geo-eu=Europe/2", "geo-global=Global/1"}) {
		t.Errorf("by-geo %v", got)
	}
	if got := groupSummary(resolve(t, r, true, "custom")); !slices.Equal(got, []string{
		"de=Germany-ish/2", "us=US/3", "services-voice=Voice & video/1"}) {
		t.Errorf("custom %v", got)
	}
	if got := groupSummary(resolve(t, r, true, "ext")); !slices.Equal(got, []string{
		"aws=Amazon Web Services/2", "services-voice=Voice & video/1", "hetzner=Hetzner/2"}) {
		t.Errorf("ext %v", got)
	}
}

func TestRegistryErrors(t *testing.T) {
	c := sampleCatalog(t)
	cases := map[string]string{
		"reserved":                "profiles: [{name: default, description: d, select: {geos: [eu]}}]",
		"defined twice":           "profiles: [{name: a, description: d, select: {geos: [eu]}}, {name: a, description: d, select: {geos: [eu]}}]",
		"lowercase":               "profiles: [{name: Europe, description: d, select: {geos: [eu]}}]",
		"description is required": "profiles: [{name: a, select: {geos: [eu]}}]",
		"geo \"europe\"":          "profiles: [{name: a, description: d, select: {geos: [europe]}}]",
		"kind \"clouds\"":         "profiles: [{name: a, description: d, select: {kinds: [clouds]}}]",
		"groups \"region\"":       "profiles: [{name: a, description: d, select: {geos: [eu]}, groups: region}]",
		"needs custom_groups":     "profiles: [{name: a, description: d, select: {geos: [eu]}, groups: custom}]",
		"need groups: custom":     "profiles: [{name: a, description: d, select: {geos: [eu]}, custom_groups: [{id: x, title: X, match: {geos: [eu]}}]}]",
		"needs a title":           "profiles: [{name: a, description: d, select: {geos: [eu]}, groups: custom, custom_groups: [{id: x, match: {geos: [eu]}}]}]",
		"not \"path\"":            "profiles: [{name: a, description: d, select: {geos: [eu]}, groups: custom, custom_groups: [{id: path, title: P, match: {geos: [eu]}}]}]",
		"needs select or extends": "profiles: [{name: a, description: d}]",
		"extends unknown":         "profiles: [{name: a, description: d, extends: [nope]}]",
		"extends itself":          "profiles: [{name: a, description: d, extends: [b]}, {name: b, description: d, extends: [a]}]",
		"field geo not found":     "profiles: [{name: a, description: d, select: {geo: [eu]}}]",
		"field select2 not found": "profiles: [{name: a, description: d, select2: {}}]",
		"rule or a list":          "profiles: [{name: a, description: d, select: eu}]",
	}
	for want, yml := range cases {
		_, err := NewRegistry(c, []byte(yml))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: got %v", want, err)
		}
	}
}

func TestConfigExtendsNamed(t *testing.T) {
	useRegistry(t, sampleRegistry(t))
	p, err := Load(write(t, `
name: mine
extends: [aws, europe]
remove: [AWS-us-west-2]
override:
  - {name: Zoom-eu, interval: 30s}
add:
  - {name: router, host: 192.168.1.1, group: hetzner, kinds: [icmp]}
`))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Gateway", "ISP edge", "Cloudflare DNS 1.1.1.1", "Google DNS 8.8.8.8", "Quad9 DNS 9.9.9.9",
		"AWS-us-east-1", "AWS-eu-central-1", "AWS-global-accelerator", "Hetzner-Falkenstein", "Zoom-eu", "router"}
	if got := names(p.Targets); !slices.Equal(got, want) || p.Name != "mine" {
		t.Errorf("%s: %v", p.Name, got)
	}
	if p.Targets[9].Interval != 30e9 {
		t.Errorf("override: %+v", p.Targets[9])
	}
	if _, err := Load(write(t, "extends: [nope]\n")); err == nil || !strings.Contains(err.Error(), "unknown profile") {
		t.Errorf("unknown: %v", err)
	}
	// extends: [default] still works without the registry.
	p, err = Load(write(t, "extends: [default, common]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Targets) != 92+1 {
		t.Errorf("default+common: %d targets", len(p.Targets))
	}
}

// TestReadmeConfigExample loads the --config example of the README against
// the real catalog.
func TestReadmeConfigExample(t *testing.T) {
	r, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	if r.Definition("aws") == nil {
		t.Skip("no aws catalog")
	}
	p, err := Load(write(t, `
name: mine
extends: [aws, europe]
remove: [AWS-eu-south-2]
groups: [{id: home, title: Home}]
add:
  - {name: nas, host: 192.168.1.10, group: home, kinds: [icmp]}
`))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%d targets, %d groups", len(p.Targets), len(p.Groups))
}

// TestBuiltinProfiles is the sanity check over the real catalog: every
// profile resolves and validates within MaxTargetsCap. Empty selections
// and profiles over the default limit are only reported (-v).
func TestBuiltinProfiles(t *testing.T) {
	r, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range r.Definitions() {
		p, err := r.Resolve([]string{d.Name}, ResolveOptions{})
		if err != nil {
			t.Errorf("%s: %v", d.Name, err)
			continue
		}
		n := len(p.Targets) - len(pathTargets())
		switch {
		case n == 0:
			t.Logf("warning: profile %s selects no targets (catalog not there yet?)", d.Name)
		case len(p.Targets) > DefaultMaxTargets:
			t.Logf("note: profile %s has %d targets: needs --max-targets", d.Name, len(p.Targets))
		}
		if n > 0 {
			if err := Validate(p, Limits{MaxTargets: MaxTargetsCap}); err != nil {
				t.Errorf("%s: %v", d.Name, err)
			}
		}
		t.Logf("%-16s %4d targets %3d groups", d.Name, n, len(p.Groups)-1)
	}
}
