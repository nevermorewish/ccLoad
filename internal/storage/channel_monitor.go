package storage

import (
	"context"
	"time"

	"ccLoad/internal/model"
)

// UpdateChannelMonitorParticipation updates channel monitor participation.
func (h *HybridStore) UpdateChannelMonitorParticipation(ctx context.Context, id int64, enabled bool) error {
	if err := h.sqlite.UpdateChannelMonitorParticipation(ctx, id, enabled); err != nil {
		return err
	}
	h.markChannelDirty(id, false)
	return nil
}

// UpdateChannelMonitorSchedule updates the channel monitor schedule.
func (h *HybridStore) UpdateChannelMonitorSchedule(ctx context.Context, id int64, schedule model.ChannelMonitorSchedule) error {
	if err := h.sqlite.UpdateChannelMonitorSchedule(ctx, id, schedule); err != nil {
		return err
	}
	h.markChannelDirty(id, false)
	return nil
}

// ListChannelMonitorStats returns channel monitor statistics for the time range.
func (h *HybridStore) ListChannelMonitorStats(ctx context.Context, since, until time.Time) ([]model.ChannelMonitorStats, error) {
	// Detection writes land locally first; show manual results immediately.
	return h.sqlite.ListChannelMonitorStats(ctx, since, until)
}
