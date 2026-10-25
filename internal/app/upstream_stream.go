package app

import (
	"strings"

	"ccLoad/internal/model"
	"ccLoad/internal/protocol"

	"github.com/tidwall/sjson"
)

// Channel stream forcing applies only to API-key generation endpoints. Provider
// OAuth transports have their own framing contracts; token counting, embeddings,
// images and compaction must keep their original endpoint semantics.
func shouldForceUpstreamStream(cfg *model.Config, upstream protocol.Protocol, path string) bool {
	if cfg == nil || cfg.GetAuthType() != model.AuthTypeAPIKey || cfg.CustomRequestRules == nil || !cfg.CustomRequestRules.ForceUpstreamStream {
		return false
	}
	path = strings.TrimRight(path, "/")
	switch upstream {
	case protocol.OpenAI:
		return strings.HasSuffix(path, "/chat/completions")
	case protocol.Anthropic:
		return strings.HasSuffix(path, "/messages")
	case protocol.Codex:
		return strings.HasSuffix(path, "/responses")
	case protocol.Gemini:
		return strings.HasSuffix(path, ":generateContent") || strings.HasSuffix(path, ":streamGenerateContent")
	default:
		return false
	}
}

func forceStreamRequestBody(body []byte, upstream protocol.Protocol, clientStreaming bool) ([]byte, error) {
	if upstream == protocol.Gemini {
		return sjson.DeleteBytes(body, "stream")
	}
	body, err := sjson.SetBytes(body, "stream", true)
	// Only aggregation needs an extra usage event; preserve streaming clients' options.
	if err != nil || upstream != protocol.OpenAI || clientStreaming {
		return body, err
	}
	return sjson.SetBytes(body, "stream_options.include_usage", true)
}

func forceStreamRequestPath(path string, upstream protocol.Protocol) string {
	if upstream == protocol.Gemini && strings.HasSuffix(path, ":generateContent") {
		return strings.TrimSuffix(path, ":generateContent") + ":streamGenerateContent"
	}
	return path
}
