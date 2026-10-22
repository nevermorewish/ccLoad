package app

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"ccLoad/internal/model"
	"ccLoad/internal/protocol"

	"github.com/tidwall/gjson"
)

const unsupportedUsageSemanticsError = `{"error":{"message":"unrecognized request argument supplied: usage_semantics (request id: 202610210242233163612828268d9d65WBpjuBj)","type":"new_api_error","param":"","code":"invalid_request"}}`

func TestUsageSemanticsRetryPreservesRequest(t *testing.T) {
	for _, value := range []string{`"normalized"`, `{"mode":"normalized"}`, `false`, `0`, `""`, `null`} {
		t.Run(value, func(t *testing.T) {
			body := []byte(`{"model":"gpt-6.1-sol","usage_semantics":` + value + `,"temperature":0,"parallel_tool_calls":false,"messages":[{"role":"user","content":"usage_semantics"}],"tools":[{"type":"function","function":{"name":"test","parameters":{"type":"object","properties":{"usage_semantics":{"type":"string"}}}}}]}`)
			original := bytes.Clone(body)
			plan := protocol.TransformPlan{TranslatedBody: body, RequestFamily: protocol.RequestFamilyChatCompletions}
			res := &fwResult{Status: http.StatusBadRequest, Body: []byte(unsupportedUsageSemanticsError)}
			retryBody, strategy, ok := retryBodyForRejectedRequest(protocol.OpenAI, nil, nil, plan, res)
			if !ok || strategy != stripUnsupportedUsageSemanticsStrategy {
				t.Fatalf("retry=%v strategy=%q", ok, strategy)
			}
			if !bytes.Equal(body, original) {
				t.Fatal("retry modified the original body")
			}
			if gjson.GetBytes(retryBody, "usage_semantics").Exists() {
				t.Fatalf("retry retained root field: %s", retryBody)
			}
			for _, field := range []string{"model", "temperature", "parallel_tool_calls", "messages", "tools"} {
				if gjson.GetBytes(retryBody, field).Raw != gjson.GetBytes(original, field).Raw {
					t.Errorf("retry changed %s: %s", field, retryBody)
				}
			}
			plan.TranslatedBody = retryBody
			if _, _, ok := usageSemanticsRetryBodyFor400(protocol.OpenAI, plan, res); ok {
				t.Fatal("must not retry again when the field is already absent")
			}
		})
	}
}

func TestUsageSemanticsRejectionMatching(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want bool
	}{
		{"observed error", unsupportedUsageSemanticsError, true},
		{"unknown param", `{"error":{"code":"unknown_parameter","param":"usage_semantics"}}`, true},
		{"unsupported param", `{"error":{"code":"unsupported_parameter","param":"usage_semantics"}}`, true},
		{"quoted unknown", `{"error":{"message":"Unknown parameter: 'usage_semantics'.","code":"invalid_request_error"}}`, true},
		{"quoted unsupported", "{\"error\":{\"message\":\"Unsupported parameter: `usage_semantics`\"}}", true},
		{"unquoted unknown", `{"error":{"message":"Unknown parameter: usage_semantics"}}`, true},
		{"double quotes", `{"error":{"message":"Unsupported parameter: \"usage_semantics\""}}`, true},
		{"wrong param", `{"error":{"message":"Unknown parameter: usage_semantics","param":"temperature"}}`, false},
		{"nested param", `{"error":{"code":"unknown_parameter","param":"metadata.usage_semantics"}}`, false},
		{"nested message", `{"error":{"message":"Unknown parameter: metadata.usage_semantics"}}`, false},
		{"similar field", `{"error":{"message":"Unknown parameter: usage_semantics_extra"}}`, false},
		{"nested suffix", `{"error":{"message":"Unknown parameter: usage_semantics.mode"}}`, false},
		{"value rejected", `{"error":{"code":"invalid_request","param":"usage_semantics","message":"Invalid value for usage_semantics"}}`, false},
		{"known field unsupported value", `{"error":{"message":"Unsupported parameter: usage_semantics must be normalized"}}`, false},
		{"unrelated code", `{"error":{"code":"invalid_api_key","message":"Unknown parameter: usage_semantics"}}`, false},
		{"unrelated message", `{"error":{"message":"Missing messages; usage_semantics is present"}}`, false},
		{"invalid param type", `{"error":{"code":"unknown_parameter","param":["usage_semantics"]}}`, false},
		{"plain text", `unrecognized request argument supplied: usage_semantics`, false},
		{"invalid JSON", `{"error":`, false},
		{"non object error", `{"error":"Unknown parameter: usage_semantics"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := matchesUnsupportedUsageSemanticsError([]byte(tc.body)); got != tc.want {
				t.Fatalf("matches=%v, want %v", got, tc.want)
			}
		})
	}
}

func TestUsageSemanticsRetryGuards(t *testing.T) {
	for _, tc := range []struct {
		name      string
		protocol  protocol.Protocol
		body      string
		status    int
		committed bool
		nilResult bool
	}{
		{"nil result", protocol.OpenAI, `{"usage_semantics":true}`, 400, false, true},
		{"committed", protocol.OpenAI, `{"usage_semantics":true}`, 400, true, false},
		{"server error", protocol.OpenAI, `{"usage_semantics":true}`, 500, false, false},
		{"success", protocol.OpenAI, `{"usage_semantics":true}`, 200, false, false},
		{"anthropic", protocol.Anthropic, `{"usage_semantics":true}`, 400, false, false},
		{"codex", protocol.Codex, `{"usage_semantics":true}`, 400, false, false},
		{"absent field", protocol.OpenAI, `{"model":"test"}`, 400, false, false},
		{"nested only", protocol.OpenAI, `{"metadata":{"usage_semantics":true}}`, 400, false, false},
		{"invalid body", protocol.OpenAI, `{"usage_semantics":true`, 400, false, false},
		{"array body", protocol.OpenAI, `[{"usage_semantics":true}]`, 400, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := &fwResult{Status: tc.status, Body: []byte(unsupportedUsageSemanticsError), ResponseCommitted: tc.committed}
			if tc.nilResult {
				res = nil
			}
			if _, _, ok := usageSemanticsRetryBodyFor400(tc.protocol, protocol.TransformPlan{TranslatedBody: []byte(tc.body)}, res); ok {
				t.Fatal("must preserve this response without retrying")
			}
		})
	}
}

func TestProxy_UnsupportedUsageSemantics(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name         string
		stream       bool
		supported    bool
		alwaysReject bool
		unrelated    bool
		bodyRule     bool
		wantAttempts int
		wantStatus   int
	}{
		{name: "same channel retry succeeds", wantAttempts: 2, wantStatus: 200},
		{name: "stream retry succeeds", stream: true, wantAttempts: 2, wantStatus: 200},
		{name: "continuing rejection stops", alwaysReject: true, wantAttempts: 2, wantStatus: 400},
		{name: "unrelated error preserved", unrelated: true, wantAttempts: 1, wantStatus: 400},
		{name: "supported provider retains field", supported: true, wantAttempts: 1, wantStatus: 200},
		{name: "channel rule not reapplied", bodyRule: true, wantAttempts: 2, wantStatus: 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var bodies [][]byte
			var rules *model.CustomRequestRules
			if tc.bodyRule {
				rules = &model.CustomRequestRules{Body: []model.CustomBodyRule{{
					Action: model.RuleActionOverride, Path: "usage_semantics", Value: json.RawMessage(`"normalized"`),
				}}}
			}
			channels := []testChannel{{
				name: "gpt-yuguang", upstreamProtocol: "anthropic", protocolTransformMode: model.ProtocolTransformModeAuto,
				models: "gpt-6.1-sol", customRequestRules: rules,
			}}
			// Unrelated 400s retain the existing routing policy. The spare
			// channel verifies that a failed compatibility retry stays put.
			if !tc.unrelated {
				channels = append(channels, testChannel{name: "spare", models: "gpt-6.1-sol"})
			}
			env := setupProxyTestEnv(t, channels, map[int]string{0: "https://yuguang.example", 1: "https://spare.example"})
			env.server.client = &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				body, _ := io.ReadAll(r.Body)
				bodies = append(bodies, body)
				if r.URL.Host != "yuguang.example" || r.URL.Path != "/v1/chat/completions" ||
					r.Header.Get("Authorization") != "Bearer sk-test-0" {
					t.Errorf("unexpected retry target/auth: %s, authorization=%q", r.URL, r.Header.Get("Authorization"))
				}
				if len(bodies) == 1 {
					if !gjson.GetBytes(body, "usage_semantics").Exists() {
						t.Errorf("initial request lost usage_semantics: %s", body)
					}
				} else {
					if gjson.GetBytes(body, "usage_semantics").Exists() {
						t.Errorf("retry must remove usage_semantics: %s", body)
					}
					for _, field := range []string{"model", "messages", "temperature", "parallel_tool_calls", "stream"} {
						if gjson.GetBytes(body, field).Raw != gjson.GetBytes(bodies[0], field).Raw {
							t.Errorf("retry changed %s: %s", field, body)
						}
					}
				}
				status, contentType := http.StatusOK, "application/json"
				response := `{"id":"chatcmpl_1","object":"chat.completion","model":"gpt-6.1-sol","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`
				if tc.unrelated {
					status, response = http.StatusBadRequest, `{"error":{"message":"Invalid value for usage_semantics","code":"invalid_request","param":"usage_semantics"}}`
				} else if !tc.supported && (len(bodies) == 1 || tc.alwaysReject) {
					status, response = http.StatusBadRequest, unsupportedUsageSemanticsError
				} else if tc.stream {
					contentType = "text/event-stream"
					response = "data: {\"id\":\"chatcmpl_1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":null}]}\n\ndata: {\"id\":\"chatcmpl_1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
				}
				return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{contentType}}, Body: io.NopCloser(strings.NewReader(response))}, nil
			})}
			request := map[string]any{
				"model": "gpt-6.1-sol", "messages": []map[string]string{{"role": "user", "content": "hi"}},
				"stream": tc.stream, "temperature": 0, "parallel_tool_calls": false,
			}
			if !tc.bodyRule {
				request["usage_semantics"] = "normalized"
			}
			w := doProxyRequest(t, env.engine, "/v1/chat/completions", request, nil)
			if w.Code != tc.wantStatus || len(bodies) != tc.wantAttempts {
				t.Fatalf("status=%d attempts=%d, want status=%d attempts=%d; body=%s", w.Code, len(bodies), tc.wantStatus, tc.wantAttempts, w.Body.String())
			}
			if tc.wantStatus == http.StatusOK {
				if tc.stream {
					if !strings.Contains(w.Body.String(), `"content":"ok"`) || !strings.Contains(w.Body.String(), "[DONE]") {
						t.Fatalf("missing successful stream: %s", w.Body.String())
					}
				} else if gjson.GetBytes(w.Body.Bytes(), "choices.0.message.content").String() != "ok" {
					t.Fatalf("missing successful response: %s", w.Body.String())
				}
			} else if !strings.Contains(w.Body.String(), "usage_semantics") {
				t.Fatalf("upstream error was lost: %s", w.Body.String())
			}
		})
	}
}
