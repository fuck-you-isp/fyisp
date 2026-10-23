package s3test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/s3test/s3fake"
)

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func opErr(errno syscall.Errno) error {
	return &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", errno)}
}

// TestClassifyErr: every network class, from the step it failed at.
func TestClassifyErr(t *testing.T) {
	for _, c := range []struct {
		st   step
		err  error
		want Class
	}{
		{stepDNS, &net.DNSError{Err: "no such host", IsNotFound: true}, ClassDNSFail},
		{stepDNS, &net.DNSError{Err: "timeout", IsTimeout: true}, ClassDNSFail},
		{stepDNS, errNoIPv4, ClassDNSNoIPv4},
		{stepDial, errAddrRefused, ClassAddrRefused},
		{stepDial, timeoutErr{}, ClassConnectTimeout},
		{stepDial, opErr(errRefused), ClassConnectRefused},
		{stepDial, opErr(errReset), ClassConnectReset},
		{stepDial, opErr(errHostUnreach), ClassConnectUnreachable},
		{stepDial, opErr(errNetUnreach), ClassConnectUnreachable},
		{stepTLS, x509.UnknownAuthorityError{}, ClassTLSCert},
		{stepTLS, x509.HostnameError{Certificate: &x509.Certificate{}, Host: "h"}, ClassTLSCert},
		{stepTLS, timeoutErr{}, ClassTLSTimeout},
		{stepTLS, io.EOF, ClassTLSReset},
		{stepTLS, opErr(errReset), ClassTLSReset},
		{stepTLS, tls.AlertError(40), ClassTLSError},
		{stepWrite, timeoutErr{}, ClassStall},
		{stepWrite, errStall, ClassStall},
		{stepWrite, opErr(syscall.EPIPE), ClassResetBeforeResponse},
		{stepWrite, errTotal, ClassTimeoutTotal},
		{stepWait, timeoutErr{}, ClassTimeoutResponse},
		{stepWait, io.ErrUnexpectedEOF, ClassResetBeforeResponse},
		{stepWait, errConnClosed, ClassConnClosed},
		{stepBody, errStall, ClassStall},
		{stepBody, errTotal, ClassTimeoutTotal},
		{stepBody, io.ErrUnexpectedEOF, ClassShortBody},
		{stepBody, opErr(errReset), ClassResetMidTransfer},
		{stepBody, fmt.Errorf("wrapped: %w", timeoutErr{}), ClassStall},
	} {
		if got := classifyErr(c.st, c.err); got != c.want {
			t.Errorf("step %d, %v: %s, want %s", c.st, c.err, got, c.want)
		}
	}
}

func TestClassifyStatus(t *testing.T) {
	skew := func(s int) *int { return &s }
	for _, c := range []struct {
		status     int
		code       string
		signed     bool
		skew       *int
		retryAfter bool
		want       Class
	}{
		{200, "", true, nil, false, ClassOK},
		{206, "", true, nil, false, ClassOK},
		{403, "AccessDenied", false, nil, false, ClassOK}, // connection-only: it answered
		{501, "", false, nil, false, ClassHTTP5xx},
		{403, "SignatureDoesNotMatch", true, nil, false, ClassHTTPAuth},
		{403, "", true, skew(20 * 60), false, ClassClockSkew}, // a refusal with the clock far off
		{403, "", true, skew(-20 * 60), false, ClassClockSkew},
		{403, "", true, skew(60), false, ClassHTTPAuth},
		{400, "RequestTimeTooSkewed", true, nil, false, ClassClockSkew},
		{400, "AuthorizationHeaderMalformed", true, nil, false, ClassHTTPAuth},
		{400, "ExpiredToken", true, nil, false, ClassHTTPAuth},
		{503, "SlowDown", true, nil, false, ClassThrottled},
		{503, "", true, nil, true, ClassThrottled},
		{503, "", true, nil, false, ClassHTTP5xx},
		{429, "", true, nil, false, ClassThrottled},
		{500, "InternalError", true, nil, false, ClassHTTP5xx},
		{301, "PermanentRedirect", true, nil, false, ClassHTTPRedirect},
		{307, "", true, nil, false, ClassHTTPRedirect},
		{404, "NoSuchKey", true, nil, false, ClassHTTP4xx},
		{409, "", true, nil, false, ClassHTTP4xx},
	} {
		if got := classifyStatus(c.status, c.code, c.signed, c.skew, c.retryAfter); got != c.want {
			t.Errorf("%d %s signed=%v: %s, want %s", c.status, c.code, c.signed, got, c.want)
		}
	}
	// A 403 refusal whose Date is 20 minutes off is clock skew even
	// without the code.
	l := newLab(t, false)
	l.srv.Fault = func(*http.Request) s3fake.Fault {
		return s3fake.Fault{Status: 403, Code: "AccessDenied", Skew: 20 * time.Minute}
	}
	if r := l.round(Phases{Cold: 1}); r.Attempts[0].Class != string(ClassClockSkew) || r.Attempts[0].SkewS == nil || *r.Attempts[0].SkewS < 1190 {
		t.Errorf("skewed Date: %+v", r.Attempts[0])
	}
}

// TestRequestID: a failure keeps the provider's request ID, unless it
// looks like an address.
func TestRequestID(t *testing.T) {
	for id, keep := range map[string]bool{"18E11AE2635901BF": true, "f3958845-c4c3-432b-9e75-c8dd99db8605": true,
		"203-0-113-45": false, "x_198_51_100_7_y": false, "a.b": false, "1:2": false} {
		l := newLab(t, false)
		l.srv.RequestID = id
		l.srv.Fault = func(*http.Request) s3fake.Fault { return s3fake.Fault{Status: 500, Code: "InternalError"} }
		a := l.round(Phases{Cold: 1}).Attempts[0]
		if (a.RequestID == id) != keep || (keep && a.RequestIDHeader != "x-amz-request-id") {
			t.Errorf("%q: kept %q (%q)", id, a.RequestID, a.RequestIDHeader)
		}
	}
	// Successes keep none.
	l := newLab(t, false)
	l.srv.RequestID = "18E11AE2635901BF"
	for _, a := range l.round(Phases{Cold: 1}).Attempts {
		if a.RequestID != "" {
			t.Errorf("ok attempt kept %q", a.RequestID)
		}
	}
}

// TestCancelStopsTransfer: a cancel ends a running transfer at once and
// isn't recorded as a failure; the round says it was cancelled.
func TestCancelStopsTransfer(t *testing.T) {
	l := newLab(t, false)
	l.srv.Put("bkt", "fyisp-test/inst/seed/1024", make([]byte, 1024))
	l.srv.Put("bkt", "fyisp-test/inst/seed/1048576", make([]byte, 1<<20))
	l.srv.Fault = func(r *http.Request) s3fake.Fault {
		if r.Header.Get("Range") != "" {
			return s3fake.Fault{StallAfter: 1 << 10, Stall: 5 * time.Second}
		}
		return s3fake.Fault{}
	}
	l.e.o.lim.stall = 10 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	r := l.e.RunRound(ctx, RoundSpec{Install: "inst", Targets: []RoundTarget{l.rt}, Phases: Phases{Down: 1 << 20}})
	if d := time.Since(start); d > 2*time.Second || !r.Cancelled {
		t.Errorf("round took %v after a 300 ms cancel, cancelled %v", d, r.Cancelled)
	}
	for _, a := range r.Attempts {
		if a.Phase == "down" {
			t.Errorf("cancelled download recorded: %+v", a)
		}
	}
}

// TestTransferBound: a download that trickles past its bound is cut and
// timeout_total, whatever the sliding stall deadlines say.
func TestTransferBound(t *testing.T) {
	l := newLab(t, false)
	l.srv.Put("bkt", "fyisp-test/inst/seed/1048576", make([]byte, 1<<20))
	l.srv.Fault = func(r *http.Request) s3fake.Fault {
		if r.Header.Get("Range") != "" {
			return s3fake.Fault{StallAfter: 1 << 10, Stall: 2 * time.Second}
		}
		return s3fake.Fault{}
	}
	l.e.o.lim.stall = 5 * time.Second // the stall alone would never fire
	start := time.Now()
	a := l.attempt(request{method: "GET", key: "fyisp-test/inst/seed/1048576", creds: l.rt.Creds, want: 1 << 20, transfer: true,
		header: map[string]string{"range": "bytes=0-1048575"}, total: 500 * time.Millisecond})
	if a.Class != string(ClassTimeoutTotal) || time.Since(start) > 1500*time.Millisecond || a.Bytes == 0 {
		t.Errorf("bound: %s, %d bytes after %v", a.Class, a.Bytes, time.Since(start))
	}
}

// TestValidate: endpoints, regions and buckets that would put something
// other than a host on the wire are refused.
func TestValidate(t *testing.T) {
	ok := func(raw, region, bucket string, path, token bool) error {
		u, err := url.Parse(raw)
		if err != nil {
			return err
		}
		tg := Target{Endpoint: u, Region: region, Bucket: bucket, PathStyle: path}
		return tg.Validate(token)
	}
	for _, c := range []struct {
		raw, region, bucket string
		path, token         bool
		valid               bool
	}{
		{"https://s3.us-east-1.amazonaws.com", "us-east-1", "fyisp-probe", false, true, true},
		{"https://minio.lan:9000/", "us-east-1", "lab", true, false, true},
		{"http://192.168.1.20:9000", "garage", "lab", true, false, true},
		{"http://192.168.1.20:9000", "garage", "lab", false, false, false}, // an IP needs path style
		{"http://192.168.1.20:9000", "garage", "lab", true, true, false},   // no token over http
		{"ftp://h", "r", "", false, false, false},
		{"https://user:pw@h", "r", "", false, false, false},
		{"https://h/path", "r", "", false, false, false},
		{"https://h?x=1", "r", "", false, false, false},
		{"https://h:0", "r", "", false, false, false},
		{"https://h:99999", "r", "", false, false, false},
		{"https://h", "r\r\nX: y", "", false, false, false},
		{"https://h", "", "", false, false, false},
		{"https://h", "r", "evil.com:22", false, false, false},
		{"https://h", "r", "a/b", false, false, false},
		{"https://h", "r", "ab", false, false, false},
		{"https://h", "r", "My_Bucket.v2", true, false, true},
	} {
		if err := ok(c.raw, c.region, c.bucket, c.path, c.token); (err == nil) != c.valid {
			t.Errorf("%s %q %q path=%v token=%v: %v", c.raw, c.region, c.bucket, c.path, c.token, err)
		}
	}
	// The engine skips an invalid target without a request.
	l := newLab(t, false)
	l.rt.Bucket = "evil.com:22"
	if r := l.round(Phases{Cold: 2}); len(r.Attempts) != 0 || r.Skips["t_1"] != "config" || len(l.srv.Seen()) != 0 {
		t.Errorf("invalid target: %v %v", classes(r.Attempts), r.Skips)
	}
}

// TestEnginePrivacy: nothing the engine returns carries an error's text,
// the keys or this machine's address, even when every error names them.
func TestEnginePrivacy(t *testing.T) {
	const ip, secret, token = "198.51.100.77", "s3cr3t-planted", "t0ken-planted"
	l := newLab(t, true)
	l.rt.Creds = NewCreds("AKIDPLANTED", secret, token)
	planted := errors.New("dial tcp4 " + ip + ":51234->127.0.0.1:443: " + secret + " " + token)
	for _, lookup := range []bool{true, false} {
		if lookup {
			l.look = func(string) ([]netip.Addr, error) { return nil, planted }
		} else {
			l.look = func(string) ([]netip.Addr, error) { return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil }
			l.e.o.Dial = func(context.Context, string, string) (net.Conn, error) {
				return nil, &net.OpError{Op: "dial", Err: planted}
			}
		}
		r := l.round(Phases{Cold: 2, Warm: 1, Down: 1024, Up: 1024})
		c := l.e.Check(context.Background(), l.rt)
		b, _ := json.Marshal([]any{r, c})
		out := string(b) + fmt.Sprintf("%+v %+v", r, c)
		for _, leak := range []string{ip, secret, token, "AKIDPLANTED", "51234"} {
			if strings.Contains(out, leak) {
				t.Errorf("%q in the results: %s", leak, out)
			}
		}
	}
}

// attempt runs one request on a fresh connection.
func (l *lab) attempt(r request) model.StorageAttempt {
	var a model.StorageAttempt
	l.e.do(context.Background(), &l.rt.Target, r, nil, false, &a)
	return a
}
