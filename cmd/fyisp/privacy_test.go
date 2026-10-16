package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/metrics"
	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/probe"
	"github.com/fuck-you-isp/fyisp/internal/web"
)

// mappedIP is the public address the fake STUN server reports to fyisp.
const mappedIP = "198.51.100.77"

// fakeSTUN answers every Binding Request with a Binding Success that carries
// XOR-MAPPED-ADDRESS and MAPPED-ADDRESS of mappedIP.
func fakeSTUN(t *testing.T) int {
	t.Helper()
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	t.Cleanup(func() { c.Close(); wg.Wait() })
	a := netip.MustParseAddr(mappedIP).As4()
	go func() {
		defer wg.Done()
		buf := make([]byte, 1500)
		for {
			n, from, err := c.ReadFromUDPAddrPort(buf)
			if err != nil {
				return
			}
			if n != 20 {
				continue
			}
			resp := make([]byte, 20, 44)
			copy(resp, buf[:20])
			binary.BigEndian.PutUint16(resp[0:2], 0x0101)
			binary.BigEndian.PutUint16(resp[2:4], 24)
			resp = append(resp, 0, 1, 0, 8, 0, 1, 0x9c, 0x40, a[0], a[1], a[2], a[3]) // MAPPED-ADDRESS
			x := []byte{0, 1, 0x9c ^ 0x21, 0x40 ^ 0x12, a[0] ^ 0x21, a[1] ^ 0x12, a[2] ^ 0xa4, a[3] ^ 0x42}
			resp = append(resp, 0, 0x20, 0, 8)
			resp = append(resp, x...) // XOR-MAPPED-ADDRESS
			c.WriteToUDPAddrPort(resp, from)
		}
	}()
	return c.LocalAddr().(*net.UDPAddr).Port
}

// TestUDPPrivacyEndToEnd runs the real probe runner against a STUN server
// that reports a known public address, through the store, /metrics and
// the dashboard API: the address appears nowhere, and neither do logs at
// debug level.
func TestUDPPrivacyEndToEnd(t *testing.T) {
	port := fakeSTUN(t)
	var logs bytes.Buffer
	var logMu sync.Mutex
	log := slog.New(slog.NewTextHandler(writerFunc(func(p []byte) (int, error) {
		logMu.Lock()
		defer logMu.Unlock()
		return logs.Write(p)
	}), &slog.HandlerOptions{Level: slog.LevelDebug}))

	cfg := filepath.Join(t.TempDir(), "p.yml")
	if err := os.WriteFile(cfg, []byte("path: false\ngroups: [{id: calls, title: Calls}]\ntargets:\n"+
		"  - {name: STUN-test, host: stun.test, port: "+strconv.Itoa(port)+", group: calls, kinds: [udp], interval: 6s, tags: [udp-path, app:meet]}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	prof, err := loadProfile(config{configPath: cfg})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	st, err := openStore(dir, prof, func(string, model.ProbeKind) time.Duration { return 5 * time.Second }, log)
	if err != nil {
		t.Fatal(err)
	}
	mc := metrics.New(func() *model.Profile { return prof })
	mos := metrics.NewCallMOS(func() *model.Profile { return prof })
	mc.Register(mos)
	runner := probe.New(probe.Options{
		Log: log,
		Lookup: func(context.Context, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		},
		LookupCNAME: func(_ context.Context, h string) (string, error) { return h + ".", nil },
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var mu sync.Mutex
	var samples []model.Sample
	sink := model.Fanout{st, mc, mos, model.SinkFunc(func(s model.Sample) {
		mu.Lock()
		defer mu.Unlock()
		samples = append(samples, s)
		if len(samples) >= 6 {
			cancel()
		}
	})}
	if err := runner.Run(ctx, prof, sink); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if len(samples) < 6 {
		t.Fatalf("%d samples", len(samples))
	}
	var outputs []string
	for _, s := range samples {
		if s.Lost {
			t.Errorf("lost: %+v", s)
		}
		outputs = append(outputs, strings.Join([]string{s.Key.Target, s.Key.Kind.String(), s.RTT.String(), s.Err}, " "))
	}
	mu.Unlock()

	deps := web.Deps{
		Profile: func() *model.Profile { return prof },
		Store:   st,
		Status:  func() web.Status { return web.Status{Version: "test", Caps: runner.Caps(), Targets: 1, Ready: 1} },
		Metrics: mc.Handler(),
		UDP:     runner.(probe.UDPStateSource),
		Log:     log,
	}
	get := func(h http.Handler, path string) string {
		req := httptest.NewRequest("GET", path, nil)
		req.Host = "127.0.0.1:3000"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		b, _ := io.ReadAll(w.Result().Body)
		if w.Code != 200 {
			t.Errorf("%s: %d %s", path, w.Code, b)
		}
		return string(b)
	}
	local := web.Local(deps, web.LocalOptions{Addr: "127.0.0.1:3000"})
	m := get(local, "/metrics")
	if !strings.Contains(m, "fyisp_udp_jitter_seconds{") || !strings.Contains(m, "fyisp_udp_loss_ratio{") {
		t.Errorf("/metrics has no UDP series:\n%s", m)
	}
	outputs = append(outputs, m,
		get(local, "/api/status"), get(local, "/api/profile"), get(local, "/api/panel?group=calls&from=now-5m"))
	calls := get(local, "/api/calls?from=now-5m")
	if !strings.Contains(calls, `"app":"meet","udp":"measured"`) {
		t.Errorf("/api/calls does not measure Meet:\n%s", calls)
	}
	outputs = append(outputs, calls)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	logMu.Lock()
	outputs = append(outputs, logs.String())
	logMu.Unlock()
	for i, o := range outputs {
		if strings.Contains(o, mappedIP) || strings.Contains(o, "198.51.100") {
			t.Errorf("output %d leaks the mapped address:\n%s", i, o)
		}
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
