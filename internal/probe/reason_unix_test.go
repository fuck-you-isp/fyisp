//go:build unix

package probe

import "syscall"

var (
	errRefused     error = syscall.ECONNREFUSED
	errReset       error = syscall.ECONNRESET
	errHostUnreach error = syscall.EHOSTUNREACH
	errNetUnreach  error = syscall.ENETUNREACH
)
