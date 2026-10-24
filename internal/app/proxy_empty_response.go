package app

import (
	"fmt"
	"net/http"
	"strings"

	"ccLoad/internal/protocol"
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

// geminiCompletionFailure checks the complete JSON before conversion or commit.
// A 200 error envelope must retain its real status and body. Token counts alone
// cannot prove that a response contains output usable by the client.
func geminiCompletionFailure(path string, body []byte) (int, error) {
	generation := strings.HasSuffix(path, ":generateContent") || strings.HasSuffix(path, ":streamGenerateContent")
	var response struct {
		Error          any `json:"error"`
		PromptFeedback struct {
			BlockReason string `json:"blockReason"`
		} `json:"promptFeedback"`
		Candidates []struct {
			FinishReason string `json:"finishReason"`
			Content      struct {
				Parts []map[string]any `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if err := sonic.Unmarshal(body, &response); err != nil {
		if generation {
			return http.StatusBadGateway, fmt.Errorf("%w (Gemini generation response is not valid JSON)", util.ErrUpstreamInvalidResponse)
		}
		return 0, nil
	}
	if response.Error != nil {
		return classifySSEErrorStatus(body), nil
	}
	if !generation || response.PromptFeedback.BlockReason != "" {
		return 0, nil
	}
	if len(response.Candidates) == 0 {
		return http.StatusBadGateway, fmt.Errorf("%w (200 OK without Gemini candidates or a prompt block reason)", util.ErrUpstreamEmptyResponse)
	}
	for _, candidate := range response.Candidates {
		// Safety, truncation and other explicit finish reasons are protocol
		// outcomes, not empty successful completions to replay on another model.
		if candidate.FinishReason != "" && candidate.FinishReason != "STOP" {
			return 0, nil
		}
		for _, part := range candidate.Content.Parts {
			for name, value := range part {
				if name != "thought" && name != "thoughtSignature" && !isEmptyChatOutputValue(value) {
					return 0, nil
				}
			}
		}
	}
	return http.StatusBadGateway, fmt.Errorf("%w (200 OK without Gemini candidate output)", util.ErrUpstreamEmptyResponse)
}

func rejectInvalidNonStreamCompletion(reqCtx *requestContext, resp *http.Response, hdr http.Header, stats *streamReadStats, upstreamProtocol string, body []byte) (*fwResult, float64, error) {
	if reqCtx.isStreaming || resp.StatusCode != http.StatusOK {
		return nil, 0, nil
	}
	var status int
	var err error
	switch upstreamProtocol {
	case util.ProtocolOpenAI:
		if isEmptyChatCompletion(body) {
			status = http.StatusBadGateway
			err = fmt.Errorf("%w (200 OK without assistant output)", util.ErrUpstreamEmptyResponse)
		}
	case util.ProtocolGemini:
		path := reqCtx.transformPlan.UpstreamPath
		if reqCtx.transformPlan.NeedsTransform {
			path = buildGeminiGeneratePath(reqCtx.transformPlan.RequestModel(), false)
		}
		// Explicitly exclude token counting and other non-generation endpoints.
		if reqCtx.transformPlan.RequestFamily == protocol.RequestFamilyCountTokens {
			path = ""
		}
		status, err = geminiCompletionFailure(path, body)
	}
	if status == 0 {
		return nil, 0, nil
	}
	return &fwResult{
		Status: status, UpstreamStatus: resp.StatusCode, Header: hdr,
		Body: body, BytesReceived: stats.totalBytes,
		FirstByteTime: responseFirstByteSec(reqCtx, stats),
	}, reqCtx.Duration().Seconds(), err
}
