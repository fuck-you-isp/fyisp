package s3test

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/s3test/s3fake"
	"github.com/fuck-you-isp/fyisp/internal/speedtest"
)

// lab is a fake server and an engine that reaches it at 127.0.0.1 under any
// name (virtual hosts included).
type lab struct {
	srv  *s3fake.Server
	e    *Engine
	mask *countMasker
	rt   RoundTarget
	pool *x509.CertPool
	now  time.Duration // the engine's clock offset
	mu   sync.Mutex
	look func(host string) ([]netip.Addr, error)
}

func newLab(t *testing.T, tlsOn bool) *lab {
	t.Helper()
	l := &lab{srv: s3fake.New("bkt"), mask: &countMasker{}}
	if tlsOn {
		l.pool = l.srv.StartTLS()
	} else {
		l.srv.Start()
	}
	t.Cleanup(l.srv.Close)
	_, port, _ := net.SplitHostPort(l.srv.Addr())
	scheme := "http"
	if tlsOn {
		scheme = "https"
	}
	ep, _ := url.Parse(scheme + "://s3.test:" + port)
	l.rt = RoundTarget{Target: Target{ID: "t_1", Endpoint: ep, Region: "us-east-1", Bucket: "bkt", RootCAs: l.pool, Private: true},
		Creds: NewCreds(l.srv.AccessKey, l.srv.Secret, "")}
	l.look = func(string) ([]netip.Addr, error) { return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil }
	l.e = New(Options{
		Lookup: func(_ context.Context, network, host string) ([]netip.Addr, error) {
			l.mu.Lock()
			defer l.mu.Unlock()
			return l.look(host)
		},
		Now: func() time.Time {
			l.mu.Lock()
			defer l.mu.Unlock()
			return time.Now().Add(l.now)
		},
		Masker: l.mask, IsSelf: func(netip.Addr) bool { return false }, Sleep: func(context.Context, time.Duration) bool { return true },
		lim: limits{dns: time.Second, connect: time.Second, handshake: time.Second, write: time.Second,
			ttfb: 300 * time.Millisecond, ttfbUpload: time.Second, stall: 300 * time.Millisecond},
	})
	return l
}

// countMasker counts masked windows (and checks they never nest).
type countMasker struct {
	mu     sync.Mutex
	n, in  int
	nested bool
}

func (m *countMasker) Begin(time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.n++
	m.in++
	m.nested = m.nested || m.in > 1
}

func (m *countMasker) End(time.Time) speedtest.Window {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.in--
	return speedtest.Window{}
}

func (m *countMasker) count() int { m.mu.Lock(); defer m.mu.Unlock(); return m.n }

func (l *lab) round(ph Phases) RoundResult {
	return l.e.RunRound(context.Background(), RoundSpec{Install: "inst", Campaign: 7, Round: 3, Targets: []RoundTarget{l.rt}, Phases: ph})
}

func classes(as []model.StorageAttempt) []string {
	var out []string
	for _, a := range as {
		out = append(out, a.Phase+"/"+a.Op+":"+a.Class)
	}
	return out
}

// TestRoundKeyed: a full round over http (hashed payloads) and https
// (unsigned payloads): seeds put once, cold checks fresh every time, warm
// ones on one connection, a ranged download and an upload deleted after.
func TestRoundKeyed(t *testing.T) {
	for _, tlsOn := range []bool{false, true} {
		t.Run(fmt.Sprint("tls=", tlsOn), func(t *testing.T) {
			l := newLab(t, tlsOn)
			ph := Phases{Cold: 3, Warm: 2, Down: 1 << 20, Up: 512 << 10}
			r := l.round(ph)
			want := []string{"seed/put:ok", "seed/put:ok", "cold/get:ok", "cold/get:ok", "cold/get:ok", "warm/get:ok", "warm/get:ok",
				"down/get:ok", "up/put:ok", "up/delete:ok"}
			if got := classes(r.Attempts); strings.Join(got, " ") != strings.Join(want, " ") {
				t.Fatalf("attempts %v", got)
			}
			if r.Down != 1<<20 || r.Up != 1<<10+1<<20+512<<10 || len(r.Skips) != 0 || len(r.Leftovers) != 0 || r.Cancelled {
				t.Errorf("result %+v", r)
			}
			if objs := l.srv.Objects("bkt"); len(objs) != 2 || objs["fyisp-test/inst/seed/1024"] != 1024 || objs["fyisp-test/inst/seed/1048576"] != 1<<20 {
				t.Errorf("objects %v", objs)
			}
			for _, a := range r.Attempts {
				if a.Target != "t_1" || a.Totalms == nil || a.DNSms == nil && a.Phase != model.StoragePhaseWarm || a.EdgePrefix != "127.0.0.0/24" ||
					(tlsOn && a.Phase == model.StoragePhaseCold && (a.TLSms == nil || a.TLSVersion != "1.3" || a.Resumed)) {
					t.Errorf("attempt %+v", a)
				}
			}
			var cold, warm []*float64
			for _, a := range r.Attempts {
				switch a.Phase {
				case model.StoragePhaseCold:
					cold = append(cold, a.Connectms)
				case model.StoragePhaseWarm:
					warm = append(warm, a.Connectms)
				}
			}
			if cold[0] == nil || cold[2] == nil || warm[0] == nil || warm[1] != nil {
				t.Errorf("connect times: cold %v warm %v (the second warm request reuses the connection)", cold, warm)
			}
			for _, a := range r.Attempts {
				if a.Phase == model.StoragePhaseDown && (a.GoodputBps == nil || a.Bytes != 1<<20 || a.Stallms == nil) {
					t.Errorf("download %+v", a)
				}
			}
			payloads := map[string]bool{}
			for _, s := range l.srv.Seen() {
				if s.Method == "PUT" {
					payloads[s.Payload] = true
				}
				if !s.Signed {
					t.Errorf("unsigned request %+v", s)
				}
			}
			if payloads[UnsignedPayload] != tlsOn {
				t.Errorf("payload hashes %v (unsigned only over TLS)", payloads)
			}
			// The big seed, the download and the upload are masked: 3 windows
			// (not the small seed or the checks).
			if n := l.mask.count(); n != 3 || l.mask.nested {
				t.Errorf("%d masked windows (nested %v), want 3", n, l.mask.nested)
			}
			// The next round finds the seeds.
			if got := classes(l.round(ph).Attempts); got[0] != "cold/get:ok" {
				t.Errorf("second round starts %v", got)
			}
		})
	}
}

// TestRoundConnectionOnly: no keys, so unsigned HEADs, and any answer
// (403 here) means the endpoint is up; no transfers.
func TestRoundConnectionOnly(t *testing.T) {
	for _, bucket := range []string{"bkt", ""} {
		l := newLab(t, true)
		l.rt.Creds, l.rt.Bucket = nil, bucket
		r := l.round(Phases{Cold: 2, Warm: 2, Down: 1 << 20, Up: 1 << 20})
		if got := strings.Join(classes(r.Attempts), " "); got != "cold/head:ok cold/head:ok warm/head:ok warm/head:ok" {
			t.Errorf("bucket %q: %s", bucket, got)
		}
		for _, s := range l.srv.Seen() {
			if s.Signed || s.Method != "HEAD" || (bucket == "" && s.Path != "/") {
				t.Errorf("bucket %q: request %+v", bucket, s)
			}
		}
	}
}

// TestRoundConfigFailures: refused keys, a skewed clock, a wrong region
// and an unknown CA end the target's round after one attempt; nothing is
// uploaded.
func TestRoundConfigFailures(t *testing.T) {
	ph := Phases{Cold: 3, Warm: 2, Down: 1 << 20, Up: 1 << 20}
	for _, c := range []struct {
		name  string
		setup func(l *lab)
		class Class
		skip  string
		code  string
	}{
		{"bad secret", func(l *lab) { l.rt.Creds = NewCreds(l.srv.AccessKey, "nope", "") }, ClassHTTPAuth, "auth", "SignatureDoesNotMatch"},
		{"unknown key", func(l *lab) { l.rt.Creds = NewCreds("AKNOPE", l.srv.Secret, "") }, ClassHTTPAuth, "auth", "InvalidAccessKeyId"},
		{"clock +20 min", func(l *lab) { l.now = 20 * time.Minute }, ClassClockSkew, "clock_skew", "RequestTimeTooSkewed"},
		{"wrong region", func(l *lab) { l.srv.StrictRegion, l.rt.Region = true, "eu-north-1" }, ClassHTTPAuth, "auth", "AuthorizationHeaderMalformed"},
		{"unknown CA", func(l *lab) { l.rt.RootCAs = x509.NewCertPool() }, ClassTLSCert, "config", ""},
		{"missing token", func(l *lab) { l.srv.Token = "tok" }, ClassHTTPAuth, "auth", "InvalidTokenId"},
	} {
		l := newLab(t, true)
		c.setup(l)
		r := l.round(ph)
		if len(r.Attempts) != 1 || r.Attempts[0].Class != string(c.class) || r.Skips["t_1"] != c.skip || (c.code != "" && r.Attempts[0].S3Code != c.code) {
			t.Errorf("%s: %v skips %v code %q", c.name, classes(r.Attempts), r.Skips, r.Attempts[0].S3Code)
		}
		if len(l.srv.Objects("bkt")) != 0 {
			t.Errorf("%s: objects %v", c.name, l.srv.Objects("bkt"))
		}
	}
	// A session token, when given, is sent and works.
	l := newLab(t, true)
	l.srv.Token = "tok/en"
	l.rt.Creds = NewCreds(l.srv.AccessKey, l.srv.Secret, "tok/en")
	if r := l.round(Phases{Cold: 1}); r.Attempts[len(r.Attempts)-1].Class != "ok" {
		t.Errorf("token: %v", classes(r.Attempts))
	}
}

// TestRoundNetworkFailures: each network and provider failure, at the step
// it happens, gets its class; the round goes on.
func TestRoundNetworkFailures(t *testing.T) {
	ph := Phases{Cold: 1, Warm: 0, Down: 256 << 10}
	onDownload := func(f s3fake.Fault) func(r *http.Request) s3fake.Fault {
		return func(r *http.Request) s3fake.Fault {
			if r.Method == "GET" && r.Header.Get("Range") != "" {
				return f
			}
			return s3fake.Fault{}
		}
	}
	onCold := func(f s3fake.Fault) func(r *http.Request) s3fake.Fault {
		return func(r *http.Request) s3fake.Fault {
			if r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/seed/1024") {
				return f
			}
			return s3fake.Fault{}
		}
	}
	for _, c := range []struct {
		name  string
		fault func(r *http.Request) s3fake.Fault
		look  func(string) ([]netip.Addr, error)
		phase string
		class Class
	}{
		{"503 SlowDown", onCold(s3fake.Fault{Status: 503, Code: "SlowDown"}), nil, "cold", ClassThrottled},
		{"503 without SlowDown", onCold(s3fake.Fault{Status: 503, Code: "ServiceUnavailable"}), nil, "cold", ClassHTTP5xx},
		{"302", onCold(s3fake.Fault{Status: 302, Code: "Found"}), nil, "cold", ClassHTTPRedirect},
		{"404 on the seed", onCold(s3fake.Fault{Status: 404, Code: "NoSuchKey"}), nil, "cold", ClassHTTP4xx},
		{"500", onCold(s3fake.Fault{Status: 500, Code: "InternalError"}), nil, "cold", ClassHTTP5xx},
		{"reset before response", onCold(s3fake.Fault{ResetBeforeResponse: true}), nil, "cold", ClassResetBeforeResponse},
		{"slow first byte", onCold(s3fake.Fault{Delay: time.Second}), nil, "cold", ClassTimeoutResponse},
		{"reset mid-download", onDownload(s3fake.Fault{ResetAfter: 64 << 10}), nil, "down", ClassResetMidTransfer},
		{"stall mid-download", onDownload(s3fake.Fault{StallAfter: 64 << 10, Stall: time.Second}), nil, "down", ClassStall},
		{"NXDOMAIN", nil, func(string) ([]netip.Addr, error) { return nil, &net.DNSError{Err: "no such host", IsNotFound: true} }, "seed", ClassDNSFail},
		{"no A record", nil, func(string) ([]netip.Addr, error) { return nil, nil }, "seed", ClassDNSNoIPv4},
		{"metadata address", nil, func(string) ([]netip.Addr, error) { return []netip.Addr{netip.MustParseAddr("169.254.169.254")}, nil }, "seed", ClassAddrRefused},
		{"public address for a private target", nil, func(string) ([]netip.Addr, error) { return []netip.Addr{netip.MustParseAddr("52.216.1.1")}, nil }, "seed", ClassAddrRefused},
	} {
		l := newLab(t, true)
		l.srv.Fault = c.fault
		if c.look != nil {
			l.look = c.look
		}
		r := l.round(ph)
		var got *model.StorageAttempt
		for i := range r.Attempts {
			if r.Attempts[i].Phase == c.phase {
				got = &r.Attempts[i]
			}
		}
		okClass := got != nil && got.Class == string(c.class)
		if got != nil && runtime.GOOS == "windows" && c.class == ClassResetMidTransfer && got.Class == string(ClassResetBeforeResponse) {
			okClass = true // the reset arrived before the headers were read
		}
		if !okClass {
			t.Errorf("%s: %v", c.name, classes(r.Attempts))
			continue
		}
		// Windows drops the received data a reset arrives behind, so the
		// bytes before it may never be read there.
		if c.phase == "down" && (got.Bytes >= 256<<10 || (got.Bytes == 0 && runtime.GOOS != "windows")) {
			t.Errorf("%s: bytes %d", c.name, got.Bytes)
		}
		if c.name == "503 SlowDown" && got.S3Code != "SlowDown" {
			t.Errorf("%s: s3 code %q", c.name, got.S3Code)
		}
	}
	// Connection refused (a dialer that says so: a closed loopback port is
	// slow to refuse on Windows).
	l := newLab(t, false)
	l.e.o.Dial = func(context.Context, string, string) (net.Conn, error) {
		return nil, &net.OpError{Op: "dial", Net: "tcp4", Err: os.NewSyscallError("connect", errRefused)}
	}
	l.rt.Creds = nil // connection checks only
	if r := l.round(Phases{Cold: 1}); len(r.Attempts) != 1 || r.Attempts[0].Class != string(ClassConnectRefused) || r.Attempts[0].Connectms == nil {
		t.Errorf("refused: %v %+v", classes(r.Attempts), r.Attempts)
	}
}

// TestWarmConnectionClosed: a server that closes the kept connection, by
// saying so or silently, ends the warm phase with conn_closed; the engine
// never redials.
func TestWarmConnectionClosed(t *testing.T) {
	for _, f := range []s3fake.Fault{{Close: true}, {Drop: true}} {
		l := newLab(t, false)
		l.srv.Fault = func(r *http.Request) s3fake.Fault {
			if r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/seed/1024") {
				return f
			}
			return s3fake.Fault{}
		}
		r := l.round(Phases{Warm: 3})
		if got := strings.Join(classes(r.Attempts), " "); got != "seed/put:ok warm/get:ok warm/get:conn_closed" {
			t.Errorf("%+v: %s", f, got)
		}
	}
}

// TestUploadDeleteFailure: an upload whose delete fails is a leftover, for
// the sweep.
func TestUploadDeleteFailure(t *testing.T) {
	l := newLab(t, false)
	l.srv.Fault = func(r *http.Request) s3fake.Fault {
		if r.Method == "DELETE" {
			return s3fake.Fault{Status: 500, Code: "InternalError"}
		}
		return s3fake.Fault{}
	}
	r := l.round(Phases{Up: 64 << 10})
	if len(r.Leftovers) != 1 || r.Leftovers[0] != "7/3-t_1" {
		t.Errorf("leftovers %v, attempts %v", r.Leftovers, classes(r.Attempts))
	}
}

// TestSweep deletes fyisp's leftovers under the install's prefix (keeping
// the seeds when asked), and never anything else.
func TestSweep(t *testing.T) {
	l := newLab(t, false)
	for _, k := range []string{"fyisp-test/inst/7/1-t_1", "fyisp-test/inst/7/2-t_1", "fyisp-test/inst/seed/1024", "fyisp-test/other/x", "mine/photo.jpg",
		"fyisp-test/inst/../../mine/x"} {
		l.srv.Put("bkt", k, []byte("x"))
	}
	n, class := l.e.Sweep(context.Background(), l.rt, "inst", true)
	objs := l.srv.Objects("bkt")
	if n != 2 || class != ClassOK || len(objs) != 4 || objs["fyisp-test/inst/seed/1024"] != 1 || objs["mine/photo.jpg"] != 1 ||
		objs["fyisp-test/other/x"] != 1 || objs["fyisp-test/inst/../../mine/x"] != 1 {
		t.Errorf("deleted %d %s, left %v", n, class, objs)
	}
	l.srv.Fault = func(r *http.Request) s3fake.Fault {
		if r.Method == "DELETE" {
			return s3fake.Fault{Status: 500, Code: "InternalError"}
		}
		return s3fake.Fault{}
	}
	if n, class := l.e.Sweep(context.Background(), l.rt, "inst", false); n != 0 || class != ClassHTTP5xx {
		t.Errorf("failing deletes: %d %s", n, class)
	}
	l.srv.Fault = nil
	if n, _ := l.e.Sweep(context.Background(), l.rt, "inst", false); n != 1 {
		t.Errorf("without seeds: deleted %d", n)
	}
}

// TestCheck: keys that work, refused keys, a wrong region and a dotted
// bucket over virtual-host TLS each say so.
func TestCheck(t *testing.T) {
	l := newLab(t, true)
	if c := l.e.Check(context.Background(), l.rt); c.Attempt.Class != "ok" || c.Keys != "work" || c.Hint != "" {
		t.Errorf("ok: %+v", c)
	}
	l.srv.StrictRegion, l.rt.Region = true, "auto"
	if c := l.e.Check(context.Background(), l.rt); c.Keys != "refused" || c.Hint != "region" {
		t.Errorf("region: %+v", c)
	}
	l.rt.Region, l.rt.Bucket = "us-east-1", "my.bucket"
	if c := l.e.Check(context.Background(), l.rt); c.Attempt.Class != string(ClassTLSCert) || c.Hint != "path_style" {
		t.Errorf("dotted: %+v", c)
	}
	l.rt.Creds = nil
	if c := l.e.Check(context.Background(), l.rt); c.Keys != "none" {
		t.Errorf("no keys: %+v", c)
	}
	l = newLab(t, true)
	l.now = 20 * time.Minute
	if c := l.e.Check(context.Background(), l.rt); c.Hint != "clock" || c.Keys != "refused" {
		t.Errorf("clock: %+v", c)
	}
	l = newLab(t, true)
	l.rt.RootCAs = x509.NewCertPool()
	if c := l.e.Check(context.Background(), l.rt); c.Hint != "ca" {
		t.Errorf("ca: %+v", c)
	}
	l = newLab(t, true)
	l.srv.Fault = func(*http.Request) s3fake.Fault { return s3fake.Fault{Status: 301, Code: "PermanentRedirect"} }
	if c := l.e.Check(context.Background(), l.rt); c.Hint != "endpoint" {
		t.Errorf("endpoint: %+v", c)
	}
}

// TestIsSelf: an address of this machine is never kept as the edge.
func TestIsSelf(t *testing.T) {
	l := newLab(t, false)
	l.e.o.IsSelf = func(a netip.Addr) bool { return a.IsLoopback() }
	for _, a := range l.round(Phases{Cold: 1, Warm: 2}).Attempts {
		if a.EdgeIP.IsValid() || a.EdgePrefix != "" {
			t.Errorf("edge kept: %+v", a)
		}
	}
}

// TestCredsRedacted: keys never print, in any format or log.
func TestCredsRedacted(t *testing.T) {
	c := NewCreds("AKIDEXAMPLE1234", "s3cr3t-value", "t0ken-value")
	var sb strings.Builder
	slog.New(slog.NewJSONHandler(&sb, nil)).Info("x", "creds", c, "target", RoundTarget{Creds: c})
	j, _ := json.Marshal(map[string]any{"c": c, "rt": RoundTarget{Creds: c}})
	out := fmt.Sprintf("%v %+v %#v %s %q", c, c, c, c, c) + sb.String() + string(j)
	for _, secret := range []string{"s3cr3t", "t0ken", "AKIDEXAMPLE"} {
		if strings.Contains(out, secret) {
			t.Errorf("%q printed: %s", secret, out)
		}
	}
	if c.AccessKeyLast4() != "1234" || !c.HasToken() {
		t.Error("last 4 or token")
	}
	c.Zero()
	if c.HasToken() || c.AccessKeyLast4() != "" {
		t.Error("Zero left key material")
	}
}

func TestPayload(t *testing.T) {
	a, _ := io.ReadAll(newPayload("k", 100_000))
	b, _ := io.ReadAll(newPayload("k", 100_000))
	c, _ := io.ReadAll(newPayload("other", 100_000))
	if len(a) != 100_000 || string(a) != string(b) || string(a) == string(c) {
		t.Error("payload not reproducible per name")
	}
	if payloadHash("k", 100_000) == payloadHash("k", 100_001) {
		t.Error("hash ignores length")
	}
}

func TestAddressPolicy(t *testing.T) {
	for _, c := range []struct {
		scheme  string
		private bool
		addr    string
		ok      bool
	}{
		{"https", false, "52.216.1.1", true},
		{"https", false, "10.0.0.5", false},  // a public target's name now points at the LAN
		{"https", true, "10.0.0.5", true},    // a LAN MinIO
		{"http", true, "192.168.1.20", true}, // plain http only on the LAN
		{"http", false, "52.216.1.1", false},
		{"https", true, "100.64.1.1", true},
		{"https", true, "127.0.0.1", true},
		{"https", false, "169.254.169.254", false},
		{"https", true, "169.254.169.254", false},
		{"https", false, "224.0.0.1", false},
		{"https", false, "0.0.0.0", false},
		{"https", false, "255.255.255.255", false},
		{"https", false, "2600::1", false}, // IPv4 only
	} {
		if got := AddrAllowed(c.scheme, c.private, netip.MustParseAddr(c.addr)); got != c.ok {
			t.Errorf("%s private=%v %s: %v, want %v", c.scheme, c.private, c.addr, got, c.ok)
		}
	}
}

func TestEstimate(t *testing.T) {
	p := Phases{Cold: 10, Warm: 5, Down: 64 << 20, Up: 16 << 20}
	d := EstimateRound(2, 2, p, [2]float64{100e6, 20e6})
	// cold 10 s, warm 2x5 s, transfers 2x(2+5.4 s + 3+6.7 s)
	if d < 50*time.Second || d > 60*time.Second {
		t.Errorf("estimate %v", d)
	}
	if down, up := DataPerRound(2, p); down != 2*(64<<20+15*1024) || up != 2*(16<<20) {
		t.Errorf("data %d %d", down, up)
	}
	if EstimateRound(0, 0, p, [2]float64{}) != 0 {
		t.Error("no targets")
	}
	if transferBound(1<<30, 0) != 2*time.Minute || transferBound(1, 1e9) != minTransferTime || transferBound(1<<30, 1e6) != maxTransferTime {
		t.Error("transfer bounds")
	}
}

func TestClassGroups(t *testing.T) {
	seen := map[Class]bool{}
	for _, c := range Classes {
		if seen[c.Class] || c.Class.Group() == "" {
			t.Errorf("class %q", c.Class)
		}
		seen[c.Class] = true
	}
	if Class("nope").Group() != "" || ClassThrottled.Group() != GroupProvider || ClassClockSkew.Group() != GroupConfig {
		t.Error("groups")
	}
	if !errors.Is(fmt.Errorf("x: %w", errStall), errStall) {
		t.Error("wrapping")
	}
}

// TestCheckFake: with Options.Fake, a check asks it instead of the network
// (fyisp-webdev), and reads keys from its answer as from a server's.
func TestCheckFake(t *testing.T) {
	var got []string
	e := New(Options{Fake: func(_ context.Context, tg Target, phase, op string, signed bool) model.StorageAttempt {
		got = append(got, fmt.Sprintf("%s %s %s %v", tg.ID, phase, op, signed))
		if signed {
			return model.StorageAttempt{Class: string(ClassHTTPAuth), HTTPStatus: 403}
		}
		return model.StorageAttempt{Class: string(ClassOK), HTTPStatus: 200}
	}})
	ep, _ := url.Parse("https://s3.test")
	tg := Target{ID: "t_1", Endpoint: ep, Region: "us-east-1", Bucket: "bkt"}
	if r := e.Check(context.Background(), RoundTarget{Target: tg}); r.Keys != "none" || r.Attempt.Class != "ok" || r.Attempt.Target != "t_1" {
		t.Errorf("unsigned: %+v", r)
	}
	if r := e.Check(context.Background(), RoundTarget{Target: tg, Creds: NewCreds("AK", "s", "")}); r.Keys != "refused" || r.Attempt.Phase != model.StoragePhaseCold {
		t.Errorf("signed: %+v", r)
	}
	if want := []string{"t_1 cold head false", "t_1 cold list true"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("asked %q, want %q", got, want)
	}
}

// TestEndlessHeaders: a server that sends headers without end fails the
// attempt once headMax is read, without filling memory or waiting out the
// check's bound.
func TestEndlessHeaders(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var sent atomic.Int64
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		go io.Copy(io.Discard, c)
		line := []byte("X-Pad: " + strings.Repeat("a", 1000) + "\r\n")
		if _, err := c.Write([]byte("HTTP/1.1 200 OK\r\n")); err != nil {
			return
		}
		for sent.Load() < 64<<20 {
			n, err := c.Write(line)
			sent.Add(int64(n))
			if err != nil {
				return
			}
		}
	}()
	l := newLab(t, false)
	l.rt.Creds = nil
	l.e.o.Dial = func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, ln.Addr().String())
	}
	c := l.e.Check(context.Background(), l.rt)
	if c.Attempt.Class != string(ClassResetBeforeResponse) {
		t.Errorf("class %q", c.Attempt.Class)
	}
	if n := sent.Load(); n >= 64<<20 {
		t.Errorf("read the whole %d bytes", n)
	}
}
