package util

import "testing"

func TestIsSamplingParameterRejection(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       bool
	}{
		{"temperature", `{"error":{"code":"unsupported_parameter","param":"temperature"}}`, true},
		{"top_p", `{"response":{"error":{"code":"unsupported_value","param":"top_p"}}}`, true},
		{"message_only", `{"error":{"message":"Unsupported parameter: 'temperature'"}}`, true},
		{"mixed", `{"error":{"code":"unsupported_parameter","param":"temperature"},"response":{"error":{"code":"unsupported_parameter","param":"reasoning"}}}`, false},
		{"policy", `{"error":{"code":"content_policy_violation","param":"temperature"}}`, false},
		{"cache", `{"error":{"code":"unsupported_parameter","param":"prompt_cache_options"}}`, false},
		{"ambiguous", `{"error":{"code":"unsupported_value","message":"Unsupported value: 'x' is not supported with the 'model'"}}`, false},
		{"invalid_json", `not json`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsSamplingParameterRejection([]byte(tc.body)); got != tc.want {
				t.Fatalf("got %t want %t", got, tc.want)
			}
		})
	}
}

func TestClassifyRequestRejection(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want RequestRejection
	}{
		{`{"error":{"code":"unsupported_parameter","param":"reasoning","message":"reasoning unsupported"}}`, RequestRejectionNativeOnly},
		{`{"response":{"error":{"code":"unsupported_value","param":"reasoning.effort","message":"minimal unsupported"}}}`, RequestRejectionNativeOnly},
		{`{"error":{"code":"content_policy_violation","message":"policy"}}`, RequestRejectionFinal},
		{`{"error":{"message":"Consecutive 'configuration_update' items are not allowed"}}`, RequestRejectionFinal},
		{`{"error":{"code":"previous_response_not_found","message":"not found"}}`, RequestRejectionNativeOnly},
		{`{"error":{"code":"unknown_parameter","param":"input[0].status"}}`, RequestRejectionNone},
		{`{"error":{"code":"rate_limit_exceeded","message":"rate limit"}}`, RequestRejectionNone},
	} {
		if got := ClassifyRequestRejection([]byte(tc.raw)); got != tc.want {
			t.Errorf("%s = %v want %v", tc.raw, got, tc.want)
		}
	}
}
