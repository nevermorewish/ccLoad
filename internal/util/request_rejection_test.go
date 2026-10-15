package util

import "testing"

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
