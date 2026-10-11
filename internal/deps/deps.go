//go:build tools

// Package deps pins every third-party dependency so parallel work never needs
// to edit go.mod. Only the lead changes go.mod/go.sum.
package deps

import (
	_ "github.com/klauspost/compress/zstd"
	_ "github.com/prometheus/client_golang/prometheus"
	_ "github.com/prometheus/client_golang/prometheus/promhttp"
	_ "golang.org/x/crypto/x509roots/fallback"
	_ "golang.org/x/net/icmp"
	_ "golang.org/x/sys/windows"
	_ "gopkg.in/yaml.v3"
	_ "modernc.org/sqlite"
)
