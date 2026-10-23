package s3test

import (
	"fmt"
	"log/slog"
	"slices"
)

// Creds are an S3 access key, its secret and an optional session token.
// They print as [redacted] in every format, logs and JSON included; only
// the signer reads the secret. Zero overwrites the secret and the token
// (best effort: Go may have copied them before).
type Creds struct {
	accessKey string
	secret    []byte
	token     []byte
}

// NewCreds copies the key material.
func NewCreds(accessKey, secret, token string) *Creds {
	return &Creds{accessKey: accessKey, secret: []byte(secret), token: []byte(token)}
}

// AccessKeyLast4 is the access key's last 4 characters (the UI's hint).
func (c *Creds) AccessKeyLast4() string {
	if len(c.accessKey) <= 4 {
		return ""
	}
	return c.accessKey[len(c.accessKey)-4:]
}

// Clone copies c (its secret and token as new byte slices).
func (c *Creds) Clone() *Creds {
	return &Creds{accessKey: c.accessKey, secret: slices.Clone(c.secret), token: slices.Clone(c.token)}
}

// Material is the key material as strings (which can't be zeroed), for the
// one place that writes it out: the storage targets file, for a recurring
// schedule's keys.
func (c *Creds) Material() (accessKey, secret, token string) {
	return c.accessKey, string(c.secret), string(c.token)
}

// HasToken reports a session token.
func (c *Creds) HasToken() bool { return len(c.token) > 0 }

// Zero overwrites the secret and the token.
func (c *Creds) Zero() {
	if c == nil {
		return
	}
	clear(c.secret)
	clear(c.token)
	c.secret, c.token, c.accessKey = nil, nil, ""
}

// Value receivers: a Creds copied by value is redacted too.
func (Creds) String() string                { return "[redacted]" }
func (Creds) GoString() string              { return "[redacted]" }
func (Creds) LogValue() slog.Value          { return slog.StringValue("[redacted]") }
func (Creds) MarshalJSON() ([]byte, error)  { return []byte(`"[redacted]"`), nil }
func (Creds) MarshalText() ([]byte, error)  { return []byte("[redacted]"), nil }
func (Creds) Format(f fmt.State, verb rune) { _, _ = f.Write([]byte("[redacted]")) }
