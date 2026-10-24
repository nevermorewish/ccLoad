package app

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"ccLoad/internal/model"
)

// Mutate the channel only after the first upstream call has started, so the
// request already owns a candidate snapshot containing the enabled channel.
func TestProxy_ChannelDisabledDuringRequest(t *testing.T) {
	for _, tc := range []struct {
		name       string
		mode       string
		failStatus int
	}{
		{"disabled fallback", "fallback", http.StatusServiceUnavailable},
		{"deleted fallback", "delete", http.StatusServiceUnavailable},
		{"same channel retry", "same", http.StatusServiceUnavailable},
		{"next key", "key", http.StatusUnauthorized},
		{"next URL", "url", http.StatusNotFound},
		{"next protocol", "protocol", http.StatusNotFound},
	} {
		for _, cooldownEnabled := range []string{"true", "false"} {
			t.Run(tc.name+"/cooldown="+cooldownEnabled, func(t *testing.T) {
				var env *proxyTestEnv
				var primaryCalls, disabledCalls, healthyCalls atomic.Int64
				primary := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					primaryCalls.Add(1)
					id := int64(1)
					if tc.mode == "fallback" || tc.mode == "delete" {
						id = 2
					}
					var err error
					if tc.mode == "delete" {
						err = env.store.DeleteConfig(context.Background(), id)
					} else {
						_, err = env.store.UpdateChannelEnabled(context.Background(), id, false)
					}
					if err != nil {
						t.Errorf("change channel state: %v", err)
					}
					env.server.InvalidateChannelListCache()
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(tc.failStatus)
					_, _ = w.Write([]byte(`{"error":{"message":"Invalid URL (POST /v1/chat/completions)"}}`))
				}))
				defer primary.Close()
				disabled := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					disabledCalls.Add(1)
					w.WriteHeader(http.StatusServiceUnavailable)
				}))
				defer disabled.Close()
				healthy := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					healthyCalls.Add(1)
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"id":"healthy","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
				}))
				defer healthy.Close()

				channels := []testChannel{{name: "primary", models: "gpt-test", priority: 100}}
				urls := map[int]string{0: primary.URL}
				switch tc.mode {
				case "fallback", "delete":
					channels = append(channels, testChannel{name: "disabled", models: "gpt-test", priority: 90})
					urls[1] = disabled.URL
				case "same":
					channels[0].sameChannelRetries = 2
				case "key":
					channels[0].retryOtherKeysOnFailure = true
				case "url":
					urls[0] += "\n" + disabled.URL
				case "protocol":
					channels[0].protocolTransformMode = model.ProtocolTransformModeAuto
				}
				urls[len(channels)] = healthy.URL
				channels = append(channels, testChannel{name: "healthy", models: "gpt-test", priority: 50})
				env = setupProxyTestEnvWithSettings(t, channels, urls, map[string]string{"cooldown_enabled": cooldownEnabled})
				if tc.mode == "url" {
					// Keep the first attempt deterministic instead of weighted URL selection.
					env.server.urlSelector = nil
				}
				if tc.mode == "key" {
					if err := env.store.CreateAPIKeysBatch(context.Background(), []*model.APIKey{
						{ChannelID: 1, KeyIndex: 1, APIKey: "sk-second-test-key"},
					}); err != nil {
						t.Fatal(err)
					}
				}
				response := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
					"model": "gpt-test", "messages": []map[string]string{{"role": "user", "content": "hi"}},
				}, nil)
				if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"healthy"`) {
					t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
				}
				if primaryCalls.Load() != 1 || disabledCalls.Load() != 0 || healthyCalls.Load() != 1 {
					t.Fatalf("upstream calls primary/disabled/healthy=%d/%d/%d, want 1/0/1",
						primaryCalls.Load(), disabledCalls.Load(), healthyCalls.Load())
				}
			})
		}
	}
}
