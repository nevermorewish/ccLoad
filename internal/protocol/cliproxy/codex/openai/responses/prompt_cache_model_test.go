package responses

import (
	"fmt"
	"testing"

	"github.com/tidwall/gjson"
)

func TestExplicitPromptCacheModelBoundary(t *testing.T) {
	for _, tc := range []struct {
		model string
		want  bool
	}{
		{"gpt-5.6", true}, {"gpt-5.6-sol", true}, {"gpt-5.6-terra", true},
		{"gpt-5.6-luna", true}, {"gpt-5.6-2026-10-01", true}, {"gpt-5.10", true},
		{"gpt-6", true}, {"gpt-6-astra", true}, {"gpt-6-sol", true},
		{"gpt-6.1-sol", true}, {"gpt-7", true}, {" GPT-6.1-SOL ", true},
		{"gpt-5.5", false}, {"gpt-5.5-pro", false}, {"gpt-5.3-codex", false},
		{"gpt-5", false}, {"gpt-4.1", false}, {"gpt-5.60invalid", false},
		{"gpt-6foo", false}, {"gpt-6.1foo", false}, {"alias", false}, {"", false},
	} {
		t.Run(tc.model, func(t *testing.T) {
			if got := preservesExplicitPromptCache(tc.model); got != tc.want {
				t.Fatalf("preservesExplicitPromptCache(%q) = %t, want %t", tc.model, got, tc.want)
			}
		})
	}
}

func TestModernGPTPreservesExplicitCacheRequest(t *testing.T) {
	for _, model := range []string{"gpt-5.6", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna", "gpt-5.10", "gpt-6-astra", "gpt-6-sol", "gpt-6.1-sol"} {
		for _, mode := range []string{"explicit", "implicit"} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/stream_%t", model, mode, stream), func(t *testing.T) {
					body := []byte(fmt.Sprintf(`{"model":%q,"prompt_cache_key":"customer-cache","prompt_cache_options":{"mode":%q,"ttl":"30m","prewarm":false},"input":[{"role":"developer","content":[{"type":"input_text","text":"Stable prefix","prompt_cache_breakpoint":{"mode":"explicit"}}]},{"type":"function_call","call_id":"c1","name":"lookup","arguments":"{}"},{"type":"function_call_output","call_id":"c1","output":[{"type":"input_text","text":"Result","prompt_cache_breakpoint":{"mode":"explicit"}}]}]}`, model, mode))
					out, err := ConvertOpenAIResponsesRequestToCodex(model, body, stream)
					if err != nil {
						t.Fatal(err)
					}
					for _, path := range []string{"prompt_cache_key", "prompt_cache_options", "input"} {
						if got, want := gjson.GetBytes(out, path).Raw, gjson.GetBytes(body, path).Raw; got != want {
							t.Fatalf("%s changed: got %s, want %s", path, got, want)
						}
					}
				})
			}
		}
	}
}

func TestExplicitCacheUsesEffectiveModel(t *testing.T) {
	for _, tc := range []struct {
		argument, bodyModel string
		wantPreserved       bool
	}{
		{"gpt-5.6-sol", "alias", true},
		{"gpt-5.5", "gpt-6.1-sol", false},
		{"", "gpt-6.1-sol", true},
		{"", "gpt-5.5", false},
	} {
		t.Run(tc.argument+"/"+tc.bodyModel, func(t *testing.T) {
			body := []byte(fmt.Sprintf(`{"model":%q,"prompt_cache_options":{"mode":"explicit"},"input":[{"role":"user","content":[{"type":"input_text","text":"hi","prompt_cache_breakpoint":{"mode":"explicit"}}]}]}`, tc.bodyModel))
			out, err := ConvertOpenAIResponsesRequestToCodex(tc.argument, body, false)
			if err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{"prompt_cache_options", "input.0.content.0.prompt_cache_breakpoint"} {
				if got := gjson.GetBytes(out, path).Exists(); got != tc.wantPreserved {
					t.Fatalf("%s exists = %t, want %t", path, got, tc.wantPreserved)
				}
			}
		})
	}
}
