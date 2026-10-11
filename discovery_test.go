package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestDiscoveryPricesProtocolsAndCache(t *testing.T) {
	modelIDs := []string{"limited-preview", "mimo-new-free", "paid-free", "messages-free", "unknown-free"}
	failCatalog := false
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
			fmt.Fprint(w, "| Temporary | limited-preview | `https://opencode.ai/zen/v1/responses` |\n| MiMo | mimo-new-free | `https://opencode.ai/zen/v1/chat/completions` |")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	originalCatalog, originalDocs := catalogURL, endpointDocsURL
	catalogURL, endpointDocsURL = server.URL+"/catalog", server.URL+"/docs"
	t.Cleanup(func() { catalogURL, endpointDocsURL = originalCatalog, originalDocs })
	discoveredModels.mu.Lock()
	originalIDs, originalMetadata, originalTime, originalError := discoveredModels.ids, discoveredModels.metadata, discoveredModels.fetchedAt, discoveredModels.lastError
	discoveredModels.mu.Unlock()
	t.Cleanup(func() {
		discoveredModels.mu.Lock()
		defer discoveredModels.mu.Unlock()
		discoveredModels.ids, discoveredModels.metadata, discoveredModels.fetchedAt, discoveredModels.lastError = originalIDs, originalMetadata, originalTime, originalError
	})
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
	modelIDs = []string{"mimo-new-free"}
	if err := pullModels(server.URL, "chosen-key", "direct", "", true); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(discoveredZenModels(loadedConfig()), []string{"mimo-new-free"}) {
		t.Fatal("removed upstream model was retained")
	}
}
