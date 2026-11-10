package app

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/tidwall/gjson"

	"ccLoad/internal/model"
	"ccLoad/internal/protocol"
)

const responsesMissingMessagesError = "{\"error\":{\"message\":\"The request failed because it is missing `messages` parameter. Request id: test\",\"type\":\"BadRequest\",\"param\":\"\",\"code\":\"MissingParameter\"}}"

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
			if w.Code != http.StatusBadRequest || w.Body.String() != responsesMissingMessagesError || strings.Join(paths, ",") != "/v1/responses" {
				t.Fatalf("native error changed or protocol probed: status=%d paths=%v body=%s", w.Code, paths, w.Body.String())
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
