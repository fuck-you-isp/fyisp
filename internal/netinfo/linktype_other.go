//go:build !linux

package netinfo

// LinkType is "unknown" outside Linux (no portable way yet).
func LinkType(string) string { return "unknown" }
