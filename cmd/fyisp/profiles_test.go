package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/profile"
)

func TestProfileFlags(t *testing.T) {
	c, err := parseFlags([]string{"--profile", "AWS, europe", "--profile=gcp", "--geo", "europe,na", "--no-path"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(c.profiles, []string{"aws", "europe", "gcp"}) || !slices.Equal(c.geos, []string{"eu", "na"}) || !c.noPath {
		t.Errorf("%+v", c)
	}
	if c, _ := parseFlags(nil); len(c.profiles) != 0 {
		t.Errorf("defaults %+v", c)
	}
	for want, args := range map[string][]string{
		"mutually exclusive":   {"--config", "x.yml", "--profile", "aws"},
		"unknown geo \"mars\"": {"--geo", "mars"},
	} {
		if _, err := parseFlags(args); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%v: %v", args, err)
		}
	}
}

func TestLoadProfileFlags(t *testing.T) {
	def, err := profile.Default()
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{nil, {"--profile", "default"}} {
		c, _ := parseFlags(args)
		p, err := loadProfile(c)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(p, def) {
			t.Errorf("%v: not the default profile", args)
		}
	}
	c, _ := parseFlags([]string{"--no-path"})
	if p, _ := loadProfile(c); len(p.Targets) != 22 || p.Groups[0].ID == profile.PathGroup {
		t.Errorf("--no-path: %d targets", len(p.Targets))
	}
	c, _ = parseFlags([]string{"--profile", "nope"})
	if _, err := loadProfile(c); err == nil || !strings.Contains(err.Error(), `unknown profile "nope"`) {
		t.Errorf("nope: %v", err)
	}

	r, err := profile.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	all, err := r.Resolve([]string{"clouds"}, profile.ResolveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	c, _ = parseFlags([]string{"--profile", "clouds"})
	if p, err := loadProfile(c); err != nil || len(p.Targets) != len(all.Targets) {
		t.Errorf("clouds (no target limit): %v", err)
	}
	c, _ = parseFlags([]string{"--profile", "all"})
	if p, err := loadProfile(c); err != nil || len(p.Targets) <= len(all.Targets) {
		t.Errorf("all: %v", err)
	}
	c, _ = parseFlags([]string{"--profile", "clouds", "--geo", "eu"})
	p, err := loadProfile(c)
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "clouds@eu" || p.Groups[0].ID != profile.PathGroup || len(p.Targets) >= len(all.Targets) {
		t.Errorf("clouds@eu: %s %d", p.Name, len(p.Targets))
	}
	for _, tg := range p.Targets {
		if ct, ok := r.Catalog.Target(tg.Name); tg.Group != profile.PathGroup && (!ok || ct.GeoKey() != "eu") {
			t.Errorf("clouds@eu has %s", tg.Name)
		}
	}
}

func TestCmdProfiles(t *testing.T) {
	var out, errb bytes.Buffer
	if code := cmdProfiles(nil, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	s := out.String()
	for _, want := range []string{"PROFILE", "default", "22", "hyperscalers", "europe", "gaming", "all", "calls", "PROVIDER"} {
		if !strings.Contains(s, want) {
			t.Errorf("list lacks %q:\n%s", want, s)
		}
	}
	if testing.Verbose() {
		t.Log("\n" + s)
	}

	out.Reset()
	if code := cmdProfiles([]string{"default"}, &out, &errb); code != 0 {
		t.Fatal(code)
	}
	if s := out.String(); !strings.Contains(s, "Profile default: 22 targets in 4 panels") || !strings.Contains(s, "CDN-Google      www.gstatic.com/generate_204") {
		t.Errorf("default:\n%s", s)
	}

	r, _ := profile.Builtin()
	if r.Definition("aws") != nil {
		out.Reset()
		if code := cmdProfiles([]string{"aws"}, &out, &errb); code != 0 {
			t.Fatal(code)
		}
		s := out.String()
		if !strings.Contains(s, "AWS-us-east-1") || !strings.Contains(s, "Amazon Web Services") {
			t.Errorf("aws:\n%s", s)
		}
		if ct, ok := r.Catalog.Target("AWS-me-south-1"); ok && !ct.Probeable() && !regexp.MustCompile(`AWS-me-south-1 +`+regexp.QuoteMeta(ct.Host)+` +unreachable, skipped`).MatchString(s) {
			t.Errorf("aws does not list the skipped me-south-1:\n%s", s)
		}
	}

	errb.Reset()
	if code := cmdProfiles([]string{"nope"}, &out, &errb); code != 1 || !strings.Contains(errb.String(), "unknown profile") {
		t.Errorf("nope: %d %s", code, errb.String())
	}
	if code := cmdProfiles([]string{"--geo", "mars"}, &out, &errb); code != 2 {
		t.Errorf("bad geo: %d", code)
	}
}

// TestNoUDP: --no-udp drops every UDP (STUN) target from the default, a
// named profile and a --config file, and empty groups with them.
func TestNoUDP(t *testing.T) {
	c, err := parseFlags([]string{"--no-udp"})
	if err != nil || !c.noUDP {
		t.Fatalf("%+v %v", c, err)
	}
	hasUDP := func(p *model.Profile) bool {
		return slices.ContainsFunc(p.Targets, func(tg model.Target) bool { return tg.IsUDP() })
	}
	p, err := loadProfile(c)
	if err != nil || hasUDP(p) || len(p.Targets) != 18+5 || slices.ContainsFunc(p.Groups, func(g model.Group) bool { return g.ID == "calls" }) {
		t.Fatalf("default --no-udp: %v, %d targets", err, len(p.Targets))
	}
	c, _ = parseFlags([]string{"--profile", "calls", "--no-udp"})
	if p, err := loadProfile(c); err != nil || hasUDP(p) || len(p.Targets) < 10 {
		t.Fatalf("calls --no-udp: %v", err)
	}
	c, _ = parseFlags([]string{"--profile", "calls"})
	if p, err := loadProfile(c); err != nil || !hasUDP(p) {
		t.Fatalf("calls: %v", err)
	}
	c, _ = parseFlags([]string{"--profile", "twilio", "--no-udp"})
	if _, err := loadProfile(c); err == nil || !strings.Contains(err.Error(), "--no-udp leaves profile twilio without targets") {
		t.Fatalf("twilio --no-udp: %v", err)
	}
	f := filepath.Join(t.TempDir(), "p.yml")
	os.WriteFile(f, []byte("extends: [default]\nadd: [{name: mine, host: stun.example.com, port: 3478, group: calls, kinds: [udp], interval: 6s}]\n"), 0o600)
	c, _ = parseFlags([]string{"--config", f, "--no-udp"})
	if p, err := loadProfile(c); err != nil || hasUDP(p) {
		t.Fatalf("--config --no-udp: %v", err)
	}
}
