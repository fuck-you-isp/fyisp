//go:build !unix && !windows

package hops

import "errors"

func open() (Prober, error) {
	return nil, errors.New("TTL-limited ICMP is not supported on this OS")
}
