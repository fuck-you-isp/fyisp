package storage

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/s3test"
	"github.com/fuck-you-isp/fyisp/internal/s3test/s3fake"
)

var ctx = context.Background()

// lab is a Service over a fake S3 server reached as s3.test (and
// <bucket>.s3.test), which resolves to 127.0.0.1.
type lab struct {
	srv   *s3fake.Server
	s     *Service
	dir   string
	port  string
	addrs map[string][]netip.Addr // more names
	busy  atomic.Bool
}

func newLab(t *testing.T, tlsOn bool) *lab {
	t.Helper()
	l := &lab{srv: s3fake.New("bkt"), dir: t.TempDir(), addrs: map[string][]netip.Addr{}}
	if tlsOn {
		l.srv.StartTLS()
	} else {
		l.srv.Start()
	}
	t.Cleanup(l.srv.Close)
	_, l.port, _ = net.SplitHostPort(l.srv.Addr())
	l.s = l.open()
	return l
}

func (l *lab) lookup(_ context.Context, _, host string) ([]netip.Addr, error) {
	if a, ok := l.addrs[host]; ok {
		return a, nil
	}
	if host == "s3.test" || strings.HasSuffix(host, ".s3.test") {
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	}
	return nil, errors.New("no such host")
}

func (l *lab) open() *Service {
	e := s3test.New(s3test.Options{Lookup: l.lookup, IsSelf: func(netip.Addr) bool { return false }})
	return Open(Options{Dir: l.dir, Engine: e, Lookup: l.lookup, Busy: l.busy.Load})
}

func (l *lab) endpoint(scheme string) string { return scheme + "://s3.test:" + l.port }

func (l *lab) add(t *testing.T, sp Spec) TargetState {
	t.Helper()
	ts, err := l.s.Add(ctx, sp)
	if err != nil {
		t.Fatalf("Add %+v: %v", sp, err)
	}
	return ts
}

func fieldOf(err error) (field, code string) {
	var fe *FieldError
	if errors.As(err, &fe) {
		return fe.Field, fe.Code
	}
	return "", ""
}

func ptr(s string) *string { return &s }

func TestAddDefaults(t *testing.T) {
	l := newLab(t, false)
	ts := l.add(t, Spec{Name: "  Lab ", Endpoint: "HTTP://S3.Test:" + l.port + "/", Bucket: "bkt"})
	tg := ts.Target
	if tg.Name != "Lab" || tg.Endpoint != l.endpoint("http") || tg.Region != "us-east-1" || tg.Addressing != "virtual-host" ||
		tg.Network != "private" || !strings.HasPrefix(tg.ID, "t_") || len(tg.ID) != 10 || tg.CreatedMs == 0 || tg.CreatedMs != tg.UpdatedMs {
		t.Errorf("target %+v", tg)
	}
	if ts.Check != nil {
		t.Errorf("state %+v", ts)
	}
	if tg.Host() != "s3.test" || tg.Port() != l.port {
		t.Errorf("host %q port %q", tg.Host(), tg.Port())
	}
	// It survives a restart.
	got, err := l.open().Targets()
	if err != nil || len(got) != 1 || got[0].Target != tg {
		t.Fatalf("reopened: %+v %v", got, err)
	}
}

func TestAddRefused(t *testing.T) {
	l := newLab(t, false)
	l.addrs["mixed.example"] = []netip.Addr{netip.MustParseAddr("10.0.0.5"), netip.MustParseAddr("52.216.1.1")}
	l.addrs["linklocal.example"] = []netip.Addr{netip.MustParseAddr("169.254.169.254")}
	l.addrs["public.example"] = []netip.Addr{netip.MustParseAddr("52.216.1.1")}
	l.addrs["cgnat.example"] = []netip.Addr{netip.MustParseAddr("100.64.1.1")}
	for _, c := range []struct {
		sp          Spec
		field, code string
	}{
		{Spec{Name: "", Endpoint: l.endpoint("http")}, "name", "bad_request"},
		{Spec{Name: strings.Repeat("x", 65), Endpoint: l.endpoint("http")}, "name", "bad_request"},
		{Spec{Name: "a\nb", Endpoint: l.endpoint("http")}, "name", "bad_request"},
		{Spec{Name: "a", Endpoint: "ftp://s3.test"}, "endpoint", "bad_request"},
		{Spec{Name: "a", Endpoint: "https://s3.test/path"}, "endpoint", "bad_request"},
		{Spec{Name: "a", Endpoint: "https://user@s3.test"}, "endpoint", "bad_request"},
		{Spec{Name: "a", Endpoint: "https://s3.test", Bucket: "a/b"}, "bucket", "bad_request"},
		{Spec{Name: "a", Endpoint: "https://s3.test", Region: "us east"}, "region", "bad_request"},
		{Spec{Name: "a", Endpoint: "https://s3.test", Addressing: "dns"}, "addressing", "bad_request"},
		{Spec{Name: "a", Endpoint: "https://s3.test", CAPEM: "not a pem"}, "ca_pem", "bad_request"},
		{Spec{Name: "a", Endpoint: "https://nowhere.example"}, "endpoint", "bad_request"},
		{Spec{Name: "a", Endpoint: "https://mixed.example"}, "endpoint", "addr_refused"},
		{Spec{Name: "a", Endpoint: "https://linklocal.example"}, "endpoint", "addr_refused"},
		{Spec{Name: "a", Endpoint: "https://169.254.169.254"}, "endpoint", "addr_refused"},
		{Spec{Name: "a", Endpoint: "http://public.example"}, "endpoint", "bad_request"},
		{Spec{Name: "a", Endpoint: "http://cgnat.example"}, "endpoint", "bad_request"},
		{Spec{Name: "a", Endpoint: "https://s3.test?"}, "endpoint", "bad_request"},
		{Spec{Name: "a", Endpoint: "https://s3.test:"}, "endpoint", "bad_request"},
		{Spec{Name: "a\u0085b", Endpoint: "https://s3.test"}, "name", "bad_request"},
		{Spec{Name: "a", Endpoint: "https://192.0.2.10"}, "endpoint", "addr_refused"},
		{Spec{Name: "a", Endpoint: "https://198.18.0.1"}, "endpoint", "addr_refused"},
		{Spec{Name: "a", Endpoint: "https://100.100.100.200"}, "endpoint", "addr_refused"},
	} {
		_, err := l.s.Add(ctx, c.sp)
		if f, code := fieldOf(err); f != c.field || code != c.code {
			t.Errorf("%+v: %v (field %q, code %q), want %s %s", c.sp, err, f, code, c.field, c.code)
		}
	}
	if ts := l.add(t, Spec{Name: "p", Endpoint: "https://public.example"}); ts.Network != "public" {
		t.Errorf("network %q", ts.Network)
	}
	if ts := l.add(t, Spec{Name: "c", Endpoint: "https://cgnat.example"}); ts.Network != "private" {
		t.Errorf("CGNAT network %q", ts.Network)
	}
	if _, err := l.s.Add(ctx, Spec{Name: "P", Endpoint: "https://public.example"}); !errors.Is(err, ErrNameTaken) {
		t.Errorf("same name, other case: %v", err)
	}
	for i := len(mustTargets(t, l.s)); i < MaxTargets; i++ {
		l.add(t, Spec{Name: fmt.Sprint("n", i), Endpoint: "https://public.example"})
	}
	if _, err := l.s.Add(ctx, Spec{Name: "one more", Endpoint: "https://public.example"}); !errors.Is(err, ErrTooMany) {
		t.Errorf("target %d: %v", MaxTargets+1, err)
	}
}

func mustTargets(t *testing.T, s *Service) []TargetState {
	t.Helper()
	ts, err := s.Targets()
	if err != nil {
		t.Fatal(err)
	}
	return ts
}

// TestUpdate: a change to what a check is bound to drops the last check;
// a new name keeps it.
func TestUpdate(t *testing.T) {
	l := newLab(t, false)
	a := l.add(t, Spec{Name: "a", Endpoint: l.endpoint("http"), Bucket: "bkt"})
	l.add(t, Spec{Name: "b", Endpoint: l.endpoint("http"), Bucket: "bkt"})
	check := func() {
		t.Helper()
		if _, err := l.s.Check(ctx, a.ID, nil); err != nil {
			t.Fatal(err)
		}
	}
	check()
	rev := l.s.Rev()
	ts, err := l.s.Update(ctx, a.ID, Patch{Name: ptr("renamed")})
	if err != nil || ts.Name != "renamed" || ts.Check == nil || l.s.Rev() <= rev {
		t.Fatalf("rename: %+v %v", ts, err)
	}
	if _, err := l.s.Update(ctx, a.ID, Patch{Name: ptr("B")}); !errors.Is(err, ErrNameTaken) {
		t.Errorf("rename to b: %v", err)
	}
	if ts, err := l.s.Update(ctx, a.ID, Patch{Name: ptr("renamed")}); err != nil || ts.UpdatedMs != mustTarget(t, l.s, a.ID).UpdatedMs {
		t.Errorf("no change: %+v %v", ts, err)
	}
	for _, p := range []Patch{{Region: ptr("eu-west-1")}, {Bucket: ptr("other")}, {Addressing: ptr("path")}, {Endpoint: ptr("http://bkt.s3.test:" + l.port)}} {
		check()
		if ts, err := l.s.Update(ctx, a.ID, p); err != nil || ts.Check != nil {
			t.Errorf("%+v: %+v %v", p, ts, err)
		}
	}
	if _, err := l.s.Update(ctx, "t_00000000", Patch{Name: ptr("x")}); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown: %v", err)
	}
	if _, err := l.s.Update(ctx, a.ID, Patch{Endpoint: ptr("https://169.254.1.1")}); err == nil {
		t.Error("a link-local endpoint was saved")
	}
}

// TestUpdateChanged: a target renamed while an update resolves its new
// endpoint is ErrChanged, and the rename stays.
func TestUpdateChanged(t *testing.T) {
	l := newLab(t, false)
	a := l.add(t, Spec{Name: "a", Endpoint: l.endpoint("http"), Bucket: "bkt"})
	l.s.o.Lookup = func(ctx context.Context, network, host string) ([]netip.Addr, error) {
		if _, err := l.s.Update(ctx, a.ID, Patch{Name: ptr("meanwhile")}); err != nil {
			t.Error(err)
		}
		return l.lookup(ctx, network, host)
	}
	if _, err := l.s.Update(ctx, a.ID, Patch{Endpoint: ptr("http://other.s3.test:" + l.port)}); !errors.Is(err, ErrChanged) {
		t.Errorf("update: %v", err)
	}
	if got := mustTarget(t, l.s, a.ID); got.Name != "meanwhile" || got.Endpoint != a.Endpoint {
		t.Errorf("target %+v", got.Target)
	}
}

// TestSaveFails: a save that fails changes nothing.
func TestSaveFails(t *testing.T) {
	l := newLab(t, false)
	a := l.add(t, Spec{Name: "a", Endpoint: l.endpoint("http"), Bucket: "bkt"})
	rev := l.s.Rev()
	if err := os.RemoveAll(l.dir); err != nil {
		t.Fatal(err)
	}
	if _, err := l.s.Add(ctx, Spec{Name: "b", Endpoint: l.endpoint("http")}); err == nil {
		t.Error("Add saved without a directory")
	}
	if _, err := l.s.Update(ctx, a.ID, Patch{Name: ptr("c")}); err == nil {
		t.Error("Update saved without a directory")
	}
	if err := l.s.Delete(a.ID); err == nil {
		t.Error("Delete saved without a directory")
	}
	if ts := mustTargets(t, l.s); len(ts) != 1 || ts[0].Target != a.Target || l.s.Rev() != rev {
		t.Errorf("after failed saves: %+v, rev %d (was %d)", ts, l.s.Rev(), rev)
	}
}

func mustTarget(t *testing.T, s *Service, id string) TargetState {
	t.Helper()
	ts, err := s.Target(id)
	if err != nil {
		t.Fatal(err)
	}
	return ts
}

func TestDelete(t *testing.T) {
	l := newLab(t, false)
	a := l.add(t, Spec{Name: "a", Endpoint: l.endpoint("http"), Bucket: "bkt"})
	b := l.add(t, Spec{Name: "b", Endpoint: l.endpoint("http"), Bucket: "bkt"})
	if err := l.s.Delete(b.ID); err != nil {
		t.Fatal(err)
	}
	if err := l.s.Delete(b.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("twice: %v", err)
	}
	if got := mustTargets(t, l.open()); len(got) != 1 || got[0].ID != a.ID {
		t.Errorf("reopened: %+v", got)
	}
}

// TestRevAcrossRestarts: rev starts at the clock, so a client doesn't see
// an old rev with new content after a restart.
func TestRevAcrossRestarts(t *testing.T) {
	l := newLab(t, false)
	l.add(t, Spec{Name: "a", Endpoint: l.endpoint("http")})
	if r := l.open().Rev(); r < time.Now().Add(-time.Minute).UnixMilli() {
		t.Errorf("rev %d after a restart", r)
	}
}

func TestCheck(t *testing.T) {
	l := newLab(t, true)
	a := l.add(t, Spec{Name: "a", Endpoint: l.endpoint("https"), Bucket: "bkt", Addressing: "path", CAPEM: l.srv.CertPEM()})
	noCA := l.add(t, Spec{Name: "no ca", Endpoint: l.endpoint("https"), Bucket: "bkt"})

	c, err := l.s.Check(ctx, a.ID, s3test.NewCreds(l.srv.AccessKey, l.srv.Secret, ""))
	if err != nil || c.Class != "ok" || c.Keys != "work" || c.DuringTest || c.TLSms == nil || c.At.IsZero() {
		t.Fatalf("keys: %+v %v", c, err)
	}
	c, _ = l.s.Check(ctx, a.ID, s3test.NewCreds(l.srv.AccessKey, "wrong", ""))
	if c.Class != "http_auth" || c.Keys != "refused" || c.HTTPStatus != 403 {
		t.Errorf("wrong secret: %+v", c)
	}
	c, _ = l.s.Check(ctx, a.ID, nil)
	if c.Keys != "none" || len(l.srv.Seen()) == 0 || l.srv.Seen()[len(l.srv.Seen())-1].Signed {
		t.Errorf("no keys: %+v, last request signed", c)
	}
	l.busy.Store(true)
	c, _ = l.s.Check(ctx, a.ID, s3test.NewCreds(l.srv.AccessKey, l.srv.Secret, ""))
	if c.Keys != "work" || !c.DuringTest {
		t.Errorf("during a test: %+v", c)
	}
	if got := mustTarget(t, l.s, a.ID); got.Check == nil || *got.Check != c {
		t.Errorf("last check %+v, want %+v", got.Check, c)
	}
	c, _ = l.s.Check(ctx, noCA.ID, nil)
	if c.Class != "tls_cert" || c.Hint != "ca" {
		t.Errorf("unknown CA: %+v", c)
	}
	if _, err := l.s.Check(ctx, "t_00000000", nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown: %v", err)
	}
	// A cancelled check keeps nothing.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := l.s.Check(cctx, noCA.ID, nil); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: %v", err)
	}
	if got := mustTarget(t, l.s, noCA.ID); got.Check == nil || got.Check.Class != "tls_cert" {
		t.Errorf("after a cancelled check: %+v", got.Check)
	}

	// One at a time.
	entered, release := make(chan struct{}), make(chan struct{})
	l.srv.Fault = func(*http.Request) s3fake.Fault {
		close(entered)
		<-release
		return s3fake.Fault{}
	}
	done := make(chan struct{})
	go func() { l.s.Check(ctx, a.ID, nil); close(done) }()
	<-entered
	if _, err := l.s.Check(ctx, a.ID, nil); !errors.Is(err, ErrBusy) {
		t.Errorf("a second check: %v", err)
	}
	close(release)
	<-done
}

// TestFileRefused: a file fyisp can't use turns storage off and is never
// overwritten.
func TestFileRefused(t *testing.T) {
	for name, write := range map[string]func(path string) error{
		"not json":    func(p string) error { return os.WriteFile(p, []byte("{nope"), 0o600) },
		"new version": func(p string) error { return os.WriteFile(p, []byte(`{"version":2,"targets":[]}`), 0o600) },
		"too large":   func(p string) error { return os.WriteFile(p, make([]byte, maxFile+1), 0o600) },
		"directory":   func(p string) error { return os.Mkdir(p, 0o700) },
		"bad endpoint": func(p string) error {
			return writeTargets(p, `{"id":"t_00000001","name":"a","endpoint":"https//x","region":"us-east-1","addressing":"path","network":"public"}`)
		},
		"bad network": func(p string) error {
			return writeTargets(p, `{"id":"t_00000001","name":"a","endpoint":"https://s3.test","region":"us-east-1","addressing":"path","network":"lan"}`)
		},
		"not normalized": func(p string) error {
			return writeTargets(p, `{"id":"t_00000001","name":" a","endpoint":"https://s3.test","region":"us-east-1","addressing":"path","network":"public"}`)
		},
		"same id": func(p string) error {
			t := `{"id":"t_00000001","name":"%s","endpoint":"https://s3.test","region":"us-east-1","addressing":"path","network":"public"}`
			return writeTargets(p, fmt.Sprintf(t, "a"), fmt.Sprintf(t, "b"))
		},
		"symlink": func(p string) error {
			other := filepath.Join(filepath.Dir(p), "elsewhere.json")
			if err := os.WriteFile(other, []byte(`{"version":1,"targets":[]}`), 0o600); err != nil {
				return err
			}
			return os.Symlink(other, p)
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, FileName)
			if err := write(path); err != nil {
				t.Skip(err) // symlinks need a privilege on Windows
			}
			before, _ := os.Lstat(path)
			s := Open(Options{Dir: dir})
			if !errors.Is(s.Err(), ErrFile) {
				t.Fatalf("Err %v", s.Err())
			}
			if _, err := s.Targets(); !errors.Is(err, ErrFile) {
				t.Errorf("Targets: %v", err)
			}
			if _, err := s.Add(ctx, Spec{Name: "a", Endpoint: "https://127.0.0.1"}); !errors.Is(err, ErrFile) {
				t.Errorf("Add: %v", err)
			}
			if err := s.Delete("t_1"); !errors.Is(err, ErrFile) {
				t.Errorf("Delete: %v", err)
			}
			after, _ := os.Lstat(path)
			if !after.ModTime().Equal(before.ModTime()) || after.Size() != before.Size() || after.Mode() != before.Mode() {
				t.Error("the file was changed")
			}
		})
	}
}

func writeTargets(path string, targets ...string) error {
	return os.WriteFile(path, []byte(`{"version":1,"targets":[`+strings.Join(targets, ",")+`]}`), 0o600)
}

// TestFileValid: a file with a valid target loads.
func TestFileValid(t *testing.T) {
	dir := t.TempDir()
	if err := writeTargets(filepath.Join(dir, FileName), `{"id":"t_00000001","name":"a","endpoint":"https://s3.test","region":"us-east-1","addressing":"path","network":"public"}`); err != nil {
		t.Fatal(err)
	}
	s := Open(Options{Dir: dir})
	if ts, err := s.Targets(); err != nil || len(ts) != 1 || ts[0].Host() != "s3.test" || ts[0].Scheme() != "https" || ts[0].Port() != "443" {
		t.Fatalf("%+v %v", ts, err)
	}
}

// TestCheckTokenOverHTTP: a session token is never sent over http://.
func TestCheckTokenOverHTTP(t *testing.T) {
	l := newLab(t, false)
	a := l.add(t, Spec{Name: "a", Endpoint: l.endpoint("http"), Bucket: "bkt"})
	_, err := l.s.Check(ctx, a.ID, s3test.NewCreds(l.srv.AccessKey, l.srv.Secret, "tok"))
	if f, _ := fieldOf(err); f != "keys" || len(l.srv.Seen()) != 0 {
		t.Errorf("%v, %d requests", err, len(l.srv.Seen()))
	}
}
