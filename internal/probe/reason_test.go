package probe

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"testing"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

func TestClassify(t *testing.T) {
	op := func(err error) error {
		return &url.Error{Op: "Get", URL: "https://x", Err: &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", err)}}
	}
	cases := []struct {
		err  error
		want model.Reason
	}{
		{&net.DNSError{Err: "no such host", Name: "x.invalid", IsNotFound: true}, model.ReasonDNS},
		{op(errRefused), model.ReasonRefused},
		{op(errReset), model.ReasonReset},
		{op(errHostUnreach), model.ReasonUnreachable},
		{op(errNetUnreach), model.ReasonNoNetwork},
		{fmt.Errorf("x: %w", context.DeadlineExceeded), model.ReasonTimeout},
		{&url.Error{Op: "Get", URL: "u", Err: os.ErrDeadlineExceeded}, model.ReasonTimeout},
		{&url.Error{Op: "Get", URL: "u", Err: io.EOF}, model.ReasonReset},
		{tls.AlertError(40), model.ReasonTLS},
		{&tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}, model.ReasonTLS},
		{errors.New("tls: first record does not look like a TLS handshake"), model.ReasonTLS},
		{errors.New("something else"), model.ReasonOther},
	}
	for _, c := range cases {
		if got := classify(c.err); got != c.want {
			t.Errorf("classify(%v) = %s, want %s", c.err, got, c.want)
		}
	}
}
