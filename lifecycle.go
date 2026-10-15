package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

const executionHeader = "X-Zen-Execution-Id"

type executionScope struct {
	ctx    context.Context
	cancel context.CancelFunc
}

var executions = struct {
	sync.Mutex
	active map[string]executionScope
}{active: make(map[string]executionScope)}

// CPA completion events cancel local HTTP requests when the client disconnects.
func trackExecution(payload []byte) ([]byte, error) {
	var req struct {
		RequestID string
		Metadata  map[string]any
	}
	if err := json.Unmarshal(payload, &req); err != nil {
		return nil, err
	}
	index, _ := req.Metadata["selected_auth_index"].(string)
	if index == "" {
		return okEnvelopeJSON(map[string]any{"ClearHeaders": []string{executionHeader}})
	}
	raw, err := credentialHost("host.auth.list", struct{}{})
	if err != nil {
		return nil, err
	}
	var listed struct {
		Files []struct {
			AuthIndex string `json:"auth_index"`
			Provider  string
		} `json:"files"`
	}
	if err := json.Unmarshal(raw, &listed); err != nil {
		return nil, err
	}
	isZen := false
	for _, file := range listed.Files {
		if file.AuthIndex == index && file.Provider == loadedConfig().Provider {
			isZen = true
			break
		}
	}
	if !isZen {
		return okEnvelopeJSON(map[string]any{"ClearHeaders": []string{executionHeader}})
	}
	raw, err = credentialHost("host.auth.get", map[string]string{"auth_index": index})
	if err != nil {
		return nil, err
	}
	var saved struct {
		JSON struct {
			ProxyURL string `json:"proxy_url"`
		} `json:"json"`
	}
	if err := json.Unmarshal(raw, &saved); err != nil {
		return nil, err
	}
	if strings.TrimSpace(saved.JSON.ProxyURL) == "" {
		return okEnvelopeJSON(map[string]any{"ClearHeaders": []string{executionHeader}})
	}
	if req.RequestID == "" {
		return nil, fmt.Errorf("CPA execution request ID is required")
	}
	executions.Lock()
	if _, exists := executions.active[req.RequestID]; !exists {
		ctx, cancel := context.WithCancel(context.Background())
		executions.active[req.RequestID] = executionScope{ctx, cancel}
	}
	executions.Unlock()
	return okEnvelopeJSON(map[string]any{"Headers": map[string][]string{executionHeader: {req.RequestID}}})
}

func executionContext(req executorRequest) (context.Context, error) {
	if proxyForRequest(req) == "" {
		return context.Background(), nil
	}
	id, _ := headerValue(req.Headers, executionHeader)
	executions.Lock()
	scope, exists := executions.active[id]
	executions.Unlock()
	if !exists {
		return nil, fmt.Errorf("CPA did not provide an active Zen execution context")
	}
	return scope.ctx, nil
}

func completeExecution(payload []byte) ([]byte, error) {
	var req struct{ RequestID string }
	if err := json.Unmarshal(payload, &req); err != nil {
		return nil, err
	}
	executions.Lock()
	if scope, exists := executions.active[req.RequestID]; exists {
		scope.cancel()
		delete(executions.active, req.RequestID)
	}
	executions.Unlock()
	return okEnvelopeJSON(struct{}{})
}

func cancelExecutions() {
	executions.Lock()
	defer executions.Unlock()
	for id, scope := range executions.active {
		scope.cancel()
		delete(executions.active, id)
	}
}
