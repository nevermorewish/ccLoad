package app

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ccLoad/internal/model"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func TestKimiCatalogResponse(t *testing.T) {
	c, w := newTestContext(t, newJSONRequest(t, http.MethodGet, "/admin/model-test/kimi/cases", nil))
	(&Server{}).HandleKimiCases(c)
	root := gjson.Parse(w.Body.String())
	if w.Code != http.StatusOK || !root.Get("success").Bool() || root.Get("data.version").String() != kimiSuiteVersion || len(root.Get("data.cases").Array()) != 12 {
		t.Fatalf("catalog must use the standard single response envelope: %s", w.Body.String())
	}
}

func TestKimiAssertions(t *testing.T) {
	response := func(content, reasoning string) []byte {
		b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": content, "reasoning_content": reasoning}}}})
		return b
	}
	for _, tc := range []struct {
		name   string
		index  int
		body   []byte
		upload float64
		pass   bool
	}{
		{"default thinking", 2, response("437", "calculation"), 0, true},
		{"missing thinking", 2, response("437", ""), 0, false},
		{"thinking off", 8, response("437", ""), 0, true},
		{"thinking leaks", 9, response("437", "calculation"), 0, false},
		{"think tag", 10, response("<think>calculation</think>437", ""), 0, false},
		{"empty response", 8, response("", ""), 0, false},
		{"flower", 5, response("一朵花", ""), 0, true},
		{"wrong flower", 5, response("black", ""), 0, false},
		{"blue", 7, response("Blue", ""), 0, true},
		{"black", 6, response("black", ""), 0, true},
		{"remote blue", 11, response("蓝色", ""), 0, true},
		{"upload observed", 1, response("black", ""), 200, true},
		{"upload missing", 1, response("black", ""), -1, false},
		{"upload too slow", 1, response("black", ""), 61000, false},
		{"malformed", 2, []byte("bad json"), 0, false},
		{"tool", 3, []byte(`{"choices":[{"finish_reason":"tool_calls","message":{"tool_calls":[{"type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"北京\"}"}}]}}]}`), 0, true},
		{"wrong city", 3, []byte(`{"choices":[{"finish_reason":"tool_calls","message":{"tool_calls":[{"type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"上海\"}"}}]}}]}`), 0, false},
		{"wrong finish", 3, []byte(`{"choices":[{"finish_reason":"stop","message":{"tool_calls":[{"type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"北京\"}"}}]}}]}`), 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ok, actual := kimiAssert(kimiCases[tc.index], tc.body, tc.upload, 60)
			if ok != tc.pass {
				t.Fatalf("got %t: %s", ok, actual)
			}
		})
	}
}

func TestKimiPayloadsAndValidation(t *testing.T) {
	opts := kimiRunRequest{Model: "kimi-k3", ImageURL: "https://example.test/blue.png"}
	items, err := opts.validate()
	if err != nil || len(items) != 12 {
		t.Fatalf("catalog: %v %d", err, len(items))
	}
	for _, item := range items {
		body, _ := json.Marshal(kimiPayload(item, "upstream-k3", opts))
		root := gjson.ParseBytes(body)
		if root.Get("model").String() != "upstream-k3" || root.Get("stream").Bool() {
			t.Fatal("model or stream changed")
		}
		if root.Get("messages.0.tools").Exists() {
			if root.Get("messages.0.role").String() != "system" || root.Get("messages.0.content").Type != gjson.String || root.Get("messages.0.content").String() != "" {
				t.Fatalf("%s: dynamic tools require a system message with empty content", item.ID)
			}
			if root.Get("tool_choice").String() != "required" {
				t.Fatalf("%s: dynamic-tool call must remain required", item.ID)
			}
		}
		switch item.ID {
		case "kimi.k3.dynamic_tool_loading_with_global_tool":
			if root.Get("messages.0.tools.0.function.name").String() != "get_weather" || root.Get("tools.0.function.name").String() != "get_stock_price" {
				t.Fatal("missing dynamic/global tool")
			}
		case "kimi.k3.dynamic_tool_loading":
			if root.Get("tools").Exists() || !root.Get("messages.0.tools").Exists() {
				t.Fatal("dynamic tools must be on system message")
			}
		case "kimi.k3.tool_call_ability":
			if root.Get("tool_choice").Exists() {
				t.Fatal("default tool call must not be forced")
			}
		case "kimi.k3.thinking_default":
			if root.Get("enable_thinking").Exists() || root.Get("chat_template_kwargs").Exists() {
				t.Fatal("default thinking must not be overridden")
			}
		case "kimi.k3.video_base64_upload_speed":
			if len(body) < 20<<20 {
				t.Fatalf("payload too small: %d", len(body))
			}
			mediaURL := root.Get("messages.0.content.1.video_url.url").String()
			_, encoded, _ := strings.Cut(mediaURL, ",")
			media, err := base64.StdEncoding.DecodeString(encoded)
			if err != nil {
				t.Fatal(err)
			}
			original, _ := kimiFixtures.ReadFile("kimi_fixtures/black.mp4")
			atom := media[len(original):]
			if string(atom[4:8]) != "free" || int(binary.BigEndian.Uint32(atom[:4])) != len(atom) {
				t.Fatal("invalid MP4 padding")
			}
			preview, _ := json.Marshal(kimiPreview(kimiPayload(item, "upstream-k3", opts)))
			if len(preview) > 2000 {
				t.Fatal("report must not include large base64")
			}
		case "kimi.k3.thinking_off_enable_thinking":
			if !root.Get("enable_thinking").Exists() || root.Get("enable_thinking").Bool() || root.Get("chat_template_kwargs").Exists() {
				t.Fatal("incorrect top-level thinking flag")
			}
		case "kimi.k3.thinking_off_chat_template_enable_thinking":
			if !root.Get("chat_template_kwargs.enable_thinking").Exists() || root.Get("chat_template_kwargs.enable_thinking").Bool() {
				t.Fatal("incorrect template enable_thinking flag")
			}
		case "kimi.k3.thinking_off_chat_template_thinking":
			if !root.Get("chat_template_kwargs.thinking").Exists() || root.Get("chat_template_kwargs.thinking").Bool() {
				t.Fatal("incorrect template thinking flag")
			}
		}
	}
	for _, r := range []kimiRunRequest{{Model: ""}, {Model: "x", CaseIDs: []string{"unknown"}}, {Model: "x", UploadMiB: 100}, {Model: "x", Concurrency: 7}, {Model: "x", ImageURL: "file:///blue.png"}, {Model: "x", ImageURL: "https://secret@example.test/x"}} {
		if _, err := r.validate(); err == nil {
			t.Fatalf("expected invalid options: %+v", r)
		}
	}
	for input, want := range map[string]string{"https://example.test": "https://example.test/v1/chat/completions", "https://example.test/v1/": "https://example.test/v1/chat/completions", "https://example.test/custom#": "https://example.test/custom", "https://example.test/v1/chat/completions": "https://example.test/v1/chat/completions"} {
		actual, err := kimiEndpoint(input)
		if err != nil || actual != want {
			t.Fatalf("%s -> %s, %v", input, actual, err)
		}
	}
}

func TestKimiHandlerFailureIsolation(t *testing.T) {
	var active, maxActive, requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := active.Add(1)
		defer active.Add(-1)
		for {
			old := maxActive.Load()
			if n <= old || maxActive.CompareAndSwap(old, n) {
				break
			}
		}
		requests.Add(1)
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer sk-kimi-test" {
			t.Error("incorrect URL or credential")
		}
		raw, _ := io.ReadAll(r.Body)
		root := gjson.ParseBytes(raw)
		if root.Get("model").String() != "kimi-k3-upstream" {
			t.Error("model redirect lost")
		}
		time.Sleep(60 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		// Match Kimi's validation: content and dynamic tools cannot both be
		// non-empty. This catches malformed probes before mocking inference.
		for _, message := range root.Get("messages").Array() {
			if message.Get("tools").Exists() && message.Get("content").String() != "" {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"error":"non-empty content together with dynamic tools"}`)
				return
			}
		}
		if root.Get("enable_thinking").Exists() {
			w.WriteHeader(400)
			_, _ = io.WriteString(w, `{"error":"unsupported flag sk-kimi-test"}`)
			return
		}
		message := map[string]any{"content": "437"}
		finish := "stop"
		if root.Get("tools").Exists() || root.Get("messages.0.tools").Exists() {
			message = map[string]any{"tool_calls": []any{map[string]any{"type": "function", "function": map[string]any{"name": "get_weather", "arguments": `{"city":"北京"}`}}}}
			finish = "tool_calls"
		} else if root.Get("messages.0.content").IsArray() {
			prompt := root.Get("messages.0.content.0.text").String()
			message["content"] = "黑色"
			if strings.Contains(prompt, "主要物体") {
				message["content"] = "花"
			}
			if strings.Contains(prompt, "图片") {
				message["content"] = "蓝色"
			}
		} else if !root.Get("chat_template_kwargs").Exists() {
			message["reasoning_content"] = "17 * 23 + 46"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"finish_reason": finish, "message": message}}})
	}))
	defer upstream.Close()
	srv := newInMemoryServer(t)
	srv.client = upstream.Client()
	ctx := context.Background()
	cfg, err := srv.store.CreateConfig(ctx, &model.Config{Name: "Kimi local test", URLs: model.ChannelURLs{{URL: upstream.URL}}, Priority: 1, Enabled: true, ModelEntries: []model.ModelEntry{{Model: "kimi-k3", RedirectModel: "kimi-k3-upstream"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err = srv.store.CreateAPIKeysBatch(ctx, []*model.APIKey{{ChannelID: cfg.ID, KeyIndex: 0, APIKey: "sk-kimi-test"}}); err != nil {
		t.Fatal(err)
	}
	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/test", kimiRunRequest{Model: "kimi-k3", ImageURL: "https://example.test/blue.png", Concurrency: 3, UploadMiB: 1}))
	c.Params = gin.Params{{Key: "id", Value: fmt.Sprint(cfg.ID)}}
	srv.HandleChannelKimiTest(c)
	if w.Code != 200 {
		t.Fatalf("HTTP %d: %s", w.Code, w.Body.String())
	}
	if requests.Load() != 12 || maxActive.Load() > 3 || maxActive.Load() < 2 {
		t.Fatalf("requests=%d max active=%d", requests.Load(), maxActive.Load())
	}
	if strings.Contains(w.Body.String(), "sk-kimi-test") {
		t.Fatal("credential leaked to report")
	}
	finished, completed := 0, false
	for _, frame := range strings.Split(w.Body.String(), "\n\n") {
		if !strings.HasPrefix(frame, "data: ") {
			continue
		}
		var event kimiEvent
		if err = json.Unmarshal([]byte(strings.TrimPrefix(frame, "data: ")), &event); err != nil {
			t.Fatal(err)
		}
		if event.Result != nil {
			finished++
			expected := "passed"
			if event.ID == "kimi.k3.thinking_off_enable_thinking" {
				expected = "failed"
			}
			if event.Result.Status != expected {
				t.Errorf("%s: %+v", event.ID, event.Result)
			}
			if event.ID == "kimi.k3.video_base64_upload_speed" && (event.Result.UploadMS < 0 || event.Result.DurationMS-event.Result.UploadMS < 40) {
				t.Errorf("upload includes inference delay: %+v", event.Result)
			}
		}
		if event.Type == "completed" {
			completed = true
			meta, _ := json.Marshal(event.Meta)
			if gjson.GetBytes(meta, "passed").Int() != 11 || gjson.GetBytes(meta, "failed").Int() != 1 {
				t.Fatalf("wrong summary: %s", meta)
			}
		}
	}
	if finished != 12 || !completed {
		t.Fatalf("incomplete events: %d %t", finished, completed)
	}
}

func TestKimiCancellation(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(started)
		select {
		case <-r.Context().Done():
			close(cancelled)
		case <-time.After(5 * time.Second):
		}
	}))
	defer upstream.Close()
	srv := newInMemoryServer(t)
	srv.client = upstream.Client()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan kimiResult, 1)
	go func() {
		result <- srv.runKimiCase(ctx, &model.Config{}, "sk-test", upstream.URL, "kimi-k3", kimiCases[2], kimiRunRequest{TimeoutSeconds: 30})
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("request not started")
	}
	cancel()
	select {
	case r := <-result:
		if r.Status != "failed" || !strings.Contains(r.Actual, "取消") {
			t.Fatalf("unexpected cancellation result: %+v", r)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("runner did not stop")
	}
	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream context not cancelled")
	}
}
