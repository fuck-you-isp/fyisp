package main

import (
	"bytes"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/fuck-you-isp/fyisp/internal/profile"
)

func TestProfileFlags(t *testing.T) {
	c, err := parseFlags([]string{"--profile", "AWS, europe", "--profile=gcp", "--geo", "europe,na", "--max-targets", "500", "--no-path"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(c.profiles, []string{"aws", "europe", "gcp"}) || !slices.Equal(c.geos, []string{"eu", "na"}) || c.maxTargets != 500 || !c.noPath {
		t.Errorf("%+v", c)
	}
	if c, _ := parseFlags(nil); c.maxTargets != profile.DefaultMaxTargets || len(c.profiles) != 0 {
		t.Errorf("defaults %+v", c)
	}
	for want, args := range map[string][]string{
		"between 1 and 1000":   {"--max-targets", "1001"},
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
	if p, _ := loadProfile(c); len(p.Targets) != 87 || p.Groups[0].ID == profile.PathGroup {
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
	if len(all.Targets) <= profile.DefaultMaxTargets {
		t.Skipf("clouds has %d targets: catalog too small for the limit test", len(all.Targets))
	}
	c, _ = parseFlags([]string{"--profile", "clouds"})
	if _, err := loadProfile(c); err == nil || !strings.Contains(err.Error(), "--max-targets") || !strings.Contains(err.Error(), "--geo eu") {
		t.Errorf("over the limit: %v", err)
	}
	c, _ = parseFlags([]string{"--profile", "clouds", "--max-targets", "1000"})
	if p, err := loadProfile(c); err != nil || len(p.Targets) != len(all.Targets) {
		t.Errorf("--max-targets 1000: %v", err)
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
	for _, want := range []string{"PROFILE", "default", "87", "hyperscalers", "europe", "gaming", "PROVIDER", "--max-targets"} {
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
	if s := out.String(); !strings.Contains(s, "Profile default: 87 targets in 7 panels") || !strings.Contains(s, "dev-GitHub  github.com/robots.txt") {
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
