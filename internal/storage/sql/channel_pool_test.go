package sql_test

import (
	"context"
	"testing"

	"ccLoad/internal/model"
)

// 渠道组随创建、整体更新、批量修改、导入与查询路径完整往返，未提交新值的写入不改变它。
func TestChannelPoolPersistsAcrossWritePaths(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	created, err := store.CreateConfig(ctx, &model.Config{
		Name:         "pool-two",
		URLs:         model.ChannelURLs{{URL: "https://api.example.com"}},
		Enabled:      true,
		ChannelPool:  2,
		ModelEntries: []model.ModelEntry{{Model: "kimi-k3"}},
	})
	if err != nil {
		t.Fatalf("CreateConfig: %v", err)
	}
	if created.ChannelPool != 2 {
		t.Fatalf("created pool=%d, want 2", created.ChannelPool)
	}

	enabled, err := store.GetEnabledChannelsByModel(ctx, "kimi-k3")
	if err != nil || len(enabled) != 1 || enabled[0].ChannelPool != 2 {
		t.Fatalf("GetEnabledChannelsByModel pool=%v err=%v", enabled, err)
	}
	all, err := store.GetEnabledChannelsByModel(ctx, "*")
	if err != nil || len(all) != 1 || all[0].ChannelPool != 2 {
		t.Fatalf("wildcard lookup pool=%v err=%v", all, err)
	}

	update := created.Clone()
	update.ChannelPool = 1
	updated, err := store.UpdateConfig(ctx, created.ID, update)
	if err != nil || updated.ChannelPool != 1 {
		t.Fatalf("UpdateConfig pool=%v err=%v", updated, err)
	}

	// 只改启用状态的快捷路径不触碰渠道组。
	toggled, err := store.UpdateChannelEnabled(ctx, created.ID, false)
	if err != nil || toggled.ChannelPool != 1 {
		t.Fatalf("UpdateChannelEnabled pool=%v err=%v", toggled, err)
	}

	pool := 0
	result, err := store.BatchPatchConfigs(ctx, []int64{created.ID}, model.BatchConfigPatch{ChannelPool: &pool})
	if err != nil || result.Updated != 1 {
		t.Fatalf("BatchPatchConfigs result=%+v err=%v", result, err)
	}
	unchanged, err := store.BatchPatchConfigs(ctx, []int64{created.ID}, model.BatchConfigPatch{ChannelPool: &pool})
	if err != nil || unchanged.Unchanged != 1 {
		t.Fatalf("repeated BatchPatchConfigs result=%+v err=%v", unchanged, err)
	}
	got, err := store.GetConfig(ctx, created.ID)
	if err != nil || got.ChannelPool != 0 {
		t.Fatalf("GetConfig after batch pool=%v err=%v", got, err)
	}

	invalid := 3
	if _, err := store.BatchPatchConfigs(ctx, []int64{created.ID}, model.BatchConfigPatch{ChannelPool: &invalid}); err == nil {
		t.Fatal("BatchPatchConfigs accepted channel_pool=3")
	}

	imported := got.Clone()
	imported.ID = 0
	imported.ChannelPool = 2
	if _, _, err := store.ImportChannelBatch(ctx, []*model.ChannelWithKeys{{
		Config:  imported,
		APIKeys: []model.APIKey{{KeyIndex: 0, APIKey: "sk-import", KeyStrategy: model.KeyStrategySequential}},
	}}); err != nil {
		t.Fatalf("ImportChannelBatch: %v", err)
	}
	configs, err := store.ListConfigs(ctx)
	if err != nil || len(configs) != 1 || configs[0].ChannelPool != 2 {
		t.Fatalf("ListConfigs after import=%v err=%v", configs, err)
	}
}
