package profile

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

// TestDefaultMatchesCatalog checks that every default target is its
// catalog entry, in its default.yml group, with only the trace added.
func TestDefaultMatchesCatalog(t *testing.T) {
	p, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	if err := Validate(p, Limits{}); err != nil {
		t.Fatalf("default profile invalid: %v", err)
	}
	r, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	traced := map[string]bool{}
	for _, tg := range withoutPath(p.Targets) {
		ct, ok := r.Catalog.Target(tg.Name)
		if !ok {
			t.Errorf("%s: not in the catalog", tg.Name)
			continue
		}
		want := ct.toTarget(tg.Group, r.Catalog.Provider(ct.Provider))
		want.Trace = tg.Trace
		if !reflect.DeepEqual(tg, want) {
			t.Errorf("%s:\n got %+v\nwant %+v", tg.Name, tg, want)
		}
		traced[tg.Name] = tg.Trace
	}
	if n := len(withoutPath(p.Targets)); n != 22 {
		t.Errorf("%d targets, want 22", n)
	}
	for n, on := range traced {
		if on != (n == "Microsoft-Teams" || n == "CDN-Cloudflare") {
			t.Errorf("%s: trace %v", n, on)
		}
	}
	// default.yml names targets; everything else comes from the catalog.
	old := defaultYAML
	t.Cleanup(func() { defaultYAML = old })
	for yml, want := range map[string]string{
		"targets: [{name: Zoom, group: g, host: zoom.us}]": "only name, group and trace",
		"targets: [{name: Zoom, group: g, tags: [x]}]":     "only name, group and trace",
		"targets: [{name: Nope, group: g}]":                "not a probeable catalog target",
		"targets: [{name: Zoom, group: g, interval: 30s}]": "only name, group and trace",
	} {
		defaultYAML = []byte("groups: [{id: g, title: G}]\n" + yml + "\n")
		if _, err := Default(); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", yml, err)
		}
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
	titles := []string{"Call path", "Call & chat apps", "Work apps", "Websites"}
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
	want := map[string][]string{
		"calls":     {"Meet-STUN", "Cloudflare-STUN", "Cloudflare-UDP53", "Twilio-STUN-global"},
		"call-apps": {"Microsoft-Teams", "Zoom", "Google-Meet", "Slack", "Webex", "Discord", "WhatsApp-Chat"},
		"work":      {"M365-Outlook", "M365-Portal", "M365-Login", "Google-Gmail", "Google-Docs", "Google-Drive"},
		"web":       {"CDN-Cloudflare", "CDN-Akamai", "CDN-Fastly", "CDN-CloudFront", "CDN-Google"},
	}
	if !reflect.DeepEqual(count, want) {
		t.Errorf("groups %v", count)
	}
}

func TestDefaultOverrides(t *testing.T) {
	p, _ := Default()
	byName := map[string]model.Target{}
	for _, tg := range p.Targets {
		byName[tg.Name] = tg
	}
	m := byName["Google-Meet"]
	if m.HostFor(model.KindHTTPS) != "meet.google.com" || m.HostFor(model.KindTCP) != "meet.google.com" ||
		m.HostFor(model.KindICMP) != "lens.l.google.com" {
		t.Errorf("Google-Meet hosts: %v / %s", m.HostOverrides, m.Host)
	}
	// Default returns independent copies.
	p.Targets[0].Name = "changed"
	byName["Google-Meet"].Tags[0] = "changed"
	byName["Google-Meet"].HostOverrides[model.KindTCP] = "changed"
	q, _ := Default()
	for _, tg := range q.Targets {
		if tg.Name == "changed" || slices.Contains(tg.Tags, "changed") || tg.HostOverrides[model.KindTCP] == "changed" {
			t.Errorf("Default shares state between calls: %+v", tg)
		}
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
  - {id: web, title: Sites}
remove: [Discord, Zoom]
override:
  - {name: CDN-Google, interval: 30s, kinds: [https, icmp]}
  - {name: Google-Meet, host_overrides: {icmp: meet.google.com}}
add:
  - {name: router, host: 192.168.1.1, group: home, kinds: [icmp], interval: 6s}
`)
	p, err := Load(f)
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "mine" || len(p.Targets) != 21+4 {
		t.Fatalf("name %q, %d targets", p.Name, len(p.Targets))
	}
	if len(p.Groups) != 6 || p.Groups[0].ID != PathGroup || p.Groups[5].ID != "home" || p.Groups[5].Order != 5 || p.Groups[4].Title != "Sites" {
		t.Errorf("groups %+v", p.Groups)
	}
	by := map[string]model.Target{}
	for _, tg := range p.Targets {
		by[tg.Name] = tg
	}
	if _, ok := by["Discord"]; ok {
		t.Error("Discord not removed")
	}
	cg := by["CDN-Google"]
	if cg.Interval != 30*time.Second || !slices.Equal(cg.Kinds, []model.ProbeKind{model.KindHTTPS, model.KindICMP}) ||
		cg.Path != "/generate_204" || cg.Host != "www.gstatic.com" {
		t.Errorf("CDN-Google override: %+v", cg)
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
	if len(p.Targets) != 2+4 || p.Targets[4].Port != 8443 || p.Groups[1].Order != 1 || p.Groups[0].ID != PathGroup {
		t.Errorf("%+v", p)
	}
	// Targets without kinds get a TCP connect only.
	for _, tg := range p.Targets[5:] {
		if !slices.Equal(tg.Kinds, []model.ProbeKind{model.KindTCP}) {
			t.Errorf("%s: kinds %v, want [tcp]", tg.Name, tg.Kinds)
		}
	}
}

// TestShippedProfilesTCPOnly checks that the default profile and every
// named profile probe their targets over TCP only (or STUN over UDP for the
// shipped call-path targets, see TestShippedUDP), while the network path
// keeps ICMP.
func TestShippedProfilesTCPOnly(t *testing.T) {
	r, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	def, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	profiles := map[string]*model.Profile{}
	for _, d := range r.Definitions() {
		p, err := r.Resolve([]string{d.Name}, ResolveOptions{})
		if err != nil {
			t.Fatalf("%s: %v", d.Name, err)
		}
		profiles[d.Name] = p
	}
	if profiles["all"] == nil || len(profiles["all"].Targets) < 1000 {
		t.Fatal("profile all missing or small")
	}
	tcp := []model.ProbeKind{model.KindTCP}
	for name, p := range profiles {
		pathICMP := 0
		for _, tg := range p.Targets {
			if tg.Group == PathGroup {
				if slices.Contains(tg.Kinds, model.KindICMP) {
					pathICMP++
				}
				continue
			}
			if tg.IsUDP() && isShippedUDP(tg) && slices.Contains(tg.Tags, "udp-path") {
				continue
			}
			if !slices.Equal(tg.ProbeKinds(), tcp) {
				t.Errorf("%s: target %s kinds %v, want [tcp]", name, tg.Name, tg.ProbeKinds())
			}
		}
		if pathICMP != 4 {
			t.Errorf("%s: %d path targets with ICMP, want 4", name, pathICMP)
		}
	}
	for _, tg := range def.Targets {
		if tg.Name == "Google-Meet" && tg.HostFor(model.KindTCP) != "meet.google.com" {
			t.Errorf("Google-Meet TCP host %s, want meet.google.com", tg.HostFor(model.KindTCP))
		}
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
	if len(p.Targets) != 22+4 || p.Groups[0].ID != PathGroup {
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
		"unknown profile \"other\"":    "extends: [other]\n",
		"not allowed with":             "extends: [default]\ntargets: [{name: a, host: b, group: dns}]\n",
		"unknown kind":                 "extends: [default]\noverride: [{name: Discord, kinds: [sctp]}]\n",
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
	// No target limit by default.
	big := base()
	for i := range 2000 {
		tg := big.Targets[0]
		tg.Name = fmt.Sprintf("t%d", i)
		big.Targets = append(big.Targets, tg)
	}
	if err := Validate(big, Limits{}); err != nil {
		t.Errorf("2001 targets: %v", err)
	}
	cases := []struct {
		want string
		mod  func(p *model.Profile)
		l    Limits
	}{
		{"duplicate target name", func(p *model.Profile) { p.Targets = append(p.Targets, p.Targets[0]) }, Limits{}},
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
	want := []string{"Microsoft-Teams", "CDN-Cloudflare"}
	if !slices.Equal(traced, want) {
		t.Errorf("traced targets %v, want %v", traced, want)
	}
	// One trace for the call apps and one for the websites: work apps share
	// their paths with them (Microsoft's and Google's edges).
	for _, g := range p.Groups {
		if w := map[string]int{"call-apps": 1, "web": 1}[g.ID]; perGroup[g.ID] != w {
			t.Errorf("group %s: %d traced targets, want %d", g.ID, perGroup[g.ID], w)
		}
	}

	f := write(t, `
extends: [default]
override:
  - {name: Microsoft-Teams, trace: false}
  - {name: Zoom, trace: true}
  - {name: CDN-Cloudflare, kinds: [https]}
add:
  - {name: router, host: 192.168.1.1, group: call-apps, kinds: [icmp], trace: true}
`)
	p, err = Load(f)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, tg := range p.Targets {
		got[tg.Name] = tg.Trace
	}
	if got["Microsoft-Teams"] || !got["Zoom"] || !got["router"] || !got["CDN-Cloudflare"] {
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

// TestShippedUDP pins the STUN targets: default has four (no Teams relay:
// it is opt-in), calls has every STUN target plus the call apps over TCP.
func TestShippedUDP(t *testing.T) {
	r, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	udpNames := func(p *model.Profile) []string {
		var out []string
		for _, tg := range p.Targets {
			if tg.IsUDP() {
				out = append(out, tg.Name)
			}
		}
		return out
	}
	def, _ := Default()
	if got := udpNames(def); !slices.Equal(got, []string{"Meet-STUN", "Cloudflare-STUN", "Cloudflare-UDP53", "Twilio-STUN-global"}) {
		t.Errorf("default UDP targets %v", got)
	}
	calls, err := r.Resolve([]string{"calls"}, ResolveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := Validate(calls, Limits{}); err != nil {
		t.Fatal(err)
	}
	want := []string{"Meet-STUN", "Meet-STUN-3478", "Cloudflare-STUN", "Cloudflare-UDP53", "Twilio-STUN-global", "Teams-Relay",
		"Twilio-us1", "Twilio-us2", "Twilio-ie1", "Twilio-de1", "Twilio-sg1", "Twilio-jp1", "Twilio-au1", "Twilio-br1", "Twilio-in1"}
	if got := udpNames(calls); !slices.Equal(got, want) {
		t.Errorf("calls UDP targets %v", got)
	}
	apps := map[string]bool{}
	ivs := map[string]time.Duration{}
	for _, tg := range calls.Targets {
		ivs[tg.Name] = tg.Interval
		for _, tag := range tg.Tags {
			if a, ok := strings.CutPrefix(tag, "app:"); ok && !tg.IsUDP() {
				apps[a] = true
			}
		}
		if tg.IsUDP() && (tg.Port == 0 || !slices.Contains(tg.Tags, "udp-path")) {
			t.Errorf("%s: port %d tags %v", tg.Name, tg.Port, tg.Tags)
		}
	}
	for _, a := range []string{"teams", "meet", "zoom", "slack", "discord", "whatsapp", "webex"} {
		if !apps[a] {
			t.Errorf("calls: no TCP target tagged app:%s", a)
		}
	}
	if ivs["Meet-STUN"] != 5*time.Second || ivs["Teams-Relay"] != 5*time.Second || ivs["Cloudflare-UDP53"] != 15*time.Second || ivs["Twilio-jp1"] != 30*time.Second {
		t.Errorf("intervals %v", ivs)
	}
	if g := calls.Groups; len(g) != 4 || g[1].ID != "calls-udp" || g[2].ID != "calls-regions" || g[3].ID != "calls-apps" {
		t.Errorf("calls groups %+v", g)
	}
	// all selects them too; default+calls keeps default's four in its own
	// group and adds the rest to calls'.
	all, _ := r.Resolve([]string{"all"}, ResolveOptions{})
	if n := len(udpNames(all)); n != len(want) {
		t.Errorf("all: %d UDP targets", n)
	}
	both, _ := r.Resolve([]string{"default", "calls"}, ResolveOptions{})
	if err := Validate(both, Limits{}); err != nil {
		t.Fatal(err)
	}
	if got := udpNames(both); len(got) != len(want) || indexGroup(both.Groups, "calls") != 1 || indexGroup(both.Groups, "calls-udp") < 0 {
		t.Errorf("default,calls: %v groups %v", got, both.Groups)
	}
	// Catalog tags reach the default's targets.
	for _, tg := range def.Targets {
		if tg.Name == "Microsoft-Teams" && !slices.Equal(tg.Tags, []string{"voice", "video", "signaling", "app:teams", "home:calls", "tile:teams"}) {
			t.Errorf("default Microsoft-Teams tags %v", tg.Tags)
		}
	}
}

func TestValidateUDP(t *testing.T) {
	udp := []model.ProbeKind{model.KindUDP}
	prof := func(ts ...model.Target) *model.Profile {
		p := &model.Profile{Groups: []model.Group{{ID: "g"}}}
		for _, tg := range ts {
			tg.Group = "g"
			p.Targets = append(p.Targets, tg)
		}
		return p
	}
	ok := model.Target{Name: "mine", Host: "stun.example.com", Port: 3478, Kinds: udp, Interval: 6 * time.Second}
	if err := Validate(prof(ok), Limits{}); err != nil {
		t.Errorf("custom 6s: %v", err)
	}
	// The shipped targets may probe every 5s, even with a stricter limit.
	def, _ := Default()
	if err := Validate(def, Limits{MinIntervalSecs: 15}); err != nil && strings.Contains(err.Error(), "STUN") {
		t.Errorf("shipped: %v", err)
	}
	ship := model.Target{Name: "Meet-STUN", Host: "stun.l.google.com", Port: 19302, Kinds: udp, Interval: 5 * time.Second}
	if err := Validate(prof(ship), Limits{MinIntervalSecs: 15}); err != nil {
		t.Errorf("shipped at 5s: %v", err)
	}
	with := func(f func(*model.Target)) model.Target { tg := ok; f(&tg); return tg }
	cases := map[string]*model.Profile{
		"below the minimum 6s":      prof(with(func(tg *model.Target) { tg.Interval = 5 * time.Second })),
		"below the minimum 15s":     prof(with(func(tg *model.Target) { tg.Interval = 10 * time.Second })),
		"needs a port":              prof(with(func(tg *model.Target) { tg.Port = 0 })),
		"cannot be combined":        prof(with(func(tg *model.Target) { tg.Kinds = []model.ProbeKind{model.KindUDP, model.KindTCP} })),
		"divide 1h evenly":          prof(with(func(tg *model.Target) { tg.Interval = 7 * time.Second })),
		"private address":           prof(with(func(tg *model.Target) { tg.Host = "127.0.0.1" })),
		"unknown kind 6":            prof(with(func(tg *model.Target) { tg.Kinds = []model.ProbeKind{model.KindUDPJitter} })),
		"host_overrides: unknown":   prof(with(func(tg *model.Target) { tg.HostOverrides = map[model.ProbeKind]string{model.KindUDP: "x.com"} })),
		"probed with ICMP only":     prof(with(func(tg *model.Target) { tg.Host = model.HostGateway })),
		"shipped name, faster host": prof(with(func(tg *model.Target) { tg.Name, tg.Interval = "Meet-STUN", 5*time.Second })),
	}
	lim := map[string]Limits{"below the minimum 15s": {MinIntervalSecs: 15}}
	for want, p := range cases {
		err := Validate(p, lim[want])
		if want == "shipped name, faster host" {
			want = "below the minimum 6s"
		}
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v", want, err)
		}
	}
	// At most MaxCustomUDP custom UDP targets; shipped ones do not count.
	var many []model.Target
	for i := range MaxCustomUDP + 1 {
		tg := ok
		tg.Name = fmt.Sprintf("u%d", i)
		many = append(many, tg)
	}
	if err := Validate(prof(many...), Limits{}); err == nil || !strings.Contains(err.Error(), "33 UDP targets, at most 32") {
		t.Errorf("33 UDP: %v", err)
	}
	if err := Validate(prof(append(many[:MaxCustomUDP], ship)...), Limits{}); err != nil {
		t.Errorf("32 custom + 1 shipped: %v", err)
	}
	// Private hosts only with AllowPrivateIPs (local files).
	lan := with(func(tg *model.Target) { tg.Host = "192.168.1.1" })
	if err := Validate(prof(lan), Limits{AllowPrivateIPs: true}); err != nil {
		t.Errorf("private with AllowPrivateIPs: %v", err)
	}
	// A --config file: tags, kinds [udp], and the limits.
	p, err := Load(write(t, `
extends: [default]
add:
  - {name: my-stun, host: stun.example.com, port: 3478, group: calls, kinds: [udp], interval: 10s, tags: [udp-path, app:mine]}
`))
	if err != nil {
		t.Fatal(err)
	}
	if tg := p.Targets[indexTarget(p.Targets, "my-stun")]; !tg.IsUDP() || tg.Interval != 10*time.Second || !slices.Equal(tg.Tags, []string{"udp-path", "app:mine"}) {
		t.Errorf("%+v", tg)
	}
	if _, err := Load(write(t, "extends: [default]\nadd: [{name: s, host: stun.example.com, port: 3478, group: calls, kinds: [udp], interval: 3s}]\n")); err == nil ||
		!strings.Contains(err.Error(), "below the minimum 6s") {
		t.Errorf("3s: %v", err)
	}
	// Overriding a shipped target's host makes it custom: the 6s floor applies.
	if _, err := Load(write(t, "extends: [default]\noverride: [{name: Meet-STUN, host: stun.example.com}]\n")); err == nil ||
		!strings.Contains(err.Error(), "below the minimum 6s") {
		t.Errorf("override host: %v", err)
	}
}

func TestWithoutUDP(t *testing.T) {
	r, _ := Builtin()
	p, err := r.Resolve([]string{"calls"}, ResolveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	n := len(p.Targets)
	WithoutUDP(p)
	for _, tg := range p.Targets {
		if tg.IsUDP() {
			t.Errorf("%s left", tg.Name)
		}
	}
	if len(p.Targets) != n-15 || indexGroup(p.Groups, "calls-udp") >= 0 || indexGroup(p.Groups, "calls-regions") >= 0 || indexGroup(p.Groups, "calls-apps") < 0 {
		t.Errorf("%d targets, groups %v", len(p.Targets), p.Groups)
	}
}
