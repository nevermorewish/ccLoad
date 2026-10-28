package testutil

import (
	"testing"

	"ccLoad/internal/model"

	"github.com/tidwall/gjson"
)

func TestAnthropicPassthroughTestOptions(t *testing.T) {
	cfg := &model.Config{URLs: model.ChannelURLs{{URL: "https://bedrock.example"}}, CustomRequestRules: &model.CustomRequestRules{AnthropicPassthrough: true}}
	for _, explicit := range []bool{false, true} {
		input := &TestChannelRequest{Model: "claude-sonnet-4-6", Content: "hello"}
		if explicit {
			input.SystemPrompt = "customer system"
			input.BuiltinSearch = true
			input.ThinkingEffort = "low"
		}
		_, headers, body, err := (&AnthropicTester{}).Build(cfg, "key", input)
		if err != nil {
			t.Fatal(err)
		}
		if gjson.GetBytes(body, "max_tokens").Int() != 512 || headers.Get("X-Claude-Code-Session-Id") != "" {
			t.Fatalf("body=%s headers=%v", body, headers)
		}
		if explicit {
			if gjson.GetBytes(body, "system.0.text").String() != "customer system" || gjson.GetBytes(body, "tools.0.type").String() != "web_search_20250305" {
				t.Fatalf("options lost: %s", body)
			}
		} else {
			for _, key := range []string{"system", "tools", "output_config", "metadata"} {
				if gjson.GetBytes(body, key).Exists() {
					t.Errorf("injected %s: %s", key, body)
				}
			}
		}
	}
}
