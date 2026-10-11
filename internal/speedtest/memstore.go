package speedtest

import (
	"context"
	"sync"
	"time"

	"github.com/fuck-you-isp/fyisp/internal/model"
)

// MemStore is an in-memory Store (tests and cmd/fyisp-webdev).
type MemStore struct {
	mu       sync.Mutex
	tests    []model.SpeedTest
	settings map[string]string
}

// SaveSpeedTest implements Store.
func (m *MemStore) SaveSpeedTest(_ context.Context, t *model.SpeedTest) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	t.ID = int64(len(m.tests) + 1)
	m.tests = append(m.tests, *t)
	return nil
}

// SetSpeedHandoff implements HandoffStore.
func (m *MemStore) SetSpeedHandoff(_ context.Context, id int64, h *model.Handoff) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.tests {
		if m.tests[i].ID == id {
			m.tests[i].Handoff = h
		}
	}
	return nil
}

// SpeedTests implements Store (in saving order).
func (m *MemStore) SpeedTests(_ context.Context, from, to time.Time) ([]model.SpeedTest, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []model.SpeedTest
	for _, t := range m.tests {
		if !t.Start.Before(from) && !t.Start.After(to) {
			out = append(out, t)
		}
	}
	return out, nil
}

// BytesSince is the data the tests started since t used.
func (m *MemStore) BytesSince(_ context.Context, t time.Time) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int64
	for _, x := range m.tests {
		if !x.Start.Before(t) {
			n += x.Bytes
		}
	}
	return n, nil
}

// Setting implements Store.
func (m *MemStore) Setting(_ context.Context, k string) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.settings[k]
	return v, ok, nil
}

// SetSetting implements Store.
func (m *MemStore) SetSetting(_ context.Context, k, v string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.settings == nil {
		m.settings = map[string]string{}
	}
	m.settings[k] = v
	return nil
}
