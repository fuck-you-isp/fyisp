package profile

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

// templateTargets reads the legacy network_exporter target list.
func templateTargets(t *testing.T) (names, hosts []string) {
	t.Helper()
	b, err := os.ReadFile("testdata/network_exporter.yml.template")
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`-\s*name:\s*(\S+)\s*\n\s*host:\s*(\S+)`)
	for _, m := range re.FindAllStringSubmatch(string(b), -1) {
		names = append(names, m[1])
		hosts = append(hosts, m[2])
	}
	return names, hosts
}

func TestDefaultMatchesTemplate(t *testing.T) {
	p, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	if err := Validate(p, Limits{}); err != nil {
		t.Fatalf("default profile invalid: %v", err)
	}
	names, hosts := templateTargets(t)
	legacy := withoutPath(p.Targets)
	if len(names) != 87 || len(legacy) != 87 {
		t.Fatalf("template has %d targets, default has %d outside the path group; want 87", len(names), len(legacy))
	}
	want := map[string]string{}
	for i, n := range names {
		want[n] = hosts[i]
	}
	var got []string
	for _, tg := range legacy {
		got = append(got, tg.Name)
		h, ok := want[tg.Name]
		if !ok {
			t.Errorf("target %q not in template", tg.Name)
		} else if tg.Host != h {
			t.Errorf("target %q host %q, template %q", tg.Name, tg.Host, h)
		}
		if tg.Interval != 15*time.Second || len(tg.Kinds) != 3 {
			t.Errorf("target %q: interval %s kinds %v", tg.Name, tg.Interval, tg.Kinds)
		}
	}
	slices.Sort(got)
	slices.Sort(names)
	if !slices.Equal(got, names) {
		t.Errorf("names differ:\n got %v\nwant %v", got, names)
	}
}

func withoutPath(ts []model.Target) []model.Target {
	var out []model.Target
	for _, tg := range ts {
		if tg.Group != PathGroup {
			out = append(out, tg)
		}
	}
	return out
}

func TestDefaultGroups(t *testing.T) {
	p, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	titles := []string{
		"Common Services (<100ms is good for audio/video calls)", "DNS", "DevTunnels",
		"Various Dev Related Services", "Amazon Web Services", "Hetzner", "Google Cloud Platform",
	}
	if len(p.Groups) != len(titles)+1 {
		t.Fatalf("%d groups", len(p.Groups))
	}
	if g := p.Groups[0]; g.ID != PathGroup || g.Title != "Network path" || g.Order != 0 {
		t.Errorf("first group %+v, want the network path group with order 0", g)
	}
	for i, g := range p.Groups[1:] {
		if g.Title != titles[i] || g.Order != i+1 {
			t.Errorf("group %d = %+v, want title %q order %d", i, g, titles[i], i+1)
		}
	}
	count := map[string][]string{}
	for _, tg := range withoutPath(p.Targets) {
		count[tg.Group] = append(count[tg.Group], tg.Name)
	}
	if c := count["common"]; !slices.Equal(c, []string{"Google-Meet", "Microsoft-Teams", "Discord"}) {
		t.Errorf("common = %v (the old panel-1 bug pulled AWS in)", c)
	}
	wantPrefix := map[string]*regexp.Regexp{
		"dns": regexp.MustCompile(`-dns$`), "devtunnels": regexp.MustCompile(`^DevTunnel-`),
		"dev": regexp.MustCompile(`^dev-`), "aws": regexp.MustCompile(`^AWS-`),
		"hetzner": regexp.MustCompile(`^Hetzner-`), "gcp": regexp.MustCompile(`^GCP-`),
	}
	sizes := map[string]int{"dns": 4, "devtunnels": 13, "dev": 2, "aws": 34, "hetzner": 6, "gcp": 25}
	for g, re := range wantPrefix {
		if len(count[g]) != sizes[g] {
			t.Errorf("group %s has %d targets, want %d", g, len(count[g]), sizes[g])
		}
		for _, n := range count[g] {
			if !re.MatchString(n) {
				t.Errorf("group %s contains %s", g, n)
			}
		}
	}
}

func TestDefaultOverrides(t *testing.T) {
	p, _ := Default()
	byName := map[string]model.Target{}
	for _, tg := range p.Targets {
		byName[tg.Name] = tg
	}
	gh := byName["dev-GitHub"]
	if gh.Path != "/robots.txt" {
		t.Errorf("dev-GitHub path %q", gh.Path)
	}
	m := byName["Google-Meet"]
	if m.HostFor(model.KindHTTPS) != "meet.google.com" || m.HostFor(model.KindTCP) != "meet.google.com" ||
		m.HostFor(model.KindICMP) != "lens.l.google.com" {
		t.Errorf("Google-Meet hosts: %v / %s", m.HostOverrides, m.Host)
	}
	// Default returns independent copies.
	p.Targets[0].Name = "changed"
	q, _ := Default()
	if q.Targets[0].Name == "changed" {
		t.Error("Default shares state between calls")
	}
}

func write(t *testing.T, s string) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "p.yml")
	if err := os.WriteFile(f, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestLoadExtends(t *testing.T) {
	f := write(t, `
name: mine
extends: [default]
groups:
  - {id: home, title: Home}
  - {id: dns, title: Resolvers}
remove: [Discord, AWS-us-east-1]
override:
  - {name: dev-GitHub, interval: 30s, kinds: [https, icmp]}
  - {name: Google-Meet, host_overrides: {icmp: meet.google.com}}
add:
  - {name: router, host: 192.168.1.1, group: home, kinds: [icmp], interval: 6s}
`)
	p, err := Load(f)
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "mine" || len(p.Targets) != 86+5 {
		t.Fatalf("name %q, %d targets", p.Name, len(p.Targets))
	}
	if len(p.Groups) != 9 || p.Groups[0].ID != PathGroup || p.Groups[8].ID != "home" || p.Groups[8].Order != 8 || p.Groups[2].Title != "Resolvers" {
		t.Errorf("groups %+v", p.Groups)
	}
	by := map[string]model.Target{}
	for _, tg := range p.Targets {
		by[tg.Name] = tg
	}
	if _, ok := by["Discord"]; ok {
		t.Error("Discord not removed")
	}
	gh := by["dev-GitHub"]
	if gh.Interval != 30*time.Second || !slices.Equal(gh.Kinds, []model.ProbeKind{model.KindHTTPS, model.KindICMP}) ||
		gh.Path != "/robots.txt" || gh.Host != "github.com" {
		t.Errorf("dev-GitHub override: %+v", gh)
	}
	if h := by["Google-Meet"].HostFor(model.KindICMP); h != "meet.google.com" {
		t.Errorf("Google-Meet icmp host %s", h)
	}
	r := by["router"]
	if r.Host != "192.168.1.1" || r.Group != "home" || r.Interval != 6*time.Second || len(r.Kinds) != 1 {
		t.Errorf("router %+v", r)
	}
}

func TestLoadStandalone(t *testing.T) {
	f := write(t, `
name: lab
groups: [{id: lab, title: Lab}]
targets:
  - {name: gw, host: 10.0.0.1, group: lab, port: 8443, path: /health}
  - {name: one, host: one.one.one.one, group: lab}
`)
	p, err := Load(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Targets) != 2+5 || p.Targets[5].Port != 8443 || p.Groups[1].Order != 1 || p.Groups[0].ID != PathGroup {
		t.Errorf("%+v", p)
	}
}

func TestPathGroup(t *testing.T) {
	p, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	type row struct {
		name, host, layer string
		kinds             []model.ProbeKind
		iv                time.Duration
	}
	icmp := []model.ProbeKind{model.KindICMP}
	icmpTCP := []model.ProbeKind{model.KindICMP, model.KindTCP}
	want := []row{
		{"Gateway", "@gateway", "gateway", icmp, 3 * time.Second},
		{"ISP edge", "@isp-edge", "isp-edge", icmp, 3 * time.Second},
		{"Cloudflare DNS 1.1.1.1", "1.1.1.1", "anycast", icmpTCP, 15 * time.Second},
		{"Google DNS 8.8.8.8", "8.8.8.8", "anycast", icmpTCP, 15 * time.Second},
		{"Quad9 DNS 9.9.9.9", "9.9.9.9", "anycast", icmpTCP, 15 * time.Second},
	}
	for i, w := range want {
		tg := p.Targets[i]
		if tg.Name != w.name || tg.Host != w.host || tg.Layer != w.layer || tg.Group != PathGroup ||
			!slices.Equal(tg.Kinds, w.kinds) || tg.Interval != w.iv {
			t.Errorf("path target %d = %+v, want %+v", i, tg, w)
		}
	}
	for _, tg := range p.Targets[len(want):] {
		if tg.Layer != "" || tg.Group == PathGroup {
			t.Errorf("non-path target %q in layer %q group %q", tg.Name, tg.Layer, tg.Group)
		}
	}
	// Public (shared) limits accept the special hosts and the 3s interval.
	if err := Validate(p, Limits{}); err != nil {
		t.Error(err)
	}

	// Opting out, standalone and extending.
	for _, body := range []string{
		"path: false\ngroups: [{id: g}]\ntargets: [{name: a, host: a.com, group: g}]\n",
		"path: false\nextends: [default]\n",
	} {
		p, err := Load(write(t, body))
		if err != nil {
			t.Fatal(err)
		}
		if indexGroup(p.Groups, PathGroup) >= 0 || indexTarget(p.Targets, "Gateway") >= 0 {
			t.Errorf("path: false still has the path group: %+v", p.Groups)
		}
	}
	p, err = Load(write(t, "path: true\nextends: [default]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Targets) != 92 || p.Groups[0].ID != PathGroup {
		t.Errorf("path: true: %d targets, groups %+v", len(p.Targets), p.Groups)
	}
	// Negative orders still sort after the path group.
	p, err = Load(write(t, "groups: [{id: g, order: -5}]\ntargets: [{name: a, host: a.com, group: g}]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if p.Groups[0].Order >= -5 {
		t.Errorf("path group order %d, other group -5", p.Groups[0].Order)
	}
	// A user target on a special host.
	p, err = Load(write(t, "path: false\ngroups: [{id: g}]\ntargets: [{name: router, host: '@gateway', group: g, kinds: [icmp], interval: 3s}]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if p.Targets[0].Host != model.HostGateway {
		t.Errorf("%+v", p.Targets[0])
	}
}

func TestLoadErrors(t *testing.T) {
	cases := map[string]string{
		"remove: no target":            "extends: [default]\nremove: [nope]\n",
		"override: no target":          "extends: [default]\noverride: [{name: nope, port: 1}]\n",
		"already exists":               "extends: [default]\nadd: [{name: Discord, host: x.com, group: dns}]\n",
		"need `extends":                "remove: [Discord]\n",
		"only [default]":               "extends: [other]\n",
		"not allowed with":             "extends: [default]\ntargets: [{name: a, host: b, group: dns}]\n",
		"unknown kind":                 "extends: [default]\noverride: [{name: Discord, kinds: [udp]}]\n",
		"field bogus not found":        "extends: [default]\nbogus: 1\n",
		"interval":                     "extends: [default]\noverride: [{name: Discord, interval: soon}]\n",
		"below the minimum":            "extends: [default]\noverride: [{name: Discord, interval: 1s}]\n",
		"unknown group":                "extends: [default]\nadd: [{name: x, host: x.com, group: nope}]\n",
		"empty profile":                "",
		"duplicate target name":        "groups: [{id: g}]\ntargets: [{name: a, host: a.com, group: g}, {name: a, host: b.com, group: g}]\n",
		"not a hostname or IPv4":       "groups: [{id: g}]\ntargets: [{name: a, host: 'https://a.com/', group: g}]\n",
		"ICMP only":                    "groups: [{id: g}]\ntargets: [{name: a, host: '@isp-edge', group: g}]\n",
		"reserved":                     "groups: [{id: path}]\ntargets: [{name: a, host: a.com, group: path}]\n",
		"name \"Gateway\" is reserved": "extends: [default]\nadd: [{name: Gateway, host: a.com, group: dns}]\n",
		"below the minimum 5s":         "groups: [{id: g}]\ntargets: [{name: a, host: a.com, group: g, interval: 3s}]\n",
		"not a hostname":               "groups: [{id: g}]\ntargets: [{name: a, host: '@router', group: g, kinds: [icmp]}]\n",
	}
	for want, body := range cases {
		_, err := Load(write(t, body))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: got err %v", want, err)
		}
	}
}

func TestValidate(t *testing.T) {
	base := func() *model.Profile {
		return &model.Profile{
			Groups:  []model.Group{{ID: "g", Title: "G"}},
			Targets: []model.Target{{Name: "a", Host: "a.example", Group: "g", Interval: 15 * time.Second}},
		}
	}
	if err := Validate(base(), Limits{}); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		want string
		mod  func(p *model.Profile)
		l    Limits
	}{
		{"duplicate target name", func(p *model.Profile) { p.Targets = append(p.Targets, p.Targets[0]) }, Limits{}},
		{"at most 300", func(p *model.Profile) {
			for i := range 300 {
				tg := p.Targets[0]
				tg.Name = fmt.Sprintf("t%d", i)
				p.Targets = append(p.Targets, tg)
			}
		}, Limits{}},
		{"at most 1 allowed", func(p *model.Profile) {
			p.Targets = append(p.Targets, model.Target{Name: "b", Host: "b.example", Group: "g"})
		}, Limits{MaxTargets: 1}},
		{"below the minimum 5s", func(p *model.Profile) { p.Targets[0].Interval = 4 * time.Second }, Limits{}},
		{"below the minimum 10s", func(p *model.Profile) { p.Targets[0].Interval = 9 * time.Second }, Limits{MinIntervalSecs: 10}},
		{"must be whole seconds", func(p *model.Profile) { p.Targets[0].Interval = 10 * time.Second }, Limits{}},
		{"must be whole seconds", func(p *model.Profile) { p.Targets[0].Interval = 7500 * time.Millisecond }, Limits{}},
		{"must be whole seconds", func(p *model.Profile) { p.Targets[0].Interval = 21 * time.Second }, Limits{}},
		{"must be whole seconds", func(p *model.Profile) { p.Targets[0].Interval = 2 * time.Hour }, Limits{}},
		{"unknown group", func(p *model.Profile) { p.Targets[0].Group = "x" }, Limits{}},
		{"duplicate group id", func(p *model.Profile) { p.Groups = append(p.Groups, p.Groups[0]) }, Limits{}},
		{"private address", func(p *model.Profile) { p.Targets[0].Host = "192.168.0.1" }, Limits{}},
		{"private address", func(p *model.Profile) { p.Targets[0].Host = "127.0.0.1" }, Limits{}},
		{"private address", func(p *model.Profile) {
			p.Targets[0].HostOverrides = map[model.ProbeKind]string{model.KindICMP: "10.1.2.3"}
		}, Limits{}},
		{"empty name", func(p *model.Profile) { p.Targets[0].Name = "" }, Limits{}},
		{"whitespace", func(p *model.Profile) { p.Targets[0].Name = "a\tb" }, Limits{}},
		{"whitespace", func(p *model.Profile) { p.Targets[0].Name = " a" }, Limits{}},
		{"whitespace", func(p *model.Profile) { p.Targets[0].Name = "a  b" }, Limits{}},
		{"below the minimum 5s", func(p *model.Profile) { p.Targets[0].Interval = 3 * time.Second }, Limits{}},
		{"ICMP only", func(p *model.Profile) { p.Targets[0].Host = model.HostGateway }, Limits{}},
		{"listed twice", func(p *model.Profile) { p.Targets[0].Kinds = []model.ProbeKind{1, 1} }, Limits{}},
		{"unknown kind", func(p *model.Profile) { p.Targets[0].Kinds = []model.ProbeKind{9} }, Limits{}},
		{"must start with /", func(p *model.Profile) { p.Targets[0].Path = "x" }, Limits{}},
		{"out of range", func(p *model.Profile) { p.Targets[0].Port = 70000 }, Limits{}},
		{"no targets", func(p *model.Profile) { p.Targets = nil }, Limits{}},
	}
	for _, c := range cases {
		p := base()
		c.mod(p)
		err := Validate(p, c.l)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: got %v", c.want, err)
		}
	}
	for _, iv := range []time.Duration{6 * time.Second, 9 * time.Second, 12 * time.Second, 15 * time.Second, 30 * time.Second, time.Minute, 5 * time.Minute, time.Hour} {
		p := base()
		p.Targets[0].Interval = iv
		if err := Validate(p, Limits{}); err != nil {
			t.Errorf("interval %s: %v", iv, err)
		}
	}
	p := base()
	p.Targets[0].Name = "My router"
	p.Targets[0].Host = model.HostGateway
	p.Targets[0].Kinds = []model.ProbeKind{model.KindICMP}
	p.Targets[0].Interval = 3 * time.Second
	if err := Validate(p, Limits{}); err != nil {
		t.Errorf("special host with 3s interval and a spaced name: %v", err)
	}
	p = base()
	p.Targets[0].Host = "192.168.0.1"
	if err := Validate(p, Limits{AllowPrivateIPs: true}); err != nil {
		t.Errorf("private IP with AllowPrivateIPs: %v", err)
	}
}

func TestTrace(t *testing.T) {
	p, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	var traced []string
	perGroup := map[string]int{}
	for _, tg := range p.Targets {
		if tg.Trace {
			traced = append(traced, tg.Name)
			perGroup[tg.Group]++
		}
	}
	want := []string{"Discord", "cloudflare-dns", "DevTunnel-Global", "dev-GitHub", "AWS-us-east-1", "Hetzner-Falkenstein", "GCP-us-central1"}
	if !slices.Equal(traced, want) {
		t.Errorf("traced targets %v, want %v", traced, want)
	}
	for _, g := range p.Groups {
		if g.ID != PathGroup && perGroup[g.ID] != 1 {
			t.Errorf("group %s: %d traced targets, want 1", g.ID, perGroup[g.ID])
		}
	}

	f := write(t, `
extends: [default]
override:
  - {name: Discord, trace: false}
  - {name: google-dns, trace: true}
  - {name: dev-GitHub, interval: 30s}
add:
  - {name: router, host: 192.168.1.1, group: common, kinds: [icmp], trace: true}
`)
	p, err = Load(f)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, tg := range p.Targets {
		got[tg.Name] = tg.Trace
	}
	if got["Discord"] || !got["google-dns"] || !got["router"] || !got["dev-GitHub"] {
		t.Errorf("trace after override/add: %v", got)
	}

	p = &model.Profile{Groups: []model.Group{{ID: "g"}}}
	for i := range MaxTraced + 1 {
		p.Targets = append(p.Targets, model.Target{Name: fmt.Sprintf("t%d", i), Host: "a.example", Group: "g", Trace: true})
	}
	if err := Validate(p, Limits{}); err == nil || !strings.Contains(err.Error(), "trace: true, at most 20") {
		t.Errorf("21 traced targets: %v", err)
	}
	p.Targets[0].Trace = false
	if err := Validate(p, Limits{}); err != nil {
		t.Errorf("20 traced targets: %v", err)
	}
}
