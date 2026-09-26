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
	if len(names) != 87 || len(p.Targets) != 87 {
		t.Fatalf("template has %d targets, default has %d; want 87", len(names), len(p.Targets))
	}
	want := map[string]string{}
	for i, n := range names {
		want[n] = hosts[i]
	}
	var got []string
	for _, tg := range p.Targets {
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

func TestDefaultGroups(t *testing.T) {
	p, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	titles := []string{
		"Common Services (<100ms is good for audio/video calls)", "DNS", "DevTunnels",
		"Various Dev Related Services", "Amazon Web Services", "Hetzner", "Google Cloud Platform",
	}
	if len(p.Groups) != len(titles) {
		t.Fatalf("%d groups", len(p.Groups))
	}
	for i, g := range p.Groups {
		if g.Title != titles[i] || g.Order != i+1 {
			t.Errorf("group %d = %+v, want title %q order %d", i, g, titles[i], i+1)
		}
	}
	count := map[string][]string{}
	for _, tg := range p.Targets {
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
  - {name: router, host: 192.168.1.1, group: home, kinds: [icmp], interval: 5s}
`)
	p, err := Load(f)
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "mine" || len(p.Targets) != 86 {
		t.Fatalf("name %q, %d targets", p.Name, len(p.Targets))
	}
	if len(p.Groups) != 8 || p.Groups[7].ID != "home" || p.Groups[7].Order != 8 || p.Groups[1].Title != "Resolvers" {
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
	if r.Host != "192.168.1.1" || r.Group != "home" || r.Interval != 5*time.Second || len(r.Kinds) != 1 {
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
	if len(p.Targets) != 2 || p.Targets[0].Port != 8443 || p.Groups[0].Order != 1 {
		t.Errorf("%+v", p)
	}
}

func TestLoadErrors(t *testing.T) {
	cases := map[string]string{
		"remove: no target":      "extends: [default]\nremove: [nope]\n",
		"override: no target":    "extends: [default]\noverride: [{name: nope, port: 1}]\n",
		"already exists":         "extends: [default]\nadd: [{name: Discord, host: x.com, group: dns}]\n",
		"need `extends":          "remove: [Discord]\n",
		"only [default]":         "extends: [other]\n",
		"not allowed with":       "extends: [default]\ntargets: [{name: a, host: b, group: dns}]\n",
		"unknown kind":           "extends: [default]\noverride: [{name: Discord, kinds: [udp]}]\n",
		"field bogus not found":  "extends: [default]\nbogus: 1\n",
		"interval":               "extends: [default]\noverride: [{name: Discord, interval: soon}]\n",
		"below the minimum":      "extends: [default]\noverride: [{name: Discord, interval: 1s}]\n",
		"unknown group":          "extends: [default]\nadd: [{name: x, host: x.com, group: nope}]\n",
		"empty profile":          "",
		"duplicate target name":  "groups: [{id: g}]\ntargets: [{name: a, host: a.com, group: g}, {name: a, host: b.com, group: g}]\n",
		"not a hostname or IPv4": "groups: [{id: g}]\ntargets: [{name: a, host: 'https://a.com/', group: g}]\n",
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
		{"unknown group", func(p *model.Profile) { p.Targets[0].Group = "x" }, Limits{}},
		{"duplicate group id", func(p *model.Profile) { p.Groups = append(p.Groups, p.Groups[0]) }, Limits{}},
		{"private address", func(p *model.Profile) { p.Targets[0].Host = "192.168.0.1" }, Limits{}},
		{"private address", func(p *model.Profile) { p.Targets[0].Host = "127.0.0.1" }, Limits{}},
		{"private address", func(p *model.Profile) {
			p.Targets[0].HostOverrides = map[model.ProbeKind]string{model.KindICMP: "10.1.2.3"}
		}, Limits{}},
		{"empty name", func(p *model.Profile) { p.Targets[0].Name = "" }, Limits{}},
		{"whitespace", func(p *model.Profile) { p.Targets[0].Name = "a b" }, Limits{}},
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
	p := base()
	p.Targets[0].Host = "192.168.0.1"
	if err := Validate(p, Limits{AllowPrivateIPs: true}); err != nil {
		t.Errorf("private IP with AllowPrivateIPs: %v", err)
	}
}
