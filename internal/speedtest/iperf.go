// Package speedtest runs scheduled throughput tests against public iperf3
// servers (YABS's list in the catalog), with a pure-Go iperf3 client that
// speaks the stock iperf3 3.x control protocol.
package speedtest

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
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
)

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
	Addr      string // host:port (the host already resolved by the caller, or a name)
	Direction Direction
	Streams   int           // parallel TCP streams, default 8
	Duration  time.Duration // measured time, default 9s; with MinDuration, the most
	// Omit is the ramp-up excluded from the result, default 1s. With
	// MinDuration it is the longest ramp-up: the ramp ends as soon as the
	// throughput of a 100 ms tick stops rising (on a fast line with a byte
	// cap, a fixed second would leave nothing to measure).
	Omit time.Duration
	// MinDuration > 0 makes the length adaptive: the test ends once at
	// least MinDuration is measured and the last second's bytes are within
	// Stable (default 5%) of the second before, else after Duration. The
	// client ends it early with TEST_END, which stock servers accept.
	MinDuration time.Duration
	Stable      float64
	// MaxBytes caps the bytes moved in this direction, ramp-up included
	// (0: no cap): the test ends there, and Result.CappedByBytes is set.
	MaxBytes int64
	// Timeout bounds the whole run, default Omit+Duration+5s. The caller's
	// ctx may end it sooner.
	Timeout time.Duration
	// Dial opens TCP connections (tests: a stub). Default net.Dialer.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

// Result is one direction's measurement.
type Result struct {
	Direction Direction
	Bytes     int64 // bytes received over the measured window (all streams)
	// WireBytes is every byte of the data phase on the wire, ramp-up and
	// the time after the window included (all streams; set even when the
	// test fails): what a data budget must count.
	WireBytes int64
	Elapsed   time.Duration // the measured window, without the omitted ramp-up
	// Retransmits summed over the streams, -1 when unknown (the sender's
	// TCP_INFO: the server's for downloads, ours on Linux for uploads).
	Retransmits int64
	Streams     int
	// CappedByBytes: the test hit Config.MaxBytes, so Elapsed may be short.
	CappedByBytes bool
	Ramp          time.Duration // the ramp-up left out
	TotalBytes    int64         // every byte moved, ramp-up included (data use)
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
	if c.Timeout <= 0 {
		c.Timeout = c.Omit + c.Duration + 5*time.Second
	}
	if c.Dial == nil {
		d := &net.Dialer{Timeout: 5 * time.Second}
		c.Dial = d.DialContext
	}
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

	ctrl, err := c.Dial(ctx, "tcp4", c.Addr) // IPv4 only, by decision
	if err != nil {
		if ctx.Err() != nil && errors.Is(err, context.Canceled) {
			return res, ctx.Err()
		}
		if isRefused(err) {
			return res, fmt.Errorf("%w: %w", ErrRefused, err)
		}
		return res, fmt.Errorf("%w: %w", ErrUnreachable, err)
	}
	t := &test{c: c, ctrl: ctrl}
	defer t.close()
	stop := context.AfterFunc(ctx, t.close) // unblocks every read and write
	defer stop()

	r, err := t.run(ctx, &res)
	t.close() // the streams stop: their counters are final
	if ctx.Err() != nil {
		res.WireBytes = t.wire()
		return res, ctx.Err()
	}
	r.WireBytes = t.wire()
	return r, err
}

// wire is the data phase's bytes so far, over every stream.
func (t *test) wire() int64 {
	t.mu.Lock()
	counts := t.counts
	t.mu.Unlock()
	var n int64
	for i := range counts {
		n += counts[i].Load()
	}
	return n
}

type test struct {
	c      Config
	ctrl   net.Conn
	cookie [cookieSize]byte

	mu      sync.Mutex
	streams []net.Conn
	closed  bool

	counts []atomic.Int64 // per stream, the whole data phase (mu: the slice)
	moved  atomic.Int64   // bytes of every stream, against MaxBytes
	capped atomic.Bool
}

func (t *test) close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return
	}
	t.closed = true
	_ = t.ctrl.Close()
	for _, s := range t.streams {
		_ = s.Close()
	}
}

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
		t.streams = append(t.streams, s)
		closed := t.closed
		t.mu.Unlock()
		if closed {
			return *res, ctx.Err()
		}
		if _, err := s.Write(t.cookie[:]); err != nil {
			return *res, fmt.Errorf("data stream: %w", err)
		}
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
	var wg sync.WaitGroup
	for i, s := range t.streams {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if t.c.Direction == Download {
				t.drain(s, &counts[i])
			} else {
				t.fill(s, &counts[i], &done)
			}
		}()
	}
	per, err := t.dataPhase(counts, res)
	done.Store(true)
	if err != nil {
		return *res, err
	}

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
	wg.Wait()
	return *res, nil
}

// idle waits until deadline on the control connection. The server sends
// nothing during the data phase unless it fails.
func (t *test) idle(deadline time.Time) error {
	for time.Now().Before(deadline) && !t.capped.Load() {
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

// dataPhase runs the ramp-up and the measured window on 100 ms ticks, then
// fills res. per is each stream's bytes in the window (for our results).
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
	base := make([]int64, n)
	retransStart := make([]int64, n)
	ackedStart := make([]int64, n)
	measuring := false
	var winStart time.Time
	var prevMax, last, secBytes int64
	kWin := 0 // the tick the window started at
	prevSec := int64(-1)
	for k := 1; ; k++ {
		at := start.Add(time.Duration(k) * tick)
		if err := t.idle(at); err != nil {
			return nil, err
		}
		b := total()
		d := b - last
		last = b
		if t.capped.Load() {
			res.CappedByBytes = true
			if !measuring { // capped in the ramp-up: the whole run is the window,
				winStart = start // counted from the connections' start
				for i := range ackedStart {
					retransStart[i], ackedStart[i] = 0, 0
					if t.c.Direction == Download {
						retransStart[i], ackedStart[i] = -1, -1
					}
				}
			}
			break
		}
		if !measuring {
			rampOver := time.Duration(k)*tick >= t.c.Omit ||
				(adaptive && k >= 3 && float64(d) <= 1.05*float64(prevMax))
			prevMax = max(prevMax, d)
			if rampOver {
				measuring = true
				t.snapshot(counts, base, retransStart, ackedStart)
				winStart, secBytes, kWin = time.Now(), b, k
				res.Ramp = winStart.Sub(start)
			}
			continue
		}
		// Seconds are counted in ticks, so every second has the same length.
		w := time.Duration(k-kWin) * tick
		if w >= t.c.Duration {
			break
		}
		if adaptive && (k-kWin)%int(time.Second/tick) == 0 {
			cur := b - secBytes
			if w >= t.c.MinDuration && prevSec > 0 && math.Abs(float64(cur-prevSec)) <= t.c.Stable*float64(prevSec) {
				break
			}
			prevSec, secBytes = cur, b
		}
	}
	final := make([]int64, n)
	retransEnd := make([]int64, n)
	ackedEnd := make([]int64, n)
	t.snapshot(counts, final, retransEnd, ackedEnd)
	res.Elapsed = time.Since(winStart)
	per := make([]int64, n)
	acked := true
	var ackedBytes int64
	for i := range per {
		per[i] = final[i] - base[i]
		res.Bytes += per[i]
		res.TotalBytes += final[i]
		if ackedStart[i] < 0 || ackedEnd[i] < 0 {
			acked = false
		}
		ackedBytes += ackedEnd[i] - ackedStart[i]
	}
	if t.c.Direction == Upload {
		// What the server received, not what sat in our send buffers.
		if acked {
			res.Bytes = ackedBytes
		}
		if retransStart[0] >= 0 {
			res.Retransmits = 0
			for i := range retransEnd {
				res.Retransmits += max(retransEnd[i]-retransStart[i], 0)
			}
		}
	}
	return per, nil
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

// moved counts the bytes of every stream against Config.MaxBytes; capped
// is set the moment it is reached, and wakes the control loop.
func (t *test) count(n *atomic.Int64, k int) {
	n.Add(int64(k))
	if t.c.MaxBytes > 0 && t.moved.Add(int64(k)) >= t.c.MaxBytes && t.capped.CompareAndSwap(false, true) {
		_ = t.ctrl.SetReadDeadline(time.Now()) // end the current wait now
	}
}

// drain reads a download stream until the server closes it, or until the
// byte cap: then it stops reading, so TCP flow control stops the server
// within its buffers while TEST_END is sent.
func (t *test) drain(s net.Conn, n *atomic.Int64) {
	buf := make([]byte, blockSize)
	for !t.capped.Load() {
		k, err := s.Read(buf)
		t.count(n, k)
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

// fill writes an upload stream until done, never past the byte cap.
func (t *test) fill(s net.Conn, n *atomic.Int64, done *atomic.Bool) {
	for !done.Load() && !t.capped.Load() {
		b := payload
		if t.c.MaxBytes > 0 {
			left := t.c.MaxBytes - t.moved.Load()
			if left <= 0 {
				t.count(n, 0)
				return
			}
			b = b[:min(int64(len(b)), left)]
		}
		_ = s.SetWriteDeadline(time.Now().Add(250 * time.Millisecond))
		k, err := s.Write(b)
		t.count(n, k)
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
// used for both directions.
func (t *test) applyServer(srv *serverResults, res *Result) {
	var retrans int64
	for _, s := range srv.Streams {
		retrans += max(s.Retransmits, 0)
	}
	if t.c.Direction == Download && srv.SenderHasRetransmits == 1 {
		res.Retransmits = retrans
	}
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
