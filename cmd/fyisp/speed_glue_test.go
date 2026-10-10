package main

import (
	"strings"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/metrics"
	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/speedtest"
)

func TestSpeedFlags(t *testing.T) {
	for _, c := range []struct {
		args  []string
		on    bool
		every time.Duration
		err   string
	}{
		{nil, false, 0, ""},
		{[]string{"--speedtest"}, true, 0, ""},
		{[]string{"--speedtest=every=30m"}, true, 30 * time.Minute, ""},
		{[]string{"--speedtest=off"}, false, 0, ""},
		{[]string{"--speedtest=every=5m"}, false, 0, "at least 15m0s"},
		{[]string{"--speedtest=every=5m", "--speedtest-server", "10.0.0.2:5201"}, true, 5 * time.Minute, ""},
		{[]string{"--speedtest-server", "nope"}, false, 0, "want host:port"},
		{[]string{"--speedtest=sometimes"}, false, 0, "want on, off or every="},
	} {
		cfg, err := parseFlags(c.args)
		switch {
		case c.err != "":
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("%v: %v, want %q", c.args, err, c.err)
			}
		case err != nil:
			t.Errorf("%v: %v", c.args, err)
		case cfg.speed.on != c.on || cfg.speed.every != c.every:
			t.Errorf("%v: %+v", c.args, cfg.speed)
		}
	}
	// The scheduler is always there (Details toggle, Test now), off unless asked.
	cfg, _ := parseFlags([]string{"--speedtest=every=1h"})
	s, tap, w, err := newSpeed(cfg, nil, nil, newTestCollector(), nil)
	if err != nil || tap == nil || w == nil {
		t.Fatal(err)
	}
	if st := s.State(); len(st.Servers) != 7 || s.Interval() != time.Hour || st.Custom != "" {
		t.Errorf("%+v", st)
	}
	cfg, _ = parseFlags([]string{"--speedtest-server", "iperf.example.net:5201"})
	s, _, _, _ = newSpeed(cfg, nil, nil, newTestCollector(), nil)
	if st := s.State(); st.Custom != "iperf.example.net:5201" || len(st.Servers) != 1 || s.Interval() != speedtest.MinInterval {
		t.Errorf("custom: %+v", st)
	}
}

func newTestCollector() *metrics.Collector {
	return metrics.New(func() *model.Profile { return nil })
}
