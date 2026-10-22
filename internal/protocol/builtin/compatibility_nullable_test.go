package builtin

import (
	"testing"

	chatresponses "ccLoad/internal/protocol/cliproxy/codex/openai/chat-completions"
)

func TestCompatibilityNullableNoopParameters(t *testing.T) {
	for _, field := range []string{"stop", "seed", "frequency_penalty", "presence_penalty", "logit_bias", "top_logprobs", "n"} {
		t.Run(field, func(t *testing.T) {
			_, err := chatresponses.ConvertOpenAIRequestToCodex("any", []byte(`{"messages":[{"role":"user","content":"hi"}],"`+field+`":null}`), false)
			if err != nil {
				t.Fatalf("optional null rejected: %v", err)
			}
		})
	}
}
