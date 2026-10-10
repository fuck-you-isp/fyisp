package main

import (
	"strings"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/metrics"
	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/netinfo"
	"github.com/fuck-you-isp/fyisp/internal/speedtest"
)

func TestSpeedFlags(t *testing.T) {
	for _, c := range []struct {
		args  []string
		mode  speedtest.Mode
		every time.Duration
		err   string
	}{
		{nil, "", 0, ""},
		{[]string{"--speedtest"}, speedtest.ModeRegions, 0, ""},
		{[]string{"--speedtest=on"}, speedtest.ModeRegions, 0, ""},
		{[]string{"--speedtest=nearest"}, speedtest.ModeNearest, 0, ""},
		{[]string{"--speedtest=regions,every=30m"}, speedtest.ModeRegions, 30 * time.Minute, ""},
		{[]string{"--speedtest=off"}, speedtest.ModeOff, 0, ""},
		{[]string{"--speedtest=every=5m"}, "", 0, "at least 15m0s"},
		{[]string{"--speedtest=every=5m", "--speedtest-server", "10.0.0.2:5201"}, speedtest.ModeCustom, 5 * time.Minute, ""},
		{[]string{"--speedtest=every=5m", "--speedtest-server", "10.0.0.2:5201,Leaseweb-Tokyo"}, "", 0, "at least 15m0s"},
		{[]string{"--speedtest", "--speedtest-server", "Leaseweb-Tokyo"}, speedtest.ModeCustom, 0, ""},
		{[]string{"--speedtest=custom"}, "", 0, "needs --speedtest-server"},
		{[]string{"--speedtest-server", "nope"}, "", 0, "want host:port or a catalog server id"},
		{[]string{"--speedtest=sometimes"}, "", 0, "want regions, nearest"},
	} {
		cfg, err := parseFlags(c.args)
		switch {
		case c.err != "":
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("%v: %v, want %q", c.args, err, c.err)
			}
		case err != nil:
			t.Errorf("%v: %v", c.args, err)
		case cfg.speed.mode != c.mode || cfg.speed.every != c.every:
			t.Errorf("%v: %+v", c.args, cfg.speed)
		}
	}
	// The scheduler is always there (Details choice, Test now), off unless asked.
	cfg, _ := parseFlags([]string{"--speedtest=regions,every=1h"})
	s, tap, w, err := newSpeed(cfg, nil, nil, newTestCollector(), func() netinfo.Path { return netinfo.Path{} }, nil)
	if err != nil || tap == nil || w == nil {
		t.Fatal(err)
	}
	if st := s.State(); len(st.Servers) != 41 || len(st.Regions) != 9 || s.Interval() != time.Hour || len(st.Custom) != 0 {
		t.Errorf("%d servers, %d regions, %v", len(st.Servers), len(st.Regions), s.Interval())
	}
	cfg, _ = parseFlags([]string{"--speedtest-server", "iperf.example.net:5201"})
	s, _, _, _ = newSpeed(cfg, nil, nil, newTestCollector(), func() netinfo.Path { return netinfo.Path{} }, nil)
	if st := s.State(); len(st.Custom) != 1 || len(st.Servers) != 42 || len(s.Modes()) != 4 {
		t.Errorf("custom: %+v", st.Custom)
	}
}

func newTestCollector() *metrics.Collector {
	return metrics.New(func() *model.Profile { return nil })
}
