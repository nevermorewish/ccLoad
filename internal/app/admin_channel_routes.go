package app

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"ccLoad/internal/model"
)

type channelRoutePoolView struct {
	Pool     int      `json:"pool"`
	Name     string   `json:"name"`
	Channels int      `json:"channels"`
	Enabled  int      `json:"enabled"`
	Inflight int      `json:"inflight"`
	Models   []string `json:"models"` // 组内启用渠道对外提供的模型（基名），供规则编辑选择
}

type channelRouteInflightView struct {
	RuleID   string `json:"rule_id"` // 空串表示未命中规则的请求
	Pool     int    `json:"pool"`
	Inflight int    `json:"inflight"`
}

type channelRouteTargetView struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
}

type channelRoutesView struct {
	Channels []channelRouteTargetView   `json:"channels"`
	Config   ChannelRouteConfig         `json:"config"`
	Pools    []channelRoutePoolView     `json:"pools"`
	Inflight []channelRouteInflightView `json:"inflight"`
}

type channelRouteSimulationPool struct {
	Pool     int    `json:"pool"`
	Name     string `json:"name"`
	Inflight int    `json:"inflight"`
}

type channelRouteSimulation struct {
	Model       string                       `json:"model"`
	Matched     bool                         `json:"matched"`
	RuleID      string                       `json:"rule_id"`
	RuleName    string                       `json:"rule_name"`
	Tier        int                          `json:"tier"`
	InputTokens int                          `json:"input_tokens"`
	Reason      string                       `json:"reason"`
	Pools       []channelRouteSimulationPool `json:"pools"`
	// FirstByteTimeoutSeconds 是命中规则的首字超时；0 表示沿用系统设置。
	FirstByteTimeoutSeconds   int   `json:"first_byte_timeout_seconds"`
	FirstByteTimeoutChannelID int64 `json:"first_byte_timeout_channel_id,omitempty"`
}

// HandleGetChannelRoutes 返回渠道路由规则、各渠道组概况与实时队列。
// GET /admin/channel-routes
func (s *Server) HandleGetChannelRoutes(c *gin.Context) {
	view, err := s.channelRoutesView(c.Request.Context())
	if err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}
	RespondJSON(c, http.StatusOK, view)
}

// HandleGetChannelRouteInflight 只返回实时队列，供管理页定时刷新（不查询渠道配置）。
// GET /admin/channel-routes/inflight
func (s *Server) HandleGetChannelRouteInflight(c *gin.Context) {
	RespondJSON(c, http.StatusOK, gin.H{"inflight": s.channelRouteInflight()})
}

// HandleUpdateChannelRoutes 整体替换渠道路由规则，保存后立即生效（无需重启）。
// PUT /admin/channel-routes
func (s *Server) HandleUpdateChannelRoutes(c *gin.Context) {
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, maxChannelRouteRulesBytes+1))
	if err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	if len(body) > maxChannelRouteRulesBytes {
		RespondErrorMsg(c, http.StatusBadRequest, fmt.Sprintf("channel routes exceed maximum size of %d bytes", maxChannelRouteRulesBytes))
		return
	}
	var req ChannelRouteConfig
	if err := json.Unmarshal(body, &req); err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	config, err := normalizeChannelRouteConfig(req)
	if err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "invalid channel routes: "+err.Error())
		return
	}
	for _, rule := range config.Rules {
		if rule.FirstByteTimeoutChannelID == 0 {
			continue
		}
		cfg, err := s.store.GetConfig(c.Request.Context(), rule.FirstByteTimeoutChannelID)
		if err != nil || cfg == nil || !cfg.Enabled || cfg.IsSystemOne() || channelRequiresProtocolBridge(cfg) {
			RespondErrorMsg(c, http.StatusBadRequest, "first byte timeout target must be an enabled native-protocol channel")
			return
		}
	}
	if err := s.commitChannelRouteConfig(c.Request.Context(), config); err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}
	view, err := s.channelRoutesView(c.Request.Context())
	if err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}
	RespondJSON(c, http.StatusOK, view)
}

// HandleSimulateChannelRoute 按当前规则与实时队列模拟一次渠道组决策，不占用队列、不读写会话粘性。
// 输入 Token 可直接给出，也可提供示例请求体由服务端估算。
// POST /admin/channel-routes/simulate
func (s *Server) HandleSimulateChannelRoute(c *gin.Context) {
	var req struct {
		Model       string          `json:"model"`
		InputTokens *int            `json:"input_tokens"`
		Body        json.RawMessage `json:"body"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	routingModel := model.RoutingModelName(strings.TrimSpace(req.Model))
	if routingModel == "" || routingModel == "*" {
		RespondErrorMsg(c, http.StatusBadRequest, "model is required")
		return
	}
	in := channelPoolRouteInput{model: routingModel, now: time.Now()}
	switch {
	case req.InputTokens != nil:
		if *req.InputTokens < 0 {
			RespondErrorMsg(c, http.StatusBadRequest, "input_tokens must be >= 0")
			return
		}
		in.inputTokens, in.inputTokensKnown = *req.InputTokens, true
	case len(req.Body) > 0 && string(req.Body) != "null":
		in.body = req.Body
	default:
		in.inputTokensKnown = true
	}
	route := s.decideChannelPoolRoute(in)
	// 规则未配置 Token 档位时决策不估算，这里仍展示输入 Token 便于核对。
	inputTokens := route.inputTokens
	if inputTokens < 0 {
		inputTokens = in.inputTokens
		if !in.inputTokensKnown {
			inputTokens = estimateRouteInputTokens(in.body)
		}
	}
	result := channelRouteSimulation{
		Model: routingModel, Matched: route.ruleID != "", RuleID: route.ruleID, RuleName: route.ruleName,
		Tier: route.tier, InputTokens: inputTokens, Reason: route.reason,
		Pools:                     make([]channelRouteSimulationPool, 0, len(route.pools)),
		FirstByteTimeoutSeconds:   int(route.firstByteTimeout / time.Second),
		FirstByteTimeoutChannelID: route.firstByteTimeoutChannelID,
	}
	for _, pool := range route.pools {
		result.Pools = append(result.Pools, channelRouteSimulationPool{
			Pool: pool, Name: channelPoolName(pool), Inflight: s.channelPoolQueues.inflight(route.ruleID, pool),
		})
	}
	RespondJSON(c, http.StatusOK, result)
}

func (s *Server) channelRoutesView(ctx context.Context) (channelRoutesView, error) {
	cfgs, err := s.store.ListConfigs(ctx)
	if err != nil {
		return channelRoutesView{}, err
	}
	pools := make([]channelRoutePoolView, model.ChannelPoolCount)
	poolModels := make([]map[string]struct{}, model.ChannelPoolCount)
	for pool := range pools {
		pools[pool] = channelRoutePoolView{Pool: pool, Name: channelPoolName(pool), Models: []string{}}
		poolModels[pool] = make(map[string]struct{})
	}
	targets := make([]channelRouteTargetView, 0, len(cfgs))
	for _, cfg := range cfgs {
		if cfg != nil && !cfg.IsSystemOne() && !channelRequiresProtocolBridge(cfg) {
			targets = append(targets, channelRouteTargetView{ID: cfg.ID, Name: cfg.Name, Enabled: cfg.Enabled})
		}
		if cfg == nil || !model.ValidChannelPool(cfg.ChannelPool) {
			continue
		}
		view := &pools[cfg.ChannelPool]
		view.Channels++
		if !cfg.Enabled {
			continue
		}
		view.Enabled++
		if cfg.IsSystemOne() {
			continue
		}
		for _, name := range cfg.GetModels() {
			poolModels[cfg.ChannelPool][name] = struct{}{}
		}
	}
	for pool := range pools {
		for name := range poolModels[pool] {
			pools[pool].Models = append(pools[pool].Models, name)
		}
		slices.Sort(pools[pool].Models)
	}

	inflight := s.channelRouteInflight()
	for _, item := range inflight {
		if model.ValidChannelPool(item.Pool) {
			pools[item.Pool].Inflight += item.Inflight
		}
	}
	return channelRoutesView{Config: s.channelRouteConfig(), Pools: pools, Inflight: inflight, Channels: targets}, nil
}

func (s *Server) channelRouteInflight() []channelRouteInflightView {
	queues := s.channelPoolQueues.snapshot()
	inflight := make([]channelRouteInflightView, 0, len(queues))
	for key, count := range queues {
		inflight = append(inflight, channelRouteInflightView{RuleID: key.ruleID, Pool: key.pool, Inflight: count})
	}
	slices.SortFunc(inflight, func(a, b channelRouteInflightView) int {
		if c := cmp.Compare(a.RuleID, b.RuleID); c != 0 {
			return c
		}
		return cmp.Compare(a.Pool, b.Pool)
	})
	return inflight
}
