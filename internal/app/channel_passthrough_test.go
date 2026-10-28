package app

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
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

func TestChannelPassthroughLegacyModesIgnoreLearnedProtocols(t *testing.T) {
	for _, tc := range []struct {
		mode    string
		cached  protocol.Protocol
		status  int
		failure string
	}{
		{"", protocol.Codex, 500, `{"error":{"message":"field messages is required","type":"new_api_error"}}`},
		{model.ProtocolTransformModeAuto, protocol.Codex, 404, `{"error":{"message":"endpoint not found"}}`},
		{model.ProtocolTransformModeLocal, protocolUnsupported, 400, `{"error":{"message":"unsupported anthropic-beta"}}`},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			const body = `{ "model":"claude-sonnet-4-6", "messages":[{"role":"user","content":"hello"}], "stream":false,"custom_option":{"keep":true} }`
			env := setupProxyTestEnv(t, []testChannel{
				{name: "legacy", upstreamProtocol: "anthropic", protocolTransformMode: tc.mode, models: "claude-sonnet-4-6", customRequestRules: &model.CustomRequestRules{AnthropicPassthrough: true}},
				{name: "native", upstreamProtocol: "anthropic", models: "claude-sonnet-4-6"},
			}, map[int]string{0: "https://first.example", 1: "https://second.example"})
			configs, err := env.store.ListConfigs(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			for _, cfg := range configs {
				if cfg.Name != "legacy" {
					continue
				}
				// The fixture defaults empty modes to local; restore the actual
				// legacy value without changing the URL's native protocol.
				cfg.ProtocolTransformMode = tc.mode
				if _, err := env.store.UpdateConfig(context.Background(), cfg.ID, cfg); err != nil {
					t.Fatal(err)
				}
				env.server.protocolCapabilities.set(protocolCapabilityKey{
					channelID: cfg.ID, baseURL: "https://first.example", clientProtocol: protocol.Anthropic,
					requestFamily: protocol.RequestFamilyMessages, upstreamModel: "claude-sonnet-4-6",
				}, tc.cached)
			}
			var hosts []string
			env.server.client = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				hosts = append(hosts, req.URL.Host)
				wire, err := io.ReadAll(req.Body)
				if err != nil {
					t.Fatal(err)
				}
				if string(wire) != body || req.URL.Path != "/v1/messages" || req.URL.RawQuery != "caller=1" {
					t.Errorf("request changed: url=%s body=%s", req.URL, wire)
				}
				status, response := http.StatusOK, `{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`
				if req.URL.Host == "first.example" {
					status, response = tc.status, tc.failure
				}
				return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(response))}, nil
			})}
			req := httptest.NewRequest(http.MethodPost, "/v1/messages?caller=1", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer test-api-key")
			w := httptest.NewRecorder()
			env.engine.ServeHTTP(w, req)
			if w.Code != http.StatusOK || gjson.Get(w.Body.String(), "content.0.text").String() != "ok" {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			if want := []string{"first.example", "second.example"}; !reflect.DeepEqual(hosts, want) {
				t.Fatalf("hosts=%v, want %v; protocol probing must not retry the first channel", hosts, want)
			}
		})
	}
}

func TestChannelPassthroughRejectsCrossProtocolPlans(t *testing.T) {
	srv := newInMemoryServer(t)
	cfg := &model.Config{URLs: model.ChannelURLs{{URL: "https://gateway.example"}}}
	input := &testutil.TestChannelRequest{Model: "claude-sonnet-4-6", Content: "hello"}
	for _, mode := range []string{"", model.ProtocolTransformModeAuto, model.ProtocolTransformModeLocal} {
		cfg.ProtocolTransformMode = mode
		if _, _, err := srv.buildTestUpstreamRequestPlan(context.Background(), cfg, "key", input, input.Model, "anthropic", "codex", cfg.GetURLs()[0]); err == nil {
			t.Fatalf("mode=%q: admin builder accepted protocol conversion", mode)
		}
		candidates := resolveConfiguredURLUpstreamProtocols(cfg, model.ChannelURL{URL: cfg.GetURLs()[0], Protocols: []string{"codex"}}, "anthropic")
		if len(candidates) != 0 {
			t.Fatalf("mode=%q: incompatible URL accepted: %v", mode, candidates)
		}
		candidates = resolveConfiguredURLUpstreamProtocols(cfg, model.ChannelURL{URL: cfg.GetURLs()[0]}, "anthropic")
		if !reflect.DeepEqual(candidates, []string{"anthropic"}) {
			t.Fatalf("mode=%q: candidates=%v", mode, candidates)
		}
	}
	srv.client = &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("cross-protocol plan reached the network")
		return nil, nil
	})}
	plan := protocol.TransformPlan{ClientProtocol: protocol.Anthropic, UpstreamProtocol: protocol.Codex, NeedsTransform: true}
	_, _, err := srv.forwardOnceAsyncWithNativeCodexWebsocket(context.Background(), cfg, "key", http.MethodPost, plan, nil, "", cfg.GetURLs()[0], httptest.NewRecorder(), nil, nil, "", nil, false, upstreamWireAliases{})
	if err == nil || !strings.Contains(err.Error(), "protocol conversion is disabled") {
		t.Fatalf("err=%v", err)
	}
}

func TestChannelPassthroughDoesNotTranslateUpstreamResponse(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "json", true: "sse"}[stream], func(t *testing.T) {
			env := setupProxyTestEnv(t, []testChannel{{name: "native", upstreamProtocol: "openai", models: "test-model"}}, map[int]string{0: "https://gateway.example"})
			payload := `{"id":"msg_1","type":"message","role":"assistant","model":"test-model","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`
			contentType := "application/json"
			if stream {
				contentType = "text/event-stream"
				payload = "event: message_start\ndata: " + payload + "\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
			}
			env.server.client = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				if req.URL.Path != "/v1/chat/completions" {
					t.Errorf("path=%s", req.URL.Path)
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(payload))}, nil
			})}
			w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{"model": "test-model", "messages": []map[string]string{{"role": "user", "content": "hi"}}, "stream": stream}, nil)
			if w.Code != 200 || w.Body.String() != payload {
				t.Fatalf("status=%d response=%s, want original=%s", w.Code, w.Body.String(), payload)
			}
		})
	}
}
