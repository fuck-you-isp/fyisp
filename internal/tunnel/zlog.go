package tunnel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"
)

type slogLogger = slog.Logger

// LevelCloudflaredDebug is the slog level cloudflared's debug logs map to.
const LevelCloudflaredDebug = slog.LevelDebug - 4

// newZerolog returns a zerolog.Logger for cloudflared that forwards each
// event to the slog logger returned by get (nil: dropped). Levels are demoted
// so a default Info logger only sees cloudflared warnings and errors, as Warn:
// fyisp reports tunnel health through its own state changes. The zerolog level
// is set from what the slog logger has enabled, so cloudflared doesn't build
// per-request debug events nobody reads.
// While quiet is set, everything is logged at LevelCloudflaredDebug.
func newZerolog(get func() *slogLogger, quiet *atomic.Bool) zerolog.Logger {
	lvl := zerolog.WarnLevel
	if l := get(); l != nil {
		switch {
		case l.Enabled(context.Background(), LevelCloudflaredDebug):
			lvl = zerolog.DebugLevel
		case l.Enabled(context.Background(), slog.LevelDebug):
			lvl = zerolog.InfoLevel
		}
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
		return LevelCloudflaredDebug
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
		l.Debug(string(bytes.TrimSpace(p)), "component", "cloudflared")
		return len(p), nil
	}
	levelStr, _ := m["level"].(string)
	lvl := mapLevel(levelStr)
	if b.quiet != nil && b.quiet.Load() {
		lvl = LevelCloudflaredDebug
	}
	if !l.Enabled(context.Background(), lvl) {
		return len(p), nil
	}
	msg, _ := m["message"].(string)
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
		switch v := m[k].(type) {
		case string:
			attrs = append(attrs, slog.String(k, v))
		case json.Number:
			attrs = append(attrs, slog.String(k, v.String()))
		default:
			attrs = append(attrs, slog.String(k, fmt.Sprint(v)))
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
