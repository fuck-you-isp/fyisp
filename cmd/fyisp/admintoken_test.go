package main

import (
	"strings"
	"testing"
)

func TestResolveAdminToken(t *testing.T) {
	// None given: a new random one at each start, never the same.
	a, gen, err := resolveAdminToken("")
	if err != nil || !gen || len(a) != minAdminToken || strings.Trim(a, "0123456789abcdef") != "" {
		t.Fatalf("generated %q %v %v", a, gen, err)
	}
	if b, _, _ := resolveAdminToken(""); b == a {
		t.Error("two starts generated the same token")
	}
	mine := strings.Repeat("a1-_.~", 6)
	if tok, gen, err := resolveAdminToken(mine); err != nil || gen || tok != mine {
		t.Errorf("given %q: %q %v %v", mine, tok, gen, err)
	}
	if _, err := parseFlags([]string{"--admin-token", mine}); err != nil {
		t.Errorf("--admin-token %q refused: %v", mine, err)
	}
	// Refused when the flags are parsed, before anything starts.
	for _, bad := range []string{"off", "short", strings.Repeat("a", minAdminToken-1), strings.Repeat("a", 31) + " b",
		strings.Repeat("a", 32) + ";", strings.Repeat("a", 32) + "+", strings.Repeat("é", 32)} {
		if _, err := parseFlags([]string{"--admin-token", bad}); err == nil {
			t.Errorf("--admin-token %q accepted", bad)
		}
	}
	t.Setenv("FYISP_ADMIN_TOKEN", "short")
	if _, err := parseFlags(nil); err == nil {
		t.Error("FYISP_ADMIN_TOKEN=short accepted")
	}
}

func TestAdminTokenEnv(t *testing.T) {
	tok := strings.Repeat("e", 32)
	t.Setenv("FYISP_ADMIN_TOKEN", tok)
	if c, err := parseFlags(nil); err != nil || c.adminToken != tok {
		t.Errorf("FYISP_ADMIN_TOKEN: %q, %v", c.adminToken, err)
	}
	if c, err := parseFlags([]string{"--admin-token", strings.Repeat("f", 32)}); err != nil || c.adminToken != strings.Repeat("f", 32) {
		t.Errorf("--admin-token over FYISP_ADMIN_TOKEN: %q, %v", c.adminToken, err)
	}
}

func TestAdminLink(t *testing.T) {
	if got := adminLink("http://127.0.0.1:3000/", "abc"); got != "http://127.0.0.1:3000/#admin=abc" {
		t.Errorf("adminLink = %q", got)
	}
}
