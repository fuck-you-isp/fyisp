// Package storage is fyisp's storage tests: the owner's S3-compatible
// targets (kept in a file of their own, never in the database), checks
// and, with internal/s3test, the rounds. Nothing in it is vendor- or
// region-specific.
package storage

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/fuck-you-isp/fyisp/internal/s3test"
)

// Limits.
const (
	MaxTargets = 32
	maxName    = 64
	maxCA      = 64 << 10
)

// Target is one saved endpoint. It never holds keys.
type Target struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Endpoint   string `json:"endpoint"` // scheme://host[:port]
	Region     string `json:"region"`
	Bucket     string `json:"bucket"`     // "" = connection-only
	Addressing string `json:"addressing"` // "virtual-host" | "path"
	CAPEM      string `json:"ca_pem"`     // a private CA to trust instead of the system's
	// Network is "public" or "private", fixed when the endpoint is saved:
	// a name that later resolves to the other kind is refused (DNS
	// rebinding).
	Network   string `json:"network"`
	CreatedMs int64  `json:"created_ms"`
	UpdatedMs int64  `json:"updated_ms"`
}

// Spec is a target's settings as entered; Patch changes some of them.
type Spec struct {
	Name, Endpoint, Region, Bucket, Addressing, CAPEM string
}

// Patch sets the fields that are not nil.
type Patch struct {
	Name, Endpoint, Region, Bucket, Addressing, CAPEM *string
}

// FieldError is a refused field (the API's 400 with "field").
type FieldError struct {
	Field, Msg string
	Code       string // "bad_request", "addr_refused"
}

func (e *FieldError) Error() string { return e.Field + ": " + e.Msg }

func bad(field, msg string) error { return &FieldError{Field: field, Msg: msg, Code: "bad_request"} }

// normalize fills defaults and checks the settings (not the network).
func (s *Spec) normalize() error {
	s.Name = strings.TrimSpace(s.Name)
	s.Endpoint = strings.TrimRight(strings.TrimSpace(s.Endpoint), "/")
	s.Region = strings.TrimSpace(s.Region)
	s.Bucket = strings.TrimSpace(s.Bucket)
	if s.Region == "" {
		s.Region = "us-east-1"
	}
	if s.Addressing == "" {
		s.Addressing = "virtual-host"
	}
	switch {
	case s.Name == "" || utf8.RuneCountInString(s.Name) > maxName || strings.ContainsFunc(s.Name, unicode.IsControl):
		return bad("name", fmt.Sprintf("a name of 1 to %d characters", maxName))
	case s.Addressing != "virtual-host" && s.Addressing != "path":
		return bad("addressing", `"virtual-host" or "path"`)
	case len(s.CAPEM) > maxCA:
		return bad("ca_pem", "at most 64 KiB")
	}
	u, err := url.Parse(s.Endpoint)
	switch {
	case err != nil:
		return bad("endpoint", "not a URL")
	case u.ForceQuery || strings.HasSuffix(u.Host, ":"):
		return bad("endpoint", "the endpoint is a scheme and a host (and a port), nothing else")
	}
	u.Host = strings.ToLower(u.Host)
	s.Endpoint = u.String()
	t := s.engineTarget(u, nil, false)
	if err := t.Validate(false); err != nil {
		field := "endpoint"
		switch {
		case strings.Contains(err.Error(), "region"):
			field = "region"
		case strings.Contains(err.Error(), "bucket"), strings.Contains(err.Error(), "path-style"):
			field = "bucket"
		}
		return bad(field, err.Error())
	}
	if s.CAPEM != "" {
		if _, err := certPool(s.CAPEM); err != nil {
			return bad("ca_pem", err.Error())
		}
	}
	return nil
}

func (s *Spec) engineTarget(u *url.URL, pool *x509.CertPool, private bool) s3test.Target {
	return s3test.Target{Endpoint: u, Region: s.Region, Bucket: s.Bucket, PathStyle: s.Addressing == "path", RootCAs: pool, Private: private}
}

// certPool parses PEM certificates.
func certPool(pemText string) (*x509.CertPool, error) {
	pool := x509.NewCertPool()
	n := 0
	rest := []byte(pemText)
	for {
		var b *pem.Block
		b, rest = pem.Decode(rest)
		if b == nil {
			break
		}
		if b.Type != "CERTIFICATE" {
			continue
		}
		c, err := x509.ParseCertificate(b.Bytes)
		if err != nil {
			return nil, errors.New("a certificate in it doesn't parse")
		}
		pool.AddCert(c)
		n++
	}
	if n == 0 {
		return nil, errors.New("no PEM certificate in it")
	}
	return pool, nil
}

// network resolves the endpoint's host and says whether it is on this
// network: every address must be of one kind, none refused, and plain
// http only on the LAN itself.
func network(ctx context.Context, lookup func(ctx context.Context, network, host string) ([]netip.Addr, error), endpoint string) (string, error) {
	u, _ := url.Parse(endpoint)
	host := u.Hostname()
	var addrs []netip.Addr
	if a, err := netip.ParseAddr(host); err == nil {
		addrs = []netip.Addr{a}
	} else {
		lctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		addrs, err = lookup(lctx, "ip4", host)
		if err != nil || len(addrs) == 0 {
			return "", bad("endpoint", "its name doesn't resolve to an IPv4 address")
		}
	}
	var private bool
	for i, a := range addrs {
		p := s3test.AddrAllowed("https", true, a)
		switch {
		case !p && !s3test.AddrAllowed("https", false, a):
			return "", &FieldError{Field: "endpoint", Msg: "its address is one fyisp never connects to (link-local, multicast or reserved)", Code: "addr_refused"}
		case i == 0:
			private = p
		case p != private:
			return "", &FieldError{Field: "endpoint", Msg: "its name resolves to addresses both on this network and on the internet", Code: "addr_refused"}
		}
	}
	for _, a := range addrs {
		if u.Scheme == "http" && !s3test.AddrAllowed("http", private, a) {
			return "", bad("endpoint", "plain http:// is only for a server on your own network; use https://")
		}
	}
	if private {
		return "private", nil
	}
	return "public", nil
}

// binding is what a check's result belongs to: a change to any of it
// drops the result.
func (t *Target) binding() string {
	h := sha256.Sum256([]byte(t.Endpoint + "\n" + t.Region + "\n" + t.Bucket + "\n" + t.Addressing + "\n" + t.CAPEM))
	return hex.EncodeToString(h[:])
}

// Engine is the target as the engine runs it.
func (t *Target) Engine() (s3test.Target, error) {
	u, err := url.Parse(t.Endpoint)
	if err != nil {
		return s3test.Target{}, err
	}
	var pool *x509.CertPool
	if t.CAPEM != "" {
		if pool, err = certPool(t.CAPEM); err != nil {
			return s3test.Target{}, err
		}
	}
	return s3test.Target{ID: t.ID, Name: t.Name, Endpoint: u, Region: t.Region, Bucket: t.Bucket, PathStyle: t.Addressing == "path",
		RootCAs: pool, Private: t.Network == "private"}, nil
}

// Scheme, Host and Port are the endpoint's (the API shows them; a saved
// endpoint always parses).
func (t *Target) Scheme() string { u, _ := url.Parse(t.Endpoint); return u.Scheme }
func (t *Target) Host() string   { u, _ := url.Parse(t.Endpoint); return u.Hostname() }
func (t *Target) Port() string {
	u, _ := url.Parse(t.Endpoint)
	if p := u.Port(); p != "" {
		return p
	}
	if u.Scheme == "https" {
		return "443"
	}
	return "80"
}

var idRe = regexp.MustCompile(`^t_[0-9a-f]{8}$`)

// valid checks a target read from the file as Add would have saved it.
func (t *Target) valid() error {
	sp := Spec{Name: t.Name, Endpoint: t.Endpoint, Region: t.Region, Bucket: t.Bucket, Addressing: t.Addressing, CAPEM: t.CAPEM}
	switch err := sp.normalize(); {
	case !idRe.MatchString(t.ID):
		return errors.New("a bad id")
	case t.Network != "public" && t.Network != "private":
		return errors.New(`network is "public" or "private"`)
	case err != nil:
		return err
	case sp != (Spec{Name: t.Name, Endpoint: t.Endpoint, Region: t.Region, Bucket: t.Bucket, Addressing: t.Addressing, CAPEM: t.CAPEM}):
		return errors.New("its settings are not as fyisp saves them")
	}
	return nil
}

func newID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return "t_" + hex.EncodeToString(b)
}

// IsSelf reports an address of this machine's interfaces
// (s3test.Options.IsSelf: never kept as a provider's edge).
func IsSelf(a netip.Addr) bool {
	as, err := net.InterfaceAddrs()
	if err != nil {
		return true // unknown: keep nothing
	}
	for _, x := range as {
		if n, ok := x.(*net.IPNet); ok {
			if ip, ok := netip.AddrFromSlice(n.IP); ok && ip.Unmap() == a.Unmap() {
				return true
			}
		}
	}
	return false
}
