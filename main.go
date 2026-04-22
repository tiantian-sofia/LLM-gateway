package main

import (
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/tiantian-sofia/LLM-gateway/backend"
	"github.com/tiantian-sofia/LLM-gateway/balancer"
	"github.com/tiantian-sofia/LLM-gateway/config"
	"github.com/tiantian-sofia/LLM-gateway/health"
	"github.com/tiantian-sofia/LLM-gateway/proxy"
	"github.com/tiantian-sofia/LLM-gateway/store"
)

func main() {
	cfgPath := flag.String("config", "config.json", "path to config file")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}

	lbs := make(map[string]balancer.LoadBalancer)
	apiKeys := make(map[string]string)
	var allBackends []*backend.Backend

	for name, pc := range cfg.Providers {
		var backends []*backend.Backend
		for _, bc := range pc.Backends {
			b, err := backend.New(bc)
			if err != nil {
				log.Fatalf("provider %q: invalid backend %q: %v", name, bc.URL, err)
			}
			backends = append(backends, b)
		}

		lb, err := balancer.New(cfg.Strategy, backends)
		if err != nil {
			log.Fatalf("provider %q: invalid strategy: %v", name, err)
		}

		lbs[name] = lb
		apiKeys[name] = pc.APIKey
		allBackends = append(allBackends, backends...)
	}

	// Initialize database for cost persistence (optional).
	if cfg.Database != nil && cfg.Database.DSN != "" {
		costStore, err := store.NewPostgresStore(cfg.Database.DSN)
		if err != nil {
			log.Printf("[db] warning: failed to connect to database, running without persistence: %v", err)
		} else {
			log.Printf("[db] connected to PostgreSQL, cost records will be persisted")
			proxy.SetCostStore(costStore)
			defer costStore.Close()
		}
	}

	// Start active health checker for all backends.
	hc := health.NewChecker(
		allBackends,
		time.Duration(cfg.HealthCheck.IntervalSeconds)*time.Second,
		time.Duration(cfg.HealthCheck.TimeoutSeconds)*time.Second,
	)
	hc.Start()

	// Create gateway handler.
	handler := proxy.NewGateway(
		lbs,
		apiKeys,
		cfg.Models,
		time.Duration(cfg.RequestTimeoutSeconds)*time.Second,
		time.Duration(cfg.HealthCheck.CooldownSeconds)*time.Second,
	)

	srv := &http.Server{
		Addr:    cfg.Listen,
		Handler: handler,
	}

	go func() {
		log.Printf("sofia gateway listening on %s", cfg.Listen)
		log.Printf("strategy: %s, providers: %d, models: %d", cfg.Strategy, len(cfg.Providers), len(cfg.Models))
		for name, pc := range cfg.Providers {
			log.Printf("  [%s] backends: %d", name, len(pc.Backends))
			for _, bc := range pc.Backends {
				log.Printf("    -> %s", bc.URL)
			}
		}
		for model, entry := range cfg.Models {
			if entry.Fallback != "" {
				log.Printf("  model %q -> provider %q (fallback: %q)", model, entry.Provider, entry.Fallback)
			} else {
				log.Printf("  model %q -> provider %q", model, entry.Provider)
			}
		}
		if err := srv.ListenAndServe(); err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()

	// Graceful shutdown
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("shutting down...")
	hc.Stop()
	srv.Close()
}
