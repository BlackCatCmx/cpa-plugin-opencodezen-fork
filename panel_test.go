package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func mockDiscoveryCatalog(t *testing.T) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/catalog" {
			io.WriteString(w, `{"opencode":{"npm":"@ai-sdk/openai-compatible","models":{"new-free":{"cost":{"input":0,"output":0}}}}}`)
		} else {
			io.WriteString(w, "| New | new-free | `https://opencode.ai/zen/v1/chat/completions` |")
		}
	}))
	originalCatalog, originalDocs := catalogURL, endpointDocsURL
	catalogURL, endpointDocsURL = server.URL+"/catalog", server.URL+"/docs"
	t.Cleanup(func() { catalogURL, endpointDocsURL = originalCatalog, originalDocs; server.Close() })
}

func TestPanelCredentialTest(t *testing.T) {
	for _, tc := range []struct {
		name, endpoint, answer string
		status                 int
		usable                 bool
	}{
		{"chat", "chat", "data: {\"choices\":[{\"delta\":{\"content\":\"OK\"}}]}\n\ndata: [DONE]\n\n", 200, true},
		{"responses", "responses", "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"OK\"}]}]}}\n\n", 200, true},
		{"rejected", "chat", "selected-secret was rejected", 401, false},
		{"empty", "chat", "data: [DONE]\n\n", 200, false},
		{"unrecognized-body", "chat", `{"unexpected":"changed format"}`, 200, false},
		{"long-error", "chat", strings.Repeat("x", 40000) + "selected-secret detail-at-end", 429, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.Host != "zen.invalid" || r.URL.Path != (modelRoute{Endpoint: tc.endpoint}).EndpointPath() {
					t.Errorf("wrong target: %s", r.URL)
				}
				if r.Header.Get("Authorization") != "Bearer selected-secret" || r.Header.Get("X-Opencode-Client") != "cli" || !canonicalShapeRe.MatchString(r.Header.Get("X-Opencode-Session")) {
					t.Errorf("missing credential or identity headers")
				}
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				if body["model"] != "test-free" || body["stream"] != true || len(body["tools"].([]any)) == 0 {
					t.Errorf("request was not prepared: %v", body)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.answer)
			}))
			defer proxy.Close()
			cfg := defaultPluginConfig()
			cfg.BaseURL = "http://zen.invalid"
			cfg.TestModel = "test-free"
			cfg.Models = []modelRoute{{Model: "test-free", Alias: "test-free", Endpoint: tc.endpoint}}
			resetConfig(t, cfg)
			original := credentialHost
			credentialHost = func(method string, payload any) (json.RawMessage, error) {
				if payload.(map[string]string)["auth_index"] != "selected" {
					t.Fatalf("unexpected credential lookup: %s %v", method, payload)
				}
				if method == "host.auth.get_runtime" {
					return json.RawMessage(`{"auth":{"provider":"zen"}}`), nil
				}
				if method != "host.auth.get" {
					t.Fatalf("unexpected host method: %s", method)
				}
				return json.RawMessage(fmt.Sprintf(`{"json":{"provider":"zen","api_key":"selected-secret","proxy_url":%q,"base_url":"http://ignored.invalid"}}`, proxy.URL)), nil
			}
			t.Cleanup(func() { credentialHost = original })
			result, err := testCredential([]byte(`{"auth_index":"selected"}`), "")
			if err != nil || result["ok"] != tc.usable || result["http_status"] != tc.status || calls != 1 {
				t.Fatalf("result = %v, error = %v, calls = %d", result, err, calls)
			}
			if tc.usable && result["reply"] != "OK" {
				t.Fatalf("reply = %v", result["reply"])
			}
			if result["protocol"] != tc.endpoint || result["endpoint"] != "http://zen.invalid"+(modelRoute{Endpoint: tc.endpoint}).EndpointPath() {
				t.Fatalf("route details = %v", result)
			}
			if !tc.usable && result["raw"] == nil {
				t.Fatalf("missing raw diagnostic: %v", result)
			}
			if tc.name == "long-error" && !strings.Contains(result["raw"].(string), "detail-at-end") {
				t.Fatal("error body was only partially read")
			}
			encoded, _ := json.Marshal(result)
			if strings.Contains(string(encoded), "selected-secret") {
				t.Fatal("test result exposed key")
			}
		})
	}
}

func TestPanelTestRejectsOtherProvider(t *testing.T) {
	original := credentialHost
	credentialHost = func(string, any) (json.RawMessage, error) {
		return json.RawMessage(`{"auth":{"provider":"other"}}`), nil
	}
	t.Cleanup(func() { credentialHost = original })
	for _, body := range []string{`{}`, `{"auth_index":"other"}`} {
		if _, err := testCredential([]byte(body), ""); err == nil {
			t.Fatal("invalid credential accepted")
		}
	}
}

func TestPluginModelPrefix(t *testing.T) {
	t.Cleanup(func() { storeConfig(defaultPluginConfig()) })
	for _, tc := range []struct{ config, prefix string }{
		{"", "opencode"}, {"model-prefix: custom\n", "custom"}, {"model-prefix: \"\"\n", ""},
	} {
		payload, _ := json.Marshal(map[string]any{"config_yaml": []byte(tc.config + "models:\n  - model: mimo-v2.6-flash-free\n    alias: public\n  - model: muse-spark-1.3-contributor-free\n")})
		if err := configure(payload); err != nil {
			t.Fatal(err)
		}
		cfg := loadedConfig()
		if cfg.ModelPrefix != tc.prefix {
			t.Fatalf("prefix = %q, want %q", cfg.ModelPrefix, tc.prefix)
		}
		response, err := modelRegistration()
		if err != nil {
			t.Fatal(err)
		}
		var result struct{ Result modelRegistrationResponse }
		if err := json.Unmarshal(response, &result); err != nil {
			t.Fatal(err)
		}
		for i, registered := range result.Result.Models {
			want := cfg.Models[i].Alias
			if tc.prefix != "" {
				want = tc.prefix + "/" + want
			}
			if registered.ID != want {
				t.Fatalf("registered ID = %q, want %q", registered.ID, want)
			}
			route, ok := routeForModel(cfg, registered.ID)
			if !ok || route.Model != cfg.Models[i].Model {
				t.Fatalf("route = %v", route)
			}
			body, err := prepareUpstreamBody(executorRequest{Model: registered.ID, Payload: chatPayload(registered.ID, "hello")}, route)
			if err != nil {
				t.Fatal(err)
			}
			var upstream map[string]any
			if err := json.Unmarshal(body, &upstream); err != nil {
				t.Fatal(err)
			}
			if upstream["model"] != route.Model {
				t.Fatalf("upstream model = %v", upstream["model"])
			}
		}
	}
	for _, config := range []string{"model-prefix: 123", "model-prefix: a/b", "model-prefix: a b"} {
		payload, _ := json.Marshal(map[string]any{"config_yaml": []byte(config)})
		if err := configure(payload); err == nil {
			t.Fatalf("accepted invalid config %q", config)
		}
	}
}

func TestPluginPrefixCoversDiscoveredModels(t *testing.T) {
	discoveredModels.mu.Lock()
	previousIDs, previousTime := discoveredModels.ids, discoveredModels.fetchedAt
	discoveredModels.ids, discoveredModels.fetchedAt = []string{"new-test-free"}, time.Now()
	discoveredModels.mu.Unlock()
	t.Cleanup(func() {
		discoveredModels.mu.Lock()
		discoveredModels.ids, discoveredModels.fetchedAt = previousIDs, previousTime
		discoveredModels.mu.Unlock()
	})
	resetConfig(t, defaultPluginConfig())
	response, err := modelRegistration()
	if err != nil {
		t.Fatal(err)
	}
	var result struct{ Result modelRegistrationResponse }
	if err := json.Unmarshal(response, &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Result.Models) != 1 {
		t.Fatalf("models = %v", result.Result.Models)
	}
	for _, model := range result.Result.Models {
		if !strings.HasPrefix(model.ID, "opencode/") {
			t.Fatalf("unprefixed model: %s", model.ID)
		}
		route, ok := routeForModel(loadedConfig(), model.ID)
		if !ok || strings.HasPrefix(route.Model, "opencode/") {
			t.Fatalf("invalid upstream route: %v", route)
		}
	}
}

func TestPanelManagementRoute(t *testing.T) {
	registration, err := panelRegistration()
	if err != nil || !strings.Contains(string(registration), `"Path":"/plugins/zen/test"`) {
		t.Fatalf("registration = %s, %v", registration, err)
	}
	payload, _ := json.Marshal(map[string]any{"Method": "POST", "Path": "/v0/management/plugins/zen/test", "Body": []byte(`{}`)})
	response, err := handlePanel(payload)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Result struct {
			StatusCode int
			Body       []byte
		}
	}
	if err := json.Unmarshal(response, &result); err != nil {
		t.Fatal(err)
	}
	if result.Result.StatusCode != 400 || !strings.Contains(string(result.Result.Body), "error") {
		t.Fatalf("response = %s", response)
	}
}

func TestPanelProxyFailure(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; io.WriteString(w, "unexpected") }))
	defer server.Close()
	cfg := defaultPluginConfig()
	cfg.BaseURL = server.URL
	cfg.TestModel = "test-free"
	resetConfig(t, cfg)
	original := credentialHost
	credentialHost = func(method string, _ any) (json.RawMessage, error) {
		if method == "host.auth.get_runtime" {
			return json.RawMessage(`{"auth":{"provider":"zen"}}`), nil
		}
		return json.RawMessage(fmt.Sprintf(`{"json":{"type":"zen","api_key":"secret","proxy_url":"http://127.0.0.1:1","base_url":%q}}`, server.URL)), nil
	}
	t.Cleanup(func() { credentialHost = original })
	result, err := testCredential([]byte(`{"auth_index":"selected"}`), "")
	if err != nil || result["ok"] != false || result["error"] == nil || calls != 0 {
		t.Fatalf("result = %v, error = %v, calls = %d", result, err, calls)
	}
}

func TestPluginServiceAddressDiscovery(t *testing.T) {
	resetDiscoveryCache(t)
	mockDiscoveryCatalog(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer selected" {
			t.Errorf("wrong discovery request: %s", r.URL)
		}
		io.WriteString(w, `{"data":[{"id":"new-free"}]}`)
	}))
	defer server.Close()
	original := credentialHost
	credentialHost = func(method string, _ any) (json.RawMessage, error) {
		if method == "host.auth.list" {
			return json.RawMessage(`{"files":[{"provider":"zen","auth_index":"selected"}]}`), nil
		}
		if method == "host.auth.get" {
			return json.RawMessage(`{"json":{"api_key":"selected","proxy_url":"direct","base_url":"http://ignored.invalid"}}`), nil
		}
		return nil, fmt.Errorf("unexpected host method %s", method)
	}
	t.Cleanup(func() { credentialHost = original; storeConfig(defaultPluginConfig()) })
	discoveredModels.mu.Lock()
	discoveredModels.ids, discoveredModels.fetchedAt = []string{"old-free"}, time.Now()
	discoveredModels.attemptedAt, discoveredModels.failures = time.Now(), maxModelRefreshFailures
	discoveredModels.mu.Unlock()
	payload, _ := json.Marshal(map[string]any{"config_yaml": []byte("base-url: " + server.URL + "/v1/\n")})
	if err := configure(payload); err != nil {
		t.Fatal(err)
	}
	if loadedConfig().BaseURL != server.URL+"/v1" {
		t.Fatalf("base URL = %s", loadedConfig().BaseURL)
	}
	ids := discoveredZenModels(loadedConfig())
	if len(ids) != 1 || ids[0] != "new-free" {
		t.Fatalf("discovery = %v", ids)
	}
	for _, address := range []string{"ftp://invalid", "https://user:password@invalid", "https://invalid?query=1", "https://invalid#fragment"} {
		payload, _ := json.Marshal(map[string]any{"config_yaml": []byte("base-url: " + address)})
		if err := configure(payload); err == nil {
			t.Fatalf("accepted invalid address %s", address)
		}
	}
}
