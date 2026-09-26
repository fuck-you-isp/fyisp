package probe

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
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
	defaultInterval  = 15 * time.Second
	defaultTimeout   = time.Second
	httpsTimeout     = 5 * time.Second
	resolveTimeout   = 5 * time.Second
	maxBody          = 64 << 10
	defaultUserAgent = "fyisp"
)

// New returns a Runner that probes for real. It detects capabilities once;
// Run opens the ICMP socket/handle and closes it when done.
func New(o Options) Runner {
	if o.ResolveEvery <= 0 {
		o.ResolveEvery = 15 * time.Minute
	}
	if o.RetryResolve <= 0 {
		o.RetryResolve = 10 * time.Second
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
	return &runner{o: o, caps: Detect(context.Background()), sleep: realSleep}
}

type runner struct {
	o     Options
	caps  Caps
	sleep func(context.Context, time.Duration) bool
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

// series is one (target, kind) and everything needed to probe it. Each
// series is driven by its own goroutine, which owns the http.Client.
type series struct {
	key    model.SeriesKey
	idx    uint32
	host   *hostState
	port   int
	url    string
	client *http.Client
	iv     time.Duration
	phase  time.Duration
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
			switch k {
			case model.KindHTTPS:
				s.phase = phaseOf(i, n, iv, 0)
				s.url = httpsURL(s.host.name, port, t.Path)
				s.client = r.newClient(s)
				s.host.onChange = append(s.host.onChange, s.client.CloseIdleConnections)
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
		if hs.literal {
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
				smp := r.probe(ctx, s, png)
				if ctx.Err() != nil {
					return // shutting down: not a measurement
				}
				smp.Slot = slot
				sink.Observe(smp)
			})
		}()
	}
	wg.Wait()
	for _, s := range all {
		if s.client != nil {
			s.client.CloseIdleConnections()
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

// probe runs one measurement. Slot is set by the caller.
func (r *runner) probe(ctx context.Context, s *series, png pinger) model.Sample {
	smp := model.Sample{Key: s.key}
	ip, err := s.host.addr()
	if err != nil {
		smp.Lost, smp.Reason, smp.Err = true, model.ReasonDNS, err.Error()
		return smp
	}
	var (
		rtt    time.Duration
		reason model.Reason
	)
	switch s.key.Kind {
	case model.KindHTTPS:
		rtt, smp.Reused, reason, err = r.probeHTTPS(ctx, s)
	case model.KindTCP:
		rtt, reason, err = r.probeTCP(ctx, ip, s.port)
	case model.KindICMP:
		rtt, reason, err = png.Ping(ctx, ip, s.idx, r.timeout(model.KindICMP))
	}
	if err != nil {
		smp.Lost, smp.Reason, smp.Err = true, reason, err.Error()
		return smp
	}
	smp.RTT = rtt
	return smp
}

func (r *runner) probeTCP(ctx context.Context, ip netip.Addr, port int) (time.Duration, model.Reason, error) {
	d := net.Dialer{Timeout: r.timeout(model.KindTCP)}
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

func (r *runner) newClient(s *series) *http.Client {
	var tc *tls.Config
	if r.o.TLSConfig != nil {
		tc = r.o.TLSConfig.Clone()
	}
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
func (r *runner) probeHTTPS(ctx context.Context, s *series) (time.Duration, bool, model.Reason, error) {
	// Trace hooks may run on transport goroutines.
	var (
		mu              sync.Mutex
		reused, tlsDone bool
		wrote, first    time.Time
	)
	trace := &httptrace.ClientTrace{
		GotConn: func(i httptrace.GotConnInfo) {
			mu.Lock()
			reused, tlsDone = i.Reused, i.Reused
			mu.Unlock()
		},
		TLSHandshakeDone: func(_ tls.ConnectionState, err error) {
			mu.Lock()
			tlsDone = err == nil
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
	resp, err := s.client.Do(req)
	mu.Lock()
	defer mu.Unlock()
	if err != nil {
		reason := classify(err)
		if reason == model.ReasonOther && tlsDone {
			reason = model.ReasonHTTP // protocol error after the handshake
		}
		return 0, reused, reason, err
	}
	mu.Unlock()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBody))
	_ = resp.Body.Close()
	mu.Lock()
	if wrote.IsZero() || first.IsZero() || first.Before(wrote) {
		return 0, reused, model.ReasonOther, fmt.Errorf("https: incomplete trace")
	}
	return first.Sub(wrote), reused, 0, nil
}

// hostState is the current resolution of one hostname, shared by every
// series that probes it.
type hostState struct {
	name     string
	literal  bool
	cur      atomic.Pointer[resolution]
	onChange []func() // called when the address changes
}

// resolution: ip is valid when known; err is set when the last lookup
// failed (a previous ip is kept until a lookup succeeds again).
type resolution struct {
	ip  netip.Addr
	err error
}

var errNotResolved = errors.New("not resolved yet")

func newHostState(h string) *hostState {
	hs := &hostState{name: h}
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
	case c.ip.IsValid():
		return c.ip, nil
	}
	return netip.Addr{}, c.err
}

func (r *runner) resolve(ctx context.Context, hs *hostState) {
	if hs.literal {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, resolveTimeout)
	defer cancel()
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip4", hs.name)
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
		if prev != nil {
			next.ip = prev.ip // keep using the last good address
		}
		if prev == nil || prev.err == nil {
			r.o.Log.Warn("dns lookup failed", "host", hs.name, "err", err, "keeping", next.ip)
		}
	} else {
		next.ip = ips[0].Unmap()
		if prev != nil && prev.err != nil {
			r.o.Log.Info("dns lookup recovered", "host", hs.name)
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
