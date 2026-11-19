package app

import (
	"testing"

	"ccLoad/internal/config"
	"ccLoad/internal/model"
)

func TestParseLogInputTokensMode(t *testing.T) {
	if got := parseLogInputTokensMode(true); got != logInputTokensBillable {
		t.Fatalf("exclude_cache=true mode = %v, 期望可计费口径", got)
	}
	if got := parseLogInputTokensMode(false); got != logInputTokensRaw {
		t.Fatalf("exclude_cache=false mode = %v, 期望上游总量口径", got)
	}
	if !config.DefaultLoggingInputTokensExcludeCache {
		t.Fatal("默认必须保持可计费口径，否则升级后日志数值会整体变大")
	}
}

func TestDisplayInputTokens(t *testing.T) {
	// 可计费 1000，缓存读 2000，缓存写 1000 → 上游 prompt 总量 4000。
	entry := &model.LogEntry{
		InputTokens:              1000,
		CacheReadInputTokens:     2000,
		CacheCreationInputTokens: 1000,
		Cache5mInputTokens:       1000,
	}

	if got := displayInputTokens(logInputTokensBillable, entry); got != 1000 {
		t.Fatalf("可计费口径 = %d，期望 1000", got)
	}
	if got := displayInputTokens(logInputTokensRaw, entry); got != 4000 {
		t.Fatalf("总量口径 = %d，期望 4000", got)
	}

	// 只有 5m/1h 分桶、没有聚合列的历史行：仍要还原成同一个总量。
	splitOnly := &model.LogEntry{
		InputTokens:          1000,
		CacheReadInputTokens: 2000,
		Cache5mInputTokens:   600,
		Cache1hInputTokens:   400,
	}
	if got := displayInputTokens(logInputTokensRaw, splitOnly); got != 4000 {
		t.Fatalf("分桶历史行总量 = %d，期望 4000", got)
	}

	// 无缓存的一次普通调用：两种口径相同。
	plain := &model.LogEntry{InputTokens: 777}
	if got := displayInputTokens(logInputTokensRaw, plain); got != 777 {
		t.Fatalf("无缓存行总量 = %d，期望 777", got)
	}

	if got := displayInputTokens(logInputTokensRaw, nil); got != 0 {
		t.Fatalf("nil 行 = %d，期望 0", got)
	}
}

func TestServerLogInputTokensMode(t *testing.T) {
	var s Server
	// 零值即默认：未初始化时沿用可计费口径。
	if got := s.logInputTokensModeValue(); got != logInputTokensBillable {
		t.Fatalf("零值 mode = %v，期望可计费口径", got)
	}
	s.setLogInputTokensMode(logInputTokensRaw)
	if got := s.logInputTokensModeValue(); got != logInputTokensRaw {
		t.Fatalf("设置后 mode = %v，期望总量口径", got)
	}
}

func TestSettingsRequireRestartForLogInputTokens(t *testing.T) {
	if settingsRequireRestart(map[string]string{config.LoggingInputTokensExcludeCacheSettingKey: "false"}) {
		t.Fatal("日志 input 口径必须在保存后热生效，不要求重启")
	}
}
