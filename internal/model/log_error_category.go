package model

import "strings"

// RateLimitCategoryRules are ordered: specific resource limits precede generic quota errors.
// Shared by display classification and SQL filtering so historical logs need no migration.
func RateLimitCategoryRules() []struct {
	Category string
	Patterns []string
} {
	return []struct {
		Category string
		Patterns []string
	}{
		{"ws_transcript", []string{"transcript budget exceeded", "responses_ws_max_transcript_bytes"}},
		{"ws_sessions", []string{"execution session limit reached", "responses_ws_max_sessions"}},
		{"ws_connections", []string{"responses_websocket_connection_limit_exceeded", "responses_ws_max_connections"}},
		{"token_rpm", []string{"token_rpm_exceeded", "token rpm limit exceeded"}},
		{"quota", []string{"insufficient_quota", "usage_limit_reached", "quota exceeded", "exceeded your current quota", "credit balance is too low", "额度不足", "余额不足"}},
	}
}

// ValidRateLimitCategory reports whether the category is supported by log filtering.
func ValidRateLimitCategory(category string) bool {
	if category == "other" {
		return true
	}
	for _, rule := range RateLimitCategoryRules() {
		if rule.Category == category {
			return true
		}
	}
	return false
}

// LogErrorCategory classifies rate-limit failures using the shared display and filter rules.
func LogErrorCategory(status int, message string) string {
	if status != 429 {
		return ""
	}
	message = strings.ToLower(message)
	for _, rule := range RateLimitCategoryRules() {
		for _, pattern := range rule.Patterns {
			if strings.Contains(message, pattern) {
				return rule.Category
			}
		}
	}
	return "other"
}
