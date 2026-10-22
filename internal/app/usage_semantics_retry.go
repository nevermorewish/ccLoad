package app

import (
	"net/http"
	"strings"

	"ccLoad/internal/protocol"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const stripUnsupportedUsageSemanticsStrategy = "strip_unsupported_usage_semantics"

// Preserve the extension for providers that support it. Only an explicit
// rejection permits removing it from the actual wire body for a same-channel
// retry; the forwarding loop deduplicates this strategy to limit it to once.
func usageSemanticsRetryBodyFor400(
	upstreamProtocol protocol.Protocol,
	plan protocol.TransformPlan,
	res *fwResult,
) ([]byte, string, bool) {
	if res == nil || res.ResponseCommitted || upstreamProtocol != protocol.OpenAI {
		return nil, "", false
	}
	errorBody, status := forwardResultErrorPayload(res)
	if status != http.StatusBadRequest || !matchesUnsupportedUsageSemanticsError(errorBody) {
		return nil, "", false
	}
	body := plan.TranslatedBody
	if !gjson.ValidBytes(body) || !gjson.ParseBytes(body).IsObject() ||
		!gjson.GetBytes(body, "usage_semantics").Exists() {
		return nil, "", false
	}
	retryBody, err := sjson.DeleteBytes(body, "usage_semantics")
	if err != nil {
		return nil, "", false
	}
	return retryBody, stripUnsupportedUsageSemanticsStrategy, true
}

func matchesUnsupportedUsageSemanticsError(body []byte) bool {
	if !gjson.ValidBytes(body) {
		return false
	}
	rejection := gjson.GetBytes(body, "error")
	if !rejection.IsObject() {
		return false
	}
	for _, key := range []string{"code", "param", "message"} {
		value := rejection.Get(key)
		if value.Exists() && value.Type != gjson.Null && value.Type != gjson.String {
			return false
		}
	}
	code := strings.ToLower(strings.TrimSpace(rejection.Get("code").String()))
	switch code {
	case "", "invalid_request", "invalid_request_error", "unknown_parameter", "unsupported_parameter":
	default:
		return false
	}
	param := strings.TrimSpace(rejection.Get("param").String())
	if param != "" && param != "usage_semantics" {
		return false
	}
	if param == "usage_semantics" && (code == "unknown_parameter" || code == "unsupported_parameter") {
		return true
	}
	message := strings.TrimSpace(rejection.Get("message").String())
	for _, prefix := range []string{
		"unrecognized request argument supplied:",
		"unknown parameter:",
		"unsupported parameter:",
	} {
		if !strings.HasPrefix(strings.ToLower(message), prefix) {
			continue
		}
		field := strings.TrimSpace(message[len(prefix):])
		// new-api appends its request ID to the rejection message.
		if before, after, found := strings.Cut(field, " (request id: "); found {
			if !strings.HasSuffix(after, ")") || len(after) <= 1 {
				return false
			}
			field = before
		}
		field = strings.TrimSpace(strings.TrimSuffix(field, "."))
		return field == "usage_semantics" || field == "'usage_semantics'" ||
			field == "\"usage_semantics\"" || field == "`usage_semantics`"
	}
	return false
}
