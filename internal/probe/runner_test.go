package probe

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

// collect runs a runner over p until done returns true for the samples seen
// so far (or the deadline passes) and returns every sample.
func collect(t *testing.T, r Runner, p *model.Profile, deadline time.Duration, done func([]model.Sample) bool) []model.Sample {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	var (
		mu  sync.Mutex
		out []model.Sample
	)
	sink := model.SinkFunc(func(s model.Sample) {
		mu.Lock()
		defer mu.Unlock()
		out = append(out, s)
		t.Logf("%-14s %-5s slot=%s lost=%v reason=%s rtt=%s reused=%v err=%s",
			s.Key.Target, s.Key.Kind, s.Slot.Format("15:04:05.000"), s.Lost, s.Reason, s.RTT, s.Reused, s.Err)
		if done(out) {
			cancel()
		}
	})
	if err := r.Run(ctx, p, sink); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	return append([]model.Sample(nil), out...)
}

func profileOf(ts ...model.Target) *model.Profile {
	p := &model.Profile{Name: "test", Groups: []model.Group{{ID: "g"}}}
	for _, t := range ts {
		t.Group = "g"
		p.Targets = append(p.Targets, t)
	}
	return p
}

func quietLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

func byKey(ss []model.Sample) map[model.SeriesKey][]model.Sample {
	m := map[model.SeriesKey][]model.Sample{}
	for _, s := range ss {
		m[s.Key] = append(m[s.Key], s)
	}
	return m
}

func icmpAvailable(t *testing.T) bool {
	c := Detect(context.Background())
	t.Logf("caps: %+v", c)
	return c.ICMP != ICMPUnavailable
}

func tlsServer(t *testing.T, h2 bool) (*httptest.Server, int, *tls.Config) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method %s", r.Method)
		}
		fmt.Fprintf(w, "ok %s", r.Proto)
	}))
	srv.EnableHTTP2 = h2
	srv.StartTLS()
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())
	return srv, port, srv.Client().Transport.(*http.Transport).TLSClientConfig
}

func TestLocalhostAllKinds(t *testing.T) {
	for _, h2 := range []bool{false, true} {
		t.Run(fmt.Sprintf("h2=%v", h2), func(t *testing.T) {
			_, port, tc := tlsServer(t, h2)
			icmp := icmpAvailable(t)
			r := New(Options{TLSConfig: tc, Log: quietLog()})
			tg := model.Target{Name: "local", Host: "127.0.0.1", Port: port, Interval: 600 * time.Millisecond}
			k := func(kind model.ProbeKind) model.SeriesKey { return model.SeriesKey{Target: "local", Kind: kind} }
			ss := collect(t, r, profileOf(tg), 20*time.Second, func(ss []model.Sample) bool {
				m := byKey(ss)
				return len(m[k(model.KindHTTPS)]) >= 3 && len(m[k(model.KindTCP)]) >= 2 &&
					(!icmp || len(m[k(model.KindICMP)]) >= 3)
			})
			m := byKey(ss)
			if len(m[k(model.KindHTTPS)]) < 3 || len(m[k(model.KindTCP)]) < 2 {
				t.Fatalf("too few samples: %d https, %d tcp", len(m[k(model.KindHTTPS)]), len(m[k(model.KindTCP)]))
			}
			for _, s := range ss {
				if s.Lost {
					t.Errorf("lost: %+v", s)
				}
				// Go's monotonic clock on Windows ticks every ~0.5ms, and
				// IcmpSendEcho reports whole milliseconds, so a loopback RTT
				// can legitimately read as 0 there.
				minRTT := time.Duration(1)
				if runtime.GOOS == "windows" {
					minRTT = 0
				}
				if s.RTT < minRTT || s.RTT > time.Second {
					t.Errorf("rtt %s: %+v", s.RTT, s)
				}
			}
			hs := m[k(model.KindHTTPS)]
			if hs[0].Reused {
				t.Error("first HTTPS request on a reused connection")
			}
			for _, s := range hs[1:] {
				if !s.Reused {
					t.Errorf("HTTPS request after the first not reused: %+v", s)
				}
			}
			if icmp && len(m[k(model.KindICMP)]) < 3 {
				t.Errorf("ICMP: %d samples", len(m[k(model.KindICMP)]))
			}
			for key, ss := range m {
				for i := 1; i < len(ss); i++ {
					if !ss[i].Slot.After(ss[i-1].Slot) {
						t.Errorf("%v: slots not increasing: %s then %s", key, ss[i-1].Slot, ss[i].Slot)
					}
				}
			}
		})
	}
}

func lostWith(t *testing.T, ss []model.Sample, key model.SeriesKey, want ...model.Reason) {
	t.Helper()
	var n int
	for _, s := range ss {
		if s.Key != key {
			continue
		}
		n++
		ok := false
		for _, w := range want {
			ok = ok || (s.Lost && s.Reason == w)
		}
		if !ok {
			t.Errorf("%s/%s: lost=%v reason=%s err=%q, want lost with %v", key.Target, key.Kind, s.Lost, s.Reason, s.Err, want)
		}
	}
	if n == 0 {
		t.Errorf("%s/%s: no samples", key.Target, key.Kind)
	}
}

func countAll(keys ...model.SeriesKey) func([]model.Sample) bool {
	return func(ss []model.Sample) bool {
		m := byKey(ss)
		for _, k := range keys {
			if len(m[k]) == 0 {
				return false
			}
		}
		return true
	}
}

func TestBlackholeTimeout(t *testing.T) {
	kinds := []model.ProbeKind{model.KindTCP, model.KindHTTPS}
	if icmpAvailable(t) {
		kinds = append(kinds, model.KindICMP)
	}
	r := New(Options{Timeout: 400 * time.Millisecond, Log: quietLog()})
	tg := model.Target{Name: "testnet", Host: "192.0.2.1", Kinds: kinds, Interval: 900 * time.Millisecond}
	var keys []model.SeriesKey
	for _, k := range kinds {
		keys = append(keys, model.SeriesKey{Target: "testnet", Kind: k})
	}
	ss := collect(t, r, profileOf(tg), 15*time.Second, countAll(keys...))
	for _, k := range keys {
		lostWith(t, ss, k, model.ReasonTimeout)
	}
}

func TestClosedPortRefused(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	// Windows retries a refused SYN for about a second before reporting it.
	r := New(Options{Log: quietLog(), Timeout: 3 * time.Second})
	tg := model.Target{Name: "closed", Host: "127.0.0.1", Port: port, Kinds: []model.ProbeKind{model.KindTCP, model.KindHTTPS}, Interval: 500 * time.Millisecond}
	keys := []model.SeriesKey{{Target: "closed", Kind: model.KindTCP}, {Target: "closed", Kind: model.KindHTTPS}}
	ss := collect(t, r, profileOf(tg), 10*time.Second, countAll(keys...))
	for _, k := range keys {
		lostWith(t, ss, k, model.ReasonRefused)
	}
}

func TestUnresolvableDNS(t *testing.T) {
	kinds := []model.ProbeKind{model.KindTCP, model.KindHTTPS}
	if icmpAvailable(t) {
		kinds = append(kinds, model.KindICMP)
	}
	r := New(Options{Log: quietLog(), RetryResolve: 200 * time.Millisecond})
	tg := model.Target{Name: "bad", Host: "nonexistent.invalid", Kinds: kinds, Interval: 500 * time.Millisecond}
	var keys []model.SeriesKey
	for _, k := range kinds {
		keys = append(keys, model.SeriesKey{Target: "bad", Kind: k})
	}
	ss := collect(t, r, profileOf(tg), 15*time.Second, countAll(keys...))
	for _, k := range keys {
		lostWith(t, ss, k, model.ReasonDNS)
	}
}

func TestTLSFailure(t *testing.T) {
	_, port, _ := tlsServer(t, false)
	r := New(Options{Log: quietLog()}) // system roots: the test CA is unknown
	tg := model.Target{Name: "badcert", Host: "127.0.0.1", Port: port, Kinds: []model.ProbeKind{model.KindHTTPS}, Interval: 500 * time.Millisecond}
	key := model.SeriesKey{Target: "badcert", Kind: model.KindHTTPS}
	ss := collect(t, r, profileOf(tg), 10*time.Second, countAll(key))
	lostWith(t, ss, key, model.ReasonTLS)
}

// TestRunnerSlots drives a real runner with a fake wall clock that jumps
// forward: slots come from the injected clock, never repeat, and slots
// skipped by the jump get no sample.
func TestRunnerSlots(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	iv := 15 * time.Second
	start := time.Date(2026, 9, 26, 12, 0, 3, 0, time.UTC)
	fc := &fakeClock{t: start, jumps: map[int]time.Duration{5: 10 * time.Minute}}
	r := New(Options{Now: fc.now, Log: quietLog()}).(*runner)
	r.sleep = func(ctx context.Context, d time.Duration) bool {
		time.Sleep(time.Millisecond)
		return fc.sleep(ctx, d)
	}
	tg := model.Target{Name: "tcp", Host: "127.0.0.1", Port: ln.Addr().(*net.TCPAddr).Port,
		Kinds: []model.ProbeKind{model.KindTCP}, Interval: iv}
	ss := collect(t, r, profileOf(tg), 10*time.Second, func(ss []model.Sample) bool { return len(ss) >= 10 })
	if len(ss) < 10 {
		t.Fatalf("%d samples", len(ss))
	}
	gaps := 0
	for i, s := range ss {
		if s.Lost {
			t.Errorf("lost: %+v", s)
		}
		if s.Slot.UnixNano()%int64(iv) != 0 || s.Slot.Location() != time.UTC {
			t.Errorf("slot %v not aligned UTC", s.Slot)
		}
		if i == 0 {
			// TCP phase for 1 target is 7.5s: the 12:00:00 slot fires at 12:00:07.5.
			if want := start.Truncate(iv); !s.Slot.Equal(want) {
				t.Errorf("first slot %s, want %s", s.Slot, want)
			}
			continue
		}
		switch d := s.Slot.Sub(ss[i-1].Slot); {
		case d == iv:
		case d > 9*time.Minute:
			gaps++
		default:
			t.Errorf("slot step %s at %d", d, i)
		}
	}
	if gaps != 1 {
		t.Errorf("want one gap from the clock jump, got %d", gaps)
	}
}

func TestNoiseFilter(t *testing.T) {
	var buf bytes.Buffer
	old, oldFlags := log.Writer(), log.Flags()
	defer func() { log.SetOutput(old); log.SetFlags(oldFlags) }()
	log.SetOutput(&buf)
	log.SetFlags(0)
	silenceHTTP2Noise()
	silenceHTTP2Noise() // idempotent
	log.Printf("Transport: unhandled response frame type %T", struct{}{})
	log.Printf("kept")
	if got := buf.String(); got != "kept\n" {
		t.Errorf("log output %q", got)
	}
	if _, ok := log.Writer().(*noiseFilter); !ok {
		t.Error("filter not installed")
	}
	if f := log.Writer().(*noiseFilter); f.w != &buf {
		t.Error("filter wrapped twice")
	}
}

// TestLiveUnprivileged is run by hand (and in the Docker unprivileged check)
// as a non-root user: FYISP_LIVE=1 ./probe.test -test.run TestLiveUnprivileged -test.v
func TestLiveUnprivileged(t *testing.T) {
	if os.Getenv("FYISP_LIVE") == "" {
		t.Skip("set FYISP_LIVE=1 to probe 127.0.0.1 and 1.1.1.1")
	}
	t.Logf("uid=%d gid=%d", os.Getuid(), os.Getgid())
	if b, err := os.ReadFile("/proc/sys/net/ipv4/ping_group_range"); err == nil {
		t.Logf("ping_group_range=%s", strings.TrimSpace(string(b)))
	}
	r := New(Options{Log: quietLog()})
	c := r.Caps()
	t.Logf("caps: %+v", c)
	if want := os.Getenv("FYISP_WANT_ICMP"); want != "" && c.ICMP != want {
		t.Fatalf("ICMP mode %q, want %q", c.ICMP, want)
	}
	p := profileOf(
		model.Target{Name: "loopback", Host: "127.0.0.1", Kinds: []model.ProbeKind{model.KindICMP}, Interval: 3 * time.Second},
		model.Target{Name: "cloudflare-dns", Host: "1.1.1.1", Interval: 3 * time.Second},
	)
	keys := []model.SeriesKey{{Target: "loopback", Kind: model.KindICMP}, {Target: "cloudflare-dns", Kind: model.KindICMP},
		{Target: "cloudflare-dns", Kind: model.KindTCP}, {Target: "cloudflare-dns", Kind: model.KindHTTPS}}
	if c.ICMP == ICMPUnavailable {
		keys = keys[2:]
	}
	ss := collect(t, r, p, 20*time.Second, func(ss []model.Sample) bool {
		m := byKey(ss)
		return countAll(keys...)(ss) && len(m[keys[len(keys)-1]]) >= 2
	})
	for _, k := range keys {
		var ok bool
		for _, s := range ss {
			ok = ok || (s.Key == k && !s.Lost)
		}
		if !ok {
			t.Errorf("%s/%s: no successful sample", k.Target, k.Kind)
		}
	}
}

func TestHTTPSRTTFallbacks(t *testing.T) {
	t0 := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	at := func(ms int) time.Time { return t0.Add(time.Duration(ms) * time.Millisecond) }
	var z time.Time
	cases := []struct {
		name                                      string
		gotConn, wroteHeaders, wrote, first, done time.Time
		want                                      time.Duration
	}{
		{"all events", at(0), at(1), at(2), at(12), at(13), 10 * time.Millisecond},
		{"first before wrote (race)", at(0), at(1), at(5), at(3), at(6), 2 * time.Millisecond},
		{"no WroteRequest yet (h2)", at(0), at(1), z, at(11), at(12), 10 * time.Millisecond},
		{"no first byte event", at(0), at(1), at(2), z, at(20), 18 * time.Millisecond},
		{"only GotConn", at(0), z, z, z, at(9), 9 * time.Millisecond},
		{"WroteHeaders recorded after the response (h2)", at(0), at(15), z, at(10), at(11), 10 * time.Millisecond},
		{"all request events late", z, at(15), at(16), at(10), at(11), 0},
		{"nothing", z, z, z, z, at(9), 0},
	}
	for _, c := range cases {
		if got := httpsRTT(c.gotConn, c.wroteHeaders, c.wrote, c.first, c.done); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
