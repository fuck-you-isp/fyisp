//go:build !unix && !windows

package netinfo

import "errors"

func openTracer() (tracer, error) {
	return nil, errors.New("edge discovery is not supported on this OS")
}
