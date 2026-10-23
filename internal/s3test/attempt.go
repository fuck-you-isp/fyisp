package s3test

import (
	"context"
	"encoding/xml"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/probe"
	"github.com/fuck-you-isp/fyisp/internal/speedtest"
	"github.com/fuck-you-isp/fyisp/internal/tcpinfo"
)

// Options configure an Engine.
type Options struct {
	// Lookup and Dial default to the system resolver and a plain dialer
	// (tests and the netns lab replace them). No proxy is ever used.
	Lookup func(ctx context.Context, network, host string) ([]netip.Addr, error)
	Dial   func(ctx context.Context, network, addr string) (net.Conn, error)
	Now    func() time.Time
	// Masker marks the transfers (downloads, uploads, seeding) as fyisp's
	// own tests: probes during them are attributed to the test. The small
	// connection checks are not masked.
	Masker Masker
	// IsSelf reports an address of this machine, or its public address:
	// never stored as an edge address. Nil: every address is (none stored).
	IsSelf    func(netip.Addr) bool
	UserAgent string
	sleep     func(ctx context.Context, d time.Duration) bool // tests
	lim       limits                                          // tests; zero: the defaults
}

// limits are the per-step bounds (tests shorten them).
type limits struct {
	dns, connect, handshake, write, ttfb, ttfbUpload, stall time.Duration
}

var defaultLimits = limits{dns: DNSTimeout, connect: ConnectTimeout, handshake: TLSTimeout, write: WriteTimeout,
	ttfb: TTFBTimeout, ttfbUpload: TTFBUpload, stall: StallTimeout}

// Masker is *speedtest.Windows: Begin and End bracket a transfer.
type Masker interface {
	Begin(t time.Time)
	End(t time.Time) speedtest.Window
}

// Engine runs storage test rounds.
type Engine struct{ o Options }

// New returns an Engine.
func New(o Options) *Engine {
	if o.Lookup == nil {
		o.Lookup = net.DefaultResolver.LookupNetIP
	}
	if o.Dial == nil {
		var d net.Dialer
		o.Dial = d.DialContext
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.UserAgent == "" {
		o.UserAgent = "fyisp"
	}
	if o.lim == (limits{}) {
		o.lim = defaultLimits
	}
	if o.IsSelf == nil {
		o.IsSelf = func(netip.Addr) bool { return true }
	}
	if o.sleep == nil {
		o.sleep = func(ctx context.Context, d time.Duration) bool {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-ctx.Done():
				return false
			case <-t.C:
				return true
			}
		}
	}
	return &Engine{o: o}
}

// request is one request's spec.
type request struct {
	method string
	key    string // object key under the bucket; "" the bucket itself
	root   bool   // the service root ("/"), no bucket
	query  url.Values
	header map[string]string // extra headers, signed ("range")
	creds  *Creds            // nil: unsigned
	body   func() io.Reader  // the upload's body (called once)
	size   int64             // its length
	hash   string            // its payload hash ("" with no body: EmptySHA256)
	sink   io.Writer         // where a download's body goes (counted); nil: discarded, up to errBodyMax
	want   int64             // a download's expected length (0: small)
	keep   int64             // how much of a small 2xx body to keep (0: errBodyMax)
	upload bool              // wait longer for the first byte (the server commits the body)
	// transfer: a download or upload whose goodput and stalls are measured
	// (not the checks' small objects).
	transfer bool
	total    time.Duration // a transfer's bound (0: none beyond the steps)
}

// errBodyMax is how much of a small or error body is read.
const errBodyMax = 4 << 10

// result is what one request returned, beyond the attempt's fields.
type result struct {
	status int
	header http.Header
	body   []byte // a small body (list results), up to request.keep
	// cancelled: ctx ended (the round's cap, a cancel, shutdown) before the
	// attempt did; it says nothing about the endpoint and is not recorded.
	cancelled bool
}

// do runs r on c (nil: open a new connection, closed after unless keep),
// filling a. It returns the connection when kept and still usable.
func (e *Engine) do(ctx context.Context, t *Target, r request, c *conn, keep bool, a *model.StorageAttempt) (*conn, result) {
	a.At = e.o.Now()
	start := a.At
	var res result
	reused := false         // the request goes out on a kept connection
	var hardTotal time.Time // the transfer's bound, when it has one
	// cause names why err happened: the ctx, the transfer's bound or a
	// stall, before the step's own classification.
	cause := func(err error) error {
		switch {
		case ctx.Err() != nil:
			return errCancelled
		case !hardTotal.IsZero() && !time.Now().Before(hardTotal) && isTimeout(err):
			return errTotal
		}
		return err
	}
	fail := func(st step, err error, cn *conn) (*conn, result) {
		err = cause(err)
		if errors.Is(err, errCancelled) {
			res.cancelled = true
			a.Class = ""
		} else {
			// The server dropped the kept connection without a word: the
			// next read sees its close (Linux), or a reset when the request
			// reached a socket already closed (macOS, Windows).
			if (st == stepWait || st == stepWrite) && cn != nil && reused && cn.readN == 0 &&
				(errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || probe.Classify(err) == model.ReasonReset) {
				err = errConnClosed
			}
			a.Class = string(classifyErr(st, err))
		}
		e.finishTCP(cn, a)
		if cn != nil {
			cn.close(true)
		}
		a.Totalms = ms(e.o.Now().Sub(start))
		return nil, res
	}
	if c == nil {
		d, err := e.open(ctx, t, r.root)
		a.HostAddrCount = d.nAddrs
		a.DNSms = ms(d.dns)
		if d.addr.IsValid() && !e.o.IsSelf(d.addr) {
			a.EdgeIP, a.EdgePrefix = d.addr, prefix24(d.addr)
		}
		if d.st >= stepDial && !errors.Is(err, errAddrRefused) {
			a.Connectms = ms(d.dial)
		}
		if d.st >= stepTLS {
			a.TLSms = ms(d.handshake)
		}
		a.TLSVersion, a.Resumed = d.tlsVersion, d.resumed
		if err != nil {
			return fail(d.st, err, nil)
		}
		c = d.c
	} else {
		reused = c.reused
		if !e.o.IsSelf(c.addr) {
			a.EdgeIP, a.EdgePrefix = c.addr, prefix24(c.addr)
		}
	}
	// A cancel stops the attempt at once: no deadline can be re-armed past it.
	stop := context.AfterFunc(ctx, func() { c.stopAt(time.Now()) })
	defer stop()
	if r.total > 0 {
		hardTotal = time.Now().Add(r.total)
		c.hard.Store(hardTotal.UnixNano())
		defer c.hard.Store(0)
	}
	// Sign and write the request.
	hdr := map[string]string{"host": t.requestHost(r.root)}
	for k, v := range r.header {
		hdr[k] = v
	}
	path := t.requestPath(r.key, r.root)
	hash := r.hash
	if hash == "" {
		hash = EmptySHA256
	}
	req := &http.Request{Method: r.method, Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		URL:  &url.URL{Scheme: t.Endpoint.Scheme, Host: hdr["host"], Opaque: path, RawQuery: canonicalQuery(r.query)},
		Host: hdr["host"], Header: http.Header{"User-Agent": {e.o.UserAgent}}, ContentLength: r.size}
	if r.creds != nil {
		sg := sign(r.creds, t.Region, r.method, path, r.query, hdr, hash, e.o.Now())
		for k, v := range sg.Headers {
			if k != "host" {
				req.Header.Set(k, v)
			}
		}
		req.Header.Set("Authorization", sg.Authorization)
	} else {
		for k, v := range r.header {
			req.Header.Set(k, v)
		}
	}
	var up *gapReader
	if r.body != nil {
		up = &gapReader{r: r.body(), now: e.o.Now}
		req.Body = io.NopCloser(up)
	}
	c.writeTimeout = e.o.lim.write
	if r.size > 0 {
		c.writeTimeout = e.o.lim.stall
	}
	bodyStart := e.o.Now()
	c.armFirst()
	werr := req.Write(c)
	wrote := e.o.Now()
	c.writeTimeout = 0
	c.reused = true
	if werr != nil {
		if errors.Is(cause(werr), errCancelled) || errors.Is(cause(werr), errTotal) || r.size == 0 {
			return fail(stepWrite, werr, c)
		}
		// A server may refuse an upload early (403, then a close): read its
		// answer if it sent one, else the write's failure stands.
		c.readTimeout = time.Second
		if resp, err := http.ReadResponse(c.br, req); err == nil {
			_ = resp.Body.Close()
			a.HTTPStatus, a.TTFBms = resp.StatusCode, ms(0)
			a.Class = string(classifyStatus(resp.StatusCode, "", r.creds != nil, skewOf(resp.Header, e.o.Now()), resp.Header.Get("Retry-After") != ""))
			if a.Class != string(ClassOK) {
				e.requestID(resp.Header, a)
				e.finishTCP(c, a)
				c.close(true)
				a.Totalms = ms(e.o.Now().Sub(start))
				return nil, res
			}
		}
		if isTimeout(werr) {
			werr = errStall
		}
		return fail(stepWrite, werr, c)
	}
	// Wait for the first response byte.
	c.readTimeout = e.o.lim.ttfb
	if r.upload {
		c.readTimeout = e.o.lim.ttfbUpload
	}
	c.armFirst()
	resp, err := http.ReadResponse(c.br, req)
	if err != nil {
		return fail(stepWait, err, c)
	}
	first := c.firstRead
	if first.IsZero() || first.Before(wrote) {
		first = wrote
	}
	a.TTFBms = ms(first.Sub(wrote))
	res.status, res.header = resp.StatusCode, resp.Header
	a.HTTPStatus = resp.StatusCode
	a.SkewS = skewOf(resp.Header, first)
	// The body.
	c.readTimeout = e.o.lim.stall
	var n int64
	var gap time.Duration
	var bodyFirst time.Time
	large := false
	if r.want > 0 && resp.StatusCode/100 == 2 {
		sink := r.sink
		if sink == nil {
			sink = io.Discard
		}
		n, gap, bodyFirst, err = e.copyBody(sink, resp.Body)
	} else {
		limit := int64(errBodyMax)
		if r.keep > 0 && resp.StatusCode/100 == 2 {
			limit = r.keep
		}
		var b []byte
		b, err = io.ReadAll(io.LimitReader(resp.Body, limit))
		n = int64(len(b))
		if resp.StatusCode/100 == 2 {
			res.body = b
		} else {
			a.S3Code = s3Code(b)
		}
		// More body than read (a HEAD has none, whatever its length says):
		// the connection can't be kept.
		large = err == nil && req.Method != "HEAD" && (resp.ContentLength > limit || resp.ContentLength < 0)
	}
	_ = resp.Body.Close()
	end := e.o.Now()
	a.Bytes = n
	if err != nil {
		if isTimeout(err) && !errors.Is(cause(err), errTotal) {
			err = errStall
		}
		return fail(stepBody, err, c)
	}
	switch {
	case r.size > 0 && r.transfer:
		a.Bytes = r.size
		g := float64(r.size*8) / max(wrote.Sub(bodyStart).Seconds(), 1e-6)
		a.GoodputBps, a.Stallms = &g, ms(up.max)
	case r.size > 0:
		a.Bytes = r.size
	case r.want > 0 && r.transfer && !bodyFirst.IsZero():
		g := float64(n*8) / max(end.Sub(bodyFirst).Seconds(), 1e-6)
		a.GoodputBps, a.Stallms = &g, ms(gap)
	}
	if r.want > 0 && resp.StatusCode/100 == 2 && n != r.want {
		a.Class = string(ClassShortBody)
	}
	if !hardTotal.IsZero() && !time.Now().Before(hardTotal) {
		a.Class = string(ClassTimeoutTotal) // finished, but past its bound
	}
	if a.Class == "" {
		a.Class = string(classifyStatus(resp.StatusCode, a.S3Code, r.creds != nil, a.SkewS, resp.Header.Get("Retry-After") != ""))
	}
	if a.Class != string(ClassOK) {
		e.requestID(resp.Header, a)
	}
	e.finishTCP(c, a)
	a.Totalms = ms(end.Sub(start))
	if !keep || resp.Close || large || a.Class != string(ClassOK) {
		c.close(!keep)
		return nil, res
	}
	return c, res
}

// copyBody copies a download to w, timing it: the longest gap between
// reads, and the first body byte.
func (e *Engine) copyBody(w io.Writer, r io.Reader) (n int64, gap time.Duration, first time.Time, err error) {
	buf := make([]byte, 64<<10)
	last := e.o.Now()
	for {
		k, rerr := r.Read(buf)
		if k > 0 {
			now := e.o.Now()
			if first.IsZero() {
				first = now
			} else if d := now.Sub(last); d > gap {
				gap = d
			}
			last = now
			if _, werr := w.Write(buf[:k]); werr != nil {
				return n, gap, first, werr
			}
			n += int64(k)
		}
		if rerr == io.EOF {
			return n, gap, first, nil
		}
		if rerr != nil {
			return n, gap, first, rerr
		}
	}
}

// finishTCP reads TCP_INFO before close (Linux).
func (e *Engine) finishTCP(c *conn, a *model.StorageAttempt) {
	if c == nil {
		return
	}
	in, ok := tcpinfo.Read(c.tcp)
	if !ok {
		return
	}
	rtt := float64(in.RTT) / float64(time.Millisecond)
	retrans, mss := int(in.Retrans), in.MSS
	a.TCPRTTms, a.Retrans, a.MSS = &rtt, &retrans, &mss
}

// requestID keeps the provider's request identifiers of a failure, when
// they look like identifiers and not like an address (a server could echo
// the client's public IP in one).
func (e *Engine) requestID(h http.Header, a *model.StorageAttempt) {
	for _, name := range requestIDHeaders {
		if v := h.Get(name); validID(v) {
			a.RequestID, a.RequestIDHeader = v, name
			break
		}
	}
	if v := h.Get("x-amz-id-2"); validID(v) {
		a.HostID = v
	}
}

func validID(v string) bool { return reqIDRe.MatchString(v) && !quadRe.MatchString(v) }

// s3Code is an S3 error body's <Code>, when it is a plain word.
func s3Code(b []byte) string {
	var x struct {
		Code string `xml:"Code"`
	}
	if xml.Unmarshal(b, &x) != nil || !s3CodeRe.MatchString(x.Code) {
		return ""
	}
	return x.Code
}

// skewOf is the server's Date minus local time at the first byte, whole
// seconds (nil without a Date).
func skewOf(h http.Header, at time.Time) *int {
	d, err := http.ParseTime(h.Get("Date"))
	if err != nil {
		return nil
	}
	s := int(d.Sub(at.Truncate(time.Second)).Seconds())
	return &s
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// ms is d in milliseconds, rounded to 0.01.
func ms(d time.Duration) *float64 {
	v := float64(d.Microseconds()/10) / 100
	return &v
}
