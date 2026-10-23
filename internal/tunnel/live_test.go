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
	tun    *Tunnel
	cancel context.CancelFunc
	origin *httptest.Server
	urls   chan string // OnURL
}

func startLive(t *testing.T, c settings, body string) *liveRun {
	t.Helper()
	return startLiveHandler(t, c, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, body)
	}))
}

func startLiveHandler(t *testing.T, c settings, h http.Handler) *liveRun {
	t.Helper()
	origin := httptest.NewServer(h)
	ctx, cancel := context.WithCancel(context.Background())
	r := &liveRun{cancel: cancel, origin: origin, urls: make(chan string, 4)}
	tun, err := start(ctx, Options{Origin: origin.URL, Log: testLogger(t), OnURL: func(u string) { r.urls <- u }}, c)
	if err != nil {
		t.Fatal(err)
	}
	r.tun = tun
	return r
}

// waitConnected waits for OnURL and returns the connected state.
func (r *liveRun) waitConnected(t *testing.T, timeout time.Duration) state {
	t.Helper()
	start := time.Now()
	select {
	case u := <-r.urls:
		st := r.tun.t.getState()
		if u != st.URL || r.tun.URL() != u {
			t.Errorf("OnURL %q, URL() %q, state %+v", u, r.tun.URL(), st)
		}
		t.Logf("connected after %v: %+v", time.Since(start).Round(time.Millisecond), st)
		return st
	case <-time.After(timeout):
		t.Fatalf("not connected after %v; state %+v", timeout, r.tun.t.getState())
	}
	return state{}
}

func (r *liveRun) stop(t *testing.T) {
	t.Helper()
	start := time.Now()
	r.cancel()
	select {
	case <-r.tun.Done():
	case <-time.After(30 * time.Second):
		t.Fatal("the tunnel did not stop within 30s of cancel")
	}
	r.origin.Close()
	t.Logf("stopped in %v; final state %+v", time.Since(start).Round(time.Millisecond), r.tun.t.getState())
	if st := r.tun.t.getState(); st.Phase != phaseDisabled || r.tun.URL() != "" {
		t.Errorf("after stop: %+v, URL() %q", st, r.tun.URL())
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
	r := startLive(t, settings{Protocol: proto}, "hello")
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

func TestLiveQUIC(t *testing.T) { liveProtocol(t, "quic") }

func TestLiveHTTP2(t *testing.T) { liveProtocol(t, "http2") }

// TestLiveReprovision: two tunnels in one process (second NewSupervisor), no panic, no leaks.
func TestLiveReprovision(t *testing.T) {
	liveOnly(t)
	before := runtime.NumGoroutine()
	baseline := goleak.IgnoreCurrent()

	r1 := startLive(t, settings{}, "hello")
	st1 := r1.waitConnected(t, 90*time.Second)
	fetch(t, st1.URL, "hello", 60*time.Second)
	r1.stop(t)

	r2 := startLive(t, settings{}, "hello again")
	st2 := r2.waitConnected(t, 90*time.Second)
	if st2.URL == st1.URL {
		t.Errorf("second tunnel reused URL %s", st1.URL)
	}
	fetch(t, st2.URL, "hello again", 60*time.Second)
	r2.stop(t)

	if err := goleak.Find(append([]goleak.Option{baseline}, leakOpts...)...); err != nil {
		t.Errorf("goroutines leaked after two tunnels: %v", err)
	}
	t.Logf("goroutines: before=%d after=%d (1 expected extra: shared observer dispatcher)", before, runtime.NumGoroutine())
}

// TestLiveAuto: protocol "auto". Set FYISP_EXPECT_PROTOCOL=http2 when UDP is blocked.
func TestLiveAuto(t *testing.T) {
	liveOnly(t)
	if os.Getenv("FYISP_BLOCK_UDP") != "" {
		blockUDP(t)
	}
	want := os.Getenv("FYISP_EXPECT_PROTOCOL")
	if want == "" {
		want = "quic"
	}
	r := startLive(t, settings{Protocol: "auto", ReprovisionAfter: 10 * time.Minute}, "hello")
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
	r := startLive(t, settings{Protocol: "quic", ReprovisionAfter: 20 * time.Second}, "hello")
	urls := map[string]bool{}
	for deadline := time.Now().Add(3 * time.Minute); len(urls) < 2; time.Sleep(100 * time.Millisecond) {
		st := r.tun.t.getState()
		if st.Phase == phaseConnecting && st.URL != "" && !urls[st.URL] {
			urls[st.URL] = true
			t.Logf("%s connecting via %s", time.Now().Format(time.TimeOnly), st.URL)
		}
		if st.Phase == phaseConnected {
			t.Fatal("connected over QUIC although UDP is blocked")
		}
		if time.Now().After(deadline) {
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

// TestLiveSameOrigin checks what fyisp's dashboard relies on (its host
// allowlist accepts the tunnel's name, and web.sameOrigin guards its
// writes): the quick tunnel passes the visitor's Host (the trycloudflare
// hostname, not the origin's 127.0.0.1:port), Origin and Sec-Fetch-Site
// unchanged, and sets Cf-Connecting-Ip.
func TestLiveSameOrigin(t *testing.T) {
	liveOnly(t)
	r := startLiveHandler(t, settings{}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cf := "no"
		if r.Header.Get("Cf-Connecting-Ip") != "" {
			cf = "yes"
		}
		_, _ = io.WriteString(w, r.Method+" host="+r.Host+" origin="+r.Header.Get("Origin")+" sfs="+r.Header.Get("Sec-Fetch-Site")+" cfip="+cf)
	}))
	st := r.waitConnected(t, 90*time.Second)
	host := strings.TrimPrefix(st.URL, "https://")
	fetch(t, st.URL, "GET host="+host+" origin= sfs= cfip=yes", 60*time.Second)

	res := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, "1.1.1.1:53")
	}}
	c := &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{DisableKeepAlives: true,
		DialContext: (&net.Dialer{Resolver: res, Timeout: 10 * time.Second}).DialContext}}
	req, _ := http.NewRequest("POST", st.URL+"/api/investigate?target=Alpha", nil)
	req.Header.Set("Origin", st.URL)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if want := "POST host=" + host + " origin=" + st.URL + " sfs=same-origin cfip=yes"; string(b) != want {
		t.Errorf("origin saw %q, want %q", b, want)
	}
	t.Logf("origin saw %q", b)
	r.stop(t)
}
