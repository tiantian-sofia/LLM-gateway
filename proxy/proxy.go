package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/http/httputil"
	"time"

	"sofia/gateway/balancer"
	"sofia/gateway/config"
	"sofia/gateway/converter"
)

type FailoverTransport struct {
	lbs     map[string]balancer.LoadBalancer // keyed by provider name
	apiKeys map[string]string               // keyed by provider name
	models  map[string]config.ModelEntry     // model name → entry with provider + fallback
	timeout time.Duration
	base    http.RoundTripper
}

func (t *FailoverTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Buffer the request body so we can replay it on retries
	var bodyBytes []byte
	if req.Body != nil {
		var err error
		bodyBytes, err = ioutil.ReadAll(req.Body)
		req.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("reading request body: %w", err)
		}
	}

	// Convert request to OpenAI format and extract model name.
	var modelName string
	if len(bodyBytes) > 0 {
		result, err := converter.ConvertToOpenAI(bodyBytes, req.URL.Path)
		if err != nil {
			return nil, fmt.Errorf("converting request format: %w", err)
		}
		bodyBytes = result.Body
		req.URL.Path = result.Path
		modelName = result.Model
	}
	if modelName == "" {
		return nil, fmt.Errorf("no model specified in request")
	}

	// Try the model and its fallback chain.
	visited := map[string]bool{}
	currentModel := modelName
	var lastErr error

	for currentModel != "" {
		if visited[currentModel] {
			break // prevent cycles
		}
		visited[currentModel] = true

		entry, ok := t.models[currentModel]
		if !ok {
			return nil, fmt.Errorf("unknown model %q", currentModel)
		}

		// If this provider has no healthy backends and a fallback exists,
		// skip straight to the fallback instead of trying retryable (down) backends.
		lb := t.lbs[entry.Provider]
		if lb != nil && !lb.HasHealthy() && entry.Fallback != "" {
			log.Printf("[proxy] provider %q has no healthy backends, skipping to fallback %q", entry.Provider, entry.Fallback)
			currentModel = entry.Fallback
			continue
		}

		// Rewrite the model name in the body if we're on a fallback.
		attemptBody := bodyBytes
		if currentModel != modelName {
			attemptBody = rewriteModel(bodyBytes, currentModel)
			log.Printf("[proxy] falling back from %q to %q", modelName, currentModel)
		}

		resp, err := t.tryProvider(req, attemptBody, entry.Provider)
		if err == nil {
			resp.Body = newCostTracker(resp.Body, currentModel, entry)
			return resp, nil
		}
		lastErr = err
		currentModel = entry.Fallback
	}

	return nil, fmt.Errorf("all backends failed (including fallbacks): %w", lastErr)
}

// tryProvider attempts all backends for a given provider.
func (t *FailoverTransport) tryProvider(req *http.Request, bodyBytes []byte, provider string) (*http.Response, error) {
	lb, ok := t.lbs[provider]
	if !ok {
		return nil, fmt.Errorf("no backends configured for provider %q", provider)
	}
	apiKey := t.apiKeys[provider]

	backends := lb.Next()
	if len(backends) == 0 {
		return nil, fmt.Errorf("no available backends for provider %q", provider)
	}

	var lastErr error
	for _, b := range backends {
		clone := req.Clone(req.Context())
		u := *req.URL
		clone.URL = &u
		clone.URL.Scheme = b.URL.Scheme
		clone.URL.Host = b.URL.Host
		clone.Host = b.URL.Host

		setAuth(clone, provider, apiKey)

		if bodyBytes != nil {
			clone.Body = ioutil.NopCloser(bytes.NewReader(bodyBytes))
			clone.ContentLength = int64(len(bodyBytes))
		}

		ctx, cancel := context.WithTimeout(clone.Context(), t.timeout)
		clone = clone.WithContext(ctx)

		resp, err := t.base.RoundTrip(clone)

		if err != nil {
			cancel()
			log.Printf("[proxy] %s -> %s: error: %v", req.URL.Path, b.URL.Host, err)
			b.MarkDown()
			lastErr = err
			continue
		}

		if resp.StatusCode >= 500 {
			cancel()
			log.Printf("[proxy] %s -> %s: got %d, trying next", req.URL.Path, b.URL.Host, resp.StatusCode)
			resp.Body.Close()
			b.MarkDown()
			lastErr = fmt.Errorf("backend %s returned %d", b.URL.Host, resp.StatusCode)
			continue
		}

		b.SetHealthy(true)
		return resp, nil
	}

	return nil, lastErr
}

// rewriteModel replaces the "model" field in the JSON body with a new model name.
func rewriteModel(body []byte, newModel string) []byte {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return body
	}
	m, _ := json.Marshal(newModel)
	raw["model"] = m
	out, err := json.Marshal(raw)
	if err != nil {
		return body
	}
	return out
}

func setAuth(req *http.Request, provider, apiKey string) {
	switch provider {
	case "claude":
		req.Header.Set("x-api-key", apiKey)
		if req.Header.Get("anthropic-version") == "" {
			req.Header.Set("anthropic-version", "2023-06-01")
		}
	case "googleai":
		req.Header.Set("x-goog-api-key", apiKey)
	default:
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
}

// NewGateway creates an http.Handler that proxies requests with failover.
func NewGateway(lbs map[string]balancer.LoadBalancer, apiKeys map[string]string, models map[string]config.ModelEntry, requestTimeout, cooldown time.Duration) http.Handler {
	balancer.Cooldown = cooldown

	transport := &FailoverTransport{
		lbs:     lbs,
		apiKeys: apiKeys,
		models:  models,
		timeout: requestTimeout,
		base:    http.DefaultTransport,
	}

	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {},
		Transport:     transport,
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("[proxy] 502: %v", err)
			http.Error(w, "Bad Gateway", http.StatusBadGateway)
		},
	}

	return proxy
}
