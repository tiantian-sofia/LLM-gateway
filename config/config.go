package config

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
)


type Config struct {
	Listen                string                    `json:"listen"`
	Strategy              string                    `json:"strategy"`
	Providers             map[string]ProviderConfig `json:"providers"`
	Models                map[string]ModelEntry     `json:"models"`
	HealthCheck           HealthCheck               `json:"health_check"`
	RequestTimeoutSeconds int                       `json:"request_timeout_seconds"`
	Database              *DatabaseConfig           `json:"database,omitempty"`
}

type DatabaseConfig struct {
	DSN string `json:"dsn"`
}

type ModelEntry struct {
	Provider           string                     `json:"provider"`
	BackendModel       string                     `json:"backend_model,omitempty"`
	Fallback           string                     `json:"fallback,omitempty"`
	InputCostPerToken  float64                    `json:"input_cost_per_token,omitempty"`
	OutputCostPerToken float64                    `json:"output_cost_per_token,omitempty"`
	ExtraBody          map[string]json.RawMessage `json:"extra_body,omitempty"`
}

type ProviderConfig struct {
	APIKey   string          `json:"api_key"`
	Backends []BackendConfig `json:"backends"`
}

type BackendConfig struct {
	URL             string `json:"url"`
	HealthCheckPath string `json:"health_check_path"`
}

type HealthCheck struct {
	IntervalSeconds int `json:"interval_seconds"`
	TimeoutSeconds  int `json:"timeout_seconds"`
	CooldownSeconds int `json:"cooldown_seconds"`
}

func Load(path string) (*Config, error) {
	data, err := ioutil.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file: %w", err)
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}

	cfg.applyDefaults()

	if len(cfg.Providers) == 0 {
		return nil, fmt.Errorf("no providers configured")
	}
	for name, pc := range cfg.Providers {
		if pc.APIKey == "" {
			return nil, fmt.Errorf("provider %q: api_key is required", name)
		}
		if len(pc.Backends) == 0 {
			return nil, fmt.Errorf("provider %q: no backends configured", name)
		}
		for i, b := range pc.Backends {
			if b.URL == "" {
				return nil, fmt.Errorf("provider %q backend %d: url is required", name, i)
			}
		}
	}
	if len(cfg.Models) == 0 {
		return nil, fmt.Errorf("no models configured")
	}
	for model, entry := range cfg.Models {
		if _, ok := cfg.Providers[entry.Provider]; !ok {
			return nil, fmt.Errorf("model %q: references unknown provider %q", model, entry.Provider)
		}
		if entry.Fallback != "" {
			if _, ok := cfg.Models[entry.Fallback]; !ok {
				return nil, fmt.Errorf("model %q: fallback references unknown model %q", model, entry.Fallback)
			}
		}
	}
	if cfg.Strategy != "round-robin" && cfg.Strategy != "primary-backup" {
		return nil, fmt.Errorf("unknown strategy %q (use \"round-robin\" or \"primary-backup\")", cfg.Strategy)
	}

	return &cfg, nil
}

// LookupModel returns the model entry for a given model name.
func (c *Config) LookupModel(model string) (ModelEntry, bool) {
	e, ok := c.Models[model]
	return e, ok
}

func (c *Config) applyDefaults() {
	if c.Listen == "" {
		c.Listen = ":8080"
	}
	if c.Strategy == "" {
		c.Strategy = "round-robin"
	}
	if c.HealthCheck.IntervalSeconds == 0 {
		c.HealthCheck.IntervalSeconds = 10
	}
	if c.HealthCheck.TimeoutSeconds == 0 {
		c.HealthCheck.TimeoutSeconds = 3
	}
	if c.HealthCheck.CooldownSeconds == 0 {
		c.HealthCheck.CooldownSeconds = 30
	}
	if c.RequestTimeoutSeconds == 0 {
		c.RequestTimeoutSeconds = 15
	}
}
