package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

type upstreamError struct {
	status  int
	message string
}

func (e *upstreamError) Error() string {
	return fmt.Sprintf("zen upstream status %d: %s", e.status, e.message)
}

func selectUpstream(ctx context.Context, req executorRequest, cfg pluginConfig, route modelRoute, baseURL string, headers map[string][]string, key string) (*hostStreamHandle, modelRoute, error) {
	endpoints := candidateEndpoints(cfg, route)
	for i, endpoint := range endpoints {
		route.Endpoint = endpoint
		body, err := prepareUpstreamBody(req, route)
		if err != nil {
			return nil, route, err
		}
		handle, err := openUpstream(ctx, req.HostCallbackID, proxyForRequest(req), http.MethodPost, baseURL+route.EndpointPath(), headers, key, body)
		if err != nil {
			return nil, route, err
		}
		if handle.StatusCode >= 200 && handle.StatusCode < 300 {
			if i > 0 {
				rememberEndpoint(route.Model, endpoint)
			}
			return handle, route, nil
		}
		chunk, readErr := handle.read()
		handle.close()
		if readErr != nil {
			return nil, route, readErr
		}
		if isEndpointMismatch(handle.StatusCode) && i+1 < len(endpoints) {
			continue
		}
		return nil, route, &upstreamError{status: handle.StatusCode, message: truncate(chunk.Payload, 512)}
	}
	return nil, route, fmt.Errorf("zen upstream: no endpoint attempted")
}

func proxyForRequest(req executorRequest) string {
	if value, ok := req.AuthMetadata["proxy_url"].(string); ok {
		return strings.TrimSpace(value)
	}
	var stored struct {
		ProxyURL string `json:"proxy_url"`
	}
	_ = json.Unmarshal(req.StorageJSON, &stored)
	return strings.TrimSpace(stored.ProxyURL)
}

func proxyClient(proxyURL string) (*http.Client, *http.Transport, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	if proxyURL != "direct" {
		parsed, err := url.Parse(proxyURL)
		if err != nil || parsed.Hostname() == "" {
			return nil, nil, fmt.Errorf("invalid proxy address")
		}
		switch parsed.Scheme {
		case "http", "https", "socks5", "socks5h":
		default:
			return nil, nil, fmt.Errorf("proxy must use http, https, socks5 or socks5h")
		}
		if parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
			return nil, nil, fmt.Errorf("proxy address cannot contain a path, query or fragment")
		}
		transport.Proxy = http.ProxyURL(parsed)
	}
	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("stopped after 10 redirects")
			}
			if req.URL.Scheme != via[0].URL.Scheme || req.URL.Host != via[0].URL.Host {
				return fmt.Errorf("upstream redirect to a different origin is refused")
			}
			return nil
		},
	}
	return client, transport, nil
}

func openUpstream(ctx context.Context, callbackID, proxyURL, method, address string, headers map[string][]string, key string, body []byte) (*hostStreamHandle, error) {
	if proxyURL == "" {
		return doUpstreamStream(ctx, callbackID, method, address, headers, key, body)
	}
	client, transport, err := proxyClient(proxyURL)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, address, bytes.NewReader(body))
	if err != nil {
		transport.CloseIdleConnections()
		return nil, err
	}
	req.Header = http.Header(withAuth(headers, key))
	resp, err := client.Do(req)
	if err != nil {
		transport.CloseIdleConnections()
		return nil, err
	}
	return &hostStreamHandle{StatusCode: resp.StatusCode, Headers: resp.Header, response: resp, transport: transport}, nil
}

func requestUpstream(ctx context.Context, callbackID, proxyURL, method, address string, headers map[string][]string, key string, body []byte) ([]byte, map[string][]string, int, error) {
	handle, err := openUpstream(ctx, callbackID, proxyURL, method, address, headers, key, body)
	if err != nil {
		return nil, nil, 0, err
	}
	return readUpstreamResponse(handle)
}

func readUpstreamResponse(handle *hostStreamHandle) ([]byte, map[string][]string, int, error) {
	defer handle.close()
	limit := loadedConfig().MaxBufferBytes
	var buffer bytes.Buffer
	for {
		chunk, err := handle.read()
		if err != nil {
			return nil, nil, 0, err
		}
		if chunk.Error != "" {
			return nil, nil, 0, fmt.Errorf("zen upstream read: %s", chunk.Error)
		}
		if limit > 0 && int64(buffer.Len()+len(chunk.Payload)) > limit {
			return nil, nil, 0, fmt.Errorf("zen response exceeds max-buffer-bytes (%d)", limit)
		}
		buffer.Write(chunk.Payload)
		if chunk.Done {
			return buffer.Bytes(), handle.Headers, handle.StatusCode, nil
		}
	}
}

func (h *hostStreamHandle) read() (httpStreamChunk, error) {
	if h.response == nil {
		return readHostStream(h.StreamID)
	}
	buffer := make([]byte, 32*1024)
	n, err := h.response.Body.Read(buffer)
	if err == io.EOF {
		return httpStreamChunk{Payload: buffer[:n], Done: true}, nil
	}
	if err != nil {
		return httpStreamChunk{Payload: buffer[:n]}, err
	}
	return httpStreamChunk{Payload: buffer[:n]}, nil
}

func (h *hostStreamHandle) close() {
	if h.stopContext != nil {
		h.stopContext()
	}
	if h.response != nil {
		h.response.Body.Close()
		h.transport.CloseIdleConnections()
	} else if h.StreamID != "" {
		_, _ = callHost("host.http.stream_close", map[string]any{"stream_id": h.StreamID})
	}
}

func bindHostHTTPContext(ctx context.Context, callbackID string, request map[string]any) (func(), error) {
	if ctx.Done() == nil {
		return func() {}, nil
	}
	raw, err := callHost("host.http.operation_open", map[string]string{"host_callback_id": callbackID})
	if err != nil {
		return nil, err
	}
	var operation struct {
		ID string `json:"operation_id"`
	}
	if err := json.Unmarshal(raw, &operation); err != nil {
		return nil, err
	}
	request["operation_id"] = operation.ID
	cancel := func() {
		_, _ = callHost("host.http.cancel", map[string]string{"host_callback_id": callbackID, "operation_id": operation.ID})
	}
	stop := context.AfterFunc(ctx, cancel)
	return func() { stop(); cancel() }, nil
}
