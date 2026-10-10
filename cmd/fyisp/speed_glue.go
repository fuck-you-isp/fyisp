package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/metrics"
	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/probe"
	"github.com/fuck-you-isp/fyisp/internal/speedtest"
)

// speedFlag is --speedtest: alone it switches the schedule on (every 15m);
// =every=30m sets the interval; =off leaves it to the Details toggle.
type speedFlag struct {
	on    bool
	every time.Duration
}

func (f *speedFlag) String() string {
	switch {
	case !f.on:
		return ""
	case f.every > 0:
		return "every=" + f.every.String()
	}
	return "on"
}

func (f *speedFlag) IsBoolFlag() bool { return true }

func (f *speedFlag) Set(v string) error {
	switch v = strings.ToLower(strings.TrimSpace(v)); v {
	case "true", "on", "":
		f.on = true
		return nil
	case "false", "off":
		f.on, f.every = false, 0
		return nil
	}
	if d, ok := strings.CutPrefix(v, "every="); ok {
		iv, err := time.ParseDuration(d)
		if err != nil || iv <= 0 {
			return fmt.Errorf("bad interval %q", d)
		}
		f.on, f.every = true, iv
		return nil
	}
	return fmt.Errorf("want on, off or every=<duration> (e.g. every=30m)")
}

// newSpeed sets up the speed tests: the scheduler (always present, so the
// Details toggle and Test now work; it only tests when switched on), the
// load tap and the windows the mask uses.
func newSpeed(c config, prof *model.Profile, st speedtest.Store, mc *metrics.Collector, log *slog.Logger) (*speedtest.Scheduler, *speedtest.LoadTap, *speedtest.Windows, error) {
	o := speedtest.Options{Enabled: c.speed.on, Interval: c.speed.every, Store: st, Windows: &speedtest.Windows{}, Log: log,
		Ping: func(ctx context.Context, host string) (time.Duration, error) {
			return probe.PingOnce(ctx, host, 2*time.Second)
		}}
	if c.speedServer != "" {
		cs, err := speedtest.ParseCustom(c.speedServer)
		if err != nil {
			return nil, nil, nil, err
		}
		o.Custom = &cs
	} else {
		ss, err := speedtest.Builtin()
		if err != nil {
			return nil, nil, nil, fmt.Errorf("speed test servers: %w", err)
		}
		o.Servers = ss
	}
	o.Load = speedtest.NewLoadTap(prof, nil)
	sm := metrics.NewSpeed()
	mc.Register(sm)
	o.OnResult = sm.Observe
	return speedtest.New(o), o.Load, o.Windows, nil
}

// maskedTrace blanks trace hop samples taken during a speed test, like
// every other probe's.
type maskedTrace struct {
	traceSinks
	mask speedtest.Mask
}

func (m maskedTrace) Observe(s model.Sample) { m.mask.Observe(s) }
