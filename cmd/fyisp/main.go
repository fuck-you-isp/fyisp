// Command fyisp is a single-binary internet quality monitor.
package main

import (
	"fmt"

	// Spike S1: link every heavy dependency to measure binary size and
	// cross-compile coverage. Replaced by real wiring after the spikes.
	_ "github.com/cloudflare/cloudflared/supervisor"
	_ "github.com/klauspost/compress/zstd"
	_ "golang.org/x/crypto/x509roots/fallback"
	_ "golang.org/x/net/icmp"
	_ "modernc.org/sqlite"
)

var version = "dev"

func main() { fmt.Println("fyisp", version) }
