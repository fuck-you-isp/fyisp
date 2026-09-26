//go:build windows

package probe

import (
	"errors"
	"syscall"

	"golang.org/x/sys/windows"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

func errnoReason(err error) (model.Reason, bool) {
	var e syscall.Errno
	if !errors.As(err, &e) {
		return 0, false
	}
	switch e {
	case windows.WSAECONNREFUSED, windows.ERROR_CONNECTION_REFUSED:
		return model.ReasonRefused, true
	case windows.WSAECONNRESET, windows.WSAECONNABORTED:
		return model.ReasonReset, true
	case windows.WSAEHOSTUNREACH, windows.ERROR_HOST_UNREACHABLE:
		return model.ReasonUnreachable, true
	case windows.WSAENETUNREACH, windows.WSAENETDOWN, windows.ERROR_NETWORK_UNREACHABLE:
		return model.ReasonNoNetwork, true
	case windows.WSAETIMEDOUT:
		return model.ReasonTimeout, true
	}
	return 0, false
}
