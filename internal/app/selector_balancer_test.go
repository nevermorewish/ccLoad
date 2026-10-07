package app

import (
	"math"
	"testing"

	modelpkg "ccLoad/internal/model"
)

func TestEffPriorityBucket_FloatEdge(t *testing.T) {
	// 模拟浮点误差：值略小于整数边界时，不应被截断到前一档。
	scaledPos := math.Nextafter(51, 0) // 50.999999999...
	pPos := scaledPos / 10
	if got := effPriorityBucket(pPos); got != 51 {
		t.Fatalf("expected bucket=51, got %d (p=%v scaled=%v)", got, pPos, pPos*10)
	}

	scaledNeg := math.Nextafter(-51, 0) // -50.999999999...
	pNeg := scaledNeg / 10
	if got := effPriorityBucket(pNeg); got != -51 {
		t.Fatalf("expected bucket=-51, got %d (p=%v scaled=%v)", got, pNeg, pNeg*10)
	}
}

func TestMedianFloat64(t *testing.T) {
	t.Parallel()
	if got := medianFloat64(nil); got != 0 {
		t.Fatalf("empty=%v", got)
	}
	if got := medianFloat64([]float64{1.0}); got != 0 {
		// fewer than 2 peers → 0 for relative scoring
		t.Fatalf("single=%v want 0", got)
	}
	if got := medianFloat64([]float64{1.0, 3.0}); got != 2.0 {
		t.Fatalf("even=%v want 2", got)
	}
	if got := medianFloat64([]float64{1.0, 2.0, 100.0}); got != 2.0 {
		t.Fatalf("odd=%v want 2", got)
	}
}

func TestCalculateEffectivePriority_TTFBPenalty(t *testing.T) {
	t.Parallel()
	s := &Server{}
	cfg := modelpkg.HealthScoreConfig{
		Enabled:                  true,
		SuccessRatePenaltyWeight: 100,
		MinConfidentSample:       20,
		EnableTTFBScore:          true,
		TTFBPenaltyWeight:        20,
		TTFBMaxSlowRatio:         2.0,
		TTFBMinConfidentSample:   10,
	}
	ch := &modelpkg.Config{ID: 1, Priority: 100}

	// Perfect success, same as median → no ttfb penalty
	stats := modelpkg.ChannelHealthStats{
		SuccessRate: 1, SampleCount: 100,
		AvgFirstByteSeconds: 1.0, FirstByteSampleCount: 20,
	}
	got := s.calculateEffectivePriority(ch, stats, cfg, 1.0)
	if got != 100 {
		t.Fatalf("equal median: got %v want 100", got)
	}

	// 2x median, full confidence → penalty 20
	stats.AvgFirstByteSeconds = 2.0
	got = s.calculateEffectivePriority(ch, stats, cfg, 1.0)
	if got != 80 {
		t.Fatalf("2x median: got %v want 80", got)
	}

	// 4x median capped at (s-1)=2 → penalty 40
	stats.AvgFirstByteSeconds = 4.0
	got = s.calculateEffectivePriority(ch, stats, cfg, 1.0)
	if got != 60 {
		t.Fatalf("4x median capped: got %v want 60", got)
	}

	// Faster than median → no reward
	stats.AvgFirstByteSeconds = 0.5
	got = s.calculateEffectivePriority(ch, stats, cfg, 1.0)
	if got != 100 {
		t.Fatalf("faster: got %v want 100", got)
	}

	// Low sample halves confidence (5/10)
	stats.AvgFirstByteSeconds = 2.0
	stats.FirstByteSampleCount = 5
	got = s.calculateEffectivePriority(ch, stats, cfg, 1.0)
	if got != 90 {
		t.Fatalf("half confidence: got %v want 90", got)
	}

	// Disabled ttfb
	cfg.EnableTTFBScore = false
	stats.FirstByteSampleCount = 20
	got = s.calculateEffectivePriority(ch, stats, cfg, 1.0)
	if got != 100 {
		t.Fatalf("disabled: got %v want 100", got)
	}

	// Fail penalty still applies with ttfb
	cfg.EnableTTFBScore = true
	stats.SuccessRate = 0.8
	stats.SampleCount = 20
	stats.AvgFirstByteSeconds = 2.0
	// fail: 0.2*100*1=20, ttfb:20 → 100-20-20=60
	got = s.calculateEffectivePriority(ch, stats, cfg, 1.0)
	if got != 60 {
		t.Fatalf("fail+ttfb: got %v want 60", got)
	}

	// medianTTFB=0 disables relative penalty
	stats.SuccessRate = 1
	got = s.calculateEffectivePriority(ch, stats, cfg, 0)
	if got != 100 {
		t.Fatalf("no median: got %v want 100", got)
	}
}

func TestCalculateEffectivePriority_SortOverrideTakesOver(t *testing.T) {
	t.Parallel()
	s := &Server{}
	cfg := modelpkg.HealthScoreConfig{
		Enabled:                  true,
		SuccessRatePenaltyWeight: 100,
		MinConfidentSample:       20,
		EnableTTFBScore:          true,
		TTFBPenaltyWeight:        20,
		TTFBMaxSlowRatio:         2.0,
		TTFBMinConfidentSample:   10,
	}
	// 极端坏的健康度：若惩罚生效，P_eff 会是 100-100-40= -40。
	stats := modelpkg.ChannelHealthStats{
		SuccessRate: 0, SampleCount: 100,
		AvgFirstByteSeconds: 4.0, FirstByteSampleCount: 20,
	}

	// 无覆盖：惩罚全额生效
	ch := &modelpkg.Config{ID: 1, Priority: 100}
	if got := s.calculateEffectivePriority(ch, stats, cfg, 1.0); got != -40 {
		t.Fatalf("no override: got %v want -40", got)
	}

	// 有覆盖：直接返回覆盖值，惩罚完全不参与
	ch.SortOverride = 7
	if got := s.calculateEffectivePriority(ch, stats, cfg, 1.0); got != 7 {
		t.Fatalf("override: got %v want 7", got)
	}

	// 负覆盖值同样生效（覆盖不做范围收敛）
	ch.SortOverride = -55
	if got := s.calculateEffectivePriority(ch, stats, cfg, 1.0); got != -55 {
		t.Fatalf("negative override: got %v want -55", got)
	}

	// 0 表示未覆盖，回到惩罚计算
	ch.SortOverride = 0
	if got := s.calculateEffectivePriority(ch, stats, cfg, 1.0); got != -40 {
		t.Fatalf("zero override falls back: got %v want -40", got)
	}
}

func TestConfigSortPriority(t *testing.T) {
	t.Parallel()
	if got := (&modelpkg.Config{Priority: 5}).SortPriority(); got != 5 {
		t.Fatalf("no override: got %d want 5", got)
	}
	if got := (&modelpkg.Config{Priority: 5, SortOverride: 9}).SortPriority(); got != 9 {
		t.Fatalf("override: got %d want 9", got)
	}
	if got := (&modelpkg.Config{Priority: 5, SortOverride: -3}).SortPriority(); got != -3 {
		t.Fatalf("negative override: got %d want -3", got)
	}
	var nilCfg *modelpkg.Config
	if got := nilCfg.SortPriority(); got != 0 {
		t.Fatalf("nil: got %d want 0", got)
	}
}
