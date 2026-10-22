package app

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"ccLoad/internal/model"
	"ccLoad/internal/protocol"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const responsesMissingMessagesError = "{\"error\":{\"message\":\"The request failed because it is missing `messages` parameter. Request id: test\",\"type\":\"BadRequest\",\"param\":\"\",\"code\":\"MissingParameter\"}}"

func TestProxy_AutomaticProtocolFallback_ResponsesMissingMessages(t *testing.T) {
	t.Parallel()
	var paths []string
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/responses" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, responsesMissingMessagesError)
			return
		}
		body, _ := io.ReadAll(r.Body)
		assertChatRequestUserText(t, body, "hi")
		_, _ = io.WriteString(w, `{"id":"chatcmpl_1","object":"chat.completion","model":"deepseek-v4-flash","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{{
		name: "responses-missing-messages", protocolTransformMode: model.ProtocolTransformModeAuto, models: "deepseek-v4-flash",
	}}, map[int]string{0: upstream.URL})

	request := func() {
		t.Helper()
		w := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
			"model": "deepseek-v4-flash",
			"input": []map[string]any{{
				"type": "message", "role": "user",
				"content": []map[string]string{{"type": "input_text", "text": "hi"}},
			}},
		}, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
		if got := gjson.GetBytes(w.Body.Bytes(), "output.0.content.0.text").String(); got != "ok" {
			t.Fatalf("translated Codex response text=%q body=%s", got, w.Body.String())
		}
	}

	request()
	request()

	if got := strings.Join(paths, ","); got != "/v1/responses,/v1/chat/completions,/v1/chat/completions" {
		t.Fatalf("upstream paths=%s, want client Codex then cached OpenAI", got)
	}
}

func TestHandleChannelTest_AutoFallsBackOnResponsesMissingMessages(t *testing.T) {
	t.Parallel()
	var gotPaths []string

	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPaths = append(gotPaths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/responses" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, responsesMissingMessagesError)
			return
		}
		body, _ := io.ReadAll(r.Body)
		assertChatRequestUserText(t, body, "hello")
		_, _ = io.WriteString(w, `{"id":"chatcmpl_1","object":"chat.completion","model":"deepseek-v4-flash","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer upstream.Close()

	srv := newInMemoryServer(t)
	srv.client = upstream.Client()
	ctx := context.Background()
	created, err := srv.store.CreateConfig(ctx, &model.Config{
		Name:                  "auto-responses-missing-messages",
		URLs:                  model.ChannelURLs{{URL: upstream.URL}},
		Priority:              1,
		ProtocolTransformMode: model.ProtocolTransformModeAuto,
		ModelEntries:          []model.ModelEntry{{Model: "deepseek-v4-flash"}},
		Enabled:               true,
	})
	if err != nil {
		t.Fatalf("CreateConfig failed: %v", err)
	}
	if err := srv.store.CreateAPIKeysBatch(ctx, []*model.APIKey{{ChannelID: created.ID, KeyIndex: 0, APIKey: "sk-test-key"}}); err != nil {
		t.Fatalf("CreateAPIKeysBatch failed: %v", err)
	}

	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, fmt.Sprintf("/admin/channels/%d/test", created.ID), map[string]any{
		"model":           "deepseek-v4-flash",
		"client_protocol": "codex",
		"content":         "hello",
		"stream":          true,
	}))
	c.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", created.ID)}}

	srv.HandleChannelTest(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	if !reflect.DeepEqual(gotPaths, []string{"/v1/responses", "/v1/chat/completions"}) {
		t.Fatalf("paths=%v, want client Codex then OpenAI", gotPaths)
	}

	resp := mustParseAPIResponse[map[string]any](t, w.Body.Bytes())
	if success, _ := resp.Data["success"].(bool); !success {
		t.Fatalf("expected data.success=true, data=%+v", resp.Data)
	}
	if got, _ := resp.Data["upstream_protocol"].(string); got != "openai" {
		t.Fatalf("upstream_protocol=%q, want openai, data=%+v", got, resp.Data)
	}
	if _, exists := resp.Data["cooldown_action"]; exists {
		t.Fatalf("capability fallback must not apply cooldown, data=%+v", resp.Data)
	}
}

func TestProxy_ResponsesMissingMessages_AnthropicStreamAndUpstreamMode(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{model.ProtocolTransformModeAuto, model.ProtocolTransformModeUpstream} {
		t.Run(mode, func(t *testing.T) {
			var paths []string
			upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				paths = append(paths, r.URL.Path)
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/v1/responses":
					w.WriteHeader(http.StatusBadRequest)
					_, _ = io.WriteString(w, responsesMissingMessagesError)
					return
				case "/v1/chat/completions":
					w.WriteHeader(http.StatusNotFound)
					_, _ = io.WriteString(w, `{"error":{"message":"endpoint not found"}}`)
					return
				case "/v1/messages":
					body, _ := io.ReadAll(r.Body)
					assertChatRequestUserText(t, body, "hi")
					if !gjson.GetBytes(body, "stream").Bool() || gjson.GetBytes(body, "input").Exists() {
						t.Errorf("expected streaming Anthropic request, got %s", body)
					}
					w.Header().Set("Content-Type", "text/event-stream")
					for _, event := range []string{
						`{"type":"message_start","message":{"id":"msg_test","type":"message","role":"assistant","content":[],"model":"deepseek-v4.1-flash","usage":{"input_tokens":3,"output_tokens":0}}}`,
						`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
						`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`,
						`{"type":"content_block_stop","index":0}`,
						`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`,
						`{"type":"message_stop"}`,
					} {
						_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", gjson.Get(event, "type").String(), event)
					}
				default:
					t.Errorf("unexpected upstream path %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer upstream.Close()
			env := setupProxyTestEnv(t, []testChannel{{
				name: "responses-missing-messages-stream", upstreamProtocol: "anthropic",
				protocolTransformMode: mode, models: "deepseek-v4.1-flash",
			}}, map[int]string{0: upstream.URL})
			w := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
				"model": "deepseek-v4.1-flash", "input": "hi", "stream": true,
			}, nil)
			if mode == model.ProtocolTransformModeUpstream {
				if w.Code != http.StatusBadRequest || strings.Join(paths, ",") != "/v1/responses" {
					t.Fatalf("upstream-only mode must preserve error: status=%d paths=%v body=%s", w.Code, paths, w.Body.String())
				}
				return
			}
			if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "response.completed") ||
				!strings.Contains(w.Body.String(), `"text":"ok"`) {
				t.Fatalf("expected completed Responses stream: status=%d body=%s", w.Code, w.Body.String())
			}
			if got := strings.Join(paths, ","); got != "/v1/responses,/v1/chat/completions,/v1/messages" {
				t.Fatalf("unexpected protocol fallback paths: %s", got)
			}
		})
	}
}

func TestProtocolMissingMessagesDoesNotRetryCommittedResponse(t *testing.T) {
	res := &fwResult{Status: http.StatusBadRequest, Body: []byte(responsesMissingMessagesError), ResponseCommitted: true}
	if isProtocolEndpointMissing(res, protocol.Codex) {
		t.Fatal("must not retry a response already committed to the client")
	}
}
