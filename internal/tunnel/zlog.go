package tunnel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"
)

type slogLogger = slog.Logger

// levelCloudflaredDebug is the slog level cloudflared's debug logs map to.
const levelCloudflaredDebug = slog.LevelDebug - 4

// newZerolog returns a zerolog.Logger for cloudflared that forwards each
// event to the slog logger returned by get (nil: dropped). Levels are demoted
// so a default Info logger only sees cloudflared warnings and errors, as Warn:
// the tunnel reports its health through its own state changes. While quiet is
// set, everything is logged at levelCloudflaredDebug.
//
// cloudflared never logs below Info, whatever the slog logger enables: at
// Debug it logs every proxied request with its headers, which carry the
// visitor's IP address (Cf-Connecting-Ip, X-Forwarded-For) and cookies. The
// bridge also drops header fields and masks IP addresses (redact), so
// nothing that could be the user's public address reaches the log.
func newZerolog(get func() *slogLogger, quiet *atomic.Bool) zerolog.Logger {
	lvl := zerolog.WarnLevel
	if l := get(); l != nil && l.Enabled(context.Background(), slog.LevelDebug) {
		lvl = zerolog.InfoLevel
	}
	return zerolog.New(zerologBridge{get, quiet}).Level(lvl)
}

type zerologBridge struct {
	get   func() *slogLogger
	quiet *atomic.Bool
}

func mapLevel(l string) slog.Level {
	switch l {
	case "trace", "debug":
		return levelCloudflaredDebug
	case "info", "":
		return slog.LevelDebug
	default: // warn, error, fatal, panic
		return slog.LevelWarn
	}
}

func (b zerologBridge) Write(p []byte) (int, error) {
	l := b.get()
	if l == nil {
		return len(p), nil
	}
	var m map[string]any
	dec := json.NewDecoder(bytes.NewReader(p))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		l.Debug(redact(string(bytes.TrimSpace(p))), "component", "cloudflared")
		return len(p), nil
	}
	levelStr, _ := m["level"].(string)
	lvl := mapLevel(levelStr)
	if b.quiet != nil && b.quiet.Load() {
		lvl = levelCloudflaredDebug
	}
	if !l.Enabled(context.Background(), lvl) {
		return len(p), nil
	}
	msg, _ := m["message"].(string)
	msg = redact(msg)
	var suppressed int
	if lvl >= slog.LevelWarn {
		ok, n := warnLimiter.allow(msg)
		if !ok {
			return len(p), nil
		}
		suppressed = n
	}
	delete(m, "level")
	delete(m, "message")
	delete(m, "time")
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	attrs := make([]slog.Attr, 0, len(keys)+2)
	attrs = append(attrs, slog.String("component", "cloudflared"))
	if levelStr == "error" || levelStr == "fatal" || levelStr == "panic" {
		attrs = append(attrs, slog.String("cf_level", levelStr))
	}
	for _, k := range keys {
		if headerKey(k) || hasHeaderKey(m[k]) || redact(k) != k {
			continue // a header, or an address as the key
		}
		switch v := m[k].(type) {
		case string:
			attrs = append(attrs, slog.String(k, redact(v)))
		case json.Number:
			attrs = append(attrs, slog.String(k, redact(v.String())))
		default:
			attrs = append(attrs, slog.String(k, redact(fmt.Sprint(v))))
		}
	}
	if suppressed > 0 {
		attrs = append(attrs, slog.Int("suppressed", suppressed))
	}
	l.LogAttrs(context.Background(), lvl, msg, attrs...)
	return len(p), nil
}

// warnLimiter rate-limits cloudflared warnings and errors per message: on a
// network that blocks UDP, cloudflared repeats the same QUIC dial failure
// every few seconds. It is process-wide so supervisor restarts don't reset it.
var warnLimiter = &logLimiter{every: time.Minute, now: time.Now}

// logLimiter lets each distinct message through at most once per every and
// counts what it dropped in between.
type logLimiter struct {
	every time.Duration
	now   func() time.Time

	mu   sync.Mutex
	seen map[string]*limitEntry
}

type limitEntry struct {
	last       time.Time
	suppressed int
}

const maxLimiterEntries = 256

// allow reports whether msg may be logged now and, if so, how many copies
// were suppressed since it was last logged.
func (l *logLimiter) allow(msg string) (bool, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if l.seen == nil {
		l.seen = map[string]*limitEntry{}
	}
	if e := l.seen[msg]; e != nil {
		if now.Sub(e.last) < l.every {
			e.suppressed++
			return false, 0
		}
		n := e.suppressed
		e.last, e.suppressed = now, 0
		return true, n
	}
	if len(l.seen) >= maxLimiterEntries { // forget stale messages
		for k, e := range l.seen {
			if now.Sub(e.last) >= l.every {
				delete(l.seen, k)
			}
		}
		if len(l.seen) >= maxLimiterEntries {
			return true, 0 // too many distinct messages: don't track
		}
	}
	l.seen[msg] = &limitEntry{last: now}
	return true, 0
}

// headerKey reports a cloudflared log field that carries HTTP headers or a
// client's address, dropped whole.
func headerKey(k string) bool {
	k = strings.ToLower(k)
	for _, w := range []string{"header", "cookie", "forwarded", "connecting-ip", "connecting_ip", "client", "remote", "authorization"} {
		if strings.Contains(k, w) {
			return true
		}
	}
	return false
}

// hasHeaderKey reports a nested value (a map or list in a field) holding a
// header or address key: the whole field is dropped.
func hasHeaderKey(v any) bool {
	switch v := v.(type) {
	case map[string]any:
		for k, x := range v {
			if headerKey(k) || hasHeaderKey(x) {
				return true
			}
		}
	case []any:
		for _, x := range v {
			if hasHeaderKey(x) {
				return true
			}
		}
	}
	return false
}

// ipCandidate finds runs of text that may hold an IP address (IPv4, IPv6,
// with or without a port); maskIPs decides.
var ipCandidate = regexp.MustCompile(`[0-9A-Fa-f:.]*[0-9A-Fa-f][:.][0-9A-Fa-f:.]*[0-9A-Fa-f:]`)

// dottedQuad is any a.b.c.d of 1-3 digit numbers, leading zeros included
// (netip refuses those, but a reader sees the address), glued to a word in
// front or not.
var dottedQuad = regexp.MustCompile(`\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b`)

// redact masks every IP address in s: cloudflared's own addresses are
// Cloudflare's edge, but a message or error can quote the visitor's, or
// this machine's own (public on a host without NAT).
func redact(s string) string {
	return ipCandidate.ReplaceAllStringFunc(dottedQuad.ReplaceAllString(s, "[ip]"), maskIPs)
}

// maxIPLen bounds an address with its port ("[" and "]" are outside the
// runs): a full IPv6 address and ":65535".
const maxIPLen = 45 + 6

// maskIPs masks every address inside c, a run of hex digits, dots and
// colons. A word or key can be glued to it ("id:1.2.3.4", "abc1.2.3.4"), so
// every start is tried, the longest address first; an address that starts
// inside one found and ends past it is masked with it (a hex word glued to
// a full IPv6 address makes the first match end early).
func maskIPs(c string) string {
	var b strings.Builder
	for i := 0; i < len(c); {
		if j := ipEnd(c, i); j > i {
			for k := i + 1; k < j; k++ {
				j = max(j, ipEnd(c, k))
			}
			b.WriteString("[ip]")
			i = j
			continue
		}
		b.WriteByte(c[i])
		i++
	}
	return b.String()
}

// ipEnd is the end of the longest address (or address:port) at c[i:], or
// i for none.
func ipEnd(c string, i int) int {
	for j := min(len(c), i+maxIPLen); j >= i+3; j-- {
		x := c[i:j]
		if !strings.ContainsAny(x, ".:") {
			break
		}
		if _, err := netip.ParseAddr(x); err == nil {
			return j
		}
		if _, err := netip.ParseAddrPort(x); err == nil {
			return j
		}
	}
	return i
}

// redactWriter masks IP addresses in what the standard log package writes:
// cloudflared's HTTP/2 server and quic-go log through it, not through the
// zerolog bridge.
type redactWriter struct{ w io.Writer }

func (r redactWriter) Write(p []byte) (int, error) {
	if _, err := r.w.Write([]byte(redact(string(p)))); err != nil {
		return 0, err
	}
	return len(p), nil
}
