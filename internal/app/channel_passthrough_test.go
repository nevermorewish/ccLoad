package app

import (
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

// compatibilityRules keeps fixtures that exercise opt-in wire repair explicit.
func compatibilityRules(rules *model.CustomRequestRules) *model.CustomRequestRules {
	if rules == nil {
		rules = &model.CustomRequestRules{}
	} else {
		rules = rules.Clone()
	}
	rules.Passthrough = new(bool)
	return rules
}

func TestChannelPassthroughDefaultPreservesWireAndUpstreamErrors(t *testing.T) {
	for _, tc := range []struct{ protocol, model, path, body, failure string }{
		{"codex", "gpt-5.6-sol", "/v1/responses", `{ "model":"gpt-5.6-sol", "input":[{"type":"reasoning","id":"caller-id","encrypted_content":"cipher"},{"type":"function_call","id":"custom-id","call_id":"call_1","name":"tool","arguments":"{}"}],"temperature":0.3,"stream":false,"tools":[{"type":"function","name":"tool","parameters":{}}],"custom_option":true}`, `{"error":{"code":"invalid_encrypted_content","message":"Encrypted content could not be decrypted"}}`},
		{"openai", "gpt-5", "/v1/chat/completions", `{ "model":"gpt-5","messages":[{"role":"user","content":"hello"}],"max_tokens":16,"stream":false,"custom_option":true}`, `{"error":{"code":"unsupported_parameter","param":"max_tokens","message":"Use max_completion_tokens"}}`},
		{"gemini", "gemini-3.6-flash", "/v1beta/models/gemini-3.6-flash:generateContent", `{ "contents":[{"role":"user","parts":[{"text":"hello","thoughtSignature":"opaque"}]}],"generationConfig":{"thinkingConfig":{"thinkingLevel":"invalid_value"}},"custom_option":true}`, `{"error":{"code":400,"message":"invalid thinking level"}}`},
		{"anthropic", "claude-sonnet-4-6", "/v1/messages", `{ "model":"claude-sonnet-4-6","max_tokens":32,"messages":[{"role":"user","content":"hello"}],"diagnostics":{"custom":true},"stream":false}`, `{"error":{"type":"invalid_request_error","message":"thinking.budget_tokens must be less than max_tokens"}}`},
	} {
		t.Run(tc.protocol, func(t *testing.T) {
			env := setupProxyTestEnv(t, []testChannel{{name: "passthrough", upstreamProtocol: tc.protocol, models: tc.model, apiKey: "upstream-key"}}, map[int]string{0: "https://gateway.example"})
			calls := 0
			env.server.client = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				body, _ := io.ReadAll(req.Body)
				if string(body) != tc.body {
					t.Errorf("wire body changed:\n%s\nwant:\n%s", body, tc.body)
				}
				if req.URL.Path != tc.path || req.URL.RawQuery != "custom=1" {
					t.Errorf("URL changed: %s", req.URL)
				}
				if req.Header.Get("X-Custom") != "keep" || req.Header.Get("User-Agent") != "caller/1" || req.Header.Get("Session-Id") != "caller-session" {
					t.Errorf("caller headers changed: %v", req.Header)
				}
				if strings.Contains(req.Header.Get("Authorization"), "test-api-key") || req.Header.Get("X-Hop") != "" {
					t.Error("client credentials or hop-by-hop header leaked")
				}
				return &http.Response{StatusCode: 400, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(tc.failure))}, nil
			})}
			req := httptest.NewRequest(http.MethodPost, tc.path+"?custom=1", strings.NewReader(tc.body))
			req.Header = http.Header{"Authorization": {"Bearer test-api-key"}, "Content-Type": {"application/json"}, "User-Agent": {"caller/1"}, "X-Custom": {"keep"}, "Session-Id": {"caller-session"}, "Connection": {"X-Hop"}, "X-Hop": {"drop"}}
			w := httptest.NewRecorder()
			env.engine.ServeHTTP(w, req)
			if calls != 1 || w.Code != 400 || w.Body.String() != tc.failure {
				t.Fatalf("calls=%d status=%d response=%s", calls, w.Code, w.Body.String())
			}
		})
	}
}

func TestChannelPassthroughAdminAndWebsocket(t *testing.T) {
	srv := newInMemoryServer(t)
	for _, upstream := range []string{"codex", "openai", "gemini"} {
		t.Run(upstream, func(t *testing.T) {
			cfg := &model.Config{URLs: model.ChannelURLs{{URL: "https://gateway.example"}}}
			input := &testutil.TestChannelRequest{Model: "test-model", Content: "hello", Headers: map[string]string{"User-Agent": "caller/1", "X-Custom": "keep"}}
			built, plan, err := srv.buildTestUpstreamRequestPlan(context.Background(), cfg, "key", input, input.Model, upstream, upstream, cfg.GetURLs()[0])
			if err != nil {
				t.Fatal(err)
			}
			req, cancel, err := srv.newTestUpstreamRequest(context.Background(), built, input, plan)
			if err != nil {
				t.Fatal(err)
			}
			defer cancel()
			body, _ := io.ReadAll(req.Body)
			if req.Header.Get("X-Custom") != "keep" || req.Header.Get("User-Agent") != "caller/1" || gjson.GetBytes(body, "prompt_cache_key").String() != gjson.GetBytes(plan.clientBody, "prompt_cache_key").String() {
				t.Fatalf("admin wire changed: %s %v", body, req.Header)
			}
		})
	}
	body := []byte(`{"input":[{"type":"function_call","id":"caller-custom-id","call_id":"call_1"}]}`)
	got, err := buildCodexWebsocketBody(body, true)
	if err != nil || gjson.GetBytes(got, "input.0.id").String() != "caller-custom-id" {
		t.Fatalf("websocket changed item: %s %v", got, err)
	}
	plan := protocol.TransformPlan{ClientProtocol: protocol.Codex, UpstreamProtocol: protocol.Codex, RequestFamily: protocol.RequestFamilyResponses}
	if got := responsesBodyForHTTPTransport(&model.Config{}, plan, []byte(`{"generate":true}`)); string(got) != `{"generate":true}` {
		t.Fatalf("HTTP body changed: %s", got)
	}
}
