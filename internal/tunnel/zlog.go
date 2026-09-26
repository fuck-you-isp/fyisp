package tunnel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"sync/atomic"

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
	l.LogAttrs(context.Background(), lvl, msg, attrs...)
	return len(p), nil
}
