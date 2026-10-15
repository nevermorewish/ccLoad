package app

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"

	"ccLoad/internal/model"
	"ccLoad/internal/protocol"
	"github.com/tidwall/gjson"
)

func TestProxyCompatibilityRejectionCannotBecomeConvertedSuccess(t *testing.T) {
	for _, tc := range []struct {
		name      string
		request   map[string]any
		rejection string
	}{
		{"logprobs", map[string]any{"input": "hi", "top_logprobs": 2}, `{"error":{"type":"invalid_request_error","code":"unsupported_parameter","param":"top_logprobs","message":"logprobs are not supported with reasoning models"}}`},
		{"reasoning", map[string]any{"input": "hi", "reasoning": map[string]any{"effort": "minimal"}}, `{"error":{"type":"invalid_request_error","code":"unsupported_value","param":"reasoning.effort","message":"minimal is not supported"}}`},
		{"configuration", map[string]any{"input": []any{map[string]any{"type": "configuration_update", "reasoning": map[string]any{"mode": "pro"}}}}, `{"error":{"type":"invalid_request_error","code":"invalid_configuration","param":"input[0]","message":"configuration_update is not available for this model"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := setupProxyTestEnv(t, []testChannel{{name: "native", upstreamProtocol: "codex", models: "compat-model", apiKey: "a"}, {name: "chat", upstreamProtocol: "openai", models: "compat-model", apiKey: "b"}}, map[int]string{0: "https://native.example", 1: "https://chat.example"})
			calls := 0
			env.server.client = &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.Host != "native.example" {
					t.Errorf("rejection bypassed via %s", r.URL)
				}
				return &http.Response{StatusCode: 400, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(tc.rejection))}, nil
			})}
			tc.request["model"] = "compat-model"
			got := doProxyRequest(t, env.engine, "/v1/responses", tc.request, nil)
			if got.Code != 400 || calls != 1 || !bytes.Equal(got.Body.Bytes(), []byte(tc.rejection)) {
				t.Fatalf("status=%d calls=%d error=%s", got.Code, calls, got.Body.String())
			}
		})
	}
}

func TestProxyCompatibilityLocalErrorIdentifiesParameter(t *testing.T) {
	env := setupProxyTestEnv(t, []testChannel{{name: "chat", upstreamProtocol: "openai", models: "compat-model", apiKey: "b"}}, map[int]string{0: "https://chat.example"})
	env.server.client = &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("unsupported conversion reached upstream")
		return nil, nil
	})}
	got := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{"model": "compat-model", "input": "hi", "reasoning": map[string]any{"mode": "pro"}}, nil)
	if got.Code != 400 || gjson.GetBytes(got.Body.Bytes(), "error.param").String() != "reasoning.mode" || gjson.GetBytes(got.Body.Bytes(), "error.code").String() != "unsupported_parameter" {
		t.Fatalf("%d %s", got.Code, got.Body.String())
	}
}

func TestCompatibilityConfigurationUpdateCannotBeDroppedOnRetry(t *testing.T) {
	plan := protocol.TransformPlan{ClientProtocol: protocol.Codex, UpstreamProtocol: protocol.Codex, TranslatedBody: []byte(`{"input":[{"type":"configuration_update"},{"role":"user","content":"hi"}]}`)}
	res := &fwResult{Status: 400, Body: []byte(`{"error":{"code":"missing_required_parameter","param":"input[0].reasoning","message":"missing required parameter"}}`)}
	if _, _, ok := retryBodyForRejectedRequest(protocol.Codex, &model.Config{}, nil, plan, res); ok {
		t.Fatal("configuration update dropped")
	}
}

func TestCompatibilityLocalErrorEnvelope(t *testing.T) {
	for _, raw := range []string{"conversion failed", `{"error":"conversion failed"}`} {
		out := localFailureJSONBody(400, []byte(raw))
		if gjson.GetBytes(out, "error.message").String() != "conversion failed" || gjson.GetBytes(out, "error.code").String() == "" {
			t.Fatalf("bad error envelope: %s", out)
		}
	}
	original := []byte(`{"error":{"type":"invalid_request_error","message":"bad mode","code":"invalid_value","param":"reasoning.mode"}}`)
	if !bytes.Equal(localFailureJSONBody(400, original), original) {
		t.Fatal("upstream error rewritten")
	}
}
