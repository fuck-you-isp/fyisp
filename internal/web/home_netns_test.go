package web

import (
	"strings"
	"testing"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/tiles"
)

// Cards the netns lab (test/netns, TestHome10) found wrong.

// TestHomeSTUNBlockedTiles is netns S10b: stun_blocked (one STUN port
// dropped) is the blocked card, not the good one, and its call tiles are
// Choppy over a slower route, not Down (B4).
func TestHomeSTUNBlockedTiles(t *testing.T) {
	sc := scenario(t, "udp-blocked")
	snap := *sc.Snap
	snap.Conn.Kind = string(model.VerdictSTUNBlocked)
	sc.Snap = &snap
	h := homeServer(t, sc, homeNow)
	code, b := homeGet(t, h, sc.Viewer, "part=now&tz=America/New_York")
	if code != 200 {
		t.Fatalf("%d %s", code, b)
	}
	d := decode[HomeNow](t, b)
	a := d.Answer
	if a.Tone != tiles.ToneWarn || a.Headline != "Calls can’t use their fast path on this network." || a.Tab != "! Calls limited" ||
		!strings.Contains(a.Sub, "blocking the ports calls prefer") {
		t.Errorf("card %+v, want the blocked card naming the ports", a)
	}
	for _, x := range d.Tiles[:4] {
		if x.State != "slow" || !strings.HasPrefix(x.Reason, "Can’t use its fast path") || !strings.HasSuffix(x.Reason, "over another port or a slower route.") {
			t.Errorf("%s: %s %q, want choppy over a slower route", x.ID, x.State, x.Reason)
		}
	}
	code, b = homeGet(t, h, sc.Viewer, "part=tile&id=zoom&tz=America/New_York")
	if p := decode[HomeTileDetail](t, b); code != 200 || p.State != "slow" {
		t.Errorf("panel: %d %s", code, p.State)
	}
}

// TestHomeProviderLossy is netns S3: an isp verdict from partial loss is
// the amber "losing some data" card, with the provider's hop slow (b).
func TestHomeProviderLossy(t *testing.T) {
	sc := scenario(t, "isp")
	snap := *sc.Snap
	snap.Conn.Lossy, snap.Conn.Tone, snap.Conn.Icon = true, tiles.ToneWarn, tiles.Slow
	snap.Conn.Key = "isp_lossy" + strings.TrimPrefix(snap.Conn.Key, "isp")
	sc.Snap = &snap
	h := homeServer(t, sc, homeNow)
	code, b := homeGet(t, h, sc.Viewer, "part=now&tz=America/New_York")
	if code != 200 {
		t.Fatalf("%d %s", code, b)
	}
	d := decode[HomeNow](t, b)
	if a := d.Answer; a.Tone != tiles.ToneWarn || a.Icon != "slow" || a.Headline != "Your internet provider is losing some data." ||
		a.Tab != "! Provider losing data" || !strings.HasPrefix(a.Key, "isp_lossy:") {
		t.Errorf("card %+v, want the amber provider-loss card", a)
	}
	if p := d.Path[1]; p.State != "slow" || !strings.HasPrefix(p.Sub, "Losing some data") {
		t.Errorf("provider hop %+v, want slow, losing some data", p)
	}
}
