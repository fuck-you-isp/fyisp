package main

import (
	"log/slog"
	"net/netip"

	"github.com/fuck-you-isp/fyisp/internal/asn"
	"github.com/fuck-you-isp/fyisp/internal/netinfo"
	"github.com/fuck-you-isp/fyisp/internal/s3test"
	"github.com/fuck-you-isp/fyisp/internal/speedtest"
	"github.com/fuck-you-isp/fyisp/internal/storage"
)

// newStorage opens the storage-test targets in dir and the campaigns that
// run rounds against them in the speed-test queue q, stored in st. A
// targets file fyisp can't use turns the storage tests off (the API
// answers 503, no campaign runs) and is left as it is. Transfers are
// marked in wins, as speed tests are; a check during a queued test says
// its timings may be high.
func newStorage(dir string, q *speedtest.Queue, st storage.Store, wins *speedtest.Windows, path func() netinfo.Path, log *slog.Logger) (*storage.Service, *storage.Campaigns) {
	e := s3test.New(s3test.Options{UserAgent: "fyisp/" + version, Masker: wins, IsSelf: notEdge(path)})
	s := storage.Open(storage.Options{Dir: dir, Engine: e, Busy: func() bool { return q.State().Running != nil }})
	if err := s.Err(); err != nil {
		log.Warn("storage tests are off", "err", err)
		return s, nil
	}
	return s, storage.NewCampaigns(storage.CampaignOptions{Store: st, Queue: q, Engine: e, Targets: s, Log: log})
}

// notEdge reports an address never kept as a provider's edge: this
// machine's, any in this connection's own provider's network (it could be
// the user's public address: a server at home reached by its DDNS name),
// and any of no known network. With the provider unknown, every address
// is. Best effort: a provider that hands out addresses from another of its
// networks isn't recognised.
func notEdge(path func() netinfo.Path) func(netip.Addr) bool {
	return func(a netip.Addr) bool {
		if storage.IsSelf(a) {
			return true
		}
		p := path()
		if !p.Edge.IsValid() {
			return true
		}
		isp, own := asn.Lookup(p.Edge).ASN, asn.Lookup(a).ASN
		return isp == 0 || own == 0 || own == isp
	}
}
