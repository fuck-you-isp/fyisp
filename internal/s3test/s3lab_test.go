//go:build s3lab

// The storage engine against real S3-compatible servers (RustFS, Garage,
// MinIO), started here from their binaries on random loopback ports: the
// Dockerfile's s3lab targets. A server whose binary is missing is skipped,
// unless S3LAB_REQUIRE names it.
package s3test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/speedtest"
)

const labDomain = "s3.lab"

// labServer is one running server: its endpoint, region and keys.
type labServer struct {
	name     string
	endpoint *url.URL
	region   string
	ak, sk   string
	pool     *x509.CertPool
	strict   bool // checks the region
}

func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_, p, _ := net.SplitHostPort(ln.Addr().String())
	return p
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func need(t *testing.T, bin string) string {
	t.Helper()
	p, err := exec.LookPath(bin)
	if err != nil {
		if strings.Contains(","+os.Getenv("S3LAB_REQUIRE")+",", ","+bin+",") {
			t.Fatalf("%s is required but not installed", bin)
		}
		t.Skipf("%s not installed", bin)
	}
	return p
}

// start runs cmd until the test ends, its output in the test log on
// failure.
func start(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	logf, err := os.Create(filepath.Join(t.TempDir(), "server.log"))
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdout, cmd.Stderr = logf, logf
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		if t.Failed() {
			b, _ := os.ReadFile(logf.Name())
			t.Logf("%s log (tail):\n%s", filepath.Base(cmd.Path), tail(string(b), 3000))
		}
	})
}

func tail(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

func waitPort(t *testing.T, port string) {
	t.Helper()
	for i := 0; i < 300; i++ {
		if c, err := net.DialTimeout("tcp4", "127.0.0.1:"+port, 200*time.Millisecond); err == nil {
			c.Close()
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("nothing listens on %s", port)
}

func startRustFS(t *testing.T) labServer {
	bin := need(t, "rustfs")
	port := freePort(t)
	s := labServer{name: "rustfs", region: "us-east-1", ak: "lab" + randHex(8), sk: randHex(20)}
	cmd := exec.Command(bin, "server", t.TempDir())
	cmd.Env = append(os.Environ(), "RUSTFS_ACCESS_KEY="+s.ak, "RUSTFS_SECRET_KEY="+s.sk, "RUSTFS_ADDRESS=127.0.0.1:"+port,
		"RUSTFS_SERVER_DOMAINS="+labDomain, "RUSTFS_CONSOLE_ENABLE=false", "RUSTFS_OBS_LOG_DIRECTORY="+t.TempDir())
	start(t, cmd)
	waitPort(t, port)
	s.endpoint, _ = url.Parse("http://" + labDomain + ":" + port)
	return s
}

func startMinIO(t *testing.T, withTLS bool) labServer {
	bin := need(t, "minio")
	port := freePort(t)
	s := labServer{name: "minio", region: "us-east-1", ak: "lab" + randHex(8), sk: randHex(20)}
	args := []string{"server", t.TempDir(), "--address", "127.0.0.1:" + port, "--console-address", "127.0.0.1:" + freePort(t)}
	scheme := "http"
	if withTLS {
		dir := t.TempDir()
		s.pool = writeCert(t, dir)
		args = append(args, "--certs-dir", dir)
		scheme, s.name = "https", "minio-tls"
	}
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), "MINIO_ROOT_USER="+s.ak, "MINIO_ROOT_PASSWORD="+s.sk, "MINIO_DOMAIN="+labDomain, "MINIO_BROWSER=off")
	start(t, cmd)
	waitPort(t, port)
	s.endpoint, _ = url.Parse(scheme + "://" + labDomain + ":" + port)
	return s
}

func startGarage(t *testing.T) labServer {
	bin := need(t, "garage")
	dir := t.TempDir()
	s3p, rpcp, admp := freePort(t), freePort(t), freePort(t)
	cfg := filepath.Join(dir, "garage.toml")
	if err := os.WriteFile(cfg, []byte(fmt.Sprintf(`metadata_dir = "%s/meta"
data_dir = "%s/data"
db_engine = "sqlite"
replication_factor = 1
rpc_bind_addr = "127.0.0.1:%s"
rpc_public_addr = "127.0.0.1:%s"
rpc_secret = "%s"
[s3_api]
s3_region = "garage"
api_bind_addr = "127.0.0.1:%s"
root_domain = ".%s"
[admin]
api_bind_addr = "127.0.0.1:%s"
admin_token = "%s"
`, dir, dir, rpcp, rpcp, randHex(32), s3p, labDomain, admp, randHex(24))), 0o600); err != nil {
		t.Fatal(err)
	}
	start(t, exec.Command(bin, "-c", cfg, "server"))
	waitPort(t, s3p)
	g := func(args ...string) string {
		var out []byte
		var err error
		for i := 0; i < 50; i++ { // the node takes a moment to answer its CLI
			out, err = exec.Command(bin, append([]string{"-c", cfg}, args...)...).CombinedOutput()
			if err == nil {
				return string(out)
			}
			time.Sleep(200 * time.Millisecond)
		}
		t.Fatalf("garage %v: %v\n%s", args, err, out)
		return ""
	}
	node := regexp.MustCompile(`(?m)^([0-9a-f]{16})\s`).FindStringSubmatch(g("status"))
	if node == nil {
		t.Fatal("garage: no node id")
	}
	g("layout", "assign", "-z", "dc1", "-c", "1G", node[1])
	g("layout", "apply", "--version", "1")
	key := g("key", "create", "fyisp-lab")
	ak := regexp.MustCompile(`Key ID:\s+(\S+)`).FindStringSubmatch(key)
	sk := regexp.MustCompile(`Secret key:\s+(\S+)`).FindStringSubmatch(key)
	if ak == nil || sk == nil {
		t.Fatal("garage: no key")
	}
	g("bucket", "create", "lab")
	g("bucket", "allow", "--read", "--write", "--owner", "lab", "--key", "fyisp-lab")
	ep, _ := url.Parse("http://" + labDomain + ":" + s3p)
	return labServer{name: "garage", endpoint: ep, region: "garage", ak: ak[1], sk: sk[1], strict: true}
}

// writeCert writes a self-signed cert for s3.lab and *.s3.lab into dir
// (MinIO's certs layout) and returns a pool trusting it.
func writeCert(t *testing.T, dir string) *x509.CertPool {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: labDomain},
		DNSNames: []string{labDomain, "*." + labDomain}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true, IsCA: true}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kb, _ := x509.MarshalECPrivateKey(key)
	_ = os.WriteFile(filepath.Join(dir, "public.crt"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "private.key"), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600)
	cert, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return pool
}

func labEngine(mask *speedtest.Windows) *Engine {
	return New(Options{Masker: mask, IsSelf: func(netip.Addr) bool { return false }, Lookup: func(_ context.Context, _, host string) ([]netip.Addr, error) {
		if host == labDomain || strings.HasSuffix(host, "."+labDomain) {
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		}
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}})
}

// makeBucket creates bucket with a signed PUT (Garage's comes from its CLI).
func makeBucket(t *testing.T, s labServer, bucket string) {
	t.Helper()
	tg := Target{Endpoint: s.endpoint, Region: s.region, Bucket: bucket, PathStyle: true, RootCAs: s.pool, Private: true}
	var a model.StorageAttempt
	for i := 0; i < 100; i++ { // servers listen before they are ready (503)
		a = model.StorageAttempt{}
		labEngine(nil).do(context.Background(), &tg, request{method: "PUT", creds: NewCreds(s.ak, s.sk, "")}, nil, false, &a)
		if a.HTTPStatus != http.StatusServiceUnavailable && a.HTTPStatus != 0 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if a.Class != "ok" && a.HTTPStatus != http.StatusConflict {
		t.Fatalf("create bucket on %s: %+v", s.name, a)
	}
}

func TestLab(t *testing.T) {
	for _, c := range []struct {
		name string
		mk   func(*testing.T) labServer
	}{
		{"rustfs", startRustFS}, {"garage", startGarage},
		{"minio", func(t *testing.T) labServer { return startMinIO(t, false) }},
		{"minio-tls", func(t *testing.T) labServer { return startMinIO(t, true) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := c.mk(t)
			bucket := "lab"
			if s.name != "garage" {
				makeBucket(t, s, bucket)
			}
			for _, path := range []bool{true, false} {
				mask := &speedtest.Windows{}
				e := labEngine(mask)
				rt := RoundTarget{Target: Target{ID: "t_lab", Endpoint: s.endpoint, Region: s.region, Bucket: bucket, PathStyle: path, RootCAs: s.pool, Private: true},
					Creds: NewCreds(s.ak, s.sk, "")}
				ph := Phases{Cold: 3, Warm: 2, Down: 4 << 20, Up: 2 << 20}
				r := e.RunRound(context.Background(), RoundSpec{Install: "lab0", Campaign: 1, Round: 1, Targets: []RoundTarget{rt}, Phases: ph})
				for _, a := range r.Attempts {
					if a.Class != "ok" {
						t.Errorf("%s path=%v: %s/%s %s %d %s", s.name, path, a.Phase, a.Op, a.Class, a.HTTPStatus, a.S3Code)
					}
				}
				if r.Down != 4<<20 || r.Up != 1<<10+4<<20+2<<20 || len(r.Leftovers) != 0 || len(r.Skips) != 0 {
					t.Errorf("%s path=%v: %+v", s.name, path, r)
				}
				t.Logf("%s path=%v: %d attempts, %v", s.name, path, len(r.Attempts), r.End.Sub(r.Start).Round(time.Millisecond))
				if c := e.Check(context.Background(), rt); c.Keys != "work" {
					t.Errorf("%s check: %+v", s.name, c)
				}
				bad := rt
				bad.Creds = NewCreds(s.ak, s.sk+"x", "")
				if c := e.Check(context.Background(), bad); c.Keys != "refused" || c.Attempt.Class != string(ClassHTTPAuth) {
					t.Errorf("%s bad secret: %+v", s.name, c)
				}
				if s.strict {
					wrong := rt
					wrong.Region = "eu-north-1"
					if c := e.Check(context.Background(), wrong); c.Hint != "region" {
						t.Errorf("%s wrong region: %+v", s.name, c)
					}
				}
				anon := rt
				anon.Creds = nil
				if r := e.RunRound(context.Background(), RoundSpec{Install: "lab0", Targets: []RoundTarget{anon}, Phases: Phases{Cold: 1, Warm: 1}}); len(r.Attempts) != 2 ||
					r.Attempts[0].Class != "ok" || r.Attempts[1].Class != "ok" {
					t.Errorf("%s connection-only: %v", s.name, classes(r.Attempts))
				}
				if n, class := e.Sweep(context.Background(), rt, "lab0", false); class != ClassOK || n != 2 { // the two seeds
					t.Errorf("%s sweep: %d %s", s.name, n, class)
				}
			}
		})
	}
}
