package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/tidwall/gjson"

	"ccLoad/internal/model"
	"ccLoad/internal/util"
)

// 渠道路由：渠道按渠道组（主渠道/渠道1/渠道2）划分，路由规则按模型匹配后，
// 依据估算输入 Token、规则在各组的进行中请求数与会话粘性，决定本次请求的渠道组尝试顺序。
// 组顺序是选路的第一排序键，组内仍沿用优先级/健康度与同档轮询。
// 未命中规则时只允许主渠道：渠道1、渠道2 仅在启用且命中的规则选中它们（或非严格规则追加、
// 队列溢出、会话粘性指向它们）时参与选路。

const (
	channelRouteRulesSettingKey = "channel_route_rules"
	maxChannelRouteRulesBytes   = 64 * 1024
	maxChannelRouteRules        = 64
	maxChannelRouteRuleModels   = 64
	maxChannelRouteTokenTiers   = 16
	maxChannelRouteNameRunes    = 64
	maxChannelRouteIDLength     = 64
	maxChannelRouteModelLength  = 191
	maxChannelRouteQueue        = 1_000_000
	// 规则级首字超时上限：超过 1 小时已失去“卡住即切换渠道”的意义。
	maxChannelRouteFirstByteTimeoutSeconds = 3600

	// 会话粘性：off 不粘；tier 同一会话在同一 Token 档位内固定渠道组（档位变化时重新分流）；
	// session 整个会话固定在首次成功的渠道组。
	channelRouteStickyOff     = "off"
	channelRouteStickyTier    = "tier"
	channelRouteStickySession = "session"

	channelRouteReasonDefault   = "default"
	channelRouteReasonTokenTier = "token_tier"
	channelRouteReasonOverflow  = "overflow"
	channelRouteReasonSticky    = "sticky"

	// 二进制载荷（base64 图片/文件、data URL）不按字符估算，按单张图片的经验值计入。
	routeBinaryPayloadMinBytes = 512
	routeBinaryPayloadTokens   = 1500
	// 估算遍历的最大嵌套深度；更深的结构按原始字节数粗估，避免病态请求体引发深递归。
	routeTokenMaxDepth = 32
)

// channelPoolNames 是渠道组的展示名，与管理后台菜单一致。
var channelPoolNames = [model.ChannelPoolCount]string{"主渠道", "渠道1", "渠道2"}

func channelPoolName(pool int) string {
	if model.ValidChannelPool(pool) {
		return channelPoolNames[pool]
	}
	return fmt.Sprintf("渠道组%d", pool)
}

// ChannelRouteTokenTier 把估算输入 Token 落在 [MinTokens, MaxTokens) 的请求分到 Pool；MaxTokens=0 表示无上限。
type ChannelRouteTokenTier struct {
	MinTokens int `json:"min_tokens"`
	MaxTokens int `json:"max_tokens"`
	Pool      int `json:"pool"`
}

// ChannelRouteOverflow 在 Pool 内本规则的进行中请求达到 MaxQueue 时，把新请求优先分到 TargetPool。
type ChannelRouteOverflow struct {
	Pool       int `json:"pool"`
	MaxQueue   int `json:"max_queue"`
	TargetPool int `json:"target_pool"`
}

// ChannelRouteRule 是一条渠道路由规则：按模型匹配后决定渠道组的尝试顺序。
// Strict=true 时只使用规则选中的渠道组（队列溢出目标与会话粘性组除外），不回退其他组。
// FirstByteTimeoutSeconds>0 时覆盖系统首字超时（含协议覆盖）：命中本规则的流式请求在某个渠道
// 等待首个有效内容超过该秒数，按首字节超时（598）放弃该渠道；配置目标时只尝试目标一次，
// 否则按原候选顺序重试。0 沿用系统设置。
// FirstByteTimeoutFailFast=true 时命中规则的请求在首字超时后不切换渠道、不重试，直接以 503
// 结束本次请求；冷却仍按首字节超时（598）记模型级，后续请求照常跳过该渠道。
// 与 FirstByteTimeoutChannelID 互斥：一个要立刻失败，一个要转最终渠道。
// NonStreamChannelID>0 时客户端非流式请求直接使用指定渠道，成功或错误均作为最终结果。
type ChannelRouteRule struct {
	ID                        string                  `json:"id"`
	Name                      string                  `json:"name"`
	Enabled                   bool                    `json:"enabled"`
	Models                    []string                `json:"models"`
	DefaultPool               int                     `json:"default_pool"`
	TokenTiers                []ChannelRouteTokenTier `json:"token_tiers"`
	Overflow                  []ChannelRouteOverflow  `json:"overflow"`
	Sticky                    string                  `json:"sticky"`
	Strict                    bool                    `json:"strict"`
	FirstByteTimeoutSeconds   int                     `json:"first_byte_timeout_seconds"`
	FirstByteTimeoutChannelID int64                   `json:"first_byte_timeout_channel_id,omitempty"`
	FirstByteTimeoutFailFast  bool                    `json:"first_byte_timeout_fail_fast,omitempty"`
	NonStreamChannelID        int64                   `json:"non_stream_channel_id,omitempty"`
}

// ChannelRouteConfig 是系统设置 channel_route_rules 的完整值；规则按顺序匹配，首条启用且命中的规则生效。
type ChannelRouteConfig struct {
	Rules []ChannelRouteRule `json:"rules"`
}

// parseChannelRouteConfig 解析并规范化持久化的渠道路由配置。
func parseChannelRouteConfig(value string) (ChannelRouteConfig, error) {
	value = strings.TrimSpace(value)
	if value == "" || value == "null" || value == "{}" {
		return ChannelRouteConfig{Rules: []ChannelRouteRule{}}, nil
	}
	if len(value) > maxChannelRouteRulesBytes {
		return ChannelRouteConfig{}, fmt.Errorf("exceeds maximum size of %d bytes", maxChannelRouteRulesBytes)
	}
	var config ChannelRouteConfig
	decoder := json.NewDecoder(strings.NewReader(value))
	if err := decoder.Decode(&config); err != nil {
		return ChannelRouteConfig{}, fmt.Errorf("invalid channel route JSON: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return ChannelRouteConfig{}, errors.New("invalid channel route JSON: trailing data")
	}
	return normalizeChannelRouteConfig(config)
}

// normalizeChannelRouteConfig 校验全部规则并返回独立副本；缺少 ID 的规则生成新 ID。
func normalizeChannelRouteConfig(config ChannelRouteConfig) (ChannelRouteConfig, error) {
	if len(config.Rules) > maxChannelRouteRules {
		return ChannelRouteConfig{}, fmt.Errorf("too many rules (max %d)", maxChannelRouteRules)
	}
	rules := make([]ChannelRouteRule, 0, len(config.Rules))
	seenIDs := make(map[string]struct{}, len(config.Rules))
	for i, rule := range config.Rules {
		normalized, err := normalizeChannelRouteRule(rule)
		if err != nil {
			return ChannelRouteConfig{}, fmt.Errorf("rules[%d]: %w", i, err)
		}
		if normalized.ID == "" {
			for {
				normalized.ID = newChannelRouteRuleID()
				if _, exists := seenIDs[normalized.ID]; !exists {
					break
				}
			}
		}
		if _, exists := seenIDs[normalized.ID]; exists {
			return ChannelRouteConfig{}, fmt.Errorf("rules[%d]: duplicate id %q", i, normalized.ID)
		}
		seenIDs[normalized.ID] = struct{}{}
		rules = append(rules, normalized)
	}
	return ChannelRouteConfig{Rules: rules}, nil
}

func normalizeChannelRouteRule(rule ChannelRouteRule) (ChannelRouteRule, error) {
	rule.ID = strings.TrimSpace(rule.ID)
	if rule.ID != "" && !validChannelRouteRuleID(rule.ID) {
		return ChannelRouteRule{}, fmt.Errorf("id may contain only letters, digits, '-' and '_' (max %d)", maxChannelRouteIDLength)
	}
	rule.Name = strings.TrimSpace(rule.Name)
	if utf8.RuneCountInString(rule.Name) > maxChannelRouteNameRunes {
		return ChannelRouteRule{}, fmt.Errorf("name is too long (max %d characters)", maxChannelRouteNameRunes)
	}
	if strings.IndexFunc(rule.Name, unicode.IsControl) >= 0 {
		return ChannelRouteRule{}, errors.New("name contains illegal characters")
	}

	models := make([]string, 0, len(rule.Models))
	seenModels := make(map[string]struct{}, len(rule.Models))
	for _, raw := range rule.Models {
		pattern := strings.TrimSpace(raw)
		if pattern == "" {
			continue
		}
		// 选路模型是去掉思考后缀的基名；通配模式按字面保留。
		if !strings.Contains(pattern, "*") {
			pattern = model.RoutingModelName(pattern)
		}
		if len(pattern) > maxChannelRouteModelLength {
			return ChannelRouteRule{}, fmt.Errorf("model %q is too long (max %d bytes)", pattern, maxChannelRouteModelLength)
		}
		if strings.IndexFunc(pattern, unicode.IsControl) >= 0 {
			return ChannelRouteRule{}, fmt.Errorf("model %q contains illegal characters", pattern)
		}
		key := strings.ToLower(pattern)
		if _, exists := seenModels[key]; exists {
			continue
		}
		seenModels[key] = struct{}{}
		models = append(models, pattern)
	}
	if len(models) == 0 {
		return ChannelRouteRule{}, errors.New("models cannot be empty")
	}
	if len(models) > maxChannelRouteRuleModels {
		return ChannelRouteRule{}, fmt.Errorf("too many models (max %d)", maxChannelRouteRuleModels)
	}
	rule.Models = models

	if !model.ValidChannelPool(rule.DefaultPool) {
		return ChannelRouteRule{}, fmt.Errorf("default_pool must be between %d and %d", model.ChannelPoolMain, model.ChannelPoolCount-1)
	}

	if len(rule.TokenTiers) > maxChannelRouteTokenTiers {
		return ChannelRouteRule{}, fmt.Errorf("too many token tiers (max %d)", maxChannelRouteTokenTiers)
	}
	tiers := make([]ChannelRouteTokenTier, 0, len(rule.TokenTiers))
	for i, tier := range rule.TokenTiers {
		switch {
		case tier.MinTokens < 0 || tier.MaxTokens < 0:
			return ChannelRouteRule{}, fmt.Errorf("token_tiers[%d]: token bounds must be >= 0", i)
		case tier.MinTokens == 0 && tier.MaxTokens == 0:
			return ChannelRouteRule{}, fmt.Errorf("token_tiers[%d]: min_tokens or max_tokens is required", i)
		case tier.MaxTokens > 0 && tier.MaxTokens <= tier.MinTokens:
			return ChannelRouteRule{}, fmt.Errorf("token_tiers[%d]: max_tokens must be greater than min_tokens", i)
		case !model.ValidChannelPool(tier.Pool):
			return ChannelRouteRule{}, fmt.Errorf("token_tiers[%d]: invalid pool %d", i, tier.Pool)
		}
		tiers = append(tiers, tier)
	}
	rule.TokenTiers = tiers

	if len(rule.Overflow) > model.ChannelPoolCount {
		return ChannelRouteRule{}, fmt.Errorf("too many overflow rules (max %d)", model.ChannelPoolCount)
	}
	overflow := make([]ChannelRouteOverflow, 0, len(rule.Overflow))
	sources := make(map[int]struct{}, len(rule.Overflow))
	for i, item := range rule.Overflow {
		switch {
		case !model.ValidChannelPool(item.Pool) || !model.ValidChannelPool(item.TargetPool):
			return ChannelRouteRule{}, fmt.Errorf("overflow[%d]: invalid pool", i)
		case item.Pool == item.TargetPool:
			return ChannelRouteRule{}, fmt.Errorf("overflow[%d]: target_pool must differ from pool", i)
		case item.MaxQueue < 1 || item.MaxQueue > maxChannelRouteQueue:
			return ChannelRouteRule{}, fmt.Errorf("overflow[%d]: max_queue must be between 1 and %d", i, maxChannelRouteQueue)
		}
		if _, exists := sources[item.Pool]; exists {
			return ChannelRouteRule{}, fmt.Errorf("overflow[%d]: duplicate overflow for pool %d", i, item.Pool)
		}
		sources[item.Pool] = struct{}{}
		overflow = append(overflow, item)
	}
	rule.Overflow = overflow

	switch strings.ToLower(strings.TrimSpace(rule.Sticky)) {
	case "", channelRouteStickyOff:
		rule.Sticky = channelRouteStickyOff
	case channelRouteStickyTier:
		rule.Sticky = channelRouteStickyTier
	case channelRouteStickySession:
		rule.Sticky = channelRouteStickySession
	default:
		return ChannelRouteRule{}, fmt.Errorf("invalid sticky %q (allowed: off, tier, session)", rule.Sticky)
	}

	if rule.FirstByteTimeoutSeconds < 0 || rule.FirstByteTimeoutSeconds > maxChannelRouteFirstByteTimeoutSeconds {
		return ChannelRouteRule{}, fmt.Errorf("first_byte_timeout_seconds must be between 0 and %d", maxChannelRouteFirstByteTimeoutSeconds)
	}
	if rule.NonStreamChannelID < 0 {
		return ChannelRouteRule{}, fmt.Errorf("non_stream_channel_id must be >= 0")
	}
	if rule.FirstByteTimeoutChannelID < 0 || (rule.FirstByteTimeoutChannelID > 0 && rule.FirstByteTimeoutSeconds == 0) {
		return ChannelRouteRule{}, fmt.Errorf("first_byte_timeout_channel_id requires a positive timeout and channel ID")
	}
	if rule.FirstByteTimeoutFailFast && rule.FirstByteTimeoutSeconds == 0 {
		return ChannelRouteRule{}, fmt.Errorf("first_byte_timeout_fail_fast requires a positive timeout")
	}
	if rule.FirstByteTimeoutFailFast && rule.FirstByteTimeoutChannelID > 0 {
		return ChannelRouteRule{}, fmt.Errorf("first_byte_timeout_fail_fast cannot be combined with first_byte_timeout_channel_id")
	}
	return rule, nil
}

func validChannelRouteRuleID(id string) bool {
	if id == "" || len(id) > maxChannelRouteIDLength {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '-' && c != '_' {
			return false
		}
	}
	return true
}

func newChannelRouteRuleID() string {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		// crypto/rand 只在系统熵源损坏时失败；时间戳 ID 仍保证进程内可区分。
		return fmt.Sprintf("r%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(raw[:])
}

// channelRouteSnapshot 发布后不可修改；保存时整份替换，代理热路径只做一次原子读取。
type channelRouteSnapshot struct {
	config ChannelRouteConfig
	rules  []channelRouteCompiledRule
}

type channelRouteCompiledRule struct {
	rule     ChannelRouteRule
	patterns []string // 小写匹配模式
}

func newChannelRouteSnapshot(config ChannelRouteConfig) *channelRouteSnapshot {
	snapshot := &channelRouteSnapshot{config: cloneChannelRouteConfig(config)}
	for _, rule := range snapshot.config.Rules {
		patterns := make([]string, len(rule.Models))
		for i, pattern := range rule.Models {
			patterns[i] = strings.ToLower(pattern)
		}
		snapshot.rules = append(snapshot.rules, channelRouteCompiledRule{rule: rule, patterns: patterns})
	}
	return snapshot
}

func cloneChannelRouteConfig(config ChannelRouteConfig) ChannelRouteConfig {
	rules := make([]ChannelRouteRule, len(config.Rules))
	for i, rule := range config.Rules {
		rule.Models = slices.Clone(rule.Models)
		rule.TokenTiers = append([]ChannelRouteTokenTier{}, rule.TokenTiers...)
		rule.Overflow = append([]ChannelRouteOverflow{}, rule.Overflow...)
		rules[i] = rule
	}
	return ChannelRouteConfig{Rules: rules}
}

// match 返回首条启用且模型命中的规则；未命中返回 nil。
func (s *channelRouteSnapshot) match(modelName string) *ChannelRouteRule {
	if s == nil {
		return nil
	}
	name := strings.ToLower(model.RoutingModelName(strings.TrimSpace(modelName)))
	if name == "" || name == "*" {
		return nil
	}
	for i := range s.rules {
		compiled := &s.rules[i]
		if !compiled.rule.Enabled {
			continue
		}
		for _, pattern := range compiled.patterns {
			if matchChannelRoutePattern(pattern, name) {
				return &compiled.rule
			}
		}
	}
	return nil
}

// matchChannelRoutePattern 只支持 * 通配（匹配任意长度），其余字符按字面比较；调用方已统一小写。
func matchChannelRoutePattern(pattern, name string) bool {
	if !strings.Contains(pattern, "*") {
		return pattern == name
	}
	parts := strings.Split(pattern, "*")
	if !strings.HasPrefix(name, parts[0]) {
		return false
	}
	rest := name[len(parts[0]):]
	for _, part := range parts[1 : len(parts)-1] {
		index := strings.Index(rest, part)
		if index < 0 {
			return false
		}
		rest = rest[index+len(part):]
	}
	return strings.HasSuffix(rest, parts[len(parts)-1])
}

func (s *Server) setChannelRouteConfig(config ChannelRouteConfig) {
	s.channelRoutes.Store(newChannelRouteSnapshot(config))
}

func (s *Server) channelRouteConfig() ChannelRouteConfig {
	if snapshot := s.channelRoutes.Load(); snapshot != nil {
		return cloneChannelRouteConfig(snapshot.config)
	}
	return ChannelRouteConfig{Rules: []ChannelRouteRule{}}
}

// loadChannelRouteConfig 读取渠道路由配置。解析失败只 WARN 回退为空规则，
// 不让一条写坏的配置挡住进程启动。
func loadChannelRouteConfig(cs *ConfigService) ChannelRouteConfig {
	config, err := parseChannelRouteConfig(cs.GetString(channelRouteRulesSettingKey, "{}"))
	if err != nil {
		log.Printf("[WARN] 无效的 %s，已回退为未配置: %v", channelRouteRulesSettingKey, err)
		return ChannelRouteConfig{Rules: []ChannelRouteRule{}}
	}
	return config
}

// commitChannelRouteConfig 持久化并发布渠道路由配置，二者串行完成，避免并发保存让数据库终值与运行态错序。
// 修改或删除的规则同时清除其渠道组粘性，避免旧会话继续按旧规则落组。
func (s *Server) commitChannelRouteConfig(ctx context.Context, config ChannelRouteConfig) error {
	encoded, err := json.Marshal(config)
	if err != nil {
		return fmt.Errorf("encode channel routes: %w", err)
	}
	s.channelRouteUpdateMu.Lock()
	defer s.channelRouteUpdateMu.Unlock()
	if err := s.configService.BatchUpdateSettings(ctx, map[string]string{channelRouteRulesSettingKey: string(encoded)}); err != nil {
		return err
	}
	previous := s.channelRoutes.Load()
	s.setChannelRouteConfig(config)
	if previous != nil {
		next := make(map[string]ChannelRouteRule, len(config.Rules))
		for _, rule := range config.Rules {
			next[rule.ID] = rule
		}
		stale := make(map[string]struct{})
		for _, compiled := range previous.rules {
			if current, ok := next[compiled.rule.ID]; !ok || !channelRoutePoolDecisionEqual(current, compiled.rule) {
				stale[compiled.rule.ID] = struct{}{}
			}
		}
		s.channelPoolAffinity.forgetRules(stale)
	}
	return nil
}

func channelRouteRuleEqual(a, b ChannelRouteRule) bool {
	return a.ID == b.ID && a.Name == b.Name && a.Enabled == b.Enabled && slices.Equal(a.Models, b.Models) &&
		a.DefaultPool == b.DefaultPool && slices.Equal(a.TokenTiers, b.TokenTiers) && slices.Equal(a.Overflow, b.Overflow) &&
		a.Sticky == b.Sticky && a.Strict == b.Strict && a.FirstByteTimeoutSeconds == b.FirstByteTimeoutSeconds && a.FirstByteTimeoutChannelID == b.FirstByteTimeoutChannelID && a.FirstByteTimeoutFailFast == b.FirstByteTimeoutFailFast && a.NonStreamChannelID == b.NonStreamChannelID
}

// channelRoutePoolDecisionEqual 忽略首字超时：它只影响渠道尝试、不改变渠道组决策，
// 单独调整时保留会话粘性（会话换组会丢失上游提示词缓存）。
func channelRoutePoolDecisionEqual(a, b ChannelRouteRule) bool {
	a.FirstByteTimeoutSeconds, b.FirstByteTimeoutSeconds = 0, 0
	return channelRouteRuleEqual(a, b)
}

// ============================================================================
// 单请求决策
// ============================================================================

// channelPoolRoute 是单个请求的渠道组决策：pools 按尝试顺序排列，未列出的组不参与选路。
// 决策同时持有首选组的队列名额（reserve），实际尝试换组时随之转移，请求结束时归还。
type channelPoolRoute struct {
	ruleID      string
	ruleName    string
	pools       []int
	tier        int // 命中的 Token 档位下标；-1 表示使用默认渠道组
	inputTokens int // 估算输入 Token；-1 表示规则未配置档位、未估算
	reason      string
	stickyKey   channelPoolAffinityKey
	// firstByteTimeout 是命中规则配置的首字超时，每次渠道尝试生效；0 表示沿用系统设置。
	firstByteTimeout          time.Duration
	firstByteTimeoutChannelID int64
	// firstByteTimeoutFailFast 时首字超时直接以 503 结束请求，不切换渠道也不再重试。
	firstByteTimeoutFailFast bool
	nonStreamChannelID       int64

	mu           sync.Mutex
	queues       *channelPoolQueues
	reserved     bool
	reservedPool int
}

type channelPoolRouteInput struct {
	model string
	body  []byte
	// session 惰性返回会话粘性键（令牌+客户端会话 ID），只在命中规则启用会话粘性时调用；
	// nil 或空串表示无会话身份。
	session func() string
	// inputTokensKnown 时直接使用 inputTokens（管理端模拟），否则按 body 估算。
	inputTokens      int
	inputTokensKnown bool
	now              time.Time
}

// memoizeSessionKey 让会话键只推导一次：渠道组粘性与渠道级会话粘性共用同一结果。
func memoizeSessionKey(resolve func() string) func() string {
	var (
		once sync.Once
		key  string
	)
	return func() string {
		once.Do(func() { key = resolve() })
		return key
	}
}

type channelPoolRouteContextKey struct{}

func withChannelPoolRoute(ctx context.Context, route *channelPoolRoute) context.Context {
	if ctx == nil || route == nil {
		return ctx
	}
	return context.WithValue(ctx, channelPoolRouteContextKey{}, route)
}

func channelPoolRouteFromContext(ctx context.Context) *channelPoolRoute {
	if ctx == nil {
		return nil
	}
	route, _ := ctx.Value(channelPoolRouteContextKey{}).(*channelPoolRoute)
	return route
}

// channelRouteFirstByteTimeout 返回渠道尝试使用的首字超时：命中规则配置了首字超时时覆盖 fallback
// （系统全局值或协议覆盖值），否则原样返回 fallback。
func channelRouteFirstByteTimeout(ctx context.Context, fallback time.Duration) time.Duration {
	if terminalChannelAttempt(ctx) {
		return fallback
	}
	if route := channelPoolRouteFromContext(ctx); route != nil && route.firstByteTimeout > 0 {
		return route.firstByteTimeout
	}
	return fallback
}

// channelRouteFirstByteFailFast 报告本次首字超时是否由命中规则要求立即失败：
// 只影响结束方式（503 + 不再重试），冷却仍按 598 记模型级。
func channelRouteFirstByteFailFast(ctx context.Context, err error) bool {
	if !errors.Is(err, util.ErrUpstreamFirstByteTimeout) {
		return false
	}
	route := channelPoolRouteFromContext(ctx)
	return route != nil && route.firstByteTimeoutFailFast
}

// rank 返回渠道组在本次决策中的尝试顺位；nil 决策下所有组同为 0。
func (r *channelPoolRoute) rank(pool int) int {
	if r == nil {
		return 0
	}
	if index := slices.Index(r.pools, pool); index >= 0 {
		return index
	}
	return len(r.pools)
}

func (r *channelPoolRoute) allows(pool int) bool {
	return r == nil || slices.Contains(r.pools, pool)
}

// restricted 报告决策是否排除了部分渠道组。
func (r *channelPoolRoute) restricted() bool {
	return r != nil && len(r.pools) < model.ChannelPoolCount
}

// inLeadingPool 报告渠道是否位于候选中顺位最靠前的渠道组；不在候选中或没有决策时返回 true。
// 执行会话钉住的渠道只在此时置顶：规则（Token 档位、队列溢出）改选其他组后，会话随之迁移。
func (r *channelPoolRoute) inLeadingPool(cands []*model.Config, channelID int64) bool {
	if r == nil {
		return true
	}
	leading, bound := len(r.pools), -1
	for _, cfg := range cands {
		if cfg == nil {
			continue
		}
		rank := r.rank(cfg.ChannelPool)
		leading = min(leading, rank)
		if cfg.ID == channelID {
			bound = rank
		}
	}
	return bound < 0 || bound == leading
}

func (r *channelPoolRoute) rankFunc() func(*model.Config) int {
	if r == nil {
		return nil
	}
	return func(cfg *model.Config) int { return r.rank(cfg.ChannelPool) }
}

// filterChannels 只保留决策允许的渠道组。返回新 slice：调用方可能按原长度复用入参
// （selector.go 的模糊匹配回退会对同一候选集过滤两次）。
func (r *channelPoolRoute) filterChannels(channels []*model.Config) []*model.Config {
	if r == nil || !r.restricted() {
		return channels
	}
	filtered := make([]*model.Config, 0, len(channels))
	for _, cfg := range channels {
		if cfg != nil && r.allows(cfg.ChannelPool) {
			filtered = append(filtered, cfg)
		}
	}
	return filtered
}

func (r *channelPoolRoute) describe() string {
	if r == nil {
		return ""
	}
	names := make([]string, len(r.pools))
	for i, pool := range r.pools {
		names[i] = channelPoolName(pool)
	}
	label := r.ruleName
	if label == "" {
		label = r.ruleID
	}
	// 未命中规则时没有规则身份，不能拼出 `channel route "" allows ...`。
	if label == "" {
		return fmt.Sprintf("channel route not matched; allows %s", strings.Join(names, " > "))
	}
	return fmt.Sprintf("channel route %q allows %s", label, strings.Join(names, " > "))
}

// reserve 在决策完成时占用首选渠道组的队列名额，保证并发突发同样按队列上限溢出。
func (r *channelPoolRoute) reserve(queues *channelPoolQueues) {
	if r == nil || queues == nil || len(r.pools) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.reserved {
		return
	}
	r.queues, r.reserved, r.reservedPool = queues, true, r.pools[0]
	queues.add(channelPoolQueueKey{ruleID: r.ruleID, pool: r.reservedPool}, 1)
}

// moveReservation 在实际尝试的渠道属于其他组时转移队列名额，队列始终反映请求当前所在的组。
func (r *channelPoolRoute) moveReservation(pool int) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.reserved || r.reservedPool == pool {
		return
	}
	r.queues.add(channelPoolQueueKey{ruleID: r.ruleID, pool: r.reservedPool}, -1)
	r.reservedPool = pool
	r.queues.add(channelPoolQueueKey{ruleID: r.ruleID, pool: pool}, 1)
}

func (r *channelPoolRoute) releaseReservation() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.reserved {
		return
	}
	r.reserved = false
	r.queues.add(channelPoolQueueKey{ruleID: r.ruleID, pool: r.reservedPool}, -1)
}

// defaultChannelPoolRoute 是未命中规则时的顺序：只允许主渠道。
// 渠道1、渠道2 是规则专用组，必须由启用且命中的规则显式选中才会承接请求，
// 否则关闭渠道路由时它们仍会作为主渠道的兜底被选中。
func defaultChannelPoolRoute() *channelPoolRoute {
	return &channelPoolRoute{pools: []int{model.ChannelPoolMain}, tier: -1, inputTokens: -1, reason: channelRouteReasonDefault}
}

// routeChannelPools 为代理请求做渠道组决策并占用首选组的队列名额；调用方必须 releaseReservation。
func (s *Server) routeChannelPools(in channelPoolRouteInput) *channelPoolRoute {
	route := s.decideChannelPoolRoute(in)
	route.reserve(s.channelPoolQueues)
	return route
}

// decideChannelPoolRoute 计算渠道组顺序，不占用队列名额（管理端模拟复用）。
// 顺序：Token 档位（未命中用默认组）→ 会话粘性 → 队列溢出 → 非严格模式追加其余组。
func (s *Server) decideChannelPoolRoute(in channelPoolRouteInput) *channelPoolRoute {
	rule := s.channelRoutes.Load().match(in.model)
	if rule == nil {
		return defaultChannelPoolRoute()
	}
	route := &channelPoolRoute{
		ruleID: rule.ID, ruleName: rule.Name, tier: -1, inputTokens: -1, reason: channelRouteReasonDefault,
		firstByteTimeout:          time.Duration(rule.FirstByteTimeoutSeconds) * time.Second,
		firstByteTimeoutChannelID: rule.FirstByteTimeoutChannelID,
		firstByteTimeoutFailFast:  rule.FirstByteTimeoutFailFast,
		nonStreamChannelID:        rule.NonStreamChannelID,
	}

	base := rule.DefaultPool
	if len(rule.TokenTiers) > 0 {
		tokens := in.inputTokens
		if !in.inputTokensKnown {
			tokens = estimateRouteInputTokens(in.body)
		}
		route.inputTokens = tokens
		for i, tier := range rule.TokenTiers {
			if tokens >= tier.MinTokens && (tier.MaxTokens == 0 || tokens < tier.MaxTokens) {
				base, route.tier, route.reason = tier.Pool, i, channelRouteReasonTokenTier
				break
			}
		}
	}

	order := make([]int, 0, model.ChannelPoolCount)
	appendPool := func(pool int) {
		if !slices.Contains(order, pool) {
			order = append(order, pool)
		}
	}

	if rule.Sticky != channelRouteStickyOff && in.session != nil {
		if sessionKey := in.session(); sessionKey != "" {
			route.stickyKey = channelPoolAffinityKey{ruleID: rule.ID, session: sessionKey}
			now := in.now
			if now.IsZero() {
				now = time.Now()
			}
			if bound, ok := s.channelPoolAffinity.lookup(route.stickyKey, now); ok && model.ValidChannelPool(bound.pool) &&
				(rule.Sticky == channelRouteStickySession || bound.tier == route.tier) {
				// 粘性组优先；它不可用时退回本次按 Token 档位计算出的组。
				appendPool(bound.pool)
				appendPool(base)
				route.reason = channelRouteReasonSticky
			}
		}
	}

	if len(order) == 0 {
		// 队列溢出可以接力（渠道1→渠道2→主渠道），遇到环即停；被溢出的组按逆序留作回退。
		visited := []int{base}
		current := base
		for range model.ChannelPoolCount {
			overflow := rule.overflowFrom(current)
			if overflow == nil || s.channelPoolQueues.inflight(rule.ID, current) < overflow.MaxQueue ||
				slices.Contains(visited, overflow.TargetPool) {
				break
			}
			current = overflow.TargetPool
			visited = append(visited, current)
			route.reason = channelRouteReasonOverflow
		}
		for i := len(visited) - 1; i >= 0; i-- {
			appendPool(visited[i])
		}
	}

	if !rule.Strict {
		for pool := range model.ChannelPoolCount {
			appendPool(pool)
		}
	}
	route.pools = order
	return route
}

func (r *ChannelRouteRule) overflowFrom(pool int) *ChannelRouteOverflow {
	for i := range r.Overflow {
		if r.Overflow[i].Pool == pool {
			return &r.Overflow[i]
		}
	}
	return nil
}

// bindChannelPoolAffinity 在请求成功后记录会话落到的渠道组；回退到其他组成功后自然改绑。
func (s *Server) bindChannelPoolAffinity(route *channelPoolRoute, pool int) {
	if s == nil || route == nil || route.stickyKey.ruleID == "" {
		return
	}
	s.channelPoolAffinity.bind(route.stickyKey, channelPoolAffinityTarget{pool: pool, tier: route.tier}, time.Now())
}

// ============================================================================
// 队列计数
// ============================================================================

type channelPoolQueueKey struct {
	ruleID string // 空串表示未命中规则的请求
	pool   int
}

// channelPoolQueues 记录各规则在各渠道组的进行中请求数（进程内状态），供队列溢出判定与管理页展示。
type channelPoolQueues struct {
	mu     sync.Mutex
	counts map[channelPoolQueueKey]int
}

func newChannelPoolQueues() *channelPoolQueues {
	return &channelPoolQueues{counts: make(map[channelPoolQueueKey]int)}
}

func (q *channelPoolQueues) add(key channelPoolQueueKey, delta int) {
	if q == nil {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if next := q.counts[key] + delta; next > 0 {
		q.counts[key] = next
	} else {
		delete(q.counts, key)
	}
}

func (q *channelPoolQueues) inflight(ruleID string, pool int) int {
	if q == nil {
		return 0
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.counts[channelPoolQueueKey{ruleID: ruleID, pool: pool}]
}

func (q *channelPoolQueues) snapshot() map[channelPoolQueueKey]int {
	result := make(map[channelPoolQueueKey]int)
	if q == nil {
		return result
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	for key, count := range q.counts {
		result[key] = count
	}
	return result
}

// ============================================================================
// 渠道组会话粘性
// ============================================================================

type channelPoolAffinityKey struct {
	ruleID  string
	session string
}

type channelPoolAffinityTarget struct {
	pool int
	tier int
}

type channelPoolAffinityEntry struct {
	target    channelPoolAffinityTarget
	expiresAt time.Time
}

// channelPoolAffinityStore 把（规则，会话）绑定到上次成功的渠道组，TTL 与渠道级会话粘性一致。
// 绑定只在成功后写入，条目增长受真实请求速率约束，过期条目由后台循环回收。
type channelPoolAffinityStore struct {
	mu      sync.Mutex
	entries map[channelPoolAffinityKey]channelPoolAffinityEntry
}

func newChannelPoolAffinityStore() *channelPoolAffinityStore {
	return &channelPoolAffinityStore{entries: make(map[channelPoolAffinityKey]channelPoolAffinityEntry)}
}

func (s *channelPoolAffinityStore) lookup(key channelPoolAffinityKey, now time.Time) (channelPoolAffinityTarget, bool) {
	if s == nil || key.ruleID == "" || key.session == "" {
		return channelPoolAffinityTarget{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.entries[key]
	if !ok || !now.Before(entry.expiresAt) {
		return channelPoolAffinityTarget{}, false
	}
	return entry.target, true
}

func (s *channelPoolAffinityStore) bind(key channelPoolAffinityKey, target channelPoolAffinityTarget, now time.Time) {
	if s == nil || key.ruleID == "" || key.session == "" || !model.ValidChannelPool(target.pool) {
		return
	}
	s.mu.Lock()
	s.entries[key] = channelPoolAffinityEntry{target: target, expiresAt: now.Add(sessionAffinityTTL)}
	s.mu.Unlock()
}

func (s *channelPoolAffinityStore) forgetRules(ruleIDs map[string]struct{}) {
	if s == nil || len(ruleIDs) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for key := range s.entries {
		if _, stale := ruleIDs[key.ruleID]; stale {
			delete(s.entries, key)
		}
	}
}

func (s *channelPoolAffinityStore) cleanupExpired(now time.Time) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, entry := range s.entries {
		if !now.Before(entry.expiresAt) {
			delete(s.entries, key)
		}
	}
}

// ============================================================================
// 输入 Token 估算
// ============================================================================

// routeTokenSkippedKeys 不计入提示词：模型名、思考签名、加密推理与缓存/会话路由提示。
var routeTokenSkippedKeys = map[string]bool{
	"model":                true,
	"signature":            true,
	"thoughtSignature":     true,
	"thought_signature":    true,
	"encrypted_content":    true,
	"metadata":             true,
	"prompt_cache_key":     true,
	"previous_response_id": true,
}

// estimateRouteInputTokens 按请求体估算输入 Token，供渠道路由的 Token 档位使用。
// 与协议无关：遍历全部 JSON 字符串按文本估算（同本地 count_tokens 的中英文比例），
// base64/data URL 等二进制载荷按单张图片计。非 JSON 请求体返回 0。
func estimateRouteInputTokens(body []byte) int {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return 0
	}
	total := 0
	var walk func(value gjson.Result, depth int)
	walk = func(value gjson.Result, depth int) {
		value.ForEach(func(key, item gjson.Result) bool {
			if key.Type == gjson.String && routeTokenSkippedKeys[key.Str] {
				return true
			}
			switch item.Type {
			case gjson.String:
				total += estimateRouteTextTokens(item.Str)
			case gjson.JSON:
				if depth >= routeTokenMaxDepth {
					total += len(item.Raw) / 4
					return true
				}
				walk(item, depth+1)
			}
			return true
		})
	}
	root := gjson.ParseBytes(body)
	switch root.Type {
	case gjson.String:
		return estimateRouteTextTokens(root.Str)
	case gjson.JSON:
		walk(root, 1)
	}
	return total
}

func estimateRouteTextTokens(text string) int {
	if len(text) >= routeBinaryPayloadMinBytes && looksLikeBinaryPayload(text) {
		return routeBinaryPayloadTokens
	}
	return estimateTextTokens(text)
}

// looksLikeBinaryPayload 识别 data URL 与纯 base64 字符串；自然语言与代码都含空白或标点，不会命中。
func looksLikeBinaryPayload(text string) bool {
	if strings.HasPrefix(text, "data:") {
		header := text[:min(len(text), 256)]
		return strings.Contains(header, ";base64,")
	}
	for i := 0; i < len(text); i++ {
		c := text[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') &&
			c != '+' && c != '/' && c != '=' && c != '-' && c != '_' {
			return false
		}
	}
	return true
}
