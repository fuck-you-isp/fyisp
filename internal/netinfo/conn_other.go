//go:build !linux

package netinfo

import "github.com/fuck-you-isp/fyisp/internal/model"

// ConnType reports the connection type of iface. Only Linux is supported
// so far; elsewhere it is unknown.
func ConnType(iface string) string { return model.ConnUnknown }
