package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"ccLoad/internal/model"
	"ccLoad/internal/protocol"
	"ccLoad/internal/util"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func TestForceUpstreamStream(t *testing.T) {
	for _, upstream := range []protocol.Protocol{protocol.Gemini, protocol.OpenAI, protocol.Anthropic, protocol.Codex} {
		for _, client := range []protocol.Protocol{protocol.OpenAI, protocol.Gemini, protocol.Anthropic, protocol.Codex} {
			t.Run(string(client)+"/"+string(upstream), func(t *testing.T) {
				env := setupProxyTestEnv(t, []testChannel{{name: "forced", upstreamProtocol: string(upstream), protocolTransformMode: model.ProtocolTransformModeLocal, models: "model-free", apiKey: "test-key", customRequestRules: &model.CustomRequestRules{ForceUpstreamStream: true}}}, map[int]string{0: "https://upstream.example.com"})
				configs, err := env.store.ListConfigs(context.Background())
				if err != nil || len(configs) != 1 || configs[0].CustomRequestRules == nil || !configs[0].CustomRequestRules.ForceUpstreamStream {
					t.Fatalf("stream setting did not survive persistence: %v", err)
				}
				prefix, suffix := openCodeFreeToolStreamFixture(upstream)
				calls := 0
				env.server.client = &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
					calls++
					body, err := io.ReadAll(r.Body)
					if err != nil {
						return nil, err
					}
					if upstream == protocol.Gemini {
						if !strings.HasSuffix(r.URL.Path, ":streamGenerateContent") || r.URL.Query().Get("alt") != "sse" || gjson.GetBytes(body, "stream").Exists() {
							t.Fatalf("invalid Gemini stream request: %s %s", r.URL, body)
						}
					} else if !gjson.GetBytes(body, "stream").Bool() {
						t.Fatalf("upstream request must stream: %s", body)
					}
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(prefix + suffix))}, nil
				})}
				path := "/v1/chat/completions"
				request := map[string]any{"model": "model-free", "stream": false, "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
				switch client {
				case protocol.Gemini:
					path = "/v1beta/models/model-free:generateContent"
					request = map[string]any{"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": "hi"}}}}}
				case protocol.Anthropic:
					path = "/v1/messages"
					request["max_tokens"] = 64
				case protocol.Codex:
					path = "/v1/responses"
					delete(request, "messages")
					request["input"] = "hi"
				}
				response := doProxyRequest(t, env.engine, path, request, nil)
				if calls != 1 || response.Code != 200 || !json.Valid(response.Body.Bytes()) || !strings.Contains(response.Header().Get("Content-Type"), "application/json") {
					t.Fatalf("nonstream response: calls=%d status=%d headers=%v body=%s", calls, response.Code, response.Header(), response.Body.String())
				}
				if !strings.Contains(response.Body.String(), "lookup") || !strings.Contains(response.Body.String(), "test") {
					t.Fatalf("tool call lost: %s", response.Body.String())
				}
				entry := waitForProxyLog(t, env, "model-free")
				if entry.StatusCode != 200 || entry.InputTokens != 11 || entry.OutputTokens != 7 {
					t.Fatalf("aggregated usage/status changed: %+v", entry)
				}
			})
		}
	}
}

func TestForceUpstreamStreamFailureAndClientMode(t *testing.T) {
	for _, tc := range []struct {
		name            string
		enabled, stream bool
		failure         string
	}{
		{name: "disabled"}, {name: "client stream", enabled: true, stream: true},
		{name: "truncated", enabled: true, failure: "truncated"}, {name: "rate limit", enabled: true, failure: "rate-limit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := setupProxyTestEnvWithSettings(t, []testChannel{{name: "forced", upstreamProtocol: "openai", models: "model-free", apiKey: "test-key", customRequestRules: &model.CustomRequestRules{ForceUpstreamStream: tc.enabled}}}, map[int]string{0: "https://upstream.example.com"}, map[string]string{"cooldown_fallback_enabled": "false"})
			prefix, suffix := openCodeFreeToolStreamFixture(protocol.OpenAI)
			env.server.client = &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					return nil, err
				}
				if gjson.GetBytes(body, "stream").Bool() != tc.enabled {
					t.Fatalf("unexpected stream mode: %s", body)
				}
				wire, contentType := prefix+suffix, "text/event-stream"
				if !tc.enabled {
					wire = `{"choices":[{"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":7}}`
					contentType = "application/json"
				}
				if tc.failure != "" {
					wire = prefix
				}
				if tc.failure == "rate-limit" {
					wire += "event: error\ndata: " + `{"error":{"type":"rate_limit_error","message":"quota exceeded"}}` + "\n\n"
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(wire))}, nil
			})}
			response := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{"model": "model-free", "stream": tc.stream, "messages": []any{map[string]any{"role": "user", "content": "hi"}}}, nil)
			if tc.failure != "" {
				if response.Code == 200 || bytes.Contains(response.Body.Bytes(), []byte("Let me check")) {
					t.Fatalf("partial success leaked: %d %s", response.Code, response.Body.String())
				}
				entry := waitForProxyLog(t, env, "model-free")
				want := util.StatusStreamIncomplete
				if tc.failure == "rate-limit" {
					want = http.StatusTooManyRequests
				}
				if entry.StatusCode != want {
					t.Fatalf("status=%d want=%d", entry.StatusCode, want)
				}
				return
			}
			if response.Code != 200 {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if tc.stream && !strings.Contains(response.Header().Get("Content-Type"), "text/event-stream") {
				t.Fatalf("client stream was buffered: %v", response.Header())
			}
			if !tc.stream && !json.Valid(response.Body.Bytes()) {
				t.Fatalf("nonstream response is not JSON: %s", response.Body.String())
			}
		})
	}
}

func TestForceUpstreamStreamExcludesNonGeneration(t *testing.T) {
	cfg := &model.Config{CustomRequestRules: &model.CustomRequestRules{ForceUpstreamStream: true}}
	for _, tc := range []struct {
		upstream protocol.Protocol
		path     string
	}{
		{protocol.Gemini, "/v1beta/models/test:countTokens"}, {protocol.OpenAI, "/v1/embeddings"}, {protocol.OpenAI, "/v1/images/generations"}, {protocol.Anthropic, "/v1/messages/count_tokens"}, {protocol.Codex, "/v1/responses/compact"},
	} {
		if shouldForceUpstreamStream(cfg, tc.upstream, tc.path) {
			t.Errorf("must not force %s", tc.path)
		}
	}
}

func TestForceUpstreamStreamAdminTest(t *testing.T) {
	for _, upstream := range []protocol.Protocol{protocol.Gemini, protocol.OpenAI} {
		t.Run(string(upstream), func(t *testing.T) {
			env := setupProxyTestEnv(t, []testChannel{{name: "forced", upstreamProtocol: string(upstream), protocolTransformMode: model.ProtocolTransformModeLocal, models: "model-free", apiKey: "test-key", customRequestRules: &model.CustomRequestRules{ForceUpstreamStream: true}}}, map[int]string{0: "https://upstream.example.com"})
			prefix, suffix := openCodeFreeToolStreamFixture(upstream)
			env.server.client = &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					return nil, err
				}
				if upstream == protocol.Gemini {
					if !strings.HasSuffix(r.URL.Path, ":streamGenerateContent") || r.URL.Query().Get("alt") != "sse" {
						t.Fatalf("admin did not stream: %s", r.URL)
					}
				} else if !gjson.GetBytes(body, "stream").Bool() {
					t.Fatalf("admin did not stream: %s", body)
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(prefix + suffix)), Request: r}, nil
			})}
			c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/1/test", map[string]any{"model": "model-free", "client_protocol": "openai", "stream": false, "content": "hello"}))
			c.Params = gin.Params{{Key: "id", Value: "1"}}
			env.server.HandleChannelTest(c)
			result := mustParseAPIResponse[map[string]any](t, w.Body.Bytes())
			if w.Code != 200 || !result.Success || result.Data["success"] != true || result.Data["is_streaming"] != false {
				t.Fatalf("admin aggregation failed: %s", w.Body.String())
			}
		})
	}
}
