package speedtest

import (
	"errors"
	"syscall"
)

// wsaeconnrefused is WSAECONNREFUSED.
const wsaeconnrefused = 10061

func isRefused(err error) bool {
	var e syscall.Errno
	return errors.Is(err, syscall.ECONNREFUSED) || (errors.As(err, &e) && e == wsaeconnrefused)
}
