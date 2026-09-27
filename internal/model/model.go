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
)

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
	}
	return "unknown"
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
	Port     int           `yaml:"port,omitempty" json:"-"` // TCP/HTTPS port, default 443
	Path     string        `yaml:"path,omitempty" json:"-"` // HTTPS path, default "/"
	Kinds    []ProbeKind   `yaml:"-" json:"kinds"`          // default: DefaultKinds (TCP)
	Interval time.Duration `yaml:"-" json:"interval"`       // TCP/HTTPS interval, default 15s; ICMP runs at Interval/3
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
	// Hop is the TTL for KindTrace series (1 = first hop), 0 otherwise.
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
	Err    string // not persisted; shown as "last error" in the UI
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
	LayerAnycast = "anycast"  // big anycast resolvers (1.1.1.1, 8.8.8.8, 9.9.9.9)
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
)

// Verdict is the current assessment. Summary is one plain-English sentence
// and must never contain IP addresses or hostnames of the user's network
// (it is served on the public link).
type Verdict struct {
	Kind    VerdictKind `json:"kind"`
	Since   time.Time   `json:"since"`
	Summary string      `json:"summary"`
	// Targets affected (names, as in the profile). Empty for path-level kinds.
	Targets []string `json:"targets,omitempty"`
	// Evidence are the numbers behind the verdict, e.g. "gateway_loss": 0.2.
	Evidence map[string]float64 `json:"evidence,omitempty"`
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
// not per sample. RDNS and Owner may reveal locations; the public view
// redacts them for the first public hops.
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

// Annotation is a user note on the timeline (e.g. "call dropped", "ISP ticket
// opened"). End is zero for a point in time. Public notes are shown on the
// public link; all notes can be included in reports.
type Annotation struct {
	ID      int64     `json:"id"`
	At      time.Time `json:"at"`
	End     time.Time `json:"end,omitempty"`
	Text    string    `json:"text"`
	Public  bool      `json:"public"`
	Created time.Time `json:"created"`
	Updated time.Time `json:"updated"`
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

// ReportMeta describes a stored evidence report snapshot.
type ReportMeta struct {
	ID      string    `json:"id"` // random, URL-safe
	Title   string    `json:"title"`
	From    time.Time `json:"from"`
	To      time.Time `json:"to"`
	Created time.Time `json:"created"`
	Public  bool      `json:"public"` // served on the public link (redacted snapshot)
	Bytes   int       `json:"bytes"`
}
