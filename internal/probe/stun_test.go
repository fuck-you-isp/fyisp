package probe

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

// mappedIP is the "public address" fake STUN servers report. It must never
// show up in a sample, an error or a log line.
var mappedIP = netip.MustParseAddr("198.51.100.77")

// stunResponse builds a Binding Success with XOR-MAPPED-ADDRESS (and
// MAPPED-ADDRESS) of mapped and a SOFTWARE attribute whose odd length needs
// padding.
func stunResponse(txid [12]byte, mapped netip.AddrPort) []byte {
	var attrs []byte
	attr := func(typ uint16, v []byte) {
		var h [4]byte
		binary.BigEndian.PutUint16(h[0:2], typ)
		binary.BigEndian.PutUint16(h[2:4], uint16(len(v)))
		attrs = append(attrs, h[:]...)
		attrs = append(attrs, v...)
		for len(attrs)%4 != 0 {
			attrs = append(attrs, 0)
		}
	}
	a4 := mapped.Addr().As4()
	plain := []byte{0, 1, byte(mapped.Port() >> 8), byte(mapped.Port()), a4[0], a4[1], a4[2], a4[3]}
	xor := slices4(plain)
	var cookie [4]byte
	binary.BigEndian.PutUint32(cookie[:], stunMagicCookie)
	xor[2] ^= cookie[0]
	xor[3] ^= cookie[1]
	for i := range 4 {
		xor[4+i] ^= cookie[i]
	}
	attr(0x0001, plain)                // MAPPED-ADDRESS
	attr(0x0020, xor)                  // XOR-MAPPED-ADDRESS
	attr(0x8022, []byte("fyisp-test")) // SOFTWARE, 10 bytes + 2 padding
	b := make([]byte, stunHeaderLen, stunHeaderLen+len(attrs))
	binary.BigEndian.PutUint16(b[0:2], stunBindingSuccess)
	binary.BigEndian.PutUint16(b[2:4], uint16(len(attrs)))
	binary.BigEndian.PutUint32(b[4:8], stunMagicCookie)
	copy(b[8:20], txid[:])
	return append(b, attrs...)
}

func slices4(b []byte) []byte { return append([]byte(nil), b...) }

func txOf(b []byte) (tx [12]byte) { copy(tx[:], b[8:20]); return }

func TestParseSTUN(t *testing.T) {
	tx := [12]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}
	good := stunResponse(tx, netip.AddrPortFrom(mappedIP, 40000))
	if got, ok := parseSTUN(good); !ok || got != tx {
		t.Fatalf("good response: %v %v", got, ok)
	}
	// Trailing bytes after the message are allowed (the header length rules).
	if _, ok := parseSTUN(append(slices4(good), 0, 0, 0, 0)); !ok {
		t.Fatal("trailing bytes rejected")
	}
	mod := func(f func(b []byte) []byte) []byte { return f(slices4(good)) }
	bad := map[string][]byte{
		"empty":       nil,
		"short":       good[:19],
		"request":     stunRequest(tx),
		"error class": mod(func(b []byte) []byte { b[1] = 0x11; return b }),
		"top bits":    mod(func(b []byte) []byte { b[0] |= 0x40; return b }),
		"cookie":      mod(func(b []byte) []byte { b[4] ^= 0xff; return b }),
		"len not /4":  mod(func(b []byte) []byte { binary.BigEndian.PutUint16(b[2:4], uint16(len(b)-stunHeaderLen-2)); return b }),
		"len > read":  mod(func(b []byte) []byte { binary.BigEndian.PutUint16(b[2:4], uint16(len(b)-stunHeaderLen+4)); return b }),
		"truncated":   good[:len(good)-4],
		// The last attribute (SOFTWARE, 10 bytes) claims 13: its padding
		// then runs past the message.
		"attr overrun": mod(func(b []byte) []byte {
			binary.BigEndian.PutUint16(b[len(b)-16+2:len(b)-16+4], 13)
			return b
		}),
		// A 2-byte attribute header stub at the end of the message.
		"attr header": mod(func(b []byte) []byte {
			binary.BigEndian.PutUint16(b[2:4], uint16(len(b)-stunHeaderLen+4))
			return append(b, 0x80, 0x22, 0, 0)[:len(b)+2]
		}),
	}
	for name, b := range bad {
		if _, ok := parseSTUN(b); ok {
			t.Errorf("%s: accepted", name)
		}
	}
	// A header-only success (no attributes) is a valid answer.
	h := stunRequest(tx)
	binary.BigEndian.PutUint16(h[0:2], stunBindingSuccess)
	if _, ok := parseSTUN(h); !ok {
		t.Error("header-only success rejected")
	}
}

func FuzzParseSTUN(f *testing.F) {
	tx := [12]byte{9}
	f.Add(stunResponse(tx, netip.AddrPortFrom(mappedIP, 1)))
	f.Add(stunRequest(tx))
	f.Add([]byte{1, 1, 0, 4, 0x21, 0x12, 0xa4, 0x42, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1, 0xff, 0xff})
	f.Fuzz(func(t *testing.T, b []byte) {
		got, ok := parseSTUN(b)
		if !ok {
			return
		}
		n := int(binary.BigEndian.Uint16(b[2:4]))
		if len(b) < stunHeaderLen+n || n%4 != 0 || binary.BigEndian.Uint16(b[0:2]) != stunBindingSuccess ||
			binary.BigEndian.Uint32(b[4:8]) != stunMagicCookie || !bytes.Equal(got[:], b[8:20]) {
			t.Fatalf("accepted an invalid message % x", b)
		}
	})
}

func TestBurstStats(t *testing.T) {
	ms := func(v ...float64) []time.Duration {
		var out []time.Duration
		for _, x := range v {
			out = append(out, time.Duration(x*float64(time.Millisecond)))
		}
		return out
	}
	if got := medianRTT(ms(30, 10, 20)); got != 20*time.Millisecond {
		t.Errorf("median odd %v", got)
	}
	if got := medianRTT(ms(40, 10, 20, 30)); got != 25*time.Millisecond {
		t.Errorf("median even %v", got)
	}
	// |20-10| + |15-20| + |15-15| = 15 over 3 differences.
	if got := jitterOf(ms(10, 20, 15, 15)); got != 5*time.Millisecond {
		t.Errorf("jitter %v", got)
	}
}

func TestBurstSamples(t *testing.T) {
	kinds := func(ss []model.Sample) string {
		var b strings.Builder
		for _, s := range ss {
			fmt.Fprintf(&b, "%s:", s.Key.Kind)
			if s.Lost {
				fmt.Fprintf(&b, "lost(%s) ", s.Reason)
			} else {
				fmt.Fprintf(&b, "%v ", s.RTT)
			}
		}
		return strings.TrimSpace(b.String())
	}
	ms := time.Millisecond
	cases := []struct {
		res  burstResult
		err  error
		want string
	}{
		{burstResult{sent: 5, rtts: []time.Duration{10 * ms, 12 * ms, 11 * ms, 13 * ms, 10 * ms}}, nil, "udp:11ms udp_jitter:2ms udp_loss:0s"},
		{burstResult{sent: 5, rtts: []time.Duration{10 * ms, 14 * ms}}, nil, "udp:12ms udp_jitter:4ms udp_loss:60ms"},
		{burstResult{sent: 5, rtts: []time.Duration{10 * ms}}, nil, "udp:10ms udp_jitter:lost(not measured) udp_loss:80ms"},
		{burstResult{sent: 5}, nil, "udp:lost(timeout) udp_jitter:lost(not measured) udp_loss:100ms"},
		{burstResult{}, &net.OpError{Op: "read", Net: "udp", Err: errRefused}, "udp:lost(refused) udp_jitter:lost(refused) udp_loss:lost(refused)"},
		{burstResult{}, &net.DNSError{Err: "x", Name: "h"}, "udp:lost(dns) udp_jitter:lost(dns) udp_loss:lost(dns)"},
	}
	for _, c := range cases {
		ss := burstSamples("t", c.res, c.err)
		if got := kinds(ss); got != c.want {
			t.Errorf("got  %s\nwant %s", got, c.want)
		}
		for _, s := range ss {
			if s.Key.Target != "t" || (s.Err != "" && !strings.HasPrefix(s.Err, "udp: ")) {
				t.Errorf("sample %+v", s)
			}
		}
	}
}

func TestCNAMERegion(t *testing.T) {
	for in, want := range map[string]string{
		"a-tr-teamsc-swce-01.swedencentral.cloudapp.azure.com.": "swedencentral",
		"b-tr-teamsc-euno-05.NorthEurope.cloudapp.azure.com":    "northeurope",
		"swedencentral.cloudapp.azure.com.":                     "swedencentral",
		"stun.l.google.com.":                                    "",
		".cloudapp.azure.com":                                   "",
		"x.bad_region.cloudapp.azure.com":                       "",
		"x.cloudapp.azure.com.evil.example":                     "",
		"":                                                      "",
	} {
		if got := cnameRegion(in); got != want {
			t.Errorf("cnameRegion(%q) = %q, want %q", in, got, want)
		}
	}
}

// stunServer is a fake STUN server on 127.0.0.1 (or addr). reply decides
// what to do with the i-th request (0-based): answer after delay, or drop.
// A wrongTx answer with a different transaction ID is sent first when set.
type stunServer struct {
	conn  *net.UDPConn
	n     atomic.Int64
	reply func(i int) (answer bool, delay time.Duration, wrongTx bool)
	// spoof, when set, answers from this other socket instead (a spoofed
	// source: the client's connected socket must drop it).
	spoof atomic.Pointer[net.UDPConn]
	wg    sync.WaitGroup
}

func newSTUNServer(t *testing.T, addr string, reply func(i int) (bool, time.Duration, bool)) *stunServer {
	t.Helper()
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	c, err := net.ListenUDP("udp4", net.UDPAddrFromAddrPort(netip.MustParseAddrPort(addr)))
	if err != nil {
		t.Skipf("listen %s: %v", addr, err)
	}
	s := &stunServer{conn: c, reply: reply}
	if s.reply == nil {
		s.reply = func(int) (bool, time.Duration, bool) { return true, 0, false }
	}
	s.wg.Add(1)
	go s.serve()
	t.Cleanup(func() { c.Close(); s.wg.Wait() })
	return s
}

func (s *stunServer) port() int { return s.conn.LocalAddr().(*net.UDPAddr).Port }

func (s *stunServer) serve() {
	defer s.wg.Done()
	buf := make([]byte, 1500)
	for {
		n, from, err := s.conn.ReadFromUDPAddrPort(buf)
		if err != nil {
			return
		}
		if n != stunHeaderLen || binary.BigEndian.Uint16(buf[0:2]) != stunBindingRequest {
			continue
		}
		i := int(s.n.Add(1) - 1)
		tx := txOf(buf[:n])
		answer, delay, wrong := s.reply(i)
		if !answer {
			continue
		}
		out := s.conn
		if sp := s.spoof.Load(); sp != nil {
			out = sp
		}
		send := func() {
			if wrong {
				w := tx
				w[0] ^= 0xff
				out.WriteToUDPAddrPort(stunResponse(w, netip.AddrPortFrom(mappedIP, 40000)), from)
			}
			out.WriteToUDPAddrPort(stunResponse(tx, netip.AddrPortFrom(mappedIP, 40000)), from)
		}
		if delay > 0 {
			time.AfterFunc(delay, send)
		} else {
			send()
		}
	}
}

func dialTo(t *testing.T, port int) *net.UDPConn {
	t.Helper()
	c, err := net.DialUDP("udp4", nil, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestRunBurst(t *testing.T) {
	ctx := context.Background()
	t.Run("all answered", func(t *testing.T) {
		srv := newSTUNServer(t, "", nil)
		t0 := time.Now()
		res, err := runBurst(ctx, dialTo(t, srv.port()), BurstSize, BurstGap, time.Second)
		if err != nil || res.sent != 5 || len(res.rtts) != 5 {
			t.Fatalf("%+v %v", res, err)
		}
		// Done as soon as the last answer is in: ~4 gaps, not the timeout.
		if d := time.Since(t0); d > 500*time.Millisecond || d < 4*BurstGap {
			t.Errorf("burst took %v", d)
		}
	})
	t.Run("partial loss", func(t *testing.T) {
		srv := newSTUNServer(t, "", func(i int) (bool, time.Duration, bool) { return i != 1 && i != 3, 0, false })
		res, err := runBurst(ctx, dialTo(t, srv.port()), BurstSize, BurstGap, 300*time.Millisecond)
		if err != nil || res.sent != 5 || len(res.rtts) != 3 {
			t.Fatalf("%+v %v", res, err)
		}
		ss := burstSamples("x", res, err)
		if ss[2].RTT != 40*time.Millisecond {
			t.Errorf("loss %v", ss[2].RTT)
		}
	})
	t.Run("late answers are lost", func(t *testing.T) {
		srv := newSTUNServer(t, "", func(i int) (bool, time.Duration, bool) { return true, 250 * time.Millisecond, false })
		res, err := runBurst(ctx, dialTo(t, srv.port()), 3, BurstGap, 100*time.Millisecond)
		if err != nil || res.sent != 3 || len(res.rtts) != 0 {
			t.Fatalf("%+v %v", res, err)
		}
	})
	t.Run("transaction id mismatch", func(t *testing.T) {
		srv := newSTUNServer(t, "", func(i int) (bool, time.Duration, bool) { return true, 5 * time.Millisecond, true })
		res, err := runBurst(ctx, dialTo(t, srv.port()), BurstSize, BurstGap, 300*time.Millisecond)
		if err != nil || len(res.rtts) != 5 {
			t.Fatalf("%+v %v", res, err)
		}
		// The wrong answer came first and was skipped: each RTT includes
		// the 5 ms delay of the right one.
		for _, d := range res.rtts {
			if d < 5*time.Millisecond {
				t.Errorf("rtt %v: the mismatched answer was taken", d)
			}
		}
	})
	t.Run("spoofed source", func(t *testing.T) {
		srv := newSTUNServer(t, "", nil)
		spoof, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
		if err != nil {
			t.Fatal(err)
		}
		defer spoof.Close()
		srv.spoof.Store(spoof)
		res, err := runBurst(ctx, dialTo(t, srv.port()), 2, BurstGap, 200*time.Millisecond)
		if err != nil || res.sent != 2 || len(res.rtts) != 0 {
			t.Fatalf("answers from another port accepted: %+v %v", res, err)
		}
	})
	t.Run("context", func(t *testing.T) {
		srv := newSTUNServer(t, "", func(int) (bool, time.Duration, bool) { return false, 0, false })
		ctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
		defer cancel()
		_, err := runBurst(ctx, dialTo(t, srv.port()), BurstSize, BurstGap, time.Second)
		if err == nil {
			t.Fatal("no error after cancel")
		}
	})
}

// udpRunner returns a real runner whose names resolve with addrs (host ->
// address, swappable) and CNAMEs from cnames.
func udpRunner(t *testing.T, addrs *sync.Map, cnames map[string]string, log *slog.Logger) *runner {
	t.Helper()
	if log == nil {
		log = quietLog()
	}
	r := New(Options{
		Log: log,
		Lookup: func(_ context.Context, h string) ([]netip.Addr, error) {
			if a, ok := addrs.Load(h); ok {
				return []netip.Addr{a.(netip.Addr)}, nil
			}
			return nil, &net.DNSError{Err: "no such host", Name: h, IsNotFound: true}
		},
		LookupCNAME: func(_ context.Context, h string) (string, error) {
			if c, ok := cnames[h]; ok {
				return c, nil
			}
			return h + ".", nil
		},
		ResolveEvery: 100 * time.Millisecond,
	}).(*runner)
	r.udpTimeout = 300 * time.Millisecond
	return r
}

func TestRunnerUDP(t *testing.T) {
	srv := newSTUNServer(t, "", func(i int) (bool, time.Duration, bool) { return i%5 != 4, 0, false })
	var addrs sync.Map
	addrs.Store("relay.test", netip.MustParseAddr("127.0.0.1"))
	r := udpRunner(t, &addrs, map[string]string{"relay.test": "a-tr-teamsc-swce-01.swedencentral.cloudapp.azure.com."}, nil)
	tg := model.Target{Name: "Teams-Relay", Host: "relay.test", Port: srv.port(), Interval: time.Second, Kinds: []model.ProbeKind{model.KindUDP}}
	ss := collect(t, r, profileOf(tg), 15*time.Second, func(ss []model.Sample) bool { return len(ss) >= 9 })
	m := byKey(ss)
	for _, k := range model.UDPKinds {
		if len(m[model.SeriesKey{Target: "Teams-Relay", Kind: k}]) < 3 {
			t.Fatalf("%s: %d samples", k, len(m[model.SeriesKey{Target: "Teams-Relay", Kind: k}]))
		}
	}
	// Siblings share each slot, in order udp, udp_jitter, udp_loss.
	for i := 0; i+2 < len(ss); i += 3 {
		if ss[i].Slot != ss[i+1].Slot || ss[i].Slot != ss[i+2].Slot ||
			ss[i].Key.Kind != model.KindUDP || ss[i+1].Key.Kind != model.KindUDPJitter || ss[i+2].Key.Kind != model.KindUDPLoss {
			t.Fatalf("burst %d not written together: %+v", i/3, ss[i:i+3])
		}
		if ss[i].Lost || ss[i+1].Lost || ss[i+2].RTT != 20*time.Millisecond {
			t.Errorf("burst %d: %+v", i/3, ss[i:i+3])
		}
	}
	st := r.UDPState()
	if len(st) != 1 || st[0].Target != "Teams-Relay" || st[0].Region != "swedencentral" || st[0].Epoch != 0 {
		t.Fatalf("UDPState %+v", st)
	}
	var _ UDPStateSource = r
	var _ UDPStateSource = &Fake{}
}

// TestRunnerUDPAddressChange: when GeoDNS moves the name to another relay,
// the socket is reopened and the next burst goes entirely to the new one
// (no jitter from mixing two relays), and the epoch counts the change.
func TestRunnerUDPAddressChange(t *testing.T) {
	a := newSTUNServer(t, "127.0.0.1:0", nil)
	// The second relay is 200 ms "further" away, on the same port of
	// 127.0.0.2: a burst split between the relays would read as at least
	// 200 ms / (BurstSize-1) = 50 ms of jitter, well above the timer noise
	// of a busy CI machine (Windows: up to ~18 ms).
	const further = 200 * time.Millisecond
	b := newSTUNServer(t, fmt.Sprintf("127.0.0.2:%d", a.port()), func(int) (bool, time.Duration, bool) { return true, further, false })
	var addrs sync.Map
	addrs.Store("relay.test", netip.MustParseAddr("127.0.0.1"))
	r := udpRunner(t, &addrs, nil, nil)
	tg := model.Target{Name: "R", Host: "relay.test", Port: a.port(), Interval: time.Second, Kinds: []model.ProbeKind{model.KindUDP}}
	var switched atomic.Bool
	ss := collect(t, r, profileOf(tg), 20*time.Second, func(ss []model.Sample) bool {
		if len(ss) == 6 && switched.CompareAndSwap(false, true) {
			addrs.Store("relay.test", netip.MustParseAddr("127.0.0.2"))
		}
		return b.n.Load() >= 15
	})
	if a.n.Load() == 0 || b.n.Load() < 15 {
		t.Fatalf("requests: a=%d b=%d", a.n.Load(), b.n.Load())
	}
	if a.n.Load()%BurstSize != 0 || b.n.Load()%BurstSize != 0 {
		t.Errorf("a burst was split between relays: a=%d b=%d", a.n.Load(), b.n.Load())
	}
	for _, s := range ss {
		if s.Key.Kind == model.KindUDPJitter && !s.Lost && s.RTT > further/(BurstSize-1)/2 {
			t.Errorf("jitter %v across the relay change", s.RTT)
		}
	}
	st := r.UDPState()
	if len(st) != 1 || st[0].Epoch != 1 || st[0].ChangedAt.IsZero() {
		t.Fatalf("UDPState %+v", st)
	}
}

// TestUDPPrivacy: the mapped address a STUN server reports is never decoded,
// so it cannot appear in a sample or a log line.
func TestUDPPrivacy(t *testing.T) {
	srv := newSTUNServer(t, "", func(i int) (bool, time.Duration, bool) { return i%2 == 0, 0, i%3 == 0 })
	var logs bytes.Buffer
	var mu sync.Mutex
	log := slog.New(slog.NewTextHandler(writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return logs.Write(p)
	}), &slog.HandlerOptions{Level: slog.LevelDebug}))
	var addrs sync.Map
	addrs.Store("stun.test", netip.MustParseAddr("127.0.0.1"))
	r := udpRunner(t, &addrs, nil, log)
	tg := model.Target{Name: "S", Host: "stun.test", Port: srv.port(), Interval: time.Second, Kinds: []model.ProbeKind{model.KindUDP}}
	ss := collect(t, r, profileOf(tg), 10*time.Second, func(ss []model.Sample) bool { return len(ss) >= 9 })
	for _, s := range ss {
		if strings.Contains(fmt.Sprintf("%#v", s), mappedIP.String()) {
			t.Fatalf("sample leaks the mapped address: %+v", s)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Contains(logs.String(), mappedIP.String()) {
		t.Fatalf("log leaks the mapped address:\n%s", logs.String())
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

func TestFakeBurst(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	slot := time.Unix(1_700_000_000, 0).UTC()
	ss := FakeBurst(r, "Meet-STUN", slot, 20, 0)
	if len(ss) != 3 || ss[0].Lost || ss[1].Lost || ss[2].RTT != 0 {
		t.Fatalf("%+v", ss)
	}
	for i, k := range model.UDPKinds {
		if ss[i].Key.Kind != k || !ss[i].Slot.Equal(slot) {
			t.Fatalf("%+v", ss[i])
		}
	}
	if ss := FakeBurst(r, "x", slot, 20, 1); !ss[0].Lost || ss[2].RTT != 100*time.Millisecond {
		t.Fatalf("all lost: %+v", ss)
	}
}
