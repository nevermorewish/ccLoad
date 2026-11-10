package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ccLoad/internal/anthropicauth"
	"ccLoad/internal/antigravityauth"
	"ccLoad/internal/codebuddyauth"
	"ccLoad/internal/codexauth"
	"ccLoad/internal/config"
	"ccLoad/internal/model"
	"ccLoad/internal/protocol"
	"ccLoad/internal/storage"
	"ccLoad/internal/util"
	"ccLoad/internal/xaiauth"

	"github.com/andybalholm/brotli"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/klauspost/compress/zstd"
	"github.com/tidwall/gjson"
)

// ============================================================================
// 代理转发集成测试
// 端到端验证：上游模拟 → Server → gin 路由 → 请求转发 → 响应返回
// ============================================================================

// testChannel 测试用渠道定义
type testChannel struct {
	name                    string
	upstreamProtocol        string
	protocolTransformMode   string
	websockets              bool
	customRequestRules      *model.CustomRequestRules
	cooldownDetectionRules  *model.CooldownDetectionRules
	retryOtherKeysOnFailure bool
	sameChannelRetries      int
	models                  string // 逗号分隔的模型列表
	modelEntries            []model.ModelEntry
	apiKey                  string
	authType                string
	oauthCredential         string
	priority                int
}

// proxyTestEnv 集成测试环境
type proxyTestEnv struct {
	server *Server
	store  storage.Store
	engine *gin.Engine
}

func TestProxy_SameChannelModelVariantsRotateAndSkipCooldown(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var sent []string
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode upstream request: %v", err)
		}
		mu.Lock()
		sent = append(sent, body.Model)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chat-1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer upstream.Close()
	env := setupProxyTestEnvWithSettings(t, []testChannel{{
		name: "variants", upstreamProtocol: "openai", modelEntries: []model.ModelEntry{
			{Model: "auto", RedirectModel: "upstream-a"},
			{Model: "auto", RedirectModel: "upstream-b"},
		},
	}}, map[int]string{0: upstream.URL}, map[string]string{"cooldown_fallback_enabled": "true"})
	configs, err := env.store.ListConfigs(context.Background())
	if err != nil || len(configs) != 1 {
		t.Fatalf("configs = (%v, %v)", configs, err)
	}
	request := func() {
		t.Helper()
		response := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
			"model": "auto", "messages": []any{map[string]any{"role": "user", "content": "hi"}},
		}, nil)
		if response.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
	}
	for range 3 {
		request()
	}
	if err := env.store.SetModelCooldown(context.Background(), configs[0].ID, "upstream-a", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	env.server.invalidateCooldownCache()
	for range 2 {
		request()
	}
	if err := env.store.ResetModelCooldown(context.Background(), configs[0].ID, "upstream-a"); err != nil {
		t.Fatal(err)
	}
	env.server.invalidateCooldownCache()
	request()
	mu.Lock()
	defer mu.Unlock()
	want := []string{"upstream-a", "upstream-b", "upstream-a", "upstream-b", "upstream-b", "upstream-a"}
	if !slices.Equal(sent, want) {
		t.Fatalf("upstream models = %v, want %v", sent, want)
	}
}

func TestProxy_ModelVariantsPreserveSecondRedirect(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var sent []string
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode upstream request: %v", err)
		}
		mu.Lock()
		sent = append(sent, body.Model)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chat-1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer upstream.Close()
	env := setupProxyTestEnvWithSettings(t, []testChannel{{
		name: "chained-variants", upstreamProtocol: "openai", modelEntries: []model.ModelEntry{
			{Model: "a", RedirectModel: "b"},
			{Model: "a", RedirectModel: "d"},
			{Model: "b", RedirectModel: "c"},
			{Model: "b", RedirectModel: "other"},
			{Model: "c", RedirectModel: "e"},
		},
	}}, map[int]string{0: upstream.URL}, map[string]string{"cooldown_fallback_enabled": "false"})
	request := func(name string) {
		t.Helper()
		response := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
			"model": name, "messages": []any{map[string]any{"role": "user", "content": "hi"}},
		}, nil)
		if response.Code != http.StatusOK {
			t.Fatalf("model=%s status=%d body=%s", name, response.Code, response.Body.String())
		}
	}
	request("a") // a -> b -> c; c -> e is not a third hop.
	request("a") // a -> d.
	request("b") // A's second lookup did not advance B's cursor: b -> c -> e.
	request("b") // B's own requests rotate to its other target.
	if err := env.store.SetModelCooldown(context.Background(), 1, "c", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	env.server.invalidateCooldownCache()
	request("a") // The a -> b row resolves to cooled model c, so use a -> d.
	mu.Lock()
	defer mu.Unlock()
	if want := []string{"c", "d", "e", "other", "d"}; !slices.Equal(sent, want) {
		t.Fatalf("upstream models=%v, want %v", sent, want)
	}
}

func TestProxy_ModelVariantsUseKeySupportingSelectedFinalTarget(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var sent []string
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json")
			switch r.Header.Get("Authorization") {
			case "Bearer only-c":
				_, _ = io.WriteString(w, `{"data":[{"id":"c"}]}`)
			case "Bearer only-d":
				_, _ = io.WriteString(w, `{"data":[{"id":"d"}]}`)
			default:
				http.Error(w, "unknown key", http.StatusUnauthorized)
			}
			return
		}
		var body struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode upstream request: %v", err)
		}
		pair := body.Model + "/" + r.Header.Get("Authorization")
		mu.Lock()
		sent = append(sent, pair)
		mu.Unlock()
		if pair != "c/Bearer only-c" && pair != "d/Bearer only-d" {
			http.Error(w, "key does not support model", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chat-1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer upstream.Close()
	env := setupProxyTestEnvWithSettings(t, []testChannel{{
		name: "per-target-key", upstreamProtocol: "openai", modelEntries: []model.ModelEntry{
			{Model: "a", RedirectModel: "b"}, {Model: "a", RedirectModel: "d"}, {Model: "b", RedirectModel: "c"},
		},
	}}, map[int]string{0: upstream.URL}, map[string]string{"cooldown_fallback_enabled": "true"})
	ctx := context.Background()
	if err := env.store.DeleteAllAPIKeys(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if err := env.store.CreateAPIKeysBatch(ctx, []*model.APIKey{
		{ChannelID: 1, KeyIndex: 0, APIKey: "only-c"},
		{ChannelID: 1, KeyIndex: 1, APIKey: "only-d"},
	}); err != nil {
		t.Fatal(err)
	}
	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/models/refresh-batch", map[string]any{
		"channel_ids": []int64{1}, "mode": "replace",
	}))
	env.server.HandleBatchRefreshModels(c)
	if w.Code != http.StatusOK || !gjson.GetBytes(w.Body.Bytes(), "success").Bool() {
		t.Fatalf("refresh status=%d body=%s", w.Code, w.Body.String())
	}
	request := func() {
		t.Helper()
		response := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
			"model": "a", "messages": []any{map[string]any{"role": "user", "content": "hi"}},
		}, nil)
		if response.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
	}
	for range 4 {
		request()
	}
	if err := env.store.SetKeyCooldown(ctx, 1, 0, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	env.server.invalidateCooldownCache()
	for range 2 {
		request()
	}
	if err := env.store.SetKeyCooldown(ctx, 1, 1, time.Now().Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	env.server.invalidateCooldownCache()
	request() // 全冷却时选择最早恢复的 Key 和对应目标。
	mu.Lock()
	defer mu.Unlock()
	if want := []string{"c/Bearer only-c", "d/Bearer only-d", "c/Bearer only-c", "d/Bearer only-d", "d/Bearer only-d", "d/Bearer only-d", "c/Bearer only-c"}; !slices.Equal(sent, want) {
		t.Fatalf("upstream model/key pairs=%v, want %v", sent, want)
	}
}

func TestProxy_DetectedKeyTargetSkipsIncompatibleProtocolURL(t *testing.T) {
	t.Parallel()
	var openAIHits atomic.Int64
	openAI := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		openAIHits.Add(1)
		http.Error(w, "unsupported model for key", http.StatusBadRequest)
	}))
	defer openAI.Close()
	var geminiPaths []string
	var geminiPathsMu sync.Mutex
	gemini := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		geminiPathsMu.Lock()
		geminiPaths = append(geminiPaths, r.URL.Path)
		geminiPathsMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"candidates":[{"content":{"parts":[{"text":"ok"}]}}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2},"modelVersion":"c"}`)
	}))
	defer gemini.Close()

	env := setupProxyTestEnv(t, []testChannel{{
		name: "protocol-target-key", upstreamProtocol: "openai", protocolTransformMode: model.ProtocolTransformModeLocal,
		modelEntries: []model.ModelEntry{{Model: "a", RedirectModel: "c"}},
		customRequestRules: &model.CustomRequestRules{Body: []model.CustomBodyRule{{
			Action: model.RuleActionOverride, Path: "model", Value: json.RawMessage(`"body-rule-target"`),
		}}},
	}}, map[int]string{0: openAI.URL})
	ctx := context.Background()
	cfg, err := env.store.GetConfig(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	cfg.URLs = model.ChannelURLs{
		{URL: openAI.URL, Protocols: []string{"openai"}},
		{URL: gemini.URL, Protocols: []string{"gemini"}},
	}
	if _, err := env.store.UpdateConfig(ctx, cfg.ID, cfg); err != nil {
		t.Fatal(err)
	}
	if err := env.store.DeleteAllAPIKeys(ctx, cfg.ID); err != nil {
		t.Fatal(err)
	}
	if err := env.store.CreateAPIKeysBatch(ctx, []*model.APIKey{{
		ChannelID: cfg.ID, KeyIndex: 0, APIKey: "only-c", DetectedModels: []string{"c"},
	}}); err != nil {
		t.Fatal(err)
	}
	env.server.InvalidateChannelListCache()
	env.server.InvalidateAPIKeysCache(cfg.ID)
	env.server.urlSelector = nil

	path, body := nativeGenerationRequest(protocol.Gemini, "a", false)
	response := doProxyRequest(t, env.engine, path, body, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	geminiPathsMu.Lock()
	defer geminiPathsMu.Unlock()
	if openAIHits.Load() != 0 || !slices.Equal(geminiPaths, []string{"/v1beta/models/c:generateContent"}) {
		t.Fatalf("OpenAI hits=%d Gemini paths=%v", openAIHits.Load(), geminiPaths)
	}
}

func TestProxy_DetectedKeyTargetUsesSendableURLProtocolAndTriesAnotherKey(t *testing.T) {
	t.Parallel()
	for _, declared := range [][]string{{"openai"}, {"openai", "gemini"}} {
		t.Run(strings.Join(declared, "+"), func(t *testing.T) {
			var sent []string
			upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Model string `json:"model"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("decode upstream request: %v", err)
				}
				sent = append(sent, body.Model+"/"+r.Header.Get("Authorization"))
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"chat-1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
			}))
			defer upstream.Close()
			env := setupProxyTestEnv(t, []testChannel{{
				name: "declared-target-key", protocolTransformMode: model.ProtocolTransformModeAuto,
				modelEntries: []model.ModelEntry{{Model: "a", RedirectModel: "c"}},
				customRequestRules: &model.CustomRequestRules{Body: []model.CustomBodyRule{{
					Action: model.RuleActionOverride, Path: "model", Value: json.RawMessage(`"d"`),
				}}},
			}}, map[int]string{0: upstream.URL})
			ctx := context.Background()
			cfg, err := env.store.GetConfig(ctx, 1)
			if err != nil {
				t.Fatal(err)
			}
			cfg.URLs[0].Protocols = declared
			if _, err := env.store.UpdateConfig(ctx, cfg.ID, cfg); err != nil {
				t.Fatal(err)
			}
			if err := env.store.DeleteAllAPIKeys(ctx, cfg.ID); err != nil {
				t.Fatal(err)
			}
			if err := env.store.CreateAPIKeysBatch(ctx, []*model.APIKey{
				{ChannelID: cfg.ID, KeyIndex: 0, APIKey: "only-c", Priority: 10, DetectedModels: []string{"c"}},
				{ChannelID: cfg.ID, KeyIndex: 1, APIKey: "only-d", Priority: 0, DetectedModels: []string{"d"}},
			}); err != nil {
				t.Fatal(err)
			}
			env.server.InvalidateChannelListCache()
			env.server.InvalidateAPIKeysCache(cfg.ID)

			response := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
				"model": "a", "messages": []any{map[string]any{"role": "user", "content": "hi"}},
			}, nil)
			if response.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if want := []string{"d/Bearer only-d"}; !slices.Equal(sent, want) {
				t.Fatalf("upstream model/key pairs=%v, want %v", sent, want)
			}
		})
	}
}

func TestProxy_ExactModelMatchPrecedesFuzzy(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var sent []string
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		mu.Lock()
		sent = append(sent, body.Model)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chat-1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer upstream.Close()
	env := setupProxyTestEnv(t, []testChannel{{name: "match-priority", upstreamProtocol: "openai", modelEntries: []model.ModelEntry{
		{Model: "foo-v2"},
	}}}, map[int]string{0: upstream.URL})
	env.server.modelFuzzyMatch = true
	cfg, err := env.store.GetConfig(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	request := func(name string) {
		t.Helper()
		response := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
			"model": name, "messages": []any{map[string]any{"role": "user", "content": "hi"}},
		}, nil)
		if response.Code != http.StatusOK {
			t.Fatalf("model=%s status=%d body=%s", name, response.Code, response.Body.String())
		}
	}
	update := func(entries []model.ModelEntry) {
		t.Helper()
		cfg.ModelEntries = entries
		if _, err := env.store.UpdateConfig(context.Background(), cfg.ID, cfg); err != nil {
			t.Fatal(err)
		}
		env.server.InvalidateChannelListCache()
	}
	request("foo")
	update([]model.ModelEntry{{Model: "foo-v2"}, {Model: "foo", RedirectModel: "exact-target"}, {Model: "bar(max)", RedirectModel: "alias-target"}})
	request("foo")
	request("bar")
	mu.Lock()
	defer mu.Unlock()
	if want := []string{"foo-v2", "exact-target", "alias-target"}; !slices.Equal(sent, want) {
		t.Fatalf("upstream models=%v, want %v", sent, want)
	}
}

func TestProxy_UnconfiguredModelIsNotServedAfterExplicitModelFailure(t *testing.T) {
	t.Parallel()
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode upstream request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		if body.Model == "a" {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"message":"rate limited","type":"rate_limit_error"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"chat-1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer upstream.Close()
	env := setupProxyTestEnvWithSettings(t, []testChannel{{
		name: "explicit-model-only", upstreamProtocol: "openai",
		modelEntries: []model.ModelEntry{{Model: "a"}},
	}}, map[int]string{0: upstream.URL}, map[string]string{"cooldown_fallback_enabled": "false"})
	request := func(name string) *httptest.ResponseRecorder {
		t.Helper()
		return doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
			"model": name, "messages": []any{map[string]any{"role": "user", "content": "hi"}},
		}, nil)
	}
	if response := request("a"); response.Code == http.StatusOK {
		t.Fatalf("expected model a to fail, body=%s", response.Body.String())
	}
	if response := request("b"); response.Code == http.StatusOK {
		t.Fatalf("unconfigured model b was served, body=%s", response.Body.String())
	}
}

func TestProxy_CodeBuddyWireAndCompletion(t *testing.T) {
	t.Parallel()
	for _, stream := range []bool{false, true} {
		for _, clientProtocol := range []string{"openai", "anthropic", "codex", "gemini"} {
			t.Run(fmt.Sprintf("%s/stream=%v", clientProtocol, stream), func(t *testing.T) {
				t.Parallel()
				upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var request map[string]any
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
						t.Error(err)
					}
					if request["stream"] != true {
						t.Error("upstream must stream")
					}
					if r.Header.Get("Authorization") != "Bearer access" || r.Header.Get("X-Refresh-Token") != "" || r.Header.Get("X-User-Id") != "uid" {
						t.Error("provider credentials missing")
					}
					if r.Header.Get("X-CodeBuddy-Request") != "1" || r.Header.Get("X-Agent-Intent") != "craft" || r.Header.Get("X-Conversation-Request-ID") == "" {
						t.Error("official CodeBuddy chat fingerprint missing")
					}
					if r.URL.Path != "/v2/chat/completions" {
						t.Errorf("path %s", r.URL.Path)
					}
					w.Header().Set("Content-Type", "text/event-stream")
					for _, chunk := range []string{
						`{"id":"chat-1","model":"hy3","created":100,"object":"response","choices":[{"index":0,"delta":{"role":"assistant","content":"hello","tool_calls":[],"function_call":null},"finish_reason":null}]}`,
						`{"id":"chat-1","model":"hy3","object":"response","choices":[{"index":0,"delta":{"content":" world"},"finish_reason":"stop"}]}`,
						`{"id":"chat-1","object":"response","choices":[],"usage":{"prompt_tokens":9,"completion_tokens":4,"total_tokens":13}}`,
					} {
						_, _ = fmt.Fprintf(w, "data: %s\n\n", chunk)
						w.(http.Flusher).Flush()
					}
					_, _ = io.WriteString(w, "data: [DONE]\n\n")
				}))
				defer upstream.Close()
				credential, _ := (&codebuddyauth.Credential{AccessToken: "access", RefreshToken: "refresh", UID: "uid"}).JSON()
				env := setupProxyTestEnv(t, []testChannel{{name: "codebuddy", upstreamProtocol: "openai", models: "hy3", authType: model.AuthTypeCodeBuddyOAuth, oauthCredential: credential}}, map[int]string{0: upstream.URL + "/v2/chat/completions#"})
				path := "/v1/chat/completions"
				body := map[string]any{"model": "hy3", "stream": stream, "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
				switch clientProtocol {
				case "anthropic":
					path = "/v1/messages"
					body["max_tokens"] = 32
				case "codex":
					path = "/v1/responses"
					delete(body, "messages")
					body["input"] = []any{
						map[string]any{"type": "compaction", "encrypted_content": "ccload-compact-v1:" + base64.RawURLEncoding.EncodeToString([]byte(`{"summary":"previous work"}`))},
						map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "hi"}}},
					}
				case "gemini":
					path = "/v1beta/models/hy3:generateContent"
					if stream {
						path = "/v1beta/models/hy3:streamGenerateContent"
					}
					delete(body, "messages")
					body["contents"] = []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": "hi"}}}}
				}
				response := doProxyRequest(t, env.engine, path, body, nil)
				if clientProtocol != "openai" {
					if response.Code != http.StatusServiceUnavailable {
						t.Fatalf("foreign protocol admitted: %d", response.Code)
					}
					return
				}
				if response.Code != 200 {
					t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
				}
				if !stream {
					var payload map[string]any
					if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
						t.Fatalf("nonstream not JSON: %v %s", err, response.Body.String())
					}
					if clientProtocol == "openai" && (gjson.GetBytes(response.Body.Bytes(), "choices.0.message.content").String() != "hello world" || gjson.GetBytes(response.Body.Bytes(), "usage.total_tokens").Int() != 13) {
						t.Fatalf("incomplete completion %s", response.Body.String())
					}
				} else {
					parser := newSSEUsageParser(clientProtocol)
					if err := parser.Feed(response.Body.Bytes()); err != nil {
						t.Fatal(err)
					}
					if !parser.IsStreamComplete() {
						t.Fatalf("incomplete SSE: %s", response.Body.String())
					}
				}
			})
		}
	}
}

func TestProxy_CodeBuddyNonStreamToolCallsAndErrors(t *testing.T) {
	for _, scenario := range []string{"tools", "length", "missing-done", "truncated", "error", "business-error", "json-error"} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if scenario == "json-error" {
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"code":11101,"msg":"rejected"}`)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				if scenario == "error" {
					_, _ = io.WriteString(w, "data: {\"error\":{\"type\":\"server_error\",\"message\":\"unavailable\"}}\n\n")
					return
				}
				if scenario == "business-error" {
					_, _ = io.WriteString(w, "data: {\"code\":11101,\"msg\":\"rejected\"}\n\n")
					return
				}
				chunks := []string{
					`{"id":"c","model":"hy3","choices":[{"index":0,"delta":{"reasoning_content":"think","tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"lookup","arguments":"{\"q\":"}},{"index":1,"id":"call_b","type":"function","function":{"name":"second","arguments":"{"}}]}}]}`,
					`{"id":"c","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"arguments":"}"}},{"index":0,"function":{"arguments":"\"test\"}"}}]}}]}`,
				}
				for _, chunk := range chunks {
					_, _ = fmt.Fprintf(w, "data: %s\n\n", chunk)
				}
				if scenario == "truncated" {
					return
				}
				finish := "tool_calls"
				if scenario == "length" {
					finish = "length"
				}
				_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":%q}]}\n\n", finish)
				_, _ = io.WriteString(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":7,\"total_tokens\":18}}\n\n")
				if scenario != "missing-done" {
					_, _ = io.WriteString(w, "data: [DONE]\n\n")
				}
			}))
			defer upstream.Close()
			credential, _ := (&codebuddyauth.Credential{AccessToken: "access"}).JSON()
			env := setupProxyTestEnv(t, []testChannel{{name: "codebuddy", upstreamProtocol: "openai", models: "hy3", authType: model.AuthTypeCodeBuddyOAuth, oauthCredential: credential}}, map[int]string{0: upstream.URL + "/v2/chat/completions#"})
			response := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{"model": "hy3", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}, nil)
			if scenario == "error" || scenario == "business-error" || scenario == "json-error" || scenario == "truncated" {
				if response.Code == 200 {
					t.Fatalf("failure reported success: %s", response.Body.String())
				}
				return
			}
			if response.Code != 200 {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			payload := gjson.ParseBytes(response.Body.Bytes())
			calls := payload.Get("choices.0.message.tool_calls").Array()
			if len(calls) != 2 || calls[0].Get("id").String() != "call_a" || calls[0].Get("function.arguments").String() != `{"q":"test"}` || calls[1].Get("function.arguments").String() != "{}" {
				t.Fatalf("broken tool aggregation %s", response.Body.String())
			}
			if payload.Get("usage.total_tokens").Int() != 18 || payload.Get("choices.0.message.reasoning_content").String() != "think" {
				t.Fatalf("lost usage/reasoning %s", response.Body.String())
			}
			if scenario == "length" && payload.Get("choices.0.finish_reason").String() != "length" {
				t.Fatal("lost length stop reason")
			}
		})
	}
}

func TestProxy_SingleURLRecordsRuntimeStats(t *testing.T) {
	t.Parallel()
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chat-1","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{{
		name: "single-url-stats", upstreamProtocol: "openai", models: "gpt-test", priority: 100,
	}}, map[int]string{0: upstream.URL})

	response := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model":    "gpt-test",
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	malformedRequest := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":`))
	malformedRequest.Header.Set("Content-Type", "application/json")
	malformedRequest.Header.Set("Authorization", "Bearer test-api-key")
	malformedResponse := httptest.NewRecorder()
	env.engine.ServeHTTP(malformedResponse, malformedRequest)
	if malformedResponse.Code != http.StatusBadRequest {
		t.Fatalf("malformed request status=%d body=%s, want 400", malformedResponse.Code, malformedResponse.Body.String())
	}

	configs, err := env.store.ListConfigs(context.Background())
	if err != nil || len(configs) != 1 {
		t.Fatalf("ListConfigs: configs=%d err=%v", len(configs), err)
	}
	stats := env.server.urlSelector.GetURLStats(configs[0].ID, configs[0].GetURLs())
	if len(stats) != 1 || stats[0].Requests != 1 || stats[0].LatencyMs <= 0 {
		t.Fatalf("unexpected single URL runtime stats: %+v", stats)
	}

	c, w := newTestContext(t, newRequest(http.MethodGet, "/admin/runtime-metrics", nil))
	env.server.HandleRuntimeMetrics(c)
	runtimeResponse := mustParseAPIResponse[map[string]any](t, w.Body.Bytes())
	httpMetrics, ok := runtimeResponse.Data["http_proxy"].(map[string]any)
	if !ok {
		t.Fatalf("HTTP runtime metrics missing: %#v", runtimeResponse.Data)
	}
	if httpMetrics["active_requests"] != float64(0) ||
		httpMetrics["completed_requests"] != float64(2) ||
		httpMetrics["non_error_responses"] != float64(1) ||
		httpMetrics["client_error_responses"] != float64(1) ||
		httpMetrics["streaming_requests"] != float64(0) ||
		httpMetrics["non_streaming_requests"] != float64(1) {
		t.Fatalf("unexpected HTTP runtime metrics: %#v", httpMetrics)
	}
	requestBodyBytes, requestBytesOK := httpMetrics["request_body_bytes"].(float64)
	responseBodyBytes, responseBytesOK := httpMetrics["response_body_bytes"].(float64)
	if !requestBytesOK || !responseBytesOK || requestBodyBytes <= 0 || responseBodyBytes <= 0 {
		t.Fatalf("HTTP byte metrics missing: %#v", httpMetrics)
	}
}

func TestProxy_SuccessResetsExpiredModelCooldownHistory(t *testing.T) {
	t.Parallel()
	const modelName = "gpt-test"

	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chat-1","model":"gpt-test","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{{
		name: "expired-model-cooldown", upstreamProtocol: "openai", models: modelName,
	}}, map[int]string{0: upstream.URL})
	ctx := context.Background()
	configs, err := env.store.ListConfigs(ctx)
	if err != nil || len(configs) != 1 {
		t.Fatalf("ListConfigs: configs=%d err=%v", len(configs), err)
	}

	// Reproduce a completed cooldown that still retains its backoff duration.
	// The channel is selectable again, but the next failure must start fresh
	// after any successful request for the same actual model.
	past := time.Now().Add(-2 * time.Minute)
	duration, err := env.store.BumpModelCooldown(ctx, configs[0].ID, modelName, past, util.StatusFirstByteTimeout)
	if err != nil {
		t.Fatalf("seed expired model cooldown: %v", err)
	}
	if duration != util.TimeoutErrorCooldown {
		t.Fatalf("seed duration=%v, want %v", duration, util.TimeoutErrorCooldown)
	}

	response := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model": modelName, "messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("proxy status=%d body=%s", response.Code, response.Body.String())
	}

	duration, err = env.store.BumpModelCooldown(ctx, configs[0].ID, modelName, time.Now(), util.StatusFirstByteTimeout)
	if err != nil {
		t.Fatalf("bump model cooldown after success: %v", err)
	}
	if duration != util.TimeoutErrorCooldown {
		t.Fatalf("cooldown after success=%v, want fresh %v", duration, util.TimeoutErrorCooldown)
	}
}

func TestProxy_APIKeyCostMultiplierSnapshotsPerKeyInLogs(t *testing.T) {
	t.Parallel()
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chat-1","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	ctx := context.Background()

	urls := channelURLsForTest(upstream.URL)
	for urlIndex := range urls {
		urls[urlIndex].Protocols = []string{util.ProtocolOpenAI}
	}
	cfg := &model.Config{
		Name:                  "key-multiplier-log-snapshot",
		AuthType:              model.AuthTypeAPIKey,
		URLs:                  urls,
		ProtocolTransformMode: model.ProtocolTransformModeLocal,
		Priority:              100,
		Enabled:               true,
		ModelEntries:          []model.ModelEntry{{Model: "gpt-multiplier-test"}},
	}
	created, err := srv.store.CreateConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("CreateConfig: %v", err)
	}
	// round_robin 让两次串行请求各命中一把 Key，各自把倍率快照进日志。
	if err := srv.store.CreateAPIKeysBatch(ctx, []*model.APIKey{
		{ChannelID: created.ID, KeyIndex: 0, APIKey: "sk-low", KeyStrategy: model.KeyStrategyRoundRobin, CostMultiplier: 0.5},
		{ChannelID: created.ID, KeyIndex: 1, APIKey: "sk-high", KeyStrategy: model.KeyStrategyRoundRobin, CostMultiplier: 2.0},
	}); err != nil {
		t.Fatalf("CreateAPIKeysBatch: %v", err)
	}

	injectAPIToken(srv.authService, "test-api-key", 0, 1)
	engine := gin.New()
	srv.SetupRoutes(engine)

	for range 2 {
		response := doProxyRequest(t, engine, "/v1/chat/completions", map[string]any{
			"model":    "gpt-multiplier-test",
			"messages": []any{map[string]any{"role": "user", "content": "hello"}},
		}, nil)
		if response.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
	}
	// 日志是批量异步落库，等待一个完整刷新周期。
	time.Sleep(srv.logService.batchTimeout + 250*time.Millisecond)

	logs, err := srv.store.ListLogs(ctx, time.Now().Add(-time.Minute), 10, 0, &model.LogFilter{Model: "gpt-multiplier-test"})
	if err != nil {
		t.Fatalf("ListLogs: %v", err)
	}
	if len(logs) != 2 {
		t.Fatalf("log count=%d, want 2", len(logs))
	}
	got := make(map[float64]bool, len(logs))
	for _, logEntry := range logs {
		got[logEntry.CostMultiplier] = true
	}
	if !got[0.5] || !got[2.0] {
		t.Fatalf("snapshotted multipliers=%v, want both 0.5 and 2.0", got)
	}
}

func TestProxy_NonResponsesGenerateFieldIsPreserved(t *testing.T) {
	t.Parallel()
	requestBody := make(chan []byte, 1)
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requestBody <- body
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chat-1","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer upstream.Close()
	env := setupProxyTestEnv(t, []testChannel{{
		name: "preserve-generate", upstreamProtocol: "openai", models: "gpt-test", priority: 100,
	}}, map[int]string{0: upstream.URL})

	response := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model": "gpt-test", "generate": true,
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("non-Responses request status=%d body=%s", response.Code, response.Body.String())
	}
	if !gjson.GetBytes(<-requestBody, "generate").Bool() {
		t.Fatal("non-Responses generate field was stripped before reaching upstream")
	}
}

// setupProxyTestEnv 创建指向 mockUpstream 的完整测试 Server
// 每个渠道的 URL 使用 upstreamURLs map（channelIndex → upstreamURL）
func setupProxyTestEnv(t testing.TB, channels []testChannel, upstreamURLs map[int]string) *proxyTestEnv {
	return setupProxyTestEnvWithSettings(t, channels, upstreamURLs, nil)
}

func setupProxyTestEnvWithSettings(
	t testing.TB,
	channels []testChannel,
	upstreamURLs map[int]string,
	settings map[string]string,
) *proxyTestEnv {
	t.Helper()

	srv := newInMemoryServerWithSettings(t, settings)
	store := srv.store

	ctx := context.Background()

	// 创建渠道和 API Key
	for i, ch := range channels {
		upURL := upstreamURLs[i]
		if upURL == "" {
			t.Fatalf("missing upstream URL for channel %d", i)
		}

		priority := ch.priority
		if priority == 0 {
			priority = 100 - i*10 // 按顺序递减优先级
		}

		upstreamProtocol := ch.upstreamProtocol
		if upstreamProtocol == "" {
			upstreamProtocol = util.ProtocolOpenAI
		}
		transformMode := ch.protocolTransformMode
		if transformMode == "" {
			transformMode = model.ProtocolTransformModeLocal
		}

		// 构建模型列表
		var modelEntries []model.ModelEntry
		for _, m := range strings.Split(ch.models, ",") {
			m = strings.TrimSpace(m)
			if m != "" {
				modelEntries = append(modelEntries, model.ModelEntry{Model: m})
			}
		}
		if ch.modelEntries != nil {
			modelEntries = model.CloneModelEntries(ch.modelEntries)
		}

		urls := channelURLsForTest(upURL)
		if transformMode == model.ProtocolTransformModeLocal && ch.authType != model.AuthTypeSystemOne {
			for urlIndex := range urls {
				urls[urlIndex].Protocols = []string{upstreamProtocol}
			}
		}
		cfg := &model.Config{
			Name:                    ch.name,
			AuthType:                ch.authType,
			OAuthCredential:         ch.oauthCredential,
			URLs:                    urls,
			Websockets:              ch.websockets,
			ProtocolTransformMode:   transformMode,
			CustomRequestRules:      ch.customRequestRules,
			CooldownDetectionRules:  ch.cooldownDetectionRules,
			RetryOtherKeysOnFailure: ch.retryOtherKeysOnFailure,
			SameChannelRetries:      ch.sameChannelRetries,
			Priority:                priority,
			Enabled:                 true,
			ModelEntries:            modelEntries,
		}
		created, err := store.CreateConfig(ctx, cfg)
		if err != nil {
			t.Fatalf("CreateConfig for %s: %v", ch.name, err)
		}

		if created.UsesOAuth() {
			continue
		}

		// 创建 API Key
		apiKey := ch.apiKey
		if apiKey == "" {
			apiKey = fmt.Sprintf("sk-test-%d", i)
		}
		err = store.CreateAPIKeysBatch(ctx, []*model.APIKey{
			{ChannelID: created.ID, KeyIndex: 0, APIKey: apiKey},
		})
		if err != nil {
			t.Fatalf("CreateAPIKeysBatch for %s: %v", ch.name, err)
		}
	}

	injectAPIToken(srv.authService, "test-api-key", 0, 1)

	engine := gin.New()
	srv.SetupRoutes(engine)

	return &proxyTestEnv{
		server: srv,
		store:  store,
		engine: engine,
	}
}

func codexProxyTestCredential(t testing.TB, accessToken, refreshToken, accountID string, accountFedRAMP ...bool) string {
	t.Helper()
	credential := &codexauth.Credential{
		Type: codexauth.ChannelType, AccessToken: accessToken, RefreshToken: refreshToken,
		AccountID: accountID, Expired: time.Now().UTC().Add(10 * 24 * time.Hour).Format(time.RFC3339),
	}
	if len(accountFedRAMP) > 0 {
		credential.AccountFedRAMP = accountFedRAMP[0]
	}
	payload, err := credential.JSON()
	if err != nil {
		t.Fatalf("Codex credential JSON: %v", err)
	}
	return payload
}

func antigravityProxyTestCredential(t testing.TB, accessToken string) string {
	t.Helper()
	credential := &antigravityauth.Credential{
		Type: antigravityauth.ChannelType, AccessToken: accessToken, RefreshToken: "rt-antigravity",
		Expired: time.Now().UTC().Add(10 * 24 * time.Hour).Format(time.RFC3339),
		Email:   "gravity@example.com", ProjectID: "gravity-project",
	}
	payload, err := credential.JSON()
	if err != nil {
		t.Fatalf("Antigravity credential JSON: %v", err)
	}
	return payload
}

// 使用真实 Claude Code 身份形态作为代理测试 fixture：64 位 hex device_id
// 和 UUID account_uuid；新版 metadata.user_id 的原生判定只要求 JSON 结构可解析。
const (
	anthropicProxyTestDeviceID    = "b7c1d0e9f28a34556677889900aabbccddeeff00112233445566778899aabbcc"
	anthropicProxyTestAccountUUID = "5c9e1a2b-3d4f-4a5b-8c6d-7e8f90a1b2c3"
)

func anthropicProxyTestCredential(t testing.TB, accessToken string) string {
	t.Helper()
	credential := &anthropicauth.Credential{
		Type: anthropicauth.ChannelType, AccessToken: accessToken, RefreshToken: "rt-anthropic",
		Expired:     time.Now().UTC().Add(10 * 24 * time.Hour).Format(time.RFC3339),
		AccountUUID: anthropicProxyTestAccountUUID, DeviceID: anthropicProxyTestDeviceID,
	}
	payload, err := credential.JSON()
	if err != nil {
		t.Fatalf("Anthropic credential JSON: %v", err)
	}
	return payload
}

func expiredProxyOAuthCredential(t testing.TB, authType, accessToken string) string {
	t.Helper()
	expired := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	var (
		payload string
		err     error
	)
	switch authType {
	case model.AuthTypeCodexOAuth:
		payload, err = (&codexauth.Credential{
			Type: codexauth.ChannelType, AccessToken: accessToken, RefreshToken: "rt-codex",
			AccountID: "account-codex", Expired: expired,
		}).JSON()
	case model.AuthTypeXAIOAuth:
		payload, err = (&xaiauth.Credential{
			Type: xaiauth.ChannelType, AuthKind: "oauth", AccessToken: accessToken,
			RefreshToken: "rt-xai", Expired: expired,
		}).JSON()
	case model.AuthTypeAnthropicOAuth:
		payload, err = (&anthropicauth.Credential{
			Type: anthropicauth.ChannelType, AccessToken: accessToken, RefreshToken: "rt-anthropic",
			Expired: expired, AccountUUID: anthropicProxyTestAccountUUID, DeviceID: anthropicProxyTestDeviceID,
		}).JSON()
	case model.AuthTypeAntigravityOAuth:
		payload, err = (&antigravityauth.Credential{
			Type: antigravityauth.ChannelType, AccessToken: accessToken, RefreshToken: "rt-antigravity",
			Expired: expired, Email: "gravity@example.com", ProjectID: "gravity-project",
		}).JSON()
	default:
		t.Fatalf("unsupported OAuth auth type %q", authType)
	}
	if err != nil {
		t.Fatalf("encode expired %s credential: %v", authType, err)
	}
	return payload
}

func TestProxy_NativeAnthropicAPIKeyRebuildsAndNormalizesWire(t *testing.T) {
	t.Parallel()
	const nativeSessionID = "e03895ad-8b34-4a84-bbf6-002e8909b17b"

	var upstreamBody []byte
	var upstreamHeaders http.Header
	env := setupProxyTestEnv(t, []testChannel{{customRequestRules: &model.CustomRequestRules{Passthrough: new(bool)},
		name: "official-anthropic-api-key", upstreamProtocol: "anthropic", models: "claude-sonnet-4-6", apiKey: "sk-ant-official",
	}}, map[int]string{0: "https://api.anthropic.com"})
	env.server.client = &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		upstreamBody, _ = io.ReadAll(r.Body)
		upstreamHeaders = r.Header.Clone()
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(
				`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"claude-sonnet-4-6","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`,
			)),
		}, nil
	})}

	response := doProxyRequest(t, env.engine, "/v1/messages", map[string]any{
		"model": "claude-sonnet-4-6",
		"system": []any{
			map[string]any{"type": "text", "text": "x-anthropic-billing-header: cc_version=2.1.220.abc; cc_entrypoint=cli; cch=00000;"},
			map[string]any{"type": "text", "text": "keep this native prompt"},
		},
		"messages": []any{
			map[string]any{"role": "user", "content": "first turn"},
			map[string]any{"role": "assistant", "content": []any{map[string]any{
				"type": "tool_use", "id": "toolu_1", "name": "lookup", "input": map[string]any{"q": "x"}, "signature": "foreign",
			}}},
			map[string]any{"role": "user", "content": "second turn"},
		},
		"tools": []any{map[string]any{
			"name": "lookup", "description": "lookup", "input_schema": map[string]any{"type": "object"},
		}},
		"tool_choice": map[string]any{"type": "auto"},
		"thinking":    map[string]any{"type": "auto", "budget_tokens": 4096},
		"temperature": 0.4,
		"top_p":       0.8,
		"top_k":       12,
		"speed":       "fast",
		"diagnostics": map[string]any{"enabled": true},
		"max_tokens":  8192,
	}, map[string]string{
		"anthropic-version":        "2023-06-01",
		"anthropic-beta":           "attacker-beta",
		"User-Agent":               "third-party-client",
		"X-Claude-Code-Session-Id": nativeSessionID,
		"Thread-Id":                "must-not-rehash-native-session",
	})

	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if got := upstreamHeaders.Get("Authorization"); got != "" {
		t.Fatalf("official Anthropic Authorization=%q, want empty", got)
	}
	if got := headerValueFold(upstreamHeaders, "x-api-key"); got != "sk-ant-official" {
		t.Fatalf("official Anthropic x-api-key=%q", got)
	}
	if got := upstreamHeaders.Get("User-Agent"); got != "claude-cli/"+anthropicCLIVersion+" (external, cli)" {
		t.Fatalf("User-Agent=%q", got)
	}
	betas := headerValueFold(upstreamHeaders, "Anthropic-Beta")
	for _, required := range []string{
		"claude-code-20250219", "prompt-caching-scope-2026-01-05",
		"effort-2025-11-24", "context-management-2025-06-27", "fast-mode-2026-02-01",
		"cache-diagnosis-2026-04-07",
	} {
		if !strings.Contains(betas, required) {
			t.Fatalf("Anthropic-Beta=%q missing %q", betas, required)
		}
	}
	// oauth beta 只属于 OAuth 凭证：真实 CLI 用 API Key 时不发，发了就是破绽。
	if strings.Contains(betas, "oauth-2025-04-20") {
		t.Fatalf("Anthropic-Beta=%q declared oauth beta for an API key credential", betas)
	}
	if gotOS, gotArch := headerValueFold(upstreamHeaders, "X-Stainless-OS"), headerValueFold(upstreamHeaders, "X-Stainless-Arch"); gotOS != "Linux" || gotArch != "arm64" {
		t.Fatalf("Stainless platform=%q/%q, want fixed Linux/arm64", gotOS, gotArch)
	}
	// 调用方没声明 cache_control.ttl，网关不主动开 1h 窗口，也就不声明对应的 beta。
	if strings.Contains(betas, "extended-cache-ttl-2025-04-11") {
		t.Fatalf("Anthropic-Beta=%q declared extended cache TTL for a caller that never asked", betas)
	}
	if upstreamHeaders.Get("X-Claude-Code-Session-Id") != nativeSessionID || headerValueFold(upstreamHeaders, "x-client-request-id") == "" ||
		upstreamHeaders.Get("X-Stainless-Runtime-Version") != "v26.3.0" {
		t.Fatalf("Claude Code identity headers=%v", upstreamHeaders)
	}
	// 下游 UA 不是 claude-cli，网关按 Claude Code CLI 指纹重写 body：system 换成
	// 三段式 CLI 提示，调用方原本的 system 降级为 messages 前缀而不是被丢弃。
	if got := gjson.GetBytes(upstreamBody, "system.1.text").String(); got != anthropicClaudeCodeIdentityPrompt {
		t.Fatalf("CLI identity prompt=%q body=%s", got, upstreamBody)
	}
	if got := gjson.GetBytes(upstreamBody, "messages.0.content").String(); !strings.Contains(got, "keep this native prompt") {
		t.Fatalf("caller system prompt was dropped: %s", upstreamBody)
	}
	// 新生成的 CLI billing block 跟随 sub2api 当前模拟路径，不写 CCH。
	if got := gjson.GetBytes(upstreamBody, "system.0.text").String(); !strings.HasPrefix(got, "x-anthropic-billing-header:") ||
		strings.Contains(got, " cch=") {
		t.Fatalf("API-key mimic billing contains CCH: %q", got)
	}
	if gjson.GetBytes(upstreamBody, "temperature").Exists() || gjson.GetBytes(upstreamBody, "top_p").Exists() ||
		gjson.GetBytes(upstreamBody, "top_k").Exists() {
		t.Fatalf("sampling fields survived: %s", upstreamBody)
	}
	if gjson.GetBytes(upstreamBody, "thinking.type").String() != "adaptive" ||
		gjson.GetBytes(upstreamBody, "thinking.budget_tokens").Exists() ||
		gjson.GetBytes(upstreamBody, "output_config.effort").String() != "medium" {
		t.Fatalf("thinking was not normalized: %s", upstreamBody)
	}
	if gjson.GetBytes(upstreamBody, "messages.3.content.0.signature").Exists() ||
		gjson.GetBytes(upstreamBody, "messages.3.content.0.type").String() != "tool_use" {
		t.Fatalf("tool history provenance was not sanitized: %s", upstreamBody)
	}
	// 断点落在 system 尾段与最后一条可缓存消息上；调用方没要 1h，两处都是默认 5m
	// 窗口（无 ttl 字段），与 header 里缺席的 extended-cache-ttl beta 同源。
	if !gjson.GetBytes(upstreamBody, "system.2.cache_control").Exists() ||
		gjson.GetBytes(upstreamBody, "system.2.cache_control.ttl").Exists() {
		t.Fatalf("system cache breakpoint=%s", upstreamBody)
	}
	if !gjson.GetBytes(upstreamBody, "messages.4.content.0.cache_control").Exists() ||
		gjson.GetBytes(upstreamBody, "messages.4.content.0.cache_control.ttl").Exists() {
		t.Fatalf("rolling cache breakpoint=%s", upstreamBody)
	}
}

func TestProxy_AnthropicOAuthPreservesNativePromptAcross400Retry(t *testing.T) {
	t.Parallel()

	const clientAccountUUID = "8b6a103d-024e-45a9-b9c5-313708184fd1"
	const clientSessionID = "e03895ad-8b34-4a84-bbf6-002e8909b17b"
	const clientParentSessionID = "11111111-2222-4333-8444-555555555555"
	credentialJSON := anthropicProxyTestCredential(t, "oauth-anthropic-token")
	credential, err := anthropicauth.ParseCredential([]byte(credentialJSON))
	if err != nil {
		t.Fatal(err)
	}
	identity, err := json.Marshal(map[string]string{
		"device_id": credential.DeviceID, "account_uuid": clientAccountUUID,
		"session_id": clientSessionID, "parent_session_id": clientParentSessionID,
	})
	if err != nil {
		t.Fatal(err)
	}
	var attempts atomic.Int32
	var bodies [][]byte
	var headers []http.Header
	env := setupProxyTestEnv(t, []testChannel{{
		name: "native-anthropic-oauth", upstreamProtocol: "anthropic", models: "claude-sonnet-4-6",
		authType: model.AuthTypeAnthropicOAuth, oauthCredential: credentialJSON,
	}}, map[int]string{0: "https://api.anthropic.com"})
	configs, err := env.store.ListConfigs(context.Background())
	if err != nil || len(configs) != 1 {
		t.Fatalf("list Anthropic OAuth configs: (%v, %v)", err, len(configs))
	}
	channelID := configs[0].ID
	wantDeviceID := anthropicSub2APIClientID(channelID, credential.AccountUUID)
	wantSessionID := anthropicSub2APISessionID(channelID, clientSessionID)
	env.server.client = &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(r.Body)
		bodies = append(bodies, body)
		headers = append(headers, r.Header.Clone())
		if attempts.Add(1) == 1 {
			return &http.Response{
				StatusCode: http.StatusBadRequest,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(strings.NewReader(
					`{"type":"error","error":{"type":"invalid_request_error","message":"thinking blocks are not supported"}}`,
				)),
			}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(
				`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"claude-sonnet-4-6","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`,
			)),
		}, nil
	})}

	response := doProxyRequest(t, env.engine, "/v1/messages", map[string]any{
		"model": "claude-sonnet-4-6",
		"system": []any{
			map[string]any{"type": "text", "text": "x-anthropic-billing-header: cc_version=2.1.220.abc; cc_entrypoint=cli; cch=00000;"},
			map[string]any{"type": "text", "text": "native OAuth prompt"},
		},
		"metadata":   map[string]any{"user_id": string(identity)},
		"messages":   []any{map[string]any{"role": "user", "content": "hello"}},
		"thinking":   map[string]any{"type": "enabled", "budget_tokens": 2048},
		"max_tokens": 4096,
		"tools":      []any{map[string]any{"name": "Bash", "input_schema": map[string]any{"type": "object"}}},
	}, map[string]string{
		"User-Agent":               "claude-cli/" + anthropicCLIVersion + " (external, cli)",
		"X-App":                    "cli",
		"Anthropic-Beta":           "claude-code-20250219",
		"X-Claude-Code-Session-Id": clientSessionID,
	})

	if response.Code != http.StatusOK || attempts.Load() != 2 || len(bodies) != 2 || len(headers) != 2 {
		t.Fatalf("status=%d attempts=%d bodies=%d response=%s", response.Code, attempts.Load(), len(bodies), response.Body.String())
	}
	mappedSessionID := ""
	for index, body := range bodies {
		if got := gjson.GetBytes(body, "system.#").Int(); got != 2 {
			t.Fatalf("attempt %d system blocks=%d body=%s", index+1, got, body)
		}
		if got := gjson.GetBytes(body, "system.1.text").String(); got != "native OAuth prompt" {
			t.Fatalf("attempt %d prompt=%q body=%s", index+1, got, body)
		}
		if got := gjson.GetBytes(body, "messages.#").Int(); got != 1 {
			t.Fatalf("attempt %d messages=%d body=%s", index+1, got, body)
		}
		if got := gjson.GetBytes(body, "tools.0.name").String(); got != "Bash" {
			t.Fatalf("attempt %d native tool renamed to %q", index+1, got)
		}
		userID := gjson.GetBytes(body, "metadata.user_id").String()
		identity := gjson.Parse(userID)
		sessionID := identity.Get("session_id").String()
		parentSessionID := identity.Get("parent_session_id").String()
		if identity.Get("account_uuid").String() != credential.AccountUUID ||
			identity.Get("device_id").String() != wantDeviceID ||
			sessionID != wantSessionID || parentSessionID != "" ||
			headerValueFold(headers[index], "X-Claude-Code-Session-Id") != sessionID {
			t.Fatalf("attempt %d account/session mismatch: header=%v body=%s", index+1, headers[index], body)
		}
		if index == 0 {
			mappedSessionID = sessionID
		} else if sessionID != mappedSessionID {
			t.Fatalf("retry changed mapped session: first=%q second=%q", mappedSessionID, sessionID)
		}
		resigned, signErr := finalizeAnthropicCCH(body)
		if signErr != nil || !bytes.Equal(resigned, body) {
			t.Fatalf("attempt %d CCH mismatch: err=%v body=%s", index+1, signErr, body)
		}
	}
	if !gjson.GetBytes(bodies[0], "thinking").Exists() || gjson.GetBytes(bodies[1], "thinking").Exists() {
		t.Fatalf("thinking downgrade failed: first=%s second=%s", bodies[0], bodies[1])
	}
	simulated := doProxyRequest(t, env.engine, "/v1/messages", map[string]any{
		"model": "claude-sonnet-4-6", "max_tokens": 64,
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}, nil)
	if simulated.Code != http.StatusOK {
		t.Fatalf("simulated status=%d body=%s", simulated.Code, simulated.Body.String())
	}
	simulatedID := gjson.Parse(gjson.GetBytes(bodies[len(bodies)-1], "metadata.user_id").String())
	if got := simulatedID.Get("device_id").String(); got != wantDeviceID {
		t.Fatalf("simulated device_id=%q, native device_id=%q", got, wantDeviceID)
	}
}

// OAuth 模拟路径把客户端工具改名为 Claude Code MCP 形态；同请求重试沿用同一套别名，
// 响应在透传/协议转换前按结构还原，正文里的同名字符串不动。
func TestProxy_AnthropicOAuthAliasesClientToolNames(t *testing.T) {
	t.Parallel()

	aliasPattern := regexp.MustCompile(`^mcp__[a-z]+_[a-z]+__[a-z]+_([A-Za-z0-9_-]+)$`)
	tools := []any{
		map[string]any{"name": "read_file", "input_schema": map[string]any{"type": "object"}},
		map[string]any{"name": "mcp__github__get_issue", "input_schema": map[string]any{"type": "object"}},
		map[string]any{"type": "web_search_20250305", "name": "web_search", "max_uses": 1},
		map[string]any{"type": "custom", "name": "apply_patch", "input_schema": map[string]any{"type": "object"}},
	}
	history := []any{
		map[string]any{"role": "user", "content": "read a.go"},
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "tool_use", "id": "toolu_prev", "name": "read_file", "input": map[string]any{"path": "a.go"}},
		}},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "toolu_prev", "content": "package a"},
		}},
	}
	message := func(alias string) string {
		return fmt.Sprintf(`{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-4-6",`+
			`"content":[{"type":"text","text":"calling %[1]s"},{"type":"tool_use","id":"toolu_1","name":%[2]q,"input":{"path":"b.go"}},`+
			`{"type":"tool_use","id":"toolu_2","name":"mcp__github__get_issue","input":{}}],`+
			`"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":1}}`, alias, alias)
	}
	stream := func(alias string) string {
		return "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-sonnet-4-6\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n" +
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_1\",\"name\":\"" + alias + "\",\"input\":{}}}\n\n" +
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"path\\\":\\\"b.go\\\"}\"}}\n\n" +
			"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":1}}\n\n" +
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	}

	env := setupProxyTestEnv(t, []testChannel{{
		name: "mimic-anthropic-oauth", upstreamProtocol: "anthropic", models: "claude-sonnet-4-6",
		authType: model.AuthTypeAnthropicOAuth, oauthCredential: anthropicProxyTestCredential(t, "oauth-alias-token"),
	}}, map[int]string{0: "https://api.anthropic.com"})
	var mu sync.Mutex
	var bodies [][]byte
	rejectNext := false
	env.server.client = &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		mu.Lock()
		bodies = append(bodies, body)
		reject := rejectNext
		rejectNext = false
		mu.Unlock()
		header := http.Header{"Content-Type": {"application/json"}}
		if reject {
			return &http.Response{StatusCode: http.StatusBadRequest, Header: header, Body: io.NopCloser(strings.NewReader(
				`{"type":"error","error":{"type":"invalid_request_error","message":"thinking blocks are not supported"}}`))}, nil
		}
		if strings.HasSuffix(r.URL.Path, "/count_tokens") {
			return &http.Response{StatusCode: http.StatusOK, Header: header, Body: io.NopCloser(strings.NewReader(`{"input_tokens":7}`))}, nil
		}
		alias := gjson.GetBytes(body, "tools.0.name").String()
		if gjson.GetBytes(body, "stream").Bool() {
			header.Set("Content-Type", "text/event-stream")
			return &http.Response{StatusCode: http.StatusOK, Header: header, Body: io.NopCloser(strings.NewReader(stream(alias)))}, nil
		}
		return &http.Response{StatusCode: http.StatusOK, Header: header, Body: io.NopCloser(strings.NewReader(message(alias)))}, nil
	})}
	send := func(path string, request map[string]any, headers map[string]string, reject bool) (*httptest.ResponseRecorder, [][]byte) {
		t.Helper()
		mu.Lock()
		bodies, rejectNext = nil, reject
		mu.Unlock()
		response := doProxyRequest(t, env.engine, path, request, headers)
		if response.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", path, response.Code, response.Body.String())
		}
		mu.Lock()
		defer mu.Unlock()
		return response, bodies
	}
	// 断言首个客户端工具的 wire 形态，返回它的别名。
	assertWire := func(t *testing.T, body []byte) string {
		t.Helper()
		wireTools := gjson.GetBytes(body, "tools").Array()
		if len(wireTools) != 4 {
			t.Fatalf("wire tools: %s", body)
		}
		readFile, applyPatch := wireTools[0].Get("name").String(), wireTools[3].Get("name").String()
		for name, semantic := range map[string]string{readFile: "read_file", applyPatch: "apply_patch"} {
			if match := aliasPattern.FindStringSubmatch(name); match == nil || match[1] != semantic || len(name) > anthropicToolNameMaxLen {
				t.Fatalf("alias %q does not carry %q: %s", name, semantic, body)
			}
		}
		if strings.Split(readFile, "__")[1] != strings.Split(applyPatch, "__")[1] {
			t.Fatalf("aliases use different MCP servers: %q %q", readFile, applyPatch)
		}
		if wireTools[3].Get("type").Exists() {
			t.Fatalf("custom tool kept type: %s", wireTools[3].Raw)
		}
		if wireTools[1].Get("name").String() != "mcp__github__get_issue" ||
			wireTools[2].Get("name").String() != "web_search" || wireTools[2].Get("type").String() != "web_search_20250305" {
			t.Fatalf("MCP/server tools renamed: %s", body)
		}
		if strings.Contains(string(body), `"read_file"`) || strings.Contains(string(body), `"apply_patch"`) {
			t.Fatalf("original tool name leaked to wire: %s", body)
		}
		return readFile
	}

	var messagesAlias string
	t.Run("json with 400 retry", func(t *testing.T) {
		response, sent := send("/v1/messages", map[string]any{
			"model": "claude-sonnet-4-6", "max_tokens": 4096, "tools": tools, "messages": history,
			"thinking": map[string]any{"type": "enabled", "budget_tokens": 2048},
		}, nil, true)
		if len(sent) != 2 || !gjson.GetBytes(sent[0], "thinking").Exists() || gjson.GetBytes(sent[1], "thinking").Exists() {
			t.Fatalf("expected thinking-downgrade retry, got %d attempts", len(sent))
		}
		messagesAlias = assertWire(t, sent[0])
		if retryAlias := assertWire(t, sent[1]); retryAlias != messagesAlias {
			t.Fatalf("retry re-aliased: first=%q retry=%q", messagesAlias, retryAlias)
		}
		for _, body := range sent {
			var toolUse gjson.Result
			gjson.GetBytes(body, "messages").ForEach(func(_, message gjson.Result) bool {
				message.Get("content").ForEach(func(_, block gjson.Result) bool {
					if block.Get("type").String() == "tool_use" {
						toolUse = block
					}
					return true
				})
				return true
			})
			if toolUse.Get("name").String() != messagesAlias || toolUse.Get("id").String() != "toolu_prev" {
				t.Fatalf("history tool_use not aliased: %s", body)
			}
		}
		out := gjson.Parse(response.Body.String())
		if out.Get("content.1.name").String() != "read_file" || out.Get("content.2.name").String() != "mcp__github__get_issue" {
			t.Fatalf("tool names not restored: %s", response.Body.String())
		}
		if out.Get("content.0.text").String() != "calling "+messagesAlias {
			t.Fatalf("text content rewritten: %s", response.Body.String())
		}
	})

	t.Run("stream", func(t *testing.T) {
		response, sent := send("/v1/messages", map[string]any{
			"model": "claude-sonnet-4-6", "max_tokens": 64, "stream": true, "tools": tools, "messages": history,
			"tool_choice": map[string]any{"type": "tool", "name": "read_file"},
		}, nil, false)
		alias := assertWire(t, sent[0])
		if gjson.GetBytes(sent[0], "tool_choice.name").String() != alias {
			t.Fatalf("tool_choice not aliased: %s", sent[0])
		}
		if strings.Contains(response.Body.String(), alias) {
			t.Fatalf("alias leaked to client stream: %s", response.Body.String())
		}
		restored := false
		for _, frame := range strings.Split(response.Body.String(), "\n\n") {
			_, data := parseSSEEventChunk([]byte(frame))
			if block := gjson.GetBytes(data, "content_block"); block.Get("type").String() == "tool_use" {
				restored = block.Get("name").String() == "read_file"
			}
		}
		if !restored {
			t.Fatalf("stream tool_use not restored: %s", response.Body.String())
		}
	})

	t.Run("count_tokens", func(t *testing.T) {
		response, sent := send("/v1/messages/count_tokens", map[string]any{
			"model": "claude-sonnet-4-6", "tools": tools, "messages": history,
		}, nil, false)
		if alias := assertWire(t, sent[0]); messagesAlias != "" && alias != messagesAlias {
			t.Fatalf("count_tokens alias %q differs from messages alias %q", alias, messagesAlias)
		}
		if gjson.Get(response.Body.String(), "input_tokens").Int() != 7 {
			t.Fatalf("count_tokens response: %s", response.Body.String())
		}
	})
}

// 只有邮箱、没有账号 UUID 的凭证：调用方自己的 device/account 不能原样发给该账号，
// 原生与模拟路径共用账号级 device，account_uuid 与真实 CLI 一样发空。
func TestProxy_AnthropicOAuthWithoutAccountUUIDReplacesCallerIdentity(t *testing.T) {
	t.Parallel()

	credentialJSON, err := (&anthropicauth.Credential{
		Type: anthropicauth.ChannelType, AccessToken: "oauth-setup-token", RefreshToken: "rt-anthropic",
		Expired:      time.Now().UTC().Add(10 * 24 * time.Hour).Format(time.RFC3339),
		EmailAddress: "setup@example.com",
	}).JSON()
	if err != nil {
		t.Fatal(err)
	}
	env := setupProxyTestEnv(t, []testChannel{{
		name: "setup-token-oauth", upstreamProtocol: "anthropic", models: "claude-sonnet-4-6",
		authType: model.AuthTypeAnthropicOAuth, oauthCredential: credentialJSON,
	}}, map[int]string{0: "https://api.anthropic.com"})
	var bodies [][]byte
	env.server.client = &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(r.Body)
		bodies = append(bodies, body)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(
				`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"claude-sonnet-4-6","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`,
			)),
		}, nil
	})}

	const callerDevice = "94a1bc03ba56d8895e3f6f33010c88d32fc9b3165576727d163261ada4af99d1"
	const callerAccount = "8b6a103d-024e-45a9-b9c5-313708184fd1"
	callerIdentity := fmt.Sprintf(`{"device_id":%q,"account_uuid":%q,"session_id":"e03895ad-8b34-4a84-bbf6-002e8909b17b"}`,
		callerDevice, callerAccount)
	native := doProxyRequest(t, env.engine, "/v1/messages", map[string]any{
		"model": "claude-sonnet-4-6", "max_tokens": 64,
		"metadata": map[string]any{"user_id": callerIdentity},
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}, map[string]string{"User-Agent": "claude-cli/" + anthropicCLIVersion + " (external, cli)", "X-App": "cli"})
	simulated := doProxyRequest(t, env.engine, "/v1/messages", map[string]any{
		"model": "claude-sonnet-4-6", "max_tokens": 64,
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}, nil)
	if native.Code != http.StatusOK || simulated.Code != http.StatusOK || len(bodies) != 2 {
		t.Fatalf("native=%d simulated=%d upstream=%d", native.Code, simulated.Code, len(bodies))
	}

	nativeID := gjson.Parse(gjson.GetBytes(bodies[0], "metadata.user_id").String())
	if nativeID.Get("account_uuid").String() != "" || nativeID.Get("device_id").String() == callerDevice {
		t.Fatalf("caller identity reached the account: %s", bodies[0])
	}
	simulatedID := gjson.Parse(gjson.GetBytes(bodies[1], "metadata.user_id").String())
	if simulatedID.Get("account_uuid").String() != "" ||
		simulatedID.Get("device_id").String() != nativeID.Get("device_id").String() {
		t.Fatalf("native and simulated identities differ: native=%s simulated=%s", bodies[0], bodies[1])
	}
}

func TestProxy_AnthropicOAuthPreservesRelayedClaudeCodeRequest(t *testing.T) {
	t.Parallel()

	credentialJSON := anthropicProxyTestCredential(t, "oauth-anthropic-token")
	env := setupProxyTestEnv(t, []testChannel{{
		name: "relayed-anthropic-oauth", upstreamProtocol: "anthropic", models: "claude-sonnet-4-6",
		authType: model.AuthTypeAnthropicOAuth, oauthCredential: credentialJSON,
	}}, map[int]string{0: "https://api.anthropic.com"})
	var capturedBody []byte
	var capturedHeaders http.Header
	env.server.client = &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		capturedBody, _ = io.ReadAll(r.Body)
		capturedHeaders = r.Header.Clone()
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(
				`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"claude-sonnet-4-6","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`,
			)),
		}, nil
	})}

	const billingBlock = "x-anthropic-billing-header: cc_version=2.1.220.abc; cc_entrypoint=claude-vscode;"
	response := doProxyRequest(t, env.engine, "/v1/messages", map[string]any{
		"model": "claude-sonnet-4-6", "max_tokens": 64,
		"metadata": map[string]any{"user_id": "relayed-client-id"},
		"system": []any{
			map[string]any{"type": "text", "text": billingBlock},
			map[string]any{"type": "text", "text": "caller prompt", "cache_control": map[string]any{"type": "ephemeral"}},
		},
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}, map[string]string{"User-Agent": "Go-http-client/1.1"})

	if response.Code != http.StatusOK || capturedHeaders == nil {
		t.Fatalf("status=%d upstream_headers=%v body=%s", response.Code, capturedHeaders, response.Body.String())
	}
	if got, want := gjson.GetBytes(capturedBody, "system.0.text").String(),
		strings.Replace(anthropicBillingHeader("hello", anthropicEffectiveCLIVersion()), "cc_entrypoint=cli;", "cc_entrypoint=claude-vscode;", 1); got != want {
		t.Fatalf("billing block=%q, want %q; body=%s", got, want, capturedBody)
	}
	if got := gjson.GetBytes(capturedBody, "system.1.text").String(); got != "caller prompt" ||
		gjson.GetBytes(capturedBody, "system.1.cache_control.type").String() != "ephemeral" ||
		gjson.GetBytes(capturedBody, "metadata.user_id").String() != "relayed-client-id" {
		t.Fatalf("caller wire changed: %s", capturedBody)
	}
	if got := headerValueFold(capturedHeaders, "User-Agent"); got != "claude-cli/"+anthropicCLIVersion+" (external, cli)" {
		t.Fatalf("upstream User-Agent=%q, want sub2api account fingerprint", got)
	}
	if headerValueFold(capturedHeaders, "X-App") != "cli" ||
		!strings.Contains(headerValueFold(capturedHeaders, "Anthropic-Beta"), "oauth-2025-04-20") ||
		headerValueFold(capturedHeaders, "Authorization") != "Bearer oauth-anthropic-token" {
		t.Fatalf("upstream headers=%v", capturedHeaders)
	}
}

func TestProxy_AnthropicOAuthAccountFingerprintPersistsAndUpgrades(t *testing.T) {
	t.Parallel()
	env := setupProxyTestEnv(t, []testChannel{{
		name: "fingerprint-anthropic-oauth", upstreamProtocol: "anthropic", models: "claude-sonnet-4-6",
		authType: model.AuthTypeAnthropicOAuth, oauthCredential: anthropicProxyTestCredential(t, "oauth-fingerprint-token"),
	}}, map[int]string{0: "https://api.anthropic.com"})
	type capturedRequest struct {
		headers http.Header
		body    []byte
	}
	var captured []capturedRequest
	env.server.client = &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(r.Body)
		captured = append(captured, capturedRequest{headers: r.Header.Clone(), body: body})
		payload := `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"claude-sonnet-4-6","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
		if strings.HasSuffix(r.URL.Path, "/count_tokens") {
			payload = `{"input_tokens":37}`
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}},
			Body: io.NopCloser(strings.NewReader(payload))}, nil
	})}

	const originalSession = "e03895ad-8b34-4a84-bbf6-002e8909b17b"
	body := map[string]any{
		"model": "claude-sonnet-4-6", "max_tokens": 64,
		"metadata": map[string]any{"user_id": `{"device_id":"native-device","account_uuid":"` + anthropicProxyTestAccountUUID + `","session_id":"` + originalSession + `"}`},
		"system":   []any{map[string]any{"type": "text", "text": "x-anthropic-billing-header: cc_version=2.1.220.abc; cc_entrypoint=cli;"}},
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}
	send := func(path string, headers map[string]string) capturedRequest {
		t.Helper()
		response := doProxyRequest(t, env.engine, path, body, headers)
		if response.Code != http.StatusOK {
			t.Fatalf("proxy status=%d body=%s", response.Code, response.Body.String())
		}
		return captured[len(captured)-1]
	}
	baseVersion := anthropicEffectiveCLIVersion()
	baseUA := "claude-cli/" + baseVersion + " (external, claude-vscode)"
	foreign := send("/v1/messages", map[string]string{
		"User-Agent": "OpenAI/Python 2.0", "X-Stainless-Lang": "python", "X-Stainless-OS": "Windows",
	})
	if headerValueFold(foreign.headers, "X-Stainless-Lang") != "js" ||
		headerValueFold(foreign.headers, "X-Stainless-OS") != "Linux" {
		t.Fatalf("foreign SDK poisoned account fingerprint: %v", foreign.headers)
	}
	first := send("/v1/messages", map[string]string{
		"User-Agent": "claude-cli/2.1.220 (external, claude-vscode)", "X-App": "cli",
		"X-Stainless-OS": "Darwin", "X-Stainless-Package-Version": "0.120.0",
	})
	if got := headerValueFold(first.headers, "User-Agent"); got != baseUA {
		t.Fatalf("first UA=%q, want %q", got, baseUA)
	}
	if got := headerValueFold(first.headers, "X-Stainless-OS"); got != "Darwin" {
		t.Fatalf("first Stainless OS=%q", got)
	}
	second := send("/v1/messages", map[string]string{
		"User-Agent": "claude-cli/2.1.100 (external, cli)", "X-App": "cli",
		"X-Stainless-OS": "Windows", "X-Stainless-Package-Version": "0.01.0",
	})
	if headerValueFold(second.headers, "User-Agent") != baseUA ||
		headerValueFold(second.headers, "X-Stainless-OS") != "Darwin" ||
		headerValueFold(second.headers, "X-Stainless-Package-Version") != anthropicStainlessPackageVersion {
		t.Fatalf("older client changed account fingerprint: %v", second.headers)
	}
	parts, ok := parseAnthropicCLIVersion(baseVersion)
	if !ok {
		t.Fatalf("invalid runtime Claude Code version %q", baseVersion)
	}
	newerVersion := fmt.Sprintf("%d.%d.%d", parts[0], parts[1], parts[2]+1)
	newerUA := "claude-cli/" + newerVersion + " (external, cli)"
	third := send("/v1/messages", map[string]string{
		"User-Agent": newerUA, "X-App": "cli", "X-Stainless-OS": "Linux",
	})
	if headerValueFold(third.headers, "User-Agent") != newerUA ||
		headerValueFold(third.headers, "X-Stainless-OS") != "Linux" ||
		headerValueFold(third.headers, "X-Stainless-Package-Version") != anthropicStainlessPackageVersion {
		t.Fatalf("newer client did not merge account fingerprint: %v", third.headers)
	}
	fourth := send("/v1/messages", map[string]string{
		"User-Agent": "claude-cli/9999.0.0 (external, cli)", "X-App": "cli", "X-Stainless-OS": "Windows",
	})
	if headerValueFold(fourth.headers, "User-Agent") != newerUA || headerValueFold(fourth.headers, "X-Stainless-OS") != "Linux" {
		t.Fatalf("implausible UA changed account fingerprint: %v", fourth.headers)
	}
	sameMajorSentinel := send("/v1/messages", map[string]string{
		"User-Agent": fmt.Sprintf("claude-cli/%d.%d.%d (external, cli)", parts[0], parts[1], parts[2]+1000),
		"X-App":      "cli", "X-Stainless-OS": "Windows",
	})
	if headerValueFold(sameMajorSentinel.headers, "User-Agent") != newerUA ||
		headerValueFold(sameMajorSentinel.headers, "X-Stainless-OS") != "Linux" {
		t.Fatalf("same-major sentinel UA changed account fingerprint: %v", sameMajorSentinel.headers)
	}
	crossMajor := send("/v1/messages", map[string]string{
		"User-Agent": "claude-cli/4.0.0 (external, cli)", "X-App": "cli", "X-Stainless-OS": "Windows",
	})
	if headerValueFold(crossMajor.headers, "User-Agent") != newerUA || headerValueFold(crossMajor.headers, "X-Stainless-OS") != "Linux" {
		t.Fatalf("cross-major UA changed account fingerprint: %v", crossMajor.headers)
	}

	configs, err := env.store.ListConfigs(context.Background())
	if err != nil || len(configs) != 1 {
		t.Fatalf("stored configs=%d err=%v", len(configs), err)
	}
	credential, err := anthropicauth.ParseCredential([]byte(configs[0].OAuthCredential))
	if err != nil || credential.Fingerprint == nil || credential.Fingerprint.UserAgent != newerUA ||
		credential.Fingerprint.StainlessOS != "Linux" || credential.Fingerprint.StainlessPackageVersion != anthropicStainlessPackageVersion {
		t.Fatalf("persisted account fingerprint=%+v err=%v", credential, err)
	}
	env.server.anthropicOAuthFingerprintMu.Lock()
	clear(env.server.anthropicOAuthFingerprints)
	env.server.anthropicOAuthFingerprintMu.Unlock()
	env.server.InvalidateChannelListCache()
	count := send("/v1/messages/count_tokens", map[string]string{
		"User-Agent": "claude-cli/2.1.100 (external, cli)", "X-App": "cli", "X-Stainless-OS": "Windows",
	})
	if headerValueFold(count.headers, "User-Agent") != newerUA || headerValueFold(count.headers, "X-Stainless-OS") != "Linux" ||
		gjson.GetBytes(count.body, "metadata").Exists() || anthropicBillingVersion(count.body) != newerVersion ||
		gjson.GetBytes(count.body, "system.0.text").String() != anthropicBillingHeader("hello", newerVersion) {
		t.Fatalf("count_tokens lost persisted fingerprint: headers=%v body=%s", count.headers, count.body)
	}
	simulated := doProxyRequest(t, env.engine, "/v1/messages", map[string]any{
		"model": "claude-sonnet-4-6", "max_tokens": 64,
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}, nil)
	if simulated.Code != http.StatusOK {
		t.Fatalf("simulated status=%d body=%s", simulated.Code, simulated.Body.String())
	}
	simulatedWire := captured[len(captured)-1]
	if got := headerValueFold(simulatedWire.headers, "User-Agent"); got != "claude-cli/"+baseVersion+" (external, cli)" ||
		anthropicBillingVersion(simulatedWire.body) != baseVersion ||
		headerValueFold(simulatedWire.headers, "X-Stainless-OS") != "Linux" {
		t.Fatalf("simulated identity diverged from billing: headers=%v body=%s", simulatedWire.headers, simulatedWire.body)
	}
}

type blockingFingerprintStore struct {
	storage.Store
	blockID int64
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *blockingFingerprintStore) CompareAndSwapOAuthCredential(
	ctx context.Context, channelID int64, expectedAuthType, expectedCredential, nextCredential string,
) (bool, error) {
	if channelID == s.blockID {
		s.once.Do(func() { close(s.entered) })
		select {
		case <-s.release:
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
	return s.Store.CompareAndSwapOAuthCredential(ctx, channelID, expectedAuthType, expectedCredential, nextCredential)
}

type countingFingerprintStore struct {
	storage.Store
	writes atomic.Int32
}

func (s *countingFingerprintStore) CompareAndSwapOAuthCredential(
	ctx context.Context, channelID int64, expectedAuthType, expectedCredential, nextCredential string,
) (bool, error) {
	s.writes.Add(1)
	return s.Store.CompareAndSwapOAuthCredential(ctx, channelID, expectedAuthType, expectedCredential, nextCredential)
}

// Sequential: it moves the process-wide runtime Claude Code version.
func TestAnthropicOAuthFingerprintVersionFloorDoesNotRewriteCredential(t *testing.T) {
	base := anthropicEffectiveCLIVersion()
	// 旧版本学到的 SDK 配对低于内置配对，与抬升后的 UA 不再匹配。
	credential := &anthropicauth.Credential{
		Type: anthropicauth.ChannelType, AccessToken: "oauth-floor", RefreshToken: "rt-anthropic",
		Expired:     time.Now().UTC().Add(10 * 24 * time.Hour).Format(time.RFC3339),
		AccountUUID: anthropicProxyTestAccountUUID, DeviceID: anthropicProxyTestDeviceID,
		Fingerprint: &anthropicauth.Fingerprint{
			UserAgent: "claude-cli/" + base + " (external, cli)", StainlessLang: "js",
			StainlessPackageVersion: "0.94.0", StainlessOS: "Linux", StainlessArch: "arm64",
			StainlessRuntime: "node", StainlessRuntimeVersion: "v24.3.0",
		},
	}
	credentialJSON, err := credential.JSON()
	if err != nil {
		t.Fatal(err)
	}
	env := setupProxyTestEnv(t, []testChannel{{
		name: "floor", upstreamProtocol: "anthropic", models: "claude-sonnet-4-6",
		authType: model.AuthTypeAnthropicOAuth, oauthCredential: credentialJSON,
	}}, map[int]string{0: "https://api.anthropic.com"})
	configs, err := env.store.ListConfigs(context.Background())
	if err != nil || len(configs) != 1 {
		t.Fatalf("configs=%d err=%v", len(configs), err)
	}
	counting := &countingFingerprintStore{Store: env.store}
	env.server.store = counting

	stored := env.server.getAnthropicOAuthFingerprint(context.Background(), configs[0], nil)
	if stored.StainlessPackageVersion != anthropicStainlessPackageVersion ||
		stored.StainlessRuntimeVersion != anthropicStainlessRuntimeVersion {
		t.Fatalf("stale SDK pair was sent: %s/%s", stored.StainlessPackageVersion, stored.StainlessRuntimeVersion)
	}
	if got := counting.writes.Load(); got != 0 {
		t.Fatalf("SDK floor rewrote the credential: writes=%d", got)
	}

	parts, ok := parseAnthropicCLIVersion(base)
	if !ok {
		t.Fatalf("invalid runtime Claude Code version %q", base)
	}
	raised := fmt.Sprintf("%d.%d.%d", parts[0], parts[1], parts[2]+1)
	previous := anthropicRuntimeCLIVersion.Load()
	anthropicRuntimeCLIVersion.Store(&raised)
	t.Cleanup(func() { anthropicRuntimeCLIVersion.Store(previous) })

	fingerprint := env.server.getAnthropicOAuthFingerprint(context.Background(), configs[0], nil)
	if fingerprint.UserAgent != "claude-cli/"+raised+" (external, cli)" {
		t.Fatalf("UA=%q did not follow the raised floor %s", fingerprint.UserAgent, raised)
	}
	if got := counting.writes.Load(); got != 0 {
		t.Fatalf("floor raise rewrote the credential: writes=%d", got)
	}
}

func TestAnthropicOAuthFingerprintPersistenceDoesNotBlockOtherAccounts(t *testing.T) {
	t.Parallel()
	credentialJSON := anthropicProxyTestCredential(t, "oauth-fingerprint")
	env := setupProxyTestEnv(t, []testChannel{
		{name: "one", upstreamProtocol: "anthropic", models: "claude-sonnet-4-6", authType: model.AuthTypeAnthropicOAuth, oauthCredential: credentialJSON},
		{name: "two", upstreamProtocol: "anthropic", models: "claude-sonnet-4-6", authType: model.AuthTypeAnthropicOAuth, oauthCredential: credentialJSON},
	}, map[int]string{0: "https://api.anthropic.com", 1: "https://api.anthropic.com"})
	configs, err := env.store.ListConfigs(context.Background())
	if err != nil || len(configs) != 2 {
		t.Fatalf("configs=%d err=%v", len(configs), err)
	}
	blocked := &blockingFingerprintStore{Store: env.store, blockID: configs[0].ID,
		entered: make(chan struct{}), release: make(chan struct{})}
	env.server.store = blocked
	header := http.Header{"User-Agent": {"claude-cli/2.1.280 (external, cli)"}}
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		env.server.getAnthropicOAuthFingerprint(context.Background(), configs[0], header)
	}()
	select {
	case <-blocked.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("first account did not enter persistence")
	}
	secondDone := make(chan struct{})
	go func() {
		defer close(secondDone)
		env.server.getAnthropicOAuthFingerprint(context.Background(), configs[1], header)
	}()
	select {
	case <-secondDone:
	case <-time.After(2 * time.Second):
		t.Fatal("another account was blocked by fingerprint persistence")
	}
	close(blocked.release)
	select {
	case <-firstDone:
	case <-time.After(2 * time.Second):
		t.Fatal("first account did not finish persistence")
	}
}

func TestProxy_AnthropicNativeBodyRulesKeepNativeWire(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name string
		path string
	}{
		{name: "messages", path: "/v1/messages"},
		{name: "count_tokens", path: "/v1/messages/count_tokens"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			credentialJSON := anthropicProxyTestCredential(t, "oauth-native-rule")
			rules := &model.CustomRequestRules{Body: []model.CustomBodyRule{{
				Action: model.RuleActionRemove, Path: "metadata.user_id",
			}}}
			env := setupProxyTestEnv(t, []testChannel{{
				name: "native-rule", upstreamProtocol: "anthropic", models: "claude-sonnet-4-6",
				authType: model.AuthTypeAnthropicOAuth, oauthCredential: credentialJSON,
				customRequestRules: rules,
			}}, map[int]string{0: "https://api.anthropic.com"})
			var capturedBody []byte
			var capturedHeaders http.Header
			env.server.client = &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				capturedBody, _ = io.ReadAll(r.Body)
				capturedHeaders = r.Header.Clone()
				responseBody := `{"input_tokens":1}`
				if testCase.path == "/v1/messages" {
					responseBody = `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"claude-sonnet-4-6","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(responseBody)),
				}, nil
			})}

			identity := `{"device_id":"native-device","account_uuid":"` + anthropicProxyTestAccountUUID + `","session_id":"e03895ad-8b34-4a84-bbf6-002e8909b17b"}`
			response := doProxyRequest(t, env.engine, testCase.path, map[string]any{
				"model": "claude-sonnet-4-6", "max_tokens": 64,
				"metadata": map[string]any{"user_id": identity},
				"system": []any{
					map[string]any{"type": "text", "text": "x-anthropic-billing-header: cc_version=2.1.280.abc; cc_entrypoint=cli;"},
					map[string]any{"type": "text", "text": "caller prompt"},
				},
				"messages": []any{map[string]any{"role": "user", "content": "hello"}},
			}, map[string]string{
				"User-Agent":        "claude-cli/2.1.280 (external, cli)",
				"X-App":             "cli",
				"Anthropic-Beta":    "claude-code-20250219",
				"Accept-Language":   "zh-CN,zh;q=0.9",
				"Sec-Fetch-Mode":    "cors",
				"X-Stainless-Async": "false",
			})
			if response.Code != http.StatusOK || capturedHeaders == nil {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if gjson.GetBytes(capturedBody, "metadata.user_id").Exists() ||
				gjson.GetBytes(capturedBody, "system.#").Int() != 2 ||
				gjson.GetBytes(capturedBody, "system.1.text").String() != "caller prompt" ||
				gjson.GetBytes(capturedBody, "messages.#").Int() != 1 {
				t.Fatalf("native body changed after rule: %s", capturedBody)
			}
			for name, want := range map[string]string{
				"User-Agent":        "claude-cli/" + anthropicEffectiveCLIVersion() + " (external, cli)",
				"Accept-Language":   "zh-CN,zh;q=0.9",
				"Sec-Fetch-Mode":    "cors",
				"X-Stainless-Async": "false",
				"Authorization":     "Bearer oauth-native-rule",
			} {
				if got := headerValueFold(capturedHeaders, name); got != want {
					t.Fatalf("%s=%q, want %q; headers=%v", name, got, want, capturedHeaders)
				}
			}
		})
	}
}

func TestProxy_AnthropicCountTokensUsesUpstreamOAuthWire(t *testing.T) {
	t.Parallel()
	credentialJSON := anthropicProxyTestCredential(t, "oauth-count-token")
	env := setupProxyTestEnv(t, []testChannel{{
		name: "anthropic-count-tokens", upstreamProtocol: "anthropic", models: "claude-sonnet-4-6",
		authType: model.AuthTypeAnthropicOAuth, oauthCredential: credentialJSON,
	}}, map[int]string{0: "https://api.anthropic.com"})
	type capturedRequest struct {
		url     string
		headers http.Header
		body    []byte
	}
	var sent []capturedRequest
	env.server.client = &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(r.Body)
		sent = append(sent, capturedRequest{url: r.URL.String(), headers: r.Header.Clone(), body: body})
		header := http.Header{"Content-Type": []string{"application/json"}}
		payload := []byte(`{"input_tokens":37}`)
		// 上游按请求声明的编码压缩：模拟头声明了 br，网关必须自己解开再交给客户端。
		if strings.Contains(headerValueFold(r.Header, "Accept-Encoding"), "br") {
			var compressed bytes.Buffer
			writer := brotli.NewWriter(&compressed)
			_, _ = writer.Write(payload)
			_ = writer.Close()
			header.Set("Content-Encoding", "br")
			payload = compressed.Bytes()
		}
		return &http.Response{StatusCode: http.StatusOK, Header: header, Body: io.NopCloser(bytes.NewReader(payload))}, nil
	})}

	requestBody := map[string]any{
		"model": "claude-sonnet-4-6", "max_tokens": 1024, "temperature": 0.4,
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}
	configs, err := env.store.ListConfigs(context.Background())
	if err != nil || len(configs) != 1 {
		t.Fatalf("list Anthropic OAuth configs: (%v, %v)", err, len(configs))
	}
	channelID := configs[0].ID
	_ = channelID
	const originalSessionID = "e03895ad-8b34-4a84-bbf6-002e8909b17b"
	response := doProxyRequest(t, env.engine, "/v1/messages/count_tokens", requestBody, nil)
	if response.Code != http.StatusOK || response.Body.String() != `{"input_tokens":37}` ||
		response.Header().Get("Content-Encoding") != "" || len(sent) != 1 {
		t.Fatalf("mimic status=%d sent=%d headers=%v body=%q", response.Code, len(sent), response.Header(), response.Body.String())
	}
	mimic := sent[0]
	if mimic.url != "https://api.anthropic.com/v1/messages/count_tokens?beta=true" ||
		headerValueFold(mimic.headers, "Authorization") != "Bearer oauth-count-token" ||
		headerValueFold(mimic.headers, "X-Stainless-Runtime-Version") != anthropicStainlessRuntimeVersion ||
		headerValueFold(mimic.headers, "X-Claude-Code-Session-Id") != "" ||
		headerValueFold(mimic.headers, "Accept-Encoding") != "gzip, deflate, br, zstd" ||
		!strings.Contains(headerValueFold(mimic.headers, "Anthropic-Beta"), "token-counting-2024-11-01") ||
		gjson.GetBytes(mimic.body, "metadata").Exists() ||
		gjson.GetBytes(mimic.body, "max_tokens").Exists() || gjson.GetBytes(mimic.body, "temperature").Exists() {
		t.Fatalf("mimic wire url=%s headers=%v body=%s", mimic.url, mimic.headers, mimic.body)
	}

	const userID = `{"device_id":"native-device","account_uuid":"` + anthropicProxyTestAccountUUID + `","session_id":"e03895ad-8b34-4a84-bbf6-002e8909b17b"}`
	requestBody["metadata"] = map[string]any{"user_id": userID}
	requestBody["system"] = []any{map[string]any{"type": "text", "text": "x-anthropic-billing-header: cc_version=2.1.280.abc; cc_entrypoint=cli;"}}
	response = doProxyRequest(t, env.engine, "/v1/messages/count_tokens", requestBody, map[string]string{
		"User-Agent": "claude-cli/2.1.280 (external, cli)", "X-App": "cli",
		"Anthropic-Beta": "claude-code-20250219",
	})
	if response.Code != http.StatusOK || gjson.Get(response.Body.String(), "input_tokens").Int() != 37 || len(sent) != 2 {
		t.Fatalf("native status=%d sent=%d body=%s", response.Code, len(sent), response.Body.String())
	}
	native := sent[1]
	if gjson.GetBytes(native.body, "metadata").Exists() ||
		gjson.GetBytes(native.body, "system.0.text").String() != anthropicBillingHeader("hello", anthropicCLIVersion) ||
		headerValueFold(native.headers, "User-Agent") != "claude-cli/"+anthropicEffectiveCLIVersion()+" (external, cli)" ||
		!strings.Contains(headerValueFold(native.headers, "Anthropic-Beta"), "token-counting-2024-11-01") {
		t.Fatalf("native wire headers=%v body=%s", native.headers, native.body)
	}

	const otherAccountUUID = "8b6a103d-024e-45a9-b9c5-313708184fd1"
	requestBody["metadata"] = map[string]any{"user_id": `{"device_id":"native-device","account_uuid":"` + otherAccountUUID + `","session_id":"` + originalSessionID + `"}`}
	response = doProxyRequest(t, env.engine, "/v1/messages/count_tokens", requestBody, map[string]string{
		"User-Agent": "claude-cli/2.1.280 (external, cli)", "X-App": "cli",
		"Anthropic-Beta": "claude-code-20250219",
	})
	if response.Code != http.StatusOK || len(sent) != 3 {
		t.Fatalf("rebased native status=%d sent=%d body=%s", response.Code, len(sent), response.Body.String())
	}
	rebased := sent[2]
	if gjson.GetBytes(rebased.body, "metadata").Exists() ||
		headerValueFold(rebased.headers, "X-Claude-Code-Session-Id") != "" ||
		gjson.GetBytes(rebased.body, "max_tokens").Exists() ||
		gjson.GetBytes(rebased.body, "temperature").Exists() {
		t.Fatalf("rebased native wire headers=%v body=%s", rebased.headers, rebased.body)
	}

	legacyUserID := "user_" + anthropicProxyTestDeviceID + "_account_" + otherAccountUUID + "_session_" + originalSessionID
	requestBody["metadata"] = map[string]any{"user_id": legacyUserID}
	response = doProxyRequest(t, env.engine, "/v1/messages/count_tokens", requestBody, map[string]string{
		"User-Agent": "claude-cli/2.1.280 (external, cli)", "X-App": "cli",
		"Anthropic-Beta": "claude-code-20250219", "X-Claude-Code-Session-Id": originalSessionID,
	})
	if response.Code != http.StatusOK || len(sent) != 4 {
		t.Fatalf("legacy native status=%d sent=%d body=%s", response.Code, len(sent), response.Body.String())
	}
	legacy := sent[3]
	if gjson.GetBytes(legacy.body, "metadata").Exists() ||
		headerValueFold(legacy.headers, "X-Claude-Code-Session-Id") != "" {
		t.Fatalf("legacy native wire headers=%v body=%s", legacy.headers, legacy.body)
	}
}

func TestProxy_AnthropicCountTokensAPIKeyUsesUpstream(t *testing.T) {
	t.Parallel()
	env := setupProxyTestEnv(t, []testChannel{{customRequestRules: &model.CustomRequestRules{Passthrough: new(bool)},
		name: "anthropic-count-tokens-key", upstreamProtocol: "anthropic", models: "claude-sonnet-4-6",
		apiKey: "sk-ant-count-token",
	}}, map[int]string{0: "https://api.anthropic.com"})
	var sentBody []byte
	var sentURL string
	var sentHeaders http.Header
	env.server.client = &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		sentURL = r.URL.String()
		sentHeaders = r.Header.Clone()
		sentBody, _ = io.ReadAll(r.Body)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"input_tokens":19}`)),
		}, nil
	})}
	response := doProxyRequest(t, env.engine, "/v1/messages/count_tokens", map[string]any{
		"model": "claude-sonnet-4-6", "max_tokens": 100,
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}, nil)
	if response.Code != http.StatusOK || gjson.Get(response.Body.String(), "input_tokens").Int() != 19 ||
		sentURL != "https://api.anthropic.com/v1/messages/count_tokens?beta=true" ||
		headerValueFold(sentHeaders, "x-api-key") != "sk-ant-count-token" ||
		headerValueFold(sentHeaders, "Authorization") != "" ||
		gjson.GetBytes(sentBody, "max_tokens").Exists() {
		t.Fatalf("status=%d url=%s headers=%v upstream=%s downstream=%s", response.Code,
			sentURL, sentHeaders, sentBody, response.Body.String())
	}
	entry := waitForCountTokensLogs(t, env, 1)[0]
	if entry.StatusCode != http.StatusOK || entry.ChannelID <= 0 || entry.Cost != 0 ||
		entry.Model != "claude-sonnet-4-6" || entry.InputTokens != 19 {
		t.Fatalf("count_tokens log=%+v", entry)
	}
}

func TestProxy_AnthropicCountTokensRecognizesNativeUAWithoutMetadata(t *testing.T) {
	t.Parallel()
	env := setupProxyTestEnv(t, []testChannel{{
		name: "native-count", upstreamProtocol: "anthropic", models: "claude-sonnet-4-6",
		authType: model.AuthTypeAnthropicOAuth, oauthCredential: anthropicProxyTestCredential(t, "native-count"),
	}}, map[int]string{0: "https://api.anthropic.com"})
	var sentBody []byte
	var sentHeaders http.Header
	env.server.client = &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		sentBody, _ = io.ReadAll(r.Body)
		sentHeaders = r.Header.Clone()
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{"input_tokens":11}`))}, nil
	})}
	response := doProxyRequest(t, env.engine, "/v1/messages/count_tokens", map[string]any{
		"model": "claude-sonnet-4-6", "messages": []any{map[string]any{"role": "user", "content": "hello"}},
		"tools": []any{map[string]any{"name": "work", "input_schema": map[string]any{"type": "object"}}},
	}, map[string]string{
		"User-Agent": "claude-cli/2.1.280 (external, cli)", "Anthropic-Beta": "claude-code-20250219,custom-beta",
	})
	betas := headerValueFold(sentHeaders, "Anthropic-Beta")
	if response.Code != http.StatusOK || !strings.Contains(betas, "custom-beta") ||
		strings.Contains(betas, "prompt-caching-scope-2026-01-05") ||
		gjson.GetBytes(sentBody, "metadata").Exists() {
		t.Fatalf("status=%d beta=%q body=%s response=%s", response.Code, betas, sentBody, response.Body.String())
	}
}

func TestProxy_AnthropicCountTokensCustomOriginUsesLocalEstimate(t *testing.T) {
	t.Parallel()
	env := setupProxyTestEnv(t, []testChannel{{
		name: "third-party-anthropic", upstreamProtocol: "anthropic", models: "claude-sonnet-4-6", apiKey: "sk-test",
	}}, map[int]string{0: "https://relay.example.com"})
	var upstreamCalls atomic.Int32
	env.server.client = &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		upstreamCalls.Add(1)
		return nil, errors.New("count_tokens should stay local")
	})}
	response := doProxyRequest(t, env.engine, "/v1/messages/count_tokens", map[string]any{
		"model":    "claude-sonnet-4-6",
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}, nil)
	if response.Code != http.StatusOK || gjson.Get(response.Body.String(), "input_tokens").Int() <= 0 || upstreamCalls.Load() != 0 {
		t.Fatalf("status=%d upstreamCalls=%d body=%s", response.Code, upstreamCalls.Load(), response.Body.String())
	}
}

func TestProxy_AnthropicCountTokensWithoutChannelsUsesLocalEstimate(t *testing.T) {
	t.Parallel()
	env := setupProxyTestEnv(t, nil, nil)
	env.server.configService.cache["debug_log_enabled"] = &model.SystemSetting{Key: "debug_log_enabled", Value: "true"}
	response := doProxyRequest(t, env.engine, "/v1/messages/count_tokens", map[string]any{
		"model": "claude-sonnet-4-6", "messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}, nil)
	if response.Code != http.StatusOK || gjson.Get(response.Body.String(), "input_tokens").Int() <= 0 {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	entry := waitForCountTokensLogs(t, env, 1)[0]
	if entry.StatusCode != http.StatusOK || entry.ChannelID != 0 || entry.Model != "claude-sonnet-4-6" ||
		entry.Message != "local: no official channel" ||
		int64(entry.InputTokens) != gjson.Get(response.Body.String(), "input_tokens").Int() {
		t.Fatalf("local count_tokens log=%+v body=%s", entry, response.Body.String())
	}
	debug, err := env.store.GetDebugLogByLogID(context.Background(), entry.ID)
	if err != nil || debug == nil || gjson.GetBytes(debug.ReqBody, "model").String() != "claude-sonnet-4-6" ||
		debug.RespStatus != http.StatusOK || string(debug.RespBody) != response.Body.String() {
		t.Fatalf("local count_tokens debug=%+v err=%v", debug, err)
	}
}

func TestProxy_AnthropicCountTokensFailureFallsBackWithoutModelCooldown(t *testing.T) {
	t.Parallel()
	// A rejected body fails the same way on every official account, so a 400
	// must not fan out to the second channel before the local estimate.
	env := setupProxyTestEnv(t, []testChannel{
		{name: "official-anthropic", upstreamProtocol: "anthropic", models: "claude-sonnet-4-6", apiKey: "sk-ant-test"},
		{name: "official-anthropic-2", upstreamProtocol: "anthropic", models: "claude-sonnet-4-6", apiKey: "sk-ant-test-2"},
	}, map[int]string{0: "https://api.anthropic.com", 1: "https://api.anthropic.com"})
	env.server.configService.cache["debug_log_enabled"] = &model.SystemSetting{Key: "debug_log_enabled", Value: "true"}
	var countCalls, messageCalls atomic.Int32
	env.server.client = &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/count_tokens") {
			countCalls.Add(1)
			return &http.Response{StatusCode: http.StatusBadRequest, Header: http.Header{"Content-Type": {"application/json"}},
				Body: io.NopCloser(strings.NewReader(`{"error":{"type":"invalid_request_error","message":"unsupported"}}`))}, nil
		}
		messageCalls.Add(1)
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"claude-sonnet-4-6","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))}, nil
	})}
	count := doProxyRequest(t, env.engine, "/v1/messages/count_tokens", map[string]any{
		"model": "claude-sonnet-4-6", "messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}, nil)
	message := doProxyRequest(t, env.engine, "/v1/messages", map[string]any{
		"model": "claude-sonnet-4-6", "max_tokens": 32,
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}, nil)
	if count.Code != http.StatusOK || gjson.Get(count.Body.String(), "input_tokens").Int() <= 0 ||
		message.Code != http.StatusOK || countCalls.Load() != 1 || messageCalls.Load() != 1 {
		t.Fatalf("count=%d %s, message=%d %s, upstream count=%d messages=%d",
			count.Code, count.Body.String(), message.Code, message.Body.String(), countCalls.Load(), messageCalls.Load())
	}
	// 上游失败与本地估算各记一条 count_tokens 日志，请求日志（渠道健康度来源）只有 messages。
	countLogs := waitForCountTokensLogs(t, env, 2)
	statuses := map[int]int64{}
	for _, entry := range countLogs {
		statuses[entry.StatusCode] = entry.ChannelID
		if entry.StatusCode != http.StatusBadRequest {
			continue
		}
		// 上游失败那条的 debug 保留官方原始响应，本地估算那条只有本地应答。
		debug, err := env.store.GetDebugLogByLogID(context.Background(), entry.ID)
		if err != nil || debug == nil || !strings.HasPrefix(debug.ReqURL, "https://api.anthropic.com/v1/messages/count_tokens") ||
			debug.RespStatus != http.StatusBadRequest || gjson.GetBytes(debug.RespBody, "error.message").String() != "unsupported" {
			t.Fatalf("upstream count_tokens debug=%+v err=%v", debug, err)
		}
	}
	if channelID, ok := statuses[http.StatusBadRequest]; !ok || channelID <= 0 {
		t.Fatalf("missing upstream 400 count_tokens log: %+v", countLogs)
	}
	if channelID, ok := statuses[http.StatusOK]; !ok || channelID != 0 {
		t.Fatalf("missing local estimate count_tokens log: %+v", countLogs)
	}
	waitForProxyLog(t, env, "claude-sonnet-4-6")
	proxyLogs, err := env.store.ListLogs(context.Background(), time.Now().Add(-time.Minute), 20, 0,
		&model.LogFilter{LogSource: model.LogSourceProxy})
	if err != nil || len(proxyLogs) != 1 || proxyLogs[0].StatusCode != http.StatusOK {
		t.Fatalf("proxy logs=%+v err=%v, want only the messages request", proxyLogs, err)
	}
}

func TestProxy_AnthropicCountTokensTokenRestrictionsUseLocalEstimate(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name     string
		restrict func(t *testing.T, env *proxyTestEnv, tokenHash string, channelID int64)
	}{
		{name: "model whitelist", restrict: func(_ *testing.T, env *proxyTestEnv, tokenHash string, _ int64) {
			env.server.authService.authTokenModels[tokenHash] = []string{"claude-opus-4-6"}
		}},
		{name: "channel restriction", restrict: func(t *testing.T, env *proxyTestEnv, tokenHash string, channelID int64) {
			env.server.authService.authTokenChannels[tokenHash] = mustChannelRestriction(t, model.ChannelRestrictionModeDeny, channelID)
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			env := setupProxyTestEnv(t, []testChannel{{
				name: "official-anthropic", upstreamProtocol: "anthropic", models: "claude-sonnet-4-6", apiKey: "sk-ant-test",
			}}, map[int]string{0: "https://api.anthropic.com"})
			configs, err := env.store.ListConfigs(context.Background())
			if err != nil || len(configs) != 1 {
				t.Fatalf("configs=%d err=%v", len(configs), err)
			}
			env.server.authService.authTokensMux.Lock()
			testCase.restrict(t, env, model.HashToken("test-api-key"), configs[0].ID)
			env.server.authService.authTokensMux.Unlock()
			var upstreamCalls atomic.Int32
			env.server.client = &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
				upstreamCalls.Add(1)
				return nil, errors.New("restricted count_tokens should stay local")
			})}
			response := doProxyRequest(t, env.engine, "/v1/messages/count_tokens", map[string]any{
				"model": "claude-sonnet-4-6", "messages": []any{map[string]any{"role": "user", "content": "hello"}},
			}, nil)
			if response.Code != http.StatusOK || gjson.Get(response.Body.String(), "input_tokens").Int() <= 0 ||
				upstreamCalls.Load() != 0 {
				t.Fatalf("status=%d upstreamCalls=%d body=%s", response.Code, upstreamCalls.Load(), response.Body.String())
			}
		})
	}
}

func TestProxy_AnthropicLegacyModelRejectsMidConversationSystemLocally(t *testing.T) {
	t.Parallel()
	var upstreamCalls atomic.Int32
	env := setupProxyTestEnv(t, []testChannel{{customRequestRules: &model.CustomRequestRules{Passthrough: new(bool)},
		name: "legacy-anthropic", upstreamProtocol: "anthropic", models: "claude-haiku-4-5-20251001", apiKey: "sk-ant",
	}}, map[int]string{0: "https://api.anthropic.com"})
	env.server.client = &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		upstreamCalls.Add(1)
		return nil, errors.New("legacy validation should stop before upstream")
	})}

	response := doProxyRequest(t, env.engine, "/v1/messages", map[string]any{
		"model": "claude-haiku-4-5-20251001",
		"messages": []any{
			map[string]any{"role": "user", "content": "hello"},
			map[string]any{"role": "system", "content": "late system"},
		},
		"max_tokens": 64,
	}, nil)
	if response.Code != http.StatusBadRequest || upstreamCalls.Load() != 0 {
		t.Fatalf("status=%d upstream_calls=%d body=%s", response.Code, upstreamCalls.Load(), response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "does not support system messages") {
		t.Fatalf("validation response=%s", response.Body.String())
	}
}

func TestProxy_AnthropicCompatibleGatewayOwnsLegacySystemTurns(t *testing.T) {
	t.Parallel()
	var upstreamCalls atomic.Int32
	env := setupProxyTestEnv(t, []testChannel{{
		name: "compatible-anthropic", upstreamProtocol: "anthropic", models: "claude-haiku-4-5-20251001", apiKey: "sk-ant",
	}}, map[int]string{0: "https://anthropic-gateway.example.com"})
	env.server.client = &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		upstreamCalls.Add(1)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(
				`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"claude-haiku-4-5-20251001","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`,
			)),
		}, nil
	})}

	response := doProxyRequest(t, env.engine, "/v1/messages", map[string]any{
		"model": "claude-haiku-4-5-20251001",
		"messages": []any{
			map[string]any{"role": "user", "content": "hello"},
			map[string]any{"role": "system", "content": "gateway-owned turn"},
		},
		"max_tokens": 64,
	}, nil)
	if response.Code != http.StatusOK || upstreamCalls.Load() != 1 {
		t.Fatalf("status=%d upstream_calls=%d body=%s", response.Code, upstreamCalls.Load(), response.Body.String())
	}
}

// TestProxy_NativeAnthropicAPIKeyPreservesExplicitCachePolicy 守住原生 Claude Code
// 请求的直通契约：调用方已经自带完整 CLI 指纹时，API Key 网关不处理 CCH，
// 也不重写 body。
// 与 TestProxy_AnthropicOAuthPreservesNativePromptAcross400Retry 对称——直通判定
// 看的是请求形态，不是凭证形态，API Key 渠道同样适用。
func TestProxy_NativeAnthropicAPIKeyPreservesExplicitCachePolicy(t *testing.T) {
	t.Parallel()
	const nativeSessionID = "e03895ad-8b34-4a84-bbf6-002e8909b17b"

	identity, err := json.Marshal(map[string]string{
		"device_id":    "3934be64e8c64eca962e5841b999eeebb24c7f889371b4d95306f39301f07bc9",
		"account_uuid": "9b079a70-8780-5404-bbaf-d74217f5adfd",
		"session_id":   nativeSessionID,
	})
	if err != nil {
		t.Fatal(err)
	}

	var upstreamBody []byte
	env := setupProxyTestEnv(t, []testChannel{{
		name: "anthropic-explicit-cache", upstreamProtocol: "anthropic", models: "claude-sonnet-4-6", apiKey: "sk-ant",
	}}, map[int]string{0: "https://anthropic-gateway.example.com"})
	env.server.client = &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		upstreamBody, _ = io.ReadAll(r.Body)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(
				`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"claude-sonnet-4-6","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`,
			)),
		}, nil
	})}

	response := doProxyRequest(t, env.engine, "/v1/messages", map[string]any{
		"model": "claude-sonnet-4-6",
		"system": []any{
			map[string]any{"type": "text", "text": "x-anthropic-billing-header: cc_version=2.1.220.abc; cc_entrypoint=cli; cch=4d721;"},
			map[string]any{
				"type": "text", "text": "explicit cache only",
				"cache_control": map[string]any{"type": "ephemeral", "ttl": "1h"},
			},
		},
		"metadata": map[string]any{"user_id": string(identity)},
		"messages": []any{
			map[string]any{"role": "user", "content": "first"},
			map[string]any{"role": "assistant", "content": "ok"},
			map[string]any{"role": "user", "content": "second"},
		},
		"tools":      []any{map[string]any{"name": "lookup", "input_schema": map[string]any{"type": "object"}}},
		"max_tokens": 1024,
	}, map[string]string{
		"anthropic-version":        "2023-06-01",
		"User-Agent":               "claude-cli/" + anthropicCLIVersion + " (external, cli)",
		"X-App":                    "cli",
		"Anthropic-Beta":           "claude-code-20250219",
		"X-Claude-Code-Session-Id": nativeSessionID,
	})

	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if got := gjson.GetBytes(upstreamBody, "system.#").Int(); got != 2 {
		t.Fatalf("native system blocks=%d body=%s", got, upstreamBody)
	}
	if gjson.GetBytes(upstreamBody, "system.1.cache_control.ttl").String() != "1h" {
		t.Fatalf("explicit cache breakpoint changed: %s", upstreamBody)
	}
	if gjson.GetBytes(upstreamBody, "tools.0.cache_control").Exists() ||
		gjson.GetBytes(upstreamBody, "messages.2.content.0.cache_control").Exists() {
		t.Fatalf("automatic cache breakpoints were added beside explicit policy: %s", upstreamBody)
	}
	if got := gjson.GetBytes(upstreamBody, "system.0.text").String(); got != "x-anthropic-billing-header: cc_version=2.1.220.abc; cc_entrypoint=cli; cch=4d721;" {
		t.Fatalf("caller-owned API-key CCH changed: %q", got)
	}
}

func TestProxy_NativeAnthropic400RepairsThinkingBudget(t *testing.T) {
	t.Parallel()
	var attempts atomic.Int32
	var bodies [][]byte
	env := setupProxyTestEnv(t, []testChannel{{customRequestRules: &model.CustomRequestRules{Passthrough: new(bool)}, name: "anthropic-repair", upstreamProtocol: "anthropic", models: "claude-sonnet-4-6", apiKey: "sk-ant"}}, map[int]string{0: "https://anthropic-gateway.example.com"})
	env.server.client = &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		bodies = append(bodies, body)
		if attempts.Add(1) == 1 {
			return &http.Response{StatusCode: http.StatusBadRequest, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"type":"invalid_request_error","message":"thinking budget_tokens must be less than max_tokens"}}`))}, nil
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"claude-sonnet-4-6","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))}, nil
	})}
	response := doProxyRequest(t, env.engine, "/v1/messages", map[string]any{
		"model": "claude-sonnet-4-6", "max_tokens": 10000, "thinking": map[string]any{"type": "enabled", "budget_tokens": 10000}, "messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}, map[string]string{"anthropic-version": "2023-06-01"})
	if response.Code != http.StatusOK || attempts.Load() != 2 || len(bodies) != 2 {
		t.Fatalf("status=%d attempts=%d bodies=%d response=%s", response.Code, attempts.Load(), len(bodies), response.Body.String())
	}
	body := bodies[1]
	if gjson.GetBytes(body, "thinking.type").String() != "enabled" || gjson.GetBytes(body, "thinking.budget_tokens").Int() != 32000 || gjson.GetBytes(body, "max_tokens").Int() != 64000 {
		t.Fatalf("thinking budget was not repaired: %s", body)
	}
}

func TestProxy_NativeAnthropicToolErrorsPreserveRequest(t *testing.T) {
	t.Parallel()
	for _, test := range []struct{ name, upstreamError string }{
		{"unsupported tools", `{"error":{"type":"invalid_request_error","message":"tool_use blocks are not supported"}}`},
		{"missing result", `{"error":{"type":"invalid_request_error","message":"tool_use ids were found without tool_result blocks immediately after: toolu_1"}}`},
		{"wrapped Google error", `{"error":{"code":400,"status":"INVALID_ARGUMENT","message":"{\"error\":{\"type\":\"invalid_request_error\",\"message\":\"tool_use ids were found without tool_result blocks immediately after: toolu_1\"}}"}}`},
		{"unsupported tool choice", `{"error":{"type":"invalid_request_error","message":"tool_choice is not supported"}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			upstreamError := test.upstreamError
			t.Parallel()
			var attempts atomic.Int32
			env := setupProxyTestEnv(t, []testChannel{{name: "anthropic-tool-rejection", upstreamProtocol: "anthropic", models: "claude-sonnet-4-6", apiKey: "sk-ant"}}, map[int]string{0: "https://anthropic-gateway.example.com"})
			env.server.client = &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				attempts.Add(1)
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				for path, want := range map[string]string{"tools.0.name": "lookup", "tool_choice.type": "auto", "messages.1.content.0.type": "tool_use", "messages.1.content.0.id": "toolu_1", "messages.1.content.0.input.q": "x", "messages.2.content.0.type": "tool_result", "messages.2.content.0.tool_use_id": "toolu_1", "messages.2.content.0.content": "result"} {
					if got := gjson.GetBytes(body, path).String(); got != want {
						t.Errorf("%s=%q, want %q", path, got, want)
					}
				}
				return &http.Response{StatusCode: http.StatusBadRequest, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(upstreamError))}, nil
			})}
			response := doProxyRequest(t, env.engine, "/v1/messages", map[string]any{
				"model": "claude-sonnet-4-6", "max_tokens": 4096,
				"tools": []any{map[string]any{"name": "lookup", "input_schema": map[string]any{"type": "object"}}}, "tool_choice": map[string]any{"type": "auto"},
				"messages": []any{
					map[string]any{"role": "user", "content": "call a tool"},
					map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": "toolu_1", "name": "lookup", "input": map[string]any{"q": "x"}}}},
					map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": "result"}}},
				},
			}, map[string]string{"anthropic-version": "2023-06-01"})
			if response.Code != http.StatusBadRequest || attempts.Load() != 1 {
				t.Fatalf("status=%d attempts=%d response=%s", response.Code, attempts.Load(), response.Body.String())
			}
			if got := gjson.Get(response.Body.String(), "error.message").String(); got != gjson.Get(upstreamError, "error.message").String() {
				t.Fatalf("upstream error lost: %s", response.Body.String())
			}
		})
	}
}

func TestProxy_NativeAnthropicToolRepairIsLocalized(t *testing.T) {
	t.Parallel()
	const requestBody = `{
		"model":"claude-sonnet-4-6","max_tokens":4096,
		"tools":[{"name":"lookup","input_schema":{"type":"object"}},{"name":"read","input_schema":{"type":"object"}},{"name":"unused","input_schema":{"type":"object"}}],
		"tool_choice":{"type":"tool","name":"lookup"},
		"messages":[
			{"role":"user","content":"call tools"},
			{"role":"assistant","content":[
				{"type":"tool_use","id":"Call_A","name":"lookup","input":{"q":"a"}},
				{"type":"tool_use","id":"Call_B","name":"read","input":{"file":"keep"}},
				{"type":"tool_use","id":"Call_C","name":"lookup","input":{"q":"c"}}
			]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"Call_A","is_error":true,"cache_control":{"type":"ephemeral","ttl":"1h"},"content":[{"type":"text","text":"a failed"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aW1hZ2U="}}]},
				{"type":"tool_result","tool_use_id":"Call_C","content":"c result"},
				{"type":"tool_result","tool_use_id":"Call_B","content":"keep result"},
				{"type":"text","text":"continue"}
			]}
		]}`
	for _, tc := range []struct {
		name, message, param                   string
		removeTool                             string
		pairOnly, retry                        bool
		brokenPair, duplicateID, repeatError   bool
		mixedTTL, emptyText, unsupportedResult bool
		scalarResult                           bool
	}{
		{name: "unused definition", message: "tools.2.custom.input_schema: Invalid schema", removeTool: "unused", retry: true},
		{name: "definition and its history", message: "tools.0.custom.input_schema: Invalid schema", removeTool: "lookup", retry: true},
		{name: "parameter location", param: "tools[0].input_schema.properties.thinking", message: "Invalid schema", removeTool: "lookup", retry: true},
		{name: "one call not all calls of same tool", message: "messages.1.content.0: tool_use blocks are not supported", pairOnly: true, retry: true},
		{name: "wire index after empty text cleanup", message: "messages.1.content.0: tool_use blocks are not supported", pairOnly: true, retry: true, emptyText: true},
		{name: "mixed TTL reorder", message: "messages.1.content.0: tool_use blocks are not supported", mixedTTL: true},
		{name: "unknown history location", param: "messages.99.content.0.input.thinking", message: "Invalid argument"},
		{name: "unparseable history location", param: "messages.1.content.0.input.thinking-mode", message: "Invalid argument"},
		{name: "unknown result content", message: "messages.1.content.0: tool_use blocks are not supported", unsupportedResult: true},
		// 标量 tool_result.content 无法在保留语义的前提下文本化，降级必须整体放弃。
		{name: "scalar result content", message: "messages.1.content.0: tool_use blocks are not supported", scalarResult: true},
		{name: "result location", message: "messages.2.content.0: tool_result is not supported", pairOnly: true, retry: true},
		{name: "result parameter", param: "messages[2].content[0]", message: "unsupported tool_result block", pairOnly: true, retry: true},
		// 第三方兼容网关的措辞不受 Anthropic 契约约束，判定必须是语义而非整句相等。
		{name: "gateway wording", message: "messages.1.content.0: This model does not support tool_use blocks", pairOnly: true, retry: true},
		{name: "gateway wording not allowed", message: "messages.1.content.0: tool_use block type is not allowed here", pairOnly: true, retry: true},
		{name: "bounded retry", message: "tools.0.custom.input_schema: Invalid schema", removeTool: "lookup", retry: true, repeatError: true},
		{name: "no location", message: "tool_use blocks are not supported"},
		{name: "unrelated prose", message: "The description mentions tools.0: tool_use blocks are not supported"},
		{name: "conflicting locations", param: "tools.1.name", message: "tools.0.name: Invalid thinking tool name"},
		{name: "unknown parameter", param: "tools.bad", message: "tools.0.name: Invalid thinking tool name"},
		{name: "out of range", message: "tools.90.name: Invalid name"},
		{name: "negative index", message: "tools[-1].name: Invalid name"},
		{name: "missing result is not incompatibility", message: "messages.1.content.0: tool_use ids were found without tool_result blocks immediately after: Call_A"},
		{name: "invalid arguments are not incompatibility", message: "messages.1.content.0.input.thinking: Invalid tool_use input"},
		{name: "non tool block", message: "messages.2.content.3: tool_result blocks are not supported"},
		{name: "incomplete pair", message: "messages.1.content.0: tool_use blocks are not supported", brokenPair: true},
		{name: "definition with incomplete pair", message: "tools.0.input_schema: Invalid schema", brokenPair: true},
		{name: "duplicate ID", message: "messages.1.content.0: tool_use blocks are not supported", duplicateID: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var request map[string]any
			if err := json.Unmarshal([]byte(requestBody), &request); err != nil {
				t.Fatal(err)
			}
			request["thinking"] = map[string]any{"type": "adaptive"}
			request["output_config"] = map[string]any{"effort": "high"}
			if strings.Contains(tc.message+tc.param, "thinking") {
				request["tool_choice"] = map[string]any{"type": "auto"}
			}
			messages := request["messages"].([]any)
			if tc.mixedTTL {
				result := messages[2].(map[string]any)["content"].([]any)[2].(map[string]any)
				result["cache_control"] = map[string]any{"type": "ephemeral", "ttl": "5m"}
			}
			if tc.emptyText {
				content := messages[1].(map[string]any)["content"].([]any)
				messages[1].(map[string]any)["content"] = append([]any{map[string]any{"type": "text", "text": ""}}, content...)
			}
			if tc.unsupportedResult {
				result := messages[2].(map[string]any)["content"].([]any)[0].(map[string]any)
				result["content"] = []any{map[string]any{"type": "unknown_content", "data": "preserve"}}
			}
			if tc.scalarResult {
				result := messages[2].(map[string]any)["content"].([]any)[0].(map[string]any)
				result["content"] = 123
			}
			if tc.brokenPair {
				content := messages[2].(map[string]any)["content"].([]any)
				messages[2].(map[string]any)["content"] = content[1:]
			}
			if tc.duplicateID {
				content := messages[1].(map[string]any)["content"].([]any)
				content[2].(map[string]any)["id"] = "Call_A"
			}
			errorBody, err := json.Marshal(map[string]any{"error": map[string]any{"type": "invalid_request_error", "message": tc.message, "param": tc.param}})
			if err != nil {
				t.Fatal(err)
			}
			env := setupProxyTestEnv(t, []testChannel{{customRequestRules: &model.CustomRequestRules{Passthrough: new(bool)}, name: "localized-repair", upstreamProtocol: "anthropic", models: "claude-sonnet-4-6", apiKey: "sk-ant"}}, map[int]string{0: "https://anthropic-gateway.example.com"})
			var bodies [][]byte
			env.server.client = &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				body, readErr := io.ReadAll(r.Body)
				if readErr != nil {
					t.Error(readErr)
				}
				bodies = append(bodies, body)
				status, response := http.StatusBadRequest, string(errorBody)
				if len(bodies) > 1 && !tc.repeatError {
					status = http.StatusOK
					response = `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"claude-sonnet-4-6","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
				}
				return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(response))}, nil
			})}
			response := doProxyRequest(t, env.engine, "/v1/messages", request, map[string]string{"anthropic-version": "2023-06-01"})
			wantAttempts, wantStatus := 1, http.StatusBadRequest
			if tc.retry {
				wantAttempts = 2
				if !tc.repeatError {
					wantStatus = http.StatusOK
				}
			}
			if len(bodies) != wantAttempts || response.Code != wantStatus {
				t.Fatalf("attempts=%d status=%d, want %d/%d: %s", len(bodies), response.Code, wantAttempts, wantStatus, response.Body.String())
			}
			if strings.Contains(tc.message+tc.param, "thinking") && !gjson.GetBytes(bodies[0], "thinking").Exists() {
				t.Fatal("fixture must retain thinking on the actual wire")
			}
			if !tc.retry {
				if gjson.Get(response.Body.String(), "error.message").String() != tc.message {
					t.Fatalf("original error lost: %s", response.Body.String())
				}
				return
			}
			before, after := gjson.ParseBytes(bodies[0]), gjson.ParseBytes(bodies[1])
			for _, path := range []string{"messages.0", "messages.1.content.1", "model", "max_tokens", "system", "thinking", "output_config"} {
				if !reflect.DeepEqual(before.Get(path).Value(), after.Get(path).Value()) {
					t.Errorf("unrelated %s changed", path)
				}
			}
			var toolNames []string
			for _, tool := range after.Get("tools").Array() {
				toolNames = append(toolNames, tool.Get("name").String())
			}
			wantTools := []string{"lookup", "read", "unused"}
			if tc.removeTool != "" {
				wantTools = slices.DeleteFunc(wantTools, func(name string) bool { return name == tc.removeTool })
			}
			if !reflect.DeepEqual(toolNames, wantTools) {
				t.Errorf("tools=%v, want %v", toolNames, wantTools)
			}
			if tc.removeTool != "" && tc.removeTool == before.Get("tool_choice.name").String() {
				if after.Get("tool_choice").Exists() {
					t.Error("removed tool is still forced by tool_choice")
				}
			} else if !reflect.DeepEqual(before.Get("tool_choice").Value(), after.Get("tool_choice").Value()) {
				t.Error("unrelated tool_choice changed")
			}
			if tc.removeTool == "unused" {
				if !reflect.DeepEqual(before.Get("messages").Value(), after.Get("messages").Value()) {
					t.Error("removing unused definition changed history")
				}
				return
			}
			converted := after.Get("messages.1.content.0")
			if converted.Get("type").String() != "text" ||
				!strings.Contains(converted.Get("text").String(), "lookup") ||
				!strings.Contains(converted.Get("text").String(), `"q":"a"`) {
				t.Errorf("selected call history lost: %s", converted.Raw)
			}
			if strings.Contains(converted.Get("text").String(), "cache_control") {
				t.Error("wire-only fields must not leak into converted history text")
			}
			if tc.pairOnly && !reflect.DeepEqual(before.Get("messages.1.content.2").Value(), after.Get("messages.1.content.2").Value()) {
				t.Error("another call of the same tool changed")
			}
			var resultIDs []string
			imageFound, errorFound, seenOther := false, false, false
			for _, part := range after.Get("messages.2.content").Array() {
				if part.Get("type").String() == "tool_result" {
					if seenOther {
						t.Error("remaining results must precede converted text")
					}
					resultIDs = append(resultIDs, part.Get("tool_use_id").String())
				} else {
					seenOther = true
				}
				if part.Get("type").String() == "image" {
					wantImage := before.Get("messages.2.content.0.content.1").Value().(map[string]any)
					wantImage["cache_control"] = before.Get("messages.2.content.0.cache_control").Value()
					imageFound = reflect.DeepEqual(part.Value(), wantImage)
				}
				if strings.Contains(part.Get("text").String(), "Call_A] (error)") {
					errorFound = true
				}
			}
			wantResults := []string{"Call_B"}
			if tc.pairOnly {
				wantResults = []string{"Call_C", "Call_B"}
			}
			if !reflect.DeepEqual(resultIDs, wantResults) || !imageFound || !errorFound {
				t.Errorf("results=%v want=%v image=%v error=%v: %s", resultIDs, wantResults, imageFound, errorFound, bodies[1])
			}
		})
	}
}

func TestProxy_NativeAnthropicDoesNotRepairUnrelatedSignature400(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int32
	env := setupProxyTestEnv(t, []testChannel{{
		name: "anthropic-signature-auth-error", upstreamProtocol: "anthropic", models: "claude-sonnet-4-6", apiKey: "sk-ant",
	}}, map[int]string{0: "https://anthropic-gateway.example.com"})
	env.server.client = &http.Client{Transport: roundTripperFunc(func(_ *http.Request) (*http.Response, error) {
		attempts.Add(1)
		return &http.Response{
			StatusCode: http.StatusBadRequest,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(
				`{"type":"error","error":{"type":"authentication_error","message":"invalid billing signature"}}`,
			)),
		}, nil
	})}

	response := doProxyRequest(t, env.engine, "/v1/messages", map[string]any{
		"model": "claude-sonnet-4-6", "max_tokens": 1024,
		"thinking": map[string]any{"type": "enabled", "budget_tokens": 512},
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}, map[string]string{"anthropic-version": "2023-06-01"})
	if response.Code != http.StatusBadRequest || attempts.Load() != 1 {
		t.Fatalf("status=%d attempts=%d body=%s", response.Code, attempts.Load(), response.Body.String())
	}
}

func TestProxy_NativeAnthropicToolRepairFailurePreservesNextChannelRequest(t *testing.T) {
	t.Parallel()

	var firstAttempts atomic.Int32
	var fallbackAttempts atomic.Int32
	firstRule := &model.CooldownDetectionRules{Rules: []model.CooldownDetectionRule{{
		Enabled: true, Name: "Anthropic request rejection", Priority: 0,
		StatusCodes: []int{http.StatusBadRequest}, Scope: model.CooldownScopeChannel,
		Mode: model.CooldownModeFixed, CooldownSeconds: 60,
	}}}
	env := setupProxyTestEnv(t, []testChannel{
		{customRequestRules: &model.CustomRequestRules{Passthrough: new(bool)},
			name: "anthropic-repair-fails", upstreamProtocol: "anthropic", models: "claude-sonnet-4-6",
			apiKey: "sk-first", priority: 100, cooldownDetectionRules: firstRule,
		},
		{customRequestRules: &model.CustomRequestRules{Passthrough: new(bool)},
			name: "anthropic-fallback", upstreamProtocol: "anthropic", models: "claude-sonnet-4-6",
			apiKey: "sk-second", priority: 90,
		},
	}, map[int]string{0: "https://first-anthropic.example.com", 1: "https://fallback-anthropic.example.com"})
	env.server.client = &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "first-anthropic.example.com" {
			attempt := firstAttempts.Add(1)
			body, readErr := io.ReadAll(r.Body)
			if readErr != nil {
				t.Error(readErr)
			}
			if attempt == 2 && (gjson.GetBytes(body, "tools").Exists() || gjson.GetBytes(body, "tool_choice").Exists() || gjson.GetBytes(body, "messages.1.content.0.type").String() != "text") {
				t.Errorf("rejected last tool was not localized: %s", body)
			}
			return &http.Response{
				StatusCode: http.StatusBadRequest,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(strings.NewReader(
					`{"type":"error","error":{"type":"invalid_request_error","message":"tools.0.input_schema: Invalid schema"}}`,
				)),
			}, nil
		}
		body, readErr := io.ReadAll(r.Body)
		if readErr != nil {
			t.Error(readErr)
		}
		if gjson.GetBytes(body, "tools.0.name").String() != "lookup" || gjson.GetBytes(body, "messages.1.content.0.type").String() != "tool_use" || gjson.GetBytes(body, "messages.2.content.0.tool_use_id").String() != "toolu_1" || gjson.GetBytes(body, "tool_choice.name").String() != "lookup" {
			t.Errorf("fallback lost tools: %s", body)
		}
		fallbackAttempts.Add(1)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(
				`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"fallback ok"}],"model":"claude-sonnet-4-6","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`,
			)),
		}, nil
	})}

	response := doProxyRequest(t, env.engine, "/v1/messages", map[string]any{
		"model": "claude-sonnet-4-6", "max_tokens": 1024,
		"messages": []any{
			map[string]any{"role": "user", "content": "hello"},
			map[string]any{"role": "assistant", "content": []any{map[string]any{
				"type": "tool_use", "id": "toolu_1", "name": "lookup", "input": map[string]any{},
			}}},
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": "keep result"}}},
		},
		"tool_choice": map[string]any{"type": "tool", "name": "lookup"},
		"tools":       []any{map[string]any{"name": "lookup", "input_schema": map[string]any{"type": "object"}}},
	}, map[string]string{"anthropic-version": "2023-06-01"})
	if response.Code != http.StatusOK || firstAttempts.Load() != 2 || fallbackAttempts.Load() != 1 {
		t.Fatalf("status=%d first=%d fallback=%d body=%s", response.Code, firstAttempts.Load(), fallbackAttempts.Load(), response.Body.String())
	}
}

func TestProxy_OAuthBaseURLSettingsOverrideChannelURLs(t *testing.T) {
	t.Parallel()
	t.Run("API key channel is unaffected", func(t *testing.T) {
		upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"chat-api-key","choices":[{"message":{"role":"assistant","content":"channel url"}}]}`)
		}))
		defer upstream.Close()

		env := setupProxyTestEnvWithSettings(t, []testChannel{{
			name: "api-key-unaffected", upstreamProtocol: "openai", models: "gpt-test",
		}}, map[int]string{0: upstream.URL}, map[string]string{
			"CODEX_BASE_URL": "http://127.0.0.1:1/ignored",
		})

		response := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
			"model": "gpt-test", "messages": []any{map[string]any{"role": "user", "content": "hello"}},
		}, nil)
		if response.Code != http.StatusOK || gjson.Get(response.Body.String(), "choices.0.message.content").String() != "channel url" {
			t.Fatalf("API key channel response=%d body=%s", response.Code, response.Body.String())
		}
	})

	t.Run("Codex", func(t *testing.T) {
		upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/codex/responses" {
				t.Errorf("Codex override path = %q, want /codex/responses", r.URL.Path)
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{"id":"resp-codex-override","status":"completed","output":[]}}`+"\n\n")
		}))
		defer upstream.Close()

		env := setupProxyTestEnvWithSettings(t, []testChannel{{
			name: "codex-oauth-override", upstreamProtocol: "codex", models: "gpt-test",
			authType:        model.AuthTypeCodexOAuth,
			oauthCredential: codexProxyTestCredential(t, "codex-override-token", "refresh", "account"),
		}}, map[int]string{0: "http://127.0.0.1:1/ignored#"}, map[string]string{
			"CODEX_BASE_URL": upstream.URL + "/codex/responses",
		})

		response := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
			"model": "gpt-test", "stream": false, "input": "hello",
		}, nil)
		if response.Code != http.StatusOK || gjson.Get(response.Body.String(), "id").String() != "resp-codex-override" {
			t.Fatalf("Codex override response=%d body=%s", response.Code, response.Body.String())
		}
	})

	t.Run("xAI", func(t *testing.T) {
		upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v1/responses" {
				t.Errorf("xAI override path = %q, want /v1/responses", r.URL.Path)
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{"id":"resp-xai-override","status":"completed","output":[]}}`+"\n\n")
		}))
		defer upstream.Close()

		credential := mustXAICredentialJSON(t, &xaiauth.Credential{
			Type: xaiauth.ChannelType, AuthKind: "oauth", AccessToken: "xai-override-token",
			RefreshToken: "refresh", Expired: time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		})
		env := setupProxyTestEnvWithSettings(t, []testChannel{{
			name: "xai-oauth-override", upstreamProtocol: "codex", models: "grok-4.5",
			authType: model.AuthTypeXAIOAuth, oauthCredential: credential,
		}}, map[int]string{0: "http://127.0.0.1:1/ignored"}, map[string]string{
			"XAI_BASE_URL": upstream.URL + "/v1",
		})

		response := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
			"model": "grok-4.5", "stream": false, "input": "hello",
		}, nil)
		if response.Code != http.StatusOK || gjson.Get(response.Body.String(), "id").String() != "resp-xai-override" {
			t.Fatalf("xAI override response=%d body=%s", response.Code, response.Body.String())
		}
	})

	t.Run("Anthropic", func(t *testing.T) {
		upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/anthropic/v1/messages" || r.URL.Query().Get("beta") != "true" {
				t.Errorf("Anthropic override URL = %s?%s", r.URL.Path, r.URL.RawQuery)
			}
			if got := r.Header.Get("Authorization"); got != "Bearer anthropic-override-token" {
				t.Errorf("Authorization = %q", got)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"msg-override","type":"message","role":"assistant","model":"claude-test","content":[{"type":"text","text":"override ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
		}))
		defer upstream.Close()

		env := setupProxyTestEnvWithSettings(t, []testChannel{{
			name: "anthropic-oauth-override", upstreamProtocol: "anthropic", models: "claude-test",
			authType:        model.AuthTypeAnthropicOAuth,
			oauthCredential: anthropicProxyTestCredential(t, "anthropic-override-token"),
		}}, map[int]string{0: "http://127.0.0.1:1/ignored"}, map[string]string{
			"ANTHROPIC_BASE_URL": upstream.URL + "/anthropic",
		})

		response := doProxyRequest(t, env.engine, "/v1/messages", map[string]any{
			"model": "claude-test", "max_tokens": 32,
			"messages": []any{map[string]any{"role": "user", "content": "hello"}},
		}, nil)
		if response.Code != http.StatusOK || gjson.Get(response.Body.String(), "content.0.text").String() != "override ok" {
			t.Fatalf("Anthropic override response=%d body=%s", response.Code, response.Body.String())
		}
	})

}

func TestProxy_AntigravityOAuthDoesNotCallDisabledOnlyURL(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"must not be called"}]}}]}}`)
	}))

	env := setupProxyTestEnv(t, []testChannel{{
		name: "antigravity-disabled-only-url", upstreamProtocol: "gemini", models: "gemini-3.6-flash-high", priority: 100,
		authType: model.AuthTypeAntigravityOAuth, oauthCredential: antigravityProxyTestCredential(t, "at-disabled-only-url"),
	}}, map[int]string{0: upstream.URL})
	configs, err := env.store.ListConfigs(context.Background())
	if err != nil || len(configs) != 1 {
		t.Fatalf("ListConfigs = (%d, %v)", len(configs), err)
	}
	env.server.urlSelector.DisableURL(configs[0].ID, upstream.URL)

	response := doProxyRequest(t, env.engine, "/v1beta/models/gemini-3.6-flash-high:generateContent", map[string]any{
		"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": "hello"}}}},
	}, nil)
	if response.Code == http.StatusOK {
		t.Fatalf("request unexpectedly succeeded: %s", response.Body.String())
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("disabled Antigravity URL calls=%d, want 0", got)
	}
}

func TestProxy_CodexOAuthChannelRefreshes401AndReassemblesNonStream(t *testing.T) {
	t.Parallel()
	var upstreamAttempts atomic.Int32
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempt := upstreamAttempts.Add(1)
		wireBody, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read Codex wire body: %v", err)
		}
		if !gjson.GetBytes(wireBody, "stream").Bool() {
			t.Errorf("Codex OAuth wire stream must be true: %s", wireBody)
		}
		if got := r.Header.Get("Accept"); got != "text/event-stream" {
			t.Errorf("Accept = %q, want text/event-stream", got)
		}
		// 官方 ChatGPT 登录的 HTTP /responses 请求体为 zstd；401 刷新后的重放同样压缩。
		if got := r.Header.Get("Content-Encoding"); got != "zstd" {
			t.Errorf("Content-Encoding = %q, want zstd", got)
		}
		if got := r.Header.Get("ChatGPT-Account-ID"); got != "account-proxy" {
			t.Errorf("ChatGPT-Account-ID = %q", got)
		}
		if attempt == 1 {
			if got := r.Header.Get("Authorization"); got != "Bearer at-old" {
				t.Errorf("first Authorization = %q", got)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"type":"authentication_error","message":"expired"}}`)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer at-new" {
			t.Errorf("refreshed Authorization = %q", got)
		}
		w.Header().Set("X-Codex-Plan-Type", "pro")
		w.Header().Set("X-Codex-Primary-Used-Percent", "6")
		w.Header().Set("X-Codex-Primary-Window-Minutes", "10080")
		w.Header().Set("X-Codex-Primary-Reset-At", "1786851417")
		w.Header().Set("X-Codex-Secondary-Used-Percent", "0")
		w.Header().Set("X-Codex-Secondary-Window-Minutes", "0")
		w.Header().Set("X-Codex-Bengalfox-Limit-Name", "GPT-5.3-Codex-Spark")
		w.Header().Set("X-Codex-Bengalfox-Primary-Used-Percent", "0")
		w.Header().Set("X-Codex-Bengalfox-Primary-Window-Minutes", "10080")
		w.Header().Set("X-Codex-Bengalfox-Primary-Reset-At", "1786876398000")
		w.Header().Set("X-Codex-Bengalfox-Secondary-Used-Percent", "0")
		w.Header().Set("X-Codex-Bengalfox-Secondary-Window-Minutes", "0")
		_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{"id":"resp-oauth","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":3,"output_tokens":1,"total_tokens":4}}}`+"\n\n")
	}))
	defer upstream.Close()

	var refreshes atomic.Int32
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshes.Add(1)
		_, _ = io.WriteString(w, `{"access_token":"at-new","refresh_token":"rt-new","expires_in":604800}`)
	}))
	defer tokenServer.Close()

	env := setupProxyTestEnv(t, []testChannel{{
		name: "codex-oauth-http", upstreamProtocol: "codex", models: "gpt-test", priority: 100,
		authType:        model.AuthTypeCodexOAuth,
		oauthCredential: codexProxyTestCredential(t, "at-old", "rt-old", "account-proxy"),
	}}, map[int]string{0: upstream.URL})
	service := codexauth.NewService(tokenServer.Client())
	service.TokenURL = tokenServer.URL
	env.server.codexCredentials.service = service
	env.server.codexCredentials.clientFor = func(*model.Config) *http.Client { return tokenServer.Client() }

	response := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
		"model": "gpt-test", "stream": false, "input": "hello",
	}, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if got := gjson.Get(response.Body.String(), "id").String(); got != "resp-oauth" {
		t.Fatalf("response id=%q body=%s", got, response.Body.String())
	}
	if upstreamAttempts.Load() != 2 || refreshes.Load() != 1 {
		t.Fatalf("upstream attempts=%d refreshes=%d", upstreamAttempts.Load(), refreshes.Load())
	}
	configs, err := env.store.ListConfigs(context.Background())
	if err != nil || len(configs) != 1 || !strings.Contains(configs[0].OAuthCredential, `"access_token":"at-new"`) {
		t.Fatalf("persisted refreshed channel=%#v err=%v", configs, err)
	}
	persistedCredential, err := codexauth.ParseCredential([]byte(configs[0].OAuthCredential))
	if err != nil || persistedCredential.PassiveUsage == nil ||
		len(persistedCredential.PassiveUsage.Windows) != 2 {
		t.Fatalf("persisted Codex quota = (%#v, %v)", persistedCredential, err)
	}
	windows := persistedCredential.PassiveUsage.Windows
	if windows[0].LimitName != "codex" || windows[0].UsedPercent != 6 ||
		windows[0].LimitWindowSeconds != 7*24*60*60 || windows[0].ResetAt != 1786851417 ||
		windows[1].LimitName != "GPT-5.3-Codex-Spark" || windows[1].UsedPercent != 0 ||
		windows[1].LimitWindowSeconds != 7*24*60*60 || windows[1].ResetAt != 1786876398 {
		t.Fatalf("persisted Codex quota windows = %#v", windows)
	}

	adminCtx, adminResponse := newTestContext(t, newRequest(http.MethodGet, "/admin/channels", nil))
	env.server.HandleChannels(adminCtx)
	list := mustParseAPIResponse[[]ChannelWithCooldown](t, adminResponse.Body.Bytes())
	if len(list.Data) != 1 || list.Data[0].OAuthUsage == nil || len(list.Data[0].OAuthUsage.Windows) != 2 ||
		list.Data[0].OAuthUsage.Windows[0].RemainingPercent != 94 ||
		list.Data[0].OAuthUsage.Windows[1].RemainingPercent != 100 {
		t.Fatalf("channel Codex quota = %+v", list.Data)
	}
}

func TestProxy_CodexFastModeBillsByRateCard(t *testing.T) {
	t.Parallel()
	const modelName = "gpt-6-astra"
	standard := util.CalculateStandardCostBreakdownWithPrice(modelName, "", util.OpenAIRateCardAPI, nil, 1000, 100, 0, 0, 0, 0, 0).Total
	for _, tc := range []struct {
		name       string
		authType   string
		credits    bool
		multiplier float64
	}{
		{"subscription allowance", model.AuthTypeCodexOAuth, false, 2.5},
		{"purchased credits", model.AuthTypeCodexOAuth, true, 2.0},
		{"api key", model.AuthTypeAPIKey, false, 2.0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				if tc.credits {
					_, _ = io.WriteString(w, `data: {"type":"codex.rate_limits","rate_limits":{"allowed":false,"limit_reached":true,"primary":{"used_percent":100,"window_minutes":300,"reset_at":1789995625}},"credits":{"has_credits":true,"unlimited":false,"balance":null}}`+"\n\n")
				}
				_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{"id":"resp-fast","status":"completed","service_tier":"priority","output":[],"usage":{"input_tokens":1000,"output_tokens":100}}}`+"\n\n")
			}))
			channel := testChannel{name: "codex-fast", upstreamProtocol: "codex", models: modelName, priority: 100, authType: tc.authType}
			if tc.authType == model.AuthTypeCodexOAuth {
				channel.oauthCredential = codexProxyTestCredential(t, "at-fast", "rt-fast", "account-fast")
			}
			env := setupProxyTestEnv(t, []testChannel{channel}, map[int]string{0: upstream.URL})

			response := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
				"model": modelName, "stream": false, "input": "hello", "service_tier": "priority",
			}, nil)
			if response.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			ctx := context.Background()
			var logs []*model.LogEntry
			for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(20 * time.Millisecond) {
				var err error
				logs, err = env.store.ListLogs(ctx, time.Now().Add(-time.Minute), 10, 0, nil)
				if err != nil {
					t.Fatal(err)
				}
				if len(logs) == 1 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("proxy log not persisted: %+v", logs)
				}
			}
			entry := logs[0]
			if entry.CodexHasCredits != tc.credits || math.Abs(entry.Cost-standard*tc.multiplier) > 1e-12 {
				t.Fatalf("credits=%t cost=%v, want credits=%t cost=%v", entry.CodexHasCredits, entry.Cost, tc.credits, standard*tc.multiplier)
			}
			breakdown := projectDashboardLogs(logs, env.server.logModelPrices(ctx, logs))[0].CostBreakdown
			if breakdown == nil || breakdown.ServiceTierMultiplier != tc.multiplier || math.Abs(breakdown.Total-entry.Cost) > 1e-12 {
				t.Fatalf("breakdown=%+v, want multiplier %v total %v", breakdown, tc.multiplier, entry.Cost)
			}
		})
	}
}

func TestProxy_CodexOAuthQuotaMergesIndependentResponseGroups(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		call := calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Codex-Primary-Used-Percent", strconv.Itoa(5+int(call)))
		w.Header().Set("X-Codex-Primary-Window-Minutes", "10080")
		w.Header().Set("X-Codex-Primary-Reset-At", "1786851417")
		if call == 1 {
			w.Header().Set("X-Codex-Bengalfox-Limit-Name", "GPT-5.3-Codex-Spark")
			w.Header().Set("X-Codex-Bengalfox-Primary-Used-Percent", "25")
			w.Header().Set("X-Codex-Bengalfox-Primary-Window-Minutes", "10080")
			w.Header().Set("X-Codex-Bengalfox-Primary-Reset-At", "1786876398")
		}
		_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{"id":"resp-quota","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`+"\n\n")
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{{
		name: "codex-quota-merge", upstreamProtocol: "codex", models: "gpt-test", priority: 100,
		authType:        model.AuthTypeCodexOAuth,
		oauthCredential: codexProxyTestCredential(t, "at-quota", "rt-quota", "account-quota"),
	}}, map[int]string{0: upstream.URL})
	for _, input := range []string{"first", "second"} {
		response := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
			"model": "gpt-test", "stream": false, "input": input,
		}, nil)
		if response.Code != http.StatusOK {
			t.Fatalf("%s response status=%d body=%s", input, response.Code, response.Body.String())
		}
	}
	configs, err := env.store.ListConfigs(context.Background())
	if err != nil || len(configs) != 1 {
		t.Fatalf("list merged quota channel = (%#v, %v)", configs, err)
	}
	persisted, err := codexauth.ParseCredential([]byte(configs[0].OAuthCredential))
	if err != nil || persisted.PassiveUsage == nil || len(persisted.PassiveUsage.Windows) != 2 {
		t.Fatalf("merged Codex quota = (%#v, %v)", persisted, err)
	}
	if persisted.PassiveUsage.Windows[0].UsedPercent != 7 ||
		persisted.PassiveUsage.Windows[1].LimitName != "GPT-5.3-Codex-Spark" ||
		persisted.PassiveUsage.Windows[1].UsedPercent != 25 {
		t.Fatalf("independent quota groups were overwritten: %#v", persisted.PassiveUsage.Windows)
	}
}

func TestProxy_XAIOAuthZeroKeyFinalizesWireAndReassemblesNonStream(t *testing.T) {
	t.Parallel()

	var gotConversationID string
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Errorf("upstream path = %q, want /v1/responses", r.URL.Path)
		}
		wireBody, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read xAI wire body: %v", err)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer xai-access" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get(xaiauth.CLITokenAuthHeader); got != xaiauth.CLITokenAuthValue {
			t.Errorf("%s = %q", xaiauth.CLITokenAuthHeader, got)
		}
		for name, want := range map[string]string{
			xaiauth.CLIClientVersionHeader: xaiauth.CLIClientVersion,
			"User-Agent":                   xaiauth.CLIUserAgent,
			xaiauth.CLIClientModeHeader:    xaiauth.CLIClientMode,
		} {
			if got := r.Header.Get(name); got != want {
				t.Errorf("%s = %q, want %q", name, got, want)
			}
		}
		if got := r.Header.Get("Accept"); got != "application/json, text/event-stream" {
			t.Errorf("Accept = %q", got)
		}
		gotConversationID = r.Header.Get("x-grok-conv-id")
		if gotConversationID == "" || gjson.GetBytes(wireBody, "prompt_cache_key").String() != gotConversationID {
			t.Errorf("conversation identity mismatch header=%q body=%s", gotConversationID, wireBody)
		}
		wireModel := gjson.GetBytes(wireBody, "model").String()
		if !gjson.GetBytes(wireBody, "stream").Bool() || wireModel != "grok-4.5" && wireModel != "grok-4.6" {
			t.Errorf("xAI required body fields missing: %s", wireBody)
		}
		tools := gjson.GetBytes(wireBody, "tools").Array()
		choice := gjson.GetBytes(wireBody, "tool_choice")
		switch {
		case !choice.Exists():
			if len(tools) != 0 {
				t.Errorf("xAI CLI tools = %s, want no implicit tools", gjson.GetBytes(wireBody, "tools").Raw)
			}
		case choice.String() == "auto":
			if len(tools) != 1 || tools[0].Get("type").String() != "web_search" ||
				tools[0].Get("search_context_size").String() != "low" {
				t.Errorf("explicit xAI search tool was not preserved: %s", gjson.GetBytes(wireBody, "tools").Raw)
			}
		case choice.Get("type").String() == "allowed_tools":
			if wireModel != "grok-4.6" || choice.Get("mode").String() != "required" ||
				choice.Get("tools.0.type").String() != "image_generation" || len(tools) != 1 ||
				tools[0].Get("type").String() != "image_generation" || tools[0].Get("action").String() != "generate" {
				t.Errorf("xAI image_generation wire contract mismatch: %s", wireBody)
			}
		default:
			t.Errorf("unexpected xAI tool_choice: %s", choice.Raw)
		}
		for _, field := range []string{"previous_response_id", "prompt_cache_retention", "safety_identifier", "stream_options"} {
			if gjson.GetBytes(wireBody, field).Exists() {
				t.Errorf("forbidden field %s survived: %s", field, wireBody)
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.completed\n")
		_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{"id":"resp-xai","object":"response","status":"completed","model":"grok-4.5","output":[],"usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3}}}`+"\n\n")
	}))
	defer upstream.Close()

	credential := mustXAICredentialJSON(t, &xaiauth.Credential{
		Type: xaiauth.ChannelType, AuthKind: "oauth", AccessToken: "xai-access", RefreshToken: "xai-refresh",
		Expired: time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	})
	rules := &model.CustomRequestRules{
		Headers: []model.CustomHeaderRule{
			{Action: model.RuleActionOverride, Name: xaiauth.CLIClientVersionHeader, Value: "client-override"},
			{Action: model.RuleActionOverride, Name: "x-grok-conv-id", Value: "client-conversation"},
			{Action: model.RuleActionOverride, Name: "User-Agent", Value: "client-agent"},
			{Action: model.RuleActionOverride, Name: "Accept", Value: "application/json"},
		},
		Body: []model.CustomBodyRule{
			{Action: model.RuleActionOverride, Path: "stream", Value: json.RawMessage(`false`)},
			{Action: model.RuleActionOverride, Path: "prompt_cache_key", Value: json.RawMessage(`"client-conversation"`)},
			{Action: model.RuleActionOverride, Path: "previous_response_id", Value: json.RawMessage(`"client-previous"`)},
		},
	}
	env := setupProxyTestEnv(t, []testChannel{{
		name: "xai-oauth-http", upstreamProtocol: "codex", models: "grok-4.5,grok-4.6", priority: 100,
		authType: model.AuthTypeXAIOAuth, oauthCredential: credential, customRequestRules: rules,
	}}, map[int]string{0: xaiauth.CLIBaseURL})
	env.server.client = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		clone := req.Clone(req.Context())
		clone.URL.Scheme = "http"
		clone.URL.Host = upstream.host
		return dispatchTestHTTPRequest(clone)
	})}
	env.server.xaiCredentials = newXAICredentialManager(env.store, env.server.getClientForChannel, nil)

	response := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
		"model": "grok-4.5", "stream": false, "input": "hello",
		"prompt_cache_retention": "24h",
		"safety_identifier":      "client", "stream_options": map[string]any{"include_usage": true},
	}, map[string]string{"Session-Id": "session", "Thread-Id": "parent"})
	if response.Code != http.StatusOK || gjson.Get(response.Body.String(), "id").String() != "resp-xai" {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	explicitSearchResponse := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
		"model": "grok-4.5", "stream": false, "input": "current price",
		"tools":       []any{map[string]any{"type": "web_search", "search_context_size": "low"}},
		"tool_choice": "auto", "parallel_tool_calls": true,
	}, map[string]string{"Session-Id": "session", "Thread-Id": "parent"})
	if explicitSearchResponse.Code != http.StatusOK || gjson.Get(explicitSearchResponse.Body.String(), "id").String() != "resp-xai" {
		t.Fatalf("explicit search status=%d body=%s", explicitSearchResponse.Code, explicitSearchResponse.Body.String())
	}
	imageResponse := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
		"model": "grok-4.6", "stream": false, "input": "draw a red circle",
		"tools":       []any{map[string]any{"type": "image_generation", "action": "generate"}},
		"tool_choice": map[string]any{"type": "image_generation"},
	}, map[string]string{"Session-Id": "session", "Thread-Id": "parent"})
	if imageResponse.Code != http.StatusOK || gjson.Get(imageResponse.Body.String(), "id").String() != "resp-xai" {
		t.Fatalf("image generation status=%d body=%s", imageResponse.Code, imageResponse.Body.String())
	}
	if gotConversationID == "" {
		t.Fatal("xAI conversation identity was not sent")
	}
	configs, err := env.store.ListConfigs(context.Background())
	if err != nil || len(configs) != 1 {
		t.Fatalf("ListConfigs = %#v, %v", configs, err)
	}
	keys, err := env.store.GetAPIKeys(context.Background(), configs[0].ID)
	if err != nil || len(keys) != 0 {
		t.Fatalf("xAI OAuth channel keys = %#v, %v", keys, err)
	}
}

func TestProxy_XAIResponsesClientToolsRoundTrip(t *testing.T) {
	t.Parallel()
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		tools := gjson.GetBytes(body, "tools").Array()
		if len(tools) != 2 || tools[0].Get("type").String() != "function" || tools[1].Get("type").String() != "function" {
			t.Errorf("upstream tools were not adapted: %s", body)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		choice := gjson.GetBytes(body, "tool_choice")
		if choice.Get("type").String() != "function" || choice.Get("name").String() != tools[0].Get("name").String() || choice.Get("namespace").Exists() {
			t.Errorf("forced namespace tool lost: %s", body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-tools\",\"model\":\"grok-4.6\",\"output\":[]}}\n\n")
		items := []string{
			fmt.Sprintf(`{"type":"function_call","id":"fc_ns","call_id":"call_ns","name":%q,"arguments":"{\"message\":\"hi\"}"}`, tools[0].Get("name").String()),
			fmt.Sprintf(`{"type":"function_call","id":"fc_custom","call_id":"call_custom","name":%q,"arguments":"{\"input\":\"patch text\"}"}`, tools[1].Get("name").String()),
		}
		for i, item := range items {
			_, _ = fmt.Fprintf(w, "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":%d,\"item\":%s}\n\n", i, item)
		}
		_, _ = fmt.Fprintf(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-tools\",\"status\":\"completed\",\"model\":\"grok-4.6\",\"output\":[%s],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n", strings.Join(items, ","))
	}))
	defer upstream.Close()
	credential := mustXAICredentialJSON(t, &xaiauth.Credential{
		Type: xaiauth.ChannelType, AuthKind: "oauth", AccessToken: "xai-tools", RefreshToken: "refresh",
		Expired: time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	})
	env := setupProxyTestEnv(t, []testChannel{{
		name: "xai-tools", upstreamProtocol: "codex", models: "grok-4.6", authType: model.AuthTypeXAIOAuth, oauthCredential: credential,
	}}, map[int]string{0: upstream.URL + "/v1"})
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", streaming), func(t *testing.T) {
			response := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
				"model": "grok-4.6", "stream": streaming, "input": "call the tools",
				"tools": []any{
					map[string]any{"type": "namespace", "name": "collaboration", "tools": []any{map[string]any{"type": "function", "name": "send_message", "parameters": map[string]any{"type": "object", "properties": map[string]any{"message": map[string]any{"type": "string"}}}}}},
					map[string]any{"type": "custom", "name": "apply_patch"},
				},
				"tool_choice": map[string]any{"type": "function", "namespace": "collaboration", "name": "send_message"},
			}, nil)
			if response.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			result := gjson.ParseBytes(response.Body.Bytes())
			if streaming {
				for _, event := range parseSSEJSONPayloads(response.Body.String()) {
					if event["type"] == "response.completed" {
						payload, err := json.Marshal(event["response"])
						if err != nil {
							t.Fatal(err)
						}
						result = gjson.ParseBytes(payload)
					}
				}
			}
			items := result.Get("output").Array()
			if len(items) != 2 || items[0].Get("namespace").String() != "collaboration" || items[0].Get("name").String() != "send_message" || items[0].Get("call_id").String() != "call_ns" {
				t.Fatalf("namespace call was not restored: %s", response.Body.String())
			}
			if items[1].Get("type").String() != "custom_tool_call" || items[1].Get("name").String() != "apply_patch" || items[1].Get("input").String() != "patch text" || items[1].Get("call_id").String() != "call_custom" {
				t.Fatalf("custom call was not restored: %s", response.Body.String())
			}
		})
	}
}

func TestProxy_XAIOAuthRefreshReplayBoundary(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		status        int
		body          string
		wantRefresh   bool
		lateRejection bool
	}{
		{name: "unauthorized", status: http.StatusUnauthorized, body: `{"error":{"type":"authentication_error","code":"invalid_token"}}`, wantRefresh: true},
		{name: "late unauthorized after another refresh", status: http.StatusUnauthorized, body: `{"error":{"type":"authentication_error","code":"invalid_token"}}`, wantRefresh: true, lateRejection: true},
		{name: "structured bad credential", status: http.StatusForbidden, body: `{"code":"invalid_token"}`, wantRefresh: true},
		{name: "ordinary forbidden", status: http.StatusForbidden, body: `{"error":{"message":"forbidden"}}`},
		{name: "entitlement forbidden", status: http.StatusForbidden, body: `{"code":"subscription_required"}`},
		{name: "quota", status: http.StatusTooManyRequests, body: `{"code":"quota_exceeded"}`},
		{name: "upstream failure", status: http.StatusBadGateway, body: `{"error":{"message":"bad gateway"}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var upstreamAttempts atomic.Int32
			var firstExecutionID string
			var refreshBeforeRejection func(context.Context) error
			upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempt := upstreamAttempts.Add(1)
				wantToken := "xai-old"
				if attempt == 2 {
					wantToken = "xai-new"
				}
				if got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "); got != wantToken {
					t.Errorf("attempt %d access token = %q, want %q", attempt, got, wantToken)
				}
				wireBody, err := io.ReadAll(r.Body)
				if err != nil {
					t.Errorf("read attempt %d body: %v", attempt, err)
				}
				executionID := r.Header.Get("x-grok-conv-id")
				if executionID == "" || gjson.GetBytes(wireBody, "prompt_cache_key").String() != executionID {
					t.Errorf("attempt %d execution identity header=%q body=%s", attempt, executionID, wireBody)
				}
				if attempt == 1 {
					firstExecutionID = executionID
				} else if executionID != firstExecutionID {
					t.Errorf("retry execution identity changed: first=%q retry=%q", firstExecutionID, executionID)
				}
				if tt.wantRefresh && attempt == 2 {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{"id":"resp-refreshed","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`+"\n\n")
					return
				}
				w.Header().Set("Content-Type", "application/json")
				if tt.lateRejection && attempt == 1 {
					if err := refreshBeforeRejection(r.Context()); err != nil {
						t.Errorf("concurrent refresh: %v", err)
					}
				}
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.body)
			}))
			defer upstream.Close()

			credential := mustXAICredentialJSON(t, &xaiauth.Credential{
				Type: xaiauth.ChannelType, AuthKind: "oauth", AccessToken: "xai-old", RefreshToken: "xai-refresh",
				Expired: time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			})
			env := setupProxyTestEnv(t, []testChannel{{
				name: "xai-replay", upstreamProtocol: "codex", models: "grok-4.5", priority: 100,
				authType: model.AuthTypeXAIOAuth, oauthCredential: credential,
			}}, map[int]string{0: upstream.URL + "/v1"})

			var refreshes atomic.Int32
			refreshClient := &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				refreshes.Add(1)
				if req.URL.String() != xaiauth.TokenURL {
					t.Fatalf("refresh URL = %s", req.URL)
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": {"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"access_token":"xai-new","refresh_token":"xai-refresh-new","expires_in":3600}`)),
					Request:    req,
				}, nil
			})}
			env.server.xaiCredentials = newXAICredentialManager(
				env.store, func(*model.Config) *http.Client { return refreshClient }, func(int64) {
					env.server.InvalidateChannelListCache()
				},
			)
			configs, err := env.store.ListConfigs(context.Background())
			if err != nil || len(configs) != 1 {
				t.Fatalf("configs=%d err=%v", len(configs), err)
			}
			refreshBeforeRejection = func(ctx context.Context) error {
				_, err := env.server.xaiCredentials.credentialAfterUnauthorized(ctx, configs[0], "xai-old")
				return err
			}

			response := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
				"model": "grok-4.5", "stream": false, "input": "hello",
			}, nil)
			wantAttempts := int32(1)
			wantRefreshes := int32(0)
			if tt.wantRefresh {
				wantAttempts = 2
				wantRefreshes = 1
				if response.Code != http.StatusOK || gjson.Get(response.Body.String(), "id").String() != "resp-refreshed" {
					t.Fatalf("response=%d body=%s", response.Code, response.Body.String())
				}
			}
			if got := upstreamAttempts.Load(); got != wantAttempts {
				t.Fatalf("upstream attempts=%d, want %d", got, wantAttempts)
			}
			if got := refreshes.Load(); got != wantRefreshes {
				t.Fatalf("refreshes=%d, want %d", got, wantRefreshes)
			}
		})
	}
}

func TestProxy_OAuthRefreshFailureChecksExistingAccessToken(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name             string
		authType         string
		upstreamProtocol string
		model            string
		path             string
		request          map[string]any
		successType      string
		successBody      string
		rejectedBody     string
		xaiBaseURL       bool
	}{
		{
			name: "codex", authType: model.AuthTypeCodexOAuth, upstreamProtocol: "codex", model: "gpt-test",
			path: "/v1/responses", request: map[string]any{"model": "gpt-test", "stream": false, "input": "hello"},
			successType:  "text/event-stream",
			successBody:  `data: {"type":"response.completed","response":{"id":"resp-codex","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}` + "\n\n",
			rejectedBody: `{"error":{"type":"authentication_error","message":"expired"}}`,
		},
		{
			name: "xai", authType: model.AuthTypeXAIOAuth, upstreamProtocol: "codex", model: "grok-4.5",
			path: "/v1/responses", request: map[string]any{"model": "grok-4.5", "stream": false, "input": "hello"},
			successType: "text/event-stream", xaiBaseURL: true,
			successBody:  `data: {"type":"response.completed","response":{"id":"resp-xai","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}` + "\n\n",
			rejectedBody: `{"error":{"type":"authentication_error","code":"invalid_token"}}`,
		},
		{
			name: "anthropic", authType: model.AuthTypeAnthropicOAuth, upstreamProtocol: "anthropic", model: "claude-test",
			path: "/v1/messages", request: map[string]any{
				"model": "claude-test", "max_tokens": 16,
				"messages": []any{map[string]any{"role": "user", "content": "hello"}},
			},
			successType:  "application/json",
			successBody:  `{"id":"msg-ok","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"claude-test","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`,
			rejectedBody: `{"type":"error","error":{"type":"authentication_error","message":"expired"}}`,
		},
	}

	for _, tt := range tests {
		for _, rejectAccessToken := range []bool{false, true} {
			outcome := "existing access token succeeds"
			if rejectAccessToken {
				outcome = "existing access token rejected"
			}
			t.Run(tt.name+"/"+outcome, func(t *testing.T) {
				const accessToken = "stale-access-token"
				var upstreamAttempts atomic.Int32
				upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					upstreamAttempts.Add(1)
					if got := r.Header.Get("Authorization"); got != "Bearer "+accessToken {
						t.Errorf("Authorization = %q", got)
					}
					w.Header().Set("Content-Type", tt.successType)
					if rejectAccessToken {
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusUnauthorized)
						_, _ = io.WriteString(w, tt.rejectedBody)
						return
					}
					_, _ = io.WriteString(w, tt.successBody)
				}))
				defer upstream.Close()

				upstreamURL := upstream.URL
				if tt.xaiBaseURL {
					upstreamURL += "/v1"
				}
				env := setupProxyTestEnv(t, []testChannel{{
					name: tt.name + "-refresh-failure", upstreamProtocol: tt.upstreamProtocol,
					models: tt.model, priority: 100, authType: tt.authType,
					oauthCredential: expiredProxyOAuthCredential(t, tt.authType, accessToken),
				}}, map[int]string{0: upstreamURL})

				var refreshAttempts atomic.Int32
				refreshClient := &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
					refreshAttempts.Add(1)
					return &http.Response{
						StatusCode: http.StatusBadRequest,
						Header:     http.Header{"Content-Type": {"application/json"}},
						Body:       io.NopCloser(strings.NewReader(`{"error":"invalid_grant"}`)),
						Request:    req,
					}, nil
				})}
				switch tt.authType {
				case model.AuthTypeCodexOAuth:
					service := codexauth.NewService(refreshClient)
					service.TokenURL = "https://oauth.test/token"
					env.server.codexCredentials.service = service
					env.server.codexCredentials.clientFor = func(*model.Config) *http.Client { return refreshClient }
				case model.AuthTypeXAIOAuth:
					env.server.xaiCredentials = newXAICredentialManager(
						env.store, func(*model.Config) *http.Client { return refreshClient }, nil,
					)
				case model.AuthTypeAnthropicOAuth:
					service := anthropicauth.NewService(refreshClient)
					service.TokenURL = "https://oauth.test/token"
					env.server.anthropicCredentials.service = service
					env.server.anthropicCredentials.clientFor = func(*model.Config) *http.Client { return refreshClient }
				case model.AuthTypeAntigravityOAuth:
					service := antigravityauth.NewService(refreshClient)
					service.TokenURL = "https://oauth.test/token"
					env.server.antigravityCredentials.service = service
					env.server.antigravityCredentials.clientFor = func(*model.Config) *http.Client { return refreshClient }
				}

				startedAt := time.Now()
				response := doProxyRequest(t, env.engine, tt.path, tt.request, nil)
				if got := refreshAttempts.Load(); got != 1 {
					t.Fatalf("refresh attempts=%d, want 1", got)
				}
				if got := upstreamAttempts.Load(); got != 1 {
					t.Fatalf("upstream attempts=%d, want 1", got)
				}

				configs, err := env.store.ListConfigs(context.Background())
				if err != nil || len(configs) != 1 {
					t.Fatalf("ListConfigs = %#v, %v", configs, err)
				}
				cooldowns, err := env.store.GetAllChannelCooldowns(context.Background())
				if err != nil {
					t.Fatalf("GetAllChannelCooldowns: %v", err)
				}
				until, cooling := cooldowns[configs[0].ID]
				if !rejectAccessToken {
					if response.Code != http.StatusOK {
						t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
					}
					if !configs[0].Enabled {
						t.Fatal("usable existing access token disabled channel")
					}
					if cooling {
						t.Fatalf("usable existing access token caused cooldown until %s", until)
					}
					return
				}
				if response.Code != http.StatusUnauthorized {
					t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
				}
				if tt.authType == model.AuthTypeCodexOAuth || tt.authType == model.AuthTypeAntigravityOAuth ||
					tt.authType == model.AuthTypeAnthropicOAuth {
					if configs[0].Enabled {
						t.Fatal("terminally rejected credential left channel enabled")
					}
					if cooling {
						t.Fatalf("disabled channel retained cooldown until %s", until)
					}
					_ = doProxyRequest(t, env.engine, tt.path, tt.request, nil)
					if got := refreshAttempts.Load(); got != 1 {
						t.Fatalf("disabled channel refresh attempts=%d, want 1", got)
					}
					if got := upstreamAttempts.Load(); got != 1 {
						t.Fatalf("disabled channel upstream attempts=%d, want 1", got)
					}
					return
				}
				if !configs[0].Enabled {
					t.Fatal("non-Codex refresh failure disabled channel")
				}
				if !cooling {
					t.Fatal("rejected existing access token did not cool channel")
				}
				if until.Before(startedAt.Add(24*time.Hour-time.Minute)) || until.After(time.Now().Add(24*time.Hour+time.Minute)) {
					t.Fatalf("cooldown until=%s, want about 24 hours", until)
				}
			})
		}
	}
}

func TestProxy_OperatorAbort_DuringOAuthRefreshDoesNotCoolChannel(t *testing.T) {
	t.Parallel()
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"type":"authentication_error","message":"expired"}}`)
	}))
	defer upstream.Close()
	backup := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp-backup","object":"response","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}]}`)
	}))
	defer backup.Close()
	env := setupProxyTestEnv(t, []testChannel{
		{name: "abort-refresh", upstreamProtocol: "codex", models: "gpt-test", priority: 100, authType: model.AuthTypeCodexOAuth, oauthCredential: codexProxyTestCredential(t, "at-old", "rt-old", "account")},
		{name: "backup", upstreamProtocol: "codex", models: "gpt-test", priority: 50},
	}, map[int]string{0: upstream.URL, 1: backup.URL})
	refreshStarted, releaseRefresh := make(chan struct{}), make(chan struct{})
	defer close(releaseRefresh)
	refreshClient := &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		close(refreshStarted)
		<-releaseRefresh
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"access_token":"at-new","refresh_token":"rt-new","expires_in":604800}`)), Request: req}, nil
	})}
	service := codexauth.NewService(refreshClient)
	service.TokenURL = "https://oauth.test/token"
	env.server.codexCredentials.service = service
	env.server.codexCredentials.clientFor = func(*model.Config) *http.Client { return refreshClient }
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-test","stream":false,"input":"hello"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-api-key")
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { defer close(done); env.engine.ServeHTTP(response, req) }()
	select {
	case <-refreshStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("refresh did not start")
	}
	active := env.server.activeRequests.List()
	if len(active) != 1 || !env.server.activeRequests.Abort(active[0].ID) {
		t.Fatal("no abortable request during refresh")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("abort did not stop waiting for refresh")
	}
	if response.Code != http.StatusOK || gjson.Get(response.Body.String(), "id").String() != "resp-backup" {
		t.Fatalf("backup response=%d %s", response.Code, response.Body.String())
	}
	cfg, err := env.store.GetConfig(context.Background(), active[0].ChannelID)
	if err != nil || !cfg.Enabled {
		t.Fatalf("channel disabled: cfg=%+v err=%v", cfg, err)
	}
	cooldowns, err := env.store.GetAllChannelCooldowns(context.Background())
	if err != nil || !cooldowns[cfg.ID].IsZero() {
		t.Fatalf("abort cooled credentials: cooldowns=%+v err=%v", cooldowns, err)
	}
	// 刷新期间的中断绕过 forwardAttempt，必须仍留下 502 记录，否则日志只剩
	// "401 + 换渠道成功"，运维无法解释换渠原因。
	abortLog := waitForProxyLogMatching(t, env, func(e *model.LogEntry) bool {
		return e.ChannelID == cfg.ID && e.StatusCode == http.StatusBadGateway &&
			strings.Contains(e.Message, errOperatorAbort.Error())
	})
	if abortLog == nil {
		t.Fatal("operator abort during refresh left no proxy log")
	}
}

func TestProxy_CodexTransientRefreshFailureCoolsInsteadOfDisables(t *testing.T) {
	t.Parallel()
	const accessToken = "stale-access-token"
	var upstreamAttempts atomic.Int32
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamAttempts.Add(1)
		if got := r.Header.Get("Authorization"); got != "Bearer "+accessToken {
			t.Errorf("Authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"type":"authentication_error","message":"expired"}}`)
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{{
		name: "codex-transient-refresh", upstreamProtocol: "codex", models: "gpt-test", priority: 100,
		authType:        model.AuthTypeCodexOAuth,
		oauthCredential: expiredProxyOAuthCredential(t, model.AuthTypeCodexOAuth, accessToken),
	}}, map[int]string{0: upstream.URL})
	var refreshAttempts atomic.Int32
	refreshClient := &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		refreshAttempts.Add(1)
		return &http.Response{
			StatusCode: http.StatusInternalServerError,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"error":"temporarily_unavailable"}`)),
			Request:    req,
		}, nil
	})}
	service := codexauth.NewService(refreshClient)
	service.TokenURL = "https://oauth.test/token"
	env.server.codexCredentials.service = service
	env.server.codexCredentials.clientFor = func(*model.Config) *http.Client { return refreshClient }

	response := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
		"model": "gpt-test", "stream": false, "input": "hello",
	}, nil)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if got := refreshAttempts.Load(); got != 1 {
		t.Fatalf("refresh attempts=%d, want 1", got)
	}
	if got := upstreamAttempts.Load(); got != 1 {
		t.Fatalf("upstream attempts=%d, want 1", got)
	}
	configs, err := env.store.ListConfigs(context.Background())
	if err != nil || len(configs) != 1 || !configs[0].Enabled {
		t.Fatalf("transient refresh failure disabled channel: configs=%+v err=%v", configs, err)
	}
	cooldowns, err := env.store.GetAllChannelCooldowns(context.Background())
	if err != nil || cooldowns[configs[0].ID].IsZero() {
		t.Fatalf("transient refresh failure did not cool channel: cooldowns=%+v err=%v", cooldowns, err)
	}
}

func TestProxy_RejectedCodexPersonalAccessTokenDisablesChannel(t *testing.T) {
	t.Parallel()
	const accessToken = "at-static-rejected"
	var upstreamAttempts atomic.Int32
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamAttempts.Add(1)
		if got := r.Header.Get("Authorization"); got != "Bearer "+accessToken {
			t.Errorf("Authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"type":"authentication_error","message":"expired"}}`)
	}))
	defer upstream.Close()

	credential, err := (&codexauth.Credential{
		Type: codexauth.ChannelType, AuthMode: codexauth.AuthModePersonalAccessToken,
		AccessToken: accessToken, ChatGPTUserID: "pat-user", AccountID: "pat-account",
	}).JSON()
	if err != nil {
		t.Fatal(err)
	}
	env := setupProxyTestEnv(t, []testChannel{{
		name: "codex-pat-rejected", upstreamProtocol: "codex", models: "gpt-test", priority: 100,
		authType: model.AuthTypeCodexOAuth, oauthCredential: credential,
	}}, map[int]string{0: upstream.URL})
	var tokenEndpointCalls atomic.Int32
	refreshClient := &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		tokenEndpointCalls.Add(1)
		return nil, fmt.Errorf("unexpected token endpoint request: %s", req.URL)
	})}
	env.server.codexCredentials.service = codexauth.NewService(refreshClient)
	env.server.codexCredentials.clientFor = func(*model.Config) *http.Client { return refreshClient }

	response := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
		"model": "gpt-test", "stream": false, "input": "hello",
	}, nil)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if got := upstreamAttempts.Load(); got != 1 {
		t.Fatalf("upstream attempts=%d, want 1", got)
	}
	if got := tokenEndpointCalls.Load(); got != 0 {
		t.Fatalf("PAT token endpoint calls=%d, want 0", got)
	}
	configs, err := env.store.ListConfigs(context.Background())
	if err != nil || len(configs) != 1 || configs[0].Enabled {
		t.Fatalf("rejected PAT channel was not disabled: configs=%+v err=%v", configs, err)
	}
}

func TestProxy_XAIOAuthDoesNotReplayAfterCommittedSemanticOutput(t *testing.T) {
	var upstreamAttempts atomic.Int32
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamAttempts.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		largeDelta := strings.Repeat("x", SSEBufferSize)
		_, _ = fmt.Fprintf(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":%q}\n\n", largeDelta)
		_, _ = io.WriteString(w, "event: error\n"+`data: {"type":"error","error":{"type":"authentication_error","code":"invalid_token","message":"expired"}}`+"\n\n")
	}))
	defer upstream.Close()

	credential := mustXAICredentialJSON(t, &xaiauth.Credential{
		Type: xaiauth.ChannelType, AuthKind: "oauth", AccessToken: "committed-access", RefreshToken: "committed-refresh",
		Expired: time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	})
	env := setupProxyTestEnv(t, []testChannel{{
		name: "xai-committed", upstreamProtocol: "codex", models: "grok-4.5", priority: 100,
		authType: model.AuthTypeXAIOAuth, oauthCredential: credential,
	}}, map[int]string{0: upstream.URL + "/v1"})
	var refreshes atomic.Int32
	env.server.xaiCredentials = newXAICredentialManager(env.store, func(*model.Config) *http.Client {
		refreshes.Add(1)
		return http.DefaultClient
	}, nil)

	response := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
		"model": "grok-4.5", "stream": true, "input": "hello",
	}, nil)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"type":"response.output_text.delta"`) {
		t.Fatalf("response=%d body=%s", response.Code, response.Body.String())
	}
	if upstreamAttempts.Load() != 1 || refreshes.Load() != 0 {
		t.Fatalf("upstream attempts=%d refreshes=%d, want 1/0", upstreamAttempts.Load(), refreshes.Load())
	}
}

func TestProxy_CodexPreservesOfficialClientIdentity(t *testing.T) {
	t.Parallel()
	const (
		clientUserAgent = "codex-tui/0.153.4 (Mac OS 26.6.2; arm64) Apple_Terminal/470.2 (codex-tui; 0.153.4)"
		clientWindowID  = "019a3c5e-7f21-7c3a-9b4d-2f6e8a1c0d11:0"
	)
	for _, authType := range []string{model.AuthTypeAPIKey, model.AuthTypeCodexOAuth} {
		t.Run(authType, func(t *testing.T) {
			for _, version := range []string{"", "0.153.4"} {
				t.Run("version="+version, func(t *testing.T) {
					captured := make(chan http.Header, 1)
					upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						captured <- r.Header.Clone()
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w, "event: response.completed\ndata: "+`{"type":"response.completed","response":{"id":"resp-identity","status":"completed","output":[],"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}}`+"\n\n")
					}))
					defer upstream.Close()
					channel := testChannel{
						name: "codex-identity", upstreamProtocol: "codex", models: "gpt-test",
						authType: authType, apiKey: "sk-upstream",
					}
					if authType == model.AuthTypeCodexOAuth {
						channel.oauthCredential = codexProxyTestCredential(t, "at-upstream", "rt-upstream", "account-upstream")
					}
					env := setupProxyTestEnv(t, []testChannel{channel}, map[int]string{0: upstream.URL})
					headers := map[string]string{
						"User-Agent": clientUserAgent, "Originator": "codex-tui",
						"X-Codex-Window-Id": clientWindowID,
					}
					if version != "" {
						headers["Version"] = version
					}
					response := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
						"model": "gpt-test", "instructions": "test", "input": []any{}, "stream": true,
					}, headers)
					if response.Code != http.StatusOK {
						t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
					}
					var got http.Header
					select {
					case got = <-captured:
					default:
						t.Fatal("upstream request was not captured")
					}
					// Version 与客户端一致：缺失时不从 UA 补写，否则会凭空触发模型版本门控。
					if got.Get("User-Agent") != clientUserAgent || got.Get("Version") != version {
						t.Errorf("upstream identity = UA %q Version %q, want UA %q Version %q",
							got.Get("User-Agent"), got.Get("Version"), clientUserAgent, version)
					}
					windowID := got.Get("X-Codex-Window-Id")
					if authType == model.AuthTypeAPIKey && windowID != clientWindowID {
						t.Errorf("upstream X-Codex-Window-Id = %q, want %s", windowID, clientWindowID)
					}
					// Codex OAuth 不透传客户端原始 ID，只保留窗口代数后缀。
					if authType == model.AuthTypeCodexOAuth && (windowID == clientWindowID || !strings.HasSuffix(windowID, ":0")) {
						t.Errorf("upstream X-Codex-Window-Id = %q, want account-scoped <id>:0", windowID)
					}
					// 官方只对 ChatGPT 登录压缩请求体；API Key 渠道原样发送。
					wantEncoding := ""
					if authType == model.AuthTypeCodexOAuth {
						wantEncoding = "zstd"
					}
					if got.Get("Content-Encoding") != wantEncoding {
						t.Errorf("upstream Content-Encoding = %q, want %q", got.Get("Content-Encoding"), wantEncoding)
					}
				})
			}
		})
	}
}

func TestProxy_CodexOAuthUsageLimitCoolsActualModel(t *testing.T) {
	t.Parallel()
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprintf("streaming=%t", streaming), func(t *testing.T) {
			var attempts atomic.Int64
			upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				attempts.Add(1)
				w.Header().Set("X-Codex-Primary-Used-Percent", "100")
				w.Header().Set("X-Codex-Primary-Window-Minutes", "10080")
				w.Header().Set("X-Codex-Primary-Reset-After-Seconds", "7260")
				if streaming {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "data: "+`{"type":"error","status_code":429,"error":{"type":"usage_limit_reached","message":"The usage limit has been reached","plan_type":"plus","resets_in_seconds":7260}}`+"\n\n")
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = io.WriteString(w, `{"error":{"type":"usage_limit_reached","message":"The usage limit has been reached","plan_type":"plus","resets_in_seconds":7260}}`)
			}))
			defer upstream.Close()

			credential := strings.TrimSuffix(codexProxyTestCredential(t, "at-usage-limit", "rt-usage-limit", "account-usage-limit"), "}") +
				`,"quota_overdraft":{"enabled":true}}`
			env := setupProxyTestEnv(t, []testChannel{{
				name: "codex-oauth-usage-limit", upstreamProtocol: "codex", models: "gpt-5.4-mini,gpt-5.4", priority: 100,
				authType: model.AuthTypeCodexOAuth, oauthCredential: credential,
			}}, map[int]string{0: upstream.URL})

			before := time.Now()
			response := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
				"model":  "gpt-5.4-mini",
				"input":  []any{map[string]any{"type": "message", "role": "user", "content": "hello"}},
				"stream": streaming,
			}, nil)
			if response.Code != http.StatusTooManyRequests || attempts.Load() != 1 {
				t.Fatalf("status=%d upstream attempts=%d, want 429 after one attempt; body=%s",
					response.Code, attempts.Load(), response.Body.String())
			}

			configs, err := env.store.ListConfigs(context.Background())
			if err != nil || len(configs) != 1 {
				t.Fatalf("ListConfigs: configs=%d err=%v", len(configs), err)
			}
			cooldowns, err := env.store.GetAllModelCooldowns(context.Background())
			if err != nil {
				t.Fatalf("get model cooldowns: %v", err)
			}
			duration := cooldowns[configs[0].ID]["gpt-5.4-mini"].Sub(before)
			if duration < 7250*time.Second || duration > 7270*time.Second {
				t.Fatalf("model cooldown duration=%v, want about 7260s", duration)
			}
			if _, exists := cooldowns[configs[0].ID]["gpt-5.4"]; exists {
				t.Fatal("unaffected Codex model must not be cooled")
			}
			persistedCredential, err := codexauth.ParseCredential([]byte(configs[0].OAuthCredential))
			if err != nil || persistedCredential.PassiveUsage == nil || len(persistedCredential.PassiveUsage.Windows) != 1 ||
				persistedCredential.PassiveUsage.Windows[0].UsedPercent != 100 ||
				persistedCredential.PassiveUsage.Windows[0].LimitWindowSeconds != 7*24*60*60 ||
				persistedCredential.PassiveUsage.Windows[0].ResetAt < before.Add(7250*time.Second).Unix() {
				t.Fatalf("Codex quota = (%#v, %v)", persistedCredential, err)
			}
		})
	}
}

func TestProxy_RetryOtherKeysOnFailure(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name                    string
		retryOtherKeysOnFailure bool
		wantPrimaryAttempts     int64
		wantFallbackAttempts    int64
	}{
		{
			name:                 "disabled keeps channel failover behavior",
			wantPrimaryAttempts:  1,
			wantFallbackAttempts: 1,
		},
		{
			name:                    "enabled tries another key before another channel",
			retryOtherKeysOnFailure: true,
			wantPrimaryAttempts:     2,
			wantFallbackAttempts:    0,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var primaryAttempts atomic.Int64
			var fallbackAttempts atomic.Int64
			upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				primaryAttempts.Add(1)
				w.Header().Set("Content-Type", "application/json")
				if r.Header.Get("Authorization") == "Bearer sk-provider-a" {
					w.WriteHeader(http.StatusBadGateway)
					_, _ = w.Write([]byte(`{"error":{"message":"provider A unavailable"}}`))
					return
				}
				if r.Header.Get("Authorization") != "Bearer sk-provider-b" {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				_, _ = w.Write([]byte(`{"id":"provider-b","choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
			}))
			defer upstream.Close()
			fallback := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				fallbackAttempts.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"fallback","choices":[{"message":{"content":"fallback"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
			}))
			defer fallback.Close()

			env := setupProxyTestEnv(t, []testChannel{
				{name: "relay", models: "gpt-test", apiKey: "sk-provider-a", priority: 100, retryOtherKeysOnFailure: tt.retryOtherKeysOnFailure},
				{name: "fallback", models: "gpt-test", priority: 50},
			}, map[int]string{0: upstream.URL, 1: fallback.URL})
			if err := env.store.CreateAPIKeysBatch(context.Background(), []*model.APIKey{{
				ChannelID: 1, KeyIndex: 1, APIKey: "sk-provider-b", Priority: -1, KeyStrategy: model.KeyStrategySequential,
			}}); err != nil {
				t.Fatalf("create secondary key: %v", err)
			}

			response := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
				"model": "gpt-test", "messages": []map[string]string{{"role": "user", "content": "hi"}},
			}, nil)
			if response.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if got := primaryAttempts.Load(); got != tt.wantPrimaryAttempts {
				t.Fatalf("primary attempts=%d, want %d", got, tt.wantPrimaryAttempts)
			}
			if got := fallbackAttempts.Load(); got != tt.wantFallbackAttempts {
				t.Fatalf("fallback attempts=%d, want %d", got, tt.wantFallbackAttempts)
			}

			if tt.retryOtherKeysOnFailure {
				keys, err := env.store.GetAPIKeys(context.Background(), 1)
				if err != nil {
					t.Fatalf("get relay keys: %v", err)
				}
				if len(keys) != 2 || keys[0].CooldownUntil <= time.Now().Unix() {
					t.Fatalf("failed provider key should be cooled, keys=%+v", keys)
				}
				cooldowns, err := env.store.GetAllModelCooldowns(context.Background())
				if err != nil {
					t.Fatalf("get model cooldowns: %v", err)
				}
				if len(cooldowns[1]) != 0 {
					t.Fatalf("key-fallback mode must not cool the model, got %+v", cooldowns[1])
				}
			}
		})
	}
}

func TestProxy_SameChannelRetries(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name                 string
		sameChannelRetries   int
		failures             int64 // 主渠道前 N 次请求失败
		failStatus           int
		wantPrimaryAttempts  int64
		wantFallbackAttempts int64
		wantPrimaryCooled    bool
	}{
		{
			name: "disabled switches channel on first 5xx", failures: 1, failStatus: http.StatusBadGateway,
			wantPrimaryAttempts: 1, wantFallbackAttempts: 1, wantPrimaryCooled: true,
		},
		{
			name: "transient 5xx recovers on same channel without cooldown", sameChannelRetries: 2, failures: 2, failStatus: http.StatusBadGateway,
			wantPrimaryAttempts: 3, wantFallbackAttempts: 0,
		},
		{
			name: "exhausted retries cool once then switch", sameChannelRetries: 2, failures: 100, failStatus: http.StatusBadGateway,
			wantPrimaryAttempts: 3, wantFallbackAttempts: 1, wantPrimaryCooled: true,
		},
		{
			name: "rate limit is not retried on same channel", sameChannelRetries: 2, failures: 100, failStatus: http.StatusTooManyRequests,
			wantPrimaryAttempts: 1, wantFallbackAttempts: 1, wantPrimaryCooled: true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var primaryAttempts, fallbackAttempts atomic.Int64
			primary := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if primaryAttempts.Add(1) <= tt.failures {
					w.WriteHeader(tt.failStatus)
					_, _ = w.Write([]byte(`{"error":{"message":"upstream flaky"}}`))
					return
				}
				_, _ = w.Write([]byte(`{"id":"primary","choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
			}))
			defer primary.Close()
			fallback := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				fallbackAttempts.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"fallback","choices":[{"message":{"content":"fallback"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
			}))
			defer fallback.Close()

			env := setupProxyTestEnv(t, []testChannel{
				{name: "flaky", models: "gpt-test", priority: 100, sameChannelRetries: tt.sameChannelRetries},
				{name: "fallback", models: "gpt-test", priority: 50},
			}, map[int]string{0: primary.URL, 1: fallback.URL})

			response := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
				"model": "gpt-test", "messages": []map[string]string{{"role": "user", "content": "hi"}},
			}, nil)
			if response.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if got := primaryAttempts.Load(); got != tt.wantPrimaryAttempts {
				t.Fatalf("primary attempts=%d, want %d", got, tt.wantPrimaryAttempts)
			}
			if got := fallbackAttempts.Load(); got != tt.wantFallbackAttempts {
				t.Fatalf("fallback attempts=%d, want %d", got, tt.wantFallbackAttempts)
			}

			ctx := context.Background()
			cfg, err := env.store.GetConfig(ctx, 1)
			if err != nil {
				t.Fatalf("get primary config: %v", err)
			}
			modelCooldowns, err := env.store.GetAllModelCooldowns(ctx)
			if err != nil {
				t.Fatalf("get model cooldowns: %v", err)
			}
			cooledUntil := time.Unix(cfg.CooldownUntil, 0)
			for _, until := range modelCooldowns[1] {
				if until.After(cooledUntil) {
					cooledUntil = until
				}
			}
			now := time.Now()
			if !tt.wantPrimaryCooled {
				if cooledUntil.After(now) {
					t.Fatalf("recovered channel must not be cooled, until=%v", cooledUntil)
				}
				return
			}
			if !cooledUntil.After(now) {
				t.Fatalf("primary channel should be cooled after giving up")
			}
			// 只提交最后一次失败：冷却停在退避首档，没有因重试翻倍。
			if remaining := cooledUntil.Sub(now); remaining > 3*time.Minute {
				t.Fatalf("cooldown applied more than once, remaining=%v", remaining)
			}
		})
	}
}

func TestProxy_SameChannelRetriesToleratesCommittedStreamInterruptions(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name               string
		sameChannelRetries int
		outcomes           []bool // 依次每个请求上游是否中途断流
		wantCooled         bool
	}{
		{name: "disabled cools on first interruption", outcomes: []bool{true}, wantCooled: true},
		{name: "interruptions within limit do not cool", sameChannelRetries: 2, outcomes: []bool{true, true}},
		{name: "interruption beyond limit cools", sameChannelRetries: 2, outcomes: []bool{true, true, true}, wantCooled: true},
		{name: "success resets consecutive count", sameChannelRetries: 2, outcomes: []bool{true, true, false, true, true}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var interrupt atomic.Bool
			upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "event: response.output_text.delta\n"+
					`data: {"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"partial"}`+"\n\n")
				if interrupt.Load() {
					if flusher, ok := w.(http.Flusher); ok {
						flusher.Flush()
					}
					panic(http.ErrAbortHandler)
				}
				_, _ = io.WriteString(w, "event: response.completed\n"+
					`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`+"\n\n")
			}))
			defer upstream.Close()

			env := setupProxyTestEnv(t, []testChannel{{
				name: "flaky", upstreamProtocol: "codex", models: "gpt-test", priority: 100,
				sameChannelRetries: tt.sameChannelRetries,
			}}, map[int]string{0: upstream.URL})

			for i, interrupted := range tt.outcomes {
				interrupt.Store(interrupted)
				response := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
					"model": "gpt-test", "stream": true, "input": "hi",
				}, nil)
				if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "partial") {
					t.Fatalf("request %d status=%d body=%s", i, response.Code, response.Body.String())
				}
			}

			modelCooldowns, err := env.store.GetAllModelCooldowns(context.Background())
			if err != nil {
				t.Fatalf("get model cooldowns: %v", err)
			}
			cooled := false
			for _, until := range modelCooldowns[1] {
				cooled = cooled || until.After(time.Now())
			}
			if cooled != tt.wantCooled {
				t.Fatalf("cooled=%v, want %v (cooldowns=%v)", cooled, tt.wantCooled, modelCooldowns[1])
			}
		})
	}
}

func TestProxy_RetryOtherKeysSessionAffinity(t *testing.T) {
	t.Parallel()
	var phase atomic.Int32
	var keyACalls, keyBCalls, fallbackCalls atomic.Int64

	primary := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var calls *atomic.Int64
		switch r.Header.Get("Authorization") {
		case "Bearer sk-provider-a":
			calls = &keyACalls
		case "Bearer sk-provider-b":
			calls = &keyBCalls
		default:
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		calls.Add(1)

		currentPhase := phase.Load()
		failed := currentPhase == 4 ||
			(currentPhase == 0 || currentPhase == 3) && r.Header.Get("Authorization") == "Bearer sk-provider-a"
		if failed {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"error":{"message":"provider unavailable"}}`))
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.completed\n"+
			`data: {"type":"response.completed","response":{"id":"resp-primary","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`+"\n\n")
	}))
	fallback := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fallbackCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.completed\n"+
			`data: {"type":"response.completed","response":{"id":"resp-fallback","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`+"\n\n")
	}))

	env := setupProxyTestEnv(t, []testChannel{
		{
			name: "relay", upstreamProtocol: "codex", models: "gpt-test",
			apiKey: "sk-provider-a", priority: 100, retryOtherKeysOnFailure: true,
		},
		{
			name: "fallback", upstreamProtocol: "codex", models: "gpt-test",
			priority: 50, retryOtherKeysOnFailure: true,
		},
	}, map[int]string{0: primary.URL, 1: fallback.URL})
	if err := env.store.CreateAPIKeysBatch(context.Background(), []*model.APIKey{{
		ChannelID: 1, KeyIndex: 1, APIKey: "sk-provider-b", Priority: -1, KeyStrategy: model.KeyStrategySequential,
	}}); err != nil {
		t.Fatalf("create relay keys: %v", err)
	}
	env.server.maxKeyRetries = 1

	request := func(input string) *httptest.ResponseRecorder {
		return doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
			"model": "gpt-test", "stream": true, "input": input,
		}, map[string]string{"Session-Id": "sticky-key-session"})
	}
	resetRelay := func() {
		for keyIndex := range 2 {
			if err := env.store.ResetKeyCooldown(context.Background(), 1, keyIndex); err != nil {
				t.Fatalf("reset relay key %d: %v", keyIndex, err)
			}
		}
		if err := env.store.ResetChannelCooldown(context.Background(), 1); err != nil {
			t.Fatalf("reset relay channel: %v", err)
		}
		env.server.invalidateChannelRelatedCache(1)
	}
	assertCalls := func(wantA, wantB, wantFallback int64) {
		t.Helper()
		if keyACalls.Load() != wantA || keyBCalls.Load() != wantB || fallbackCalls.Load() != wantFallback {
			t.Fatalf("calls a/b/fallback=%d/%d/%d, want %d/%d/%d",
				keyACalls.Load(), keyBCalls.Load(), fallbackCalls.Load(), wantA, wantB, wantFallback)
		}
	}

	if response := request("one"); response.Code != http.StatusOK {
		t.Fatalf("first response status=%d body=%s", response.Code, response.Body.String())
	}
	assertCalls(1, 1, 0)

	// Make the fallback globally preferable. Session affinity must still keep the
	// established relay first while it has an available Key.
	fallbackCfg, err := env.store.GetConfig(context.Background(), 2)
	if err != nil {
		t.Fatalf("get fallback config: %v", err)
	}
	fallbackCfg.Priority = 200
	if _, err := env.store.UpdateConfig(context.Background(), fallbackCfg.ID, fallbackCfg); err != nil {
		t.Fatalf("raise fallback priority: %v", err)
	}
	env.server.InvalidateChannelListCache()

	phase.Store(1)
	if response := request("two"); response.Code != http.StatusOK {
		t.Fatalf("cooled-key response status=%d body=%s", response.Code, response.Body.String())
	}
	assertCalls(1, 2, 0)

	// Once A recovers, sequential selection must fail back to it instead of
	// pinning the last successful Key B.
	if err := env.store.ResetKeyCooldown(context.Background(), 1, 0); err != nil {
		t.Fatalf("recover relay key A: %v", err)
	}
	env.server.invalidateChannelRelatedCache(1)

	phase.Store(2)
	if response := request("three"); response.Code != http.StatusOK {
		t.Fatalf("recovered-key response status=%d body=%s", response.Code, response.Body.String())
	}
	assertCalls(2, 2, 0)

	phase.Store(3)
	if response := request("four"); response.Code != http.StatusOK {
		t.Fatalf("recooled-key response status=%d body=%s", response.Code, response.Body.String())
	}
	assertCalls(3, 3, 0)

	phase.Store(1)
	if response := request("five"); response.Code != http.StatusOK {
		t.Fatalf("second cooled-key response status=%d body=%s", response.Code, response.Body.String())
	}
	assertCalls(3, 4, 0)

	resetRelay()
	phase.Store(4)
	if response := request("six"); response.Code != http.StatusOK {
		t.Fatalf("channel fallback response status=%d body=%s", response.Code, response.Body.String())
	}
	assertCalls(4, 5, 1)

	// The preferred relay is now fully cooled, so the next turn must stay on the
	// fallback without probing relay Keys early.
	if response := request("seven"); response.Code != http.StatusOK {
		t.Fatalf("cooled-channel response status=%d body=%s", response.Code, response.Body.String())
	}
	assertCalls(4, 5, 2)

	// A successful fallback must not replace the first successful channel. Once
	// the relay recovers it wins again despite the fallback's higher priority.
	resetRelay()
	phase.Store(5)
	if response := request("eight"); response.Code != http.StatusOK {
		t.Fatalf("recovered-channel response status=%d body=%s", response.Code, response.Body.String())
	}
	assertCalls(5, 5, 2)
}

func TestProxy_GlobalCooldownDetectionRulesFallbackAndChannelOverride(t *testing.T) {
	t.Parallel()
	globalRules := `{"rules":[{"enabled":true,"name":"Global maintenance","priority":0,"status_codes":[406],"message_pattern":"planned maintenance","scope":"channel","mode":"fixed","cooldown_seconds":120}]}`

	t.Run("channel without rules inherits global rules", func(t *testing.T) {
		var fallbackCalls atomic.Int64
		upstreamFail := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotAcceptable)
			_, _ = w.Write([]byte(`{"error":{"message":"planned maintenance"}}`))
		}))
		defer upstreamFail.Close()
		upstreamFallback := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			fallbackCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"global-fallback","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
		}))
		defer upstreamFallback.Close()

		env := setupProxyTestEnvWithSettings(t, []testChannel{
			{name: "inherits-global", models: "gpt-4", priority: 100},
			{name: "fallback", models: "gpt-4", priority: 50},
		}, map[int]string{0: upstreamFail.URL, 1: upstreamFallback.URL}, map[string]string{
			"global_cooldown_detection_rules": globalRules,
		})

		response := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
			"model": "gpt-4", "messages": []map[string]string{{"role": "user", "content": "hi"}},
		}, nil)
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "global-fallback") {
			t.Fatalf("status=%d body=%s, want fallback success", response.Code, response.Body.String())
		}
		if got := fallbackCalls.Load(); got != 1 {
			t.Fatalf("fallback calls=%d, want 1", got)
		}
	})

	t.Run("channel rules replace global rules", func(t *testing.T) {
		var fallbackCalls atomic.Int64
		upstreamFail := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotAcceptable)
			_, _ = w.Write([]byte(`{"error":{"message":"planned maintenance"}}`))
		}))
		defer upstreamFail.Close()
		upstreamFallback := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			fallbackCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"unexpected-fallback","choices":[]}`))
		}))
		defer upstreamFallback.Close()

		env := setupProxyTestEnvWithSettings(t, []testChannel{
			{
				name: "overrides-global", models: "gpt-4", priority: 100,
				cooldownDetectionRules: &model.CooldownDetectionRules{Rules: []model.CooldownDetectionRule{{
					Enabled: true, Name: "Local teapot", Priority: 0, StatusCodes: []int{http.StatusTeapot},
					Scope: model.CooldownScopeChannel, Mode: model.CooldownModeFixed, CooldownSeconds: 60,
				}}},
			},
			{name: "fallback", models: "gpt-4", priority: 50},
		}, map[int]string{0: upstreamFail.URL, 1: upstreamFallback.URL}, map[string]string{
			"global_cooldown_detection_rules": globalRules,
		})

		response := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
			"model": "gpt-4", "messages": []map[string]string{{"role": "user", "content": "hi"}},
		}, nil)
		if response.Code != http.StatusNotAcceptable {
			t.Fatalf("status=%d body=%s, want channel rule override to preserve 406", response.Code, response.Body.String())
		}
		if got := fallbackCalls.Load(); got != 0 {
			t.Fatalf("fallback calls=%d, want 0", got)
		}
	})
}

// doProxyRequest 发送代理请求并返回响应
func TestProxy_DecodesCompressedRequestBody(t *testing.T) {
	t.Parallel()
	var upstreamCalls atomic.Int32
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read upstream body: %v", err)
		}
		if got := r.Header.Get("Content-Encoding"); got != "" {
			t.Errorf("upstream Content-Encoding = %q, want plaintext forward", got)
		}
		if got := gjson.GetBytes(body, "messages.0.content").String(); got != "compressed hi" {
			t.Errorf("upstream body = %s", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chat-1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer upstream.Close()
	env := setupProxyTestEnv(t, []testChannel{{
		name: "compressed-inbound", upstreamProtocol: "openai", models: "gpt-test",
	}}, map[int]string{0: upstream.URL})

	encoder, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatalf("zstd writer: %v", err)
	}
	compressed := encoder.EncodeAll([]byte(`{"model":"gpt-test","messages":[{"role":"user","content":"compressed hi"}]}`), nil)
	_ = encoder.Close()
	send := func(contentEncoding string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(compressed))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Content-Encoding", contentEncoding)
		req.Header.Set("Authorization", "Bearer test-api-key")
		w := httptest.NewRecorder()
		env.engine.ServeHTTP(w, req)
		return w
	}

	if response := send("zstd"); response.Code != http.StatusOK {
		t.Fatalf("zstd status=%d body=%s", response.Code, response.Body.String())
	}
	if response := send("compress"); response.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("unsupported encoding status=%d body=%s", response.Code, response.Body.String())
	}
	if got := upstreamCalls.Load(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1", got)
	}
}

func doProxyRequest(t testing.TB, engine *gin.Engine, path string, body any, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()

	var bodyReader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		bodyReader = bytes.NewReader(b)
	}

	req := httptest.NewRequest(http.MethodPost, path, bodyReader)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-api-key") // 默认 token

	for k, v := range headers {
		req.Header.Set(k, v)
	}

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	return w
}

func createDashboardSession(t testing.TB, env *proxyTestEnv, plainToken string, authToken *model.AuthToken) string {
	t.Helper()
	env.server.authService.apiTokenLoginEnabled = true
	authToken.Token = model.HashToken(plainToken)
	authToken.CreatedAt = time.Now()
	authToken.IsActive = true
	if err := env.store.CreateAuthToken(context.Background(), authToken); err != nil {
		t.Fatalf("CreateAuthToken failed: %v", err)
	}
	if err := env.server.authService.ReloadAuthTokens(); err != nil {
		t.Fatalf("ReloadAuthTokens failed: %v", err)
	}

	w := doProxyRequest(t, env.engine, "/login", map[string]any{
		"mode":  model.WebRoleAPIToken,
		"token": plainToken,
	}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("dashboard login status=%d body=%s", w.Code, w.Body.String())
	}
	var data struct {
		Token string `json:"token"`
	}
	mustUnmarshalAPIResponseData(t, w.Body.Bytes(), &data)
	if data.Token == "" || data.Token == plainToken {
		t.Fatalf("dashboard login returned invalid web session token %q", data.Token)
	}
	return data.Token
}

// ============================================================================
// P0: 代理转发核心链路测试
// ============================================================================

func TestProxy_Success_NonStreaming(t *testing.T) {
	t.Parallel()

	// 模拟上游：返回 200 + JSON
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","choices":[{"message":{"content":"hello"}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "ch1", models: "gpt-4", apiKey: "sk-1"},
	}, map[int]string{0: upstream.URL})

	w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model":    "gpt-4",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// 验证响应透传
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp["id"] != "chatcmpl-1" {
		t.Fatalf("expected id=chatcmpl-1, got %v", resp["id"])
	}
}

func TestProxy_ModelCooldownSSEErrorReturns429(t *testing.T) {
	t.Parallel()
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, "event: error\n")
		_, _ = fmt.Fprint(w, "data: "+`{"type":"error","error":{"code":"model_cooldown","message":"model temporarily unavailable","model":"gpt-5.5"}}`+"\n\n")
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "sse-model-cooldown", models: "gpt-5.5"},
	}, map[int]string{0: upstream.URL})

	w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model":    "gpt-5.5",
		"messages": []map[string]string{{"role": "user", "content": "hello"}},
		"stream":   true,
	}, nil)

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status=%d, want 429; body=%s", w.Code, w.Body.String())
	}
}

// OpenAI Responses 的 rate limit 失败终态：HTTP 200 + event:response.failed，
// error 嵌在 response.error。漏判会把限流当成功 200 返回。
func TestProxy_ResponseFailedSSERateLimitReturns429(t *testing.T) {
	t.Parallel()
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, "event: response.failed\n")
		_, _ = fmt.Fprint(w, "data: "+`{"type":"response.failed","response":{"id":"resp_5ca0fb7943504d6a93576c7fb7e3a760","object":"response","model":"gpt-5.6-sol","status":"failed","output":[],"error":{"code":"rate_limit_exceeded","message":"Upstream rate limit exceeded, please retry later"}}}`+"\n\n")
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "sse-response-failed", models: "gpt-5.6-sol"},
	}, map[int]string{0: upstream.URL})

	w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model":    "gpt-5.6-sol",
		"messages": []map[string]string{{"role": "user", "content": "hello"}},
		"stream":   true,
	}, nil)

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status=%d, want 429; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "rate_limit_exceeded") &&
		!strings.Contains(w.Body.String(), "rate limit") &&
		!strings.Contains(strings.ToLower(w.Body.String()), "rate") {
		// 至少不能再当成功 200 空响应；body 内容允许被包装，但状态码必须是 429
		t.Logf("response body: %s", w.Body.String())
	}
}

func TestProxy_ModelCooldownUsesCustomRuleFinalModelKey(t *testing.T) {
	t.Parallel()
	const finalModel = "shared-upstream-model"

	var primaryHits atomic.Int64
	primary := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryHits.Add(1)
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode primary request: %v", err)
		} else if body["model"] != finalModel {
			t.Errorf("primary model=%v, want %s", body["model"], finalModel)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"code":"model_cooldown","message":"model temporarily unavailable","model":"shared-upstream-model","reset_seconds":300}}`))
	}))
	defer primary.Close()

	secondary := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp-ok","object":"response","status":"completed","model":"fallback-model","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer secondary.Close()

	rules := &model.CustomRequestRules{Body: []model.CustomBodyRule{{
		Action: model.RuleActionOverride,
		Path:   "model",
		Value:  json.RawMessage(`"shared-upstream-model"`),
	}}}
	env := setupProxyTestEnv(t, []testChannel{
		{
			name:               "custom-rule-primary",
			upstreamProtocol:   util.ProtocolCodex,
			customRequestRules: rules,
			models:             "external-model-a,external-model-b",
			priority:           100,
		},
		{
			name:             "fallback-secondary",
			upstreamProtocol: util.ProtocolCodex,
			models:           "external-model-a,external-model-b",
			priority:         50,
		},
	}, map[int]string{0: primary.URL, 1: secondary.URL})

	request := func(modelName string) {
		t.Helper()
		w := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
			"model":  modelName,
			"input":  "hello",
			"stream": false,
		}, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("model=%s status=%d, want 200; body=%s", modelName, w.Code, w.Body.String())
		}
	}

	request("external-model-a")

	configs, err := env.store.ListConfigs(context.Background())
	if err != nil {
		t.Fatalf("list configs: %v", err)
	}
	var primaryID int64
	for _, cfg := range configs {
		if cfg.Name == "custom-rule-primary" {
			primaryID = cfg.ID
			break
		}
	}
	if primaryID == 0 {
		t.Fatal("primary channel not found")
	}
	cooldowns, err := env.store.GetAllModelCooldowns(context.Background())
	if err != nil {
		t.Fatalf("get model cooldowns: %v", err)
	}
	if until := cooldowns[primaryID][finalModel]; !until.After(time.Now()) {
		t.Fatalf("final model cooldown=%s, want active cooldown", until.Format(time.RFC3339))
	}
	if _, exists := cooldowns[primaryID]["external-model-a"]; exists {
		t.Fatal("external alias must not be used as model cooldown key")
	}
	channelCooldowns, err := env.store.GetAllChannelCooldowns(context.Background())
	if err != nil {
		t.Fatalf("get channel cooldowns: %v", err)
	}
	if until := channelCooldowns[primaryID]; until.After(time.Now()) {
		t.Fatalf("channel cooldown=%s, want model-only cooldown because another protocol path may resolve differently", until.Format(time.RFC3339))
	}

	request("external-model-b")
	if got := primaryHits.Load(); got != 1 {
		t.Fatalf("primary hits=%d, want 1 after shared final model cooldown", got)
	}
}

func TestProxy_AlphaSearchPassthroughWithRestrictedToken(t *testing.T) {
	t.Parallel()

	var upstreamHits atomic.Int64
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		if r.Method != http.MethodPost {
			t.Errorf("upstream method=%q, want POST", r.Method)
		}
		if r.URL.Path != "/v1/alpha/search" {
			t.Errorf("upstream path=%q, want /v1/alpha/search", r.URL.Path)
		}
		if got := r.URL.Query().Get("scope"); got != "repo" {
			t.Errorf("upstream scope=%q, want repo", got)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode upstream body: %v", err)
		} else if body["query"] != "codegraph" {
			t.Errorf("upstream query=%v, want codegraph", body["query"])
		}
		if _, exists := body["prompt_cache_key"]; exists {
			t.Errorf("upstream body contains prompt_cache_key: %v", body)
		}
		if _, exists := body["prompt_cache_retention"]; exists {
			t.Errorf("upstream body contains prompt_cache_retention: %v", body)
		}
		if got := r.Header.Get("Session_id"); got != "" {
			t.Errorf("upstream Session_id=%q, want empty", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "native-codex", upstreamProtocol: util.ProtocolCodex, models: "gpt-5"},
	}, map[int]string{0: upstream.URL})

	plainToken := "sk-alpha-restricted"
	authToken := &model.AuthToken{
		Token:         model.HashToken(plainToken),
		Description:   "alpha search restricted token",
		CreatedAt:     time.Now(),
		IsActive:      true,
		AllowedModels: []string{"gpt-5"},
	}
	if err := env.store.CreateAuthToken(context.Background(), authToken); err != nil {
		t.Fatalf("CreateAuthToken failed: %v", err)
	}
	if err := env.server.authService.ReloadAuthTokens(); err != nil {
		t.Fatalf("ReloadAuthTokens failed: %v", err)
	}

	w := doProxyRequest(t, env.engine, "/v1/alpha/search?scope=repo", map[string]any{
		"query":                  "codegraph",
		"prompt_cache_key":       "responses-cache-key",
		"prompt_cache_retention": "24h",
	}, map[string]string{"Authorization": "Bearer " + plainToken})

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200: %s", w.Code, w.Body.String())
	}
	if upstreamHits.Load() != 1 {
		t.Fatalf("upstream hits=%d, want 1", upstreamHits.Load())
	}
	// alpha/search 无 request model：日志 model 记 search_call，按次 $0.01
	entry := waitForProxyLog(t, env, util.BillingModelSearchCall)
	if entry.AuthTokenID != authToken.ID {
		t.Fatalf("AuthTokenID=%d, want %d", entry.AuthTokenID, authToken.ID)
	}
	if entry.Cost != 0.01 {
		t.Fatalf("Cost=%v, want 0.01", entry.Cost)
	}

	blocked := doProxyRequest(t, env.engine, "/v1/alpha/search", map[string]any{
		"model": "blocked-model",
		"query": "codegraph",
	}, map[string]string{"Authorization": "Bearer " + plainToken})
	if blocked.Code != http.StatusForbidden {
		t.Fatalf("explicit blocked model status=%d, want 403: %s", blocked.Code, blocked.Body.String())
	}
	if upstreamHits.Load() != 1 {
		t.Fatalf("blocked model reached upstream, hits=%d", upstreamHits.Load())
	}
}

func TestProxy_AlphaSearchUnsupportedFallsBackToEmptyResult(t *testing.T) {
	t.Parallel()
	var upstreamHits atomic.Int64
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"message":"Invalid URL (POST /v1/alpha/search)"}}`))
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "alpha-search-failure", upstreamProtocol: util.ProtocolCodex, models: "gpt-5"},
	}, map[int]string{0: upstream.URL})

	request := func() *httptest.ResponseRecorder {
		return doProxyRequest(t, env.engine, "/v1/alpha/search", map[string]any{
			"query": "codegraph",
		}, nil)
	}

	w := request()
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200: %s", w.Code, w.Body.String())
	}
	var fallback struct {
		EncryptedOutput *string `json:"encrypted_output"`
		Output          string  `json:"output"`
		Results         []any   `json:"results"`
	}
	mustUnmarshalJSON(t, w.Body.Bytes(), &fallback)
	if fallback.EncryptedOutput != nil || fallback.Output != "" || len(fallback.Results) != 0 {
		t.Fatalf("unexpected empty search fallback: %+v", fallback)
	}
	configs, err := env.store.ListConfigs(context.Background())
	if err != nil || len(configs) != 1 {
		t.Fatalf("ListConfigs: configs=%d err=%v", len(configs), err)
	}
	if got := env.server.costCache.Get(configs[0].ID); got != 0 {
		t.Fatalf("failed request cached cost=%v, want 0", got)
	}
	cooldowns, err := env.store.GetAllChannelCooldowns(context.Background())
	if err != nil {
		t.Fatalf("GetAllChannelCooldowns failed: %v", err)
	}
	if until := cooldowns[configs[0].ID]; until.After(time.Now()) {
		t.Fatalf("alpha search capability miss cooled channel until %v", until)
	}

	w = request()
	if w.Code != http.StatusOK {
		t.Fatalf("cached fallback status=%d, want 200: %s", w.Code, w.Body.String())
	}
	if got := upstreamHits.Load(); got != 1 {
		t.Fatalf("unsupported alpha search upstream hits=%d, want 1", got)
	}
}

func TestProxy_AlphaSearchUnsupportedFallsBackToNextChannel(t *testing.T) {
	t.Parallel()
	var unsupportedHits atomic.Int64
	unsupported := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		unsupportedHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"message":"Invalid URL (POST /v1/alpha/search)"}}`))
	}))
	defer unsupported.Close()

	var supportedHits atomic.Int64
	supported := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		supportedHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"encrypted_output":null,"output":"search result","results":[]}`))
	}))
	defer supported.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "alpha-search-unsupported", upstreamProtocol: util.ProtocolCodex, models: "gpt-5", priority: 100},
		{name: "alpha-search-supported", upstreamProtocol: util.ProtocolCodex, models: "gpt-5", priority: 90},
	}, map[int]string{0: unsupported.URL, 1: supported.URL})

	request := func() *httptest.ResponseRecorder {
		return doProxyRequest(t, env.engine, "/v1/alpha/search", map[string]any{
			"query": "codegraph",
		}, nil)
	}
	for i := 0; i < 2; i++ {
		w := request()
		if w.Code != http.StatusOK {
			t.Fatalf("request %d status=%d, want 200: %s", i+1, w.Code, w.Body.String())
		}
		var response struct {
			Output string `json:"output"`
		}
		mustUnmarshalJSON(t, w.Body.Bytes(), &response)
		if response.Output != "search result" {
			t.Fatalf("request %d output=%q, want search result", i+1, response.Output)
		}
	}
	if got := unsupportedHits.Load(); got != 1 {
		t.Fatalf("unsupported upstream hits=%d, want 1", got)
	}
	if got := supportedHits.Load(); got != 2 {
		t.Fatalf("supported upstream hits=%d, want 2", got)
	}
}

func TestProxy_AlphaSearchExactURLRouting(t *testing.T) {
	t.Parallel()
	t.Run("responses exact URL is rewritten to alpha/search", func(t *testing.T) {
		var upstreamHits atomic.Int64
		upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			upstreamHits.Add(1)
			if r.URL.Path != "/backend-api/codex/alpha/search" {
				t.Errorf("upstream path=%q, want /backend-api/codex/alpha/search", r.URL.Path)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[]}`))
		}))
		defer upstream.Close()

		env := setupProxyTestEnv(t, []testChannel{{
			name:             "codex-oauth",
			upstreamProtocol: util.ProtocolCodex,
			models:           "gpt-5",
		}}, map[int]string{0: upstream.URL + "/backend-api/codex/responses#"})

		w := doProxyRequest(t, env.engine, "/v1/alpha/search", map[string]any{
			"query": "codegraph",
		}, nil)

		if w.Code != http.StatusOK {
			t.Fatalf("status=%d, want 200: %s", w.Code, w.Body.String())
		}
		if upstreamHits.Load() != 1 {
			t.Fatalf("upstream hits=%d, want 1", upstreamHits.Load())
		}
	})

	t.Run("non-responses exact URL cannot shadow native search", func(t *testing.T) {
		var wrongHits atomic.Int64
		wrong := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			wrongHits.Add(1)
			w.WriteHeader(http.StatusBadRequest)
		}))
		defer wrong.Close()

		var nativeHits atomic.Int64
		native := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			nativeHits.Add(1)
			if r.URL.Path != "/v1/alpha/search" {
				t.Errorf("native upstream path=%q, want /v1/alpha/search", r.URL.Path)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[]}`))
		}))
		defer native.Close()

		env := setupProxyTestEnv(t, []testChannel{
			{
				name:             "chat-exact",
				upstreamProtocol: util.ProtocolCodex,
				models:           "gpt-5",
				priority:         100,
			},
			{
				name:             "native-search",
				upstreamProtocol: util.ProtocolCodex,
				models:           "gpt-5",
				priority:         90,
			},
		}, map[int]string{
			0: wrong.URL + "/v1/chat/completions#",
			1: native.URL,
		})

		w := doProxyRequest(t, env.engine, "/v1/alpha/search", map[string]any{
			"query": "codegraph",
		}, nil)

		if w.Code != http.StatusOK {
			t.Fatalf("status=%d, want 200: %s", w.Code, w.Body.String())
		}
		if wrongHits.Load() != 0 || nativeHits.Load() != 1 {
			t.Fatalf("upstream hits wrong=%d native=%d, want 0/1", wrongHits.Load(), nativeHits.Load())
		}
	})

	t.Run("matching exact URL remains eligible", func(t *testing.T) {
		var upstreamHits atomic.Int64
		upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			upstreamHits.Add(1)
			if r.URL.Path != "/v1/alpha/search" {
				t.Errorf("upstream path=%q, want /v1/alpha/search", r.URL.Path)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[]}`))
		}))
		defer upstream.Close()

		env := setupProxyTestEnv(t, []testChannel{{
			name:             "alpha-search-exact",
			upstreamProtocol: util.ProtocolCodex,
			models:           "gpt-5",
		}}, map[int]string{0: upstream.URL + "/v1/alpha/search#"})

		w := doProxyRequest(t, env.engine, "/v1/alpha/search", map[string]any{
			"query": "codegraph",
		}, nil)

		if w.Code != http.StatusOK {
			t.Fatalf("status=%d, want 200: %s", w.Code, w.Body.String())
		}
		if upstreamHits.Load() != 1 {
			t.Fatalf("upstream hits=%d, want 1", upstreamHits.Load())
		}
	})

	t.Run("codex direct alias rewrites oauth responses url", func(t *testing.T) {
		var upstreamHits atomic.Int64
		upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			upstreamHits.Add(1)
			if r.URL.Path != "/backend-api/codex/alpha/search" {
				t.Errorf("upstream path=%q, want /backend-api/codex/alpha/search", r.URL.Path)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[]}`))
		}))
		defer upstream.Close()

		env := setupProxyTestEnv(t, []testChannel{{
			name:             "codex-oauth",
			upstreamProtocol: util.ProtocolCodex,
			models:           "gpt-5",
		}}, map[int]string{0: upstream.URL + "/backend-api/codex/responses#"})

		w := doProxyRequest(t, env.engine, "/backend-api/codex/alpha/search", map[string]any{
			"query": "codegraph",
		}, nil)

		if w.Code != http.StatusOK {
			t.Fatalf("status=%d, want 200: %s", w.Code, w.Body.String())
		}
		if upstreamHits.Load() != 1 {
			t.Fatalf("upstream hits=%d, want 1", upstreamHits.Load())
		}
	})
}

func TestDashboardProxy_UsesBoundTokenAndStreams(t *testing.T) {
	t.Parallel()

	var upstreamHits atomic.Int64
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("upstream path=%q, want /v1/chat/completions", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"dashboard\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "dashboard-stream", models: "gpt-dashboard", apiKey: "sk-dashboard-upstream"},
	}, map[int]string{0: upstream.URL})
	configs, err := env.store.ListConfigs(context.Background())
	if err != nil || len(configs) != 1 {
		t.Fatalf("ListConfigs len=%d err=%v", len(configs), err)
	}
	authToken := &model.AuthToken{
		Description:       "dashboard stream owner",
		AllowedModels:     []string{"gpt-dashboard"},
		AllowedChannelIDs: []int64{configs[0].ID},
	}
	webSession := createDashboardSession(t, env, "sk-dashboard-stream-owner", authToken)

	w := doProxyRequest(t, env.engine, "/dashboard/v1/chat/completions", map[string]any{
		"model":    "gpt-dashboard",
		"stream":   true,
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, map[string]string{"Authorization": "Bearer " + webSession})
	if w.Code != http.StatusOK {
		t.Fatalf("dashboard proxy status=%d body=%s", w.Code, w.Body.String())
	}
	if upstreamHits.Load() != 1 {
		t.Fatalf("upstream hits=%d, want 1", upstreamHits.Load())
	}
	if body := w.Body.String(); !strings.Contains(body, "dashboard") || !strings.Contains(body, "[DONE]") {
		t.Fatalf("unexpected dashboard SSE body: %s", body)
	}

	entry := waitForProxyLog(t, env, "gpt-dashboard")
	if entry.AuthTokenID != authToken.ID {
		t.Fatalf("AuthTokenID=%d, want %d", entry.AuthTokenID, authToken.ID)
	}
	if entry.ChannelID != configs[0].ID {
		t.Fatalf("ChannelID=%d, want %d", entry.ChannelID, configs[0].ID)
	}
}

func TestDashboardProxy_EnforcesModelAndChannelRestrictions(t *testing.T) {
	t.Parallel()
	t.Run("model", func(t *testing.T) {
		var hits atomic.Int64
		upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			hits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"must-not-run"}`))
		}))
		defer upstream.Close()

		env := setupProxyTestEnv(t, []testChannel{
			{name: "model-restricted", models: "allowed-model,blocked-model"},
		}, map[int]string{0: upstream.URL})
		webSession := createDashboardSession(t, env, "sk-dashboard-model-owner", &model.AuthToken{
			Description:   "model restricted",
			AllowedModels: []string{"allowed-model"},
		})

		w := doProxyRequest(t, env.engine, "/dashboard/v1/chat/completions", map[string]any{
			"model":    "blocked-model",
			"messages": []map[string]string{{"role": "user", "content": "hi"}},
		}, map[string]string{"Authorization": "Bearer " + webSession})
		if w.Code != http.StatusForbidden {
			t.Fatalf("status=%d, want 403: %s", w.Code, w.Body.String())
		}
		if hits.Load() != 0 {
			t.Fatalf("disallowed model reached upstream %d times", hits.Load())
		}
	})

	t.Run("channel", func(t *testing.T) {
		var blockedHits atomic.Int64
		blocked := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			blockedHits.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer blocked.Close()
		var allowedHits atomic.Int64
		allowed := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			allowedHits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"allowed-channel","choices":[{"message":{"content":"ok"}}]}`))
		}))
		defer allowed.Close()

		env := setupProxyTestEnv(t, []testChannel{
			{name: "blocked-channel", models: "gpt-dashboard", priority: 100},
			{name: "allowed-channel", models: "gpt-dashboard", priority: 90},
		}, map[int]string{0: blocked.URL, 1: allowed.URL})
		configs, err := env.store.ListConfigs(context.Background())
		if err != nil {
			t.Fatalf("ListConfigs failed: %v", err)
		}
		var allowedChannelID int64
		for _, cfg := range configs {
			if cfg.Name == "allowed-channel" {
				allowedChannelID = cfg.ID
			}
		}
		if allowedChannelID == 0 {
			t.Fatal("allowed channel not found")
		}
		webSession := createDashboardSession(t, env, "sk-dashboard-channel-owner", &model.AuthToken{
			Description:       "channel restricted",
			AllowedModels:     []string{"gpt-dashboard"},
			AllowedChannelIDs: []int64{allowedChannelID},
		})

		w := doProxyRequest(t, env.engine, "/dashboard/v1/chat/completions", map[string]any{
			"model":    "gpt-dashboard",
			"messages": []map[string]string{{"role": "user", "content": "hi"}},
		}, map[string]string{"Authorization": "Bearer " + webSession})
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d, want 200: %s", w.Code, w.Body.String())
		}
		if blockedHits.Load() != 0 || allowedHits.Load() != 1 {
			t.Fatalf("upstream hits blocked=%d allowed=%d, want 0/1", blockedHits.Load(), allowedHits.Load())
		}
	})
}

func TestDashboardProxy_RejectsRevokedToken(t *testing.T) {
	t.Parallel()
	var hits atomic.Int64
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "revoked-dashboard", models: "gpt-dashboard"},
	}, map[int]string{0: upstream.URL})
	authToken := &model.AuthToken{Description: "revoked dashboard owner"}
	webSession := createDashboardSession(t, env, "sk-dashboard-revoked-owner", authToken)
	authToken.IsActive = false
	if err := env.store.UpdateAuthToken(context.Background(), authToken); err != nil {
		t.Fatalf("UpdateAuthToken failed: %v", err)
	}
	if err := env.server.authService.ReloadAuthTokens(); err != nil {
		t.Fatalf("ReloadAuthTokens failed: %v", err)
	}

	w := doProxyRequest(t, env.engine, "/dashboard/v1/chat/completions", map[string]any{
		"model":    "gpt-dashboard",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, map[string]string{"Authorization": "Bearer " + webSession})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d, want 401: %s", w.Code, w.Body.String())
	}
	if hits.Load() != 0 {
		t.Fatalf("revoked dashboard session reached upstream %d times", hits.Load())
	}
}

func TestProxy_NoAvailableUpstreamLogKeepsAuthTokenID(t *testing.T) {
	t.Parallel()

	srv := newInMemoryServer(t)
	injectAPIToken(srv.authService, "test-api-key", 0, 77)

	engine := gin.New()
	srv.SetupRoutes(engine)
	env := &proxyTestEnv{server: srv, store: srv.store, engine: engine}

	w := doProxyRequest(t, engine, "/v1/chat/completions", map[string]any{
		"model":    "no-upstream-model",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", w.Code, w.Body.String())
	}

	entry := waitForProxyLog(t, env, "no-upstream-model")
	if entry.AuthTokenID != 77 {
		t.Fatalf("AuthTokenID=%d, want 77", entry.AuthTokenID)
	}
}

func TestProxy_LogsAnthropicBudgetAsThinkingEffort(t *testing.T) {
	t.Parallel()

	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"mimo-v2.5","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5}}`))
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "fufu-thinking", models: "mimo-v2.5", apiKey: "sk-fufu-thinking", upstreamProtocol: util.ProtocolAnthropic},
	}, map[int]string{0: upstream.URL})

	w := doProxyRequest(t, env.engine, "/v1/messages", map[string]any{
		"model":      "mimo-v2.5",
		"max_tokens": 32000,
		"thinking": map[string]any{
			"type":          "enabled",
			"budget_tokens": 31999,
			"display":       "summarized",
		},
		"messages": []map[string]any{{
			"role": "user",
			"content": []map[string]string{{
				"type": "text",
				"text": "hi",
			}},
		}},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	entry := waitForProxyLog(t, env, "mimo-v2.5")
	if entry.ThinkingEffort != "high" {
		t.Fatalf("ThinkingEffort=%q, want high", entry.ThinkingEffort)
	}
}

func TestProxy_LogsThinkingEffortFromRequestAndJSONResponseOverride(t *testing.T) {
	t.Parallel()

	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","thinking":{"level":"high"},"choices":[{"message":{"content":"hello"}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "json-thinking", models: "gpt-4", apiKey: "sk-json-thinking"},
	}, map[int]string{0: upstream.URL})

	w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model":            "gpt-4",
		"reasoning_effort": "low",
		"messages":         []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	entry := waitForProxyLog(t, env, "gpt-4")
	if entry.ThinkingEffort != "high" {
		t.Fatalf("ThinkingEffort=%q, want high", entry.ThinkingEffort)
	}
}

func TestProxy_LogsThinkingEffortFromRequestAndSSEOverride(t *testing.T) {
	t.Parallel()

	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		chunks := []string{
			`event: response.created` + "\n" + `data: {"type":"response.created","response":{"reasoning":{"effort":"medium"}}}`,
			`event: response.completed` + "\n" + `data: {"type":"response.completed","response":{"usage":{"input_tokens":10,"output_tokens":5}}}`,
		}
		for _, chunk := range chunks {
			_, _ = fmt.Fprintf(w, "%s\n\n", chunk)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "sse-thinking", models: "gpt-5-codex", apiKey: "sk-sse-thinking", upstreamProtocol: util.ProtocolCodex},
	}, map[int]string{0: upstream.URL})

	w := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
		"model":     "gpt-5-codex",
		"stream":    true,
		"reasoning": map[string]any{"effort": "high"},
		"input":     "hi",
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	entry := waitForProxyLog(t, env, "gpt-5-codex")
	if entry.ThinkingEffort != "medium" {
		t.Fatalf("ThinkingEffort=%q, want medium", entry.ThinkingEffort)
	}
}

func TestProxy_CodexPriorityRequestBillsFastModeWithoutResponseTier(t *testing.T) {
	t.Parallel()

	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "event: response.completed\n"+
			`data: {"type":"response.completed","response":{"id":"resp-fast","status":"completed","model":"gpt-5.6","output":[],"usage":{"input_tokens":1000,"output_tokens":1000,"total_tokens":2000}}}`+"\n\n")
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "codex-fast", models: "gpt-5.6", upstreamProtocol: util.ProtocolCodex},
	}, map[int]string{0: upstream.URL})

	w := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
		"model":        "gpt-5.6",
		"stream":       true,
		"service_tier": "priority",
		"input":        "hi",
	}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200: %s", w.Code, w.Body.String())
	}

	entry := waitForProxyLog(t, env, "gpt-5.6")
	if entry.ServiceTier != "priority" {
		t.Fatalf("ServiceTier=%q, want priority", entry.ServiceTier)
	}
	wantCost := util.CalculateCostDetailed("gpt-5.6", 1000, 1000, 0, 0, 0) * 2
	if !floatEquals(entry.Cost, wantCost) {
		t.Fatalf("Cost=%v, want fast-mode cost %v", entry.Cost, wantCost)
	}
}

func TestProxy_CodexPriorityRequestBillsFastModeDespiteUpstreamDefaultTier(t *testing.T) {
	t.Parallel()

	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "event: response.completed\n"+
			`data: {"type":"response.completed","response":{"id":"resp-standard","status":"completed","model":"gpt-5.6","service_tier":"default","output":[],"usage":{"input_tokens":1000,"output_tokens":1000,"total_tokens":2000}}}`+"\n\n")
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "codex-standard", models: "gpt-5.6", upstreamProtocol: util.ProtocolCodex},
	}, map[int]string{0: upstream.URL})

	w := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
		"model":        "gpt-5.6",
		"stream":       true,
		"service_tier": "priority",
		"input":        "hi",
	}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200: %s", w.Code, w.Body.String())
	}

	entry := waitForProxyLog(t, env, "gpt-5.6")
	if entry.ServiceTier != "priority" {
		t.Fatalf("ServiceTier=%q, want priority", entry.ServiceTier)
	}
	wantCost := util.CalculateCostDetailed("gpt-5.6", 1000, 1000, 0, 0, 0) * 2
	if !floatEquals(entry.Cost, wantCost) {
		t.Fatalf("Cost=%v, want fast-mode cost %v", entry.Cost, wantCost)
	}
}

func TestProxy_CodexAutoResponseChargesFastMode(t *testing.T) {
	t.Parallel()

	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "event: response.completed\n"+
			`data: {"type":"response.completed","response":{"id":"resp-auto","status":"completed","model":"gpt-5.6-sol","service_tier":"auto","output":[],"usage":{"input_tokens":1000,"output_tokens":1000,"total_tokens":2000}}}`+"\n\n")
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "codex-auto", models: "gpt-5.6-sol", upstreamProtocol: util.ProtocolCodex},
	}, map[int]string{0: upstream.URL})

	w := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
		"model":        "gpt-5.6-sol",
		"stream":       true,
		"service_tier": "priority",
		"input":        "hi",
	}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200: %s", w.Code, w.Body.String())
	}

	entry := waitForProxyLog(t, env, "gpt-5.6-sol")
	if entry.ServiceTier != "auto" {
		t.Fatalf("ServiceTier=%q, want auto", entry.ServiceTier)
	}
	wantCost := util.CalculateCostDetailed("gpt-5.6-sol", 1000, 1000, 0, 0, 0) * 2
	if !floatEquals(entry.Cost, wantCost) {
		t.Fatalf("Cost=%v, want auto fast-mode cost %v", entry.Cost, wantCost)
	}
}

func TestProxy_CodexUltrafastResponseChargesSixfold(t *testing.T) {
	t.Parallel()

	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "event: response.completed\n"+
			`data: {"type":"response.completed","response":{"id":"resp-ultrafast","status":"completed","model":"gpt-5.6","service_tier":"ultrafast","output":[],"usage":{"input_tokens":1000,"output_tokens":1000,"total_tokens":2000}}}`+"\n\n")
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "codex-ultrafast", models: "gpt-5.6", upstreamProtocol: util.ProtocolCodex},
	}, map[int]string{0: upstream.URL})

	w := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
		"model":        "gpt-5.6",
		"stream":       true,
		"service_tier": "priority",
		"input":        "hi",
	}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200: %s", w.Code, w.Body.String())
	}

	entry := waitForProxyLog(t, env, "gpt-5.6")
	if entry.ServiceTier != "ultrafast" {
		t.Fatalf("ServiceTier=%q, want ultrafast", entry.ServiceTier)
	}
	wantCost := util.CalculateCostDetailed("gpt-5.6", 1000, 1000, 0, 0, 0) * 6
	if !floatEquals(entry.Cost, wantCost) {
		t.Fatalf("Cost=%v, want ultrafast cost %v", entry.Cost, wantCost)
	}
}

func waitForProxyLog(t testing.TB, env *proxyTestEnv, modelName string) *model.LogEntry {
	t.Helper()

	ctx := context.Background()
	since := time.Now().Add(-time.Minute)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		logs, err := env.store.ListLogs(ctx, since, 20, 0, &model.LogFilter{LogSource: model.LogSourceProxy})
		if err != nil {
			t.Fatalf("ListLogs failed: %v", err)
		}
		for _, entry := range logs {
			if entry.Model == modelName {
				return entry
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("proxy log for model %q not found within deadline", modelName)
	return nil
}

// waitForCountTokensLogs 等待 count_tokens 来源的日志条数达到 want，按时间倒序返回。
func waitForCountTokensLogs(t testing.TB, env *proxyTestEnv, want int) []*model.LogEntry {
	t.Helper()

	ctx := context.Background()
	since := time.Now().Add(-time.Minute)
	var logs []*model.LogEntry
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		var err error
		logs, err = env.store.ListLogs(ctx, since, 20, 0, &model.LogFilter{LogSource: model.LogSourceCountTokens})
		if err != nil {
			t.Fatalf("ListLogs failed: %v", err)
		}
		if len(logs) >= want {
			break
		}
	}
	if len(logs) != want {
		t.Fatalf("count_tokens logs=%d, want %d: %+v", len(logs), want, logs)
	}
	return logs
}

func waitForProxyLogMatching(t testing.TB, env *proxyTestEnv, match func(*model.LogEntry) bool) *model.LogEntry {
	t.Helper()

	ctx := context.Background()
	since := time.Now().Add(-time.Minute)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		logs, err := env.store.ListLogs(ctx, since, 50, 0, &model.LogFilter{LogSource: model.LogSourceProxy})
		if err != nil {
			t.Fatalf("ListLogs failed: %v", err)
		}
		for _, entry := range logs {
			if match(entry) {
				return entry
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return nil
}

func TestProxy_SkipsChannelAfterRPMLimitExceeded(t *testing.T) {
	t.Parallel()

	var firstHits atomic.Int64
	firstUpstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firstHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"from-limited","choices":[{"message":{"content":"limited"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer firstUpstream.Close()

	var fallbackHits atomic.Int64
	fallbackUpstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"from-fallback","choices":[{"message":{"content":"fallback"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer fallbackUpstream.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "limited", models: "gpt-4", apiKey: "sk-limited", priority: 100},
		{name: "fallback", models: "gpt-4", apiKey: "sk-fallback", priority: 90},
	}, map[int]string{0: firstUpstream.URL, 1: fallbackUpstream.URL})

	ctx := context.Background()
	cfgs, err := env.store.ListConfigs(ctx)
	if err != nil {
		t.Fatalf("ListConfigs failed: %v", err)
	}
	for _, cfg := range cfgs {
		if cfg.Name != "limited" {
			continue
		}
		cfg.RPMLimit = 1
		if _, err := env.store.UpdateConfig(ctx, cfg.ID, cfg); err != nil {
			t.Fatalf("UpdateConfig failed: %v", err)
		}
	}
	env.server.InvalidateChannelListCache()

	requestBody := map[string]any{
		"model":    "gpt-4",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}

	first := doProxyRequest(t, env.engine, "/v1/chat/completions", requestBody, nil)
	if first.Code != http.StatusOK {
		t.Fatalf("first request status=%d body=%s", first.Code, first.Body.String())
	}

	second := doProxyRequest(t, env.engine, "/v1/chat/completions", requestBody, nil)
	if second.Code != http.StatusOK {
		t.Fatalf("second request status=%d body=%s", second.Code, second.Body.String())
	}
	if firstHits.Load() != 1 {
		t.Fatalf("limited upstream hits=%d, want 1", firstHits.Load())
	}
	if fallbackHits.Load() != 1 {
		t.Fatalf("fallback upstream hits=%d, want 1", fallbackHits.Load())
	}
	if !strings.Contains(second.Body.String(), "from-fallback") {
		t.Fatalf("second response should come from fallback, got %s", second.Body.String())
	}
}

func TestProxy_SkipsChannelAfterConcurrencyLimitExceeded(t *testing.T) {
	t.Parallel()

	var limitedHits atomic.Int64
	limitedUpstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		limitedHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"from-limited","choices":[{"message":{"content":"limited"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer limitedUpstream.Close()

	var fallbackHits atomic.Int64
	fallbackUpstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"from-fallback","choices":[{"message":{"content":"fallback"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer fallbackUpstream.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "limited", models: "gpt-4", apiKey: "sk-limited", priority: 100},
		{name: "fallback", models: "gpt-4", apiKey: "sk-fallback", priority: 90},
	}, map[int]string{0: limitedUpstream.URL, 1: fallbackUpstream.URL})

	ctx := context.Background()
	cfgs, err := env.store.ListConfigs(ctx)
	if err != nil {
		t.Fatalf("ListConfigs failed: %v", err)
	}
	var limitedID int64
	for _, cfg := range cfgs {
		if cfg.Name != "limited" {
			continue
		}
		cfg.MaxConcurrency = 1
		limitedID = cfg.ID
		if _, err := env.store.UpdateConfig(ctx, cfg.ID, cfg); err != nil {
			t.Fatalf("UpdateConfig failed: %v", err)
		}
	}
	if limitedID == 0 {
		t.Fatal("limited channel not found")
	}
	env.server.InvalidateChannelListCache()

	release, _, _, ok := env.server.channelConcurrencyLimiter.acquire(limitedID, 1)
	if !ok {
		t.Fatal("pre-acquire limited channel slot failed")
	}
	defer release()

	w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model":    "gpt-4",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("request status=%d body=%s", w.Code, w.Body.String())
	}
	if limitedHits.Load() != 0 {
		t.Fatalf("limited upstream hits=%d, want 0", limitedHits.Load())
	}
	if fallbackHits.Load() != 1 {
		t.Fatalf("fallback upstream hits=%d, want 1", fallbackHits.Load())
	}
	if !strings.Contains(w.Body.String(), "from-fallback") {
		t.Fatalf("response should come from fallback, got %s", w.Body.String())
	}
}

func TestProxy_AllCooledFallback_UsesCooledKey(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if got := r.Header.Get("Authorization"); got != "Bearer sk-cooled" {
			t.Fatalf("expected cooled key to be used, got %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"chatcmpl-fallback","choices":[{"message":{"content":"fallback"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "cooled-key-channel", models: "gpt-4", apiKey: "sk-cooled"},
	}, map[int]string{0: upstream.URL})

	ctx := context.Background()
	configs, err := env.store.ListConfigs(ctx)
	if err != nil {
		t.Fatalf("ListConfigs failed: %v", err)
	}
	if len(configs) != 1 {
		t.Fatalf("expected 1 config, got %d", len(configs))
	}
	if err := env.store.SetKeyCooldown(ctx, configs[0].ID, 0, time.Now().Add(time.Minute)); err != nil {
		t.Fatalf("SetKeyCooldown failed: %v", err)
	}
	env.server.invalidateChannelRelatedCache(configs[0].ID)

	w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model":    "gpt-4",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 from all-cooled fallback, got %d: %s", w.Code, w.Body.String())
	}
	if calls.Load() != 1 {
		t.Fatalf("expected upstream to be called once, got %d", calls.Load())
	}
}

func TestProxy_DebugLogDistinguishesTransportErrorFromEmptyHTTPResponse(t *testing.T) {
	t.Parallel()

	t.Run("transport error before response", func(t *testing.T) {
		t.Parallel()

		env := setupProxyTestEnv(t, []testChannel{
			{name: "transport-debug", upstreamProtocol: "openai", models: "gpt-transport-debug", apiKey: "sk-test"},
		}, map[int]string{0: "https://transport-error.example.com"})
		env.server.client = &http.Client{Transport: roundTripperFunc(func(_ *http.Request) (*http.Response, error) {
			return nil, io.ErrUnexpectedEOF
		})}
		env.server.configService.mu.Lock()
		env.server.configService.cache["debug_log_enabled"] = &model.SystemSetting{Key: "debug_log_enabled", Value: "true"}
		env.server.configService.mu.Unlock()

		w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
			"model":    "gpt-transport-debug",
			"messages": []map[string]string{{"role": "user", "content": "hi"}},
		}, nil)
		if w.Code != http.StatusBadGateway {
			t.Fatalf("status=%d, want 502; body=%s", w.Code, w.Body.String())
		}

		entry := waitForProxyLog(t, env, "gpt-transport-debug")
		if !strings.Contains(entry.Message, "before HTTP response (no response body)") ||
			!strings.Contains(entry.Message, "unexpected EOF") {
			t.Fatalf("log message=%q", entry.Message)
		}
		debugLog, err := env.store.GetDebugLogByLogID(context.Background(), entry.ID)
		if err != nil || debugLog == nil {
			t.Fatalf("GetDebugLogByLogID failed: debug=%+v err=%v", debugLog, err)
		}
		if debugLog.RespStatus != 0 || len(debugLog.RespBody) != 0 {
			t.Fatalf("transport error invented HTTP response: status=%d body=%q", debugLog.RespStatus, debugLog.RespBody)
		}
		if !strings.Contains(debugLog.UpstreamError, "unexpected EOF") {
			t.Fatalf("debug upstream error=%q", debugLog.UpstreamError)
		}
	})

	t.Run("empty HTTP response", func(t *testing.T) {
		t.Parallel()

		env := setupProxyTestEnv(t, []testChannel{
			{name: "empty-response-debug", upstreamProtocol: "openai", models: "gpt-empty-response", apiKey: "sk-test"},
		}, map[int]string{0: "https://empty-response.example.com"})
		env.server.client = &http.Client{Transport: roundTripperFunc(func(_ *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader("")),
			}, nil
		})}
		env.server.configService.mu.Lock()
		env.server.configService.cache["debug_log_enabled"] = &model.SystemSetting{Key: "debug_log_enabled", Value: "true"}
		env.server.configService.mu.Unlock()

		w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
			"model":    "gpt-empty-response",
			"messages": []map[string]string{{"role": "user", "content": "hi"}},
		}, nil)
		if w.Code != http.StatusBadGateway {
			t.Fatalf("status=%d, want 502; body=%s", w.Code, w.Body.String())
		}

		entry := waitForProxyLog(t, env, "gpt-empty-response")
		if strings.Contains(entry.Message, "before HTTP response") || !strings.Contains(entry.Message, "200 OK") {
			t.Fatalf("empty HTTP response log message=%q", entry.Message)
		}
		debugLog, err := env.store.GetDebugLogByLogID(context.Background(), entry.ID)
		if err != nil || debugLog == nil {
			t.Fatalf("GetDebugLogByLogID failed: debug=%+v err=%v", debugLog, err)
		}
		if debugLog.RespStatus != http.StatusOK {
			t.Fatalf("debug response status=%d, want 200", debugLog.RespStatus)
		}
	})
}

func TestProxy_Success_Streaming(t *testing.T) {
	t.Parallel()

	// 模拟上游：返回 200 + SSE 流
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		chunks := []string{
			`data: {"choices":[{"delta":{"content":"Hello"}}]}`,
			`data: {"choices":[{"delta":{"content":" World"}}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`,
			`data: [DONE]`,
		}
		for _, chunk := range chunks {
			_, _ = fmt.Fprintf(w, "%s\n\n", chunk)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "ch1", models: "gpt-4", apiKey: "sk-1"},
	}, map[int]string{0: upstream.URL})

	w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model":    "gpt-4",
		"stream":   true,
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// 验证 SSE 内容被透传
	body := w.Body.String()
	if !strings.Contains(body, "Hello") {
		t.Fatalf("expected SSE to contain 'Hello', body: %s", body)
	}
	if !strings.Contains(body, "[DONE]") {
		t.Fatalf("expected SSE to contain '[DONE]', body: %s", body)
	}
}

func TestProxy_URLProtocolsSkipIncompatibleEndpointWithoutCoolingChannel(t *testing.T) {
	t.Parallel()
	var incompatibleHits atomic.Int64
	incompatible := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		incompatibleHits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer incompatible.Close()

	var compatibleHits atomic.Int64
	compatible := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		compatibleHits.Add(1)
		if r.URL.Path != "/v1/embeddings" {
			t.Errorf("compatible path=%q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"object":"list","data":[{"object":"embedding","embedding":[0.1],"index":0}],"model":"shared-model","usage":{"prompt_tokens":1,"total_tokens":1}}`)
	}))
	defer compatible.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "incompatible", upstreamProtocol: "anthropic", models: "shared-model", priority: 100},
		{name: "compatible", upstreamProtocol: "openai", models: "shared-model", priority: 90},
	}, map[int]string{0: incompatible.URL, 1: compatible.URL})
	configs, err := env.store.ListConfigs(context.Background())
	if err != nil || len(configs) != 2 {
		t.Fatalf("ListConfigs: configs=%d err=%v", len(configs), err)
	}
	for _, cfg := range configs {
		switch cfg.Name {
		case "incompatible":
			cfg.URLs[0].Protocols = []string{"codex"}
		case "compatible":
			cfg.URLs[0].Protocols = []string{"openai"}
		}
		if _, err := env.store.UpdateConfig(context.Background(), cfg.ID, cfg); err != nil {
			t.Fatalf("UpdateConfig(%s): %v", cfg.Name, err)
		}
	}
	env.server.InvalidateChannelListCache()

	w := doProxyRequest(t, env.engine, "/v1/embeddings", map[string]any{
		"model": "shared-model", "input": "hi",
	}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if incompatibleHits.Load() != 0 || compatibleHits.Load() != 1 {
		t.Fatalf("hits incompatible=%d compatible=%d, want 0/1", incompatibleHits.Load(), compatibleHits.Load())
	}
	cooldowns, err := env.store.GetAllChannelCooldowns(context.Background())
	if err != nil {
		t.Fatalf("GetAllChannelCooldowns: %v", err)
	}
	for _, cfg := range configs {
		if cfg.Name == "incompatible" && cooldowns[cfg.ID].After(time.Now()) {
			t.Fatalf("incompatible declared URL cooled channel until %v", cooldowns[cfg.ID])
		}
	}
}

func TestProxy_OfficialCodexClientPrefersDeclaredNativeCodex(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name           string
		protocols      []string
		userAgent      string
		wantPath       string
		wantNativeBody bool
	}{
		{
			name:           "official client prefers codex when declared",
			protocols:      []string{"openai", "codex"},
			userAgent:      "codex_cli_rs/0.153.4",
			wantPath:       "/v1/responses",
			wantNativeBody: true,
		},
		{
			name:      "official client does not invent codex capability",
			protocols: []string{"openai"},
			userAgent: "codex_cli_rs/0.153.4",
			wantPath:  "/v1/responses",
		},
		{
			name:      "non official client keeps declaration order",
			protocols: []string{"openai", "codex"},
			wantPath:  "/v1/responses",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotPath string
			var gotBody []byte
			upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				gotBody, _ = io.ReadAll(r.Body)
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/v1/responses":
					_, _ = io.WriteString(w, `{"id":"resp-native","object":"response","status":"completed","model":"gpt-test","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)
				case "/v1/chat/completions":
					_, _ = io.WriteString(w, `{"id":"chatcmpl-translated","object":"chat.completion","model":"gpt-test","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer upstream.Close()

			env := setupProxyTestEnv(t, []testChannel{{
				name: "official-codex-preference", upstreamProtocol: "openai",
				protocolTransformMode: model.ProtocolTransformModeLocal, models: "gpt-test",
			}}, map[int]string{0: upstream.URL})
			configs, err := env.store.ListConfigs(context.Background())
			if err != nil || len(configs) != 1 {
				t.Fatalf("ListConfigs: configs=%d err=%v", len(configs), err)
			}
			configs[0].URLs[0].Protocols = tt.protocols
			if _, err := env.store.UpdateConfig(context.Background(), configs[0].ID, configs[0]); err != nil {
				t.Fatalf("UpdateConfig: %v", err)
			}
			env.server.InvalidateChannelListCache()

			headers := map[string]string{}
			if tt.userAgent != "" {
				headers["User-Agent"] = tt.userAgent
			}
			response := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
				"model":        "gpt-test",
				"instructions": "preserve this",
				"input": []any{map[string]any{
					"type": "message", "role": "user",
					"content": []any{map[string]any{"type": "input_text", "text": "hello"}},
				}},
				"stream": false,
			}, headers)
			if len(tt.protocols) == 1 {
				if response.Code != http.StatusServiceUnavailable || gotPath != "" {
					t.Fatalf("Chat-only channel admitted Responses: %d path=%s", response.Code, gotPath)
				}
				return
			}
			if response.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s path=%s", response.Code, response.Body.String(), gotPath)
			}
			if gotPath != tt.wantPath {
				t.Fatalf("upstream path=%q, want %q", gotPath, tt.wantPath)
			}
			if tt.wantNativeBody {
				if !gjson.GetBytes(gotBody, "input.0.content.0.text").Exists() || gjson.GetBytes(gotBody, "messages").Exists() {
					t.Fatalf("native Codex body was translated: %s", gotBody)
				}
			} else if tt.wantPath == "/v1/chat/completions" && !gjson.GetBytes(gotBody, "messages").Exists() {
				t.Fatalf("OpenAI fallback body was not translated: %s", gotBody)
			}
		})
	}
}

func TestProxy_AutoModePrioritizesAutomaticURLBeforeDeclaredConversion(t *testing.T) {
	t.Parallel()
	var automaticPathsMu sync.Mutex
	var automaticPaths []string
	automatic := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		automaticPathsMu.Lock()
		automaticPaths = append(automaticPaths, r.URL.Path)
		automaticPathsMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/v1/chat/completions" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"message":"endpoint not found"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"chatcmpl_auto","object":"chat.completion","model":"shared-model","choices":[{"index":0,"message":{"role":"assistant","content":"direct"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer automatic.Close()

	var declaredHits atomic.Int64
	declared := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		declaredHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"converted"}],"model":"shared-model","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer declared.Close()

	env := setupProxyTestEnv(t, []testChannel{{
		name: "auto-original-protocol-first", upstreamProtocol: util.ProtocolAnthropic,
		protocolTransformMode: model.ProtocolTransformModeAuto, models: "shared-model",
	}}, map[int]string{0: automatic.URL})
	configs, err := env.store.ListConfigs(context.Background())
	if err != nil || len(configs) != 1 {
		t.Fatalf("ListConfigs: configs=%d err=%v", len(configs), err)
	}
	configs[0].URLs = model.ChannelURLs{
		{URL: declared.URL, Protocols: []string{"anthropic"}},
		{URL: automatic.URL},
	}
	if _, err := env.store.UpdateConfig(context.Background(), configs[0].ID, configs[0]); err != nil {
		t.Fatalf("UpdateConfig: %v", err)
	}
	env.server.InvalidateChannelListCache()
	// 固定配置顺序，证明 auto 模式会主动把自动检测 URL 提到转换 URL 之前。
	env.server.urlSelector = nil

	w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model": "shared-model", "messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	automaticPathsMu.Lock()
	gotAutomaticPaths := append([]string(nil), automaticPaths...)
	automaticPathsMu.Unlock()
	if !slices.Equal(gotAutomaticPaths, []string{"/v1/chat/completions"}) {
		t.Fatalf("automatic paths=%v, want original client protocol first", gotAutomaticPaths)
	}
	if got := declaredHits.Load(); got != 0 {
		t.Fatalf("declared conversion URL hits=%d, want 0 when automatic URL accepts original protocol", got)
	}
}

func TestProxy_AutomaticProtocolFallback_DoesNotTranslateOrdinaryErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		status     int
		body       string
		wantStatus int
	}{
		{name: "model 404", status: http.StatusNotFound, body: `{"error":{"message":"model claude-3-5-sonnet not found","type":"invalid_request_error","code":"model_not_found"}}`, wantStatus: http.StatusNotFound},
		{name: "unauthorized", status: http.StatusUnauthorized, body: `{"error":{"message":"unauthorized"}}`, wantStatus: http.StatusUnauthorized},
		{name: "forbidden", status: http.StatusForbidden, body: `{"error":{"message":"forbidden"}}`, wantStatus: http.StatusForbidden},
		{name: "rate limited", status: http.StatusTooManyRequests, body: `{"error":{"message":"rate limited"}}`, wantStatus: http.StatusTooManyRequests},
		{name: "server error", status: http.StatusInternalServerError, body: `{"error":{"message":"upstream failed"}}`, wantStatus: http.StatusInternalServerError},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var paths []string
			upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				paths = append(paths, r.URL.Path)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer upstream.Close()
			env := setupProxyTestEnv(t, []testChannel{{
				name: "error-anthropic", upstreamProtocol: "anthropic",
				protocolTransformMode: model.ProtocolTransformModeAuto, models: "claude-3-5-sonnet",
			}}, map[int]string{0: upstream.URL})

			w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
				"model": "claude-3-5-sonnet", "messages": []map[string]string{{"role": "user", "content": "hi"}},
			}, nil)
			if w.Code != tc.wantStatus {
				t.Fatalf("status=%d want=%d body=%s", w.Code, tc.wantStatus, w.Body.String())
			}
			if got := strings.Join(paths, ","); got != "/v1/chat/completions" {
				t.Fatalf("upstream paths=%s, ordinary error must stop protocol fallback", got)
			}
		})
	}
}

func TestProxy_CodexMap429To503Setting(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		clientProtocol   string
		upstreamProtocol string
		path             string
		body             map[string]any
		headers          map[string]string
		settings         map[string]string
		wantStatus       int
	}{
		{
			name: "disabled by default for Codex", clientProtocol: util.ProtocolCodex,
			upstreamProtocol: util.ProtocolCodex, path: "/v1/responses",
			body:       map[string]any{"model": "gpt-test", "input": "hello", "stream": false},
			headers:    map[string]string{"User-Agent": "codex_cli_rs/0.147.0"},
			wantStatus: http.StatusTooManyRequests,
		},
		{
			name: "enabled for Codex", clientProtocol: util.ProtocolCodex,
			upstreamProtocol: util.ProtocolCodex, path: "/v1/responses",
			body:       map[string]any{"model": "gpt-test", "input": "hello", "stream": false},
			headers:    map[string]string{"User-Agent": "codex_cli_rs/0.147.0"},
			settings:   map[string]string{config.CodexMap429To503SettingKey: "true"},
			wantStatus: http.StatusServiceUnavailable,
		},
		{
			name: "enabled does not affect other Responses clients", clientProtocol: util.ProtocolCodex,
			upstreamProtocol: util.ProtocolCodex, path: "/v1/responses",
			body:       map[string]any{"model": "gpt-test", "input": "hello", "stream": false},
			headers:    map[string]string{"User-Agent": "openai-python/2.21.0"},
			settings:   map[string]string{config.CodexMap429To503SettingKey: "true"},
			wantStatus: http.StatusTooManyRequests,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upstreamBody := `{"error":{"type":"rate_limit_error","message":"retry later"}}`
			upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Retry-After", "60")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = io.WriteString(w, upstreamBody)
			}))
			defer upstream.Close()

			env := setupProxyTestEnvWithSettings(t, []testChannel{{
				name: "final-429", upstreamProtocol: tt.upstreamProtocol, models: "gpt-test",
			}}, map[int]string{0: upstream.URL}, tt.settings)
			response := doProxyRequest(t, env.engine, tt.path, tt.body, tt.headers)

			if response.Code != tt.wantStatus {
				t.Fatalf("client protocol %s status=%d, want %d; body=%s",
					tt.clientProtocol, response.Code, tt.wantStatus, response.Body.String())
			}
			if response.Header().Get("Retry-After") != "60" || response.Body.String() != upstreamBody {
				t.Fatalf("upstream response changed: headers=%v body=%s", response.Header(), response.Body.String())
			}
		})
	}

	t.Run("summary log preserves upstream 429", func(t *testing.T) {
		upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"message":"retry later"}}`)
		}))
		defer upstream.Close()

		env := setupProxyTestEnvWithSettings(t, []testChannel{
			{name: "first-429", upstreamProtocol: util.ProtocolCodex, models: "gpt-log-test", priority: 100},
			{name: "second-429", upstreamProtocol: util.ProtocolCodex, models: "gpt-log-test", priority: 90},
		}, map[int]string{0: upstream.URL, 1: upstream.URL}, map[string]string{
			config.CodexMap429To503SettingKey: "true",
		})
		response := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
			"model": "gpt-log-test", "input": "hello", "stream": false,
		}, map[string]string{"User-Agent": "codex_cli_rs/0.147.0"})
		if response.Code != http.StatusServiceUnavailable {
			t.Fatalf("status=%d, want 503; body=%s", response.Code, response.Body.String())
		}

		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			logs, err := env.store.ListLogs(
				context.Background(), time.Now().Add(-time.Minute), 20, 0,
				&model.LogFilter{LogSource: model.LogSourceProxy},
			)
			if err != nil {
				t.Fatalf("ListLogs failed: %v", err)
			}
			for _, entry := range logs {
				if entry.Model == "gpt-log-test" && entry.ChannelID == 0 {
					if entry.StatusCode != http.StatusTooManyRequests {
						t.Fatalf("summary log status=%d, want upstream 429", entry.StatusCode)
					}
					return
				}
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatal("proxy summary log not found within deadline")
	})
}

// 一个请求无法转换到已学协议时可以回退，但回退结果只说明这一个请求，
// 不能覆盖同一渠道/模型的学习结果。

func TestProxy_AutomaticProtocolFallback_SkipsUnrepresentableTransforms(t *testing.T) {
	t.Parallel()
	var paths []string
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_1","object":"response","status":"completed","model":"gpt-5.6-sol","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{{
		name: "codex-unrepresentable-transform", upstreamProtocol: "codex",
		protocolTransformMode: model.ProtocolTransformModeAuto, models: "gpt-5.6-sol",
	}}, map[int]string{0: upstream.URL})

	w := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
		"model": "gpt-5.6-sol",
		"input": []map[string]any{{
			"type":              "compaction",
			"encrypted_content": "opaque",
		}},
	}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if !slices.Equal(paths, []string{"/v1/responses"}) {
		t.Fatalf("paths=%v, want native Codex after local transform rejection", paths)
	}
}

func TestProxy_LocalMode_SkipsUnrepresentableTransformsAndRetriesNextProtocol(t *testing.T) {
	t.Parallel()
	var paths []string
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/v1/responses" {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":{"message":"unexpected protocol"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"resp_1","object":"response","status":"completed","model":"gpt-5.6-sol","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{{
		name: "local-compaction-protocol-skip", upstreamProtocol: "anthropic",
		protocolTransformMode: model.ProtocolTransformModeLocal, models: "gpt-5.6-sol",
	}}, map[int]string{0: upstream.URL})
	configs, err := env.store.ListConfigs(context.Background())
	if err != nil || len(configs) != 1 {
		t.Fatalf("ListConfigs: configs=%d err=%v", len(configs), err)
	}
	configs[0].URLs[0].Protocols = []string{"anthropic", "codex"}
	if _, err := env.store.UpdateConfig(context.Background(), configs[0].ID, configs[0]); err != nil {
		t.Fatalf("UpdateConfig: %v", err)
	}
	env.server.InvalidateChannelListCache()

	w := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
		"model": "gpt-5.6-sol",
		"input": []map[string]any{{
			"type":              "compaction",
			"encrypted_content": "opaque",
		}},
	}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if !slices.Equal(paths, []string{"/v1/responses"}) {
		t.Fatalf("paths=%v, want native Codex after local Anthropic transform rejection", paths)
	}
}

func TestProxy_LocalMode_SkipsUnrepresentableTransformsAndRetriesNextChannel(t *testing.T) {
	t.Parallel()
	var anthropicHits, codexHits int
	anthropicUpstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		anthropicHits++
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":{"message":"anthropic should not be called"}}`)
	}))
	defer anthropicUpstream.Close()
	codexUpstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		codexHits++
		if r.URL.Path != "/v1/responses" {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":{"message":"unexpected path"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_1","object":"response","status":"completed","model":"gpt-5.6-sol","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)
	}))
	defer codexUpstream.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "anthropic-local", upstreamProtocol: "anthropic", protocolTransformMode: model.ProtocolTransformModeLocal, models: "gpt-5.6-sol"},
		{name: "codex-local", upstreamProtocol: "codex", protocolTransformMode: model.ProtocolTransformModeLocal, models: "gpt-5.6-sol"},
	}, map[int]string{0: anthropicUpstream.URL, 1: codexUpstream.URL})

	w := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
		"model": "gpt-5.6-sol",
		"input": []map[string]any{{
			"type":              "compaction",
			"encrypted_content": "opaque",
		}},
	}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if anthropicHits != 0 || codexHits != 1 {
		t.Fatalf("anthropicHits=%d codexHits=%d, want 0/1", anthropicHits, codexHits)
	}
}

func TestProxy_AutomaticProtocolFallback_ExactURLTranslatesDirectly(t *testing.T) {
	t.Parallel()
	var paths []string
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"claude-3-5-sonnet","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer upstream.Close()
	env := setupProxyTestEnv(t, []testChannel{{
		name: "exact-anthropic", upstreamProtocol: "anthropic",
		protocolTransformMode: model.ProtocolTransformModeAuto, models: "claude-3-5-sonnet",
	}}, map[int]string{0: upstream.URL + "/v1/messages#"})

	w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model": "claude-3-5-sonnet", "messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if got := strings.Join(paths, ","); got != "/v1/messages" {
		t.Fatalf("exact URL paths=%s, want one direct local request", got)
	}
}

func TestProxy_OpenAIShapedBodyOnAnthropicPathIsRejected(t *testing.T) {
	t.Parallel()

	called := false

	env := setupProxyTestEnv(t, []testChannel{
		{name: "anthropic-ch", upstreamProtocol: "anthropic", models: "mimo-v2.5", apiKey: "sk-ant"},
	}, map[int]string{0: "https://token-plan-cn.example.com/anthropic"})

	env.server.client = &http.Client{
		Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			called = true
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(bytes.NewReader([]byte(
					`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"mimo-v2.5","stop_reason":"end_turn","usage":{"input_tokens":7,"output_tokens":4}}`,
				))),
			}, nil
		}),
	}

	configs, err := env.store.ListConfigs(context.Background())
	if err != nil {
		t.Fatalf("ListConfigs failed: %v", err)
	}
	cfg := configs[0]
	if _, err := env.store.UpdateConfig(context.Background(), cfg.ID, cfg); err != nil {
		t.Fatalf("UpdateConfig failed: %v", err)
	}
	env.server.InvalidateChannelListCache()

	w := doProxyRequest(t, env.engine, "/v1/messages?beta=true", map[string]any{
		"model": "mimo-v2.5",
		"messages": []map[string]string{
			{"role": "user", "content": "hello"},
		},
		"max_tokens":       4096,
		"response_format":  map[string]string{"type": "json_object"},
		"stream_options":   map[string]bool{"include_usage": true},
		"prompt_cache_key": "cache-key-1",
	}, nil)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
	if called {
		t.Fatal("upstream should not be called for mismatched client protocol body")
	}
	if !strings.Contains(w.Body.String(), "OpenAI chat completions") {
		t.Fatalf("expected protocol mismatch error, got %s", w.Body.String())
	}
}

func TestProxy_OpenAIShapedBodyOnGeminiPathIsRejected(t *testing.T) {
	t.Parallel()

	called := false

	env := setupProxyTestEnv(t, []testChannel{
		{name: "gemini-ch", upstreamProtocol: "gemini", models: "gemini-2.5-pro", apiKey: "sk-gem"},
	}, map[int]string{0: "https://gemini-upstream.example.com"})

	env.server.client = &http.Client{
		Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			called = true
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(bytes.NewReader([]byte(
					`{"candidates":[{"content":{"parts":[{"text":"ok"}]}}],"usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":4,"totalTokenCount":11},"modelVersion":"gemini-2.5-pro"}`,
				))),
			}, nil
		}),
	}

	configs, err := env.store.ListConfigs(context.Background())
	if err != nil {
		t.Fatalf("ListConfigs failed: %v", err)
	}
	cfg := configs[0]
	if _, err := env.store.UpdateConfig(context.Background(), cfg.ID, cfg); err != nil {
		t.Fatalf("UpdateConfig failed: %v", err)
	}
	env.server.InvalidateChannelListCache()

	w := doProxyRequest(t, env.engine, "/v1beta/models/gemini-2.5-pro:generateContent", map[string]any{
		"model": "gemini-2.5-pro",
		"messages": []map[string]string{
			{"role": "user", "content": "hello"},
		},
		"response_format":  map[string]string{"type": "json_object"},
		"stream_options":   map[string]bool{"include_usage": true},
		"prompt_cache_key": "cache-key-1",
	}, nil)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
	if called {
		t.Fatal("upstream should not be called for mismatched client protocol body")
	}
	if !strings.Contains(w.Body.String(), "OpenAI chat completions") {
		t.Fatalf("expected protocol mismatch error, got %s", w.Body.String())
	}
}

func assertChatRequestUserText(t *testing.T, body []byte, want string) {
	t.Helper()

	var request struct {
		Messages []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		t.Fatalf("unmarshal chat request: %v; body=%s", err, body)
	}
	for _, message := range request.Messages {
		if message.Role != "user" {
			continue
		}
		switch content := message.Content.(type) {
		case string:
			if content == want {
				return
			}
		case []any:
			for _, rawBlock := range content {
				block, _ := rawBlock.(map[string]any)
				if text, _ := block["text"].(string); text == want {
					return
				}
			}
		}
	}
	t.Fatalf("expected user text %q in chat request, got %s", want, body)
}

func TestProxy_AutomaticProtocolFallback_UsesNativeProtocolFirst(t *testing.T) {
	t.Parallel()

	var gotPath string
	var gotAuth string
	var gotAPIKey string
	var gotBody []byte

	env := setupProxyTestEnv(t, []testChannel{
		{name: "anthropic-ch", upstreamProtocol: "anthropic", protocolTransformMode: model.ProtocolTransformModeAuto, models: "gpt-4o", apiKey: "sk-openai-upstream"},
	}, map[int]string{0: "https://openai-upstream.example.com"})

	env.server.client = &http.Client{
		Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			gotPath = r.URL.Path
			gotAuth = r.Header.Get("Authorization")
			gotAPIKey = r.Header.Get("x-api-key")
			gotBody, _ = io.ReadAll(r.Body)
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(bytes.NewReader([]byte(
					`{"id":"chatcmpl-upstream","object":"chat.completion","model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"openai native"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`,
				))),
			}, nil
		}),
	}

	configs, err := env.store.ListConfigs(context.Background())
	if err != nil {
		t.Fatalf("ListConfigs failed: %v", err)
	}
	cfg := configs[0]
	if _, err := env.store.UpdateConfig(context.Background(), cfg.ID, cfg); err != nil {
		t.Fatalf("UpdateConfig failed: %v", err)
	}
	env.server.InvalidateChannelListCache()
	candidates, err := env.server.selectCandidatesByModelAndClientProtocol(context.Background(), "gpt-4o", "openai")
	if err != nil {
		t.Fatalf("selectCandidatesByModelAndClientProtocol failed: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(candidates))
	}
	w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model":    "gpt-4o",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if gotPath != "/v1/chat/completions" {
		t.Fatalf("expected native OpenAI upstream path, got %s", gotPath)
	}
	if gotAuth != "Bearer sk-openai-upstream" {
		t.Fatalf("expected bearer auth header, got %q", gotAuth)
	}
	if gotAPIKey != "sk-openai-upstream" {
		t.Fatalf("expected configured x-api-key header, got %q", gotAPIKey)
	}
	if !bytes.Contains(gotBody, []byte(`"messages"`)) {
		t.Fatalf("expected OpenAI request body, got %s", gotBody)
	}

	var resp struct {
		Object string `json:"object"`
		Model  string `json:"model"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.Object != "chat.completion" || resp.Model != "gpt-4o" {
		t.Fatalf("expected response translated back to OpenAI, got %+v", resp)
	}
}

func TestProxy_CodexInvalidEncryptedContentRetriesWithoutEncryptedInputItems(t *testing.T) {
	t.Parallel()

	const invalidEncryptedContentBody = `{"error":{"message":"Could not decrypt the provided encrypted_content. Ensure the value is the unmodified encrypted_content from a previous response.","type":"packy_invalid-argument","code":"invalid-argument"}}`

	var attempts atomic.Int32
	var bodies [][]byte

	env := setupProxyTestEnv(t, []testChannel{
		{customRequestRules: &model.CustomRequestRules{Passthrough: new(bool)}, name: "codex-ch", upstreamProtocol: "codex", models: "gpt-5.5", apiKey: "sk-codex"},
	}, map[int]string{0: "https://codex-upstream.example.com"})

	env.server.client = &http.Client{
		Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			body, _ := io.ReadAll(r.Body)
			bodies = append(bodies, body)
			if attempts.Add(1) == 1 {
				return &http.Response{
					StatusCode: http.StatusBadRequest,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(bytes.NewReader([]byte(invalidEncryptedContentBody))),
				}, nil
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(bytes.NewReader([]byte(
					`{"id":"resp_1","object":"response","status":"completed","model":"gpt-5.5","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`,
				))),
			}, nil
		}),
	}

	w := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
		"model": "gpt-5.5",
		"input": []map[string]any{
			{"type": "compaction", "encrypted_content": "keep-compaction"},
			{"type": "reasoning", "summary": []any{}, "content": nil, "encrypted_content": "drop-reasoning"},
			{"type": "message", "role": "user", "content": []map[string]any{{"type": "input_text", "text": "hi"}}},
		},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected retry success, got %d: %s", w.Code, w.Body.String())
	}
	if attempts.Load() != 2 {
		t.Fatalf("attempts=%d, want 2", attempts.Load())
	}
	if len(bodies) != 2 {
		t.Fatalf("captured bodies=%d, want 2", len(bodies))
	}
	if !bytes.Contains(bodies[0], []byte(`"type":"reasoning"`)) {
		t.Fatalf("first request should include reasoning item, got %s", bodies[0])
	}
	if bytes.Contains(bodies[1], []byte(`"type":"reasoning"`)) {
		t.Fatalf("retry request should remove reasoning item, got %s", bodies[1])
	}
	if bytes.Contains(bodies[1], []byte(`"type":"reasoning"`)) ||
		!bytes.Contains(bodies[1], []byte(`"type":"compaction"`)) ||
		!bytes.Contains(bodies[1], []byte(`"encrypted_content":"keep-compaction"`)) {
		t.Fatalf("retry request should remove encrypted reasoning while preserving compaction, got %s", bodies[1])
	}
	if !bytes.Contains(bodies[1], []byte(`"type":"message"`)) {
		t.Fatalf("retry request should keep non-encrypted input items, got %s", bodies[1])
	}
}

func TestProxy_CodexSSEInvalidEncryptedContentRetriesWithoutEncryptedInputItems(t *testing.T) {
	t.Parallel()

	const invalidEncryptedContentEvent = `{"type":"error","error":{"message":"The encrypted content could not be verified. Reason: Encrypted content could not be decrypted or parsed.","type":"invalid_request_error","param":null,"code":"invalid_encrypted_content"},"status":400}`

	var attempts atomic.Int32
	var bodies [][]byte
	env := setupProxyTestEnv(t, []testChannel{
		{customRequestRules: &model.CustomRequestRules{Passthrough: new(bool)}, name: "codex-sse-ch", upstreamProtocol: "codex", models: "gpt-5.5", apiKey: "sk-codex"},
	}, map[int]string{0: "https://codex-upstream.example.com"})

	env.server.client = &http.Client{
		Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			body, _ := io.ReadAll(r.Body)
			bodies = append(bodies, body)
			if attempts.Add(1) == 1 {
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
					Body: io.NopCloser(bytes.NewReader([]byte(
						"event: error\n" + "data: " + invalidEncryptedContentEvent + "\n\n",
					))),
				}, nil
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body: io.NopCloser(bytes.NewReader([]byte(
					"data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n" +
						"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\",\"status\":\"completed\"}}\n\n",
				))),
			}, nil
		}),
	}

	w := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
		"model":  "gpt-5.5",
		"stream": true,
		"input": []map[string]any{
			{"type": "reasoning", "summary": []any{}, "encrypted_content": "drop-reasoning"},
			{"type": "message", "role": "user", "content": []map[string]any{{"type": "input_text", "text": "hi"}}},
		},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected retry success, got %d: %s", w.Code, w.Body.String())
	}
	if attempts.Load() != 2 || len(bodies) != 2 {
		t.Fatalf("attempts=%d bodies=%d, want 2/2", attempts.Load(), len(bodies))
	}
	if !bytes.Contains(bodies[0], []byte(`"type":"reasoning"`)) {
		t.Fatalf("first request should include reasoning item, got %s", bodies[0])
	}
	if bytes.Contains(bodies[1], []byte(`"type":"reasoning"`)) ||
		bytes.Contains(bodies[1], []byte(`"encrypted_content"`)) {
		t.Fatalf("SSE 400 retry should remove encrypted thinking state, got %s", bodies[1])
	}
	if !bytes.Contains(bodies[1], []byte(`"type":"message"`)) ||
		!strings.Contains(w.Body.String(), `"delta":"ok"`) {
		t.Fatalf("retry should preserve user message and return second response, body=%s response=%s", bodies[1], w.Body.String())
	}
}

func TestProxy_Codex400PreservesExplicitThinkingAndUpstreamError(t *testing.T) {
	t.Parallel()

	const unsupportedThinkingBody = `{"error":{"message":"unsupported parameter: reasoning","type":"invalid_request_error","param":"reasoning","code":"unsupported_parameter"}}`

	var attempts atomic.Int32
	var bodies [][]byte

	env := setupProxyTestEnv(t, []testChannel{
		{name: "codex-no-thinking", upstreamProtocol: "codex", models: "gpt-5-codex", apiKey: "sk-codex"},
	}, map[int]string{0: "https://codex-upstream.example.com"})

	env.server.client = &http.Client{
		Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			body, _ := io.ReadAll(r.Body)
			bodies = append(bodies, body)
			if attempts.Add(1) == 1 {
				return &http.Response{
					StatusCode: http.StatusBadRequest,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(bytes.NewReader([]byte(unsupportedThinkingBody))),
				}, nil
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(bytes.NewReader([]byte(
					`{"id":"resp_1","object":"response","status":"completed","model":"gpt-5-codex","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`,
				))),
			}, nil
		}),
	}

	w := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
		"model": "gpt-5-codex",
		"input": []map[string]any{
			{"type": "reasoning", "summary": []any{}, "content": []map[string]any{{"type": "reasoning_text", "text": "drop thinking"}}},
			{"type": "message", "role": "user", "content": []map[string]any{{"type": "input_text", "text": "hi"}}},
		},
		"reasoning": map[string]any{"effort": "medium", "summary": "auto"},
		"include":   []string{"reasoning.encrypted_content"},
	}, nil)

	if w.Code != http.StatusBadRequest || attempts.Load() != 1 || len(bodies) != 1 {
		t.Fatalf("explicit reasoning must not be stripped: status=%d attempts=%d body=%s", w.Code, attempts.Load(), w.Body.String())
	}
	if !bytes.Contains(bodies[0], []byte(`"reasoning"`)) || !bytes.Contains(bodies[0], []byte(`"include"`)) {
		t.Fatalf("explicit controls lost: %s", bodies[0])
	}
	if gjson.GetBytes(w.Body.Bytes(), "error.code").String() != "unsupported_parameter" {
		t.Fatalf("upstream error lost: %s", w.Body.String())
	}
}

func TestProxy_CodexNormalizesToolSearchArgumentsBeforeForward(t *testing.T) {
	t.Parallel()

	var upstreamBody []byte
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		upstreamBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read upstream body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"resp_1","object":"response","status":"completed","model":"gpt-5.5","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`))
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{customRequestRules: &model.CustomRequestRules{Passthrough: new(bool)}, name: "codex-normalize", upstreamProtocol: "codex", models: "gpt-5.5", apiKey: "sk-codex"},
	}, map[int]string{0: upstream.URL})

	w := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
		"model": "gpt-5.5",
		"input": []map[string]any{
			{"type": "tool_search_call", "call_id": "search_valid", "status": "completed", "arguments": `{"query":"codegraph_explore","limit":5}`},
			{"type": "tool_search_call", "call_id": "search_object", "status": "completed", "arguments": map[string]any{"query": "already_object"}},
			{"type": "tool_search_call", "call_id": "search_bad", "status": "completed", "arguments": `{bad json`},
			{"type": "tool_search_call", "call_id": "search_number", "status": "completed", "arguments": 7},
			{"type": "tool_search_output", "call_id": "search_valid", "status": "completed", "tools": []any{}},
			{"type": "message", "role": "user", "content": []map[string]any{{"type": "input_text", "text": "hi"}}},
		},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var sent struct {
		Input []map[string]any `json:"input"`
	}
	if err := json.Unmarshal(upstreamBody, &sent); err != nil {
		t.Fatalf("unmarshal upstream body: %v", err)
	}

	foundValid := false
	foundObject := false
	foundBad := false
	foundNumber := false
	foundOutput := false
	for _, item := range sent.Input {
		switch item["call_id"] {
		case "search_valid":
			if item["type"] == "tool_search_output" {
				foundOutput = true
				continue
			}
			args, ok := item["arguments"].(map[string]any)
			if !ok {
				t.Fatalf("tool_search_call arguments type=%T, want object in %s", item["arguments"], upstreamBody)
			}
			if args["query"] != "codegraph_explore" || int(args["limit"].(float64)) != 5 {
				t.Fatalf("unexpected normalized arguments: %#v", args)
			}
			foundValid = true
		case "search_object":
			args, ok := item["arguments"].(map[string]any)
			if !ok || args["query"] != "already_object" {
				t.Fatalf("object arguments should be preserved, got %#v", item["arguments"])
			}
			foundObject = true
		case "search_bad":
			foundBad = true
		case "search_number":
			foundNumber = true
		}
	}
	if !foundValid {
		t.Fatalf("valid tool_search_call missing from upstream body: %s", upstreamBody)
	}
	if !foundObject {
		t.Fatalf("object tool_search_call missing from upstream body: %s", upstreamBody)
	}
	if foundBad {
		t.Fatalf("invalid tool_search_call should be removed from upstream body: %s", upstreamBody)
	}
	if foundNumber {
		t.Fatalf("non-object tool_search_call arguments should be removed from upstream body: %s", upstreamBody)
	}
	if !foundOutput {
		t.Fatalf("tool_search_output without arguments should be preserved: %s", upstreamBody)
	}
}

func TestProxy_AnyrouterCodexContentItemKinds(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		url   string
		strip bool
	}{
		{"prefix-AnyRouter-codex", "https://upstream.example.com", true},
		{"regular-codex", "https://upstream.example.com", false},
		{"url-only", "https://anyrouter.top", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := setupProxyTestEnv(t, []testChannel{
				{customRequestRules: &model.CustomRequestRules{Passthrough: new(bool)}, name: tc.name, upstreamProtocol: "codex", models: "gpt-6-astra", apiKey: "sk-test"},
			}, map[int]string{0: tc.url})
			var captured []byte
			env.server.client = &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				var err error
				captured, err = io.ReadAll(r.Body)
				if err != nil {
					return nil, err
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"id":"resp_test","object":"response","status":"completed","model":"gpt-6-astra","output":[]}`)),
				}, nil
			})}
			input := []map[string]any{
				{"type": "message", "role": "user", "content": []map[string]any{{"type": "input_text", "text": "hello"}}, "internal_chat_message_metadata_passthrough": map[string]any{"content_item_kinds": []string{"test"}, "turn_id": "turn-1", "create_time": 123}},
				{"type": "message", "role": "user", "content": []map[string]any{{"type": "input_text", "text": "world"}}, "internal_chat_message_metadata_passthrough": map[string]any{"content_item_kinds": []string{"test"}}},
			}
			w := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{"model": "gpt-6-astra", "input": input}, nil)
			if w.Code != http.StatusOK {
				t.Fatalf("status=%d: %s", w.Code, w.Body.String())
			}
			var got struct {
				Input []map[string]any `json:"input"`
			}
			if err := json.Unmarshal(captured, &got); err != nil {
				t.Fatal(err)
			}
			if tc.strip {
				for _, item := range input {
					delete(item["internal_chat_message_metadata_passthrough"].(map[string]any), "content_item_kinds")
				}
			}
			wantJSON, err := json.Marshal(input)
			if err != nil {
				t.Fatal(err)
			}
			var want []map[string]any
			if err := json.Unmarshal(wantJSON, &want); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got.Input, want) {
				t.Fatalf("upstream input mismatch: got %#v, want %#v", got.Input, want)
			}
		})
	}
}

func TestProxy_AnyrouterCodexStripsToolSearchBeforeForwardThenRetriesEncryptedContent(t *testing.T) {
	t.Parallel()

	const invalidResponsesRequestBody = `{"error":{"message":"invalid codex request (request id: req_test)","type":"new_api_error","param":"","code":"invalid_responses_request"}}`

	var attempts atomic.Int32
	var bodies [][]byte

	env := setupProxyTestEnv(t, []testChannel{
		{customRequestRules: &model.CustomRequestRules{Passthrough: new(bool)}, name: "anyrouter-codex", upstreamProtocol: "codex", models: "gpt-5.5", apiKey: "sk-codex"},
	}, map[int]string{0: "https://codex-upstream.example.com"})

	env.server.client = &http.Client{
		Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			body, _ := io.ReadAll(r.Body)
			bodies = append(bodies, body)
			switch attempts.Add(1) {
			case 1:
				return &http.Response{
					StatusCode: http.StatusBadRequest,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(bytes.NewReader([]byte(invalidResponsesRequestBody))),
				}, nil
			default:
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body: io.NopCloser(bytes.NewReader([]byte(
						`{"id":"resp_1","object":"response","status":"completed","model":"gpt-5.5","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`,
					))),
				}, nil
			}
		}),
	}

	w := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
		"model": "gpt-5.5",
		"input": []map[string]any{
			{"type": "reasoning", "summary": []any{}, "encrypted_content": "drop-reasoning"},
			{"type": "tool_search_call", "call_id": "search_1", "status": "completed", "arguments": `{"query":"x"}`},
			{"type": "tool_search_output", "call_id": "search_1", "status": "completed", "tools": []any{}},
			{"type": "message", "role": "user", "content": []map[string]any{{"type": "input_text", "text": "hi"}}},
		},
		"include": []string{"reasoning.encrypted_content"},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected retry success, got %d: %s", w.Code, w.Body.String())
	}
	if attempts.Load() != 2 {
		t.Fatalf("attempts=%d, want 2", attempts.Load())
	}
	if len(bodies) != 2 {
		t.Fatalf("captured bodies=%d, want 2", len(bodies))
	}
	if !bytes.Contains(bodies[0], []byte(`"encrypted_content"`)) ||
		bytes.Contains(bodies[0], []byte(`"type":"tool_search_call"`)) ||
		bytes.Contains(bodies[0], []byte(`"type":"tool_search_output"`)) {
		t.Fatalf("first request should strip tool search history before forwarding, got %s", bodies[0])
	}
	if bytes.Contains(bodies[1], []byte(`"encrypted_content"`)) ||
		bytes.Contains(bodies[1], []byte(`"type":"tool_search_call"`)) ||
		bytes.Contains(bodies[1], []byte(`"type":"tool_search_output"`)) {
		t.Fatalf("second request should remove encrypted content after sanitized anyrouter request fails, got %s", bodies[1])
	}
	if !bytes.Contains(bodies[1], []byte(`"type":"reasoning"`)) ||
		!bytes.Contains(bodies[1], []byte(`"type":"message"`)) {
		t.Fatalf("second request should keep sanitized reasoning summary and messages, got %s", bodies[1])
	}
}

func TestProxy_CodexInvalidEncryptedContentWrappedRequestErrorRetries(t *testing.T) {
	t.Parallel()

	const wrappedInvalidEncryptedContentBody = `{"error":{"message":"all 2 attempts failed: HTTP 400: {\"error\":{\"message\":\"The encrypted content gAAA...fnaA could not be verified. Reason: Encrypted content could not be decrypted or parsed.\",\"type\":\"invalid_request_error\",\"param\":\"\",\"code\":\"invalid_encrypted_content\"}}","type":"request_error"}}`

	var attempts atomic.Int32
	var bodies [][]byte

	env := setupProxyTestEnv(t, []testChannel{
		{customRequestRules: &model.CustomRequestRules{Passthrough: new(bool)}, name: "codex-ch", upstreamProtocol: "codex", models: "gpt-5.5", apiKey: "sk-codex"},
	}, map[int]string{0: "https://codex-upstream.example.com"})

	env.server.client = &http.Client{
		Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			body, _ := io.ReadAll(r.Body)
			bodies = append(bodies, body)
			if attempts.Add(1) == 1 {
				return &http.Response{
					StatusCode: http.StatusBadRequest,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(bytes.NewReader([]byte(wrappedInvalidEncryptedContentBody))),
				}, nil
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(bytes.NewReader([]byte(
					`{"id":"resp_1","object":"response","status":"completed","model":"gpt-5.5","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`,
				))),
			}, nil
		}),
	}

	w := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
		"model": "gpt-5.5",
		"input": []map[string]any{
			{"type": "reasoning", "summary": []any{}, "content": nil, "encrypted_content": "drop-reasoning"},
			{"type": "message", "role": "user", "content": []map[string]any{{"type": "input_text", "text": "hi"}}},
		},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected retry success, got %d: %s", w.Code, w.Body.String())
	}
	if attempts.Load() != 2 {
		t.Fatalf("attempts=%d, want 2", attempts.Load())
	}
	if len(bodies) != 2 {
		t.Fatalf("captured bodies=%d, want 2", len(bodies))
	}
	if bytes.Contains(bodies[1], []byte(`"type":"reasoning"`)) ||
		bytes.Contains(bodies[1], []byte(`"encrypted_content"`)) {
		t.Fatalf("retry request should remove encrypted thinking state, got %s", bodies[1])
	}
	if !bytes.Contains(bodies[1], []byte(`"type":"message"`)) {
		t.Fatalf("retry request should keep non-encrypted input items, got %s", bodies[1])
	}
}

func TestProxy_CodexInvalidEncryptedContentRetryFailureReturnsUpstreamError(t *testing.T) {
	t.Parallel()

	const firstError = `{"error":{"message":"The encrypted content could not be verified. Reason: Encrypted content could not be decrypted or parsed.","type":"invalid_request_error","param":"","code":"invalid_encrypted_content"}}`
	const secondError = `{"error":{"message":"still invalid after retry","type":"invalid_request_error","code":"invalid_encrypted_content"}}`

	var attempts, backupAttempts atomic.Int32

	env := setupProxyTestEnv(t, []testChannel{
		{customRequestRules: &model.CustomRequestRules{Passthrough: new(bool)}, name: "codex-ch", upstreamProtocol: "codex", models: "gpt-5.5", apiKey: "sk-codex"},
		{customRequestRules: &model.CustomRequestRules{Passthrough: new(bool)}, name: "codex-backup", upstreamProtocol: "codex", models: "gpt-5.5", apiKey: "sk-backup"},
	}, map[int]string{0: "https://codex-upstream.example.com", 1: "https://codex-backup.example.com"})

	env.server.client = &http.Client{
		Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			if r.URL.Host == "codex-backup.example.com" {
				backupAttempts.Add(1)
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"id":"resp_backup","object":"response","status":"completed","output":[]}`)),
				}, nil
			}
			body := firstError
			if attempts.Add(1) == 2 {
				body = secondError
			}
			return &http.Response{
				StatusCode: http.StatusBadRequest,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(bytes.NewReader([]byte(body))),
			}, nil
		}),
	}

	w := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
		"model": "gpt-5.5",
		"input": []map[string]any{
			{"type": "reasoning", "summary": []any{}, "content": nil, "encrypted_content": "drop-reasoning"},
			{"type": "message", "role": "user", "content": []map[string]any{{"type": "input_text", "text": "hi"}}},
		},
	}, nil)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected retry failure to return upstream 400, got %d: %s", w.Code, w.Body.String())
	}
	if attempts.Load() != 2 {
		t.Fatalf("attempts=%d, want 2", attempts.Load())
	}
	if !strings.Contains(w.Body.String(), "still invalid after retry") {
		t.Fatalf("expected second upstream error body, got %s", w.Body.String())
	}
	if got := backupAttempts.Load(); got != 0 {
		t.Fatalf("backup attempts=%d, want 0: repaired request rejection must not fail over", got)
	}
	assertNoProxyCooldowns(t, env)
}

func TestProxy_RepairRetryNonRequestFailureFailsOver(t *testing.T) {
	t.Parallel()

	const (
		maxTokensRejected      = `{"error":{"message":"Unsupported parameter: 'max_tokens' is not supported with this model. Use 'max_completion_tokens' instead.","type":"invalid_request_error","param":"max_tokens","code":"unsupported_parameter"}}`
		encryptedRejected      = `{"error":{"message":"The encrypted content could not be verified.","type":"invalid_request_error","code":"invalid_encrypted_content"}}`
		usageSemanticsRejected = `{"error":{"message":"unsupported parameter: usage_semantics","type":"invalid_request_error","param":"usage_semantics","code":"unsupported_parameter"}}`
		serviceUnavailable     = `{"error":{"message":"upstream overloaded","type":"server_error"}}`
		creditBalanceTooLow    = `{"error":{"message":"Your credit balance is too low to access the API.","type":"invalid_request_error"}}`
	)
	type upstreamReply struct {
		status int
		body   string
	}
	openAIRequest := map[string]any{
		"model":           "deepseek-chat",
		"max_tokens":      64,
		"usage_semantics": "inclusive",
		"messages":        []map[string]string{{"role": "user", "content": "hi"}},
	}
	codexRequest := map[string]any{
		"model": "gpt-5.5",
		"input": []map[string]any{
			{"type": "reasoning", "summary": []any{}, "content": nil, "encrypted_content": "drop-reasoning"},
			{"type": "message", "role": "user", "content": []map[string]any{{"type": "input_text", "text": "hi"}}},
		},
	}

	tests := []struct {
		name          string
		protocol      string
		model         string
		path          string
		request       map[string]any
		primary       []upstreamReply
		backupBody    string
		wantModelCool bool
	}{
		{
			name:          "max_tokens remap then 503",
			protocol:      "openai",
			model:         "deepseek-chat",
			path:          "/v1/chat/completions",
			request:       openAIRequest,
			primary:       []upstreamReply{{http.StatusBadRequest, maxTokensRejected}, {http.StatusServiceUnavailable, serviceUnavailable}},
			backupBody:    `{"id":"from-backup","object":"chat.completion","model":"deepseek-chat","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`,
			wantModelCool: true,
		},
		{
			// Explicit reasoning must be preserved; use two supported Chat repairs.
			name:     "two openai repairs then 503",
			protocol: "openai", model: "deepseek-chat", path: "/v1/chat/completions",
			request:       openAIRequest,
			primary:       []upstreamReply{{http.StatusBadRequest, maxTokensRejected}, {http.StatusBadRequest, usageSemanticsRejected}, {http.StatusServiceUnavailable, serviceUnavailable}},
			backupBody:    `{"id":"from-backup","object":"chat.completion","model":"deepseek-chat","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`,
			wantModelCool: true,
		},
		{
			name:     "codex encrypted history repair then 503",
			protocol: "codex",
			model:    "gpt-5.5",
			path:     "/v1/responses",
			request:  codexRequest,
			primary: []upstreamReply{
				{http.StatusBadRequest, encryptedRejected},
				{http.StatusServiceUnavailable, serviceUnavailable},
			},
			backupBody:    `{"id":"from-backup","object":"response","status":"completed","model":"gpt-5.5","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`,
			wantModelCool: true,
		},
		{
			name:       "max_tokens remap then key-level 400",
			protocol:   "openai",
			model:      "deepseek-chat",
			path:       "/v1/chat/completions",
			request:    openAIRequest,
			primary:    []upstreamReply{{http.StatusBadRequest, maxTokensRejected}, {http.StatusBadRequest, creditBalanceTooLow}},
			backupBody: `{"id":"from-backup","object":"chat.completion","model":"deepseek-chat","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var primaryAttempts, backupAttempts atomic.Int32
			env := setupProxyTestEnv(t, []testChannel{
				{customRequestRules: &model.CustomRequestRules{Passthrough: new(bool)}, name: "primary", upstreamProtocol: tt.protocol, models: tt.model, apiKey: "sk-primary"},
				{customRequestRules: &model.CustomRequestRules{Passthrough: new(bool)}, name: "backup", upstreamProtocol: tt.protocol, models: tt.model, apiKey: "sk-backup"},
			}, map[int]string{0: "https://primary.example.com", 1: "https://backup.example.com"})
			env.server.client = &http.Client{
				Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
					reply := upstreamReply{http.StatusOK, tt.backupBody}
					if r.URL.Host == "backup.example.com" {
						backupAttempts.Add(1)
					} else {
						n := int(primaryAttempts.Add(1))
						if n > len(tt.primary) {
							t.Errorf("unexpected primary attempt %d", n)
							n = len(tt.primary)
						}
						reply = tt.primary[n-1]
					}
					return &http.Response{
						StatusCode: reply.status,
						Header:     http.Header{"Content-Type": []string{"application/json"}},
						Body:       io.NopCloser(strings.NewReader(reply.body)),
					}, nil
				}),
			}

			w := doProxyRequest(t, env.engine, tt.path, tt.request, nil)

			if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "from-backup") {
				t.Fatalf("expected backup response, got %d: %s", w.Code, w.Body.String())
			}
			if got := primaryAttempts.Load(); got != int32(len(tt.primary)) {
				t.Fatalf("primary attempts=%d, want %d", got, len(tt.primary))
			}
			if got := backupAttempts.Load(); got != 1 {
				t.Fatalf("backup attempts=%d, want 1", got)
			}

			ctx := context.Background()
			primaryID := proxyTestChannelID(t, env, "primary")
			models, err := env.store.GetAllModelCooldowns(ctx)
			if err != nil {
				t.Fatalf("GetAllModelCooldowns: %v", err)
			}
			_, modelCooled := models[primaryID][tt.model]
			if modelCooled != tt.wantModelCool {
				t.Fatalf("primary model cooldown=%v, want %v (all=%v)", modelCooled, tt.wantModelCool, models)
			}
			if !tt.wantModelCool {
				keys, err := env.store.GetAllKeyCooldowns(ctx)
				if err != nil {
					t.Fatalf("GetAllKeyCooldowns: %v", err)
				}
				if len(keys[primaryID]) == 0 {
					t.Fatalf("expected key-level cooldown on primary, got %v", keys)
				}
			}

		})
	}
}

func proxyTestChannelID(t *testing.T, env *proxyTestEnv, name string) int64 {
	t.Helper()
	configs, err := env.store.ListConfigs(context.Background())
	if err != nil {
		t.Fatalf("ListConfigs: %v", err)
	}
	for _, cfg := range configs {
		if cfg.Name == name {
			return cfg.ID
		}
	}
	t.Fatalf("channel %q not found", name)
	return 0
}

func assertNoProxyCooldowns(t *testing.T, env *proxyTestEnv) {
	t.Helper()
	ctx := context.Background()
	models, err := env.store.GetAllModelCooldowns(ctx)
	if err != nil {
		t.Fatalf("GetAllModelCooldowns: %v", err)
	}
	keys, err := env.store.GetAllKeyCooldowns(ctx)
	if err != nil {
		t.Fatalf("GetAllKeyCooldowns: %v", err)
	}
	channels, err := env.store.GetAllChannelCooldowns(ctx)
	if err != nil {
		t.Fatalf("GetAllChannelCooldowns: %v", err)
	}
	if len(models) != 0 || len(keys) != 0 || len(channels) != 0 {
		t.Fatalf("unexpected cooldowns: models=%v keys=%v channels=%v", models, keys, channels)
	}
}

func TestProxy_Success_Streaming_CodexCompletedWithoutEOF(t *testing.T) {
	t.Parallel()

	sse := []byte("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"Hello\"}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"model\":\"gpt-5-codex\",\"usage\":{\"input_tokens\":7,\"output_tokens\":4,\"total_tokens\":11}}}\n\n")
	trailing := []byte("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"late\"}\n\n")

	for _, contentType := range []string{"text/event-stream", "text/plain; charset=utf-8"} {
		t.Run(contentType, func(t *testing.T) {
			t.Parallel()

			upstreamData := append(append([]byte(nil), sse...), trailing...)
			upstreamBody := newDataThenBlockReadCloser(upstreamData, len(upstreamData))
			defer func() { _ = upstreamBody.Close() }()

			env := setupProxyTestEnv(t, []testChannel{
				{name: "codex-no-eof", upstreamProtocol: "codex", models: "gpt-5-codex", apiKey: "sk-codex"},
			}, map[int]string{0: "https://codex-upstream.example.com"})
			env.server.client = &http.Client{
				Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     http.Header{"Content-Type": []string{contentType}},
						Body:       upstreamBody,
					}, nil
				}),
			}

			done := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				done <- doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
					"model":  "gpt-5-codex",
					"stream": true,
					"input":  "hi",
				}, nil)
			}()

			var w *httptest.ResponseRecorder
			select {
			case w = <-done:
			case <-time.After(2 * time.Second):
				_ = upstreamBody.Close()
				<-done
				t.Fatal("Responses stream waited for upstream EOF after response.completed")
			}

			if w.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
			}
			if w.Body.String() != string(sse) {
				t.Fatalf("forwarded SSE mismatch:\n got: %q\nwant: %q", w.Body.String(), string(sse))
			}
			entry := waitForProxyLog(t, env, "gpt-5-codex")
			if entry.InputTokens != 7 || entry.OutputTokens != 4 {
				t.Fatalf("logged usage=(%d,%d), want (7,4)", entry.InputTokens, entry.OutputTokens)
			}
			select {
			case <-upstreamBody.closed:
			default:
				t.Fatal("stream returned without closing the upstream body")
			}
		})
	}
}

func TestProxy_Success_Streaming_AnthropicMessageStopWithoutEOF(t *testing.T) {
	t.Parallel()
	sse := []byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-3-5-sonnet\",\"stop_reason\":null,\"usage\":{\"input_tokens\":7,\"output_tokens\":0}}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hello\"}}\n\n" +
		"data: {\"type\":\"message_stop\"}\n\n")
	trailing := []byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"late\"}}\n\n")

	tests := []struct {
		name         string
		path         string
		requestBody  map[string]any
		wantRaw      bool
		wantTerminal string
	}{
		{
			name: "native Anthropic",
			path: "/v1/messages",
			requestBody: map[string]any{
				"model": "claude-3-5-sonnet", "max_tokens": 64, "stream": true,
				"messages": []map[string]string{{"role": "user", "content": "hi"}},
			},
			wantRaw: true,
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			upstreamData := append(append([]byte(nil), sse...), trailing...)
			upstreamBody := newDataThenBlockReadCloser(upstreamData, len(upstreamData))
			defer func() { _ = upstreamBody.Close() }()

			env := setupProxyTestEnv(t, []testChannel{{
				name: "anthropic-no-eof", upstreamProtocol: util.ProtocolAnthropic, models: "claude-3-5-sonnet", apiKey: "sk-ant",
			}}, map[int]string{0: "https://anthropic-upstream.example.com"})
			env.server.client = &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
					Body:       upstreamBody,
				}, nil
			})}

			done := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				done <- doProxyRequest(t, env.engine, testCase.path, testCase.requestBody, nil)
			}()

			var response *httptest.ResponseRecorder
			select {
			case response = <-done:
			case <-time.After(2 * time.Second):
				_ = upstreamBody.Close()
				<-done
				t.Fatal("Anthropic stream waited for upstream EOF after message_stop")
			}

			if response.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			body := response.Body.String()
			if testCase.wantRaw && body != string(sse) {
				t.Fatalf("forwarded SSE mismatch:\n got: %q\nwant: %q", body, string(sse))
			}
			if testCase.wantTerminal != "" && !strings.Contains(body, testCase.wantTerminal) {
				t.Fatalf("translated terminal event missing: %s", body)
			}
			if strings.Contains(body, "late") {
				t.Fatalf("data after message_stop was forwarded: %s", body)
			}
			select {
			case <-upstreamBody.closed:
			default:
				t.Fatal("stream returned without closing the upstream body")
			}
		})
	}
}

// 跨协议转换流在上游与转换输出都没有终态时干净 EOF：Claude/Responses/OpenAI Chat 客户端
// 拿到的是半截回复，必须判 599 并冷却；Gemini 不带 usage 的 finishReason 与 Anthropic
// 不带 message_stop 的 stop_reason 是合法终态，客户端要收到完整终止序列。

// Gemini 拦截提示词时只回 promptFeedback.blockReason、没有 candidates；跨协议客户端必须收到
// 400 和拦截原因，而不是一条空的正常完成。拦截由提示词决定：不换渠道、不冷却。

// assertResponsesStreamEndsFailed 断言 Responses 流以补写的 response.failed 收尾，
// 且沿用流内 response id、紧接最后一个 sequence_number。

// assertOpenAIStreamEndsWithError 断言 OpenAI Chat 流以唯一的错误数据帧收尾且没有 [DONE]。

// 上游在已向客户端输出后发 error 事件：Responses 转换器不产出终态，须补写带上游原因的
// response.failed；OpenAI Chat 转换器自带错误帧，不得再叠一帧。

func TestProxy_ThinkingSuffixUsesResolvedModelCapabilities(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		configuredModel string
		requestedModel  string
		redirectModel   string
		fuzzyMatch      bool
	}{
		{
			name:            "redirect",
			configuredModel: "latest",
			requestedModel:  "latest(max)",
			redirectModel:   "gpt-5.5",
		},
		{
			name:            "fuzzy match",
			configuredModel: "gpt-5.5",
			requestedModel:  "5.5(max)",
			fuzzyMatch:      true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := setupProxyTestEnv(t, []testChannel{{
				name: "openai-ch", upstreamProtocol: "openai", models: tt.configuredModel, apiKey: "sk-oai",
			}}, map[int]string{0: "https://openai-upstream.example.com"})

			if tt.redirectModel != "" {
				configs, err := env.store.ListConfigs(context.Background())
				if err != nil {
					t.Fatalf("ListConfigs failed: %v", err)
				}
				cfg := configs[0]
				cfg.ModelEntries = []model.ModelEntry{{Model: tt.configuredModel, RedirectModel: tt.redirectModel}}
				if _, err := env.store.UpdateConfig(context.Background(), cfg.ID, cfg); err != nil {
					t.Fatalf("UpdateConfig failed: %v", err)
				}
				env.server.InvalidateChannelListCache()
			}
			env.server.modelFuzzyMatch = tt.fuzzyMatch

			var gotModel, gotEffort string
			env.server.client = &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				requestBody, err := io.ReadAll(r.Body)
				if err != nil {
					return nil, err
				}
				gotModel = gjson.GetBytes(requestBody, "model").String()
				gotEffort = gjson.GetBytes(requestBody, "reasoning_effort").String()
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body: io.NopCloser(strings.NewReader(
						`{"id":"chatcmpl-thinking","object":"chat.completion","model":"gpt-5.5","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`,
					)),
				}, nil
			})}

			w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
				"model": tt.requestedModel,
				"messages": []map[string]string{{
					"role": "user", "content": "hi",
				}},
			}, nil)
			if w.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
			}
			if gotModel != "gpt-5.5" {
				t.Fatalf("upstream model = %q, want gpt-5.5", gotModel)
			}
			if gotEffort != "xhigh" {
				t.Fatalf("upstream reasoning_effort = %q, want xhigh", gotEffort)
			}
		})
	}
}

func TestProxy_ChannelRetry_On503(t *testing.T) {
	t.Parallel()

	// 渠道1：返回 503
	upstream1 := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"service unavailable"}`))
	}))
	defer upstream1.Close()

	// 渠道2：返回 200
	upstream2 := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"from-ch2","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer upstream2.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "ch1-fail", models: "gpt-4", apiKey: "sk-1", priority: 100},
		{name: "ch2-ok", models: "gpt-4", apiKey: "sk-2", priority: 50},
	}, map[int]string{0: upstream1.URL, 1: upstream2.URL})

	w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model":    "gpt-4",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (fallback to ch2), got %d: %s", w.Code, w.Body.String())
	}
}

func TestProxy_NonStreamingEmpty200RetriesNextChannel(t *testing.T) {
	t.Parallel()

	var emptyCalls atomic.Int32
	upstreamEmpty := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		emptyCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstreamEmpty.Close()

	var okCalls atomic.Int32
	upstreamOK := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		okCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"from-ch2","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer upstreamOK.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "ch-empty", models: "gpt-4", apiKey: "sk-empty", priority: 100},
		{name: "ch-ok", models: "gpt-4", apiKey: "sk-ok", priority: 50},
	}, map[int]string{
		0: upstreamEmpty.URL,
		1: upstreamOK.URL,
	})

	w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model":    "gpt-4",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 after retrying next channel, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "from-ch2") {
		t.Fatalf("expected response from second channel, got body: %s", w.Body.String())
	}
	if got := emptyCalls.Load(); got != 1 {
		t.Fatalf("empty upstream calls=%d, want 1", got)
	}
	if got := okCalls.Load(); got != 1 {
		t.Fatalf("ok upstream calls=%d, want 1", got)
	}
}

func TestProxy_StreamingEmpty200RetriesNextChannel(t *testing.T) {
	t.Parallel()

	var emptyCalls atomic.Int32
	upstreamEmpty := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		emptyCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstreamEmpty.Close()

	var okCalls atomic.Int32
	upstreamOK := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		okCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		_, _ = fmt.Fprint(w, "data: {\"id\":\"from-ch2\",\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":null}]}\n\n")
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer upstreamOK.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "ch-empty-stream", models: "gpt-4", apiKey: "sk-empty", priority: 100},
		{name: "ch-ok-stream", models: "gpt-4", apiKey: "sk-ok", priority: 50},
	}, map[int]string{
		0: upstreamEmpty.URL,
		1: upstreamOK.URL,
	})

	w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model":    "gpt-4",
		"stream":   true,
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 after retrying next streaming channel, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "from-ch2") {
		t.Fatalf("expected stream response from second channel, got body: %s", w.Body.String())
	}
	if got := emptyCalls.Load(); got != 1 {
		t.Fatalf("empty upstream calls=%d, want 1", got)
	}
	if got := okCalls.Load(); got != 1 {
		t.Fatalf("ok upstream calls=%d, want 1", got)
	}
}

func TestProxy_StreamingPingOnly200RetriesNextChannel(t *testing.T) {
	t.Parallel()

	var pingCalls atomic.Int32
	upstreamPing := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pingCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		_, _ = fmt.Fprint(w, "event: ping\n")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"ping\"}\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer upstreamPing.Close()

	var okCalls atomic.Int32
	upstreamOK := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		okCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		_, _ = fmt.Fprint(w, "data: {\"id\":\"from-ch2\",\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":null}]}\n\n")
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer upstreamOK.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "ch-ping-stream", models: "gpt-4", apiKey: "sk-ping", priority: 100},
		{name: "ch-ok-stream", models: "gpt-4", apiKey: "sk-ok", priority: 50},
	}, map[int]string{
		0: upstreamPing.URL,
		1: upstreamOK.URL,
	})

	w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model":    "gpt-4",
		"stream":   true,
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 after retrying next streaming channel, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "from-ch2") {
		t.Fatalf("expected stream response from second channel, got body: %s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), `"type":"ping"`) {
		t.Fatalf("expected ping-only response not to leak to client, got body: %s", w.Body.String())
	}
	if got := pingCalls.Load(); got != 1 {
		t.Fatalf("ping upstream calls=%d, want 1", got)
	}
	if got := okCalls.Load(); got != 1 {
		t.Fatalf("ok upstream calls=%d, want 1", got)
	}
}

func TestProxy_MultiURL5xx_SwitchesToNextChannel(t *testing.T) {
	t.Parallel()

	var ch1FailCalls atomic.Int64
	var ch1SecondURLCalls atomic.Int64
	var ch2Calls atomic.Int64

	// 渠道1 URL1: 固定 503
	upstreamFail := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ch1FailCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"service unavailable"}`))
	}))
	defer upstreamFail.Close()

	// 渠道1 URL2: 即使可用也不应被尝试（新策略：5xx 直接切渠道）
	upstreamShouldSkip := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ch1SecondURLCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"from-ch1-url2","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer upstreamShouldSkip.Close()

	// 渠道2: 正常返回，用于验证“切换到下一个渠道”
	upstreamCh2 := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ch2Calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"from-ch2","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer upstreamCh2.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{
			name: "ch-multi-url", models: "gpt-4", apiKey: "sk-1", priority: 100,
			cooldownDetectionRules: &model.CooldownDetectionRules{Rules: []model.CooldownDetectionRule{{
				Enabled: true, Name: "Provider unavailable", Priority: 0, StatusCodes: []int{http.StatusServiceUnavailable},
				Scope: model.CooldownScopeChannel, Mode: model.CooldownModeFixed, CooldownSeconds: 120,
			}}},
		},
		{name: "ch-fallback", models: "gpt-4", apiKey: "sk-2", priority: 50},
	}, map[int]string{
		0: upstreamFail.URL + "\n" + upstreamShouldSkip.URL,
		1: upstreamCh2.URL,
	})

	ctx := context.Background()
	configs, err := env.store.ListConfigs(ctx)
	if err != nil {
		t.Fatalf("ListConfigs: %v", err)
	}
	if len(configs) != 2 {
		t.Fatalf("expected 2 config, got %d", len(configs))
	}

	var channelID int64
	for _, cfg := range configs {
		if cfg.Name == "ch-multi-url" {
			channelID = cfg.ID
			break
		}
	}
	if channelID == 0 {
		t.Fatalf("ch-multi-url not found in configs")
	}

	// 强制渠道1首跳命中失败URL，避免随机首跳影响稳定性
	env.server.urlSelector.CooldownURL(channelID, upstreamShouldSkip.URL)

	w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model":    "gpt-4",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "from-ch2") {
		t.Fatalf("expected switch to next channel, got body: %s", w.Body.String())
	}
	ch1Fail := ch1FailCalls.Load()
	ch1Second := ch1SecondURLCalls.Load()
	ch2 := ch2Calls.Load()
	if ch1Fail < 1 {
		t.Fatalf("expected channel1 first URL attempted, got %d", ch1Fail)
	}
	if ch1Second != 0 {
		t.Fatalf("expected channel1 second URL not attempted on 5xx, got %d", ch1Second)
	}
	if ch2 < 1 {
		t.Fatalf("expected next channel attempted, got %d", ch2)
	}
	cooldowns, err := env.store.GetAllChannelCooldowns(ctx)
	if err != nil {
		t.Fatalf("GetAllChannelCooldowns: %v", err)
	}
	until, exists := cooldowns[channelID]
	if !exists {
		t.Fatalf("expected configured channel cooldown to be persisted for channel_id=%d", channelID)
	}
	if remaining := time.Until(until); remaining < 115*time.Second || remaining > 125*time.Second {
		t.Fatalf("configured channel cooldown remaining=%v, want about 2m", remaining)
	}
}

func TestProxy_MultiURLFallbackOn598_DoesNotChannelCooldownEarly(t *testing.T) {
	t.Parallel()

	var failCalls atomic.Int64
	var okCalls atomic.Int64

	// URL1: 首字节超时（598）
	upstreamTimeout := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		failCalls.Add(1)
		time.Sleep(120 * time.Millisecond)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"late\"}}]}\n\n")
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer upstreamTimeout.Close()

	// URL2: 正常返回
	upstreamOK := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		okCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"from-url2\"}}]}\n\n")
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer upstreamOK.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "ch-multi-url", models: "gpt-4", apiKey: "sk-1"},
	}, map[int]string{
		0: upstreamTimeout.URL + "\n" + upstreamOK.URL,
	})

	// 缩短首字节超时，稳定触发 598
	env.server.firstByteTimeout = 50 * time.Millisecond

	ctx := context.Background()
	configs, err := env.store.ListConfigs(ctx)
	if err != nil {
		t.Fatalf("ListConfigs: %v", err)
	}
	if len(configs) != 1 {
		t.Fatalf("expected 1 config, got %d", len(configs))
	}
	channelID := configs[0].ID

	// 强制 URL2 进入冷却，确保首跳先打到 timeout URL
	env.server.urlSelector.CooldownURL(channelID, upstreamOK.URL)

	w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model":    "gpt-4",
		"stream":   true,
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "from-url2") {
		t.Fatalf("expected fallback to url2 on 598, got body: %s", w.Body.String())
	}
	fail := failCalls.Load()
	ok := okCalls.Load()
	if fail < 1 || ok < 1 {
		t.Fatalf("expected both URLs attempted, failCalls=%d okCalls=%d", fail, ok)
	}

	// 关键断言：598 触发多URL内部回退成功后，不应残留渠道级冷却
	cooldowns, err := env.store.GetAllChannelCooldowns(ctx)
	if err != nil {
		t.Fatalf("GetAllChannelCooldowns: %v", err)
	}
	if _, exists := cooldowns[channelID]; exists {
		t.Fatalf("unexpected channel cooldown for multi-url fallback success, channel_id=%d", channelID)
	}
}

func TestProxy_StreamTimeoutDoesNotRetryAfterResponseCommit(t *testing.T) {
	t.Parallel()

	upstreamStarted := make(chan struct{})
	upstreamTimedOut := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n")
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		close(upstreamStarted)
		<-r.Context().Done()
	}))
	defer upstreamTimedOut.Close()

	var fallbackCalls atomic.Int64
	upstreamFallback := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fallbackCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"fallback\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer upstreamFallback.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "timeout", models: "gpt-4", priority: 100},
		{name: "fallback", models: "gpt-4", priority: 1},
	}, map[int]string{0: upstreamTimedOut.URL, 1: upstreamFallback.URL})
	env.server.streamTimeout = 50 * time.Millisecond

	response := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model":    "gpt-4",
		"stream":   true,
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	select {
	case <-upstreamStarted:
	default:
		t.Fatal("timed-out upstream was not selected first")
	}
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "partial") {
		t.Fatalf("response status=%d body=%q, want committed partial stream", response.Code, response.Body.String())
	}
	if calls := fallbackCalls.Load(); calls != 0 {
		t.Fatalf("fallback calls=%d, want 0 after response commit", calls)
	}
	entry := waitForProxyLog(t, env, "gpt-4")
	if entry.StatusCode != util.StatusStreamIncomplete || entry.UpstreamWebsocket {
		t.Fatalf("proxy log status/ws=%d/%v, want ordinary HTTP 599", entry.StatusCode, entry.UpstreamWebsocket)
	}
	configs, err := env.store.ListConfigs(context.Background())
	if err != nil {
		t.Fatalf("list configs: %v", err)
	}
	var timeoutChannelID int64
	for _, cfg := range configs {
		if cfg.Name == "timeout" {
			timeoutChannelID = cfg.ID
			break
		}
	}
	cooldowns, err := env.store.GetAllModelCooldowns(context.Background())
	if err != nil {
		t.Fatalf("get model cooldowns: %v", err)
	}
	if until := cooldowns[timeoutChannelID]["gpt-4"]; timeoutChannelID == 0 || !until.After(time.Now()) {
		t.Fatalf("ordinary HTTP 599 did not cool model: channel=%d cooldowns=%v", timeoutChannelID, cooldowns)
	}
}

func TestProxy_MultiURLFirstAttempt_UsesWeightedRandom(t *testing.T) {
	t.Parallel()

	var fastCalls atomic.Int64
	var slowCalls atomic.Int64

	upstreamFast := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fastCalls.Add(1)
		time.Sleep(5 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"from-fast","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer upstreamFast.Close()

	upstreamSlow := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		slowCalls.Add(1)
		time.Sleep(30 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"from-slow","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer upstreamSlow.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "ch-weighted-first", models: "gpt-4", apiKey: "sk-1"},
	}, map[int]string{
		0: upstreamSlow.URL + "\n" + upstreamFast.URL,
	})

	ctx := context.Background()
	configs, err := env.store.ListConfigs(ctx)
	if err != nil {
		t.Fatalf("ListConfigs: %v", err)
	}
	if len(configs) != 1 {
		t.Fatalf("expected 1 config, got %d", len(configs))
	}
	channelID := configs[0].ID

	// 预热EWMA，确保不是“未探索优先”分支
	env.server.urlSelector.RecordLatency(channelID, upstreamFast.URL, 5*time.Millisecond)
	env.server.urlSelector.RecordLatency(channelID, upstreamSlow.URL, 30*time.Millisecond)

	const rounds = 120
	for range rounds {
		w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
			"model":    "gpt-4",
			"messages": []map[string]string{{"role": "user", "content": "hi"}},
		}, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}
	}

	fast := fastCalls.Load()
	slow := slowCalls.Load()
	if fast <= slow {
		t.Fatalf("expected weighted random to prefer fast URL, fast=%d slow=%d", fast, slow)
	}
	if slow < 5 {
		t.Fatalf("expected slow URL to be selected sometimes (not deterministic first pick), fast=%d slow=%d", fast, slow)
	}
}

func TestProxy_KeyRetry_On401(t *testing.T) {
	t.Parallel()

	callCount := 0
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		auth := r.Header.Get("Authorization")
		if strings.Contains(auth, "sk-bad") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"invalid api key","type":"authentication_error"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"ok","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer upstream.Close()

	// 创建服务器并使用其 store
	srv := newInMemoryServer(t)
	store := srv.store

	ctx := context.Background()
	cfg := &model.Config{
		Name:         "ch1-multikey",
		URLs:         model.ChannelURLs{{URL: upstream.URL}},
		Priority:     100,
		Enabled:      true,
		ModelEntries: []model.ModelEntry{{Model: "gpt-4"}},
	}
	created, err := store.CreateConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("CreateConfig: %v", err)
	}
	err = store.CreateAPIKeysBatch(ctx, []*model.APIKey{
		{ChannelID: created.ID, KeyIndex: 0, APIKey: "sk-bad", Priority: 1},
		{ChannelID: created.ID, KeyIndex: 1, APIKey: "sk-good"},
	})
	if err != nil {
		t.Fatalf("CreateAPIKeysBatch: %v", err)
	}

	injectAPIToken(srv.authService, "test-api-key", 0, 1)

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	srv.SetupRoutes(engine)

	w := doProxyRequest(t, engine, "/v1/chat/completions", map[string]any{
		"model":    "gpt-4",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (key retry to sk-good), got %d: %s", w.Code, w.Body.String())
	}
	if callCount < 2 {
		t.Fatalf("expected at least 2 upstream calls (key retry), got %d", callCount)
	}
}

func TestProxy_APIKeyModelAllowlistRoutesToMatchingKey(t *testing.T) {
	t.Parallel()

	var gotAuth atomic.Value
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth.Store(r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"ok","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	created, err := srv.store.CreateConfig(context.Background(), &model.Config{
		Name: "key-model-scope", URLs: model.ChannelURLs{{URL: upstream.URL}}, Priority: 100, Enabled: true,
		ModelEntries: []model.ModelEntry{{Model: "gpt-5"}, {Model: "qwen3"}},
	})
	if err != nil {
		t.Fatalf("CreateConfig: %v", err)
	}
	if err := srv.store.CreateAPIKeysBatch(context.Background(), []*model.APIKey{
		{ChannelID: created.ID, KeyIndex: 0, APIKey: "sk-gpt", Priority: 100, AllowedModels: []string{"gpt-5"}},
		{ChannelID: created.ID, KeyIndex: 1, APIKey: "sk-qwen", Priority: -10, AllowedModels: []string{"qwen3"}},
	}); err != nil {
		t.Fatalf("CreateAPIKeysBatch: %v", err)
	}

	injectAPIToken(srv.authService, "test-api-key", 0, 1)
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	srv.SetupRoutes(engine)

	w := doProxyRequest(t, engine, "/v1/chat/completions", map[string]any{
		"model": "qwen3", "messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if auth, _ := gotAuth.Load().(string); !strings.Contains(auth, "sk-qwen") {
		t.Fatalf("request used %q, want qwen-scoped key", auth)
	}
}

func TestProxy_NoKeyForModelSkipsChannelWithoutCooldown(t *testing.T) {
	t.Parallel()

	var firstCalls atomic.Int64
	firstUpstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firstCalls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer firstUpstream.Close()
	secondUpstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"ok","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer secondUpstream.Close()

	srv := newInMemoryServer(t)
	ctx := context.Background()
	first, err := srv.store.CreateConfig(ctx, &model.Config{
		Name: "no-matching-key", URLs: model.ChannelURLs{{URL: firstUpstream.URL}}, Priority: 200, Enabled: true,
		ModelEntries: []model.ModelEntry{{Model: "gpt-5"}},
	})
	if err != nil {
		t.Fatalf("CreateConfig(first): %v", err)
	}
	second, err := srv.store.CreateConfig(ctx, &model.Config{
		Name: "fallback", URLs: model.ChannelURLs{{URL: secondUpstream.URL}}, Priority: 100, Enabled: true,
		ModelEntries: []model.ModelEntry{{Model: "gpt-5"}},
	})
	if err != nil {
		t.Fatalf("CreateConfig(second): %v", err)
	}
	if err := srv.store.CreateAPIKeysBatch(ctx, []*model.APIKey{
		{ChannelID: first.ID, KeyIndex: 0, APIKey: "sk-qwen", Priority: -10, AllowedModels: []string{"qwen3"}},
		{ChannelID: second.ID, KeyIndex: 0, APIKey: "sk-fallback"},
	}); err != nil {
		t.Fatalf("CreateAPIKeysBatch: %v", err)
	}

	injectAPIToken(srv.authService, "test-api-key", 0, 1)
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	srv.SetupRoutes(engine)
	w := doProxyRequest(t, engine, "/v1/chat/completions", map[string]any{
		"model": "gpt-5", "messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected fallback 200, got %d: %s", w.Code, w.Body.String())
	}
	if firstCalls.Load() != 0 {
		t.Fatalf("incompatible key reached upstream %d times", firstCalls.Load())
	}
	cooldowns, err := srv.store.GetAllChannelCooldowns(ctx)
	if err != nil {
		t.Fatalf("GetAllChannelCooldowns: %v", err)
	}
	if until, ok := cooldowns[first.ID]; ok && until.After(time.Now()) {
		t.Fatalf("model-scoped miss cooled the whole channel until %s", until)
	}
}

func TestProxy_AllChannelsExhausted(t *testing.T) {
	t.Parallel()

	callCount1 := 0
	upstream1 := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount1++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"internal server error"}`))
	}))
	defer upstream1.Close()

	callCount2 := 0
	upstream2 := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount2++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"internal server error"}`))
	}))
	defer upstream2.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "ch1", models: "gpt-4", apiKey: "sk-1", priority: 100},
		{name: "ch2", models: "gpt-4", apiKey: "sk-2", priority: 50},
	}, map[int]string{0: upstream1.URL, 1: upstream2.URL})

	w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model":    "gpt-4",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	// 所有渠道失败时应返回最后一个错误状态码
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", w.Code, w.Body.String())
	}
	// 关键行为：必须耗尽所有可用渠道，而不是只尝试第一个就返回（避免“假绿”）。
	if callCount1 < 1 || callCount2 < 1 {
		t.Fatalf("expected to try all channels at least once, got upstream1=%d upstream2=%d", callCount1, callCount2)
	}
}

// TestProxy_SingleChannel5xx_SkipsSummaryLog 验证：模型仅有 1 个渠道时，
// 渠道级失败日志已完整反映失败原因，不再写"系统/exhausted backends"汇总日志。
func TestProxy_SingleChannel5xx_SkipsSummaryLog(t *testing.T) {
	t.Parallel()

	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"internal server error"}`))
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "only-ch", models: "gpt-4", apiKey: "sk-1", priority: 100},
	}, map[int]string{0: upstream.URL})

	w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model":    "gpt-4",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", w.Code, w.Body.String())
	}

	// 等待异步日志落盘：至少要看到 1 条渠道级失败日志（ChannelID 非零）
	ctx := context.Background()
	since := time.Now().Add(-time.Minute)
	deadline := time.Now().Add(2 * time.Second)
	var logs []*model.LogEntry
	for time.Now().Before(deadline) {
		got, err := env.store.ListLogs(ctx, since, 20, 0, &model.LogFilter{LogSource: model.LogSourceProxy})
		if err != nil {
			t.Fatalf("ListLogs failed: %v", err)
		}
		hasChannelLog := false
		for _, e := range got {
			if e.ChannelID != 0 {
				hasChannelLog = true
				break
			}
		}
		if hasChannelLog {
			logs = got
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if logs == nil {
		t.Fatalf("expected at least one channel-level proxy log within deadline")
	}

	// 关键断言：不能出现"汇总日志"（ChannelID=0 的 Proxy 日志）
	for _, e := range logs {
		if e.ChannelID == 0 {
			t.Fatalf("unexpected summary log (ChannelID=0, message=%q, status=%d) for single-channel failure",
				e.Message, e.StatusCode)
		}
	}
}

func TestProxy_ClientCancel_Returns499(t *testing.T) {
	t.Parallel()

	// 上游延迟响应
	upstreamStarted := make(chan struct{})
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-upstreamStarted:
			// already closed
		default:
			close(upstreamStarted)
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(2 * time.Second):
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "ch1", models: "gpt-4", apiKey: "sk-1"},
	}, map[int]string{0: upstream.URL})

	// 创建可取消的请求
	ctx, cancel := context.WithCancel(context.Background())
	body, _ := json.Marshal(map[string]any{
		"model":    "gpt-4",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	req = req.WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-api-key")

	// 等上游请求真的发出后再取消，避免“还没发出去就 cancel”导致语义漂移
	go func() {
		select {
		case <-upstreamStarted:
		case <-time.After(1 * time.Second):
		}
		cancel()
	}()

	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	// 客户端取消应返回 499 或超时相关状态
	if w.Code != StatusClientClosedRequest && w.Code != http.StatusGatewayTimeout {
		t.Fatalf("expected 499 or 504, got %d: %s", w.Code, w.Body.String())
	}
}

// 下游响应未提交时，中断跳过当前渠道所有 URL/Key，不施加故障冷却。
func TestProxy_OperatorAbort_SkipsChannelWithoutCooldown(t *testing.T) {
	t.Parallel()
	upstreamStarted := make(chan struct{})
	var startOnce sync.Once
	var primaryHits atomic.Int64
	primary := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryHits.Add(1)
		startOnce.Do(func() { close(upstreamStarted) })
		select {
		case <-r.Context().Done(): // 被中断：不提交任何响应
		case <-time.After(5 * time.Second):
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer primary.Close()

	var backupHits atomic.Int64
	backup := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		backupHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chat-1","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer backup.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "abort-primary", models: "gpt-abort", apiKey: "sk-1", priority: 100, retryOtherKeysOnFailure: true},
		{name: "abort-backup", models: "gpt-abort", apiKey: "sk-2", priority: 50},
	}, map[int]string{0: primary.URL, 1: backup.URL})

	configs, err := env.store.ListConfigs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, cfg := range configs {
		if cfg.Name != "abort-primary" {
			continue
		}
		cfg.URLs = model.ChannelURLs{{URL: primary.URL}, {URL: primary.URL + "/alternate"}}
		if _, err := env.store.UpdateConfig(context.Background(), cfg.ID, cfg); err != nil {
			t.Fatal(err)
		}
		if err := env.store.CreateAPIKeysBatch(context.Background(), []*model.APIKey{{ChannelID: cfg.ID, KeyIndex: 1, APIKey: "sk-extra"}}); err != nil {
			t.Fatal(err)
		}
	}
	env.server.InvalidateChannelListCache()

	body, _ := json.Marshal(map[string]any{
		"model":    "gpt-abort",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-api-key")

	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		env.engine.ServeHTTP(w, req)
	}()

	select {
	case <-upstreamStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream never received the request")
	}

	// 等到中断句柄登记后再触发，否则中断会打空
	var aborted bool
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		for _, active := range env.server.activeRequests.List() {
			if active.Abortable && env.server.activeRequests.Abort(active.ID) {
				aborted = true
				break
			}
		}
		if aborted {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !aborted {
		t.Fatal("no abortable active request appeared")
	}

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("proxy request did not finish after abort")
	}

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200 (failover to backup); body=%s", w.Code, w.Body.String())
	}
	if w.Code == StatusClientClosedRequest {
		t.Fatal("operator abort must not be classified as client cancellation")
	}
	if got := backupHits.Load(); got != 1 {
		t.Fatalf("backup channel hits=%d, want 1", got)
	}

	if got := primaryHits.Load(); got != 1 {
		t.Fatalf("primary hits=%d, want 1: abort must skip remaining URLs and keys", got)
	}
	channels, err := env.store.ListConfigs(context.Background())
	if err != nil {
		t.Fatalf("ListConfigs: %v", err)
	}
	var primaryID int64
	for _, ch := range channels {
		if ch.Name == "abort-primary" {
			primaryID = ch.ID
		}
	}
	if primaryID == 0 {
		t.Fatal("primary channel not found")
	}

	cooldowns, err := env.store.GetAllModelCooldowns(context.Background())
	if err != nil {
		t.Fatalf("GetAllModelCooldowns: %v", err)
	}
	until, ok := cooldowns[primaryID]["gpt-abort"]
	if ok && until.After(time.Now()) {
		t.Fatalf("operator abort must not cool the channel, got %v (ok=%v)", until, ok)
	}
}

func TestProxy_OperatorAbort_NonStreamPingOnlyHTTP(t *testing.T) {
	t.Parallel()
	upstreamStarted := make(chan struct{})
	primary := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		close(upstreamStarted)
		flusher := w.(http.Flusher)
		_, _ = io.WriteString(w, ": PING\n\n")
		flusher.Flush()
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-ticker.C:
				_, _ = io.WriteString(w, ": PING\n\n")
				flusher.Flush()
			}
		}
	}))
	defer primary.Close()

	var backupHits atomic.Int64
	backup := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		backupHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chat-1","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer backup.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "abort-nonstream-primary", models: "gpt-abort-nonstream", apiKey: "sk-1", priority: 100},
		{name: "abort-nonstream-backup", models: "gpt-abort-nonstream", apiKey: "sk-2", priority: 50},
	}, map[int]string{0: primary.URL, 1: backup.URL})

	body, _ := json.Marshal(map[string]any{
		"model": "gpt-abort-nonstream", "stream": false,
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-api-key")
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		env.engine.ServeHTTP(w, req)
	}()
	select {
	case <-upstreamStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("non-stream upstream did not start")
	}
	time.Sleep(100 * time.Millisecond)
	var aborted bool
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		for _, active := range env.server.activeRequests.List() {
			if active.Abortable && env.server.activeRequests.Abort(active.ID) {
				aborted = true
				break
			}
		}
		if aborted {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !aborted {
		t.Fatal("no abortable non-stream request appeared")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("non-stream proxy request did not finish after abort")
	}
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200 after failover; body=%q", w.Code, w.Body.String())
	}
	if got := strings.TrimSpace(w.Body.String()); got != `{"id":"chat-1","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}` {
		t.Fatalf("non-stream response contains partial primary data: %q", got)
	}
	if got := backupHits.Load(); got != 1 {
		t.Fatalf("backup hits=%d, want 1", got)
	}
}

// 响应已提交给客户端后再中断：按契约禁止网关内部切换或重放，正确收场是 599
// （流式中断），不施加冷却，也不能换渠道重发。
func TestProxy_OperatorAbort_AfterCommitDoesNotSwitchChannel(t *testing.T) {
	t.Parallel()
	primary := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"id\":\"chat-1\",\"choices\":[{\"delta\":{\"content\":\"hel\"}}]}\n\n")
		w.(http.Flusher).Flush()
		// 只发一半就挂住，等中断把连接掐掉——流没有终止事件
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	defer primary.Close()

	var backupHits atomic.Int64
	backup := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		backupHits.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer backup.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "abort-committed-primary", models: "gpt-abort-stream", apiKey: "sk-1", priority: 100},
		{name: "abort-committed-backup", models: "gpt-abort-stream", apiKey: "sk-2", priority: 50},
	}, map[int]string{0: primary.URL, 1: backup.URL})

	body, _ := json.Marshal(map[string]any{
		"model":    "gpt-abort-stream",
		"stream":   true,
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-api-key")

	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		env.engine.ServeHTTP(w, req)
	}()

	// ClientFirstByteTime > 0 = 首个客户端可见事件已写出，此刻响应对下游已提交
	var aborted bool
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		for _, active := range env.server.activeRequests.List() {
			if active.ClientFirstByteTime > 0 && active.Abortable && env.server.activeRequests.Abort(active.ID) {
				aborted = true
				break
			}
		}
		if aborted {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !aborted {
		t.Fatal("no committed abortable active request appeared")
	}

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("proxy request did not finish after abort")
	}

	if got := backupHits.Load(); got != 0 {
		t.Fatalf("backup channel hits=%d, want 0: a committed stream must not be replayed", got)
	}

	entry := waitForProxyLog(t, env, "gpt-abort-stream")
	if entry.StatusCode != util.StatusStreamIncomplete {
		t.Fatalf("log status=%d, want %d (stream interrupted); message=%s",
			entry.StatusCode, util.StatusStreamIncomplete, entry.Message)
	}

	channels, err := env.store.ListConfigs(context.Background())
	if err != nil {
		t.Fatalf("ListConfigs: %v", err)
	}
	var primaryID int64
	for _, ch := range channels {
		if ch.Name == "abort-committed-primary" {
			primaryID = ch.ID
		}
	}
	if primaryID == 0 {
		t.Fatal("primary channel not found")
	}

	cooldowns, err := env.store.GetAllModelCooldowns(context.Background())
	if err != nil {
		t.Fatalf("GetAllModelCooldowns: %v", err)
	}
	if until, ok := cooldowns[primaryID]["gpt-abort-stream"]; ok && until.After(time.Now()) {
		t.Fatalf("operator abort must not cool the channel, got %v (ok=%v)", until, ok)
	}
}

// abortOnCommitDownstream 在 deferredResponseWriter.Commit() 复制响应头时触发中断，
// 精确命中"提交动作已开始、但首字节尚未写出下游"的窗口。
// gin 会在代理注册活跃请求前就调用 Header()，所以 trigger 必须重试到真正中断为止。
type abortOnCommitDownstream struct {
	http.ResponseWriter
	trigger      func() bool
	armed        atomic.Bool
	wroteHdr     atomic.Bool
	bodyBytes    atomic.Int64
	bytesAtAbort atomic.Int64
}

func (w *abortOnCommitDownstream) Header() http.Header {
	if w.trigger != nil && !w.armed.Load() && w.trigger() {
		w.armed.Store(true)
		w.bytesAtAbort.Store(w.bodyBytes.Load())
	}
	return w.ResponseWriter.Header()
}

func (w *abortOnCommitDownstream) WriteHeader(status int) {
	w.wroteHdr.Store(true)
	w.ResponseWriter.WriteHeader(status)
}

func (w *abortOnCommitDownstream) Write(p []byte) (int, error) {
	w.bodyBytes.Add(int64(len(p)))
	return w.ResponseWriter.Write(p)
}

func (w *abortOnCommitDownstream) Flush() {
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}

func (w *abortOnCommitDownstream) SetWriteDeadline(time.Time) error { return nil }

// 中断恰好落在首字节提交窗口时，下游实际未收到任何字节，必须仍按"未提交"切换渠道，
// 而不是把空的 200 留给客户端。
func TestProxy_OperatorAbort_AtCommitBoundaryStillFailsOver(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		stream     bool
		translated bool
	}{
		{name: "passthrough_stream", stream: true},

		{name: "passthrough_nonstream"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.stream {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"hello"}}]}`+"\n\n")
					w.(http.Flusher).Flush()
					<-r.Context().Done()
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"primary","choices":[{"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}]}`)
			}))
			defer upstream.Close()

			var backupHits atomic.Int64
			backup := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				backupHits.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"resp-backup","choices":[{"message":{"role":"assistant","content":"backup"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
			}))
			defer backup.Close()

			env := setupProxyTestEnv(t, []testChannel{
				{name: "abort-at-commit", models: "abort-commit", priority: 100},
				{name: "backup", models: "abort-commit", priority: 50},
			}, map[int]string{0: upstream.URL, 1: backup.URL})

			var probe *abortOnCommitDownstream
			done := make(chan struct{})
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(done)
				probe = &abortOnCommitDownstream{ResponseWriter: w}
				probe.trigger = func() bool {
					for _, entry := range env.server.activeRequests.List() {
						if env.server.activeRequests.Abort(entry.ID) {
							return true
						}
					}
					return false
				}
				env.engine.ServeHTTP(probe, r)
			}))
			defer proxy.Close()

			path := "/v1/chat/completions"
			if tc.translated {
				path = "/v1/messages"
			}
			body := fmt.Sprintf(`{"model":"abort-commit","stream":%t,"max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`, tc.stream)
			req, err := http.NewRequest(http.MethodPost, proxy.URL+path, strings.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer test-api-key")
			client := &http.Client{Timeout: 5 * time.Second}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = resp.Body.Close() }()
			responseBody, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			<-done
			if !probe.armed.Load() {
				t.Fatal("abort never triggered at commit boundary")
			}
			if probe.bytesAtAbort.Load() != 0 {
				t.Fatalf("downstream received %d bytes before abort", probe.bytesAtAbort.Load())
			}
			if backupHits.Load() != 1 {
				t.Fatalf("backup hits=%d, want 1; body=%q", backupHits.Load(), responseBody)
			}
			var payload struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(responseBody, &payload); err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != http.StatusOK || payload.ID != "resp-backup" {
				t.Fatalf("status=%d body=%s, want backup response", resp.StatusCode, responseBody)
			}
		})
	}
}

// 中断落在 WriteHeader 成功之后：响应头已真正写给下游，Commit 必须置 committed=true。
// 旧代码用 responseWriteAborted（取消状态反推）会在此窗口误判为"未提交"，导致非法换渠。
// 本测试用 cancelOnWriteHeader 在下游 WriteHeader 成功后立即取消 ctx，精确命中
// "写出成功、但取消已可见"的边界。
func TestDeferredCommit_CancelAfterHeaderDelivered_StillCommits(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancelCause(context.Background())
	raw := &cancelOnWriteHeader{ResponseRecorder: httptest.NewRecorder(), cancel: cancel}

	cw, stopWrites := newCancelableResponseWriter(ctx, raw)
	defer stopWrites()

	dw := newDeferredResponseWriter(cw)
	dw.WriteHeader(http.StatusOK)

	err := dw.Commit()
	if err != nil {
		t.Fatalf("Commit() returned error %v after header was already delivered; must succeed", err)
	}
	if !dw.Committed() {
		t.Fatal("Committed() is false after header was delivered; must be true")
	}
	if raw.Code != http.StatusOK {
		t.Fatalf("raw recorder status=%d, want 200", raw.Code)
	}
}

// cancelOnWriteHeader 在 WriteHeader 完成后立即取消 ctx。
// 模拟"响应头刚写给下游、取消紧随其后"的精确竞态。
type cancelOnWriteHeader struct {
	*httptest.ResponseRecorder
	cancel context.CancelCauseFunc
}

func (w *cancelOnWriteHeader) WriteHeader(status int) {
	w.ResponseRecorder.WriteHeader(status)
	if w.cancel != nil {
		w.cancel(errOperatorAbort)
		w.cancel = nil // 只触发一次
	}
}

// cancelBeforeWriteHeader 在包装链传递 WriteHeader 时取消，精确覆盖
// Commit 前置检查已通过、但 cancelableResponseWriter 尚未接受写头的窗口。
type cancelBeforeWriteHeader struct {
	http.ResponseWriter
	cancel context.CancelCauseFunc
}

func (w *cancelBeforeWriteHeader) WriteHeader(status int) {
	w.cancel(errOperatorAbort)
	w.ResponseWriter.WriteHeader(status)
}

func (w *cancelBeforeWriteHeader) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func TestDeferredCommit_CancelBeforeHeaderDelivered_ReturnsError(t *testing.T) {
	t.Parallel()
	for _, duringWriteHeader := range []bool{false, true} {
		t.Run(fmt.Sprintf("during_write_header=%t", duringWriteHeader), func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			raw := httptest.NewRecorder()
			cw, stopWrites := newCancelableResponseWriter(ctx, raw)
			defer stopWrites()

			var target http.ResponseWriter = cw
			if duringWriteHeader {
				target = &cancelBeforeWriteHeader{ResponseWriter: cw, cancel: cancel}
			}
			dw := newDeferredResponseWriter(target)
			dw.Header().Set("X-Primary", "must-not-be-committed")
			dw.WriteHeader(http.StatusAccepted)
			if _, err := dw.Write([]byte(`{"id":"primary"}`)); err != nil {
				t.Fatal(err)
			}
			if !duringWriteHeader {
				cancel(errOperatorAbort)
			}

			if err := dw.Commit(); !errors.Is(err, errOperatorAbort) {
				t.Fatalf("Commit() error=%v, want operator abort", err)
			}
			if dw.Committed() {
				t.Fatal("Committed() is true but header was never delivered")
			}
			// 同一底层连接仍能写入完整备用响应，且主渠道没有提交状态或正文。
			raw.Header().Del("X-Primary")
			raw.WriteHeader(http.StatusCreated)
			_, _ = raw.Write([]byte(`{"id":"backup"}`))
			response := raw.Result()
			defer func() { _ = response.Body.Close() }()
			var payload struct {
				ID string `json:"id"`
			}
			if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != http.StatusCreated || response.Header.Get("X-Primary") != "" || payload.ID != "backup" {
				t.Fatalf("unexpected fallback response: status=%d header=%v payload=%+v", response.StatusCode, response.Header, payload)
			}
		})
	}
}

// blockedDownstream 模拟客户端不读取时阻塞的 Write/Flush，写截止时间必须能解除阻塞。
type blockedDownstream struct {
	*httptest.ResponseRecorder
	blockFlush  bool
	started     chan struct{}
	released    chan struct{}
	startOnce   sync.Once
	releaseOnce sync.Once
}

func (w *blockedDownstream) block() {
	w.startOnce.Do(func() { close(w.started) })
	<-w.released
}

func (w *blockedDownstream) Write(p []byte) (int, error) {
	if w.blockFlush {
		return w.ResponseRecorder.Write(p)
	}
	w.block()
	return 0, os.ErrDeadlineExceeded
}

func (w *blockedDownstream) Flush() { w.block() }

func (w *blockedDownstream) SetWriteDeadline(deadline time.Time) error {
	if !deadline.IsZero() && !deadline.After(time.Now()) {
		w.releaseOnce.Do(func() { close(w.released) })
	}
	return nil
}

func TestProxy_OperatorAbort_UnblocksDownstream(t *testing.T) {
	t.Parallel()
	for _, translated := range []bool{false} {
		for _, flush := range []bool{false, true} {
			t.Run(fmt.Sprintf("translated=%v/flush=%v", translated, flush), func(t *testing.T) {
				upstreamClosed := make(chan struct{})
				upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					if translated {
						_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"abort-blocked\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"hello\"}}\n\n")
					} else {
						_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"hello"}}]}`+"\n\n")
					}
					w.(http.Flusher).Flush()
					<-r.Context().Done()
					close(upstreamClosed)
				}))
				defer upstream.Close()
				var backupHits atomic.Int64
				backup := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { backupHits.Add(1); w.WriteHeader(502) }))
				defer backup.Close()
				upstreamProtocol := "openai"
				if translated {
					upstreamProtocol = "anthropic"
				}
				env := setupProxyTestEnv(t, []testChannel{
					{name: "blocked", upstreamProtocol: upstreamProtocol, models: "abort-blocked", priority: 100},
					{name: "backup", models: "abort-blocked", priority: 50},
				}, map[int]string{0: upstream.URL, 1: backup.URL})
				requestPath, requestBody := "/v1/chat/completions", `{"model":"abort-blocked","stream":true,"messages":[{"role":"user","content":"hi"}]}`
				if translated {
					requestPath, requestBody = "/v1/responses", `{"model":"abort-blocked","stream":true,"input":"hi"}`
				}
				req := httptest.NewRequest(http.MethodPost, requestPath, strings.NewReader(requestBody))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Authorization", "Bearer test-api-key")
				w := &blockedDownstream{ResponseRecorder: httptest.NewRecorder(), blockFlush: flush, started: make(chan struct{}), released: make(chan struct{})}
				done := make(chan struct{})
				go func() { defer close(done); env.engine.ServeHTTP(w, req) }()
				defer func() { _ = w.SetWriteDeadline(time.Now()); <-done }()
				select {
				case <-w.started:
				case <-time.After(5 * time.Second):
					t.Fatal("downstream never blocked")
				}
				active := env.server.activeRequests.List()
				if len(active) != 1 || !env.server.activeRequests.Abort(active[0].ID) {
					t.Fatal("no abortable request")
				}
				select {
				case <-done:
				case <-time.After(time.Second):
					t.Fatal("abort did not unblock downstream")
				}
				select {
				case <-upstreamClosed:
				case <-time.After(time.Second):
					t.Fatal("upstream still connected")
				}
				if backupHits.Load() != 0 {
					t.Fatal("committed response retried on backup")
				}
				if len(env.server.activeRequests.List()) != 0 {
					t.Fatal("aborted request still active")
				}
				entry := waitForProxyLog(t, env, "abort-blocked")
				if entry.StatusCode != util.StatusStreamIncomplete {
					t.Fatalf("status=%d, want interrupted", entry.StatusCode)
				}
			})
		}
	}
}

func TestProxy_ModelNotAllowed_Returns403(t *testing.T) {
	t.Parallel()

	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "ch1", models: "gpt-4,gpt-3.5-turbo", apiKey: "sk-1"},
	}, map[int]string{0: upstream.URL})

	// 限制 token 只能使用 gpt-3.5-turbo
	tokenHash := model.HashToken("test-api-key")
	env.server.authService.authTokensMux.Lock()
	env.server.authService.authTokenModels[tokenHash] = []string{"gpt-3.5-turbo"}
	env.server.authService.authTokensMux.Unlock()

	w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model":    "gpt-4",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", w.Code, w.Body.String())
	}
}

func TestProxy_ChannelRestriction_DenySkipsListedChannel(t *testing.T) {
	t.Parallel()

	var disallowedHits atomic.Int32
	disallowedUpstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		disallowedHits.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"disallowed"}`))
	}))
	defer disallowedUpstream.Close()

	var allowedHits atomic.Int32
	allowedUpstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		allowedHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"allowed","choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer allowedUpstream.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "disallowed-high-priority", models: "gpt-4", apiKey: "sk-disallowed", priority: 100},
		{name: "allowed-low-priority", models: "gpt-4", apiKey: "sk-allowed", priority: 10},
	}, map[int]string{0: disallowedUpstream.URL, 1: allowedUpstream.URL})

	configs, err := env.store.ListConfigs(context.Background())
	if err != nil {
		t.Fatalf("ListConfigs failed: %v", err)
	}
	var allowedID int64
	var disallowedID int64
	for _, cfg := range configs {
		switch cfg.Name {
		case "allowed-low-priority":
			allowedID = cfg.ID
		case "disallowed-high-priority":
			disallowedID = cfg.ID
		}
	}
	if allowedID == 0 || disallowedID == 0 {
		t.Fatalf("channel ids not found: allowed=%d disallowed=%d", allowedID, disallowedID)
	}

	tokenHash := model.HashToken("test-api-key")
	env.server.authService.authTokensMux.Lock()
	env.server.authService.authTokenChannels[tokenHash] = mustChannelRestriction(t, model.ChannelRestrictionModeDeny, disallowedID)
	env.server.authService.authTokensMux.Unlock()

	w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model":    "gpt-4",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if got := allowedHits.Load(); got != 1 {
		t.Fatalf("allowed upstream hits=%d, want 1", got)
	}
	if got := disallowedHits.Load(); got != 0 {
		t.Fatalf("disallowed upstream hits=%d, want 0", got)
	}
}

func TestProxy_ChannelRestriction_Returns403WhenNoAllowedCandidate(t *testing.T) {
	t.Parallel()

	var upstreamHits atomic.Int32
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "only-channel", models: "gpt-4", apiKey: "sk-1"},
	}, map[int]string{0: upstream.URL})

	tokenHash := model.HashToken("test-api-key")
	env.server.authService.authTokensMux.Lock()
	env.server.authService.authTokenChannels[tokenHash] = mustChannelRestriction(t, model.ChannelRestrictionModeAllow, 999999)
	env.server.authService.authTokensMux.Unlock()

	w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model":    "gpt-4",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", w.Code, w.Body.String())
	}
	if got := upstreamHits.Load(); got != 0 {
		t.Fatalf("upstream hits=%d, want 0", got)
	}
}

func TestProxy_ChannelRestriction_PreservesNoCandidateResponse(t *testing.T) {
	t.Parallel()

	var upstreamHits atomic.Int32
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "only-channel", models: "gpt-3.5-turbo", apiKey: "sk-1"},
	}, map[int]string{0: upstream.URL})

	configs, err := env.store.ListConfigs(context.Background())
	if err != nil {
		t.Fatalf("ListConfigs failed: %v", err)
	}
	if len(configs) != 1 {
		t.Fatalf("expected 1 config, got %d", len(configs))
	}

	tokenHash := model.HashToken("test-api-key")
	env.server.authService.authTokensMux.Lock()
	env.server.authService.authTokenChannels[tokenHash] = mustChannelRestriction(t, model.ChannelRestrictionModeAllow, configs[0].ID)
	env.server.authService.authTokensMux.Unlock()

	w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model":    "gpt-4",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", w.Code, w.Body.String())
	}
	if got := upstreamHits.Load(); got != 0 {
		t.Fatalf("upstream hits=%d, want 0", got)
	}
}

func TestProxy_CostLimitExceeded_Returns429(t *testing.T) {
	t.Parallel()

	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "ch1", models: "gpt-4", apiKey: "sk-1"},
	}, map[int]string{0: upstream.URL})

	// 设置 token 费用已超限
	tokenHash := model.HashToken("test-api-key")
	env.server.authService.authTokensMux.Lock()
	env.server.authService.authTokenCostLimits[tokenHash] = tokenCostLimit{
		usedMicroUSD:  200_000, // $0.20
		limitMicroUSD: 100_000, // $0.10 限额
	}
	env.server.authService.authTokensMux.Unlock()

	w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model":    "gpt-4",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d: %s", w.Code, w.Body.String())
	}

	// 验证错误包含 cost_limit_exceeded
	body := w.Body.String()
	if !strings.Contains(body, "cost_limit_exceeded") {
		t.Fatalf("expected 'cost_limit_exceeded' in body: %s", body)
	}
}

func TestProxy_NoChannels_Returns503(t *testing.T) {
	t.Parallel()

	// 创建没有渠道的环境
	srv := newInMemoryServer(t)
	injectAPIToken(srv.authService, "test-api-key", 0, 1)

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	srv.SetupRoutes(engine)

	w := doProxyRequest(t, engine, "/v1/chat/completions", map[string]any{
		"model":    "gpt-4",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", w.Code, w.Body.String())
	}
}

func TestProxy_SSEErrorEvent_TriggersCooldown(t *testing.T) {
	t.Parallel()

	// 模拟上游：返回 200 + SSE 但包含 error 事件
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		// 先正常发几个 chunk，然后发 error
		// 这里首个 chunk 故意做大于 SSEBufferSize，确保代理已经向客户端提交过响应，
		// 后续 error event 才会落到“只能冷却，不能同请求重试”的路径。
		largeContent := strings.Repeat("Hi", SSEBufferSize)
		chunks := []string{
			fmt.Sprintf(`data: {"choices":[{"delta":{"content":"%s"}}]}`, largeContent),
			`event: error` + "\n" + `data: {"error":{"message":"rate limit exceeded","type":"rate_limit_error"}}`,
		}
		for _, chunk := range chunks {
			_, _ = fmt.Fprintf(w, "%s\n\n", chunk)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "ch1", models: "gpt-4", apiKey: "sk-1"},
	}, map[int]string{0: upstream.URL})

	ctx := context.Background()
	// 先拿到渠道ID（避免硬编码）
	var channelID int64
	configs, err := env.store.ListConfigs(ctx)
	if err != nil {
		t.Fatalf("ListConfigs: %v", err)
	}
	for _, cfg := range configs {
		if cfg.Name == "ch1" {
			channelID = cfg.ID
			break
		}
	}
	if channelID == 0 {
		t.Fatalf("channel ch1 not found")
	}

	// 预期：请求前没有渠道冷却（否则测试语义不成立）
	beforeCooldowns, err := env.store.GetAllChannelCooldowns(ctx)
	if err != nil {
		t.Fatalf("GetAllChannelCooldowns(before): %v", err)
	}
	if _, exists := beforeCooldowns[channelID]; exists {
		t.Fatalf("expected no channel cooldown before request, but found one for channel_id=%d", channelID)
	}

	w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model":    "gpt-4",
		"stream":   true,
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	// SSE error 事件的处理：HTTP 状态码已经是 200（头部已发送），
	// 但内部应触发冷却逻辑。测试验证响应不崩溃。
	// 响应仍是 200（因为 header 已发送），但内部会记录冷却
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (header already sent), got %d: %s", w.Code, w.Body.String())
	}

	// 关键断言：SSE error 事件必须触发冷却副作用（单Key渠道会升级为渠道级冷却）。
	afterCooldowns, err := env.store.GetAllChannelCooldowns(ctx)
	if err != nil {
		t.Fatalf("GetAllChannelCooldowns(after): %v", err)
	}
	until, exists := afterCooldowns[channelID]
	if !exists {
		t.Fatalf("expected channel cooldown to be set after SSE error event, channel_id=%d", channelID)
	}
	if time.Until(until) <= 0 {
		t.Fatalf("expected channel cooldown until in the future, got %v", until)
	}
}

func TestProxy_AnthropicSSERateLimitUsesUnifiedResetForModelCooldown(t *testing.T) {
	t.Parallel()
	resetAt := time.Now().Add(2 * time.Hour).Truncate(time.Second)
	var upstreamCalls atomic.Int32
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Anthropic-Ratelimit-Unified-Reset", strconv.FormatInt(resetAt.Unix(), 10))
		_, _ = fmt.Fprint(w, "event: error\n")
		_, _ = fmt.Fprint(w, `data: {"type":"error","error":{"type":"rate_limit_error","message":"This request would exceed your account's rate limit. Please try again later."}}`+"\n\n")
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{{
		name: "anthropic-sse-rate-limit", upstreamProtocol: util.ProtocolAnthropic,
		models: "claude-sonnet-4-6", apiKey: "sk-ant", retryOtherKeysOnFailure: true,
	}}, map[int]string{0: upstream.URL})
	configs, err := env.store.ListConfigs(context.Background())
	if err != nil || len(configs) != 1 {
		t.Fatalf("ListConfigs()=(%d, %v), want one channel", len(configs), err)
	}
	if err := env.store.CreateAPIKeysBatch(context.Background(), []*model.APIKey{{
		ChannelID: configs[0].ID, KeyIndex: 1, APIKey: "sk-ant-second",
	}}); err != nil {
		t.Fatalf("CreateAPIKeysBatch: %v", err)
	}

	response := doProxyRequest(t, env.engine, "/v1/messages", map[string]any{
		"model": "claude-sonnet-4-6", "stream": true, "max_tokens": 16,
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)
	if response.Code != http.StatusTooManyRequests {
		t.Fatalf("status=%d body=%s, want 429", response.Code, response.Body.String())
	}
	if got := upstreamCalls.Load(); got != 1 {
		t.Fatalf("upstream calls=%d, want 1 without rotating keys", got)
	}
	cooldowns, err := env.store.GetAllModelCooldowns(context.Background())
	if err != nil {
		t.Fatalf("GetAllModelCooldowns: %v", err)
	}
	got, exists := cooldowns[configs[0].ID]["claude-sonnet-4-6"]
	if !exists {
		t.Fatal("expected Anthropic SSE rate limit to cool the model")
	}
	if !got.Equal(resetAt) {
		t.Fatalf("model cooldown=%s, want header reset %s", got, resetAt)
	}
	keyCooldowns, err := env.store.GetAllKeyCooldowns(context.Background())
	if err != nil {
		t.Fatalf("GetAllKeyCooldowns: %v", err)
	}
	if len(keyCooldowns[configs[0].ID]) != 0 {
		t.Fatalf("key cooldowns=%v, want none for model-wide Anthropic limit", keyCooldowns[configs[0].ID])
	}
}

// TestProxy_SuccessBodyRuleRetriesKeyBeforeOutput 成功状态正文命中显式冷却规则时，在输出前冷却当前 Key 并切换同渠道 Key。
// @param t 当前测试上下文。
func TestProxy_SuccessBodyRuleRetriesKeyBeforeOutput(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string ///< 响应及客户端协议组合。
		contentType string ///< 上游响应媒体类型。
		stream      bool   ///< 是否返回 SSE。
		anthropic   bool   ///< 是否转换为 Anthropic 客户端协议。
		status      int    ///< 伪成功响应的原始状态码。
	}{
		{name: "sse_split_text", contentType: "text/event-stream", stream: true, status: http.StatusOK},
		{name: "json_created", contentType: "application/json", status: http.StatusCreated},
		{name: "plain_text", contentType: "text/plain", status: http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var badCalls, goodCalls atomic.Int32
			upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				bad := r.Header.Get("Authorization") == "Bearer sk-low-balance"
				text := "recovered answer"
				w.Header().Set("Content-Type", tc.contentType)
				if bad {
					badCalls.Add(1)
					text = "余额不够这次调用。当前余额：0"
					w.WriteHeader(tc.status)
				} else {
					goodCalls.Add(1)
				}
				if tc.stream {
					_, _ = fmt.Fprint(w, "data: {\"id\":\"chat-body-rule\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n")
					parts := []string{text}
					if bad {
						parts = []string{"余额不够这次", "调用。当前余额：0"}
					}
					for _, part := range parts {
						_, _ = fmt.Fprintf(w, "data: {\"id\":\"chat-body-rule\",\"choices\":[{\"index\":0,\"delta\":{\"content\":%q}}]}\n\n", part)
						w.(http.Flusher).Flush()
					}
					_, _ = fmt.Fprint(w, "data: {\"id\":\"chat-body-rule\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\ndata: [DONE]\n\n")
				} else if tc.contentType == "application/json" {
					_, _ = fmt.Fprintf(w, `{"id":"chat-body-rule","object":"chat.completion","model":"gpt-4","choices":[{"index":0,"message":{"role":"assistant","content":%q},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`, text)
				} else {
					_, _ = io.WriteString(w, text)
				}
			}))
			defer upstream.Close()
			env := setupProxyTestEnvWithSettings(t, []testChannel{{
				name: "success-body-rule", models: "gpt-4", apiKey: "sk-low-balance",
				cooldownDetectionRules: &model.CooldownDetectionRules{Rules: []model.CooldownDetectionRule{{
					Enabled: true, Name: "Low balance", StatusCodes: []int{tc.status},
					MessagePattern: "^余额不够这次调用。当前余额", Scope: model.CooldownScopeKey,
					Mode: model.CooldownModeFixed, CooldownSeconds: 90,
				}}},
			}}, map[int]string{0: upstream.URL}, map[string]string{"debug_log_enabled": "true"})
			configs, err := env.store.ListConfigs(context.Background())
			if err != nil || len(configs) != 1 {
				t.Fatalf("ListConfigs: configs=%v err=%v", configs, err)
			}
			channelID := configs[0].ID
			if err := env.store.CreateAPIKeysBatch(context.Background(), []*model.APIKey{{
				ChannelID: channelID, KeyIndex: 1, APIKey: "sk-recovered", Priority: -10,
			}}); err != nil {
				t.Fatalf("CreateAPIKeysBatch: %v", err)
			}
			path := "/v1/chat/completions"
			if tc.anthropic {
				path = "/v1/messages"
			}
			response := doProxyRequest(t, env.engine, path, map[string]any{
				"model": "gpt-4", "stream": tc.stream, "max_tokens": 128,
				"messages": []map[string]string{{"role": "user", "content": "hi"}},
			}, nil)
			var answer strings.Builder
			if tc.stream {
				for frame := range strings.SplitSeq(response.Body.String(), "\n\n") {
					_, data := parseSSEEventChunk([]byte(frame))
					if tc.anthropic {
						answer.WriteString(gjson.GetBytes(data, "delta.text").String())
					} else {
						answer.WriteString(gjson.GetBytes(data, "choices.0.delta.content").String())
					}
				}
			} else if tc.contentType == "application/json" {
				answer.WriteString(gjson.GetBytes(response.Body.Bytes(), "choices.0.message.content").String())
			} else {
				answer.Write(response.Body.Bytes())
			}
			if response.Code != http.StatusOK || answer.String() != "recovered answer" {
				t.Fatalf("status=%d answer=%q body=%s, want only recovered answer", response.Code, answer.String(), response.Body.String())
			}
			if badCalls.Load() != 1 || goodCalls.Load() != 1 {
				t.Fatalf("bad/good key calls=%d/%d, want 1/1", badCalls.Load(), goodCalls.Load())
			}
			cooldowns, err := env.store.GetAllKeyCooldowns(context.Background())
			if err != nil {
				t.Fatalf("GetAllKeyCooldowns: %v", err)
			}
			if remaining := time.Until(cooldowns[channelID][0]); remaining < 85*time.Second || remaining > 95*time.Second {
				t.Fatalf("old key cooldown=%v, want about 90s", remaining)
			}
			if until := cooldowns[channelID][1]; !until.IsZero() {
				t.Fatalf("healthy key unexpectedly cooled until %s", until)
			}
			channels, err := env.store.GetAllChannelCooldowns(context.Background())
			if err != nil || len(channels) != 0 {
				t.Fatalf("channel cooldowns=%v err=%v, want none with a healthy key", channels, err)
			}
			failedLog := waitForProxyLogMatching(t, env, func(entry *model.LogEntry) bool {
				return entry.Model == "gpt-4" && entry.StatusCode == util.StatusSSEError
			})
			successLog := waitForProxyLogMatching(t, env, func(entry *model.LogEntry) bool {
				return entry.Model == "gpt-4" && entry.StatusCode == http.StatusOK
			})
			if failedLog == nil || successLog == nil {
				t.Fatalf("missing rejected attempt or successful retry log: failed=%v success=%v", failedLog, successLog)
			}
			if !strings.Contains(failedLog.Message, "余额不够这次调用。当前余额") {
				t.Fatalf("failed attempt lost matched body: %q", failedLog.Message)
			}
			debug, err := env.store.GetDebugLogByLogID(context.Background(), failedLog.ID)
			if err != nil || debug == nil || debug.RespStatus != tc.status {
				t.Fatalf("failed attempt debug=%+v err=%v, want original HTTP status %d", debug, err, tc.status)
			}
		})
	}
}

// TestProxy_SuccessBodyRuleExhaustedCandidatesReturnsJSON 成功正文命中冷却规则且候选耗尽时，返回媒体类型与内容一致的 502 JSON 错误。
// @param t 当前测试上下文。
func TestProxy_SuccessBodyRuleExhaustedCandidatesReturnsJSON(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string ///< 上游成功响应格式。
		contentType string ///< 原始上游媒体类型。
		body        string ///< 余额不足正文及完整协议终态。
	}{
		{
			name: "sse", contentType: "text/event-stream",
			body: "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"余额不够这次调用。当前余额：0\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n",
		},
		{name: "plain_text", contentType: "text/plain", body: "余额不够这次调用。当前余额：0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", tc.contentType)
				w.Header().Set("Content-Length", strconv.Itoa(len(tc.body)))
				w.Header().Set("Content-Encoding", "identity")
				_, _ = io.WriteString(w, tc.body)
			}))
			defer upstream.Close()
			env := setupProxyTestEnv(t, []testChannel{{
				name: "exhausted-success-body", models: "gpt-4",
				cooldownDetectionRules: &model.CooldownDetectionRules{Rules: []model.CooldownDetectionRule{{
					Enabled: true, Name: "Low balance", StatusCodes: []int{http.StatusOK},
					MessagePattern: "^余额不够这次调用。当前余额", Scope: model.CooldownScopeKey,
					Mode: model.CooldownModeFixed, CooldownSeconds: 90,
				}}},
			}}, map[int]string{0: upstream.URL})
			response := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
				"model": "gpt-4", "stream": true, "messages": []map[string]string{{"role": "user", "content": "hi"}},
			}, nil)
			if response.Code != http.StatusBadGateway || response.Header().Get("Content-Type") != "application/json" || !json.Valid(response.Body.Bytes()) {
				t.Fatalf("status=%d headers=%v body=%q, want 502 application/json with valid JSON", response.Code, response.Header(), response.Body.String())
			}
			if response.Header().Get("Content-Length") != "" || response.Header().Get("Content-Encoding") != "" {
				t.Fatalf("replaced response retained upstream representation headers: %v", response.Header())
			}
			if calls.Load() != 1 {
				t.Fatalf("upstream calls=%d, want one rejected success response", calls.Load())
			}
		})
	}
}

// TestProxy_SuccessBodyRulesPreserveUnmatchedResponses 成功响应只受显式状态及正文规则影响，未命中或不可执行时保持原始 SSE。
// @param t 当前测试上下文。
func TestProxy_SuccessBodyRulesPreserveUnmatchedResponses(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"unmatched", "no_status", "no_pattern", "disabled", "other_status", "expired_reset"} {
		t.Run(scenario, func(t *testing.T) {
			rule := model.CooldownDetectionRule{
				Enabled: true, Name: "Explicit success body", StatusCodes: []int{http.StatusOK},
				MessagePattern: "^余额不够这次调用。当前余额", Scope: model.CooldownScopeKey,
				Mode: model.CooldownModeFixed, CooldownSeconds: 90,
			}
			text := "余额不够这次调用。当前余额：0"
			switch scenario {
			case "unmatched":
				text = "normal answer"
			case "no_status":
				rule.StatusCodes = nil
			case "no_pattern":
				rule.MessagePattern = ""
			case "disabled":
				rule.Enabled = false
			case "other_status":
				rule.StatusCodes = []int{http.StatusCreated}
			case "expired_reset":
				rule.MessagePattern = `^余额不够这次调用。当前余额：(?P<reset>\d+)`
				rule.Mode, rule.TimeCapture, rule.TimeFormat = model.CooldownModeResetTime, "reset", model.CooldownTimeFormatUnix
			}
			body := fmt.Sprintf("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":%q}}]}\n\ndata: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\ndata: [DONE]\n\n", text)
			var calls atomic.Int32
			upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, body)
			}))
			defer upstream.Close()
			env := setupProxyTestEnv(t, []testChannel{{
				name: "preserve-success", models: "gpt-4",
				cooldownDetectionRules: &model.CooldownDetectionRules{Rules: []model.CooldownDetectionRule{rule}},
			}}, map[int]string{0: upstream.URL})
			response := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
				"model": "gpt-4", "stream": true, "messages": []map[string]string{{"role": "user", "content": "hi"}},
			}, nil)
			if response.Code != http.StatusOK || response.Body.String() != body || calls.Load() != 1 {
				t.Fatalf("status=%d calls=%d body=%s, want unchanged upstream SSE", response.Code, calls.Load(), response.Body.String())
			}
			cooldowns, err := env.store.GetAllKeyCooldowns(context.Background())
			if err != nil || len(cooldowns) != 0 {
				t.Fatalf("key cooldowns=%v err=%v, want none", cooldowns, err)
			}
		})
	}
}

// TestProxy_SuccessBodyRulesGlobalFallbackAndChannelOverride 成功响应正文检测继承全局规则，渠道规则仍完整覆盖全局配置。
// @param t 当前测试上下文。
func TestProxy_SuccessBodyRulesGlobalFallbackAndChannelOverride(t *testing.T) {
	t.Parallel()
	for _, override := range []bool{false, true} {
		t.Run(fmt.Sprintf("channel_override_%v", override), func(t *testing.T) {
			var firstCalls, fallbackCalls atomic.Int32
			first := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				firstCalls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"low-balance","choices":[{"message":{"role":"assistant","content":"余额不够这次调用。当前余额：0"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
			}))
			defer first.Close()
			fallback := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				fallbackCalls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"global-recovered","choices":[{"message":{"role":"assistant","content":"recovered answer"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
			}))
			defer fallback.Close()
			var channelRules *model.CooldownDetectionRules
			if override {
				channelRules = &model.CooldownDetectionRules{Rules: []model.CooldownDetectionRule{{
					Enabled: true, Name: "Local success body", StatusCodes: []int{http.StatusOK},
					MessagePattern: "^local maintenance", Scope: model.CooldownScopeChannel,
					Mode: model.CooldownModeFixed, CooldownSeconds: 90,
				}}}
			}
			env := setupProxyTestEnvWithSettings(t, []testChannel{
				{name: "global-first", models: "gpt-4", priority: 100, cooldownDetectionRules: channelRules},
				{name: "global-fallback", models: "gpt-4", priority: 50},
			}, map[int]string{0: first.URL, 1: fallback.URL}, map[string]string{
				"global_cooldown_detection_rules": `{"rules":[{"enabled":true,"name":"Global success balance","status_codes":[200],"message_pattern":"^余额不够这次调用。当前余额","scope":"channel","mode":"fixed","cooldown_seconds":90}]}`,
			})
			response := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
				"model": "gpt-4", "messages": []map[string]string{{"role": "user", "content": "hi"}},
			}, nil)
			wantAnswer, wantFallbackCalls := "recovered answer", int32(1)
			if override {
				wantAnswer, wantFallbackCalls = "余额不够这次调用。当前余额：0", 0
			}
			answer := gjson.GetBytes(response.Body.Bytes(), "choices.0.message.content").String()
			if response.Code != http.StatusOK || answer != wantAnswer || firstCalls.Load() != 1 || fallbackCalls.Load() != wantFallbackCalls {
				t.Fatalf("status=%d answer=%q first/fallback=%d/%d, want answer=%q calls=1/%d", response.Code, answer, firstCalls.Load(), fallbackCalls.Load(), wantAnswer, wantFallbackCalls)
			}
			cooldowns, err := env.store.GetAllChannelCooldowns(context.Background())
			if err != nil {
				t.Fatalf("GetAllChannelCooldowns: %v", err)
			}
			wantCooldowns := 1
			if override {
				wantCooldowns = 0
			}
			if len(cooldowns) != wantCooldowns {
				t.Fatalf("channel cooldowns=%v, want %d cooled channels", cooldowns, wantCooldowns)
			}
		})
	}
}

func TestProxy_SSEErrorRuleMatchesOriginalHTTPStatus(t *testing.T) {
	t.Parallel()

	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		largeContent := strings.Repeat("Hi", SSEBufferSize)
		_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q}}]}\n\n", largeContent)
		if flusher != nil {
			flusher.Flush()
		}
		_, _ = fmt.Fprint(w, "event: error\n")
		_, _ = fmt.Fprint(w, `data: {"error":{"message":"rate limit exceeded","type":"rate_limit_error"}}`+"\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{{
		name: "ch-sse-original-status", models: "gpt-4", apiKey: "sk-1",
		cooldownDetectionRules: &model.CooldownDetectionRules{Rules: []model.CooldownDetectionRule{{
			Enabled: true, Name: "HTTP 200 soft error", Priority: 0, StatusCodes: []int{http.StatusOK},
			MessagePattern: "rate limit exceeded", Scope: model.CooldownScopeChannel,
			Mode: model.CooldownModeFixed, CooldownSeconds: 90,
		}}},
	}}, map[int]string{0: upstream.URL})

	configs, err := env.store.ListConfigs(context.Background())
	if err != nil || len(configs) != 1 {
		t.Fatalf("ListConfigs() = (%d, %v), want one channel", len(configs), err)
	}
	w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model": "gpt-4", "stream": true,
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}

	cooldowns, err := env.store.GetAllChannelCooldowns(context.Background())
	if err != nil {
		t.Fatalf("GetAllChannelCooldowns: %v", err)
	}
	until, exists := cooldowns[configs[0].ID]
	if !exists {
		t.Fatalf("expected configured HTTP 200 rule to cool channel %d", configs[0].ID)
	}
	if remaining := time.Until(until); remaining < 85*time.Second || remaining > 95*time.Second {
		t.Fatalf("configured channel cooldown remaining=%v, want about 90s", remaining)
	}
}

func TestProxy_SSEFreeTierBudgetExceededCoolsOnlyKeyThenPromotesChannel(t *testing.T) {
	t.Parallel()

	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		largeContent := strings.Repeat("Hi", SSEBufferSize)
		chunks := []string{
			fmt.Sprintf(`data: {"choices":[{"delta":{"content":"%s"}}]}`, largeContent),
			`event: error` + "\n" + `data: {"type":"error","error":{"type":"api_error","message":"403 {\"error\":{\"code\":\"FREE_TIER_BUDGET_EXCEEDED\",\"message\":\"Free tier monthly spend limit exceeded. Please upgrade to a paid plan to continue using this service.\"}}"}}`,
		}
		for _, chunk := range chunks {
			_, _ = fmt.Fprintf(w, "%s\n\n", chunk)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "ch-free-tier-sse", models: "gpt-4", apiKey: "sk-free-tier"},
	}, map[int]string{0: upstream.URL})

	ctx := context.Background()
	configs, err := env.store.ListConfigs(ctx)
	if err != nil {
		t.Fatalf("ListConfigs: %v", err)
	}
	if len(configs) != 1 {
		t.Fatalf("expected 1 config, got %d", len(configs))
	}
	channelID := configs[0].ID

	before := time.Now()
	w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model":    "gpt-4",
		"stream":   true,
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (header already sent), got %d: %s", w.Code, w.Body.String())
	}

	keyCooldowns, err := env.store.GetAllKeyCooldowns(ctx)
	if err != nil {
		t.Fatalf("GetAllKeyCooldowns: %v", err)
	}
	channelKeyCooldowns := keyCooldowns[channelID]
	if channelKeyCooldowns == nil {
		t.Fatalf("expected key cooldown for channel_id=%d", channelID)
	}
	cooldownUntil, exists := channelKeyCooldowns[0]
	if !exists {
		t.Fatalf("expected key 0 cooldown for channel_id=%d", channelID)
	}
	duration := cooldownUntil.Sub(before)
	if duration < 29*time.Minute+55*time.Second || duration > 30*time.Minute+5*time.Second {
		t.Fatalf("key cooldown duration=%v, want about 30m", duration)
	}

	channelCooldowns, err := env.store.GetAllChannelCooldowns(ctx)
	if err != nil {
		t.Fatalf("GetAllChannelCooldowns: %v", err)
	}
	channelUntil, exists := channelCooldowns[channelID]
	if !exists || !channelUntil.After(time.Now()) {
		t.Fatalf("single-key channel should be cooled after its only key is cooled")
	}
	if channelUntil.Sub(cooldownUntil).Abs() > time.Second {
		t.Fatalf("channel cooldown=%s, want key recovery time %s", channelUntil, cooldownUntil)
	}
}

func TestProxy_SSEErrorEventBeforeClientOutput_RetriesNextChannel(t *testing.T) {
	t.Parallel()

	var firstCalls atomic.Int32
	upstream1 := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firstCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		_, _ = fmt.Fprint(w, "event: error\n")
		_, _ = fmt.Fprint(w, "data: "+`{"type":"error","error":{"type":"service_unavailable_error","code":"server_is_overloaded","message":"Our servers are currently overloaded. Please try again later.","param":null},"sequence_number":2}`+"\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer upstream1.Close()

	var secondCalls atomic.Int32
	upstream2 := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		chunks := []string{
			`data: {"choices":[{"delta":{"content":"from-ch2"}}]}`,
			`data: [DONE]`,
		}
		for _, chunk := range chunks {
			_, _ = fmt.Fprintf(w, "%s\n\n", chunk)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer upstream2.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "ch1-overloaded", models: "gpt-4", apiKey: "sk-1", priority: 100},
		{name: "ch2-ok", models: "gpt-4", apiKey: "sk-2", priority: 50},
	}, map[int]string{0: upstream1.URL, 1: upstream2.URL})

	w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model":    "gpt-4",
		"stream":   true,
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 after retrying next channel, got %d: %s", w.Code, w.Body.String())
	}

	body := w.Body.String()
	if !strings.Contains(body, "from-ch2") {
		t.Fatalf("expected response body from second channel, got: %s", body)
	}
	if strings.Contains(body, "server_is_overloaded") {
		t.Fatalf("expected first channel SSE error not to leak to client, body: %s", body)
	}
	if firstCalls.Load() != 1 {
		t.Fatalf("expected first channel to be tried once, got %d", firstCalls.Load())
	}
	if secondCalls.Load() != 1 {
		t.Fatalf("expected second channel to be tried once, got %d", secondCalls.Load())
	}
}

func TestProxy_ResponsesMetadataThenSSEError_RetriesNextChannel(t *testing.T) {
	t.Parallel()

	var firstCalls atomic.Int32
	upstream1 := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firstCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		_, _ = fmt.Fprint(w, "event: response.created\n")
		_, _ = fmt.Fprint(w, `data: {"type":"response.created","response":{"id":"resp-ch1-created","status":"in_progress"}}`+"\n\n")
		_, _ = fmt.Fprint(w, "event: error\n")
		_, _ = fmt.Fprint(w, "data: "+`{"type":"error","error":{"type":"service_unavailable_error","code":"server_is_overloaded","message":"Our servers are currently overloaded. Please try again later.","param":null},"sequence_number":2}`+"\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer upstream1.Close()

	var secondCalls atomic.Int32
	upstream2 := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		_, _ = fmt.Fprint(w, `data: {"type":"response.completed","response":{"id":"resp-ch2","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`+"\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer upstream2.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "ch1-created-then-overloaded", upstreamProtocol: "codex", models: "gpt-test", apiKey: "sk-1", priority: 100},
		{name: "ch2-ok", upstreamProtocol: "codex", models: "gpt-test", apiKey: "sk-2", priority: 50},
	}, map[int]string{0: upstream1.URL, 1: upstream2.URL})

	w := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
		"model":  "gpt-test",
		"stream": true,
		"input":  "hi",
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 after retrying next channel, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "resp-ch2") {
		t.Fatalf("expected completed response from second channel, got: %s", body)
	}
	if strings.Contains(body, "resp-ch1-created") || strings.Contains(body, "server_is_overloaded") {
		t.Fatalf("expected first channel metadata/error not to leak to client, body: %s", body)
	}
	if firstCalls.Load() != 1 || secondCalls.Load() != 1 {
		t.Fatalf("upstream calls first=%d second=%d, want 1/1", firstCalls.Load(), secondCalls.Load())
	}
}

// 上游静默中止：response.incomplete 无 output 且 output_tokens=0，首包前按流中断切渠道；
// content_filter 与 interrupted（上游确认 response.interrupt）原样返回不切渠道。
func TestProxy_ResponsesEmptyIncompleteBeforeOutput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		reason     string
		wantRetry  bool
		wantInBody string
	}{
		{name: "silent abort retries next channel", reason: "max_output_tokens", wantRetry: true, wantInBody: "resp-ch2"},
		{name: "content filter is returned", reason: "content_filter", wantRetry: false, wantInBody: "resp-ch1-incomplete"},
		{name: "interrupt acknowledgement is returned", reason: "interrupted", wantRetry: false, wantInBody: "resp-ch1-incomplete"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			upstream1 := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				_, _ = fmt.Fprint(w, `data: {"type":"response.created","response":{"id":"resp-ch1-incomplete","status":"in_progress"}}`+"\n\n")
				_, _ = fmt.Fprint(w, `data: {"type":"response.incomplete","response":{"id":"resp-ch1-incomplete","status":"incomplete","incomplete_details":{"reason":"`+tc.reason+`"},"output":[],"usage":{"input_tokens":5,"output_tokens":0,"total_tokens":5}}}`+"\n\n")
			}))
			defer upstream1.Close()

			var secondCalls atomic.Int32
			upstream2 := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				secondCalls.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				_, _ = fmt.Fprint(w, `data: {"type":"response.completed","response":{"id":"resp-ch2","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`+"\n\n")
			}))
			defer upstream2.Close()

			env := setupProxyTestEnv(t, []testChannel{
				{name: "ch1-empty-incomplete", upstreamProtocol: "codex", models: "gpt-test", apiKey: "sk-1", priority: 100},
				{name: "ch2-ok", upstreamProtocol: "codex", models: "gpt-test", apiKey: "sk-2", priority: 50},
			}, map[int]string{0: upstream1.URL, 1: upstream2.URL})

			w := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
				"model":  "gpt-test",
				"stream": true,
				"input":  "hi",
			}, nil)

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, body: %s", w.Code, w.Body.String())
			}
			if body := w.Body.String(); !strings.Contains(body, tc.wantInBody) {
				t.Fatalf("body missing %q: %s", tc.wantInBody, body)
			}
			if got := secondCalls.Load() == 1; got != tc.wantRetry {
				t.Fatalf("second channel called = %v, want %v", got, tc.wantRetry)
			}
		})
	}
}

// Codex 上游在响应开始与终态之间会插入 `event: keepalive`（data: {"type":"keepalive",...}）。
// 它既不是 ping 心跳，也不在 Responses 元数据事件列表里，修复前会被算成语义输出，
// 导致 deferredWriter 提前 commit，随后的 server_is_overloaded 无法切渠道。
func TestProxy_ResponsesKeepaliveThenSSEError_RetriesNextChannel(t *testing.T) {
	t.Parallel()

	var firstCalls atomic.Int32
	upstream1 := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firstCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		_, _ = fmt.Fprint(w, "event: response.created\n")
		_, _ = fmt.Fprint(w, `data: {"type":"response.created","response":{"id":"resp-ch1-created","status":"in_progress"}}`+"\n\n")
		_, _ = fmt.Fprint(w, "event: response.in_progress\n")
		_, _ = fmt.Fprint(w, `data: {"type":"response.in_progress","response":{"id":"resp-ch1-created","status":"in_progress"}}`+"\n\n")
		_, _ = fmt.Fprint(w, "event: keepalive\n")
		_, _ = fmt.Fprint(w, `data: {"type":"keepalive","sequence_number":2}`+"\n\n")
		_, _ = fmt.Fprint(w, "event: error\n")
		_, _ = fmt.Fprint(w, "data: "+`{"type":"error","error":{"type":"service_unavailable_error","code":"server_is_overloaded","message":"Our servers are currently overloaded. Please try again later.","param":null},"sequence_number":3}`+"\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer upstream1.Close()

	var secondCalls atomic.Int32
	upstream2 := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		_, _ = fmt.Fprint(w, `data: {"type":"response.completed","response":{"id":"resp-ch2","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`+"\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer upstream2.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "ch1-keepalive-then-overloaded", upstreamProtocol: "codex", models: "gpt-test", apiKey: "sk-1", priority: 100},
		{name: "ch2-ok", upstreamProtocol: "codex", models: "gpt-test", apiKey: "sk-2", priority: 50},
	}, map[int]string{0: upstream1.URL, 1: upstream2.URL})

	w := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
		"model":  "gpt-test",
		"stream": true,
		"input":  "hi",
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 after retrying next channel, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "resp-ch2") {
		t.Fatalf("expected completed response from second channel, got: %s", body)
	}
	if strings.Contains(body, "resp-ch1-created") || strings.Contains(body, "server_is_overloaded") {
		t.Fatalf("expected first channel metadata/keepalive/error not to leak to client, body: %s", body)
	}
	if firstCalls.Load() != 1 || secondCalls.Load() != 1 {
		t.Fatalf("upstream calls first=%d second=%d, want 1/1", firstCalls.Load(), secondCalls.Load())
	}
}

// Codex 上游会在过载拒绝前先发 response.metadata（提示审核标记）和内容为空的条目/片段骨架
// （CLIProxyAPI #6856、sub2api e068c3d02）。它们都不是模型输出，不能提交响应，否则随后的
// server_is_overloaded 会原样漏给客户端而不是切渠道。
func TestProxy_ResponsesMetadataSkeletonThenOverload_RetriesNextChannel(t *testing.T) {
	t.Parallel()

	var firstCalls atomic.Int32
	upstream1 := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firstCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, frame := range []string{
			`{"type":"response.created","response":{"id":"resp-ch1-created","status":"in_progress"}}`,
			`{"type":"response.in_progress","response":{"id":"resp-ch1-created","status":"in_progress"}}`,
			`{"type":"response.metadata","response_id":"resp-ch1-created","metadata":{"openai_chatgpt_moderation_metadata":{"prompt":{}}}}`,
			`{"type":"response.output_item.added","output_index":0,"item":{"id":"rs_ch1","type":"reasoning","summary":[]}}`,
			`{"type":"response.reasoning_summary_part.added","item_id":"rs_ch1","output_index":0,"summary_index":0,"part":{"type":"summary_text","text":""}}`,
			`{"type":"error","error":{"type":"service_unavailable_error","code":"server_is_overloaded","message":"Our servers are currently overloaded. Please try again later.","param":null}}`,
		} {
			_, _ = fmt.Fprint(w, "event: "+gjson.Get(frame, "type").String()+"\ndata: "+frame+"\n\n")
		}
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}))
	defer upstream1.Close()

	var secondCalls atomic.Int32
	upstream2 := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, `data: {"type":"response.completed","response":{"id":"resp-ch2","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`+"\n\n")
	}))
	defer upstream2.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "ch1-skeleton-then-overloaded", upstreamProtocol: "codex", models: "gpt-test", apiKey: "sk-1", priority: 100},
		{name: "ch2-ok", upstreamProtocol: "codex", models: "gpt-test", apiKey: "sk-2", priority: 50},
	}, map[int]string{0: upstream1.URL, 1: upstream2.URL})

	w := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
		"model":  "gpt-test",
		"stream": true,
		"input":  "hi",
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 after retrying next channel, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "resp-ch2") {
		t.Fatalf("expected completed response from second channel, got: %s", body)
	}
	if strings.Contains(body, "resp-ch1-created") || strings.Contains(body, "rs_ch1") || strings.Contains(body, "server_is_overloaded") {
		t.Fatalf("expected first channel metadata/skeleton/error not to leak to client, body: %s", body)
	}
	if firstCalls.Load() != 1 || secondCalls.Load() != 1 {
		t.Fatalf("upstream calls first=%d second=%d, want 1/1", firstCalls.Load(), secondCalls.Load())
	}
}

// keepalive 是上游保活帧：它不算语义输出（否则会提前 commit、阻断切渠道），
// 但字节本身必须原样透传给客户端，不能像 ping 那样被解析器丢弃。
func TestProxy_ResponsesKeepaliveIsForwardedToClient(t *testing.T) {
	t.Parallel()

	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		_, _ = fmt.Fprint(w, "event: response.created\n")
		_, _ = fmt.Fprint(w, `data: {"type":"response.created","response":{"id":"resp-keepalive","status":"in_progress"}}`+"\n\n")
		_, _ = fmt.Fprint(w, "event: keepalive\n")
		_, _ = fmt.Fprint(w, `data: {"type":"keepalive","sequence_number":2}`+"\n\n")
		_, _ = fmt.Fprint(w, `data: {"type":"response.completed","response":{"id":"resp-keepalive","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`+"\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "ch-keepalive-ok", upstreamProtocol: "codex", models: "gpt-test", apiKey: "sk-1", priority: 100},
	}, map[int]string{0: upstream.URL})

	w := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
		"model":  "gpt-test",
		"stream": true,
		"input":  "hi",
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `"type":"keepalive"`) {
		t.Fatalf("keepalive frame must be forwarded to client, body: %s", body)
	}
	if !strings.Contains(body, "resp-keepalive") {
		t.Fatalf("expected completed response from channel, body: %s", body)
	}
}

func TestProxy_ResponsesMetadataDoesNotBecomeLoggedFirstByte(t *testing.T) {
	t.Parallel()

	semanticDelay := 300 * time.Millisecond
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		_, _ = fmt.Fprint(w, "event: response.created\n")
		_, _ = fmt.Fprint(w, `data: {"type":"response.created","response":{"id":"resp-first-byte","status":"in_progress"}}`+"\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		time.Sleep(semanticDelay)
		_, _ = fmt.Fprint(w, `data: {"type":"response.output_text.delta","delta":"hi"}`+"\n\n")
		_, _ = fmt.Fprint(w, `data: {"type":"response.completed","response":{"id":"resp-first-byte","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`+"\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{{
		name: "metadata-first-byte", upstreamProtocol: "codex", models: "gpt-first-byte", apiKey: "sk-first-byte",
	}}, map[int]string{0: upstream.URL})

	w := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
		"model": "gpt-first-byte", "stream": true, "input": "hi",
	}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "resp-first-byte") || !strings.Contains(body, `"hi"`) {
		t.Fatalf("expected created+delta, got: %s", body)
	}

	entry := waitForProxyLog(t, env, "gpt-first-byte")
	if entry.FirstByteTime < semanticDelay.Seconds() {
		t.Fatalf("first_byte_time=%.3f should be client-visible content after semantic delay %.3f; duration=%.3f",
			entry.FirstByteTime, semanticDelay.Seconds(), entry.Duration)
	}
	if entry.Duration < semanticDelay.Seconds() {
		t.Fatalf("duration=%.3f shorter than semantic delay, test setup invalid", entry.Duration)
	}
}

func TestProxy_ResponsesMetadataDoesNotSetClientFirstByteBeforeCommit(t *testing.T) {
	t.Parallel()

	metadataSent := make(chan struct{})
	releaseSemantic := make(chan struct{})
	semanticSent := make(chan struct{})
	releaseComplete := make(chan struct{})
	var releaseSemanticOnce sync.Once
	var releaseCompleteOnce sync.Once
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, `data: {"type":"response.created","response":{"id":"resp-client-byte","status":"in_progress"}}`+"\n\n")
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		close(metadataSent)
		<-releaseSemantic
		_, _ = fmt.Fprint(w, `data: {"type":"response.output_text.delta","delta":"hi"}`+"\n\n")
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		close(semanticSent)
		<-releaseComplete
		_, _ = fmt.Fprint(w, `data: {"type":"response.completed","response":{"id":"resp-client-byte","status":"completed","output":[]}}`+"\n\n")
	}))
	releaseSemanticOutput := func() {
		releaseSemanticOnce.Do(func() { close(releaseSemantic) })
	}
	finishResponse := func() {
		releaseCompleteOnce.Do(func() { close(releaseComplete) })
	}
	defer releaseSemanticOutput()
	defer finishResponse()

	env := setupProxyTestEnv(t, []testChannel{{
		name: "metadata-client-byte", upstreamProtocol: "codex", models: "gpt-client-byte", apiKey: "sk-client-byte",
	}}, map[int]string{0: upstream.URL})

	body, err := json.Marshal(map[string]any{
		"model": "gpt-client-byte", "stream": true, "input": "hi",
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-api-key")
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		env.engine.ServeHTTP(response, req)
		close(done)
	}()

	select {
	case <-metadataSent:
	case <-time.After(time.Second):
		t.Fatal("upstream metadata was not sent")
	}

	var active *ActiveRequest
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		requests := env.server.activeRequests.List()
		if len(requests) == 1 && requests[0].BytesReceived > 0 {
			active = requests[0]
			break
		}
		time.Sleep(time.Millisecond)
	}
	if active == nil {
		t.Fatal("active request did not receive metadata bytes")
	}
	if active.ClientFirstByteTime != 0 {
		t.Fatalf("client_first_byte_time=%.6f before deferred response commit, want 0", active.ClientFirstByteTime)
	}

	releaseSemanticOutput()
	select {
	case <-semanticSent:
	case <-time.After(time.Second):
		t.Fatal("upstream semantic output was not sent")
	}

	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		requests := env.server.activeRequests.List()
		if len(requests) == 1 && requests[0].ClientFirstByteTime > 0 {
			active = requests[0]
			break
		}
		time.Sleep(time.Millisecond)
	}
	if active == nil || active.ClientFirstByteTime <= 0 {
		t.Fatal("active request did not record client-visible first byte")
	}
	liveFirstByte := active.ClientFirstByteTime

	finishResponse()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("proxy request did not finish after semantic output")
	}
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"delta":"hi"`) {
		t.Fatalf("unexpected response status=%d body=%s", response.Code, response.Body.String())
	}
	entry := waitForProxyLog(t, env, "gpt-client-byte")
	difference := entry.FirstByteTime - liveFirstByte
	if difference < -0.002 || difference > 0.002 {
		t.Fatalf("live first byte %.6f differs from logged first byte %.6f", liveFirstByte, entry.FirstByteTime)
	}
}

func TestProxy_ResponsesMissingRequiredParameterRetriesWithPrunedInput(t *testing.T) {
	t.Parallel()

	requests := make(chan []byte, 2)
	var calls atomic.Int32
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read upstream request: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		requests <- bytes.Clone(body)
		if calls.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprint(w, `{"error":{"code":"missing_required_parameter","message":"Missing required parameter: 'input[1].content[0].text'.","param":"input[1].content[0].text","type":"invalid_request_error"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, `data: {"type":"response.completed","response":{"id":"resp-after-pruning","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`+"\n\n")
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{{customRequestRules: &model.CustomRequestRules{Passthrough: new(bool)},
		name: "responses-missing-required", upstreamProtocol: "codex", models: "gpt-test", apiKey: "sk-1",
	}}, map[int]string{0: upstream.URL})
	w := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
		"model": "gpt-test", "stream": true,
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": "keep-user"},
			map[string]any{"type": "message", "role": "assistant", "content": []any{
				map[string]any{"type": "output_text"},
			}},
			map[string]any{"type": "message", "role": "user", "content": "keep-follow-up"},
		},
	}, nil)

	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "resp-after-pruning") {
		t.Fatalf("status=%d body=%s, want completed retried response", w.Code, w.Body.String())
	}
	if calls.Load() != 2 {
		t.Fatalf("upstream calls=%d, want 2", calls.Load())
	}
	first := <-requests
	retry := <-requests
	if gjson.GetBytes(first, "input.#").Int() != 3 {
		t.Fatalf("first request input=%s, want three items", first)
	}
	if gjson.GetBytes(retry, "input.#").Int() != 2 ||
		gjson.GetBytes(retry, "input.0.content").String() != "keep-user" ||
		gjson.GetBytes(retry, "input.1.content").String() != "keep-follow-up" {
		t.Fatalf("retry did not remove only invalid input item: %s", retry)
	}
}

func TestProxy_ResponsesSSEMissingStoredItemRetriesOnceWithStrippedBody(t *testing.T) {
	t.Parallel()

	const missingID = "rs_item_missing"
	requests := make(chan []byte, 2)
	var calls atomic.Int32
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read upstream request: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		requests <- bytes.Clone(body)
		call := calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		if call == 1 {
			_, _ = fmt.Fprintf(w, "event: error\ndata: %s\n\n", fmt.Sprintf(
				`{"type":"error","error":{"type":"invalid_request_error","code":null,"message":"Item with id '%s' not found. Items are not persisted when store is set to false.","param":"input"},"status":404}`,
				missingID,
			))
			return
		}
		_, _ = fmt.Fprint(w, `data: {"type":"response.completed","response":{"id":"resp-after-strips","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`+"\n\n")
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{{customRequestRules: &model.CustomRequestRules{Passthrough: new(bool)},
		name: "responses-missing-items", upstreamProtocol: "codex", models: "gpt-test", apiKey: "sk-1",
	}}, map[int]string{0: upstream.URL})
	w := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
		"model": "gpt-test", "stream": true, "store": false,
		"input": []any{
			map[string]any{"type": "reasoning", "id": missingID, "summary": []any{}},
			map[string]any{"type": "message", "id": "msg_keep", "role": "user", "content": "keep"},
		},
	}, nil)

	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "resp-after-strips") {
		t.Fatalf("status=%d body=%s, want completed response", w.Code, w.Body.String())
	}
	if calls.Load() != 2 {
		t.Fatalf("upstream calls=%d, want initial request plus one retry", calls.Load())
	}
	first := <-requests
	second := <-requests
	if !gjson.GetBytes(first, "input.#(id==\""+missingID+"\").id").Exists() {
		t.Fatalf("initial request lost missing item: %s", first)
	}
	if gjson.GetBytes(second, "input.#").Int() != 1 ||
		gjson.GetBytes(second, "input.0.id").String() != "msg_keep" {
		t.Fatalf("retry did not remove only %s: %s", missingID, second)
	}
}

func TestProxy_ResponsesMissingStoredItemsPreserveWireHistory(t *testing.T) {
	t.Parallel()
	for _, sseError := range []bool{false, true} {
		for _, rejectedAgain := range []bool{false, true} {
			t.Run(fmt.Sprintf("sse=%t/rejected_again=%t", sseError, rejectedAgain), func(t *testing.T) {
				t.Parallel()
				input := []any{
					map[string]any{"type": "message", "role": "user", "content": "removed by channel rule"},
					map[string]any{"type": "reasoning", "id": "rs_original", "encrypted_content": nil},
					map[string]any{"type": "reasoning", "id": "rs_empty", "encrypted_content": ""},
					map[string]any{"type": "reasoning", "id": "rs_blank", "encrypted_content": " \t"},
					map[string]any{"type": "reasoning", "id": "rs_absent"},
					map[string]any{"type": "reasoning", "summary": []any{}},
					map[string]any{"type": "message", "role": "user", "content": "keep"},
					map[string]any{"type": "reasoning", "id": "rs_encrypted", "encrypted_content": "opaque"},
					map[string]any{"type": "function_call", "id": "fc_keep", "call_id": "call_keep", "name": "lookup", "arguments": "{}"},
					map[string]any{"type": "function_call_output", "call_id": "call_keep", "output": "result"},
					map[string]any{"type": "message", "id": "msg_keep", "role": "assistant", "content": "done"},
				}
				var requests [][]byte
				upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					body, err := io.ReadAll(r.Body)
					if err != nil {
						t.Errorf("read request: %v", err)
						w.WriteHeader(http.StatusInternalServerError)
						return
					}
					requests = append(requests, body)
					if len(requests) == 1 || rejectedAgain {
						payload := `{"type":"error","status":404,"error":{"type":"invalid_request_error","param":"input","message":"Item with id 'rs_wire' not found. Items are not persisted when store is set to false."}}`
						if sseError {
							w.Header().Set("Content-Type", "text/event-stream")
							_, _ = fmt.Fprintf(w, "event: error\ndata: %s\n\n", payload)
						} else {
							w.Header().Set("Content-Type", "application/json")
							w.WriteHeader(http.StatusNotFound)
							_, _ = io.WriteString(w, payload)
						}
						return
					}
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-recovered\",\"status\":\"completed\",\"output\":[]}}\n\n")
				}))
				defer upstream.Close()
				env := setupProxyTestEnv(t, []testChannel{{
					name: "codex-recovery", upstreamProtocol: "codex", models: "gpt-test", authType: model.AuthTypeCodexOAuth,
					oauthCredential: codexProxyTestCredential(t, "at-recovery", "rt-recovery", "account-recovery"),
					customRequestRules: &model.CustomRequestRules{Body: []model.CustomBodyRule{
						{Action: model.RuleActionRemove, Path: "input.0"},
						{Action: model.RuleActionOverride, Path: "input.0.id", Value: json.RawMessage(`"rs_wire"`)},
					}},
				}}, map[int]string{0: upstream.URL})
				response := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
					"model": "gpt-test", "stream": true, "store": false, "input": input,
				}, nil)
				if len(requests) != 2 {
					t.Fatalf("upstream calls=%d, want exactly one retry; response=%s", len(requests), response.Body.String())
				}
				if gjson.GetBytes(requests[0], "input.0.id").String() != "rs_wire" || gjson.GetBytes(requests[0], "input.#").Int() != int64(len(input)-1) {
					t.Fatalf("initial wire rules not applied: %s", requests[0])
				}
				var first, second map[string]any
				if err := json.Unmarshal(requests[0], &first); err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(requests[1], &second); err != nil {
					t.Fatal(err)
				}
				first["input"] = input[5:]
				if !reflect.DeepEqual(first, second) {
					t.Fatalf("retry changed retained history or reapplied channel rules: %s", requests[1])
				}
				if !rejectedAgain {
					_, payload := parseSSEEventChunk(response.Body.Bytes())
					if response.Code != http.StatusOK || gjson.GetBytes(payload, "response.status").String() != "completed" {
						t.Fatalf("recovery failed: status=%d body=%s", response.Code, response.Body.String())
					}
				}
				entry := waitForProxyLog(t, env, "gpt-test")
				if !strings.Contains(entry.Message, "strip_missing_stored_input_item:rs_wire:removed=4") {
					t.Fatalf("recovery strategy missing from log: %s", entry.Message)
				}
			})
		}
	}
}

func TestProxy_ResponsesSSEMissingStoredItemsRecoverTogether(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	requests := make(chan []byte, responsesMissingStoredItemRetryLimit+2)
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read upstream request: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		requests <- bytes.Clone(body)
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		missingID := gjson.GetBytes(body, "input.0.id").String()
		if missingID == "" {
			_, _ = fmt.Fprint(w, `data: {"type":"response.completed","response":{"id":"resp-after-unbounded-retries","status":"completed","output":[]}}`+"\n\n")
			return
		}
		_, _ = fmt.Fprintf(w, "event: error\ndata: %s\n\n", fmt.Sprintf(
			`{"type":"error","error":{"type":"invalid_request_error","code":null,"message":"Item with id '%s' not found. Items are not persisted when store is set to false.","param":"input"},"status":404}`,
			missingID,
		))
	}))
	defer upstream.Close()

	input := make([]any, 0, responsesMissingStoredItemRetryLimit+1)
	for index := range responsesMissingStoredItemRetryLimit + 1 {
		input = append(input, map[string]any{
			"type": "reasoning", "id": fmt.Sprintf("rs_item_missing_%02d", index), "summary": []any{},
		})
	}
	env := setupProxyTestEnv(t, []testChannel{{customRequestRules: &model.CustomRequestRules{Passthrough: new(bool)},
		name: "responses-missing-item-limit", upstreamProtocol: "codex", models: "gpt-test", apiKey: "sk-1",
	}}, map[int]string{0: upstream.URL})
	w := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
		"model": "gpt-test", "stream": true, "store": false, "input": input,
	}, nil)

	wantCalls := int32(responsesMissingStoredItemRetryLimit + 1)
	if calls.Load() != wantCalls {
		t.Fatalf("upstream calls=%d, want initial request plus %d retries (%d total); status=%d body=%s",
			calls.Load(), responsesMissingStoredItemRetryLimit, wantCalls, w.Code, w.Body.String())
	}
	_, completed := parseSSEEventChunk(w.Body.Bytes())
	if gjson.GetBytes(completed, "response.status").String() != "completed" {
		t.Fatalf("expected completed response after batch recovery: %s", w.Body.String())
	}
	var last []byte
	for range wantCalls {
		last = <-requests
	}
	if gjson.GetBytes(last, "input.#").Int() != 0 {
		t.Fatalf("batch recovery retained missing reasoning: %s", last)
	}
}

func TestProxy_ResponsesHTTPMissingStoredItemRetriesOnce(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	requests := make(chan []byte, responsesMissingStoredItemRetryLimit+2)
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read upstream request: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		requests <- bytes.Clone(body)
		calls.Add(1)
		missingID := gjson.GetBytes(body, "input.0.id").String()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = fmt.Fprintf(w,
			`{"error":{"type":"invalid_request_error","code":null,"message":"Item with id '%s' not found. Items are not persisted when store is set to false.","param":"input"}}`,
			missingID,
		)
	}))
	defer upstream.Close()

	input := make([]any, 0, responsesMissingStoredItemRetryLimit+1)
	for index := range responsesMissingStoredItemRetryLimit + 1 {
		input = append(input, map[string]any{
			"type": "reasoning", "id": fmt.Sprintf("rs_item_missing_%02d", index), "summary": []any{},
		})
	}
	env := setupProxyTestEnv(t, []testChannel{{customRequestRules: &model.CustomRequestRules{Passthrough: new(bool)},
		name: "responses-http-missing-item-limit", upstreamProtocol: "codex", models: "gpt-test", apiKey: "sk-1",
	}}, map[int]string{0: upstream.URL})
	w := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
		"model": "gpt-test", "store": false, "input": input,
	}, nil)

	wantCalls := int32(responsesMissingStoredItemRetryLimit + 1)
	if calls.Load() != wantCalls {
		t.Fatalf("upstream calls=%d, want initial request plus %d retries (%d total); status=%d body=%s",
			calls.Load(), responsesMissingStoredItemRetryLimit, wantCalls, w.Code, w.Body.String())
	}
	var last []byte
	for range wantCalls {
		last = <-requests
	}
	if gjson.GetBytes(last, "input.#").Int() != 0 {
		t.Fatalf("batch recovery retained missing reasoning: %s", last)
	}
	if w.Code != http.StatusNotFound {
		t.Fatalf("failed recovery status=%d, want 404", w.Code)
	}
}

func TestProxy_SSEContextLengthExceededReturns400WithoutRetryOrCooldown(t *testing.T) {
	t.Parallel()
	var firstCalls atomic.Int32
	upstream1 := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firstCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, "event: error\n")
		_, _ = fmt.Fprint(w, "data: "+`{"type":"error","error":{"type":"invalid_request_error","code":"context_length_exceeded","message":"Your input exceeds the context window of this model. Please adjust your input and try again.","param":"input"},"sequence_number":2}`+"\n\n")
	}))
	defer upstream1.Close()

	var secondCalls atomic.Int32
	upstream2 := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: "+`{"type":"response.completed","response":{"id":"resp-unexpected","status":"completed","output":[]}}`+"\n\n")
	}))
	defer upstream2.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "context-too-large", upstreamProtocol: "codex", models: "gpt-test", apiKey: "sk-1", priority: 100},
		{name: "must-not-run", upstreamProtocol: "codex", models: "gpt-test", apiKey: "sk-2", priority: 50},
	}, map[int]string{0: upstream1.URL, 1: upstream2.URL})

	w := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
		"model":  "gpt-test",
		"stream": true,
		"input":  "long conversation",
	}, nil)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400; body=%s", w.Code, w.Body.String())
	}
	if got := gjson.GetBytes(w.Body.Bytes(), "error.code").String(); got != "context_length_exceeded" {
		t.Fatalf("error.code=%q, want context_length_exceeded; body=%s", got, w.Body.String())
	}
	if firstCalls.Load() != 1 || secondCalls.Load() != 0 {
		t.Fatalf("upstream calls first=%d second=%d, want 1/0", firstCalls.Load(), secondCalls.Load())
	}

	ctx := context.Background()
	keyCooldowns, err := env.store.GetAllKeyCooldowns(ctx)
	if err != nil {
		t.Fatalf("GetAllKeyCooldowns: %v", err)
	}
	if len(keyCooldowns) != 0 {
		t.Fatalf("key cooldowns=%v, want none", keyCooldowns)
	}
	modelCooldowns, err := env.store.GetAllModelCooldowns(ctx)
	if err != nil {
		t.Fatalf("GetAllModelCooldowns: %v", err)
	}
	if len(modelCooldowns) != 0 {
		t.Fatalf("model cooldowns=%v, want none", modelCooldowns)
	}
	channelCooldowns, err := env.store.GetAllChannelCooldowns(ctx)
	if err != nil {
		t.Fatalf("GetAllChannelCooldowns: %v", err)
	}
	if len(channelCooldowns) != 0 {
		t.Fatalf("channel cooldowns=%v, want none", channelCooldowns)
	}
}

func TestProxy_GeminiCachedContentNotFoundReturns403WithoutRetryOrCooldown(t *testing.T) {
	t.Parallel()
	const notFound = `{"error":{"code":403,"message":"CachedContent not found (or permission denied)","status":"PERMISSION_DENIED"}}`
	var firstCalls atomic.Int32
	upstream1 := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firstCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = fmt.Fprint(w, notFound)
	}))
	defer upstream1.Close()

	var secondCalls atomic.Int32
	upstream2 := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":"unexpected"}]}}]}`)
	}))
	defer upstream2.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "cache-missing", upstreamProtocol: "gemini", models: "gemini-test", apiKey: "sk-1", priority: 100},
		{name: "must-not-run", upstreamProtocol: "gemini", models: "gemini-test", apiKey: "sk-2", priority: 50},
	}, map[int]string{0: upstream1.URL, 1: upstream2.URL})

	w := doProxyRequest(t, env.engine, "/v1beta/models/gemini-test:generateContent", map[string]any{
		"cachedContent": "cachedContents/unknown",
		"contents":      []map[string]any{{"role": "user", "parts": []map[string]any{{"text": "hi"}}}},
	}, nil)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status=%d, want 403; body=%s", w.Code, w.Body.String())
	}
	if got := gjson.GetBytes(w.Body.Bytes(), "error.status").String(); got != "PERMISSION_DENIED" {
		t.Fatalf("error.status=%q, want upstream PERMISSION_DENIED; body=%s", got, w.Body.String())
	}
	if firstCalls.Load() != 1 || secondCalls.Load() != 0 {
		t.Fatalf("upstream calls first=%d second=%d, want 1/0", firstCalls.Load(), secondCalls.Load())
	}

	ctx := context.Background()
	keyCooldowns, err := env.store.GetAllKeyCooldowns(ctx)
	if err != nil {
		t.Fatalf("GetAllKeyCooldowns: %v", err)
	}
	if len(keyCooldowns) != 0 {
		t.Fatalf("key cooldowns=%v, want none", keyCooldowns)
	}
	channelCooldowns, err := env.store.GetAllChannelCooldowns(ctx)
	if err != nil {
		t.Fatalf("GetAllChannelCooldowns: %v", err)
	}
	if len(channelCooldowns) != 0 {
		t.Fatalf("channel cooldowns=%v, want none", channelCooldowns)
	}
}

func TestProxy_MultimodalFallbackRewritesRequestModel(t *testing.T) {
	t.Parallel()
	upstreamModels := make(chan string, 4)
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read upstream request: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		upstreamModel := gjson.GetBytes(body, "model").String()
		upstreamModels <- upstreamModel
		responseModel := upstreamModel
		if upstreamModel == "gpt-vision" {
			responseModel = "gpt-vision-served"
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"chat-1","model":%q,"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`, responseModel)
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{{
		name: "multimodal-fallback-http", upstreamProtocol: "openai",
		models: "gpt-text,gpt-vision", priority: 100,
	}}, map[int]string{0: upstream.URL})
	env.server.setMultimodalFallbackModels(map[string]string{"gpt-text": "gpt-vision"})

	imageParts := []any{
		map[string]any{"type": "text", "text": "describe this"},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://example.com/a.png"}},
	}

	// 含图请求命中映射 → 按回退模型选路并改写上游 body。
	response := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model":    "gpt-text",
		"messages": []any{map[string]any{"role": "user", "content": imageParts}},
	}, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("image request status=%d body=%s", response.Code, response.Body.String())
	}
	if got := <-upstreamModels; got != "gpt-vision" {
		t.Fatalf("upstream model=%q, want gpt-vision", got)
	}
	fallbackLog := waitForProxyLog(t, env, "gpt-text")
	if fallbackLog.ActualModel != "gpt-vision" || fallbackLog.ResponseModel != "gpt-vision-served" {
		t.Fatalf("fallback log model=%q actual_model=%q response_model=%q, want gpt-text / gpt-vision / gpt-vision-served",
			fallbackLog.Model, fallbackLog.ActualModel, fallbackLog.ResponseModel)
	}

	// 纯文本请求不触发改写。
	textResponse := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model":    "gpt-text",
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}, nil)
	if textResponse.Code != http.StatusOK {
		t.Fatalf("text request status=%d body=%s", textResponse.Code, textResponse.Body.String())
	}
	if got := <-upstreamModels; got != "gpt-text" {
		t.Fatalf("upstream model=%q, want gpt-text", got)
	}

	// 含图但模型不在映射里 → 模型不变。
	unmappedResponse := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model":    "gpt-vision",
		"messages": []any{map[string]any{"role": "user", "content": imageParts}},
	}, nil)
	if unmappedResponse.Code != http.StatusOK {
		t.Fatalf("unmapped request status=%d body=%s", unmappedResponse.Code, unmappedResponse.Body.String())
	}
	if got := <-upstreamModels; got != "gpt-vision" {
		t.Fatalf("upstream model=%q, want gpt-vision", got)
	}
}

func TestProxy_MixedModelKeyCooldownFallback(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name          string
		fallback      bool
		secondChannel bool
		wantStatus    int
		wantKey       string
	}{
		{"enabled", true, false, http.StatusOK, "Bearer only-x"},
		{"disabled", false, false, http.StatusServiceUnavailable, ""},
		{"earlier channel", true, true, http.StatusOK, "Bearer second-key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called := make(chan string, 4)
			upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Model string `json:"model"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if body.Model != "x" {
					t.Errorf("upstream model=%q, want x", body.Model)
				}
				called <- r.Header.Get("Authorization")
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"chat-1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
			}))
			defer upstream.Close()
			channels := []testChannel{{name: "mixed", upstreamProtocol: "openai", modelEntries: []model.ModelEntry{{Model: "a", RedirectModel: "x"}, {Model: "a", RedirectModel: "y"}}}}
			if tc.secondChannel {
				channels = append(channels, testChannel{name: "second", upstreamProtocol: "openai", modelEntries: []model.ModelEntry{{Model: "a", RedirectModel: "x"}}})
			}
			env := setupProxyTestEnvWithSettings(t, channels, map[int]string{0: upstream.URL, 1: upstream.URL}, map[string]string{"cooldown_fallback_enabled": strconv.FormatBool(tc.fallback)})
			ctx := context.Background()
			if err := env.store.DeleteAllAPIKeys(ctx, 1); err != nil {
				t.Fatal(err)
			}
			if err := env.store.CreateAPIKeysBatch(ctx, []*model.APIKey{{ChannelID: 1, KeyIndex: 0, APIKey: "only-x", DetectedModels: []string{"x"}}, {ChannelID: 1, KeyIndex: 1, APIKey: "only-y", DetectedModels: []string{"y"}}}); err != nil {
				t.Fatal(err)
			}
			if err := env.store.SetModelCooldown(ctx, 1, "x", time.Now().Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			if err := env.store.SetKeyCooldown(ctx, 1, 1, time.Now().Add(2*time.Hour)); err != nil {
				t.Fatal(err)
			}
			if tc.secondChannel {
				if err := env.store.DeleteAllAPIKeys(ctx, 2); err != nil {
					t.Fatal(err)
				}
				if err := env.store.CreateAPIKeysBatch(ctx, []*model.APIKey{{ChannelID: 2, KeyIndex: 0, APIKey: "second-key"}}); err != nil {
					t.Fatal(err)
				}
				if err := env.store.SetModelCooldown(ctx, 2, "x", time.Now().Add(30*time.Minute)); err != nil {
					t.Fatal(err)
				}
				env.server.InvalidateAPIKeysCache(2)
			}
			env.server.InvalidateChannelListCache()
			env.server.InvalidateAPIKeysCache(1)
			env.server.invalidateCooldownCache()
			response := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{"model": "a", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}, nil)
			if response.Code != tc.wantStatus {
				t.Fatalf("status=%d, want %d: %s", response.Code, tc.wantStatus, response.Body.String())
			}
			if tc.wantKey != "" {
				select {
				case got := <-called:
					if got != tc.wantKey {
						t.Errorf("key=%q, want %q", got, tc.wantKey)
					}
				default:
					t.Fatal("upstream not called")
				}
			} else if len(called) != 0 {
				t.Fatal("disabled fallback called upstream")
			}
		})
	}
}

func TestProxy_AnthropicLocalErrorsUseAnthropicShape(t *testing.T) {
	t.Parallel()

	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	env := setupProxyTestEnv(t, []testChannel{
		{name: "claude", upstreamProtocol: "anthropic", models: "claude-sonnet-4-6", apiKey: "sk-1"},
	}, map[int]string{0: upstream.URL})
	request := func(model string) *httptest.ResponseRecorder {
		return doProxyRequest(t, env.engine, "/v1/messages", map[string]any{
			"model": model, "max_tokens": 16,
			"messages": []map[string]string{{"role": "user", "content": "hi"}},
		}, nil)
	}

	w := request("claude-unknown-model")
	if w.Code != http.StatusNotFound {
		t.Fatalf("unknown model status=%d body=%s", w.Code, w.Body.String())
	}
	if got := gjson.Get(w.Body.String(), "type").String(); got != "error" {
		t.Fatalf("type=%q body=%s", got, w.Body.String())
	}
	if got := gjson.Get(w.Body.String(), "error.type").String(); got != "not_found_error" {
		t.Fatalf("error.type=%q body=%s", got, w.Body.String())
	}

	tokenHash := model.HashToken("test-api-key")
	env.server.authService.authTokensMux.Lock()
	env.server.authService.authTokenCostLimits[tokenHash] = tokenCostLimit{usedMicroUSD: 200_000, limitMicroUSD: 100_000}
	env.server.authService.authTokensMux.Unlock()

	w = request("claude-sonnet-4-6")
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("cost limit status=%d body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("x-should-retry"); got != "false" {
		t.Fatalf("x-should-retry=%q", got)
	}
	if got := gjson.Get(w.Body.String(), "error.type").String(); got != "rate_limit_error" {
		t.Fatalf("error.type=%q body=%s", got, w.Body.String())
	}
	if got := gjson.Get(w.Body.String(), "error.code").String(); got != "cost_limit_exceeded" {
		t.Fatalf("error.code=%q body=%s", got, w.Body.String())
	}
}

func TestProxy_AnthropicCommittedStreamInterruptionEndsWithErrorEvent(t *testing.T) {
	t.Parallel()

	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-sonnet-4-6\",\"usage\":{\"input_tokens\":3,\"output_tokens\":0}}}\n\n"+
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hel\"}}\n\n")
		// 没有 message_stop 就结束：模拟上游断流。
	}))
	defer upstream.Close()
	env := setupProxyTestEnv(t, []testChannel{
		{name: "claude", upstreamProtocol: "anthropic", models: "claude-sonnet-4-6", apiKey: "sk-1"},
	}, map[int]string{0: upstream.URL})

	w := doProxyRequest(t, env.engine, "/v1/messages", map[string]any{
		"model": "claude-sonnet-4-6", "max_tokens": 16, "stream": true,
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	events := strings.Split(strings.TrimSpace(w.Body.String()), "\n\n")
	last := events[len(events)-1]
	eventType, data := parseSSEEventChunk([]byte(last + "\n\n"))
	if eventType != "error" {
		t.Fatalf("last event=%q, want error; body=%s", eventType, w.Body.String())
	}
	if got := gjson.GetBytes(data, "type").String(); got != "error" {
		t.Fatalf("data.type=%q data=%s", got, data)
	}
	if got := gjson.GetBytes(data, "error.type").String(); got != "api_error" {
		t.Fatalf("error.type=%q data=%s", got, data)
	}
	if !strings.Contains(w.Body.String(), "\"text\":\"hel\"") {
		t.Fatalf("committed prefix missing: %s", w.Body.String())
	}
}

func TestProxy_AnthropicStreamIdleTimeout(t *testing.T) {
	t.Parallel()

	const messageStart = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-sonnet-4-6\",\"usage\":{\"input_tokens\":3,\"output_tokens\":0}}}\n\n"
	const idle = 150 * time.Millisecond
	tests := []struct {
		name      string
		upstream  func(w http.ResponseWriter, r *http.Request)
		wantError bool
	}{
		{
			name: "silent upstream is cut",
			upstream: func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, messageStart)
				w.(http.Flusher).Flush()
				select {
				case <-r.Context().Done():
				case <-time.After(5 * time.Second):
				}
			},
			wantError: true,
		},
		{
			name: "pings keep stream alive",
			upstream: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, messageStart)
				w.(http.Flusher).Flush()
				for range 4 {
					time.Sleep(idle / 2)
					_, _ = io.WriteString(w, "event: ping\ndata: {\"type\":\"ping\"}\n\n")
					w.(http.Flusher).Flush()
				}
				_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				tt.upstream(w, r)
			}))
			defer upstream.Close()
			env := setupProxyTestEnv(t, []testChannel{
				{name: "claude", upstreamProtocol: "anthropic", models: "claude-sonnet-4-6", apiKey: "sk-1"},
			}, map[int]string{0: upstream.URL})
			env.server.protocolTimeouts[string(protocol.Anthropic)] = protocolTimeoutConfig{StreamIdleTimeout: idle}

			w := doProxyRequest(t, env.engine, "/v1/messages", map[string]any{
				"model": "claude-sonnet-4-6", "max_tokens": 16, "stream": true,
				"messages": []map[string]string{{"role": "user", "content": "hi"}},
			}, nil)
			events := strings.Split(strings.TrimSpace(w.Body.String()), "\n\n")
			eventType, _ := parseSSEEventChunk([]byte(events[len(events)-1] + "\n\n"))
			entry := waitForProxyLog(t, env, "claude-sonnet-4-6")
			if tt.wantError {
				if eventType != "error" {
					t.Fatalf("last event=%q, want error; body=%s", eventType, w.Body.String())
				}
				if entry.StatusCode != util.StatusStreamIncomplete || !strings.Contains(entry.Message, "idle timeout") {
					t.Fatalf("log status=%d message=%q, want 599 idle timeout", entry.StatusCode, entry.Message)
				}
				return
			}
			if eventType != "message_stop" || entry.StatusCode != http.StatusOK {
				t.Fatalf("last event=%q log status=%d message=%q; body=%s", eventType, entry.StatusCode, entry.Message, w.Body.String())
			}
		})
	}
}

// 已向客户端提交后中断的流，上游已为已解析的用量收费：日志成本与令牌费用都要计入，
// 否则令牌限额、渠道日限额和 OAuth 窗口全部漏计。
func TestProxy_InterruptedCommittedStreamBillsPartialUsage(t *testing.T) {
	t.Parallel()

	const streamPrefix = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-sonnet-4-6\",\"usage\":{\"input_tokens\":1000,\"output_tokens\":1}}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hel\"}}\n\n"
	tests := []struct {
		name         string
		clientCancel bool
		wantStatus   int
		wantFailures int64
	}{
		{name: "upstream cut", wantStatus: util.StatusStreamIncomplete, wantFailures: 1},
		{name: "client cancel", clientCancel: true, wantStatus: StatusClientClosedRequest, wantFailures: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, streamPrefix)
				w.(http.Flusher).Flush()
				if tt.clientCancel {
					select {
					case <-r.Context().Done():
					case <-time.After(5 * time.Second):
					}
				}
			}))
			defer upstream.Close()
			env := setupProxyTestEnv(t, []testChannel{
				{name: "claude", upstreamProtocol: "anthropic", models: "claude-sonnet-4-6", apiKey: "sk-1"},
			}, map[int]string{0: upstream.URL})

			ctx := context.Background()
			tokenHash := model.HashToken("test-api-key")
			if err := env.store.CreateAuthToken(ctx, &model.AuthToken{Token: tokenHash, IsActive: true}); err != nil {
				t.Fatalf("CreateAuthToken: %v", err)
			}
			stored, err := env.store.GetAuthTokenByValue(ctx, tokenHash)
			if err != nil {
				t.Fatalf("GetAuthTokenByValue: %v", err)
			}
			injectAPIToken(env.server.authService, "test-api-key", 0, stored.ID)

			body, _ := json.Marshal(map[string]any{
				"model": "claude-sonnet-4-6", "max_tokens": 16, "stream": true,
				"messages": []map[string]string{{"role": "user", "content": "hi"}},
			})
			reqCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			req := httptest.NewRequestWithContext(reqCtx, http.MethodPost, "/v1/messages", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer test-api-key")
			w := newAsyncResponseRecorder()
			done := make(chan struct{})
			go func() {
				defer close(done)
				env.engine.ServeHTTP(w, req)
			}()
			if tt.clientCancel {
				for committed := false; !committed; {
					select {
					case chunk := <-w.writes:
						committed = bytes.Contains(chunk, []byte(`"text":"hel"`))
					case <-time.After(5 * time.Second):
						t.Fatal("committed delta never reached the client")
					}
				}
				cancel()
			}
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("proxy request did not finish")
			}

			entry := waitForProxyLogMatching(t, env, func(e *model.LogEntry) bool { return e.StatusCode == tt.wantStatus })
			if entry == nil {
				t.Fatalf("no proxy log with status %d", tt.wantStatus)
			}
			if entry.InputTokens != 1000 || entry.Cost <= 0 {
				t.Fatalf("log input_tokens=%d cost=%v, want 1000 and >0", entry.InputTokens, entry.Cost)
			}
			for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(20 * time.Millisecond) {
				token, err := env.store.GetAuthTokenByValue(ctx, tokenHash)
				if err == nil && token.TotalCostUSD > 0 {
					if math.Abs(token.TotalCostUSD-entry.Cost) > 1e-12 || token.PromptTokensTotal != 1000 {
						t.Fatalf("token cost=%v prompt=%d, want %v and 1000", token.TotalCostUSD, token.PromptTokensTotal, entry.Cost)
					}
					if token.SuccessCount != 0 || token.FailureCount != tt.wantFailures {
						t.Fatalf("token success=%d failure=%d, want 0 and %d", token.SuccessCount, token.FailureCount, tt.wantFailures)
					}
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("token stats not billed: token=%+v err=%v", token, err)
				}
			}
		})
	}
}

func TestProxy_SessionAffinityRespectsEffectivePriority(t *testing.T) {
	t.Parallel()
	for _, protocol := range []string{"anthropic", "codex"} {
		t.Run(protocol, func(t *testing.T) {
			t.Parallel()
			for _, tt := range []struct {
				name         string
				health       bool
				aSuccessRate float64
				bSuccessRate float64
				want         string
			}{
				{name: "lower_effective_priority", health: true, aSuccessRate: 1, bSuccessRate: 0.8, want: "a"},
				// A=179.99、B=179.96 属同一有效分档，粘性仍可保留较低有效分的 B。
				{name: "same_effective_bucket", health: true, aSuccessRate: 0.9999, bSuccessRate: 0.8996, want: "b"},
				{name: "health_disabled", aSuccessRate: 1, bSuccessRate: 0.8, want: "b"},
			} {
				t.Run(tt.name, func(t *testing.T) {
					t.Parallel()
					served := make(chan string, 8)
					upstreams := make(map[int]string, 2)
					channels := make([]testChannel, 0, 2)
					for i, name := range []string{"a", "b"} {
						upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
							served <- name
							if protocol == "anthropic" {
								w.Header().Set("Content-Type", "application/json")
								_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"test-model","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
								return
							}
							w.Header().Set("Content-Type", "text/event-stream")
							_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`+"\n\n")
						}))
						defer upstream.Close()
						upstreams[i] = upstream.URL
						channels = append(channels, testChannel{name: name, upstreamProtocol: protocol, models: "test-model", priority: 180 + i*10})
					}
					env := setupProxyTestEnv(t, channels, upstreams)
					healthCfg := model.DefaultHealthScoreConfig()
					healthCfg.Enabled = tt.health
					env.server.healthCache = &HealthCache{config: healthCfg}
					path, sessionHeader := "/v1/messages", "X-Claude-Code-Session-Id"
					body := map[string]any{
						"model": "test-model", "max_tokens": 16,
						"messages": []any{map[string]any{"role": "user", "content": "hi"}},
					}
					if protocol == "codex" {
						path, sessionHeader = "/v1/responses", "Session-Id"
						body = map[string]any{"model": "test-model", "stream": true, "input": "hi"}
					}
					headers := map[string]string{sessionHeader: uuid.NewString()}
					send := func() string {
						t.Helper()
						response := doProxyRequest(t, env.engine, path, body, headers)
						if response.Code != http.StatusOK {
							t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
						}
						select {
						case name := <-served:
							return name
						default:
							t.Fatal("request succeeded without reaching an upstream")
							return ""
						}
					}
					if got := send(); got != "b" {
						t.Fatalf("initial session served by %s, want b", got)
					}
					configs, err := env.store.ListConfigs(context.Background())
					if err != nil {
						t.Fatalf("ListConfigs: %v", err)
					}
					stats := make(map[int64]model.ChannelHealthStats, len(configs))
					for _, cfg := range configs {
						successRate := tt.aSuccessRate
						if cfg.Name == "b" {
							successRate = tt.bSuccessRate
						}
						stats[cfg.ID] = model.ChannelHealthStats{SuccessRate: successRate, SampleCount: 20}
					}
					env.server.healthCache.snapshot.Store(newChannelHealthSnapshot(stats))
					if got := send(); got != tt.want {
						t.Fatalf("session served by %s after health changed, want %s", got, tt.want)
					}
				})
			}
		})
	}
}

func TestProxy_AnthropicSessionAffinity(t *testing.T) {
	t.Parallel()
	names := []string{"primary", "peer-a", "peer-b"}
	var failing sync.Map // name → bool
	failing.Store("primary", true)
	served := make(chan string, 64)
	upstreams := make(map[int]string, len(names))
	channels := make([]testChannel, 0, len(names))
	for index, name := range names {
		upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if broken, _ := failing.Load(name); broken == true {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = io.WriteString(w, `{"type":"error","error":{"type":"api_error","message":"boom"}}`)
				return
			}
			served <- name
			_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"claude-test","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
		}))
		defer upstream.Close()
		upstreams[index] = upstream.URL
		priority := 100
		if name == "primary" {
			priority = 200
		}
		channels = append(channels, testChannel{name: name, upstreamProtocol: "anthropic", models: "claude-test", priority: priority})
	}
	env := setupProxyTestEnv(t, channels, upstreams)
	ctx := context.Background()
	configs, err := env.store.ListConfigs(ctx)
	if err != nil {
		t.Fatalf("ListConfigs: %v", err)
	}
	channelIDs := make(map[string]int64, len(configs))
	for _, cfg := range configs {
		channelIDs[cfg.Name] = cfg.ID
	}
	heal := func(name string) {
		t.Helper()
		failing.Store(name, false)
		if err := env.server.cooldownManager.ClearAllCooldowns(ctx, channelIDs[name]); err != nil {
			t.Fatalf("ClearAllCooldowns(%s): %v", name, err)
		}
		env.server.invalidateChannelRelatedCache(channelIDs[name])
	}
	send := func(sessionID string) string {
		t.Helper()
		headers := map[string]string{}
		if sessionID != "" {
			headers["X-Claude-Code-Session-Id"] = sessionID
		}
		response := doProxyRequest(t, env.engine, "/v1/messages", map[string]any{
			"model": "claude-test", "max_tokens": 16,
			"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		}, headers)
		if response.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
		select {
		case name := <-served:
			return name
		default:
			t.Fatal("request succeeded without reaching an upstream")
			return ""
		}
	}

	// 主渠道故障后，无会话请求在同层两个渠道间轮询：证明粘性不是测试环境的巧合。
	rotated := map[string]bool{}
	for range 4 {
		rotated[send("")] = true
	}
	if !rotated["peer-a"] || !rotated["peer-b"] {
		t.Fatalf("requests without session hit %v, want rotation across peer-a and peer-b", rotated)
	}

	session := uuid.NewString()
	bound := send(session)
	for range 3 {
		if got := send(session); got != bound {
			t.Fatalf("session moved from %s to %s while %s stayed healthy", bound, got, bound)
		}
	}

	// 绑定渠道失败后切到同层另一渠道；它恢复后会话仍留在新渠道，不回到轮询。
	other := "peer-a"
	if bound == other {
		other = "peer-b"
	}
	failing.Store(bound, true)
	if got := send(session); got != other {
		t.Fatalf("failover served by %s, want %s", got, other)
	}
	heal(bound)
	for range 3 {
		if got := send(session); got != other {
			t.Fatalf("session served by %s after rebinding, want %s", got, other)
		}
	}

	// 更高优先级渠道恢复后，主备意图优先于会话粘性。
	heal("primary")
	if got := send(session); got != "primary" {
		t.Fatalf("session served by %s after primary recovered, want primary", got)
	}
}

// Codex 会话按 Session-Id 粘在一个账号上；客户端回带的 turn-state 只发回签发它的账号，
// 故障转移到其他账号时删除，不把 A 账号的 sticky routing 令牌泄露给 B 账号。
func TestProxy_CodexSessionAffinityScopesTurnState(t *testing.T) {
	t.Parallel()
	type upstreamHit struct {
		name      string
		turnState []string
	}
	names := []string{"account-a", "account-b"}
	var failing sync.Map // name → bool
	hits := make(chan upstreamHit, 64)
	upstreams := make(map[int]string, len(names))
	channels := make([]testChannel, 0, len(names))
	for index, name := range names {
		upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if broken, _ := failing.Load(name); broken == true {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = io.WriteString(w, `{"error":{"type":"server_error","message":"boom"}}`)
				return
			}
			hits <- upstreamHit{name: name, turnState: r.Header.Values("X-Codex-Turn-State")}
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("X-Codex-Turn-State", "state-"+name)
			_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{"id":"resp-`+name+`","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`+"\n\n")
		}))
		defer upstream.Close()
		upstreams[index] = upstream.URL
		channels = append(channels, testChannel{
			name: name, upstreamProtocol: "codex", models: "gpt-test", priority: 100,
			authType:        model.AuthTypeCodexOAuth,
			oauthCredential: codexProxyTestCredential(t, "at-"+name, "rt-"+name, name),
		})
	}
	env := setupProxyTestEnv(t, channels, upstreams)
	ctx := context.Background()
	configs, err := env.store.ListConfigs(ctx)
	if err != nil {
		t.Fatalf("ListConfigs: %v", err)
	}
	channelIDs := make(map[string]int64, len(configs))
	for _, cfg := range configs {
		channelIDs[cfg.Name] = cfg.ID
	}
	send := func(sessionID, turnState string) (upstreamHit, string) {
		t.Helper()
		headers := map[string]string{}
		if sessionID != "" {
			headers["Session-Id"] = sessionID
		}
		if turnState != "" {
			headers["X-Codex-Turn-State"] = turnState
		}
		response := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
			"model": "gpt-test", "stream": true, "input": "hi",
		}, headers)
		if response.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
		select {
		case hit := <-hits:
			return hit, response.Header().Get("X-Codex-Turn-State")
		default:
			t.Fatal("request succeeded without reaching an upstream")
			return upstreamHit{}, ""
		}
	}

	rotated := map[string]bool{}
	for range 4 {
		hit, _ := send("", "")
		rotated[hit.name] = true
	}
	if !rotated["account-a"] || !rotated["account-b"] {
		t.Fatalf("requests without session hit %v, want rotation across both accounts", rotated)
	}

	session := uuid.NewString()
	first, boundState := send(session, "")
	bound := first.name
	if boundState == "state-"+bound || !strings.HasSuffix(boundState, "state-"+bound) {
		t.Fatalf("relayed turn-state = %q, want account-tagged state-%s", boundState, bound)
	}
	for range 2 {
		hit, _ := send(session, boundState)
		if hit.name != bound {
			t.Fatalf("session moved from %s to %s while %s stayed healthy", bound, hit.name, bound)
		}
		if len(hit.turnState) != 1 || hit.turnState[0] != "state-"+bound {
			t.Fatalf("issuing account received turn-state %q, want original state-%s", hit.turnState, bound)
		}
	}

	other := "account-a"
	if bound == other {
		other = "account-b"
	}
	failing.Store(bound, true)
	hit, otherState := send(session, boundState)
	if hit.name != other || len(hit.turnState) != 0 {
		t.Fatalf("failover hit %s with turn-state %q, want %s without turn-state", hit.name, hit.turnState, other)
	}
	if !strings.HasSuffix(otherState, "state-"+other) || otherState == "state-"+other {
		t.Fatalf("failover relayed turn-state = %q, want account-tagged state-%s", otherState, other)
	}

	// 原账号恢复后会话留在新账号，客户端本 turn 仍回带旧账号的令牌，照样删除。
	failing.Store(bound, false)
	if err := env.server.cooldownManager.ClearAllCooldowns(ctx, channelIDs[bound]); err != nil {
		t.Fatalf("ClearAllCooldowns(%s): %v", bound, err)
	}
	env.server.invalidateChannelRelatedCache(channelIDs[bound])
	hit, _ = send(session, boundState)
	if hit.name != other || len(hit.turnState) != 0 {
		t.Fatalf("rebound session hit %s with turn-state %q, want %s without turn-state", hit.name, hit.turnState, other)
	}
	hit, _ = send(session, "untagged-state")
	if hit.name != other || len(hit.turnState) != 1 || hit.turnState[0] != "untagged-state" {
		t.Fatalf("untagged turn-state reached %s as %q, want unchanged on %s", hit.name, hit.turnState, other)
	}
}

func TestProxy_AnthropicSessionAffinityPinsKey(t *testing.T) {
	t.Parallel()
	servedKeys := make(chan string, 16)
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		servedKeys <- r.Header.Get("X-Api-Key") + r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"claude-test","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer upstream.Close()
	env := setupProxyTestEnv(t, []testChannel{{
		name: "multi-key", upstreamProtocol: "anthropic", models: "claude-test", apiKey: "sk-ant-first",
	}}, map[int]string{0: upstream.URL})
	ctx := context.Background()
	configs, err := env.store.ListConfigs(ctx)
	if err != nil || len(configs) != 1 {
		t.Fatalf("ListConfigs: %v (%d configs)", err, len(configs))
	}
	channelID := configs[0].ID
	if err := env.store.CreateAPIKeysBatch(ctx, []*model.APIKey{{ChannelID: channelID, KeyIndex: 1, APIKey: "sk-ant-second"}}); err != nil {
		t.Fatalf("CreateAPIKeysBatch: %v", err)
	}
	env.server.InvalidateAPIKeysCache(channelID)

	send := func(sessionID string) string {
		t.Helper()
		headers := map[string]string{}
		if sessionID != "" {
			headers["X-Claude-Code-Session-Id"] = sessionID
		}
		response := doProxyRequest(t, env.engine, "/v1/messages", map[string]any{
			"model": "claude-test", "max_tokens": 16,
			"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		}, headers)
		if response.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
		return <-servedKeys
	}

	rotated := map[string]bool{}
	for range 4 {
		rotated[send("")] = true
	}
	if len(rotated) != 2 {
		t.Fatalf("requests without session used keys %v, want rotation across both keys", rotated)
	}
	session := uuid.NewString()
	bound := send(session)
	for range 3 {
		if got := send(session); got != bound {
			t.Fatalf("session switched key from %s to %s", bound, got)
		}
	}
}

// OpenAI 兼容入口（chat/completions）按客户端显式会话 Header 粘在渠道上。
// Gemini 入口共用同一实现，键按协议段隔离，与 Anthropic/Codex 不相交。
func TestProxy_OpenAICompatSessionAffinity(t *testing.T) {
	t.Parallel()
	names := []string{"peer-a", "peer-b"}
	served := make(chan string, 64)
	upstreams := make(map[int]string, len(names))
	channels := make([]testChannel, 0, len(names))
	for index, name := range names {
		upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			served <- name
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"chat-1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
		}))
		defer upstream.Close()
		upstreams[index] = upstream.URL
		channels = append(channels, testChannel{name: name, upstreamProtocol: "openai", models: "gpt-test", priority: 100})
	}
	env := setupProxyTestEnv(t, channels, upstreams)

	send := func(sessionID string) string {
		t.Helper()
		headers := map[string]string{}
		if sessionID != "" {
			headers["X-Session-Id"] = sessionID
		}
		response := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
			"model": "gpt-test", "messages": []any{map[string]any{"role": "user", "content": "hi"}},
		}, headers)
		if response.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
		select {
		case name := <-served:
			return name
		default:
			t.Fatal("request succeeded without reaching an upstream")
			return ""
		}
	}

	// 无会话标识时在同层两个渠道间轮询：证明粘性不是测试环境的巧合。
	rotated := map[string]bool{}
	for range 4 {
		rotated[send("")] = true
	}
	if !rotated["peer-a"] || !rotated["peer-b"] {
		t.Fatalf("requests without session hit %v, want rotation across both peers", rotated)
	}

	session := uuid.NewString()
	bound := send(session)
	for range 3 {
		if got := send(session); got != bound {
			t.Fatalf("session moved from %s to %s while %s stayed healthy", bound, got, bound)
		}
	}
}

// 关闭 enable_session_affinity 后忽略会话标识，回到常规同层轮询。
func TestProxy_SessionAffinityDisabledFallsBackToRotation(t *testing.T) {
	t.Parallel()
	names := []string{"peer-a", "peer-b"}
	served := make(chan string, 64)
	upstreams := make(map[int]string, len(names))
	channels := make([]testChannel, 0, len(names))
	for index, name := range names {
		upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			served <- name
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"chat-1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
		}))
		defer upstream.Close()
		upstreams[index] = upstream.URL
		channels = append(channels, testChannel{name: name, upstreamProtocol: "openai", models: "gpt-test", priority: 100})
	}
	env := setupProxyTestEnvWithSettings(t, channels, upstreams,
		map[string]string{config.SessionAffinityEnabledSettingKey: "false"})

	// 同一个会话反复请求也必须轮到两个渠道，证明显式会话标识没有生效。
	session := uuid.NewString()
	rotated := map[string]bool{}
	for range 4 {
		response := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
			"model": "gpt-test", "messages": []any{map[string]any{"role": "user", "content": "hi"}},
		}, map[string]string{"X-Session-Id": session})
		if response.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
		select {
		case name := <-served:
			rotated[name] = true
		default:
			t.Fatal("request succeeded without reaching an upstream")
		}
	}
	if !rotated["peer-a"] || !rotated["peer-b"] {
		t.Fatalf("sticky disabled but session pinned to %v, want rotation across both peers", rotated)
	}
}

func TestTokenRPMHTTPUpstreamRetriesAndFailureCountOnce(t *testing.T) {
	for _, success := range []bool{true, false} {
		t.Run(fmt.Sprintf("success=%t", success), func(t *testing.T) {
			var hits atomic.Int32
			upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				n := hits.Add(1)
				w.Header().Set("Content-Type", "application/json")
				if n < 3 || !success {
					w.WriteHeader(http.StatusInternalServerError)
					_, _ = io.WriteString(w, `{"error":{"type":"server_error","message":"try again"}}`)
					return
				}
				_, _ = io.WriteString(w, `{"id":"chat-rpm","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
			}))
			env := setupProxyTestEnv(t, []testChannel{{name: "rpm-retry", models: "gpt-rpm", sameChannelRetries: 2}}, map[int]string{0: upstream.URL})
			token := &model.AuthToken{Token: model.HashToken("rpm-retry"), Description: "retry", IsActive: true, RPMLimit: 1}
			if err := env.store.CreateAuthToken(context.Background(), token); err != nil {
				t.Fatal(err)
			}
			if err := env.server.authService.ReloadAuthTokens(); err != nil {
				t.Fatal(err)
			}
			clock := &channelRPMFakeClock{now: time.Unix(1000, 0)}
			env.server.authService.tokenRPMLimiter = newRPMLimiter(clock.Now)
			request := func() *httptest.ResponseRecorder {
				return doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{"model": "gpt-rpm", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}, map[string]string{"Authorization": "Bearer rpm-retry"})
			}
			w := request()
			if success && w.Code != http.StatusOK || !success && w.Code != http.StatusInternalServerError {
				t.Fatalf("first response status=%d body=%s", w.Code, w.Body.String())
			}
			if hits.Load() != 3 {
				t.Fatalf("upstream attempts=%d, want 3", hits.Load())
			}
			assertTokenRPMHTTPError(t, request(), 60)
			if hits.Load() != 3 {
				t.Fatalf("rejected client request reached upstream: %d", hits.Load())
			}
		})
	}
}

// /v1/systemone 只透传到 System One 渠道；聊天入口也永不选中 System One 渠道。
func TestProxySystemOneEndpointRoutesOnlySystemOneChannels(t *testing.T) {
	t.Parallel()
	var systemOneCalls, chatCalls atomic.Int32
	systemOne := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		systemOneCalls.Add(1)
		body, _ := io.ReadAll(r.Body)
		if r.URL.Path != "/v1/systemone" || r.Header.Get("Authorization") != "Bearer sk-one" || r.Header.Get("X-Api-Key") != "" {
			t.Errorf("System One wire: path=%s authorization=%q x-api-key=%q", r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("X-Api-Key"))
		}
		if gjson.GetBytes(body, "model").String() != "jev-1" || gjson.GetBytes(body, "state").String() != "task data" ||
			gjson.GetBytes(body, "questions.effort.type").String() != "choice" {
			t.Errorf("System One body was not passed through: %s", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"model":"jev-1.13.0","answers":{"effort":{"type":"choice","choice":"high"}},"usage":{"input_tokens":12,"output_tokens":6}}`)
	}))
	t.Cleanup(systemOne.Close)
	chat := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chatCalls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chat-1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`)
	}))
	t.Cleanup(chat.Close)
	env := setupProxyTestEnv(t, []testChannel{
		{name: "decision", authType: model.AuthTypeSystemOne, apiKey: "sk-one",
			modelEntries: []model.ModelEntry{{Model: "jev-latest", RedirectModel: "jev-1"}}},
		{name: "chat", upstreamProtocol: "openai", models: "jev-latest,gpt-chat", apiKey: "sk-chat", priority: 100},
	}, map[int]string{0: systemOne.URL, 1: chat.URL})

	decision := map[string]any{"model": "jev-latest", "state": "task data", "stream": true,
		"questions": map[string]any{"effort": map[string]any{"type": "choice", "criteria": map[string]string{"low": "easy", "high": "hard"}}}}
	w := doProxyRequest(t, env.engine, "/v1/systemone", decision, nil)
	if w.Code != http.StatusOK || gjson.Get(w.Body.String(), "answers.effort.choice").String() != "high" {
		t.Fatalf("System One status=%d body=%s", w.Code, w.Body.String())
	}
	entry := waitForProxyLogMatching(t, env, func(entry *model.LogEntry) bool { return entry.ClientProtocol == util.ProtocolSystemOne })
	if entry.UpstreamProtocol != util.ProtocolSystemOne || entry.IsStreaming || entry.InputTokens != 12 || entry.OutputTokens != 6 ||
		entry.ActualModel != "jev-1" || entry.ResponseModel != "jev-1.13.0" {
		t.Fatalf("System One log = %+v", entry)
	}

	// 只有聊天渠道支持的模型不会被转换后发给聊天渠道。
	if w = doProxyRequest(t, env.engine, "/v1/systemone", map[string]any{"model": "gpt-chat", "state": "x"}, nil); w.Code == http.StatusOK {
		t.Fatalf("chat-only model served by /v1/systemone: %s", w.Body.String())
	}
	if w = doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{"model": "jev-latest",
		"messages": []any{map[string]string{"role": "user", "content": "hi"}}}, nil); w.Code != http.StatusOK {
		t.Fatalf("chat status=%d body=%s", w.Code, w.Body.String())
	}
	if systemOneCalls.Load() != 1 || chatCalls.Load() != 1 {
		t.Fatalf("upstream calls: system_one=%d chat=%d", systemOneCalls.Load(), chatCalls.Load())
	}
}
