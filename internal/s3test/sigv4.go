package s3test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"sort"
	"strings"
	"time"
)

// EmptySHA256 is the payload hash of a request without a body.
const EmptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// UnsignedPayload is the payload hash an HTTPS upload sends: TLS protects
// the body, and the upload needn't be read twice.
const UnsignedPayload = "UNSIGNED-PAYLOAD"

const amzDate = "20060102T150405Z"

// uriEncode is S3's URI encoding: every byte but A-Z a-z 0-9 - . _ ~ as
// %XX (upper case); '/' too unless keepSlash. S3 never normalises paths.
func uriEncode(s string, keepSlash bool) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case 'A' <= c && c <= 'Z', 'a' <= c && c <= 'z', '0' <= c && c <= '9', c == '-', c == '.', c == '_', c == '~':
			b.WriteByte(c)
		case c == '/' && keepSlash:
			b.WriteByte(c)
		default:
			const hexd = "0123456789ABCDEF"
			b.WriteByte('%')
			b.WriteByte(hexd[c>>4])
			b.WriteByte(hexd[c&15])
		}
	}
	return b.String()
}

// canonicalQuery sorts and encodes q; a parameter without a value is
// "name=".
func canonicalQuery(q url.Values) string {
	type kv struct{ k, v string }
	var all []kv
	for k, vs := range q {
		for _, v := range vs {
			all = append(all, kv{uriEncode(k, false), uriEncode(v, false)})
		}
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].k != all[j].k {
			return all[i].k < all[j].k
		}
		return all[i].v < all[j].v
	})
	parts := make([]string, len(all))
	for i, p := range all {
		parts[i] = p.k + "=" + p.v
	}
	return strings.Join(parts, "&")
}

// signed is one request's signature: the headers to send (lower case
// names) and Authorization.
type signed struct {
	Headers       map[string]string
	Authorization string
}

// sign signs a request: method, the encoded path (exactly as it goes on the
// request line), its query, the headers to sign (lower case names; host
// included, x-amz-date, x-amz-content-sha256 and x-amz-security-token
// added here), the payload hash and the time. The region is the target's,
// as typed.
func sign(c *Creds, region, method, path string, q url.Values, headers map[string]string, payloadHash string, t time.Time) signed {
	t = t.UTC()
	stamp := t.Format(amzDate)
	day := stamp[:8]
	h := make(map[string]string, len(headers)+3)
	for k, v := range headers {
		h[strings.ToLower(k)] = v
	}
	h["x-amz-date"] = stamp
	h["x-amz-content-sha256"] = payloadHash
	if len(c.token) > 0 {
		h["x-amz-security-token"] = string(c.token)
	}
	names := make([]string, 0, len(h))
	for k := range h {
		names = append(names, k)
	}
	sort.Strings(names)
	var ch strings.Builder
	for _, k := range names {
		ch.WriteString(k)
		ch.WriteByte(':')
		ch.WriteString(strings.Join(strings.Fields(h[k]), " "))
		ch.WriteByte('\n')
	}
	signedHeaders := strings.Join(names, ";")
	if path == "" {
		path = "/"
	}
	creq := method + "\n" + path + "\n" + canonicalQuery(q) + "\n" + ch.String() + "\n" + signedHeaders + "\n" + payloadHash
	scope := day + "/" + region + "/s3/aws4_request"
	sum := sha256.Sum256([]byte(creq))
	sts := "AWS4-HMAC-SHA256\n" + stamp + "\n" + scope + "\n" + hex.EncodeToString(sum[:])
	seed := make([]byte, 0, 4+len(c.secret))
	seed = append(append(seed, "AWS4"...), c.secret...)
	key := hmacSHA256(seed, day)
	clear(seed) // the secret's only copy outside Creds (the HMAC state is garbage now)
	key = hmacSHA256(key, region)
	key = hmacSHA256(key, "s3")
	key = hmacSHA256(key, "aws4_request")
	sig := hex.EncodeToString(hmacSHA256(key, sts))
	return signed{Headers: h, Authorization: "AWS4-HMAC-SHA256 Credential=" + c.accessKey + "/" + scope +
		", SignedHeaders=" + signedHeaders + ", Signature=" + sig}
}

func hmacSHA256(key []byte, s string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(s))
	return m.Sum(nil)
}
