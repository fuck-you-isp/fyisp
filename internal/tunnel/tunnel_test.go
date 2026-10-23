package tunnel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rs/zerolog"
	"go.uber.org/goleak"
)

// The shared connection.Observer's dispatch goroutine lives for the whole
// process once the first Tunnel has run (see setupProcessGlobals).
var leakOpts = []goleak.Option{
	goleak.IgnoreTopFunction("github.com/cloudflare/cloudflared/connection.(*Observer).dispatchEvents"),
}

func TestMain(m *testing.M) { goleak.VerifyTestMain(m, leakOpts...) }

func TestPhaseString(t *testing.T) {
	for p, want := range map[phase]string{
		phaseDisabled: "disabled", phaseProvisioning: "provisioning", phaseConnecting: "connecting",
		phaseConnected: "connected", phaseReconnecting: "reconnecting", phaseFailed: "failed", 42: "phase(42)",
	} {
		if got := p.String(); got != want {
			t.Errorf("phase(%d) = %q, want %q", p, got, want)
		}
	}
}

func TestOptionsValidate(t *testing.T) {
	origin := Options{Origin: "http://127.0.0.1:3000"}
	c := settings{}.withDefaults()
	if err := validate(origin, c); err != nil {
		t.Fatalf("valid options: %v", err)
	}
	if c.Protocol != "auto" || c.QuickService != defaultQuickService || c.ReprovisionAfter != 5*time.Minute || c.FallbackAfter != 30*time.Second {
		t.Fatalf("defaults not applied: %+v", c)
	}
	for _, x := range []struct {
		o Options
		c settings
	}{
		{origin, settings{Protocol: "h2mux"}},
		{Options{Origin: "127.0.0.1:3000"}, settings{}},
		{Options{Origin: "tcp://127.0.0.1:3000"}, settings{}},
		{origin, settings{QuickService: "::"}},
	} {
		if err := validate(x.o, x.c.withDefaults()); err == nil {
			t.Errorf("validate(%+v, %+v) = nil, want error", x.o, x.c)
		}
	}
	if _, err := Start(context.Background(), Options{}); err == nil {
		t.Error("Start without an origin returned nil")
	}
}

// TestURLAnnounced: OnURL hears a URL once its tunnel connects, URL keeps it
// while the tunnel reconnects, and a replacement tunnel's URL is new.
func TestURLAnnounced(t *testing.T) {
	var heard []string
	tn := &tunnel{opts: Options{OnURL: func(u string) { heard = append(heard, u) }}, log: slog.New(slog.DiscardHandler)}
	T := &Tunnel{t: tn}
	steps := []struct {
		s    state
		want string // URL()
	}{
		{state{Phase: phaseProvisioning}, ""},
		{state{Phase: phaseConnecting, URL: "https://a.trycloudflare.com"}, ""},
		{state{Phase: phaseConnected, URL: "https://a.trycloudflare.com", Protocol: "quic"}, "https://a.trycloudflare.com"},
		{state{Phase: phaseReconnecting, URL: "https://a.trycloudflare.com"}, "https://a.trycloudflare.com"},
		{state{Phase: phaseConnected, URL: "https://a.trycloudflare.com", Protocol: "http2"}, "https://a.trycloudflare.com"},
		{state{Phase: phaseProvisioning}, ""},
		{state{Phase: phaseConnecting, URL: "https://b.trycloudflare.com"}, ""},
		{state{Phase: phaseConnected, URL: "https://b.trycloudflare.com", Protocol: "quic"}, "https://b.trycloudflare.com"},
		{state{Phase: phaseDisabled}, ""},
	}
	for i, st := range steps {
		tn.update(func(s *state) { *s = st.s })
		if got := T.URL(); got != st.want {
			t.Errorf("step %d (%s): URL() = %q, want %q", i, st.s.Phase, got, st.want)
		}
	}
	if strings.Join(heard, " ") != "https://a.trycloudflare.com https://b.trycloudflare.com" {
		t.Errorf("OnURL heard %q", heard)
	}
	tn.update(func(s *state) { *s = state{Phase: phaseFailed, LastErr: "boom"} })
	if err := T.Err(); err == nil || err.Error() != "boom" {
		t.Errorf("Err() = %v", err)
	}
	// OnURL runs outside the lock: it may call URL.
	tn.opts.OnURL = func(u string) {
		if T.URL() != u {
			t.Errorf("URL() in OnURL = %q, want %q", T.URL(), u)
		}
	}
	tn.update(func(s *state) { *s = state{Phase: phaseConnected, URL: "https://c.trycloudflare.com"} })
}

// TestErrClearedAfterDone: a run that failed and was then cancelled ends
// with no error.
func TestErrClearedAfterDone(t *testing.T) {
	svc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer svc.Close()
	ctx, cancel := context.WithCancel(context.Background())
	tun, err := start(ctx, Options{Origin: "http://127.0.0.1:1"}, settings{QuickService: svc.URL})
	if err != nil {
		t.Fatal(err)
	}
	waitPhase(t, tun, phaseFailed)
	if tun.Err() == nil {
		t.Error("no error while failing")
	}
	cancel()
	<-tun.Done()
	if err := tun.Err(); err != nil {
		t.Errorf("Err() after Done = %v", err)
	}
}

// TestRedactWriter: what the standard log package writes is masked.
func TestRedactWriter(t *testing.T) {
	var buf bytes.Buffer
	w := redactWriter{&buf}
	if n, err := w.Write([]byte("http2: server: error reading preface from client 203.0.113.9:7844: read tcp 198.51.100.77:5000->203.0.113.9:7844\n")); err != nil || n == 0 {
		t.Fatal(n, err)
	}
	if out := buf.String(); strings.Contains(out, "198.51.100.77") || strings.Contains(out, "203.0.113.9") {
		t.Errorf("standard log: %q", out)
	}
}

func TestParseProvisionResponse(t *testing.T) {
	good := `{"success":true,"result":{"id":"6a8e3b0e-3a64-4a53-8a5a-0d4f2f7f9b11","name":"qt","hostname":"a-b-c.trycloudflare.com","account_tag":"acct","secret":"c2VjcmV0"}}`
	for _, host := range []string{"localhost", "evil.example", "a.trycloudflare.com.evil.example", "a b.trycloudflare.com", "\x1b[2Ja.trycloudflare.com"} {
		bad := strings.Replace(good, "a-b-c.trycloudflare.com", host, 1)
		if _, err := parseProvisionResponse(200, nil, []byte(bad)); err == nil {
			t.Errorf("hostname %q accepted", host)
		}
	}
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
	for _, ra := range []string{"99999999999999999", "86400"} {
		_, err = parseProvisionResponse(429, http.Header{"Retry-After": {ra}}, nil)
		if !errors.As(err, &rl) || rl.retryAfter != time.Hour {
			t.Errorf("Retry-After %s: got %v, want capped at 1h", ra, err)
		}
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
	for range 200 { // never before Retry-After, jitter or not
		if d := provisionBackoff(1, &rateLimitedError{retryAfter: 2 * time.Minute}); d < 2*time.Minute || d > 144*time.Second {
			t.Fatalf("backoff after Retry-After 2m: %v", d)
		}
	}
}

// TestServeQuickTunnel, with the supervisor faked (it checks what
// runSupervisor's ticker checks): an "auto" tunnel that never connects
// falls back to HTTP/2 after FallbackAfter and keeps its URL; one that
// connected over QUIC and then lost the network never falls back, and is
// replaced after ReprovisionAfter.
func TestServeQuickTunnel(t *testing.T) {
	newTunnel := func(heard *[]string) *tunnel {
		return &tunnel{opts: Options{OnURL: func(u string) { *heard = append(*heard, u) }}, cfg: settings{}.withDefaults(),
			log: slog.New(slog.DiscardHandler)}
	}
	qt := &quickTunnel{Hostname: "a.trycloudflare.com"}
	synctest.Test(t, func(t *testing.T) {
		var heard, protos []string
		tn := newTunnel(&heard)
		start := time.Now()
		var fellBack time.Duration
		tn.runSup = func(ctx context.Context, _ *quickTunnel, w *watchdog, proto string) (stopReason, error) {
			protos = append(protos, proto)
			if proto == "http2" {
				fellBack = time.Since(start)
				w.connected()
				tn.update(func(s *state) { s.Phase, s.Protocol = phaseConnected, proto })
				<-ctx.Done()
				return stopCtx, nil
			}
			for { // QUIC blocked
				time.Sleep(time.Second)
				if fallbackDue(proto, tn.cfg.FallbackAfter, w.everUp(), w.active(), w.downFor()) {
					return stopFallback, nil
				}
			}
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { tn.serveQuickTunnel(ctx, qt); close(done) }()
		time.Sleep(10 * time.Minute)
		cancel()
		<-done
		if strings.Join(protos, " ") != "auto http2" || fellBack < 30*time.Second || fellBack > 31*time.Second {
			t.Errorf("protocols %v, fell back after %v", protos, fellBack)
		}
		if strings.Join(heard, " ") != "https://a.trycloudflare.com" {
			t.Errorf("OnURL heard %q", heard)
		}
	})
	synctest.Test(t, func(t *testing.T) {
		var heard, protos []string
		tn := newTunnel(&heard)
		var lost time.Time
		tn.runSup = func(ctx context.Context, _ *quickTunnel, w *watchdog, proto string) (stopReason, error) {
			protos = append(protos, proto)
			if len(protos) == 1 {
				w.connected()
				tn.update(func(s *state) { s.Phase, s.Protocol = phaseConnected, "quic" })
				time.Sleep(time.Hour)
				w.disconnected() // the network goes away for good
				lost = time.Now()
			}
			for {
				time.Sleep(time.Second)
				switch {
				case w.expired():
					return stopWatchdog, nil
				case fallbackDue(proto, tn.cfg.FallbackAfter, w.everUp(), w.active(), w.downFor()):
					return stopFallback, nil
				}
			}
		}
		tn.serveQuickTunnel(context.Background(), qt)
		if d := time.Since(lost); strings.Join(protos, " ") != "auto" || d < 5*time.Minute || d > 5*time.Minute+2*time.Second {
			t.Errorf("protocols %v, replaced %v after the loss", protos, d)
		}
	})
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

	ctx, cancel := context.WithCancel(context.Background())
	tun, err := start(ctx, Options{Origin: "http://127.0.0.1:1"}, settings{QuickService: svc.URL})
	if err != nil {
		t.Fatal(err)
	}
	waitPhase(t, tun, phaseFailed)
	if !strings.Contains(fmt.Sprint(tun.Err()), "maintenance") {
		t.Errorf("Err() = %v", tun.Err())
	}

	// A second tunnel cannot run concurrently.
	if _, err := Start(context.Background(), Options{Origin: "http://127.0.0.1:1"}); !errors.Is(err, ErrAlreadyRunning) {
		t.Errorf("concurrent Start = %v, want ErrAlreadyRunning", err)
	}

	cancel()
	select {
	case <-tun.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the tunnel did not stop after cancel")
	}
	if st := tun.t.getState(); st.Phase != phaseDisabled || st.URL != "" || tun.URL() != "" {
		t.Errorf("final state %+v", st)
	}
	if calls.Load() != 1 {
		t.Errorf("service called %d times, want 1 (backoff)", calls.Load())
	}
}

// waitPhase waits (10 s at most) for the tunnel to reach phase p.
func waitPhase(t *testing.T, tun *Tunnel, p phase) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); tun.t.getState().Phase != p; {
		if time.Now().After(deadline) {
			t.Fatalf("no %s; state %+v", p, tun.t.getState())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestWatchdog(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
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
	})
}

func TestZerologBridge(t *testing.T) {
	warnLimiter = &logLimiter{every: time.Minute, now: time.Now}
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
// TestCloudflaredVersionMatchesGoMod: cloudflaredCommit is the commit go.mod
// links, and CloudflaredVersion the release that commit is: the first line
// of its RELEASE_NOTES, read offline from the module cache.
func TestCloudflaredVersionMatchesGoMod(t *testing.T) {
	if out, err := goList(t, "{{.Dir}}"); out != "" {
		b, err := os.ReadFile(filepath.Join(strings.TrimSpace(string(out)), "RELEASE_NOTES"))
		if err != nil {
			t.Fatal(err)
		}
		if first, _, _ := strings.Cut(string(b), "\n"); strings.TrimSpace(first) != CloudflaredVersion {
			t.Errorf("CloudflaredVersion %q, but the linked cloudflared is release %q", CloudflaredVersion, strings.TrimSpace(first))
		}
	} else {
		t.Logf("RELEASE_NOTES not checked: %v", err)
	}
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
	t.Fatal("cloudflared is not in the test binary's build info")
}

// goList runs go list -m -f format on cloudflared, offline (the module
// cache). Without a go command (the livetest image) it returns "" and the
// reason; any other failure fails the test.
func goList(t *testing.T, format string) (string, error) {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		return "", err
	}
	out, err := exec.Command("go", "list", "-m", "-f", format, "github.com/cloudflare/cloudflared").Output()
	if err != nil {
		t.Fatalf("go list -m %s: %v", format, err)
	}
	return strings.TrimSpace(string(out)), nil
}

func TestFallbackDue(t *testing.T) {
	const after = 30 * time.Second
	for _, c := range []struct {
		proto   string
		after   time.Duration
		ever    bool
		active  int
		downFor time.Duration
		want    bool
	}{
		{"auto", after, false, 0, 31 * time.Second, true},
		{"auto", after, false, 0, 30 * time.Second, true},
		{"auto", after, false, 0, 29 * time.Second, false}, // not yet
		{"auto", after, false, 1, time.Hour, false},        // connected
		{"auto", after, true, 0, time.Hour, false},         // QUIC worked: an outage, not a block
		{"quic", after, false, 0, time.Hour, false},        // explicit modes never switch
		{"http2", after, false, 0, time.Hour, false},
		{"auto", -1, false, 0, time.Hour, false}, // disabled
	} {
		if got := fallbackDue(c.proto, c.after, c.ever, c.active, c.downFor); got != c.want {
			t.Errorf("fallbackDue(%q, %v, %v, %d, %v) = %v, want %v", c.proto, c.after, c.ever, c.active, c.downFor, got, c.want)
		}
	}
	if o := (settings{FallbackAfter: -1}).withDefaults(); o.FallbackAfter != -1 {
		t.Errorf("negative FallbackAfter overridden: %v", o.FallbackAfter)
	}

	w := &watchdog{after: time.Minute, downSince: time.Now().Add(-40 * time.Second)}
	if !fallbackDue("auto", after, w.everUp(), w.active(), w.downFor()) {
		t.Error("no fallback 40s after start without connection")
	}
	w.connected()
	if w.downFor() != 0 || fallbackDue("auto", after, w.everUp(), w.active(), w.downFor()) {
		t.Error("fallback while connected")
	}
	w.disconnected() // the clock restarts at the disconnect
	if fallbackDue("auto", after, w.everUp(), w.active(), w.downFor()) {
		t.Error("fallback right after a disconnect")
	}
	w.downSince = time.Now().Add(-time.Hour)
	if fallbackDue("auto", after, w.everUp(), w.active(), w.downFor()) {
		t.Error("fallback after an outage of a tunnel that once connected")
	}
}

func TestLogLimiter(t *testing.T) {
	now := time.Unix(1000, 0)
	l := &logLimiter{every: time.Minute, now: func() time.Time { return now }}
	if ok, n := l.allow("a"); !ok || n != 0 {
		t.Fatalf("first a: %v %d", ok, n)
	}
	for i := 0; i < 5; i++ {
		now = now.Add(5 * time.Second)
		if ok, _ := l.allow("a"); ok {
			t.Fatalf("repeat %d of a allowed within a minute", i)
		}
	}
	if ok, _ := l.allow("b"); !ok {
		t.Fatal("distinct message b suppressed")
	}
	now = now.Add(time.Minute)
	if ok, n := l.allow("a"); !ok || n != 5 {
		t.Fatalf("a after a minute: %v, suppressed %d (want 5)", ok, n)
	}
	// The map stays bounded.
	for i := 0; i < 3*maxLimiterEntries; i++ {
		now = now.Add(time.Second)
		l.allow(strings.Repeat("x", i))
	}
	if len(l.seen) > maxLimiterEntries {
		t.Errorf("limiter tracks %d messages", len(l.seen))
	}
}

func TestZerologBridgeRateLimitsWarnings(t *testing.T) {
	now := time.Unix(1000, 0)
	warnLimiter = &logLimiter{every: time.Minute, now: func() time.Time { return now }}
	defer func() { warnLimiter = &logLimiter{every: time.Minute, now: time.Now} }()
	var buf bytes.Buffer
	l := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	zl := newZerolog(func() *slogLogger { return l }, nil)
	for i := 0; i < 10; i++ {
		zl.Error().Int("attempt", i).Msg("Failed to dial a quic connection")
		zl.Info().Msg("info is not rate-limited")
		now = now.Add(time.Second)
	}
	now = now.Add(time.Minute)
	zl.Warn().Msg("Failed to dial a quic connection")
	out := buf.String()
	if n := strings.Count(out, "Failed to dial"); n != 2 {
		t.Errorf("warning logged %d times, want 2:\n%s", n, out)
	}
	if n := strings.Count(out, "info is not rate-limited"); n != 10 {
		t.Errorf("info logged %d times, want 10", n)
	}
	if !strings.Contains(out, `"suppressed":9`) {
		t.Errorf("suppressed count missing:\n%s", out)
	}
}

// TestZerologBridgePrivacy: whatever slog enables, cloudflared never logs
// below Info (its per-request debug lines carry visitors' headers), header
// fields are dropped, and IP addresses are masked.
func TestZerologBridgePrivacy(t *testing.T) {
	warnLimiter = &logLimiter{every: time.Minute, now: time.Now}
	const visitor = "198.51.100.77"
	var buf bytes.Buffer
	l := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.Level(-100)}))
	zl := newZerolog(func() *slogLogger { return l }, nil)
	zl.Debug().Str("headers", "map[Cf-Connecting-Ip:["+visitor+"]]").Msg("GET https://a.trycloudflare.com/ HTTP/1.1")
	zl.Info().Str("Cf-Connecting-Ip", visitor).Str("cookie", "fyisp_admin=secret").Str("ip", "198.41.200.13").
		Str("dest", "["+"2001:db8::1]:443").Msg("Request from " + visitor + ":51234 version 2026.10.3 at 10:00:01")
	out := buf.String()
	for _, leak := range []string{visitor, "secret", "198.41.200.13", "2001:db8::1", "GET https://"} {
		if strings.Contains(out, leak) {
			t.Errorf("%q in the log: %s", leak, out)
		}
	}
	if !strings.Contains(out, "Request from [ip]:51234 version 2026.10.3 at 10:00:01") || !strings.Contains(out, `"ip":"[ip]"`) {
		t.Errorf("redacted line: %s", out)
	}
	buf.Reset()
	zl.Warn().Interface("req", map[string]any{"headers": map[string]any{"Cookie": "fyisp_admin=secret"}}).Interface("n", map[string]any{"a": visitor}).Msg("nested")
	if out := buf.String(); strings.Contains(out, "secret") || strings.Contains(out, visitor) || !strings.Contains(out, "map[a:[ip]]") {
		t.Errorf("nested fields: %s", out)
	}
	buf.Reset()
	zl.Warn().Str(visitor, "x").Int64("n", 1).Msg("address as a key")
	if out := buf.String(); strings.Contains(out, visitor) || !strings.Contains(out, `"n":"1"`) {
		t.Errorf("address key: %s", out)
	}
	for in, want := range map[string]string{
		"1.2.3.4": "[ip]", "1.2.3.4:443": "[ip]:443", "fe80::1": "[ip]", "v2026.10.3": "v2026.10.3", "12:30:45": "12:30:45", "a.b.c": "a.b.c",
		"ip:198.51.100.77": "ip:[ip]", "id:198.51.100.77": "id:[ip]", "X-Forwarded-For:198.51.100.77": "X-Forwarded-For:[ip]",
		"map[a:1.2.3.4]": "map[a:[ip]]", "abc1.2.3.4": "abc[ip]", "cf_connecting_ip:203.0.113.9,203.0.113.10": "cf_connecting_ip:[ip],[ip]",
		"[2001:db8::1]:443": "[[ip]]:443", "read tcp 203.0.113.9:40000->104.16.1.1:443": "read tcp [ip]:40000->[ip]:443",
		"2a01:4f8:1:2::": "[ip]", "010.001.002.003": "[ip]", "203.0.113.009": "[ip]", "to 2a01:4f8:1:2:: now": "to [ip] now",
		"fe:2001:db8:85a3:0:0:8a2e:370:7334": "[ip]", "a010.001.002.003": "a[ip]", "x1.2.3.4 y": "x[ip] y",
	} {
		if got := redact(in); got != want {
			t.Errorf("redact(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestReplaceDirectivesMatchCloudflared: fyisp's go.mod repeats the pinned
// cloudflared's replace directives (Go ignores them in dependencies), read
// offline from the module cache. Its no-op golang_client one is left out.
func TestReplaceDirectivesMatchCloudflared(t *testing.T) {
	gomod, err := goList(t, "{{.GoMod}}")
	if gomod == "" {
		t.Skipf("no go command: %v", err)
	}
	theirs, err := os.ReadFile(gomod)
	if err != nil {
		t.Fatal(err)
	}
	ours, err := os.ReadFile("../../go.mod")
	if err != nil {
		t.Fatal(err)
	}
	want, got := replaces(string(theirs)), replaces(string(ours))
	want = slices.DeleteFunc(want, func(r string) bool { return strings.Contains(r, "golang_client") })
	if !slices.Equal(want, got) {
		t.Errorf("go.mod replace directives\n got %q\nwant %q (cloudflared's): copy them when bumping cloudflared", got, want)
	}
}

// replaces lists a go.mod's replace directives ("old => new version"),
// sorted, comments and spacing removed.
func replaces(mod string) []string {
	var out []string
	block := false
	for _, l := range strings.Split(mod, "\n") {
		l, _, _ = strings.Cut(l, "//")
		l = strings.Join(strings.Fields(l), " ")
		switch {
		case l == "replace (":
			block = true
		case block && l == ")":
			block = false
		case block && l != "":
			out = append(out, l)
		case strings.HasPrefix(l, "replace "):
			out = append(out, strings.TrimPrefix(l, "replace "))
		}
	}
	slices.Sort(out)
	return out
}

// TestNoPrivateRouting: the orchestrator fyisp builds refuses every TCP and
// UDP flow the edge could ask for, to a listening port too, and keeps doing
// so after the edge pushes a configuration, which it never applies: fyisp's
// own is the newest version.
func TestNoPrivateRouting(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	tcp := netip.MustParseAddrPort(ln.Addr().String())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	zl := zerolog.Nop()
	orch, dialer, err := newOrchestrator(ctx, "http://127.0.0.1:3000", "auto", nil, &zl)
	if err != nil {
		t.Fatal(err)
	}
	refused := func(when string) {
		t.Helper()
		if c, err := dialer.DialTCP(ctx, tcp); !errors.Is(err, errNoPrivateRouting) {
			if c != nil {
				c.Close()
			}
			t.Errorf("%s: TCP to %s: %v, want refused", when, tcp, err)
		}
		if c, err := dialer.DialUDP(netip.MustParseAddrPort("127.0.0.1:53")); !errors.Is(err, errNoPrivateRouting) {
			if c != nil {
				c.Close()
			}
			t.Errorf("%s: UDP: %v, want refused", when, err)
		}
	}
	refused("built")
	push := `{"ingress":[{"service":"http://192.168.1.1:80"}],"warp-routing":{}}`
	if r := orch.UpdateConfig(7, []byte(push)); r.LastAppliedVersion != pinnedVersion || r.Err != nil {
		t.Errorf("a pushed configuration: applied version %d, %v", r.LastAppliedVersion, r.Err)
	}
	refused("after a push")
	b, err := orch.GetConfigJSON()
	if err != nil || strings.Contains(string(b), "192.168.1.1") || !strings.Contains(string(b), "127.0.0.1:3000") {
		t.Errorf("ingress after a push: %s %v", b, err)
	}
}
