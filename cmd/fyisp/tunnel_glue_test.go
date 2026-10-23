package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/fuck-you-isp/fyisp/internal/tunnel"
)

// TestDashTunnel: each connected URL becomes the allowed host name and is
// printed; the admin link only with a generated token, and never logged.
func TestDashTunnel(t *testing.T) {
	var got tunnel.Options
	startTunnel = func(_ context.Context, o tunnel.Options) (*tunnel.Tunnel, error) { got = o; return nil, nil }
	t.Cleanup(func() { startTunnel = tunnel.Start })
	for _, stored := range []bool{true, false} {
		var logBuf bytes.Buffer
		log := slog.New(slog.NewTextHandler(&logBuf, nil))
		d := newDashTunnel()
		if d.Host() != "" {
			t.Fatalf("host before start: %q", d.Host())
		}
		if err := d.start(context.Background(), "0.0.0.0:3001", "tok123", stored, log); err != nil {
			t.Fatal(err)
		}
		if got.Origin != "http://127.0.0.1:3001" {
			t.Errorf("origin %q", got.Origin)
		}
		out := captureStderr(t, func() { got.OnURL("https://calm-river-1234.trycloudflare.com") })
		if d.Host() != "calm-river-1234.trycloudflare.com" {
			t.Errorf("host %q", d.Host())
		}
		if !strings.Contains(out, "Dashboard over HTTPS:  https://calm-river-1234.trycloudflare.com/\n") {
			t.Errorf("stderr %q", out)
		}
		if strings.Contains(out, "#admin=tok123") != stored {
			t.Errorf("stored %v: admin link printed %v: %q", stored, !stored, out)
		}
		if strings.Contains(logBuf.String(), "tok123") || !strings.Contains(logBuf.String(), "trycloudflare.com/") {
			t.Errorf("log %q", logBuf.String())
		}
		captureStderr(t, func() { got.OnURL("https://new-name.trycloudflare.com") })
		if d.Host() != "new-name.trycloudflare.com" {
			t.Errorf("host after a replacement %q", d.Host())
		}
	}
}

func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	fn()
	os.Stderr = old
	w.Close()
	b, _ := io.ReadAll(r)
	return string(b)
}

func TestTunnelOrigin(t *testing.T) {
	for addr, want := range map[string]string{
		"127.0.0.1:3000": "http://127.0.0.1:3000", "0.0.0.0:3001": "http://127.0.0.1:3001", "[::]:3000": "http://127.0.0.1:3000",
		"192.168.1.10:3000": "http://192.168.1.10:3000", "[fe80::1%eth0]:3000": "http://[fe80::1%25eth0]:3000",
	} {
		got := tunnelOrigin(addr)
		if got != want {
			t.Errorf("tunnelOrigin(%q) = %q, want %q", addr, got, want)
		}
		if u, err := url.Parse(got); err != nil || u.Host == "" {
			t.Errorf("%q: %v", got, err)
		}
	}
}
