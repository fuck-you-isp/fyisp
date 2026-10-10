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
	Omit      time.Duration // ramp-up excluded from the result, default 1s
	// MinDuration > 0 makes the length adaptive: the test ends once at
	// least MinDuration is measured and the last second's bytes are within
	// Stable (default 5%) of the second before, else after Duration. The
	// client ends it early with TEST_END, which stock servers accept.
	MinDuration time.Duration
	Stable      float64
	// Timeout bounds the whole run, default Omit+Duration+5s. The caller's
	// ctx may end it sooner.
	Timeout time.Duration
	// Dial opens TCP connections (tests: a stub). Default net.Dialer.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

// Result is one direction's measurement.
type Result struct {
	Direction Direction
	Bytes     int64         // bytes received over the measured window (all streams)
	Elapsed   time.Duration // the measured window, without the omitted ramp-up
	// Retransmits summed over the streams, -1 when unknown (the sender's
	// TCP_INFO: the server's for downloads, ours on Linux for uploads).
	Retransmits int64
	Streams     int
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

	ctrl, err := c.Dial(ctx, "tcp", c.Addr)
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
	if ctx.Err() != nil {
		return res, ctx.Err()
	}
	return r, err
}

type test struct {
	c      Config
	ctrl   net.Conn
	cookie [cookieSize]byte

	mu      sync.Mutex
	streams []net.Conn
	closed  bool
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
		s, err := t.c.Dial(ctx, "tcp", t.c.Addr)
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
	var done atomic.Bool
	var wg sync.WaitGroup
	for i, s := range t.streams {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if t.c.Direction == Download {
				drain(s, &counts[i])
			} else {
				fill(s, &counts[i], &done)
			}
		}()
	}
	omitEnd := time.Now().Add(t.c.Omit)
	if err := t.idle(omitEnd); err != nil {
		return *res, err
	}
	base := make([]int64, len(t.streams))
	retransStart := make([]int64, len(t.streams))
	t.snapshot(counts, base, retransStart)
	winStart := time.Now()
	if err := t.measure(counts, winStart); err != nil {
		return *res, err
	}
	final := make([]int64, len(t.streams))
	retransEnd := make([]int64, len(t.streams))
	t.snapshot(counts, final, retransEnd)
	res.Elapsed = time.Since(winStart)
	done.Store(true)
	per := make([]int64, len(t.streams))
	for i := range per {
		per[i] = final[i] - base[i]
		res.Bytes += per[i]
	}
	if t.c.Direction == Upload && retransStart[0] >= 0 {
		res.Retransmits = 0
		for i := range retransEnd {
			res.Retransmits += max(retransEnd[i]-retransStart[i], 0)
		}
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

// measure waits out the measured window: Duration, or with MinDuration
// until the per-second throughput settles.
func (t *test) measure(counts []atomic.Int64, start time.Time) error {
	end := start.Add(t.c.Duration)
	if t.c.MinDuration <= 0 {
		return t.idle(end)
	}
	total := func() int64 {
		var n int64
		for i := range counts {
			n += counts[i].Load()
		}
		return n
	}
	prev, last := int64(-1), total()
	for k := 1; ; k++ {
		tick := start.Add(time.Duration(k) * time.Second)
		if tick.After(end) {
			return t.idle(end)
		}
		if err := t.idle(tick); err != nil {
			return err
		}
		n := total()
		cur := n - last
		last = n
		if time.Duration(k)*time.Second >= t.c.MinDuration && prev > 0 &&
			math.Abs(float64(cur-prev)) <= t.c.Stable*float64(prev) {
			return nil
		}
		prev = cur
	}
}

func (t *test) snapshot(counts []atomic.Int64, into, retrans []int64) {
	for i := range counts {
		into[i] = counts[i].Load()
		retrans[i] = -1
		if t.c.Direction == Upload {
			retrans[i] = totalRetrans(t.streams[i])
		}
	}
}

// drain reads a download stream until the server closes it.
func drain(s net.Conn, n *atomic.Int64) {
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

func fill(s net.Conn, n *atomic.Int64, done *atomic.Bool) {
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
		"tcp":            true,
		"omit":           int(t.c.Omit / time.Second),
		"time":           int(t.c.Duration / time.Second),
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

// applyServer uses the server's side where it is the better measure: for
// uploads the bytes it received after the ramp-up and its window, for
// downloads its retransmits.
func (t *test) applyServer(srv *serverResults, res *Result) {
	var bytes, retrans int64
	var window float64
	for _, s := range srv.Streams {
		bytes += s.Bytes
		retrans += max(s.Retransmits, 0)
		window = max(window, s.EndTime-s.StartTime)
	}
	switch t.c.Direction {
	case Upload:
		if bytes > 0 && window > 0.5 {
			res.Bytes, res.Elapsed = bytes, time.Duration(window*float64(time.Second))
		}
	case Download:
		if srv.SenderHasRetransmits == 1 {
			res.Retransmits = retrans
		}
	}
}

func newCookie(c *[cookieSize]byte) error {
	const alphabet = "abcdefghijklmnopqrstuvwxyz234567"
	var r [cookieSize - 1]byte
	if _, err := rand.Read(r[:]); err != nil {
		return err
	}
	for i, b := range r {
		c[i] = alphabet[int(b)%len(alphabet)]
	}
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
