package app

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/xaiauth"
)

func TestProxy_XAIOAuthStreamsThroughAllClientProtocols(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		body    map[string]any
		headers map[string]string
		text    func([]byte) string
	}{
		{
			name: "Codex", path: "/v1/responses",
			body: map[string]any{"model": "grok-4.5", "stream": true, "input": "hello"},
			text: func(body []byte) string {
				for _, block := range bytes.Split(body, []byte("\n\n")) {
					eventType, data := parseSSEEventChunk(block)
					payload, ok := decodeSSEPayload(data)
					if ok && eventType == "response.output_text.delta" {
						text, _ := payload["delta"].(string)
						return text
					}
				}
				return ""
			},
		},
		{
			name: "OpenAI", path: "/v1/chat/completions",
			body: map[string]any{"model": "grok-4.5", "stream": true, "messages": []any{map[string]any{"role": "user", "content": "hello"}}},
			text: func(body []byte) string {
				for _, block := range bytes.Split(body, []byte("\n\n")) {
					_, data := parseSSEEventChunk(block)
					payload, ok := decodeSSEPayload(data)
					if !ok {
						continue
					}
					choices, _ := payload["choices"].([]any)
					if len(choices) == 0 {
						continue
					}
					choice, _ := choices[0].(map[string]any)
					delta, _ := choice["delta"].(map[string]any)
					if text, _ := delta["content"].(string); text != "" {
						return text
					}
				}
				return ""
			},
		},
		{
			name: "Anthropic", path: "/v1/messages",
			body:    map[string]any{"model": "grok-4.5", "stream": true, "max_tokens": 64, "messages": []any{map[string]any{"role": "user", "content": "hello"}}},
			headers: map[string]string{"anthropic-version": "2023-06-01"},
			text: func(body []byte) string {
				for _, block := range bytes.Split(body, []byte("\n\n")) {
					eventType, data := parseSSEEventChunk(block)
					payload, ok := decodeSSEPayload(data)
					if !ok || eventType != "content_block_delta" {
						continue
					}
					delta, _ := payload["delta"].(map[string]any)
					if text, _ := delta["text"].(string); text != "" {
						return text
					}
				}
				return ""
			},
		},
		{
			name: "Gemini", path: "/v1beta/models/grok-4.5:streamGenerateContent?alt=sse",
			body: map[string]any{"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": "hello"}}}}},
			text: func(body []byte) string {
				for _, block := range bytes.Split(body, []byte("\n\n")) {
					_, data := parseSSEEventChunk(block)
					payload, ok := decodeSSEPayload(data)
					if !ok {
						continue
					}
					candidates, _ := payload["candidates"].([]any)
					if len(candidates) == 0 {
						continue
					}
					candidate, _ := candidates[0].(map[string]any)
					content, _ := candidate["content"].(map[string]any)
					parts, _ := content["parts"].([]any)
					if len(parts) == 0 {
						continue
					}
					part, _ := parts[0].(map[string]any)
					if text, _ := part["text"].(string); text != "" {
						return text
					}
				}
				return ""
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/v1/responses" {
					t.Errorf("upstream request = %s %s", r.Method, r.URL.Path)
				}
				wireBody, err := io.ReadAll(r.Body)
				if err != nil {
					t.Errorf("read xAI request: %v", err)
				}
				if got := r.Header.Get("Authorization"); got != "Bearer stream-access" {
					t.Errorf("Authorization = %q", got)
				}
				var wire map[string]any
				if err := json.Unmarshal(wireBody, &wire); err != nil {
					t.Errorf("decode xAI request: %v", err)
				}
				if wire["model"] != "grok-4.5" || wire["stream"] != true {
					t.Errorf("xAI request = %#v", wire)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "event: response.created\n"+`data: {"type":"response.created","response":{"id":"resp-stream","model":"grok-4.5"}}`+"\n\n")
				_, _ = io.WriteString(w, "event: response.output_text.delta\n"+`data: {"type":"response.output_text.delta","delta":"hello-xai"}`+"\n\n")
				_, _ = io.WriteString(w, "event: response.completed\n"+`data: {"type":"response.completed","response":{"id":"resp-stream","status":"completed","model":"grok-4.5","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`+"\n\n")
			}))
			defer upstream.Close()

			credential := mustXAICredentialJSON(t, &xaiauth.Credential{
				Type: xaiauth.ChannelType, AuthKind: "oauth", AccessToken: "stream-access", RefreshToken: "stream-refresh",
				Expired: time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			})
			env := setupProxyTestEnv(t, []testChannel{{
				name: "xai-stream", upstreamProtocol: "codex", models: "grok-4.5", priority: 100,
				authType: model.AuthTypeXAIOAuth, oauthCredential: credential,
			}}, map[int]string{0: upstream.URL + "/v1"})
			env.server.xaiCredentials = newXAICredentialManager(env.store, env.server.getClientForChannel, nil)

			response := doProxyRequest(t, env.engine, tt.path, tt.body, tt.headers)
			if tt.name != "Codex" {
				if response.Code != http.StatusServiceUnavailable {
					t.Fatalf("foreign protocol admitted: %d", response.Code)
				}
				return
			}
			if response.Code != http.StatusOK {
				t.Fatalf("response=%d body=%s", response.Code, response.Body.String())
			}
			if got := tt.text(response.Body.Bytes()); got != "hello-xai" {
				t.Fatalf("translated stream text=%q body=%s", got, response.Body.String())
			}
		})
	}
}
