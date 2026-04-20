package balancer

import (
	"sync/atomic"
	"time"

	"github.com/tiantian-sofia/LLM-gateway/backend"
)

// Cooldown used when checking if unhealthy backends should be retried.
// This is set by the proxy package at startup.
var Cooldown time.Duration

type RoundRobin struct {
	backends []*backend.Backend
	current  uint64
}

func NewRoundRobin(backends []*backend.Backend) *RoundRobin {
	return &RoundRobin{backends: backends}
}

func (rr *RoundRobin) Next() []*backend.Backend {
	n := len(rr.backends)
	idx := atomic.AddUint64(&rr.current, 1) - 1
	start := int(idx % uint64(n))

	// Build ordered candidate list: healthy first, then retryable
	var healthy []*backend.Backend
	var retryable []*backend.Backend

	for i := 0; i < n; i++ {
		b := rr.backends[(start+i)%n]
		if b.IsHealthy() {
			healthy = append(healthy, b)
		} else if b.ShouldRetry(Cooldown) {
			retryable = append(retryable, b)
		}
	}

	return append(healthy, retryable...)
}

func (rr *RoundRobin) HasHealthy() bool {
	for _, b := range rr.backends {
		if b.IsHealthy() {
			return true
		}
	}
	return false
}
