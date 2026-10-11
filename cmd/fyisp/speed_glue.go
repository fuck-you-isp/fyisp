package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/metrics"
	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/netinfo"
	"github.com/fuck-you-isp/fyisp/internal/probe"
	"github.com/fuck-you-isp/fyisp/internal/speedtest"
)

// speedFlag is --speedtest[=MODE][,every=DUR][,add=REGION...][,far=REGION...]:
// alone (or on) it means speedtest.DefaultMode, or custom with
// --speedtest-server; MODE is regions, nearby, custom or off. add= (mode
// regions) and far= (mode nearby) take region IDs, comma-separated. Unset, the saved Details
// choice decides (off by default).
type speedFlag struct {
	set   bool
	mode  speedtest.Mode
	every time.Duration
	extra []string
}

func (f *speedFlag) String() string {
	if !f.set {
		return ""
	}
	s := string(f.mode)
	if f.every > 0 {
		s += ",every=" + f.every.String()
	}
	if len(f.extra) > 0 {
		s += ",add=" + strings.Join(f.extra, ",")
	}
	return s
}

func (f *speedFlag) IsBoolFlag() bool { return true }

func (f *speedFlag) Set(v string) error {
	*f = speedFlag{set: true}
	regions := false
	for _, part := range strings.Split(strings.ToLower(strings.TrimSpace(v)), ",") {
		part = strings.TrimSpace(part)
		switch {
		case part == "true" || part == "on" || part == "":
			f.mode, regions = "", false // checkSpeedFlags: the default mode, or custom with --speedtest-server
		case part == "false" || part == "off":
			f.mode, regions = speedtest.ModeOff, false
		case part == "regions" || part == "nearby" || part == "custom":
			f.mode, regions = speedtest.Mode(part), false
		case strings.HasPrefix(part, "every="):
			iv, err := time.ParseDuration(strings.TrimPrefix(part, "every="))
			if err != nil || iv <= 0 {
				return fmt.Errorf("bad interval in %q", part)
			}
			f.every, regions = iv, false
		case strings.HasPrefix(part, "add=") || strings.HasPrefix(part, "far="):
			regions = true
			f.extra = append(f.extra, part[4:])
		case regions && !strings.Contains(part, "="):
			f.extra = append(f.extra, part)
		default:
			return fmt.Errorf("want regions, nearby, custom, off, every=<duration>, add=<regions> or far=<regions> (e.g. nearby,far=us-west)")
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

// checkSpeedFlags settles the mode (--speedtest alone: the default mode, or
// custom with --speedtest-server) and checks the servers, the regions and
// the interval floor.
func checkSpeedFlags(c *config) error {
	cat, err := speedtest.Builtin()
	if err != nil {
		return err
	}
	var custom []speedtest.Server
	if c.speedServer != "" {
		if custom, err = speedtest.Custom(c.speedServer, cat); err != nil {
			return err
		}
	}
	if c.speed.set && c.speed.mode == "" {
		c.speed.mode = speedtest.DefaultMode
		if len(custom) > 0 {
			c.speed.mode = speedtest.ModeCustom
		}
	}
	if c.speed.mode == speedtest.ModeCustom && len(custom) == 0 {
		return fmt.Errorf("--speedtest=custom needs --speedtest-server")
	}
	for _, id := range speedSkips(c) {
		if _, ok := cat.Server(id); !ok {
			return fmt.Errorf("--speedtest-skip: no catalog server %q (see the README's \"Speed tests\")", id)
		}
	}
	for _, r := range c.speed.extra {
		if !slices.ContainsFunc(cat.Regions, func(x speedtest.Region) bool { return x.ID == r }) {
			return fmt.Errorf("--speedtest: no region %q (see the README's \"Speed tests\")", r)
		}
	}
	own := c.speed.mode == speedtest.ModeCustom && speedtest.OwnOnly(custom)
	if c.speed.every > 0 && c.speed.every < speedtest.MinInterval && !own {
		return fmt.Errorf("--speedtest every=%s: at least %s against public servers (down to %s in mode custom with only servers on your own network)",
			c.speed.every, speedtest.MinInterval, speedtest.MinOwnInterval)
	}
	if c.speed.every > 0 && c.speed.every < speedtest.MinOwnInterval {
		return fmt.Errorf("--speedtest every=%s: at least %s", c.speed.every, speedtest.MinOwnInterval)
	}
	return nil
}

// newSpeed sets up the speed tests: the scheduler (always present, so the
// Details choice and Test now work; it only tests when a mode is on), the
// load tap and the windows the mask uses.
func newSpeed(c config, prof *model.Profile, st speedtest.Store, mc *metrics.Collector, path func() netinfo.Path, log *slog.Logger) (*speedtest.Scheduler, *speedtest.LoadTap, *speedtest.Windows, error) {
	cat, err := speedtest.Builtin()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("speed test catalog: %w", err)
	}
	o := speedtest.Options{Catalog: cat, Mode: c.speed.mode, Interval: c.speed.every, Extra: c.speed.extra, Store: st,
		Windows: &speedtest.Windows{}, Log: log,
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
	o.Skip = speedSkips(&c)
	o.Load = speedtest.NewLoadTap(prof, nil)
	// Latency under load (ISP edge and gateway), the connection type and
	// the handoff network, from the network path.
	o.Path = func() (netip.Addr, netip.Addr) { p := path(); return p.Gateway, p.Edge }
	o.OpenPinger = func() (speedtest.LoadPinger, error) { return probe.OpenPinger() }
	o.Conn = func() string { return netinfo.ConnType(path().Iface) }
	hf := &speedtest.HandoffFinder{Edge: func() netip.Addr { return path().Edge }}
	o.Handoff, o.HandoffCached = hf.Handoff, hf.Cached
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

// speedSkips is --speedtest-skip's server IDs.
func speedSkips(c *config) []string {
	var out []string
	for _, id := range strings.Split(c.speedSkip, ",") {
		if id = strings.TrimSpace(id); id != "" {
			out = append(out, id)
		}
	}
	return out
}
