package probe

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

// UDP targets are measured with STUN Binding Requests (RFC 5389), the first
// packets a WebRTC call sends, on the UDP path call media uses. A sample is a
// burst of BurstSize requests BurstGap apart on one long-lived connected
// socket per target: real calls keep one 5-tuple, and a fresh socket per
// sample would use a NAT mapping each. The socket is reopened only when the
// target's address changes or after a socket error, so a burst never spans
// an address change (a GeoDNS relay switch is not jitter).
//
// Privacy: a Binding Success carries the user's public address
// (XOR-MAPPED-ADDRESS). The parser skips it without decoding, datagrams are
// never kept, and UDP error strings (which contain addresses) are never
// logged or stored: only the model.Reason is.
const (
	// BurstSize is the number of requests per sample.
	BurstSize = 5
	// BurstGap is the time between the requests of a burst.
	BurstGap = 20 * time.Millisecond
	// udpTimeout is how long a request waits for its answer. Options.Timeout
	// does not apply to UDP.
	udpTimeout = time.Second

	// stunBufSize bounds a read: the IPv4 minimum reassembly size less the
	// IP and UDP headers. Real answers are 32-76 bytes; anything larger is
	// truncated and then rejected by parseSTUN.
	stunBufSize = 548

	stunHeaderLen      = 20
	stunMagicCookie    = 0x2112A442
	stunBindingRequest = 0x0001
	stunBindingSuccess = 0x0101
)

// stunRequest is a Binding Request without attributes (20 bytes): every
// STUN server answers it, and it carries nothing about the client.
func stunRequest(txid [12]byte) []byte {
	b := make([]byte, stunHeaderLen)
	binary.BigEndian.PutUint16(b[0:2], stunBindingRequest)
	binary.BigEndian.PutUint16(b[2:4], 0)
	binary.BigEndian.PutUint32(b[4:8], stunMagicCookie)
	copy(b[8:20], txid[:])
	return b
}

// parseSTUN validates a Binding Success Response and returns its
// transaction ID. It checks the header (type, length, magic cookie) and
// bounds-checks every attribute including its padding, but never decodes
// one: XOR-MAPPED-ADDRESS and MAPPED-ADDRESS (the user's public address)
// are skipped like any other attribute.
func parseSTUN(b []byte) (txid [12]byte, ok bool) {
	if len(b) < stunHeaderLen {
		return txid, false
	}
	// The two top bits of a STUN message are zero; the type must be a
	// Binding Success (an error response or a request is not an answer).
	if binary.BigEndian.Uint16(b[0:2]) != stunBindingSuccess {
		return txid, false
	}
	n := int(binary.BigEndian.Uint16(b[2:4]))
	if n%4 != 0 || n > len(b)-stunHeaderLen {
		return txid, false
	}
	if binary.BigEndian.Uint32(b[4:8]) != stunMagicCookie {
		return txid, false
	}
	end := stunHeaderLen + n
	for off := stunHeaderLen; off < end; {
		if end-off < 4 {
			return txid, false
		}
		alen := int(binary.BigEndian.Uint16(b[off+2 : off+4]))
		padded := (alen + 3) &^ 3
		if padded > end-off-4 {
			return txid, false
		}
		off += 4 + padded
	}
	copy(txid[:], b[8:20])
	return txid, true
}

// burstResult is one burst's outcome. rtts holds the answered requests'
// RTTs in the order they were sent.
type burstResult struct {
	sent int
	rtts []time.Duration
}

// runBurst sends n Binding Requests gap apart on c and collects the
// answers, each within timeout of its request. Answers with an unknown or
// repeated transaction ID, and anything that is not a valid Binding
// Success, are ignored: reading goes on until the deadline. A connected
// socket only receives datagrams from the target's address, so spoofed
// sources never reach it. err is a socket error (e.g. ICMP port
// unreachable, reported as ECONNREFUSED on Linux and macOS; Windows hides
// it, so there it is a timeout) or ctx's error.
func runBurst(ctx context.Context, c *net.UDPConn, n int, gap, timeout time.Duration) (burstResult, error) {
	var res burstResult
	if n <= 0 {
		return res, nil
	}
	txs := make([][12]byte, n)
	sentAt := make([]time.Time, n)
	rtt := make([]time.Duration, n) // 0: no answer (yet)
	var buf [stunBufSize]byte
	start := time.Now()
	answered := 0
	for {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		now := time.Now()
		if res.sent < n && !now.Before(start.Add(time.Duration(res.sent)*gap)) {
			i := res.sent
			if _, err := rand.Read(txs[i][:]); err != nil {
				return res, err
			}
			req := stunRequest(txs[i])
			sentAt[i] = time.Now()
			if _, err := c.Write(req); err != nil {
				return res, err
			}
			res.sent++
			continue
		}
		var dl time.Time
		if res.sent < n {
			dl = start.Add(time.Duration(res.sent) * gap)
		} else {
			dl = sentAt[n-1].Add(timeout)
			if answered == n || !now.Before(dl) {
				break
			}
		}
		if err := c.SetReadDeadline(dl); err != nil {
			return res, err
		}
		m, err := c.Read(buf[:])
		at := time.Now()
		if err != nil {
			if isTimeout(err) {
				continue
			}
			return res, err
		}
		tx, ok := parseSTUN(buf[:m])
		if !ok {
			continue
		}
		for i := 0; i < res.sent; i++ {
			if txs[i] != tx {
				continue
			}
			if d := at.Sub(sentAt[i]); rtt[i] == 0 && d <= timeout {
				rtt[i] = max(d, 1) // never 0: 0 means unanswered
				answered++
			}
			break
		}
	}
	for _, d := range rtt[:res.sent] {
		if d > 0 {
			res.rtts = append(res.rtts, d)
		}
	}
	return res, nil
}

func isTimeout(err error) bool {
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// median of the RTTs (the mean of the middle two for an even count).
func medianRTT(ds []time.Duration) time.Duration {
	s := slices.Clone(ds)
	slices.Sort(s)
	m := len(s) / 2
	if len(s)%2 == 1 {
		return s[m]
	}
	return (s[m-1] + s[m]) / 2
}

// jitterOf is the mean absolute difference of consecutive RTTs (in send
// order): packet-scale jitter, close to what a call's jitter buffer sees.
func jitterOf(ds []time.Duration) time.Duration {
	var sum time.Duration
	for i := 1; i < len(ds); i++ {
		d := ds[i] - ds[i-1]
		if d < 0 {
			d = -d
		}
		sum += d
	}
	return sum / time.Duration(len(ds)-1)
}

// burstSamples turns a burst into its three sibling samples (KindUDP,
// KindUDPJitter, KindUDPLoss of target). Only timeouts count as loss; a
// socket error (refused, unreachable) loses all three with its reason.
func burstSamples(target string, res burstResult, err error) []model.Sample {
	key := func(k model.ProbeKind) model.SeriesKey { return model.SeriesKey{Target: target, Kind: k} }
	out := []model.Sample{{Key: key(model.KindUDP)}, {Key: key(model.KindUDPJitter)}, {Key: key(model.KindUDPLoss)}}
	if err != nil {
		reason := classify(err)
		if reason == model.ReasonTimeout {
			reason = model.ReasonOther // a socket error is not an unanswered request
		}
		for i := range out {
			out[i].Lost, out[i].Reason, out[i].Err = true, reason, udpErr(reason)
		}
		return out
	}
	if len(res.rtts) > 0 {
		out[0].RTT = medianRTT(res.rtts)
	} else {
		out[0].Lost, out[0].Reason, out[0].Err = true, model.ReasonTimeout, udpErr(model.ReasonTimeout)
	}
	if len(res.rtts) >= 2 {
		out[1].RTT = jitterOf(res.rtts)
	} else {
		out[1].Lost, out[1].Reason = true, model.ReasonGap // not measured
	}
	if res.sent > 0 {
		lost := res.sent - len(res.rtts)
		out[2].RTT = time.Duration(lost*100/res.sent) * time.Millisecond
	} else {
		out[2].Lost, out[2].Reason = true, model.ReasonGap
	}
	return out
}

// udpErr is the "last error" shown for a UDP sample: the reason only, never
// the socket error's text (it contains addresses).
func udpErr(r model.Reason) string { return "udp: " + r.String() }

// udpSock is a UDP series' long-lived socket, connected to ip. Only the
// series' goroutine uses it.
type udpSock struct {
	conn *net.UDPConn
	ip   netip.Addr
}

func (u *udpSock) close() {
	if u.conn != nil {
		_ = u.conn.Close()
		u.conn = nil
	}
}

// probeUDP runs one burst of s. ok is false when the slot is not measured
// (the host never resolved yet, or ctx ended mid-burst).
func (r *runner) probeUDP(ctx context.Context, s *series) (smps []model.Sample, ok bool) {
	ip, err := s.host.addr()
	if err != nil {
		if s.host.pending() {
			return nil, false
		}
		return burstSamples(s.key.Target, burstResult{}, &net.DNSError{Err: "lookup failed", Name: s.host.name}), true
	}
	if s.udp.conn != nil && s.udp.ip != ip {
		s.udp.close() // the address changed: a new relay is a new path
	}
	if s.udp.conn == nil {
		c, err := net.DialUDP("udp4", nil, net.UDPAddrFromAddrPort(netip.AddrPortFrom(ip, uint16(s.port))))
		if err != nil {
			return burstSamples(s.key.Target, burstResult{}, err), true
		}
		s.udp = udpSock{conn: c, ip: ip}
	}
	res, err := runBurst(ctx, s.udp.conn, BurstSize, BurstGap, r.udpTimeout)
	if ctx.Err() != nil {
		return nil, false
	}
	if err != nil {
		s.udp.close() // reopened on the next burst
	}
	return burstSamples(s.key.Target, res, err), true
}

// UDPState is the live state of one UDP (STUN) target.
type UDPState struct {
	Target string `json:"target"`
	// Region is the cloud region of the server the target's name resolves to
	// now, from the name's CNAME: "swedencentral" for a Teams relay at
	// ...swedencentral.cloudapp.azure.com. Empty when the CNAME names none.
	Region string `json:"region,omitempty"`
	// Epoch counts the address changes of the target's name since start
	// (GeoDNS moving it to another relay); ChangedAt is the last one (zero
	// if none). Bursts never span a change, so stats compared across bursts
	// should stay within one epoch.
	Epoch     uint64    `json:"epoch"`
	ChangedAt time.Time `json:"changed_at,omitempty"`
}

// UDPStateSource reports the live state of the UDP targets of a running
// profile (the real Runner and Fake implement it), in profile order.
type UDPStateSource interface {
	UDPState() []UDPState
}

// cnameRegion extracts the Azure region from a canonical name like
// "a-tr-teamsc-euno-05.northeurope.cloudapp.azure.com.": the label before
// cloudapp.azure.com. Anything else gives "".
func cnameRegion(cname string) string {
	c := strings.ToLower(strings.TrimSuffix(cname, "."))
	rest, ok := strings.CutSuffix(c, ".cloudapp.azure.com")
	if !ok {
		return ""
	}
	if i := strings.LastIndexByte(rest, '.'); i >= 0 {
		rest = rest[i+1:]
	}
	if rest == "" || len(rest) > 32 {
		return ""
	}
	for _, ch := range rest {
		if (ch < 'a' || ch > 'z') && (ch < '0' || ch > '9') {
			return ""
		}
	}
	return rest
}

// resolveCNAME refreshes hs's region from its CNAME. A failed lookup keeps
// the last region: the A lookup decides whether the host is reachable.
func (r *runner) resolveCNAME(ctx context.Context, hs *hostState) {
	if r.lookups != nil {
		select {
		case r.lookups <- struct{}{}:
			defer func() { <-r.lookups }()
		case <-ctx.Done():
			return
		}
	}
	ctx, cancel := context.WithTimeout(ctx, resolveTimeout)
	defer cancel()
	cname, err := r.o.LookupCNAME(ctx, hs.name)
	if err != nil {
		return
	}
	reg := cnameRegion(cname)
	hs.region.Store(&reg)
}
