package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/metrics"
	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/s3test"
	"github.com/fuck-you-isp/fyisp/internal/s3test/s3fake"
	"github.com/fuck-you-isp/fyisp/internal/speedtest"
	"github.com/fuck-you-isp/fyisp/internal/storage"
	"github.com/fuck-you-isp/fyisp/internal/store"
	"github.com/fuck-you-isp/fyisp/internal/web"
)

// TestStoragePrivacyEndToEnd runs a storage round with keys against a fake
// S3 that plants a "public address" in everything it sends back (request
// IDs, error codes), through the real engine, campaigns, store, /metrics,
// the API and the report: the address appears nowhere, and neither do the
// secret, the signatures or the bucket's host, except the target's own
// settings where the owner sees them (the targets file and its API).
func TestStoragePrivacyEndToEnd(t *testing.T) {
	const secret = "s3cr3t-never-shown-anywhere"
	srv := s3fake.New("privacy-bucket")
	srv.Secret = secret
	srv.RequestID = mappedIP
	n := 0
	var fmu sync.Mutex
	srv.Fault = func(r *http.Request) s3fake.Fault {
		fmu.Lock()
		defer fmu.Unlock()
		if r.Method == "HEAD" || r.Method == "GET" && r.URL.Query().Has("list-type") {
			return s3fake.Fault{}
		}
		n++
		if n%3 == 0 {
			return s3fake.Fault{Status: 503, Code: mappedIP} // an error code that is an address
		}
		return s3fake.Fault{}
	}
	srv.StartTLS()
	defer srv.Close()
	_, port, _ := net.SplitHostPort(srv.Addr())

	var logs bytes.Buffer
	var logMu sync.Mutex
	log := slog.New(slog.NewTextHandler(writerFunc(func(p []byte) (int, error) {
		logMu.Lock()
		defer logMu.Unlock()
		return logs.Write(p)
	}), &slog.HandlerOptions{Level: slog.LevelDebug}))

	dir := t.TempDir()
	st, err := store.Open(dir, store.Options{Log: log})
	if err != nil {
		t.Fatal(err)
	}
	lookup := func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	}
	e := s3test.New(s3test.Options{Lookup: lookup, IsSelf: storage.IsSelf,
		Sleep: func(ctx context.Context, _ time.Duration) bool { return ctx.Err() == nil }}) // no second between requests
	svc := storage.Open(storage.Options{Dir: dir, Engine: e, Lookup: lookup})
	q := speedtest.NewQueue(speedtest.QueueOptions{})
	mc := metrics.New(func() *model.Profile { return &model.Profile{} })
	sm := metrics.NewStorage()
	mc.Register(sm)
	cs := storage.NewCampaigns(storage.CampaignOptions{Store: st, Queue: q, Engine: e, Targets: svc, Log: log, OnRound: sm.Observe})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{}, 2)
	go func() { q.Run(ctx); done <- struct{}{} }()
	go func() { cs.Run(ctx); done <- struct{}{} }()
	defer func() { cancel(); <-done; <-done }()

	ts, err := svc.Add(ctx, storage.Spec{Name: "Lab", Endpoint: "https://s3.test:" + port, Bucket: "privacy-bucket", Addressing: "path", CAPEM: srv.CertPEM()})
	if err != nil {
		t.Fatal(err)
	}
	c, err := cs.RunOnce(ctx, []string{ts.ID}, map[string]*s3test.Creds{ts.ID: s3test.NewCreds(srv.AccessKey, secret, "")})
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(30 * time.Second); len(cs.State()) > 0; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the round didn't end")
		}
	}

	deps := web.Deps{Profile: func() *model.Profile { return &model.Profile{} }, Store: st, Metrics: mc.Handler(), Log: log,
		Storage: svc, Campaigns: cs, StorageData: st,
		Status: func() web.Status { return web.Status{Version: "test"} }}
	local := web.Local(deps, web.LocalOptions{Addr: "127.0.0.1:3000", AdminToken: privacyAdmin})
	get := func(path string) string {
		req := httptest.NewRequest("GET", path, nil)
		req.Host = "127.0.0.1:3000"
		req.Header.Set("Authorization", "Bearer "+privacyAdmin)
		w := httptest.NewRecorder()
		local.ServeHTTP(w, req)
		b, _ := io.ReadAll(w.Result().Body)
		if w.Code != 200 {
			t.Errorf("%s: %d %s", path, w.Code, b)
		}
		return string(b)
	}
	id := strconv.FormatInt(c.ID, 10)
	camp := get("/api/storage?part=campaign&id=" + id)
	if !strings.Contains(camp, `"rounds_done":1`) {
		t.Fatalf("the round didn't run: %s", camp)
	}
	var rid string
	if i := strings.Index(camp, `"rounds":[{"id":`); i >= 0 {
		rid = strings.SplitN(camp[i+len(`"rounds":[{"id":`):], ",", 2)[0]
	}
	// Where the owner reads the target's own settings, the host may show.
	owner := []string{get("/api/storage")}
	if raw, err := os.ReadFile(filepath.Join(dir, storage.FileName)); err == nil {
		owner = append(owner, string(raw))
	}
	outputs := map[string]string{
		"campaign": camp, "round": get("/api/storage?part=round&id=" + rid), "attempts": get("/api/storage?part=attempts&campaign=" + id),
		"campaigns": get("/api/storage?part=campaigns"), "live": get("/api/storage?part=live"),
		"report": get("/api/storage/report?campaign=" + id + "&format=md"), "csv": get("/api/storage/report?campaign=" + id + "&format=csv"),
		"metrics": get("/metrics"),
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{store.DBName, store.DBName + "-wal"} {
		if b, err := os.ReadFile(filepath.Join(dir, f)); err == nil {
			outputs["file "+f] = string(b)
		}
	}
	logMu.Lock()
	outputs["log"] = logs.String()
	logMu.Unlock()
	if !strings.Contains(outputs["metrics"], "fyisp_storage_attempts_total{") || !strings.Contains(outputs["csv"], "\n1,") {
		t.Errorf("no storage data to check:\n%s\n%s", outputs["metrics"], outputs["csv"])
	}
	host := url.URL{Host: "s3.test:" + port}
	for name, o := range outputs {
		for _, bad := range []string{mappedIP, "198.51.100", secret, "Authorization", "AWS4-HMAC-SHA256", "Signature=", host.Host, "privacy-bucket"} {
			if strings.Contains(o, bad) {
				t.Errorf("%s holds %q", name, bad)
			}
		}
	}
	for i, o := range owner {
		for _, bad := range []string{mappedIP, secret, "Signature="} {
			if strings.Contains(o, bad) {
				t.Errorf("owner output %d holds %q", i, bad)
			}
		}
	}
}
