package app

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/protocol"
	"ccLoad/internal/testutil"
)

// nativeGenerationRequest makes each fixture use the protocol it is testing.
func nativeGenerationRequest(p protocol.Protocol, modelName string, stream bool) (string, map[string]any) {
	body := map[string]any{"model": modelName, "stream": stream}
	switch p {
	case protocol.Codex:
		body["input"] = "hello"
		return "/v1/responses", body
	case protocol.Gemini:
		action := "generateContent"
		if stream {
			action = "streamGenerateContent?alt=sse"
		}
		return "/v1beta/models/" + modelName + ":" + action, map[string]any{"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": "hello"}}}}}
	default:
		body["messages"] = []any{map[string]any{"role": "user", "content": "hello"}}
		if p == protocol.Anthropic {
			body["max_tokens"] = 64
			return "/v1/messages", body
		}
		return "/v1/chat/completions", body
	}
}

// Replaces upstream's conversion/probing matrix with the fork's public contract.
// Opting into wire repair must never opt back into protocol conversion.
func TestNativeProtocolAdmissionMatrix(t *testing.T) {
	protocols := []protocol.Protocol{protocol.OpenAI, protocol.Codex, protocol.Anthropic, protocol.Gemini}
	for _, mode := range []string{model.ProtocolTransformModeAuto, model.ProtocolTransformModeLocal} {
		for _, repair := range []bool{false, true} {
			for _, client := range protocols {
				for _, upstream := range protocols {
					if client == upstream {
						continue // Success and wire fidelity are covered by channel_passthrough_test.go.
					}
					for _, stream := range []bool{false, true} {
						t.Run(fmt.Sprintf("%s/repair=%t/%s-to-%s/stream=%t", mode, repair, client, upstream, stream), func(t *testing.T) {
							var rules *model.CustomRequestRules
							if repair {
								rules = compatibilityRules(nil)
							}
							env := setupProxyTestEnv(t, []testChannel{{name: "foreign", upstreamProtocol: string(upstream), models: "contract-model", customRequestRules: rules}}, map[int]string{0: "https://foreign.invalid"})
							cfg, err := env.store.GetConfig(context.Background(), 1)
							if err != nil {
								t.Fatal(err)
							}
							cfg.ProtocolTransformMode = mode
							if _, err := env.store.UpdateConfig(context.Background(), cfg.ID, cfg); err != nil {
								t.Fatal(err)
							}
							env.server.InvalidateChannelListCache()
							env.server.client = &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
								t.Error("incompatible channel reached the network")
								return nil, fmt.Errorf("unexpected network request")
							})}
							path, body := nativeGenerationRequest(client, "contract-model", stream)
							response := doProxyRequest(t, env.engine, path, body, nil)
							if response.Code != http.StatusServiceUnavailable {
								t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
							}
							input := &testutil.TestChannelRequest{Model: "contract-model", Content: "hello", Stream: stream}
							_, _, err = env.server.buildTestUpstreamRequestPlan(context.Background(), cfg, "key", input, input.Model, string(client), string(upstream), cfg.GetURLs()[0])
							if err == nil || !strings.Contains(err.Error(), "protocol conversion is disabled") {
								t.Fatalf("admin accepted conversion: %v", err)
							}
							if until, _ := env.store.GetAllChannelCooldowns(context.Background()); len(until) != 0 {
								t.Fatalf("local protocol refusal cooled channel: %v", until)
							}
						})
					}
				}
			}
		}
	}
}

func TestNativeProtocolBridgeProvidersAreNotAdmitted(t *testing.T) {
	for _, authType := range []string{model.AuthTypeCursorOAuth, model.AuthTypeZedOAuth, model.AuthTypeAntigravityOAuth} {
		for _, p := range []protocol.Protocol{protocol.OpenAI, protocol.Codex, protocol.Anthropic, protocol.Gemini} {
			t.Run(authType+"/"+string(p), func(t *testing.T) {
				env := setupProxyTestEnv(t, []testChannel{{name: "bridge", authType: authType, upstreamProtocol: string(p), models: "contract-model", oauthCredential: `{"access_token":"test-token"}`}}, map[int]string{0: "https://bridge.invalid"})
				env.server.client = &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
					t.Error("bridge request reached HTTP")
					return nil, fmt.Errorf("unexpected bridge request")
				})}
				runner := &fakeCursorRunner{text: "must not run"}
				env.server.cursorRunner = runner
				path, body := nativeGenerationRequest(p, "contract-model", false)
				response := doProxyRequest(t, env.engine, path, body, nil)
				if response.Code != http.StatusServiceUnavailable || runner.model != "" {
					t.Fatalf("bridge admitted: status=%d model=%s body=%s", response.Code, runner.model, response.Body.String())
				}
				cfg, err := env.store.GetConfig(context.Background(), 1)
				if err != nil {
					t.Fatal(err)
				}
				input := &testutil.TestChannelRequest{Model: "contract-model", Content: "hello"}
				_, _, err = env.server.buildTestUpstreamRequestPlan(context.Background(), cfg, "key", input, input.Model, string(p), string(p), cfg.GetURLs()[0])
				if err == nil || !strings.Contains(err.Error(), "protocol conversion is disabled") {
					t.Fatalf("admin admitted bridge: %v", err)
				}
			})
		}
	}
}

func TestNativeResponsesWebsocketRejectsForeignChannels(t *testing.T) {
	for _, upstream := range []string{"openai", "anthropic", "gemini"} {
		t.Run(upstream, func(t *testing.T) {
			env := setupProxyTestEnv(t, []testChannel{{name: "foreign", upstreamProtocol: upstream, models: "contract-model"}}, map[int]string{0: "https://foreign.invalid"})
			env.server.client = &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
				t.Error("Responses websocket translated to foreign HTTP channel")
				return nil, fmt.Errorf("unexpected foreign request")
			})}
			conn := dialResponsesWebsocket(t, env.engine)
			if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
				t.Fatal(err)
			}
			if err := conn.WriteJSON(map[string]any{"type": "response.create", "model": "contract-model", "input": []any{map[string]any{"role": "user", "content": "hello"}}}); err != nil {
				t.Fatal(err)
			}
			var event struct {
				Type   string                      `json:"type"`
				Status int                         `json:"status"`
				Error  responsesWebsocketTestError `json:"error"`
			}
			if err := conn.ReadJSON(&event); err != nil {
				t.Fatal(err)
			}
			if event.Type != "error" || event.Status != http.StatusBadRequest || event.Error.Code != "upstream_error" || event.Error.Message != "no available upstream" {
				t.Fatalf("unexpected event: %+v", event)
			}
		})
	}
}

func TestNativeProtocolSubresourcesDoNotUseForeignChannels(t *testing.T) {
	for _, path := range []string{"/v1/responses/compact", "/v1/responses/input_tokens"} {
		t.Run(path, func(t *testing.T) {
			env := setupProxyTestEnv(t, []testChannel{{name: "chat", upstreamProtocol: "openai", models: "contract-model"}}, map[int]string{0: "https://chat.invalid"})
			env.server.client = &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
				t.Error("Responses subresource was translated")
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
			})}
			response := doProxyRequest(t, env.engine, path, map[string]any{"model": "contract-model", "input": "hello"}, nil)
			if response.Code != http.StatusServiceUnavailable {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}
