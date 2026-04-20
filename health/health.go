package health

import (
	"log"
	"net/http"
	"time"

	"github.com/tiantian-sofia/LLM-gateway/backend"
)

type Checker struct {
	backends []*backend.Backend
	interval time.Duration
	client   *http.Client
	stop     chan struct{}
	done     chan struct{}
}

func NewChecker(backends []*backend.Backend, interval, timeout time.Duration) *Checker {
	return &Checker{
		backends: backends,
		interval: interval,
		client:   &http.Client{Timeout: timeout},
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
}

func (c *Checker) Start() {
	go c.run()
}

func (c *Checker) Stop() {
	close(c.stop)
	<-c.done
}

func (c *Checker) run() {
	defer close(c.done)

	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()

	// Run an initial check immediately
	c.checkAll()

	for {
		select {
		case <-ticker.C:
			c.checkAll()
		case <-c.stop:
			return
		}
	}
}

func (c *Checker) checkAll() {
	for _, b := range c.backends {
		if b.HealthCheckPath == "" {
			continue
		}
		c.check(b)
	}
}

func (c *Checker) check(b *backend.Backend) {
	url := b.URL.String() + b.HealthCheckPath
	resp, err := c.client.Get(url)
	if err != nil {
		log.Printf("[health] %s ping failed: %v", b.URL.Host, err)
		b.SetHealthy(false)
		return
	}
	resp.Body.Close()

	// Any non-5xx response means the host is reachable (4xx like 401 is expected
	// for API endpoints that require authentication on the health check path).
	if resp.StatusCode < 500 {
		log.Printf("[health] %s ping OK (status %d)", b.URL.Host, resp.StatusCode)
		b.SetHealthy(true)
	} else {
		log.Printf("[health] %s ping failed (status %d)", b.URL.Host, resp.StatusCode)
		b.SetHealthy(false)
	}
}
