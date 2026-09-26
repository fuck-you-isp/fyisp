package tunnel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"
)

// The shared connection.Observer's dispatch goroutine lives for the whole
// process once the first Tunnel has run (see setupProcessGlobals).
var leakOpts = []goleak.Option{
	goleak.IgnoreTopFunction("github.com/cloudflare/cloudflared/connection.(*Observer).dispatchEvents"),
}

func TestMain(m *testing.M) { goleak.VerifyTestMain(m, leakOpts...) }

func TestPhaseString(t *testing.T) {
	for p, want := range map[Phase]string{
		Disabled: "disabled", Provisioning: "provisioning", Connecting: "connecting",
		Connected: "connected", Reconnecting: "reconnecting", Failed: "failed", 42: "phase(42)",
	} {
		if got := p.String(); got != want {
			t.Errorf("Phase(%d) = %q, want %q", p, got, want)
		}
	}
}

func TestOptionsValidate(t *testing.T) {
	ok := Options{OriginURL: "http://127.0.0.1:3000"}.withDefaults()
	if err := ok.validate(); err != nil {
		t.Fatalf("valid options: %v", err)
	}
	if ok.Protocol != "auto" || ok.QuickService != defaultQuickService || ok.ReprovisionAfter != 5*time.Minute {
		t.Fatalf("defaults not applied: %+v", ok)
	}
	for _, o := range []Options{
		{OriginURL: "http://127.0.0.1:3000", Protocol: "h2mux"},
		{OriginURL: "127.0.0.1:3000"},
		{OriginURL: "tcp://127.0.0.1:3000"},
		{OriginURL: "http://127.0.0.1:3000", QuickService: "::"},
	} {
		if err := o.withDefaults().validate(); err == nil {
			t.Errorf("validate(%+v) = nil, want error", o)
		}
	}
	if err := New(Options{Protocol: "bogus"}).Run(context.Background()); err == nil {
		t.Error("Run with invalid options returned nil")
	}
}

func TestParseProvisionResponse(t *testing.T) {
	good := `{"success":true,"result":{"id":"6a8e3b0e-3a64-4a53-8a5a-0d4f2f7f9b11","name":"qt","hostname":"a-b-c.trycloudflare.com","account_tag":"acct","secret":"c2VjcmV0"}}`
	qt, err := parseProvisionResponse(200, nil, []byte(good))
	if err != nil {
		t.Fatal(err)
	}
	if qt.Hostname != "a-b-c.trycloudflare.com" || qt.AccountTag != "acct" || string(qt.Secret) != "secret" || qt.tunnelID.String() != "6a8e3b0e-3a64-4a53-8a5a-0d4f2f7f9b11" {
		t.Fatalf("bad parse: %+v", qt)
	}

	h := http.Header{"Retry-After": {"120"}}
	_, err = parseProvisionResponse(429, h, []byte("slow down"))
	var rl *rateLimitedError
	if !errors.As(err, &rl) || rl.retryAfter != 2*time.Minute {
		t.Fatalf("429: got %v", err)
	}
	if d := provisionBackoff(1, err); d < 90*time.Second {
		t.Errorf("backoff after 429 with Retry-After 120 = %v", d)
	}

	_, err = parseProvisionResponse(500, nil, []byte(`{"success":false,"errors":[{"code":1003,"message":"nope"}]}`))
	if err == nil || !strings.Contains(err.Error(), "[1003] nope") {
		t.Errorf("errors array: got %v", err)
	}
	for _, c := range []struct {
		status int
		body   string
	}{
		{502, "<html>bad gateway</html>"},
		{200, "not json"},
		{200, `{"success":false}`},
		{200, `{"success":true,"result":{"id":"not-a-uuid"}}`},
		{200, `{"success":true,"result":{"id":"6a8e3b0e-3a64-4a53-8a5a-0d4f2f7f9b11"}}`},
	} {
		if _, err := parseProvisionResponse(c.status, nil, []byte(c.body)); err == nil {
			t.Errorf("parse(%d, %q) = nil error", c.status, c.body)
		}
	}
}

func TestProvisionBackoffBounds(t *testing.T) {
	if d := provisionBackoff(1, errors.New("x")); d < 4*time.Second || d > 6*time.Second {
		t.Errorf("first backoff %v", d)
	}
	if d := provisionBackoff(50, errors.New("x")); d > 6*time.Minute {
		t.Errorf("capped backoff %v", d)
	}
}

// Run against a fake quick-tunnel service that always fails: exercises the
// provisioning loop, state events, cancellation and leak-freedom offline.
func TestRunProvisionFailureAndCancel(t *testing.T) {
	var calls atomic.Int32
	svc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/tunnel" || !strings.HasPrefix(r.UserAgent(), "cloudflared/"+CloudflaredVersion) {
			t.Errorf("unexpected request %s %s UA=%q", r.Method, r.URL.Path, r.UserAgent())
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"success":false,"errors":[{"code":1,"message":"maintenance"}]}`))
	}))
	defer svc.Close()

	tun := New(Options{OriginURL: "http://127.0.0.1:1", QuickService: svc.URL})
	events, unsub := tun.Subscribe()
	defer unsub()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- tun.Run(ctx) }()

	var seen []Phase
	deadline := time.After(10 * time.Second)
wait:
	for {
		select {
		case ev := <-events:
			seen = append(seen, ev.State.Phase)
			if ev.State.Phase == Failed {
				if !strings.Contains(ev.State.LastErr, "maintenance") {
					t.Errorf("LastErr = %q", ev.State.LastErr)
				}
				break wait
			}
		case <-deadline:
			t.Fatalf("no Failed event; saw %v", seen)
		}
	}
	if seen[0] != Provisioning {
		t.Errorf("first phase %v, want provisioning", seen[0])
	}

	// A second tunnel cannot run concurrently.
	if err := New(Options{OriginURL: "http://127.0.0.1:1"}).Run(context.Background()); !errors.Is(err, ErrAlreadyRunning) {
		t.Errorf("concurrent Run = %v, want ErrAlreadyRunning", err)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	if st := tun.State(); st.Phase != Disabled || st.URL != "" {
		t.Errorf("final state %+v", st)
	}
	if calls.Load() != 1 {
		t.Errorf("service called %d times, want 1 (backoff)", calls.Load())
	}
}

func TestSubscribeKeepsNewest(t *testing.T) {
	tun := New(Options{}).(*tunnel)
	ch, unsub := tun.Subscribe()
	for i := 0; i < 40; i++ {
		tun.update(func(s *State) { s.LastErr = strings.Repeat("x", i+1) })
	}
	var last Event
	for len(ch) > 0 {
		last = <-ch
	}
	if len(last.State.LastErr) != 40 {
		t.Errorf("newest event lost: %q", last.State.LastErr)
	}
	unsub()
	unsub() // idempotent
	if _, ok := <-ch; ok {
		t.Error("channel not closed after unsubscribe")
	}
	tun.update(func(s *State) { s.LastErr = "after" }) // must not panic
}

func TestWatchdog(t *testing.T) {
	w := &watchdog{after: 50 * time.Millisecond, downSince: time.Now()}
	w.connected()
	time.Sleep(60 * time.Millisecond)
	if w.expired() {
		t.Fatal("expired while connected")
	}
	w.disconnected()
	w.disconnected() // extra disconnects are ignored
	if w.expired() || w.active() != 0 {
		t.Fatal("expired right after disconnect")
	}
	time.Sleep(60 * time.Millisecond)
	if !w.expired() {
		t.Fatal("not expired after ReprovisionAfter without connection")
	}
}

func TestZerologBridge(t *testing.T) {
	var buf bytes.Buffer
	l := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	zl := newZerolog(func() *slogLogger { return l }, nil)
	zl.Info().Str("connIndex", "0").Msg("Registered tunnel connection") // -> Debug: hidden
	zl.Debug().Msg("noise")
	zl.Error().Int("attempt", 3).Msg("Failed to dial a quic connection")
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("want 1 line at Info, got %q", buf.String())
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &m); err != nil {
		t.Fatal(err)
	}
	if m["level"] != "WARN" || m["msg"] != "Failed to dial a quic connection" || m["component"] != "cloudflared" || m["attempt"] != "3" || m["cf_level"] != "error" {
		t.Errorf("bridged record %v", m)
	}

	buf.Reset()
	l = slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	var quiet atomic.Bool
	zl = newZerolog(func() *slogLogger { return l }, &quiet)
	zl.Info().Msg("info shows at debug")
	zl.Debug().Msg("debug still hidden")
	quiet.Store(true)
	zl.Error().Msg("error while stopping is hidden")
	if got := strings.Count(buf.String(), "\n"); got != 1 {
		t.Errorf("at Debug want 1 line, got %q", buf.String())
	}
	nilLog := newZerolog(func() *slogLogger { return nil }, nil)
	nilLog.Error().Msg("dropped") // must not panic
}

// CloudflaredVersion is reported to the edge; it must name the release
// go.mod actually links.
func TestCloudflaredVersionMatchesGoMod(t *testing.T) {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		t.Skip("no build info")
	}
	for _, d := range bi.Deps {
		if d.Path == "github.com/cloudflare/cloudflared" {
			if !strings.HasSuffix(d.Version, "-"+cloudflaredCommit) {
				t.Fatalf("go.mod links cloudflared %s but CloudflaredVersion %s is commit %s; update both", d.Version, CloudflaredVersion, cloudflaredCommit)
			}
			return
		}
	}
	t.Skip("cloudflared not in build info deps")
}
