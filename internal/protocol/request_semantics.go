package protocol

import (
	"fmt"

	"github.com/tidwall/gjson"
)

// ValidateRequestSemantics prevents a successful conversion from silently
// changing explicit client controls. Native requests remain upstream-owned.
func ValidateRequestSemantics(from, to Protocol, raw []byte) error {
	if from == to {
		return nil
	}
	root := gjson.ParseBytes(raw)
	reject := func(path string) error {
		return &RequestParameterError{Param: path, Message: fmt.Sprintf("parameter %q cannot be represented by %s -> %s conversion; use a native %s upstream", path, from, to, from)}
	}
	if from == Codex {
		if root.Get("top_logprobs").Exists() {
			return reject("top_logprobs")
		}
		for _, value := range root.Get("include").Array() {
			if value.String() == "message.output_text.logprobs" {
				return reject("include")
			}
		}
		for _, path := range []string{"previous_response_id", "conversation", "reasoning.mode", "reasoning.context", "prompt_cache_options", "background"} {
			if value := root.Get(path); value.Type == gjson.Null || path == "background" && value.Type == gjson.False {
				continue
			}
			if root.Get(path).Exists() {
				return reject(path)
			}
		}
		if root.Get("store").Bool() {
			return reject("store")
		}
		if to != OpenAI {
			for _, path := range []string{"metadata"} {
				if root.Get(path).Exists() {
					return reject(path)
				}
			}
		}
	}
	if from == OpenAI {
		// Request and response conversion must both preserve the requested feature.
		// Current response translators do not carry token log probabilities.
		if root.Get("logprobs").Bool() {
			return reject("logprobs")
		}
		paths := []string{"prediction", "audio", "prompt_cache_options", "top_logprobs"}
		if to != Gemini {
			paths = append(paths, "modalities")
		} else {
			for _, modality := range root.Get("modalities").Array() {
				if modality.String() == "audio" {
					return reject("modalities")
				}
			}
		}
		if to == Codex {
			paths = append(paths, "stop", "seed", "frequency_penalty", "presence_penalty", "logit_bias", "functions", "function_call")
			if v := root.Get("n"); v.Exists() && v.Int() != 1 {
				return reject("n")
			}
		} else {
			paths = append(paths, "metadata")
		}
		for _, path := range paths {
			if root.Get(path).Exists() {
				return reject(path)
			}
		}
	}
	if to == Anthropic && (from == OpenAI || from == Codex) {
		field := "messages"
		if from == Codex {
			field = "input"
		}
		for i, message := range root.Get(field).Array() {
			for j, part := range message.Get("content").Array() {
				if part.Get("type").String() == "input_audio" {
					return reject(fmt.Sprintf("%s[%d].content[%d]", field, i, j))
				}
			}
		}
	}
	return nil
}

// RequestParameterError retains a machine-readable parameter across conversion.
type RequestParameterError struct{ Param, Message string }

func (e *RequestParameterError) Error() string { return e.Message }
