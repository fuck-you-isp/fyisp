package probe

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/netip"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

const (
	defaultInterval = 15 * time.Second
	// defaultTimeout is the TCP/ICMP timeout: long enough that a slow
	// success on a bufferbloated link is not recorded as loss, and below the
	// default ICMP interval (5s). A series never waits longer than its
	// interval: probes of one series run one at a time (schedule).
	defaultTimeout = 3 * time.Second
	httpsTimeout   = 5 * time.Second
	resolveTimeout = 5 * time.Second
	// maxLookups bounds concurrent DNS lookups. Resolving every host of a
	// large profile at once (1300 lookups for --profile all) overflows
	// home routers and Docker's resolver: in a measured start 552 of them
	// timed out, and every probe of those hosts was lost with ReasonDNS
	// until the retry 10 s later.
	maxLookups       = 16
	maxBody          = 64 << 10
	defaultUserAgent = "fyisp"
)

// New returns a Runner that probes for real. It detects capabilities once;
// Run opens the ICMP socket/handle and closes it when done.
func New(o Options) Runner {
	if o.ResolveEvery <= 0 {
		o.ResolveEvery = time.Minute
	}
	if o.RetryResolve <= 0 {
		o.RetryResolve = 10 * time.Second
	}
	if o.Lookup == nil {
		o.Lookup = func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip4", host)
		}
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.UserAgent == "" {
		o.UserAgent = defaultUserAgent
	}
	silenceHTTP2Noise()
	return &runner{o: o, caps: Detect(context.Background()), sleep: realSleep, lookups: make(chan struct{}, maxLookups)}
}

type runner struct {
	o       Options
	caps    Caps
	sleep   func(context.Context, time.Duration) bool
	lookups chan struct{} // semaphore: at most maxLookups lookups in flight
}

func (r *runner) Caps() Caps { return r.caps }

func (r *runner) timeout(k model.ProbeKind) time.Duration {
	switch {
	case r.o.Timeout > 0:
		return r.o.Timeout
	case k == model.KindHTTPS:
		return httpsTimeout
	}
	return defaultTimeout
}

// seriesTimeout is the timeout of one TCP/ICMP probe of s: the default is
// capped at the series' interval, so a probe that times out does not also
// skip the series' next slot.
func (r *runner) seriesTimeout(s *series) time.Duration {
	d := r.timeout(s.key.Kind)
	if r.o.Timeout <= 0 && s.iv > 0 {
		d = min(d, s.iv)
	}
	return d
}

// series is one (target, kind) and everything needed to probe it. Each
// series is driven by its own goroutine, which owns the http.Client.
type series struct {
	key  model.SeriesKey
	idx  uint32
	host *hostState
	port int
	url  string
	// client is replaced after a failed HTTPS probe (see resetClient); it is
	// also read by the DNS-change callback, hence atomic.
	client atomic.Pointer[http.Client]
	// sessions keeps the TLS session ticket across reconnects and client
	// resets: a resumed handshake skips the certificate chain (~3-5 KB down
	// and a signature check), and servers close idle connections often.
	sessions tls.ClientSessionCache
	iv       time.Duration
	phase    time.Duration
}

// Run probes every target of p until ctx is done, then returns nil. Samples
// in flight when ctx ends are dropped (not reported as loss).
func (r *runner) Run(ctx context.Context, p *model.Profile, sink model.Sink) error {
	if p == nil || len(p.Targets) == 0 {
		return errors.New("probe: empty profile")
	}
	silenceHTTP2Noise()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var png pinger
	if r.caps.ICMP != ICMPUnavailable {
		var err error
		if png, _, err = openPinger(); err != nil {
			r.o.Log.Warn("ICMP socket failed; probing HTTPS and TCP only", "err", err)
			png = nil
		} else {
			defer png.Close()
		}
	} else {
		r.o.Log.Warn("ICMP unavailable", "hint", r.caps.ICMPHint)
	}

	hosts := map[string]*hostState{}
	hostOf := func(h string) *hostState {
		hs := hosts[h]
		if hs == nil {
			hs = newHostState(h)
			hosts[h] = hs
		}
		return hs
	}
	var all []*series
	n := len(p.Targets)
	for i, t := range p.Targets {
		iv := t.Interval
		if iv <= 0 {
			iv = defaultInterval
		}
		port := t.Port
		if port == 0 {
			port = 443
		}
		for _, k := range kindsOf(t) {
			s := &series{key: model.SeriesKey{Target: t.Name, Kind: k}, idx: uint32(i), port: port, iv: iv}
			s.host = hostOf(t.HostFor(k))
			if s.host.special != "" && k != model.KindICMP {
				continue // path layers are measured with ICMP only
			}
			switch k {
			case model.KindHTTPS:
				s.phase = phaseOf(i, n, iv, 0)
				s.url = httpsURL(s.host.name, port, t.Path)
				s.client.Store(r.newClient(s))
				s.host.onChange = append(s.host.onChange, func() { s.client.Load().CloseIdleConnections() })
			case model.KindTCP:
				s.phase = phaseOf(i, n, iv, 0.5)
			case model.KindICMP:
				if png == nil {
					continue
				}
				s.iv = iv / 3
				s.phase = phaseOf(i, n, s.iv, 0.25)
			default:
				continue
			}
			all = append(all, s)
		}
	}

	// Resolve every host once before the first probe, then keep them fresh.
	var wg sync.WaitGroup
	for _, hs := range hosts {
		wg.Add(1)
		go func() { defer wg.Done(); r.resolve(ctx, hs) }()
	}
	wg.Wait()
	for _, hs := range hosts {
		if hs.literal || hs.special != "" {
			continue
		}
		wg.Add(1)
		go func() { defer wg.Done(); r.resolveLoop(ctx, hs) }()
	}

	c := clock{now: r.o.Now, sleep: r.sleep}
	for _, s := range all {
		wg.Add(1)
		go func() {
			defer wg.Done()
			schedule(ctx, c, s.iv, s.phase, func(slot time.Time) {
				smp, ok := r.probe(ctx, s, png)
				if !ok || ctx.Err() != nil {
					return // not measured, or shutting down
				}
				smp.Slot = slot
				sink.Observe(smp)
			})
		}()
	}
	wg.Wait()
	for _, s := range all {
		if c := s.client.Load(); c != nil {
			c.CloseIdleConnections()
		}
	}
	return nil
}

func httpsURL(host string, port int, path string) string {
	if path == "" {
		path = "/"
	}
	hp := host
	if port != 443 {
		hp = net.JoinHostPort(host, strconv.Itoa(port))
	}
	return "https://" + hp + path
}

// probe runs one measurement. Slot is set by the caller. ok is false when
// the slot is not measured (a special host whose address is not known).
func (r *runner) probe(ctx context.Context, s *series, png pinger) (smp model.Sample, ok bool) {
	smp = model.Sample{Key: s.key}
	var (
		ip  netip.Addr
		err error
	)
	if s.host.special != "" {
		ip, ok, err = r.pathAddr(s.host.special)
		if !ok {
			return smp, false
		}
		if err != nil {
			smp.Lost, smp.Reason, smp.Err = true, model.ReasonNoNetwork, err.Error()
			return smp, true
		}
	} else if ip, err = s.host.addr(); err != nil {
		smp.Lost, smp.Reason, smp.Err = true, model.ReasonDNS, err.Error()
		return smp, true
	}
	var (
		rtt    time.Duration
		reason model.Reason
	)
	switch s.key.Kind {
	case model.KindHTTPS:
		rtt, smp.Reused, reason, err = r.probeHTTPS(ctx, s)
	case model.KindTCP:
		rtt, reason, err = r.probeTCP(ctx, ip, s.port, r.seriesTimeout(s))
	case model.KindICMP:
		rtt, reason, err = png.Ping(ctx, ip, s.idx, r.seriesTimeout(s))
	}
	if err != nil {
		smp.Lost, smp.Reason, smp.Err = true, reason, err.Error()
		return smp, true
	}
	smp.RTT = rtt
	return smp, true
}

var errNoRoute = errors.New("no default route")

// pathAddr resolves a special host from the current network path. ok is
// false when the slot is not measured (no Path, or the address is not known
// yet); err is set when there is no default route (the slot is lost).
func (r *runner) pathAddr(special string) (ip netip.Addr, ok bool, err error) {
	if r.o.Path == nil {
		return netip.Addr{}, false, nil
	}
	p := r.o.Path()
	switch special {
	case model.HostGateway:
		ip = p.Gateway
	case model.HostEdge:
		ip = p.Edge
	}
	switch {
	case ip.IsValid():
		return ip.Unmap(), true, nil
	case p.NoRoute():
		return netip.Addr{}, true, errNoRoute
	}
	return netip.Addr{}, false, nil
}

func (r *runner) probeTCP(ctx context.Context, ip netip.Addr, port int, timeout time.Duration) (time.Duration, model.Reason, error) {
	d := net.Dialer{Timeout: timeout}
	t0 := time.Now()
	c, err := d.DialContext(ctx, "tcp4", netip.AddrPortFrom(ip, uint16(port)).String())
	rtt := time.Since(t0)
	if err != nil {
		return 0, classify(err), err
	}
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.SetLinger(0) // RST on close: no TIME_WAIT sockets left behind
	}
	_ = c.Close()
	return rtt, 0, nil
}

// resetClient drops a series' connection pool after a failed probe. With one
// connection per target, a connection stalled by an outage (TCP retransmit
// backoff, an unresponsive HTTP/2 connection) would otherwise keep failing
// requests for several intervals after the path recovers.
func (r *runner) resetClient(s *series) {
	old := s.client.Swap(r.newClient(s))
	old.CloseIdleConnections()
	if tr, ok := old.Transport.(*http.Transport); ok {
		tr.CloseIdleConnections()
	}
}

func (r *runner) newClient(s *series) *http.Client {
	tc := &tls.Config{}
	if r.o.TLSConfig != nil {
		tc = r.o.TLSConfig.Clone()
	}
	if s.sessions == nil {
		s.sessions = tls.NewLRUClientSessionCache(1)
	}
	tc.ClientSessionCache = s.sessions
	d := &net.Dialer{Timeout: r.timeout(model.KindHTTPS)}
	tr := &http.Transport{
		Proxy: nil, // measure the direct path, never a proxy
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			ip, err := s.host.addr()
			if err != nil {
				return nil, err
			}
			return d.DialContext(ctx, "tcp4", netip.AddrPortFrom(ip, uint16(s.port)).String())
		},
		TLSClientConfig:     tc,
		ForceAttemptHTTP2:   true,
		MaxIdleConns:        1,
		MaxIdleConnsPerHost: 1,
		MaxConnsPerHost:     1,
		IdleConnTimeout:     60 * time.Second,
		TLSHandshakeTimeout: r.timeout(model.KindHTTPS),
		DisableCompression:  true,
	}
	return &http.Client{
		Transport:     tr,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// probeHTTPS sends a GET on the target's kept-alive connection. RTT is the
// time from the request being written to the first response byte, on new
// and reused connections alike. Any HTTP status is a success.
//
// httptrace events run on transport goroutines and are not ordered: under
// load GotFirstResponseByte can be observed before WroteRequest, and on
// HTTP/2 WroteRequest may not have fired yet when Do returns. A completed
// request is never reported as lost because of that: the start falls back
// from WroteHeaders to WroteRequest to GotConn, and the end from
// GotFirstResponseByte to the moment Do returned the response headers.
func (r *runner) probeHTTPS(ctx context.Context, s *series) (time.Duration, bool, model.Reason, error) {
	// Trace hooks may run on transport goroutines.
	var (
		mu                    sync.Mutex
		reused, tlsDone       bool
		gotConn, wroteHeaders time.Time
		wrote, first          time.Time
	)
	trace := &httptrace.ClientTrace{
		GotConn: func(i httptrace.GotConnInfo) {
			now := time.Now()
			mu.Lock()
			gotConn = now
			reused, tlsDone = i.Reused, i.Reused
			mu.Unlock()
		},
		TLSHandshakeDone: func(_ tls.ConnectionState, err error) {
			mu.Lock()
			tlsDone = err == nil
			mu.Unlock()
		},
		WroteHeaders: func() {
			now := time.Now()
			mu.Lock()
			wroteHeaders = now
			mu.Unlock()
		},
		WroteRequest: func(i httptrace.WroteRequestInfo) {
			now := time.Now()
			mu.Lock()
			if i.Err == nil {
				wrote = now
			}
			mu.Unlock()
		},
		GotFirstResponseByte: func() {
			now := time.Now()
			mu.Lock()
			first = now
			mu.Unlock()
		},
	}
	ctx, cancel := context.WithTimeout(ctx, r.timeout(model.KindHTTPS))
	defer cancel()
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace), http.MethodGet, s.url, nil)
	if err != nil {
		return 0, false, model.ReasonOther, err
	}
	req.Header.Set("User-Agent", r.o.UserAgent)
	resp, err := s.client.Load().Do(req)
	headersAt := time.Now()
	mu.Lock()
	defer mu.Unlock()
	if err != nil {
		reason := classify(err)
		if reason == model.ReasonOther && tlsDone {
			reason = model.ReasonHTTP // protocol error after the handshake
		}
		r.resetClient(s)
		return 0, reused, reason, err
	}
	mu.Unlock()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBody))
	_ = resp.Body.Close()
	mu.Lock()
	return httpsRTT(gotConn, wroteHeaders, wrote, first, headersAt), reused, 0, nil
}

// hostState is the current resolution of one hostname, shared by every
// series that probes it.
type hostState struct {
	name     string
	literal  bool
	special  string // model.HostGateway or model.HostEdge: resolved per probe from Options.Path
	cur      atomic.Pointer[resolution]
	onChange []func() // called when the address changes
}

// resolution: ip is valid when known; err is set when the last lookup
// failed, fails counts consecutive failed lookups. The previous ip is kept
// and used until fails reaches DNSFailLimit, then probes are lost with
// ReasonDNS until a lookup succeeds again.
type resolution struct {
	ip    netip.Addr
	err   error
	fails int
}

var errNotResolved = errors.New("not resolved yet")

func newHostState(h string) *hostState {
	hs := &hostState{name: h}
	if h == model.HostGateway || h == model.HostEdge {
		hs.special = h
		return hs
	}
	if a, err := netip.ParseAddr(h); err == nil {
		hs.literal = true
		hs.cur.Store(&resolution{ip: a.Unmap()})
	}
	return hs
}

func (hs *hostState) addr() (netip.Addr, error) {
	c := hs.cur.Load()
	switch {
	case c == nil:
		return netip.Addr{}, &net.DNSError{Err: errNotResolved.Error(), Name: hs.name}
	case c.ip.IsValid() && c.fails < DNSFailLimit:
		return c.ip, nil
	}
	return netip.Addr{}, c.err
}

func (r *runner) resolve(ctx context.Context, hs *hostState) {
	if hs.literal || hs.special != "" {
		return
	}
	// The timeout starts once a lookup slot is free: waiting for one is not
	// a slow resolver.
	if r.lookups != nil {
		select {
		case r.lookups <- struct{}{}:
			defer func() { <-r.lookups }()
		case <-ctx.Done():
			return
		}
	}
	ctx, cancel := context.WithTimeout(ctx, resolveTimeout)
	defer cancel()
	ips, err := r.o.Lookup(ctx, hs.name)
	if ctx.Err() != nil && err == nil {
		err = ctx.Err()
	}
	prev := hs.cur.Load()
	next := &resolution{}
	if err == nil && len(ips) == 0 {
		err = &net.DNSError{Err: "no IPv4 address", Name: hs.name, IsNotFound: true}
	}
	if err != nil {
		var de *net.DNSError
		if errors.As(err, &de) {
			// Drop the resolver address: errors are shown in the UI.
			c := *de
			c.Server = ""
			err = &c
		} else {
			err = &net.DNSError{Err: err.Error(), Name: hs.name}
		}
		next.err = err
		next.fails = 1
		if prev != nil {
			next.ip = prev.ip // keep using the last good address (for a while)
			next.fails = prev.fails + 1
		}
		switch {
		case prev == nil || prev.err == nil:
			r.o.Log.Warn("dns lookup failed", "host", hs.name, "err", err, "keeping", next.ip)
		case next.ip.IsValid() && next.fails == DNSFailLimit:
			r.o.Log.Warn("dns lookup keeps failing; probes of this host count as lost", "host", hs.name, "err", err, "failed_lookups", next.fails)
		}
	} else {
		next.ip = ips[0].Unmap()
		if prev != nil && prev.err != nil {
			r.o.Log.Info("dns lookup recovered", "host", hs.name, "failed_lookups", prev.fails)
		}
	}
	hs.cur.Store(next)
	if prev != nil && prev.ip.IsValid() && next.ip != prev.ip {
		r.o.Log.Debug("address changed", "host", hs.name, "from", prev.ip, "to", next.ip)
		for _, f := range hs.onChange {
			f()
		}
	}
}

// resolveLoop re-resolves every ResolveEvery, or every RetryResolve while
// the last lookup failed.
func (r *runner) resolveLoop(ctx context.Context, hs *hostState) {
	for {
		d := r.o.ResolveEvery
		if c := hs.cur.Load(); c == nil || c.err != nil {
			d = r.o.RetryResolve
		}
		if !realSleep(ctx, d) {
			return
		}
		r.resolve(ctx, hs)
	}
}

// httpsRTT picks the most precise available start and end (see probeHTTPS).
// Zero times are missing events. Trace callbacks run on transport goroutines
// and can be observed late: on HTTP/2 WroteHeaders may even be recorded after
// the response arrived. So the end is the first response byte (else the moment
// Do returned), and the start is the latest request-side event that is not
// after that end. The result is never negative.
func httpsRTT(gotConn, wroteHeaders, wrote, first, headersAt time.Time) time.Duration {
	end := headersAt
	if !first.IsZero() && !first.After(headersAt) {
		end = first
	}
	var start time.Time
	for _, t := range []time.Time{gotConn, wroteHeaders, wrote} {
		if !t.IsZero() && !t.After(end) && t.After(start) {
			start = t
		}
	}
	if start.IsZero() {
		return 0
	}
	return end.Sub(start)
}
