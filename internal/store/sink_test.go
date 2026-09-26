package store_test

import (
	"github.com/fuck-you-isp/fyisp/internal/store"
	"github.com/fuck-you-isp/fyisp/internal/trace"
)

// The store is the tracer's sink (checked here, not in package store, so
// that store never depends on trace).
var (
	_ trace.Sink = (*store.SQLite)(nil)
	_ trace.Sink = (*store.Fake)(nil)
)
