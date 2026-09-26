// Command fyisp is a single-binary internet quality monitor.
package main

import (
	"fmt"

	// Linked for now so the binary size tracks every dependency.
	_ "github.com/cloudflare/cloudflared/supervisor"
	_ "github.com/klauspost/compress/zstd"
	_ "golang.org/x/crypto/x509roots/fallback"
	_ "golang.org/x/net/icmp"
	_ "modernc.org/sqlite"
)

var version = "dev"

func main() { fmt.Println("fyisp", version) }
