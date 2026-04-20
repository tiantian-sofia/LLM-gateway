package balancer

import (
	"github.com/tiantian-sofia/LLM-gateway/backend"
)

type PrimaryBackup struct {
	backends []*backend.Backend
}

func NewPrimaryBackup(backends []*backend.Backend) *PrimaryBackup {
	return &PrimaryBackup{backends: backends}
}

func (pb *PrimaryBackup) Next() []*backend.Backend {
	var healthy []*backend.Backend
	var retryable []*backend.Backend

	for _, b := range pb.backends {
		if b.IsHealthy() {
			healthy = append(healthy, b)
		} else if b.ShouldRetry(Cooldown) {
			retryable = append(retryable, b)
		}
	}

	return append(healthy, retryable...)
}

func (pb *PrimaryBackup) HasHealthy() bool {
	for _, b := range pb.backends {
		if b.IsHealthy() {
			return true
		}
	}
	return false
}
