package main

import (
	"embed"
	"encoding/json"
	"strings"
)

//go:embed web/*
var panelFiles embed.FS

func panelRegistration() ([]byte, error) {
	return okEnvelopeJSON(map[string]any{"resources": []map[string]string{
		{"Path": "/panel", "Menu": "OpenCode Zen", "Description": "管理 Zen Key 和独立代理"},
		{"Path": "/panel.mjs"},
	}})
}

func handlePanel(payload []byte) ([]byte, error) {
	var req struct{ Method, Path string }
	if err := json.Unmarshal(payload, &req); err != nil {
		return nil, err
	}
	status, contentType := 200, "text/html; charset=utf-8"
	var body []byte
	resource := strings.TrimPrefix(req.Path, "/v0/resource/plugins/"+pluginID)
	switch {
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
