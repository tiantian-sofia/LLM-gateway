package backend

import (
	"fmt"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"sofia/gateway/config"
)

type Backend struct {
	URL             *url.URL
	HealthCheckPath string

	healthy   int32 // atomic: 1 = healthy, 0 = unhealthy
	downSince time.Time
	mu        sync.Mutex
}

func New(cfg config.BackendConfig) (*Backend, error) {
	u, err := url.Parse(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("parsing url %q: %w", cfg.URL, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("invalid url %q: must include scheme and host", cfg.URL)
	}

	b := &Backend{
		URL:             u,
		HealthCheckPath: cfg.HealthCheckPath,
	}
	b.SetHealthy(true)
	return b, nil
}

func (b *Backend) IsHealthy() bool {
	return atomic.LoadInt32(&b.healthy) == 1
}

func (b *Backend) SetHealthy(v bool) {
	if v {
		atomic.StoreInt32(&b.healthy, 1)
		b.mu.Lock()
		b.downSince = time.Time{}
		b.mu.Unlock()
	} else {
		atomic.StoreInt32(&b.healthy, 0)
		b.mu.Lock()
		if b.downSince.IsZero() {
			b.downSince = time.Now()
		}
		b.mu.Unlock()
	}
}

func (b *Backend) MarkDown() {
	b.SetHealthy(false)
}

// ShouldRetry returns true if the backend is unhealthy but the cooldown period
// has elapsed, meaning it should be given another chance.
func (b *Backend) ShouldRetry(cooldown time.Duration) bool {
	if b.IsHealthy() {
		return false
	}
	b.mu.Lock()
	ds := b.downSince
	b.mu.Unlock()
	return !ds.IsZero() && time.Since(ds) >= cooldown
}

func (b *Backend) String() string {
	return b.URL.String()
}
