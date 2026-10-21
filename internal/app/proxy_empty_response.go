package app

import (
	"net/http"

	"ccLoad/internal/util"

	"github.com/bytedance/sonic"
)

// isEmptyChatCompletion only rejects completed assistant messages with no output.
// Truncation, filtering, hidden reasoning usage and unknown output extensions are
// deliberately preserved: a missing text field alone does not imply failure.
func isEmptyChatCompletion(body []byte) bool {
	var response struct {
		Object  string `json:"object"`
		Error   any    `json:"error"`
		Choices []struct {
			FinishReason string         `json:"finish_reason"`
			Message      map[string]any `json:"message"`
		} `json:"choices"`
		Usage struct {
			CompletionTokens        int `json:"completion_tokens"`
			OutputTokens            int `json:"output_tokens"`
			CompletionTokensDetails struct {
				ReasoningTokens int `json:"reasoning_tokens"`
			} `json:"completion_tokens_details"`
		} `json:"usage"`
	}
	if err := sonic.Unmarshal(body, &response); err != nil {
		return false
	}
	if response.Error != nil || (response.Object != "" && response.Object != "chat.completion") || len(response.Choices) == 0 {
		return false
	}
	if response.Usage.CompletionTokens > 0 || response.Usage.OutputTokens > 0 || response.Usage.CompletionTokensDetails.ReasoningTokens > 0 {
		return false
	}
	for _, choice := range response.Choices {
		if choice.FinishReason != "stop" || choice.Message["role"] != "assistant" {
			return false
		}
		for name, value := range choice.Message {
			if name != "role" && !isEmptyChatOutputValue(value) {
				return false
			}
		}
	}
	return true
}

func isEmptyChatOutputValue(value any) bool {
	switch v := value.(type) {
	case nil:
		return true
	case string:
		return v == ""
	case []any:
		return len(v) == 0
	case map[string]any:
		return len(v) == 0
	default:
		return false
	}
}

func rejectEmptyNonStreamChatResponse(reqCtx *requestContext, resp *http.Response, hdr http.Header, stats *streamReadStats, upstreamProtocol string, body []byte) (*fwResult, float64, error) {
	if reqCtx.isStreaming || resp.StatusCode != http.StatusOK || upstreamProtocol != util.ProtocolOpenAI || !isEmptyChatCompletion(body) {
		return nil, 0, nil
	}
	result, duration, err := emptyOKResponseResult(reqCtx, resp, hdr, stats, "without assistant output")
	// Retain the actual upstream response for diagnostics, before any translation.
	result.Body = body
	result.BytesReceived = stats.totalBytes
	return result, duration, err
}
