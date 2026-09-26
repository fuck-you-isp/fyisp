package tunnel

// Live tests against the real trycloudflare.com service. Skipped unless
// FYISP_LIVE_TUNNEL=1. Each test provisions one or two quick tunnels; keep the
// count low, the service rate-limits (HTTP 429). Run them with:
//
//	docker build --target livetest -t fyisp-livetest .
//	docker run --rm -e FYISP_LIVE_TUNNEL=1 fyisp-livetest -test.run Live -test.v
//
// To see what Protocol "auto" does without UDP, block QUIC egress first:
//
//	docker run --rm --cap-add NET_ADMIN -e FYISP_LIVE_TUNNEL=1 -e FYISP_BLOCK_UDP=1 \
//	  -e FYISP_EXPECT_PROTOCOL=http2 fyisp-livetest -test.run LiveAuto -test.v

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"
)

func liveOnly(t *testing.T) {
	t.Helper()
	if os.Getenv("FYISP_LIVE_TUNNEL") != "1" {
		t.Skip("set FYISP_LIVE_TUNNEL=1 to run against trycloudflare.com")
	}
}

func testLogger(t *testing.T) *slog.Logger {
	lvl := slog.LevelInfo
	if os.Getenv("FYISP_TUNNEL_DEBUG") == "1" {
		lvl = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(tWriter{t}, &slog.HandlerOptions{Level: lvl}))
}

type tWriter struct{ t *testing.T }

func (w tWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimSpace(string(p)))
	return len(p), nil
}

type liveRun struct {
	tun    Tunnel
	cancel context.CancelFunc
	done   chan error
	origin *httptest.Server
}

func startLive(t *testing.T, o Options, body string) *liveRun {
	t.Helper()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, body)
	}))
	o.OriginURL = origin.URL
	o.Log = testLogger(t)
	tun := New(o)
	ctx, cancel := context.WithCancel(context.Background())
	r := &liveRun{tun: tun, cancel: cancel, done: make(chan error, 1), origin: origin}
	go func() { r.done <- tun.Run(ctx) }()
	return r
}

// waitConnected waits for Phase Connected with a URL and returns that state.
func (r *liveRun) waitConnected(t *testing.T, timeout time.Duration) State {
	t.Helper()
	ch, unsub := r.tun.Subscribe()
	defer unsub()
	start := time.Now()
	deadline := time.After(timeout)
	for {
		if st := r.tun.State(); st.Phase == Connected && st.URL != "" {
			t.Logf("connected after %v: %+v", time.Since(start).Round(time.Millisecond), st)
			return st
		}
		select {
		case <-ch:
		case <-deadline:
			t.Fatalf("not connected after %v; state %+v", timeout, r.tun.State())
		}
	}
}

func (r *liveRun) stop(t *testing.T) {
	t.Helper()
	start := time.Now()
	r.cancel()
	select {
	case err := <-r.done:
		if err != nil {
			t.Errorf("Run returned %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not return within 30s of cancel")
	}
	r.origin.Close()
	t.Logf("stopped in %v; final state %+v", time.Since(start).Round(time.Millisecond), r.tun.State())
	if st := r.tun.State(); st.Phase != Disabled {
		t.Errorf("phase after stop = %v", st.Phase)
	}
}

// fetch GETs url until it returns want (new hostnames take a moment to
// resolve). It resolves via 1.1.1.1 to dodge negative DNS caching.
func fetch(t *testing.T, url, want string, timeout time.Duration) {
	t.Helper()
	res := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, "1.1.1.1:53")
	}}
	tr := &http.Transport{DisableKeepAlives: true, DialContext: (&net.Dialer{Resolver: res, Timeout: 10 * time.Second}).DialContext}
	defer tr.CloseIdleConnections()
	c := &http.Client{Transport: tr, Timeout: 15 * time.Second}
	start := time.Now()
	var last string
	for attempt := 1; time.Since(start) < timeout; attempt++ {
		resp, err := c.Get(url + "/")
		if err == nil {
			b, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == 200 && string(b) == want {
				t.Logf("GET %s -> 200 %q after %d attempt(s), %v", url, b, attempt, time.Since(start).Round(time.Millisecond))
				return
			}
			last = resp.Status + " " + strings.TrimSpace(string(b[:min(len(b), 120)]))
		} else {
			last = err.Error()
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("GET %s never returned %q within %v; last: %s", url, want, timeout, last)
}

func liveProtocol(t *testing.T, proto string) {
	liveOnly(t)
	r := startLive(t, Options{Protocol: proto}, "hello")
	st := r.waitConnected(t, 90*time.Second)
	if st.Protocol != proto {
		t.Errorf("protocol = %q, want %q", st.Protocol, proto)
	}
	if !strings.HasSuffix(st.URL, ".trycloudflare.com") {
		t.Errorf("URL = %q", st.URL)
	}
	fetch(t, st.URL, "hello", 60*time.Second)
	r.stop(t)
}

// G2
func TestLiveQUIC(t *testing.T) { liveProtocol(t, "quic") }

// G3
func TestLiveHTTP2(t *testing.T) { liveProtocol(t, "http2") }

// G4: two tunnels in one process (second NewSupervisor), no panic, no leaks.
func TestLiveReprovision(t *testing.T) {
	liveOnly(t)
	before := runtime.NumGoroutine()
	baseline := goleak.IgnoreCurrent()

	r1 := startLive(t, Options{}, "hello")
	st1 := r1.waitConnected(t, 90*time.Second)
	fetch(t, st1.URL, "hello", 60*time.Second)
	r1.stop(t)

	r2 := startLive(t, Options{}, "hello again")
	st2 := r2.waitConnected(t, 90*time.Second)
	if st2.URL == st1.URL {
		t.Errorf("second tunnel reused URL %s", st1.URL)
	}
	fetch(t, st2.URL, "hello again", 60*time.Second)
	r2.stop(t)

	if err := goleak.Find(append([]goleak.Option{baseline}, leakOpts...)...); err != nil {
		t.Errorf("goroutines leaked after two tunnels: %v", err)
	}
	time.Sleep(500 * time.Millisecond)
	t.Logf("goroutines: before=%d after=%d (1 expected extra: shared observer dispatcher)", before, runtime.NumGoroutine())
}

// G5: Protocol "auto". Set FYISP_EXPECT_PROTOCOL=http2 when UDP is blocked.
func TestLiveAuto(t *testing.T) {
	liveOnly(t)
	if os.Getenv("FYISP_BLOCK_UDP") != "" {
		blockUDP(t)
	}
	want := os.Getenv("FYISP_EXPECT_PROTOCOL")
	if want == "" {
		want = "quic"
	}
	r := startLive(t, Options{Protocol: "auto", ReprovisionAfter: 10 * time.Minute}, "hello")
	start := time.Now()
	st := r.waitConnected(t, 8*time.Minute)
	took := time.Since(start)
	t.Logf("auto connected with %s after %v", st.Protocol, took.Round(time.Second))
	if st.Protocol != want {
		t.Errorf("auto chose %q, want %q", st.Protocol, want)
	}
	// Fast fallback: HTTP/2 about FallbackAfter (30s) after provisioning.
	if want == "http2" && took > 75*time.Second {
		t.Errorf("fallback to http2 took %v, want about 30-45s", took.Round(time.Second))
	}
	fetch(t, st.URL, "hello", 60*time.Second)
	r.stop(t)
}

// Watchdog: with UDP blocked and Protocol "quic" nothing can connect, so after
// ReprovisionAfter the tunnel must re-provision (new URL).
func TestLiveWatchdog(t *testing.T) {
	liveOnly(t)
	if os.Getenv("FYISP_BLOCK_UDP") == "" {
		t.Skip("needs FYISP_BLOCK_UDP=1|input (and --cap-add NET_ADMIN)")
	}
	blockUDP(t)
	r := startLive(t, Options{Protocol: "quic", ReprovisionAfter: 20 * time.Second}, "hello")
	ch, unsub := r.tun.Subscribe()
	defer unsub()
	urls := map[string]bool{}
	deadline := time.After(3 * time.Minute)
	for len(urls) < 2 {
		select {
		case ev := <-ch:
			if ev.State.Phase == Connecting && ev.State.URL != "" && !urls[ev.State.URL] {
				urls[ev.State.URL] = true
				t.Logf("%s connecting via %s", ev.Time.Format(time.TimeOnly), ev.State.URL)
			}
			if ev.State.Phase == Connected {
				t.Fatal("connected over QUIC although UDP is blocked")
			}
		case <-deadline:
			t.Fatalf("no re-provision within 3m; urls %v", urls)
		}
	}
	r.stop(t)
}

// blockUDP blocks QUIC to Cloudflare's tunnel port 7844 inside the test
// container (needs root, iptables and --cap-add NET_ADMIN).
// FYISP_BLOCK_UDP=1 drops egress packets: the kernel fails sendmsg with EPERM
// at once. FYISP_BLOCK_UDP=input drops the replies instead, which looks like a
// firewall silently eating UDP upstream (QUIC handshake timeouts).
func blockUDP(t *testing.T) {
	t.Helper()
	rule := []string{"OUTPUT", "-p", "udp", "--dport", "7844", "-j", "DROP"}
	if os.Getenv("FYISP_BLOCK_UDP") == "input" {
		rule = []string{"INPUT", "-p", "udp", "--sport", "7844", "-j", "DROP"}
	}
	for _, bin := range []string{"iptables", "ip6tables"} {
		if exec.Command(bin, append([]string{"-C"}, rule...)...).Run() == nil {
			continue
		}
		if out, err := exec.Command(bin, append([]string{"-A"}, rule...)...).CombinedOutput(); err != nil {
			t.Fatalf("%s: %v: %s", bin, err, out)
		}
	}
	t.Logf("UDP port 7844 blocked: %v", rule)
}
