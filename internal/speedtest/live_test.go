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
	// Nobody listening: unreachable.
	if _, err := Run(ctx, Config{Addr: "127.0.0.1:1", Direction: Download}); !errors.Is(err, ErrUnreachable) {
		t.Errorf("closed port: %v", err)
	}
}
