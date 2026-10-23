// Package tcpinfo reads a TCP connection's kernel counters (TCP_INFO):
// RTT, retransmits, MSS and bytes acked or received. Linux only; elsewhere
// Read reports nothing.
package tcpinfo

import "time"

// Info is a TCP connection's counters at the time of Read.
type Info struct {
	RTT, MinRTT   time.Duration // smoothed and smallest seen RTT
	Retrans       int64         // segments retransmitted, in all
	MSS           int           // the send MSS
	BytesAcked    int64         // sent and acked by the peer
	BytesReceived int64         // received, read or still in the buffer
}
