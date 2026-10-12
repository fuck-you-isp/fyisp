package tiles

import (
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/profile"
	"github.com/fuck-you-isp/fyisp/internal/speedtest"
)

// allTiles is every tile's evidence, Good unless set in bad.
func allTiles(bad map[string]State) []tileEv {
	var out []tileEv
	for _, d := range profile.Tiles {
		s := Good
		if b, ok := bad[d.ID]; ok {
			s = b
		}
		out = append(out, tileEv{id: d.ID, vendor: d.Vendors[0], kind: d.Kind, worst: s})
	}
	return out
}

func TestAttribute(t *testing.T) {
	from, to := at(10*time.Minute), at(12*time.Minute)
	now := at(time.Hour)
	inc := func(k model.VerdictKind, start, end time.Duration, peak float64) []model.Incident {
		return []model.Incident{{Kind: k, Start: at(start), End: at(end), PeakLoss: peak}}
	}
	lay := func(gw, edge, ac layerEv) [nLayers]layerEv { return [nLayers]layerEv{gw, edge, ac} }
	clean := layerEv{known: true}
	dropped := layerEv{known: true, dropped: true, loss: 0.3}
	lossy := func(l float64) layerEv { return layerEv{known: true, loss: l} }
	calls := map[string]State{"zoom": Slow, "teams": Slow, "meet": Slow, "slack": Slow}
	cases := []struct {
		name      string
		incidents []model.Incident
		layers    [nLayers]layerEv
		tiles     map[string]State
		speed     speedWindows
		want      Cause
	}{
		{"lan, lost everything", inc(model.VerdictLAN, 10*time.Minute, 12*time.Minute, 1), lay(clean, clean, clean), nil, nil,
			Cause{Who: WhoRouter, Evidence: EvRouterDown}},
		{"lan, lossy", inc(model.VerdictLAN, 10*time.Minute, 12*time.Minute, 0.3), lay(clean, clean, clean), nil, nil,
			Cause{Who: WhoRouter, Evidence: EvRouterLossy}},
		{"no_network", inc(model.VerdictNoNetwork, 10*time.Minute, 12*time.Minute, 1), lay(clean, clean, clean), nil, nil,
			Cause{Who: WhoOffline, Evidence: EvNoNetwork}},
		{"isp, the edge dropped", inc(model.VerdictISP, 10*time.Minute, 12*time.Minute, 0.6), lay(clean, dropped, clean), nil, nil,
			Cause{Who: WhoProvider, Evidence: EvProviderDrop}},
		{"isp, lossy", inc(model.VerdictISP, 10*time.Minute, 12*time.Minute, 0.25), lay(clean, lossy(0.25), clean), nil, nil,
			Cause{Who: WhoProvider, Evidence: EvProviderLossy}},
		{"upstream", inc(model.VerdictUpstream, 10*time.Minute, 12*time.Minute, 0.5), lay(clean, clean, dropped), nil, nil,
			Cause{Who: WhoInternet, Evidence: EvBeyond}},
		{"dns", inc(model.VerdictDNS, 10*time.Minute, 12*time.Minute, 0), lay(clean, clean, clean), nil, nil,
			Cause{Who: WhoNames, Evidence: EvDNS}},
		{"an incident within a minute before counts", inc(model.VerdictISP, 8*time.Minute, 9*time.Minute+30*time.Second, 1), lay(clean, clean, clean), nil, nil,
			Cause{Who: WhoProvider, Evidence: EvProviderDrop}},
		{"an incident 2 minutes before doesn't", inc(model.VerdictISP, 7*time.Minute, 8*time.Minute-time.Second, 1), lay(clean, clean, clean), map[string]State{"zoom": Down}, nil,
			Cause{Who: WhoApp, Vendor: "zoom", Evidence: EvAppOnly}},
		{"service incidents don't count", inc(model.VerdictService, 10*time.Minute, 12*time.Minute, 1), lay(clean, clean, clean), nil, nil,
			Cause{Who: WhoUnknown}},
		{"an incident beats layer evidence", inc(model.VerdictISP, 10*time.Minute, 12*time.Minute, 1), lay(dropped, dropped, dropped), nil, nil,
			Cause{Who: WhoProvider, Evidence: EvProviderDrop}},
		{"speed test beats the provider", inc(model.VerdictISP, 10*time.Minute, 12*time.Minute, 1), lay(clean, dropped, clean), nil,
			speedWindows{{Start: at(11 * time.Minute), End: at(11*time.Minute + 20*time.Second)}},
			Cause{Who: WhoSpeedtest, Evidence: EvSpeedtest}},
		{"layers: gateway lost too", nil, lay(dropped, dropped, dropped), nil, nil, Cause{Who: WhoRouter, Evidence: EvRouterDown}},
		{"layers: gateway fine, edge lost", nil, lay(clean, dropped, dropped), nil, nil, Cause{Who: WhoProvider, Evidence: EvProviderDrop}},
		{"layers: edge fine, anycast lost", nil, lay(clean, clean, dropped), nil, nil, Cause{Who: WhoInternet, Evidence: EvBeyond}},
		{"layers: 6% provider loss, clean gateway", nil, lay(lossy(0.005), lossy(0.06), lossy(0.06)), calls, nil,
			Cause{Who: WhoProvider, Evidence: EvProviderLossy}},
		{"layers: gateway loss", nil, lay(lossy(0.03), lossy(0.03), lossy(0.03)), calls, nil, Cause{Who: WhoRouter, Evidence: EvRouterLossy}},
		{"layers: 1.5% gateway loss is neither", nil, lay(lossy(0.015), lossy(0.06), clean), calls, nil, Cause{Who: WhoCallsPath, Evidence: EvCallsPath}},
		{"layers: unknown gateway is not a clean one", nil, lay(layerEv{}, lossy(0.05), clean), nil, nil, Cause{Who: WhoUnknown}},
		{"layers: unknown gateway, edge lost", nil, lay(layerEv{}, dropped, clean), nil, nil, Cause{Who: WhoUnknown}},
		{"layers: unknown edge, anycast lost", nil, lay(clean, layerEv{}, dropped), nil, nil, Cause{Who: WhoUnknown}},
		{"one app", nil, lay(clean, clean, clean), map[string]State{"zoom": Down}, nil, Cause{Who: WhoApp, Vendor: "zoom", Evidence: EvAppOnly}},
		{"one vendor", nil, lay(clean, clean, clean), map[string]State{"meet": Slow, "gws": Down, "youtube": Slow}, nil,
			Cause{Who: WhoApp, Vendor: "google", Evidence: EvVendorOnly}},
		{"OneDrive groups with Microsoft 365 and Teams", nil, lay(clean, clean, clean), map[string]State{"teams": Down, "m365": Down, "onedrive": Down}, nil,
			Cause{Who: WhoApp, Vendor: "microsoft", Evidence: EvVendorOnly}},
		{"two vendors", nil, lay(clean, clean, clean), map[string]State{"zoom": Down, "gws": Down}, nil, Cause{Who: WhoUnknown}},
		{"every call tile choppy", nil, lay(clean, clean, clean), calls, nil, Cause{Who: WhoCallsPath, Evidence: EvCallsPath}},
		{"call tiles and an app", nil, lay(clean, clean, clean), map[string]State{"zoom": Slow, "teams": Slow, "meet": Slow, "slack": Slow, "github": Slow}, nil,
			Cause{Who: WhoUnknown}},
		{"nothing bad", nil, lay(clean, clean, clean), nil, nil, Cause{Who: WhoUnknown}},
	}
	for _, c := range cases {
		in := attrIn{from: from, to: to, now: now, incidents: c.incidents, layers: c.layers, tiles: allTiles(c.tiles)}
		if c.speed != nil {
			in.speed = c.speed
		}
		got := attribute(in)
		if got != c.want {
			t.Errorf("%s: got %+v, want %+v", c.name, got, c.want)
		}
	}

	// one vendor needs another vendor's tile that was good to compare with
	only := []tileEv{{id: "meet", vendor: "google", kind: profile.TileCall, worst: Down}, {id: "gws", vendor: "google", kind: profile.TileApp, worst: Good}}
	if got := attribute(attrIn{from: from, to: to, now: now, tiles: only}); got.Who != WhoUnknown {
		t.Errorf("only Google covered: got %+v", got)
	}
	// an ongoing episode and an ongoing incident
	got := attribute(attrIn{from: from, now: now, incidents: []model.Incident{{Kind: model.VerdictISP, Start: at(30 * time.Minute), PeakLoss: 1}}})
	if got.Who != WhoProvider {
		t.Errorf("ongoing: got %+v", got)
	}
}

func TestLayerEvidence(t *testing.T) {
	sec := time.Second
	now := at(time.Hour)
	mins := func(ok, lost int, n int) []minute {
		out := make([]minute, n)
		for i := range out {
			out[i] = minute{ok: uint16(ok), lost: uint16(lost)}
		}
		return out
	}
	// 6% loss, scattered: lossy, not dropped
	if ev := layerEvidence(mins(56, 4, 3), nil, t0, at(3*time.Minute), now); !ev.known || ev.dropped || ev.loss < 0.066 || ev.loss > 0.067 {
		t.Errorf("lossy: got %+v", ev)
	}
	// a 40 s drop, kept as a window (from layerDrops), a week later
	_, s := series(sec, t0, rep(".", 50)+rep("x", 40)+rep(".", 50))
	ws, _ := layerDrops([]runSet{s}, now)
	if len(ws) != 1 {
		t.Fatalf("windows: %+v", ws)
	}
	if ev := layerEvidence(mins(20, 40, 1), ws, at(60*sec), at(70*sec), at(7*24*time.Hour)); !ev.dropped {
		t.Errorf("drop: got %+v", ev)
	}
	if ev := layerEvidence(mins(60, 0, 1), ws, at(5*time.Minute), at(6*time.Minute), now); ev.dropped {
		t.Errorf("another minute: got %+v", ev)
	}
	// every series of the layer must have dropped
	_, ok := series(sec, t0, rep(".", 140))
	if ws, _ := layerDrops([]runSet{s, ok}, now); ws != nil {
		t.Errorf("one of two series: %+v", ws)
	}
	// not measured
	if ev := layerEvidence(mins(0, 0, 3), nil, t0, at(time.Hour), now); ev.known {
		t.Errorf("no checks: got %+v", ev)
	}
}

func TestCauseKind(t *testing.T) {
	want := map[Who]string{
		WhoRouter: KindHome, WhoOffline: KindHome, WhoProvider: KindProvider, WhoApp: KindApp,
		WhoSpeedtest: KindSpeedtest, WhoInternet: KindOther, WhoNames: KindOther, WhoCallsPath: KindOther, WhoUnknown: KindOther,
	}
	for w, k := range want {
		if got := (Cause{Who: w}).Kind(); got != k {
			t.Errorf("%s: got %s, want %s", w, got, k)
		}
	}
}

func TestSpeedOverlap(t *testing.T) {
	now := at(time.Hour)
	wins := speedWindows{{Start: at(10 * time.Minute), End: at(11 * time.Minute)}}
	cases := []struct {
		name     string
		from, to time.Time
		want     bool
	}{
		{"inside", at(10*time.Minute + 5*time.Second), at(10*time.Minute + 6*time.Second), true},
		{"before", at(9 * time.Minute), at(10*time.Minute - time.Second), false},
		{"within MaskTail after", at(11*time.Minute + speedtest.MaskTail), at(12 * time.Minute), true},
		{"after MaskTail", at(11*time.Minute + speedtest.MaskTail + time.Second), at(12 * time.Minute), false},
		{"ongoing episode", at(9 * time.Minute), time.Time{}, true},
	}
	for _, c := range cases {
		if got := speedOverlap(c.from, c.to, now, wins); got != c.want {
			t.Errorf("%s: got %v", c.name, got)
		}
	}
	if !speedOverlap(at(55*time.Minute), at(56*time.Minute), now, speedWindows{{Start: at(50 * time.Minute)}}) {
		t.Error("running test")
	}
	if speedOverlap(t0, now, now, nil) {
		t.Error("speed tests off")
	}
	// the live windows answer the same
	var live speedtest.Windows
	live.Add(speedtest.Window{Start: at(10 * time.Minute), End: at(11 * time.Minute)})
	for _, c := range cases[:4] {
		if got := speedOverlap(c.from, c.to, now, &live); got != c.want {
			t.Errorf("live %s: got %v", c.name, got)
		}
	}
}
