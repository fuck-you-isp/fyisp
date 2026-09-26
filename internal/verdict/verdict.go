// Package verdict turns recent samples into a plain-English judgement of
// whose fault a problem is (LAN, ISP, upstream, DNS, one service), and keeps
// a log of incidents.
package verdict

import (
	"context"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

// IncidentStore persists incidents (implemented by internal/store).
type IncidentStore interface {
	// SaveIncident inserts (ID == 0, sets ID) or updates an incident.
	SaveIncident(ctx context.Context, in *model.Incident) error
	// Incidents returns incidents overlapping [from, to], newest first.
	Incidents(ctx context.Context, from, to time.Time) ([]model.Incident, error)
}

// Engine consumes samples (model.Sink) and evaluates every few seconds while
// Run runs. Safe for concurrent use.
//
//	func New(profile func() *model.Profile, st IncidentStore, o Options) Engine
type Engine interface {
	model.Sink
	Run(ctx context.Context) error
	Current() model.Verdict
	Incidents(ctx context.Context, from, to time.Time) ([]model.Incident, error)
}
