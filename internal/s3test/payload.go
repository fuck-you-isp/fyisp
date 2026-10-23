package s3test

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"math/rand/v2"
	"time"
)

// payload is n incompressible, reproducible bytes for name (a ChaCha8
// stream keyed by it): the same object always has the same content, so a
// seed's length and hash can be checked, and nothing on the path can
// compress it.
type payload struct {
	r    *rand.ChaCha8
	left int64
}

func newPayload(name string, n int64) *payload {
	return &payload{r: rand.NewChaCha8(sha256.Sum256([]byte("fyisp storage payload\n" + name))), left: n}
}

func (p *payload) Read(b []byte) (int, error) {
	if p.left <= 0 {
		return 0, io.EOF
	}
	if int64(len(b)) > p.left {
		b = b[:p.left]
	}
	n, _ := p.r.Read(b)
	p.left -= int64(n)
	return n, nil
}

// payloadHash is the SHA-256 of name's payload of n bytes (a plain-http
// upload signs it; read before the timed window).
func payloadHash(name string, n int64) string {
	h := sha256.New()
	_, _ = io.Copy(h, newPayload(name, n))
	return hex.EncodeToString(h.Sum(nil))
}

// gapReader times the gaps between an upload body's reads: the request
// writer reads the next chunk once the previous one is written, so a long
// gap is a write that blocked (the peer stopped acknowledging).
type gapReader struct {
	r    io.Reader
	now  func() time.Time
	last time.Time
	max  time.Duration
}

func (g *gapReader) Read(b []byte) (int, error) {
	now := g.now()
	if !g.last.IsZero() {
		g.max = max(g.max, now.Sub(g.last))
	}
	n, err := g.r.Read(b)
	g.last = g.now()
	return n, err
}
