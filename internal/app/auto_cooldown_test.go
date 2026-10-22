package app

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"ccLoad/internal/cooldown"
	"ccLoad/internal/model"
)

func TestAutoCooldownDisabledPreservesErrorActions(t *testing.T) {
	s := newInMemoryServerWithSettings(t, map[string]string{autoCooldownEnabledSettingKey: "false"})
	ctx := context.Background()
	cfg := addPreviewChannel(t, s, &model.Config{Name: "errors"}, &model.APIKey{})
	for _, tc := range []struct {
		name     string
		input    cooldown.ErrorInput
		fallback bool
		want     cooldown.Action
	}{
		{"auth", cooldown.ErrorInput{StatusCode: 401}, false, cooldown.ActionRetryKey},
		{"model", cooldown.ErrorInput{StatusCode: 503, Model: "test-model"}, false, cooldown.ActionRetryModel},
		{"network", cooldown.ErrorInput{StatusCode: 502, IsNetworkError: true}, false, cooldown.ActionRetryChannel},
		{"key-fallback", cooldown.ErrorInput{StatusCode: 503, Model: "test-model"}, true, cooldown.ActionRetryKey},
		{"client", cooldown.ErrorInput{StatusCode: 406}, false, cooldown.ActionReturnClient},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg.RetryOtherKeysOnFailure = tc.fallback
			tc.input.ChannelID = cfg.ID
			if got := s.applyCooldownDecision(ctx, cfg, tc.input); got != tc.want {
				t.Fatalf("action=%v want=%v", got, tc.want)
			}
		})
	}
	// Provider-specific paths must respect the same switch.
	s.cooldownRejectedOAuthCredential(ctx, cfg, "test")
	s.applyAntigravityModelCapacityCooldown(ctx, cfg, -1, "test-model", &fwResult{Status: 503})
	assertNoStoredAutoCooldowns(t, s)
	s.urlSelector.CooldownURL(cfg.ID, cfg.URLs[0].URL)
	if s.urlSelector.IsCooledDown(cfg.ID, cfg.URLs[0].URL) {
		t.Fatal("URL cooldown was written while disabled")
	}
}

func assertNoStoredAutoCooldowns(t *testing.T, s *Server) {
	t.Helper()
	ctx := context.Background()
	channels, err := s.store.GetAllChannelCooldowns(ctx)
	if err != nil || len(channels) != 0 {
		t.Fatalf("stored channel cooldowns=%v err=%v", channels, err)
	}
	keys, err := s.store.GetAllKeyCooldowns(ctx)
	if err != nil || len(keys) != 0 {
		t.Fatalf("stored key cooldowns=%v err=%v", keys, err)
	}
	models, err := s.store.GetAllModelCooldowns(ctx)
	if err != nil || len(models) != 0 {
		t.Fatalf("stored model cooldowns=%v err=%v", models, err)
	}
}

func TestAutoCooldownExistingStateRoutingAndAdmin(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		t.Run(strconv.FormatBool(enabled), func(t *testing.T) {
			s := newInMemoryServerWithSettings(t, map[string]string{
				autoCooldownEnabledSettingKey: strconv.FormatBool(enabled),
				"cooldown_fallback_enabled":   "false",
			})
			ctx := context.Background()
			cfg := addPreviewChannel(t, s, &model.Config{Name: "old-cooldowns"}, &model.APIKey{})
			until := time.Now().Add(time.Hour).Truncate(time.Second)
			for _, err := range []error{
				s.store.SetChannelCooldown(ctx, cfg.ID, until),
				s.store.SetKeyCooldown(ctx, cfg.ID, 0, until),
				s.store.SetModelCooldown(ctx, cfg.ID, "test-model", until),
			} {
				if err != nil {
					t.Fatal(err)
				}
			}
			s.invalidateChannelRelatedCache(cfg.ID)
			preview := previewRoute(t, s, map[string]any{"client_protocol": "openai", "model": "test-model"})
			wantCandidates := 1
			if enabled {
				wantCandidates = 0
			}
			if len(preview.Candidates) != wantCandidates {
				t.Fatalf("preview=%+v", preview)
			}

			keys, err := s.getAPIKeysSnapshot(ctx, cfg.ID)
			if err != nil {
				t.Fatal(err)
			}
			_, _, keyErr := s.keySelector.SelectAvailableKey(cfg.ID, keys, nil)
			_, rowErr := s.selectChannelModelRow(ctx, cfg, "test-model", "openai", nil, keys)
			if (keyErr != nil) != enabled || (rowErr != nil) != enabled {
				t.Fatalf("enabled=%v keyErr=%v rowErr=%v", enabled, keyErr, rowErr)
			}
			c, w := newTestContext(t, newRequest(http.MethodGet, "/admin/channels", nil))
			s.handleListChannels(c)
			response := mustParseAPIResponse[[]ChannelWithCooldown](t, w.Body.Bytes())
			if len(response.Data) != 1 {
				t.Fatalf("list=%s", w.Body.String())
			}
			view := response.Data[0]
			if (view.CooldownRemainingMS > 0) != enabled || (len(view.ModelCooldowns) > 0) != enabled ||
				len(view.KeyCooldowns) != 1 || (view.KeyCooldowns[0].CooldownRemainingMS > 0) != enabled {
				t.Fatalf("enabled=%v list=%s", enabled, w.Body.String())
			}
			// Neither persisted state nor the shared cache may be cleared by reads.
			stored, err := s.store.GetAPIKeys(ctx, cfg.ID)
			if err != nil || stored[0].CooldownUntil != until.Unix() {
				t.Fatalf("stored=%v err=%v", stored, err)
			}
			cached, err := s.getChannelCache().GetAPIKeysSnapshot(ctx, cfg.ID)
			if err != nil || cached[0].CooldownUntil != until.Unix() {
				t.Fatalf("cached=%v err=%v", cached, err)
			}
			c, w = newTestContext(t, newRequest(http.MethodGet, "/admin/channels?status=cooldown", nil))
			s.handleListChannels(c)
			filtered := mustParseAPIResponse[[]ChannelWithCooldown](t, w.Body.Bytes())
			if (len(filtered.Data) > 0) != enabled {
				t.Fatalf("cooldown filter=%s", w.Body.String())
			}
		})
	}
}

func TestAutoCooldownDisabledPreservesKeyRestrictions(t *testing.T) {
	s := newInMemoryServerWithSettings(t, map[string]string{autoCooldownEnabledSettingKey: "false"})
	addPreviewChannel(t, s, &model.Config{Name: "disabled-key"}, &model.APIKey{Disabled: true})
	addPreviewChannel(t, s, &model.Config{Name: "wrong-model"}, &model.APIKey{AllowedModels: []string{"other-model"}})
	addPreviewChannel(t, s, &model.Config{Name: "disabled-model", ModelEntries: []model.ModelEntry{{Model: "test-model", Disabled: true}}}, &model.APIKey{})
	preview := previewRoute(t, s, map[string]any{"client_protocol": "openai", "model": "test-model"})
	if len(preview.Candidates) != 0 {
		t.Fatalf("restrictions bypassed: %+v", preview)
	}
}

func TestProxy_AutoCooldownDisabledRetriesEveryRequest(t *testing.T) {
	for _, retryOtherKeys := range []bool{false, true} {
		for _, status := range []int{401, 403, 429, 503} {
			t.Run(strconv.Itoa(status)+"/key-fallback="+strconv.FormatBool(retryOtherKeys), func(t *testing.T) {
				var primaryCalls, fallbackCalls atomic.Int64
				primary := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					primaryCalls.Add(1)
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(status)
					_, _ = io.WriteString(w, `{"error":{"message":"upstream failed"}}`)
				}))
				defer primary.Close()
				fallback := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					fallbackCalls.Add(1)
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"id":"chat-1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
				}))
				defer fallback.Close()
				env := setupProxyTestEnvWithSettings(t, []testChannel{
					{name: "primary", upstreamProtocol: "openai", models: "test-model", priority: 100, retryOtherKeysOnFailure: retryOtherKeys},
					{name: "fallback", upstreamProtocol: "openai", models: "test-model", priority: 50},
				}, map[int]string{0: primary.URL, 1: fallback.URL}, map[string]string{
					autoCooldownEnabledSettingKey: "false", "cooldown_fallback_enabled": "false",
				})
				if err := env.store.CreateAPIKeysBatch(context.Background(), []*model.APIKey{{ChannelID: 1, KeyIndex: 1, APIKey: "sk-second"}}); err != nil {
					t.Fatal(err)
				}
				env.server.invalidateChannelRelatedCache(1)
				for range 2 {
					response := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
						"model": "test-model", "messages": []any{map[string]any{"role": "user", "content": "hi"}},
					}, nil)
					if response.Code != http.StatusOK {
						t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
					}
				}
				wantPrimary := int64(4)
				if !retryOtherKeys && (status == 429 || status == 503) {
					wantPrimary = 2
				}
				if primaryCalls.Load() != wantPrimary || fallbackCalls.Load() != 2 {
					t.Fatalf("primary calls=%d fallback calls=%d; want %d and 2", primaryCalls.Load(), fallbackCalls.Load(), wantPrimary)
				}
				assertNoStoredAutoCooldowns(t, env.server)
			})
		}
	}
}

func TestAutoCooldownSettingDefaultsAndPersistence(t *testing.T) {
	s := newInMemoryServer(t)
	setting, err := s.configService.GetSettingFresh(context.Background(), autoCooldownEnabledSettingKey)
	if err != nil || setting.Value != "true" || setting.ValueType != "bool" || !s.autoCooldownEnabled() {
		t.Fatalf("default setting=%+v err=%v", setting, err)
	}
	updates := map[string]string{autoCooldownEnabledSettingKey: "false"}
	restart, err := s.commitSettingUpdates(updates, func() error { return s.configService.BatchUpdateSettings(context.Background(), updates) })
	if err != nil || !restart || !s.autoCooldownEnabled() {
		t.Fatalf("restart=%v enabled=%v err=%v", restart, s.autoCooldownEnabled(), err)
	}
	cs := NewConfigService(s.store)
	if err := cs.LoadDefaults(context.Background()); err != nil {
		t.Fatal(err)
	}
	if (&Server{configService: cs}).autoCooldownEnabled() {
		t.Fatal("saved disabled setting did not survive reload")
	}
}

func TestURLSelectorAutoCooldownDisabledPreservesManualDisablement(t *testing.T) {
	s := NewURLSelector()
	urls := []string{"https://fast.test", "https://slow.test", "https://disabled.test"}
	for i, url := range urls {
		s.RecordLatency(1, url, time.Duration(i+1)*time.Second)
	}
	s.CooldownURL(1, urls[0])
	s.DisableURL(1, urls[2])
	if got := s.SortURLs(1, urls); got[0].url != urls[1] {
		t.Fatalf("enabled order=%v", got)
	}
	s.SetAutoCooldownEnabled(false)
	s.CooldownURL(1, urls[1])
	if got := s.SortURLs(1, urls); len(got) != 2 || got[0].url != urls[0] {
		t.Fatalf("disabled order=%v", got)
	}
	if got, _ := s.SelectURL(1, []string{urls[0], urls[2]}); got != urls[0] {
		t.Fatalf("selected=%s", got)
	}
	for _, stat := range s.GetURLStats(1, urls) {
		if stat.CooledDown || stat.CooldownRemainMs != 0 {
			t.Fatalf("stale cooldown=%+v", stat)
		}
	}
	s.SetAutoCooldownEnabled(true)
	if !s.IsCooledDown(1, urls[0]) || s.IsCooledDown(1, urls[1]) {
		t.Fatal("old cooldown lost or new cooldown written while disabled")
	}
}

func TestProxy_AutoCooldownDisabledAntigravityRateLimit(t *testing.T) {
	var calls atomic.Int64
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"RATE_LIMIT_EXCEEDED"},{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"30s"}]}}`)
	}))
	defer upstream.Close()
	env := setupProxyTestEnvWithSettings(t, []testChannel{{
		name: "rate", upstreamProtocol: "gemini", models: "claude-sonnet-4-6",
		authType: model.AuthTypeAntigravityOAuth, oauthCredential: antigravityProxyTestCredential(t, "rate-token"),
	}}, map[int]string{0: upstream.URL}, map[string]string{autoCooldownEnabledSettingKey: "false", "cooldown_fallback_enabled": "false"})
	for range 2 {
		response := doProxyRequest(t, env.engine, "/v1beta/models/claude-sonnet-4-6:generateContent", map[string]any{
			"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": "hello"}}}},
		}, nil)
		if response.Code != http.StatusTooManyRequests {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("calls=%d want=2", calls.Load())
	}
	assertNoStoredAutoCooldowns(t, env.server)
}
