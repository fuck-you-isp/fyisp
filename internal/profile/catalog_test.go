package profile

import (
	"os"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

func sampleCatalog(t *testing.T) *Catalog {
	t.Helper()
	c, err := LoadCatalog(os.DirFS("catalog/testdata"))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestCatalogSample(t *testing.T) {
	c := sampleCatalog(t)
	var ids []string
	for _, p := range c.Providers {
		ids = append(ids, p.ID)
	}
	if strings.Join(ids, " ") != "aws hetzner services-voice" {
		t.Errorf("providers %v", ids)
	}
	if len(c.Targets) != 9 {
		t.Errorf("%d targets", len(c.Targets))
	}
	d, ok := c.Target("Discord")
	if !ok || d.Provider != "services-voice" || !d.Anycast || d.GeoKey() != "global" {
		t.Errorf("Discord %+v", d)
	}
	a, _ := c.Target("AWS-us-east-1")
	if a.Verified["https"] != 200 || a.Path != "/ping" || a.GeoKey() != "na" {
		t.Errorf("AWS-us-east-1 %+v", a)
	}
	z, _ := c.Target("Zoom-eu")
	if zt := z.toTarget("g"); zt.HostFor(model.KindICMP) != "zoom-icmp.example.com" || zt.HostFor(model.KindHTTPS) != "zoom-eu.example.com" {
		t.Errorf("host_overrides: %+v", zt)
	}
	mt := a.toTarget("g")
	if mt.Group != "g" || len(mt.Kinds) != 2 || mt.Interval != DefaultInterval || mt.Path != "/ping" {
		t.Errorf("toTarget %+v", mt)
	}
}

// TestEmbeddedCatalog validates the real catalog/*.yml files.
func TestEmbeddedCatalog(t *testing.T) {
	r, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("embedded catalog: %d providers, %d targets", len(r.Catalog.Providers), len(r.Catalog.Targets))
	// Catalog names that are also default targets are the same identity:
	// combined with default, the catalog definition wins.
	def, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	shared := 0
	for _, tg := range def.Targets {
		if _, ok := r.Catalog.Target(tg.Name); ok {
			shared++
		}
	}
	t.Logf("%d default.yml targets are also in the catalog", shared)
}

func TestCatalogErrors(t *testing.T) {
	good := "provider: p\ndisplay: P\nkind: cloud\ntargets:\n  - {name: a, host: a.example, kinds: [https], geo: eu}\n"
	cases := map[string]fstest.MapFS{
		"already used in a.yml": {
			"a.yml": {Data: []byte(good)},
			"b.yml": {Data: []byte("provider: q\ndisplay: Q\nkind: cdn\ntargets:\n  - {name: a, host: b.example, kinds: [tcp], geo: na}\n")},
		},
		"provider \"p\" already defined": {
			"a.yml": {Data: []byte(good)},
			"b.yml": {Data: []byte(strings.Replace(good, "name: a,", "name: b,", 1))},
		},
		"duplicate name":               {"a.yml": {Data: []byte(good + "  - {name: a, host: c.example, kinds: [https], geo: eu}\n")}},
		"geo \"europe\"":               {"a.yml": {Data: []byte(strings.Replace(good, "geo: eu", "geo: europe", 1))}},
		"geo is required":              {"a.yml": {Data: []byte(strings.Replace(good, ", geo: eu", "", 1))}},
		"unknown kind \"udp\"":         {"a.yml": {Data: []byte(strings.Replace(good, "[https]", "[https, udp]", 1))}},
		"listed twice":                 {"a.yml": {Data: []byte(strings.Replace(good, "[https]", "[https, https]", 1))}},
		"host is required":             {"a.yml": {Data: []byte(strings.Replace(good, "host: a.example, ", "", 1))}},
		"private address":              {"a.yml": {Data: []byte(strings.Replace(good, "a.example", "10.0.0.1", 1))}},
		"not allowed in the catal":     {"a.yml": {Data: []byte(strings.Replace(good, "a.example", "'@gateway'", 1))}},
		"kind \"isp\"":                 {"a.yml": {Data: []byte(strings.Replace(good, "kind: cloud", "kind: isp", 1))}},
		"display is required":          {"a.yml": {Data: []byte(strings.Replace(good, "display: P\n", "", 1))}},
		"lowercase":                    {"a.yml": {Data: []byte(strings.Replace(good, "provider: p", "provider: AWS", 1))}},
		"no targets":                   {"a.yml": {Data: []byte("provider: p\ndisplay: P\nkind: cloud\n")}},
		"1-40 characters":              {"a.yml": {Data: []byte(strings.Replace(good, "name: a,", "name: a b,", 1))}},
		"ISO 3166":                     {"a.yml": {Data: []byte(strings.Replace(good, "geo: eu", "geo: eu, country: Germany", 1))}},
		"must start with /":            {"a.yml": {Data: []byte(strings.Replace(good, "geo: eu", "geo: eu, path: ping", 1))}},
		"not a valid URL path":         {"a.yml": {Data: []byte(strings.Replace(good, "geo: eu", "geo: eu, path: '/a b'", 1))}},
		"host_overrides: unknown kind": {"a.yml": {Data: []byte(strings.Replace(good, "geo: eu", "geo: eu, host_overrides: {udp: b.example}", 1))}},
		"host_overrides[icmp]":         {"a.yml": {Data: []byte(strings.Replace(good, "geo: eu", "geo: eu, host_overrides: {icmp: 192.168.1.1}", 1))}},
		"field regoin not found":       {"a.yml": {Data: []byte(strings.Replace(good, "geo: eu", "geo: eu, regoin: x", 1))}},
		"field website not found":      {"a.yml": {Data: []byte("website: x\n" + good)}},
		"empty file":                   {"a.yml": {Data: nil}},
	}
	for want, fsys := range cases {
		_, err := LoadCatalog(fsys)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: got %v", want, err)
		}
	}
	// Metadata in any shape, an anycast target without geo, and files that
	// are not *.yml are fine.
	ok := fstest.MapFS{
		"a.yml": {Data: []byte("provider: p\ndisplay: P\nkind: cloud\nsources: [https://x]\nnotes: n\ncategory: [x]\ntargets:\n" +
			"  - {name: a, host: a.example, kinds: [https], anycast: true, notes: hi, verified: {date: 2026-09-27, whatever: [1]}}\n")},
		"SCHEMA.md":      {Data: []byte("# not yaml")},
		"testdata/x.yml": {Data: []byte("garbage: [")},
	}
	if _, err := LoadCatalog(ok); err != nil {
		t.Error(err)
	}
	// DoH paths with a query string are fine.
	doh := strings.Replace(good, "geo: eu", "geo: eu, path: '/dns-query?dns=AAABAAABAAAAAAAAA3d3dwdleGFtcGxlA2NvbQAAAQAB'", 1)
	if _, err := LoadCatalog(fstest.MapFS{"a.yml": {Data: []byte(doh)}}); err != nil {
		t.Error(err)
	}
	// kinds: [] (unreachable for now) is accepted.
	c, err := LoadCatalog(fstest.MapFS{"a.yml": {Data: []byte(strings.Replace(good, "[https]", "[]", 1))}})
	if err != nil || c.Targets[0].Probeable() {
		t.Errorf("empty kinds: %v", err)
	}
}
