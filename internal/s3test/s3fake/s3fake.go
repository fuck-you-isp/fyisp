// Package s3fake is a small S3-compatible server for tests: objects in
// memory, SigV4 checked independently of the client's signer (from the
// request as received: its Host, raw path and query), and faults on
// demand (errors, resets, stalls, closes, clock skew).
package s3fake

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"encoding/xml"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Fault changes how one request is answered.
type Fault struct {
	Status int    // answer with this status and an S3 error body with Code
	Code   string // (e.g. 503 SlowDown)
	// ResetBeforeResponse resets the connection once the request is read.
	ResetBeforeResponse bool
	// Delay waits before the response headers (a slow first byte).
	Delay time.Duration
	// ResetAfter sends the headers and this many body bytes, then resets;
	// StallAfter sends this many and then stops for Stall.
	ResetAfter, StallAfter int64
	Stall                  time.Duration
	// Close answers with Connection: close; Drop answers in full, then
	// closes the connection without saying so (an idle kept connection the
	// server dropped).
	Close, Drop bool
	// Skew moves the response's Date header.
	Skew time.Duration
}

// Seen is one request the server answered.
type Seen struct {
	Method, Path string
	Signed       bool
	Payload      string // x-amz-content-sha256
}

// Server is the fake. Set the fields before Start.
type Server struct {
	Region, AccessKey, Secret, Token string
	// VHost is the domain of virtual-host requests: a Host of
	// <bucket>.<VHost> addresses that bucket.
	VHost string
	// StrictRegion refuses a signature for another region (400
	// AuthorizationHeaderMalformed), as some servers do.
	StrictRegion bool
	// Fault is called for every request (nil: none).
	Fault func(r *http.Request) Fault
	// RequestID is sent as x-amz-request-id ("" none).
	RequestID string

	srv *httptest.Server

	mu      sync.Mutex
	buckets map[string]map[string][]byte
	seen    []Seen
}

// New returns a server with one bucket.
func New(bucket string) *Server {
	return &Server{Region: "us-east-1", AccessKey: "AKFAKE00000000000001", Secret: "fake/secret+key", VHost: "s3.test",
		buckets: map[string]map[string][]byte{bucket: {}}}
}

// Start serves plain HTTP on 127.0.0.1.
func (s *Server) Start() {
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
}

// StartTLS serves HTTPS on 127.0.0.1 with a test certificate for
// "s3.test", "*.s3.test" and 127.0.0.1; the pool returned trusts it.
func (s *Server) StartTLS() *x509.CertPool {
	s.srv = httptest.NewUnstartedServer(http.HandlerFunc(s.serve))
	s.srv.EnableHTTP2 = false
	s.srv.TLS = &tls.Config{Certificates: []tls.Certificate{testCert()}}
	s.srv.StartTLS()
	pool := x509.NewCertPool()
	pool.AddCert(s.srv.Certificate())
	return pool
}

// Handler serves the fake's S3 API, for a server the caller runs (the
// netns lab's responder).
func (s *Server) Handler() http.Handler { return http.HandlerFunc(s.serve) }

// CertPEM is the TLS server's certificate as PEM (a target's private CA).
func (s *Server) CertPEM() string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.srv.Certificate().Raw}))
}

// Close stops the server.
func (s *Server) Close() { s.srv.Close() }

// Addr is the listener's address.
func (s *Server) Addr() string { return s.srv.Listener.Addr().String() }

// Seen is every request so far.
func (s *Server) Seen() []Seen {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Seen(nil), s.seen...)
}

// Objects is a bucket's keys and sizes.
func (s *Server) Objects(bucket string) map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]int{}
	for k, v := range s.buckets[bucket] {
		out[k] = len(v)
	}
	return out
}

// Put stores an object directly.
func (s *Server) Put(bucket, key string, b []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buckets[bucket][key] = b
}

func (s *Server) note(r *http.Request, signed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen = append(s.seen, Seen{Method: r.Method, Path: rawPath(r), Signed: signed, Payload: r.Header.Get("x-amz-content-sha256")})
}

func rawPath(r *http.Request) string {
	p, _, _ := strings.Cut(r.RequestURI, "?")
	return p
}

func rawQuery(r *http.Request) string {
	_, q, _ := strings.Cut(r.RequestURI, "?")
	return q
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	var f Fault
	if s.Fault != nil {
		f = s.Fault(r)
	}
	body, _ := io.ReadAll(r.Body)
	if f.ResetBeforeResponse {
		s.note(r, r.Header.Get("Authorization") != "")
		reset(w)
		return
	}
	if f.Delay > 0 {
		time.Sleep(f.Delay)
	}
	w.Header().Set("Date", time.Now().Add(f.Skew).UTC().Format(http.TimeFormat))
	if s.RequestID != "" {
		w.Header().Set("x-amz-request-id", s.RequestID)
	}
	if f.Close {
		w.Header().Set("Connection", "close")
	}
	if f.Status != 0 {
		s.fail(w, r, f.Status, f.Code, true)
		return
	}
	signed, status, code := s.auth(r, body)
	if status != 0 {
		s.fail(w, r, status, code, signed)
		return
	}
	bucket, key := s.target(r)
	s.mu.Lock()
	objs, ok := s.buckets[bucket]
	s.mu.Unlock()
	switch {
	case bucket == "":
		if !signed {
			s.fail(w, r, http.StatusForbidden, "AccessDenied", false)
			return
		}
		s.ok(w, r, http.StatusOK, nil, signed)
		return
	case !ok:
		s.fail(w, r, http.StatusNotFound, "NoSuchBucket", signed)
		return
	case !signed:
		s.fail(w, r, http.StatusForbidden, "AccessDenied", false)
		return
	}
	switch {
	case key == "" && (r.Method == "HEAD"):
		s.ok(w, r, http.StatusOK, nil, signed)
	case key == "" && r.Method == "GET" && r.URL.Query().Get("list-type") == "2":
		s.list(w, r, objs)
	case r.Method == "PUT" && key != "":
		s.mu.Lock()
		objs[key] = body
		s.mu.Unlock()
		s.ok(w, r, http.StatusOK, nil, signed)
	case r.Method == "DELETE" && key != "":
		s.mu.Lock()
		delete(objs, key)
		s.mu.Unlock()
		s.ok(w, r, http.StatusNoContent, nil, signed)
	case (r.Method == "GET" || r.Method == "HEAD") && key != "":
		s.mu.Lock()
		b, ok := objs[key]
		s.mu.Unlock()
		if !ok {
			s.fail(w, r, http.StatusNotFound, "NoSuchKey", signed)
			return
		}
		status := http.StatusOK
		if rg := r.Header.Get("Range"); rg != "" {
			from, to, ok := parseRange(rg, len(b))
			if !ok {
				s.fail(w, r, http.StatusRequestedRangeNotSatisfiable, "InvalidRange", signed)
				return
			}
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", from, to, len(b)))
			b, status = b[from:to+1], http.StatusPartialContent
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(b)))
		s.note(r, signed)
		w.WriteHeader(status)
		if r.Method != "HEAD" {
			s.body(w, b, f)
		}
		if f.Drop {
			drop(w)
		}
	default:
		s.fail(w, r, http.StatusMethodNotAllowed, "MethodNotAllowed", signed)
	}
}

// body writes b, with the fault's reset or stall part-way.
func (s *Server) body(w http.ResponseWriter, b []byte, f Fault) {
	cut := int64(-1)
	if f.ResetAfter > 0 {
		cut = f.ResetAfter
	}
	if f.StallAfter > 0 {
		cut = f.StallAfter
	}
	if cut < 0 || cut >= int64(len(b)) {
		_, _ = w.Write(b)
		return
	}
	_, _ = w.Write(b[:cut])
	if fl, ok := w.(http.Flusher); ok {
		fl.Flush()
	}
	if f.StallAfter > 0 {
		time.Sleep(f.Stall)
		_, _ = w.Write(b[cut:])
		return
	}
	reset(w)
}

func (s *Server) ok(w http.ResponseWriter, r *http.Request, status int, b []byte, signed bool) {
	s.note(r, signed)
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, status int, code string, signed bool) {
	s.note(r, signed)
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	if r.Method != "HEAD" {
		fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>%s</Code><Message>fake</Message></Error>`, code)
	}
}

func (s *Server) list(w http.ResponseWriter, r *http.Request, objs map[string][]byte) {
	prefix := r.URL.Query().Get("prefix")
	type content struct {
		Key  string
		Size int
	}
	var out struct {
		XMLName  xml.Name `xml:"ListBucketResult"`
		Contents []content
	}
	s.mu.Lock()
	for k, v := range objs {
		if strings.HasPrefix(k, prefix) {
			out.Contents = append(out.Contents, content{k, len(v)})
		}
	}
	s.mu.Unlock()
	sort.Slice(out.Contents, func(i, j int) bool { return out.Contents[i].Key < out.Contents[j].Key })
	b, _ := xml.Marshal(out)
	s.ok(w, r, http.StatusOK, b, true)
}

// target is the request's bucket and key: virtual host or path style.
func (s *Server) target(r *http.Request) (bucket, key string) {
	host, _, err := net.SplitHostPort(r.Host)
	if err != nil {
		host = r.Host
	}
	p, _ := url.PathUnescape(rawPath(r))
	if s.VHost != "" && strings.HasSuffix(host, "."+s.VHost) {
		return strings.TrimSuffix(host, "."+s.VHost), strings.TrimPrefix(p, "/")
	}
	p = strings.TrimPrefix(p, "/")
	bucket, key, _ = strings.Cut(p, "/")
	return bucket, key
}

// auth checks the signature from the request as received: unsigned
// requests pass (with signed false); a bad one gets a status and code.
func (s *Server) auth(r *http.Request, body []byte) (signed bool, status int, code string) {
	h := r.Header.Get("Authorization")
	if h == "" {
		return false, 0, ""
	}
	const algo = "AWS4-HMAC-SHA256 "
	if !strings.HasPrefix(h, algo) {
		return true, http.StatusBadRequest, "AuthorizationHeaderMalformed"
	}
	fields := map[string]string{}
	for _, p := range strings.Split(h[len(algo):], ",") {
		k, v, _ := strings.Cut(strings.TrimSpace(p), "=")
		fields[k] = v
	}
	cred := strings.Split(fields["Credential"], "/")
	if len(cred) != 5 || cred[3] != "s3" || cred[4] != "aws4_request" {
		return true, http.StatusBadRequest, "AuthorizationHeaderMalformed"
	}
	if cred[0] != s.AccessKey {
		return true, http.StatusForbidden, "InvalidAccessKeyId"
	}
	if s.StrictRegion && cred[2] != s.Region {
		return true, http.StatusBadRequest, "AuthorizationHeaderMalformed"
	}
	stamp := r.Header.Get("x-amz-date")
	t, err := time.Parse("20060102T150405Z", stamp)
	if err != nil || stamp[:8] != cred[1] {
		return true, http.StatusForbidden, "AccessDenied"
	}
	if d := time.Since(t); d > 15*time.Minute || d < -15*time.Minute {
		return true, http.StatusForbidden, "RequestTimeTooSkewed"
	}
	if s.Token != "" && r.Header.Get("x-amz-security-token") != s.Token {
		return true, http.StatusForbidden, "InvalidTokenId"
	}
	ph := r.Header.Get("x-amz-content-sha256")
	if ph == "" {
		return true, http.StatusBadRequest, "MissingSecurityHeader"
	}
	if ph != "UNSIGNED-PAYLOAD" {
		sum := sha256.Sum256(body)
		if hex.EncodeToString(sum[:]) != ph {
			return true, http.StatusBadRequest, "XAmzContentSHA256Mismatch"
		}
	}
	names := strings.Split(fields["SignedHeaders"], ";")
	// What must be signed, whatever the client chose.
	must := []string{"host", "x-amz-content-sha256", "x-amz-date"}
	if r.Header.Get("Range") != "" {
		must = append(must, "range")
	}
	if r.Header.Get("x-amz-security-token") != "" {
		must = append(must, "x-amz-security-token")
	}
	for _, m := range must {
		if !slices.Contains(names, m) {
			return true, http.StatusForbidden, "SignatureDoesNotMatch"
		}
	}
	var ch strings.Builder
	for _, n := range names {
		v := r.Header.Get(n)
		if n == "host" {
			v = r.Host
		}
		ch.WriteString(n + ":" + strings.Join(strings.Fields(v), " ") + "\n")
	}
	creq := r.Method + "\n" + canonPath(rawPath(r)) + "\n" + canonQuery(rawQuery(r)) + "\n" + ch.String() + "\n" + fields["SignedHeaders"] + "\n" + ph
	sum := sha256.Sum256([]byte(creq))
	scope := strings.Join(cred[1:], "/")
	sts := "AWS4-HMAC-SHA256\n" + stamp + "\n" + scope + "\n" + hex.EncodeToString(sum[:])
	k := mac([]byte("AWS4"+s.Secret), cred[1])
	k = mac(k, cred[2])
	k = mac(k, "s3")
	k = mac(k, "aws4_request")
	if !hmac.Equal([]byte(hex.EncodeToString(mac(k, sts))), []byte(fields["Signature"])) {
		return true, http.StatusForbidden, "SignatureDoesNotMatch"
	}
	return true, 0, ""
}

// canonPath re-encodes a raw path the S3 way, segment by segment (its own
// encoder, not the client's): a client that sent a character unencoded
// fails here.
func canonPath(raw string) string {
	segs := strings.Split(raw, "/")
	for i, seg := range segs {
		d, err := url.PathUnescape(seg)
		if err != nil {
			return "!" // can't match
		}
		segs[i] = enc(d)
	}
	if out := strings.Join(segs, "/"); out != "" {
		return out
	}
	return "/"
}

// canonQuery re-encodes a raw query the S3 way, sorted by name, then value.
func canonQuery(raw string) string {
	if raw == "" {
		return ""
	}
	type kv struct{ k, v string }
	var parts []kv
	for _, p := range strings.Split(raw, "&") {
		k, v, _ := strings.Cut(p, "=")
		k, _ = url.QueryUnescape(k)
		v, _ = url.QueryUnescape(v)
		parts = append(parts, kv{enc(k), enc(v)})
	}
	sort.Slice(parts, func(i, j int) bool {
		if parts[i].k != parts[j].k {
			return parts[i].k < parts[j].k
		}
		return parts[i].v < parts[j].v
	})
	out := make([]string, len(parts))
	for i, p := range parts {
		out[i] = p.k + "=" + p.v
	}
	return strings.Join(out, "&")
}

func enc(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || strings.IndexByte("-._~", c) >= 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// testCert is a self-signed certificate for s3.test, *.s3.test and
// 127.0.0.1.
func testCert() tls.Certificate {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "s3.test"},
		DNSNames: []string{"s3.test", "*.s3.test"}, IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true, IsCA: true}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		panic(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func mac(k []byte, s string) []byte {
	m := hmac.New(sha256.New, k)
	m.Write([]byte(s))
	return m.Sum(nil)
}

func parseRange(h string, n int) (from, to int, ok bool) {
	spec, ok := strings.CutPrefix(h, "bytes=")
	if !ok {
		return 0, 0, false
	}
	a, b, _ := strings.Cut(spec, "-")
	from, err1 := strconv.Atoi(a)
	to, err2 := strconv.Atoi(b)
	if err1 != nil || err2 != nil || from > to || from >= n {
		return 0, 0, false
	}
	return from, min(to, n-1), true
}

// drop closes the request's connection after its response, without a
// Connection: close.
func drop(w http.ResponseWriter) {
	if fl, ok := w.(http.Flusher); ok {
		fl.Flush()
	}
	if hj, ok := w.(http.Hijacker); ok {
		if c, _, err := hj.Hijack(); err == nil {
			_ = c.Close()
		}
	}
}

// reset closes the request's connection with a TCP reset.
func reset(w http.ResponseWriter) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		return
	}
	c, _, err := hj.Hijack()
	if err != nil {
		return
	}
	if tc, ok := c.(*tls.Conn); ok {
		c = tc.NetConn() // no close_notify: a bare reset
	}
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.SetLinger(0)
	}
	_ = c.Close()
}
