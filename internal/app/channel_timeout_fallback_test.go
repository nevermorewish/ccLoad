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

func TestTimeoutChannelReturnsFinalResponse(t *testing.T) {
	t.Parallel()
	for _, status := range []int{200, 401, 429, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			t.Parallel()
			var primaryCalls, targetCalls, otherCalls atomic.Int32
			primary := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				primaryCalls.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			}))
			t.Cleanup(primary.Close)
			body := `{"error":{"message":"target result"}}`
			if status == 200 {
				body = "data: {\"choices\":[{\"delta\":{\"content\":\"target result\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
			}
			target := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				targetCalls.Add(1)
				// A slow final destination must not inherit the rule's one-second timer.
				if status == 200 {
					select {
					case <-time.After(1200 * time.Millisecond):
					case <-r.Context().Done():
						return
					}
					w.Header().Set("Content-Type", "text/event-stream")
				} else {
					w.Header().Set("Content-Type", "application/json")
				}
				w.Header().Set("X-Target-Result", "final")
				w.WriteHeader(status)
				_, _ = io.WriteString(w, body)
			}))
			t.Cleanup(target.Close)
			other := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				otherCalls.Add(1)
				w.WriteHeader(http.StatusTeapot)
			}))
			t.Cleanup(other.Close)
			env := setupProxyTestEnv(t, []testChannel{
				{name: "primary", models: "gpt-test", priority: 100, sameChannelRetries: 3},
				{name: "ordinary-next", models: "gpt-test", priority: 90},
				{name: "target", models: "gpt-test", priority: 80, channelPool: 2, sameChannelRetries: 3, retryOtherKeysOnFailure: true},
			}, map[int]string{0: primary.URL, 1: other.URL, 2: target.URL})
			cfg, err := env.store.GetConfig(context.Background(), 3)
			if err != nil {
				t.Fatal(err)
			}
			cfg.URLs = append(cfg.URLs, model.ChannelURL{URL: target.URL + "/second"})
			if _, err := env.store.UpdateConfig(context.Background(), 3, cfg); err != nil {
				t.Fatal(err)
			}
			if err := env.store.CreateAPIKeysBatch(context.Background(), []*model.APIKey{{ChannelID: 3, KeyIndex: 1, APIKey: "sk-second"}}); err != nil {
				t.Fatal(err)
			}
			env.server.InvalidateChannelListCache()
			useChannelRoutes(t, env.server, ChannelRouteRule{
				ID: "timeout", Enabled: true, Models: []string{"gpt-test"}, DefaultPool: 0,
				Strict: status != 200, FirstByteTimeoutSeconds: 1, FirstByteTimeoutChannelID: 3,
			})
			response := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
				"model": "gpt-test", "stream": true, "messages": []map[string]string{{"role": "user", "content": "hello"}},
			}, nil)
			if response.Code != status || response.Body.String() != body || response.Header().Get("X-Target-Result") != "final" {
				t.Fatalf("response=%d %s headers=%v", response.Code, response.Body.String(), response.Header())
			}
			if primaryCalls.Load() != 1 || targetCalls.Load() != 1 || otherCalls.Load() != 0 {
				t.Fatalf("calls primary=%d target=%d other=%d", primaryCalls.Load(), targetCalls.Load(), otherCalls.Load())
			}
			if counts := env.server.channelPoolQueues.snapshot(); len(counts) != 0 {
				t.Fatalf("reservations leaked: %v", counts)
			}
		})
	}
}

func TestTimeoutChannelUnavailableDoesNotUseOtherChannel(t *testing.T) {
	t.Parallel()
	var otherCalls atomic.Int32
	primary := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	t.Cleanup(primary.Close)
	other := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		otherCalls.Add(1)
		w.WriteHeader(http.StatusTeapot)
	}))
	t.Cleanup(other.Close)
	env := setupProxyTestEnv(t, []testChannel{
		{name: "primary", models: "gpt-test", priority: 100},
		{name: "other", models: "gpt-test", priority: 90},
		{name: "disabled-target", models: "gpt-test", priority: 80},
	}, map[int]string{0: primary.URL, 1: other.URL, 2: other.URL})
	if _, err := env.store.UpdateChannelEnabled(context.Background(), 3, false); err != nil {
		t.Fatal(err)
	}
	env.server.InvalidateChannelListCache()
	useChannelRoutes(t, env.server, ChannelRouteRule{ID: "timeout", Enabled: true, Models: []string{"gpt-test"}, FirstByteTimeoutSeconds: 1, FirstByteTimeoutChannelID: 3})
	response := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model": "gpt-test", "stream": true, "messages": []map[string]string{{"role": "user", "content": "hello"}},
	}, nil)
	if response.Code != http.StatusServiceUnavailable || otherCalls.Load() != 0 {
		t.Fatalf("status=%d other calls=%d body=%s", response.Code, otherCalls.Load(), response.Body.String())
	}
}
