package app

import (
	"net/http"
	"strings"
	"time"

	"ccLoad/internal/model"
	"github.com/gin-gonic/gin"
)

type channelMonitorItem struct {
	ID             int64                        `json:"id"`
	Name           string                       `json:"name"`
	Enabled        bool                         `json:"enabled"`
	AuthType       string                       `json:"auth_type"`
	Models         []string                     `json:"models"`
	Schedule       model.ChannelMonitorSchedule `json:"schedule"`
	EffectiveModel string                       `json:"effective_model"`
	Status         string                       `json:"status"`
	Running        bool                         `json:"running"`
	NextCheck      *time.Time                   `json:"next_check"`
	Stats          model.ChannelMonitorStats    `json:"stats"`
}

func nextChannelMonitorCheck(cfg *model.Config, now time.Time) *time.Time {
	if !cfg.Enabled || !cfg.ScheduledCheckEnabled {
		return nil
	}
	// Include availability windows and day boundaries using the scheduler's exact rule.
	for candidate, end := now.Truncate(time.Minute).Add(time.Minute), now.Add(48*time.Hour); candidate.Before(end); candidate = candidate.Add(time.Minute) {
		if cfg.ScheduledCheckDueAt(candidate) {
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

func (s *Server) HandleChannelMonitor(c *gin.Context) {
	ctx := c.Request.Context()
	configs, err := s.store.ListConfigs(ctx)
	if err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}
	now := time.Now()
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
			Schedule:       model.ChannelMonitorSchedule{Enabled: cfg.ScheduledCheckEnabled, IntervalMinutes: cfg.ScheduledCheckIntervalMinutes, StartTime: cfg.ScheduledCheckStartTime, Model: cfg.ScheduledCheckModel},
			EffectiveModel: name, Status: monitorStatus(cfg, stat), Running: running, NextCheck: nextChannelMonitorCheck(cfg, now), Stats: stat,
		})
	}
	zone, offset := now.Zone()
	RespondJSON(c, http.StatusOK, gin.H{"items": items, "server_time": now, "timezone": zone, "timezone_offset_minutes": offset / 60})
}

func (s *Server) HandleChannelMonitorSchedule(c *gin.Context) {
	id, err := ParseInt64Param(c, "id")
	if err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "invalid channel id")
		return
	}
	var schedule model.ChannelMonitorSchedule
	if err := c.ShouldBindJSON(&schedule); err != nil {
		RespondError(c, http.StatusBadRequest, err)
		return
	}
	schedule.Model = strings.TrimSpace(schedule.Model)
	if err := model.ValidateScheduledCheckSchedule(schedule.IntervalMinutes, schedule.StartTime); err != nil {
		RespondError(c, http.StatusBadRequest, err)
		return
	}
	cfg, err := s.store.GetConfig(c.Request.Context(), id)
	if err != nil {
		RespondErrorMsg(c, http.StatusNotFound, "channel not found")
		return
	}
	copy := *cfg
	copy.ScheduledCheckModel = schedule.Model
	if schedule.Enabled || schedule.Model != "" {
		if _, reason := selectScheduledCheckModel(&copy); reason != "" {
			RespondErrorMsg(c, http.StatusBadRequest, reason)
			return
		}
	}
	if err := s.store.UpdateChannelMonitorSchedule(c.Request.Context(), id, schedule); err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}
	s.InvalidateChannelListCache()
	RespondJSON(c, http.StatusOK, schedule)
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
