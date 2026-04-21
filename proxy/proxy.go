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
	"strings"
	"time"

	"github.com/tiantian-sofia/LLM-gateway/balancer"
	"github.com/tiantian-sofia/LLM-gateway/config"
	"github.com/tiantian-sofia/LLM-gateway/converter"
)

type contextKey string

const originalPathKey contextKey = "originalPath"

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

	// Store the original path so ModifyResponse can detect Anthropic callers.
	originalPath := req.URL.Path
	req = req.WithContext(context.WithValue(req.Context(), originalPathKey, originalPath))

	// Log incoming request.
	log.Printf("[proxy] >>> %s %s body=%s", req.Method, originalPath, truncateLog(bodyBytes, 2048))

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
		// Return a JSON error response instead of a transport error so the caller
		// gets a proper 400 rather than an opaque 502 (e.g. browser favicon, health probes).
		body := `{"type":"error","error":{"type":"invalid_request_error","message":"no model specified in request"}}`
		return &http.Response{
			StatusCode:    400,
			Status:        "400 Bad Request",
			Header:        http.Header{"Content-Type": {"application/json"}},
			Body:          ioutil.NopCloser(strings.NewReader(body)),
			ContentLength: int64(len(body)),
			Request:       req,
		}, nil
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

		// Rewrite the model name in the body if we're on a fallback or if
		// backend_model is configured (the backend knows the model by a different name).
		attemptBody := bodyBytes
		targetModel := currentModel
		if entry.BackendModel != "" {
			targetModel = entry.BackendModel
		}
		if targetModel != modelName {
			attemptBody = rewriteModel(bodyBytes, targetModel)
			if currentModel != modelName {
				log.Printf("[proxy] falling back from %q to %q", modelName, currentModel)
			}
		}

		// Inject any extra body fields configured for this model
		// (e.g. thinking config for Gemini thinking models).
		attemptBody = injectExtraBody(attemptBody, entry.ExtraBody)

		resp, err := t.tryProvider(req, attemptBody, entry.Provider)
		if err == nil {
			log.Printf("[proxy] %s model=%s provider=%s src=%s ua=%q -> %d",
				req.URL.Path, currentModel, entry.Provider, req.RemoteAddr, req.Header.Get("User-Agent"), resp.StatusCode)
			resp.Body = newCostTracker(resp.Body, currentModel, entry, req.RemoteAddr, req.Header.Get("User-Agent"))
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

// injectExtraBody merges extra fields from the model config into the request body.
// This allows per-model backend-specific parameters (e.g. thinking config for Gemini).
func injectExtraBody(body []byte, extra map[string]json.RawMessage) []byte {
	if len(extra) == 0 {
		return body
	}
	var raw map[string]json.RawMessage
	if json.Unmarshal(body, &raw) != nil {
		return body
	}
	for k, v := range extra {
		raw[k] = v
	}
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

// truncateLog returns body as a string, truncated to maxLen bytes with a
// trailing "...(truncated)" marker when the body exceeds the limit.
func truncateLog(body []byte, maxLen int) string {
	if len(body) <= maxLen {
		return string(body)
	}
	return string(body[:maxLen]) + "...(truncated)"
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
		ModifyResponse: func(resp *http.Response) error {
			origPath, _ := resp.Request.Context().Value(originalPathKey).(string)
			isStream := strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream")

			if strings.HasPrefix(origPath, "/v1/messages") {
				// Anthropic caller: convert OpenAI → Anthropic format.
				if isStream {
					log.Printf("[proxy] <<< %s %d (streaming)", origPath, resp.StatusCode)
					resp.Body = converter.NewStreamingAnthropicConverter(resp.Body)
					resp.Header.Set("Content-Type", "text/event-stream")
					resp.Header.Del("Content-Length")
					resp.ContentLength = -1
					return nil
				}

				body, err := ioutil.ReadAll(resp.Body)
				resp.Body.Close()
				if err != nil {
					return fmt.Errorf("reading response body for conversion: %w", err)
				}

				log.Printf("[proxy] <<< %s %d body=%s", origPath, resp.StatusCode, truncateLog(body, 2048))

				converted, err := converter.ConvertResponseToAnthropic(body)
				if err != nil {
					log.Printf("[proxy] response conversion failed, passing through: %v", err)
					converted = body
				}
				resp.Header.Set("Content-Type", "application/json")
				resp.Body = ioutil.NopCloser(bytes.NewReader(converted))
				resp.ContentLength = int64(len(converted))
				resp.Header.Set("Content-Length", fmt.Sprintf("%d", len(converted)))
				return nil

			} else if converter.IsGoogleAIPath(origPath) {
				// Google AI (Gemini) caller: convert OpenAI → Gemini format.
				if isStream {
					log.Printf("[proxy] <<< %s %d (streaming, gemini)", origPath, resp.StatusCode)
					resp.Body = converter.NewStreamingGeminiConverter(resp.Body)
					resp.Header.Set("Content-Type", "text/event-stream")
					resp.Header.Del("Content-Length")
					resp.ContentLength = -1
					return nil
				}

				body, err := ioutil.ReadAll(resp.Body)
				resp.Body.Close()
				if err != nil {
					return fmt.Errorf("reading response body for conversion: %w", err)
				}

				log.Printf("[proxy] <<< %s %d body=%s", origPath, resp.StatusCode, truncateLog(body, 2048))

				converted, err := converter.ConvertResponseToGoogleAI(body)
				if err != nil {
					log.Printf("[proxy] gemini response conversion failed, passing through: %v", err)
					converted = body
				}
				resp.Header.Set("Content-Type", "application/json")
				resp.Body = ioutil.NopCloser(bytes.NewReader(converted))
				resp.ContentLength = int64(len(converted))
				resp.Header.Set("Content-Length", fmt.Sprintf("%d", len(converted)))
				return nil
			}

			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("[proxy] 502: %v", err)
			http.Error(w, "Bad Gateway", http.StatusBadGateway)
		},
	}

	mux := http.NewServeMux()
	mux.Handle("/ui/costs", CostDashboardHandler())
	mux.Handle("/", proxy)

	return mux
}
