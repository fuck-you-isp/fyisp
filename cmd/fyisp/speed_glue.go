package main

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/metrics"
	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/probe"
	"github.com/fuck-you-isp/fyisp/internal/speedtest"
)

// speedFlag is --speedtest[=MODE][,every=DUR]: alone it means regions;
// MODE is on (regions), regions, nearest, custom or off. Unset, the saved
// Details choice decides (off by default).
type speedFlag struct {
	set   bool
	mode  speedtest.Mode
	every time.Duration
}

func (f *speedFlag) String() string {
	if !f.set {
		return ""
	}
	s := string(f.mode)
	if f.every > 0 {
		s += ",every=" + f.every.String()
	}
	return s
}

func (f *speedFlag) IsBoolFlag() bool { return true }

func (f *speedFlag) Set(v string) error {
	*f = speedFlag{set: true}
	for _, part := range strings.Split(strings.ToLower(strings.TrimSpace(v)), ",") {
		switch part = strings.TrimSpace(part); part {
		case "true", "on", "":
		case "false", "off":
			f.mode = speedtest.ModeOff
		case "regions", "nearest", "custom":
			f.mode = speedtest.Mode(part)
		default:
			d, ok := strings.CutPrefix(part, "every=")
			iv, err := time.ParseDuration(d)
			if !ok || err != nil || iv <= 0 {
				return fmt.Errorf("want regions, nearest, custom, off and/or every=<duration> (e.g. regions,every=30m)")
			}
			f.every = iv
		}
	}
	return nil
}

// optBool is a bool flag that remembers whether it was given.
type optBool struct {
	set, on bool
}

func (b *optBool) String() string {
	if !b.set {
		return ""
	}
	return strconv.FormatBool(b.on)
}

func (b *optBool) IsBoolFlag() bool { return true }

func (b *optBool) Set(v string) error {
	on, err := strconv.ParseBool(v)
	if err != nil {
		return fmt.Errorf("want true or false")
	}
	b.set, b.on = true, on
	return nil
}

// byteSize is a size flag: 50GB, 500MB, 2TB or plain bytes (decimal units).
type byteSize int64

func (b *byteSize) String() string {
	if *b == 0 {
		return ""
	}
	return strconv.FormatInt(int64(*b), 10)
}

func (b *byteSize) Set(v string) error {
	v = strings.ToUpper(strings.TrimSpace(v))
	mult := int64(1)
	for _, u := range []struct {
		suffix string
		m      int64
	}{{"TB", 1e12}, {"GB", 1e9}, {"MB", 1e6}, {"KB", 1e3}, {"B", 1}} {
		if n, ok := strings.CutSuffix(v, u.suffix); ok {
			v, mult = strings.TrimSpace(n), u.m
			break
		}
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f <= 0 {
		return fmt.Errorf("want a size such as 50GB or 500MB")
	}
	*b = byteSize(f * float64(mult))
	return nil
}

// checkSpeedFlags settles the mode (--speedtest alone: regions, or custom
// with --speedtest-server) and checks the servers and the interval floor.
func checkSpeedFlags(c *config) error {
	var custom []speedtest.Server
	if c.speedServer != "" {
		cat, err := speedtest.Builtin()
		if err != nil {
			return err
		}
		if custom, err = speedtest.Custom(c.speedServer, cat); err != nil {
			return err
		}
	}
	if c.speed.set && c.speed.mode == "" {
		c.speed.mode = speedtest.ModeRegions
		if len(custom) > 0 {
			c.speed.mode = speedtest.ModeCustom
		}
	}
	if c.speed.mode == speedtest.ModeCustom && len(custom) == 0 {
		return fmt.Errorf("--speedtest=custom needs --speedtest-server")
	}
	if c.speed.every > 0 && c.speed.every < speedtest.MinInterval && !(c.speed.mode == speedtest.ModeCustom && speedtest.OwnOnly(custom)) {
		return fmt.Errorf("--speedtest every=%s: at least %s against public servers (any interval in mode custom with only your own servers)", c.speed.every, speedtest.MinInterval)
	}
	return nil
}

// newSpeed sets up the speed tests: the scheduler (always present, so the
// Details choice and Test now work; it only tests when a mode is on), the
// load tap and the windows the mask uses.
func newSpeed(c config, prof *model.Profile, st speedtest.Store, mc *metrics.Collector, log *slog.Logger) (*speedtest.Scheduler, *speedtest.LoadTap, *speedtest.Windows, error) {
	cat, err := speedtest.Builtin()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("speed test catalog: %w", err)
	}
	o := speedtest.Options{Catalog: cat, Mode: c.speed.mode, Interval: c.speed.every, Store: st, Windows: &speedtest.Windows{}, Log: log,
		Ping: func(ctx context.Context, host string) (time.Duration, error) {
			return probe.PingOnce(ctx, host, 2*time.Second)
		}}
	if c.speedServer != "" {
		if o.Custom, err = speedtest.Custom(c.speedServer, cat); err != nil {
			return nil, nil, nil, err
		}
	}
	if c.speedPublic.set {
		on := c.speedPublic.on
		o.Public = &on
	}
	o.PublicBudget = int64(c.speedBudget)
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
