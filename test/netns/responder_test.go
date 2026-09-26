//go:build netns && linux

package netns

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// The responder runs in the "internet" namespace as a re-exec of the test
// binary (see TestMain). It serves:
//
//   - HTTPS (and so TCP) on :443 of every address: a certificate from the
//     test CA for the "good" addresses and svc.fyisp.test, a self-signed one
//     for badTLSAddr;
//   - DNS on dnsAddr:53/udp: svc.fyisp.test -> dnsTargetAddr, NXDOMAIN for
//     everything else.
//
// ICMP echo is answered by the kernel.

const (
	responderEnv = "FYISP_NETNS_RESPONDER" // set to the directory holding the certificates
	dnsName      = "svc.fyisp.test"
)

// writeCerts creates the test CA (ca.pem, trusted by fyisp via
// SSL_CERT_FILE), the server certificate it signs (good.pem/good.key) and an
// untrusted self-signed certificate (bad.pem/bad.key).
func writeCerts(dir string, goodIPs []netip.Addr, badIP netip.Addr) error {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	now := time.Now()
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "fyisp netns test CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return err
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return err
	}
	if err := writePEM(filepath.Join(dir, "ca.pem"), "CERTIFICATE", caDER); err != nil {
		return err
	}
	leaf := func(name string, serial int64, ips []netip.Addr, dns []string, parent *x509.Certificate, parentKey *ecdsa.PrivateKey) error {
		k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return err
		}
		tmpl := &x509.Certificate{
			SerialNumber: big.NewInt(serial),
			Subject:      pkix.Name{CommonName: name},
			NotBefore:    now.Add(-time.Hour),
			NotAfter:     now.Add(24 * time.Hour),
			KeyUsage:     x509.KeyUsageDigitalSignature,
			ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
			DNSNames:     dns,
		}
		for _, ip := range ips {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip.AsSlice())
		}
		if parent == nil { // self-signed
			parent, parentKey = tmpl, k
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, &k.PublicKey, parentKey)
		if err != nil {
			return err
		}
		kb, err := x509.MarshalECPrivateKey(k)
		if err != nil {
			return err
		}
		if err := writePEM(filepath.Join(dir, name+".pem"), "CERTIFICATE", der); err != nil {
			return err
		}
		return writePEM(filepath.Join(dir, name+".key"), "EC PRIVATE KEY", kb)
	}
	if err := leaf("good", 2, goodIPs, []string{dnsName}, caCert, caKey); err != nil {
		return err
	}
	return leaf("bad", 3, []netip.Addr{badIP}, nil, nil, nil)
}

func writePEM(path, typ string, der []byte) error {
	return os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), 0o644)
}

// runResponder serves until killed. It never returns.
func runResponder(dir string) {
	log.SetPrefix("responder: ")
	good, err := tls.LoadX509KeyPair(filepath.Join(dir, "good.pem"), filepath.Join(dir, "good.key"))
	if err != nil {
		log.Fatal(err)
	}
	bad, err := tls.LoadX509KeyPair(filepath.Join(dir, "bad.pem"), filepath.Join(dir, "bad.key"))
	if err != nil {
		log.Fatal(err)
	}
	tc := &tls.Config{
		GetCertificate: func(h *tls.ClientHelloInfo) (*tls.Certificate, error) {
			if a, ok := h.Conn.LocalAddr().(*net.TCPAddr); ok && a.IP.Equal(badTLSAddr.AsSlice()) {
				return &bad, nil
			}
			return &good, nil
		},
		NextProtos: []string{"h2", "http/1.1"},
	}
	ln, err := net.Listen("tcp4", ":443")
	if err != nil {
		log.Fatal(err)
	}
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}),
		TLSConfig: tc,
		ErrorLog:  log.New(os.Stderr, "responder https: ", 0),
	}
	go func() { log.Fatal(srv.ServeTLS(ln, "", "")) }()

	pc, err := net.ListenPacket("udp4", net.JoinHostPort(dnsAddr.String(), "53"))
	if err != nil {
		log.Fatal(err)
	}
	go serveDNS(pc)
	fmt.Println("responder ready") // the test waits for this line
	select {}
}

func serveDNS(pc net.PacketConn) {
	buf := make([]byte, 1500)
	for {
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			log.Fatal(err)
		}
		var p dnsmessage.Parser
		h, err := p.Start(buf[:n])
		if err != nil {
			continue
		}
		q, err := p.Question()
		if err != nil {
			continue
		}
		rh := dnsmessage.Header{ID: h.ID, Response: true, Authoritative: true, RecursionDesired: h.RecursionDesired, RecursionAvailable: true}
		name := strings.ToLower(strings.TrimSuffix(q.Name.String(), "."))
		if name != dnsName {
			rh.RCode = dnsmessage.RCodeNameError
		}
		b := dnsmessage.NewBuilder(make([]byte, 0, 512), rh)
		b.EnableCompression()
		_ = b.StartQuestions()
		_ = b.Question(q)
		if name == dnsName && q.Type == dnsmessage.TypeA && q.Class == dnsmessage.ClassINET {
			_ = b.StartAnswers()
			_ = b.AResource(dnsmessage.ResourceHeader{Name: q.Name, Class: dnsmessage.ClassINET, TTL: 5},
				dnsmessage.AResource{A: dnsTargetAddr.As4()})
		}
		out, err := b.Finish()
		if err != nil {
			continue
		}
		_, _ = pc.WriteTo(out, from)
	}
}
