//go:build netns && linux

package netns

import (
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"
)

// TestInvestigateToken: on-demand traces need the admin token like every
// route, and with it they start through an --allow-host name (from behind a
// proxy) and through 127.0.0.1, and their rounds run. The speed-test
// controls take the same token through the same proxy.
func TestInvestigateToken(t *testing.T) {
	parallel(t)
	h := newHarness(t)

	type resp struct {
		code int
		body string
	}
	csrf := func() string {
		m := regexp.MustCompile(`<meta name="fyisp-token" content="([^"]+)">`).FindSubmatch(h.get(t, "/"))
		if m == nil {
			t.Fatal("no CSRF token in the page")
		}
		return string(m[1])
	}
	post := func(tok, host, path, body string, hdr map[string]string) resp {
		t.Helper()
		var r io.Reader
		if body != "" {
			r = strings.NewReader(body)
		}
		req, _ := http.NewRequest("POST", "http://"+listenAddr+path, r)
		req.Host = host
		req.Header.Set("X-FYISP-Token", tok)
		req.Header.Set("Origin", "http://"+host)
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		res, err := h.client.Do(req)
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return resp{res.StatusCode, string(b)}
	}
	started := func(r resp, what string) {
		t.Helper()
		var inv struct {
			Target, State string
			RoundsMax     int `json:"rounds_max"`
		}
		_ = json.Unmarshal([]byte(r.body), &inv)
		if r.code != http.StatusOK || inv.State != "running" || inv.RoundsMax != 250 {
			t.Errorf("%s: %d %s, want 200 running", what, r.code, r.body)
		}
	}
	xff := map[string]string{"X-Forwarded-For": "203.0.113.99"}

	xffNoAuth := map[string]string{"X-Forwarded-For": "203.0.113.99", noAuth: "1"}

	fy := h.startFyisp(t, "--ephemeral", "--allow-host", "fyisp.example.com")
	h.waitReady(t, fy)
	h.stopAtEnd(t, fy)
	tok := csrf()
	if r := post(tok, "fyisp.example.com", "/api/investigate?target=alpha", "", xffNoAuth); r.code != http.StatusUnauthorized {
		t.Errorf("trace without the token: %d %s, want 401", r.code, r.body)
	}
	started(post(tok, "fyisp.example.com", "/api/investigate?target=alpha", "", xff), "through the allowed host name")
	started(post(tok, listenAddr, "/api/investigate?target=bravo", "", nil), "through 127.0.0.1")
	if r := post(tok, "fyisp.example.com", "/api/speed/config", `{"skip": []}`, xffNoAuth); r.code != http.StatusUnauthorized {
		t.Errorf("speed control without the token: %d %s, want 401", r.code, r.body)
	}
	if r := post(tok, "fyisp.example.com", "/api/speed/config", `{"skip": []}`, xff); r.code != http.StatusOK {
		t.Errorf("speed control through the proxy with the token: %d %s, want 200", r.code, r.body)
	}
	// The trace runs: rounds every 2.5 s.
	deadline := time.Now().Add(30 * time.Second)
	rounds := 0
	for time.Now().Before(deadline) && rounds < 3 {
		time.Sleep(time.Second)
		var tr struct {
			Investigation *struct {
				RoundsDone int `json:"rounds_done"`
			} `json:"investigation"`
		}
		_ = json.Unmarshal(h.get(t, "/api/trace?target=alpha"), &tr)
		if tr.Investigation != nil {
			rounds = tr.Investigation.RoundsDone
		}
	}
	t.Logf("alpha: %d rounds", rounds)
	if rounds < 3 {
		t.Errorf("alpha's trace ran %d rounds in 30 s, want 3 or more", rounds)
	}
}
