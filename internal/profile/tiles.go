package profile

import (
	"fmt"
	"slices"
	"strings"

	"github.com/fuck-you-isp/fyisp/internal/callq"
	"github.com/fuck-you-isp/fyisp/internal/model"
)

// TagTile puts a target in one of the fixed Home tiles: "tile:<id>" with
// an id from Tiles. The catalog tags the targets of every tile; a --config
// file tags its own targets the same way. At most one per target.
const TagTile = "tile:"

// TileKind is how a tile is judged.
type TileKind string

const (
	TileCall   TileKind = "call"   // a call or chat app: its TCP front door and a UDP call path
	TileApp    TileKind = "app"    // a web app
	TileStream TileKind = "stream" // a streaming site
)

// TileDef is one tile of the Home view.
type TileDef struct {
	ID, Name string
	Kind     TileKind
	// Vendors attribute an outage of several tiles to one company.
	Vendors []string
	// Targets are the catalog TCP targets tagged for the tile, UDP its own
	// STUN targets. A call tile without UDP targets in the profile is
	// judged on the shared call path.
	Targets []string
	UDP     []string
	// Enable is the profile that adds all of the tile's targets.
	Enable string
}

// Tiles are the Home tiles, in display order. Their number and order are
// fixed: a tile whose targets the profile lacks is shown as off.
var Tiles = []TileDef{
	{ID: "zoom", Name: "Zoom", Kind: TileCall, Vendors: []string{"zoom"},
		Targets: []string{"Zoom"}, Enable: "calls"},
	{ID: "teams", Name: "Microsoft Teams", Kind: TileCall, Vendors: []string{"microsoft"},
		Targets: []string{"Microsoft-Teams", "Teams-Relay-Global", "Teams-Relay-EU", "Teams-Relay-US"},
		UDP:     []string{"Teams-Relay"}, Enable: "calls"},
	{ID: "meet", Name: "Google Meet", Kind: TileCall, Vendors: []string{"google"},
		Targets: []string{"Google-Meet", "Google-Meet-Web", "Google-Meet-Media", "Google-Meet-Media-Workspace"},
		UDP:     []string{"Meet-STUN", "Meet-STUN-3478"}, Enable: "calls"},
	{ID: "slack", Name: "Slack", Kind: TileCall, Vendors: []string{"slack"},
		Targets: []string{"Slack", "Slack-Backup"}, Enable: "calls"},
	{ID: "m365", Name: "Microsoft 365", Kind: TileApp, Vendors: []string{"microsoft"},
		Targets: []string{"M365-Outlook", "M365-Portal", "M365-Login"}, Enable: "services-productivity"},
	{ID: "gws", Name: "Google Workspace", Kind: TileApp, Vendors: []string{"google"},
		Targets: []string{"Google-Gmail", "Google-Docs", "Google-Drive"}, Enable: "services-productivity"},
	{ID: "github", Name: "GitHub", Kind: TileApp, Vendors: []string{"github"},
		Targets: []string{"dev-GitHub"}, Enable: "services-dev"},
	{ID: "dropbox", Name: "Dropbox", Kind: TileApp, Vendors: []string{"dropbox"},
		Targets: []string{"Dropbox"}, Enable: "services-productivity"},
	{ID: "onedrive", Name: "OneDrive", Kind: TileApp, Vendors: []string{"microsoft"},
		Targets: []string{"OneDrive"}, Enable: "services-productivity"},
	{ID: "youtube", Name: "YouTube", Kind: TileStream, Vendors: []string{"google"},
		Targets: []string{"YouTube", "YouTube-Video"}, Enable: "streaming"},
	{ID: "netflix", Name: "Netflix", Kind: TileStream, Vendors: []string{"netflix"},
		Targets: []string{"Netflix", "Netflix-Fast"}, Enable: "streaming"},
}

// tileIndex returns the index in Tiles of the tile with this id, or -1.
func tileIndex(id string) int {
	return slices.IndexFunc(Tiles, func(d TileDef) bool { return d.ID == id })
}

// TileOf returns the tile id of a target's tile: tag, or "".
func TileOf(t model.Target) string {
	for _, tag := range t.Tags {
		if id, ok := strings.CutPrefix(tag, TagTile); ok {
			return id
		}
	}
	return ""
}

// ResolvedTile is a tile with the profile targets that feed it.
type ResolvedTile struct {
	TileDef
	// Web are the tile's TCP (or, in --config files, HTTPS/ICMP) targets,
	// in profile order.
	Web []model.Target
	// UDP are the tile's own STUN targets or, for a call tile with TCP
	// targets but none of its own (SharedUDP), the shared call path: the
	// anycast STUN targets not tagged for an app or a tile.
	UDP       []model.Target
	SharedUDP bool
	// Missing are the catalog targets of the tile that do not feed it.
	// Untagged are those of them the profile has by name, without the
	// tile's tag (e.g. a --config override that replaced the tags).
	Missing  []string
	Untagged []string
}

// Off reports a tile the profile has no targets for.
func (r ResolvedTile) Off() bool { return len(r.Web) == 0 && len(r.UDP) == 0 }

// ResolveTiles maps the targets of p to the tiles by their tile: tags:
// always len(Tiles) tiles, in order.
func ResolveTiles(p *model.Profile) []ResolvedTile {
	out := make([]ResolvedTile, len(Tiles))
	for i, d := range Tiles {
		d.Vendors, d.Targets, d.UDP = slices.Clone(d.Vendors), slices.Clone(d.Targets), slices.Clone(d.UDP)
		out[i].TileDef = d
	}
	var shared []model.Target
	names := map[string]bool{}
	for _, t := range p.Targets {
		names[t.Name] = true
		i := tileIndex(TileOf(t))
		switch {
		case i >= 0 && t.IsUDP():
			out[i].UDP = append(out[i].UDP, t)
		case i >= 0:
			out[i].Web = append(out[i].Web, t)
		case callq.IsCorePath(t) && !callq.IsApp(t):
			shared = append(shared, t)
		}
	}
	for i := range out {
		r := &out[i]
		if r.Kind == TileCall && len(r.UDP) == 0 && len(r.Web) > 0 && len(shared) > 0 {
			r.UDP, r.SharedUDP = slices.Clone(shared), true
		}
		for _, n := range slices.Concat(r.Targets, r.TileDef.UDP) {
			if !slices.ContainsFunc(slices.Concat(r.Web, r.UDP), func(t model.Target) bool { return t.Name == n }) {
				r.Missing = append(r.Missing, n)
				if names[n] {
					r.Untagged = append(r.Untagged, n)
				}
			}
		}
	}
	return out
}

// checkTileTags reports an unknown tile: id, more than one tile: tag, or a
// UDP target tagged for a tile other than a call tile.
func checkTileTags(tags []string, udp bool) error {
	n := 0
	for _, tag := range tags {
		id, ok := strings.CutPrefix(tag, TagTile)
		if !ok {
			continue
		}
		i := tileIndex(id)
		if i >= 0 && udp && Tiles[i].Kind != TileCall {
			return fmt.Errorf("tag %q: UDP targets only feed call tiles", tag)
		}
		if i < 0 {
			ids := make([]string, len(Tiles))
			for i, d := range Tiles {
				ids[i] = d.ID
			}
			return fmt.Errorf("tag %q: want %s%s", tag, TagTile, strings.Join(ids, "|"+TagTile))
		}
		if n++; n > 1 {
			return fmt.Errorf("more than one %s tag", TagTile)
		}
	}
	return nil
}
