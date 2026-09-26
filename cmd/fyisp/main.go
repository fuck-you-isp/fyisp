// Command fyisp is a single-binary internet quality monitor: it probes
// hyperscalers and popular services over HTTPS, TCP and ICMP, keeps every
// sample for up to 90 days in a local SQLite file, serves a dashboard, and can
// share a redacted read-only view over a Cloudflare quick tunnel.
package main

import (
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
	"strconv"
	"strings"
	"syscall"
	"time"

	// Embedded CA roots, used only when the OS has none (scratch image,
	// minimal systems), so HTTPS probes and the tunnel still work.
	_ "golang.org/x/crypto/x509roots/fallback"

	"github.com/fuck-you-isp/fyisp/internal/metrics"
	"github.com/fuck-you-isp/fyisp/internal/model"
	"github.com/fuck-you-isp/fyisp/internal/probe"
	"github.com/fuck-you-isp/fyisp/internal/profile"
	"github.com/fuck-you-isp/fyisp/internal/web"
)

var version = "dev"

const (
	defaultListen = "127.0.0.1:3000"
	maxRetention  = 90 * 24 * time.Hour
	flushEvery    = time.Minute
	pruneEvery    = time.Hour
)

type config struct {
	listen        string
	share         bool
	shareProtocol string
	dataDir       string
	ephemeral     bool
	retention     time.Duration
	configPath    string
	openBrowser   bool
	adminToken    string
	allowHosts    hostList
	pprofAddr     string
	logFormat     string
	logLevel      string
	force         bool
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
	fs.StringVar(&c.listen, "listen", defaultListen, "local dashboard address (0.0.0.0:3000 exposes it to your LAN)")
	fs.BoolVar(&c.share, "share", false, "start a public read-only link (Cloudflare quick tunnel) at startup")
	fs.StringVar(&c.shareProtocol, "share-protocol", "auto", "tunnel protocol: auto, quic or http2")
	fs.StringVar(&c.dataDir, "data-dir", "", "where to keep data (default: per-OS state directory; see `fyisp paths`)")
	fs.BoolVar(&c.ephemeral, "ephemeral", false, "keep data in a temporary directory deleted on exit")
	fs.StringVar(&retention, "retention", "90d", "how long to keep data (max 90d), e.g. 14d or 48h")
	fs.StringVar(&c.configPath, "config", "", "local profile file (YAML); may `extends: [default]`")
	fs.BoolVar(&c.openBrowser, "open-browser", true, "open the dashboard in a browser at startup (interactive terminals only)")
	fs.StringVar(&c.adminToken, "admin-token", "", "token required for dashboard controls when --listen is not loopback")
	fs.Var(&c.allowHosts, "allow-host", "extra `name` the dashboard may be opened as, e.g. nas.local or nas.local:3000 (repeatable or comma-separated)")
	fs.StringVar(&c.pprofAddr, "pprof", "", "serve Go pprof on this loopback address (debugging)")
	fs.StringVar(&c.logFormat, "log-format", "auto", "log format: auto, text or json")
	fs.StringVar(&c.logLevel, "log-level", "info", "log level: debug, info, warn or error")
	fs.BoolVar(&c.force, "force", false, "start even if the data directory has less than 200 MB free")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: fyisp [flags]\n       fyisp export [flags]\n       fyisp paths\n       fyisp version\n\nFlags:\n")
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
	switch c.shareProtocol {
	case "auto", "quic", "http2":
	default:
		return c, fmt.Errorf("--share-protocol must be auto, quic or http2")
	}
	if c.ephemeral && c.dataDir != "" {
		return c, fmt.Errorf("--ephemeral and --data-dir are mutually exclusive")
	}
	return c, nil
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
	log := newLogger(c, os.Stderr)
	// Before probe.New: probe filters the standard logger's http2 noise.
	slog.SetDefault(log)
	started := time.Now()

	prof, err := loadProfile(c.configPath)
	if err != nil {
		return err
	}

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

	st, err := openStore(dir, prof, log)
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
	var warm web.Warmup
	runner := probe.New(probe.Options{Log: log, UserAgent: "fyisp/" + version})
	caps := runner.Caps()

	// Listeners: the local dashboard (+ /metrics), and a loopback-only public
	// origin that the tunnel publishes.
	localLn, err := listenLocal(c.listen)
	if err != nil {
		return err
	}
	publicLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	secret, err := randomHex(16)
	if err != nil {
		return err
	}
	share := newShareControl(ctx, "http://"+publicLn.Addr().String(), secret, c.shareProtocol, log)
	defer share.Stop()

	deps := web.Deps{
		Profile: func() *model.Profile { return prof },
		Store:   st,
		Status: func() web.Status {
			return web.Status{Version: version, Started: started, Caps: caps, Targets: len(prof.Targets), Ready: warm.Ready()}
		},
		Metrics: mc.Handler(),
		Share:   share,
		Log:     log,
	}
	localAddr := localLn.Addr().String()
	localSrv := newServer(web.Local(deps, web.LocalOptions{Addr: localAddr, AdminToken: c.adminToken, ExtraHosts: c.allowHosts}))
	publicSrv := newServer(web.Public(deps, secret))
	serve := func(name string, s *http.Server, ln net.Listener) {
		if err := s.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http server failed", "server", name, "err", err)
			stop()
		}
	}
	go serve("local", localSrv, localLn)
	go serve("public", publicSrv, publicLn)
	if c.pprofAddr != "" {
		if err := servePprof(c.pprofAddr, log); err != nil {
			return err
		}
	}

	localURL := "http://" + displayAddr(localAddr) + "/"
	setLockURL(st, localURL)
	log.Info("fyisp started", "version", version, "data_dir", dir, "targets", len(prof.Targets),
		"icmp", caps.ICMP, "tcp", caps.TCP, "https", caps.HTTPS, "retention", c.retention.String(), "url", localURL)
	if caps.ICMP == "unavailable" && caps.ICMPHint != "" {
		log.Warn("ICMP probes disabled", "hint", caps.ICMPHint)
	}
	fmt.Fprintf(os.Stderr, "\n  Dashboard:  %s\n\n", localURL)
	if c.share {
		if err := share.Start(ctx); err != nil {
			log.Warn("could not start the public link", "err", err)
		}
	}
	if c.openBrowser && isTerminal(os.Stdout) {
		openBrowser(localURL)
	}

	// Probing, persistence and retention.
	probeDone := make(chan struct{})
	go func() {
		defer close(probeDone)
		if err := runner.Run(ctx, prof, model.Fanout{st, mc, &warm}); err != nil {
			log.Error("probing stopped", "err", err)
			stop()
		}
	}()
	go maintain(ctx, st, c.retention, log)

	<-ctx.Done()
	log.Info("shutting down")
	stop() // restore default signal handling: a second Ctrl-C exits at once
	<-probeDone
	flushCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := st.Flush(flushCtx); err != nil {
		log.Error("final save failed", "err", err)
	}
	share.Stop()
	shutCtx, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	_ = localSrv.Shutdown(shutCtx)
	_ = publicSrv.Shutdown(shutCtx)
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

func loadProfile(path string) (*model.Profile, error) {
	if path == "" {
		return profile.Default()
	}
	p, err := profile.Load(path)
	if err != nil {
		return nil, fmt.Errorf("loading --config: %w", err)
	}
	return p, nil
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
