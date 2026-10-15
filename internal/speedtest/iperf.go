// Package speedtest runs scheduled throughput tests against the public
// iperf3 servers of internal/profile/catalog/iperf3 (and the user's own
// servers), with a pure-Go iperf3 client that speaks the stock iperf3 3.x
// control protocol.
package speedtest

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/netip"
	"os"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// The iperf3 control protocol (esnet/iperf iperf_api.c, iperf_client_api.c,
// iperf_server_api.c):
//
//	client                                  server
//	connect control, send cookie (37 B) ->
//	                                     <- PARAM_EXCHANGE, or ACCESS_DENIED when busy
//	params: 4-byte BE length + JSON     ->
//	                                     <- CREATE_STREAMS
//	connect N data streams, each sends the cookie
//	                                     <- TEST_START, TEST_RUNNING
//	data for omit+time seconds (reverse: the server sends)
//	TEST_END                            ->  (the server closes the data streams)
//	                                     <- EXCHANGE_RESULTS
//	client results JSON                 ->
//	                                     <- server results JSON
//	                                     <- DISPLAY_RESULTS
//	IPERF_DONE                          ->
//
// States are single signed bytes; SERVER_ERROR is followed by two 4-byte
// BE ints (iperf error, errno).
const (
	stateTestStart       = 1
	stateTestRunning     = 2
	stateTestEnd         = 4
	stateParamExchange   = 9
	stateCreateStreams   = 10
	stateServerTerminate = 11
	stateExchangeResults = 13
	stateDisplayResults  = 14
	stateIperfDone       = 16
	stateAccessDenied    = -1
	stateServerError     = -2

	cookieSize  = 37 // 36 characters and a NUL
	blockSize   = 128 << 10
	maxJSONSize = 1 << 20
)

// Direction is what a test measures.
type Direction int

const (
	Download Direction = iota // the server sends (iperf3 --reverse)
	Upload
)

func (d Direction) String() string {
	if d == Upload {
		return "upload"
	}
	return "download"
}

// Errors of one test. A busy server is never a slow result.
var (
	// ErrBusy: the server is running another test (ACCESS_DENIED, or it
	// closed the control connection before the parameter exchange).
	ErrBusy = errors.New("iperf3 server busy")
	// ErrUnreachable wraps a failed control connection (timed out, no
	// route, DNS): retrying other ports of the server does not help.
	ErrUnreachable = errors.New("iperf3 server unreachable")
	// ErrRefused wraps a refused control connection: nothing listens on
	// that port right now. A stock iperf3 server closes its listener for a
	// moment after every test, so another port (or a retry) may work.
	ErrRefused = errors.New("iperf3 connection refused")
	// ErrOverrun: the run reached the backstop (the server ignored
	// TEST_END or kept sending) before its window was complete, and was
	// cut with an RST.
	ErrOverrun = errors.New("iperf3 server kept sending past the backstop")
)

// ErrCancelledByOwner is the cause (context.WithCancelCause) of the owner's
// cancel, so a cancelled test's row says who stopped it.
var ErrCancelledByOwner = errors.New("cancelled by the owner")

// ServerError is an iperf3 SERVER_ERROR or SERVER_TERMINATE.
type ServerError struct {
	Code, Errno int32 // iperf3 i_errno and errno; 0 for SERVER_TERMINATE
}

func (e *ServerError) Error() string {
	if e.Code == 0 && e.Errno == 0 {
		return "iperf3 server terminated the test"
	}
	return fmt.Sprintf("iperf3 server error %d (errno %d)", e.Code, e.Errno)
}

// Config is one test run.
type Config struct {
	Addr      string // host:port: an IPv4 address or a name, resolved once per run
	Direction Direction
	Streams   int           // parallel TCP streams, default 8
	Duration  time.Duration // measured time, default 9s; with MinDuration, the most
	// Omit is the ramp-up excluded from the result, default 1s. With
	// MinDuration it is the longest ramp-up: the ramp ends as soon as the
	// throughput of a 100 ms tick stops rising.
	Omit time.Duration
	// MinDuration > 0 makes the length adaptive: the test ends once at
	// least MinDuration is measured and the last second's rate is within
	// Stable (default 5%) of the second before's, else after Duration. The
	// client ends it early with TEST_END, which stock servers accept.
	MinDuration time.Duration
	Stable      float64
	// Backstop is when, from the data phase's start, the streams are cut
	// with an RST whatever the server does (a server that ignores TEST_END
	// or keeps sending), default Omit+Duration+2s. A complete window
	// still counts (Result.Overrun).
	Backstop time.Duration
	// Timeout bounds the whole run, default Omit+Duration+7s. The caller's
	// ctx may end it sooner.
	Timeout time.Duration
	// Dial opens TCP connections (tests: a stub). Default net.Dialer.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// Lookup resolves a name to IPv4 addresses (default net.DefaultResolver).
	// The run resolves once and dials that one address for the control
	// connection and every stream: they must all reach one server.
	Lookup func(ctx context.Context, host string) ([]netip.Addr, error)
	// Log takes the FYISP_TICKS log (nil: slog's default).
	Log *slog.Logger
	// Check, when set, vets the address before any dial (catalog servers
	// must be public, the user's LAN servers private).
	Check func(netip.Addr) error
}

// Result is one direction's measurement.
type Result struct {
	Direction Direction
	Bytes     int64 // bytes received over the measured window (all streams)
	// WireBytes is every byte of the run on the wire: the control
	// connection's and, per stream, the larger of what the application and
	// the kernel (TCP_INFO at close, Linux) counted, ramp-up and the time
	// after the window included (set even when the test fails or is
	// cancelled): what a data budget must count.
	WireBytes int64
	Elapsed   time.Duration // the measured window, without the omitted ramp-up
	// Retransmits summed over the streams, -1 when unknown (the sender's
	// TCP_INFO: the server's for downloads, ours on Linux for uploads).
	Retransmits int64
	Streams     int
	Ramp        time.Duration // the ramp-up left out
	IP          netip.Addr    // the address the test ran against
	TotalBytes  int64         // every byte moved, ramp-up included (data use)
	// RateMethod is how the rate was taken: MethodPeak90 or MethodWhole.
	// LowerBound: it was still speeding up at the time cap, so the speed
	// was at least this.
	RateMethod string
	LowerBound bool
	// Overrun: the server kept sending past the time cap and the run was
	// cut at the backstop (after a complete window; else the run fails with
	// ErrOverrun).
	Overrun bool
}

// BitsPerSecond is the throughput.
func (r Result) BitsPerSecond() float64 {
	if r.Elapsed <= 0 {
		return 0
	}
	return float64(r.Bytes) * 8 / r.Elapsed.Seconds()
}

func (c *Config) defaults() {
	if c.Streams <= 0 {
		c.Streams = 8
	}
	if c.Duration <= 0 {
		c.Duration = 9 * time.Second
	}
	if c.Omit < 0 {
		c.Omit = 0
	} else if c.Omit == 0 {
		c.Omit = time.Second
	}
	if c.MinDuration > 0 && c.Stable <= 0 {
		c.Stable = 0.05
	}
	if c.Backstop <= 0 {
		c.Backstop = c.Omit + c.Duration + 2*time.Second
	}
	if c.Timeout <= 0 {
		c.Timeout = c.Omit + c.Duration + 7*time.Second
	}
	if c.Dial == nil {
		d := &net.Dialer{Timeout: 5 * time.Second}
		c.Dial = d.DialContext
	}
	if c.Lookup == nil {
		c.Lookup = func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip4", host)
		}
	}
}

// resolve picks the one IPv4 address of c.Addr's host the run uses, and
// returns ip:port. A failed lookup is ErrUnreachable.
func (c *Config) resolve(ctx context.Context) (netip.AddrPort, error) {
	host, port, err := net.SplitHostPort(c.Addr)
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("%w: %w", ErrUnreachable, err)
	}
	p, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("%w: bad port %q", ErrUnreachable, port)
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		ips, lerr := c.Lookup(ctx, host)
		for _, a := range ips {
			if a = a.Unmap(); a.Is4() {
				ip = a
				break
			}
		}
		if !ip.IsValid() {
			if lerr == nil {
				lerr = errors.New("no IPv4 address")
			}
			return netip.AddrPort{}, fmt.Errorf("%w: %s: %w", ErrUnreachable, host, lerr)
		}
	}
	if ip = ip.Unmap(); !ip.Is4() {
		return netip.AddrPort{}, fmt.Errorf("%w: %s is not IPv4", ErrUnreachable, ip)
	}
	if c.Check != nil {
		if err := c.Check(ip); err != nil {
			return netip.AddrPort{}, fmt.Errorf("%w: %w", ErrUnreachable, err)
		}
	}
	return netip.AddrPortFrom(ip, uint16(p)), nil
}

// stepTimeout bounds every control-channel step outside the data phase.
const stepTimeout = 5 * time.Second

// Run runs one iperf3 test. It returns ErrBusy, an error wrapping
// ErrRefused or ErrUnreachable, a *ServerError, ctx's error, or another
// protocol error.
func Run(ctx context.Context, c Config) (Result, error) {
	c.defaults()
	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	res := Result{Direction: c.Direction, Retransmits: -1, Streams: c.Streams}

	ap, err := c.resolve(ctx)
	if err != nil {
		return res, err
	}
	res.IP = ap.Addr()
	c.Addr = ap.String() // IPv4 only, by decision; one address for every dial
	t0 := time.Now()
	ctrl, err := c.Dial(ctx, "tcp4", c.Addr)
	dialRTT := time.Since(t0)
	if err != nil {
		if ctx.Err() != nil && errors.Is(err, context.Canceled) {
			return res, ctx.Err()
		}
		if isRefused(err) {
			return res, fmt.Errorf("%w: %w", ErrRefused, err)
		}
		return res, fmt.Errorf("%w: %w", ErrUnreachable, err)
	}
	t := &test{c: c, dialRTT: dialRTT}
	t.ctrl = countConn{ctrl, &t.setup}
	defer t.close()
	stop := context.AfterFunc(ctx, t.abort) // unblocks every read and write
	defer stop()

	r, err := t.run(ctx, &res)
	if err != nil && ctx.Err() == nil && t.measured.Load() {
		// The client ended the direction, but the server didn't end the test
		// (it ignored TEST_END, or its results never came): the window is
		// the client's own, so it stands; the streams are cut.
		t.overrun.Store(true)
	}
	if t.overrun.Load() {
		// The measurement stands when the window was complete (only the
		// server's retransmits are lost); else the run failed.
		r, err = res, nil
		r.Overrun = true
		if res.Elapsed <= 0 {
			err = ErrOverrun
		}
	}
	if err != nil || r.Overrun {
		t.abort() // nothing more to send: drop what is queued
	}
	t.close() // the streams stop: their counters are final
	t.wg.Wait()
	if ctx.Err() != nil {
		res.WireBytes = t.wire()
		return res, ctx.Err()
	}
	r.WireBytes = t.wire()
	return r, err
}

// wire is the run's bytes so far: the control connection's and the
// stream cookies, plus each stream's larger count, the application's or
// the kernel's (read at close; never their sum).
func (t *test) wire() int64 {
	t.mu.Lock()
	counts, kern := t.counts, t.kern
	t.mu.Unlock()
	n := t.setup.Load()
	for i := range max(len(counts), len(kern)) {
		var app, k int64
		if i < len(counts) {
			app = counts[i].Load()
		}
		if i < len(kern) {
			k = kern[i]
		}
		n += max(app, k)
	}
	return n
}

// kernelBytes is a stream's bytes as TCP counts them (Linux; -1
// elsewhere): received for a download (read or still buffered), acked for
// an upload.
func (t *test) kernelBytes(s net.Conn) int64 {
	if t.c.Direction == Download {
		return tcpReceived(s)
	}
	_, acked := tcpInfo(s)
	return acked
}

// countConn counts a connection's bytes both ways.
type countConn struct {
	net.Conn
	n *atomic.Int64
}

func (c countConn) Read(b []byte) (int, error) {
	k, err := c.Conn.Read(b)
	c.n.Add(int64(k))
	return k, err
}

func (c countConn) Write(b []byte) (int, error) {
	k, err := c.Conn.Write(b)
	c.n.Add(int64(k))
	return k, err
}

type test struct {
	c      Config
	ctrl   net.Conn
	cookie [cookieSize]byte

	mu      sync.Mutex
	streams []net.Conn
	closed  bool
	wg      sync.WaitGroup // the data phase's stream goroutines

	counts   []atomic.Int64 // per stream, the whole data phase (mu: the slice)
	kern     []int64        // per stream, kernelBytes at close (mu)
	setup    atomic.Int64   // the control connection's bytes and the stream cookies
	acked    bool           // uploads: Result.Bytes is TCP_INFO acked bytes
	dialRTT  time.Duration  // the control connection's connect time: the RTT without TCP_INFO
	measured atomic.Bool    // the data phase ended with a complete window
	overrun  atomic.Bool    // the backstop cut the run
}

func (t *test) close() { t.shut(false) }

// shut closes the run once; rst: the data streams with SO_LINGER 0.
func (t *test) shut(rst bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return
	}
	t.closed = true
	// The kernel's counts first: closing ends them, and an RST drops what
	// is still queued.
	t.kern = make([]int64, len(t.streams))
	for i, s := range t.streams {
		t.kern[i] = t.kernelBytes(s)
	}
	if rst {
		for _, s := range t.streams {
			if tc, ok := s.(*net.TCPConn); ok {
				_ = tc.SetLinger(0)
			}
		}
	}
	_ = t.ctrl.Close()
	for _, s := range t.streams {
		_ = s.Close()
	}
}

// abort closes the run on cancel, timeout or error (close reads the
// kernel's counts before anything changes): the data streams with
// SO_LINGER 0, so upload data still queued in the kernel is dropped (an RST)
// rather than sent on after the test's window. The happy path closes
// gracefully (a stock server reading an RST mid-test logs an error).
func (t *test) abort() { t.shut(true) }

func (t *test) run(ctx context.Context, res *Result) (Result, error) {
	if err := newCookie(&t.cookie); err != nil {
		return *res, err
	}
	if err := t.write(t.cookie[:]); err != nil {
		return *res, err
	}
	st, err := t.state(stepTimeout)
	if err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || isReset(err) {
			return *res, ErrBusy // closed before the exchange: another test runs
		}
		return *res, err
	}
	if err := expect(st, stateParamExchange); err != nil {
		return *res, err
	}
	if err := t.writeJSON(t.params()); err != nil {
		return *res, err
	}
	if st, err = t.state(stepTimeout); err != nil {
		return *res, err
	}
	if err := expect(st, stateCreateStreams); err != nil {
		return *res, err
	}
	// Streams connect one at a time, in order: the server numbers them in
	// accept order (1, 3, 4, ...) and so do we.
	for range t.c.Streams {
		s, err := t.c.Dial(ctx, "tcp4", t.c.Addr)
		if err != nil {
			return *res, fmt.Errorf("data stream: %w", err)
		}
		t.mu.Lock()
		closed := t.closed
		if !closed {
			t.streams = append(t.streams, s)
		}
		t.mu.Unlock()
		if closed { // dialled after the run was closed: close() won't see it
			_ = s.Close()
			return *res, ctx.Err()
		}
		_ = s.SetWriteDeadline(time.Now().Add(stepTimeout))
		k, err := s.Write(t.cookie[:])
		t.setup.Add(int64(k))
		if err != nil {
			return *res, fmt.Errorf("data stream: %w", err)
		}
		_ = s.SetWriteDeadline(time.Time{})
	}
	for _, want := range []int8{stateTestStart, stateTestRunning} {
		if st, err = t.state(stepTimeout); err != nil {
			return *res, err
		}
		if err := expect(st, want); err != nil {
			return *res, err
		}
	}

	// The data phase. Counters are per stream; the window starts after the
	// omitted ramp-up.
	counts := make([]atomic.Int64, len(t.streams))
	t.mu.Lock()
	t.counts = counts
	t.mu.Unlock()
	var done atomic.Bool
	for i, s := range t.streams {
		t.wg.Add(1)
		go func() {
			defer t.wg.Done()
			if t.c.Direction == Download {
				t.drain(s, &counts[i])
			} else {
				t.fill(s, &counts[i], &done)
			}
		}()
	}
	cut := time.AfterFunc(t.c.Backstop, func() { // the time backstop
		t.overrun.Store(true)
		t.abort()
	})
	defer cut.Stop()
	per, err := t.dataPhase(counts, res)
	done.Store(true)
	if err != nil {
		return *res, err
	}
	t.measured.Store(true)

	if err := t.write([]byte{stateTestEnd}); err != nil {
		return *res, err
	}
	if st, err = t.state(stepTimeout); err != nil {
		return *res, err
	}
	if err := expect(st, stateExchangeResults); err != nil {
		return *res, err
	}
	if err := t.writeJSON(t.results(per, res)); err != nil {
		return *res, err
	}
	var srv serverResults
	if err := t.readJSON(&srv); err != nil {
		return *res, fmt.Errorf("server results: %w", err)
	}
	t.applyServer(&srv, res)
	if st, err = t.state(stepTimeout); err != nil {
		return *res, err
	}
	if err := expect(st, stateDisplayResults); err != nil {
		return *res, err
	}
	_ = t.write([]byte{stateIperfDone})
	t.close()
	t.wg.Wait()
	return *res, nil
}

// idle waits until deadline on the control connection. The server sends
// nothing during the data phase unless it fails.
func (t *test) idle(deadline time.Time) error {
	for time.Now().Before(deadline) {
		_ = t.ctrl.SetReadDeadline(deadline)
		var b [1]byte
		_, err := t.ctrl.Read(b[:])
		if err == nil {
			return t.stateErr(int8(b[0]))
		}
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			continue
		}
		return fmt.Errorf("control connection: %w", err)
	}
	return nil
}

// tick is the data phase's clock: progress is read every 100 ms.
const tick = 100 * time.Millisecond

// dataPhase runs the data phase on 100 ms ticks until the time cap
// (Omit + Duration) or, when adaptive, a plateau-aware early
// stop, then fills res from the one estimator (peak90 over the
// timestamped ticks: the ramp-up's end is decided after the fact, the
// same way whatever ended the run). per is each stream's bytes (for our
// results).
func (t *test) dataPhase(counts []atomic.Int64, res *Result) ([]int64, error) {
	n := len(t.streams)
	total := func() int64 {
		var x int64
		for i := range counts {
			x += counts[i].Load()
		}
		return x
	}
	adaptive := t.c.MinDuration > 0
	start := time.Now()
	retransStart := make([]int64, n)
	ackedStart := make([]int64, n)
	t.snapshot(counts, make([]int64, n), retransStart, ackedStart)
	// cum is the bytes counted at each tick and ats when it was read:
	// received for downloads, acked for uploads (Linux; else written).
	var cum []int64
	var ats []time.Duration
	ackedSum := func() int64 {
		var x int64
		for i := range n {
			_, a := t.tcpCounters(i)
			if a < 0 {
				return -1
			}
			x += a
		}
		return x
	}
	acked := t.c.Direction == Upload && ackedSum() >= 0
	timeCapped := false // the run reached Omit + Duration
	for k := 1; ; k++ {
		if err := t.idle(start.Add(time.Duration(k) * tick)); err != nil {
			return nil, err
		}
		now := time.Since(start) // ticks are read late on a busy machine: rates use the actual time
		b := total()
		c := b
		if acked {
			if c = ackedSum(); c < 0 {
				acked, c = false, b
			}
		}
		cum, ats = append(cum, c), append(ats, now)
		if time.Duration(k)*tick >= t.c.Omit+t.c.Duration {
			timeCapped = true
			break
		}
		if adaptive && k%int(time.Second/tick) == 0 && t.steady(cum, ats, t.c.Direction == Upload && !acked) {
			break
		}
	}
	final := make([]int64, n)
	retransEnd := make([]int64, n)
	ackedEnd := make([]int64, n)
	t.snapshot(counts, final, retransEnd, ackedEnd)
	per := make([]int64, n)
	allAcked := true
	var ackedBytes int64
	for i := range per {
		per[i] = final[i]
		res.TotalBytes += final[i]
		if ackedStart[i] < 0 || ackedEnd[i] < 0 {
			allAcked = false
		}
		ackedBytes += ackedEnd[i] - ackedStart[i]
	}
	if t.c.Direction == Upload {
		if allAcked {
			t.acked = true // the bytes below are what the server acknowledged
		}
		if retransStart[0] >= 0 {
			res.Retransmits = 0
			for i := range retransEnd {
				res.Retransmits += max(retransEnd[i]-retransStart[i], 0)
			}
		}
	}
	// The estimate: over the ticks, to the last byte.
	end, endBytes := ats[len(ats)-1], cum[len(cum)-1]
	from := 0
	if t.c.Direction == Upload && !acked {
		from = 1 // written bytes: the first tick fills the send buffers
	}
	e := peak90From(from, cum, ats, end, endBytes, t.pathRTT())
	res.RateMethod = e.method
	if e.method == MethodPeak90 {
		res.Bytes, res.Elapsed, res.Ramp = e.bytes, e.win, e.start
		res.LowerBound = timeCapped && e.lowerBound // an early stop has levelled off
	} else { // no tick with bytes: the whole run
		res.Bytes, res.Elapsed = endBytes, end
		if t.c.Direction == Upload && allAcked {
			res.Bytes = ackedBytes
		}
		res.LowerBound = timeCapped
	}
	res.clampTotal()
	if os.Getenv("FYISP_TICKS") != "" { // for checking the estimator on a real line
		ms := make([]float64, len(ats))
		for i, a := range ats {
			ms[i] = float64(a.Microseconds()) / 1000
		}
		cmp.Or(t.c.Log, slog.Default()).Info("speed test ticks", "direction", t.c.Direction.String(), "at_ms", ms, "bytes", cum,
			"rtt_ms", float64(t.pathRTT().Microseconds())/1000, "method", e.method, "lower_bound", res.LowerBound,
			"time_capped", timeCapped)
	}
	return per, nil
}

// clampTotal keeps TotalBytes at least Bytes: an upload's acked bytes can
// run ahead of the written count (the kernel acks part of a block before
// its Write returns), and every byte measured was moved.
func (r *Result) clampTotal() { r.TotalBytes = max(r.TotalBytes, r.Bytes) }

// steady is the plateau-aware early stop, at a 1 s boundary: the ticks so
// far have levelled off (stillRising says no, over the idle RTT), at
// least MinDuration has passed since the first smoothed tick at 90% of the
// peak, and the last second is within Stable of the one before.
// written: the bytes are written, not acked, so the first tick (filling
// the send buffers) neither counts toward the plateau nor starts its clock.
func (t *test) steady(cum []int64, at []time.Duration, written bool) bool {
	k := len(cum)
	per := int(time.Second / tick)
	if k < 2*per+1 {
		return false
	}
	r := make([]float64, k)
	var prev int64
	var prevAt time.Duration
	for i := range cum {
		if dt := at[i] - prevAt; dt > 0 {
			r[i] = float64(cum[i]-prev) / dt.Seconds()
		}
		prev, prevAt = cum[i], at[i]
	}
	if stillRising(r, t.pathRTT()) {
		return false
	}
	skip := 0
	if written {
		skip = 1
	}
	sm := median3(r)[skip:]
	p := slices.Max(sm)
	f := skip + slices.IndexFunc(sm, func(v float64) bool { return v >= 0.9*p })
	var from time.Duration
	if f > 0 {
		from = at[f-1]
	}
	if at[k-1]-from < t.c.MinDuration {
		return false
	}
	// Rates over the ticks' actual times: a tick read late on a busy
	// machine moves bytes from one second to the next. A "second" squeezed
	// under half a second (ticks read back to back after a stall) is too
	// short to judge.
	span := func(i, j int) float64 { return (at[j] - at[i]).Seconds() }
	if span(k-1-per, k-1) < 0.5 || span(k-1-2*per, k-1-per) < 0.5 {
		return false
	}
	cur := float64(cum[k-1]-cum[k-1-per]) / span(k-1-per, k-1)
	prevSec := float64(cum[k-1-per]-cum[k-1-2*per]) / span(k-1-2*per, k-1-per)
	return prevSec > 0 && math.Abs(cur-prevSec) <= t.c.Stable*prevSec
}

// pathRTT is the path's idle RTT, for the estimator's smoothing: a
// loaded srtt includes queueing (a bloated line's 100-300 ms), and a
// standing queue paces packets rather than bunching them per RTT.
func (t *test) pathRTT() time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	var mins []time.Duration
	for _, s := range t.streams {
		mins = append(mins, tcpMinRTT(s))
	}
	return idleRTT(t.dialRTT, mins)
}

// idleRTT is the streams' smallest min_rtt (TCP_INFO), else the control
// connection's connect time.
func idleRTT(dial time.Duration, minRTT []time.Duration) time.Duration {
	var r time.Duration
	for _, x := range minRTT {
		if x > 0 && (r == 0 || x < r) {
			r = x
		}
	}
	if r == 0 {
		r = max(dial, 0)
	}
	return r
}

// tcpCounters is stream i's retransmits and bytes acked so far (upload),
// -1 when unknown.
func (t *test) tcpCounters(i int) (retrans, acked int64) {
	if t.c.Direction != Upload {
		return -1, -1
	}
	return tcpInfo(t.streams[i])
}

func (t *test) snapshot(counts []atomic.Int64, into, retrans, acked []int64) {
	for i := range counts {
		into[i] = counts[i].Load()
		retrans[i], acked[i] = t.tcpCounters(i)
	}
}

// drain reads a download stream until the server closes it.
func (t *test) drain(s net.Conn, n *atomic.Int64) {
	buf := make([]byte, blockSize)
	for {
		k, err := s.Read(buf)
		n.Add(int64(k))
		if err != nil {
			return
		}
	}
}

// payload is what uploads send: random bytes, as iperf3 does by default
// (compressible data would overstate a compressing link).
var payload = func() []byte {
	b := make([]byte, blockSize)
	_, _ = rand.Read(b)
	return b
}()

// fill writes an upload stream until done.
func (t *test) fill(s net.Conn, n *atomic.Int64, done *atomic.Bool) {
	for !done.Load() {
		_ = s.SetWriteDeadline(time.Now().Add(250 * time.Millisecond))
		k, err := s.Write(payload)
		n.Add(int64(k))
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return
		}
	}
}

func (t *test) params() map[string]any {
	p := map[string]any{
		"tcp": true,
		// The client leaves the ramp-up out itself (it may be shorter than a
		// second) and ends the test with TEST_END; time only bounds it.
		"omit":           0,
		"time":           int((t.c.Omit+t.c.Duration+time.Second-1)/time.Second) + 1,
		"num":            0,
		"blockcount":     0,
		"parallel":       t.c.Streams,
		"len":            blockSize,
		"pacing_timer":   1000,
		"client_version": "3.17.1",
	}
	if t.c.Direction == Download {
		p["reverse"] = true
	}
	return p
}

type streamResult struct {
	ID             int     `json:"id"`
	Bytes          int64   `json:"bytes"`
	Retransmits    int64   `json:"retransmits"`
	Jitter         float64 `json:"jitter"`
	Errors         int64   `json:"errors"`
	OmittedErrors  int64   `json:"omitted_errors"`
	Packets        int64   `json:"packets"`
	OmittedPackets int64   `json:"omitted_packets"`
	StartTime      float64 `json:"start_time"`
	EndTime        float64 `json:"end_time"`
}

type serverResults struct {
	CPUUtilTotal         float64        `json:"cpu_util_total"`
	CPUUtilUser          float64        `json:"cpu_util_user"`
	CPUUtilSystem        float64        `json:"cpu_util_system"`
	SenderHasRetransmits int            `json:"sender_has_retransmits"`
	Streams              []streamResult `json:"streams"`
}

// streamID is iperf3's numbering: 1, then 3, 4, ... (iperf_add_stream).
func streamID(i int) int {
	if i == 0 {
		return 1
	}
	return i + 2
}

func (t *test) results(per []int64, res *Result) serverResults {
	out := serverResults{SenderHasRetransmits: -1, Streams: []streamResult{}}
	if t.c.Direction == Upload {
		out.SenderHasRetransmits = 0
		if res.Retransmits >= 0 {
			out.SenderHasRetransmits = 1
		}
	}
	for i, b := range per {
		out.Streams = append(out.Streams, streamResult{ID: streamID(i), Bytes: b, Retransmits: -1, EndTime: res.Elapsed.Seconds()})
	}
	return out
}

// applyServer takes the server's retransmits for downloads (it is the
// sender). Its byte counts include the ramp-up, so the client's own are
// used for both directions; without TCP_INFO acked bytes (not Linux), an
// upload's bytes drop by what the server says never arrived (what sat in
// our send buffers at the end), never rising above what we wrote.
func (t *test) applyServer(srv *serverResults, res *Result) {
	// Bounded: a server's figures are not trusted.
	var retrans, got int64
	for _, s := range srv.Streams {
		retrans += min(max(s.Retransmits, 0), maxRetrans)
		got += min(max(s.Bytes, 0), maxBytesReported)
	}
	if t.c.Direction == Upload && !t.acked {
		unsent := min(max(t.written()-got, 0), res.Bytes)
		res.Bytes -= unsent
	}
	retrans = min(retrans, maxRetrans)
	if t.c.Direction == Download && srv.SenderHasRetransmits == 1 {
		res.Retransmits = retrans
	}
}

// maxRetrans bounds a server-reported retransmit count.
const maxRetrans = 1e9

// maxBytesReported bounds one server-reported stream byte count.
const maxBytesReported = 1 << 50

// written is every byte the streams wrote so far.
func (t *test) written() int64 {
	t.mu.Lock()
	counts := t.counts
	t.mu.Unlock()
	var n int64
	for i := range counts {
		n += counts[i].Load()
	}
	return n
}

// cookiePrefix makes fyisp's tests recognisable in a server's debug log
// (the server checks only the cookie's length).
const cookiePrefix = "fyisp"

func newCookie(c *[cookieSize]byte) error {
	const alphabet = "abcdefghijklmnopqrstuvwxyz234567"
	var r [cookieSize - 1]byte
	if _, err := rand.Read(r[:]); err != nil {
		return err
	}
	for i, b := range r {
		c[i] = alphabet[int(b)%len(alphabet)]
	}
	copy(c[:], cookiePrefix)
	c[cookieSize-1] = 0
	return nil
}

func (t *test) write(b []byte) error {
	_ = t.ctrl.SetWriteDeadline(time.Now().Add(stepTimeout))
	if _, err := t.ctrl.Write(b); err != nil {
		return fmt.Errorf("control connection: %w", err)
	}
	return nil
}

func (t *test) writeJSON(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	msg := binary.BigEndian.AppendUint32(make([]byte, 0, 4+len(b)), uint32(len(b)))
	return t.write(append(msg, b...))
}

func (t *test) readJSON(v any) error {
	_ = t.ctrl.SetReadDeadline(time.Now().Add(stepTimeout))
	var h [4]byte
	if _, err := io.ReadFull(t.ctrl, h[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(h[:])
	if n > maxJSONSize {
		return fmt.Errorf("JSON of %d bytes", n)
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(t.ctrl, b); err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// state reads the next state byte; ACCESS_DENIED and the error states
// become errors.
func (t *test) state(timeout time.Duration) (int8, error) {
	_ = t.ctrl.SetReadDeadline(time.Now().Add(timeout))
	var b [1]byte
	if _, err := io.ReadFull(t.ctrl, b[:]); err != nil {
		return 0, err
	}
	st := int8(b[0])
	switch st {
	case stateAccessDenied, stateServerError, stateServerTerminate:
		return st, t.stateErr(st)
	}
	return st, nil
}

func (t *test) stateErr(st int8) error {
	switch st {
	case stateAccessDenied:
		return ErrBusy
	case stateServerTerminate:
		return &ServerError{}
	case stateServerError:
		var b [8]byte
		_ = t.ctrl.SetReadDeadline(time.Now().Add(stepTimeout))
		if _, err := io.ReadFull(t.ctrl, b[:]); err != nil {
			return &ServerError{Code: -1}
		}
		return &ServerError{Code: int32(binary.BigEndian.Uint32(b[:4])), Errno: int32(binary.BigEndian.Uint32(b[4:]))}
	}
	return fmt.Errorf("iperf3: unexpected state %d", st)
}

func expect(got, want int8) error {
	if got != want {
		return fmt.Errorf("iperf3: state %d, want %d", got, want)
	}
	return nil
}

func isReset(err error) bool {
	var oe *net.OpError
	return errors.As(err, &oe) && !oe.Timeout() && oe.Op == "read"
}
