//go:build netns && linux

package netns

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// A small iperf3 server (the stock 3.x control protocol, TCP only), run by
// the responder in the internet namespace for TestSpeed. Like stock iperf3,
// each port runs one test at a time and answers other clients with
// ACCESS_DENIED. Servers (one address each, ports iperfPorts):
//
//	iperfA    a normal server ("provider A")
//	iperfB    a normal server ("provider B", shaped alone in one-path)
//	iperfBusy every port is always busy (ACCESS_DENIED)
//	iperfFail closes the data streams 1 s into every test
var (
	iperfA     = netip.MustParseAddr("203.0.113.60") // .40-.46: hopshare_test.go
	iperfB     = netip.MustParseAddr("203.0.113.61")
	iperfBusy  = netip.MustParseAddr("203.0.113.62")
	iperfFail  = netip.MustParseAddr("203.0.113.63")
	iperfAddrs = []netip.Addr{iperfA, iperfB, iperfBusy, iperfFail}
	iperfPorts = []int{5201, 5202, 5203}
)

const (
	ipStateTestStart       = 1
	ipStateTestRunning     = 2
	ipStateTestEnd         = 4
	ipStateParamExchange   = 9
	ipStateCreateStreams   = 10
	ipStateExchangeResults = 13
	ipStateDisplayResults  = 14
	ipStateIperfDone       = 16
	ipStateAccessDenied    = 0xff
	ipCookieSize           = 37
)

// iperfServer is one port.
type iperfServer struct {
	ln   net.Listener
	mode string // "", "busy", "fail"

	mu  sync.Mutex
	cur *iperfTest // the running test
}

type iperfTest struct {
	cookie  string
	streams chan net.Conn
}

// serveIperf starts every server; it never returns errors after start.
func serveIperf() {
	for _, a := range iperfAddrs {
		mode := ""
		switch a {
		case iperfBusy:
			mode = "busy"
		case iperfFail:
			mode = "fail"
		}
		for _, p := range iperfPorts {
			ln, err := net.Listen("tcp4", net.JoinHostPort(a.String(), strconv.Itoa(p)))
			if err != nil {
				log.Fatal(err)
			}
			s := &iperfServer{ln: ln, mode: mode}
			go s.accept()
		}
	}
}

func (s *iperfServer) accept() {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			log.Fatal(err)
		}
		go s.conn(c)
	}
}

// conn reads the cookie and decides: a data stream of the running test, a
// busy answer, or a new test.
func (s *iperfServer) conn(c net.Conn) {
	var b [ipCookieSize]byte
	c.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.ReadFull(c, b[:]); err != nil {
		c.Close()
		return
	}
	c.SetReadDeadline(time.Time{})
	cookie := string(b[:ipCookieSize-1])
	s.mu.Lock()
	cur := s.cur
	switch {
	case cur != nil && cur.cookie == cookie:
		s.mu.Unlock()
		select {
		case cur.streams <- c:
		case <-time.After(5 * time.Second):
			c.Close()
		}
		return
	case cur != nil || s.mode == "busy":
		s.mu.Unlock()
		c.Write([]byte{ipStateAccessDenied})
		c.Close()
		return
	}
	t := &iperfTest{cookie: cookie, streams: make(chan net.Conn, 128)}
	s.cur = t
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.cur = nil
		s.mu.Unlock()
	}()
	if err := s.session(c, t); err != nil {
		log.Printf("iperf %s: %v", s.ln.Addr(), err)
	}
}

func ipWriteJSON(c net.Conn, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = c.Write(append(binary.BigEndian.AppendUint32(nil, uint32(len(b))), b...))
	return err
}

func ipReadJSON(c net.Conn, v any) error {
	var n uint32
	if err := binary.Read(c, binary.BigEndian, &n); err != nil {
		return err
	}
	if n > 1<<20 {
		return errors.New("JSON too long")
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(c, b); err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

type ipStream struct {
	ID          int     `json:"id"`
	Bytes       int64   `json:"bytes"`
	Retransmits int64   `json:"retransmits"`
	StartTime   float64 `json:"start_time"`
	EndTime     float64 `json:"end_time"`
}

// session runs one test on control connection c.
func (s *iperfServer) session(c net.Conn, t *iperfTest) error {
	defer c.Close()
	c.SetDeadline(time.Now().Add(60 * time.Second))
	if _, err := c.Write([]byte{ipStateParamExchange}); err != nil {
		return err
	}
	var p struct {
		Parallel int  `json:"parallel"`
		Reverse  bool `json:"reverse"`
		Omit     int  `json:"omit"`
	}
	if err := ipReadJSON(c, &p); err != nil {
		return err
	}
	if p.Parallel < 1 || p.Parallel > 128 {
		return errors.New("bad parallel")
	}
	if _, err := c.Write([]byte{ipStateCreateStreams}); err != nil {
		return err
	}
	streams := make([]net.Conn, 0, p.Parallel)
	defer func() {
		for _, st := range streams {
			st.Close()
		}
	}()
	for range p.Parallel {
		select {
		case st := <-t.streams:
			streams = append(streams, st)
		case <-time.After(10 * time.Second):
			return errors.New("data streams did not connect")
		}
	}
	if _, err := c.Write([]byte{ipStateTestStart, ipStateTestRunning}); err != nil {
		return err
	}
	start := time.Now()
	omitEnd := start.Add(time.Duration(p.Omit) * time.Second)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	recv := make([]atomic.Int64, len(streams))
	for i, st := range streams {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]byte, 128<<10)
			for {
				select {
				case <-stop:
					return
				default:
				}
				if p.Reverse {
					if _, err := st.Write(buf); err != nil {
						return
					}
					continue
				}
				k, err := st.Read(buf)
				if time.Now().After(omitEnd) {
					recv[i].Add(int64(k))
				}
				if err != nil {
					return
				}
			}
		}()
	}
	if s.mode == "fail" {
		time.Sleep(time.Second)
		close(stop)
		for _, st := range streams {
			st.Close()
		}
		wg.Wait()
		return nil // and the control connection closes
	}
	var st [1]byte
	_, err := io.ReadFull(c, st[:])
	close(stop)
	for _, x := range streams {
		x.Close() // as iperf3 does on TEST_END
	}
	wg.Wait()
	if err != nil || st[0] != ipStateTestEnd {
		return errors.New("no TEST_END")
	}
	end := time.Since(start).Seconds()
	if _, err := c.Write([]byte{ipStateExchangeResults}); err != nil {
		return err
	}
	var cr map[string]any
	if err := ipReadJSON(c, &cr); err != nil {
		return err
	}
	res := struct {
		CPU        float64    `json:"cpu_util_total"`
		HasRetrans int        `json:"sender_has_retransmits"`
		Streams    []ipStream `json:"streams"`
	}{HasRetrans: -1}
	for i := range streams {
		id := 1
		if i > 0 {
			id = i + 2
		}
		res.Streams = append(res.Streams, ipStream{ID: id, Bytes: recv[i].Load(), Retransmits: -1, StartTime: float64(p.Omit), EndTime: end})
	}
	if err := ipWriteJSON(c, res); err != nil {
		return err
	}
	if _, err := c.Write([]byte{ipStateDisplayResults}); err != nil {
		return err
	}
	_, _ = io.ReadFull(c, st[:]) // IPERF_DONE
	return nil
}
