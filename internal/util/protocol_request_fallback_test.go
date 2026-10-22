package util

import (
	"net/http"
	"strings"
	"testing"

	"ccLoad/internal/protocol"
)

func TestShouldFallbackProtocolForUpstream(t *testing.T) {
	missingMessages := "{\"error\":{\"message\":\"The request failed because it is missing `messages` parameter. Request id: test\",\"type\":\"BadRequest\",\"param\":\"\",\"code\":\"MissingParameter\"}}"
	for _, tt := range []struct {
		name     string
		upstream protocol.Protocol
		status   int
		body     string
		want     bool
	}{
		{"responses expects messages", protocol.Codex, 400, missingMessages, true},
		{"single quoted messages", protocol.Codex, 400, strings.ReplaceAll(missingMessages, "`", "'"), true},
		{"missing input is validation", protocol.Codex, 400, strings.ReplaceAll(missingMessages, "messages", "input"), false},
		{"missing nested messages is validation", protocol.Codex, 400, strings.ReplaceAll(missingMessages, "`messages`", "`tools.messages`"), false},
		{"other error code", protocol.Codex, 400, strings.ReplaceAll(missingMessages, "MissingParameter", "InvalidParameter"), false},
		{"chat missing messages is validation", protocol.OpenAI, 400, missingMessages, false},
		{"anthropic missing messages is validation", protocol.Anthropic, 400, missingMessages, false},
		{"unknown protocol", "", 400, missingMessages, false},
		{"server failure", protocol.Codex, 500, missingMessages, false},
		{"invalid json", protocol.Codex, 400, `{"error":`, false},
		{"empty body", protocol.Codex, 400, "", false},
		{"unrelated unknown field", protocol.Codex, 400, `{"error":{"message":"json: unknown field \"summary\"","code":"InvalidParameter"}}`, false},
		{"existing endpoint fallback", protocol.Anthropic, http.StatusNotFound, `{"error":{"message":"endpoint not found"}}`, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := ShouldFallbackProtocolForUpstream(tt.status, []byte(tt.body), tt.upstream); got != tt.want {
				t.Fatalf("fallback=%v, want %v", got, tt.want)
			}
		})
	}
}
