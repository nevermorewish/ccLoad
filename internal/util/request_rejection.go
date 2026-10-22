package util

import (
	"encoding/json"
	"strconv"
	"strings"
)

// RequestRejection 描述上游 4xx 是否在拒绝调用方请求本身，而不是在说明该上游不可用。
type RequestRejection int

const (
	// RequestRejectionNone 不属于下列两类，按既有状态码与响应体规则分类。
	RequestRejectionNone RequestRejection = iota
	// RequestRejectionNativeOnly 上游拒绝了调用方显式设置的顶层参数或取值、模型不支持的取值，
	// 或找不到 previous_response_id 指向的响应。另一家同协议上游可能支持，允许继续切换；
	// 默认只允许原生协议；调用方可为已验证能无损保留参数的转换放行。与渠道健康无关，不冷却。
	RequestRejectionNativeOnly
	// RequestRejectionFinal 不应通过换上游绕过的拒绝：内容安全拦截与确定性的请求结构错误。
	// 直接返回客户端，不冷却。
	RequestRejectionFinal
)

// requestParameterRoots 是请求顶层成员中代表调用方显式意图的部分。只有拒绝落在这些成员上，
// 才能断定问题在请求本身。input/messages/tools 等会话内容、附件下载（url）、model、
// max_output_tokens 等的拒绝常常是上游特有的（存储条目、加密状态、历史形状、下载超时、上限差异），
// 切换渠道能合法恢复，刻意不在此列。
var requestParameterRoots = map[string]struct{}{
	"temperature": {}, "top_p": {}, "top_k": {}, "top_logprobs": {}, "logprobs": {}, "include": {},
	"reasoning": {}, "reasoning_effort": {}, "prompt_cache_options": {}, "prediction": {},
	"modalities": {}, "audio": {}, "stop": {}, "text": {}, "verbosity": {}, "tool_choice": {},
	"parallel_tool_calls": {}, "max_tool_calls": {}, "n": {}, "seed": {}, "frequency_penalty": {},
	"presence_penalty": {}, "logit_bias": {}, "functions": {}, "function_call": {}, "truncation": {},
	"previous_response_id": {}, "store": {}, "metadata": {}, "prompt_cache_key": {}, "prompt_cache_retention": {},
}

// requestParameterRejectionCodes 是 OpenAI 契约中针对参数或取值的错误码。
var requestParameterRejectionCodes = map[string]struct{}{
	"unsupported_parameter": {}, "unknown_parameter": {}, "invalid_parameter": {},
	"unsupported_value": {}, "invalid_value": {},
}

type upstreamRequestError struct {
	code    string
	param   string
	message string
}

// ClassifyRequestRejection 判断上游 HTTP 400（或携带同类错误的 SSE 错误事件）是否属于
// 请求本身被拒绝。调用方仍需自行判断状态码是否适用。
func ClassifyRequestRejection(responseBody []byte) RequestRejection {
	errs := parseUpstreamRequestErrors(responseBody)
	for _, e := range errs {
		if isFinalRequestRejection(e) {
			return RequestRejectionFinal
		}
	}
	for _, e := range errs {
		if isNativeOnlyRequestRejection(e) {
			return RequestRejectionNativeOnly
		}
	}
	return RequestRejectionNone
}

// IsSamplingParameterRejection identifies errors whose only rejected controls are
// temperature/top_p. Callers must still verify the target converter preserves
// both controls. Ambiguous or mixed errors retain the native-only restriction.
func IsSamplingParameterRejection(body []byte) bool {
	errs := parseUpstreamRequestErrors(body)
	if len(errs) == 0 {
		return false
	}
	for _, e := range errs {
		if !isNativeOnlyRequestRejection(e) || isFinalRequestRejection(e) {
			return false
		}
		param := strings.TrimPrefix(e.param, "$.")
		if param == "" {
			param, _ = quotedParameterAfter(e.message, "unsupported parameter: '")
		}
		if param != "temperature" && param != "top_p" {
			return false
		}
	}
	return true
}

func isFinalRequestRejection(e upstreamRequestError) bool {
	return e.code == "content_policy_violation" ||
		strings.Contains(e.message, "flagged by omni-moderation") ||
		strings.Contains(e.message, "flagged for possible cybersecurity risk") ||
		strings.Contains(e.message, "consecutive 'configuration_update' items are not allowed")
}

func isNativeOnlyRequestRejection(e upstreamRequestError) bool {
	if _, ok := requestParameterRejectionCodes[e.code]; ok {
		root := requestParameterRoot(e.param)
		if _, named := requestParameterRoots[root]; named {
			// include 的其他取值（如 reasoning.encrypted_content）是上游能力差异，只有 logprobs 属于调用方意图。
			if root != "include" || strings.Contains(e.message, "logprobs") {
				return true
			}
		}
	}
	// 以下覆盖不带 code/param 的同类拒绝（部分中转站只保留官方文案）。
	switch {
	case strings.Contains(e.message, "logprobs are not supported with reasoning models"):
		return true
	case strings.HasPrefix(e.message, "unsupported value: '") && strings.Contains(e.message, "is not supported with the '"):
		return true
	case strings.Contains(e.message, "previous_response_id is not available"),
		e.code == "previous_response_not_found",
		strings.Contains(e.message, "previous response with id") && strings.Contains(e.message, "not found"):
		return true
	}
	if name, ok := quotedParameterAfter(e.message, "unsupported parameter: '"); ok {
		_, named := requestParameterRoots[requestParameterRoot(name)]
		return named
	}
	if name, ok := strings.CutSuffix(e.message, " is not supported on this model"); ok {
		_, named := requestParameterRoots[requestParameterRoot(name)]
		return named
	}
	return false
}

// requestParameterRoot 取参数路径的顶层成员："reasoning.effort" → "reasoning"，"include[0]" → "include"。
func requestParameterRoot(param string) string {
	param = strings.TrimPrefix(param, "$.")
	if index := strings.IndexAny(param, ".["); index >= 0 {
		param = param[:index]
	}
	return param
}

func quotedParameterAfter(message, prefix string) (string, bool) {
	rest, ok := strings.CutPrefix(message, prefix)
	if !ok {
		return "", false
	}
	name, _, found := strings.Cut(rest, "'")
	return name, found && name != ""
}

// parseUpstreamRequestErrors 宽松解析 error、response.error 与顶层错误字段；code/param 可能是
// null、数字或字符串，error 也可能直接是字符串。返回值均已小写并去除首尾空白。
func parseUpstreamRequestErrors(body []byte) []upstreamRequestError {
	if len(body) == 0 {
		return nil
	}
	var payload struct {
		Code     any             `json:"code"`
		Param    any             `json:"param"`
		Message  any             `json:"message"`
		Error    json.RawMessage `json:"error"`
		Response struct {
			Error json.RawMessage `json:"error"`
		} `json:"response"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil
	}
	var errs []upstreamRequestError
	for _, raw := range []json.RawMessage{payload.Error, payload.Response.Error} {
		if detail, ok := decodeUpstreamRequestError(raw); ok {
			errs = append(errs, detail)
		}
	}
	top := upstreamRequestError{
		code:    requestErrorField(payload.Code),
		param:   requestErrorField(payload.Param),
		message: requestErrorField(payload.Message),
	}
	if top.code != "" || top.message != "" {
		errs = append(errs, top)
	}
	return errs
}

func decodeUpstreamRequestError(raw json.RawMessage) (upstreamRequestError, bool) {
	if len(raw) == 0 {
		return upstreamRequestError{}, false
	}
	var message string
	if json.Unmarshal(raw, &message) == nil {
		message = strings.ToLower(strings.TrimSpace(message))
		return upstreamRequestError{message: message}, message != ""
	}
	var detail struct {
		Code    any `json:"code"`
		Param   any `json:"param"`
		Message any `json:"message"`
	}
	if json.Unmarshal(raw, &detail) != nil {
		return upstreamRequestError{}, false
	}
	return upstreamRequestError{
		code:    requestErrorField(detail.Code),
		param:   requestErrorField(detail.Param),
		message: requestErrorField(detail.Message),
	}, true
}

func requestErrorField(value any) string {
	switch typed := value.(type) {
	case string:
		return strings.ToLower(strings.TrimSpace(typed))
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	default:
		return ""
	}
}
