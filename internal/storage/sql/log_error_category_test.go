package sql_test

import (
	"context"
	"testing"
	"time"

	"ccLoad/internal/model"
)

func TestLogRateLimitCategoryFilter(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	ctx := context.Background()
	now := time.Now()
	cases := []struct {
		status            int
		message, category string
	}{
		{429, "status_code=429, Responses WebSocket transcript budget exceeded (current: 276892730 bytes, limit: 268435456 bytes).", "ws_transcript"},
		{429, "RESPONSES_WS_MAX_TRANSCRIPT_BYTES quota exceeded", "ws_transcript"},
		{429, "Responses WebSocket execution session limit reached", "ws_sessions"},
		{429, "responses_websocket_connection_limit_exceeded", "ws_connections"},
		{429, "token_rpm_exceeded: too many requests", "token_rpm"},
		{429, `{"error":{"code":"insufficient_quota"}}`, "quota"},
		{429, "rate_limit_exceeded", "other"},
		{429, "responsesXwsXmaxXtranscriptXbytes", "other"}, // SQL LIKE must escape underscores.
		{503, "transcript budget exceeded", ""},
		{200, "ok", ""},
	}
	for _, tc := range cases {
		if err := store.AddLog(ctx, &model.LogEntry{Time: newJSONTime(now), StatusCode: tc.status, Message: tc.message, AuthTokenID: 42}); err != nil {
			t.Fatal(err)
		}
	}
	for _, category := range []string{"ws_transcript", "ws_sessions", "ws_connections", "token_rpm", "quota", "other"} {
		t.Run(category, func(t *testing.T) {
			want := 0
			for _, tc := range cases {
				if tc.category == category {
					want++
				}
			}
			filter := &model.LogFilter{ErrorCategory: category}
			rows, total, err := store.ListLogsRangeWithCount(ctx, now.Add(-time.Minute), now.Add(time.Minute), 1, 0, filter)
			if err != nil {
				t.Fatal(err)
			}
			if total != want || len(rows) != 1 {
				t.Fatalf("total=%d rows=%d want total=%d", total, len(rows), want)
			}
			if rows[0].ErrorCategory != category || rows[0].StatusCode != 429 {
				t.Fatalf("wrong category: %+v", rows[0])
			}
			count, err := store.CountLogs(ctx, now.Add(-time.Minute), filter)
			if err != nil || count != want {
				t.Fatalf("count=%d err=%v", count, err)
			}
			otherToken := int64(43)
			filter.AuthTokenID = &otherToken
			count, err = store.CountLogs(ctx, now.Add(-time.Minute), filter)
			if err != nil || count != 0 {
				t.Fatalf("token scope leaked: count=%d err=%v", count, err)
			}
		})
	}
}
