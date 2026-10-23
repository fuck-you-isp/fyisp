package s3test

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/netinfo"
)

// Per-step bounds (an attempt's class says which one was hit).
const (
	DNSTimeout      = 5 * time.Second
	ConnectTimeout  = 5 * time.Second
	TLSTimeout      = 5 * time.Second
	WriteTimeout    = 5 * time.Second  // the request line and headers
	TTFBTimeout     = 10 * time.Second // the first response byte after a small request
	TTFBUpload      = 30 * time.Second // after an upload's body (servers commit it first)
	StallTimeout    = 10 * time.Second // no body bytes either way
	minTransferTime = 30 * time.Second
	maxTransferTime = 180 * time.Second
)

// Target is one S3-compatible endpoint, as the owner set it up. Nothing
// about it is vendor-specific.
type Target struct {
	ID        string
	Name      string
	Endpoint  *url.URL // scheme://host[:port], nothing else
	Region    string   // signed as typed ("us-east-1", "auto", "garage", ...)
	Bucket    string   // "" = connection-only
	PathStyle bool     // /bucket/key instead of bucket.host/key
	// RootCAs, when set, are the only roots trusted (a self-hosted
	// endpoint's private CA).
	RootCAs *x509.CertPool
	// Private pins the target's network at save: true allows only private
	// addresses (RFC 1918, 100.64/10, loopback), false only public ones.
	Private bool
}

// hostport is the endpoint's host[:port] (the port only when not the
// scheme's default).
func (t *Target) hostport() string { return t.Endpoint.Host }

func (t *Target) tls() bool { return t.Endpoint.Scheme == "https" }

// requestHost and requestPath are what a request for key goes to: the
// Host header and the encoded path, exactly as signed. key "" is the
// bucket; root asks for the service root ("/", no bucket).
func (t *Target) requestHost(root bool) string {
	if t.Bucket == "" || t.PathStyle || root {
		return t.hostport()
	}
	return t.Bucket + "." + t.hostport()
}

func (t *Target) requestPath(key string, root bool) string {
	switch {
	case root || t.Bucket == "":
		return "/"
	case t.PathStyle:
		p := "/" + uriEncode(t.Bucket, false)
		if key != "" {
			p += "/" + uriEncode(key, true)
		}
		return p
	}
	return "/" + uriEncode(key, true)
}

// dialHost is the name to look up and the port (the bucket's virtual host
// resolves to the same front ends, but a client would look it up, so we
// do too).
func (t *Target) dialHost(root bool) (host string, port string) {
	host, port = t.Endpoint.Hostname(), t.Endpoint.Port()
	if port == "" {
		port = "80"
		if t.tls() {
			port = "443"
		}
	}
	if t.Bucket != "" && !t.PathStyle && !root {
		host = t.Bucket + "." + host
	}
	return host, port
}

var (
	bucketRe  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{1,61}[A-Za-z0-9]$`)
	regionRe  = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)
	installRe = regexp.MustCompile(`^[a-z0-9]{1,32}$`)
	hostRe    = regexp.MustCompile(`^[A-Za-z0-9.-]{1,253}$`)
)

// Validate checks a target's settings: an http or https endpoint with a
// host and nothing else, a region and a bucket of S3's characters, path
// style for an IP address, and a session token only over https (token).
// The engine checks every target it runs; the API checks them when saved.
func (t *Target) Validate(token bool) error {
	u := t.Endpoint
	switch {
	case u == nil || (u.Scheme != "http" && u.Scheme != "https"):
		return errors.New("the endpoint must start with https:// or http://")
	case u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "":
		return errors.New("the endpoint is a scheme and a host (and a port), nothing else")
	case !hostRe.MatchString(u.Hostname()):
		return errors.New("the endpoint needs a host name or an IPv4 address")
	case u.Port() != "" && !portOK(u.Port()):
		return errors.New("the endpoint's port must be 1 to 65535")
	case !regionRe.MatchString(t.Region):
		return errors.New("the region is 1 to 32 letters, digits, - or _")
	case t.Bucket != "" && !bucketRe.MatchString(t.Bucket):
		return errors.New("the bucket name is 3 to 63 letters, digits, '.', '-' or '_'")
	case t.Bucket != "" && !t.PathStyle && net.ParseIP(u.Hostname()) != nil:
		return errors.New("an endpoint given as an IP address needs path-style addressing")
	case token && !t.tls():
		return errors.New("a session token is only sent over https")
	}
	return nil
}

func portOK(p string) bool {
	n, err := strconv.Atoi(p)
	return err == nil && n > 0 && n < 65536
}

// addrClass is an address's network for the address policy: private (RFC
// 1918, loopback, carrier-grade NAT), public (netinfo.Traceable, as
// Investigate's hosts), or refused: IPv6, link-local (cloud metadata),
// multicast, reserved, "this network", the IETF block, benchmarking and
// documentation space, and the metadata address some clouds put in
// carrier-grade NAT space.
func addrClass(a netip.Addr) (private, refused bool) {
	a = a.Unmap()
	switch {
	case !a.Is4() || a == cloudMetadata: // IPv4 only
		return false, true
	case a.IsPrivate(), a.IsLoopback(), cgnat.Contains(a):
		return true, false
	case netinfo.Traceable(a):
		return false, false
	}
	return false, true
}

// cloudMetadata is Alibaba Cloud's metadata service (others use 169.254/16).
var cloudMetadata = netip.MustParseAddr("100.100.100.200")

// allowed is the address policy, checked on every dial: never link-local
// (cloud metadata), unspecified, multicast or reserved; the address's
// network must be the target's (no rebinding a public name to the LAN);
// plain http:// only on the LAN itself (RFC 1918, loopback: not the ISP's
// shared 100.64/10).
func (t *Target) allowed(a netip.Addr) bool {
	private, refused := addrClass(a)
	if refused || private != t.Private {
		return false
	}
	return t.tls() || (private && !cgnat.Contains(a.Unmap()))
}

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// AddrAllowed reports whether a target with this scheme and network may use
// address a (the targets API checks endpoints when they're saved).
func AddrAllowed(scheme string, private bool, a netip.Addr) bool {
	t := Target{Endpoint: &url.URL{Scheme: scheme}, Private: private}
	return t.allowed(a)
}

// conn is one connection: the TCP socket (for TCP_INFO and linger), the
// stream on it (TLS or not) and its counters. Reads and writes arm their
// own deadlines.
type conn struct {
	tcp  net.Conn
	nc   net.Conn
	br   *bufio.Reader
	addr netip.Addr // the address it went to

	// Sliding deadlines, armed before every read and write (0: none), never
	// past hard (unix ns; 0: none): a transfer's bound, or now once the
	// attempt is cancelled. Deadlines are wall-clock times.
	readTimeout  time.Duration
	writeTimeout time.Duration
	hard         atomic.Int64
	// firstRead and readN: the first read with bytes, and the bytes read,
	// since armFirst (the first response byte; an idle close).
	firstRead time.Time
	readN     int64
	wire      atomic.Int64 // bytes read and written above TLS (wire totals are an estimate)
	headLeft  int64        // > 0: reads allowed before errHeadLarge (reading a response's head)
	reused    bool         // a request has already been sent on it
	now       func() time.Time
}

// deadline is now+d (d 0: none), never past hard.
func (c *conn) deadline(d time.Duration) time.Time {
	var t time.Time
	if d > 0 {
		t = time.Now().Add(d)
	}
	if h := c.hard.Load(); h != 0 && (t.IsZero() || time.Unix(0, h).Before(t)) {
		t = time.Unix(0, h)
	}
	return t
}

// stopAt makes every read and write fail from t on (a cancel).
func (c *conn) stopAt(t time.Time) {
	c.hard.Store(t.UnixNano())
	_ = c.nc.SetDeadline(t)
}

func (c *conn) armFirst() { c.firstRead, c.readN = time.Time{}, 0 }

func (c *conn) Read(p []byte) (int, error) {
	if c.headLeft < 0 {
		return 0, errHeadLarge
	}
	if c.headLeft > 0 && int64(len(p)) > c.headLeft {
		p = p[:c.headLeft]
	}
	_ = c.nc.SetReadDeadline(c.deadline(c.readTimeout))
	n, err := c.nc.Read(p)
	if c.headLeft > 0 {
		if c.headLeft -= int64(n); c.headLeft == 0 {
			c.headLeft = -1
		}
	}
	if n > 0 {
		if c.firstRead.IsZero() {
			c.firstRead = c.now()
		}
		c.readN += int64(n)
		c.wire.Add(int64(n))
	}
	return n, err
}

func (c *conn) Write(p []byte) (int, error) {
	_ = c.nc.SetWriteDeadline(c.deadline(c.writeTimeout))
	n, err := c.nc.Write(p)
	c.wire.Add(int64(n))
	return n, err
}

// close closes the connection; hard sends a reset (no TIME_WAIT: cold
// checks open many connections).
func (c *conn) close(hard bool) {
	if hard {
		if tc, ok := c.tcp.(*net.TCPConn); ok {
			_ = tc.SetLinger(0)
		}
	}
	_ = c.nc.Close()
}

// dialed is what opening a connection measured.
type dialed struct {
	c          *conn
	addr       netip.Addr
	nAddrs     int
	dns, dial  time.Duration
	handshake  time.Duration
	tlsVersion string
	resumed    bool
	st         step // how far it got
}

// open looks up the host, picks its first IPv4 address (no fallback: that
// hides a bad front end), checks the policy, connects and, for https,
// shakes hands with a fresh TLS session (no resumption).
func (e *Engine) open(ctx context.Context, t *Target, root bool) (dialed, error) {
	var d dialed
	host, port := t.dialHost(root)
	start := e.o.Now()
	lctx, cancel := context.WithTimeout(ctx, e.o.lim.dns)
	addrs, err := e.o.Lookup(lctx, "ip4", host)
	cancel()
	d.dns = e.o.Now().Sub(start)
	if err == nil && len(addrs) == 0 {
		err = errNoIPv4
	}
	if err != nil {
		d.st = stepDNS
		return d, err
	}
	d.nAddrs, d.addr = len(addrs), addrs[0].Unmap()
	d.st = stepDial
	if !t.allowed(d.addr) {
		return d, errAddrRefused
	}
	start = e.o.Now()
	dctx, cancel := context.WithTimeout(ctx, e.o.lim.connect)
	tcp, err := e.o.Dial(dctx, "tcp4", net.JoinHostPort(d.addr.String(), port))
	cancel()
	d.dial = e.o.Now().Sub(start)
	if err != nil {
		return d, err
	}
	c := &conn{tcp: tcp, nc: tcp, addr: d.addr, now: e.o.Now}
	if t.tls() {
		d.st = stepTLS
		cfg := &tls.Config{ServerName: host, RootCAs: t.RootCAs, MinVersion: tls.VersionTLS12, NextProtos: []string{"http/1.1"}}
		tc := tls.Client(tcp, cfg)
		start = e.o.Now()
		hctx, cancel := context.WithTimeout(ctx, e.o.lim.handshake)
		err = tc.HandshakeContext(hctx)
		cancel()
		d.handshake = e.o.Now().Sub(start)
		if err != nil {
			_ = tcp.Close()
			return d, err
		}
		cs := tc.ConnectionState()
		d.tlsVersion, d.resumed = tlsVersion(cs.Version), cs.DidResume
		c.nc = tc
	}
	d.st = stepWrite
	c.br = bufio.NewReaderSize(c, 32<<10)
	d.c = c
	return d, nil
}

func tlsVersion(v uint16) string {
	switch v {
	case tls.VersionTLS12:
		return "1.2"
	case tls.VersionTLS13:
		return "1.3"
	}
	return strconv.Itoa(int(v))
}

// prefix24 is a's /24 ("52.216.12.0/24").
func prefix24(a netip.Addr) string {
	if !a.Is4() {
		return ""
	}
	p, _ := a.Prefix(24)
	return p.String()
}
