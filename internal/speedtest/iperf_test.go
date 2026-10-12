package speedtest

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeServer is an in-process iperf3 server: the server side of the control
// protocol, as iperf_server_api.c runs it. mode picks a misbehaviour.
type fakeServer struct {
	ln   net.Listener
	mode string // "", "busy", "close", "error", "terminate", "stall", "deaf" (ignores TEST_END, keeps sending)
	rate int    // bytes per 10 ms per stream when sending (downloads)

	mu      sync.Mutex
	params  map[string]any
	results serverResults
	cookies []string // control cookie, then each stream's
	recv    int64
	moved   int64 // every byte sent or received on data streams, over every test
	err     error
	wg      sync.WaitGroup

	out, in  atomic.Int64 // data bytes written and read so far, every stream and test
	busyNext atomic.Int32 // the next sessions to answer busy
	open     atomic.Int32 // connections accepted and not yet closed by the server
	peak     atomic.Int32 // the most open at once
}

// trackedConn keeps fakeServer.open.
type trackedConn struct {
	net.Conn
	f    *fakeServer
	once sync.Once
}

func (c *trackedConn) Close() error {
	c.once.Do(func() { c.f.open.Add(-1) })
	return c.Conn.Close()
}

func (f *fakeServer) accept() (net.Conn, error) {
	c, err := f.ln.Accept()
	if err != nil {
		return nil, err
	}
	n := f.open.Add(1)
	for {
		p := f.peak.Load()
		if n <= p || f.peak.CompareAndSwap(p, n) {
			break
		}
	}
	return &trackedConn{Conn: c, f: f}, nil
}

func newFakeServer(t *testing.T, mode string) *fakeServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeServer{ln: ln, mode: mode, rate: 64 << 10}
	f.wg.Add(1)
	go f.serve()
	t.Cleanup(func() {
		ln.Close()
		f.wg.Wait()
		if f.err != nil {
			t.Errorf("fake server: %v", f.err)
		}
	})
	return f
}

const (
	// maxCatchUp bounds the ticks one download write makes up (100 ms:
	// one tick of the client's).
	maxCatchUp = 10
	// upRate is the bytes per second the fake reads per upload stream
	// (~80 MB/s for 3 streams).
	upRate = 256 << 10 * 100
)

func (f *fakeServer) addr() string { return f.ln.Addr().String() }

func (f *fakeServer) fail(err error) {
	if f.mode == "stall" || f.mode == "deaf" {
		return // the client gives up on purpose
	}
	f.mu.Lock()
	if f.err == nil {
		f.err = err
	}
	f.mu.Unlock()
}

func (f *fakeServer) serve() {
	defer f.wg.Done()
	for {
		c, err := f.accept()
		if err != nil {
			return
		}
		f.session(c)
	}
}

func readCookie(c net.Conn) (string, error) {
	var b [cookieSize]byte
	if _, err := io.ReadFull(c, b[:]); err != nil {
		return "", err
	}
	if b[cookieSize-1] != 0 || bytes.IndexByte(b[:cookieSize-1], 0) >= 0 {
		return "", errors.New("cookie not 36 characters and a NUL")
	}
	return string(b[:cookieSize-1]), nil
}

func writeJSON(c net.Conn, v any) error {
	b, _ := json.Marshal(v)
	return binary.Write(c, binary.BigEndian, append(binary.BigEndian.AppendUint32(nil, uint32(len(b))), b...))
}

func readJSON(c net.Conn, v any) error {
	var n uint32
	if err := binary.Read(c, binary.BigEndian, &n); err != nil {
		return err
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(c, b); err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// session runs one test on control connection c.
func (f *fakeServer) session(c net.Conn) {
	defer c.Close()
	if f.busyNext.Load() > 0 {
		f.busyNext.Add(-1)
		c.Write([]byte{0xff})
		return
	}
	switch f.mode {
	case "busy":
		c.Write([]byte{0xff}) // ACCESS_DENIED, without reading the cookie
		return
	case "close":
		return
	}
	cookie, err := readCookie(c)
	if err != nil {
		f.fail(err)
		return
	}
	f.mu.Lock()
	f.cookies = append(f.cookies, cookie)
	f.mu.Unlock()
	c.Write([]byte{stateParamExchange})
	var p map[string]any
	if err := readJSON(c, &p); err != nil {
		f.fail(err)
		return
	}
	f.mu.Lock()
	f.params = p
	f.mu.Unlock()
	if f.mode == "error" {
		c.Write([]byte{0xfe, 0, 0, 0, 101, 0, 0, 0, 24})
		return
	}
	c.Write([]byte{stateCreateStreams})
	n := int(p["parallel"].(float64))
	streams := make([]net.Conn, 0, n)
	defer func() {
		for _, s := range streams {
			s.Close()
		}
	}()
	for range n {
		s, err := f.accept()
		if err != nil {
			f.fail(err)
			return
		}
		ck, err := readCookie(s)
		if err != nil || ck != cookie {
			s.Close()
			f.fail(errors.New("stream cookie differs from the control cookie"))
			return
		}
		streams = append(streams, s)
	}
	c.Write([]byte{stateTestStart, stateTestRunning})
	if f.mode == "terminate" {
		time.Sleep(200 * time.Millisecond)
		c.Write([]byte{stateServerTerminate})
		return
	}
	reverse := p["reverse"] == true
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var recv, sent [16]int64
	for i, s := range streams {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if reverse {
				// Each write carries the bytes due since the last one, so
				// ticks a busy machine drops are made up and every second
				// carries the same bytes (up to maxCatchUp ticks at once).
				buf := make([]byte, maxCatchUp*f.rate)
				tk := time.NewTicker(10 * time.Millisecond)
				defer tk.Stop()
				t0 := time.Now()
				last := t0
				for {
					select {
					case <-stop:
						return
					case <-tk.C:
						now := time.Now()
						n := int(float64(f.rate) * min(maxCatchUp, float64(now.Sub(last))/float64(10*time.Millisecond)))
						last = now
						if f.mode == "ramp" { // slow start: full rate after 500 ms
							n = int(float64(n) * min(1, float64(time.Since(t0))/float64(500*time.Millisecond)))
						}
						if f.mode == "slowramp" { // a far path's slow start: full rate after 2.5 s
							n = int(float64(n) * min(1, float64(time.Since(t0))/float64(2500*time.Millisecond)))
						}
						if f.mode == "climb" { // still speeding up, 1.5× per 100 ms until 3 s: past a
							// 2 s run, and steep enough that a tick's noise never looks level
							n = int(float64(n) * math.Pow(1.5, min(0, float64(time.Since(t0)-3*time.Second)/float64(100*time.Millisecond))))
						}
						k, err := s.Write(buf[:max(n, 1)])
						sent[i] += int64(k)
						f.out.Add(int64(k))
						if err != nil {
							return
						}
					}
				}
			}
			// Uploads are read at upRate: unpaced, one moves GB/s over the
			// loopback, which drops the probe package's packets when go
			// test runs the packages side by side (macOS, issue #15).
			buf := make([]byte, 64<<10)
			t0 := time.Now()
			var got int64
			for {
				if ahead := time.Duration(float64(got)/upRate*float64(time.Second)) - time.Since(t0); ahead > 0 {
					time.Sleep(ahead)
				}
				k, err := s.Read(buf)
				got += int64(k)
				recv[i] += int64(k)
				f.in.Add(int64(k))
				if err != nil {
					return
				}
			}
		}()
	}
	var st [1]byte
	_, err = io.ReadFull(c, st[:])
	if f.mode == "deaf" { // sends on until the client cuts the streams
		wg.Wait()
		return
	}
	if f.mode == "slowend" { // a far server: TEST_END takes a while to act on
		time.Sleep(40 * time.Millisecond)
	}
	if err != nil || st[0] != stateTestEnd {
		if f.mode != "stall" {
			f.fail(errors.New("no TEST_END"))
		}
		close(stop)
		return
	}
	close(stop)
	for _, s := range streams {
		s.Close() // as iperf3 does on TEST_END
	}
	wg.Wait()
	c.Write([]byte{stateExchangeResults})
	var cr serverResults
	if err := readJSON(c, &cr); err != nil {
		f.fail(err)
		return
	}
	var total int64
	srv := serverResults{SenderHasRetransmits: -1}
	if reverse {
		srv.SenderHasRetransmits = 1
	}
	for i := range streams {
		total += recv[i]
		sr := streamResult{ID: streamID(i), Bytes: recv[i], Retransmits: -1, StartTime: 0, EndTime: 1.5}
		if reverse {
			sr.Retransmits = 7
		}
		srv.Streams = append(srv.Streams, sr)
	}
	f.mu.Lock()
	f.results, f.recv = cr, total
	for i := range streams {
		f.moved += recv[i] + sent[i]
	}
	f.mu.Unlock()
	writeJSON(c, srv)
	c.Write([]byte{stateDisplayResults})
	if _, err := io.ReadFull(c, st[:]); err != nil || st[0] != stateIperfDone {
		f.fail(errors.New("no IPERF_DONE"))
	}
}

// setupOverhead bounds a run's bytes besides its data: the control
// connection's and the stream cookies (counted in WireBytes).
const setupOverhead = 4096

func quick(addr string, d Direction) Config {
	return Config{Addr: addr, Direction: d, Streams: 3, Duration: 1500 * time.Millisecond, Omit: 500 * time.Millisecond}
}

func TestDownload(t *testing.T) {
	f := newFakeServer(t, "")
	r, err := Run(context.Background(), quick(f.addr(), Download))
	if err != nil {
		t.Fatal(err)
	}
	// 3 streams × 64 KiB per 10 ms = 19.7 MB/s; the ramp-up is left out.
	want := 3 * float64(64<<10) * 100 * 8
	if r.WireBytes <= r.Bytes { // the ramp-up is on the wire too
		t.Errorf("wire bytes %d, measured %d", r.WireBytes, r.Bytes)
	}
	// Wall-clock rates vary with CI load: a wide band, and the bytes
	// checked against what the server itself sent.
	if bps := r.BitsPerSecond(); bps < want*0.3 || bps > want*2 || r.Elapsed <= 0 || r.Elapsed > 2500*time.Millisecond {
		t.Errorf("%.1f Mbit/s (want ~%.1f) over %v", bps/1e6, want/1e6, r.Elapsed)
	}
	f.mu.Lock()
	sent := f.moved
	f.mu.Unlock()
	if r.Bytes <= 0 || r.WireBytes > sent+4096 { // (TCP_INFO may count a few bytes of framing)
		t.Errorf("measured %d, wire %d, the server sent %d", r.Bytes, r.WireBytes, sent)
	}
	if r.Retransmits != 21 {
		t.Errorf("retransmits %d, want the server's 3×7", r.Retransmits)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.params
	if p["reverse"] != true || p["tcp"] != true || p["parallel"] != 3.0 || p["time"] != 3.0 || p["omit"] != 0.0 || p["len"] != float64(blockSize) {
		t.Errorf("params %v", p)
	}
	if len(f.cookies) != 1 {
		t.Errorf("cookies %v", f.cookies)
	}
	// Our results: receiver, ids 1, 3, 4.
	if f.results.SenderHasRetransmits != -1 || len(f.results.Streams) != 3 || f.results.Streams[1].ID != 3 || f.results.Streams[2].ID != 4 {
		t.Errorf("client results %+v", f.results)
	}
}

func TestUpload(t *testing.T) {
	f := newFakeServer(t, "")
	r, err := Run(context.Background(), quick(f.addr(), Upload))
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.params["reverse"]; ok {
		t.Errorf("params %v", f.params)
	}
	// The bytes the server acknowledged in the window (Linux): at most what
	// it received, plus what its kernel acked but the fake never read
	// (it closes its streams on TEST_END, as iperf3 does): its receive
	// buffers, a few MB per stream.
	if r.Bytes == 0 || r.Bytes > f.recv+3*16<<20 || r.TotalBytes < r.Bytes || r.Elapsed <= 0 || r.Elapsed > 2500*time.Millisecond {
		t.Errorf("result %+v, server received %d", r, f.recv)
	}
	if f.results.SenderHasRetransmits < 0 {
		t.Errorf("upload results claim to be the receiver: %+v", f.results)
	}
}

func TestFailures(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		mode string
		want func(error) bool
	}{
		{"busy", func(err error) bool { return errors.Is(err, ErrBusy) }},
		{"close", func(err error) bool { return errors.Is(err, ErrBusy) }},
		{"error", func(err error) bool {
			var se *ServerError
			return errors.As(err, &se) && se.Code == 101 && se.Errno == 24
		}},
		{"terminate", func(err error) bool {
			var se *ServerError
			return errors.As(err, &se) && se.Code == 0
		}},
	} {
		f := newFakeServer(t, c.mode)
		_, err := Run(ctx, quick(f.addr(), Download))
		if !c.want(err) {
			t.Errorf("%s: %v", c.mode, err)
		}
	}
	if _, err := Run(ctx, Config{Addr: "192.0.2.1:5201", Timeout: 300 * time.Millisecond, Dial: (&net.Dialer{Timeout: 200 * time.Millisecond}).DialContext}); !errors.Is(err, ErrUnreachable) && !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("no answer: %v", err)
	}
}

// TestTimeout: a server that stops answering is cut off by the hard
// timeout, and the caller's ctx ends a test too.
func TestTimeout(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		readCookie(c)
		c.Write([]byte{stateParamExchange})
		time.Sleep(5 * time.Second) // never asks for streams
	}()
	cfg := quick(ln.Addr().String(), Upload)
	cfg.Timeout = 2500 * time.Millisecond
	t0 := time.Now()
	if _, err := Run(context.Background(), cfg); err == nil || time.Since(t0) > 4*time.Second {
		t.Fatalf("%v after %v", err, time.Since(t0))
	}
	f := newFakeServer(t, "stall")
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	cfg.Addr, cfg.Timeout = f.addr(), time.Minute
	if _, err := Run(ctx, cfg); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("ctx: %v", err)
	}
}

func TestCookie(t *testing.T) {
	var a, b [cookieSize]byte
	newCookie(&a)
	newCookie(&b)
	if a == b || a[cookieSize-1] != 0 {
		t.Fatalf("%q %q", a, b)
	}
	for _, ch := range a[:cookieSize-1] {
		if !bytes.ContainsRune([]byte("abcdefghijklmnopqrstuvwxyz234567"), rune(ch)) {
			t.Fatalf("cookie %q", a)
		}
	}
}

// TestAdaptive: a steady rate ends the test once 2 s are measured (2 s
// minimum here); a refused port is ErrRefused, not ErrUnreachable.
func TestAdaptive(t *testing.T) {
	f := newFakeServer(t, "")
	cfg := quick(f.addr(), Download)
	cfg.Duration, cfg.MinDuration = 6*time.Second, 2*time.Second
	t0 := time.Now()
	r, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if r.Elapsed < 1900*time.Millisecond || r.Elapsed > 5*time.Second || time.Since(t0) > 7*time.Second {
		t.Errorf("steady rate: measured %v in %v, want an early end before the 6 s limit", r.Elapsed, time.Since(t0))
	}
	want := 3 * float64(64<<10) * 100 * 8
	if bps := r.BitsPerSecond(); bps < want*0.3 || bps > want*2 {
		t.Errorf("%.1f Mbit/s, want ~%.1f", bps/1e6, want/1e6)
	}
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close()
	if _, err := Run(context.Background(), quick(addr, Download)); !errors.Is(err, ErrRefused) || errors.Is(err, ErrUnreachable) {
		t.Errorf("closed port: %v", err)
	}
}

// TestByteCap: a direction stops at MaxBytes, ramp-up included, and says
// so; the cookie carries the fyisp prefix.
func TestByteCap(t *testing.T) {
	f := newFakeServer(t, "")
	cfg := quick(f.addr(), Download)
	cfg.MaxBytes = 2 << 20 // 3 streams at ~6.5 MB/s each: ~100-200 ms
	t0 := time.Now()
	r, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	// It stops early enough that what keeps arriving stays within the cap.
	if !r.CappedByBytes || r.TotalBytes < cfg.MaxBytes/2 || r.WireBytes > cfg.MaxBytes+setupOverhead || time.Since(t0) > time.Second || r.Elapsed <= 0 {
		t.Errorf("capped %v, %d bytes in all, %d measured over %v, run %v", r.CappedByBytes, r.TotalBytes, r.Bytes, r.Elapsed, time.Since(t0))
	}
	f.mu.Lock()
	if !strings.HasPrefix(f.cookies[0], cookiePrefix) {
		t.Errorf("cookie %q", f.cookies[0])
	}
	f.mu.Unlock()
	// A cap above what the test moves changes nothing.
	cfg.MaxBytes = 1 << 40
	if r, _ := Run(context.Background(), cfg); r.CappedByBytes {
		t.Error("capped without reaching the cap")
	}
}

// TestAdaptiveRamp: the ramp-up's end is decided after the fact, where
// the rate levels off (here after ~500 ms of slow start), not after a
// fixed second, adaptive or not.
func TestAdaptiveRamp(t *testing.T) {
	f := newFakeServer(t, "ramp")
	cfg := quick(f.addr(), Download)
	cfg.Omit, cfg.Duration, cfg.MinDuration = time.Second, 3*time.Second, 2*time.Second
	r, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if r.Ramp < 400*time.Millisecond || r.Ramp > 900*time.Millisecond {
		t.Errorf("adaptive ramp %v, want ~500-700 ms", r.Ramp)
	}
	want := 3 * float64(64<<10) * 100 * 8
	if bps := r.BitsPerSecond(); bps < want*0.75 || bps > want*1.25 {
		t.Errorf("%.1f Mbit/s, want ~%.1f: the ramp leaked into the window", bps/1e6, want/1e6)
	}
	cfg.MinDuration, cfg.Duration = 0, 1500*time.Millisecond
	if r, _ := Run(context.Background(), cfg); r.Ramp < 400*time.Millisecond || r.Ramp > 900*time.Millisecond {
		t.Errorf("not adaptive: ramp %v", r.Ramp)
	}
}

// trackConn records its Close.
type trackConn struct {
	net.Conn
	closed *atomic.Int32
}

func (c trackConn) Close() error { c.closed.Add(1); return c.Conn.Close() }

// TestResolveOnce: a name is resolved once and every dial (control and
// streams) goes to that one IPv4 address; Check vets it before any dial.
func TestResolveOnce(t *testing.T) {
	f := newFakeServer(t, "")
	_, port, _ := net.SplitHostPort(f.addr())
	var lookups atomic.Int32
	var mu sync.Mutex
	var dialled []string
	cfg := quick("speed.example:"+port, Download)
	cfg.Lookup = func(context.Context, string) ([]netip.Addr, error) {
		lookups.Add(1)
		return []netip.Addr{netip.MustParseAddr("::1"), netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("127.0.0.2")}, nil
	}
	cfg.Dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		mu.Lock()
		dialled = append(dialled, network+" "+addr)
		mu.Unlock()
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}
	r, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if lookups.Load() != 1 || r.IP != netip.MustParseAddr("127.0.0.1") || len(dialled) != 4 {
		t.Fatalf("%d lookups, ip %v, dials %v", lookups.Load(), r.IP, dialled)
	}
	for _, d := range dialled {
		if d != "tcp4 127.0.0.1:"+port {
			t.Errorf("dialled %s", d)
		}
	}
	cfg.Check = func(a netip.Addr) error { return errors.New("not public") }
	dialled = nil
	if _, err := Run(context.Background(), cfg); !errors.Is(err, ErrUnreachable) || len(dialled) != 0 {
		t.Errorf("checked: %v, dials %v", err, dialled)
	}
	cfg.Lookup = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("::1")}, nil
	}
	if _, err := Run(context.Background(), cfg); !errors.Is(err, ErrUnreachable) {
		t.Errorf("IPv6 only: %v", err)
	}
}

// TestCancelDuringStreamDial: a stream dialled after the run was cancelled
// is closed, not leaked (M1), and so is every other socket.
func TestCancelDuringStreamDial(t *testing.T) {
	f := newFakeServer(t, "stall")
	ctx, cancel := context.WithCancel(context.Background())
	var n, closed atomic.Int32
	cfg := quick(f.addr(), Upload)
	cfg.Dial = func(dctx context.Context, network, addr string) (net.Conn, error) {
		c, err := (&net.Dialer{}).DialContext(context.Background(), network, addr)
		if err != nil {
			return nil, err
		}
		if n.Add(1) == 3 { // the second stream: cancelled while it dials
			cancel()
			time.Sleep(50 * time.Millisecond) // the AfterFunc closes the run first
		}
		return trackConn{c, &closed}, nil
	}
	if _, err := Run(ctx, cfg); !errors.Is(err, context.Canceled) {
		t.Fatalf("run: %v", err)
	}
	if n.Load() != 3 || closed.Load() < 3 {
		t.Errorf("%d dialled, %d closed", n.Load(), closed.Load())
	}
}

func TestAddressChecks(t *testing.T) {
	for a, want := range map[string][2]bool{ // public, private
		"8.8.8.8": {true, false}, "192.168.1.20": {false, true}, "10.1.2.3": {false, true}, "100.64.1.1": {false, true},
		"127.0.0.1": {false, true}, "169.254.1.1": {false, true}, "224.0.0.1": {false, false}, "0.0.0.0": {false, false},
		"0.1.2.3": {false, false}, "192.0.0.9": {false, false}, "192.0.2.1": {false, false}, "198.18.0.1": {false, false},
		"198.51.100.7": {false, false}, "203.0.113.5": {false, false}, "240.0.0.1": {false, false}, "1.1.1.1": {true, false},
	} {
		ip := netip.MustParseAddr(a)
		if PublicIPv4(ip) != want[0] || PrivateIPv4(ip) != want[1] {
			t.Errorf("%s: public %v private %v", a, PublicIPv4(ip), PrivateIPv4(ip))
		}
	}
	cat := Server{Host: "speed.example"}
	lan := Server{Host: "nas", Custom: true, LAN: true}
	far := Server{Host: "vps.example", Custom: true}
	pub, priv := netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("192.168.1.20")
	if cat.check(pub) != nil || cat.check(priv) == nil || lan.check(priv) != nil || lan.check(pub) == nil || far.check(pub) != nil || far.check(priv) != nil {
		t.Error("checks")
	}
	own, err := Custom("192.168.1.20:5201,8.8.8.8:5201", nil)
	if err != nil || !own[0].LAN || own[1].LAN || OwnOnly(own) || !OwnOnly(own[:1]) {
		t.Errorf("custom %+v %v", own, err)
	}
}

// TestByteCapInFlight: at a high rate with a server that keeps sending for
// a while after TEST_END (a far one), a download stops early enough that
// everything that arrives, read or still buffered, stays within the cap.
func TestByteCapInFlight(t *testing.T) {
	f := newFakeServer(t, "slowend")
	f.rate = 512 << 10 // 3 streams × 512 KiB per 10 ms: ~150 MB/s
	cfg := quick(f.addr(), Download)
	cfg.MaxBytes = 40 << 20
	r, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	// Within the cap as a rule; a starved CPU (the race detector on a busy
	// machine) can misjudge what's in flight, so the hard bound is the
	// backstop's.
	if !r.CappedByBytes || r.WireBytes > Backstop(cfg.MaxBytes)+setupOverhead || r.WireBytes < cfg.MaxBytes/3 {
		t.Errorf("capped %v: %d bytes arrived (cap %d), %d measured", r.CappedByBytes, r.WireBytes, cfg.MaxBytes, r.Bytes)
	}
	// Uploads never write past the cap, whatever the streams' interleaving.
	f2 := newFakeServer(t, "")
	cfg = quick(f2.addr(), Upload)
	cfg.MaxBytes = 3 << 20
	if r, err := Run(context.Background(), cfg); err != nil || !r.CappedByBytes || r.WireBytes > cfg.MaxBytes+setupOverhead {
		t.Errorf("upload: %v capped %v, %d written (cap %d)", err, r.CappedByBytes, r.WireBytes, cfg.MaxBytes)
	}
}

// TestUploadUnacked: without TCP_INFO acked bytes (not Linux), an upload
// counts what the server received, never more than we wrote.
func TestUploadUnacked(t *testing.T) {
	for _, c := range []struct{ got, want int64 }{
		{990, 890},     // 10 sat in our buffers
		{1000, 900},    // all arrived
		{5000, 900},    // a server claiming more changes nothing
		{-1, 0},        // nor one claiming less than nothing
		{1 << 62, 900}, // bounded
	} {
		tt := &test{c: Config{Direction: Upload}, counts: make([]atomic.Int64, 2)}
		tt.counts[0].Store(600)
		tt.counts[1].Store(400)
		res := Result{Bytes: 900}
		tt.applyServer(&serverResults{Streams: []streamResult{{Bytes: c.got / 2}, {Bytes: c.got - c.got/2}}}, &res)
		if res.Bytes != c.want {
			t.Errorf("server got %d: bytes %d, want %d", c.got, res.Bytes, c.want)
		}
		tt.acked = true
		res.Bytes = 900
		tt.applyServer(&serverResults{Streams: []streamResult{{Bytes: 10}}}, &res)
		if res.Bytes != 900 {
			t.Errorf("acked bytes changed: %d", res.Bytes)
		}
	}
}

// TestBytesHonest: a run's bytes are what moved on the wire (the kernel's
// counts where it has them, never added to the application's), whether it
// finishes, is cancelled mid-data, or during setup.
func TestBytesHonest(t *testing.T) {
	const slack = 1 << 20
	settle := func(f *fakeServer) { // the fake's stream goroutines notice the close
		for range 100 {
			if f.open.Load() == 0 {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	t.Run("download", func(t *testing.T) {
		f := newFakeServer(t, "")
		r, err := Run(context.Background(), quick(f.addr(), Download))
		if err != nil {
			t.Fatal(err)
		}
		settle(f)
		if out := f.out.Load(); r.WireBytes < out-slack || r.WireBytes > out+4096 {
			t.Errorf("wire %d, the server sent %d", r.WireBytes, out)
		}
	})
	t.Run("upload", func(t *testing.T) {
		f := newFakeServer(t, "")
		r, err := Run(context.Background(), quick(f.addr(), Upload))
		if err != nil {
			t.Fatal(err)
		}
		settle(f)
		if in := f.in.Load(); r.WireBytes < in || r.WireBytes > in+in/2 {
			t.Errorf("wire %d, the server received %d", r.WireBytes, in)
		}
	})
	t.Run("cancel mid-download", func(t *testing.T) {
		f := newFakeServer(t, "stall")
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			for f.out.Load() < 4<<20 {
				time.Sleep(5 * time.Millisecond)
			}
			cancel()
		}()
		c := quick(f.addr(), Download)
		c.Duration = 10 * time.Second
		r, err := Run(ctx, c)
		if err == nil {
			t.Fatal("not cancelled")
		}
		settle(f)
		if out := f.out.Load(); r.WireBytes < out-slack || r.WireBytes > out+4096 {
			t.Errorf("wire %d, the server sent %d", r.WireBytes, out)
		}
	})
	t.Run("cancel in setup", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		go func() { // accepts, reads the cookie, never answers
			c, err := ln.Accept()
			if err == nil {
				io.Copy(io.Discard, c)
				c.Close()
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		r, err := Run(ctx, quick(ln.Addr().String(), Download))
		if err == nil || r.WireBytes < cookieSize {
			t.Errorf("wire %d, err %v", r.WireBytes, err)
		}
	})
}

// TestBackstop: a server that ignores TEST_END and keeps sending is cut
// with an RST once the bytes received reach Backstop, at once rather than
// after the control connection's timeout; a complete window still counts.
func TestBackstop(t *testing.T) {
	f := newFakeServer(t, "deaf")
	f.rate = 256 << 10
	cfg := quick(f.addr(), Download)
	cfg.MaxBytes = 1 << 20 // small, so the receive buffers alone pass the backstop
	t0 := time.Now()
	r, err := Run(context.Background(), cfg)
	if err != nil || !r.Overrun || !r.CappedByBytes || r.Bytes <= 0 || time.Since(t0) > stepTimeout/2 {
		t.Fatalf("%v after %v: %+v", err, time.Since(t0), r)
	}
	if lim := Backstop(cfg.MaxBytes); r.WireBytes < lim || r.WireBytes > lim+2<<20 {
		t.Errorf("wire %d, backstop %d", r.WireBytes, lim)
	}
}

// TestTimeBackstop: without a byte cap, a server that ignores TEST_END is
// cut with an RST at the time backstop, not left to the control
// connection's timeout; the complete window still counts.
func TestTimeBackstop(t *testing.T) {
	f := newFakeServer(t, "deaf")
	cfg := quick(f.addr(), Download)
	cfg.Backstop = 2500 * time.Millisecond // the window ends at 2 s
	t0 := time.Now()
	r, err := Run(context.Background(), cfg)
	if el := time.Since(t0); err != nil || !r.Overrun || r.CappedByBytes || r.Elapsed <= 0 || el > stepTimeout || el < cfg.Backstop {
		t.Fatalf("%v after %v: %+v", err, el, r)
	}
}

// TestTimeCapped: an uncapped direction measured for its whole Duration
// is a lower bound when still rising (the same rule as a byte-capped
// one), a valid rate when level.
func TestTimeCapped(t *testing.T) {
	for _, c := range []struct {
		mode string
		lb   bool
	}{{"", false}, {"climb", true}} {
		f := newFakeServer(t, c.mode)
		r, err := Run(context.Background(), quick(f.addr(), Download))
		if err != nil || r.CappedByBytes || r.RateMethod != MethodPeak90 || r.LowerBound != c.lb {
			t.Errorf("%q: %v %+v", c.mode, err, r)
		}
	}
}

// TestSteadyLateTick: a tick read late on a busy machine moves bytes from
// one second to the next; at a level rate that is still steady.
func TestSteadyLateTick(t *testing.T) {
	const rate = 1e6 // bytes/s
	tt := &test{c: Config{MinDuration: time.Second, Stable: 0.05}}
	var cum []int64
	var at []time.Duration
	for i := range 30 {
		a := time.Duration(i+1) * tick
		if i == 19 { // the tick between the last two seconds, 60 ms late
			a += 60 * time.Millisecond
		}
		cum, at = append(cum, int64(rate*a.Seconds())), append(at, a)
	}
	if !tt.steady(cum, at, false) {
		t.Error("a level rate with a late tick is not steady")
	}
	for i := 20; i < 30; i++ { // the last second 10% faster
		cum[i] += int64(float64(i-19) * rate * tick.Seconds() / 10)
	}
	if tt.steady(cum, at, false) {
		t.Error("a 10% faster last second is steady")
	}
}

// TestEarlyStopWaitsForPlateau: the adaptive early stop needs the ticks to
// have levelled off and MinDuration since the first tick near the peak: a
// slow ramp (a far path's slow start, 2.5 s) runs on past it, and the rate
// comes from the plateau; a level near server stops after 3 to 5 s.
func TestEarlyStopWaitsForPlateau(t *testing.T) {
	for _, c := range []struct {
		mode     string
		min, max time.Duration
	}{{"slowramp", 5 * time.Second, 9 * time.Second}, {"", 3 * time.Second, 5500 * time.Millisecond}} {
		f := newFakeServer(t, c.mode)
		cfg := quick(f.addr(), Download)
		cfg.Duration, cfg.MinDuration, cfg.Omit = 10*time.Second, 3*time.Second, time.Second
		t0 := time.Now()
		r, err := Run(context.Background(), cfg)
		el := time.Since(t0)
		full := float64(3*f.rate) * 100 // bytes/s: 3 streams, f.rate per 10 ms
		if err != nil || r.LowerBound || el < c.min || el > c.max || math.Abs(float64(r.Bytes)/r.Elapsed.Seconds()/full-1) > 0.15 {
			t.Errorf("%q: %v after %v: %+v (%.3g of %.3g B/s)", c.mode, err, el, r, float64(r.Bytes)/r.Elapsed.Seconds(), full)
		}
	}
}
