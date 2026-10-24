package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/protocol"
	"ccLoad/internal/util"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const emptyChatCompletion = `{"choices":[{"finish_reason":"stop","index":0,"message":{"role":"assistant"}}],"id":"empty-chat","model":"gpt-6.1-sol","object":"chat.completion","usage":{"prompt_tokens":15,"completion_tokens":0,"total_tokens":15}}`

func TestNonStreamChatOutputValidation(t *testing.T) {
	t.Parallel()
	chat := func(message, finish, usage string) string {
		return fmt.Sprintf(`{"choices":[{"finish_reason":%q,"message":%s}],"usage":%s}`, finish, message, usage)
	}
	for _, tc := range []struct {
		name  string
		body  string
		empty bool
	}{
		{"captured_empty", emptyChatCompletion, true},
		{"null_content", chat(`{"role":"assistant","content":null}`, "stop", `{}`), true},
		{"empty_text", chat(`{"role":"assistant","content":""}`, "stop", `{}`), true},
		{"empty_fields", chat(`{"role":"assistant","content":[],"tool_calls":[],"function_call":{},"refusal":null,"reasoning_content":""}`, "stop", `{}`), true},
		{"text", chat(`{"role":"assistant","content":"pong"}`, "stop", `{"completion_tokens":0}`), false},
		{"whitespace", chat(`{"role":"assistant","content":" "}`, "stop", `{}`), false},
		{"tools", chat(`{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]}`, "stop", `{}`), false},
		{"legacy_function", chat(`{"role":"assistant","function_call":{"name":"lookup","arguments":"{}"}}`, "stop", `{}`), false},
		{"refusal", chat(`{"role":"assistant","refusal":"Cannot comply"}`, "stop", `{}`), false},
		{"reasoning_content", chat(`{"role":"assistant","reasoning_content":"thinking"}`, "stop", `{}`), false},
		{"reasoning", chat(`{"role":"assistant","reasoning":"thinking"}`, "stop", `{}`), false},
		{"reasoning_details", chat(`{"role":"assistant","reasoning_details":[{"text":"thinking"}]}`, "stop", `{}`), false},
		{"audio", chat(`{"role":"assistant","audio":{"data":"YQ=="}}`, "stop", `{}`), false},
		{"multimodal_content", chat(`{"role":"assistant","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,YQ=="}}]}`, "stop", `{}`), false},
		{"extension", chat(`{"role":"assistant","future_output":{"data":"x"}}`, "stop", `{}`), false},
		{"truncated", chat(`{"role":"assistant"}`, "length", `{}`), false},
		{"filtered", chat(`{"role":"assistant"}`, "content_filter", `{}`), false},
		{"tool_finish", chat(`{"role":"assistant"}`, "tool_calls", `{}`), false},
		{"no_finish", chat(`{"role":"assistant"}`, "", `{}`), false},
		{"hidden_output", chat(`{"role":"assistant"}`, "stop", `{"completion_tokens":8}`), false},
		{"hidden_output_alias", chat(`{"role":"assistant"}`, "stop", `{"output_tokens":8}`), false},
		{"hidden_reasoning", chat(`{"role":"assistant"}`, "stop", `{"completion_tokens_details":{"reasoning_tokens":8}}`), false},
		{"missing_message", chat(`null`, "stop", `{}`), false},
		{"wrong_role", chat(`{"role":"user"}`, "stop", `{}`), false},
		{"mixed_choices", `{"choices":[{"finish_reason":"stop","message":{"role":"assistant"}},{"finish_reason":"stop","message":{"role":"assistant","content":"pong"}}]}`, false},
		{"no_choices", `{"choices":[],"usage":{"completion_tokens":0}}`, false},
		{"other_protocol", `{"type":"message","content":[],"stop_reason":"end_turn"}`, false},
		{"last_duplicate_wins_empty", chat(`{"role":"assistant","content":"pong","content":""}`, "stop", `{}`), true},
		{"last_duplicate_wins_text", chat(`{"role":"assistant","content":"","content":"pong"}`, "stop", `{}`), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reqCtx := &requestContext{ctx: context.Background(), startTime: time.Now()}
			resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(tc.body))}
			rec := newRecorder()
			result, _, err := (&Server{}).handleResponse(reqCtx, resp, rec, "openai", &model.Config{ID: 1}, "test", nil)
			if tc.empty {
				if !errors.Is(err, util.ErrUpstreamEmptyResponse) || result == nil || result.ResponseCommitted || rec.Body.Len() != 0 {
					t.Fatalf("expected uncommitted empty-response failure: result=%+v err=%v body=%s", result, err, rec.Body.String())
				}
				if string(result.Body) != tc.body {
					t.Fatal("raw upstream body not preserved for diagnostics")
				}
			} else if err != nil || result == nil || !result.ResponseCommitted || rec.Body.String() != tc.body {
				t.Fatalf("legitimate response changed: result=%+v err=%v body=%s", result, err, rec.Body.String())
			}
		})
	}
}

func TestProxy_NonStreamingEmptyAssistant(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		path     string
		fallback bool
	}{
		{"chat_fallback", "/v1/chat/completions", true},
		{"messages_fallback", "/v1/messages", true},
		{"chat_exhausted", "/v1/chat/completions", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var badCalls, goodCalls atomic.Int32
			bad := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				badCalls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(emptyChatCompletion))
			}))
			defer bad.Close()
			good := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				goodCalls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"fallback","choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"pong"}}],"usage":{"prompt_tokens":15,"completion_tokens":1}}`))
			}))
			defer good.Close()
			channels := []testChannel{{name: "bad", models: "gpt-6.1-sol", apiKey: "test-bad", priority: 100, upstreamProtocol: "openai", protocolTransformMode: model.ProtocolTransformModeLocal}}
			urls := map[int]string{0: bad.URL}
			if tc.fallback {
				channels = append(channels, testChannel{name: "good", models: "gpt-6.1-sol", apiKey: "test-good", priority: 50, upstreamProtocol: "openai", protocolTransformMode: model.ProtocolTransformModeLocal})
				urls[1] = good.URL
			}
			env := setupProxyTestEnv(t, channels, urls)
			w := doProxyRequest(t, env.engine, tc.path, map[string]any{
				"model": "gpt-6.1-sol", "stream": false, "max_tokens": 16,
				"messages": []map[string]string{{"role": "user", "content": "Reply with the single word: pong"}},
			}, nil)
			if tc.fallback {
				if w.Code != http.StatusOK || goodCalls.Load() != 1 || !strings.Contains(w.Body.String(), "pong") {
					t.Fatalf("expected healthy fallback: status=%d calls=%d body=%s", w.Code, goodCalls.Load(), w.Body.String())
				}
			} else if w.Code < 500 {
				t.Fatalf("expected upstream failure when exhausted: status=%d body=%s", w.Code, w.Body.String())
			}
			if badCalls.Load() != 1 || strings.Contains(w.Body.String(), "empty-chat") {
				t.Fatalf("empty response leaked or was retried: calls=%d body=%s", badCalls.Load(), w.Body.String())
			}
			configs, err := env.store.ListConfigs(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			cooldowns, err := env.store.GetAllModelCooldowns(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			for _, cfg := range configs {
				if cfg.Name == "bad" && !cooldowns[cfg.ID]["gpt-6.1-sol"].After(time.Now()) {
					t.Fatal("empty assistant did not trigger model cooldown")
				}
			}
		})
	}
}

func TestGeminiNonStreamCompletionValidation(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"no candidates", `{}`, 502},
		{"empty candidates", `{"candidates":[]}`, 502},
		{"empty parts", `{"candidates":[{"content":{"role":"model","parts":[]},"finishReason":"STOP"}],"usageMetadata":{"candidatesTokenCount":8}}`, 502},
		{"empty text", `{"candidates":[{"content":{"parts":[{"text":""},{"thoughtSignature":"opaque"}]},"finishReason":"STOP"}]}`, 502},
		{"invalid JSON", `{"candidates":`, 502},
		{"JSON null", `null`, 502},
		{"embedded 429", `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","message":"quota exhausted"}}`, 429},
		{"embedded 503", `{"error":{"code":503,"status":"UNAVAILABLE","message":"model unavailable"}}`, 503},
		{"text with zero tokens", `{"candidates":[{"content":{"parts":[{"text":"OK"}]},"finishReason":"STOP"}],"usageMetadata":{"candidatesTokenCount":0}}`, 200},
		{"tool call", `{"candidates":[{"content":{"parts":[{"functionCall":{"name":"lookup","args":{}}}]},"finishReason":"STOP"}]}`, 200},
		{"image", `{"candidates":[{"content":{"parts":[{"inlineData":{"mimeType":"image/png","data":"YQ=="}}]},"finishReason":"STOP"}]}`, 200},
		{"reasoning", `{"candidates":[{"content":{"parts":[{"text":"thinking","thought":true}]},"finishReason":"STOP"}]}`, 200},
		{"future output", `{"candidates":[{"content":{"parts":[{"futureOutput":{"value":"x"}}]},"finishReason":"STOP"}]}`, 200},
		{"prompt blocked", `{"promptFeedback":{"blockReason":"SAFETY"}}`, 200},
		{"candidate blocked", `{"candidates":[{"finishReason":"SAFETY"}]}`, 200},
		{"token limit", `{"candidates":[{"finishReason":"MAX_TOKENS"}]}`, 200},
		{"mixed candidates", `{"candidates":[{"finishReason":"STOP"},{"content":{"parts":[{"text":"OK"}]},"finishReason":"STOP"}]}`, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := &requestContext{ctx: context.Background(), startTime: time.Now(), transformPlan: protocol.TransformPlan{UpstreamPath: "/v1beta/models/test:generateContent"}}
			resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(tc.body))}
			rec := newRecorder()
			result, _, err := (&Server{}).handleResponse(ctx, resp, rec, "gemini", &model.Config{ID: 1}, "test", nil)
			if tc.status == 200 {
				if err != nil || result == nil || !result.ResponseCommitted || rec.Body.String() != tc.body {
					t.Fatalf("legitimate response changed: result=%+v err=%v body=%s", result, err, rec.Body.String())
				}
				return
			}
			if result == nil || result.ResponseCommitted || rec.Body.Len() != 0 {
				t.Fatalf("failure committed: result=%+v err=%v body=%s", result, err, rec.Body.String())
			}
			status := result.Status
			if err != nil {
				status, _, _ = util.ClassifyError(err)
			}
			if status != tc.status || string(result.Body) != tc.body {
				t.Fatalf("status=%d want=%d body=%s err=%v", status, tc.status, result.Body, err)
			}
		})
	}
	status, err := geminiCompletionFailure("/v1beta/models/test:countTokens", []byte(`{"totalTokens":10}`))
	if status != 0 || err != nil {
		t.Fatalf("token counting rejected: %d %v", status, err)
	}
}

func TestProxy_GeminiEmptyAndEmbeddedErrors(t *testing.T) {
	const empty = `{"candidates":[{"content":{"role":"model","parts":[]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":0}}`
	const good = `{"candidates":[{"content":{"role":"model","parts":[{"text":"OK"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":1}}`
	const quota = `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","message":"quota exhausted"}}`
	for _, tc := range []struct {
		name                               string
		force, fallback, translated, admin bool
		body                               string
		status                             int
	}{
		{name: "admin empty", admin: true, body: empty, status: 502},
		{name: "admin forced empty", admin: true, force: true, body: empty, status: 502},
		{name: "admin embedded quota", admin: true, body: quota, status: 429},
		{name: "native empty", body: empty, status: 502},
		{name: "forced stream empty", force: true, body: empty, status: 502},
		{name: "converted empty", translated: true, body: empty, status: 502},
		{name: "forced converted empty", force: true, translated: true, body: empty, status: 502},
		{name: "embedded quota", body: quota, status: 429},
		{name: "forced embedded quota", force: true, body: quota, status: 429},
		{name: "long converted error", translated: true, body: `{"padding":"` + strings.Repeat("x", 1024) + `","error":{"code":429,"status":"RESOURCE_EXHAUSTED","message":"quota exhausted"}}`, status: 429},
		{name: "native fallback", fallback: true, body: empty, status: 200},
		{name: "forced fallback", force: true, fallback: true, body: empty, status: 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			channels := []testChannel{{name: "bad", models: "test", apiKey: "key-bad", priority: 100, upstreamProtocol: "gemini", protocolTransformMode: model.ProtocolTransformModeLocal, customRequestRules: &model.CustomRequestRules{ForceUpstreamStream: tc.force}}}
			urls := map[int]string{0: "https://bad.example.com"}
			if tc.fallback {
				channels = append(channels, testChannel{name: "good", models: "test", apiKey: "key-good", priority: 50, upstreamProtocol: "gemini", protocolTransformMode: model.ProtocolTransformModeLocal})
				urls[1] = "https://good.example.com"
			}
			env := setupProxyTestEnvWithSettings(t, channels, urls, map[string]string{"cooldown_fallback_enabled": "false"})
			var hosts []string
			env.server.client = &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				hosts = append(hosts, r.URL.Host)
				body, ct := tc.body, "application/json"
				if r.URL.Host == "good.example.com" {
					body = good
				} else if tc.force && tc.body == empty {
					body = "data: " + body + "\n\n"
					ct = "text/event-stream"
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {ct}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
			})}
			// The admin test must report the same failure instead of a green empty reply.
			if tc.admin {
				c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/1/test", map[string]any{"model": "test", "client_protocol": "gemini", "stream": false, "content": "hi"}))
				c.Params = gin.Params{{Key: "id", Value: "1"}}
				env.server.HandleChannelTest(c)
				data := gjson.GetBytes(w.Body.Bytes(), "data")
				if data.Get("success").Bool() || int(data.Get("status_code").Int()) != tc.status || data.Get("error").String() == "" {
					t.Fatalf("admin returned empty success: %s", w.Body.String())
				}
				return
			}
			path := "/v1beta/models/test:generateContent"
			request := map[string]any{"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": "hi"}}}}}
			if tc.translated {
				path = "/v1/chat/completions"
				request = map[string]any{"model": "test", "stream": false, "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
			}
			w := doProxyRequest(t, env.engine, path, request, nil)
			if w.Code != tc.status {
				t.Fatalf("status=%d want=%d body=%s", w.Code, tc.status, w.Body.String())
			}
			if tc.fallback {
				if len(hosts) != 2 || hosts[0] != "bad.example.com" || hosts[1] != "good.example.com" || !strings.Contains(w.Body.String(), "OK") {
					t.Fatalf("fallback failed: hosts=%v body=%s", hosts, w.Body.String())
				}
			} else {
				message := gjson.GetBytes(w.Body.Bytes(), "error.message").String()
				if len(hosts) != 1 || message == "" {
					t.Fatalf("missing error: hosts=%v body=%s", hosts, w.Body.String())
				}
				if tc.status == 429 && message != "quota exhausted" {
					t.Fatalf("upstream error lost: %s", w.Body.String())
				}
				if tc.status == 502 && !strings.Contains(message, "without Gemini candidate output") {
					t.Fatalf("empty-response reason lost: %s", w.Body.String())
				}
			}
		})
	}
}
