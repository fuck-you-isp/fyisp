package probe

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"os"
	"strings"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

// classify maps a probe error to a loss reason. Socket errnos are checked
// first so that, e.g., a reset during the TLS handshake counts as a reset.
func classify(err error) model.Reason {
	if err == nil {
		return model.ReasonOther
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return model.ReasonDNS
	}
	if r, ok := errnoReason(err); ok {
		return r
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) {
		return model.ReasonTimeout
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return model.ReasonTimeout
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return model.ReasonReset
	}
	if isTLS(err) {
		return model.ReasonTLS
	}
	return model.ReasonOther
}

func isTLS(err error) bool {
	var (
		rh    tls.RecordHeaderError
		alert tls.AlertError
		cv    *tls.CertificateVerificationError
		ua    x509.UnknownAuthorityError
		he    x509.HostnameError
		ci    x509.CertificateInvalidError
	)
	switch {
	case errors.As(err, &rh), errors.As(err, &alert), errors.As(err, &cv),
		errors.As(err, &ua), errors.As(err, &he), errors.As(err, &ci):
		return true
	}
	return strings.Contains(err.Error(), "tls: ") || strings.Contains(err.Error(), "x509: ")
}
