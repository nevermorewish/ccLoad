package cooldown

import (
	"context"
	"testing"
	"time"

	"ccLoad/internal/model"
)

func TestAutoCooldownDisabledSkipsConfiguredRuleWrites(t *testing.T) {
	for _, tc := range []struct {
		scope string
		want  Action
	}{
		{model.CooldownScopeKey, ActionRetryKey},
		{model.CooldownScopeModel, ActionRetryModel},
		{model.CooldownScopeChannel, ActionRetryChannel},
	} {
		t.Run(tc.scope, func(t *testing.T) {
			// A nil store ensures neither explicit deadlines nor resource
			// exhaustion checks touch persistence when disabled.
			manager := NewManager(nil, nil)
			manager.SetAutoCooldownEnabled(false)
			input := ErrorInput{
				ChannelID: 1, KeyIndex: 0, Model: "test-model", StatusCode: 406,
				CooldownDetectionRules: &model.CooldownDetectionRules{Rules: []model.CooldownDetectionRule{{
					Enabled: true, Name: "configured", StatusCodes: []int{406}, Scope: tc.scope,
					Mode: model.CooldownModeFixed, CooldownSeconds: 600,
				}}},
			}
			if got := manager.HandleError(context.Background(), input); got != tc.want {
				t.Fatalf("action=%v want=%v", got, tc.want)
			}
			wantFallback := tc.want
			if manager.CanFallbackToOtherKey(input) {
				wantFallback = ActionRetryKey
			}
			if got := manager.HandleErrorWithKeyFallback(context.Background(), input); got != wantFallback {
				t.Fatalf("fallback action=%v want=%v", got, wantFallback)
			}
		})
	}
}

func TestAutoCooldownReenabledRestoresUnexpiredState(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	manager := NewManager(store, nil)
	cfg := createTestChannel(t, store, "reenabled")
	ctx := context.Background()
	input := ErrorInput{ChannelID: cfg.ID, KeyIndex: NoKeyIndex, StatusCode: 502, IsNetworkError: true}
	manager.HandleError(ctx, input)
	before, err := store.GetConfig(ctx, cfg.ID)
	if err != nil || !before.IsCoolingDown(time.Now()) {
		t.Fatalf("initial cooldown=%+v err=%v", before, err)
	}
	manager.SetAutoCooldownEnabled(false)
	manager.HandleError(ctx, input)
	unchanged, err := store.GetConfig(ctx, cfg.ID)
	if err != nil || unchanged.CooldownUntil != before.CooldownUntil || unchanged.CooldownDurationMs != before.CooldownDurationMs {
		t.Fatalf("disabled policy changed existing cooldown: %+v err=%v", unchanged, err)
	}
	manager.SetAutoCooldownEnabled(true)
	manager.HandleError(ctx, input)
	after, err := store.GetConfig(ctx, cfg.ID)
	if err != nil || after.CooldownDurationMs <= before.CooldownDurationMs {
		t.Fatalf("reenabled cooldown=%+v err=%v", after, err)
	}
}
