package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func resetDiscoveryCache(t *testing.T) {
	t.Helper()
	discoveredModels.mu.Lock()
	ids, metadata := discoveredModels.ids, discoveredModels.metadata
	fetchedAt, attemptedAt := discoveredModels.fetchedAt, discoveredModels.attemptedAt
	lastError, failures := discoveredModels.lastError, discoveredModels.failures
	discoveredModels.ids, discoveredModels.metadata = nil, nil
	discoveredModels.fetchedAt, discoveredModels.attemptedAt = time.Time{}, time.Time{}
	discoveredModels.lastError, discoveredModels.failures = "", 0
	discoveredModels.mu.Unlock()
	t.Cleanup(func() {
		discoveredModels.mu.Lock()
		defer discoveredModels.mu.Unlock()
		discoveredModels.ids, discoveredModels.metadata = ids, metadata
		discoveredModels.fetchedAt, discoveredModels.attemptedAt = fetchedAt, attemptedAt
		discoveredModels.lastError, discoveredModels.failures = lastError, failures
	})
}

func TestDiscoveryPricesProtocolsAndCache(t *testing.T) {
	resetDiscoveryCache(t)
	modelIDs := []string{"limited-preview", "mimo-new-free", "paid-free", "messages-free", "unknown-free"}
	failCatalog := false
	failDocs := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/models":
			if r.Header.Get("Authorization") != "Bearer chosen-key" {
				t.Error("missing model-list credential")
			}
			data := make([]map[string]string, 0)
			for _, id := range modelIDs {
				data = append(data, map[string]string{"id": id})
			}
			json.NewEncoder(w).Encode(map[string]any{"data": data})
		case "/catalog":
			if r.Header.Get("Authorization") != "" {
				t.Error("key sent to public metadata service")
			}
			if failCatalog {
				http.Error(w, "catalog unavailable", 503)
				return
			}
			fmt.Fprint(w, `{"opencode":{"npm":"@ai-sdk/openai-compatible","models":{
			"limited-preview":{"cost":{"input":0,"output":0},"limit":{"context":123456,"output":2048},"reasoning":true,"reasoning_options":[{"type":"effort","values":["low","medium","high","xhigh"]}]},
			"mimo-new-free":{"cost":{"input":0,"output":0}},
			"paid-free":{"cost":{"input":1,"output":2}},
			"messages-free":{"cost":{"input":0,"output":0},"provider":{"npm":"@ai-sdk/anthropic"}}
			}}}`)
		case "/docs":
			if failDocs {
				http.NotFound(w, r)
				return
			}
			fmt.Fprint(w, "| Temporary | limited-preview | `https://opencode.ai/inference/v1/responses` |\n| MiMo | mimo-new-free | `https://opencode.ai/inference/v1/chat/completions` |")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	originalCatalog, originalDocs := catalogURL, endpointDocsURL
	catalogURL, endpointDocsURL = server.URL+"/catalog", server.URL+"/docs"
	t.Cleanup(func() { catalogURL, endpointDocsURL = originalCatalog, originalDocs })
	cfg := defaultPluginConfig()
	cfg.BaseURL = server.URL
	resetConfig(t, cfg)
	if err := pullModels(server.URL, "chosen-key", "direct", "", true); err != nil {
		t.Fatal(err)
	}
	if got := discoveredZenModels(cfg); !reflect.DeepEqual(got, []string{"limited-preview", "mimo-new-free"}) {
		t.Fatalf("discovered %v", got)
	}
	route, _ := routeForModel(cfg, "opencode/limited-preview")
	if route.Endpoint != "responses" {
		t.Fatalf("endpoint=%s", route.Endpoint)
	}
	caps := resolveModelCapabilities(route)
	if caps.ContextLength != 123456 || caps.MaxCompletionTokens != 2048 || caps.Thinking == nil {
		t.Fatalf("capabilities=%+v", caps)
	}
	if !reflect.DeepEqual(caps.ReasoningLevels, []string{"low", "medium", "high", "xhigh"}) || !reflect.DeepEqual(caps.Thinking.Levels, caps.ReasoningLevels) {
		t.Fatalf("discovered reasoning levels = %+v", caps)
	}
	root := map[string]any{"reasoning_effort": "max"}
	normalizeReasoningEffort(root, "responses", "limited-preview")
	if root["reasoning"].(map[string]any)["effort"] != "medium" {
		t.Fatalf("live effort adaptation = %v", root)
	}
	cfg.Models = []modelRoute{{Model: "limited-preview", Alias: "public"}}
	route, _ = routeForModel(cfg, "opencode/public")
	if route.Endpoint != "responses" {
		t.Fatal("alias must use the upstream model protocol")
	}
	cfg.Models[0].Endpoint = "chat"
	route, _ = routeForModel(cfg, "limited-preview")
	if route.Endpoint != "chat" {
		t.Fatal("explicit protocol must win")
	}
	failCatalog = true
	if err := pullModels(server.URL, "chosen-key", "direct", "", true); err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("error=%v", err)
	}
	if !reflect.DeepEqual(discoveredZenModels(loadedConfig()), []string{"limited-preview", "mimo-new-free"}) {
		t.Fatal("failed refresh destroyed cache")
	}
	failCatalog = false
	failDocs = true
	if err := pullModels(server.URL, "chosen-key", "direct", "", true); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("docs error=%v", err)
	}
	if !reflect.DeepEqual(discoveredZenModels(loadedConfig()), []string{"limited-preview", "mimo-new-free"}) {
		t.Fatal("failed docs refresh destroyed cache")
	}
	failDocs = false
	modelIDs = []string{"mimo-new-free"}
	if err := pullModels(server.URL, "chosen-key", "direct", "", true); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(discoveredZenModels(loadedConfig()), []string{"mimo-new-free"}) {
		t.Fatal("removed upstream model was retained")
	}
}

func TestDiscoveryFailureLimitsAndManualRecovery(t *testing.T) {
	resetDiscoveryCache(t)
	cfg := defaultPluginConfig()
	resetConfig(t, cfg)
	var calls atomic.Int32
	fail := true
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if fail {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		switch r.URL.Path {
		case "/models":
			fmt.Fprint(w, `{"data":[{"id":"new-free"}]}`)
		case "/catalog":
			fmt.Fprint(w, `{"opencode":{"npm":"@ai-sdk/openai-compatible","models":{"new-free":{"cost":{"input":0,"output":0}}}}}`)
		case "/docs":
			fmt.Fprint(w, "| New | new-free | `https://opencode.ai/inference/v1/chat/completions` |")
		}
	}))
	defer server.Close()
	cfg.BaseURL = server.URL
	storeConfig(cfg)
	originalCatalog, originalDocs := catalogURL, endpointDocsURL
	catalogURL, endpointDocsURL = server.URL+"/catalog", server.URL+"/docs"
	t.Cleanup(func() { catalogURL, endpointDocsURL = originalCatalog, originalDocs })
	originalHost := credentialHost
	var hostCalls atomic.Int32
	credentialHost = func(method string, _ any) (json.RawMessage, error) {
		hostCalls.Add(1)
		if method == "host.auth.list" {
			return json.RawMessage(`{"files":[{"provider":"zen","auth_index":"chosen"}]}`), nil
		}
		if method == "host.auth.get" {
			return json.RawMessage(`{"json":{"api_key":"chosen-key","proxy_url":"direct"}}`), nil
		}
		return nil, fmt.Errorf("unexpected host method %s", method)
	}
	t.Cleanup(func() { credentialHost = originalHost })
	discoveredModels.ids = []string{"cached-free"}
	for attempt := 1; attempt <= maxModelRefreshFailures; attempt++ {
		var workers sync.WaitGroup
		for range 8 {
			workers.Add(2)
			go func() {
				defer workers.Done()
				refreshDiscoveredModels(cfg.BaseURL, "chosen-key", "direct")
			}()
			go func() {
				defer workers.Done()
				discoveredZenModels(cfg)
			}()
		}
		workers.Wait()
		if got := calls.Load(); got != int32(attempt) {
			t.Fatalf("attempt %d: upstream calls = %d", attempt, got)
		}
		discoveredModels.mu.Lock()
		failures, ids := discoveredModels.failures, discoveredModels.ids
		discoveredModels.attemptedAt = time.Now().Add(-modelRefreshInterval)
		discoveredModels.mu.Unlock()
		if failures != attempt || !reflect.DeepEqual(ids, []string{"cached-free"}) {
			t.Fatalf("failures = %d, cache = %v", failures, ids)
		}
	}
	before := hostCalls.Load()
	discoveredZenModels(cfg)
	refreshDiscoveredModels(cfg.BaseURL, "chosen-key", "direct")
	if calls.Load() != maxModelRefreshFailures || hostCalls.Load() != before {
		t.Fatal("automatic discovery continued after the failure limit")
	}
	if message := panelModels()["error"].(string); !strings.Contains(message, "自动拉取已暂停") {
		t.Fatalf("missing pause reason: %s", message)
	}
	fail = false
	if err := refreshModelsFromHost(cfg, "", true); err != nil {
		t.Fatal(err)
	}
	if discoveredModels.failures != 0 || discoveredModels.lastError != "" || !reflect.DeepEqual(discoveredZenModels(cfg), []string{"new-free"}) {
		t.Fatal("manual discovery did not recover")
	}
	// After expiry, automatic discovery resumes following a successful manual pull.
	discoveredModels.mu.Lock()
	discoveredModels.fetchedAt, discoveredModels.attemptedAt = time.Now().Add(-modelRefreshInterval), time.Now().Add(-modelRefreshInterval)
	discoveredModels.mu.Unlock()
	discoveredZenModels(cfg)
	if got := calls.Load(); got != maxModelRefreshFailures+6 {
		t.Fatalf("calls after recovery = %d", got)
	}
}

func TestDiscoveryCredentialFailuresAreThrottled(t *testing.T) {
	resetDiscoveryCache(t)
	cfg := defaultPluginConfig()
	resetConfig(t, cfg)
	original := credentialHost
	calls := 0
	credentialHost = func(string, any) (json.RawMessage, error) {
		calls++
		return nil, fmt.Errorf("credential service unavailable")
	}
	t.Cleanup(func() { credentialHost = original })
	for range 5 {
		discoveredZenModels(cfg)
	}
	if calls != 1 || !strings.Contains(panelModels()["error"].(string), "credential service unavailable") {
		t.Fatalf("calls=%d, panel=%v", calls, panelModels())
	}
}
