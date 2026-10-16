package web

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

// TestTraceModes: /api/trace and /api/trace/panel read the mode's hop
// series and report the modes this machine supports; investigate starts a
// trace per (target, mode) and refuses a mode the machine lacks.
func TestTraceModes(t *testing.T) {
	d, ft, cs := traceFixture(t)
	h := Local(d, LocalOptions{Addr: "127.0.0.1:3000", CSRFToken: "tok123"})
	good := map[string]string{"Host": "127.0.0.1:3000", "Origin": "http://127.0.0.1:3000", "Sec-Fetch-Site": "same-origin", HeaderCSRF: "tok123"}
	type modeResp struct {
		traceResp
		Mode  string          `json:"mode"`
		Port  int             `json:"port"`
		Modes []TraceModeInfo `json:"modes"`
	}
	var tr modeResp
	w := do(h, "GET", "/api/trace?target=Alpha&mode=tcp", good)
	if w.Code != 200 {
		t.Fatalf("trace tcp = %d %s", w.Code, w.Body.String())
	}
	_ = json.Unmarshal(w.Body.Bytes(), &tr)
	if tr.Mode != "tcp" || tr.Port != 443 || ft.asked != model.TraceTCP || tr.Traced || len(tr.Changes) != 0 || len(tr.Modes) != 3 {
		t.Errorf("trace tcp: mode %q port %d asked %v traced %v changes %d modes %+v", tr.Mode, tr.Port, ft.asked, tr.Traced, len(tr.Changes), tr.Modes)
	}
	tr = modeResp{}
	_ = json.Unmarshal(do(h, "GET", "/api/trace?target=Alpha", good).Body.Bytes(), &tr)
	if tr.Mode != "icmp" || tr.Port != 0 || ft.asked != model.TraceICMP || !tr.Traced || len(tr.Changes) == 0 {
		t.Errorf("trace icmp: mode %q port %d asked %v traced %v", tr.Mode, tr.Port, ft.asked, tr.Traced)
	}
	for _, q := range []string{"mode=sctp", "mode=tcp&mode=udp", "mode="} {
		if w := do(h, "GET", "/api/trace?target=Alpha&"+q, good); w.Code != 400 {
			t.Errorf("%s: %d, want 400", q, w.Code)
		}
	}
	if w := do(h, "GET", "/api/trace/panel?target=Alpha&mode=udp&hops=1,2", good); w.Code != 200 {
		t.Fatalf("panel udp = %d", w.Code)
	}
	cs.mu.Lock()
	keys := cs.last.Keys
	cs.mu.Unlock()
	if len(keys) != 2 || keys[0] != (model.SeriesKey{Target: "Alpha", Kind: model.KindTraceUDP, Hop: 1}) {
		t.Errorf("panel udp keys %+v", keys)
	}

	// One target, two modes: two traces.
	var r investigationJSON
	for _, m := range []string{"tcp", "icmp"} {
		w := do(h, "POST", "/api/investigate?target=Beta&mode="+m, good)
		_ = json.Unmarshal(w.Body.Bytes(), &r)
		if w.Code != 200 || r.Mode != m {
			t.Fatalf("investigate %s = %d %s", m, w.Code, w.Body.String())
		}
	}
	if !slices.Equal(ft.modes, []model.TraceMode{model.TraceTCP, model.TraceICMP}) {
		t.Errorf("started modes %v", ft.modes)
	}
	ft.sessions[0].advance(7)
	tr = modeResp{}
	_ = json.Unmarshal(do(h, "GET", "/api/trace?target=Beta&mode=tcp", good).Body.Bytes(), &tr)
	if tr.Investigation == nil || tr.Investigation.RoundsDone != 7 {
		t.Errorf("trace Beta tcp: %+v", tr.Investigation)
	}
	tr = modeResp{}
	_ = json.Unmarshal(do(h, "GET", "/api/trace?target=Beta", good).Body.Bytes(), &tr)
	if tr.Investigation == nil || tr.Investigation.RoundsDone != 0 {
		t.Errorf("trace Beta icmp: %+v", tr.Investigation)
	}

	// Windows: TCP via ICMP, no UDP.
	ft.avail = []TraceModeInfo{{Mode: "icmp", Available: true}, {Mode: "tcp", Port: 443, Available: true, HopsVia: "icmp"},
		{Mode: "udp", Port: 33434, Reason: "windows_firewall"}}
	w = do(h, "POST", "/api/investigate?target=Alpha&mode=udp", good)
	if w.Code != 400 || !strings.Contains(w.Body.String(), `"unsupported_mode"`) {
		t.Errorf("udp on windows = %d %s", w.Code, w.Body.String())
	}
	if w := do(h, "POST", "/api/investigate?target=Alpha&mode=bogus", good); w.Code != 400 {
		t.Errorf("bogus mode = %d", w.Code)
	}
	if len(ft.started) != 2 {
		t.Errorf("refused modes started traces: %v", ft.modes)
	}
}
