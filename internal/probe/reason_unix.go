//go:build unix

package probe

import (
	"errors"
	"syscall"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

func errnoReason(err error) (model.Reason, bool) {
	var e syscall.Errno
	if !errors.As(err, &e) {
		return 0, false
	}
	switch e {
	case syscall.ECONNREFUSED:
		return model.ReasonRefused, true
	case syscall.ECONNRESET, syscall.ECONNABORTED, syscall.EPIPE:
		return model.ReasonReset, true
	case syscall.EHOSTUNREACH, syscall.EHOSTDOWN:
		return model.ReasonUnreachable, true
	case syscall.ENETUNREACH, syscall.ENETDOWN, syscall.EADDRNOTAVAIL:
		return model.ReasonNoNetwork, true
	case syscall.ETIMEDOUT:
		return model.ReasonTimeout, true
	}
	return 0, false
}
