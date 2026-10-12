//go:build netns && linux

package netns

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/pem"
	"fmt"
	"log"
	"maps"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
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
//   - DNS on dnsAddr:53/udp: svc.fyisp.test and www.fyisp.test ->
//     dnsTargetAddr, stun.provider-a.test and stun.provider-b.test ->
//     stunAddr, NXDOMAIN for everything else;
//   - STUN (RFC 5389) on stunAddr, UDP ports stunPorts, and on the
//     addresses of stunMore: a Binding Success carrying XOR-MAPPED-ADDRESS
//     mappedAddr:mappedPort for every Binding Request (udp_test.go checks
//     that fyisp never shows it);
//   - iperf3 on iperfAddrs, ports iperfPorts (iperf_test.go).
//
// ICMP echo is answered by the kernel.

const (
	responderEnv = "FYISP_NETNS_RESPONDER" // set to the directory holding the certificates
	dnsName      = "svc.fyisp.test"
	dnsName2     = "www.fyisp.test" // a second name for the same address (TestVerdict)

	// Two STUN "providers" (different registrable domains, which is how
	// fyisp tells providers apart), both served by the responder on stunAddr.
	stunNameA  = "stun.provider-a.test"
	stunNameB  = "stun.provider-b.test"
	stunPortA  = 3478 // stun-a (provider A)
	stunPortB  = 3479 // stun-b (provider B)
	stunPort53 = 53   // stun-53 (provider B, tagged udp-53)
	// stunPortGoogle is Google's STUN port (stunAddr3).
	stunPortGoogle = 19302
	mappedPort     = 40077
)

// mappedAddr is the "public address" the STUN responder reports.
var (
	mappedAddr = netip.MustParseAddr("198.51.100.77")
	stunPorts  = []int{stunPortA, stunPortB, stunPort53}
	// stunMore are the STUN servers on other addresses (TestHome10, where
	// every address is a provider of its own): a second one on 3478, and a
	// third on Google's 19302 and 3478.
	stunMore = map[netip.Addr][]int{stunAddr2: {stunPortA}, stunAddr3: {stunPortGoogle, stunPortA}}
)

// dnsRecords are the names the test resolver answers.
var dnsRecords = map[string]netip.Addr{
	dnsName: dnsTargetAddr, dnsName2: dnsTargetAddr,
	stunNameA: stunAddr, stunNameB: stunAddr,
}

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
	if err := leaf("good", 2, goodIPs, []string{dnsName, dnsName2}, caCert, caKey); err != nil {
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
	stun := map[netip.Addr][]int{stunAddr: stunPorts}
	maps.Copy(stun, stunMore)
	for a, ports := range stun {
		for _, port := range ports {
			pc, err := net.ListenPacket("udp4", net.JoinHostPort(a.String(), strconv.Itoa(port)))
			if err != nil {
				log.Fatal(err)
			}
			go serveSTUN(pc)
		}
	}
	serveIperf()
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
		addr, known := dnsRecords[name]
		if !known {
			rh.RCode = dnsmessage.RCodeNameError
		}
		b := dnsmessage.NewBuilder(make([]byte, 0, 512), rh)
		b.EnableCompression()
		_ = b.StartQuestions()
		_ = b.Question(q)
		if known && q.Type == dnsmessage.TypeA && q.Class == dnsmessage.ClassINET {
			_ = b.StartAnswers()
			_ = b.AResource(dnsmessage.ResourceHeader{Name: q.Name, Class: dnsmessage.ClassINET, TTL: 5},
				dnsmessage.AResource{A: addr.As4()})
		}
		out, err := b.Finish()
		if err != nil {
			continue
		}
		_, _ = pc.WriteTo(out, from)
	}
}

// serveSTUN answers every Binding Request on pc with a Binding Success
// (RFC 5389) that carries XOR-MAPPED-ADDRESS mappedAddr:mappedPort, like a
// public STUN server telling the client its public address.
func serveSTUN(pc net.PacketConn) {
	const cookie = 0x2112A442
	buf := make([]byte, 1500)
	for {
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			log.Fatal(err)
		}
		req := buf[:n]
		if n < 20 || binary.BigEndian.Uint16(req[0:2]) != 0x0001 || binary.BigEndian.Uint32(req[4:8]) != cookie {
			continue
		}
		out := make([]byte, 20+12)
		binary.BigEndian.PutUint16(out[0:2], 0x0101)   // Binding Success
		binary.BigEndian.PutUint16(out[2:4], 12)       // one attribute
		copy(out[4:20], req[4:20])                     // magic cookie, transaction ID
		binary.BigEndian.PutUint16(out[20:22], 0x0020) // XOR-MAPPED-ADDRESS
		binary.BigEndian.PutUint16(out[22:24], 8)
		out[25] = 0x01 // IPv4
		binary.BigEndian.PutUint16(out[26:28], mappedPort^(cookie>>16))
		a := mappedAddr.As4()
		binary.BigEndian.PutUint32(out[28:32], binary.BigEndian.Uint32(a[:])^cookie)
		_, _ = pc.WriteTo(out, from)
	}
}
