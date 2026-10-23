package s3test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"os"
	"regexp"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/probe"
)

// Class is how an attempt ended: ClassOK, or a failure. It is stored and
// served, never an error's text (a net.OpError names the local address).
type Class string

// Classes, by how far the attempt got.
const (
	ClassOK Class = "ok"

	// network: the line, the path or the provider's front end
	ClassDNSFail             Class = "dns_fail"
	ClassDNSNoIPv4           Class = "dns_no_ipv4"
	ClassConnectTimeout      Class = "connect_timeout"
	ClassConnectRefused      Class = "connect_refused"
	ClassConnectReset        Class = "connect_reset"
	ClassConnectUnreachable  Class = "connect_unreachable"
	ClassTLSTimeout          Class = "tls_timeout"
	ClassTLSReset            Class = "tls_reset"
	ClassTLSError            Class = "tls_error"
	ClassResetBeforeResponse Class = "reset_before_response"
	ClassTimeoutResponse     Class = "timeout_response"
	ClassConnClosed          Class = "conn_closed"
	ClassResetMidTransfer    Class = "reset_mid_transfer"
	ClassStall               Class = "stall"
	ClassShortBody           Class = "short_body"
	ClassTimeoutTotal        Class = "timeout_total"

	// provider: the service answered with an error
	ClassHTTP5xx   Class = "http_5xx"
	ClassThrottled Class = "throttled"

	// config: the target's settings, the keys or this machine, not the network
	ClassHTTPAuth     Class = "http_auth"
	ClassClockSkew    Class = "clock_skew"
	ClassTLSCert      Class = "tls_cert"
	ClassHTTPRedirect Class = "http_redirect"
	ClassHTTP4xx      Class = "http_4xx"
	ClassAddrRefused  Class = "addr_refused"
)

// Groups of classes.
const (
	GroupOK       = "ok"
	GroupNetwork  = "network"
	GroupProvider = "provider"
	GroupConfig   = "config"
)

// Classes is every class with its group, in a stable order (the docs and
// the UI's wording are checked against it).
var Classes = []struct {
	Class Class
	Group string
}{
	{ClassOK, GroupOK},
	{ClassDNSFail, GroupNetwork}, {ClassDNSNoIPv4, GroupNetwork},
	{ClassConnectTimeout, GroupNetwork}, {ClassConnectRefused, GroupNetwork}, {ClassConnectReset, GroupNetwork}, {ClassConnectUnreachable, GroupNetwork},
	{ClassTLSTimeout, GroupNetwork}, {ClassTLSReset, GroupNetwork}, {ClassTLSError, GroupNetwork},
	{ClassResetBeforeResponse, GroupNetwork}, {ClassTimeoutResponse, GroupNetwork}, {ClassConnClosed, GroupNetwork},
	{ClassResetMidTransfer, GroupNetwork}, {ClassStall, GroupNetwork}, {ClassShortBody, GroupNetwork}, {ClassTimeoutTotal, GroupNetwork},
	{ClassHTTP5xx, GroupProvider}, {ClassThrottled, GroupProvider},
	{ClassHTTPAuth, GroupConfig}, {ClassClockSkew, GroupConfig}, {ClassTLSCert, GroupConfig}, {ClassHTTPRedirect, GroupConfig},
	{ClassHTTP4xx, GroupConfig}, {ClassAddrRefused, GroupConfig},
}

// Group is c's group ("" for an unknown class).
func (c Class) Group() string {
	for _, x := range Classes {
		if x.Class == c {
			return x.Group
		}
	}
	return ""
}

// step is how far an attempt got when it ended.
type step int

const (
	stepDNS step = iota
	stepDial
	stepTLS
	stepWrite // the request line, headers and body
	stepWait  // waiting for the response
	stepBody  // reading the response body
)

// errNoIPv4: the name has no A record (fyisp tests over IPv4).
var errNoIPv4 = errors.New("no IPv4 address")

// errAddrRefused: the address is one fyisp never sends a request to
// (link-local, multicast, ...), or doesn't match the target's network.
var errAddrRefused = errors.New("address refused")

// errStall: no body bytes for StallTimeout (or a request write that
// blocked as long); errTotal: the transfer's time bound passed;
// errConnClosed: the server dropped a kept connection without answering;
// errCancelled: the attempt's context ended (not recorded).
var (
	errStall      = errors.New("stalled")
	errTotal      = errors.New("transfer took too long")
	errConnClosed = errors.New("connection closed")
	errCancelled  = errors.New("cancelled")
)

// classifyErr maps a failed attempt's error, by the step it failed at.
func classifyErr(st step, err error) Class {
	switch {
	case errors.Is(err, errAddrRefused):
		return ClassAddrRefused
	case errors.Is(err, errNoIPv4):
		return ClassDNSNoIPv4
	case errors.Is(err, errStall):
		return ClassStall
	case errors.Is(err, errTotal):
		return ClassTimeoutTotal
	case errors.Is(err, errConnClosed):
		return ClassConnClosed
	}
	timeout := errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded)
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		timeout = true
	}
	reason := probe.Classify(err)
	reset := reason == model.ReasonReset || errors.Is(err, io.EOF)
	switch st {
	case stepDNS:
		return ClassDNSFail
	case stepDial:
		switch {
		case timeout:
			return ClassConnectTimeout
		case reason == model.ReasonRefused:
			return ClassConnectRefused
		case reason == model.ReasonUnreachable || reason == model.ReasonNoNetwork:
			return ClassConnectUnreachable
		}
		return ClassConnectReset
	case stepTLS:
		switch {
		case isCertErr(err):
			return ClassTLSCert
		case timeout:
			return ClassTLSTimeout
		case reset:
			return ClassTLSReset
		}
		return ClassTLSError
	case stepWrite:
		if timeout {
			return ClassStall // the server stopped reading the request
		}
		return ClassResetBeforeResponse
	case stepWait:
		if timeout {
			return ClassTimeoutResponse
		}
		return ClassResetBeforeResponse
	}
	switch {
	case timeout:
		return ClassStall
	case errors.Is(err, io.ErrUnexpectedEOF):
		return ClassShortBody
	}
	return ClassResetMidTransfer
}

func isCertErr(err error) bool {
	var (
		cv *tls.CertificateVerificationError
		ua x509.UnknownAuthorityError
		he x509.HostnameError
		ci x509.CertificateInvalidError
	)
	return errors.As(err, &cv) || errors.As(err, &ua) || errors.As(err, &he) || errors.As(err, &ci)
}

// skewLimit is how far off a server's clock may be before a refusal counts
// as clock_skew rather than an auth error (S3 refuses at 15 minutes).
const skewLimit = 14 * 60

// classifyStatus maps a response; signed reports a request with keys
// (without keys, any answer means the endpoint is up). A 503 is throttling
// only when the server says so (SlowDown, or a Retry-After); otherwise it
// is the service failing, like any 5xx.
func classifyStatus(status int, s3code string, signed bool, skew *int, retryAfter bool) Class {
	switch {
	case status >= 200 && status < 300:
		return ClassOK
	case !signed && status < 500:
		return ClassOK // connection-only: the endpoint answered
	case s3code == "RequestTimeTooSkewed", status == 403 && skew != nil && (*skew > skewLimit || *skew < -skewLimit):
		return ClassClockSkew
	case status == 429 || s3code == "SlowDown" || (status == 503 && retryAfter):
		return ClassThrottled
	case status >= 500:
		return ClassHTTP5xx
	case status >= 300 && status < 400:
		return ClassHTTPRedirect
	case status == 401 || status == 403 || authCodes[s3code]:
		return ClassHTTPAuth
	}
	return ClassHTTP4xx
}

// authCodes are the error codes that mean the keys, the token or the
// region (part of the signature) are wrong, whatever the status.
var authCodes = map[string]bool{"SignatureDoesNotMatch": true, "InvalidAccessKeyId": true, "AccessDenied": true,
	"InvalidTokenId": true, "ExpiredToken": true, "AuthorizationHeaderMalformed": true, "InvalidSecurity": true}

var (
	s3CodeRe = regexp.MustCompile(`^[A-Za-z]{1,64}$`)
	reqIDRe  = regexp.MustCompile(`^[A-Za-z0-9+/=_-]{1,200}$`)
	// quadRe is four decimal numbers between separators: an address written
	// "203-0-113-45" (no '.' or ':' get through reqIDRe). An address encoded
	// another way (hex, base64) can't be told from an ID.
	quadRe = regexp.MustCompile(`(^|[^0-9])\d{1,3}[-_+=/]\d{1,3}[-_+=/]\d{1,3}[-_+=/]\d{1,3}([^0-9]|$)`)
)

// requestIDHeaders are read, first match wins, for a failure's request ID
// (vendor-neutral: what S3-compatible servers send).
var requestIDHeaders = []string{"x-amz-request-id", "x-request-id"}
