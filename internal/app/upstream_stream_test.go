package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/protocol"
	"ccLoad/internal/testutil"
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
					if upstream == protocol.OpenAI && !gjson.GetBytes(body, "stream_options.include_usage").Bool() {
						t.Fatalf("aggregation must request usage: %s", body)
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

func TestForceUpstreamStreamPreservesClientUsageOptions(t *testing.T) {
	for _, options := range []any{nil, map[string]any{"include_usage": false, "include_obfuscation": true}, map[string]any{"include_usage": true}} {
		encoded, err := json.Marshal(options)
		if err != nil {
			t.Fatal(err)
		}
		t.Run(string(encoded), func(t *testing.T) {
			env := setupProxyTestEnv(t, []testChannel{{name: "forced", upstreamProtocol: "openai", models: "test", apiKey: "key", customRequestRules: &model.CustomRequestRules{ForceUpstreamStream: true}}}, map[int]string{0: "https://upstream.example.com"})
			const content = "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"OK\"},\"finish_reason\":\"stop\"}]}\n\n"
			const usage = "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":7}}\n\n"
			expected := content
			if options != nil && options.(map[string]any)["include_usage"] == true {
				expected += usage
			}
			expected += "data: [DONE]\n\n"
			env.server.client = &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					return nil, err
				}
				got := gjson.GetBytes(body, "stream_options")
				if (options == nil && got.Exists()) || (options != nil && got.Raw != string(encoded)) {
					t.Fatalf("client options changed: %s", body)
				}
				if _, ok := r.Context().Deadline(); ok {
					t.Fatal("streaming client inherited nonstream deadline")
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(expected))}, nil
			})}
			request := map[string]any{"model": "test", "stream": true, "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
			if options != nil {
				request["stream_options"] = options
			}
			response := doProxyRequest(t, env.engine, "/v1/chat/completions", request, nil)
			if response.Code != 200 || response.Body.String() != expected {
				t.Fatalf("stream changed: %d %s", response.Code, response.Body.String())
			}
		})
	}
}

func TestForceUpstreamStreamResignsAnthropicCCH(t *testing.T) {
	srv := newInMemoryServer(t)
	const billing = "x-anthropic-billing-header: cc_version=2.1.280.abc; cc_entrypoint=cli; cch=00000;"
	cfg := &model.Config{ID: 1, Name: "forced", CustomRequestRules: compatibilityRules(&model.CustomRequestRules{ForceUpstreamStream: true})}
	headers := http.Header{"User-Agent": {"claude-cli/2.1.280 (external, cli)"}, "X-App": {"cli"}, "Anthropic-Beta": {"claude-code-20250219"}}
	for _, admin := range []bool{false, true} {
		name := "proxy"
		if admin {
			name = "admin"
		}
		t.Run(name, func(t *testing.T) {
			var req *http.Request
			var err error
			if admin {
				cfg = cfg.Clone()
				cfg.CustomRequestRules.Body = []model.CustomBodyRule{{Action: model.RuleActionOverride, Path: "system", Value: json.RawMessage(`[{"type":"text","text":"` + billing + `"}]`)}}
				testReq := &testutil.TestChannelRequest{Model: "claude-sonnet-4-6", SystemPrompt: billing, Content: "hello", ClientProtocol: "anthropic"}
				built, plan, buildErr := srv.buildTestUpstreamRequestPlan(context.Background(), cfg, "test-key", testReq, testReq.Model, "anthropic", "anthropic", "https://upstream.example.com")
				if buildErr != nil {
					t.Fatal(buildErr)
				}
				if _, ok := anthropicCCHDigitsOffset(plan.requestBody); !ok {
					t.Fatal("fixture has no CCH")
				}
				var cancel context.CancelFunc
				req, cancel, err = srv.newTestUpstreamRequest(context.Background(), built, testReq, plan)
				if err != nil {
					t.Fatal(err)
				}
				defer cancel()
			} else {
				body, marshalErr := json.Marshal(map[string]any{"model": "claude-sonnet-4-6", "stream": false, "max_tokens": 64, "system": []any{map[string]any{"type": "text", "text": billing}}, "metadata": map[string]any{"user_id": "test-native"}, "messages": []any{map[string]any{"role": "user", "content": "hello"}}})
				if marshalErr != nil {
					t.Fatal(marshalErr)
				}
				body, err = finalizeAnthropicCCH(body)
				if err != nil {
					t.Fatal(err)
				}
				if !classifyAnthropicCallerWire(body, headers).ownsWire() {
					t.Fatal("fixture must own native wire")
				}
				plan := mustBuildTestTransformPlan(t, cfg, body)
				ctx := &requestContext{ctx: context.Background(), startTime: time.Now(), transformPlan: plan, forceUpstreamStream: true}
				req, err = srv.buildProxyRequest(ctx, cfg, "test-key", http.MethodPost, body, headers, "", "/v1/messages", "https://upstream.example.com")
				if err != nil {
					t.Fatal(err)
				}
			}
			body, err := io.ReadAll(req.Body)
			if err != nil {
				t.Fatal(err)
			}
			if err = req.Body.Close(); err != nil {
				t.Fatal(err)
			}
			if !gjson.GetBytes(body, "stream").Bool() {
				t.Fatalf("not streamed: %s", body)
			}
			signed, err := finalizeAnthropicCCH(body)
			if err != nil || !bytes.Equal(signed, body) {
				t.Fatalf("CCH does not match final wire: %v %s", err, body)
			}
		})
	}
}

func TestForceUpstreamStreamRetainsNonStreamDeadline(t *testing.T) {
	for _, admin := range []bool{false, true} {
		for _, budget := range []time.Duration{0, 10 * time.Minute, 37 * time.Second} {
			name := "proxy/" + budget.String()
			if admin {
				name = "admin/" + budget.String()
			}
			t.Run(name, func(t *testing.T) {
				srv := newInMemoryServer(t)
				srv.nonStreamTimeout = budget
				srv.firstByteTimeout = 0
				srv.streamTimeout = 0
				srv.streamIdleTimeout = 0
				srv.protocolTimeouts = nil
				cfg := &model.Config{ID: 1, Name: "forced", CustomRequestRules: &model.CustomRequestRules{ForceUpstreamStream: true}}
				start := time.Now()
				check := func(ctx context.Context) {
					deadline, ok := ctx.Deadline()
					if ok != (budget > 0) {
						t.Fatalf("deadline exists=%v, budget=%s", ok, budget)
					}
					if ok && (deadline.Before(start.Add(budget)) || deadline.After(time.Now().Add(budget))) {
						t.Fatalf("wrong deadline %s", deadline)
					}
				}
				if admin {
					plan := &channelTestRequestPlan{upstreamProtocol: "openai", forceUpstreamStream: true, fullURL: "https://upstream.example.com/v1/chat/completions", endpointPath: "/v1/chat/completions", requestBody: []byte(`{"model":"test","stream":true}`), headers: make(http.Header)}
					req, cancel, err := srv.newTestUpstreamRequest(context.Background(), cfg, &testutil.TestChannelRequest{Model: "test"}, plan)
					if err != nil {
						t.Fatal(err)
					}
					defer cancel()
					check(req.Context())
					cancel()
					if req.Context().Err() != context.Canceled {
						t.Fatal("request cancellation did not propagate")
					}
					return
				}
				srv.client = &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
					check(r.Context())
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"x","type":"message","role":"assistant","content":[{"type":"text","text":"OK"}],"stop_reason":"end_turn"}`))}, nil
				})}
				body := []byte(`{"model":"test","stream":false,"messages":[{"role":"user","content":"hi"}]}`)
				plan := mustBuildTestTransformPlan(t, cfg, body)
				result, _, err := srv.forwardOnceAsync(context.Background(), cfg, "key", http.MethodPost, plan, nil, "", "https://upstream.example.com", newRecorder(), nil)
				if err != nil || result == nil || result.Status != 200 {
					t.Fatalf("forward: %v %+v", err, result)
				}
			})
		}
	}
}

func TestForceUpstreamStreamJSONFirstByte(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := newRequestContextForStreaming(context.Background(), true, protocolTimeoutConfig{FirstByteTimeout: time.Second})
		defer ctx.cleanup()
		ctx.isStreaming = false
		ctx.forceUpstreamStream = true
		response := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json; charset=utf-8"}}, Body: io.NopCloser(strings.NewReader(`{"choices":[]}`))}
		attachFirstByteDetector(ctx, response, &streamReadStats{}, nil)
		defer func() { _ = response.Body.Close() }()
		if _, err := response.Body.Read(make([]byte, 1)); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Second) // Virtual time: the remainder of the JSON has not arrived yet.
		if ctx.ctx.Err() != nil || ctx.firstByteTimeoutTriggered() {
			t.Fatal("JSON first byte did not stop first-byte timeout")
		}
	})
}
