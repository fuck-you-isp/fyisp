package main

import (
	"log/slog"

	"github.com/fuck-you-isp/fyisp/internal/s3test"
	"github.com/fuck-you-isp/fyisp/internal/speedtest"
	"github.com/fuck-you-isp/fyisp/internal/storage"
)

// newStorage opens the storage-test targets in dir. A targets file fyisp
// can't use turns the storage tests off (the API answers 503) and is left
// as it is. A check during a queued test says its timings may be high.
func newStorage(dir string, q *speedtest.Queue, log *slog.Logger) *storage.Service {
	e := s3test.New(s3test.Options{UserAgent: "fyisp/" + version, IsSelf: storage.IsSelf})
	s := storage.Open(storage.Options{Dir: dir, Engine: e, Busy: func() bool { return q.State().Running != nil }})
	if err := s.Err(); err != nil {
		log.Warn("storage tests are off", "err", err)
	}
	return s
}
