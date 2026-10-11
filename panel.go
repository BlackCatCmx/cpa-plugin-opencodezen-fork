package main

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

//go:embed web/*
var panelFiles embed.FS

func panelRegistration() ([]byte, error) {
	return okEnvelopeJSON(map[string]any{"routes": []map[string]string{
		{"Method": "POST", "Path": "/plugins/zen/test"},
		{"Method": "GET", "Path": "/plugins/zen/models"},
		{"Method": "POST", "Path": "/plugins/zen/models/refresh"},
	}, "resources": []map[string]string{
		{"Path": "/panel", "Menu": "OpenCode Zen", "Description": "管理 Zen Key 和独立代理"},
		{"Path": "/panel.mjs"},
	}})
}

func handlePanel(payload []byte) ([]byte, error) {
	var req struct {
		Method, Path   string
		Body           []byte
		HostCallbackID string `json:"host_callback_id"`
	}
	if err := json.Unmarshal(payload, &req); err != nil {
		return nil, err
	}
	status, contentType := 200, "text/html; charset=utf-8"
	var body []byte
	resource := strings.TrimPrefix(req.Path, "/v0/resource/plugins/"+pluginID)
	switch {
	case req.Path == "/v0/management/plugins/zen/models" && req.Method == "GET":
		contentType = "application/json; charset=utf-8"
		body, _ = json.Marshal(panelModels())
	case req.Path == "/v0/management/plugins/zen/models/refresh" && req.Method == "POST":
		contentType = "application/json; charset=utf-8"
		if err := refreshModelsFromHost(loadedConfig(), req.HostCallbackID, true); err != nil {
			status = 400
			body, _ = json.Marshal(map[string]any{"error": err.Error()})
		} else {
			body, _ = json.Marshal(panelModels())
		}
	case req.Path == "/v0/management/plugins/zen/test" && req.Method == "POST":
		contentType = "application/json; charset=utf-8"
		result, err := testCredential(req.Body, req.HostCallbackID)
		if err != nil {
			status = 400
			result = map[string]any{"error": err.Error()}
		}
		body, _ = json.Marshal(result)
	case req.Method != "GET":
		status, body = 405, []byte("method not allowed")
	case resource == "/panel":
		body, _ = panelFiles.ReadFile("web/panel.html")
	case resource == "/panel.mjs":
		contentType = "text/javascript; charset=utf-8"
		body, _ = panelFiles.ReadFile("web/panel.mjs")
	default:
		status, body = 404, []byte("not found")
	}
	return okEnvelopeJSON(struct {
		StatusCode int
		Headers    map[string][]string
		Body       []byte
	}{status, map[string][]string{
		"Content-Type":            {contentType},
		"Cache-Control":           {"no-store"},
		"X-Content-Type-Options":  {"nosniff"},
		"Referrer-Policy":         {"no-referrer"},
		"Content-Security-Policy": {"default-src 'none'; script-src 'self'; style-src 'unsafe-inline'; connect-src 'self'; frame-ancestors 'self'; base-uri 'none'; form-action 'none'"},
	}, body})
}

func testCredential(body []byte, callbackID string) (map[string]any, error) {
	var input struct {
		AuthIndex string `json:"auth_index"`
	}
	if err := json.Unmarshal(body, &input); err != nil || strings.TrimSpace(input.AuthIndex) == "" {
		return nil, fmt.Errorf("请选择要测试的 Key")
	}
	raw, err := credentialHost("host.auth.get_runtime", map[string]string{"auth_index": input.AuthIndex})
	if err != nil {
		return nil, fmt.Errorf("读取凭据失败：%w", err)
	}
	var runtime struct {
		Auth struct{ Provider string } `json:"auth"`
	}
	if err := json.Unmarshal(raw, &runtime); err != nil {
		return nil, err
	}
	cfg := loadedConfig()
	if !cfg.Enabled {
		return nil, fmt.Errorf("Zen 插件已停用")
	}
	if runtime.Auth.Provider != cfg.Provider {
		return nil, fmt.Errorf("所选凭据不是 Zen Key")
	}
	raw, err = credentialHost("host.auth.get", map[string]string{"auth_index": input.AuthIndex})
	if err != nil {
		return nil, fmt.Errorf("读取凭据失败：%w", err)
	}
	var saved struct {
		JSON map[string]any `json:"json"`
	}
	if err := json.Unmarshal(raw, &saved); err != nil {
		return nil, err
	}
	key := extractAPIKey(saved.JSON)
	if key == "" {
		return nil, fmt.Errorf("凭据缺少 API Key")
	}
	model := cfg.TestModel
	if model == "" {
		return nil, fmt.Errorf("请先选择并保存测试模型")
	}
	route, _ := routeForModel(cfg, model)
	if endpoint, known := officialZenEndpoint(route.Model); known && endpoint == endpointUnsupported && !routeConfigured(cfg, route.Model) {
		return nil, fmt.Errorf("测试模型使用插件不支持的协议")
	}
	payload, _ := json.Marshal(map[string]any{
		"model": model, "max_tokens": 256,
		"messages": []map[string]string{{"role": "user", "content": "Reply with OK, nothing else."}},
	})
	storage, _ := json.Marshal(saved.JSON)
	req := executorRequest{Model: model, Payload: payload, StorageJSON: storage, HostCallbackID: callbackID}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	started := time.Now()
	result := map[string]any{"ok": false, "model": model}
	response, usedRoute, err := selectUpstream(ctx, req, route, cfg.BaseURL, gateHeaders(req), key)
	result["protocol"], result["endpoint"] = usedRoute.Endpoint, cfg.BaseURL+usedRoute.EndpointPath()
	var rawResponse []byte
	if err == nil {
		result["http_status"] = response.StatusCode
		var data []byte
		data, _, _, err = readUpstreamResponse(response)
		rawResponse = data
		if err == nil {
			data, err = foldSSEToJSON(data, usedRoute.Endpoint)
		}
		if err == nil && usedRoute.Endpoint == "responses" {
			data, err = responsesCompletionToChat(data, usedRoute.Model)
		}
		if err == nil {
			var completion struct {
				Choices []struct{ Message struct{ Content string } }
			}
			err = json.Unmarshal(data, &completion)
			if err == nil && (len(completion.Choices) == 0 || strings.TrimSpace(completion.Choices[0].Message.Content) == "") {
				err = fmt.Errorf("上游未返回有效文本回答")
			}
			if err == nil {
				result["ok"] = true
				result["reply"] = completion.Choices[0].Message.Content
			}
		}
	}
	result["duration_ms"] = time.Since(started).Milliseconds()
	if err != nil {
		if upstream, ok := err.(*upstreamError); ok {
			result["http_status"] = upstream.status
			rawResponse = []byte(upstream.raw)
		}
		message := strings.ReplaceAll(err.Error(), key, "[redacted]")
		if proxy := proxyForRequest(req); proxy != "" {
			message = strings.ReplaceAll(message, proxy, "[proxy]")
		}
		result["error"] = message
		if len(rawResponse) > 0 {
			detail := strings.ReplaceAll(string(rawResponse), key, "[redacted]")
			if proxy := proxyForRequest(req); proxy != "" {
				detail = strings.ReplaceAll(detail, proxy, "[proxy]")
			}
			result["raw"] = detail
		}
	}
	return result, nil
}
