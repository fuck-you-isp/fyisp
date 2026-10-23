package model

import (
	"net/netip"
	"time"
)

// StorageAttempt is one request of a storage test round (internal/s3test):
// its timings, what it moved, and how it ended. Every field that a
// request never reached is nil. It never holds the local socket address,
// a host name, a bucket, a key, a signature or an error's text.
type StorageAttempt struct {
	Target string    // the target's stable ID
	Phase  string    // StoragePhase*
	Op     string    // get, head, put, delete, list
	N      int       // 0-based within the phase and target
	At     time.Time // the attempt's start
	Class  string    // s3test.Class: "ok" or how it failed
	// HTTPStatus is 0 when no response came; S3Code is the error body's
	// <Code>, checked against ^[A-Za-z]{1,64}$.
	HTTPStatus int
	S3Code     string
	// RequestID and HostID are the provider's request identifiers, kept for
	// failures only, from RequestIDHeader (checked against
	// ^[A-Za-z0-9+/=_-]{1,200}$, so no address can ride in them).
	RequestID, HostID, RequestIDHeader string

	DNSms, Connectms, TLSms, TTFBms, Totalms *float64
	Bytes                                    int64    // payload bytes moved
	GoodputBps                               *float64 // payload bits per second, transfers only
	Stallms                                  *float64 // the longest gap with no body bytes, transfers only

	// EdgeIP is the provider's address this attempt used (invalid when it
	// is this machine's own, or unknown); EdgePrefix its /24.
	EdgeIP     netip.Addr
	EdgePrefix string
	TLSVersion string // "1.2", "1.3"; "" for http:// or no handshake
	Resumed    bool   // TLS session resumed (never, by design)

	TCPRTTms      *float64 // TCP_INFO (Linux) at the end of the attempt
	Retrans, MSS  *int
	SkewS         *int // the server's Date minus local time, seconds
	HostAddrCount int  // A records the lookup returned
}

// Storage test phases.
const (
	StoragePhaseSeed = "seed" // the download objects, put when missing
	StoragePhaseCold = "cold" // a fresh lookup, connection and handshake per request
	StoragePhaseWarm = "warm" // requests on one kept connection
	StoragePhaseDown = "down" // a ranged download
	StoragePhaseUp   = "up"   // an upload, then its delete
)
