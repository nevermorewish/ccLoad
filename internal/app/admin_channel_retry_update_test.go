package app

import (
	"context"
	"net/http"
	"testing"

	"ccLoad/internal/model"
)

func TestChannelUpdatePreservesOmittedSameChannelRetries(t *testing.T) {
	for _, authType := range []string{model.AuthTypeAPIKey, model.AuthTypeSystemOne} {
		for _, scenario := range []struct {
			name      string
			submitted bool
			value     int
			want      int
		}{
			{name: "older editor omits field", want: 3},
			{name: "explicit zero disables retries", submitted: true, value: 0, want: 0},
			{name: "explicit value updates retries", submitted: true, value: 5, want: 5},
		} {
			t.Run(authType+"/"+scenario.name, func(t *testing.T) {
				server, store, cleanup := setupAdminTestServer(t)
				defer cleanup()
				ctx := context.Background()
				cfg, err := store.CreateConfig(ctx, &model.Config{
					Name: "before", AuthType: authType, Enabled: true, SameChannelRetries: 3,
					URLs:         model.ChannelURLs{{URL: "https://example.invalid"}},
					ModelEntries: []model.ModelEntry{{Model: "test-model"}},
				})
				if err != nil {
					t.Fatal(err)
				}
				keys := []*model.APIKey{
					{ChannelID: cfg.ID, KeyIndex: 0, APIKey: "fixture-a", CostMultiplier: 1},
					{ChannelID: cfg.ID, KeyIndex: 1, APIKey: "fixture-b", CostMultiplier: 2},
				}
				if err := store.CreateAPIKeysBatch(ctx, keys); err != nil {
					t.Fatal(err)
				}
				payload := map[string]any{
					"name": "renamed", "auth_type": authType, "enabled": true,
					"urls": cfg.URLs, "models": cfg.ModelEntries,
					"api_keys": []map[string]any{{"api_key": "fixture-a", "cost_multiplier": 1}, {"api_key": "fixture-b", "cost_multiplier": 2}},
				}
				if scenario.submitted {
					payload["same_channel_retries"] = scenario.value
				}
				c, w := newTestContext(t, newJSONRequest(t, http.MethodPut, "/admin/channels/1", payload))
				server.handleUpdateChannel(c, cfg.ID)
				if w.Code != http.StatusOK {
					t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
				}
				updated, err := store.GetConfig(ctx, cfg.ID)
				if err != nil {
					t.Fatal(err)
				}
				if updated.Name != "renamed" || updated.SameChannelRetries != scenario.want {
					t.Fatalf("name=%q retries=%d, want renamed/%d", updated.Name, updated.SameChannelRetries, scenario.want)
				}
				storedKeys, err := store.GetAPIKeys(ctx, cfg.ID)
				if err != nil {
					t.Fatal(err)
				}
				if len(storedKeys) != 2 || storedKeys[1].APIKey != "fixture-b" || storedKeys[1].CostMultiplier != 2 {
					t.Fatalf("keys changed: %+v", storedKeys)
				}
			})
		}
	}
}
