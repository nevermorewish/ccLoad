package app

import (
	"ccLoad/internal/config"
	"ccLoad/internal/model"
)

// logInputTokensMode 决定日志接口返回的 input_tokens 是哪种口径。
//
// logs.input_tokens 在库里恒为「可计费输入」：它是成本明细与计费对账的唯一输入，
// 缓存读、缓存写各有独立单价，混进 input 会按输入价重复收费。这个开关只改变
// 读取时的展示值，不改变库里的数值，也不改变任何已计算出的成本。
type logInputTokensMode int32

const (
	// logInputTokensBillable 直接返回库中的可计费输入（历史口径，默认）。
	logInputTokensBillable logInputTokensMode = iota
	// logInputTokensRaw 返回上游声明的 prompt 总量，含缓存读写。
	// 用于和上游 usage 逐字段对账（例如 Moonshot prompt_tokens 是否等于各分项之和）。
	logInputTokensRaw
)

// parseLogInputTokensMode 把设置值解析为口径。exclude_cache=true（默认）是可计费口径。
func parseLogInputTokensMode(excludeCache bool) logInputTokensMode {
	if excludeCache {
		return logInputTokensBillable
	}
	return logInputTokensRaw
}

func (s *Server) setLogInputTokensMode(mode logInputTokensMode) {
	s.logInputTokensMode.Store(int32(mode))
}

func (s *Server) logInputTokensModeValue() logInputTokensMode {
	return logInputTokensMode(s.logInputTokensMode.Load())
}

// loadLogInputTokensMode 启动时读取口径设置。
func loadLogInputTokensMode(cs *ConfigService) logInputTokensMode {
	return parseLogInputTokensMode(cs.GetBool(
		config.LoggingInputTokensExcludeCacheSettingKey,
		config.DefaultLoggingInputTokensExcludeCache,
	))
}

// displayInputTokens 按当前口径返回一条日志应展示的输入量。
//
// 上游总量可以直接由库里的三个数还原：prompt = 可计费输入 + 缓存读 + 缓存写。
// 解析层已经把缓存分项从 input 中扣除，所以三者相加就是上游声明的 prompt，
// 不需要额外存一列。缓存分项都缺失时即为 0，此时两种口径相同。
func displayInputTokens(mode logInputTokensMode, entry *model.LogEntry) int {
	if entry == nil {
		return 0
	}
	if mode != logInputTokensRaw {
		return entry.InputTokens
	}
	cacheWrite := entry.CacheCreationInputTokens
	if cacheWrite == 0 {
		cacheWrite = entry.Cache5mInputTokens + entry.Cache1hInputTokens +
			entry.Cache30mInputTokens + entry.Cache12hInputTokens
	}
	return entry.InputTokens + entry.CacheReadInputTokens + cacheWrite
}
