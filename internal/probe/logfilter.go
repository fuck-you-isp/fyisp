package probe

import (
	"bytes"
	"io"
	"log"
)

// The HTTP/2 client in net/http logs every frame type it does not know
// (e.g. PRIORITY_UPDATE or ORIGIN sent by Google and Cloudflare) through the
// standard "log" package, with no per-Transport hook:
//
//	http2: Transport: unhandled response frame type *http2.UnknownFrame
//
// silenceHTTP2Noise wraps the current log output with a filter that drops
// those lines. It is called from New and Run, so it survives a later
// slog.SetDefault (which replaces the log output) as long as that happens
// before Run.
func silenceHTTP2Noise() {
	if _, ok := log.Writer().(*noiseFilter); ok {
		return
	}
	log.SetOutput(&noiseFilter{w: log.Writer()})
}

var noise = [][]byte{[]byte("unhandled response frame type")}

type noiseFilter struct{ w io.Writer }

func (f *noiseFilter) Write(b []byte) (int, error) {
	for _, n := range noise {
		if bytes.Contains(b, n) {
			return len(b), nil
		}
	}
	return f.w.Write(b)
}
