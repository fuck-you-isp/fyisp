// Package model holds the types shared by every fyisp package. It has no
// dependencies so any package can import it without cycles.
package model

import (
	"net/netip"
	"time"
)

// ProbeKind identifies how a target is measured.
type ProbeKind uint8

const (
	KindHTTPS ProbeKind = 1
	KindTCP   ProbeKind = 2
	KindICMP  ProbeKind = 3
	// KindTrace is one hop of a traceroute: SeriesKey.Hop is the TTL (1..).
	KindTrace ProbeKind = 4
	// KindUDP is a burst of STUN Binding Requests (see probe.UDPBurst): the
	// burst's median RTT, lost (ReasonTimeout) when no request was answered.
	// A target lists only KindUDP; every burst also writes KindUDPJitter and
	// KindUDPLoss with the same slot. The three are never written apart.
	KindUDP ProbeKind = 5
	// KindUDPJitter is the burst's jitter in the RTT field: the mean |ΔRTT|
	// of consecutive answers, not measured (a gap) with fewer than 2.
	KindUDPJitter ProbeKind = 6
	// KindUDPLoss is the burst's loss in the RTT field, as a percentage in
	// milliseconds: 1 ms = 1% of the burst's requests timed out (0..100).
	KindUDPLoss ProbeKind = 7
	// KindTraceTCP and KindTraceUDP are hops of on-demand traceroutes sent
	// as TCP SYNs (to TraceTCPPort) and UDP datagrams (to TraceUDPPort):
	// like KindTrace, SeriesKey.Hop is the TTL.
	KindTraceTCP ProbeKind = 8
	KindTraceUDP ProbeKind = 9
)

// UDPKinds are the kinds every UDP burst writes, in this order.
var UDPKinds = []ProbeKind{KindUDP, KindUDPJitter, KindUDPLoss}

// IsUDP reports one of the UDP burst kinds.
func (k ProbeKind) IsUDP() bool { return k >= KindUDP && k <= KindUDPLoss }

// IsTrace reports a traceroute hop kind (any TraceMode).
func (k ProbeKind) IsTrace() bool { return k == KindTrace || k == KindTraceTCP || k == KindTraceUDP }

// DefaultKinds is the kind list of a target that sets none: a TCP connect.
// HTTPS and ICMP stay available to targets that list them (the network path
// is measured with ICMP; local profile files may ask for either).
var DefaultKinds = []ProbeKind{KindTCP}

func (k ProbeKind) String() string {
	switch k {
	case KindHTTPS:
		return "https"
	case KindTCP:
		return "tcp"
	case KindICMP:
		return "icmp"
	case KindTrace:
		return "trace"
	case KindUDP:
		return "udp"
	case KindUDPJitter:
		return "udp_jitter"
	case KindUDPLoss:
		return "udp_loss"
	case KindTraceTCP:
		return "trace_tcp"
	case KindTraceUDP:
		return "trace_udp"
	}
	return "unknown"
}

// TraceMode is how a traceroute probes. Always-on traces use TraceICMP;
// on-demand ones (Investigate) may use any mode the OS supports.
type TraceMode uint8

const (
	TraceICMP TraceMode = iota // ICMP echo requests (KindTrace)
	TraceTCP                   // TCP SYNs to TraceTCPPort (KindTraceTCP)
	TraceUDP                   // UDP datagrams to TraceUDPPort (KindTraceUDP)
)

// The fixed destination ports of TCP and UDP traces. The source port
// varies per probe, so per-flow ECMP can still show several addresses at
// one hop, as with mtr.
const (
	TraceTCPPort = 443
	TraceUDPPort = 33434
)

// TraceModes lists every mode, in the UI's order.
var TraceModes = []TraceMode{TraceICMP, TraceTCP, TraceUDP}

func (m TraceMode) String() string {
	switch m {
	case TraceTCP:
		return "tcp"
	case TraceUDP:
		return "udp"
	}
	return "icmp"
}

// Kind is the kind of m's hop series.
func (m TraceMode) Kind() ProbeKind {
	switch m {
	case TraceTCP:
		return KindTraceTCP
	case TraceUDP:
		return KindTraceUDP
	}
	return KindTrace
}

// Port is m's destination port (0 for ICMP).
func (m TraceMode) Port() int {
	switch m {
	case TraceTCP:
		return TraceTCPPort
	case TraceUDP:
		return TraceUDPPort
	}
	return 0
}

// ParseTraceMode parses "icmp", "tcp" or "udp".
func ParseTraceMode(s string) (TraceMode, bool) {
	for _, m := range TraceModes {
		if s == m.String() {
			return m, true
		}
	}
	return 0, false
}

// HostTargetPrefix starts the target name of an on-demand trace of a host
// outside the profile: "host:example.com", "host:192.0.2.1". Its hop series
// and in-memory route use that name. Profile target names never start with
// it (profile.checkName refuses them).
const HostTargetPrefix = "host:"

// HostTarget is the target name of an on-demand trace of host (normalised
// by the caller: lowercase, no trailing dot).
func HostTarget(host string) string { return HostTargetPrefix + host }

// HostOf returns the host of a HostTarget name.
func HostOf(name string) (string, bool) {
	if len(name) > len(HostTargetPrefix) && name[:len(HostTargetPrefix)] == HostTargetPrefix {
		return name[len(HostTargetPrefix):], true
	}
	return "", false
}

// Reason explains a missing sample. It fits in 4 bits of the storage format.
// ReasonGap means "not measured" (fyisp was stopped, the machine slept, the
// clock jumped) and must never be reported as loss.
type Reason uint8

const (
	ReasonGap         Reason = 0
	ReasonTimeout     Reason = 1
	ReasonRefused     Reason = 2 // TCP RST on connect
	ReasonReset       Reason = 3 // reset/EOF after connect
	ReasonUnreachable Reason = 4 // ICMP unreachable, EHOSTUNREACH
	ReasonDNS         Reason = 5
	ReasonTLS         Reason = 6
	ReasonHTTP        Reason = 7 // protocol error before the first byte
	ReasonNoNetwork   Reason = 8 // ENETUNREACH, no default route
	ReasonOther       Reason = 15
)

// MaxReason is the largest value the storage format can hold.
const MaxReason Reason = 15

func (r Reason) String() string {
	switch r {
	case ReasonGap:
		return "not measured"
	case ReasonTimeout:
		return "timeout"
	case ReasonRefused:
		return "refused"
	case ReasonReset:
		return "reset"
	case ReasonUnreachable:
		return "unreachable"
	case ReasonDNS:
		return "dns"
	case ReasonTLS:
		return "tls"
	case ReasonHTTP:
		return "http"
	case ReasonNoNetwork:
		return "no network"
	}
	return "other"
}

// Group is one dashboard panel.
type Group struct {
	ID    string `yaml:"id" json:"id"`
	Title string `yaml:"title" json:"title"`
	Order int    `yaml:"order" json:"order"`
}

// Target is one destination. Name is the identity: it survives host changes.
type Target struct {
	Name     string        `yaml:"name" json:"name"`
	Host     string        `yaml:"host" json:"-"`
	Group    string        `yaml:"group" json:"group"`
	Port     int           `yaml:"port,omitempty" json:"-"` // TCP/HTTPS port, default 443; UDP: required
	Path     string        `yaml:"path,omitempty" json:"-"` // HTTPS path, default "/"
	Kinds    []ProbeKind   `yaml:"-" json:"kinds"`          // default: DefaultKinds (TCP)
	Interval time.Duration `yaml:"-" json:"interval"`       // TCP/HTTPS/UDP interval, default 15s; ICMP runs at Interval/3
	// Trace runs an always-on traceroute to this target (KindTrace series).
	Trace bool `yaml:"-" json:"trace,omitempty"`
	// Layer is LayerGateway, LayerEdge or LayerAnycast for path targets.
	Layer string `yaml:"-" json:"layer,omitempty"`
	// HostOverrides uses a different host for some kinds (e.g. Google Meet:
	// HTTPS/TCP to meet.google.com, ICMP to lens.l.google.com).
	HostOverrides map[ProbeKind]string `yaml:"-" json:"-"`
	// Catalog metadata for the dashboard's Overview, set only for targets
	// from the catalog (profile files cannot set it): the provider's ID,
	// display name and kind, and the endpoint's city, ISO country code and
	// geo (na sa eu me af as oc, or "global" for anycast targets).
	Provider      string `yaml:"-" json:"provider,omitempty"`
	ProviderTitle string `yaml:"-" json:"provider_title,omitempty"`
	ProviderKind  string `yaml:"-" json:"provider_kind,omitempty"`
	City          string `yaml:"-" json:"city,omitempty"`
	Country       string `yaml:"-" json:"country,omitempty"`
	Geo           string `yaml:"-" json:"geo,omitempty"`
	// Tags are the catalog's (or a profile file's) labels, e.g. "udp-path"
	// or "app:teams": the Calls view maps targets to apps by them.
	Tags []string `yaml:"-" json:"tags,omitempty"`
}

// IsUDP reports a target probed with STUN bursts (its kinds are [udp]).
func (t Target) IsUDP() bool {
	for _, k := range t.Kinds {
		if k == KindUDP {
			return true
		}
	}
	return false
}

// ProbeKinds returns the kinds t is probed with: Kinds, or DefaultKinds
// when it sets none.
func (t Target) ProbeKinds() []ProbeKind {
	if len(t.Kinds) > 0 {
		return t.Kinds
	}
	return DefaultKinds
}

// HostFor returns the host to probe for a kind.
func (t Target) HostFor(k ProbeKind) string {
	if h, ok := t.HostOverrides[k]; ok && h != "" {
		return h
	}
	return t.Host
}

// Profile is a named set of groups and targets.
type Profile struct {
	Name    string   `yaml:"name" json:"name"`
	Version string   `yaml:"version" json:"version"`
	Groups  []Group  `yaml:"groups" json:"groups"`
	Targets []Target `yaml:"targets" json:"targets"`
}

// SeriesKey identifies one stored time series.
type SeriesKey struct {
	Target string    `json:"target"`
	Kind   ProbeKind `json:"kind"`
	// Hop is the TTL for trace series (Kind.IsTrace; 1 = first hop), 0 otherwise.
	Hop uint8 `json:"hop,omitempty"`
}

// Sample is one probe result. Slot is the wall-clock UTC start of the
// scheduled slot (assigned by the scheduler, not the completion time).
type Sample struct {
	Key    SeriesKey
	Slot   time.Time
	RTT    time.Duration // valid when !Lost; HTTPS: request written -> first response byte
	Lost   bool
	Reason Reason // valid when Lost
	Reused bool   // HTTPS only: connection was reused
	Err    string // not persisted; for tests and debugging
	// At is when the probe started (not persisted; zero when unknown): the
	// speed test's load tap files the gateway's and the edge's samples by it.
	At time.Time
}

// Sink receives samples. Observe must not block.
type Sink interface{ Observe(Sample) }

// SinkFunc adapts a function to Sink.
type SinkFunc func(Sample)

func (f SinkFunc) Observe(s Sample) { f(s) }

// Fanout sends every sample to all sinks.
type Fanout []Sink

func (f Fanout) Observe(s Sample) {
	for _, k := range f {
		k.Observe(s)
	}
}

// Layer tags a target that measures one layer of the path, so the verdict
// engine can tell whose fault a problem is. Empty for ordinary targets.
const (
	LayerGateway = "gateway"  // the default gateway (home router)
	LayerEdge    = "isp-edge" // the first public hop (the ISP's side)
	LayerAnycast = "anycast"  // big anycast resolvers (1.1.1.1, 8.8.8.8)
)

// Special hosts resolved at runtime from the local network path.
const (
	HostGateway = "@gateway"
	HostEdge    = "@isp-edge"
)

// VerdictKind is the engine's conclusion about the current problem.
type VerdictKind string

const (
	VerdictOK        VerdictKind = "ok"
	VerdictWarmingUp VerdictKind = "warming_up" // not enough data yet
	VerdictLAN       VerdictKind = "lan"        // gateway unhealthy: Wi-Fi/LAN/router
	VerdictISP       VerdictKind = "isp"        // gateway fine, ISP edge unhealthy
	VerdictUpstream  VerdictKind = "upstream"   // edge fine, most internet targets unhealthy
	VerdictDNS       VerdictKind = "dns"        // path fine, name resolution failing widely
	VerdictService   VerdictKind = "service"    // path fine, only some targets/groups unhealthy
	VerdictNoNetwork VerdictKind = "no_network" // no default route / all layers down
	// The call path (UDP) kinds rank below every other problem: they are
	// only given while TCP and the network path look fine.
	VerdictUDPBlocked  VerdictKind = "udp_blocked"  // UDP to ≥ 2 STUN providers failing, UDP 53 too
	VerdictSTUNBlocked VerdictKind = "stun_blocked" // STUN ports failing on ≥ 2 providers, UDP 53 answers
	VerdictUDPDegraded VerdictKind = "udp_degraded" // ≥ 2 STUN providers lossy or jittery for minutes
)

// Verdict is the current assessment. Summary is one plain-English sentence
// and must never contain IP addresses or hostnames of the user's network
// (the dashboard quotes it as is).
type Verdict struct {
	Kind    VerdictKind `json:"kind"`
	Since   time.Time   `json:"since"`
	Summary string      `json:"summary"`
	// Targets affected (names, as in the profile). Empty for path-level kinds.
	Targets []string `json:"targets,omitempty"`
	// Evidence are the numbers behind the verdict, e.g. "gateway_loss": 0.2.
	Evidence map[string]float64 `json:"evidence,omitempty"`
	// Quiet marks a long-standing udp_blocked or stun_blocked verdict
	// (blocked for hours: a hotel or office network that never allows
	// UDP) that the UI shows as information rather than as a problem.
	Quiet bool `json:"quiet,omitempty"`
}

// Incident is one period with a non-OK verdict. End is zero while ongoing.
type Incident struct {
	ID       int64       `json:"id"`
	Start    time.Time   `json:"start"`
	End      time.Time   `json:"end,omitempty"`
	Kind     VerdictKind `json:"kind"`
	Summary  string      `json:"summary"`
	Targets  []string    `json:"targets,omitempty"`
	PeakLoss float64     `json:"peak_loss"` // worst 1-minute loss fraction seen
}

// HopInfo describes the router seen at one hop address. Stored once per IP,
// not per sample. RDNS and Owner may reveal locations.
type HopInfo struct {
	IP        netip.Addr `json:"ip"`
	RDNS      string     `json:"rdns,omitempty"`
	ASN       uint32     `json:"asn,omitempty"`
	Owner     string     `json:"owner,omitempty"` // AS name, e.g. "COMCAST-7922"
	FirstSeen time.Time  `json:"first_seen"`
	LastSeen  time.Time  `json:"last_seen"`
}

// Route is the hop sequence of one traced target. Hops[i] is TTL i+1; an
// invalid address means that hop did not answer.
type Route struct {
	Target string       `json:"target"`
	Hops   []netip.Addr `json:"hops"`
	Since  time.Time    `json:"since"`
}

// RouteChange records that a target's path changed.
type RouteChange struct {
	ID     int64        `json:"id"`
	Target string       `json:"target"`
	At     time.Time    `json:"at"`
	From   []netip.Addr `json:"from"`
	To     []netip.Addr `json:"to"`
	// FirstDiff is the first TTL (1-based) that differs.
	FirstDiff int `json:"first_diff"`
}

// Baseline is a series' "normal" from its own recent history (default the
// past 7 days, excluding the current hour). When HourOfDay is true the values
// are for the same UTC hour of day as the time asked about.
type Baseline struct {
	Key       SeriesKey `json:"key"`
	MedianMs  float64   `json:"median_ms"`
	P95Ms     float64   `json:"p95_ms"`
	Loss      float64   `json:"loss"` // fraction
	Samples   int64     `json:"samples"`
	HourOfDay bool      `json:"hour_of_day"`
	From      time.Time `json:"from"`
	To        time.Time `json:"to"`
}

// Speed test outcomes (SpeedTest.Outcome).
const (
	SpeedOK          = "ok"
	SpeedBusy        = "busy"        // every attempt found the server running another test: never a slow result
	SpeedUnreachable = "unreachable" // the server did not accept a connection
	SpeedFailed      = "failed"      // anything else (timeout, protocol error)
	// SpeedCancelled: stopped by the owner (or shutdown) mid-test. Stored
	// without speeds so its window stays explained (charts, Home);
	// never counted as a result, busy or failed.
	SpeedCancelled = "cancelled"
	// SpeedRemoved: a test of a server since removed from the catalog,
	// kept as a bare window (start, end, trigger, bytes) so the charts and
	// Home still explain it. Store.SpeedTests leaves it out; only
	// SpeedWindows and the data used see it.
	SpeedRemoved = "removed"
)

// SpeedTest is one iperf3 speed test (download then upload). Start..End is
// its window: probes in it measure a loaded link; the Home tiles attribute a
// problem in it to the test, and the charts shade it. Bits per second, RTTs
// in ms; nil when not measured.
type SpeedTest struct {
	ID      int64     `json:"id"`
	Start   time.Time `json:"start"`
	End     time.Time `json:"end"`
	Trigger string    `json:"trigger"` // "schedule", "manual" (Test now), "sweep"
	Outcome string    `json:"outcome"`
	// Region is the catalog region the test was for ("" outside mode
	// regions); Server the server actually used: a catalog ID, or host:port
	// of the user's own server. Fallback: not the region's first choice.
	Region   string   `json:"region"`
	Server   string   `json:"server"`
	Fallback bool     `json:"fallback"`
	Port     int      `json:"port"`
	Attempts int      `json:"attempts"`
	DownBps  *float64 `json:"down_bps"`
	UpBps    *float64 `json:"up_bps"`
	// Retransmits of the sending side, nil when unknown.
	DownRetrans *int64 `json:"down_retrans"`
	UpRetrans   *int64 `json:"up_retrans"`
	// Latency under load at the ISP edge (the WAN queue) and the gateway
	// (the home network): nil when neither was probed.
	Latency *SpeedLatency `json:"latency"`
	// Conn is the connection type of the interface the test ran on:
	// ConnWired, ConnWiFi or ConnUnknown.
	Conn string `json:"conn"`
	// Handoff is the first network outside the user's ISP on the path to
	// the server; nil when unknown.
	Handoff *Handoff `json:"handoff"`
	// FallbackLevel is "none", "same_metro" or "nearby_city".
	FallbackLevel string `json:"fallback_level"`
	// CappedByServer: a direction measured within 10% of the server's
	// published link capacity (the server, not the line, set the limit).
	CappedByServer bool    `json:"capped_by_server"`
	DownSecs       float64 `json:"down_s"`
	UpSecs         float64 `json:"up_s"`
	Bytes          int64   `json:"bytes"` // data used, both directions
	// ScoreDown/ScoreUp: the result relative to the server's baseline (the
	// p90 of its first-choice scheduled tests over 14 days); nil while the
	// server has fewer than 20 such tests.
	ScoreDown *float64 `json:"score_down"`
	ScoreUp   *float64 `json:"score_up"`
	// Extra holds further measurements as JSON (the ramp-ups): see
	// speedtest.Extra.
	Extra string `json:"extra,omitempty"`
}

// Connection types (SpeedTest.Conn).
const (
	ConnWired   = "wired"
	ConnWiFi    = "wifi"
	ConnUnknown = "unknown"
)

// SpeedLatency is the latency under load of a speed test, per layer.
type SpeedLatency struct {
	Edge    LoadPhases `json:"edge"`
	Gateway LoadPhases `json:"gateway"`
}

// LoadPhases is one layer's RTT in the 2 minutes before the test (Idle) and
// during its download and upload; nil with fewer than 3 probes.
type LoadPhases struct {
	Idle *LoadStats `json:"idle"`
	Down *LoadStats `json:"down"`
	Up   *LoadStats `json:"up"`
}

// LoadStats summarises the ICMP probes of one layer over one phase. A lost
// probe counts as loss and, in the percentiles, as an RTT of the probe's
// timeout: it is never dropped (under heavy bufferbloat most probes time
// out, and the survivors alone would understate it).
type LoadStats struct {
	N     int     `json:"n"`    // probes
	Lost  int     `json:"lost"` // of N
	Loss  float64 `json:"loss"` // Lost/N
	P50Ms float64 `json:"p50_ms"`
	P95Ms float64 `json:"p95_ms"`
}

// Handoff is where the path to a speed test server leaves the user's ISP:
// the first traceroute hop whose ASN is not the ISP's.
type Handoff struct {
	ASN    uint32    `json:"asn"`
	Name   string    `json:"name"`
	ISPASN uint32    `json:"isp_asn"`
	At     time.Time `json:"at"` // when the traceroute ran
	// Via is the internet exchange the path crossed to reach ASN (local
	// only; not stored: the handoff is re-traced daily).
	Via string `json:"via,omitempty"`
}

// OnDemand reports a test someone asked for (Test now, a sweep) rather than
// a schedule slot: patterns leave these out (they cluster at the hours
// people look, and sweeps hit every server at once).
func (t SpeedTest) OnDemand() bool {
	switch t.Trigger {
	case "manual", "sweep":
		return true
	}
	return false
}

// Origin is who asked for the test: "scheduled", "owner", or "storage" (a
// storage round's transfers, SpeedWindows only).
func (t SpeedTest) Origin() string {
	switch {
	case t.Trigger == TriggerStorage:
		return "storage"
	case t.OnDemand():
		return "owner"
	}
	return "scheduled"
}
