package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"sync/atomic"

	"github.com/fuck-you-isp/fyisp/internal/tunnel"
)

// dashTunnel is --tunnel: the dashboard on a Cloudflare quick tunnel, run
// in-process (internal/tunnel).
type dashTunnel struct {
	host atomic.Value // string: the tunnel's latest host name, "" before the first
	t    *tunnel.Tunnel
}

// newDashTunnel is a tunnel not started yet: its Host is "" until start.
func newDashTunnel() *dashTunnel {
	d := &dashTunnel{}
	d.host.Store("")
	return d
}

// startTunnel is tunnel.Start (a variable: tests).
var startTunnel = tunnel.Start

// start publishes the dashboard listening on localAddr and prints each new
// link once it is connected, and, with a generated admin token, the private
// link that also turns on the speed-test controls in that browser.
func (d *dashTunnel) start(ctx context.Context, localAddr, adminTok string, adminStored bool, log *slog.Logger) error {
	t, err := startTunnel(ctx, tunnel.Options{
		Origin: tunnelOrigin(localAddr),
		Log:    log,
		OnURL: func(u string) {
			if p, err := url.Parse(u); err == nil {
				d.host.Store(p.Hostname())
			}
			link := u + "/"
			log.Info("dashboard over HTTPS, through a Cloudflare quick tunnel: anyone with the link can open it", "url", link)
			fmt.Fprintf(os.Stderr, "\n  Dashboard over HTTPS:  %s\n  Anyone with this link can open the dashboard. It changes when the tunnel is replaced.\n", link)
			if adminStored {
				fmt.Fprintf(os.Stderr, "  For the speed-test controls there too, open this link once (keep it private):\n  %s\n", adminLink(link, adminTok))
			}
			fmt.Fprintln(os.Stderr)
		},
	})
	if err != nil {
		return err
	}
	d.t = t
	return nil
}

// Host is the tunnel's latest host name (web.LocalOptions.TunnelHost). It
// stays allowed while the tunnel reconnects, until a replacement's name is
// known; Cloudflare no longer routes it by then.
func (d *dashTunnel) Host() string {
	h, _ := d.host.Load().(string)
	return h
}

// Done is closed once the tunnel has stopped.
func (d *dashTunnel) Done() <-chan struct{} { return d.t.Done() }

// tunnelOrigin is the URL the tunnel forwards to: the dashboard's listen
// address, a wildcard one as 127.0.0.1; an IPv6 zone (fe80::1%eth0) is
// escaped as a URL needs it.
func tunnelOrigin(localAddr string) string {
	return "http://" + strings.Replace(displayAddr(localAddr), "%", "%25", 1)
}
