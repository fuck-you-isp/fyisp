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

// Finding 6: when the request that fills a cache entry goes away, the
// requests waiting on the same entry still get the answer.
func TestPublicCacheLeaderDisconnect(t *testing.T) {
	_, d, _ := fixture(t, time.Now())
	bs := &blockingStore{Reader: d.Store, started: make(chan struct{}), release: make(chan struct{})}
	d.Store = bs
	h := Public(d, testSecret)
	u := "/s/" + testSecret + "/api/panel?group=common&from=now-30m&to=now&points=500"

	ctx, cancel := context.WithCancel(context.Background())
	leaderDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		r := httptest.NewRequest("GET", u, nil).WithContext(ctx)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		leaderDone <- w
	}()
	select {
	case <-bs.started:
	case w := <-leaderDone:
		t.Fatalf("leader returned without querying: %d %s", w.Code, w.Body.String())
	}
	followerDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", u, nil))
		followerDone <- w
	}()
	time.Sleep(50 * time.Millisecond) // let the follower join the entry
	cancel()                          // the leader's client disconnects
	time.Sleep(50 * time.Millisecond)
	close(bs.release)
	var w *httptest.ResponseRecorder
	select {
	case w = <-followerDone:
	case <-time.After(30 * time.Second):
		t.Fatal("follower did not return")
	}
	if w.Code != 200 {
		t.Errorf("follower after leader disconnect: %d %s", w.Code, w.Body.String())
	}
	<-leaderDone
}

// Finding 6: a canceled or timed-out query is 503, not 500.
func TestPanelCanceledIs503(t *testing.T) {
	_, d, _ := fixture(t, time.Now())
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
