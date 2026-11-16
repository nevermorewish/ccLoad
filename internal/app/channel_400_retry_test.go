package app

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"testing"

	"ccLoad/internal/model"
)

func TestChannel400RetryPolicy(t *testing.T) {
	for _, tc := range []struct {
		name     string
		disabled bool
		status   int
		body     string
	}{
		{"disabled generic", true, 400, `{"error":{"message":"response_format.type must be one of 'text', 'json_object', 'json_schema'"}}`},
		{"disabled repair", true, 400, `{"error":{"message":"Unsupported parameter: 'max_tokens' is not supported with this model. Use 'max_completion_tokens' instead."}}`},
		{"disabled native fallback", true, 400, `{"error":{"message":"temperature is not supported"}}`},
		{"default fallback", false, 400, `{"error":{"message":"response_format.type must be one of 'text', 'json_object', 'json_schema'"}}`},
		{"server error unaffected", true, 500, `{"error":{"message":"internal server error"}}`},
		{"rate limit unaffected", true, 429, `{"error":{"message":"rate limit exceeded"}}`},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", tc.name, stream), func(t *testing.T) {
				var primaryCalls, fallbackCalls atomic.Int32
				primary := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					primaryCalls.Add(1)
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(tc.status)
					_, _ = io.WriteString(w, tc.body)
				}))
				t.Cleanup(primary.Close)
				fallback := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					fallbackCalls.Add(1)
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"id":"fallback","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
				}))
				t.Cleanup(fallback.Close)
				env := setupProxyTestEnv(t, []testChannel{
					{name: "primary", models: "gpt-test", priority: 100, sameChannelRetries: 2, retryOtherKeysOnFailure: true},
					{name: "fallback", models: "gpt-test", priority: 50},
				}, map[int]string{0: primary.URL, 1: fallback.URL})
				ctx := context.Background()
				cfg, err := env.store.GetConfig(ctx, 1)
				if err != nil {
					t.Fatal(err)
				}
				cfg.Disable400Retry = tc.disabled
				cfg.URLs = append(cfg.URLs, model.ChannelURL{URL: primary.URL + "/second"})
				if _, err := env.store.UpdateConfig(ctx, 1, cfg); err != nil {
					t.Fatal(err)
				}
				if err := env.store.CreateAPIKeysBatch(ctx, []*model.APIKey{{ChannelID: 1, KeyIndex: 1, APIKey: "fixture-second"}}); err != nil {
					t.Fatal(err)
				}
				env.server.InvalidateChannelListCache()
				response := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
					"model": "gpt-test", "stream": stream, "max_tokens": 50,
					"messages": []map[string]string{{"role": "user", "content": "hello"}},
				}, nil)
				if tc.disabled && tc.status == 400 {
					if response.Code != 400 || response.Body.String() != tc.body || primaryCalls.Load() != 1 || fallbackCalls.Load() != 0 {
						t.Fatalf("response=%d %s calls=%d/%d", response.Code, response.Body.String(), primaryCalls.Load(), fallbackCalls.Load())
					}
					// A second request still starts on the primary: the rejected request did not cool it.
					doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{"model": "gpt-test", "messages": []map[string]string{{"role": "user", "content": "hello"}}}, nil)
					if primaryCalls.Load() != 2 || fallbackCalls.Load() != 0 {
						t.Fatalf("400 cooled primary: calls=%d/%d", primaryCalls.Load(), fallbackCalls.Load())
					}
				} else if fallbackCalls.Load() == 0 || response.Code != 200 {
					t.Fatalf("fallback missing: response=%d %s calls=%d/%d", response.Code, response.Body.String(), primaryCalls.Load(), fallbackCalls.Load())
				}
			})
		}
	}
}
