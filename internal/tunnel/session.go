package tunnel

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net"
	"net/netip"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cloudflare/cloudflared/client"
	"github.com/cloudflare/cloudflared/config"
	"github.com/cloudflare/cloudflared/connection"
	"github.com/cloudflare/cloudflared/edgediscovery/allregions"
	"github.com/cloudflare/cloudflared/features"
	"github.com/cloudflare/cloudflared/ingress"
	"github.com/cloudflare/cloudflared/ingress/origins"
	"github.com/cloudflare/cloudflared/orchestration"
	"github.com/cloudflare/cloudflared/signal"
	"github.com/cloudflare/cloudflared/supervisor"
	"github.com/cloudflare/cloudflared/tlsconfig"
	"github.com/cloudflare/cloudflared/tunnelrpc/pogs"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"
)

// Defaults copied from cloudflared's tunnel command flags (cmd/cloudflared/tunnel/cmd.go).
const (
	defaultRetries = 5
	// gracePeriod is how long an unregistered connection lingers for
	// in-flight requests (cloudflared's default is 30 s): the dashboard
	// drains its own requests at shutdown, so a short one lets the tunnel
	// stop within gracefulStopTimeout.
	defaultGracePeriod        = time.Second
	defaultMaxEdgeAddrRetries = 8
	defaultRPCTimeout         = 5 * time.Second
	defaultQUICConnFlowLimit  = 30 << 20
	defaultQUICStreamFlowLim  = 6 << 20
	haConnections             = 1 // quick tunnels use a single connection upstream too

	// On shutdown, ask the edge to unregister gracefully for at most this long
	// before cancelling the connection.
	gracefulStopTimeout = 5 * time.Second
	// barrierIndex is a connection index cloudflared never uses (HA <= 4);
	// an event with it marks "everything sent before this has been dispatched".
	barrierIndex uint8 = 255
)

// ---- process-wide cloudflared state -----------------------------------------

type noopRegisterer struct{}

func (noopRegisterer) Register(prometheus.Collector) error  { return nil }
func (noopRegisterer) MustRegister(...prometheus.Collector) {}
func (noopRegisterer) Unregister(prometheus.Collector) bool { return true }

var (
	globalsOnce sync.Once
	obs         *connection.Observer

	// activeLog is where process-wide cloudflared logs (the observer) go.
	activeLog atomic.Pointer[slogLogger]

	sinkMu   sync.Mutex
	sinkFn   func(connection.Event)
	barrierC = make(chan struct{}, 1)
)

// setupProcessGlobals runs once, before the first NewSupervisor:
//
//   - supervisor.NewSupervisor calls v3.NewMetrics(prometheus.DefaultRegisterer)
//     which MustRegisters fixed collectors, so a second supervisor panics
//     with a duplicate registration. Point DefaultRegisterer at a no-op, once,
//     and never swap it back. (Collectors registered by cloudflared package
//     init() functions already sit in the real default registry; fyisp must
//     serve its own registry, not the default one.)
//   - connection.NewObserver starts a dispatch goroutine that never exits, so
//     one observer is shared by every session in the process. Its sink fans
//     out to the current session only.
//   - quic-go prints a one-off UDP buffer size warning through the standard
//     log package (bypassing slog); silence it unless the user set the knob.
//   - cloudflared's HTTP/2 server and quic-go write other errors through the
//     standard log package too, quoting addresses: mask them (redactWriter)
//     in whatever the standard log writes to from now on.
func setupProcessGlobals() {
	globalsOnce.Do(func() {
		log.SetOutput(redactWriter{log.Writer()})
		if _, ok := os.LookupEnv("QUIC_GO_DISABLE_RECEIVE_BUFFER_WARNING"); !ok {
			_ = os.Setenv("QUIC_GO_DISABLE_RECEIVE_BUFFER_WARNING", "true")
		}
		prometheus.DefaultRegisterer = noopRegisterer{}
		zl := newZerolog(func() *slogLogger { return activeLog.Load() }, nil)
		obs = connection.NewObserver(&zl)
		obs.RegisterSink(connection.EventSinkFunc(dispatch))
	})
}

func dispatch(e connection.Event) {
	if e.Index == barrierIndex && e.EventType == connection.Reconnecting {
		select {
		case barrierC <- struct{}{}:
		default:
		}
		return
	}
	sinkMu.Lock()
	defer sinkMu.Unlock()
	if sinkFn != nil {
		sinkFn(e)
	}
}

func attachSink(fn func(connection.Event)) {
	sinkMu.Lock()
	defer sinkMu.Unlock()
	sinkFn = fn
}

// detachSink waits until every event the finished supervisor queued on the
// shared observer has been handled, then stops routing events to fn.
func detachSink() {
	select {
	case <-barrierC:
	default:
	}
	obs.SendReconnect(barrierIndex)
	select {
	case <-barrierC:
	case <-time.After(2 * time.Second): // observer buffer was full; give up
	}
	attachSink(nil)
}

// ---- one supervisor run ------------------------------------------------------

type stopReason int

const (
	stopCtx      stopReason = iota // Run's context was cancelled
	stopWatchdog                   // no connection for ReprovisionAfter
	stopExited                     // cloudflared's supervisor returned by itself
	stopFallback                   // "auto" found no connection: retry with http2
)

// runSupervisor runs one cloudflared supervisor for qt with protocol proto
// until ctx is done, the watchdog fires, the fast fallback fires (proto
// "auto" only) or the supervisor gives up.
func (t *tunnel) runSupervisor(ctx context.Context, qt *quickTunnel, w *watchdog, proto string) (stopReason, error) {
	var (
		stopping    atomic.Bool
		conns       = map[uint8]bool{}
		connectedAt atomic.Int64
	)
	// While we stop the supervisor on purpose its "Connection terminated"
	// errors are expected: demote them to debug.
	zl := newZerolog(func() *slogLogger { return t.opts.Log }, &stopping)

	// The session context is detached from ctx so a graceful unregister can
	// still talk to the edge after ctx is cancelled.
	sctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	defer cancel()
	attachSink(func(e connection.Event) {
		switch e.EventType {
		case connection.Connected:
			if !conns[e.Index] {
				conns[e.Index] = true
				w.connected()
			}
			connectedAt.Store(time.Now().UnixNano())
			t.update(func(s *state) {
				s.Phase, s.Protocol, s.Location, s.LastErr = phaseConnected, e.Protocol.String(), e.Location, ""
			})
		case connection.Disconnected, connection.Reconnecting, connection.Unregistering:
			if !conns[e.Index] {
				return
			}
			conns[e.Index] = false
			w.disconnected()
			if w.active() > 0 || stopping.Load() {
				return // still other connections up (HA>1) or shutting down
			}
			t.update(func(s *state) { s.Phase, s.Protocol, s.Location = phaseReconnecting, "", "" })
		}
	})
	defer func() {
		detachSink()
		for _, up := range conns { // safe: sink detached
			if up {
				w.disconnected()
			}
		}
	}()

	tc, orch, err := buildConfig(sctx, qt, t.opts.Origin, proto, &zl)
	if err != nil {
		return stopExited, err
	}
	graceC := make(chan struct{})
	sup, err := supervisor.NewSupervisor(tc, orch, graceC)
	if err != nil {
		return stopExited, fmt.Errorf("start tunnel supervisor: %w", err)
	}
	errC := make(chan error, 1)
	go func() { errC <- sup.Run(sctx, signal.New(make(chan struct{}))) }()

	period := min(max(t.cfg.ReprovisionAfter/10, 100*time.Millisecond), 5*time.Second)
	if proto == "auto" && t.cfg.FallbackAfter > 0 {
		period = min(period, max(t.cfg.FallbackAfter/10, 100*time.Millisecond), time.Second)
	}
	tick := time.NewTicker(period)
	defer tick.Stop()
	for {
		select {
		case err := <-errC:
			if err == nil {
				err = errors.New("tunnel supervisor stopped")
			}
			return stopExited, err
		case <-tick.C:
			if w.expired() {
				stopping.Store(true)
				cancel()
				<-errC
				return stopWatchdog, nil
			}
			if fallbackDue(proto, t.cfg.FallbackAfter, w.everUp(), w.active(), w.downFor()) {
				stopping.Store(true)
				cancel()
				<-errC
				return stopFallback, nil
			}
		case <-ctx.Done():
			stopping.Store(true)
			// Unregister gracefully only once the supervisor is surely past its
			// initial connect: closing graceC while cloudflared's
			// Supervisor.initialize is still waiting leaks its first-tunnel
			// goroutine (it never drains tunnelErrors on early shutdown).
			if at := connectedAt.Load(); at != 0 && time.Since(time.Unix(0, at)) > time.Second {
				close(graceC)
				select {
				case <-errC:
					return stopCtx, nil
				case <-time.After(gracefulStopTimeout):
				}
			}
			cancel()
			<-errC
			return stopCtx, nil
		}
	}
}

// errNoPrivateRouting refuses every private-network flow (refuseDialer).
var errNoPrivateRouting = errors.New("tunnel: private-network routing is off")

// refuseDialer is the origin dialer for everything but the published
// origin: it dials nothing.
type refuseDialer struct{}

func (refuseDialer) DialTCP(context.Context, netip.AddrPort) (net.Conn, error) {
	return nil, errNoPrivateRouting
}
func (refuseDialer) DialUDP(netip.AddrPort) (net.Conn, error) { return nil, errNoPrivateRouting }

// buildConfig mirrors cloudflared's prepareTunnelConfig + StartServer for a
// quick tunnel, minus the CLI, metrics server, management, ICMP and updater.
func buildConfig(ctx context.Context, qt *quickTunnel, originURL, proto string, zl *zerolog.Logger) (*supervisor.TunnelConfig, *orchestration.Orchestrator, error) {
	fs, err := features.NewFeatureSelector(ctx, qt.AccountTag, nil, false, zl)
	if err != nil {
		return nil, nil, fmt.Errorf("feature selector: %w", err)
	}
	cc, err := client.NewConfig(CloudflaredVersion, runtime.GOOS+"_"+runtime.GOARCH, fs)
	if err != nil {
		return nil, nil, err
	}
	ps, err := connection.NewProtocolSelector(proto, zl)
	if err != nil {
		return nil, nil, err
	}
	edgeTLS := make(map[connection.Protocol]*tls.Config, len(connection.ProtocolList))
	for _, p := range connection.ProtocolList {
		ts := p.TLSSettings()
		c, err := tlsconfig.CreateTunnelConfig("", ts.ServerName)
		if err != nil {
			return nil, nil, fmt.Errorf("edge TLS config: %w", err)
		}
		if len(ts.NextProtos) > 0 {
			c.NextProtos = ts.NextProtos
		}
		edgeTLS[p] = c
	}

	// An http.Handler cannot be plugged in: the origin must be a URL.
	//
	// Never set OriginRequest.HTTPHostHeader: the origin's host allowlist and
	// same-origin check rely on cloudflared passing the visitor's Host (the
	// trycloudflare hostname) through unchanged. TestLiveSameOrigin checks it.
	rules, err := ingress.ParseIngress(&config.Configuration{
		Ingress: []config.UnvalidatedIngressRule{{Service: originURL}},
	})
	if err != nil {
		return nil, nil, fmt.Errorf("ingress: %w", err)
	}
	warp := ingress.NewWarpRoutingConfig(&config.WarpRoutingConfig{})
	// Private-network routing (TCP and UDP flows the edge asks for, to any
	// address, and the virtual DNS service) is refused: only the origin
	// above is published, and the tunnel never reaches the LAN. The HTTP
	// origin dials with its own transport, not this dialer.
	dialer := ingress.NewOriginDialer(ingress.OriginConfig{DefaultDialer: refuseDialer{}}, zl)
	// Supervisor.Run calls OriginDNSService.StartRefreshLoop: it must be
	// non-nil, though it is never reserved on the dialer. Use the static
	// variant: its refresh loop returns at once. (The dynamic one has an
	// upstream data race in origins.(*resolver).peekDial, flagged by -race,
	// and polls DNS forever.)
	dns := origins.NewStaticDNSResolverService([]netip.AddrPort{netip.MustParseAddrPort("127.0.0.1:53")},
		origins.NewDNSDialer(), zl, origins.NewMetrics(prometheus.NewRegistry()))

	tags := []pogs.Tag{{Name: "ID", Value: cc.ConnectorID.String()}}
	tc := &supervisor.TunnelConfig{
		ClientConfig:       cc,
		GracePeriod:        defaultGracePeriod,
		EdgeIPVersion:      allregions.Auto,
		HAConnections:      haConnections,
		Tags:               tags,
		Log:                zl,
		Observer:           obs,
		ReportedVersion:    CloudflaredVersion,
		Retries:            defaultRetries,
		MaxEdgeAddrRetries: defaultMaxEdgeAddrRetries,
		NamedTunnel: &connection.TunnelProperties{
			Credentials: connection.Credentials{
				AccountTag:   qt.AccountTag,
				TunnelSecret: qt.Secret,
				TunnelID:     qt.tunnelID,
			},
			QuickTunnelUrl: qt.Hostname,
		},
		ProtocolSelector:                    ps,
		EdgeTLSConfigs:                      edgeTLS,
		ICMPRouterServer:                    nil, // disabled for quick tunnels upstream too
		OriginDNSService:                    dns,
		OriginDialerService:                 dialer,
		RPCTimeout:                          defaultRPCTimeout,
		QUICConnectionLevelFlowControlLimit: defaultQUICConnFlowLimit,
		QUICStreamLevelFlowControlLimit:     defaultQUICStreamFlowLim,
	}
	orch, err := orchestration.NewOrchestrator(ctx, &orchestration.Config{
		Ingress:             &rules,
		WarpRouting:         warp,
		OriginDialerService: dialer,
		ConfigurationFlags:  map[string]string{"protocol": proto},
	}, tags, nil, zl)
	if err != nil {
		return nil, nil, fmt.Errorf("orchestrator: %w", err)
	}
	return tc, orch, nil
}
