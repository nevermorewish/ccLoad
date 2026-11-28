package app

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"ccLoad/internal/model"
)

func TestNonStreamDesignatedChannel(t *testing.T) {
	for _, status := range []int{200, 429, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var primaryCalls, targetCalls atomic.Int32
			primary := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				primaryCalls.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"primary\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			}))
			defer primary.Close()
			body := `{"error":{"message":"target result"}}`
			if status == 200 {
				body = `{"id":"chatcmpl-test","object":"chat.completion","model":"gpt-test","choices":[{"index":0,"message":{"role":"assistant","content":"target result"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`
			}
			target := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				targetCalls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				_, _ = io.WriteString(w, body)
			}))
			defer target.Close()
			env := setupProxyTestEnv(t, []testChannel{
				{name: "primary", models: "gpt-test", priority: 100},
				{name: "target", models: "gpt-test", priority: 80, channelPool: 2, sameChannelRetries: 3, retryOtherKeysOnFailure: true},
			}, map[int]string{0: primary.URL, 1: target.URL})
			useChannelRoutes(t, env.server, ChannelRouteRule{ID: "direct", Enabled: true, Models: []string{"gpt-test"}, Strict: true, NonStreamChannelID: 2})
			for _, explicit := range []bool{true, false} {
				payload := map[string]any{"model": "gpt-test", "messages": []map[string]string{{"role": "user", "content": "hello"}}}
				if explicit {
					payload["stream"] = false
				}
				// Exercise explicit false and the default when stream is omitted.
				response := doProxyRequest(t, env.engine, "/v1/chat/completions", payload, nil)
				if response.Code != status || primaryCalls.Load() != 0 {
					t.Fatalf("status=%d primary=%d body=%s", response.Code, primaryCalls.Load(), response.Body.String())
				}
				if status != 200 {
					break
				}
			}
			expected := int32(1)
			if status == 200 {
				expected = 2
			}
			if targetCalls.Load() != expected {
				t.Fatalf("target calls=%d want %d", targetCalls.Load(), expected)
			}
			response := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{"model": "gpt-test", "stream": true, "messages": []map[string]string{{"role": "user", "content": "hello"}}}, nil)
			if response.Code != 200 || primaryCalls.Load() != 1 || targetCalls.Load() != expected {
				t.Fatalf("stream status=%d primary=%d target=%d", response.Code, primaryCalls.Load(), targetCalls.Load())
			}
			if counts := env.server.channelPoolQueues.snapshot(); len(counts) != 0 {
				t.Fatalf("queue leak: %v", counts)
			}
		})
	}
}

func TestNonStreamDesignatedChannelUnavailable(t *testing.T) {
	for _, kind := range []string{"disabled", "missing", "unsupported", "restricted", "cooled"} {
		t.Run(kind, func(t *testing.T) {
			var calls atomic.Int32
			upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(200) }))
			defer upstream.Close()
			env := setupProxyTestEnv(t, []testChannel{{name: "primary", models: "gpt-test", priority: 100}, {name: "target", models: "gpt-test", priority: 80, channelPool: 2}}, map[int]string{0: upstream.URL, 1: upstream.URL})
			if kind == "restricted" {
				auth := env.server.authService
				auth.authTokensMux.Lock()
				auth.authTokenChannels[model.HashToken("test-api-key")] = mustChannelRestriction(t, model.ChannelRestrictionModeAllow, 1)
				auth.authTokensMux.Unlock()
			}
			if kind == "cooled" {
				if err := env.store.SetChannelCooldown(context.Background(), 2, time.Now().Add(time.Hour)); err != nil {
					t.Fatal(err)
				}
			}
			targetID := int64(2)
			switch kind {
			case "disabled":
				if _, err := env.store.UpdateChannelEnabled(context.Background(), 2, false); err != nil {
					t.Fatal(err)
				}
			case "missing":
				targetID = 999
			}
			useChannelRoutes(t, env.server, ChannelRouteRule{ID: "direct", Enabled: true, Models: []string{"*"}, NonStreamChannelID: targetID})
			modelName := "gpt-test"
			if kind == "unsupported" {
				modelName = "unknown-model"
				useChannelRoutes(t, env.server, ChannelRouteRule{ID: "direct", Enabled: true, Models: []string{"*"}, NonStreamChannelID: 2})
			}
			response := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{"model": modelName, "stream": false, "messages": []map[string]string{{"role": "user", "content": "hello"}}}, nil)
			wantStatus := 503
			if kind == "restricted" {
				wantStatus = 403
			}
			if response.Code != wantStatus || calls.Load() != 0 {
				t.Fatalf("status=%d calls=%d body=%s", response.Code, calls.Load(), response.Body.String())
			}
		})
	}
}

func TestAdminNonStreamDesignatedChannel(t *testing.T) {
	server := newInMemoryServer(t)
	target := addPoolChannel(t, server, "direct", 2, 10, "kimi-k3")
	rule := ChannelRouteRule{ID: "direct", Enabled: true, Models: []string{"kimi-k3"}, NonStreamChannelID: target.ID}
	status, view, body := putChannelRoutes(t, server, ChannelRouteConfig{Rules: []ChannelRouteRule{rule}})
	if status != 200 || view.Config.Rules[0].NonStreamChannelID != target.ID {
		t.Fatalf("status=%d body=%s", status, body)
	}
	setting, err := server.store.GetSetting(context.Background(), channelRouteRulesSettingKey)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := parseChannelRouteConfig(setting.Value)
	if err != nil || loaded.Rules[0].NonStreamChannelID != target.ID {
		t.Fatalf("loaded=%+v err=%v", loaded, err)
	}
	status, simulation := simulateChannelRoute(t, server, map[string]any{"model": "kimi-k3"})
	if status != 200 || simulation.NonStreamChannelID != target.ID {
		t.Fatalf("simulation=%+v", simulation)
	}
	for _, id := range []int64{-1, 999} {
		rule.NonStreamChannelID = id
		status, _, _ = putChannelRoutes(t, server, ChannelRouteConfig{Rules: []ChannelRouteRule{rule}})
		if status != 400 {
			t.Fatalf("id=%d status=%d", id, status)
		}
	}
	if _, err := server.store.UpdateChannelEnabled(context.Background(), target.ID, false); err != nil {
		t.Fatal(err)
	}
	rule.NonStreamChannelID = target.ID
	status, _, _ = putChannelRoutes(t, server, ChannelRouteConfig{Rules: []ChannelRouteRule{rule}})
	if status != 400 {
		t.Fatalf("disabled status=%d", status)
	}
}

func TestLogprobsDesignatedChannelAndPrecedence(t *testing.T) {
	var primaryCalls, nonStreamCalls, logprobsCalls atomic.Int32
	primary := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		primaryCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"primary\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer primary.Close()
	nonStream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		nonStreamCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"ns","object":"chat.completion","model":"gpt-test","choices":[{"index":0,"message":{"role":"assistant","content":"ns"},"finish_reason":"stop"}]}`)
	}))
	defer nonStream.Close()
	logprobsTarget := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		logprobsCalls.Add(1)
		payload, _ := io.ReadAll(r.Body)
		if len(payload) > 0 && payload[0] == '{' && r.Header.Get("Accept") == "text/event-stream" {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"lp\"},\"logprobs\":{\"content\":[]},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"lp","object":"chat.completion","model":"gpt-test","choices":[{"index":0,"message":{"role":"assistant","content":"lp"},"logprobs":{"content":[]},"finish_reason":"stop"}]}`)
	}))
	defer logprobsTarget.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "primary", models: "gpt-test", priority: 100},
		{name: "nonstream", models: "gpt-test", priority: 90, channelPool: 1},
		{name: "logprobs", models: "gpt-test", priority: 80, channelPool: 2},
	}, map[int]string{0: primary.URL, 1: nonStream.URL, 2: logprobsTarget.URL})

	useChannelRoutes(t, env.server, ChannelRouteRule{
		ID: "lp-rule", Enabled: true, Models: []string{"gpt-test"}, Strict: true,
		NonStreamChannelID: 2, LogprobsChannelID: 3,
	})

	// Non-stream with logprobs=true must prefer LogprobsChannelID (3) over NonStreamChannelID (2).
	resp := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model": "gpt-test", "stream": false, "logprobs": true, "messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)
	if resp.Code != 200 || logprobsCalls.Load() != 1 || nonStreamCalls.Load() != 0 || primaryCalls.Load() != 0 {
		t.Fatalf("non-stream logprobs: code=%d lp=%d ns=%d primary=%d body=%s", resp.Code, logprobsCalls.Load(), nonStreamCalls.Load(), primaryCalls.Load(), resp.Body.String())
	}

	// Stream with logprobs=true must also route to LogprobsChannelID (3).
	resp = doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model": "gpt-test", "stream": true, "logprobs": true, "messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, map[string]string{"Accept": "text/event-stream"})
	if resp.Code != 200 || logprobsCalls.Load() != 2 || primaryCalls.Load() != 0 {
		t.Fatalf("stream logprobs: code=%d lp=%d primary=%d body=%s", resp.Code, logprobsCalls.Load(), primaryCalls.Load(), resp.Body.String())
	}

	// Non-stream with logprobs=false falls back to NonStreamChannelID (2).
	resp = doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model": "gpt-test", "stream": false, "logprobs": false, "messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)
	if resp.Code != 200 || nonStreamCalls.Load() != 1 || logprobsCalls.Load() != 2 {
		t.Fatalf("non-stream logprobs=false: code=%d ns=%d lp=%d", resp.Code, nonStreamCalls.Load(), logprobsCalls.Load())
	}

	// Stream without logprobs uses primary channel (1).
	resp = doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model": "gpt-test", "stream": true, "messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)
	if resp.Code != 200 || primaryCalls.Load() != 1 {
		t.Fatalf("stream without logprobs: code=%d primary=%d", resp.Code, primaryCalls.Load())
	}
}
