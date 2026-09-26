// Package probe measures targets over HTTPS, TCP and ICMP without elevated
// privileges and reports results to a model.Sink.
package probe

import (
	"context"
	"crypto/tls"
	"log/slog"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/netinfo"
)

// Caps reports which probe kinds work on this machine.
type Caps struct {
	ICMP  string `json:"icmp"` // "udp", "raw", "iphlpapi" or "unavailable"
	TCP   bool   `json:"tcp"`
	HTTPS bool   `json:"https"`
	// ICMPHint explains how to enable ICMP when it is unavailable.
	ICMPHint string `json:"icmp_hint,omitempty"`
}

// Options configures a Runner.
type Options struct {
	Timeout      time.Duration    // per probe; when zero: 3s for TCP/ICMP (at most the interval), 5s for HTTPS; when set: all kinds
	ResolveEvery time.Duration    // default 15m
	RetryResolve time.Duration    // default 10s for unresolved hosts
	Now          func() time.Time // wall clock for slot assignment; default time.Now
	Log          *slog.Logger     // default slog.Default()
	UserAgent    string           // HTTPS User-Agent, default "fyisp"
	TLSConfig    *tls.Config      // HTTPS client TLS config (tests: custom roots); cloned per target
	// Path resolves the special hosts model.HostGateway and model.HostEdge
	// (typically netinfo.Watcher.Current), on every probe. Targets with a
	// special host are probed over ICMP only. When Path is nil, or the
	// address is not known yet, their slots are not measured; while there is
	// no default route they are lost with model.ReasonNoNetwork.
	Path func() netinfo.Path
}

// Runner probes every target of a profile until ctx is cancelled.
type Runner interface {
	Run(ctx context.Context, p *model.Profile, sink model.Sink) error
	Caps() Caps
}
