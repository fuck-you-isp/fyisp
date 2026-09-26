// Command fyisp is a single-binary internet quality monitor.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	// Spike S1: link every heavy dependency to measure binary size and
	// cross-compile coverage. Replaced by real wiring after the spikes.
	_ "github.com/klauspost/compress/zstd"
	_ "golang.org/x/crypto/x509roots/fallback"
	_ "golang.org/x/net/icmp"
	_ "modernc.org/sqlite"

	"github.com/fuck-you-isp/fyisp/internal/tunnel"
)

var version = "dev"

func main() {
	// Spike S2: `fyisp spike-tunnel <origin-url> [auto|quic|http2]` shares a
	// local origin on a quick tunnel until SIGINT/SIGTERM.
	if len(os.Args) >= 3 && os.Args[1] == "spike-tunnel" {
		os.Exit(spikeTunnel(os.Args[2], os.Args[3:]))
	}
	fmt.Println("fyisp", version)
}

func spikeTunnel(origin string, rest []string) int {
	proto := "auto"
	if len(rest) > 0 {
		proto = rest[0]
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	t := tunnel.New(tunnel.Options{OriginURL: origin, Protocol: proto, Log: log})
	if err := t.Run(ctx); err != nil {
		log.Error("tunnel", "err", err)
		return 1
	}
	return 0
}
