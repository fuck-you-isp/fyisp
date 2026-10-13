//go:build !linux && !windows && !darwin

package netinfo

import "errors"

func defaultRoute() (route, error) {
	return route{}, errors.New("gateway discovery is not supported on this OS")
}
