package profile

import (
	"strings"
	"testing"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

func TestHomeCategory(t *testing.T) {
	cases := []struct {
		t    model.Target
		want string
	}{
		{model.Target{Tags: []string{"home:work"}, Provider: "services-streaming"}, HomeWork}, // the tag wins
		{model.Target{Tags: []string{"app:zoom"}}, HomeCalls},
		{model.Target{Tags: []string{"udp-path"}, Kinds: []model.ProbeKind{model.KindUDP}}, HomeCalls},
		{model.Target{Provider: "services-productivity"}, HomeWork},
		{model.Target{Provider: "cdn-fastly", ProviderKind: "cdn"}, HomeWeb},
		{model.Target{Provider: "services-streaming", ProviderKind: "service"}, HomeWeb},
		{model.Target{Provider: "aws", ProviderKind: "hyperscaler"}, HomeOther},
		{model.Target{Name: "nas", Tags: []string{"home:bogus"}}, HomeOther}, // rejected by Validate
		{model.Target{}, HomeOther},
	}
	for _, c := range cases {
		if got := HomeCategory(c.t); got != c.want {
			t.Errorf("%+v: %s, want %s", c.t, got, c.want)
		}
	}
}

// TestHomeCategoriesShipped checks the home: tags of the catalog and the
// default: at most one per catalog target, exactly one per default target,
// and each agrees with what the target's other tags and provider imply.
func TestHomeCategoriesShipped(t *testing.T) {
	r, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	homeTags := func(tags []string) (n int) {
		for _, tag := range tags {
			if strings.HasPrefix(tag, TagHome) {
				n++
			}
		}
		return n
	}
	for _, ct := range r.Catalog.Targets {
		if n := homeTags(ct.Tags); n > 1 {
			t.Errorf("catalog %s: %d home: tags", ct.Name, n)
		}
	}
	def, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for _, tg := range withoutPath(def.Targets) {
		if n := homeTags(tg.Tags); n != 1 {
			t.Errorf("default %s: %d home: tags, want 1", tg.Name, n)
		}
		c := HomeCategory(tg)
		got[c]++
		untagged := tg
		untagged.Tags = nil
		for _, tag := range tg.Tags {
			if !strings.HasPrefix(tag, TagHome) {
				untagged.Tags = append(untagged.Tags, tag)
			}
		}
		if d := HomeCategory(untagged); d != c {
			t.Errorf("default %s: tagged %s, derived %s", tg.Name, c, d)
		}
	}
	if got[HomeCalls] != 11 || got[HomeWork] != 6 || got[HomeWeb] != 5 || len(got) != 3 {
		t.Errorf("default categories %v", got)
	}
}

func TestValidateHomeTags(t *testing.T) {
	prof := func(tags ...string) *model.Profile {
		return &model.Profile{Groups: []model.Group{{ID: "g"}},
			Targets: []model.Target{{Name: "a", Host: "a.example", Group: "g", Tags: tags}}}
	}
	for _, ok := range [][]string{nil, {"home:calls"}, {"app:zoom", "home:web"}, {"homework"}} {
		if err := Validate(prof(ok...), Limits{}); err != nil {
			t.Errorf("%v: %v", ok, err)
		}
	}
	for tags, want := range map[string]string{
		"home:other":            `tag "home:other": want home:calls|home:work|home:web`,
		"home:":                 `tag "home:"`,
		"home:Calls":            `tag "home:Calls"`,
		"home:calls,home:work":  "more than one home: tag",
		"home:calls,home:calls": "more than one home: tag",
	} {
		if err := Validate(prof(strings.Split(tags, ",")...), Limits{}); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", tags, err)
		}
	}
	// --config files are validated the same way.
	if _, err := Load(write(t, "groups: [{id: g}]\ntargets: [{name: a, host: a.com, group: g, tags: [home:tv]}]\n")); err == nil ||
		!strings.Contains(err.Error(), `tag "home:tv"`) {
		t.Errorf("load: %v", err)
	}
}
