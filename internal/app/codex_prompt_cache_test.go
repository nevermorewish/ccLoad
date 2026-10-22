package app

import (
	"fmt"
	"net/http"
	"testing"

	"ccLoad/internal/model"
	"ccLoad/internal/protocol"
	"github.com/tidwall/gjson"
)

func TestModernGPTCacheSurvivesWirePreparation(t *testing.T) {
	for _, modelName := range []string{"gpt-5.6", "gpt-5.6-sol", "gpt-6-astra", "gpt-6.1-sol"} {
		for _, auth := range []string{model.AuthTypeCodexOAuth, model.AuthTypeAPIKey} {
			t.Run(modelName+"/"+auth, func(t *testing.T) {
				cfg := &model.Config{AuthType: auth}
				original := []byte(fmt.Sprintf(`{"model":%q,"prompt_cache_key":"customer-cache","prompt_cache_options":{"mode":"explicit","ttl":"30m"},"input":[{"role":"developer","content":[{"type":"input_text","text":"Stable prefix","prompt_cache_breakpoint":{"mode":"explicit"}}]}]}`, modelName))
				body := prepareCodexOAuthResponsesBody(cfg, protocol.Codex, "/v1/responses", original, make(http.Header))
				// Both HTTP and WebSocket use the common preparation; HTTP also
				// removes transport-only fields and must retain cache controls.
				for transport, out := range map[string][]byte{
					"websocket": body,
					"http":      prepareCodexOAuthHTTPBody(cfg, protocol.Codex, "/v1/responses", body),
				} {
					for _, path := range []string{"prompt_cache_key", "prompt_cache_options", "input"} {
						if got, want := gjson.GetBytes(out, path).Raw, gjson.GetBytes(original, path).Raw; got != want {
							t.Fatalf("%s: %s changed: got %s want %s", transport, path, got, want)
						}
					}
				}
			})
		}
	}
}
