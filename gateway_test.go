package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tiantian-sofia/LLM-gateway/backend"
	"github.com/tiantian-sofia/LLM-gateway/balancer"
	"github.com/tiantian-sofia/LLM-gateway/config"
	"github.com/tiantian-sofia/LLM-gateway/proxy"
)

const testAPIKey = "sk-test-key"
const testModel = "test-model"

var testBody = `{"model":"` + testModel + `","messages":[{"role":"user","content":"hi"}]}`

func makeBackends(servers ...*httptest.Server) []*backend.Backend {
	var backends []*backend.Backend
	for _, s := range servers {
		b, _ := backend.New(config.BackendConfig{
			URL:             s.URL,
			HealthCheckPath: "/health",
		})
		backends = append(backends, b)
	}
	return backends
}

func makeGateway(lb balancer.LoadBalancer, apiKey string, requestTimeout, cooldown time.Duration) http.Handler {
	lbs := map[string]balancer.LoadBalancer{"openai": lb}
	apiKeys := map[string]string{"openai": apiKey}
	models := map[string]config.ModelEntry{
		testModel:                  {Provider: "openai"},
		"claude-sonnet-4-20250514": {Provider: "openai"},
	}
	return proxy.NewGateway(lbs, apiKeys, models, requestTimeout, cooldown)
}

func postTest(url string) (*http.Response, error) {
	return http.Post(url, "application/json", strings.NewReader(testBody))
}

func okHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

func TestRoundRobinDistribution(t *testing.T) {
	var counts [3]int64
	handler := func(idx int) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt64(&counts[idx], 1)
			w.WriteHeader(200)
			w.Write([]byte("ok"))
		}
	}

	s0 := httptest.NewServer(handler(0))
	s1 := httptest.NewServer(handler(1))
	s2 := httptest.NewServer(handler(2))
	defer s0.Close()
	defer s1.Close()
	defer s2.Close()

	backends := makeBackends(s0, s1, s2)
	lb := balancer.NewRoundRobin(backends)
	balancer.Cooldown = 30 * time.Second
	gw := makeGateway(lb, testAPIKey, 5*time.Second, 30*time.Second)
	gwServer := httptest.NewServer(gw)
	defer gwServer.Close()

	for i := 0; i < 9; i++ {
		resp, err := postTest(gwServer.URL + "/v1/chat/completions")
		if err != nil {
			t.Fatalf("request %d failed: %v", i, err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("request %d: expected 200, got %d", i, resp.StatusCode)
		}
	}

	for i, c := range counts {
		got := atomic.LoadInt64(&c)
		if got != 3 {
			t.Errorf("backend %d: expected 3 requests, got %d", i, got)
		}
	}
}

func TestPrimaryBackupPreference(t *testing.T) {
	var counts [2]int64
	handler := func(idx int) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt64(&counts[idx], 1)
			w.WriteHeader(200)
			w.Write([]byte("ok"))
		}
	}

	s0 := httptest.NewServer(handler(0))
	s1 := httptest.NewServer(handler(1))
	defer s0.Close()
	defer s1.Close()

	backends := makeBackends(s0, s1)
	lb := balancer.NewPrimaryBackup(backends)
	balancer.Cooldown = 30 * time.Second
	gw := makeGateway(lb, testAPIKey, 5*time.Second, 30*time.Second)
	gwServer := httptest.NewServer(gw)
	defer gwServer.Close()

	for i := 0; i < 5; i++ {
		resp, err := postTest(gwServer.URL + "/v1/chat/completions")
		if err != nil {
			t.Fatalf("request %d failed: %v", i, err)
		}
		resp.Body.Close()
	}

	if c := atomic.LoadInt64(&counts[0]); c != 5 {
		t.Errorf("primary: expected 5 requests, got %d", c)
	}
	if c := atomic.LoadInt64(&counts[1]); c != 0 {
		t.Errorf("backup: expected 0 requests, got %d", c)
	}
}

func TestFailoverOnServerError(t *testing.T) {
	s0 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
	}))
	s1 := httptest.NewServer(http.HandlerFunc(okHandler))
	defer s0.Close()
	defer s1.Close()

	backends := makeBackends(s0, s1)
	lb := balancer.NewPrimaryBackup(backends)
	balancer.Cooldown = 30 * time.Second
	gw := makeGateway(lb, testAPIKey, 5*time.Second, 30*time.Second)
	gwServer := httptest.NewServer(gw)
	defer gwServer.Close()

	resp, err := postTest(gwServer.URL + "/v1/chat/completions")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
}

func TestFailoverOnConnectionError(t *testing.T) {
	s0 := httptest.NewServer(http.HandlerFunc(okHandler))
	s1 := httptest.NewServer(http.HandlerFunc(okHandler))
	defer s1.Close()

	s0.Close()

	backends := makeBackends(s0, s1)
	lb := balancer.NewPrimaryBackup(backends)
	balancer.Cooldown = 30 * time.Second
	gw := makeGateway(lb, testAPIKey, 5*time.Second, 30*time.Second)
	gwServer := httptest.NewServer(gw)
	defer gwServer.Close()

	resp, err := postTest(gwServer.URL + "/v1/chat/completions")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
}

func TestAllBackendsDown(t *testing.T) {
	s0 := httptest.NewServer(http.HandlerFunc(okHandler))
	s1 := httptest.NewServer(http.HandlerFunc(okHandler))
	s0.Close()
	s1.Close()

	backends := makeBackends(s0, s1)
	lb := balancer.NewPrimaryBackup(backends)
	balancer.Cooldown = 30 * time.Second
	gw := makeGateway(lb, testAPIKey, 2*time.Second, 30*time.Second)
	gwServer := httptest.NewServer(gw)
	defer gwServer.Close()

	resp, err := postTest(gwServer.URL + "/v1/chat/completions")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 502 {
		t.Errorf("expected 502, got %d", resp.StatusCode)
	}
}

func TestBodyPreservation(t *testing.T) {
	var received string
	s0 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
	}))
	s1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := ioutil.ReadAll(r.Body)
		received = string(body)
		w.WriteHeader(200)
		w.Write([]byte("ok"))
	}))
	defer s0.Close()
	defer s1.Close()

	backends := makeBackends(s0, s1)
	lb := balancer.NewPrimaryBackup(backends)
	balancer.Cooldown = 30 * time.Second
	gw := makeGateway(lb, testAPIKey, 5*time.Second, 30*time.Second)
	gwServer := httptest.NewServer(gw)
	defer gwServer.Close()

	body := `{"model":"claude-sonnet-4-20250514","max_tokens":1024,"messages":[{"role":"user","content":"hello"}]}`
	resp, err := http.Post(gwServer.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
	if received != body {
		t.Errorf("body not preserved: expected %q, got %q", body, received)
	}
}

func TestPassiveHealthRecovery(t *testing.T) {
	var count int64
	s0 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&count, 1)
		w.WriteHeader(200)
		w.Write([]byte("ok"))
	}))
	defer s0.Close()

	backends := makeBackends(s0)
	backends[0].MarkDown()

	balancer.Cooldown = 100 * time.Millisecond
	lb := balancer.NewRoundRobin(backends)
	gw := makeGateway(lb, testAPIKey, 5*time.Second, 100*time.Millisecond)
	gwServer := httptest.NewServer(gw)
	defer gwServer.Close()

	time.Sleep(200 * time.Millisecond)

	resp, err := postTest(gwServer.URL + "/v1/chat/completions")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Errorf("expected 200 after cooldown recovery, got %d", resp.StatusCode)
	}
	if c := atomic.LoadInt64(&count); c != 1 {
		t.Errorf("expected 1 request to recovered backend, got %d", c)
	}
}

func TestAPIKeyInjection(t *testing.T) {
	var receivedAuth string
	s0 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		w.WriteHeader(200)
		w.Write([]byte("ok"))
	}))
	defer s0.Close()

	backends := makeBackends(s0)
	lb := balancer.NewRoundRobin(backends)
	balancer.Cooldown = 30 * time.Second
	gw := makeGateway(lb, testAPIKey, 5*time.Second, 30*time.Second)
	gwServer := httptest.NewServer(gw)
	defer gwServer.Close()

	resp, err := postTest(gwServer.URL + "/v1/chat/completions")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	resp.Body.Close()

	expected := "Bearer " + testAPIKey
	if receivedAuth != expected {
		t.Errorf("expected Authorization %q, got %q", expected, receivedAuth)
	}
}

func TestAPIKeyReplacesUserKey(t *testing.T) {
	var receivedAuth string
	s0 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		w.WriteHeader(200)
		w.Write([]byte("ok"))
	}))
	defer s0.Close()

	backends := makeBackends(s0)
	lb := balancer.NewRoundRobin(backends)
	balancer.Cooldown = 30 * time.Second
	gw := makeGateway(lb, testAPIKey, 5*time.Second, 30*time.Second)
	gwServer := httptest.NewServer(gw)
	defer gwServer.Close()

	req, _ := http.NewRequest("POST", gwServer.URL+"/v1/chat/completions", strings.NewReader(`{"model":"claude-sonnet-4-20250514","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sk-user-key-should-be-replaced")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	resp.Body.Close()

	expected := "Bearer " + testAPIKey
	if receivedAuth != expected {
		t.Errorf("expected key %q, got %q (user key was not replaced)", expected, receivedAuth)
	}
}

func TestStreamingPassthrough(t *testing.T) {
	s0 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n")
		flusher.Flush()
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer s0.Close()

	backends := makeBackends(s0)
	lb := balancer.NewRoundRobin(backends)
	balancer.Cooldown = 30 * time.Second
	gw := makeGateway(lb, testAPIKey, 5*time.Second, 30*time.Second)
	gwServer := httptest.NewServer(gw)
	defer gwServer.Close()

	resp, err := http.Post(gwServer.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"claude-sonnet-4-20250514","max_tokens":1024,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	body, _ := ioutil.ReadAll(resp.Body)
	if !strings.Contains(string(body), "hello") {
		t.Errorf("expected streamed content to contain 'hello', got: %s", body)
	}
	if !strings.Contains(string(body), "[DONE]") {
		t.Errorf("expected [DONE] in stream, got: %s", body)
	}
}

// TestBodilessRequestReturns400 verifies that requests with no body (e.g. favicon)
// get a 400 response instead of a 502.
func TestBodilessRequestReturns400(t *testing.T) {
	s0 := httptest.NewServer(http.HandlerFunc(okHandler))
	defer s0.Close()

	backends := makeBackends(s0)
	lb := balancer.NewRoundRobin(backends)
	balancer.Cooldown = 30 * time.Second
	gw := makeGateway(lb, testAPIKey, 5*time.Second, 30*time.Second)
	gwServer := httptest.NewServer(gw)
	defer gwServer.Close()

	resp, err := http.Get(gwServer.URL + "/favicon.ico")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 400 {
		t.Errorf("expected 400 for bodiless request, got %d", resp.StatusCode)
	}
}

// TestAnthropicToolUseRoundTrip sends an Anthropic-format request with tool_use and tool_result
// messages and verifies the backend receives properly converted OpenAI-format messages.
func TestAnthropicToolUseRoundTrip(t *testing.T) {
	var received map[string]json.RawMessage
	s0 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := ioutil.ReadAll(r.Body)
		json.Unmarshal(body, &received)
		// Respond with a tool_calls response.
		resp := `{
			"id": "chatcmpl-abc",
			"object": "chat.completion",
			"model": "test-model",
			"choices": [{
				"index": 0,
				"message": {
					"role": "assistant",
					"tool_calls": [{
						"id": "call_123",
						"type": "function",
						"function": {"name": "Read", "arguments": "{\"file_path\":\"/tmp/test.go\"}"}
					}]
				},
				"finish_reason": "tool_calls"
			}],
			"usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}
		}`
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.Write([]byte(resp))
	}))
	defer s0.Close()

	backends := makeBackends(s0)
	lb := balancer.NewRoundRobin(backends)
	balancer.Cooldown = 30 * time.Second
	gw := makeGateway(lb, testAPIKey, 5*time.Second, 30*time.Second)
	gwServer := httptest.NewServer(gw)
	defer gwServer.Close()

	// Anthropic-format request with tool_use in assistant message and tool_result in user message.
	anthropicBody := `{
		"model": "test-model",
		"max_tokens": 1024,
		"messages": [
			{"role": "user", "content": "read the file"},
			{"role": "assistant", "content": [
				{"type": "text", "text": "I'll read that file."},
				{"type": "tool_use", "id": "toolu_abc", "name": "Read", "input": {"file_path": "/tmp/test.go"}}
			]},
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "toolu_abc", "content": "package main\nfunc main() {}"}
			]}
		],
		"tools": [
			{"name": "Read", "description": "Reads a file", "input_schema": {"type": "object", "properties": {"file_path": {"type": "string"}}, "required": ["file_path"]}}
		]
	}`

	resp, err := http.Post(gwServer.URL+"/v1/messages", "application/json", strings.NewReader(anthropicBody))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := ioutil.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	// Verify the backend received OpenAI format.
	var msgs []json.RawMessage
	json.Unmarshal(received["messages"], &msgs)
	if len(msgs) < 3 {
		t.Fatalf("expected at least 3 messages forwarded to backend, got %d", len(msgs))
	}

	// Verify the response was converted back to Anthropic format.
	body, _ := ioutil.ReadAll(resp.Body)
	var anthropicResp map[string]interface{}
	if err := json.Unmarshal(body, &anthropicResp); err != nil {
		t.Fatalf("failed to parse anthropic response: %v", err)
	}
	if anthropicResp["type"] != "message" {
		t.Errorf("expected type 'message', got %v", anthropicResp["type"])
	}
	if anthropicResp["stop_reason"] != "tool_use" {
		t.Errorf("expected stop_reason 'tool_use', got %v", anthropicResp["stop_reason"])
	}

	content, ok := anthropicResp["content"].([]interface{})
	if !ok || len(content) == 0 {
		t.Fatalf("expected content array with tool_use, got %v", anthropicResp["content"])
	}

	// Find the tool_use block.
	found := false
	for _, c := range content {
		block, _ := c.(map[string]interface{})
		if block["type"] == "tool_use" {
			found = true
			if block["name"] != "Read" {
				t.Errorf("expected tool name 'Read', got %v", block["name"])
			}
		}
	}
	if !found {
		t.Errorf("no tool_use block found in response content: %s", body)
	}
}

// TestExtraBodyInjection verifies that extra_body fields from the model config
// are injected into the request forwarded to the backend.
func TestExtraBodyInjection(t *testing.T) {
	var received map[string]json.RawMessage
	s0 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := ioutil.ReadAll(r.Body)
		json.Unmarshal(body, &received)
		w.WriteHeader(200)
		w.Write([]byte(`{"id":"chatcmpl-1","model":"test-model","choices":[{"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer s0.Close()

	backends := makeBackends(s0)
	lb := balancer.NewRoundRobin(backends)
	balancer.Cooldown = 30 * time.Second

	// Configure model with extra_body fields (like thinking config for Gemini).
	lbs := map[string]balancer.LoadBalancer{"openai": lb}
	apiKeys := map[string]string{"openai": testAPIKey}
	models := map[string]config.ModelEntry{
		testModel: {
			Provider: "openai",
			ExtraBody: map[string]json.RawMessage{
				"thinking":              json.RawMessage(`{"type":"enabled","budget_tokens":8192}`),
				"allowed_openai_params": json.RawMessage(`["thinking"]`),
			},
		},
	}
	gw := proxy.NewGateway(lbs, apiKeys, models, 5*time.Second, 30*time.Second)
	gwServer := httptest.NewServer(gw)
	defer gwServer.Close()

	resp, err := postTest(gwServer.URL + "/v1/chat/completions")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	// Verify the extra fields were injected into the forwarded request.
	if _, ok := received["thinking"]; !ok {
		t.Errorf("expected 'thinking' field in forwarded request, got keys: %v", mapKeys(received))
	}
	if _, ok := received["allowed_openai_params"]; !ok {
		t.Errorf("expected 'allowed_openai_params' field in forwarded request, got keys: %v", mapKeys(received))
	}

	// Verify the thinking value is correct.
	var thinking struct {
		Type        string `json:"type"`
		BudgetTokens int   `json:"budget_tokens"`
	}
	if err := json.Unmarshal(received["thinking"], &thinking); err != nil {
		t.Fatalf("failed to parse thinking field: %v", err)
	}
	if thinking.Type != "enabled" || thinking.BudgetTokens != 8192 {
		t.Errorf("unexpected thinking config: %+v", thinking)
	}
}

func mapKeys(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// TestAnthropicStreamingConversion verifies that streaming responses from the backend
// are converted from OpenAI SSE format to Anthropic SSE format.
func TestAnthropicStreamingConversion(t *testing.T) {
	s0 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, "data: {\"id\":\"chatcmpl-1\",\"model\":\"test-model\",\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"\"}}]}\n\n")
		flusher.Flush()
		fmt.Fprint(w, "data: {\"id\":\"chatcmpl-1\",\"model\":\"test-model\",\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}\n\n")
		flusher.Flush()
		fmt.Fprint(w, "data: {\"id\":\"chatcmpl-1\",\"model\":\"test-model\",\"choices\":[{\"delta\":{\"content\":\" world\"}}]}\n\n")
		flusher.Flush()
		fmt.Fprint(w, "data: {\"id\":\"chatcmpl-1\",\"model\":\"test-model\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2,\"total_tokens\":7}}\n\n")
		flusher.Flush()
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer s0.Close()

	backends := makeBackends(s0)
	lb := balancer.NewRoundRobin(backends)
	balancer.Cooldown = 30 * time.Second
	gw := makeGateway(lb, testAPIKey, 5*time.Second, 30*time.Second)
	gwServer := httptest.NewServer(gw)
	defer gwServer.Close()

	anthropicBody := `{"model":"test-model","max_tokens":1024,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(gwServer.URL+"/v1/messages", "application/json", strings.NewReader(anthropicBody))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := ioutil.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	body, _ := ioutil.ReadAll(resp.Body)
	output := string(body)

	// Verify Anthropic SSE event structure.
	if !strings.Contains(output, "event: message_start") {
		t.Errorf("missing message_start event")
	}
	if !strings.Contains(output, "event: content_block_start") {
		t.Errorf("missing content_block_start event")
	}
	if !strings.Contains(output, "text_delta") {
		t.Errorf("missing text_delta in content")
	}
	if !strings.Contains(output, "Hello") {
		t.Errorf("missing 'Hello' text content")
	}
	if !strings.Contains(output, " world") {
		t.Errorf("missing ' world' text content")
	}
	if !strings.Contains(output, "event: content_block_stop") {
		t.Errorf("missing content_block_stop event")
	}
	if !strings.Contains(output, "event: message_delta") {
		t.Errorf("missing message_delta event")
	}
	if !strings.Contains(output, "end_turn") {
		t.Errorf("missing end_turn stop_reason")
	}
	if !strings.Contains(output, "event: message_stop") {
		t.Errorf("missing message_stop event")
	}
}

// TestImageResponseConversion verifies that image responses from the backend
// (OpenAI format with "images" field) are converted to Anthropic image content blocks.
func TestImageResponseConversion(t *testing.T) {
	// Mock backend that returns an image in the OpenAI "images" field.
	s0 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.Write([]byte(`{
			"id":"chatcmpl-img1",
			"model":"test-model",
			"object":"chat.completion",
			"choices":[{
				"index":0,
				"message":{
					"role":"assistant",
					"content":null,
					"images":[{"image_url":{"url":"data:image/jpeg;base64,/9j/4AAQfakedata"}}]
				},
				"finish_reason":"stop"
			}],
			"usage":{"prompt_tokens":5,"completion_tokens":100,"total_tokens":105}
		}`))
	}))
	defer s0.Close()

	backends := makeBackends(s0)
	lb := balancer.NewRoundRobin(backends)
	balancer.Cooldown = 30 * time.Second
	gw := makeGateway(lb, testAPIKey, 5*time.Second, 30*time.Second)
	gwServer := httptest.NewServer(gw)
	defer gwServer.Close()

	// Send Anthropic-format request (like Cherry Studio would).
	anthropicBody := `{"model":"test-model","max_tokens":1024,"messages":[{"role":"user","content":"Generate a flower image"}]}`
	resp, err := http.Post(gwServer.URL+"/v1/messages", "application/json", strings.NewReader(anthropicBody))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	body, _ := ioutil.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	// Parse the Anthropic response.
	var result struct {
		ID      string `json:"id"`
		Type    string `json:"type"`
		Role    string `json:"role"`
		Content []struct {
			Type   string `json:"type"`
			Source *struct {
				Type      string `json:"type"`
				MediaType string `json:"media_type"`
				Data      string `json:"data"`
			} `json:"source,omitempty"`
		} `json:"content"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("failed to parse response: %v\nbody: %s", err, body)
	}

	if len(result.Content) == 0 {
		t.Fatalf("expected non-empty content array, got: %s", body)
	}

	// Find the image content block.
	found := false
	for _, block := range result.Content {
		if block.Type == "image" {
			found = true
			if block.Source == nil {
				t.Fatal("image block has nil source")
			}
			if block.Source.Type != "base64" {
				t.Errorf("expected source type 'base64', got %q", block.Source.Type)
			}
			if block.Source.MediaType != "image/jpeg" {
				t.Errorf("expected media_type 'image/jpeg', got %q", block.Source.MediaType)
			}
			if block.Source.Data != "/9j/4AAQfakedata" {
				t.Errorf("expected base64 data '/9j/4AAQfakedata', got %q", block.Source.Data)
			}
		}
	}
	if !found {
		t.Errorf("no image content block found in response: %s", body)
	}

	// Verify usage is preserved.
	if result.Usage.InputTokens != 5 {
		t.Errorf("expected 5 input tokens, got %d", result.Usage.InputTokens)
	}
	if result.Usage.OutputTokens != 100 {
		t.Errorf("expected 100 output tokens, got %d", result.Usage.OutputTokens)
	}
}
