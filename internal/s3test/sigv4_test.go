package s3test

import (
	"net/url"
	"strings"
	"testing"
	"testing/quick"
	"time"
)

// The S3 worked examples of "Signature Calculations for the Authorization
// Header: Transferring Payload in a Single Chunk" (AWS S3 API Reference,
// docs.aws.amazon.com/AmazonS3/latest/API/sig-v4-header-based-auth.html):
// AWS's example credentials and bucket, 2013-05-24.
func TestSignS3Examples(t *testing.T) {
	c := NewCreds("AKIAIOSFODNN7EXAMPLE", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", "")
	at := time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)
	host := "examplebucket.s3.amazonaws.com"
	for _, x := range []struct {
		name, method, path string
		query              url.Values
		headers            map[string]string
		payload, want      string
	}{
		{"GET object, range", "GET", "/test.txt", nil, map[string]string{"host": host, "range": "bytes=0-9"}, EmptySHA256,
			"f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"},
		{"PUT object", "PUT", "/" + uriEncode("test$file.text", true), nil,
			map[string]string{"host": host, "date": "Fri, 24 May 2013 00:00:00 GMT", "x-amz-storage-class": "REDUCED_REDUNDANCY"},
			"44ce7dd67c959e0d3524ffac1771dfbba87d2b6b4b4e99e42034a8b803f8b072",
			"98ad721746da40c64f1a55b78f14c238d841ea1380cd77a1b5971af0ece108bd"},
		{"GET bucket lifecycle", "GET", "/", url.Values{"lifecycle": {""}}, map[string]string{"host": host}, EmptySHA256,
			"fea454ca298b7da1c68078a5d1bdbfbbe0d65c699e0f91ac7a200a0136783543"},
		{"GET bucket list", "GET", "/", url.Values{"max-keys": {"2"}, "prefix": {"J"}}, map[string]string{"host": host}, EmptySHA256,
			"34b48302e7b5fa45bde8084f4b7868a86f0a534bc59db6670ed5711ef69dc6f7"},
	} {
		got := sign(c, "us-east-1", x.method, x.path, x.query, x.headers, x.payload, at)
		if !strings.HasSuffix(got.Authorization, "Signature="+x.want) {
			t.Errorf("%s: %s", x.name, got.Authorization)
		}
		if !strings.HasPrefix(got.Authorization, "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, SignedHeaders=") {
			t.Errorf("%s: %s", x.name, got.Authorization)
		}
	}
}

func TestSignSessionToken(t *testing.T) {
	at := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	plain := sign(NewCreds("AK", "SK", ""), "auto", "GET", "/k", nil, map[string]string{"host": "h"}, EmptySHA256, at)
	tok := sign(NewCreds("AK", "SK", "tok/en+=="), "auto", "GET", "/k", nil, map[string]string{"host": "h"}, EmptySHA256, at)
	if plain.Headers["x-amz-security-token"] != "" || tok.Headers["x-amz-security-token"] != "tok/en+==" ||
		!strings.Contains(tok.Authorization, "SignedHeaders=host;x-amz-content-sha256;x-amz-date;x-amz-security-token,") ||
		plain.Authorization == tok.Authorization {
		t.Errorf("plain %+v\ntoken %+v", plain, tok)
	}
	// The scope's date and x-amz-date come from one timestamp, in UTC.
	late := sign(NewCreds("AK", "SK", ""), "r", "GET", "/", nil, map[string]string{"host": "h"}, EmptySHA256,
		time.Date(2026, 10, 23, 23, 59, 59, 999e6, time.FixedZone("x", -5*3600)))
	if late.Headers["x-amz-date"] != "20261024T045959Z" || !strings.Contains(late.Authorization, "/20261024/r/s3/") {
		t.Errorf("late: %+v", late)
	}
}

func TestURIEncode(t *testing.T) {
	for in, want := range map[string]string{
		"fyisp-test/a b+c~d(é)/ü%2F=&?.bin": "fyisp-test/a%20b%2Bc~d%28%C3%A9%29/%C3%BC%252F%3D%26%3F.bin",
		"../x/./y":                          "../x/./y", // S3 never normalises
		"A-Z_a.z~0":                         "A-Z_a.z~0",
	} {
		if got := uriEncode(in, true); got != want {
			t.Errorf("uriEncode(%q) = %q, want %q", in, got, want)
		}
	}
	if got := uriEncode("a/b", false); got != "a%2Fb" {
		t.Errorf("slash = %q", got)
	}
	// Every key decodes back to itself.
	if err := quick.Check(func(s string) bool {
		d, err := url.PathUnescape(uriEncode(s, true))
		return err == nil && d == s
	}, nil); err != nil {
		t.Error(err)
	}
	if got := canonicalQuery(url.Values{"uploads": {""}, "b": {"2", "1"}, "a b": {"x/y"}}); got != "a%20b=x%2Fy&b=1&b=2&uploads=" {
		t.Errorf("canonicalQuery = %q", got)
	}
}
