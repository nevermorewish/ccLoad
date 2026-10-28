package app

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ccLoad/internal/model"
	"ccLoad/internal/protocol"
	"ccLoad/internal/testutil"

	"github.com/tidwall/gjson"
)

func TestProxyAnthropicPassthroughPreservesCallerWire(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "json", true: "sse"}[stream], func(t *testing.T) {
			env := setupProxyTestEnv(t, []testChannel{{
				name: "bedrock", upstreamProtocol: "anthropic", models: "claude-sonnet-4-6", apiKey: "upstream-key",
				customRequestRules: &model.CustomRequestRules{AnthropicPassthrough: true},
			}}, map[int]string{0: "https://bedrock-gateway.example"})
			body := `{"model":"claude-sonnet-4-6", "max_tokens":32,"system":"keep my prompt","messages":[{"role":"user","content":"hello"}],"metadata":{"user_id":"caller"},"temperature":0.3,"tools":[],"diagnostics":{"custom":true},"stream":false}`
			if stream {
				body = strings.Replace(body, `"stream":false`, `"stream":true`, 1)
			}
			calls := 0
			env.server.client = &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				got, _ := io.ReadAll(r.Body)
				if string(got) != body {
					t.Errorf("body changed:\n%s\nwant:\n%s", got, body)
				}
				if r.URL.RawQuery != "caller=1" {
					t.Errorf("query = %q", r.URL.RawQuery)
				}
				for k, v := range map[string]string{"User-Agent": "ccworkapi-test", "Anthropic-Beta": "caller-beta", "X-Custom": "preserved", "X-Api-Key": "upstream-key", "Authorization": "Bearer upstream-key"} {
					if r.Header.Get(k) != v {
						t.Errorf("%s = %q, want %q", k, r.Header.Get(k), v)
					}
				}
				if r.Header.Get("X-Hop") != "" || r.Header.Get("X-App") != "" {
					t.Errorf("unexpected headers: %v", r.Header)
				}
				payload := `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"OK"}],"model":"claude-sonnet-4-6","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
				contentType := "application/json"
				if stream {
					contentType = "text/event-stream"
					payload = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-sonnet-4-6\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(payload))}, nil
			})}
			req := httptest.NewRequest(http.MethodPost, "/v1/messages?caller=1", strings.NewReader(body))
			req.Header = http.Header{"Authorization": {"Bearer test-api-key"}, "Content-Type": {"application/json"}, "User-Agent": {"ccworkapi-test"}, "Anthropic-Beta": {"caller-beta"}, "X-Custom": {"preserved"}, "Connection": {"X-Hop"}, "X-Hop": {"drop"}}
			w := httptest.NewRecorder()
			env.engine.ServeHTTP(w, req)
			if w.Code != 200 || calls != 1 {
				t.Fatalf("status=%d calls=%d body=%s", w.Code, calls, w.Body.String())
			}
		})
	}
}

func TestAnthropicPassthroughCountTokensAndExplicitRules(t *testing.T) {
	srv := newInMemoryServer(t)
	cfg := &model.Config{CustomRequestRules: &model.CustomRequestRules{AnthropicPassthrough: true,
		Headers: []model.CustomHeaderRule{{Action: "append", Name: "X-Rule", Value: "once"}},
	}}
	body := []byte(`{ "model":"claude-sonnet-4-6","messages":[],"metadata":{"user_id":"keep"},"diagnostics":{"enabled":true} }`)
	ctx := &requestContext{ctx: context.Background(), upstreamProtocol: protocol.Anthropic, clientProtocol: protocol.Anthropic}
	req, err := srv.buildProxyRequest(ctx, cfg, "upstream-key", http.MethodPost, body, http.Header{"Content-Type": {"application/json"}}, "", "/v1/messages/count_tokens", "https://bedrock-gateway.example")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(req.Body)
	if !bytes.Equal(got, body) || req.URL.RawQuery != "" || req.Header.Get("Anthropic-Beta") != "" || req.Header.Get("X-Rule") != "once" {
		t.Fatalf("body=%s url=%s headers=%v", got, req.URL, req.Header)
	}
	cfg.CustomRequestRules.Body = []model.CustomBodyRule{{Action: "remove", Path: "diagnostics"}}
	prepared, _, err := srv.prepareTranslatedUpstreamBody(cfg, protocol.Anthropic, "/v1/messages", "claude-sonnet-4-6", body, body, "key", req.Header, false, req.URL, false, true)
	if err != nil || gjson.GetBytes(prepared, "diagnostics").Exists() || gjson.GetBytes(prepared, "metadata.user_id").String() != "keep" {
		t.Fatalf("explicit rule: %s, %v", prepared, err)
	}
}

func TestAdminAnthropicPassthroughUsesPlainMessages(t *testing.T) {
	srv := newInMemoryServer(t)
	cfg := &model.Config{URLs: model.ChannelURLs{{URL: "https://bedrock-gateway.example"}}, CustomRequestRules: &model.CustomRequestRules{AnthropicPassthrough: true}}
	for _, client := range []string{"anthropic", "openai"} {
		t.Run(client, func(t *testing.T) {
			input := &testutil.TestChannelRequest{Model: "claude-sonnet-4-6", Content: "Reply OK", MaxTokens: 32, Headers: map[string]string{"User-Agent": "probe", "X-Custom": "keep"}}
			built, plan, err := srv.buildTestUpstreamRequestPlan(context.Background(), cfg, "upstream-key", input, input.Model, client, "anthropic", cfg.GetURLs()[0])
			if err != nil {
				t.Fatal(err)
			}
			req, cancel, err := srv.newTestUpstreamRequest(context.Background(), built, input, plan)
			if err != nil {
				t.Fatal(err)
			}
			defer cancel()
			body, _ := io.ReadAll(req.Body)
			if req.Header.Get("Anthropic-Beta") != "" || req.Header.Get("User-Agent") != "probe" || req.Header.Get("X-Custom") != "keep" || req.Header.Get("X-Api-Key") != "upstream-key" || req.URL.RawQuery != "" {
				t.Errorf("headers=%v url=%s", req.Header, req.URL)
			}
			if strings.Contains(string(body), "Claude Code") || strings.Contains(string(body), "cache_control") || gjson.GetBytes(body, "tools.#").Int() != 0 {
				t.Errorf("injected body: %s", body)
			}
			if client == "anthropic" && (!bytes.Equal(body, plan.clientBody) || gjson.GetBytes(body, "max_tokens").Int() != 32) {
				t.Errorf("plain test body changed: %s", body)
			}
		})
	}
}

func TestAnthropicPassthroughDefaultsOnAndAPIKeyOnly(t *testing.T) {
	for _, auth := range []string{model.AuthTypeAPIKey, model.AuthTypeAnthropicOAuth, model.AuthTypeCodexOAuth} {
		cfg := &model.Config{AuthType: auth, CustomRequestRules: &model.CustomRequestRules{AnthropicPassthrough: true}}
		if got := isAnthropicPassthroughRequest(cfg, protocol.Anthropic, "/v1/messages"); got != (auth == model.AuthTypeAPIKey) {
			t.Errorf("auth=%s enabled=%v", auth, got)
		}
		if isAnthropicPassthroughRequest(cfg, protocol.OpenAI, "/v1/chat/completions") {
			t.Fatal("affected OpenAI")
		}
	}
	if !isAnthropicPassthroughRequest(&model.Config{}, protocol.Anthropic, "/v1/messages") {
		t.Fatal("disabled by default")
	}
}
