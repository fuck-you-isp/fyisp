//go:build windows

package s3test

import "golang.org/x/sys/windows"

// The socket errors as this OS reports them (syscall's ECONNREFUSED and
// the like are made-up values on Windows).
var (
	errRefused     = windows.WSAECONNREFUSED
	errReset       = windows.WSAECONNRESET
	errHostUnreach = windows.WSAEHOSTUNREACH
	errNetUnreach  = windows.WSAENETUNREACH
)
