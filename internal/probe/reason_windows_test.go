//go:build windows

package probe

import "golang.org/x/sys/windows"

var (
	errRefused     error = windows.WSAECONNREFUSED
	errReset       error = windows.WSAECONNRESET
	errHostUnreach error = windows.WSAEHOSTUNREACH
	errNetUnreach  error = windows.WSAENETUNREACH
)
