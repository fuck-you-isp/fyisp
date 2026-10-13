package profile

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

// TestTilesCatalog checks the tile list against the catalog both ways:
// every tile target is a probeable catalog target tagged for its tile (UDP
// targets under UDP, the others under Targets), and every catalog target
// with a tile: tag is listed by its tile.
func TestTilesCatalog(t *testing.T) {
	r, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	if len(Tiles) != 11 {
		t.Errorf("%d tiles, want 11", len(Tiles))
	}
	var ids []string
	listed := map[string]string{}
	for _, d := range Tiles {
		ids = append(ids, d.ID)
		if d.Name == "" || len(d.Vendors) == 0 || len(d.Targets) == 0 || r.Definition(d.Enable) == nil {
			t.Errorf("%s: incomplete %+v", d.ID, d)
		}
		if len(d.UDP) > 0 && d.Kind != TileCall {
			t.Errorf("%s: UDP targets on a %s tile", d.ID, d.Kind)
		}
		for _, n := range slices.Concat(d.Targets, d.UDP) {
			ct, ok := r.Catalog.Target(n)
			switch {
			case !ok:
				t.Errorf("%s: %s not in the catalog", d.ID, n)
				continue
			case !ct.Probeable():
				t.Errorf("%s: %s not probeable", d.ID, n)
			case listed[n] != "":
				t.Errorf("%s: %s already in %s", d.ID, n, listed[n])
			}
			listed[n] = d.ID
			mt := ct.toTarget("g", nil)
			if TileOf(mt) != d.ID {
				t.Errorf("%s: %s tagged %q", d.ID, n, TileOf(mt))
			}
			if mt.IsUDP() != slices.Contains(d.UDP, n) {
				t.Errorf("%s: %s udp %v, listed under the wrong field", d.ID, n, mt.IsUDP())
			}
		}
	}
	if got := strings.Join(ids, " "); got != "zoom teams meet slack m365 gws github dropbox onedrive youtube netflix" {
		t.Errorf("tile order %s", got)
	}
	for _, ct := range r.Catalog.Targets {
		if id := TileOf(ct.toTarget("g", nil)); id != "" && listed[ct.Name] != id {
			t.Errorf("catalog %s: tagged tile:%s but not listed by it", ct.Name, id)
		}
	}
}

// tileSummary is one line per tile: its targets, or "off".
func tileSummary(rs []ResolvedTile) []string {
	names := func(ts []model.Target) string {
		var s []string
		for _, t := range ts {
			s = append(s, t.Name)
		}
		return strings.Join(s, ",")
	}
	var out []string
	for _, r := range rs {
		if r.Off() {
			out = append(out, r.ID+" off")
			continue
		}
		line := fmt.Sprintf("%s web=%s udp=%s", r.ID, names(r.Web), names(r.UDP))
		if r.SharedUDP {
			line += " shared"
		}
		if len(missing(r)) > 0 {
			line += " missing=" + strings.Join(missing(r), ",")
		}
		out = append(out, line)
	}
	return out
}

// TestResolveTilesDefault checks that the default covers every tile, one
// target each at least, with Teams judged on its own relay.
func TestResolveTilesDefault(t *testing.T) {
	p, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	shared := "udp=Cloudflare-STUN,Cloudflare-UDP53,Twilio-STUN-global shared"
	want := []string{
		"zoom web=Zoom " + shared,
		"teams web=Microsoft-Teams udp=Teams-Relay missing=Teams-Relay-Global,Teams-Relay-EU,Teams-Relay-US",
		"meet web=Google-Meet udp=Meet-STUN missing=Google-Meet-Web,Google-Meet-Media,Google-Meet-Media-Workspace,Meet-STUN-3478",
		"slack web=Slack " + shared + " missing=Slack-Backup",
		"m365 web=M365-Outlook,M365-Portal,M365-Login udp=",
		"gws web=Google-Gmail,Google-Docs,Google-Drive udp=",
		"github web=dev-GitHub udp=",
		"dropbox web=Dropbox udp=",
		"onedrive web=OneDrive udp=",
		"youtube web=YouTube udp= missing=YouTube-Video",
		"netflix web=Netflix udp= missing=Netflix-Fast",
	}
	if got := tileSummary(ResolveTiles(p)); !slices.Equal(got, want) {
		t.Errorf("default:\n got %q\nwant %q", got, want)
	}
}

// TestDefaultWithoutStreaming checks the opt-out the README documents: a
// --config file that extends the default and removes the streaming targets
// turns their tiles off and keeps the others.
func TestDefaultWithoutStreaming(t *testing.T) {
	p, err := Load(write(t, "extends: [default]\nremove: [YouTube, Netflix]\n"))
	if err != nil {
		t.Fatal(err)
	}
	var off []string
	for _, rt := range ResolveTiles(p) {
		if rt.Off() {
			off = append(off, rt.ID)
		}
	}
	if !slices.Equal(off, []string{"youtube", "netflix"}) {
		t.Errorf("off %v, want [youtube netflix]", off)
	}
	if slices.ContainsFunc(p.Targets, func(t model.Target) bool { return slices.Contains(t.Tags, "streaming") }) {
		t.Errorf("a streaming target is left")
	}
}

// TestDefaultWithoutRelay checks the other opt-out: without the Teams
// relay, Teams is judged on the shared call path again.
func TestDefaultWithoutRelay(t *testing.T) {
	p, err := Load(write(t, "extends: [default]\nremove: [Teams-Relay]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := tileSummary(ResolveTiles(p))[1]; got != "teams web=Microsoft-Teams udp=Cloudflare-STUN,Cloudflare-UDP53,Twilio-STUN-global shared missing=Teams-Relay-Global,Teams-Relay-EU,Teams-Relay-US,Teams-Relay" {
		t.Errorf("teams: %s", got)
	}
}

// TestResolveTilesNamed checks the tiles of named profiles: which are off,
// which judge calls on the shared path, and that every tile they cover has
// all of its targets.
func TestResolveTilesNamed(t *testing.T) {
	r, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		names       string
		off, shared string
	}{
		{"all", "", "zoom slack"},
		{"calls", "m365 gws github dropbox onedrive youtube netflix", "zoom slack"},
		{"all,calls", "", "zoom slack"},
		{"default,streaming", "", "zoom slack"},
		{"clouds", "zoom teams meet slack m365 gws github dropbox onedrive youtube netflix", ""},
	}
	for _, c := range cases {
		p, err := r.Resolve(strings.Split(c.names, ","), ResolveOptions{})
		if err != nil {
			t.Fatal(err)
		}
		rs := ResolveTiles(p)
		if len(rs) != len(Tiles) {
			t.Fatalf("%s: %d tiles", c.names, len(rs))
		}
		var off, shared []string
		for i, rt := range rs {
			if rt.ID != Tiles[i].ID {
				t.Errorf("%s: tile %d is %s", c.names, i, rt.ID)
			}
			if rt.Off() {
				off = append(off, rt.ID)
			}
			if rt.SharedUDP {
				shared = append(shared, rt.ID)
			}
			if !rt.Off() && c.names != "default,streaming" && len(missing(rt)) > 0 {
				t.Errorf("%s: %s missing %v", c.names, rt.ID, missing(rt))
			}
		}
		if strings.Join(off, " ") != c.off || strings.Join(shared, " ") != c.shared {
			t.Errorf("%s: off %v, shared %v; want off [%s], shared [%s]", c.names, off, shared, c.off, c.shared)
		}
		if c.names == "all" {
			if got := tileSummary(rs)[1]; got != "teams web=Microsoft-Teams,Teams-Relay-Global,Teams-Relay-EU,Teams-Relay-US udp=Teams-Relay" {
				t.Errorf("all: %s", got)
			}
		}
	}
	// Each tile's Enable profile adds all of its targets.
	for i, d := range Tiles {
		p, err := r.Resolve([]string{d.Enable}, ResolveOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if rt := ResolveTiles(p)[i]; rt.Off() || len(missing(rt)) > 0 {
			t.Errorf("%s: --profile %s: off %v, missing %v", d.ID, d.Enable, rt.Off(), missing(rt))
		}
	}
}

// TestResolveTilesConfig checks a --config file's own targets: tile: tags
// feed tiles, untagged targets with catalog names do not, and a call tile
// uses the shared call path only when it has TCP targets.
func TestResolveTilesConfig(t *testing.T) {
	p, err := Load(write(t, `groups: [{id: g}]
targets:
  - {name: my-zoom, host: zoom.example.com, group: g, kinds: [tcp], tags: [tile:zoom]}
  - {name: nas, host: nas.example.com, group: g, kinds: [https], tags: [tile:dropbox]}
  - {name: Slack, host: slack.example.com, group: g, kinds: [tcp]}
  - {name: my-stun, host: stun.example.com, group: g, kinds: [udp], port: 3478, tags: [udp-path]}
  - {name: meet-stun, host: meet.example.com, group: g, kinds: [udp], port: 3478, tags: [udp-path, app:meet, tile:meet]}
`))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"zoom web=my-zoom udp=my-stun shared missing=Zoom",
		"teams off",
		"meet web= udp=meet-stun missing=Google-Meet,Google-Meet-Web,Google-Meet-Media,Google-Meet-Media-Workspace,Meet-STUN,Meet-STUN-3478",
		"slack off",
		"m365 off", "gws off", "github off",
		"dropbox web=nas udp= missing=Dropbox",
		"onedrive off", "youtube off", "netflix off",
	}
	if got := tileSummary(ResolveTiles(p)); !slices.Equal(got, want) {
		t.Errorf("config:\n got %q\nwant %q", got, want)
	}
	// Without any call path the call tiles are judged on TCP alone.
	p.Targets = slices.DeleteFunc(p.Targets, func(t model.Target) bool { return t.IsUDP() })
	if rt := ResolveTiles(p)[0]; rt.Off() || rt.SharedUDP || len(rt.UDP) != 0 {
		t.Errorf("zoom without a call path: %+v", rt)
	}
	if got := ResolveTiles(&model.Profile{}); len(got) != len(Tiles) || !got[0].Off() || !got[10].Off() {
		t.Errorf("empty profile: %v", tileSummary(got))
	}
}

func TestValidateTileTags(t *testing.T) {
	prof := func(tags ...string) *model.Profile {
		return &model.Profile{Groups: []model.Group{{ID: "g"}},
			Targets: []model.Target{{Name: "a", Host: "a.example", Group: "g", Tags: tags}}}
	}
	for _, ok := range [][]string{nil, {"tile:zoom"}, {"tile:onedrive", "home:work"}, {"tilezoom"}} {
		if err := Validate(prof(ok...), false); err != nil {
			t.Errorf("%v: %v", ok, err)
		}
	}
	for tags, want := range map[string]string{
		"tile:storage":           `tag "tile:storage": want tile:zoom|tile:teams|tile:meet|tile:slack|tile:m365|tile:gws|tile:github|tile:dropbox|tile:onedrive|tile:youtube|tile:netflix`,
		"tile:":                  `tag "tile:"`,
		"tile:Zoom":              `tag "tile:Zoom"`,
		"tile:zoom,tile:slack":   "more than one tile: tag",
		"tile:zoom,tile:zoom":    "more than one tile: tag",
		"home:calls,tile:bogus":  `tag "tile:bogus"`,
		"tile:netflix,tile:nope": `tag "tile:nope"`,
	} {
		if err := Validate(prof(strings.Split(tags, ",")...), false); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", tags, err)
		}
	}
	// --config files and the catalog are validated the same way.
	if _, err := Load(write(t, "groups: [{id: g}]\ntargets: [{name: a, host: a.com, group: g, tags: [tile:tv]}]\n")); err == nil ||
		!strings.Contains(err.Error(), `tag "tile:tv"`) {
		t.Errorf("load: %v", err)
	}
	good := "provider: p\ndisplay: P\nkind: cloud\ntargets:\n  - {name: a, host: a.example, kinds: [tcp], geo: eu, tags: [%s]}\n"
	for tags, want := range map[string]string{"tile:tv": `tag "tile:tv"`, "tile:zoom, tile:meet": "more than one tile: tag"} {
		if _, err := LoadCatalog(fstest.MapFS{"a.yml": {Data: fmt.Appendf(nil, good, tags)}}); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("catalog %s: %v", tags, err)
		}
	}
}

func TestOneDriveTarget(t *testing.T) {
	r, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	ct, ok := r.Catalog.Target("OneDrive")
	if !ok || ct.Provider != "services-productivity" || ct.Host != "skyapi.onedrive.live.com" || !ct.Anycast ||
		!slices.Equal(ct.Kinds, []string{"tcp"}) || ct.Verified["tcp"] != true {
		t.Fatalf("OneDrive %+v", ct)
	}
	for _, name := range []string{"common", "all", "services-productivity"} {
		p, err := r.Resolve([]string{name}, ResolveOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if !slices.ContainsFunc(p.Targets, func(t model.Target) bool { return t.Name == "OneDrive" }) {
			t.Errorf("%s: no OneDrive", name)
		}
	}
}

// TestResolveTilesOverride checks a --config override that replaces a
// catalog target's tags: the target no longer feeds its tile.
func TestResolveTilesOverride(t *testing.T) {
	p, err := Load(write(t, `extends: [default]
override:
  - {name: Zoom, tags: [voice]}
  - {name: Google-Meet, tags: [app:meet, tile:meet]}
`))
	if err != nil {
		t.Fatal(err)
	}
	rs := ResolveTiles(p)
	if z := rs[0]; !z.Off() || !slices.Equal(missing(z), []string{"Zoom"}) {
		t.Errorf("zoom: off %v missing %v", z.Off(), missing(z))
	}
	// Meet keeps its tag.
	if m := rs[2]; m.Off() || len(missing(m)) != 4 {
		t.Errorf("meet: missing %v", missing(m))
	}
}

// missing lists the tile's catalog targets that do not feed it.
func missing(r ResolvedTile) []string {
	var out []string
	for _, n := range slices.Concat(r.Targets, r.TileDef.UDP) {
		if !slices.ContainsFunc(slices.Concat(r.Web, r.UDP), func(t model.Target) bool { return t.Name == n }) {
			out = append(out, n)
		}
	}
	return out
}

// TestResolveTilesNoAliasing checks that resolved tiles share no slices
// with Tiles or with each other.
func TestResolveTilesNoAliasing(t *testing.T) {
	p, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	rs := ResolveTiles(p)
	rs[0].UDP[0].Name = "changed"
	rs[0].UDP = append(rs[0].UDP[:1], model.Target{Name: "appended"})
	rs[1].Targets[0], rs[1].Vendors[0] = "changed", "changed"
	rs[2].TileDef.UDP[0] = "changed"
	if Tiles[1].Targets[0] != "Microsoft-Teams" || Tiles[1].Vendors[0] != "microsoft" || Tiles[2].UDP[0] != "Meet-STUN" {
		t.Errorf("Tiles changed: %+v %+v", Tiles[1], Tiles[2])
	}
	if got := tileSummary(rs)[3]; got != "slack web=Slack udp=Cloudflare-STUN,Cloudflare-UDP53,Twilio-STUN-global shared missing=Slack-Backup" {
		t.Errorf("slack shares zoom's call path: %s", got)
	}
}

// TestValidateTileUDP checks that a UDP target only feeds a call tile.
func TestValidateTileUDP(t *testing.T) {
	cfg := `extends: [default]
add:
  - {name: my-stun, host: stun.example.com, group: calls, kinds: [udp], port: 3478, tags: [udp-path, %s]}
`
	if _, err := Load(write(t, fmt.Sprintf(cfg, "tile:netflix"))); err == nil ||
		!strings.Contains(err.Error(), `tag "tile:netflix": UDP targets only feed call tiles`) {
		t.Errorf("udp tile:netflix: %v", err)
	}
	if _, err := Load(write(t, fmt.Sprintf(cfg, "tile:zoom"))); err != nil {
		t.Errorf("udp tile:zoom: %v", err)
	}
	cat := "provider: p\ndisplay: P\nkind: service\ntargets:\n  - {name: a, host: a.example, kinds: [udp], port: 3478, anycast: true, tags: [tile:gws]}\n"
	if _, err := LoadCatalog(fstest.MapFS{"a.yml": {Data: []byte(cat)}}); err == nil || !strings.Contains(err.Error(), "only feed call tiles") {
		t.Errorf("catalog udp tile:gws: %v", err)
	}
}
