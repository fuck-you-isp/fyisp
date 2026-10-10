package main

import (
	"slices"
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
		mode  speedtest.Mode
		every time.Duration
		extra []string
		err   string
	}{
		{nil, "", 0, nil, ""},
		{[]string{"--speedtest"}, speedtest.DefaultMode, 0, nil, ""},
		{[]string{"--speedtest=on"}, speedtest.DefaultMode, 0, nil, ""},
		{[]string{"--speedtest=nearby"}, speedtest.ModeNearby, 0, nil, ""},
		{[]string{"--speedtest=nearby,far=us-west"}, speedtest.ModeNearby, 0, []string{"us-west"}, ""},
		{[]string{"--speedtest=regions,add=canada,us-northwest,every=2h"}, speedtest.ModeRegions, 2 * time.Hour, []string{"canada", "us-northwest"}, ""},
		{[]string{"--speedtest=regions,every=30m"}, speedtest.ModeRegions, 30 * time.Minute, nil, ""},
		{[]string{"--speedtest=off"}, speedtest.ModeOff, 0, nil, ""},
		{[]string{"--speedtest=regions,add=mars"}, "", 0, nil, `no region "mars"`},
		{[]string{"--speedtest=every=5m"}, "", 0, nil, "at least 15m0s"},
		{[]string{"--speedtest=every=5m", "--speedtest-server", "10.0.0.2:5201"}, speedtest.ModeCustom, 5 * time.Minute, nil, ""},
		{[]string{"--speedtest=every=5m", "--speedtest-server", "10.0.0.2:5201,Leaseweb-Tokyo"}, "", 0, nil, "at least 15m0s"},
		{[]string{"--speedtest", "--speedtest-server", "Leaseweb-Tokyo"}, speedtest.ModeCustom, 0, nil, ""},
		{[]string{"--speedtest=custom"}, "", 0, nil, "needs --speedtest-server"},
		{[]string{"--speedtest-server", "nope"}, "", 0, nil, "want host:port or a catalog server id"},
		{[]string{"--speedtest=sometimes"}, "", 0, nil, "want regions, nearby"},
		{[]string{"--speedtest=nearest"}, "", 0, nil, "want regions, nearby"},
	} {
		cfg, err := parseFlags(c.args)
		switch {
		case c.err != "":
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("%v: %v, want %q", c.args, err, c.err)
			}
		case err != nil:
			t.Errorf("%v: %v", c.args, err)
		case cfg.speed.mode != c.mode || cfg.speed.every != c.every || !slices.Equal(cfg.speed.extra, c.extra):
			t.Errorf("%v: %+v", c.args, cfg.speed)
		}
	}
	// The scheduler is always there (Details choice, Test now, sweeps).
	cfg, _ := parseFlags([]string{"--speedtest=regions,every=2h"})
	s, tap, w, done, err := newSpeed(cfg, nil, nil, newTestCollector(), nil, nil)
	if err != nil || tap == nil || w == nil {
		t.Fatal(err)
	}
	done()
	if st := s.State(); len(st.Servers) != 45 || len(st.Regions) != 19 || len(st.Custom) != 0 || s.SweepSize() != 45 {
		t.Errorf("%d servers, %d regions", len(st.Servers), len(st.Regions))
	}
	cfg, _ = parseFlags([]string{"--speedtest-server", "iperf.example.net:5201"})
	s, _, _, _, _ = newSpeed(cfg, nil, nil, newTestCollector(), nil, nil)
	if st := s.State(); len(st.Custom) != 1 || len(st.Servers) != 46 || len(st.Modes) != 4 {
		t.Errorf("custom: %+v", st.Custom)
	}
}

func newTestCollector() *metrics.Collector {
	return metrics.New(func() *model.Profile { return nil })
}
