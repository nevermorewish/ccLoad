package chat_completions

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// Match clients that decode the optional reasoning alias as a string.
type chatReasoningMessage struct {
	Content          string `json:"content"`
	Reasoning        string `json:"reasoning"`
	ReasoningContent string `json:"reasoning_content"`
}

type chatReasoningResponse struct {
	Choices []struct {
		Message chatReasoningMessage `json:"message"`
		Delta   chatReasoningMessage `json:"delta"`
	} `json:"choices"`
}

func TestClaudeNativeReasoningStringClientCompatibility(t *testing.T) {
	for _, tc := range []struct{ name, blocks, want string }{
		{"thinking", `{"type":"thinking","thinking":"first","signature":"private-signature"},{"type":"thinking","thinking":" second"},`, "first second"},
		{"redacted", `{"type":"redacted_thinking","data":"private-redacted"},`, ""},
		{"empty", `{"type":"thinking","thinking":"","signature":"private-signature"},`, ""},
		{"plain", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := []byte(`{"id":"msg_1","type":"message","model":"kimi-k2.7-code","content":[` + tc.blocks + `{"type":"text","text":"answer"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":5}}`)
			out := ConvertClaudeResponseToOpenAINonStream(context.Background(), "kimi-k2.7-code", nil, nil, raw, nil)
			var decoded chatReasoningResponse
			if err := json.Unmarshal(out, &decoded); err != nil {
				t.Fatalf("string-based client cannot decode response: %v; %s", err, out)
			}
			if len(decoded.Choices) != 1 || decoded.Choices[0].Message.Content != "answer" || decoded.Choices[0].Message.ReasoningContent != tc.want {
				t.Fatalf("lost answer or reasoning: %s", out)
			}
			if strings.Contains(string(out), `"reasoning":`) || strings.Contains(string(out), "private-") {
				t.Fatalf("unexpected structured reasoning metadata: %s", out)
			}
		})
	}
}

func TestClaudeStreamReasoningStringClientCompatibility(t *testing.T) {
	events := []string{
		`{"type":"message_start","message":{"id":"msg_1","model":"kimi-k2.7-code"}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"first"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":" second"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"private-signature"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"redacted_thinking","data":"private-redacted"}}`,
		`{"type":"content_block_stop","index":1}`,
		`{"type":"content_block_start","index":2,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":2,"delta":{"type":"text_delta","text":"answer"}}`,
		`{"type":"content_block_stop","index":2}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}`,
		`{"type":"message_stop"}`,
	}
	var state any
	var text, reasoning strings.Builder
	for _, event := range events {
		for _, chunk := range ConvertClaudeResponseToOpenAI(context.Background(), "kimi-k2.7-code", nil, nil, []byte("data: "+event), &state) {
			if string(chunk) == "[DONE]" {
				continue
			}
			var decoded chatReasoningResponse
			if err := json.Unmarshal(chunk, &decoded); err != nil {
				t.Fatalf("string-based client cannot decode chunk: %v; %s", err, chunk)
			}
			if strings.Contains(string(chunk), `"reasoning":`) || strings.Contains(string(chunk), "private-") {
				t.Fatalf("unexpected structured reasoning metadata: %s", chunk)
			}
			for _, choice := range decoded.Choices {
				text.WriteString(choice.Delta.Content)
				reasoning.WriteString(choice.Delta.ReasoningContent)
			}
		}
	}
	if text.String() != "answer" || reasoning.String() != "first second" {
		t.Fatalf("lost or duplicated streamed text: answer=%q reasoning=%q", text.String(), reasoning.String())
	}
	buffered := []byte("data: " + strings.Join(events, "\ndata: ") + "\n")
	out := ConvertClaudeResponseToOpenAINonStream(context.Background(), "kimi-k2.7-code", nil, nil, buffered, nil)
	var decoded chatReasoningResponse
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("string-based client cannot decode buffered response: %v; %s", err, out)
	}
	if len(decoded.Choices) != 1 || decoded.Choices[0].Message.Content != text.String() || decoded.Choices[0].Message.ReasoningContent != reasoning.String() {
		t.Fatalf("buffered and streamed text differ: %s", out)
	}
	if strings.Contains(string(out), `"reasoning":`) || strings.Contains(string(out), "private-") {
		t.Fatalf("unexpected buffered reasoning metadata: %s", out)
	}
}
