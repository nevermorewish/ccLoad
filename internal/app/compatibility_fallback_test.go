package app

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/tidwall/gjson"

	"ccLoad/internal/model"
	"ccLoad/internal/protocol"
)

func TestCompatibilityMultiURLPreservesUpstreamError(t *testing.T) {
	env := setupProxyTestEnv(t, []testChannel{{name: "mixed", upstreamProtocol: "codex", models: "compat-model", apiKey: "a"}}, map[int]string{0: "https://native.example"})
	cfg, err := env.store.GetConfig(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	cfg.URLs = model.ChannelURLs{
		{URL: "https://native.example", Protocols: []string{"codex"}},
		{URL: "https://chat.example", Protocols: []string{"openai"}},
	}
	if _, err = env.store.UpdateConfig(context.Background(), cfg.ID, cfg); err != nil {
		t.Fatal(err)
	}
	env.server.InvalidateChannelListCache()
	env.server.urlSelector = nil
	calls := 0
	rejection := `{"error":{"type":"invalid_request_error","code":"unsupported_parameter","param":"top_logprobs","message":"logprobs are not supported with reasoning models"}}`
	env.server.client = &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Host != "native.example" {
			t.Errorf("unexpected upstream: %s", r.URL)
		}
		return &http.Response{StatusCode: 400, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(rejection))}, nil
	})}
	got := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{"model": "compat-model", "input": "hi", "top_logprobs": 2}, nil)
	if got.Code != 400 || got.Body.String() != rejection {
		t.Fatalf("status=%d calls=%d body=%s", got.Code, calls, got.Body.String())
	}
}

func TestCompatibilitySamplingFallback(t *testing.T) {
	for _, field := range []string{"temperature", "top_p"} {
		for _, value := range []float64{0, 0.5} {
			for _, withNative := range []bool{false, true} {
				name := "chat_only"
				channels := []testChannel{{name: "chat", upstreamProtocol: "openai", models: "compat-model", apiKey: "b"}}
				urls := map[int]string{0: "https://chat.example"}
				if withNative {
					name = "native_reject_then_chat"
					channels = append([]testChannel{{name: "native", upstreamProtocol: "codex", models: "compat-model", apiKey: "a"}}, channels...)
					urls = map[int]string{0: "https://native.example", 1: "https://chat.example"}
				}
				t.Run(fmt.Sprintf("%s/%s/%g", name, field, value), func(t *testing.T) {
					env := setupProxyTestEnv(t, channels, urls)
					nativeCalls, chatCalls := 0, 0
					env.server.client = &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
						status := 200
						body := `{"id":"chat_1","object":"chat.completion","model":"compat-model","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":1}}`
						if r.URL.Host == "native.example" {
							nativeCalls++
							status = 400
							body = fmt.Sprintf(`{"error":{"type":"invalid_request_error","code":"unsupported_parameter","param":%q,"message":"Unsupported parameter"}}`, field)
						} else {
							chatCalls++
							data, _ := io.ReadAll(r.Body)
							if v := gjson.GetBytes(data, field); !v.Exists() || v.Float() != value {
								t.Errorf("%s lost: %s", field, data)
							}
						}
						return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
					})}
					got := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{"model": "compat-model", "input": "hi", field: value}, nil)
					if got.Code != 200 || chatCalls != 1 {
						t.Fatalf("status=%d nativeCalls=%d chatCalls=%d body=%s", got.Code, nativeCalls, chatCalls, got.Body.String())
					}
				})
			}
		}
	}
}

func TestCompatibilitySamplingFallbackExcludesLossyConverters(t *testing.T) {
	candidates := []protocol.Protocol{protocol.Codex, protocol.OpenAI, protocol.Anthropic, protocol.Gemini}
	for _, client := range []protocol.Protocol{protocol.Codex, protocol.OpenAI} {
		got := samplingProtocolCandidates(candidates, client, &model.Config{})
		if len(got) != 2 || got[0] != protocol.Codex || got[1] != protocol.OpenAI {
			t.Fatalf("%s: unexpected candidates %v", client, got)
		}
		got = samplingProtocolCandidates(candidates, client, &model.Config{AuthType: model.AuthTypeCodexOAuth})
		if len(got) != 1 || got[0] != protocol.OpenAI {
			t.Fatalf("%s: OAuth normalization can drop sampling controls: %v", client, got)
		}
	}
}
