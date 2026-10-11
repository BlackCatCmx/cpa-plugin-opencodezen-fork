package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
)

var catalogURL = "https://models.opencode.ai/api.json"
var endpointDocsURL = "https://raw.githubusercontent.com/anomalyco/opencode/dev/packages/web/src/content/docs/zen.mdx"
var endpointRow = regexp.MustCompile("\\|[^|]+\\|\\s*`?([^|`\\s]+)`?\\s*\\|\\s*`[^`]+/v1/(chat/completions|responses|messages|systemone)`")

type catalogModel struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Provider *struct {
		NPM string `json:"npm"`
	} `json:"provider"`
	Cost *struct {
		Input      *float64 `json:"input"`
		Output     *float64 `json:"output"`
		CacheRead  float64  `json:"cache_read"`
		CacheWrite float64  `json:"cache_write"`
	} `json:"cost"`
	Limit struct {
		Context int `json:"context"`
		Output  int `json:"output"`
	} `json:"limit"`
	Modalities struct {
		Input []string `json:"input"`
	} `json:"modalities"`
	Reasoning        bool `json:"reasoning"`
	ReasoningOptions []struct {
		Type   string   `json:"type"`
		Values []string `json:"values"`
	} `json:"reasoning_options"`
}

type modelMetadata struct {
	Endpoint     string
	Free         bool
	Capabilities modelCapabilities
}

func fetchCatalog(ctx context.Context, callbackID, proxy string) (map[string]modelMetadata, error) {
	data, _, status, err := requestUpstream(ctx, callbackID, proxy, http.MethodGet, catalogURL, nil, "", nil)
	if err != nil {
		return nil, err
	}
	if status/100 != 2 {
		return nil, fmt.Errorf("官方模型目录 HTTP %d: %s", status, truncate(data, 512))
	}
	var providers map[string]struct {
		NPM    string                  `json:"npm"`
		Models map[string]catalogModel `json:"models"`
	}
	if err := json.Unmarshal(data, &providers); err != nil {
		return nil, fmt.Errorf("官方模型目录解析失败: %w", err)
	}
	provider, ok := providers["opencode"]
	if !ok || len(provider.Models) == 0 {
		return nil, fmt.Errorf("官方目录缺少 OpenCode Zen 模型")
	}
	result := make(map[string]modelMetadata, len(provider.Models))
	for id, model := range provider.Models {
		if model.ID != "" {
			id = model.ID
		}
		npm := provider.NPM
		if model.Provider != nil && model.Provider.NPM != "" {
			npm = model.Provider.NPM
		}
		endpoint := endpointUnsupported
		switch npm {
		case "@ai-sdk/openai":
			endpoint = "responses"
		case "@ai-sdk/openai-compatible":
			endpoint = "chat"
		}
		free := false
		if model.Cost != nil && model.Cost.Input != nil && model.Cost.Output != nil {
			free = *model.Cost.Input == 0 && *model.Cost.Output == 0 && model.Cost.CacheRead == 0 && model.Cost.CacheWrite == 0
		} else {
			free = strings.HasSuffix(strings.ToLower(id), "-free")
		}
		caps := modelCapabilities{DisplayName: model.Name, ContextLength: int64(model.Limit.Context), MaxCompletionTokens: int64(model.Limit.Output), InputModalities: model.Modalities.Input}
		if model.Reasoning {
			caps.Thinking = &thinkingSupport{ZeroAllowed: true, DynamicAllowed: true}
		}
		for _, option := range model.ReasoningOptions {
			if option.Type == "effort" {
				caps.ReasoningLevels = option.Values
				caps.Thinking = &thinkingSupport{Levels: option.Values}
			}
		}
		result[id] = modelMetadata{Endpoint: endpoint, Free: free, Capabilities: caps}
	}
	data, _, status, err = requestUpstream(ctx, callbackID, proxy, http.MethodGet, endpointDocsURL, nil, "", nil)
	if err != nil {
		return nil, fmt.Errorf("官方端点表拉取失败: %w", err)
	}
	if status/100 != 2 {
		return nil, fmt.Errorf("官方端点表 HTTP %d", status)
	}
	rows := endpointRow.FindAllStringSubmatch(string(data), -1)
	if len(rows) == 0 {
		return nil, fmt.Errorf("官方端点表未包含可解析的模型行")
	}
	for _, row := range rows {
		metadata, exists := result[row[1]]
		if !exists {
			continue
		}
		metadata.Endpoint = endpointUnsupported
		switch row[2] {
		case "chat/completions":
			metadata.Endpoint = "chat"
		case "responses":
			metadata.Endpoint = "responses"
		}
		result[row[1]] = metadata
	}
	return result, nil
}

func pullModels(baseURL, key, proxy, callbackID string, force bool) error {
	discoveredModels.refreshMu.Lock()
	defer discoveredModels.refreshMu.Unlock()
	discoveredModels.mu.RLock()
	fresh := time.Since(discoveredModels.fetchedAt) < 10*time.Minute
	discoveredModels.mu.RUnlock()
	if !force && fresh && baseURL == loadedConfig().BaseURL {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	upstreamIDs, err := fetchZenModels(ctx, callbackID, baseURL, key, proxy)
	if err != nil {
		return err
	}
	metadata, err := fetchCatalog(ctx, callbackID, proxy)
	if err != nil {
		return err
	}
	ids := make([]string, 0, len(upstreamIDs))
	seen := make(map[string]bool)
	for _, id := range upstreamIDs {
		entry, known := metadata[id]
		if id != "" && !seen[id] && known && entry.Free && entry.Endpoint != endpointUnsupported {
			ids = append(ids, id)
			seen[id] = true
		}
	}
	discoveredModels.mu.Lock()
	defer discoveredModels.mu.Unlock()
	if baseURL != loadedConfig().BaseURL {
		return fmt.Errorf("服务地址已修改，请重新拉取")
	}
	discoveredModels.ids, discoveredModels.metadata, discoveredModels.fetchedAt = ids, metadata, time.Now()
	discoveredModels.lastError = ""
	return nil
}

func refreshModelsFromHost(cfg pluginConfig, callbackID string, force bool) error {
	raw, err := credentialHost("host.auth.list", struct{}{})
	if err != nil {
		return fmt.Errorf("读取模型拉取凭据失败: %w", err)
	}
	var listed struct {
		Files []struct {
			Provider  string
			Disabled  bool
			AuthIndex string `json:"auth_index"`
		} `json:"files"`
	}
	if err := json.Unmarshal(raw, &listed); err != nil {
		return err
	}
	for _, file := range listed.Files {
		if file.Provider != cfg.Provider || file.Disabled {
			continue
		}
		raw, err := credentialHost("host.auth.get", map[string]string{"auth_index": file.AuthIndex})
		if err != nil {
			return err
		}
		var saved struct {
			JSON map[string]any `json:"json"`
		}
		if err := json.Unmarshal(raw, &saved); err != nil {
			return err
		}
		key := extractAPIKey(saved.JSON)
		if key == "" {
			continue
		}
		proxy, _ := saved.JSON["proxy_url"].(string)
		err = pullModels(cfg.BaseURL, key, proxy, callbackID, force)
		if err != nil {
			message := strings.ReplaceAll(err.Error(), key, "[redacted]")
			if proxy != "" {
				message = strings.ReplaceAll(message, proxy, "[proxy]")
			}
			discoveredModels.mu.Lock()
			if cfg.BaseURL == loadedConfig().BaseURL {
				discoveredModels.lastError = message
			}
			discoveredModels.mu.Unlock()
			return fmt.Errorf("%s", message)
		}
		return nil
	}
	return fmt.Errorf("请先添加并启用一把 Zen Key，再拉取模型")
}

func panelModels() map[string]any {
	cfg := loadedConfig()
	discoveredModels.mu.RLock()
	ids := append([]string(nil), discoveredModels.ids...)
	lastError, updatedAt := discoveredModels.lastError, discoveredModels.fetchedAt
	discoveredModels.mu.RUnlock()
	models := make([]map[string]any, 0)
	routes := cfg.Models
	if len(routes) == 0 {
		for _, id := range ids {
			if !modelExcluded(cfg, id) {
				routes = append(routes, modelRoute{Model: id})
			}
		}
	}
	for _, model := range routes {
		route, _ := routeForModel(cfg, model.Model)
		if route.Endpoint != endpointUnsupported {
			models = append(models, map[string]any{"id": route.Model, "endpoint": route.Endpoint, "reasoning_levels": resolveModelCapabilities(route).ReasoningLevels})
		}
	}
	return map[string]any{"models": models, "error": lastError, "updated_at": updatedAt, "test_model": cfg.TestModel}
}
