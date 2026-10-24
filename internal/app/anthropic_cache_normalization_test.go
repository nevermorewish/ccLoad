package app

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	"ccLoad/internal/model"
	"ccLoad/internal/protocol"
)

func TestAnthropicInclusiveCacheSnapshots(t *testing.T) {
	for _, tc := range []struct {
		name, start, delta string
		input, read, write int
	}{
		{"inclusive", `{"input_tokens":165182,"cache_read_input_tokens":164608}`, `{"output_tokens":10}`, 574, 164608, 0},
		{"late_cache", `{"input_tokens":100}`, `{"cache_read_input_tokens":100,"output_tokens":10}`, 0, 100, 0},
		{"split_writes", `{"input_tokens":100,"cache_read_input_tokens":60,"cache_creation":{"ephemeral_5m_input_tokens":10,"ephemeral_1h_input_tokens":20}}`, `{"input_tokens":0,"cache_read_input_tokens":0,"output_tokens":10}`, 10, 60, 30},
		{"inconsistent", `{"input_tokens":10,"cache_read_input_tokens":100}`, `{"output_tokens":10}`, 0, 100, 0},
		{"explicit_zero", `{"input_tokens":0,"cache_read_input_tokens":0}`, `{"output_tokens":10}`, 0, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := "event: message_start\r\nid: preserved\r\ndata: {\r\ndata: \"type\":\"message_start\",\"message\":{\"usage\":" + tc.start + "}}\r\n\r\n" +
				"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":" + tc.delta + "}\n\n" +
				"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
			b := &anthropicCacheBody{ReadCloser: io.NopCloser(strings.NewReader(raw)), reader: bufio.NewReaderSize(strings.NewReader(raw), 16), stream: true}
			out, err := io.ReadAll(b)
			if err != nil {
				t.Fatal(err)
			}
			if b.inputTokens() != tc.input || b.usage.CacheReadInputTokens != tc.read || b.usage.CacheCreationInputTokens != tc.write {
				t.Fatalf("snapshot=%+v input=%d", b.usage, b.inputTokens())
			}
			if !bytes.Contains(out, []byte("id: preserved")) {
				t.Fatal("SSE id lost")
			}
			frames := bytes.Split(out, []byte("\n\n"))
			found := false
			for _, frame := range frames {
				_, data := parseSSEEventChunk(frame)
				if gjson.GetBytes(data, "type").String() == "message_delta" {
					found = true
					if got := gjson.GetBytes(data, "usage.input_tokens"); !got.Exists() || int(got.Int()) != tc.input {
						t.Fatalf("final input=%s out=%s", got.Raw, out)
					}
				}
			}
			if !found {
				t.Fatalf("missing delta: %s", out)
			}
		})
	}
}

func TestAnthropicInclusiveCacheOptInIsolation(t *testing.T) {
	for _, tc := range []struct {
		enabled  bool
		upstream protocol.Protocol
		base     string
		want     int
	}{
		{true, protocol.Anthropic, "https://a", 10}, {false, protocol.Anthropic, "https://a", 100},
		{true, protocol.OpenAI, "https://a", 100}, {true, protocol.Anthropic, "https://b", 100},
	} {
		cfg := &model.Config{URLs: model.ChannelURLs{{URL: "https://a", AnthropicInputTokensIncludeCache: tc.enabled}, {URL: "https://b"}}}
		resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}, "Content-Length": {"100"}}, Body: io.NopCloser(strings.NewReader(`{"type":"message","usage":{"input_tokens":100,"cache_read_input_tokens":90}}`))}
		normalizeAnthropicCacheResponse(resp, cfg, tc.base, tc.upstream)
		out, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		if got := gjson.GetBytes(out, "usage.input_tokens").Int(); got != int64(tc.want) {
			t.Fatalf("%+v got=%d", tc, got)
		}
	}
}

func TestAnthropicInclusiveCacheDetectsMislabeledBody(t *testing.T) {
	for _, tc := range []struct {
		raw          string
		headerStream bool
	}{
		{`{"type":"message","usage":{"input_tokens":100,"cache_read_input_tokens":90}}`, true},
		{"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"input_tokens\":100,\"cache_read_input_tokens\":90}}\n\n", false},
	} {
		b := &anthropicCacheBody{ReadCloser: io.NopCloser(strings.NewReader(tc.raw)), reader: bufio.NewReader(strings.NewReader(tc.raw)), stream: tc.headerStream}
		out, err := io.ReadAll(b)
		if err != nil || !bytes.Contains(out, []byte(`"input_tokens":10`)) {
			t.Fatalf("normalization missed mislabeled body: %s %v", out, err)
		}
	}
}

func TestAnthropicInclusiveCacheObservesRawBytesAndCloseUnblocks(t *testing.T) {
	input, writer := io.Pipe()
	defer func() { _ = writer.Close() }()
	resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: input}
	cfg := &model.Config{URLs: model.ChannelURLs{{URL: "https://inclusive", AnthropicInputTokensIncludeCache: true}}}
	normalizeAnthropicCacheResponse(resp, cfg, "https://inclusive", protocol.Anthropic)
	req := newRequestContextForStreaming(context.Background(), false, protocolTimeoutConfig{})
	defer req.cancel()
	observed := make(chan int64, 1)
	attachFirstByteDetector(req, resp, &streamReadStats{}, &ForwardObserver{OnBytesRead: func(n int64) {
		select {
		case observed <- n:
		default:
		}
	}})
	finished := make(chan error, 1)
	go func() { _, err := io.ReadAll(resp.Body); finished <- err }()
	if _, err := io.WriteString(writer, `{"type":"message",`); err != nil {
		t.Fatal(err)
	}
	select {
	case <-observed:
	case <-time.After(time.Second):
		t.Fatal("raw byte observation blocked until whole JSON")
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-finished:
		if err == nil {
			t.Fatal("interrupted JSON accepted")
		}
	case <-time.After(time.Second):
		t.Fatal("close did not interrupt normalization")
	}
}

func TestProxyAnthropicInclusiveCacheNativeAndConverted(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		for _, client := range []string{"anthropic", "openai", "codex"} {
			t.Run(fmt.Sprintf("%s/stream=%v", client, streaming), func(t *testing.T) {
				env := setupProxyTestEnv(t, []testChannel{{name: "inclusive", upstreamProtocol: "anthropic", models: "cache-model", apiKey: "test"}}, map[int]string{0: "https://inclusive.example"})
				configs, err := env.store.ListConfigs(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				configs[0].URLs[0].AnthropicInputTokensIncludeCache = true
				if _, err = env.store.UpdateConfig(context.Background(), configs[0].ID, configs[0]); err != nil {
					t.Fatal(err)
				}
				env.server.invalidateChannelRelatedCache(configs[0].ID)
				env.server.client = &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
					body := `{"id":"msg_cache","type":"message","role":"assistant","model":"cache-model","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":100,"cache_read_input_tokens":100,"output_tokens":1}}`
					ct := "application/json"
					if streaming {
						ct = "text/event-stream"
						body = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_cache\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"cache-model\",\"content\":[],\"usage\":{\"input_tokens\":100}}}\n\n" +
							"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
							"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\n" +
							"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
							"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"cache_read_input_tokens\":100,\"output_tokens\":1}}\n\n" +
							"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
					}
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {ct}}, Body: io.NopCloser(strings.NewReader(body))}, nil
				})}
				path := "/v1/messages"
				request := map[string]any{"model": "cache-model", "stream": streaming, "max_tokens": 100, "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
				if client == "openai" {
					path = "/v1/chat/completions"
					request["stream_options"] = map[string]any{"include_usage": true}
				}
				if client == "codex" {
					path = "/v1/responses"
					delete(request, "messages")
					delete(request, "max_tokens")
					request["input"] = "hi"
				}
				got := doProxyRequest(t, env.engine, path, request, nil)
				if got.Code != 200 {
					t.Fatalf("status=%d %s", got.Code, got.Body.String())
				}
				var usage gjson.Result
				inspect := func(data []byte) {
					r := gjson.ParseBytes(data)
					if r.Get("type").String() == "response.completed" {
						r = r.Get("response")
					}
					if u := r.Get("usage"); u.IsObject() {
						usage = u
					}
				}
				if streaming {
					for _, frame := range bytes.Split(got.Body.Bytes(), []byte("\n\n")) {
						_, data := parseSSEEventChunk(frame)
						inspect(data)
					}
				} else {
					inspect(got.Body.Bytes())
				}
				field, want := "input_tokens", int64(0)
				if client == "openai" {
					field, want = "prompt_tokens", 100
				}
				if client == "codex" {
					want = 100
				}
				if !usage.Get(field).Exists() || usage.Get(field).Int() != want {
					t.Fatalf("usage=%s body=%s", usage.Raw, got.Body.String())
				}
				deadline := time.Now().Add(2 * time.Second)
				for {
					logs, err := env.store.ListLogs(context.Background(), time.Now().Add(-time.Minute), 20, 0, &model.LogFilter{LogSource: model.LogSourceProxy})
					if err != nil {
						t.Fatal(err)
					}
					if len(logs) > 0 {
						if logs[0].InputTokens != 0 || logs[0].CacheReadInputTokens != 100 {
							t.Fatalf("log=%+v", logs[0])
						}
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("missing usage log")
					}
					time.Sleep(10 * time.Millisecond)
				}
			})
		}
	}
}
