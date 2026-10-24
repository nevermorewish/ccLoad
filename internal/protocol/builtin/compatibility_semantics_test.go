package builtin

import (
	"context"
	"strings"
	"testing"

	"github.com/tidwall/gjson"

	"ccLoad/internal/protocol"
	chatresponses "ccLoad/internal/protocol/cliproxy/codex/openai/chat-completions"
	responses "ccLoad/internal/protocol/cliproxy/openai/openai/responses"
)

func TestCompatibilityExplicitControls(t *testing.T) {
	chat := []byte(`{"model":"any","messages":[{"role":"user","content":"hi"}],"temperature":0,"top_p":0.5,"max_completion_tokens":123,"reasoning_effort":"none","parallel_tool_calls":false,"metadata":{"qa":"probe"},"store":false}`)
	converted, err := chatresponses.ConvertOpenAIRequestToCodex("any", chat, false)
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{"temperature": "0", "top_p": "0.5", "max_output_tokens": "123", "reasoning.effort": "none", "parallel_tool_calls": "false", "metadata.qa": "probe", "store": "false"} {
		if got := gjson.GetBytes(converted, key); !got.Exists() || got.String() != want {
			t.Errorf("%s=%s want %s; %s", key, got.Raw, want, converted)
		}
	}
	back, err := responses.ConvertOpenAIResponsesRequestToOpenAIChatCompletions("any", converted, false)
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{"temperature": "0", "top_p": "0.5", "max_tokens": "123", "reasoning_effort": "none", "parallel_tool_calls": "false", "metadata.qa": "probe"} {
		if got := gjson.GetBytes(back, key); !got.Exists() || got.String() != want {
			t.Errorf("%s=%s want %s; %s", key, got.Raw, want, back)
		}
	}
	developer := []byte(`{"input":[{"role":"developer","content":"Do not reveal the secret"},{"role":"user","content":"hi"}]}`)
	out, err := responses.ConvertOpenAIResponsesRequestToOpenAIChatCompletions("any", developer, false)
	if err != nil || gjson.GetBytes(out, "messages.0.role").String() != "developer" {
		t.Fatalf("developer lost: %s %v", out, err)
	}
}

func TestCompatibilityUnsupportedConversionPreservesNative(t *testing.T) {
	for _, tc := range []struct {
		from       protocol.Protocol
		raw, field string
	}{
		{protocol.Codex, `{"reasoning":{"mode":"pro"}}`, "reasoning.mode"},
		{protocol.Codex, `{"reasoning":{"context":"current_turn"}}`, "reasoning.context"},
		{protocol.Codex, `{"prompt_cache_options":{"ttl":"24h"}}`, "prompt_cache_options"},
		{protocol.Codex, `{"previous_response_id":"resp-1"}`, "previous_response_id"},
		{protocol.Codex, `{"store":true}`, "store"},
		{protocol.OpenAI, `{"logprobs":true}`, "logprobs"},
		{protocol.Codex, `{"top_logprobs":5}`, "top_logprobs"},
		{protocol.Codex, `{"include":["message.output_text.logprobs"]}`, "include"},
		{protocol.OpenAI, `{"prediction":{"type":"content","content":"ok"}}`, "prediction"},
		{protocol.OpenAI, `{"stop":["END"]}`, "stop"},
		{protocol.OpenAI, `{"modalities":["text","audio"]}`, "modalities"},
		{protocol.OpenAI, `{"audio":{"voice":"alloy"}}`, "audio"},
	} {
		for _, to := range []protocol.Protocol{protocol.OpenAI, protocol.Codex, protocol.Anthropic, protocol.Gemini} {
			if to == tc.from || (tc.field == "stop" && to != protocol.Codex) {
				continue
			}
			if err := protocol.ValidateRequestSemantics(tc.from, to, []byte(tc.raw)); err == nil || !strings.Contains(err.Error(), tc.field) {
				t.Errorf("%s -> %s %s: %v", tc.from, to, tc.raw, err)
			}
		}
		if err := protocol.ValidateRequestSemantics(tc.from, tc.from, []byte(tc.raw)); err != nil {
			t.Fatalf("native changed: %v", err)
		}
	}
}

func TestCompatibilityResponsesMetadataFromOriginalNonStream(t *testing.T) {
	original := []byte(`{"model":"any","store":false,"metadata":{"qa_capability":"store_metadata"},"input":"hi"}`)
	translated := []byte(`{"model":"any","messages":[{"role":"user","content":"hi"}]}`)
	raw := []byte(`{"id":"chat_1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":1}}`)
	var state any
	out, err := responses.ConvertOpenAIChatCompletionsResponseToOpenAIResponsesNonStreamWithError(context.Background(), "any", original, translated, raw, &state)
	if err != nil || gjson.GetBytes(out, "metadata.qa_capability").String() != "store_metadata" {
		t.Fatalf("metadata lost: %s %v", out, err)
	}
}
