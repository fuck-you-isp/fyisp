//go:build unix

package s3test

import "syscall"

// The socket errors as this OS reports them.
var (
	errRefused     = syscall.ECONNREFUSED
	errReset       = syscall.ECONNRESET
	errHostUnreach = syscall.EHOSTUNREACH
	errNetUnreach  = syscall.ENETUNREACH
)
