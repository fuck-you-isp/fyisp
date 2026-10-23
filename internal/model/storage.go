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

// Owners of hops kept without their address (StorageHop).
const (
	HopYourNetwork  = "your network"
	HopYourProvider = "your provider"
)

// StorageCampaign is a storage test campaign: rounds every Interval, on
// demand (from Start, Count rounds or until End) or recurring (inside a
// weekly window in TZ, until Until).
type StorageCampaign struct {
	ID       int64
	Created  time.Time
	Updated  time.Time
	Shape    string // StorageOnDemand, StorageRecurring
	State    string // StorageScheduled ... StorageStopped
	Stopped  string // why it ended: StopOwner, StopFinished, StopUntil, StopRemoved
	Targets  []StorageCampaignTarget
	Cold     int
	Warm     int
	Down, Up int64 // bytes per transfer
	Interval time.Duration
	// On demand: the first round, and Count rounds or until End.
	Start time.Time
	Count int
	// End is when it ends (planned, by duration) or ended.
	End time.Time
	// Recurring: weekdays (bit 0 Monday ... bit 6 Sunday), the window's
	// start and end in minutes after midnight (an end at or before the
	// start crosses midnight), its zone, and when the schedule ends (zero:
	// never).
	Days       uint8
	FromMin    int
	ToMin      int
	TZ         string
	Until      time.Time
	KeysSaved  bool
	RoundsDone int
	BytesDown  int64
	BytesUp    int64
}

// StorageCampaignTarget is a target as the campaign started with it.
type StorageCampaignTarget struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Mode string `json:"mode"` // "conn" (connection checks only) or "bucket"
}

// Live reports a campaign that hasn't ended.
func (c StorageCampaign) Live() bool {
	return c.State == StorageScheduled || c.State == StorageRunning || c.State == StoragePaused
}

// Campaign shapes, states and why one stopped.
const (
	StorageOnDemand  = "on_demand"
	StorageRecurring = "recurring"

	StorageScheduled = "scheduled"
	StorageRunning   = "running"
	StoragePaused    = "paused"
	StorageDone      = "done"
	StorageStopped   = "stopped"

	StopOwner    = "owner"    // the owner stopped it
	StopFinished = "finished" // it ran its rounds or its time
	StopUntil    = "until"    // a recurring schedule's end date passed
	StopRemoved  = "removed"  // the owner removed the schedule
)

// StorageRound is one due round of a campaign: it ran (Seq set, the
// attempts stored with it), was skipped (Outcome RoundSkipped, SkipReason),
// or is a gap while fyisp wasn't running (Seq nil, Due..End).
type StorageRound struct {
	ID         int64
	Campaign   int64
	Seq        *int
	Due        time.Time
	Start, End time.Time // zero when it didn't run (End: a gap's end)
	QueueWait  time.Duration
	Outcome    string            // Round*
	SkipReason string            // Skip*, for RoundSkipped
	Skips      map[string]string // target -> why the round left it out
	GwLoss     *float64
	GwRTTms    *float64
	EdgeLoss   *float64
	EdgeRTTms  *float64
	BytesDown  int64
	BytesUp    int64
}

// Round outcomes, and why a round or a target in it was skipped.
const (
	RoundOK        = "ok"
	RoundSlow      = "slow"
	RoundFailed    = "failed"
	RoundConfig    = "config"
	RoundCancelled = "cancelled"
	RoundSkipped   = "skipped"
	RoundGap       = "gap"

	SkipBusy          = "busy"           // a round of the campaign was still waiting
	SkipStale         = "stale"          // it waited too long in the queue
	SkipPaused        = "paused"         // the campaign was paused
	SkipNoKeys        = "no_keys"        // keys are missing (a target, or every one)
	SkipOtherCampaign = "other_campaign" // another campaign was running
)

// TriggerStorage is the trigger of a storage round's transfer window in
// SpeedWindows (charts and Home explain probes during it).
const TriggerStorage = "storage"

// StorageTrace is a traceroute to a target's edge, taken at a campaign's
// first round with it (trigger "start") or after a failure ("failure"):
// the route's hops as seen then (ad-hoc traces are kept in memory only,
// so this is the record).
type StorageTrace struct {
	ID           int64
	Campaign     int64
	Target       string
	At           time.Time
	Trigger      string
	EdgePrefix   string
	Hops         []StorageHop
	HandoffASN   uint32 // the first network past this connection's provider (0: unknown)
	RouteChanged bool   // the route differs from the campaign's last one to this /24
}

// StorageHop is one hop of a StorageTrace. Hops on the user's network or
// in their provider's network have no IP, only Owner HopYourNetwork or
// HopYourProvider; a hop that didn't answer has neither.
type StorageHop struct {
	IP    string `json:"ip"`
	ASN   uint32 `json:"asn,omitempty"`
	Owner string `json:"owner,omitempty"`
}
