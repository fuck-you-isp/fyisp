// Package model holds the types shared by every fyisp package. It has no
// dependencies so any package can import it without cycles.
package model

import "time"

// ProbeKind identifies how a target is measured.
type ProbeKind uint8

const (
	KindHTTPS ProbeKind = 1
	KindTCP   ProbeKind = 2
	KindICMP  ProbeKind = 3
)

func (k ProbeKind) String() string {
	switch k {
	case KindHTTPS:
		return "https"
	case KindTCP:
		return "tcp"
	case KindICMP:
		return "icmp"
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
	Kinds    []ProbeKind   `yaml:"-" json:"kinds"`          // default: all three
	Interval time.Duration `yaml:"-" json:"interval"`       // HTTPS/TCP interval, default 15s; ICMP runs at Interval/3
	// Layer is LayerGateway, LayerEdge or LayerAnycast for path targets.
	Layer string `yaml:"-" json:"layer,omitempty"`
	// HostOverrides uses a different host for some kinds (e.g. Google Meet:
	// HTTPS/TCP to meet.google.com, ICMP to lens.l.google.com).
	HostOverrides map[ProbeKind]string `yaml:"-" json:"-"`
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
