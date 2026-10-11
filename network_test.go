package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func mockCredentialHost(t *testing.T) {
	t.Helper()
	original := credentialHost
	credentialHost = func(method string, payload any) (json.RawMessage, error) {
		return json.RawMessage(`{"files":[]}`), nil
	}
	t.Cleanup(func() { credentialHost = original })
}

func mockSelectedCredential(t *testing.T, provider, proxy string) {
	t.Helper()
	original := credentialHost
	credentialHost = func(method string, payload any) (json.RawMessage, error) {
		switch method {
		case "host.auth.get_runtime":
			return json.RawMessage(fmt.Sprintf(`{"auth":{"provider":%q}}`, provider)), nil
		case "host.auth.get":
			return json.RawMessage(fmt.Sprintf(`{"json":{"proxy_url":%q}}`, proxy)), nil
		default:
			return nil, fmt.Errorf("unexpected host method %s", method)
		}
	}
	t.Cleanup(func() { credentialHost = original })
}

func TestCredentialImportPreservesExistingFilesAndReportsFailure(t *testing.T) {
	original := credentialHost
	t.Cleanup(func() { credentialHost = original })
	cfg := testConfig()
	saves := 0
	credentialHost = func(method string, payload any) (json.RawMessage, error) {
		if method == "host.auth.list" {
			return json.RawMessage(fmt.Sprintf(`{"files":[{"name":"zen-%s.json"}]}`, sha256Prefix(cfg.APIKeys[0]))), nil
		}
		saves++
		return nil, errors.New("disk write failed")
	}
	if err := persistVirtualAuths(cfg); err == nil || !strings.Contains(err.Error(), "disk write failed") {
		t.Fatalf("save error = %v", err)
	}
	if saves != 1 {
		t.Fatalf("saves = %d, existing file must not be overwritten", saves)
	}
}

func TestIndependentProxiesAndDiscovery(t *testing.T) {
	for _, key := range []string{"key-one", "key-two"} {
		t.Run(key, func(t *testing.T) {
			calls := 0
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Header.Get("Authorization") != "Bearer "+key {
					t.Errorf("wrong credential: %s", r.Header.Get("Authorization"))
				}
				if r.URL.Host != "zen.invalid" {
					t.Errorf("target = %s", r.URL)
				}
				if r.URL.Path == "/models" {
					fmt.Fprint(w, `{"data":[{"id":"test-free"}]}`)
					return
				}
				fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
			}))
			defer proxy.Close()
			req := executorRequest{AuthMetadata: map[string]any{"proxy_url": proxy.URL}}
			data, _, status, err := requestUpstream(context.Background(), "", proxyForRequest(req), "POST", "http://zen.invalid/chat/completions", nil, key, []byte(`{}`))
			if err != nil || status != 200 || !strings.Contains(string(data), "ok") {
				t.Fatalf("request: %s %d %v", data, status, err)
			}
			models, err := fetchZenModels(context.Background(), "", "http://zen.invalid", key, proxy.URL)
			if err != nil || len(models) != 1 || models[0] != "test-free" {
				t.Fatalf("discovery: %v %v", models, err)
			}
			if calls != 2 {
				t.Fatalf("proxy calls = %d", calls)
			}
		})
	}
}

func TestDirectBypassesEnvironmentProxyAndInvalidProxyFails(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "ok") }))
	defer srv.Close()
	data, _, _, err := requestUpstream(context.Background(), "", "direct", "GET", srv.URL, nil, "test", nil)
	if err != nil || string(data) != "ok" {
		t.Fatalf("direct: %s %v", data, err)
	}
	for _, proxy := range []string{"proxy:8080", "ftp://localhost:80", "http://localhost:80/path"} {
		if _, _, err := proxyClient(proxy); err == nil {
			t.Fatalf("accepted invalid proxy %s", proxy)
		}
	}
}

func TestProxyFailureDoesNotFallBackToDirect(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; fmt.Fprint(w, "ok") }))
	defer srv.Close()
	_, _, _, err := requestUpstream(context.Background(), "", "http://127.0.0.1:1", "GET", srv.URL, nil, "test", nil)
	if err == nil || calls != 0 {
		t.Fatalf("error = %v, direct calls = %d", err, calls)
	}
}

func TestCompletionCancelsLocalRequest(t *testing.T) {
	mockSelectedCredential(t, "zen", "direct")
	defer cancelExecutions()
	_, err := trackExecution([]byte(`{"RequestID":"cancel-test","Metadata":{"selected_auth_index":"test"}}`))
	if err != nil {
		t.Fatal(err)
	}
	req := executorRequest{Headers: map[string][]string{executionHeader: {"cancel-test"}}, AuthMetadata: map[string]any{"proxy_url": "direct"}}
	ctx, err := executionContext(req)
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(started); <-r.Context().Done() }))
	defer srv.Close()
	done := make(chan error, 1)
	go func() {
		_, _, _, err := requestUpstream(ctx, "", "direct", "GET", srv.URL, nil, "test", nil)
		done <- err
	}()
	<-started
	_, err = completeExecution([]byte(`{"RequestID":"cancel-test","Outcome":"canceled"}`))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("local request was not canceled")
	}
}

func TestStreamEndpointRetryKeepsKeyAndReturnsUsedDialect(t *testing.T) {
	var keys []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		keys = append(keys, r.Header.Get("Authorization"))
		if strings.HasSuffix(r.URL.Path, "/chat/completions") {
			w.WriteHeader(404)
			return
		}
		fmt.Fprint(w, "data: {\"type\":\"response.completed\"}\n\n")
	}))
	defer srv.Close()
	cfg := defaultPluginConfig()
	req := executorRequest{Model: "custom", Payload: chatPayload("custom", "hello"), AuthMetadata: map[string]any{"proxy_url": "direct"}}
	handle, route, err := selectUpstream(context.Background(), req, cfg, modelRoute{Model: "custom", Endpoint: "chat"}, srv.URL, nil, "selected")
	if err != nil {
		t.Fatal(err)
	}
	defer handle.close()
	data, err := io.ReadAll(handle.response.Body)
	if err != nil || route.Endpoint != "responses" || !strings.Contains(string(data), "response.completed") {
		t.Fatalf("route: %v, body: %s, error: %v", route, data, err)
	}
	if len(keys) != 2 || keys[0] != "Bearer selected" || keys[1] != keys[0] {
		t.Fatalf("credentials changed: %v", keys)
	}
}

func TestAuthenticationFailureRetainsSelectedKeyAndHTTPStatus(t *testing.T) {
	mockSelectedCredential(t, "zen", "direct")
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "Bearer selected" {
			t.Errorf("wrong key")
		}
		w.WriteHeader(401)
		fmt.Fprint(w, "invalid key")
	}))
	defer srv.Close()
	_, _ = trackExecution([]byte(`{"RequestID":"auth-test","Metadata":{"selected_auth_index":"test"}}`))
	defer completeExecution([]byte(`{"RequestID":"auth-test"}`))
	cfg := testConfig()
	cfg.BaseURL = srv.URL
	resetConfig(t, cfg)
	req := executorRequest{Model: "mimo-v2.6-flash-free", Payload: chatPayload("mimo-v2.6-flash-free", "hello"), StreamID: "stream", Headers: map[string][]string{executionHeader: {"auth-test"}}, StorageJSON: []byte(`{"api_key":"selected","base_url":"http://ignored.invalid","proxy_url":"direct"}`)}
	payload, _ := json.Marshal(req)
	for _, method := range []string{"executor.execute", "executor.execute_stream"} {
		raw, status := processPluginCall(method, payload)
		var result envelope
		if err := json.Unmarshal(raw, &result); err != nil {
			t.Fatal(err)
		}
		if status != 1 || result.Error.HTTPStatus != 401 {
			t.Fatalf("%s: %s", method, raw)
		}
	}
	if calls != 2 {
		t.Fatalf("auth failure retried %d times", calls)
	}
}

func TestExecutionTrackingOnlyForZenLocalHTTP(t *testing.T) {
	for _, test := range []struct {
		provider, proxy string
		tracked         bool
	}{
		{"openai", "direct", false}, {"zen", "", false}, {"zen", "direct", true},
	} {
		t.Run(test.provider+test.proxy, func(t *testing.T) {
			mockSelectedCredential(t, test.provider, test.proxy)
			defer cancelExecutions()
			raw, err := trackExecution([]byte(`{"RequestID":"scope-test","Metadata":{"selected_auth_index":"test"}}`))
			if err != nil {
				t.Fatal(err)
			}
			var env envelope
			if err := json.Unmarshal(raw, &env); err != nil {
				t.Fatal(err)
			}
			var response struct {
				Headers      map[string][]string
				ClearHeaders []string
			}
			if err := json.Unmarshal(env.Result, &response); err != nil {
				t.Fatal(err)
			}
			executions.Lock()
			count := len(executions.active)
			executions.Unlock()
			if test.tracked {
				if count != 1 || len(response.Headers[executionHeader]) != 1 {
					t.Fatalf("missing tracking: %s", raw)
				}
				if _, err := completeExecution([]byte(`{"RequestID":"scope-test"}`)); err != nil {
					t.Fatal(err)
				}
				executions.Lock()
				remaining := len(executions.active)
				executions.Unlock()
				if remaining != 0 {
					t.Fatalf("completion retained %d scopes", remaining)
				}
			} else if count != 0 || len(response.Headers) != 0 || len(response.ClearHeaders) != 1 {
				t.Fatalf("unexpected tracking: %s", raw)
			}
		})
	}
}

func TestResponseBufferLimitClosesUpstream(t *testing.T) {
	closed := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, strings.Repeat("x", 1024))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(closed)
	}))
	defer srv.Close()
	cfg := defaultPluginConfig()
	cfg.MaxBufferBytes = 32
	resetConfig(t, cfg)
	_, _, _, err := requestUpstream(context.Background(), "", "direct", "GET", srv.URL, nil, "test", nil)
	if err == nil || !strings.Contains(err.Error(), "max-buffer-bytes") {
		t.Fatalf("limit error: %v", err)
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("upstream remained open after buffer limit")
	}
}

func TestSSEBufferLimitAndReset(t *testing.T) {
	for _, payload := range []string{strings.Repeat("x", 65), strings.Repeat("data: abc\n", 16)} {
		r := &sseReassembler{maxBufferBytes: 64, emit: func([]byte) error { return nil }}
		if err := r.write([]byte(payload)); err == nil {
			t.Fatal("accepted oversized SSE event")
		}
	}
	emitted := 0
	r := &sseReassembler{maxBufferBytes: 64, emit: func([]byte) error { emitted++; return nil }}
	if err := r.write([]byte(strings.Repeat("data: abc\n\n", 100))); err != nil {
		t.Fatal(err)
	}
	if emitted != 100 || r.pending.Len() != 0 || r.lineBuf.Len() != 0 {
		t.Fatalf("buffer did not reset: %d", emitted)
	}
}

func TestConfigureResponseBufferLimit(t *testing.T) {
	original := loadedConfig()
	t.Cleanup(func() { storeConfig(original) })
	if err := configure(nil); err != nil {
		t.Fatal(err)
	}
	if loadedConfig().MaxBufferBytes != 64<<20 {
		t.Fatal("incorrect default buffer limit")
	}
	for _, test := range []struct {
		yaml    string
		limit   int64
		invalid bool
	}{
		{"max-buffer-bytes: 0\n", 0, false},
		{"max-buffer-bytes: 1024\n", 1024, false},
		{"max-buffer-bytes: -1\n", 0, true},
		{"max-buffer-bytes: 1.5\n", 0, true},
	} {
		raw, _ := json.Marshal(map[string]any{"config_yaml": []byte(test.yaml)})
		err := configure(raw)
		if test.invalid {
			if err == nil {
				t.Fatalf("accepted %s", test.yaml)
			}
		} else if err != nil || loadedConfig().MaxBufferBytes != test.limit {
			t.Fatalf("config %s: %v", test.yaml, err)
		}
	}
}
