package app

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	"ccLoad/internal/model"
)

func TestResponsesHTTPRouteIsolationExpiryAndBound(t *testing.T) {
	var routes responsesHTTPRouteStore
	routes.put("token-a", "resp-1", responsesHTTPRoute{channelID: 7})
	if _, ok := routes.get("token-b", "resp-1"); ok {
		t.Fatal("cross-token route exposed")
	}
	if route, ok := routes.get("token-a", "resp-1"); !ok || route.channelID != 7 {
		t.Fatal("route missing")
	}
	routes.entries[responsesHTTPRouteKey{"token-a", "resp-1"}] = responsesHTTPRoute{expires: time.Now().Add(-time.Second)}
	if _, ok := routes.get("token-a", "resp-1"); ok {
		t.Fatal("expired route retained")
	}
	for i := 0; i < responsesHTTPRouteLimit+1; i++ {
		routes.put("token-a", fmt.Sprint(i), responsesHTTPRoute{})
	}
	if len(routes.entries) != responsesHTTPRouteLimit {
		t.Fatalf("unbounded routes: %d", len(routes.entries))
	}
}

func TestResponsesHTTPRouteParsesLargeNonStreamWithoutNestedIDCollision(t *testing.T) {
	raw := `{"id":"resp-owner","object":"response","output":[{"id":"msg-wrong","status":"in_progress","content":[{"type":"output_text","text":"` + strings.Repeat("x", maxUsageBodySize) + `"}]}],"status":"completed","metadata":{"id":"wrong","object":"other","status":"failed"}}`
	parser := newJSONUsageParser("codex")
	for len(raw) > 0 {
		n := min(117, len(raw))
		if err := parser.Feed([]byte(raw[:n])); err != nil {
			t.Fatal(err)
		}
		raw = raw[n:]
	}
	turn, ok := parser.GetResponsesTurnResult()
	if !ok || turn.completedResponseID != "resp-owner" {
		t.Fatalf("large response route=%+v ok=%v", turn, ok)
	}
}

func TestProxyResponsesHTTPContinuationPinsRoute(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprint(streaming), func(t *testing.T) {
			env := setupProxyTestEnv(t, []testChannel{
				{name: "first", upstreamProtocol: "codex", models: "state-model", apiKey: "key-first"},
				{name: "owner", upstreamProtocol: "codex", models: "state-model", apiKey: "key-owner"},
			}, map[int]string{0: "https://first.example", 1: "https://owner.example"})
			ownerCalls, foreignCalls := 0, 0
			env.server.client = &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				body, _ := io.ReadAll(r.Body)
				if r.URL.Host == "first.example" {
					foreignCalls++
					return &http.Response{StatusCode: 400, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"code":"unsupported_parameter","param":"reasoning","message":"unsupported parameter"}}`))}, nil
				}
				ownerCalls++
				if ownerCalls == 2 && gjson.GetBytes(body, "previous_response_id").String() != "resp_owned" {
					t.Errorf("continuation state removed: %s", body)
				}
				response := `{"id":"resp_owned","object":"response","status":"completed","model":"state-model","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":10,"output_tokens":1}}`
				ct := "application/json"
				if streaming {
					ct = "text/event-stream"
					response = "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":" + response + "}\n\n"
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {ct}}, Body: io.NopCloser(strings.NewReader(response))}, nil
			})}
			request := map[string]any{"model": "state-model", "stream": streaming, "input": "hi"}
			first := doProxyRequest(t, env.engine, "/v1/responses", request, nil)
			if first.Code != 200 {
				t.Fatalf("first=%d %s", first.Code, first.Body.String())
			}
			request["previous_response_id"] = "resp_owned"
			second := doProxyRequest(t, env.engine, "/v1/responses", request, nil)
			if second.Code != 200 || ownerCalls != 2 || foreignCalls != 1 {
				t.Fatalf("second=%d owner=%d foreign=%d %s", second.Code, ownerCalls, foreignCalls, second.Body.String())
			}
			request["previous_response_id"] = "resp_unknown"
			unknown := doProxyRequest(t, env.engine, "/v1/responses", request, nil)
			if unknown.Code != 400 || gjson.GetBytes(unknown.Body.Bytes(), "error.code").String() != "previous_response_not_found" || ownerCalls != 2 || foreignCalls != 1 {
				t.Fatalf("unknown=%d %s", unknown.Code, unknown.Body.String())
			}
			// Changing the owner credential must not send the old response ID elsewhere.
			configs, _ := env.store.ListConfigs(context.Background())
			for _, cfg := range configs {
				if cfg.Name == "owner" {
					if err := env.store.DeleteAllAPIKeys(context.Background(), cfg.ID); err != nil {
						t.Fatal(err)
					}
					if err := env.store.CreateAPIKeysBatch(context.Background(), []*model.APIKey{{ChannelID: cfg.ID, KeyIndex: 0, APIKey: "replacement"}}); err != nil {
						t.Fatal(err)
					}
					env.server.invalidateChannelRelatedCache(cfg.ID)
				}
			}
			request["previous_response_id"] = "resp_owned"
			changed := doProxyRequest(t, env.engine, "/v1/responses", request, nil)
			if changed.Code == 200 || ownerCalls != 2 || foreignCalls != 1 {
				t.Fatalf("credential boundary crossed: %d owner=%d foreign=%d", changed.Code, ownerCalls, foreignCalls)
			}
		})
	}
}
