package app

import (
	"net/http"
	"time"

	"ccLoad/internal/config"
	"ccLoad/internal/model"
	"github.com/gin-gonic/gin"
)

type channelMonitorItem struct {
	ID             int64                     `json:"id"`
	Name           string                    `json:"name"`
	Enabled        bool                      `json:"enabled"`
	AuthType       string                    `json:"auth_type"`
	Models         []string                  `json:"models"`
	Monitored      bool                      `json:"monitored"`
	EffectiveModel string                    `json:"effective_model"`
	Status         string                    `json:"status"`
	Running        bool                      `json:"running"`
	NextCheck      *time.Time                `json:"next_check"`
	Stats          model.ChannelMonitorStats `json:"stats"`
}

func nextChannelMonitorCheck(cfg *model.Config, now time.Time, intervalMinutes int) *time.Time {
	if !cfg.Enabled || !cfg.ScheduledCheckEnabled {
		return nil
	}
	// 用调度器同一套规则（全局间隔 + 可用时段）逐一试探后续分钟。
	for candidate, end := now.Truncate(time.Minute).Add(time.Minute), now.Add(48*time.Hour); candidate.Before(end); candidate = candidate.Add(time.Minute) {
		if cfg.ChannelMonitorDueAt(candidate, intervalMinutes) {
			return &candidate
		}
	}
	return nil
}

func monitorStatus(cfg *model.Config, stat model.ChannelMonitorStats) string {
	if !cfg.Enabled {
		return "disabled"
	}
	if stat.Samples == 0 || len(stat.Recent) == 0 {
		return "unknown"
	}
	var latest *model.ChannelMonitorProbe
	for index := range stat.Recent {
		probe := &stat.Recent[index]
		// status_code=0 and duration=0 is a scheduler skip record, not a failed probe.
		if probe.StatusCode == 0 && probe.Duration == 0 {
			continue
		}
		latest = probe
		break
	}
	if latest == nil {
		return "unknown"
	}
	if latest.StatusCode < 200 || latest.StatusCode >= 300 {
		return "offline"
	}
	if latest.Duration >= 3 || stat.AverageLatency >= 3 {
		return "degraded"
	}
	// A successful latest probe with recent failures is reachable but unstable.
	if stat.Successes < stat.Samples {
		return "degraded"
	}
	return "online"
}

// HandleChannelMonitor 返回监控视图：渠道列表、每渠道的探测统计与状态。
// 检测间隔是全局设置，这里一并下发，前端不再提供按渠道的间隔与开始时间。
func (s *Server) HandleChannelMonitor(c *gin.Context) {
	ctx := c.Request.Context()
	configs, err := s.store.ListConfigs(ctx)
	if err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}
	now := time.Now()
	interval := s.configService.GetInt(config.ChannelMonitorIntervalSettingKey, config.DefaultChannelMonitorIntervalMinutes)
	stats, err := s.store.ListChannelMonitorStats(ctx, now.Add(-24*time.Hour), now)
	if err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}
	type key struct {
		id    int64
		model string
	}
	byModel := make(map[key]model.ChannelMonitorStats, len(stats))
	for _, stat := range stats {
		byModel[key{stat.ChannelID, stat.Model}] = stat
	}
	items := make([]channelMonitorItem, 0, len(configs))
	for _, cfg := range configs {
		name, _ := selectScheduledCheckModel(cfg)
		stat, ok := byModel[key{cfg.ID, name}]
		if !ok {
			stat = model.ChannelMonitorStats{ChannelID: cfg.ID, Model: name, Recent: []model.ChannelMonitorProbe{}}
		}
		_, running := s.scheduledChannelChecksRunning.Load(cfg.ID)
		items = append(items, channelMonitorItem{
			ID: cfg.ID, Name: cfg.Name, Enabled: cfg.Enabled, AuthType: cfg.GetAuthType(), Models: cfg.GetModels(),
			// 检测模型由后端自动选择，这里回传实际选中的模型供展示。
			Monitored:      cfg.ScheduledCheckEnabled,
			EffectiveModel: name, Status: monitorStatus(cfg, stat), Running: running, NextCheck: nextChannelMonitorCheck(cfg, now, interval), Stats: stat,
		})
	}
	zone, offset := now.Zone()
	RespondJSON(c, http.StatusOK, gin.H{
		"items": items, "server_time": now, "timezone": zone, "timezone_offset_minutes": offset / 60,
		"interval_minutes": interval,
		"interval_min":     config.ChannelMonitorIntervalMinMinutes,
		"interval_max":     config.ChannelMonitorIntervalMaxMinutes,
	})
}

// HandleChannelMonitorExclude 只切换某渠道是否参与统一监控。
// 间隔与检测模型是全局的（模型由后端自动选择），因此这里不再接收排程字段。
func (s *Server) HandleChannelMonitorExclude(c *gin.Context) {
	id, err := ParseInt64Param(c, "id")
	if err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "invalid channel id")
		return
	}
	var request struct {
		Enabled bool `json:"enabled"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		RespondError(c, http.StatusBadRequest, err)
		return
	}
	cfg, err := s.store.GetConfig(c.Request.Context(), id)
	if err != nil {
		RespondErrorMsg(c, http.StatusNotFound, "channel not found")
		return
	}
	// 参与监控要求渠道有可用的检测模型（自动选择时取第一个已启用模型）。
	if request.Enabled {
		if _, reason := selectScheduledCheckModel(cfg); reason != "" {
			RespondErrorMsg(c, http.StatusBadRequest, reason)
			return
		}
	}
	if err := s.store.UpdateChannelMonitorParticipation(c.Request.Context(), id, request.Enabled); err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}
	s.InvalidateChannelListCache()
	RespondJSON(c, http.StatusOK, gin.H{"id": id, "monitored": request.Enabled})
}

func (s *Server) HandleChannelMonitorRun(c *gin.Context) {
	id, err := ParseInt64Param(c, "id")
	if err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "invalid channel id")
		return
	}
	cfg, err := s.store.GetConfig(c.Request.Context(), id)
	if err != nil {
		RespondErrorMsg(c, http.StatusNotFound, "channel not found")
		return
	}
	if !cfg.Enabled {
		RespondErrorMsg(c, http.StatusBadRequest, "渠道已禁用，请先启用渠道")
		return
	}
	if _, reason := selectScheduledCheckModel(cfg); reason != "" {
		RespondErrorMsg(c, http.StatusBadRequest, reason)
		return
	}
	if _, loaded := s.scheduledChannelChecksRunning.LoadOrStore(id, struct{}{}); loaded {
		RespondErrorMsg(c, http.StatusConflict, "渠道正在检测中")
		return
	}
	defer s.scheduledChannelChecksRunning.Delete(id)
	keys, err := s.store.GetAPIKeys(c.Request.Context(), id)
	if err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}
	s.runChannelMonitorCheck(c.Request.Context(), cfg, keys, configuredChannelTestContent(s.configService), model.LogSourceManualTest)
	RespondJSON(c, http.StatusOK, gin.H{"completed": true})
}
