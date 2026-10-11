package web

import (
	"context"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/store"
)

// blockingStore's Panel waits for release or ctx.
type blockingStore struct {
	store.Reader
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingStore) Panel(ctx context.Context, q store.PanelQuery) (*store.PanelResult, error) {
	b.once.Do(func() { close(b.started) })
	select {
	case <-b.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return b.Reader.Panel(ctx, q)
}

// Finding 6: a canceled or timed-out query is 503, not 500.
func TestPanelCanceledIs503(t *testing.T) {
	_, d := fixture(t, time.Now())
	bs := &blockingStore{Reader: d.Store, started: make(chan struct{}), release: make(chan struct{})}
	d.Store = bs
	h := Local(d, LocalOptions{Addr: "127.0.0.1:3000"})
	ctx, cancel := context.WithCancel(context.Background())
	r := httptest.NewRequest("GET", "/api/panel?group=common", nil).WithContext(ctx)
	r.Host = "127.0.0.1:3000"
	go func() { <-bs.started; cancel() }()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 503 {
		t.Errorf("canceled query: %d, want 503", w.Code)
	}
}
