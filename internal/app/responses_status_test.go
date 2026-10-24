package app

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/protocol"

	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

func TestResponsesStatusRetryPreservesOtherItemTypes(t *testing.T) {
	t.Parallel()
	body := []byte(`{"seed":9007199254740993,"input":[{"type":"tool_search_call","status":"completed","call_id":"search_1","arguments":{}},{"type":"function_call","status":"completed","call_id":"call_1","arguments":"{}"},{"type":"tool_search_output","status":"completed","call_id":"search_1","tools":[]},{"type":"message","status":"completed","role":"assistant","content":[]},{"type":"function_call_output","call_id":"call_1","status":"completed","output":"ok"}],"metadata":{"status":"keep"}}`)
	errBody := []byte(`{"error":{"code":"unknown_parameter","param":"input[1].status"}}`)
	plan := protocol.TransformPlan{
		ClientProtocol: protocol.Codex, UpstreamProtocol: protocol.Codex,
		RequestFamily: protocol.RequestFamilyResponses, TranslatedBody: body,
	}
	for _, res := range []*fwResult{
		{Status: http.StatusBadRequest, Body: errBody},
		{Status: http.StatusOK, SSEErrorEvent: []byte(`{"type":"error","status":400,"error":{"code":"unknown_parameter","param":"input[1].status"}}`)},
	} {
		got, _, ok := responsesRetryBodyForUnknownParameter(protocol.Codex, plan, res)
		want := bytes.Replace(body, []byte(`"type":"function_call","status":"completed",`), []byte(`"type":"function_call",`), 1)
		if !ok || !bytes.Equal(got, want) {
			t.Fatalf("retry changed other item types or serialized bytes: ok=%v body=%s", ok, got)
		}
	}
	// The rejected index belongs to the wire body; an incremental WS transcript
	// can have different indexes. Strip by the resolved type, never by that index.
	incremental := []byte(`{"input":[{"type":"function_call","status":"completed","call_id":"call_2"},{"type":"tool_search_call","status":"completed"}]}`)
	itemType := rejectedResponsesStatusItemType(body, errBody)
	got := stripResponsesInputItemStatus(incremental, itemType)
	if gjson.GetBytes(got, "input.0.status").Exists() || gjson.GetBytes(got, "input.1.status").String() != "completed" {
		t.Fatalf("incremental retry used the replay index: %s", got)
	}
}

func TestResponsesMissingStatusNeverDropsHistory(t *testing.T) {
	t.Parallel()
	body := []byte(`{"input":[{"type":"tool_search_call","call_id":"search_1","arguments":{}},{"type":"tool_search_output","call_id":"search_1","tools":[]}]}`)
	for _, errBody := range []string{
		`{"error":{"code":"missing_required_parameter","param":"input[0].status"}}`,
		`{"error":{"code":"missing_required_parameter","message":"Missing required parameter: 'input[0].status'."}}`,
		`{"code":"missing_required_parameter","param":"input[0].status"}`,
	} {
		res := &fwResult{Status: http.StatusBadRequest, Body: []byte(errBody)}
		if got, strategy, ok := retryBodyForRejectedRequest(protocol.Codex, nil, nil, protocol.TransformPlan{TranslatedBody: body}, res); ok {
			t.Fatalf("missing status must not drop a history item: strategy=%s body=%s", strategy, got)
		}
	}
}

func TestResponsesUnknownStatusRetryRequiresExistingTypedItem(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ body, param string }{
		{`{"input":[{"type":"function_call","status":"completed"}]}`, "input[150].status"},
		{`{"input":[{"status":"completed"}]}`, "input[0].status"},
		{`{"input":[{"type":"function_call"},{"type":"function_call","status":"completed"}]}`, "input[0].status"},
	} {
		plan := protocol.TransformPlan{ClientProtocol: protocol.Codex, RequestFamily: protocol.RequestFamilyResponses, TranslatedBody: []byte(tc.body)}
		res := &fwResult{Status: http.StatusBadRequest, Body: []byte(fmt.Sprintf(`{"error":{"code":"unknown_parameter","param":%q}}`, tc.param))}
		if _, _, ok := responsesRetryBodyForUnknownParameter(protocol.Codex, plan, res); ok {
			t.Fatalf("invalid rejected item must not trigger stripping: %s %s", tc.body, tc.param)
		}
	}
}

func TestProxyResponsesStatusCompatibility(t *testing.T) {
	for _, mode := range []string{"required", "unknown_function_status", "missing_status"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempt := calls.Add(1)
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				if gjson.GetBytes(body, "input.#").Int() != 4 {
					t.Errorf("history item removed: %s", body)
				}
				for _, index := range []int{0, 1} {
					if gjson.GetBytes(body, fmt.Sprintf("input.%d.status", index)).String() != "completed" {
						t.Errorf("required search status stripped: %s", body)
					}
				}
				w.Header().Set("Content-Type", "application/json")
				if mode == "missing_status" {
					w.WriteHeader(http.StatusBadRequest)
					_, _ = io.WriteString(w, `{"error":{"code":"missing_required_parameter","param":"input[2].status","message":"Missing required parameter: 'input[2].status'."}}`)
					return
				}
				if attempt == 1 && !gjson.GetBytes(body, "input.2.status").Exists() {
					t.Errorf("first send stripped function status: %s", body)
				}
				if mode == "unknown_function_status" && attempt == 1 {
					w.WriteHeader(http.StatusBadRequest)
					_, _ = io.WriteString(w, `{"error":{"code":"unknown_parameter","param":"input[2].status","message":"Unknown parameter: 'input[2].status'."}}`)
					return
				}
				if attempt > 1 && gjson.GetBytes(body, "input.2.status").Exists() {
					t.Errorf("rejected status survived retry: %s", body)
				}
				_, _ = io.WriteString(w, `{"id":"resp_status","object":"response","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)
			}))
			defer upstream.Close()
			env := setupProxyTestEnv(t, []testChannel{{name: "status-test", upstreamProtocol: "codex", models: "gpt-test", priority: 100}}, map[int]string{0: upstream.URL})
			env.server.client = upstream.Client()
			function := map[string]any{"type": "function_call", "call_id": "call_1", "name": "lookup", "arguments": "{}"}
			if mode != "missing_status" {
				function["status"] = "completed"
			}
			response := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
				"model": "gpt-test", "input": []any{
					map[string]any{"type": "tool_search_call", "call_id": "search_1", "execution": "client", "arguments": map[string]any{}, "status": "completed"},
					map[string]any{"type": "tool_search_output", "call_id": "search_1", "tools": []any{}, "status": "completed"},
					function,
					map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "ok"},
				},
			}, nil)
			wantStatus, wantCalls := http.StatusOK, int32(1)
			switch mode {
			case "missing_status":
				wantStatus = http.StatusBadRequest
			case "unknown_function_status":
				wantCalls = 2
			}
			if response.Code != wantStatus || calls.Load() != wantCalls {
				t.Fatalf("status=%d calls=%d, want %d/%d; body=%s", response.Code, calls.Load(), wantStatus, wantCalls, response.Body.String())
			}
		})
	}
}

func TestNativeCodexWebsocketStatusRetryPreservesRequiredStatus(t *testing.T) {
	var calls atomic.Int32
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = conn.Close() }()
		_, body, err := conn.ReadMessage()
		if err != nil {
			t.Error(err)
			return
		}
		attempt := calls.Add(1)
		if gjson.GetBytes(body, "input.#").Int() != 3 || gjson.GetBytes(body, "input.0.status").String() != "completed" {
			t.Errorf("WebSocket send dropped history or required status: %s", body)
		}
		if attempt == 1 {
			if gjson.GetBytes(body, "input.1.status").String() != "completed" {
				t.Errorf("first WebSocket send stripped status: %s", body)
			}
			_ = conn.WriteJSON(map[string]any{
				"type": "error", "status": http.StatusBadRequest,
				"error": map[string]any{"code": "unknown_parameter", "param": "input[1].status", "message": "Unknown parameter: 'input[1].status'."},
			})
			return
		}
		if gjson.GetBytes(body, "input.1.status").Exists() {
			t.Errorf("WebSocket retry retained rejected status: %s", body)
		}
		_ = conn.WriteJSON(map[string]any{
			"type":     "response.completed",
			"response": map[string]any{"id": "resp_status_retry", "output": []any{}, "usage": map[string]any{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2}},
		})
	}))
	defer upstream.Close()
	env := setupProxyTestEnv(t, []testChannel{{
		name: "status-retry", upstreamProtocol: "codex", websockets: true,
		models: "gpt-test", priority: 100, authType: model.AuthTypeCodexOAuth,
		oauthCredential: codexProxyTestCredential(t, "at-status", "rt-status", "account-status"),
	}}, map[int]string{0: upstream.URL})
	downstream := dialResponsesWebsocketWithSessionID(t, env.engine, "status-retry")
	if err := downstream.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := downstream.WriteJSON(map[string]any{
		"type": "response.create", "model": "gpt-test", "store": false,
		"input": []any{
			map[string]any{"type": "message", "role": "assistant", "status": "completed", "content": "continue"},
			map[string]any{"type": "function_call", "call_id": "call_1", "name": "lookup", "arguments": "{}", "status": "completed"},
			map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "ok"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	readWebsocketUntilType(t, downstream, "response.completed")
	if calls.Load() != 2 {
		t.Fatalf("WebSocket requests=%d, want exactly one compatibility retry", calls.Load())
	}
}
