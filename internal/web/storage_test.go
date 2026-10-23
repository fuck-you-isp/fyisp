package web

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/s3test"
	"github.com/fuck-you-isp/fyisp/internal/s3test/s3fake"
	"github.com/fuck-you-isp/fyisp/internal/storage"
)

// storageLab is a Local with storage over a fake S3 server reached as
// s3.test (127.0.0.1).
type storageLab struct {
	h    http.Handler
	srv  *s3fake.Server
	dir  string
	port string
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
	"/api/storage/check?id=t_1"}

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
