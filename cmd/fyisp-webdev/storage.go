package main

import (
	"context"
	"log/slog"
	"net/netip"
	"os"
	"strings"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/s3test"
	"github.com/fuck-you-isp/fyisp/internal/storage"
)

// Storage scenarios (-storage).
const (
	storageNone    = "none"    // no storage source: Storage hidden
	storageEmpty   = "empty"   // no targets yet
	storageTargets = "targets" // a few targets
)

// newFakeStorage is the real storage service over a temporary data
// directory, with the engine's network faked: no request leaves the
// machine. Names ending .lan or .local resolve to the LAN, others to a
// public address; a host with "down" in it times out.
func newFakeStorage(ctx context.Context, scenario string, log *slog.Logger) (*storage.Service, func()) {
	dir, err := os.MkdirTemp("", "fyisp-webdev-storage-")
	if err != nil {
		log.Error("storage", "err", err)
		return nil, func() {}
	}
	lookup := func(_ context.Context, _, host string) ([]netip.Addr, error) {
		if strings.HasSuffix(host, ".lan") || strings.HasSuffix(host, ".local") {
			return []netip.Addr{netip.MustParseAddr("192.168.1.20")}, nil
		}
		return []netip.Addr{netip.MustParseAddr("52.216.10.10")}, nil
	}
	e := s3test.New(s3test.Options{Lookup: lookup, Fake: fakeAttempt, IsSelf: func(netip.Addr) bool { return false }})
	s := storage.Open(storage.Options{Dir: dir, Engine: e, Lookup: lookup})
	if scenario == storageTargets {
		for _, sp := range []storage.Spec{
			{Name: "AWS Virginia", Endpoint: "https://s3.us-east-1.amazonaws.com", Region: "us-east-1", Bucket: "fyisp-probe"},
			{Name: "Backups (R2)", Endpoint: "https://0123456789abcdef.r2.cloudflarestorage.com", Region: "auto", Bucket: "fyisp-probe", Addressing: "path"},
			{Name: "NAS", Endpoint: "http://nas.lan:9000", Bucket: "probe", Addressing: "path"},
			{Name: "Flaky edge", Endpoint: "https://down.storage.example"},
		} {
			if _, err := s.Add(ctx, sp); err != nil {
				log.Error("storage target", "name", sp.Name, "err", err)
			}
		}
	}
	return s, func() { os.RemoveAll(dir) }
}

// fakeAttempt answers one request after a short wait, as a server would:
// an unsigned request for a bucket is refused (403), as by a real one.
func fakeAttempt(ctx context.Context, t s3test.Target, phase, op string, signed bool) model.StorageAttempt {
	f := func(v float64) *float64 { return &v }
	a := model.StorageAttempt{Phase: phase, Op: op, At: time.Now(), DNSms: f(3.2)}
	select {
	case <-ctx.Done():
		return model.StorageAttempt{} // cut off: no class
	case <-time.After(400 * time.Millisecond):
	}
	switch {
	case strings.Contains(t.Endpoint.Hostname(), "down"):
		a.Class, a.Totalms = string(s3test.ClassConnectTimeout), f(5003)
		return a
	case t.Bucket != "" && !signed:
		a.Class, a.HTTPStatus = string(s3test.ClassOK), 403
	default:
		a.Class, a.HTTPStatus = string(s3test.ClassOK), 200
	}
	a.Connectms, a.TTFBms, a.Totalms = f(11.4), f(31.0), f(48.6)
	if t.Endpoint.Scheme == "https" {
		a.TLSms, a.TLSVersion = f(23.1), "1.3"
	}
	return a
}
