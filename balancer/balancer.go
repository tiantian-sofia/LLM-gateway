package balancer

import (
	"fmt"

	"github.com/tiantian-sofia/LLM-gateway/backend"
)

// LoadBalancer returns an ordered list of backends to try for a request.
// The caller tries them in order, stopping at the first success.
type LoadBalancer interface {
	Next() []*backend.Backend
	HasHealthy() bool
}

func New(strategy string, backends []*backend.Backend) (LoadBalancer, error) {
	switch strategy {
	case "round-robin":
		return NewRoundRobin(backends), nil
	case "primary-backup":
		return NewPrimaryBackup(backends), nil
	default:
		return nil, fmt.Errorf("unknown strategy: %s", strategy)
	}
}
