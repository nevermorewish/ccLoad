package sql

import (
	"context"
	"fmt"
	"time"

	"ccLoad/internal/model"
)

func (s *SQLStore) UpdateChannelMonitorSchedule(ctx context.Context, id int64, schedule model.ChannelMonitorSchedule) error {
	if err := model.ValidateScheduledCheckSchedule(schedule.IntervalMinutes, schedule.StartTime); err != nil {
		return err
	}
	// Only touch schedule fields: concurrent credential refreshes and channel edits
	// must never be replaced by an older configuration snapshot.
	_, err := s.ExecContext(ctx, `UPDATE channels SET scheduled_check_enabled = ?, scheduled_check_interval_minutes = ?, scheduled_check_start_time = ?, scheduled_check_model = ?, updated_at = ? WHERE id = ?`, schedule.Enabled, schedule.IntervalMinutes, schedule.StartTime, schedule.Model, timeToUnix(time.Now()), id)
	return err
}

// UpdateChannelMonitorParticipation 只切换渠道是否参与统一监控。
// 间隔与检测模型是全局的，因此不动 scheduled_check_interval_minutes / start_time / model；
// 订阅时把遗留的单渠道排程字段归零，避免两套语义并存。
func (s *SQLStore) UpdateChannelMonitorParticipation(ctx context.Context, id int64, enabled bool) error {
	var err error
	if enabled {
		_, err = s.ExecContext(ctx, `UPDATE channels SET scheduled_check_enabled = 1,
			scheduled_check_interval_minutes = 0, scheduled_check_start_time = '', scheduled_check_model = '', updated_at = ? WHERE id = ?`,
			timeToUnix(time.Now()), id)
	} else {
		_, err = s.ExecContext(ctx, `UPDATE channels SET scheduled_check_enabled = 0, updated_at = ? WHERE id = ?`, timeToUnix(time.Now()), id)
	}
	return err
}

func (s *SQLStore) ListChannelMonitorStats(ctx context.Context, since, until time.Time) ([]model.ChannelMonitorStats, error) {
	// Aggregate every sample, but return at most twelve recent probes per model.
	// Proxy traffic, chats and balance/check-in requests cannot restore test health.
	rows, err := s.QueryContext(ctx, `SELECT channel_id, model, time, status_code, duration, message, samples, successes, average_latency FROM (
		SELECT id, channel_id, model, time, status_code, COALESCE(duration, 0) AS duration, message,
		SUM(CASE WHEN status_code <> 0 OR duration > 0 THEN 1 ELSE 0 END) OVER (PARTITION BY channel_id, model) AS samples,
		SUM(CASE WHEN status_code >= 200 AND status_code < 300 THEN 1 ELSE 0 END) OVER (PARTITION BY channel_id, model) AS successes,
		COALESCE(AVG(CASE WHEN status_code >= 200 AND status_code < 300 THEN duration END) OVER (PARTITION BY channel_id, model), 0) AS average_latency,
		ROW_NUMBER() OVER (PARTITION BY channel_id, model ORDER BY time DESC, id DESC) AS recent_rank
		FROM logs WHERE time >= ? AND time <= ? AND log_source IN ('scheduled_check', 'manual_test')
	) ranked WHERE recent_rank <= 12 ORDER BY channel_id, model, time DESC`, since.UnixMilli(), until.UnixMilli())
	if err != nil {
		return nil, fmt.Errorf("channel monitor stats: %w", err)
	}
	defer rows.Close()
	result := make([]model.ChannelMonitorStats, 0)
	for rows.Next() {
		var stat model.ChannelMonitorStats
		var probe model.ChannelMonitorProbe
		var millis int64
		if err := rows.Scan(&stat.ChannelID, &stat.Model, &millis, &probe.StatusCode, &probe.Duration, &probe.Message, &stat.Samples, &stat.Successes, &stat.AverageLatency); err != nil {
			return nil, err
		}
		probe.Time = model.JSONTime{Time: time.UnixMilli(millis)}
		if len(result) == 0 || result[len(result)-1].ChannelID != stat.ChannelID || result[len(result)-1].Model != stat.Model {
			result = append(result, stat)
		}
		last := &result[len(result)-1]
		last.Recent = append(last.Recent, probe)
		if last.LastError == "" && !(probe.StatusCode == 0 && probe.Duration == 0) && (probe.StatusCode < 200 || probe.StatusCode >= 300) {
			last.LastError = probe.Message
		}
	}
	return result, rows.Err()
}
