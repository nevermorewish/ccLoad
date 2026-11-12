package app

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"ccLoad/internal/model"
)

func putChannelRoutes(t *testing.T, server *Server, payload any) (int, channelRoutesView, string) {
	t.Helper()
	c, w := newTestContext(t, newJSONRequest(t, http.MethodPut, "/admin/channel-routes", payload))
	server.HandleUpdateChannelRoutes(c)
	var view channelRoutesView
	if w.Code == http.StatusOK {
		mustUnmarshalAPIResponseData(t, w.Body.Bytes(), &view)
	}
	return w.Code, view, w.Body.String()
}

func simulateChannelRoute(t *testing.T, server *Server, payload map[string]any) (int, channelRouteSimulation) {
	t.Helper()
	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channel-routes/simulate", payload))
	server.HandleSimulateChannelRoute(c)
	var result channelRouteSimulation
	if w.Code == http.StatusOK {
		mustUnmarshalAPIResponseData(t, w.Body.Bytes(), &result)
	}
	return w.Code, result
}

func simulatedPools(result channelRouteSimulation) []int {
	pools := make([]int, len(result.Pools))
	for i, pool := range result.Pools {
		pools[i] = pool.Pool
	}
	return pools
}

func TestAdminChannelRouteTimeoutTarget(t *testing.T) {
	t.Parallel()
	server := newInMemoryServer(t)
	target := addPoolChannel(t, server, "final", 2, 10, "kimi-k3")
	rule := ChannelRouteRule{ID: "timeout", Enabled: true, Models: []string{"kimi-k3"}, FirstByteTimeoutSeconds: 20, FirstByteTimeoutChannelID: target.ID}
	status, view, body := putChannelRoutes(t, server, ChannelRouteConfig{Rules: []ChannelRouteRule{rule}})
	if status != http.StatusOK || len(view.Channels) != 1 || view.Config.Rules[0].FirstByteTimeoutChannelID != target.ID {
		t.Fatalf("save status=%d body=%s", status, body)
	}
	status, simulation := simulateChannelRoute(t, server, map[string]any{"model": "kimi-k3"})
	if status != http.StatusOK || simulation.FirstByteTimeoutChannelID != target.ID {
		t.Fatalf("simulation=%+v status=%d", simulation, status)
	}
	for _, invalid := range []ChannelRouteRule{
		{Models: []string{"kimi-k3"}, FirstByteTimeoutSeconds: 20, FirstByteTimeoutChannelID: -1},
		{Models: []string{"kimi-k3"}, FirstByteTimeoutChannelID: target.ID},
		{Models: []string{"kimi-k3"}, FirstByteTimeoutSeconds: 20, FirstByteTimeoutChannelID: target.ID + 100},
	} {
		status, _, body := putChannelRoutes(t, server, ChannelRouteConfig{Rules: []ChannelRouteRule{invalid}})
		if status != http.StatusBadRequest {
			t.Fatalf("invalid target accepted: status=%d body=%s", status, body)
		}
	}
}

func TestAdminChannelRoutesSaveGetAndSimulate(t *testing.T) {
	server := newInMemoryServer(t)
	addPoolChannel(t, server, "main", 0, 10, "kimi-k3")
	addPoolChannel(t, server, "pool1", 1, 10, "kimi-k3")
	disabled := addPoolChannel(t, server, "pool2", 2, 10, "kimi-k2")
	if _, err := server.store.UpdateChannelEnabled(context.Background(), disabled.ID, false); err != nil {
		t.Fatal(err)
	}

	status, view, body := putChannelRoutes(t, server, map[string]any{"rules": []any{map[string]any{
		"name": "kimi", "enabled": true, "models": []string{"kimi-k3"}, "default_pool": 0, "sticky": "tier",
		"token_tiers":                []any{map[string]any{"max_tokens": 8000, "pool": 1}, map[string]any{"min_tokens": 50000, "pool": 2}},
		"overflow":                   []any{map[string]any{"pool": 1, "max_queue": 100, "target_pool": 2}},
		"first_byte_timeout_seconds": 15,
	}}})
	if status != http.StatusOK {
		t.Fatalf("PUT status=%d body=%s", status, body)
	}
	if len(view.Config.Rules) != 1 || !validChannelRouteRuleID(view.Config.Rules[0].ID) || view.Config.Rules[0].Sticky != channelRouteStickyTier ||
		view.Config.Rules[0].FirstByteTimeoutSeconds != 15 {
		t.Fatalf("saved config=%+v", view.Config)
	}
	ruleID := view.Config.Rules[0].ID
	if len(view.Pools) != model.ChannelPoolCount || view.Pools[0].Name != "主渠道" || view.Pools[1].Channels != 1 ||
		view.Pools[2].Channels != 1 || view.Pools[2].Enabled != 0 || !slices.Equal(view.Pools[1].Models, []string{"kimi-k3"}) || len(view.Pools[2].Models) != 0 {
		t.Fatalf("pool summary=%+v", view.Pools)
	}

	// 持久化值与运行态一致，重新解析得到同一份规则。
	setting, err := server.store.GetSetting(context.Background(), channelRouteRulesSettingKey)
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := parseChannelRouteConfig(setting.Value)
	if err != nil || len(persisted.Rules) != 1 || !channelRouteRuleEqual(persisted.Rules[0], view.Config.Rules[0]) {
		t.Fatalf("persisted=%+v err=%v", persisted, err)
	}
	if loaded := loadChannelRouteConfig(newTestConfigService(t, map[string]string{channelRouteRulesSettingKey: setting.Value})); len(loaded.Rules) != 1 || loaded.Rules[0].ID != ruleID {
		t.Fatalf("startup load=%+v", loaded)
	}

	// 非法配置整体拒绝，运行态保持原值。
	status, _, body = putChannelRoutes(t, server, map[string]any{"rules": []any{map[string]any{"enabled": true, "models": []string{"x"}, "default_pool": 5}}})
	if status != http.StatusBadRequest || !strings.Contains(body, "default_pool") {
		t.Fatalf("invalid PUT status=%d body=%s", status, body)
	}
	if rules := server.channelRouteConfig().Rules; len(rules) != 1 || rules[0].ID != ruleID {
		t.Fatalf("invalid PUT replaced runtime rules: %+v", rules)
	}

	status, result := simulateChannelRoute(t, server, map[string]any{"model": "kimi-k3(high)", "input_tokens": 100})
	if status != http.StatusOK || !result.Matched || result.RuleID != ruleID || result.Reason != channelRouteReasonTokenTier ||
		!slices.Equal(simulatedPools(result), []int{1, 0, 2}) || result.Pools[0].Name != "渠道1" || result.FirstByteTimeoutSeconds != 15 {
		t.Fatalf("simulate short=%d %+v", status, result)
	}
	server.channelPoolQueues.add(channelPoolQueueKey{ruleID: ruleID, pool: 1}, 100)
	status, result = simulateChannelRoute(t, server, map[string]any{"model": "kimi-k3", "input_tokens": 100})
	if status != http.StatusOK || result.Reason != channelRouteReasonOverflow || !slices.Equal(simulatedPools(result), []int{2, 1, 0}) || result.Pools[1].Inflight != 100 {
		t.Fatalf("simulate overflow=%d %+v", status, result)
	}
	body60k, _ := json.Marshal(map[string]any{"messages": []any{map[string]any{"role": "user", "content": largePrompt}}})
	status, result = simulateChannelRoute(t, server, map[string]any{"model": "kimi-k3", "body": json.RawMessage(body60k)})
	if status != http.StatusOK || result.Tier != 1 || result.InputTokens < 50000 || simulatedPools(result)[0] != 2 {
		t.Fatalf("simulate body=%d %+v", status, result)
	}
	status, result = simulateChannelRoute(t, server, map[string]any{"model": "glm-5", "input_tokens": 9})
	if status != http.StatusOK || result.Matched || result.InputTokens != 9 || !slices.Equal(simulatedPools(result), []int{0}) ||
		result.FirstByteTimeoutSeconds != 0 {
		t.Fatalf("simulate unmatched=%d %+v", status, result)
	}
	for _, invalid := range []map[string]any{{"model": ""}, {"model": "*"}, {"model": "kimi-k3", "input_tokens": -1}} {
		if status, _ := simulateChannelRoute(t, server, invalid); status != http.StatusBadRequest {
			t.Fatalf("simulate(%v) status=%d", invalid, status)
		}
	}

	c, w := newTestContext(t, newRequest(http.MethodGet, "/admin/channel-routes", nil))
	server.HandleGetChannelRoutes(c)
	if w.Code != http.StatusOK {
		t.Fatalf("GET status=%d", w.Code)
	}
	mustUnmarshalAPIResponseData(t, w.Body.Bytes(), &view)
	if view.Pools[1].Inflight != 100 || len(view.Inflight) != 1 || view.Inflight[0] != (channelRouteInflightView{RuleID: ruleID, Pool: 1, Inflight: 100}) {
		t.Fatalf("GET inflight pools=%+v inflight=%+v", view.Pools, view.Inflight)
	}

	// 清空规则后恢复默认顺序。
	if status, view, body := putChannelRoutes(t, server, map[string]any{"rules": []any{}}); status != http.StatusOK || len(view.Config.Rules) != 0 {
		t.Fatalf("clear status=%d body=%s", status, body)
	}
	if route := decidePools(server, "kimi-k3", 100, ""); route.ruleID != "" {
		t.Fatalf("cleared rules still route: %+v", route)
	}
}

func TestAdminSettingsHideChannelRoutes(t *testing.T) {
	server := newInMemoryServer(t)
	c, w := newTestContext(t, newRequest(http.MethodGet, "/admin/settings", nil))
	server.AdminListSettings(c)
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), channelRouteRulesSettingKey) {
		t.Fatalf("settings list exposes %s: %s", channelRouteRulesSettingKey, w.Body.String())
	}

	c, w = newTestContext(t, newRequest(http.MethodGet, "/admin/settings/"+channelRouteRulesSettingKey, nil))
	c.Params = gin.Params{{Key: "key", Value: channelRouteRulesSettingKey}}
	server.AdminGetSetting(c)
	if w.Code != http.StatusNotFound {
		t.Fatalf("get hidden setting status=%d", w.Code)
	}

	c, w = newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/settings/batch", map[string]string{channelRouteRulesSettingKey: `{"rules":[]}`}))
	server.AdminBatchUpdateSettings(c)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "/admin/channel-routes") {
		t.Fatalf("batch update status=%d body=%s", w.Code, w.Body.String())
	}
}

func listChannelsInPool(t *testing.T, server *Server, pool string) []string {
	t.Helper()
	c, w := newTestContext(t, newRequest(http.MethodGet, "/admin/channels?channel_pool="+pool, nil))
	server.HandleChannels(c)
	if w.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", w.Code, w.Body.String())
	}
	var channels []ChannelWithCooldown
	mustUnmarshalAPIResponseData(t, w.Body.Bytes(), &channels)
	names := make([]string, len(channels))
	for i, channel := range channels {
		names[i] = channel.Name
	}
	slices.Sort(names)
	return names
}

func TestAdminChannelPoolEditing(t *testing.T) {
	server := newInMemoryServer(t)
	create := func(name string, pool int) (int, *model.Config) {
		t.Helper()
		c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels", map[string]any{
			"name": name, "urls": model.ChannelURLs{{URL: "https://api.example.com"}}, "api_key": "sk-" + name,
			"models": []model.ModelEntry{{Model: "kimi-k3"}}, "enabled": true, "channel_pool": pool,
		}))
		server.handleCreateChannel(c)
		if w.Code != http.StatusCreated {
			return w.Code, nil
		}
		var created model.Config
		mustUnmarshalAPIResponseData(t, w.Body.Bytes(), &created)
		return w.Code, &created
	}
	status, first := create("pool1-a", 1)
	if status != http.StatusCreated || first.ChannelPool != 1 {
		t.Fatalf("create status=%d channel=%+v", status, first)
	}
	if status, _ := create("pool-invalid", 3); status != http.StatusBadRequest {
		t.Fatalf("invalid pool create status=%d", status)
	}
	_, second := create("main-a", 0)

	update := func(id int64, payload map[string]any) *model.Config {
		t.Helper()
		c, w := newTestContext(t, newJSONRequest(t, http.MethodPut, "/admin/channels/"+strconv.FormatInt(id, 10), payload))
		c.Params = gin.Params{{Key: "id", Value: strconv.FormatInt(id, 10)}}
		server.HandleChannelByID(c)
		if w.Code != http.StatusOK {
			t.Fatalf("update status=%d body=%s", w.Code, w.Body.String())
		}
		var updated model.Config
		mustUnmarshalAPIResponseData(t, w.Body.Bytes(), &updated)
		return &updated
	}
	base := map[string]any{
		"name": "pool1-a", "urls": model.ChannelURLs{{URL: "https://api.example.com"}}, "api_key": "sk-pool1-a",
		"models": []model.ModelEntry{{Model: "kimi-k3"}}, "enabled": true,
	}
	if updated := update(first.ID, base); updated.ChannelPool != 1 {
		t.Fatalf("update without channel_pool moved channel to %d", updated.ChannelPool)
	}
	base["channel_pool"] = 2
	if updated := update(first.ID, base); updated.ChannelPool != 2 {
		t.Fatalf("update channel_pool=%d, want 2", updated.ChannelPool)
	}
	if !slices.Equal(listChannelsInPool(t, server, "2"), []string{"pool1-a"}) || !slices.Equal(listChannelsInPool(t, server, "0"), []string{"main-a"}) ||
		len(listChannelsInPool(t, server, "1")) != 0 || len(listChannelsInPool(t, server, "all")) != 2 || len(listChannelsInPool(t, server, "x")) != 0 {
		t.Fatal("channel_pool list filter mismatch")
	}

	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/batch-advanced", map[string]any{
		"channel_ids": []int64{first.ID, second.ID}, "channel_pool": 1,
	}))
	server.HandleBatchPatchChannels(c)
	if w.Code != http.StatusOK {
		t.Fatalf("batch move status=%d body=%s", w.Code, w.Body.String())
	}
	if !slices.Equal(listChannelsInPool(t, server, "1"), []string{"main-a", "pool1-a"}) {
		t.Fatalf("batch move result=%v", listChannelsInPool(t, server, "1"))
	}
	c, w = newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/batch-advanced", map[string]any{
		"channel_ids": []int64{first.ID}, "channel_pool": 9,
	}))
	server.HandleBatchPatchChannels(c)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("invalid batch pool status=%d", w.Code)
	}

	c, w = newTestContext(t, newRequest(http.MethodGet, "/admin/channels/filter-options?channel_pool=2", nil))
	server.HandleChannelsFilterOptions(c)
	var options channelFilterOptionsResponse
	mustUnmarshalAPIResponseData(t, w.Body.Bytes(), &options)
	if w.Code != http.StatusOK || len(options.ChannelNames) != 0 {
		t.Fatalf("filter options for empty pool=%+v", options)
	}
}

func TestAdminChannelImportPreservesOrAssignsPool(t *testing.T) {
	server := newInMemoryServer(t)
	existing := addPoolChannel(t, server, "existing", 2, 10, "kimi-k3")

	// 旧备份不含 channel_pool：已有渠道保留原组，新渠道归入导入入口指定的组。
	doc := map[string]any{"version": 1, "channels": []any{
		map[string]any{"config": map[string]any{"name": "existing", "auth_type": "api_key", "urls": []any{map[string]any{"url": "https://api.example.com"}},
			"models": []any{map[string]any{"model": "kimi-k3"}}, "enabled": true}, "api_keys": []any{map[string]any{"api_key": "sk-a"}}},
		map[string]any{"config": map[string]any{"name": "fresh", "auth_type": "api_key", "urls": []any{map[string]any{"url": "https://api.example.com"}},
			"models": []any{map[string]any{"model": "kimi-k3"}}, "enabled": true}, "api_keys": []any{map[string]any{"api_key": "sk-b"}}},
	}}
	data, _ := json.Marshal(doc)
	status, body := importJSONToPool(t, server, data, "1")
	if status != http.StatusOK {
		t.Fatalf("JSON import status=%d body=%s", status, body)
	}
	pools := channelPoolsByName(t, server)
	if pools["existing"] != 2 || pools["fresh"] != 1 {
		t.Fatalf("JSON import pools=%v", pools)
	}

	// 声明了 channel_pool 时以文件为准；非法值整体拒绝。
	doc["channels"].([]any)[0].(map[string]any)["config"].(map[string]any)["channel_pool"] = 0
	data, _ = json.Marshal(doc)
	if status, body := importJSONToPool(t, server, data, ""); status != http.StatusOK || channelPoolsByName(t, server)["existing"] != 0 {
		t.Fatalf("declared pool import status=%d body=%s pools=%v", status, body, channelPoolsByName(t, server))
	}
	doc["channels"].([]any)[0].(map[string]any)["config"].(map[string]any)["channel_pool"] = 4
	data, _ = json.Marshal(doc)
	if status, _ := importJSONToPool(t, server, data, ""); status != http.StatusBadRequest {
		t.Fatalf("invalid JSON pool status=%d", status)
	}
	if status, _ := importJSONToPool(t, server, []byte(`{"version":1,"channels":[{"config":{"name":"x"}}]}`), "7"); status != http.StatusBadRequest {
		t.Fatalf("invalid default pool status=%d", status)
	}

	// CSV 缺列时同样保留现有归属，新渠道归入默认组；导出带出该列。
	urls := `[{"url":"https://api.example.com"}]`
	csvData := csvText(t, []string{"name", "api_key", "urls", "models"},
		[]string{"existing", "sk-a", urls, "kimi-k3"}, []string{"csv-new", "sk-c", urls, "kimi-k3"})
	if status, body := importCSVToPool(t, server, csvData, "2"); status != http.StatusOK || strings.Contains(body, "errors") {
		t.Fatalf("CSV import status=%d body=%s", status, body)
	}
	pools = channelPoolsByName(t, server)
	if pools["existing"] != 0 || pools["csv-new"] != 2 {
		t.Fatalf("CSV import pools=%v", pools)
	}
	csvData = csvText(t, []string{"name", "api_key", "urls", "models", "channel_pool"}, []string{"bad-pool", "sk-d", urls, "kimi-k3", "5"})
	status, body = importCSVToPool(t, server, csvData, "")
	if status != http.StatusOK || !strings.Contains(body, "channel_pool") {
		t.Fatalf("CSV invalid pool status=%d body=%s", status, body)
	}
	if _, ok := channelPoolsByName(t, server)["bad-pool"]; ok {
		t.Fatal("row with invalid channel_pool was imported")
	}

	c, w := newTestContext(t, newRequest(http.MethodGet, "/admin/channels/export.json?channel_pool=2", nil))
	server.HandleExportChannelsJSON(c)
	var exported channelJSONDocument
	if err := json.Unmarshal(w.Body.Bytes(), &exported); err != nil {
		t.Fatal(err)
	}
	if len(exported.Channels) != 1 || exported.Channels[0].Config.Name != "csv-new" || !strings.Contains(w.Body.String(), `"channel_pool": 2`) {
		t.Fatalf("pool export=%s", w.Body.String())
	}
	_ = existing
}

func csvText(t *testing.T, rows ...[]string) string {
	t.Helper()
	var buffer strings.Builder
	writer := csv.NewWriter(&buffer)
	if err := writer.WriteAll(rows); err != nil {
		t.Fatal(err)
	}
	return buffer.String()
}

func importJSONToPool(t *testing.T, server *Server, data []byte, pool string) (int, string) {
	t.Helper()
	return importFileToPool(t, server, "/admin/channels/import.json", "channels.json", data, pool, server.HandleImportChannelsJSON)
}

func importCSVToPool(t *testing.T, server *Server, data, pool string) (int, string) {
	t.Helper()
	return importFileToPool(t, server, "/admin/channels/import", "channels.csv", []byte(data), pool, server.HandleImportChannelsCSV)
}

func importFileToPool(t *testing.T, _ *Server, path, filename string, data []byte, pool string, handler gin.HandlerFunc) (int, string) {
	t.Helper()
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile("file", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if pool != "" {
		path += "?channel_pool=" + pool
	}
	req := newRequest(http.MethodPost, path, body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	c, w := newTestContext(t, req)
	handler(c)
	return w.Code, w.Body.String()
}

func channelPoolsByName(t *testing.T, server *Server) map[string]int {
	t.Helper()
	cfgs, err := server.store.ListConfigs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	pools := make(map[string]int, len(cfgs))
	for _, cfg := range cfgs {
		pools[cfg.Name] = cfg.ChannelPool
	}
	return pools
}
