package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/s3test"
	"github.com/fuck-you-isp/fyisp/internal/s3test/s3fake"
	"github.com/fuck-you-isp/fyisp/internal/speedtest"
	"github.com/fuck-you-isp/fyisp/internal/storage"
	"github.com/fuck-you-isp/fyisp/internal/store"
)

// storageLab is a Local with storage over a fake S3 server reached as
// s3.test (127.0.0.1).
type storageLab struct {
	h    http.Handler
	srv  *s3fake.Server
	dir  string
	port string
	db   *store.SQLite
}

func storageLookup(_ context.Context, _, host string) ([]netip.Addr, error) {
	switch {
	case host == "s3.test" || strings.HasSuffix(host, ".s3.test"):
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	case host == "meta.test":
		return []netip.Addr{netip.MustParseAddr("169.254.169.254")}, nil
	}
	return nil, errors.New("no such host")
}

func newStorageLab(t *testing.T, o LocalOptions) *storageLab {
	t.Helper()
	l := &storageLab{srv: s3fake.New("bkt"), dir: t.TempDir()}
	l.srv.Start()
	t.Cleanup(l.srv.Close)
	_, l.port, _ = net.SplitHostPort(l.srv.Addr())
	_, d := fixture(t, time.Now())
	e := s3test.New(s3test.Options{Lookup: storageLookup, IsSelf: func(netip.Addr) bool { return false }})
	d.Storage = storage.Open(storage.Options{Dir: l.dir, Engine: e, Lookup: storageLookup})
	// Campaigns over a database of their own and a queue of their own.
	db, err := store.Open(t.TempDir(), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	l.db = db
	q := speedtest.NewQueue(speedtest.QueueOptions{})
	cs := storage.NewCampaigns(storage.CampaignOptions{Store: db, Queue: q, Engine: e, Targets: d.Storage})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{}, 2)
	go func() { q.Run(ctx); done <- struct{}{} }()
	go func() { cs.Run(ctx); done <- struct{}{} }()
	t.Cleanup(func() { cancel(); <-done; <-done; db.Close() })
	d.Campaigns, d.StorageData = cs, db
	o.Addr, o.CSRFToken = "127.0.0.1:3000", "tok123"
	l.h = Local(d, o)
	return l
}

func (l *storageLab) post(path, body string) (int, map[string]any, string) {
	w := doBody(l.h, "POST", path, body, goodHdr)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out, w.Body.String()
}

func (l *storageLab) addBody() string {
	return `{"name":"Lab","endpoint":"http://s3.test:` + l.port + `","bucket":"bkt","addressing":"path"}`
}

var storagePOSTs = []string{"/api/storage/target/add", "/api/storage/target/update?id=t_1", "/api/storage/target/delete?id=t_1",
	"/api/storage/target/forget-keys?id=t_1", "/api/storage/check?id=t_1", "/api/storage/run", "/api/storage/campaign/start",
	"/api/storage/campaign/stop?id=1", "/api/storage/campaign/pause?id=1", "/api/storage/campaign/resume?id=1", "/api/storage/campaign/keys?id=1"}

// TestStorageWritesGuarded: every storage write needs the admin token, the
// write checks and POST; a body must be JSON.
func TestStorageWritesGuarded(t *testing.T) {
	l := newStorageLab(t, LocalOptions{AdminToken: testAdmin})
	admin := map[string]string{HeaderAdmin: testAdmin}
	with := func(base map[string]string, more ...map[string]string) map[string]string {
		out := map[string]string{}
		for _, m := range append([]map[string]string{base}, more...) {
			for k, v := range m {
				out[k] = v
			}
		}
		return out
	}
	for _, p := range storagePOSTs {
		for name, c := range map[string]struct {
			method string
			hdr    map[string]string
			code   int
		}{
			"no token":     {"POST", goodHdr, 401},
			"no csrf":      {"POST", with(goodHdr, admin, map[string]string{HeaderCSRF: ""}), 403},
			"cross origin": {"POST", with(goodHdr, admin, map[string]string{"Origin": "http://evil.test", "Sec-Fetch-Site": "cross-site"}), 403},
			"get":          {"GET", with(goodHdr, admin), 405},
		} {
			if w := doBody(l.h, c.method, p, `{}`, c.hdr); w.Code != c.code {
				t.Errorf("%s %s (%s) = %d, want %d", c.method, p, name, w.Code, c.code)
			}
		}
	}
	for _, ct := range []string{"text/plain", "application/x-www-form-urlencoded", ""} {
		if w := doBody(l.h, "POST", "/api/storage/target/add", l.addBody(), with(goodHdr, admin, map[string]string{"Content-Type": ct})); w.Code != 415 {
			t.Errorf("Content-Type %q = %d, want 415", ct, w.Code)
		}
	}
	if w := do(l.h, "GET", "/api/storage", map[string]string{"Host": "127.0.0.1:3000"}); w.Code != 401 {
		t.Errorf("GET api/storage without the token = %d", w.Code)
	}

	_, d := fixture(t, time.Now())
	h := Local(d, LocalOptions{Addr: "127.0.0.1:3000", CSRFToken: "tok123"})
	for _, p := range append(storagePOSTs, "/api/storage") {
		method := "POST"
		if p == "/api/storage" {
			method = "GET"
		}
		if w := doBody(h, method, p, `{}`, goodHdr); w.Code != 404 {
			t.Errorf("no storage: %s %s = %d", method, p, w.Code)
		}
	}
}

func TestStorageAPI(t *testing.T) {
	l := newStorageLab(t, LocalOptions{})
	code, out, raw := l.post("/api/storage/target/add", l.addBody())
	if code != 201 {
		t.Fatalf("add = %d %s", code, raw)
	}
	tg := out["target"].(map[string]any)
	id := tg["id"].(string)
	for k, want := range map[string]any{"name": "Lab", "scheme": "http", "host": "s3.test", "port": float64(mustAtoi(t, l.port)), "region": "us-east-1",
		"bucket": "bkt", "addressing": "path", "has_ca": false, "network": "private", "last_check": nil} {
		if tg[k] != want {
			t.Errorf("%s = %v, want %v", k, tg[k], want)
		}
	}

	for _, c := range []struct {
		path, body string
		code       int
		errCode    string
		field      string
	}{
		{"/api/storage/target/add", l.addBody(), 409, "name_taken", ""},
		{"/api/storage/target/add", `{"name":"x","endpoint":"https://meta.test"}`, 400, "addr_refused", "endpoint"},
		{"/api/storage/target/add", `{"name":"x","endpoint":"https://s3.test","region":"a b"}`, 400, "bad_request", "region"},
		{"/api/storage/target/add", `{"name":"x"}`, 400, "bad_request", "endpoint"},
		{"/api/storage/target/add", `{"name":"x","endpoint":"https://s3.test","keys":{}}`, 400, "bad_request", ""},
		{"/api/storage/target/add", `{"name":"x","endpoint":"https://s3.test","AKIAhunter2":1}`, 400, "bad_request", ""},
		{"/api/storage/check?id=" + id, `{"keys":{"hunter2Secret":"x"}}`, 400, "bad_request", ""},
		{"/api/storage/target/add", `{"name":"x","endpoint":"https://s3.test"} {}`, 400, "bad_request", ""},
		{"/api/storage/target/add", ``, 400, "bad_request", ""},
		{"/api/storage/target/add?id=" + id, l.addBody(), 400, "bad_request", ""},
		{"/api/storage/target/add", `{"name":"x","endpoint":"https://s3.test","ca_pem":"` + strings.Repeat("x", 100<<10) + `"}`, 413, "too_large", ""},
		{"/api/storage/target/update", `{}`, 400, "bad_request", ""},
		{"/api/storage/target/update?id=t_00000000", `{"name":"y"}`, 404, "not_found", ""},
		{"/api/storage/target/delete?id=" + id, `{"x":1}`, 400, "bad_request", ""},
		{"/api/storage/check?id=" + id, `{"keys":{"access_key":"AK EXAMPLE","secret":"hunter2-secret"}}`, 400, "bad_request", "keys"},
		{"/api/storage/check?id=" + id, `{"keys":{"access_key":"AKEXAMPLE","secret":"hunter2-secret","session_token":"tok"}}`, 400, "bad_request", "keys"},
		{"/api/storage/check?id=t_00000000", ``, 404, "not_found", ""},
		{"/api/storage/nope", ``, 404, "not_found", ""},
	} {
		code, out, raw := l.post(c.path, c.body)
		if code != c.code || out["code"] != c.errCode || (c.field != "" && out["field"] != c.field) {
			t.Errorf("%s %.60s = %d %s, want %d %s %s", c.path, c.body, code, raw, c.code, c.errCode, c.field)
		}
		if strings.Contains(raw, "hunter2") || strings.Contains(raw, "AKIA") {
			t.Errorf("%s echoed the secret: %s", c.path, raw)
		}
	}

	// A check with keys, then without (unsigned).
	keys := `{"keys":{"access_key":"` + l.srv.AccessKey + `","secret":"` + l.srv.Secret + `"}}`
	code, out, raw = l.post("/api/storage/check?id="+id, keys)
	if code != 200 {
		t.Fatalf("check = %d %s", code, raw)
	}
	res := out["result"].(map[string]any)
	if res["class"] != "ok" || res["group"] != "ok" || res["keys"] != "work" || res["http_status"] != float64(200) || res["during_test"] != false {
		t.Errorf("check %v", res)
	}
	if strings.Contains(raw, l.srv.Secret) || strings.Contains(raw, l.srv.AccessKey) {
		t.Errorf("check echoed keys: %s", raw)
	}
	code, out, raw = l.post("/api/storage/check?id="+id, `{"keys":{"access_key":"`+l.srv.AccessKey+`","secret":"wrong-secret"}}`)
	if res := out["result"].(map[string]any); code != 200 || res["class"] != "http_auth" || res["group"] != "config" || res["keys"] != "refused" {
		t.Errorf("wrong secret: %d %s", code, raw)
	}
	if code, _, raw := l.post("/api/storage/check?id="+id, ``); code != 200 || !strings.Contains(raw, `"keys":"none"`) {
		t.Errorf("unsigned check = %d %s", code, raw)
	}

	w := do(l.h, "GET", "/api/storage", goodHdr)
	var sum struct {
		At, Rev int64
		Targets []storageTargetJSON
	}
	if err := json.Unmarshal(w.Body.Bytes(), &sum); err != nil || w.Code != 200 || len(sum.Targets) != 1 || sum.Rev == 0 ||
		sum.Targets[0].LastCheck == nil || sum.Targets[0].LastCheck.Keys != "none" || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("GET = %d %s", w.Code, w.Body)
	}
	if w := do(l.h, "GET", "/api/storage?x=1", goodHdr); w.Code != 400 {
		t.Errorf("unknown parameter = %d", w.Code)
	}

	// A new region keeps the target and clears its check.
	code, out, raw = l.post("/api/storage/target/update?id="+id, `{"region":"eu-west-1"}`)
	if code != 200 || out["target"].(map[string]any)["last_check"] != nil || out["target"].(map[string]any)["region"] != "eu-west-1" {
		t.Errorf("update = %d %s", code, raw)
	}

	// One check at a time.
	entered, release := make(chan struct{}), make(chan struct{})
	l.srv.Fault = func(*http.Request) s3fake.Fault {
		close(entered)
		<-release
		return s3fake.Fault{}
	}
	done := make(chan struct{})
	go func() { l.post("/api/storage/check?id="+id, ``); close(done) }()
	<-entered
	if code, out, raw := l.post("/api/storage/check?id="+id, ``); code != 429 || out["code"] != "busy" {
		t.Errorf("second check = %d %s", code, raw)
	}
	close(release)
	<-done
	l.srv.Fault = nil

	// At most 32 targets.
	for i := 1; i < storage.MaxTargets; i++ {
		if code, _, raw := l.post("/api/storage/target/add", `{"name":"n`+strconv.Itoa(i)+`","endpoint":"https://s3.test"}`); code != 201 {
			t.Fatalf("add %d = %d %s", i, code, raw)
		}
	}
	if code, out, raw := l.post("/api/storage/target/add", `{"name":"one more","endpoint":"https://s3.test"}`); code != 409 || out["code"] != "too_many" {
		t.Errorf("target 33 = %d %s", code, raw)
	}
	if code, out, raw := l.post("/api/storage/target/delete?id="+id, ``); code != 200 || len(out) != 0 {
		t.Errorf("delete = %d %s", code, raw)
	}
	if code, _, _ := l.post("/api/storage/target/delete?id="+id, ``); code != 404 {
		t.Errorf("delete twice = %d", code)
	}
}

func mustAtoi(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// TestStorageFileBroken: a targets file fyisp can't use answers 503
// storage_file everywhere and stays as it is.
func TestStorageFileBroken(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, storage.FileName)
	if err := os.WriteFile(path, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, d := fixture(t, time.Now())
	d.Storage = storage.Open(storage.Options{Dir: dir, Lookup: storageLookup})
	h := Local(d, LocalOptions{Addr: "127.0.0.1:3000", CSRFToken: "tok123"})
	if w := do(h, "GET", "/api/storage", goodHdr); w.Code != 503 || !strings.Contains(w.Body.String(), `"code":"storage_file"`) {
		t.Errorf("GET = %d %s", w.Code, w.Body)
	}
	if w := doBody(h, "POST", "/api/storage/target/add", `{"name":"a","endpoint":"https://s3.test"}`, goodHdr); w.Code != 503 {
		t.Errorf("add = %d %s", w.Code, w.Body)
	}
	if b, _ := os.ReadFile(path); string(b) != "{broken" {
		t.Errorf("file now %q", b)
	}
}

func (l *storageLab) get(t *testing.T, path string) map[string]any {
	t.Helper()
	w := do(l.h, "GET", path, goodHdr)
	if w.Code != 200 {
		t.Fatalf("GET %s = %d %s", path, w.Code, w.Body)
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestStorageCampaignAPI: one round now with keys against the fake S3
// (seed, checks, transfers), read back as a campaign, a round and its
// attempts; the keys are dropped when it ends and never served. Then a
// schedule: its keys are saved, its target can't be deleted, and
// removing it deletes the keys.
func TestStorageCampaignAPI(t *testing.T) {
	l := newStorageLab(t, LocalOptions{})
	code, out, raw := l.post("/api/storage/target/add", l.addBody())
	if code != 201 {
		t.Fatalf("add = %d %s", code, raw)
	}
	id := out["target"].(map[string]any)["id"].(string)
	keys := `{"` + id + `":{"access_key":"` + l.srv.AccessKey + `","secret":"` + l.srv.Secret + `"}}`
	code, out, raw = l.post("/api/storage/run", `{"targets":["`+id+`"],"keys":`+keys+`}`)
	if code != 202 {
		t.Fatalf("run = %d %s", code, raw)
	}
	camp := int64(out["campaign"].(map[string]any)["id"].(float64))
	deadline := time.Now().Add(30 * time.Second)
	var sum map[string]any
	for {
		sum = l.get(t, "/api/storage")
		if len(sum["live"].([]any)) == 0 && len(sum["history"].([]any)) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the round didn't finish: %v", sum)
		}
		time.Sleep(50 * time.Millisecond)
	}
	h := sum["history"].([]any)[0].(map[string]any)
	if h["state"] != "done" || h["stop_reason"] != "finished" || h["rounds_done"] != float64(1) || h["bytes_down"].(float64) == 0 {
		t.Errorf("history %v", h)
	}
	if k := sum["targets"].([]any)[0].(map[string]any)["keys"].(map[string]any); k["state"] != "none" {
		t.Errorf("keys after the round: %v", k)
	}
	c := l.get(t, fmt.Sprintf("/api/storage?part=campaign&id=%d", camp))
	rounds := c["rounds"].([]any)
	if len(rounds) != 1 || c["cells"].(map[string]any)[id] != "o" {
		t.Fatalf("campaign %v", c)
	}
	if tp := c["throughput"].(map[string]any)[id].(map[string]any); len(tp["down_bps"].([]any)) != 1 || tp["down_bps"].([]any)[0] == nil {
		t.Errorf("throughput %v", tp)
	}
	rid := int64(rounds[0].(map[string]any)["id"].(float64))
	rd := l.get(t, fmt.Sprintf("/api/storage?part=round&id=%d", rid))
	var phases []string
	for _, a := range rd["attempts"].(map[string]any)[id].([]any) {
		phases = append(phases, a.(map[string]any)["phase"].(string))
	}
	if !slices.Contains(phases, "seed") || !slices.Contains(phases, "cold") || !slices.Contains(phases, "down") || !slices.Contains(phases, "up") {
		t.Errorf("phases %v", phases)
	}
	at := l.get(t, fmt.Sprintf("/api/storage?part=attempts&campaign=%d&phase=cold&limit=2", camp))
	if at["total"] != float64(storage.DefaultCold) || len(at["items"].([]any)) != 2 || at["next_before"] == nil {
		t.Errorf("attempts %v", at)
	}
	for _, p := range []string{"/api/storage", fmt.Sprintf("/api/storage?part=campaign&id=%d", camp), fmt.Sprintf("/api/storage?part=round&id=%d", rid),
		fmt.Sprintf("/api/storage?part=attempts&campaign=%d", camp), "/api/storage?part=campaigns", "/api/storage?part=live"} {
		if b := do(l.h, "GET", p, goodHdr).Body.String(); strings.Contains(b, l.srv.Secret) || strings.Contains(b, l.srv.AccessKey) {
			t.Errorf("%s serves the keys: %s", p, b)
		}
	}

	// A schedule saves its keys; its target is in use; removing it deletes them.
	code, out, raw = l.post("/api/storage/campaign/start", `{"targets":["`+id+`"],"shape":"recurring","interval_s":600,"cold":1,
		"days":[1,2,3,4,5,6,7],"from":"00:00","to":"23:59","tz":"America/New_York","keys":`+keys+`}`)
	if code != 201 {
		t.Fatalf("start = %d %s", code, raw)
	}
	rec := int64(out["campaign"].(map[string]any)["id"].(float64))
	if sc := out["campaign"].(map[string]any)["schedule"].(map[string]any); sc["from"] != "00:00" || len(sc["days"].([]any)) != 7 || sc["keys_saved"] != true {
		t.Errorf("schedule %v", sc)
	}
	if code, out, _ := l.post("/api/storage/campaign/start", `{"targets":["`+id+`"],"shape":"recurring","interval_s":600,"cold":1,
		"days":[1],"from":"19:00","to":"20:00","tz":"UTC"}`); code != 409 || out["code"] != "schedule_exists" {
		t.Errorf("second schedule = %d %v", code, out)
	}
	tg := l.get(t, "/api/storage")["targets"].([]any)[0].(map[string]any)
	if k := tg["keys"].(map[string]any); k["state"] != "saved" || k["campaign"] != float64(rec) || tg["in_use_by"] != float64(rec) {
		t.Errorf("target %v", tg)
	}
	if raw, _ := os.ReadFile(filepath.Join(l.dir, storage.FileName)); !strings.Contains(string(raw), l.srv.Secret) {
		t.Errorf("the schedule's keys aren't saved")
	}
	if code, out, _ := l.post("/api/storage/target/delete?id="+id, ``); code != 409 || out["code"] != "in_use" {
		t.Errorf("delete in use = %d %v", code, out)
	}
	if code, out, raw := l.post(fmt.Sprintf("/api/storage/campaign/pause?id=%d", rec), ``); code != 200 || out["campaign"].(map[string]any)["state"] != "paused" {
		t.Errorf("pause = %d %s", code, raw)
	}
	if code, out, _ := l.post(fmt.Sprintf("/api/storage/campaign/pause?id=%d", rec), ``); code != 409 || out["code"] != "bad_state" {
		t.Errorf("pause twice = %d %v", code, out)
	}
	if code, out, raw := l.post(fmt.Sprintf("/api/storage/campaign/stop?id=%d", rec), ``); code != 200 || out["campaign"].(map[string]any)["stop_reason"] != "removed" {
		t.Errorf("stop = %d %s", code, raw)
	}
	if raw, _ := os.ReadFile(filepath.Join(l.dir, storage.FileName)); strings.Contains(string(raw), l.srv.Secret) {
		t.Errorf("the removed schedule's keys are still saved")
	}
	if code, out, _ := l.post(fmt.Sprintf("/api/storage/campaign/stop?id=%d", rec), ``); code != 404 || out["code"] != "not_found" {
		t.Errorf("stop twice = %d %v", code, out)
	}
	for _, c := range []struct {
		path, body, code string
	}{
		{"/api/storage/campaign/start", `{"targets":["` + id + `"],"shape":"on_demand","interval_s":60,"cold":20,"down_bytes":1073741824,"count":2,"keys":` + keys + `}`, "interval_too_short"},
		{"/api/storage/campaign/start", `{"targets":["` + id + `"],"shape":"weekly","interval_s":600,"cold":1}`, "bad_request"},
		{"/api/storage/campaign/start", `{"targets":["` + id + `"],"shape":"recurring","interval_s":600,"cold":1,"days":[8],"from":"19:00","to":"20:00","tz":"UTC"}`, "bad_request"},
		{"/api/storage/run", `{"targets":["` + id + `"],"keys":{"` + id + `":{"access_key":"AKIA","secret":"wrong-hunter2"}}}`, "keys_refused"},
		{"/api/storage/campaign/keys?id=999", `{"keys":` + keys + `}`, "not_found"},
	} {
		code, out, raw := l.post(c.path, c.body)
		if out["code"] != c.code || strings.Contains(raw, "hunter2") || strings.Contains(raw, l.srv.Secret) {
			t.Errorf("%s %.80s = %d %s, want %s", c.path, c.body, code, raw, c.code)
		}
	}
	est := l.get(t, "/api/storage?part=estimate&targets="+id+"&keyed="+id+"&shape=recurring&interval_s=600&cold=2&down_bytes=1048576&days=1,2&from=19:00&to=21:00")
	if est["rounds"] != float64(12) || est["per_window"].(map[string]any)["down"].(float64) < 12<<20 || est["per_30d"] == nil {
		t.Errorf("estimate %v", est)
	}
	for _, p := range []string{"/api/storage?part=nope", "/api/storage?part=campaign", "/api/storage?part=campaign&id=x", "/api/storage?part=attempts&campaign=1&class=a%20b",
		"/api/storage?part=estimate&targets=" + id + "&shape=on_demand&cold=99", "/api/storage?part=round&id=1&x=1"} {
		if w := do(l.h, "GET", p, goodHdr); w.Code != 400 {
			t.Errorf("GET %s = %d", p, w.Code)
		}
	}
	if w := do(l.h, "GET", "/api/storage?part=campaign&id=999", goodHdr); w.Code != 404 {
		t.Errorf("unknown campaign = %d", w.Code)
	}
}

// TestStorageReport: a campaign's report as Markdown (with its finding,
// and hosts only when asked) and as CSV; never the endpoint's address.
func TestStorageReport(t *testing.T) {
	l := newStorageLab(t, LocalOptions{})
	_, out, _ := l.post("/api/storage/target/add", l.addBody())
	id := out["target"].(map[string]any)["id"].(string)
	code, out, raw := l.post("/api/storage/run", `{"targets":["`+id+`"]}`)
	if code != 202 {
		t.Fatalf("run = %d %s", code, raw)
	}
	camp := int64(out["campaign"].(map[string]any)["id"].(float64))
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if len(l.get(t, "/api/storage")["live"].([]any)) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the round didn't finish")
		}
	}
	c := l.get(t, fmt.Sprintf("/api/storage?part=campaign&id=%d", camp))
	if f := c["finding"].(map[string]any); f["kicker"] != "early" || c["cards"].([]any)[0].(map[string]any)["cold_n"] != float64(storage.DefaultCold) {
		t.Errorf("finding %v, cards %v", f, c["cards"])
	}
	md := do(l.h, "GET", fmt.Sprintf("/api/storage/report?campaign=%d&format=md", camp), goodHdr)
	body := md.Body.String()
	if md.Code != 200 || !strings.HasPrefix(md.Header().Get("Content-Type"), "text/markdown") || !strings.Contains(body, "## Finding") ||
		!strings.Contains(body, "| Lab |") || strings.Contains(body, "s3.test") || strings.Contains(body, "127.0.0.1") {
		t.Errorf("md %d %q", md.Code, body)
	}
	hosts := do(l.h, "GET", fmt.Sprintf("/api/storage/report?campaign=%d&format=md&hosts=1", camp), goodHdr).Body.String()
	if !strings.Contains(hosts, "| s3.test/bkt |") {
		t.Errorf("hosts=1: %q", hosts)
	}
	csvw := do(l.h, "GET", fmt.Sprintf("/api/storage/report?campaign=%d&format=csv", camp), goodHdr)
	if csvw.Code != 200 || !strings.Contains(csvw.Header().Get("Content-Disposition"), "attachment") || strings.Count(csvw.Body.String(), "\n") != storage.DefaultCold+storage.DefaultWarm+1 {
		t.Errorf("csv %d %q", csvw.Code, csvw.Body)
	}
	for _, p := range []string{"/api/storage/report?campaign=999&format=md", "/api/storage/report?campaign=1&format=pdf", "/api/storage/report?format=md"} {
		if w := do(l.h, "GET", p, goodHdr); w.Code != 404 && w.Code != 400 {
			t.Errorf("%s = %d", p, w.Code)
		}
	}
}
