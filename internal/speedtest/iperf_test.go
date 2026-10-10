package speedtest

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeServer is an in-process iperf3 server: the server side of the control
// protocol, as iperf_server_api.c runs it. mode picks a misbehaviour.
type fakeServer struct {
	ln   net.Listener
	mode string // "", "busy", "close", "error", "terminate", "stall"
	rate int    // bytes per 10 ms per stream when sending (downloads)

	mu      sync.Mutex
	params  map[string]any
	results serverResults
	cookies []string // control cookie, then each stream's
	recv    int64
	err     error
	wg      sync.WaitGroup
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

func (f *fakeServer) addr() string { return f.ln.Addr().String() }

func (f *fakeServer) fail(err error) {
	if f.mode == "stall" {
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
		c, err := f.ln.Accept()
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
		s, err := f.ln.Accept()
		if err != nil {
			f.fail(err)
			return
		}
		ck, err := readCookie(s)
		if err != nil || ck != cookie {
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
	var recv [16]int64
	for i, s := range streams {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if reverse {
				buf := make([]byte, f.rate)
				tk := time.NewTicker(10 * time.Millisecond)
				defer tk.Stop()
				t0 := time.Now()
				for {
					select {
					case <-stop:
						return
					case <-tk.C:
						n := len(buf)
						if f.mode == "ramp" { // slow start: full rate after 500 ms
							n = int(float64(n) * min(1, float64(time.Since(t0))/float64(500*time.Millisecond)))
						}
						if _, err := s.Write(buf[:max(n, 1)]); err != nil {
							return
						}
					}
				}
			}
			buf := make([]byte, 64<<10)
			for {
				k, err := s.Read(buf)
				recv[i] += int64(k)
				if err != nil {
					return
				}
			}
		}()
	}
	var st [1]byte
	if _, err := io.ReadFull(c, st[:]); err != nil || st[0] != stateTestEnd {
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
	f.mu.Unlock()
	writeJSON(c, srv)
	c.Write([]byte{stateDisplayResults})
	if _, err := io.ReadFull(c, st[:]); err != nil || st[0] != stateIperfDone {
		f.fail(errors.New("no IPERF_DONE"))
	}
}

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
	if bps := r.BitsPerSecond(); bps < want*0.7 || bps > want*1.3 || r.Elapsed < 1400*time.Millisecond || r.Elapsed > 1700*time.Millisecond {
		t.Errorf("%.1f Mbit/s (want ~%.1f) over %v", bps/1e6, want/1e6, r.Elapsed)
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
	// The bytes the server acknowledged in the window (Linux), at most what
	// it received in all.
	if r.Bytes == 0 || r.Bytes > f.recv || r.TotalBytes < r.Bytes || r.Elapsed < 1400*time.Millisecond || r.Elapsed > 1700*time.Millisecond {
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
	if r.Elapsed < 1900*time.Millisecond || r.Elapsed > 3500*time.Millisecond || time.Since(t0) > 5*time.Second {
		t.Errorf("steady rate: measured %v in %v, want an early end at ~2-3 s", r.Elapsed, time.Since(t0))
	}
	want := 3 * float64(64<<10) * 100 * 8
	if bps := r.BitsPerSecond(); bps < want*0.7 || bps > want*1.3 {
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
	if !r.CappedByBytes || r.TotalBytes < cfg.MaxBytes || r.TotalBytes > cfg.MaxBytes+4<<20 || time.Since(t0) > time.Second || r.Elapsed <= 0 {
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

// TestAdaptiveRamp: the ramp-up ends when a 100 ms tick stops rising (here
// after ~500 ms of slow start), not after a fixed second; without
// MinDuration it is Omit.
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
	if r, _ := Run(context.Background(), cfg); r.Ramp < time.Second || r.Ramp > 1200*time.Millisecond {
		t.Errorf("fixed ramp %v", r.Ramp)
	}
}
