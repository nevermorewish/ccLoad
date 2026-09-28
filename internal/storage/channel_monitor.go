package storage

import (
	"ccLoad/internal/model"
	"context"
	"time"
)

func (h *HybridStore) UpdateChannelMonitorSchedule(ctx context.Context, id int64, schedule model.ChannelMonitorSchedule) error {
	if err := h.sqlite.UpdateChannelMonitorSchedule(ctx, id, schedule); err != nil {
		return err
	}
	h.markChannelDirty(id, false)
	return nil
}

func (h *HybridStore) ListChannelMonitorStats(ctx context.Context, since, until time.Time) ([]model.ChannelMonitorStats, error) {
	// Detection writes land locally first; show manual results immediately.
	return h.sqlite.ListChannelMonitorStats(ctx, since, until)
}
