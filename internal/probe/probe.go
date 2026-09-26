// Package probe measures targets over HTTPS, TCP and ICMP without elevated
// privileges and reports results to a model.Sink.
package probe

import (
	"context"
	"log/slog"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
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
	Timeout      time.Duration // per probe, default 1s for TCP/ICMP, 5s for HTTPS
	ResolveEvery time.Duration // default 15m
	RetryResolve time.Duration // default 10s for unresolved hosts
	Now          func() time.Time
	Log          *slog.Logger
}

// Runner probes every target of a profile until ctx is cancelled.
type Runner interface {
	Run(ctx context.Context, p *model.Profile, sink model.Sink) error
	Caps() Caps
}
