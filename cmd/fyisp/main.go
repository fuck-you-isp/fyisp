// Command fyisp is a single-binary internet quality monitor: it probes the
// apps and services you use (TCP, HTTPS, ICMP, STUN over UDP, traceroutes)
// and optionally runs iperf3 speed tests, keeps every sample for up to 90
// days in a local SQLite file, and serves a dashboard that says whose fault
// a problem is.
package main

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	// Embedded CA roots, used only when the OS has none (scratch image,
	// minimal systems), so HTTPS probes of --config files still work.
	_ "golang.org/x/crypto/x509roots/fallback"

	"github.com/fuck-you-isp/fyisp/internal/asn"
	"github.com/fuck-you-isp/fyisp/internal/baseline"
	"github.com/fuck-you-isp/fyisp/internal/metrics"
	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/netinfo"
	"github.com/fuck-you-isp/fyisp/internal/probe"
	"github.com/fuck-you-isp/fyisp/internal/profile"
	"github.com/fuck-you-isp/fyisp/internal/tiles"
	"github.com/fuck-you-isp/fyisp/internal/trace"
	"github.com/fuck-you-isp/fyisp/internal/verdict"
	"github.com/fuck-you-isp/fyisp/internal/web"
)

var version = "dev"

// GC defaults, unless GOGC / GOMEMLIMIT are set. The live heap is small
// (~15 MB: probe connections, the store's current hours) but GOGC=100 lets
// the heap grow to twice that between collections, and the runtime returns
// freed heap to the OS only slowly, so RSS follows the peaks. GOGC=50 keeps
// the peaks ~10-15 MB lower for about one more 3 ms collection a minute
// (<0.1% of a core). The soft memory limit is a safety net: steady state
// is well below it, and it only makes the GC work harder when a burst of
// large dashboard queries would otherwise let the heap double. It grows
// with the target count (a target probed over HTTPS keeps a connection,
// ~60 KB live; shipped profiles use TCP only, which needs far less): a fixed
// 96 MB limit made an 800-target HTTPS run collect every second and spend
// 60% of its CPU in the GC.
const (
	defaultGCPercent     = 50
	baseMemoryLimit      = 96 << 20
	memoryLimitPerTarget = 128 << 10
)

// memoryLimit is the soft memory limit for a run with n targets.
func memoryLimit(n int) int64 { return baseMemoryLimit + int64(n)*memoryLimitPerTarget }

// tuneGC applies the GC defaults above where the environment sets none.
func tuneGC(targets int) {
	if os.Getenv("GOGC") == "" {
		debug.SetGCPercent(defaultGCPercent)
	}
	if os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(memoryLimit(targets))
	}
}

const (
	defaultListen = "127.0.0.1:3000"
	maxRetention  = 90 * 24 * time.Hour
	flushEvery    = time.Minute
	pruneEvery    = time.Hour
)

type config struct {
	listen      string
	dataDir     string
	ephemeral   bool
	retention   time.Duration
	configPath  string
	profiles    profileNames
	geos        geoList
	noPath      bool
	noUDP       bool
	speed       speedFlag
	speedSkip   string // --speedtest-skip
	speedServer string
	openBrowser bool
	adminToken  string
	allowHosts  hostList
	pprofAddr   string
	logFormat   string
	logLevel    string
	force       bool
}

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "version", "--version", "-version":
			fmt.Println("fyisp", version)
			return
		case "paths":
			os.Exit(cmdPaths(os.Args[2:]))
		case "export":
			os.Exit(cmdExport(os.Args[2:]))
		case "profiles":
			runProfiles()
		}
	}
	cfg, err := parseFlags(os.Args[1:])
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		fmt.Fprintln(os.Stderr, "fyisp:", err)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, stop, cfg); err != nil {
		var ec exitCode
		if errors.As(err, &ec) {
			os.Exit(int(ec))
		}
		slog.Error("fyisp stopped", "err", err)
		os.Exit(1)
	}
}

// exitCode lets run request a specific process exit code.
type exitCode int

func (e exitCode) Error() string { return "exit " + strconv.Itoa(int(e)) }

func parseFlags(args []string) (config, error) {
	var c config
	var retention string
	fs := flag.NewFlagSet("fyisp", flag.ContinueOnError)
	fs.StringVar(&c.listen, "listen", cmp.Or(os.Getenv("FYISP_LISTEN"), defaultListen), "local dashboard address (0.0.0.0:3000 exposes it to your LAN; default: $FYISP_LISTEN, else "+defaultListen+")")
	fs.StringVar(&c.dataDir, "data-dir", "", "where to keep data (default: $FYISP_DATA_DIR, else the per-OS state directory; see `fyisp paths`)")
	fs.BoolVar(&c.ephemeral, "ephemeral", false, "keep data in a temporary directory deleted on exit")
	fs.StringVar(&retention, "retention", "90d", "how long to keep data (max 90d), e.g. 14d or 48h")
	fs.StringVar(&c.configPath, "config", "", "local profile file (YAML); may `extends: [default]` or other named profiles")
	fs.Var(&c.profiles, "profile", "target `profiles` to measure, comma-separated (a union), e.g. aws,europe; see `fyisp profiles` (default: $FYISP_PROFILE, else default)")
	fs.Var(&c.geos, "geo", "with --profile: only targets in these `regions`: na, sa, eu, me, af, as, oc (or north-america, europe, ...)")
	fs.BoolVar(&c.noPath, "no-path", false, "leave out the built-in Network path group (gateway, ISP edge, anycast resolvers)")
	fs.BoolVar(&c.noUDP, "no-udp", false, "leave out every UDP (STUN) target: no call-path measurements")
	fs.Var(&c.speed, "speedtest", "run iperf3 speed tests on a schedule: --speedtest (9 regions, each hourly), --speedtest=nearby (3 nearby providers and 1 far region, every 15m), --speedtest=regions,add=canada, --speedtest=nearby,far=us-west, every=30m, --speedtest=off (default: the Details setting); see the README's \"Speed tests\"")
	fs.StringVar(&c.speedSkip, "speedtest-skip", "", "catalog server ids never to test, comma-separated (an operator asked, or you prefer not to)")
	fs.StringVar(&c.speedServer, "speedtest-server", "", "your own iperf3 servers (`host:port`) and/or catalog server ids, comma-separated: mode custom; no 15-minute floor (1 minute) when all are on your own network")
	fs.BoolVar(&c.openBrowser, "open-browser", true, "open the dashboard in a browser at startup (interactive terminals only)")
	fs.StringVar(&c.adminToken, "admin-token", "", "token required for speed-test controls (Test speed now, cancel, the schedule, skipped servers) when --listen is not loopback or --allow-host is set; by default one is generated, kept in the data directory and shown as a link in the log; off turns the speed-test controls off (traces stay open to any viewer)")
	fs.Var(&c.allowHosts, "allow-host", "extra `name` the dashboard may be opened as, e.g. nas.local or nas.local:3000 (repeatable or comma-separated)")
	fs.StringVar(&c.pprofAddr, "pprof", "", "serve Go pprof on this loopback address (debugging)")
	fs.StringVar(&c.logFormat, "log-format", "auto", "log format: auto, text or json")
	fs.StringVar(&c.logLevel, "log-level", "info", "log level: debug, info, warn or error")
	fs.BoolVar(&c.force, "force", false, "start even if the data directory has less than 200 MB free")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: fyisp [flags]\n       fyisp profiles [NAME[,NAME...]]\n       fyisp export [flags]\n       fyisp paths\n       fyisp version\n\nFlags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return c, err
	}
	if fs.NArg() > 0 {
		return c, fmt.Errorf("unexpected argument %q (see fyisp -h)", fs.Arg(0))
	}
	d, err := parseRetention(retention)
	if err != nil {
		return c, err
	}
	c.retention = d
	if c.ephemeral && c.dataDir != "" {
		return c, fmt.Errorf("--ephemeral and --data-dir are mutually exclusive")
	}
	if c.configPath != "" && (len(c.profiles) > 0 || len(c.geos) > 0) {
		return c, fmt.Errorf("--config and --profile/--geo are mutually exclusive: use `extends: [%s]` in the file", strings.Join(c.profiles, ", "))
	}
	if err := checkSpeedFlags(&c); err != nil {
		return c, err
	}
	envProfiles(&c.profiles, c.configPath)
	return c, nil
}

// envProfiles fills in profiles from FYISP_PROFILE (for container and NAS
// templates, where an environment variable is easier to set than an
// argument) unless --profile or --config was given: the flags win.
func envProfiles(p *profileNames, configPath string) {
	if v := os.Getenv("FYISP_PROFILE"); v != "" && len(*p) == 0 && configPath == "" {
		_ = p.Set(v)
	}
}

// hostList is --allow-host: Host header values, repeatable or comma-separated.
type hostList []string

func (h *hostList) String() string { return strings.Join(*h, ",") }

func (h *hostList) Set(v string) error {
	for _, s := range strings.Split(v, ",") {
		s = strings.ToLower(strings.TrimSpace(s))
		if s == "" {
			continue
		}
		if strings.ContainsAny(s, "/ @?#") {
			return fmt.Errorf("invalid host %q: want a name or name:port, e.g. nas.local", s)
		}
		*h = append(*h, s)
	}
	return nil
}

// parseRetention accepts Go durations plus a "d" (days) suffix.
func parseRetention(s string) (time.Duration, error) {
	var d time.Duration
	var err error
	if n, ok := strings.CutSuffix(s, "d"); ok {
		var days float64
		days, err = strconv.ParseFloat(n, 64)
		d = time.Duration(days * float64(24*time.Hour))
	} else {
		d, err = time.ParseDuration(s)
	}
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("invalid --retention %q", s)
	}
	if d > maxRetention {
		return 0, fmt.Errorf("--retention %q exceeds the 90d maximum", s)
	}
	return d, nil
}

func newLogger(c config, w io.Writer) *slog.Logger {
	var lvl slog.Level
	_ = lvl.UnmarshalText([]byte(c.logLevel))
	opts := &slog.HandlerOptions{Level: lvl}
	if c.logFormat == "json" || (c.logFormat == "auto" && !isTerminal(os.Stderr)) {
		return slog.New(slog.NewJSONHandler(w, opts))
	}
	return slog.New(slog.NewTextHandler(w, opts))
}

func run(ctx context.Context, stop context.CancelFunc, c config) error {
	tuneGC(0)
	log := newLogger(c, os.Stderr)
	// Before probe.New: probe filters the standard logger's http2 noise.
	slog.SetDefault(log)
	started := time.Now()

	prof, err := loadProfile(c)
	if err != nil {
		return err
	}
	tuneGC(len(prof.Targets))

	dir, cleanup, err := resolveDataDir(c)
	if err != nil {
		return err
	}
	defer cleanup()
	if !c.force {
		if err := checkFreeSpace(dir, 200<<20); err != nil {
			return err
		}
	}

	// Path discovery (gateway, ISP edge) feeds the @gateway/@isp-edge targets
	// and the tracer; it starts running below.
	ni := netinfo.New(netinfo.Options{Log: log})
	// Traceroutes: always-on for profile targets marked trace, on demand
	// (Investigate) for any target. Hop owners come from the built-in table.
	tr := trace.New(trace.Options{Log: log, Lookup: asn.Lookup, Path: ni.Current})

	st, err := openStore(dir, prof, tr.Interval, log)
	if err != nil {
		var locked *lockedError
		if errors.As(err, &locked) {
			fmt.Fprintf(os.Stderr, "fyisp is already running (%s)\n", locked.Holder)
			return exitCode(3)
		}
		return err
	}
	defer st.Close() // last: final save, checkpoint, release the lock

	mc := metrics.New(func() *model.Profile { return prof })
	mos := metrics.NewCallMOS(func() *model.Profile { return prof })
	mc.Register(mos)
	// Warm-up counts ordinary targets only (path targets may never report).
	warm := web.Warmup{Only: map[string]bool{}}
	for _, t := range prof.Targets {
		if t.Layer == "" {
			warm.Only[t.Name] = true
		}
	}
	// The verdict engine judges whose fault a problem is and keeps the
	// outage log in the store; it must stop before the store closes.
	// "Your normal": per-series baselines from stored history, refreshed
	// hourly; the verdict engine judges latency against them.
	bs := baseline.New(st, func() []model.SeriesKey { return probeKeys(prof) }, baseline.Options{Log: log})
	eng := verdict.New(func() *model.Profile { return prof }, st, verdict.Options{Log: log, Baseline: bs.Get})
	mc.SetVerdictSource(func() (model.Verdict, map[string]*bool) { return eng.Current(), eng.Layers() })
	mc.SetVsNormalSource(eng.EachVsNormal)
	go func() {
		if err := ni.Run(ctx); err != nil {
			log.Warn("network path discovery stopped", "err", err)
		}
	}()
	runner := probe.New(probe.Options{Log: log, UserAgent: "fyisp/" + version, Path: ni.Current})
	// Speed tests: the load tap keeps loaded ICMP samples for latency
	// under load; the windows are recorded for the charts.
	speed, tap, wins, err := newSpeed(c, prof, st, mc, ni.Current, log)
	if err != nil {
		return err
	}
	// The Home tiles: judged per minute from their targets' samples, the
	// last 8 days replayed from the store at start.
	tl := tiles.New(prof, profile.ResolveTiles(prof), tiles.Sources{
		Store: st, SpeedTests: st.SpeedTests, Incidents: eng.Incidents, Verdict: eng.Current,
		Layers: eng.Layers, SpeedWins: wins, PathChanges: ni.Subscribe,
	}, tiles.Options{Log: log})
	// Samples taken during a speed test are kept as measured: the chart
	// shades the test windows, so a spike can be matched to a test.
	probeSinks := model.Fanout{tap, st, mc, mos, eng, &warm, tl}
	caps := runner.Caps()

	// The dashboard (+ /metrics).
	localLn, err := listenLocal(c.listen)
	if err != nil {
		return err
	}

	deps := web.Deps{
		Profile:       func() *model.Profile { return prof },
		CustomProfile: c.configPath != "",
		Store:         st,
		Status: func() web.Status {
			return web.Status{Version: version, Started: started, Caps: caps, Targets: len(warm.Only), Ready: warm.Ready()}
		},
		Metrics:   mc.Handler(),
		Verdict:   eng,
		Baselines: bs,
		Slow:      eng.Slow,
		Trace:     newWebTrace(st, tr, prof),
		Path:      ni.Current,
		Tiles:     tl,
		Log:       log,
	}
	deps.UDP, _ = runner.(probe.UDPStateSource)
	deps.Speed = speed
	localAddr := localLn.Addr().String()
	adminTok, adminStored, err := resolveAdminToken(c.adminToken, localAddr, c.allowHosts, dir)
	if err != nil {
		return err
	}
	if c.adminToken == adminTokenOff && len(c.allowHosts) > 0 && isLoopbackAddr(localAddr) {
		log.Warn("--admin-token=off with --allow-host: speed-test controls are disabled for every browser, this one included")
	}
	localSrv := newServer(web.Local(deps, web.LocalOptions{Addr: localAddr, AdminToken: adminTok, ExtraHosts: c.allowHosts}))
	serve := func(name string, s *http.Server, ln net.Listener) {
		if err := s.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http server failed", "server", name, "err", err)
			stop()
		}
	}
	go serve("local", localSrv, localLn)
	if c.pprofAddr != "" {
		if err := servePprof(c.pprofAddr, log); err != nil {
			return err
		}
	}

	localURL := "http://" + displayAddr(localAddr) + "/"
	setLockURL(st, localURL)
	log.Info("fyisp started", "version", version, "data_dir", dir, "targets", len(prof.Targets),
		"icmp", caps.ICMP, "retention", c.retention.String(), "url", localURL)
	if caps.ICMP == "unavailable" && caps.ICMPHint != "" {
		log.Warn("ICMP probes disabled", "hint", caps.ICMPHint)
	}
	fmt.Fprintf(os.Stderr, "\n  Dashboard:  %s\n\n", localURL)
	openURL := localURL
	if adminStored {
		// The generated token, every start (like Jupyter), so a recreated
		// container on the same volume can still show it. Reading the log
		// already takes as much access as reading the data directory.
		link := adminLink(localURL, adminTok)
		log.Info("open this link once in your browser to use Test speed now and the other speed-test controls", "url", link)
		fmt.Fprintf(os.Stderr, "  To use Test speed now and the other speed-test controls, open this link once:\n  %s\n\n", link)
		openURL = link
	}
	if c.openBrowser && isTerminal(os.Stdout) {
		openBrowser(openURL)
	}

	// Probing, persistence and retention.
	probeDone := make(chan struct{})
	go func() {
		defer close(probeDone)
		if err := runner.Run(ctx, prof, probeSinks); err != nil {
			log.Error("probing stopped", "err", err)
			stop()
		}
	}()
	go maintain(ctx, st, c.retention, log)
	speedDone := make(chan struct{})
	go func() {
		defer close(speedDone)
		if err := speed.Run(ctx); err != nil && ctx.Err() == nil {
			log.Warn("speed tests stopped", "err", err)
		}
	}()
	go func() {
		if err := bs.Run(ctx); err != nil && ctx.Err() == nil {
			log.Warn("baselines stopped", "err", err)
		}
	}()
	traceDone := make(chan struct{})
	go func() {
		defer close(traceDone)
		// Trace samples go to the store and /metrics only: the verdict
		// engine and warm-up judge targets, not individual hops.
		ts := traceSinks{st, mc}
		if err := tr.Run(ctx, prof, ts); err != nil && ctx.Err() == nil {
			log.Warn("traceroutes unavailable", "err", err)
		}
	}()
	go func() {
		if err := tl.Run(ctx); err != nil {
			log.Error("tiles engine stopped", "err", err)
		}
	}()
	verdictDone := make(chan struct{})
	go func() {
		defer close(verdictDone)
		if err := eng.Run(ctx); err != nil {
			log.Error("verdict engine stopped", "err", err)
		}
	}()

	<-ctx.Done()
	log.Info("shutting down")
	stop() // restore default signal handling: a second Ctrl-C exits at once
	<-probeDone
	<-verdictDone // saves the open incident
	<-traceDone
	<-speedDone // a test is abandoned, not saved, on shutdown
	flushCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := st.Flush(flushCtx); err != nil {
		log.Error("final save failed", "err", err)
	}
	shutCtx, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	_ = localSrv.Shutdown(shutCtx)
	return nil
}

// maintain saves the current hour every minute and prunes hourly.
func maintain(ctx context.Context, st storeT, retention time.Duration, log *slog.Logger) {
	flush := time.NewTicker(flushEvery)
	prune := time.NewTicker(pruneEvery)
	defer flush.Stop()
	defer prune.Stop()
	doPrune := func() {
		if err := st.Prune(ctx, time.Now().Add(-retention)); err != nil && ctx.Err() == nil {
			log.Warn("retention prune failed", "err", err)
		}
	}
	doPrune()
	var lastErr string
	for {
		select {
		case <-ctx.Done():
			return
		case <-flush.C:
			err := st.Flush(ctx)
			switch {
			case err != nil && ctx.Err() == nil && err.Error() != lastErr:
				log.Error("saving data failed; keeping it in memory and retrying", "err", err)
				lastErr = err.Error()
			case err == nil && lastErr != "":
				log.Info("saving data works again")
				lastErr = ""
			}
		case <-prune.C:
			doPrune()
		}
	}
}

// listenLocal binds the dashboard. With the default address, busy ports fall
// through to 3001..3010.
func listenLocal(addr string) (net.Listener, error) {
	ln, err := net.Listen("tcp", addr)
	if err == nil || addr != defaultListen {
		return ln, err
	}
	for p := 3001; p <= 3010; p++ {
		if ln, err2 := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(p)); err2 == nil {
			return ln, nil
		}
	}
	return nil, fmt.Errorf("ports 3000-3010 are busy: %w", err)
}

func newServer(h http.Handler) *http.Server {
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
}

func servePprof(addr string, log *slog.Logger) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("--pprof: %w", err)
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("--pprof must be a loopback address, got %q", addr)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	go func() { _ = newServer(mux).Serve(ln) }()
	log.Info("pprof enabled", "addr", ln.Addr().String())
	return nil
}

// displayAddr turns a wildcard listen address into one a browser can open.
func displayAddr(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
