package web

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

func sameOriginHdr(host string) map[string]string {
	return map[string]string{"Host": host, "Origin": "http://" + host, "Sec-Fetch-Site": "same-origin", HeaderCSRF: "tok123"}
}

// Slots bound on-demand traces: with sessions that never end, 4 start and
// the 5th is busy.
func TestInvestigateSlots(t *testing.T) {
	d, ft, _ := traceFixture(t)
	p := d.Profile()
	for i := range 10 {
		p.Targets = append(p.Targets, model.Target{Name: fmt.Sprintf("S%d", i), Group: "common"})
	}
	h := Local(d, LocalOptions{Addr: "0.0.0.0:3000", AdminToken: "adm", CSRFToken: "tok123"})
	admin := sameOriginHdr("192.168.1.5:3000")
	admin[HeaderAdmin] = "adm"
	for i := range MaxInvestigations {
		if w := do(h, "POST", fmt.Sprintf("/api/investigate?target=S%d", i), admin); w.Code != 200 {
			t.Fatal(w.Code)
		}
	}
	for i := MaxInvestigations; i < 10; i++ {
		if w := do(h, "POST", fmt.Sprintf("/api/investigate?target=S%d", i), admin); w.Code != 429 || !json.Valid(w.Body.Bytes()) {
			t.Fatalf("5th = %d", w.Code)
		}
	}
	if len(ft.started) != MaxInvestigations {
		t.Fatalf("started %d", len(ft.started))
	}
}
