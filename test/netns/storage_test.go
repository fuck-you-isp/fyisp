//go:build netns && linux

package netns

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/s3test/s3fake"
)

// storageBucket is the fake S3's bucket (responder_test.go).
const storageBucket = "bkt"

// TestStorage: a storage campaign against two S3 endpoints in the lab, one
// of which stops answering TCP after the first rounds. Its cells turn
// failed while the other's stay ok, the finding points to the path, the
// transfers are marked like speed tests, and the keys are gone once the
// campaign ends.
func TestStorage(t *testing.T) {
	parallel(t)
	h := newHarness(t)
	fy := h.startFyisp(t, "--ephemeral")
	h.waitReady(t, fy)
	h.stopAtEnd(t, fy)
	m := regexp.MustCompile(`<meta name="fyisp-token" content="([^"]+)">`).FindSubmatch(h.get(t, "/"))
	if m == nil {
		t.Fatal("no CSRF token in the page")
	}
	csrf := string(m[1])
	post := func(path, body string) (int, map[string]any) {
		t.Helper()
		req, _ := http.NewRequest("POST", "http://"+listenAddr+path, strings.NewReader(body))
		req.Header.Set("X-FYISP-Token", csrf)
		req.Header.Set("Origin", "http://"+listenAddr)
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		req.Header.Set("Content-Type", "application/json")
		resp, err := h.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		var out map[string]any
		_ = json.Unmarshal(b, &out)
		return resp.StatusCode, out
	}
	fake := s3fake.New(storageBucket) // its default keys are the responder's
	var ids []string
	for i, name := range []string{"Provider A", "Provider B"} {
		code, out := post("/api/storage/target/add", fmt.Sprintf(`{"name":%q,"endpoint":"https://%s","bucket":%q,"addressing":"path"}`, name, storageAddrs[i], storageBucket))
		if code != 201 {
			t.Fatalf("add %s = %d %v", name, code, out)
		}
		ids = append(ids, out["target"].(map[string]any)["id"].(string))
	}
	if code, out := post("/api/storage/check?id="+ids[0], ""); code != 200 || out["result"].(map[string]any)["class"] != "ok" {
		t.Fatalf("check = %d %v", code, out)
	}
	keys := fmt.Sprintf(`{"access_key":%q,"secret":%q}`, fake.AccessKey, fake.Secret)
	code, out := post("/api/storage/campaign/start", fmt.Sprintf(`{"targets":[%q,%q],"shape":"on_demand","interval_s":60,"count":5,"cold":4,"warm":2,
		"down_bytes":1048576,"up_bytes":262144,"keys":{%q:%s,%q:%s}}`, ids[0], ids[1], ids[0], keys, ids[1], keys))
	if code != 201 {
		t.Fatalf("start = %d %v", code, out)
	}
	camp := int64(out["campaign"].(map[string]any)["id"].(float64))
	type campaignAPI struct {
		Campaign struct {
			State      string `json:"state"`
			RoundsDone int    `json:"rounds_done"`
		} `json:"campaign"`
		Rounds []struct {
			Start *int64 `json:"start"`
			End   *int64 `json:"end"`
		} `json:"rounds"`
		Cells   map[string]string `json:"cells"`
		Finding struct {
			Kicker string `json:"kicker"`
			Text   string `json:"text"`
		} `json:"finding"`
	}
	read := func() campaignAPI {
		var c campaignAPI
		if err := json.Unmarshal(h.get(t, fmt.Sprintf("/api/storage?part=campaign&id=%d", camp)), &c); err != nil {
			t.Fatal(err)
		}
		return c
	}
	wait := func(what string, d time.Duration, ok func(campaignAPI) bool) campaignAPI {
		t.Helper()
		for deadline := time.Now().Add(d); ; time.Sleep(2 * time.Second) {
			if c := read(); ok(c) {
				return c
			} else if time.Now().After(deadline) {
				t.Fatalf("%s: %+v", what, c)
			}
		}
	}
	wait("two rounds", 3*time.Minute, func(c campaignAPI) bool { return c.Campaign.RoundsDone >= 2 })
	h.nft(t, fmt.Sprintf("add rule inet fyt fault ip daddr %s tcp dport 443 drop", storageAddrs[0]))
	c := wait("the campaign's end", 6*time.Minute, func(c campaignAPI) bool { return c.Campaign.State == "done" })
	h.nft(t, "flush chain inet fyt fault")
	a, b := c.Cells[ids[0]], c.Cells[ids[1]]
	t.Logf("cells: A %q, B %q; finding %s: %s", a, b, c.Finding.Kicker, c.Finding.Text)
	if len(a) != 5 || !strings.HasPrefix(a, "oo") || !strings.Contains(a[2:], "f") || b != "ooooo" {
		t.Errorf("cells A %q B %q, want A ok then failed, B all ok", a, b)
	}
	if c.Finding.Kicker != "path" || !strings.Contains(c.Finding.Text, "Provider A") {
		t.Errorf("finding %+v, want the path to Provider A", c.Finding)
	}
	// The transfers are marked like speed tests (charts and Home).
	from, to := *c.Rounds[0].Start, *c.Rounds[len(c.Rounds)-1].End
	var ws struct {
		Windows []struct {
			Origin string `json:"origin"`
		} `json:"windows"`
	}
	if err := json.Unmarshal(h.get(t, fmt.Sprintf("/api/speed?part=windows&from=%d&to=%d", from, to+1000)), &ws); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, w := range ws.Windows {
		if w.Origin == "storage" {
			n++
		}
	}
	if n < 2 {
		t.Errorf("%d storage windows, want the rounds' transfers: %+v", n, ws.Windows)
	}
	// The keys went with the campaign.
	var sum struct {
		Targets []struct {
			Keys struct {
				State string `json:"state"`
			} `json:"keys"`
		} `json:"targets"`
	}
	if err := json.Unmarshal(h.get(t, "/api/storage"), &sum); err != nil {
		t.Fatal(err)
	}
	for _, tg := range sum.Targets {
		if tg.Keys.State != "none" {
			t.Errorf("keys %s after the campaign", tg.Keys.State)
		}
	}
}
