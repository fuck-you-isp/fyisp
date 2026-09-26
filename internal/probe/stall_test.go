package probe

import (
	"context"
	"io"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"
)

// freezingProxy forwards TCP to target. freeze() stops forwarding on every
// connection open at that moment (they stay open, like a connection stalled
// by an outage); connections made afterwards work normally.
type freezingProxy struct {
	ln     net.Listener
	target string
	mu     sync.Mutex
	gen    int
}

func newFreezingProxy(t *testing.T, target string) *freezingProxy {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &freezingProxy{ln: ln, target: target}
	t.Cleanup(func() { ln.Close() })
	go p.serve()
	return p
}

func (p *freezingProxy) port() int { return p.ln.Addr().(*net.TCPAddr).Port }

func (p *freezingProxy) freeze() { p.mu.Lock(); p.gen++; p.mu.Unlock() }

func (p *freezingProxy) alive(gen int) bool { p.mu.Lock(); defer p.mu.Unlock(); return gen == p.gen }

func (p *freezingProxy) serve() {
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		p.mu.Lock()
		gen := p.gen
		p.mu.Unlock()
		u, err := net.Dial("tcp4", p.target)
		if err != nil {
			c.Close()
			continue
		}
		pipe := func(dst, src net.Conn) {
			buf := make([]byte, 32<<10)
			for {
				n, err := src.Read(buf)
				if n > 0 {
					if !p.alive(gen) {
						select {} // frozen: hold the connection open, forward nothing
					}
					if _, werr := dst.Write(buf[:n]); werr != nil {
						return
					}
				}
				if err != nil {
					if err != io.EOF {
						return
					}
					return
				}
			}
		}
		go pipe(u, c)
		go pipe(c, u)
	}
}

// After an outage the HTTPS probe must not keep using the stalled
// connection: the first probe after the path recovers has to succeed.
func TestHTTPSRecoversFromStalledConnection(t *testing.T) {
	_, port, tc := tlsServer(t, true) // HTTP/2: one shared connection
	px := newFreezingProxy(t, "127.0.0.1:"+strconv.Itoa(port))
	r := New(Options{TLSConfig: tc, Timeout: 500 * time.Millisecond, Log: quietLog()}).(*runner)
	s := &series{host: newHostState("127.0.0.1"), port: px.port(), url: httpsURL("127.0.0.1", px.port(), "/")}
	s.client.Store(r.newClient(s))
	ctx := context.Background()

	if _, _, _, err := r.probeHTTPS(ctx, s); err != nil {
		t.Fatalf("baseline probe: %v", err)
	}
	px.freeze() // outage: the kept-alive connection stops forwarding
	if _, _, _, err := r.probeHTTPS(ctx, s); err == nil {
		t.Fatal("probe during the outage succeeded")
	}
	// Path is back (new connections work). The next probe must succeed.
	for i := 0; i < 2; i++ {
		if _, _, _, err := r.probeHTTPS(ctx, s); err != nil {
			t.Fatalf("probe %d after recovery still failing: %v", i+1, err)
		}
	}
}
