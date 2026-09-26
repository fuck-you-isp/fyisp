package tunnel

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
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
	defaultRetries            = 5
	defaultGracePeriod        = 30 * time.Second
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
func setupProcessGlobals() {
	globalsOnce.Do(func() {
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
)

// runSupervisor runs one cloudflared supervisor for qt until ctx is done, the
// watchdog fires or the supervisor gives up.
func (t *tunnel) runSupervisor(ctx context.Context, qt *quickTunnel, w *watchdog) (stopReason, error) {
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
			t.update(func(s *State) {
				s.Phase, s.Protocol, s.Location, s.LastErr = Connected, e.Protocol.String(), e.Location, ""
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
			t.update(func(s *State) { s.Phase, s.Protocol, s.Location = Reconnecting, "", "" })
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

	tc, orch, err := buildConfig(sctx, qt, t.opts, &zl)
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

	tick := time.NewTicker(min(max(t.opts.ReprovisionAfter/10, 100*time.Millisecond), 5*time.Second))
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

// buildConfig mirrors cloudflared's prepareTunnelConfig + StartServer for a
// quick tunnel, minus the CLI, metrics server, management, ICMP and updater.
func buildConfig(ctx context.Context, qt *quickTunnel, o Options, zl *zerolog.Logger) (*supervisor.TunnelConfig, *orchestration.Orchestrator, error) {
	fs, err := features.NewFeatureSelector(ctx, qt.AccountTag, nil, false, zl)
	if err != nil {
		return nil, nil, fmt.Errorf("feature selector: %w", err)
	}
	cc, err := client.NewConfig(CloudflaredVersion, runtime.GOOS+"_"+runtime.GOARCH, fs)
	if err != nil {
		return nil, nil, err
	}
	ps, err := connection.NewProtocolSelector(o.Protocol, zl)
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
	rules, err := ingress.ParseIngress(&config.Configuration{
		Ingress: []config.UnvalidatedIngressRule{{Service: o.OriginURL}},
	})
	if err != nil {
		return nil, nil, fmt.Errorf("ingress: %w", err)
	}
	warp := ingress.NewWarpRoutingConfig(&config.WarpRoutingConfig{})
	dialer := ingress.NewOriginDialer(ingress.OriginConfig{DefaultDialer: ingress.NewDialer(warp)}, zl)
	// Supervisor.Run calls OriginDNSService.StartRefreshLoop: it must be
	// non-nil. The service only answers WARP private-network DNS, which quick
	// tunnels never carry, so use the static variant: its refresh loop returns
	// at once. (The dynamic one has an upstream data race in
	// origins.(*resolver).peekDial, flagged by -race, and polls DNS forever.)
	dns := origins.NewStaticDNSResolverService([]netip.AddrPort{netip.MustParseAddrPort("127.0.0.1:53")},
		origins.NewDNSDialer(), zl, origins.NewMetrics(prometheus.NewRegistry()))
	dialer.AddReservedService(dns, []netip.AddrPort{origins.VirtualDNSServiceAddr})

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
		ConfigurationFlags:  map[string]string{"protocol": o.Protocol},
	}, tags, nil, zl)
	if err != nil {
		return nil, nil, fmt.Errorf("orchestrator: %w", err)
	}
	return tc, orch, nil
}
