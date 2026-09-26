//go:build tools

// Package deps pins every third-party dependency so parallel work never needs
// to edit go.mod. Only the lead changes go.mod/go.sum.
package deps

import (
	_ "github.com/cloudflare/cloudflared/client"
	_ "github.com/cloudflare/cloudflared/config"
	_ "github.com/cloudflare/cloudflared/connection"
	_ "github.com/cloudflare/cloudflared/features"
	_ "github.com/cloudflare/cloudflared/ingress"
	_ "github.com/cloudflare/cloudflared/ingress/origins"
	_ "github.com/cloudflare/cloudflared/orchestration"
	_ "github.com/cloudflare/cloudflared/signal"
	_ "github.com/cloudflare/cloudflared/supervisor"
	_ "github.com/cloudflare/cloudflared/tlsconfig"
	_ "github.com/klauspost/compress/zstd"
	_ "github.com/prometheus/client_golang/prometheus"
	_ "github.com/prometheus/client_golang/prometheus/promhttp"
	_ "golang.org/x/crypto/x509roots/fallback"
	_ "golang.org/x/net/icmp"
	_ "golang.org/x/sys/windows"
	_ "gopkg.in/yaml.v3"
	_ "modernc.org/sqlite"
)
