package speedtest

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

// TestStockServer runs both directions against a stock iperf3 server, e.g.
// a networkstatic/iperf3 container on a Docker network:
//
//	FYISP_IPERF3=iperf:5201 go test -run StockServer ./internal/speedtest
//
// It also checks that a second client is told the server is busy.
func TestStockServer(t *testing.T) {
	addr := os.Getenv("FYISP_IPERF3")
	if addr == "" {
		t.Skip("set FYISP_IPERF3=host:port of an iperf3 -s server")
	}
	ctx := context.Background()
	for _, d := range []Direction{Download, Upload} {
		r, err := Run(ctx, Config{Addr: addr, Direction: d, Duration: 3 * time.Second})
		if err != nil {
			t.Fatalf("%s: %v", d, err)
		}
		t.Logf("%s: %.0f Mbit/s, %d bytes in %v, retransmits %d", d, r.BitsPerSecond()/1e6, r.Bytes, r.Elapsed, r.Retransmits)
		if r.BitsPerSecond() < 1e6 || r.Elapsed < 2500*time.Millisecond || r.Elapsed > 4*time.Second {
			t.Errorf("%s: %+v", d, r)
		}
		if d == Download && r.Retransmits < 0 {
			t.Errorf("download: no retransmit count from the server")
		}
		time.Sleep(time.Second)
	}
	// Busy: start one test, then try a second while it runs. (iperf3 -s
	// closes its listener for a moment after each test: connects in that
	// window are refused, hence the pauses.)
	time.Sleep(time.Second)
	first := make(chan error, 1)
	go func() {
		_, err := Run(ctx, Config{Addr: addr, Direction: Download, Duration: 3 * time.Second, Streams: 2})
		first <- err
	}()
	time.Sleep(time.Second)
	if _, err := Run(ctx, Config{Addr: addr, Direction: Upload, Duration: 2 * time.Second}); !errors.Is(err, ErrBusy) {
		t.Errorf("second client: %v, want ErrBusy", err)
	}
	if err := <-first; err != nil {
		t.Errorf("first client: %v", err)
	}
	// Nobody listening: refused (retryable on another port).
	if _, err := Run(ctx, Config{Addr: "127.0.0.1:1", Direction: Download}); !errors.Is(err, ErrRefused) {
		t.Errorf("closed port: %v", err)
	}
}

// TestStockAdaptive compares the adaptive length (1 s omitted, 3-8 s,
// ending when a second is within 5% of the one before) with a full 10 s
// run (1 s omitted, 9 s), both directions; FYISP_IPERF3 as above.
func TestStockAdaptive(t *testing.T) {
	addr := os.Getenv("FYISP_IPERF3")
	if addr == "" {
		t.Skip("set FYISP_IPERF3=host:port of an iperf3 -s server")
	}
	ctx := context.Background()
	for _, d := range []Direction{Download, Upload} {
		time.Sleep(time.Second)
		full, err := Run(ctx, Config{Addr: addr, Direction: d})
		if err != nil {
			t.Fatalf("%s full: %v", d, err)
		}
		time.Sleep(time.Second)
		ad, err := Run(ctx, Config{Addr: addr, Direction: d, Duration: 8 * time.Second, MinDuration: 3 * time.Second})
		time.Sleep(time.Second)
		capped, err2 := Run(ctx, Config{Addr: addr, Direction: d, Duration: 8 * time.Second, MinDuration: 3 * time.Second, MaxBytes: 100 << 20})
		if err2 != nil {
			t.Fatalf("%s capped: %v", d, err2)
		}
		t.Logf("%s: capped at 100 MB: %.0f Mbit/s over %v after a %v ramp, %d MB in all, capped=%v", d,
			capped.BitsPerSecond()/1e6, capped.Elapsed.Round(time.Millisecond), capped.Ramp.Round(time.Millisecond), capped.TotalBytes>>20, capped.CappedByBytes)
		if err != nil {
			t.Fatalf("%s adaptive: %v", d, err)
		}
		diff := ad.BitsPerSecond()/full.BitsPerSecond() - 1
		t.Logf("%s: full %.0f Mbit/s over %v; adaptive %.0f Mbit/s over %v (%+.1f%%)", d,
			full.BitsPerSecond()/1e6, full.Elapsed.Round(time.Millisecond), ad.BitsPerSecond()/1e6, ad.Elapsed.Round(time.Millisecond), diff*100)
		if ad.Elapsed < 2900*time.Millisecond || ad.Elapsed > 8500*time.Millisecond {
			t.Errorf("%s adaptive window %v", d, ad.Elapsed)
		}
	}
}
