package main

import (
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
